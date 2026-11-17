// Package contracttest reads the one published contract from Connector tests.
package contracttest

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// Read returns a published file, never a fallback copy from the server sources.
// A local joint-development build can be selected outside CI.
func Read(t testing.TB, name string) []byte {
	t.Helper()
	if !fs.ValidPath(name) {
		t.Fatalf("invalid contract path: %q", name)
	}
	root := os.Getenv("GENTILITY_CONTRACT_DIR")
	if root != "" && (os.Getenv("CI") != "" || os.Getenv("GITHUB_ACTIONS") != "") {
		t.Fatal("local contract overrides are forbidden in CI")
	}
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		// Tests run within the Go module; the public layout puts contract/
		// beside it. Stop at the module rather than probing server fixtures.
		for {
			_, err := os.Stat(filepath.Join(cwd, "go.mod"))
			if err == nil {
				root = filepath.Join(filepath.Dir(cwd), "contract")
				break
			}
			if !os.IsNotExist(err) {
				t.Fatal(err)
			}
			parent := filepath.Dir(cwd)
			if parent == cwd {
				t.Fatal("cannot locate Connector module for contract")
			}
			cwd = parent
		}
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("read published contract %s: %v", name, err)
	}
	return data
}
