package gateway

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// issued is one certificate in the shape the internal certificate authority answers it: a
// P-256 leaf, its SEC1 "EC PRIVATE KEY" PEM — which is what
// `:public_key.pem_entry_encode(:ECPrivateKey, key)` writes — and the root it
// chains to. Parsing that exact PEM type is half of what this file is proving.
type issued struct {
	certificatePEM string
	privateKeyPEM  string
	caPEM          string
	notBefore      time.Time
	notAfter       time.Time
}

// root is the stand-in for the internal certificate authority's own root: one self-signed
// CA that every leaf in these tests is signed by, and the ca_pem every fetch
// comes back with.
type root struct {
	certificate *x509.Certificate
	key         *ecdsa.PrivateKey
	pem         string
}

// authority mints that root once for the whole package. Signing is the
// expensive part of these tests and there is nothing per-case about the root.
var authority = sync.OnceValues(func() (root, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return root{}, err
	}

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Gentility Overlay Root (test)"},
		NotBefore:             time.Now().Add(-24 * time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return root{}, err
	}

	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		return root{}, err
	}

	return root{
		certificate: certificate,
		key:         key,
		pem:         string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
	}, nil
})

// pool is the roots a node would be pinned to — the ca_pem out of the config
// document, and nothing else.
func pool(t *testing.T) *x509.CertPool {
	t.Helper()

	ca, err := authority()
	if err != nil {
		t.Fatalf("minting the test root: %v", err)
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(ca.pem)) {
		t.Fatal("the test root would not go into a pool")
	}

	return roots
}

// mint signs one leaf for hostname and addresses.
//
// addresses become the iPAddress SANs, which are the SANs that decide
// anything: a node dials its gateway by address literal, so the address is
// what its handshake verifies.
func mint(t *testing.T, hostname string, addresses []netip.Addr, notBefore, notAfter time.Time) issued {
	t.Helper()

	ca, err := authority()
	if err != nil {
		t.Fatalf("minting the test root: %v", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}

	named := make([]net.IP, 0, len(addresses))
	for _, address := range addresses {
		named = append(named, net.IP(address.AsSlice()))
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: hostname},
		DNSNames:     []string{hostname},
		IPAddresses:  named,
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, ca.certificate, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("signing a certificate: %v", err)
	}

	sec1, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("encoding the key: %v", err)
	}

	return issued{
		certificatePEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		privateKeyPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1})),
		caPEM:          ca.pem,
		notBefore:      notBefore,
		notAfter:       notAfter,
	}
}

// endpoint stands in for the internal certificate endpoint, answering the
// way it answers: 403 to any request carrying an overlay peer header, 401
// without an acceptable bearer, 422 to a body that is not a list of addresses,
// and otherwise a leaf, its key and the root.
func endpoint(t *testing.T, secret []byte, leaf func(addresses []netip.Addr) issued) (*httptest.Server, *atomic.Int64) {
	t.Helper()

	var issues atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for name := range r.Header {
			if strings.HasPrefix(strings.ToLower(name), "x-overlay-peer") {
				refuse(w, http.StatusForbidden, "overlay_peer_on_internal_surface")
				return
			}
		}

		if !acceptable(r.Header.Get("Authorization"), secret) {
			refuse(w, http.StatusUnauthorized, "invalid_bearer")
			return
		}

		var body certRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			refuse(w, http.StatusUnprocessableEntity, "invalid_request")
			return
		}
		if len(body.Addresses) == 0 {
			refuse(w, http.StatusUnprocessableEntity, "no_addresses")
			return
		}
		asked := make([]netip.Addr, 0, len(body.Addresses))
		for _, address := range body.Addresses {
			parsed, err := netip.ParseAddr(address)
			if err != nil || !strings.HasPrefix(address, "fd06:") {
				refuse(w, http.StatusUnprocessableEntity, "invalid_address")
				return
			}
			asked = append(asked, parsed)
		}

		issues.Add(1)
		answer := leaf(asked)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(certResponse{
			CertificatePEM: answer.certificatePEM,
			PrivateKeyPEM:  answer.privateKeyPEM,
			CAPEM:          answer.caPEM,
			NotBefore:      answer.notBefore,
			NotAfter:       answer.notAfter,
		})
	}))
	t.Cleanup(server.Close)

	return server, &issues
}

