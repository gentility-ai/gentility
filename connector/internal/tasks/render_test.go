package tasks

import (
	"strings"
	"testing"
)

func TestRenderTasksLaysOutOneLineEach(t *testing.T) {
	got := RenderTasks([]Task{
		{
			ID: "11111111-1111-1111-1111-111111111111", Title: "Rotate the WorkOS key",
			Status: "open", Kinds: []string{"ops", "security"}, UncheckedCount: 2, Actionable: true,
		},
		{
			ID: "22222222-2222-2222-2222-222222222222", Title: "Ship the overlay",
			Status: "open", Kinds: []string{"eng"}, Actionable: false,
		},
		{
			ID: "33333333-3333-3333-3333-333333333333", Title: "Old thing",
			Status: "done",
		},
	})

	want := `11111111-1111-1111-1111-111111111111  open  ops,security  2 left   Rotate the WorkOS key
22222222-2222-2222-2222-222222222222  open  eng           waiting  Ship the overlay
33333333-3333-3333-3333-333333333333  done                         Old thing
`
	if got != want {
		t.Fatalf("RenderTasks =\n%s\nwant\n%s", got, want)
	}
}

// Task ids are UUIDv7: the first 32 bits are a timestamp, and Gentility mints
// them in 65-second buckets, so every task created in the same minute shares
// its first eight hex digits. Printing a prefix would render a whole minute's
// work as one repeated id.
func TestRenderTasksPrintsIdsThatDifferAfterTheTimestampPrefix(t *testing.T) {
	const (
		first  = "0198f2a0-7c31-7a1e-8f00-000000000001"
		second = "0198f2a0-7c31-7a1e-8f00-000000000002"
	)

	got := RenderTasks([]Task{
		{ID: first, Title: "One", Status: "open"},
		{ID: second, Title: "Two", Status: "open"},
	})

	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("RenderTasks printed %d lines:\n%s", len(lines), got)
	}
	if !strings.HasPrefix(lines[0], first) || !strings.HasPrefix(lines[1], second) {
		t.Fatalf("two ids sharing a timestamp prefix did not render distinctly:\n%s", got)
	}
}

// The id a row prints is the id the next command takes: `task check` and
// `task close` both run it through parseTaskID, and a rendered id that does
// not survive that round trip is a row nobody can act on.
func TestRenderTasksPrintsAnIdTheNextCommandAccepts(t *testing.T) {
	const id = "0198f2a0-7c31-7a1e-8f00-000000000001"

	row := RenderTasks([]Task{{ID: id, Title: "One", Status: "open"}})
	printed := strings.Fields(row)[0]

	got, err := parseTaskID(printed)
	if err != nil {
		t.Fatalf("parseTaskID(%q): %v", printed, err)
	}
	if got != id {
		t.Fatalf("parseTaskID(%q) = %q, want %q", printed, got, id)
	}
}

// A column no row fills is a column that is not there: padding it leaves a
// gutter of spaces between two values that are meant to be adjacent.
func TestRenderTasksLeavesNoGutterForAnEmptyColumn(t *testing.T) {
	got := RenderTasks([]Task{
		{ID: "11111111-1111-1111-1111-111111111111", Title: "One", Status: "done", Kinds: []string{"ops"}},
		{ID: "22222222-2222-2222-2222-222222222222", Title: "Two", Status: "done", Kinds: []string{"ops"}},
	})

	want := `11111111-1111-1111-1111-111111111111  done  ops  One
22222222-2222-2222-2222-222222222222  done  ops  Two
`
	if got != want {
		t.Fatalf("RenderTasks =\n%q\nwant\n%q", got, want)
	}
}

func TestRenderTasksSaysSoWhenThereAreNone(t *testing.T) {
	if got := RenderTasks(nil); got != "No tasks.\n" {
		t.Fatalf("RenderTasks(nil) = %q", got)
	}
}

