package tasks

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// argCase is one invocation and what it must map onto: either the exact
// arguments a tools/call would carry, or a fragment the refusal must name.
type argCase struct {
	name    string
	argv    []string
	org     string
	want    map[string]any
	wantErr string
}

// check runs one case against the mapper under test.
func (c argCase) check(t *testing.T, got map[string]any, err error) {
	t.Helper()

	if c.wantErr != "" {
		if err == nil {
			t.Fatalf("no error, want one naming %q; got %#v", c.wantErr, got)
		}
		if !strings.Contains(err.Error(), c.wantErr) {
			t.Fatalf("error %q does not name %q", err, c.wantErr)
		}
		return
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(got, c.want) {
		t.Fatalf("arguments =\n  %#v\nwant\n  %#v", got, c.want)
	}
}

func TestAddArgs(t *testing.T) {
	cases := []argCase{
		{
			name: "title and one kind",
			argv: []string{"Rotate the WorkOS key", "--kind", "ops"},
			org:  "acme",
			want: map[string]any{
				"organization": "acme",
				"title":        "Rotate the WorkOS key",
				"kinds":        []string{"ops"},
			},
		},
		{
			name: "repeated --kind accumulates in order",
			argv: []string{"Thing", "--kind", "ops", "--kind", "security"},
			org:  "acme",
			want: map[string]any{
				"organization": "acme", "title": "Thing",
				"kinds": []string{"ops", "security"},
			},
		},
		{
			name: "body, for and dedup-key ride along under their schema names",
			argv: []string{"Thing", "--kind", "ops", "--body", "why", "--for", "user:user@example.com", "--dedup-key", "cli:1"},
			org:  "acme",
			want: map[string]any{
				"organization": "acme", "title": "Thing", "kinds": []string{"ops"},
				"body": "why", "for": "user:user@example.com", "dedup_key": "cli:1",
			},
		},
		{
			name: "--org outranks the remembered organization",
			argv: []string{"Thing", "--kind", "ops", "--org", "other"},
			org:  "acme",
			want: map[string]any{
				"organization": "other", "title": "Thing", "kinds": []string{"ops"},
			},
		},
		{
			name: "flags may come before the title",
			argv: []string{"--kind", "ops", "Thing"},
			org:  "acme",
			want: map[string]any{
				"organization": "acme", "title": "Thing", "kinds": []string{"ops"},
			},
		},
		{
			name:    "no kind is refused locally, with the tool that lists them",
			argv:    []string{"Thing"},
			org:     "acme",
			wantErr: "task_kind_list",
		},
		{
			name:    "no org is refused before any network call",
			argv:    []string{"Thing", "--kind", "ops"},
			org:     "",
			wantErr: "--org",
		},
		{
			name:    "an empty title is refused",
			argv:    []string{"", "--kind", "ops"},
			org:     "acme",
			wantErr: "title",
		},
		{
			name:    "a missing title is refused",
			argv:    []string{"--kind", "ops"},
			org:     "acme",
			wantErr: "title",
		},
		{
			name:    "an unquoted title is refused rather than truncated",
			argv:    []string{"Rotate", "the", "key", "--kind", "ops"},
			org:     "acme",
			wantErr: "quote it",
		},
		{
			name:    "an unknown flag is refused",
			argv:    []string{"Thing", "--kind", "ops", "--priority", "high"},
			org:     "acme",
			wantErr: "priority",
		},
		{
			name:    "an --org that swallowed a flag is refused",
			argv:    []string{"Thing", "--kind", "ops", "--org", "--json"},
			org:     "acme",
			wantErr: "never starts with a dash",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := AddArgs(c.argv, c.org)
			c.check(t, got, err)
		})
	}
}