// acceptable is the internal bearer verifier: the bearer this
// deployment would have minted, at any second inside the minute either side of
// now.
func acceptable(header string, secret []byte) bool {
	presented, found := strings.CutPrefix(header, "Bearer ")
	if !found {
		return false
	}

	now := time.Now().Unix()
	for unixTs := now - 60; unixTs <= now+60; unixTs++ {
		want, err := Bearer(secret, unixTs)
		if err == nil && want == presented {
			return true
		}
	}

	return false
}

func certs(t *testing.T, upstream string, secret []byte) *Certs {
	t.Helper()

	target, err := url.Parse(upstream)
	if err != nil {
		t.Fatalf("parsing the upstream: %v", err)
	}

	return NewCerts(target, secret, nil, nil)
}

var testAddresses = []netip.Addr{
	netip.MustParseAddr("fd06:6e7:0:1::1"),
	netip.MustParseAddr("fd06:6e7:0:2::1"),
}

func TestFetchInstallsTheLeaf(t *testing.T) {
	notBefore := time.Now().Truncate(time.Second)
	notAfter := notBefore.Add(90 * 24 * time.Hour)

	server, issues := endpoint(t, testSecret, func(addresses []netip.Addr) issued {
		return mint(t, "gentility-overlay.internal", addresses, notBefore, notAfter)
	})

	client := certs(t, server.URL, testSecret)

	leaf, err := client.Fetch(context.Background(), testAddresses)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if issues.Load() != 1 {
		t.Errorf("the endpoint issued %d times", issues.Load())
	}
	if !leaf.NotAfter.Equal(notAfter) {
		t.Errorf("not_after = %s, want %s", leaf.NotAfter, notAfter)
	}

	got, err := client.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if got.Leaf == nil && len(got.Certificate) == 0 {
		t.Error("GetCertificate answered an empty certificate")
	}
}

func TestFetchSendsTheAddressesItWasGiven(t *testing.T) {
	var sent []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body certRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		sent = body.Addresses
		refuse(w, http.StatusUnprocessableEntity, "no_addresses")
	}))
	t.Cleanup(server.Close)

	_, _ = certs(t, server.URL, testSecret).Fetch(context.Background(), testAddresses)

	if len(sent) != 2 || sent[0] != "fd06:6e7:0:1::1" || sent[1] != "fd06:6e7:0:2::1" {
		t.Errorf("the endpoint was asked for %v", sent)
	}
}

// The hostname is not sent, because it is not the caller's to choose: the app
// issues for its own platform_hostname, so a bearer-holder cannot have the
// internal CA sign a certificate for a name of its choosing.
func TestFetchNamesNoHostname(t *testing.T) {
	var raw map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&raw)
		refuse(w, http.StatusUnprocessableEntity, "no_addresses")
	}))
	t.Cleanup(server.Close)

	_, _ = certs(t, server.URL, testSecret).Fetch(context.Background(), testAddresses)

	if len(raw) != 1 {
		t.Errorf("the request body carried %v, want addresses and nothing else", raw)
	}
}

// The app answers 403 to any request on this surface carrying an overlay peer
// header, because such a request arrived through a gateway and no node may ask
// for the gateway's key. The gateway's own fetch must therefore carry none.
func TestFetchCarriesNoOverlayPeerHeaders(t *testing.T) {
	server, _ := endpoint(t, testSecret, func(addresses []netip.Addr) issued {
		return mint(t, "gentility-overlay.internal", addresses, time.Now(), time.Now().Add(time.Hour))
	})

	if _, err := certs(t, server.URL, testSecret).Fetch(context.Background(), testAddresses); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
}

func TestFetchRefusalsCarryTheMothershipsReason(t *testing.T) {
	leaf := func(addresses []netip.Addr) issued {
		return mint(t, "gentility-overlay.internal", addresses, time.Now(), time.Now().Add(time.Hour))
	}

	cases := []struct {
		name      string
		secret    []byte
		addresses []netip.Addr
		status    int
		reason    string
	}{
		{
			name: "a bearer the app did not mint", secret: []byte("the wrong secret"),
			addresses: testAddresses, status: http.StatusUnauthorized, reason: "invalid_bearer",
		},
		{
			name: "an address outside the overlay", secret: testSecret,
			addresses: []netip.Addr{netip.MustParseAddr("2001:db8::1")},
			status:    http.StatusUnprocessableEntity, reason: "invalid_address",
		},
		{
			name: "no addresses at all", secret: testSecret, addresses: nil,
			status: http.StatusUnprocessableEntity, reason: "no_addresses",
		},
	}

	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			server, _ := endpoint(t, testSecret, leaf)

			_, err := certs(t, server.URL, one.secret).Fetch(context.Background(), one.addresses)

			var refused *RefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("Fetch answered %v, want a RefusedError", err)
			}
			if refused.StatusCode != one.status || refused.Reason != one.reason {
				t.Errorf("refused with %d %s, want %d %s",
					refused.StatusCode, refused.Reason, one.status, one.reason)
			}
		})
	}
}

