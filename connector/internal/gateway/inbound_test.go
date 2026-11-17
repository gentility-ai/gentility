package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// peerAddress is the address the fake attribution answers with — what a real
// WhoIs would have resolved off the connection.
const peerAddress = "fd06:6e7:0:1::2"

var testSecret = []byte("overlay wire vector secret")

// listener is the signed-peer verifier and the plug in front of
// it, in Go: the same three headers, the same MAC, the same window, and the
// same refusals on the wire.
//
// The timestamp check is the contract's: canonical decimal, no sign and no
// leading zero. Both ends are strict about it — the app guards `Integer.parse/1`
// with a regex for exactly that shape — so a stamp this refuses is a stamp the
// app refuses, and the two agree on the bytes rather than on the number.
func listener(t *testing.T, secret []byte, handler http.HandlerFunc) *httptest.Server {
	t.Helper()

	return listenerAt(t, secret, time.Now, handler)
}

// listenerAt is listener with the clock as an argument, for the one test that
// has to move both sides of the hop at once.
func listenerAt(t *testing.T, secret []byte, now func() time.Time, handler http.HandlerFunc) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(verifying(secret, now, handler))
	t.Cleanup(server.Close)

	return server
}

// verifying is the admission check above without the server under it, for the
// tests that have to serve the app on an address of their own choosing —
// `rolling_test.go` rebinds one, which is what a replaced app container does to
// its network alias and not something httptest will pick a port for.
func verifying(secret []byte, now func() time.Time, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		peer := r.Header.Get(PeerHeader)
		rawTs := r.Header.Get(PeerTsHeader)
		sig := r.Header.Get(PeerSigHeader)

		if peer == "" || rawTs == "" || sig == "" {
			refuse(w, http.StatusUnauthorized, "unsigned_peer")
			return
		}

		if strings.TrimLeft(rawTs, "0123456789") != "" || (len(rawTs) > 1 && rawTs[0] == '0') {
			refuse(w, http.StatusUnauthorized, "bad_signature")
			return
		}
		unixTs, err := strconv.ParseInt(rawTs, 10, 64)
		if err != nil {
			refuse(w, http.StatusUnauthorized, "bad_signature")
			return
		}

		presented, err := base64.RawURLEncoding.DecodeString(sig)
		if err != nil {
			refuse(w, http.StatusUnauthorized, "bad_signature")
			return
		}

		mac := hmac.New(sha256.New, secret)
		mac.Write([]byte(peer + ":" + rawTs))
		if !hmac.Equal(presented, mac.Sum(nil)) {
			refuse(w, http.StatusUnauthorized, "bad_signature")
			return
		}

		if delta := now().Unix() - unixTs; delta > 30 || delta < -30 {
			refuse(w, http.StatusUnauthorized, "stale_signature")
			return
		}

		handler(w, r)
	}
}

func refuse(w http.ResponseWriter, status int, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": reason})
}

// gate stands the gateway's inbound door in front of upstream, attributing
// every connection as peerAddress.
func gate(t *testing.T, upstream string) (*httptest.Server, *Inbound) {
	t.Helper()

	return gateAttributing(t, upstream, func(context.Context, netip.AddrPort, netip.Addr) (netip.Addr, error) {
		return netip.MustParseAddr(peerAddress), nil
	})
}

func gateAttributing(t *testing.T, upstream string, attribute Attribute) (*httptest.Server, *Inbound) {
	t.Helper()

	target, err := url.Parse(upstream)
	if err != nil {
		t.Fatalf("parsing the upstream: %v", err)
	}

	inbound := NewInbound(target, attribute, testSecret, nil)

	server := httptest.NewServer(inbound)
	t.Cleanup(server.Close)

	return server, inbound
}

func get(t *testing.T, server *httptest.Server, path string, header http.Header) *http.Response {
	t.Helper()

	request, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	for name, values := range header {
		request.Header[name] = values
	}

	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("requesting %s: %v", path, err)
	}
	t.Cleanup(func() { response.Body.Close() })

	return response
}

func body(t *testing.T, response *http.Response) string {
	t.Helper()

	read, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}

	return string(read)
}

func TestForwardsToUpstreamWithSignedPeer(t *testing.T) {
	upstream := listener(t, testSecret, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.Header.Get(PeerHeader))
	})

	gateway, _ := gate(t, upstream.URL)

	response := get(t, gateway, "/overlay/self/config", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d %s, want 200", response.StatusCode, body(t, response))
	}
	if got := body(t, response); got != peerAddress {
		t.Errorf("the listener saw peer %q, want %q", got, peerAddress)
	}
}

