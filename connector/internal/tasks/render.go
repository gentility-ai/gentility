package tasks

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/gentility-ai/gentility/connector/internal/terminal"
)

// Task is the part of the task tool's response a
// terminal shows. The tool returns a great deal more — every timestamp, every
// resource, the whole checklist — and decoding only what is printed keeps a
// field added server-side from being a field this must learn about.
type Task struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	Status         string   `json:"status"`
	Kinds          []string `json:"kinds"`
	UncheckedCount int      `json:"unchecked_count"`
	Actionable     bool     `json:"actionable"`
	Items          []Item   `json:"items"`
}

// Item is the part of TaskHelpers.item/1 a terminal shows: which item, where
// it sits in the checklist, and — for `task check` — whether the call that
// just ran is the one that checked it.
type Item struct {
	ID        string `json:"id"`
	Text      string `json:"text"`
	Position  int    `json:"position"`
	CheckedAt string `json:"checked_at"`
}

// DecodeTask reads the `task` a task_create, task_check or task_close result
// carries.
//
// The key must be there. All three tools answer with the task they acted on,
// so a payload without one is a payload this connector did not understand —
// and quietly decoding it into a zero Task would print a blank row as though
// the server had said something.
func DecodeTask(payload map[string]any) (Task, error) {
	value, present := payload["task"]
	if !present || value == nil {
		return Task{}, errors.New(`the tool's answer carried no "task"`)
	}

	var task Task
	if err := remarshal(value, &task); err != nil {
		return Task{}, fmt.Errorf("the tool returned a task this connector cannot read: %w", err)
	}
	return task, nil
}

// DecodeTasks reads the `tasks` a task_list result carries.
//
// An empty array is an answer — the organization has no tasks matching. No
// `tasks` key at all is not: printing "No tasks." for it would report a fact
// nobody established.
func DecodeTasks(payload map[string]any) ([]Task, error) {
	value, present := payload["tasks"]
	if !present || value == nil {
		return nil, errors.New(`the tool's answer carried no "tasks"`)
	}

	var tasks []Task
	if err := remarshal(value, &tasks); err != nil {
		return nil, fmt.Errorf("the tool returned a list this connector cannot read: %w", err)
	}
	return tasks, nil
}

// remarshal re-encodes the decoded JSON the MCP client handed back and decodes
// it into a typed value. The client's contract is map[string]any — it decodes
// two layers of JSON and cannot know what any tool's payload means — so this
// is the seam where a payload becomes a type, and it is one round trip through
// encoding/json rather than a hand-written walk over `any`.
func remarshal(value any, into any) error {
	if value == nil {
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, into)
}

// RenderTasks lays a task list out as one line each: the id, the status, the
// kinds, what is outstanding, and the title.
//
// The id is printed whole. Task ids are UUIDv7, whose first 32 bits are a
// timestamp Gentility advances in 65-second buckets, so every task created in
// the same minute shares its first eight hex digits — a prefix would render a
// morning's work as one id repeated down the page, and would not name a task
// to the command a person types next.
//
// Columns are padded to the widest value present rather than to a fixed width,
// so a list of one-kind tasks does not carry a gutter sized for a list that
// has none, and a column no row fills takes no space at all.
func RenderTasks(tasks []Task) string {
	if len(tasks) == 0 {
		return "No tasks.\n"
	}

	rows := make([][5]string, len(tasks))
	for i, task := range tasks {
		rows[i] = [5]string{task.ID, task.Status, strings.Join(task.Kinds, ","), outstanding(task), task.Title}
		for column := range rows[i] {
			rows[i][column] = terminal.Text(rows[i][column])
		}
	}

	// Width is measured in bytes, which is the right measure here and only
	// here: every padded column is a uuid, a status from a fixed enum, kind
	// slugs, or a count — all ASCII. The title is the one column that can
	// carry any text a person wrote, and it is last and never padded, both
	// because trailing space at the end of a line is invisible until someone
	// copies it and because padding it would need display width, not bytes.
	var width [4]int
	for _, row := range rows {
		for column := range width {
			width[column] = max(width[column], len(row[column]))
		}
	}

	var out strings.Builder
	for _, row := range rows {
		for column := range width {
			if width[column] == 0 {
				continue
			}
			fmt.Fprintf(&out, "%-*s  ", width[column], row[column])
		}
		out.WriteString(row[4])
		out.WriteByte('\n')
	}
	return out.String()
}

// CheckedLine says which item `task check` just checked: its position in the
// checklist, how many items there are, and what it says. The task that comes
// back carries the checklist; the arguments say which item was named, by the
// same two keys CheckArgs wrote — `item_id` or `position`.
//
// A task that came back without its checklist still had an item checked, so
// the word stands alone rather than the connector inventing a position it was
// not told.
func CheckedLine(task Task, args map[string]any) string {
	item, found := checkedItem(task, args)
	if !found {
		return "checked"
	}
	return fmt.Sprintf("checked %d/%d  %s", item.Position, len(task.Items), terminal.Text(item.Text))
}

// checkedItem finds the item the arguments named in the task that came back.
func checkedItem(task Task, args map[string]any) (Item, bool) {
	id, byID := args["item_id"].(string)
	position, byPosition := args["position"].(int)

	for _, item := range task.Items {
		switch {
		case byID && item.ID == id:
			return item, true
		case byPosition && item.Position == position:
			return item, true
		}
	}
	return Item{}, false
}

// outstanding is what stands between the task and being done: how many
// checklist items are unchecked, and whether the task is waiting on something
// (`actionable` is false for an open task blocked by another or waiting on
// something outside Gentility, which is the difference between a task to pick
// up and one to leave alone).
func outstanding(task Task) string {
	var notes []string
	if task.UncheckedCount > 0 {
		notes = append(notes, fmt.Sprintf("%d left", task.UncheckedCount))
	}
	if task.Status == "open" && !task.Actionable {
		notes = append(notes, "waiting")
	}
	return strings.Join(notes, " ")
}