func TestDecodeTasksReadsTheToolPayload(t *testing.T) {
	payload := map[string]any{"tasks": []any{
		map[string]any{
			"id": "11111111-1111-1111-1111-111111111111", "title": "Rotate the WorkOS key",
			"status": "open", "kinds": []any{"ops"}, "unchecked_count": float64(2), "actionable": true,
			// Fields this connector does not print must not stop it reading
			// the ones it does.
			"inserted_at": "2026-08-28T00:00:00Z", "resources": []any{},
		},
	}}

	tasks, err := DecodeTasks(payload)
	if err != nil {
		t.Fatalf("DecodeTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("decoded %d tasks, want 1", len(tasks))
	}
	if tasks[0].UncheckedCount != 2 || tasks[0].Kinds[0] != "ops" || !tasks[0].Actionable {
		t.Fatalf("decoded %#v", tasks[0])
	}
}

func TestDecodeTasksAcceptsAnEmptyList(t *testing.T) {
	got, err := DecodeTasks(map[string]any{"tasks": []any{}})
	if err != nil {
		t.Fatalf("DecodeTasks: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("decoded %#v, want none", got)
	}
}

// An answer with no `tasks` at all is not an empty list: it is a payload this
// connector did not understand, and printing "No tasks." for it would report
// a fact nobody established.
func TestDecodeTasksRefusesAPayloadWithNoTasksKey(t *testing.T) {
	for _, payload := range []map[string]any{
		{"success": true},
		{"success": true, "tasks": nil},
	} {
		_, err := DecodeTasks(payload)
		if err == nil {
			t.Fatalf("no error for %#v", payload)
		}
		if !strings.Contains(err.Error(), "tasks") {
			t.Fatalf("error %q does not name the key it wanted", err)
		}
	}
}

func TestDecodeTaskReadsTheToolPayload(t *testing.T) {
	task, err := DecodeTask(map[string]any{"task": map[string]any{
		"id": "t1", "title": "Thing", "status": "open",
	}})
	if err != nil {
		t.Fatalf("DecodeTask: %v", err)
	}
	if task.ID != "t1" || task.Title != "Thing" {
		t.Fatalf("decoded %#v", task)
	}
}

func TestDecodeTaskReadsTheChecklist(t *testing.T) {
	task, err := DecodeTask(map[string]any{"task": map[string]any{
		"id": "t1", "title": "Thing", "status": "open",
		"items": []any{
			map[string]any{
				"id": "22222222-2222-2222-2222-222222222222", "text": "Rotate the key",
				"position": float64(1), "checked_at": "2026-08-28T00:00:00Z",
				"body": nil, "checked_by_looper_id": nil,
			},
			map[string]any{
				"id": "33333333-3333-3333-3333-333333333333", "text": "Tell the team",
				"position": float64(2), "checked_at": nil,
			},
		},
	}})
	if err != nil {
		t.Fatalf("DecodeTask: %v", err)
	}
	if len(task.Items) != 2 {
		t.Fatalf("decoded %d items, want 2", len(task.Items))
	}
	if task.Items[0].Text != "Rotate the key" || task.Items[0].Position != 1 || task.Items[0].CheckedAt == "" {
		t.Fatalf("decoded %#v", task.Items[0])
	}
	if task.Items[1].CheckedAt != "" {
		t.Fatalf("an unchecked item carried a checked_at: %#v", task.Items[1])
	}
}

func TestDecodeTaskRefusesAPayloadItCannotRead(t *testing.T) {
	if _, err := DecodeTask(map[string]any{"task": "not a task"}); err == nil {
		t.Fatal("no error for a task that is not an object")
	}
}

// task_create, task_check and task_close all answer with the task they acted
// on. An answer without one is not a task to print.
func TestDecodeTaskRefusesAPayloadWithNoTaskKey(t *testing.T) {
	for _, payload := range []map[string]any{
		{"success": true},
		{"success": true, "task": nil},
	} {
		_, err := DecodeTask(payload)
		if err == nil {
			t.Fatalf("no error for %#v", payload)
		}
		if !strings.Contains(err.Error(), "task") {
			t.Fatalf("error %q does not name the key it wanted", err)
		}
	}
}

func TestCheckedLineNamesTheItemByPosition(t *testing.T) {
	task := Task{ID: "t1", Items: []Item{
		{ID: "a", Text: "Rotate the key", Position: 1},
		{ID: "b", Text: "Tell the team", Position: 2},
		{ID: "c", Text: "Close the ticket", Position: 3},
	}}

	got := CheckedLine(task, map[string]any{"position": 2})
	if got != "checked 2/3  Tell the team" {
		t.Fatalf("CheckedLine = %q", got)
	}
}

func TestCheckedLineNamesTheItemByID(t *testing.T) {
	task := Task{ID: "t1", Items: []Item{
		{ID: "a", Text: "Rotate the key", Position: 1},
		{ID: "b", Text: "Tell the team", Position: 2},
	}}

	got := CheckedLine(task, map[string]any{"item_id": "a"})
	if got != "checked 1/2  Rotate the key" {
		t.Fatalf("CheckedLine = %q", got)
	}
}

// A task that came back without its checklist still had an item checked; the
// connector just cannot say which, and inventing a position would be worse
// than saying less.
func TestCheckedLineSaysOnlyWhatItKnows(t *testing.T) {
	if got := CheckedLine(Task{ID: "t1"}, map[string]any{"position": 2}); got != "checked" {
		t.Fatalf("CheckedLine = %q", got)
	}
	if got := CheckedLine(Task{ID: "t1", Items: []Item{{ID: "a", Position: 1}}}, map[string]any{"position": 9}); got != "checked" {
		t.Fatalf("CheckedLine for an item the task does not carry = %q", got)
	}
}