// The signature the gateway sets is one the app's own verifier would accept:
// the listener above refuses anything else, so a 200 here IS the assertion.
// This test names the failure the other way round, so a broken MAC reads as a
// signature problem rather than as "the fixture said 401".
func TestTheSignatureIsOneTheListenerAccepts(t *testing.T) {
	upstream := listener(t, testSecret, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "admitted")
	})

	gateway, _ := gate(t, upstream.URL)

	if got := body(t, get(t, gateway, "/x", nil)); got != "admitted" {
		t.Errorf("the listener refused the gateway's own signature: %s", got)
	}
}

// A gateway signing with a secret the app does not hold is refused, and it is
// refused as bad_signature rather than let through as anything else. This is
// the mismatch an operator sees when the two containers hold different
// OVERLAY_INGRESS_SECRETs.
func TestTheListenerRefusesAGatewaySigningWithAnotherSecret(t *testing.T) {
	upstream := listener(t, testSecret, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "admitted")
	})

	target, _ := url.Parse(upstream.URL)
	gateway := httptest.NewServer(NewInbound(target, func(context.Context, netip.AddrPort, netip.Addr) (netip.Addr, error) {
		return netip.MustParseAddr(peerAddress), nil
	}, []byte("a secret the app does not hold"), nil))
	t.Cleanup(gateway.Close)

	response := get(t, gateway, "/x", nil)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.StatusCode)
	}
	if got := body(t, response); !strings.Contains(got, "bad_signature") {
		t.Errorf("the listener answered %q, want bad_signature", got)
	}
}

func TestStripsClientSuppliedPeerHeaders(t *testing.T) {
	upstream := listener(t, testSecret, func(w http.ResponseWriter, r *http.Request) {
		if got := len(r.Header.Values(PeerHeader)); got != 1 {
			t.Errorf("the listener saw %d peer headers, want exactly 1", got)
		}
		fmt.Fprint(w, r.Header.Get(PeerHeader))
	})

	gateway, _ := gate(t, upstream.URL)

	forged := http.Header{}
	forged.Set(PeerHeader, "fd06:6e7:0:9::9")

	if got := body(t, get(t, gateway, "/x", forged)); got != peerAddress {
		t.Errorf("the listener saw peer %q, want the attributed %q", got, peerAddress)
	}
}

// The forgery that would matter most: a node that supplies all three, a valid
// signature of its own over the address it wants to be. The gateway deletes
// them before it signs, so what reaches the listener is the address WhoIs
// resolved and a signature over that.
func TestStripsClientSuppliedSignatureAndTimestamp(t *testing.T) {
	var seenTs, seenSig string

	upstream := listener(t, testSecret, func(w http.ResponseWriter, r *http.Request) {
		if got := len(r.Header.Values(PeerSigHeader)); got != 1 {
			t.Errorf("the listener saw %d signature headers, want exactly 1", got)
		}
		seenTs = r.Header.Get(PeerTsHeader)
		seenSig = r.Header.Get(PeerSigHeader)
		fmt.Fprint(w, r.Header.Get(PeerHeader))
	})

	gateway, _ := gate(t, upstream.URL)

	// A well-formed lie: the client signs "the address I claim" with the real
	// secret, which it would only hold if it were the gateway.
	claimed := "fd06:6e7:0:9::9"
	unixTs := time.Now().Unix()

	forged := http.Header{}
	forged.Set(PeerHeader, claimed)
	forged.Set(PeerTsHeader, strconv.FormatInt(unixTs, 10))
	forged.Set(PeerSigHeader, Sign(testSecret, claimed, unixTs))

	if got := body(t, get(t, gateway, "/x", forged)); got != peerAddress {
		t.Fatalf("the listener saw peer %q, want the attributed %q", got, peerAddress)
	}
	if seenSig == Sign(testSecret, claimed, unixTs) {
		t.Error("the client's own signature reached the listener")
	}
	if seenSig != Sign(testSecret, peerAddress, mustParseInt(t, seenTs)) {
		t.Error("the signature that reached the listener is not one over the attributed peer")
	}
}

func mustParseInt(t *testing.T, raw string) int64 {
	t.Helper()

	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("the timestamp header %q is not canonical decimal: %v", raw, err)
	}

	return value
}

