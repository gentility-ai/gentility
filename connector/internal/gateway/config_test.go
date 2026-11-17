package gateway

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/enroll"
	"github.com/gentility-ai/gentility/connector/internal/exitcodes"
)

func environment() map[string]string {
	return map[string]string{
		enroll.VarHeadURL:      "https://head.gentility.ai",
		enroll.VarPreauthKey:   "the-preauth-key",
		enroll.VarEnrollmentID: "01920000-0000-7000-8000-000000000001",
		enroll.VarNonce:        "the-nonce",
		enroll.VarStateDir:     "/var/lib/gentility/overlay",
		enroll.VarHostname:     "gateway",
		VarUpstream:            "http://gentility-web:24081",
		VarIngressSecret:       "the-shared-secret",
		VarLeaseAddr:           "0.0.0.0:8079",
	}
}

func lookupIn(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	}
}

func TestReadEnvReadsTheGatewayEnvironment(t *testing.T) {
	cfg, err := ReadEnv(lookupIn(environment()))
	if err != nil {
		t.Fatalf("ReadEnv: %v", err)
	}

	if cfg.Upstream.String() != "http://gentility-web:24081" {
		t.Errorf("upstream = %s", cfg.Upstream)
	}
	if string(cfg.Secret) != "the-shared-secret" {
		t.Error("the ingress secret did not survive the read")
	}
	if cfg.Join.Hostname != "gateway" || cfg.Join.HeadURL != "https://head.gentility.ai" {
		t.Errorf("the join half read as %+v", cfg.Join)
	}
	if cfg.LeaseAddr.String() != "0.0.0.0:8079" {
		t.Errorf("the lease API binds on %s", cfg.LeaseAddr)
	}
}

// The lease API is the app's door on the deploy's docker network. Bound on an
// overlay address it would be a door every node could knock on, guarded by a
// secret that was never meant to travel there — so an operator who reaches for
// one is told at boot rather than after.
func TestReadEnvRefusesALeaseAddressOnTheOverlay(t *testing.T) {
	for _, raw := range []string{"[fd06:6e7:0:1::1]:8079", "[fd06:6e7::]:8079"} {
		env := environment()
		env[VarLeaseAddr] = raw

		_, err := ReadEnv(lookupIn(env))
		if err == nil {
			t.Fatalf("ReadEnv accepted %s = %q", VarLeaseAddr, raw)
		}
		if !strings.Contains(err.Error(), VarLeaseAddr) {
			t.Errorf("the refusal does not name %s: %v", VarLeaseAddr, err)
		}
		if code, fatal := exitcodes.Of(err); !fatal || code != exitcodes.Usage {
			t.Errorf("an overlay lease address exits %d (fatal %v), want %d", code, fatal, exitcodes.Usage)
		}
	}
}

// A bind wants an address and a port. A name is refused because a name that
// resolved to two addresses would be a bind that silently picked one, and a
// bare port is refused because there is no such thing as binding nowhere.
func TestReadEnvRefusesALeaseAddressThatIsNotOne(t *testing.T) {
	for _, raw := range []string{"gentility-gateway:8079", "8079", ":8079", "0.0.0.0", "[::1]"} {
		env := environment()
		env[VarLeaseAddr] = raw

		if _, err := ReadEnv(lookupIn(env)); err == nil {
			t.Errorf("ReadEnv accepted %s = %q", VarLeaseAddr, raw)
		}
	}
}

// The two the deploy and a worktree actually set.
func TestReadEnvAcceptsTheAddressesTheDeployUses(t *testing.T) {
	for _, raw := range []string{"0.0.0.0:8079", "127.0.0.1:9005"} {
		env := environment()
		env[VarLeaseAddr] = raw

		cfg, err := ReadEnv(lookupIn(env))
		if err != nil {
			t.Fatalf("ReadEnv refused %s = %q: %v", VarLeaseAddr, raw, err)
		}
		if cfg.LeaseAddr.String() != raw {
			t.Errorf("the lease API binds on %s, want %s", cfg.LeaseAddr, raw)
		}
	}
}

// The gateway is the one node with no gateway in front of it, so it is the one
// node that does not need to be told where one is.
func TestReadEnvWantsNoGatewayAddress(t *testing.T) {
	env := environment()
	delete(env, enroll.VarGatewayAddr)

	if _, err := ReadEnv(lookupIn(env)); err != nil {
		t.Errorf("ReadEnv wanted a gateway address: %v", err)
	}
}

