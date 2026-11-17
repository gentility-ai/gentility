package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestGeneratedClientContractIsDeterministicAndInventoried(t *testing.T) {
	root := t.TempDir()
	one, two := filepath.Join(root, "one"), filepath.Join(root, "two")
	for _, out := range []string{one, two} {
		if err := generate(out); err != nil {
			t.Fatal(err)
		}
	}
	read := func(root, name string) []byte {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	first := read(one, "manifest.json")
	if !bytes.Equal(first, read(two, "manifest.json")) {
		t.Fatal("client manifest is not deterministic")
	}
	var manifest struct {
		Files map[string]struct {
			SHA256 string `json:"sha256"`
			Owner  string `json:"owner"`
		} `json:"files"`
	}
	if err := json.Unmarshal(first, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 5 {
		t.Fatal("client contract inventory changed; review its readers")
	}
	for name, entry := range manifest.Files {
		data := read(one, name)
		hash := sha256.Sum256(data)
		if entry.Owner != "client" || entry.SHA256 != hex.EncodeToString(hash[:]) || !bytes.Equal(data, read(two, name)) {
			t.Errorf("client contract differs or is not inventoried: %s", name)
		}
	}
	if err := generate(one); err == nil {
		t.Fatal("generation overwrote an existing directory")
	}
}
