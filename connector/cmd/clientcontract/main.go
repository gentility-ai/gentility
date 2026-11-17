// Command clientcontract generates the client-owned contract from this build.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/exitcodes"
	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/tsnode"
	"github.com/gentility-ai/gentility/connector/internal/version"
)

func main() {
	out := flag.String("out", "", "absent output directory for the client contract")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "clientcontract: --out is required")
		os.Exit(1)
	}
	if err := generate(*out); err != nil {
		fmt.Fprintln(os.Stderr, "clientcontract:", err)
		os.Exit(1)
	}
}

func generate(out string) error {
	// No overwrite: a caller must choose a fresh destination.
	if err := os.Mkdir(out, 0755); err != nil {
		return err
	}
	entries := map[string]any{}
	write := func(name string, data []byte) error {
		path := filepath.Join(out, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(path, data, 0644); err != nil {
			return err
		}
		hash := sha256.Sum256(data)
		entries[name] = map[string]any{"sha256": hex.EncodeToString(hash[:]), "owner": "client", "executable": false}
		return nil
	}
	subnets, err := tsnode.ReservedSubnets()
	if err != nil {
		return err
	}
	reserved := make([]map[string]any, 0, len(subnets))
	for _, subnet := range subnets {
		reserved = append(reserved, map[string]any{"index": subnet.Index, "prefix": subnet.Prefix.String(), "source": subnet.Source})
	}
	ranges, err := json.MarshalIndent(map[string]any{
		"genscale_version": version.Genscale, "ula_range": tsnode.ULARange().String(),
		"org_prefix_bits": tsnode.OrgPrefixBits, "reserved": reserved,
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := write("tsaddr_reserved.json", append(ranges, '\n')); err != nil {
		return err
	}
	base := state.Status{
		WrittenAt: time.Date(2026, 10, 9, 11, 59, 30, 0, time.UTC), Revision: 7,
		Relayed: true, Endpoint: "derp-test", Connector: "0.2.0",
		PeerPaths: []state.PeerPath{
			{Address: "fd06:6e7:0:1::b", Path: "direct"},
			{Address: "fd06:6e7:0:1::c", Path: "derp", Relay: "derp-test"},
		},
		UDPProbe: &state.UDPProbe{
			ObservedAt: time.Date(2026, 10, 9, 11, 59, 10, 0, time.UTC),
			UDP:        true, IPv4: true, GlobalV4: "192.0.2.4:41641",
		},
		Addresses:     []string{"fd06:6e7:0:1::a"},
		Organizations: []state.OrgStatus{{Org: "acme", Address: "fd06:6e7:0:1::a", Label: "test-desktop"}},
	}
	named := base
	named.Interface = "utun4"
	named.Name = "desktop.acme.example.test"
	named.Organizations = []state.OrgStatus{{Org: "acme", Address: "fd06:6e7:0:1::a", Label: "test-desktop", Name: named.Name}}
	for name, status := range map[string]state.Status{"joined": base, "interface": named, "zero": {}} {
		// Exercise the producer, including its encoder, rather than duplicate it.
		dir, err := os.MkdirTemp("", "connector-status-contract-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		store := state.Store{Dir: dir}
		if err := store.SaveStatus(status); err != nil {
			return err
		}
		data, err := os.ReadFile(filepath.Join(dir, "status.json"))
		if err != nil {
			return err
		}
		if err := write("status/"+name+".json", data); err != nil {
			return err
		}
	}
	// These are the argument vectors the Desk relies on. Connector tests feed
	// them through the actual parser with --help before they may be pinned.
	cli, err := json.MarshalIndent(map[string]any{
		"success_exit": exitcodes.OK,
		"commands": map[string][]string{
			"install": {"install", "--interface", "--source-state-dir", "{staging}", "--place-binary", "{placed}"},
			"update":  {"install", "--interface", "--place-binary", "{placed}"},
			"leave":   {"leave"},
		},
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := write("cli.json", append(cli, '\n')); err != nil {
		return err
	}
	manifest, err := json.MarshalIndent(map[string]any{"format": 1, "manifest_owner": "client", "files": entries}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, "manifest.json"), append(manifest, '\n'), 0644)
}