func TestRefusesConnectionWhenWhoIsFails(t *testing.T) {
	var forwarded atomic.Int64

	upstream := listener(t, testSecret, func(w http.ResponseWriter, r *http.Request) {
		forwarded.Add(1)
	})

	gateway, _ := gateAttributing(t, upstream.URL, func(context.Context, netip.AddrPort, netip.Addr) (netip.Addr, error) {
		return netip.Addr{}, fmt.Errorf("tsnode: whois: the local API is down")
	})

	response := get(t, gateway, "/x", nil)
	if response.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", response.StatusCode)
	}
	if !response.Close {
		t.Error("the connection was left open on a peer the gateway could not attribute")
	}
	if forwarded.Load() != 0 {
		t.Errorf("%d request(s) reached the listener anyway", forwarded.Load())
	}
}

// A peer the head knows that reached a listener in an organization it holds no
// grant in. tsnode.WhoIs is what refuses it; this asserts the gateway does not
// forward it anyway.
func TestRefusesACrossOrgPeer(t *testing.T) {
	var forwarded atomic.Int64

	upstream := listener(t, testSecret, func(w http.ResponseWriter, r *http.Request) {
		forwarded.Add(1)
	})

	gateway, _ := gateAttributing(t, upstream.URL, func(context.Context, netip.AddrPort, netip.Addr) (netip.Addr, error) {
		return netip.Addr{}, fmt.Errorf("peer is in fd06:6e7:0:2::/64 but reached fd06:6e7:0:1::/64")
	})

	if got := get(t, gateway, "/x", nil).StatusCode; got != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", got)
	}
	if forwarded.Load() != 0 {
		t.Errorf("%d cross-org request(s) were forwarded", forwarded.Load())
	}
}

// An attribution that answers no error and no address is a bug, not an
// admission. Fail closed.
func TestRefusesAnAttributionThatNamesNoAddress(t *testing.T) {
	var forwarded atomic.Int64

	upstream := listener(t, testSecret, func(w http.ResponseWriter, r *http.Request) {
		forwarded.Add(1)
	})

	gateway, _ := gateAttributing(t, upstream.URL, func(context.Context, netip.AddrPort, netip.Addr) (netip.Addr, error) {
		return netip.Addr{}, nil
	})

	if got := get(t, gateway, "/x", nil).StatusCode; got != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", got)
	}
	if forwarded.Load() != 0 {
		t.Error("a request with no attributed address was forwarded")
	}
}

func TestForwardsMethodPathHeadersAndBody(t *testing.T) {
	type seen struct {
		method, path, query, capver, authorization, body string
	}
	var got seen

	upstream := listener(t, testSecret, func(w http.ResponseWriter, r *http.Request) {
		read, _ := io.ReadAll(r.Body)
		got = seen{
			method:        r.Method,
			path:          r.URL.Path,
			query:         r.URL.RawQuery,
			capver:        r.Header.Get("X-Node-Capver"),
			authorization: r.Header.Get("Authorization"),
			body:          string(read),
		}
		w.Header().Set("X-Answer", "from the listener")
		w.WriteHeader(http.StatusNoContent)
	})

	gateway, _ := gate(t, upstream.URL)

	request, err := http.NewRequest(http.MethodPost, gateway.URL+"/overlay/self/presence?since=4",
		strings.NewReader(`{"capver":1}`))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	request.Header.Set("X-Node-Capver", "1")
	request.Header.Set("Authorization", "Bearer the-node-credential")

	response, err := gateway.Client().Do(request)
	if err != nil {
		t.Fatalf("requesting: %v", err)
	}
	defer response.Body.Close()

	want := seen{
		method: http.MethodPost, path: "/overlay/self/presence", query: "since=4",
		capver: "1", authorization: "Bearer the-node-credential", body: `{"capver":1}`,
	}
	if got != want {
		t.Errorf("the listener saw %+v, want %+v", got, want)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", response.StatusCode)
	}
	if response.Header.Get("X-Answer") != "from the listener" {
		t.Error("the listener's own response headers did not come back")
	}
}

// The config channel is a long poll: the mothership holds the request open and
// the answer has to arrive when it is written, not when a buffer fills.
//
// The plan's version of this test waits out a seventy-second poll. This one
// proves the same property in milliseconds: the first chunk must reach the
// client while the upstream is still holding the response open.
func TestStreamsALongPollWithoutBuffering(t *testing.T) {
	release := make(chan struct{})

	upstream := listener(t, testSecret, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "first")
		w.(http.Flusher).Flush()
		<-release
		fmt.Fprint(w, "second")
	})
	t.Cleanup(func() { close(release) })

	gateway, _ := gate(t, upstream.URL)

	response := get(t, gateway, "/overlay/self/config", nil)

	first := make([]byte, 5)
	read := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(response.Body, first)
		read <- err
	}()

	select {
	case err := <-read:
		if err != nil {
			t.Fatalf("reading the first chunk: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first chunk never arrived; the gateway is buffering the response")
	}

	if string(first) != "first" {
		t.Errorf("the first chunk was %q", first)
	}

	release <- struct{}{}

	rest, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the rest: %v", err)
	}
	if string(rest) != "second" {
		t.Errorf("the rest was %q", rest)
	}
}