func TestReadEnvNamesTheVariableThatIsMissing(t *testing.T) {
	for _, name := range []string{
		enroll.VarHeadURL, enroll.VarPreauthKey, enroll.VarEnrollmentID, enroll.VarNonce,
		enroll.VarStateDir, enroll.VarHostname, VarUpstream, VarIngressSecret, VarLeaseAddr,
	} {
		t.Run(name, func(t *testing.T) {
			env := environment()
			delete(env, name)

			_, err := ReadEnv(lookupIn(env))
			if err == nil {
				t.Fatalf("ReadEnv accepted an environment with no %s", name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("the refusal does not name %s: %v", name, err)
			}
			if code, fatal := exitcodes.Of(err); !fatal || code != exitcodes.Usage {
				t.Errorf("a missing %s exits %d (fatal %v), want %d", name, code, fatal, exitcodes.Usage)
			}
		})
	}
}

// An empty variable is a missing one: an exported-but-blank secret is a recipe
// that went wrong, not a gateway with no secret.
func TestReadEnvTreatsAnEmptySecretAsMissing(t *testing.T) {
	env := environment()
	env[VarIngressSecret] = ""

	if _, err := ReadEnv(lookupIn(env)); err == nil {
		t.Error("ReadEnv accepted a blank ingress secret")
	}
}

func TestReadEnvRefusesAnUpstreamThatIsNotAURL(t *testing.T) {
	for _, raw := range []string{"gentility-web:24081", "/overlay", "not a url at all"} {
		env := environment()
		env[VarUpstream] = raw

		if _, err := ReadEnv(lookupIn(env)); err == nil {
			t.Errorf("ReadEnv accepted %s = %q", VarUpstream, raw)
		}
	}
}

// A URL may carry a credential, and this variable is read on a path whose
// refusals go to stderr and into whatever collects it. So a value that carries
// userinfo is refused — the app authenticates this container by the ingress
// secret and by nothing in this URL — and neither the refusal for it nor the
// refusal for a value that is not a URL at all ever prints what was set.
func TestReadEnvRefusesAnUpstreamCarryingACredential(t *testing.T) {
	for _, raw := range []string{
		"http://gentility:hunter2@gentility-web:24081",
		"//gentility:hunter2@gentility-web",
	} {
		env := environment()
		env[VarUpstream] = raw

		_, err := ReadEnv(lookupIn(env))
		if err == nil {
			t.Fatalf("ReadEnv accepted %s with userinfo in it", VarUpstream)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("the refusal prints the credential: %v", err)
		}
		if !strings.Contains(err.Error(), VarUpstream) {
			t.Errorf("the refusal does not name %s: %v", VarUpstream, err)
		}
	}
}

// The gateway names itself in the same header, with the same MAC, that it
// names everybody else with — and strips whatever the caller put there first,
// the way it does on a forwarded request.
func TestSelfClientSignsItsOwnRequests(t *testing.T) {
	var peer, rawTs, sig string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer = r.Header.Get(PeerHeader)
		rawTs = r.Header.Get(PeerTsHeader)
		sig = r.Header.Get(PeerSigHeader)
	}))
	t.Cleanup(upstream.Close)

	self := netip.MustParseAddr("fd06:6e7:0:1::1")
	client := SelfClient(testSecret, self)

	request, err := http.NewRequest(http.MethodGet, upstream.URL+"/overlay/self/config", nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	request.Header.Set(PeerHeader, "fd06:6e7:0:9::9")

	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("requesting: %v", err)
	}
	defer response.Body.Close()

	if peer != self.String() {
		t.Errorf("the listener saw peer %q, want the gateway's own %q", peer, self)
	}

	unixTs, err := strconv.ParseInt(rawTs, 10, 64)
	if err != nil {
		t.Fatalf("the timestamp %q is not canonical decimal: %v", rawTs, err)
	}
	if want := Sign(testSecret, self.String(), unixTs); sig != want {
		t.Errorf("the signature was %q, want %q", sig, want)
	}
	if drift := time.Since(time.Unix(unixTs, 0)); drift > 30*time.Second || drift < -30*time.Second {
		t.Errorf("the timestamp is %s away from now", drift)
	}
}
