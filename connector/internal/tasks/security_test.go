package tasks

import (
	"strings"
	"testing"
)

func TestTerminalFieldsRefuseControls(t *testing.T) {
	poison := "before\x1b]52;c;clipboard\x07\r\n\t\x7f\u0085\u009bafter"
	task := Task{ID: poison, Status: poison, Kinds: []string{poison}, Title: poison,
		Items: []Item{{ID: "item", Position: 1, Text: poison}}}
	for _, output := range []string{RenderTasks([]Task{task}), CheckedLine(task, map[string]any{"position": 1})} {
		for _, r := range strings.TrimSuffix(output, "\n") {
			if r < 32 || r >= 127 && r <= 159 {
				t.Fatalf("terminal control U+%04X in %q", r, output)
			}
		}
		if !strings.Contains(output, "before") || !strings.Contains(output, "after") {
			t.Fatalf("lost printable text: %q", output)
		}
	}
}