func TestListArgs(t *testing.T) {
	cases := []argCase{
		{
			name: "no filters is the organization alone — the tool defaults to open tasks itself",
			argv: nil,
			org:  "acme",
			want: map[string]any{"organization": "acme"},
		},
		{
			name: "--mine is the only value the for enum takes",
			argv: []string{"--mine"},
			org:  "acme",
			want: map[string]any{"organization": "acme", "for": "me"},
		},
		{
			name: "--status rides along under its schema name",
			argv: []string{"--status", "done"},
			org:  "acme",
			want: map[string]any{"organization": "acme", "status": "done"},
		},
		{
			name: "--status all with a limit",
			argv: []string{"--status", "all", "--limit", "100"},
			org:  "acme",
			want: map[string]any{"organization": "acme", "status": "all", "limit": 100},
		},
		{
			name: "--mine and --status together",
			argv: []string{"--mine", "--status", "open"},
			org:  "acme",
			want: map[string]any{"organization": "acme", "for": "me", "status": "open"},
		},
		{
			name: "--json is an output choice, not an argument",
			argv: []string{"--mine", "--json"},
			org:  "acme",
			want: map[string]any{"organization": "acme", "for": "me"},
		},
		{
			name:    "an unknown status names the ones that exist",
			argv:    []string{"--status", "weird"},
			org:     "acme",
			wantErr: "open, done, dismissed, superseded, all",
		},
		{
			name:    "a limit below the schema's minimum is refused",
			argv:    []string{"--limit", "0"},
			org:     "acme",
			wantErr: "1-100",
		},
		{
			name:    "a limit above the schema's maximum is refused",
			argv:    []string{"--limit", "101"},
			org:     "acme",
			wantErr: "1-100",
		},
		{
			name:    "no org is refused before any network call",
			argv:    []string{"--mine"},
			org:     "",
			wantErr: "--org",
		},
		{
			name:    "a stray word is refused rather than ignored",
			argv:    []string{"ops"},
			org:     "acme",
			wantErr: "unexpected argument",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ListArgs(c.argv, c.org)
			c.check(t, got, err)
		})
	}
}

// Two ids the tables share, spelled out because the shape is the point: the
// schemas call for uuids, and the CLI tells one from a position by looking.
const (
	taskUUID = "11111111-1111-1111-1111-111111111111"
	itemUUID = "22222222-2222-2222-2222-222222222222"
)

func TestCheckArgs(t *testing.T) {
	cases := []argCase{
		{
			name: "a uuid names the item by id",
			argv: []string{taskUUID, itemUUID},
			org:  "acme",
			want: map[string]any{"organization": "acme", "task_id": taskUUID, "item_id": itemUUID},
		},
		{
			name: "an integer names it by position",
			argv: []string{taskUUID, "2"},
			org:  "acme",
			want: map[string]any{"organization": "acme", "task_id": taskUUID, "position": 2},
		},
		{
			name: "--cause rides along under its schema name",
			argv: []string{taskUUID, "2", "--cause", "ran it"},
			org:  "acme",
			want: map[string]any{"organization": "acme", "task_id": taskUUID, "position": 2, "cause": "ran it"},
		},
		{
			name:    "position 0 is refused: the schema counts from 1",
			argv:    []string{taskUUID, "0"},
			org:     "acme",
			wantErr: "1-based",
		},
		{
			name:    "a word that is neither is refused, naming both spellings",
			argv:    []string{taskUUID, "banana"},
			org:     "acme",
			wantErr: "neither an item id nor a 1-based position",
		},
		{
			name:    "a task alone satisfies neither half of the schema's oneOf",
			argv:    []string{taskUUID},
			org:     "acme",
			wantErr: "one or the other",
		},
		{
			name:    "no task at all",
			argv:    nil,
			org:     "acme",
			wantErr: "which task?",
		},
		{
			name:    "a mistyped task id is refused locally",
			argv:    []string{"not-a-uuid", "2"},
			org:     "acme",
			wantErr: "is not a task id",
		},
		{
			name:    "no org is refused before any network call",
			argv:    []string{taskUUID, "2"},
			org:     "",
			wantErr: "--org",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := CheckArgs(c.argv, c.org)
			c.check(t, got, err)
		})
	}
}

