// Package tasks maps `connector task …` invocations onto the arguments of the
// four MCP tools behind them: task_create, task_list, task_check, task_close.
//
// Everything here is pure. A function takes the words a person typed and the
// remembered organization, and returns the arguments map a tools/call carries
// — or an error, before any socket is opened. The command layer does the I/O;
// this layer decides what would be sent, which is the part worth testing
// exhaustively and the part a checked-in schema fixture can keep honest.
//
// Two rules hold throughout.
//
// **Argument names are copied, never invented.** Every key produced here is a
// property of the tool's advertised schema, spelled the way the schema spells
// it, and schema_test.go proves it against
// contract/connector/tool_schemas.json — the file the server's tool schema generator writes and
// the server's schema drift test fails on when it drifts. A flag that maps to
// an invented name is a round trip that comes back "argument not recognised".
//
// **Every call names its organization.** `organization` is in the arguments
// map of every one of the four tools, always, and there is no code path here
// that omits it. The server authorizes the caller for that organization on
// every call; the token carries no default organization or authorization
// snapshot.
//
// So the connector's remembered organization is a default *for the argument*,
// resolved on this laptop, and never a default the server applies. There is no
// server-side org to fall back on, by design.
package tasks

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The vocabularies below are copied from the tools' schemas, and schema_test.go
// asserts each one still equals the enum or bound the fixture carries. Copying
// them is what lets the CLI refuse a typo in a sentence naming the alternatives
// instead of spending a round trip to be told the same thing less clearly.
var (
	// listStatuses is task_list's `status` enum.
	listStatuses = []string{"open", "done", "dismissed", "superseded", "all"}

	// closeDispositions is task_close's `disposition` enum, in the schema's
	// order. The first is the default: closing a task is overwhelmingly
	// closing it done, and making a person spell that out every time would
	// buy nothing — unlike the cause, which the schema requires because a
	// close with no reason is a fact nobody can audit later.
	closeDispositions = []string{"done", "dismissed", "superseded"}
)

const (
	// listForMe is the only value task_list's `for` enum accepts. The tool
	// takes an `assignee` membership id for anyone else, which is not
	// something a person has to hand, so `--mine` is the whole of it.
	listForMe = "me"

	// listLimitMin and listLimitMax are task_list's `limit` bounds.
	listLimitMin = 1
	listLimitMax = 100

	// positionMin is task_check's `position` minimum: checklist positions are
	// 1-based, and a 0 is a person counting from the wrong end.
	positionMin = 1
)

// uuid is the shape every id in these schemas has — task ids and checklist
// item ids alike. Recognising one locally is what lets `task check` tell an
// item id from a position without asking the server, and what turns a
// mistyped id into a sentence instead of a round trip.
var uuid = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// AddArgs maps `task add` onto task_create's arguments.
//
// org is the remembered default; `--org` overrides it. `kinds` is the tool's
// only required argument besides `organization`, and a title is required here
// even though the schema does not require it: task_create accepts a task with
// only `text` or only an attachment, and neither is a shape this command can
// produce.
func AddArgs(argv []string, org string) (map[string]any, error) {
	cmd := newCommand("task add", `task add "<title>" --kind <slug> [flags]`)
	orgFlag := cmd.flags.String("org", "", "the organization to act on — its slug or id")
	body := cmd.flags.String("body", "", "markdown context or runbook overview")
	assignee := cmd.flags.String("for", "", "assignment target: user:<email-or-membership-id>, group:<slug> or looper:<slug>")
	dedupKey := cmd.flags.String("dedup-key", "", "stable key that makes a retry return the same task")
	var kinds stringList
	cmd.flags.Var(&kinds, "kind", "a kind slug; repeat for more than one")

	positional, err := cmd.parse(argv)
	if err != nil {
		return nil, err
	}

	organization, err := resolveOrg(*orgFlag, org)
	if err != nil {
		return nil, err
	}

	title, err := onlyPositional(positional, "title")
	if err != nil {
		return nil, err
	}
	if title == "" {
		return nil, errors.New(`a task needs a title: connector task add "Rotate the WorkOS key" --kind ops`)
	}
	if len(kinds) == 0 {
		return nil, errors.New("a task needs at least one kind: pass --kind <slug> (task_kind_list shows the ones this organization has)")
	}

	args := map[string]any{
		"organization": organization,
		"title":        title,
		"kinds":        []string(kinds),
	}
	if err := errors.Join(
		optional(args, "body", *body),
		optional(args, "for", *assignee),
		optional(args, "dedup_key", *dedupKey),
	); err != nil {
		return nil, err
	}
	return args, nil
}

