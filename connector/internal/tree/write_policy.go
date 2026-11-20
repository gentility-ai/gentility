package tree

import (
	"bytes"
	"encoding/json"
	"os"
	"path"
	"strings"
)

type writePolicy struct {
	Create bool `json:"create"`
	Edit   bool `json:"edit"`
}

// Unexpected policy shapes fail closed. In particular old servers sent true,
// which cannot distinguish append-only posts from editable notes.
func decodeWritePolicy(raw json.RawMessage) (writePolicy, bool) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("false")) {
		return writePolicy{}, true
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return writePolicy{}, false
	}
	var result writePolicy
	for name, value := range fields {
		var enabled bool
		if json.Unmarshal(value, &enabled) != nil || bytes.Equal(value, []byte("null")) {
			return writePolicy{}, false
		}
		switch name {
		case "create":
			result.Create = enabled
		case "edit":
			result.Edit = enabled
		default:
			return writePolicy{}, false
		}
	}
	return result, true
}

func (m Manifest) policy(name string) writePolicy {
	parent := parentPath(name)
	// An explicitly advertised descendant always overrides inheritance.
	for _, dir := range m.Directories {
		if dir.Path == parent {
			policy, _ := decodeWritePolicy(dir.Write)
			return policy
		}
	}
	// Only new note directories inherit. Four segments includes the filename.
	for _, dir := range m.Directories {
		if notesRoot(dir.Path) && strings.HasPrefix(name, dir.Path+"/") {
			relative := strings.TrimPrefix(name, dir.Path+"/")
			if len(strings.Split(relative, "/")) <= 4 {
				policy, _ := decodeWritePolicy(dir.Write)
				return policy
			}
		}
	}
	return writePolicy{}
}

// pluginManifest names a plugin's .claude-plugin/plugin.json. Only the server
// writes one, on every layout: a plugin manifest can declare hooks and MCP
// servers, so a hand edit would hand a harness code to run.
func pluginManifest(name string) bool {
	return path.Base(name) == "plugin.json" && path.Base(path.Dir(name)) == ".claude-plugin"
}

func editable(entry Entry, manifest Manifest) bool {
	if entry.Kind == "post" || entry.Kind == "task" || path.Base(entry.Path) == "INDEX.md" || pluginManifest(entry.Path) {
		return false
	}
	return manifest.policy(entry.Path).Edit
}

func fileMode(entry Entry, manifest Manifest, principal string) os.FileMode {
	if principal == "node" {
		return 0440
	}
	if editable(entry, manifest) {
		return 0600
	}
	return 0400
}

func notesRoot(name string) bool {
	parts := strings.Split(name, "/")
	if parts[0] == "skills" { // layouts 2 to 4: skills/<org> for an org slugged notes
		return false
	}
	return len(parts) == 2 && parts[1] == "notes" || len(parts) == 3 && parts[1] == "me" && parts[2] == "notes" || len(parts) == 4 && parts[1] == "projects" && parts[3] == "notes"
}
