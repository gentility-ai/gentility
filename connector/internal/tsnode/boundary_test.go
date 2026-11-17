package tsnode

import (
	"bytes"
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"testing"
)

// Tailscale stays behind tsnode. The one fixture package shares that boundary,
// but only test files may import it; it must never become a production shortcut.
func TestOnlyTsnodeImportsTailscale(t *testing.T) {
	const (
		module  = "github.com/gentility-ai/gentility/connector"
		self    = module + "/internal/tsnode"
		fixture = self + "/tsnodetest"
	)
	out, err := exec.Command("go", "list", "-json", module+"/...").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	decoder := json.NewDecoder(bytes.NewReader(out))
	for {
		var pkg struct {
			ImportPath                         string
			Imports, TestImports, XTestImports []string
		}
		if err := decoder.Decode(&pkg); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("go list JSON: %v", err)
		}
		for _, imp := range pkg.Imports {
			if imp == fixture {
				t.Errorf("%s imports %s from a non-test file; only _test.go files may import the fixture", pkg.ImportPath, fixture)
			}
		}
		if pkg.ImportPath == self || pkg.ImportPath == fixture {
			continue
		}
		imports := append(append(pkg.Imports, pkg.TestImports...), pkg.XTestImports...)
		for _, imp := range imports {
			if strings.HasPrefix(imp, "tailscale.com/") || strings.HasPrefix(imp, "gvisor.dev/") {
				t.Errorf("%s imports %s directly; reach the overlay through %s (tests may use %s)", pkg.ImportPath, imp, self, fixture)
			}
		}
	}
}