// ListArgs maps `task list` onto task_list's arguments.
//
// Every filter is optional: the tool defaults to the organization's open tasks
// on its own, so a bare `task list` sends nothing but the organization.
func ListArgs(argv []string, org string) (map[string]any, error) {
	cmd := newCommand("task list", "task list [flags]")
	orgFlag := cmd.flags.String("org", "", "the organization to act on — its slug or id")
	mine := cmd.flags.Bool("mine", false, "only tasks assigned to you")
	status := cmd.flags.String("status", "", "one of "+strings.Join(listStatuses, ", "))
	limit := cmd.flags.Int("limit", 0, fmt.Sprintf("how many tasks to return (%d-%d)", listLimitMin, listLimitMax))
	cmd.flags.Bool("json", false, "print the tool's payload instead of a table")

	positional, err := cmd.parse(argv)
	if err != nil {
		return nil, err
	}
	if len(positional) > 0 {
		return nil, fmt.Errorf("unexpected argument %q — task list takes flags only", positional[0])
	}

	organization, err := resolveOrg(*orgFlag, org)
	if err != nil {
		return nil, err
	}

	args := map[string]any{"organization": organization}

	if *mine {
		args["for"] = listForMe
	}
	if *status != "" {
		if !slices.Contains(listStatuses, *status) {
			return nil, fmt.Errorf("--status %q is not one of %s", *status, strings.Join(listStatuses, ", "))
		}
		args["status"] = *status
	}
	if wasSet(cmd.flags, "limit") {
		if *limit < listLimitMin || *limit > listLimitMax {
			return nil, fmt.Errorf("--limit %d is outside the %d-%d the tool accepts", *limit, listLimitMin, listLimitMax)
		}
		args["limit"] = *limit
	}
	return args, nil
}

// CheckArgs maps `task check` onto task_check's arguments.
//
// The second positional argument is the whole of the schema's
// `oneOf: [required item_id, required position]`: a uuid names the item by id,
// an integer names it by its 1-based position, and nothing else is either. One
// argument for two spellings is right because they answer the same question —
// which item — and a person reading `task check <task> 2` has no doubt what it
// means.
func CheckArgs(argv []string, org string) (map[string]any, error) {
	cmd := newCommand("task check", "task check <task-id> <item-id-or-position> [flags]")
	orgFlag := cmd.flags.String("org", "", "the organization to act on — its slug or id")
	cause := cmd.flags.String("cause", "", "evidence or the decision this records")

	positional, err := cmd.parse(argv)
	if err != nil {
		return nil, err
	}

	organization, err := resolveOrg(*orgFlag, org)
	if err != nil {
		return nil, err
	}

	switch len(positional) {
	case 0:
		return nil, errors.New("which task? connector task check <task-id> <item-id-or-position>")
	case 1:
		return nil, errors.New("which item? name it by its item id or by its 1-based position — the tool takes one or the other")
	case 2:
	default:
		return nil, fmt.Errorf("unexpected argument %q — task check takes a task and one item", positional[2])
	}

	taskID, err := parseTaskID(positional[0])
	if err != nil {
		return nil, err
	}

	args := map[string]any{"organization": organization, "task_id": taskID}

	item, err := itemArgument(positional[1])
	if err != nil {
		return nil, err
	}
	for key, value := range item {
		args[key] = value
	}

	if err := optional(args, "cause", *cause); err != nil {
		return nil, err
	}
	return args, nil
}

// itemArgument decides which half of task_check's oneOf a person named.
func itemArgument(selector string) (map[string]any, error) {
	if position, err := strconv.Atoi(selector); err == nil {
		if position < positionMin {
			return nil, fmt.Errorf("%d is not a checklist position: positions are 1-based", position)
		}
		return map[string]any{"position": position}, nil
	}
	if uuid.MatchString(selector) {
		return map[string]any{"item_id": selector}, nil
	}
	return nil, fmt.Errorf("%q is neither an item id nor a 1-based position", selector)
}

