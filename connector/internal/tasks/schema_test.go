package tasks

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/gentility-ai/gentility/connector/internal/contracttest"
)

// schemasJSON is the schemas the four task tools actually advertise, written
// by the server's tool schema generator and kept honest by
// the server's schema drift test: edit a tool's schema without regenerating
// this file and the server suite fails, in the same change.
//
// Reading the published contract makes these tests hermetic. Mapping a flag onto a name
// the server does not have is a failure a person only meets on their laptop,
// as "argument not recognised" — so it is caught here, against the real
// schema, with no server running.
func schemasJSON(t *testing.T) []byte {
	t.Helper()
	return contracttest.Read(t, "connector/tool_schemas.json")
}

// schema is the part of a JSON Schema these tests read.
type schema struct {
	Properties map[string]property `json:"properties"`
	Required   []string            `json:"required"`
	OneOf      []struct {
		Required []string `json:"required"`
	} `json:"oneOf"`
}

// property is the part of one property these tests read: the vocabulary it
// accepts, and the range it accepts.
type property struct {
	Type    string   `json:"type"`
	Enum    []string `json:"enum"`
	Minimum *int     `json:"minimum"`
	Maximum *int     `json:"maximum"`
}

func loadSchemas(t *testing.T) map[string]schema {
	t.Helper()

	var schemas map[string]schema
	if err := json.Unmarshal(schemasJSON(t), &schemas); err != nil {
		t.Fatalf("contract/connector/tool_schemas.json is not readable: %v", err)
	}
	return schemas
}

// assertSatisfies checks one produced arguments map against the tool's real
// schema, in both directions: nothing invented, nothing required left out.
func assertSatisfies(t *testing.T, tool string, args map[string]any) {
	t.Helper()

	tools := loadSchemas(t)
	s, ok := tools[tool]
	if !ok {
		t.Fatalf("contract/connector/tool_schemas.json has no %s", tool)
	}

	for key := range args {
		if _, declared := s.Properties[key]; !declared {
			t.Errorf("%s: argument %q is not a property of the tool's schema", tool, key)
		}
	}
	for _, required := range s.Required {
		if _, present := args[required]; !present {
			t.Errorf("%s: required argument %q is missing", tool, required)
		}
	}

	// oneOf is exclusive: task_check's schema says an item is named by its id
	// or by its position, and satisfying neither or both is a refusal.
	if len(s.OneOf) == 0 {
		return
	}
	satisfied := 0
	for _, branch := range s.OneOf {
		if hasAll(args, branch.Required) {
			satisfied++
		}
	}
	if satisfied != 1 {
		t.Errorf("%s: %d of the schema's %d oneOf branches are satisfied, want exactly 1: %#v",
			tool, satisfied, len(s.OneOf), args)
	}
}

func hasAll(args map[string]any, keys []string) bool {
	for _, key := range keys {
		if _, present := args[key]; !present {
			return false
		}
	}
	return true
}

// mustAdd is AddArgs for a case the test asserts cannot fail.
func mustAdd(t *testing.T, argv []string, org string) map[string]any {
	t.Helper()
	args, err := AddArgs(argv, org)
	if err != nil {
		t.Fatalf("AddArgs(%q): %v", argv, err)
	}
	return args
}

func TestAddArgsSatisfyTheTaskCreateSchema(t *testing.T) {
	assertSatisfies(t, "task_create", mustAdd(t,
		[]string{"T", "--kind", "ops", "--body", "b", "--for", "user:user@example.com", "--dedup-key", "k"}, "acme"))

	// The minimal call is the one most likely to drop a required argument.
	assertSatisfies(t, "task_create", mustAdd(t, []string{"T", "--kind", "ops"}, "acme"))
}

// enumOf returns one property's enum, failing when the property or the enum
// has gone.
func enumOf(t *testing.T, tool, name string) []string {
	t.Helper()

	p, ok := loadSchemas(t)[tool].Properties[name]
	if !ok {
		t.Fatalf("%s has no %s property", tool, name)
	}
	if len(p.Enum) == 0 {
		t.Fatalf("%s.%s no longer has an enum", tool, name)
	}
	return p.Enum
}

// mustList is ListArgs for a case the test asserts cannot fail.
func mustList(t *testing.T, argv []string, org string) map[string]any {
	t.Helper()
	args, err := ListArgs(argv, org)
	if err != nil {
		t.Fatalf("ListArgs(%q): %v", argv, err)
	}
	return args
}

func TestListArgsSatisfyTheTaskListSchema(t *testing.T) {
	assertSatisfies(t, "task_list", mustList(t, []string{"--mine", "--status", "open", "--limit", "10"}, "acme"))
	assertSatisfies(t, "task_list", mustList(t, nil, "acme"))
}