func TestCloseArgs(t *testing.T) {
	cases := []argCase{
		{
			name: "a close is done unless a person says otherwise",
			argv: []string{taskUUID, "--cause", "shipped"},
			org:  "acme",
			want: map[string]any{
				"organization": "acme", "task_id": taskUUID,
				"disposition": "done", "cause": "shipped",
			},
		},
		{
			name: "--dismiss",
			argv: []string{taskUUID, "--dismiss", "--cause", "not ours"},
			org:  "acme",
			want: map[string]any{
				"organization": "acme", "task_id": taskUUID,
				"disposition": "dismissed", "cause": "not ours",
			},
		},
		{
			name: "--superseded-by names the replacement and the disposition",
			argv: []string{taskUUID, "--superseded-by", itemUUID, "--cause", "replaced"},
			org:  "acme",
			want: map[string]any{
				"organization": "acme", "task_id": taskUUID,
				"disposition": "superseded", "cause": "replaced", "superseded_by": itemUUID,
			},
		},
		{
			name:    "no --cause is refused: the schema requires one on every close",
			argv:    []string{taskUUID},
			org:     "acme",
			wantErr: "--cause",
		},
		{
			name:    "an empty --cause is no cause",
			argv:    []string{taskUUID, "--cause", "  "},
			org:     "acme",
			wantErr: "--cause",
		},
		{
			name:    "two dispositions is a question, not a default to resolve",
			argv:    []string{taskUUID, "--superseded-by", itemUUID, "--dismiss", "--cause", "x"},
			org:     "acme",
			wantErr: "one disposition",
		},
		{
			name:    "a replacement that is not a task id is refused locally",
			argv:    []string{taskUUID, "--superseded-by", "later", "--cause", "x"},
			org:     "acme",
			wantErr: "is not a task id",
		},
		{
			name:    "no task at all",
			argv:    []string{"--cause", "x"},
			org:     "acme",
			wantErr: "which task?",
		},
		{
			name:    "no org is refused before any network call",
			argv:    []string{taskUUID, "--cause", "shipped"},
			org:     "",
			wantErr: "--org",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := CloseArgs(c.argv, c.org)
			c.check(t, got, err)
		})
	}
}

func TestListWantsJSON(t *testing.T) {
	for _, argv := range [][]string{{"--json"}, {"--mine", "--json"}, {"-json"}} {
		if !ListWantsJSON(argv) {
			t.Errorf("ListWantsJSON(%q) = false", argv)
		}
	}
	for _, argv := range [][]string{nil, {"--mine"}, {"--status", "open"}} {
		if ListWantsJSON(argv) {
			t.Errorf("ListWantsJSON(%q) = true", argv)
		}
	}
}

func TestEveryMapperAlwaysCarriesTheOrganization(t *testing.T) {
	// The call says which
	// organization it is for, every time. There is no path through any mapper
	// that produces arguments without it.
	mappers := map[string]struct {
		mapper func([]string, string) (map[string]any, error)
		argvs  [][]string
	}{
		"AddArgs": {AddArgs, [][]string{
			{"Thing", "--kind", "ops"},
			{"Thing", "--kind", "ops", "--org", "other"},
			{"Thing", "--kind", "ops", "--body", "b"},
		}},
		"ListArgs": {ListArgs, [][]string{
			nil,
			{"--mine"},
			{"--org", "other", "--status", "all"},
		}},
		"CheckArgs": {CheckArgs, [][]string{
			{taskUUID, "2"},
			{taskUUID, itemUUID, "--org", "other"},
		}},
		"CloseArgs": {CloseArgs, [][]string{
			{taskUUID, "--cause", "shipped"},
			{taskUUID, "--dismiss", "--cause", "x", "--org", "other"},
		}},
	}

	for name, m := range mappers {
		for _, argv := range m.argvs {
			args, err := m.mapper(argv, "acme")
			if err != nil {
				t.Fatalf("%s(%q): %v", name, argv, err)
			}
			if org, _ := args["organization"].(string); org == "" {
				t.Fatalf("%s(%q) sent no organization: %#v", name, argv, args)
			}
		}
	}
}