// CloseArgs maps `task close` onto task_close's arguments.
//
// `cause` is required by the schema on every close, so it is required here:
// making the round trip only to be told the same thing costs a person a
// network round trip and tells them nothing the CLI did not already know.
func CloseArgs(argv []string, org string) (map[string]any, error) {
	cmd := newCommand("task close", `task close <task-id> --cause "<why>" [flags]`)
	orgFlag := cmd.flags.String("org", "", "the organization to act on — its slug or id")
	cause := cmd.flags.String("cause", "", "why the task is being closed; the tool requires one")
	dismiss := cmd.flags.Bool("dismiss", false, "close as dismissed rather than done")
	supersededBy := cmd.flags.String("superseded-by", "", "the task id that replaces this one")

	positional, err := cmd.parse(argv)
	if err != nil {
		return nil, err
	}

	organization, err := resolveOrg(*orgFlag, org)
	if err != nil {
		return nil, err
	}

	if len(positional) == 0 {
		return nil, errors.New(`which task? connector task close <task-id> --cause "shipped"`)
	}
	if len(positional) > 1 {
		return nil, fmt.Errorf("unexpected argument %q — task close takes one task", positional[1])
	}

	taskID, err := parseTaskID(positional[0])
	if err != nil {
		return nil, err
	}
	reason, err := value("cause", *cause)
	if err != nil {
		return nil, err
	}
	if reason == "" {
		return nil, errors.New(`a close needs a cause: pass --cause "why this is closed" (the tool requires one on every close)`)
	}

	disposition, err := closeDisposition(*dismiss, *supersededBy)
	if err != nil {
		return nil, err
	}

	args := map[string]any{
		"organization": organization,
		"task_id":      taskID,
		"disposition":  disposition,
		"cause":        reason,
	}
	if *supersededBy != "" {
		replacement, err := parseTaskID(*supersededBy)
		if err != nil {
			return nil, err
		}
		args["superseded_by"] = replacement
	}
	return args, nil
}

// closeDisposition picks one value of task_close's enum. A close is done unless a
// person said otherwise, and saying two otherwises is a question, not a
// default to resolve on their behalf.
func closeDisposition(dismissed bool, supersededBy string) (string, error) {
	switch {
	case dismissed && supersededBy != "":
		return "", errors.New("one disposition: --dismiss and --superseded-by cannot both describe the same close")
	case dismissed:
		return "dismissed", nil
	case supersededBy != "":
		return "superseded", nil
	default:
		return closeDispositions[0], nil
	}
}

// parseTaskID checks that what a person typed is the uuid these schemas call for,
// so a mistyped id is a sentence here rather than a refusal from the server.
func parseTaskID(raw string) (string, error) {
	id := strings.TrimSpace(raw)
	if !uuid.MatchString(id) {
		return "", fmt.Errorf("%q is not a task id — task list shows them", id)
	}
	return id, nil
}

// ListWantsJSON reports whether a `task list` argv asked for the tool's
// payload instead of a rendered table.
//
// It reads the token rather than the parsed flag because the arguments map is
// what the parsers return, and an output choice is not an argument to a tool.
// The reading is unambiguous *for this verb's flags*, which is why the
// function names the verb: --json takes no value, and every `task list` flag
// that does take one refuses "--json" as its own (a value may not start with a
// dash, a status must be in the enum, a limit must be a number). A verb with a
// free-text flag whose value could legitimately be "--json" would need the
// parsed flag, not this scan.
func ListWantsJSON(argv []string) bool {
	return slices.Contains(argv, "--json") || slices.Contains(argv, "-json")
}

// wasSet reports whether a flag was given, which is not the same question as
// whether it holds its zero value: `--limit 0` is a mistake worth naming, and
// no --limit at all is not.
func wasSet(flags *flag.FlagSet, name string) bool {
	given := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == name {
			given = true
		}
	})
	return given
}

// resolveOrg decides the value of the `organization` argument: the flag, then
// the organization the last successful command named. There is no third
// option — see the package doc — so a laptop with neither is stopped here,
// naming the flag, rather than sent at the network to be refused.
func resolveOrg(flagOrg, remembered string) (string, error) {
	org, err := value("org", flagOrg)
	if err != nil {
		return "", err
	}
	if org == "" {
		org = strings.TrimSpace(remembered)
	}
	if org == "" {
		return "", errors.New("no organization: pass --org <slug>; the connector remembers the last one you used")
	}
	return org, nil
}