// Nothing this client says out loud may carry the secret it holds, the bearer
// it minted, or the key it was given. The endpoint below answers a refusal
// that echoes all three back, which is the shape a compromised or confused
// upstream would take.
func TestRefusalsNeverCarryTheSecretOrTheBearer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := r.Header.Get("Authorization")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"`+bearer+" "+string(testSecret)+`"}`)
	}))
	t.Cleanup(server.Close)

	_, err := certs(t, server.URL, testSecret).Fetch(context.Background(), testAddresses)
	if err == nil {
		t.Fatal("Fetch did not fail")
	}

	bearer, mintErr := Bearer(testSecret, time.Now().Unix())
	if mintErr != nil {
		t.Fatalf("Bearer: %v", mintErr)
	}

	said := err.Error()
	if strings.Contains(said, string(testSecret)) {
		t.Errorf("the error carries the ingress secret: %s", said)
	}
	if strings.Contains(said, bearer) {
		t.Errorf("the error carries the internal bearer: %s", said)
	}
}

// A leaf and a key that are not a pair is a refusal, and the refusal says so
// without quoting either of them.
func TestAnUnusablePairIsRefusedWithoutQuotingIt(t *testing.T) {
	other := mint(t, "somebody-else", testAddresses, time.Now(), time.Now().Add(time.Hour))
	leaf := mint(t, "gentility-overlay.internal", testAddresses, time.Now(), time.Now().Add(time.Hour))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(certResponse{
			CertificatePEM: leaf.certificatePEM,
			PrivateKeyPEM:  other.privateKeyPEM,
			CAPEM:          leaf.caPEM,
			NotBefore:      leaf.notBefore,
			NotAfter:       leaf.notAfter,
		})
	}))
	t.Cleanup(server.Close)

	_, err := certs(t, server.URL, testSecret).Fetch(context.Background(), testAddresses)
	if err == nil {
		t.Fatal("a mismatched certificate and key were accepted")
	}
	if strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Errorf("the refusal quotes the key material: %s", err)
	}
}

func TestGetCertificateRefusesBeforeTheFirstFetch(t *testing.T) {
	server, _ := endpoint(t, testSecret, func(addresses []netip.Addr) issued {
		return mint(t, "gentility-overlay.internal", addresses, time.Now(), time.Now().Add(time.Hour))
	})

	if _, err := certs(t, server.URL, testSecret).GetCertificate(&tls.ClientHelloInfo{}); err == nil {
		t.Error("a handshake was answered with no certificate")
	}
}

func TestRenewAtIsTwoThirdsOfTheLife(t *testing.T) {
	notBefore := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	leaf := Leaf{NotBefore: notBefore, NotAfter: notBefore.Add(90 * 24 * time.Hour)}

	if want := notBefore.Add(60 * 24 * time.Hour); !leaf.renewAt().Equal(want) {
		t.Errorf("renewAt = %s, want %s (sixty days into a ninety-day life)", leaf.renewAt(), want)
	}
}

// The renewal swaps the certificate through GetCertificate, so no listener
// restarts and nothing in flight is disturbed.
func TestRunRenewsAndSwapsWithoutARestart(t *testing.T) {
	// Already two thirds spent, so the loop renews on its first turn.
	server, issues := endpoint(t, testSecret, func(addresses []netip.Addr) issued {
		return mint(t, "gentility-overlay.internal", addresses,
			time.Now().Add(-80*24*time.Hour), time.Now().Add(10*24*time.Hour))
	})

	client := certs(t, server.URL, testSecret)
	client.CheckEvery = time.Millisecond
	client.RenewFloor = time.Millisecond
	client.RenewRetry = time.Millisecond

	if _, err := client.Fetch(context.Background(), testAddresses); err != nil {
		t.Fatalf("the first Fetch: %v", err)
	}
	first := client.Current()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go client.Run(ctx, func(context.Context) ([]netip.Addr, error) { return testAddresses, nil })

	deadline := time.Now().Add(5 * time.Second)
	for client.Current() == first && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	if issues.Load() < 2 {
		t.Fatal("the renewal loop never asked for a second certificate")
	}
	if client.Current() == first {
		t.Error("a second certificate was issued but the gateway is still serving the first")
	}
}

// A renewal that fails leaves the certificate the gateway is serving alone and
// tries again. Thirty days of headroom is what makes that the right answer
// rather than a scramble.
func TestAFailedRenewalKeepsTheCertificateItHas(t *testing.T) {
	var attempts atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) > 1 {
			refuse(w, http.StatusInternalServerError, "internal_error")
			return
		}

		leaf := mint(t, "gentility-overlay.internal", testAddresses,
			time.Now().Add(-80*24*time.Hour), time.Now().Add(10*24*time.Hour))
		_ = json.NewEncoder(w).Encode(certResponse{
			CertificatePEM: leaf.certificatePEM,
			PrivateKeyPEM:  leaf.privateKeyPEM,
			CAPEM:          leaf.caPEM,
			NotBefore:      leaf.notBefore,
			NotAfter:       leaf.notAfter,
		})
	}))
	t.Cleanup(server.Close)

	client := certs(t, server.URL, testSecret)
	client.CheckEvery = time.Millisecond
	client.RenewFloor = time.Millisecond
	client.RenewRetry = time.Millisecond

	if _, err := client.Fetch(context.Background(), testAddresses); err != nil {
		t.Fatalf("the first Fetch: %v", err)
	}
	held := client.Current()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go client.Run(ctx, func(context.Context) ([]netip.Addr, error) { return testAddresses, nil })

	deadline := time.Now().Add(5 * time.Second)
	for attempts.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	if attempts.Load() < 3 {
		t.Fatal("the renewal loop gave up after one failure")
	}
	if client.Current() != held {
		t.Error("a failed renewal replaced the certificate the gateway was serving")
	}
	if _, err := client.GetCertificate(&tls.ClientHelloInfo{}); err != nil {
		t.Errorf("handshakes stopped being answered during a failed renewal: %v", err)
	}
}

// An organization added after boot is an address this gateway starts
// answering on within a second, and a leaf that does not name it is a
// handshake every node in that organization fails. So the SAN set follows the
// address set: Run notices the address it does not cover, fetches for the set
// the node actually holds, and a node dialling the new address — by address
// literal, which is what makes the iPAddress SAN the one that decides —
// verifies against the certificate that comes back.
func TestRunFetchesForAnAddressAppearingAfterBoot(t *testing.T) {
	booted := netip.MustParseAddr("fd06:6e7:0:1::1")
	added := netip.MustParseAddr("fd06:6e7:0:2::1")

	var asked atomic.Pointer[[]netip.Addr]

	server, _ := endpoint(t, testSecret, func(addresses []netip.Addr) issued {
		named := append([]netip.Addr(nil), addresses...)
		asked.Store(&named)

		return mint(t, "gentility-overlay.internal", addresses,
			time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour))
	})

	client := certs(t, server.URL, testSecret)
	client.CheckEvery = time.Millisecond
	client.RenewFloor = time.Millisecond
	client.RenewRetry = time.Millisecond

	if _, err := client.Fetch(context.Background(), []netip.Addr{booted}); err != nil {
		t.Fatalf("the boot fetch: %v", err)
	}
	first := client.Current()

	held := &atomic.Pointer[[]netip.Addr]{}
	held.Store(&[]netip.Addr{booted})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go client.Run(ctx, func(context.Context) ([]netip.Addr, error) { return *held.Load(), nil })

	// The grant lands: the node now holds a second address.
	held.Store(&[]netip.Addr{booted, added})

	deadline := time.Now().Add(5 * time.Second)
	for client.Current() == first && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if client.Current() == first {
		t.Fatal("the loop never fetched a certificate for the address that appeared")
	}

	requested := asked.Load()
	if requested == nil || !slices.Contains(*requested, added) {
		t.Fatalf("the request body carried %v, want the new address %s in it", requested, added)
	}

	// And the handshake a node in that organization makes now succeeds. It
	// names the address rather than the hostname because that is what a node
	// dials: enroll.Gateway builds https://[address]:443 out of the config
	// document, so the address is the name being verified.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("binding a stand-in gateway listener: %v", err)
	}

	door := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "served")
	})}
	go door.Serve(tls.NewListener(ln, &tls.Config{
		GetCertificate: client.GetCertificate,
		MinVersion:     tls.VersionTLS13,
		NextProtos:     []string{"http/1.1"},
	}))
	t.Cleanup(func() { door.Close() })

	node := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:    pool(t),
		ServerName: added.String(),
		MinVersion: tls.VersionTLS13,
	}}}

	response, err := node.Get("https://" + ln.Addr().String() + "/overlay/self/config")
	if err != nil {
		t.Fatalf("a node dialling the new address: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Errorf("the new address answered %d", response.StatusCode)
	}
}

// The other half of the same rule: a certificate that already covers what the
// node holds, and is nowhere near renewal, is left alone. Checking every five
// seconds is only affordable because checking is not fetching.
func TestRunLeavesACoveringCertificateAlone(t *testing.T) {
	server, issues := endpoint(t, testSecret, func(addresses []netip.Addr) issued {
		return mint(t, "gentility-overlay.internal", addresses,
			time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour))
	})

	client := certs(t, server.URL, testSecret)
	client.CheckEvery = time.Millisecond
	client.RenewFloor = time.Millisecond
	client.RenewRetry = time.Millisecond

	if _, err := client.Fetch(context.Background(), testAddresses); err != nil {
		t.Fatalf("the boot fetch: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go client.Run(ctx, func(context.Context) ([]netip.Addr, error) { return testAddresses, nil })

	// A hundred turns of the loop at a millisecond apiece.
	time.Sleep(100 * time.Millisecond)

	if issues.Load() != 1 {
		t.Errorf("the endpoint issued %d times, want only the boot fetch", issues.Load())
	}
}

// A leaf that does not chain to the ca_pem it came with is refused rather than
// installed. Every node is pinned to exactly that root, so such a leaf is one
// no node will accept — and a gateway that installed it would swap a working
// certificate for an outage that only shows up at the far end.
func TestALeafThatDoesNotChainToItsCAIsRefused(t *testing.T) {
	leaf := mint(t, "gentility-overlay.internal", testAddresses, time.Now(), time.Now().Add(time.Hour))

	stranger, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "somebody else's root"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &stranger.PublicKey, stranger)
	if err != nil {
		t.Fatalf("signing a root: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(certResponse{
			CertificatePEM: leaf.certificatePEM,
			PrivateKeyPEM:  leaf.privateKeyPEM,
			CAPEM:          string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
			NotBefore:      leaf.notBefore,
			NotAfter:       leaf.notAfter,
		})
	}))
	t.Cleanup(server.Close)

	client := certs(t, server.URL, testSecret)

	if _, err := client.Fetch(context.Background(), testAddresses); err == nil {
		t.Fatal("a leaf that chains to nothing the nodes trust was installed")
	}
	if client.Current() != nil {
		t.Error("the refused leaf was installed anyway")
	}
}

// A reason nobody wrote down is not a reason. The vocabulary is a fixed list
// in this package and the far end only gets to say which of them it means, so
// there is no shape of text an upstream can put in a log line here.
func TestOnlyKnownRefusalReasonsAreCarried(t *testing.T) {
	cases := map[string]string{
		"invalid_bearer":                   "invalid_bearer",
		"overlay_peer_on_internal_surface": "overlay_peer_on_internal_surface",
		"internal_error":                   "internal_error",
		"a_reason_nobody_wrote_down":       "",
		"Bearer eyJhbGciOi":                "",
		"":                                 "",
	}

	for answered, want := range cases {
		t.Run(answered, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				refuse(w, http.StatusForbidden, answered)
			}))
			t.Cleanup(server.Close)

			_, err := certs(t, server.URL, testSecret).Fetch(context.Background(), testAddresses)

			var refused *RefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("Fetch answered %v, want a RefusedError", err)
			}
			if refused.Reason != want {
				t.Errorf("the refusal carried %q, want %q", refused.Reason, want)
			}
		})
	}
}
