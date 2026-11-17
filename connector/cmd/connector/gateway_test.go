package main

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/gentility-ai/gentility/connector/internal/enroll"
	"github.com/gentility-ai/gentility/connector/internal/exitcodes"
	"github.com/gentility-ai/gentility/connector/internal/gateway"
	"github.com/gentility-ai/gentility/connector/internal/state"
)

func TestUsageMentionsTheGateway(t *testing.T) {
	var out bytes.Buffer
	usage(&out)

	if !strings.Contains(out.String(), "gateway") {
		t.Fatalf("usage does not mention the gateway:\n%s", out.String())
	}
}

// Like `join`, the gateway refuses before it touches a head: a missing
// variable will be just as missing after a restart, so it exits Usage rather
// than Temporary and a supervisor does not restart into it.
func TestRunGatewayWithoutItsEnvironmentIsAUsageError(t *testing.T) {
	t.Setenv(enroll.VarHeadURL, "")

	var out bytes.Buffer
	if code := run([]string{"gateway"}, &out, &out); code != exitcodes.Usage {
		t.Fatalf("run(gateway) = %d, want %d:\n%s", code, exitcodes.Usage, out.String())
	}
	if !strings.Contains(out.String(), enroll.VarHeadURL) {
		t.Fatalf("the refusal does not name the missing variable:\n%s", out.String())
	}
}

func TestRunGatewayWithoutAnIngressSecretIsAUsageError(t *testing.T) {
	t.Setenv(enroll.VarHeadURL, "https://head.gentility.ai")
	t.Setenv(enroll.VarPreauthKey, "the-preauth-key")
	t.Setenv(enroll.VarEnrollmentID, "01920000-0000-7000-8000-000000000001")
	t.Setenv(enroll.VarNonce, "the-nonce")
	t.Setenv(enroll.VarStateDir, t.TempDir())
	t.Setenv(enroll.VarHostname, "gateway")
	t.Setenv(gateway.VarUpstream, "http://gentility-web:24081")
	t.Setenv(gateway.VarIngressSecret, "")

	var out bytes.Buffer
	if code := run([]string{"gateway"}, &out, &out); code != exitcodes.Usage {
		t.Fatalf("run(gateway) = %d, want %d:\n%s", code, exitcodes.Usage, out.String())
	}
	if !strings.Contains(out.String(), gateway.VarIngressSecret) {
		t.Fatalf("the refusal does not name the missing variable:\n%s", out.String())
	}
}

// The gateway runs two doors, and the outbound one has to be told where to
// bind. A gateway with no lease API is a deployment the app has no way onto
// the overlay through, and that is an operator's mistake rather than something
// to start up quietly without.
func TestRunGatewayWithoutALeaseAddressIsAUsageError(t *testing.T) {
	t.Setenv(enroll.VarHeadURL, "https://head.gentility.ai")
	t.Setenv(enroll.VarPreauthKey, "the-preauth-key")
	t.Setenv(enroll.VarEnrollmentID, "01920000-0000-7000-8000-000000000001")
	t.Setenv(enroll.VarNonce, "the-nonce")
	t.Setenv(enroll.VarStateDir, t.TempDir())
	t.Setenv(enroll.VarHostname, "gateway")
	t.Setenv(gateway.VarUpstream, "http://gentility-web:24081")
	t.Setenv(gateway.VarIngressSecret, "the-shared-secret")
	t.Setenv(gateway.VarLeaseAddr, "")

	var out bytes.Buffer
	if code := run([]string{"gateway"}, &out, &out); code != exitcodes.Usage {
		t.Fatalf("run(gateway) = %d, want %d:\n%s", code, exitcodes.Usage, out.String())
	}
	if !strings.Contains(out.String(), gateway.VarLeaseAddr) {
		t.Fatalf("the refusal does not name the missing variable:\n%s", out.String())
	}
}

func TestRunGatewayTakesNoArguments(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"gateway", "somewhere"}, &out, &out); code != exitcodes.Usage {
		t.Fatalf("run(gateway somewhere) = %d, want %d", code, exitcodes.Usage)
	}
	if !strings.Contains(out.String(), "somewhere") {
		t.Fatalf("output %q does not name the stray argument", out.String())
	}
}

func TestRunGatewayAnswersHelpBeforeItReadsTheEnvironment(t *testing.T) {
	t.Setenv(enroll.VarHeadURL, "")

	for _, ask := range []string{"--help", "-h"} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"gateway", ask}, &stdout, &stderr); code != 0 {
			t.Fatalf("run(gateway %s) = %d, want 0:\n%s%s", ask, code, stdout.String(), stderr.String())
		}
		if !strings.HasPrefix(stdout.String(), "usage: "+invoked+" gateway") {
			t.Fatalf("run(gateway %s) printed:\n%s", ask, stdout.String())
		}
	}
}