// The rolling-deploy shape, unit-sized: kamal replaces the app container, so
// for a few seconds there is nothing behind the alias. The gateway answers 502
// and stays up — a gateway that died with the app it proxies would take the
// whole fleet off the network on every deploy.
func TestUpstreamDownReturns502AndKeepsListening(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("binding a stand-in upstream: %v", err)
	}
	address := ln.Addr().String()
	_ = ln.Close()

	gateway, _ := gate(t, "http://"+address)

	if got := get(t, gateway, "/x", nil).StatusCode; got != http.StatusBadGateway {
		t.Errorf("status = %d with no upstream, want 502", got)
	}
	if got := get(t, gateway, "/x", nil).StatusCode; got != http.StatusBadGateway {
		t.Errorf("the gateway stopped answering after one refusal: %d", got)
	}
}

func TestUpstreamComesBackWithoutARestart(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("binding the first upstream: %v", err)
	}
	address := ln.Addr().String()

	first := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "container A")
	})}
	go first.Serve(ln)

	gateway, _ := gate(t, "http://"+address)

	if got := body(t, get(t, gateway, "/x", nil)); got != "container A" {
		t.Fatalf("the first container answered %q", got)
	}

	// The old container goes away.
	_ = first.Close()
	if got := get(t, gateway, "/x", nil).StatusCode; got != http.StatusBadGateway {
		t.Fatalf("status during the swap = %d, want 502", got)
	}

	// The new one takes the same alias.
	again, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("rebinding for the second upstream: %v", err)
	}
	second := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "container B")
	})}
	go second.Serve(again)
	t.Cleanup(func() { second.Close() })

	if got := body(t, get(t, gateway, "/x", nil)); got != "container B" {
		t.Errorf("after the swap the gateway answered %q, want container B", got)
	}
}

// Two requests on one keep-alive connection, forty seconds apart, both have to
// verify — so the timestamp is minted per request and never per connection.
// The clock is moved rather than waited out.
func TestTimestampIsMintedPerRequestNotPerConnection(t *testing.T) {
	// One clock, moved once, read by both ends of the hop — which is what
	// forty seconds of real time would have done, without the forty seconds.
	var clock atomic.Int64
	clock.Store(time.Now().Unix())
	now := func() time.Time { return time.Unix(clock.Load(), 0) }

	var stamps []string
	seen := make(chan string, 2)

	upstream := listenerAt(t, testSecret, now, func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get(PeerTsHeader)
	})

	gateway, inbound := gate(t, upstream.URL)
	inbound.Now = now

	if got := get(t, gateway, "/x", nil).StatusCode; got != http.StatusOK {
		t.Fatalf("the first request was refused: %d", got)
	}
	stamps = append(stamps, <-seen)

	clock.Add(40)

	if got := get(t, gateway, "/x", nil).StatusCode; got != http.StatusOK {
		t.Fatalf("the second request, forty seconds later, was refused: %d", got)
	}
	stamps = append(stamps, <-seen)

	if stamps[0] == stamps[1] {
		t.Errorf("both requests carried %s; the second reused the first's timestamp", stamps[0])
	}
}

// The refusals the app answers, replayed: a stale header is stale_signature
// and a forged one is bad_signature, decided in that order.
func TestAStaleTimestampIsRefusedAsStale(t *testing.T) {
	upstream := listener(t, testSecret, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "admitted")
	})

	gateway, inbound := gate(t, upstream.URL)
	inbound.Now = func() time.Time { return time.Now().Add(-45 * time.Second) }

	response := get(t, gateway, "/x", nil)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.StatusCode)
	}
	if got := body(t, response); !strings.Contains(got, "stale_signature") {
		t.Errorf("the listener answered %q, want stale_signature", got)
	}
}