// value is what a person meant to put after a flag, refused when it starts
// with a dash. That is almost always a flag that swallowed the next one —
// `--org --json`, `--cause --dismiss` — and nothing these tools take as a
// value begins with one, so the mistake is a sentence here rather than an
// organization named "--json" sent at the network.
func value(name, raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "-") {
		return "", fmt.Errorf("--%s %q is not a value: a value never starts with a dash", name, trimmed)
	}
	return trimmed, nil
}

// onlyPositional returns the single positional argument a command takes,
// naming what it was for when there is more than one. A second bare word is
// nearly always a missing pair of quotes around a title.
func onlyPositional(positional []string, what string) (string, error) {
	switch len(positional) {
	case 0:
		return "", nil
	case 1:
		return strings.TrimSpace(positional[0]), nil
	default:
		return "", fmt.Errorf("unexpected argument %q — the %s is one argument, so quote it", positional[1], what)
	}
}

// optional adds a value to the arguments only when a person gave one. An
// empty string is not a value: sending `"body": ""` would be the connector
// asserting a blank body rather than staying silent about it.
//
// The flag is the schema property spelled with dashes — `dedup_key` is
// `--dedup-key` — so a refusal names the word that was actually typed.
func optional(args map[string]any, key, raw string) error {
	given, err := value(strings.ReplaceAll(key, "_", "-"), raw)
	if err != nil {
		return err
	}
	if given != "" {
		args[key] = given
	}
	return nil
}

// HelpError reports that a person asked what a command takes rather than
// telling it. `--help` is a question, not a mistake: it travels as an error
// only because that is how a parse stops, and the command layer answers it on
// stdout and exits 0.
//
// It carries data, not output — the synopsis and the very flag set the parse
// used, so a flag added to a verb cannot go undocumented. This package stays
// pure; the command layer owns what reaches a terminal. The synopsis starts at
// the verb because the word in front of it is whichever name the binary was
// invoked by, and only the command layer knows that.
type HelpError struct {
	Synopsis string        // one line: what the command takes, from the verb on
	Flags    *flag.FlagSet // the flags this verb accepts
}

func (e *HelpError) Error() string { return "usage: " + e.Synopsis }

// command is one verb: the flag set it parses with, and the synopsis its
// `--help` prints above the flags, from the verb on.
type command struct {
	flags    *flag.FlagSet
	synopsis string
}

// newCommand returns a flag set that reports errors by returning them rather
// than by printing them: these functions are pure, and the command layer owns
// what reaches a terminal.
func newCommand(name, synopsis string) *command {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	return &command{flags: flags, synopsis: synopsis}
}

// parse runs the flags over argv and returns the positional arguments in
// order, allowing flags and positionals to interleave.
//
// flag.FlagSet stops at the first bare word, which would make `task add
// --kind ops "A title"` and `task add "A title" --kind ops` mean different
// things. Re-entering the parse after each positional makes them the same
// command, which is what a person typing either one expects.
func (c *command) parse(argv []string) ([]string, error) {
	var positional []string
	for {
		if err := c.flags.Parse(argv); err != nil {
			return nil, c.diagnose(err)
		}
		rest := c.flags.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		argv = rest[1:]
	}
}

// diagnose puts the flag package's two stock diagnostics into this connector's
// own words. `--help` becomes a HelpError the command layer answers with a
// usage rather than a failure; "flag needs an argument: -org" becomes "--org
// needs a value", which spells the flag the way the person typed it and calls
// the missing thing what every other message here calls it.
func (c *command) diagnose(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return &HelpError{Synopsis: c.synopsis, Flags: c.flags}
	}
	if name, found := strings.CutPrefix(err.Error(), "flag needs an argument: -"); found {
		return fmt.Errorf("--%s needs a value", name)
	}
	return err
}

// stringList is a flag that may be repeated, in the order it was given.
type stringList []string

func (l *stringList) String() string {
	if l == nil {
		return ""
	}
	return strings.Join(*l, ",")
}

func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}
