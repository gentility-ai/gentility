package auth

import (
	"encoding/json"
	"testing"

	"github.com/gentility-ai/gentility/connector/internal/contracttest"
	"github.com/gentility-ai/gentility/connector/internal/mcp"
	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/version"
)

func TestLoginOriginContract(t *testing.T) {
	data := contracttest.Read(t, "connector/login_origins.json")
	var fixture struct {
		Environments  map[string]string
		Normalization []struct {
			Input       string
			Origin      string
			Development bool
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	origins := fixture.Environments
	original := version.Connector
	t.Cleanup(func() { version.Connector = original })
	for _, row := range fixture.Normalization {
		version.Connector = "release"
		if row.Development {
			if _, err := mcp.NormalizeBaseURL(row.Input); err == nil {
				t.Fatalf("release accepted development origin %q", row.Input)
			}
			version.Connector = "dev"
		}
		got, err := mcp.NormalizeBaseURL(row.Input)
		if err != nil || got != row.Origin {
			t.Fatalf("normalization drift: %q %v", got, err)
		}
	}
	version.Connector = original
	t.Setenv(EnvBaseURL, "")
	store := &state.Store{Dir: t.TempDir()}
	origin, err := ResolveOrigin("", store)
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := mcp.NormalizeBaseURL(DefaultBaseURL)
	if err != nil || normalized != origins["production"] || origin.URL != normalized {
		t.Fatalf("production drift: %v %v", origin, err)
	}
	t.Setenv(EnvBaseURL, origins["staging"])
	origin, err = ResolveOrigin("", store)
	if err != nil || origin.URL != origins["staging"] {
		t.Fatalf("staging drift: %v %v", origin, err)
	}
}