// Closing a listener is how the reconcile loop unbinds an address whose grant
// was revoked — and closing a listener does nothing whatever to the
// connections it already accepted. Without the server going with it, a node in
// a revoked organization keeps being served, on the keep-alive connection it
// already holds, for as long as it cares to hold the socket.
func TestServeClosesTheConnectionsItAlreadyAccepted(t *testing.T) {
	upstream := listener(t, testSecret, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "served")
	})

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parsing the upstream: %v", err)
	}

	inbound := NewInbound(target, func(context.Context, netip.AddrPort, netip.Addr) (netip.Addr, error) {
		return netip.MustParseAddr(peerAddress), nil
	}, testSecret, nil)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("binding: %v", err)
	}
	address := ln.Addr().String()

	go Serve(context.Background(), ln, inbound)

	// One request, read to the end and its body closed, so the connection goes
	// back in the pool and the next request would reuse it.
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport}

	response, err := client.Get("http://" + address + "/overlay/self/config")
	if err != nil {
		t.Fatalf("the first request: %v", err)
	}
	read, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(read) != "served" {
		t.Fatalf("the first request answered %q, %v", read, err)
	}

	// The grant is revoked.
	if err := ln.Close(); err != nil {
		t.Fatalf("closing the listener: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get("http://" + address + "/overlay/self/config")
		if err != nil {
			return
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		time.Sleep(10 * time.Millisecond)
	}

	t.Error("the connection accepted before the revocation is still being served on")
}

// A Rewrite has no way to return an error, and the one thing it must never do
// is sign an empty address: a valid signature over "" is a request the
// listener would resolve to no node at all. ServeHTTP attributes first and
// refuses when it cannot, so this is unreachable through the front door —
// which is why it is reached here through the proxy directly.
func TestTheProxyForwardsNothingWithNoAttributedPeer(t *testing.T) {
	var forwarded atomic.Int64

	upstream := listener(t, testSecret, func(w http.ResponseWriter, r *http.Request) {
		forwarded.Add(1)
	})

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parsing the upstream: %v", err)
	}

	inbound := NewInbound(target, nil, testSecret, nil)

	recorder := httptest.NewRecorder()
	inbound.proxy.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/overlay/self/config", nil))

	if recorder.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", recorder.Code)
	}
	if forwarded.Load() != 0 {
		t.Errorf("%d request(s) with no attributed peer reached the listener", forwarded.Load())
	}
}

// The peer headers are the client's to lose in the trailers as well as in the
// headers: a client may announce `Trailer: X-Overlay-Peer` and send the value
// after the body, which is the same forgery through a door nobody looks at.
func TestStripsClientSuppliedPeerTrailers(t *testing.T) {
	upstream := listener(t, testSecret, func(w http.ResponseWriter, r *http.Request) {})

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parsing the upstream: %v", err)
	}

	inbound := NewInbound(target, nil, testSecret, nil)

	in := httptest.NewRequest(http.MethodGet, "/x", nil)
	in = in.WithContext(context.WithValue(in.Context(), peerKey{}, peerAddress))

	out := in.Clone(in.Context())
	out.Trailer = http.Header{
		PeerHeader:    {"fd06:6e7:0:9::9"},
		PeerSigHeader: {"a signature of its own"},
	}

	inbound.proxy.Rewrite(&httputil.ProxyRequest{In: in, Out: out})

	if len(out.Trailer) != 0 {
		t.Errorf("the trailers forwarded upstream are %v, want none of them", out.Trailer)
	}
	if got := out.Header.Get(PeerHeader); got != peerAddress {
		t.Errorf("the header forwarded upstream names %q, want the attributed %q", got, peerAddress)
	}
}

// The other exit. A revoked grant is somebody's access being withdrawn and the
// accepted connections are the point; ctx being done is this process going
// away, and nobody's access has changed — so what is in flight gets its few
// seconds to finish rather than arriving at the node as a truncated body.
func TestServeLetsAnInFlightRequestFinishOnShutdown(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("binding: %v", err)
	}
	address := ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	served := make(chan error, 1)
	go func() {
		served <- Serve(ctx, ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(started)
			<-release
			fmt.Fprint(w, "finished")
		}))
	}()

	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)

	answered := make(chan string, 1)
	failed := make(chan error, 1)
	go func() {
		response, err := (&http.Client{Transport: transport}).Get("http://" + address + "/overlay/self/config")
		if err != nil {
			failed <- err
			return
		}
		defer response.Body.Close()

		read, err := io.ReadAll(response.Body)
		if err != nil {
			failed <- err
			return
		}
		answered <- string(read)
	}()

	select {
	case <-started:
	case err := <-failed:
		t.Fatalf("the request never reached the handler: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached the handler")
	}

	cancel()
	close(release)

	select {
	case got := <-answered:
		if got != "finished" {
			t.Errorf("the in-flight request answered %q, want the whole body", got)
		}
	case err := <-failed:
		t.Fatalf("the in-flight request was cut off by the shutdown: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight request never answered")
	}

	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve answered %v, want nil for a shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Serve never returned after ctx was done")
	}
}
