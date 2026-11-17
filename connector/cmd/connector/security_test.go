package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestTaskTerminalOutputRefusesControls(t *testing.T) {
	const taskID = "11111111-1111-1111-1111-111111111111"
	poison := "Review\x1b]52;c;clipboard\x07\r\n\u009btext"
	for _, verb := range []string{"add", "list", "check", "close", "refusal", "non-json"} {
		t.Run(verb, func(t *testing.T) {
			srv, _ := mcpServer(t, "test", func(t *testing.T, c toolCall) string {
				if verb == "non-json" {
					return poison
				}
				task := map[string]any{"id": taskID, "title": poison, "status": "open", "items": []any{map[string]any{"id": "item", "position": 1, "text": poison}}}
				payload := map[string]any{"success": true, "task": task, "tasks": []any{task}}
				if verb == "refusal" {
					payload = map[string]any{"success": false, "error": poison}
				}
				data, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				return string(data)
			})
			signIn(t, srv.URL, "test")
			args := map[string][]string{
				"add":      {"task", "add", "Review", "--kind", "eng", "--org", "acme"},
				"list":     {"task", "list", "--org", "acme"},
				"check":    {"task", "check", taskID, "1", "--cause", "checked", "--org", "acme"},
				"close":    {"task", "close", taskID, "--cause", "done", "--org", "acme"},
				"refusal":  {"task", "list", "--org", "acme"},
				"non-json": {"task", "list", "--org", "acme"},
			}[verb]
			var out bytes.Buffer
			code := run(args, &out, &out)
			want := 0
			if verb == "refusal" || verb == "non-json" {
				want = 1
			}
			if code != want {
				t.Fatalf("exit %d: %s", code, out.String())
			}
			for _, r := range out.String() {
				if r < 32 && r != '\n' || r >= 127 && r <= 159 {
					t.Fatalf("control U+%04X in %q", r, out.String())
				}
			}
			if !strings.Contains(out.String(), "Review") {
				t.Fatalf("missing text: %q", out.String())
			}
			t.Logf("CLI output:\n%s", out.String())
		})
	}
}

func TestTaskJSONEscapesTerminalControls(t *testing.T) {
	var poison strings.Builder
	for r := rune(0); r <= 159; r++ {
		poison.WriteRune(r)
	}
	srv, _ := mcpServer(t, "test", func(t *testing.T, c toolCall) string {
		data, err := json.Marshal(map[string]any{"success": true, "text": poison.String()})
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	})
	signIn(t, srv.URL, "test")
	var out, stderr bytes.Buffer
	if code := run([]string{"task", "list", "--json", "--org", "acme"}, &out, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	for _, r := range out.String() {
		if r < 32 && r != '\n' || r >= 127 && r <= 159 {
			t.Fatalf("raw control U+%04X in JSON", r)
		}
	}
	var decoded map[string]any
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["text"] != poison.String() {
		t.Fatal("JSON changed the decoded value")
	}
}