// A 401 from the cert endpoint is the two containers holding different
// secrets, which no restart fixes. Everything else — the app not up yet, a
// refusal about the addresses — is retryable, and the supervisor restarting is
// the retry.
func TestFirstCertFailureIsFatalOnlyOnA401(t *testing.T) {
	fatal := firstCertFailure(&gateway.RefusedError{StatusCode: http.StatusUnauthorized, Reason: "invalid_bearer"})

	code, isFatal := exitcodes.Of(fatal)
	if !isFatal || code != exitcodes.Usage {
		t.Errorf("a 401 exits %d (fatal %v), want %d", code, isFatal, exitcodes.Usage)
	}
	if !strings.Contains(fatal.Error(), gateway.VarIngressSecret) {
		t.Errorf("the refusal does not name the variable to fix: %v", fatal)
	}

	for _, retryable := range []error{
		&gateway.RefusedError{StatusCode: http.StatusUnprocessableEntity, Reason: "invalid_address"},
		&gateway.RefusedError{StatusCode: http.StatusServiceUnavailable},
		errors.New("dial tcp: connection refused"),
	} {
		if _, isFatal := exitcodes.Of(firstCertFailure(retryable)); isFatal {
			t.Errorf("%v was treated as fatal", retryable)
		}
	}
}

// The state volume is the identity, and the three one-shot variables buy only
// the FIRST one. The pre-auth key expires in ten minutes, the nonce is spent by
// the exchange and its material is purged an hour later — so a gateway
// container re-created without them has to come back on the node in its volume,
// exactly as `serve` does after a reboot.
//
// It did not, once. A staging gateway re-created with the spent secrets no
// longer in its shell crash-looped on "OVERLAY_PREAUTH_KEY is not set" with its
// own identity sitting in the volume beside it, and every node it names was cut
// off for as long as that lasted.
//
// What the run reaches here is the join, which refuses because this state
// directory holds a node.json and no tailscale node key — the step AFTER the
// environment, and so the proof the environment was read.
func TestRunGatewayWithAStoredNodeDoesNotNeedTheOneShotMaterial(t *testing.T) {
	dir := t.TempDir()
	store := &state.Store{Dir: dir}
	if err := store.SaveNode(state.Node{
		NodeID:         "01920000-0000-7000-8000-0000000000ff",
		NodeCredential: "the-node-credential",
		HeadURL:        "https://head.gentility.ai",
		Hostname:       "gateway",
		LastRevision:   state.NoRevision,
	}); err != nil {
		t.Fatalf("seeding the state directory: %v", err)
	}

	gatewayEnvironment(t, dir)

	var out bytes.Buffer
	code := run([]string{"gateway"}, &out, &out)

	for _, spent := range []string{enroll.VarPreauthKey, enroll.VarEnrollmentID, enroll.VarNonce} {
		if strings.Contains(out.String(), spent+" is not set") {
			t.Fatalf("the gateway demanded %s, which it has an identity instead of:\n%s", spent, out.String())
		}
	}
	if !strings.Contains(out.String(), "a rejoin needs the state directory") {
		t.Fatalf("run(gateway) = %d and did not reach the join:\n%s", code, out.String())
	}
}

// The other half of the same rule: with no node in the state directory there is
// nothing to come back as, and the one-shot trio is the only way in. A gateway
// that started quietly without it would join nothing and say nothing about why.
func TestRunGatewayWithAnEmptyStateDirectoryStillNeedsThePreauthKey(t *testing.T) {
	gatewayEnvironment(t, t.TempDir())

	var out bytes.Buffer
	if code := run([]string{"gateway"}, &out, &out); code != exitcodes.Usage {
		t.Fatalf("run(gateway) = %d, want %d:\n%s", code, exitcodes.Usage, out.String())
	}
	if !strings.Contains(out.String(), enroll.VarPreauthKey+" is not set") {
		t.Fatalf("the refusal does not name the missing variable:\n%s", out.String())
	}
}

// gatewayEnvironment is a re-created container's: everything the accessory sets
// on every start, and none of the one-shot material a first boot was handed.
// The empty values are deliberate — they shield the test from an ambient
// OVERLAY_* the way an exported-but-blank variable is shielded from meaning
// something in production.
func gatewayEnvironment(t *testing.T, stateDir string) {
	t.Helper()

	t.Setenv(enroll.VarHeadURL, "https://head.gentility.ai")
	t.Setenv(enroll.VarPreauthKey, "")
	t.Setenv(enroll.VarEnrollmentID, "")
	t.Setenv(enroll.VarNonce, "")
	t.Setenv(enroll.VarStateDir, stateDir)
	t.Setenv(enroll.VarHostname, "gateway")
	t.Setenv(gateway.VarUpstream, "http://gentility-web:24081")
	t.Setenv(gateway.VarIngressSecret, "the-shared-secret")
	t.Setenv(gateway.VarLeaseAddr, "0.0.0.0:8079")
}
