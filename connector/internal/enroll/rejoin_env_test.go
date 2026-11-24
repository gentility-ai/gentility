package enroll

import (
	"strings"
	"testing"

	"github.com/gentility-ai/gentility/connector/internal/exitcodes"
	"github.com/gentility-ai/gentility/connector/internal/state"
)

// rejoinEnvironment is environment() with its state directory a real one,
// holding a node.json when enrolled says so.
func rejoinEnvironment(t *testing.T, enrolled bool) map[string]string {
	t.Helper()

	vars := environment()
	vars[VarStateDir] = t.TempDir()
	if enrolled {
		store := &state.Store{Dir: vars[VarStateDir]}
		if err := store.SaveNode(state.Node{NodeID: "node-1", NodeCredential: "test-node-secret"}); err != nil {
			t.Fatal(err)
		}
	}
	return vars
}

// A first start has no node.json, so it needs all three one-shot variables,
// exactly as ReadEnv does.
func TestEnvOrRejoinWantsTheOneShotThreeOnAFirstStart(t *testing.T) {
	for _, name := range []string{VarPreauthKey, VarEnrollmentID, VarNonce} {
		vars := rejoinEnvironment(t, false)
		delete(vars, name)

		_, err := ReadEnvOrRejoin(lookupIn(vars))
		if err == nil {
			t.Fatalf("a first start with no %s was accepted", name)
		}
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("the refusal for a missing %s does not name it: %v", name, err)
		}
		if code, fatal := exitcodes.Of(err); !fatal || code != exitcodes.Usage {
			t.Fatalf("%s missing exits (%d, %v), want (%d, true)", name, code, fatal, exitcodes.Usage)
		}
	}
}

// A restart has its node.json, so it starts with none of the three.
func TestEnvOrRejoinRestartsWithNoneOfTheOneShotThree(t *testing.T) {
	vars := rejoinEnvironment(t, true)
	for _, name := range []string{VarPreauthKey, VarEnrollmentID, VarNonce} {
		delete(vars, name)
	}

	env, err := ReadEnvOrRejoin(lookupIn(vars))
	if err != nil {
		t.Fatalf("a restart from its own node.json was refused: %v", err)
	}
	if env.GatewayAddr.String() != "[fd06:6e7:0:1::1]:443" {
		t.Fatalf("GatewayAddr = %s, want the hop the environment names", env.GatewayAddr)
	}
}

// A unit that still carries the spent three ignores them once node.json is
// there. An empty auth key is what arms tsnode.Up's rejoin refusal, so a node
// the head has forgotten exits EnrollRefused instead of waiting for a login.
func TestEnvOrRejoinIgnoresTheSpentThreeOnARestart(t *testing.T) {
	env, err := ReadEnvOrRejoin(lookupIn(rejoinEnvironment(t, true)))
	if err != nil {
		t.Fatalf("ReadEnvOrRejoin: %v", err)
	}
	if env.PreauthKey != "" || env.EnrollmentID != "" || env.Nonce != "" {
		t.Fatalf("a restart read the spent one-shot variables (key set %t, enrollment %q, nonce set %t)",
			env.PreauthKey != "", env.EnrollmentID, env.Nonce != "")
	}
}

// The hop is read on a restart as on a first start: rejoining is about the
// three, not about where Gentility answers.
func TestEnvOrRejoinStillWantsTheGatewayHop(t *testing.T) {
	vars := rejoinEnvironment(t, true)
	delete(vars, VarGatewayAddr)

	if _, err := ReadEnvOrRejoin(lookupIn(vars)); err == nil || !strings.Contains(err.Error(), VarGatewayAddr) {
		t.Fatalf("a restart with no %s gave %v, want a refusal naming it", VarGatewayAddr, err)
	}
}