func TestListStatusValuesAreTheSchemaEnum(t *testing.T) {
	if !reflect.DeepEqual(listStatuses, enumOf(t, "task_list", "status")) {
		t.Fatalf("--status accepts %v, the schema's enum is %v", listStatuses, enumOf(t, "task_list", "status"))
	}
	// Every value the CLI accepts must survive the round trip into the map.
	for _, status := range listStatuses {
		args := mustList(t, []string{"--status", status}, "acme")
		if args["status"] != status {
			t.Errorf("--status %s mapped to %v", status, args["status"])
		}
	}
}

func TestListForMeIsTheSchemaEnumsOnlyValue(t *testing.T) {
	if got := enumOf(t, "task_list", "for"); !reflect.DeepEqual(got, []string{listForMe}) {
		t.Fatalf("task_list.for accepts %v, and --mine sends %q", got, listForMe)
	}
}

func TestListLimitBoundsAreTheSchemaBounds(t *testing.T) {
	limit := loadSchemas(t)["task_list"].Properties["limit"]

	if limit.Minimum == nil || *limit.Minimum != listLimitMin {
		t.Errorf("the schema's minimum is %v, the CLI enforces %d", limit.Minimum, listLimitMin)
	}
	if limit.Maximum == nil || *limit.Maximum != listLimitMax {
		t.Errorf("the schema's maximum is %v, the CLI enforces %d", limit.Maximum, listLimitMax)
	}
}

// mustCheck and mustClose are CheckArgs and CloseArgs for cases the tests
// assert cannot fail.
func mustCheck(t *testing.T, argv []string, org string) map[string]any {
	t.Helper()
	args, err := CheckArgs(argv, org)
	if err != nil {
		t.Fatalf("CheckArgs(%q): %v", argv, err)
	}
	return args
}

func mustClose(t *testing.T, argv []string, org string) map[string]any {
	t.Helper()
	args, err := CloseArgs(argv, org)
	if err != nil {
		t.Fatalf("CloseArgs(%q): %v", argv, err)
	}
	return args
}

func TestCheckArgsSatisfyTheTaskCheckSchema(t *testing.T) {
	// Both halves of the schema's oneOf, and exactly one of them each time:
	// assertSatisfies fails a call that satisfies neither or both.
	assertSatisfies(t, "task_check", mustCheck(t, []string{taskUUID, "2", "--cause", "done"}, "acme"))
	assertSatisfies(t, "task_check", mustCheck(t, []string{taskUUID, itemUUID}, "acme"))
}

func TestCloseArgsSatisfyTheTaskCloseSchema(t *testing.T) {
	assertSatisfies(t, "task_close", mustClose(t, []string{taskUUID, "--cause", "shipped"}, "acme"))
	assertSatisfies(t, "task_close", mustClose(t, []string{taskUUID, "--dismiss", "--cause", "not ours"}, "acme"))
	assertSatisfies(t, "task_close", mustClose(t,
		[]string{taskUUID, "--superseded-by", itemUUID, "--cause", "replaced"}, "acme"))
}

func TestCloseDispositionValuesAreTheSchemaEnum(t *testing.T) {
	if !reflect.DeepEqual(closeDispositions, enumOf(t, "task_close", "disposition")) {
		t.Fatalf("the CLI can send %v, the schema's enum is %v",
			closeDispositions, enumOf(t, "task_close", "disposition"))
	}

	// Every disposition the CLI can produce, and that it produces each one.
	for _, c := range []struct {
		argv []string
		want string
	}{
		{[]string{taskUUID, "--cause", "x"}, "done"},
		{[]string{taskUUID, "--dismiss", "--cause", "x"}, "dismissed"},
		{[]string{taskUUID, "--superseded-by", itemUUID, "--cause", "x"}, "superseded"},
	} {
		if got := mustClose(t, c.argv, "acme")["disposition"]; got != c.want {
			t.Errorf("close(%q) disposition = %v, want %v", c.argv, got, c.want)
		}
	}
}

func TestCheckPositionMinimumIsTheSchemaMinimum(t *testing.T) {
	position := loadSchemas(t)["task_check"].Properties["position"]

	if position.Minimum == nil || *position.Minimum != positionMin {
		t.Fatalf("the schema's minimum is %v, the CLI enforces %d", position.Minimum, positionMin)
	}
}

func TestEveryTaskToolRequiresTheOrganizationArgument(t *testing.T) {
	// The server's rule, read back from the schemas: OrgScope.augment_schema/2
	// adds `organization` to every one of these tools as required. If that ever
	// stops being true the CLI's "always send it" rule needs re-reading, not
	// quietly relaxing.
	for tool, s := range loadSchemas(t) {
		found := false
		for _, required := range s.Required {
			if required == "organization" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s does not require the organization argument", tool)
		}
		if _, declared := s.Properties["organization"]; !declared {
			t.Errorf("%s does not declare the organization property", tool)
		}
	}
}
