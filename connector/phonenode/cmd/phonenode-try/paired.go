package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const pairedFile = "phone.json"

// paired is what claim earned: the bearer the phone will hold in its
// keychain and the join material, until join spends it. Every field but
// origin and phone_name is a secret, and this file is their only home.
type paired struct {
	Origin      string          `json:"origin"`
	PhoneName   string          `json:"phone_name"`
	AccessToken string          `json:"access_token"`
	TokenType   string          `json:"token_type"`
	ExpiresIn   int             `json:"expires_in"`
	Scope       string          `json:"scope"`
	Enrollment  json.RawMessage `json:"enrollment,omitempty"`
}

// nodeDir is where the node keeps its identity, as the app's Application
// Support directory will.
func nodeDir(dir string) string { return filepath.Join(dir, "node") }

func readPaired(dir string) (paired, error) {
	data, err := os.ReadFile(filepath.Join(dir, pairedFile))
	if errors.Is(err, os.ErrNotExist) {
		return paired{}, fmt.Errorf("%s holds no %s: run claim first", dir, pairedFile)
	}
	if err != nil {
		return paired{}, err
	}
	var p paired
	if err := json.Unmarshal(data, &p); err != nil {
		return paired{}, fmt.Errorf("%s is not a pairing", pairedFile)
	}
	return p, nil
}

// stateDir makes dir 0700 if it is new, and refuses one that group or
// others can enter, because it holds every secret claim earned. A directory
// that is there already is never changed: it is the person's.
func stateDir(dir string) error {
	info, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return os.MkdirAll(dir, 0o700)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s is %v, open to group or others: chmod 700 it, or name a new directory", dir, perm)
	}
	return nil
}

// writePaired replaces the file whole: 0600 from creation (os.CreateTemp),
// renamed into a directory stateDir has passed.
func writePaired(dir string, p paired) error {
	if err := stateDir(dir); err != nil {
		return err
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+pairedFile+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, pairedFile))
}