// help is the one refusal that is not a refusal: --help is a question, and the
// answer is the command's own shape. Every verb answers it, and answers it
// with its own flags rather than with a paragraph someone has to keep in step.
func TestEveryMapperAnswersHelp(t *testing.T) {
	mappers := map[string]struct {
		mapper func([]string, string) (map[string]any, error)
		flags  []string
	}{
		"AddArgs":   {AddArgs, []string{"-kind", "-body", "-for", "-dedup-key", "-org"}},
		"ListArgs":  {ListArgs, []string{"-mine", "-status", "-limit", "-json", "-org"}},
		"CheckArgs": {CheckArgs, []string{"-cause", "-org"}},
		"CloseArgs": {CloseArgs, []string{"-cause", "-dismiss", "-superseded-by", "-org"}},
	}

	for name, m := range mappers {
		for _, argv := range [][]string{{"--help"}, {"-h"}} {
			_, err := m.mapper(argv, "acme")

			var help *HelpError
			if !errors.As(err, &help) {
				t.Fatalf("%s(%q) = %v, want a *HelpError", name, argv, err)
			}

			var out strings.Builder
			help.Flags.SetOutput(&out)
			help.Flags.PrintDefaults()
			for _, flag := range m.flags {
				if !strings.Contains(out.String(), flag) {
					t.Fatalf("%s help does not document %s:\n%s", name, flag, out.String())
				}
			}
			// The synopsis starts at the verb: the binary's name belongs to
			// the command layer, which knows which of its two names it was
			// invoked by.
			if !strings.HasPrefix(help.Synopsis, "task ") {
				t.Fatalf("%s help synopsis = %q", name, help.Synopsis)
			}
		}
	}
}

// `--org` with nothing after it is flag's "flag needs an argument: -org",
// which spells the flag the way nobody typed it and says "argument" for what
// this CLI calls a value everywhere else.
func TestAFlagWithNoValueIsRefusedInTheConnectorsOwnWords(t *testing.T) {
	cases := []struct {
		name string
		got  func() error
		want string
	}{
		{"add --org", func() error { _, err := AddArgs([]string{"Thing", "--kind", "ops", "--org"}, ""); return err }, "--org needs a value"},
		{"list --status", func() error { _, err := ListArgs([]string{"--status"}, "acme"); return err }, "--status needs a value"},
		{"close --cause", func() error { _, err := CloseArgs([]string{taskUUID, "--cause"}, "acme"); return err }, "--cause needs a value"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.got()
			if err == nil {
				t.Fatal("no error")
			}
			if err.Error() != c.want {
				t.Fatalf("error %q, want %q", err, c.want)
			}
		})
	}
}

// A value starting with a dash is a flag that swallowed the next flag. `--org`
// has said so since it shipped; every flag that takes free text says it the
// same way, because it is the same mistake.
func TestAValueThatStartsWithADashIsRefused(t *testing.T) {
	cases := []struct {
		name string
		got  func() error
		want string
	}{
		{"--body", func() error {
			_, err := AddArgs([]string{"Thing", "--kind", "ops", "--body", "--json"}, "acme")
			return err
		}, `--body "--json"`},
		{"--for", func() error {
			_, err := AddArgs([]string{"Thing", "--kind", "ops", "--for", "--mine"}, "acme")
			return err
		}, `--for "--mine"`},
		{"--cause on check", func() error { _, err := CheckArgs([]string{taskUUID, "2", "--cause", "--org"}, "acme"); return err }, `--cause "--org"`},
		{"--cause on close", func() error { _, err := CloseArgs([]string{taskUUID, "--cause", "--dismiss"}, "acme"); return err }, `--cause "--dismiss"`},
		{"--org", func() error { _, err := ListArgs([]string{"--org", "--json"}, "acme"); return err }, `--org "--json"`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.got()
			if err == nil {
				t.Fatal("no error")
			}
			if !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "never starts with a dash") {
				t.Fatalf("error %q does not name %s the way --org does", err, c.want)
			}
		})
	}
}
