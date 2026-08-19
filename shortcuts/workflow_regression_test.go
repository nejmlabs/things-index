package shortcutasset

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const (
	addTodoAction    = "com.culturedcode.ThingsMac.TAIAddTodo2"
	editItemsAction  = "com.culturedcode.ThingsMac.TAIEditItems"
	itemEntityAction = "com.culturedcode.ThingsMac.TAIItemEntity"
)

// TestHelperSourceKeepsThingsEnumsLiteral guards an easy-to-miss App Intents
// constraint: Things' Start parameter is a non-resolvable enumeration. Passing
// a Cherri variable compiles successfully, but Things silently ignores it.
func TestHelperSourceKeepsThingsEnumsLiteral(t *testing.T) {
	t.Parallel()

	source := helperSource(t)
	if strings.Contains(source, `"start": "{@start}"`) {
		t.Fatal("Things Start must not be supplied as a dynamic text value; branch to literal onDate, anytime, and someday values")
	}
	for _, literal := range []string{`"start": "onDate"`, `"start": "anytime"`, `"start": "someday"`} {
		if !strings.Contains(source, literal) {
			t.Errorf("helper source does not contain native Things enum %s", literal)
		}
	}
}

// TestHelperSourceKeepsIndependentDateOffsets catches wiring mistakes before a
// Shortcut is installed. In particular, a deadline is relative to today, not
// to the task's start date, while a reminder is relative to the start date.
func TestHelperSourceKeepsIndependentDateOffsets(t *testing.T) {
	t.Parallel()

	source := helperSource(t)
	want := []string{
		`@startDateValue = adjustDate(@todayStart, "Add", qty(@startDayOffset, "days"))`,
		`@reminderTimeValue = adjustDate(@startDateValue, "Add", qty(@reminderMinuteOffset, "min"))`,
		`@deadlineValue = adjustDate(@todayStart, "Add", qty(@deadlineDayOffset, "days"))`,
	}
	for _, snippet := range want {
		if !strings.Contains(source, snippet) {
			t.Errorf("helper source is missing native date wiring %q", snippet)
		}
	}
	if strings.Contains(source, "Specified Date") {
		t.Error("helper must not use locale-dependent Specified Date parsing")
	}
}

func TestHelperSourceParsesNestedProtocolDictionaries(t *testing.T) {
	t.Parallel()

	source := helperSource(t)
	for _, unsafe := range []string{
		`@task = @request['task']`,
		`@destination = @task['destination']`,
		`@evening = @task['evening']`,
		`@tags = @task['tags']`,
		`number(@task['startDayOffset'])`,
		`number(@task['reminderMinuteOffset'])`,
		`number(@task['deadlineDayOffset'])`,
	} {
		if strings.Contains(source, unsafe) {
			t.Errorf("nested JSON object uses untyped property access %q; parse it as a dictionary first", unsafe)
		}
	}
}

func TestHelperSourceUsesStableUniqueThingsActionUUIDs(t *testing.T) {
	t.Parallel()

	source := helperSource(t)
	actions := sourceThingsRawActions(t, source)
	if got, want := len(actions), strings.Count(source, `rawAction("com.culturedcode.ThingsMac`); got != want {
		t.Fatalf("parsed %d of %d Things rawAction calls", got, want)
	}
	seen := make(map[string]int)
	for index, action := range actions {
		values := sourceStringValues(action.Body, "UUID")
		if len(values) != 1 || !isFixedUUID(values[0]) {
			t.Errorf("Things rawAction %d (%s) must contain one fixed literal UUID, got %#v", index, action.Identifier, values)
			continue
		}
		uuid := strings.ToLower(values[0])
		if previous, exists := seen[uuid]; exists {
			t.Errorf("Things rawActions %d and %d reuse UUID %s", previous, index, values[0])
		}
		seen[uuid] = index
	}
}

// TestHelperSourceUsesPlacementSpecificCreates guards Things' asymmetric API:
// project, area, and heading placement work during Create To-Do, while the
// remaining task fields are applied through typed Edit Items details.
func TestHelperSourceUsesPlacementSpecificCreates(t *testing.T) {
	t.Parallel()

	source := helperSource(t)
	actions := rawActionBodies(t, source, addTodoAction)
	if len(actions) != 4 {
		t.Fatalf("helper has %d Create To-Do actions, want Inbox, project, project+heading, and area branches", len(actions))
	}
	queryKinds := sourceEntityQueryKinds(t, source)
	shapes := map[string]int{"inbox": 0, "project": 0, "project_heading": 0, "area": 0}
	for i, action := range actions {
		for _, key := range []string{"AppIntentDescriptor", "ShowWhenRun", "title"} {
			if !hasSourceKey(action, key) {
				t.Errorf("Create To-Do action %d is missing %q", i, key)
			}
		}
		for _, key := range []string{"start", "startDate", "tags", "notes", "checklist"} {
			if hasSourceKey(action, key) {
				t.Errorf("Create To-Do action %d includes %q; use a typed Edit Items detail", i, key)
			}
		}

		parent := sourceAttachedVariable(action, "parent")
		heading := sourceAttachedVariable(action, "heading")
		switch {
		case heading != "" && parent == "":
			t.Errorf("Create To-Do action %d supplies Heading without Parent", i)
		case heading != "":
			if !queryKinds[parent]["project"] {
				t.Errorf("heading Create action %d Parent @%s is not the direct project query", i, parent)
			}
			if !queryKinds[heading]["heading"] {
				t.Errorf("heading Create action %d Heading @%s is not the direct heading query", i, heading)
			}
			shapes["project_heading"]++
		case parent == "":
			shapes["inbox"]++
		case queryKinds[parent]["project"]:
			shapes["project"]++
		case queryKinds[parent]["area"]:
			shapes["area"]++
		default:
			t.Errorf("parent-only Create action %d uses @%s without direct project/area query provenance", i, parent)
		}
	}
	for shape, count := range shapes {
		if count != 1 {
			t.Errorf("helper has %d %s Create branches, want 1", count, shape)
		}
	}
}

// TestHelperSourceCreateLeavesAreMutuallyExclusive guards the control-flow
// contract behind the four Create To-Do variants. Merely having four correctly
// shaped actions is insufficient: two independent true branches can create two
// pending tasks for one request.
func TestHelperSourceCreateLeavesAreMutuallyExclusive(t *testing.T) {
	t.Parallel()

	source := helperSource(t)
	pendingCreate := sourceIfBlock(t, source, `@pendingCount == 0`)
	if got := len(rawActionBodies(t, pendingCreate.Body, addTodoAction)); got != 4 {
		t.Fatalf("pendingCount == 0 branch has %d Create To-Do actions, want 4 mutually exclusive leaves", got)
	}
	if got := len(rawActionBodies(t, source, addTodoAction)); got != 4 {
		t.Fatalf("helper has %d Create To-Do actions in total; every Create must live under pendingCount == 0", got)
	}

	projectHeadingSide, nonProjectSide := sourceIfElseBlocks(t, pendingCreate.Body, `@destinationKind == "project"`)
	headingLeaf, projectLeaf := sourceIfElseBlocks(t, projectHeadingSide.Body, `@hasHeading`)
	areaLeaf, inboxLeaf := sourceIfElseBlocks(t, nonProjectSide.Body, `@destinationKind == "area"`)

	queryKinds := sourceEntityQueryKinds(t, source)
	leaves := []struct {
		name        string
		block       sourceBlock
		parentKind  string
		headingKind string
	}{
		{name: "project+heading", block: headingLeaf, parentKind: "project", headingKind: "heading"},
		{name: "project", block: projectLeaf, parentKind: "project"},
		{name: "area", block: areaLeaf, parentKind: "area"},
		{name: "inbox", block: inboxLeaf},
	}
	for _, leaf := range leaves {
		creates := sourceRawActions(t, leaf.block.Body, addTodoAction)
		if len(creates) != 1 {
			t.Errorf("%s leaf has %d Create To-Do actions, want exactly 1", leaf.name, len(creates))
			continue
		}
		create := creates[0]
		parent := sourceAttachedVariable(create.Body, "parent")
		heading := sourceAttachedVariable(create.Body, "heading")
		if leaf.parentKind == "" {
			if parent != "" || heading != "" {
				t.Errorf("%s leaf unexpectedly supplies Parent @%s or Heading @%s", leaf.name, parent, heading)
			}
			continue
		}
		if !queryKinds[parent][leaf.parentKind] {
			t.Errorf("%s leaf Parent @%s is not the direct %s query", leaf.name, parent, leaf.parentKind)
		}
		if leaf.headingKind == "" {
			if heading != "" {
				t.Errorf("%s leaf unexpectedly supplies Heading @%s", leaf.name, heading)
			}
		} else if !queryKinds[heading][leaf.headingKind] {
			t.Errorf("%s leaf Heading @%s is not the direct %s query", leaf.name, heading, leaf.headingKind)
		}
	}
}

func TestHelperSourceUsesExplicitEditDetails(t *testing.T) {
	t.Parallel()

	actions := rawActionBodies(t, helperSource(t), editItemsAction)
	want := map[string]string{
		"start":        "startAction",
		"reminderTime": "reminderTimeAction",
		"deadline":     "deadlineAction",
		"tags":         "tagsAction",
		"notes":        "notesAction",
		"checklist":    "checklistAction",
		"title":        "titleAction",
		"status":       "statusAction",
	}
	for detail, actionKey := range want {
		found := false
		for _, action := range actions {
			if !strings.Contains(action, `"detail": "`+detail+`"`) {
				continue
			}
			found = true
			if !hasSourceKey(action, actionKey) || !hasSourceKey(action, detail) {
				t.Errorf("Edit Items detail %q must include %q and %q", detail, actionKey, detail)
			}
			if !strings.Contains(action, `"`+actionKey+`": "set"`) {
				t.Errorf("Edit Items detail %q must use %s=set", detail, actionKey)
			}
		}
		if !found {
			t.Errorf("helper source has no Edit Items action for %s", detail)
		}
	}
	for _, action := range actions {
		if strings.Contains(action, `"detail": "parent"`) || strings.Contains(action, `"detail": "heading"`) {
			t.Error("project/area/heading placement must be supplied during Create To-Do, not Edit Items")
		}
	}
}

func TestHelperSourceSelectsRetryAndFreshTasksWithoutEntityIDRequery(t *testing.T) {
	t.Parallel()

	source := helperSource(t)
	targets := make(map[string]bool)
	for index, action := range rawActionBodies(t, source, editItemsAction) {
		// Title edits (finalise, rename-heading) and status edits
		// (archive-heading) target their own resolved entities, not the
		// capture selected-task variable.
		if strings.Contains(action, `"detail": "title"`) || strings.Contains(action, `"detail": "status"`) {
			continue
		}
		target := sourceAttachedVariable(action, "items")
		if target == "" {
			t.Errorf("capture Edit Items action %d has no variable-backed items input", index)
			continue
		}
		targets[target] = true
	}
	if len(targets) != 1 {
		t.Fatalf("capture Edit Items actions use %d target variables, want one common selected-task variable", len(targets))
	}
	var target string
	for name := range targets {
		target = name
		if strings.Contains(source, "@"+name+" +=") {
			t.Errorf("edit-target variable @%s is accumulated and may contain multiple tasks", name)
		}
	}

	retry := sourceIfBlock(t, source, `@pendingCount == 1`)
	retryPattern := regexp.MustCompile(`(?m)@` + regexp.QuoteMeta(target) + `\s*=\s*@([A-Za-z_][A-Za-z0-9_]*)\s*$`)
	retryMatches := retryPattern.FindAllStringSubmatch(retry.Body, -1)
	if len(retryMatches) != 1 {
		t.Fatalf("retry branch has %d direct assignments to @%s, want exactly one", len(retryMatches), target)
	}
	pendingVariable := retryMatches[0][1]
	if len(sourceThingsRawActions(t, retry.Body)) != 0 {
		t.Error("retry branch re-queries Things instead of using the pending query result directly")
	}
	pendingQuery := sourceEntityQueryForVariable(t, source, pendingVariable)
	assertSourcePendingTitleQuery(t, pendingQuery, "retry pending-item")

	fresh := sourceIfBlock(t, source, `@pendingCount == 0`)
	freshQueries := sourceRawActions(t, fresh.Body, itemEntityAction)
	if len(freshQueries) != 1 {
		t.Fatalf("fresh branch has %d Things item queries, want one pending-title query after Create", len(freshQueries))
	}
	freshQuery := freshQueries[0]
	assertSourcePendingTitleQuery(t, freshQuery, "fresh selected-task")

	queryUUIDs := sourceStringValues(freshQuery.Body, "UUID")
	if len(queryUUIDs) != 1 {
		t.Fatalf("fresh selected-task query has UUIDs %#v, want exactly one", queryUUIDs)
	}
	var targetBindings []sourceRawAction
	for _, binding := range sourceRawActions(t, fresh.Body, "is.workflow.actions.setvariable") {
		if values := sourceStringValues(binding.Body, "WFVariableName"); len(values) == 1 && values[0] == target {
			targetBindings = append(targetBindings, binding)
		}
	}
	if len(targetBindings) != 1 {
		t.Fatalf("fresh branch has %d explicit output bindings to @%s, want exactly one", len(targetBindings), target)
	}
	boundUUIDs := sourceStringValues(targetBindings[0].Body, "OutputUUID")
	if len(boundUUIDs) != 1 || !strings.EqualFold(boundUUIDs[0], queryUUIDs[0]) {
		t.Errorf("fresh @%s binding uses output UUID %#v, want fresh query UUID %s", target, boundUUIDs, queryUUIDs[0])
	}
	createUUIDs := make(map[string]bool)
	for _, create := range sourceRawActions(t, fresh.Body, addTodoAction) {
		for _, uuid := range sourceStringValues(create.Body, "UUID") {
			createUUIDs[strings.ToLower(uuid)] = true
		}
	}
	if len(boundUUIDs) == 1 && createUUIDs[strings.ToLower(boundUUIDs[0])] {
		t.Error("fresh selected-task variable is bound to a Create output rather than the independent pending-title query")
	}

	countPattern := regexp.MustCompile(`(?m)@([A-Za-z_][A-Za-z0-9_]*)\s*=\s*count\(@` + regexp.QuoteMeta(target) + `\)`)
	countMatches := countPattern.FindAllStringSubmatchIndex(source, -1)
	if len(countMatches) != 1 {
		t.Fatalf("selected-task variable @%s has %d count checks, want exactly 1", target, len(countMatches))
	}
	countVariable := source[countMatches[0][2]:countMatches[0][3]]
	countStart := countMatches[0][0]
	if countStart <= retry.Close || countStart <= fresh.Close {
		t.Error("selected-task count runs before both retry and fresh selection branches have completed")
	}
	guard := sourceIfBlock(t, source[countStart:], `@`+countVariable+` != 1`)
	guardClose := countStart + guard.Close

	derivePattern := regexp.MustCompile(`(?m)for\s+targetTask\s+in\s+@` + regexp.QuoteMeta(target) + `\s*\{`)
	derive := derivePattern.FindStringIndex(source[countStart:])
	if derive == nil {
		t.Fatalf("targetId is not derived by iterating the exactly-one @%s selection", target)
	}
	deriveStart := countStart + derive[0]
	if deriveStart <= guardClose {
		t.Error("targetId is derived before the exactly-one selected-task guard")
	}
	deriveBlock := sourceIfLikeBlockAt(t, source, countStart+derive[1]-1)
	if !strings.Contains(deriveBlock.Body, `@targetId = "{@targetTask['id']}"`) {
		t.Error("targetId is not derived from the sole selected Things task")
	}
	if strings.Contains(freshQuery.Body, "targetId") || strings.Contains(pendingQuery.Body, "targetId") {
		t.Error("selected-task query depends on an entity-derived targetId")
	}
}

// TestCompiledHelperWorkflow validates Cherri's output, where several previous
// regressions were invisible in the source. It is opt-in because Cherri is a
// maintainer tool rather than an application dependency:
//
//	THINGS_INDEX_UNSIGNED_SHORTCUT=/tmp/ThingsIndex\ Helper_unsigned.shortcut \
//	  go test ./shortcuts -run TestCompiledHelperWorkflow
//
// A JSON rendering of the plist is accepted as well.
func TestCompiledHelperWorkflow(t *testing.T) {
	path := os.Getenv("THINGS_INDEX_UNSIGNED_SHORTCUT")
	if path == "" {
		t.Skip("set THINGS_INDEX_UNSIGNED_SHORTCUT to validate a compiled unsigned workflow")
	}

	workflow := readWorkflow(t, path)
	validateThingsActionUUIDs(t, workflow.Actions)
	validateCreateActions(t, workflow.Actions)
	validateEditActions(t, workflow.Actions)
	validateDateActions(t, workflow.Actions)
	validateDictionaryReads(t, workflow.Actions)
	validateEditInputsAreNotAccumulated(t, workflow.Actions)
	validateSelectedTaskProvenance(t, workflow.Actions)
}

type workflow struct {
	Actions []workflowAction `json:"WFWorkflowActions"`
}

type workflowAction struct {
	Identifier string         `json:"WFWorkflowActionIdentifier"`
	Parameters map[string]any `json:"WFWorkflowActionParameters"`
}

func validateThingsActionUUIDs(t *testing.T, actions []workflowAction) {
	t.Helper()

	sourceActions := sourceThingsRawActions(t, helperSource(t))
	var compiledActions []workflowAction
	seen := make(map[string]int)
	for index, action := range actions {
		if !strings.HasPrefix(action.Identifier, "com.culturedcode.ThingsMac") {
			continue
		}
		sequenceIndex := len(compiledActions)
		compiledActions = append(compiledActions, action)
		uuid, _ := action.Parameters["UUID"].(string)
		if !isFixedUUID(uuid) {
			t.Errorf("compiled Things action %d (%s) has non-fixed UUID %q", index, action.Identifier, uuid)
			continue
		}
		normalized := strings.ToLower(uuid)
		if previous, exists := seen[normalized]; exists {
			t.Errorf("compiled Things actions %d and %d reuse UUID %s", previous, sequenceIndex, uuid)
		}
		seen[normalized] = sequenceIndex
	}
	if len(compiledActions) != len(sourceActions) {
		t.Fatalf("compiled workflow has %d Things actions, source has %d", len(compiledActions), len(sourceActions))
	}
	for index := range sourceActions {
		sourceUUIDs := sourceStringValues(sourceActions[index].Body, "UUID")
		compiledUUID, _ := compiledActions[index].Parameters["UUID"].(string)
		if sourceActions[index].Identifier != compiledActions[index].Identifier || len(sourceUUIDs) != 1 || !strings.EqualFold(sourceUUIDs[0], compiledUUID) {
			t.Errorf("Things action %d source=(%s, %#v) compiled=(%s, %q); identifier/UUID sequence must be stable", index, sourceActions[index].Identifier, sourceUUIDs, compiledActions[index].Identifier, compiledUUID)
		}
	}
}

func validateCreateActions(t *testing.T, actions []workflowAction) {
	t.Helper()

	var createActions []workflowAction
	for _, action := range actions {
		if action.Identifier == addTodoAction {
			createActions = append(createActions, action)
		}
	}
	if len(createActions) != 4 {
		t.Fatalf("compiled workflow has %d Create To-Do actions, want Inbox, project, project+heading, and area branches", len(createActions))
	}

	queryKinds := compiledEntityQueryKinds(actions)
	shapes := map[string]int{"inbox": 0, "project": 0, "project_heading": 0, "area": 0}
	for i, action := range createActions {
		parameters := action.Parameters
		for _, key := range []string{"AppIntentDescriptor", "ShowWhenRun", "title"} {
			if _, ok := parameters[key]; !ok {
				t.Errorf("compiled Create To-Do action %d is missing %q", i, key)
			}
		}
		for _, key := range []string{"start", "startDate", "tags", "notes", "checklist"} {
			if _, ok := parameters[key]; ok {
				t.Errorf("compiled Create To-Do action %d includes %q instead of a typed Edit Items detail", i, key)
			}
		}
		assertDescriptor(t, i, parameters, "TAIAddTodo2")
		assertSerialization(t, i, parameters, "title", "WFTextTokenString")

		parent := attachedVariable(parameters["parent"])
		heading := attachedVariable(parameters["heading"])
		if _, exists := parameters["parent"]; exists {
			assertSerialization(t, i, parameters, "parent", "WFTextTokenAttachment")
		}
		if _, exists := parameters["heading"]; exists {
			assertSerialization(t, i, parameters, "heading", "WFTextTokenAttachment")
		}
		switch {
		case heading != "" && parent == "":
			t.Errorf("compiled Create To-Do action %d supplies Heading without Parent", i)
		case heading != "":
			if !queryKinds[parent]["project"] {
				t.Errorf("compiled heading Create action %d Parent %q is not the direct project query", i, parent)
			}
			if !queryKinds[heading]["heading"] {
				t.Errorf("compiled heading Create action %d Heading %q is not the direct heading query", i, heading)
			}
			shapes["project_heading"]++
		case parent == "":
			shapes["inbox"]++
		case queryKinds[parent]["project"]:
			shapes["project"]++
		case queryKinds[parent]["area"]:
			shapes["area"]++
		default:
			t.Errorf("compiled parent-only Create action %d uses %q without direct project/area query provenance", i, parent)
		}
	}
	for shape, count := range shapes {
		if count != 1 {
			t.Errorf("compiled workflow has %d %s Create branches, want 1", count, shape)
		}
	}
}

func compiledEntityQueryKinds(actions []workflowAction) map[string]map[string]bool {
	outputKinds := make(map[string]map[string]bool)
	for _, action := range actions {
		if action.Identifier != "com.culturedcode.ThingsMac.TAIItemEntity" {
			continue
		}
		uuid, _ := action.Parameters["UUID"].(string)
		if uuid == "" {
			continue
		}
		kinds := make(map[string]bool)
		walkWorkflowValue(action.Parameters, func(value map[string]any) {
			identifier, _ := value["identifier"].(string)
			wireValue, _ := value["value"].(string)
			if identifier == wireValue && (identifier == "project" || identifier == "area" || identifier == "heading") {
				kinds[identifier] = true
			}
		})
		if len(kinds) > 0 {
			outputKinds[uuid] = kinds
		}
	}

	variableKinds := make(map[string]map[string]bool)
	for _, action := range actions {
		if action.Identifier != "is.workflow.actions.setvariable" {
			continue
		}
		name, _ := action.Parameters["WFVariableName"].(string)
		kinds := outputKinds[attachedOutputUUID(action.Parameters["WFInput"])]
		if name == "" || len(kinds) == 0 {
			continue
		}
		if variableKinds[name] == nil {
			variableKinds[name] = make(map[string]bool)
		}
		for kind := range kinds {
			variableKinds[name][kind] = true
		}
	}
	return variableKinds
}

func validateEditActions(t *testing.T, actions []workflowAction) {
	t.Helper()

	want := map[string]struct {
		actionKey string
		valueKey  string
		variable  string
	}{
		"reminderTime": {"reminderTimeAction", "reminderTime", "reminderTimeValue"},
		"deadline":     {"deadlineAction", "deadline", "deadlineValue"},
		"tags":         {"tagsAction", "tags", "tags"},
		"notes":        {"notesAction", "notes", "notes"},
		"checklist":    {"checklistAction", "checklist", "checklist"},
		"title":        {"titleAction", "title", "finalTitle"},
		"status":       {"statusAction", "status", ""},
	}
	found := make(map[string]bool)
	startEnums := make(map[string]bool)
	for i, action := range actions {
		if action.Identifier != editItemsAction {
			continue
		}
		detail, _ := action.Parameters["detail"].(string)
		if detail != "" {
			assertDescriptor(t, i, action.Parameters, "TAIEditItems")
		}
		if detail == "parent" || detail == "heading" {
			t.Errorf("compiled Edit Items action %d performs placement detail %q; placement belongs in Create To-Do", i, detail)
		}
		if detail == "start" {
			found[detail] = true
			if action.Parameters["startAction"] != "set" {
				t.Errorf("compiled Edit Items action %d does not set start", i)
			}
			start, ok := action.Parameters["start"].(string)
			if !ok {
				t.Errorf("compiled Edit Items action %d has dynamically serialized Start: %#v", i, action.Parameters["start"])
			} else {
				if start != "onDate" && start != "anytime" && start != "someday" {
					t.Errorf("compiled Edit Items action %d has invalid Start enum %q", i, start)
				}
				startEnums[start] = true
				if start == "onDate" {
					assertSerialization(t, i, action.Parameters, "startDate", "WFTextTokenString")
					assertAttachedVariable(t, i, action.Parameters["startDate"], "startDateValue")
				} else if _, exists := action.Parameters["startDate"]; exists {
					t.Errorf("compiled Edit Items action %d supplies startDate for %s", i, start)
				}
			}
			assertSerialization(t, i, action.Parameters, "items", "WFTextTokenAttachment")
			continue
		}
		expectation, tracked := want[detail]
		if !tracked {
			continue
		}
		found[detail] = true
		if action.Parameters[expectation.actionKey] != "set" {
			t.Errorf("compiled Edit Items action %d does not set %s", i, detail)
		}
		if expectation.variable != "" {
			assertAttachedVariable(t, i, action.Parameters[expectation.valueKey], expectation.variable)
		}
		assertSerialization(t, i, action.Parameters, "items", "WFTextTokenAttachment")
	}
	for detail := range want {
		if !found[detail] {
			t.Errorf("compiled workflow has no Edit Items action for %s", detail)
		}
	}

	// Scheduling uses explicit Start edits. Every compiled value must remain a
	// native literal rather than a text attachment.
	for _, start := range []string{"onDate", "anytime", "someday"} {
		if !startEnums[start] {
			t.Errorf("compiled workflow has no native scheduling path for %s", start)
		}
	}
}

func validateDateActions(t *testing.T, actions []workflowAction) {
	t.Helper()

	type dateAdjustment struct {
		base, magnitude, unit string
	}
	want := map[dateAdjustment]bool{
		{base: "todayStart", magnitude: "startDayOffset", unit: "days"}:          false,
		{base: "startDateValue", magnitude: "reminderMinuteOffset", unit: "min"}: false,
		{base: "todayStart", magnitude: "deadlineDayOffset", unit: "days"}:       false,
	}
	for _, action := range actions {
		if action.Identifier != "is.workflow.actions.adjustdate" || action.Parameters["WFAdjustOperation"] != "Add" {
			continue
		}
		base := attachedVariable(action.Parameters["WFDate"])
		duration, _ := action.Parameters["WFDuration"].(map[string]any)
		value, _ := duration["Value"].(map[string]any)
		unit, _ := value["Unit"].(string)
		magnitude, _ := value["Magnitude"].(map[string]any)
		magnitudeName, _ := magnitude["VariableName"].(string)
		adjustment := dateAdjustment{base: base, magnitude: magnitudeName, unit: unit}
		if _, ok := want[adjustment]; ok {
			want[adjustment] = true
		}
	}
	for adjustment, found := range want {
		if !found {
			t.Errorf("compiled workflow lacks date adjustment base=%s magnitude=%s unit=%s", adjustment.base, adjustment.magnitude, adjustment.unit)
		}
	}
}

func validateDictionaryReads(t *testing.T, actions []workflowAction) {
	t.Helper()

	protocolKeys := map[string]bool{
		"schemaVersion": true, "operation": true, "requestId": true, "task": true, "project": true,
		"title": true, "notes": true, "destination": true, "kind": true, "name": true, "heading": true,
		"start": true, "startDate": true, "evening": true, "reminderTime": true, "deadline": true,
		"startDayOffset": true, "reminderMinuteOffset": true, "deadlineDayOffset": true,
		"tags": true, "checklist": true,
	}
	dictionaryKeys := make(map[string]bool)
	for actionIndex, action := range actions {
		walkWorkflowValue(action.Parameters, func(value map[string]any) {
			typeName, _ := value["Type"].(string)
			if typeName == "WFPropertyVariableAggrandizement" {
				property, _ := value["PropertyName"].(string)
				if protocolKeys[property] {
					t.Errorf("compiled action %d reads protocol key %q as a generic object property; use a dictionary-value read", actionIndex, property)
				}
			}
			if key, _ := value["DictionaryKey"].(string); key != "" {
				dictionaryKeys[key] = true
			}
			if key, _ := value["WFDictionaryKey"].(string); key != "" {
				dictionaryKeys[key] = true
			}
		})
	}
	for _, key := range []string{"task", "destination"} {
		if !dictionaryKeys[key] {
			t.Errorf("compiled workflow has no typed dictionary read for nested key %q", key)
		}
	}
}

func validateEditInputsAreNotAccumulated(t *testing.T, actions []workflowAction) {
	t.Helper()

	protectedVariables := make(map[string]bool)
	for _, action := range actions {
		if action.Identifier != editItemsAction {
			continue
		}
		detail, _ := action.Parameters["detail"].(string)
		if detail == "parent" {
			if name := attachedVariable(action.Parameters["parent"]); name != "" {
				protectedVariables[name] = true
			}
		}
		if detail != "" && detail != "title" {
			if name := attachedVariable(action.Parameters["items"]); name != "" {
				protectedVariables[name] = true
			}
		}
	}
	for _, action := range actions {
		if action.Identifier != "is.workflow.actions.appendvariable" {
			continue
		}
		name, _ := action.Parameters["WFVariableName"].(string)
		if protectedVariables[name] {
			t.Errorf("compiled Edit Items input variable %q is append-accumulated across branches", name)
		}
	}
}

func validateSelectedTaskProvenance(t *testing.T, actions []workflowAction) {
	t.Helper()

	targets := make(map[string]bool)
	firstEditIndex := len(actions)
	for index, action := range actions {
		if action.Identifier != editItemsAction {
			continue
		}
		detail, _ := action.Parameters["detail"].(string)
		if detail == "" || detail == "title" || detail == "status" {
			continue
		}
		name := attachedVariable(action.Parameters["items"])
		if name == "" {
			t.Errorf("compiled capture Edit Items action %d has no variable-backed items input", index)
			continue
		}
		targets[name] = true
		if index < firstEditIndex {
			firstEditIndex = index
		}
	}
	if len(targets) != 1 {
		t.Fatalf("compiled capture Edit Items actions use %d target variables, want one selected-task variable", len(targets))
	}
	var target string
	for name := range targets {
		target = name
	}

	actionIndexByUUID := make(map[string]int)
	for index, action := range actions {
		if uuid, _ := action.Parameters["UUID"].(string); uuid != "" {
			actionIndexByUUID[uuid] = index
		}
	}

	type targetBinding struct {
		index         int
		inputVariable string
		outputUUID    string
	}
	var bindings []targetBinding
	for index, action := range actions {
		if action.Identifier != "is.workflow.actions.setvariable" || action.Parameters["WFVariableName"] != target {
			continue
		}
		if action.Parameters["WFInput"] == nil { // Cherri's typed-array declaration.
			continue
		}
		bindings = append(bindings, targetBinding{
			index:         index,
			inputVariable: attachedVariable(action.Parameters["WFInput"]),
			outputUUID:    attachedOutputUUID(action.Parameters["WFInput"]),
		})
	}
	if len(bindings) != 2 {
		t.Fatalf("compiled selected-task variable %q has %d meaningful assignments, want retry-direct and fresh-query bindings", target, len(bindings))
	}

	var retry, fresh *targetBinding
	for index := range bindings {
		binding := &bindings[index]
		switch {
		case binding.inputVariable != "" && binding.outputUUID == "":
			retry = binding
		case binding.inputVariable == "" && binding.outputUUID != "":
			fresh = binding
		default:
			t.Errorf("compiled selected-task assignment %d has ambiguous input variable=%q outputUUID=%q", binding.index, binding.inputVariable, binding.outputUUID)
		}
	}
	if retry == nil || fresh == nil {
		t.Fatalf("compiled selected-task bindings do not contain one retry-direct and one fresh-query assignment: %#v", bindings)
	}
	if !compiledIfBranchContains(actions, retry.index, "pendingCount", 1) {
		t.Errorf("retry selected-task assignment %d is not inside pendingCount == 1", retry.index)
	}
	if !compiledIfBranchContains(actions, fresh.index, "pendingCount", 0) {
		t.Errorf("fresh selected-task assignment %d is not inside pendingCount == 0", fresh.index)
	}

	pendingQueryIndex, pendingQuery := compiledEntityQueryForVariable(t, actions, actionIndexByUUID, retry.inputVariable)
	assertCompiledPendingTitleQuery(t, pendingQueryIndex, pendingQuery, "retry pending-item")
	if pendingQueryIndex >= retry.index {
		t.Errorf("retry binding at action %d precedes its pending query at %d", retry.index, pendingQueryIndex)
	}

	freshQueryIndex, exists := actionIndexByUUID[fresh.outputUUID]
	if !exists || actions[freshQueryIndex].Identifier != itemEntityAction {
		t.Fatalf("fresh selected-task binding references non-Things query UUID %q", fresh.outputUUID)
	}
	assertCompiledPendingTitleQuery(t, freshQueryIndex, actions[freshQueryIndex], "fresh selected-task")
	if freshQueryIndex >= fresh.index {
		t.Errorf("fresh selected-task binding at action %d precedes its query at %d", fresh.index, freshQueryIndex)
	}
	if !compiledIfBranchContains(actions, freshQueryIndex, "pendingCount", 0) {
		t.Errorf("fresh selected-task query %d is not inside pendingCount == 0", freshQueryIndex)
	}
	for index, action := range actions {
		if action.Identifier != addTodoAction {
			continue
		}
		if !compiledIfBranchContains(actions, index, "pendingCount", 0) {
			t.Errorf("Create To-Do action %d is outside the fresh pendingCount == 0 branch", index)
		}
		if action.Parameters["UUID"] == fresh.outputUUID {
			t.Error("fresh selected-task binding uses a Create output instead of the independent pending-title query")
		}
	}

	var countIndices []int
	for index, action := range actions {
		if action.Identifier != "is.workflow.actions.count" {
			continue
		}
		input := action.Parameters["Input"]
		if input == nil {
			input = action.Parameters["WFInput"]
		}
		if attachedVariable(input) == target {
			countIndices = append(countIndices, index)
		}
	}
	if len(countIndices) != 1 {
		t.Fatalf("compiled selected-task variable %q has %d count checks, want exactly 1", target, len(countIndices))
	}
	countIndex := countIndices[0]
	if countIndex <= retry.index || countIndex <= fresh.index || countIndex >= firstEditIndex {
		t.Errorf("compiled exactly-one count for %q is at action %d, not after both selections and before edits", target, countIndex)
	}

	countUUID, _ := actions[countIndex].Parameters["UUID"].(string)
	countVariable := ""
	countBindingIndex := -1
	for index, action := range actions {
		if action.Identifier == "is.workflow.actions.setvariable" && attachedOutputUUID(action.Parameters["WFInput"]) == countUUID {
			countVariable, _ = action.Parameters["WFVariableName"].(string)
			countBindingIndex = index
		}
	}
	guardIndex := -1
	for index, action := range actions {
		if action.Identifier != "is.workflow.actions.conditional" || !workflowNumberEquals(action.Parameters["WFControlFlowMode"], 0) {
			continue
		}
		if conditionalInputVariable(action.Parameters) == countVariable && workflowNumberEquals(action.Parameters["WFNumberValue"], 1) && workflowNumberEquals(action.Parameters["WFCondition"], 5) {
			guardIndex = index
			break
		}
	}
	if countVariable == "" || countBindingIndex <= countIndex || guardIndex <= countBindingIndex {
		t.Errorf("compiled selected-task count lacks a following != 1 guard (count=%d binding=%d guard=%d)", countIndex, countBindingIndex, guardIndex)
	}
	guardCloseIndex := compiledControlFlowClose(actions, guardIndex)

	derivedTargetIDIndex := -1
	for index, action := range actions {
		if action.Identifier != "is.workflow.actions.setvariable" || action.Parameters["WFVariableName"] != "targetId" {
			continue
		}
		producerIndex, exists := actionIndexByUUID[attachedOutputUUID(action.Parameters["WFInput"])]
		if !exists {
			continue
		}
		derivedFromID := false
		derivedFromTargetTask := false
		walkWorkflowValue(actions[producerIndex].Parameters, func(value map[string]any) {
			if value["Type"] == "WFPropertyVariableAggrandizement" && value["PropertyName"] == "id" {
				derivedFromID = true
			}
			if value["VariableName"] == "targetTask" {
				derivedFromTargetTask = true
			}
		})
		if derivedFromID && derivedFromTargetTask {
			derivedTargetIDIndex = index
		}
	}
	if guardCloseIndex < 0 || derivedTargetIDIndex <= guardCloseIndex || derivedTargetIDIndex >= firstEditIndex {
		t.Errorf("compiled targetId derivation is at action %d, want after exactly-one guard close %d and before first edit %d", derivedTargetIDIndex, guardCloseIndex, firstEditIndex)
	}
}

func compiledEntityQueryForVariable(t *testing.T, actions []workflowAction, actionIndexByUUID map[string]int, variable string) (int, workflowAction) {
	t.Helper()
	var queryIndices []int
	for _, action := range actions {
		if action.Identifier != "is.workflow.actions.setvariable" || action.Parameters["WFVariableName"] != variable {
			continue
		}
		if index, exists := actionIndexByUUID[attachedOutputUUID(action.Parameters["WFInput"])]; exists && actions[index].Identifier == itemEntityAction {
			queryIndices = append(queryIndices, index)
		}
	}
	if len(queryIndices) != 1 {
		t.Fatalf("compiled variable %q has Things entity query indices %#v, want exactly one", variable, queryIndices)
	}
	return queryIndices[0], actions[queryIndices[0]]
}

func assertCompiledPendingTitleQuery(t *testing.T, index int, query workflowAction, context string) {
	t.Helper()
	properties := make(map[string]int)
	variables := make(map[string]bool)
	todoIdentifier, todoValue := false, false
	walkWorkflowValue(query.Parameters, func(value map[string]any) {
		if property, _ := value["Property"].(string); property != "" {
			properties[property]++
		}
		if variable, _ := value["VariableName"].(string); variable != "" {
			variables[variable] = true
		}
		if value["identifier"] == "todo" {
			todoIdentifier = true
		}
		if value["value"] == "todo" {
			todoValue = true
		}
	})
	if len(properties) != 2 || properties["type"] != 1 || properties["title"] != 1 {
		t.Errorf("compiled %s query %d filters by %#v, want exactly type and title", context, index, properties)
	}
	if !todoIdentifier || !todoValue || len(variables) != 1 || !variables["pendingTitle"] {
		t.Errorf("compiled %s query %d does not select type=todo and exact pendingTitle", context, index)
	}
	if variables["targetId"] {
		t.Errorf("compiled %s query %d depends on entity-derived targetId", context, index)
	}
	if query.Parameters["WFContentItemLimitEnabled"] != true || !workflowNumberEquals(query.Parameters["WFContentItemLimitNumber"], 2) {
		t.Errorf("compiled %s query %d has limit enabled=%#v number=%#v, want enabled and 2", context, index, query.Parameters["WFContentItemLimitEnabled"], query.Parameters["WFContentItemLimitNumber"])
	}
}

func compiledIfBranchContains(actions []workflowAction, targetIndex int, variable string, number float64) bool {
	for openIndex, action := range actions {
		mode, modeOK := action.Parameters["WFControlFlowMode"].(float64)
		conditionNumber, numberOK := action.Parameters["WFNumberValue"].(float64)
		if action.Identifier != "is.workflow.actions.conditional" || !modeOK || mode != 0 {
			continue
		}
		if conditionalInputVariable(action.Parameters) != variable || !numberOK || conditionNumber != number || !workflowNumberEquals(action.Parameters["WFCondition"], 4) {
			continue
		}
		group, _ := action.Parameters["GroupingIdentifier"].(string)
		for closeIndex := openIndex + 1; closeIndex < len(actions); closeIndex++ {
			candidate := actions[closeIndex]
			candidateGroup, _ := candidate.Parameters["GroupingIdentifier"].(string)
			if candidate.Identifier != "is.workflow.actions.conditional" || candidateGroup != group {
				continue
			}
			candidateMode, _ := candidate.Parameters["WFControlFlowMode"].(float64)
			if candidateMode == 1 || candidateMode == 2 {
				if targetIndex > openIndex && targetIndex < closeIndex {
					return true
				}
				break
			}
		}
	}
	return false
}

func compiledControlFlowClose(actions []workflowAction, openIndex int) int {
	if openIndex < 0 || openIndex >= len(actions) {
		return -1
	}
	group, _ := actions[openIndex].Parameters["GroupingIdentifier"].(string)
	for index := openIndex + 1; index < len(actions); index++ {
		action := actions[index]
		if action.Identifier == actions[openIndex].Identifier && action.Parameters["GroupingIdentifier"] == group && workflowNumberEquals(action.Parameters["WFControlFlowMode"], 2) {
			return index
		}
	}
	return -1
}

func conditionalInputVariable(parameters map[string]any) string {
	input, _ := parameters["WFInput"].(map[string]any)
	variable, _ := input["Variable"].(map[string]any)
	value, _ := variable["Value"].(map[string]any)
	name, _ := value["VariableName"].(string)
	return name
}

func workflowNumberEquals(value any, want float64) bool {
	switch value := value.(type) {
	case float64:
		return value == want
	case float32:
		return float64(value) == want
	case int:
		return float64(value) == want
	case int64:
		return float64(value) == want
	default:
		return false
	}
}

func walkWorkflowValue(value any, visit func(map[string]any)) {
	switch value := value.(type) {
	case map[string]any:
		visit(value)
		for _, child := range value {
			walkWorkflowValue(child, visit)
		}
	case []any:
		for _, child := range value {
			walkWorkflowValue(child, visit)
		}
	}
}

func helperSource(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("ThingsIndex Helper.cherri")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

type sourceBlock struct {
	Body        string
	Open, Close int
}

type sourceRawAction struct {
	Identifier string
	Variable   string
	Body       string
	Start, End int
}

func sourceIfBlock(t *testing.T, source, condition string) sourceBlock {
	t.Helper()
	pattern := regexp.MustCompile(`if\s+` + regexp.QuoteMeta(condition) + `\s*\{`)
	match := pattern.FindStringIndex(source)
	if match == nil {
		t.Fatalf("helper source has no if branch for %s", condition)
	}
	open := match[1] - 1
	close := matchingSourceBrace(t, source, open)
	return sourceBlock{Body: source[open+1 : close], Open: open, Close: close}
}

func sourceIfLikeBlockAt(t *testing.T, source string, open int) sourceBlock {
	t.Helper()
	if open < 0 || open >= len(source) || source[open] != '{' {
		t.Fatalf("expected source block opening brace at byte %d", open)
	}
	close := matchingSourceBrace(t, source, open)
	return sourceBlock{Body: source[open+1 : close], Open: open, Close: close}
}

func sourceIfElseBlocks(t *testing.T, source, condition string) (sourceBlock, sourceBlock) {
	t.Helper()
	ifBlock := sourceIfBlock(t, source, condition)
	position := skipSourceTrivia(source, ifBlock.Close+1)
	if !strings.HasPrefix(source[position:], "else") {
		t.Fatalf("helper branch for %s has no mutually exclusive else leaf", condition)
	}
	position = skipSourceTrivia(source, position+len("else"))
	if position >= len(source) || source[position] != '{' {
		t.Fatalf("helper branch for %s has malformed else leaf", condition)
	}
	close := matchingSourceBrace(t, source, position)
	return ifBlock, sourceBlock{Body: source[position+1 : close], Open: position, Close: close}
}

func skipSourceTrivia(source string, position int) int {
	for position < len(source) {
		switch source[position] {
		case ' ', '\t', '\r', '\n':
			position++
			continue
		}
		if strings.HasPrefix(source[position:], "//") {
			if newline := strings.IndexByte(source[position:], '\n'); newline >= 0 {
				position += newline + 1
				continue
			}
			return len(source)
		}
		return position
	}
	return position
}

func sourceAssignedRawActions(t *testing.T, source, identifier string) []sourceRawAction {
	t.Helper()
	pattern := regexp.MustCompile(`(?m)@([A-Za-z_][A-Za-z0-9_]*)\s*=\s*rawAction\("` + regexp.QuoteMeta(identifier) + `",\s*\{`)
	var actions []sourceRawAction
	for _, match := range pattern.FindAllStringSubmatchIndex(source, -1) {
		open := match[1] - 1
		close := matchingSourceBrace(t, source, open)
		actions = append(actions, sourceRawAction{
			Identifier: identifier,
			Variable:   source[match[2]:match[3]],
			Body:       source[open : close+1],
			Start:      match[0],
			End:        close + 1,
		})
	}
	return actions
}

func sourceRawActions(t *testing.T, source, identifier string) []sourceRawAction {
	t.Helper()
	needle := `rawAction("` + identifier + `", {`
	var actions []sourceRawAction
	for offset := 0; ; {
		relative := strings.Index(source[offset:], needle)
		if relative < 0 {
			return actions
		}
		start := offset + relative
		open := start + len(needle) - 1
		close := matchingSourceBrace(t, source, open)
		actions = append(actions, sourceRawAction{
			Identifier: identifier,
			Body:       source[open : close+1],
			Start:      start,
			End:        close + 1,
		})
		offset = close + 1
	}
}

func sourceThingsRawActions(t *testing.T, source string) []sourceRawAction {
	t.Helper()
	pattern := regexp.MustCompile(`rawAction\("(com\.culturedcode\.ThingsMac[^"]*)"\s*,\s*\{`)
	var actions []sourceRawAction
	for _, match := range pattern.FindAllStringSubmatchIndex(source, -1) {
		open := match[1] - 1
		close := matchingSourceBrace(t, source, open)
		actions = append(actions, sourceRawAction{
			Identifier: source[match[2]:match[3]],
			Body:       source[open : close+1],
			Start:      match[0],
			End:        close + 1,
		})
	}
	return actions
}

func sourceStringValues(body, key string) []string {
	pattern := regexp.MustCompile(`"` + regexp.QuoteMeta(key) + `"\s*:\s*"([^"]*)"`)
	matches := pattern.FindAllStringSubmatch(body, -1)
	values := make([]string, 0, len(matches))
	for _, match := range matches {
		values = append(values, match[1])
	}
	return values
}

func isFixedUUID(value string) bool {
	return regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(value)
}

func rawActionBodies(t *testing.T, source, identifier string) []string {
	t.Helper()
	actions := sourceRawActions(t, source, identifier)
	bodies := make([]string, 0, len(actions))
	for _, action := range actions {
		bodies = append(bodies, action.Body)
	}
	return bodies
}

func hasSourceKey(body, key string) bool {
	return strings.Contains(body, `"`+key+`":`)
}

func sourceAttachedVariable(body, key string) string {
	pattern := regexp.MustCompile(`"` + regexp.QuoteMeta(key) + `"\s*:\s*"\$\{@([A-Za-z_][A-Za-z0-9_]*)\}"`)
	match := pattern.FindStringSubmatch(body)
	if len(match) != 2 {
		return ""
	}
	return match[1]
}

func sourceEntityQueryForVariable(t *testing.T, source, variable string) sourceRawAction {
	t.Helper()
	var outputUUIDs []string
	for _, binding := range sourceRawActions(t, source, "is.workflow.actions.setvariable") {
		names := sourceStringValues(binding.Body, "WFVariableName")
		if len(names) != 1 || names[0] != variable {
			continue
		}
		outputUUIDs = append(outputUUIDs, sourceStringValues(binding.Body, "OutputUUID")...)
	}
	if len(outputUUIDs) != 1 {
		t.Fatalf("source variable @%s has Things output UUIDs %#v, want exactly one", variable, outputUUIDs)
	}
	for _, query := range sourceRawActions(t, source, itemEntityAction) {
		uuids := sourceStringValues(query.Body, "UUID")
		if len(uuids) == 1 && strings.EqualFold(uuids[0], outputUUIDs[0]) {
			return query
		}
	}
	t.Fatalf("source variable @%s references missing Things entity UUID %s", variable, outputUUIDs[0])
	return sourceRawAction{}
}

func assertSourcePendingTitleQuery(t *testing.T, query sourceRawAction, context string) {
	t.Helper()
	properties := regexp.MustCompile(`"Property"\s*:\s*"([^"]+)"`).FindAllStringSubmatch(query.Body, -1)
	counts := make(map[string]int)
	for _, property := range properties {
		counts[property[1]]++
	}
	if len(counts) != 2 || counts["type"] != 1 || counts["title"] != 1 {
		t.Errorf("%s query filters by %#v, want exactly type and title", context, counts)
	}
	if !strings.Contains(query.Body, `"identifier": "todo"`) || !strings.Contains(query.Body, `"value": "todo"`) {
		t.Errorf("%s query does not constrain type to Things to-do", context)
	}
	if !strings.Contains(query.Body, `"String": "{@pendingTitle}"`) {
		t.Errorf("%s query does not match the exact worker-generated @pendingTitle", context)
	}
	references := regexp.MustCompile(`\{@([A-Za-z_][A-Za-z0-9_]*)`).FindAllStringSubmatch(query.Body, -1)
	referenceCounts := make(map[string]int)
	for _, reference := range references {
		referenceCounts[reference[1]]++
	}
	if len(referenceCounts) != 1 || referenceCounts["pendingTitle"] != 1 {
		t.Errorf("%s query references %#v, want only @pendingTitle", context, referenceCounts)
	}
	if !strings.Contains(query.Body, `"WFContentItemLimitEnabled": true`) || !strings.Contains(query.Body, `"WFContentItemLimitNumber": 2`) {
		t.Errorf("%s query must be limited to two so duplicate pending titles are detectable", context)
	}
}

func sourceEntityQueryKinds(t *testing.T, source string) map[string]map[string]bool {
	t.Helper()
	uuidKinds := make(map[string]map[string]bool)
	for _, query := range sourceRawActions(t, source, itemEntityAction) {
		uuids := sourceStringValues(query.Body, "UUID")
		if len(uuids) != 1 {
			continue
		}
		kinds := make(map[string]bool)
		for _, kind := range []string{"project", "area", "heading"} {
			if strings.Contains(query.Body, `"identifier": "`+kind+`"`) && strings.Contains(query.Body, `"value": "`+kind+`"`) {
				kinds[kind] = true
			}
		}
		if len(kinds) > 0 {
			uuidKinds[strings.ToLower(uuids[0])] = kinds
		}
	}

	result := make(map[string]map[string]bool)
	for _, binding := range sourceRawActions(t, source, "is.workflow.actions.setvariable") {
		names := sourceStringValues(binding.Body, "WFVariableName")
		outputUUIDs := sourceStringValues(binding.Body, "OutputUUID")
		if len(names) != 1 || len(outputUUIDs) != 1 {
			continue
		}
		for kind := range uuidKinds[strings.ToLower(outputUUIDs[0])] {
			if result[names[0]] == nil {
				result[names[0]] = make(map[string]bool)
			}
			result[names[0]][kind] = true
		}
	}
	return result
}

func matchingSourceBrace(t *testing.T, source string, start int) int {
	t.Helper()
	depth, quote, escaped := 0, byte(0), false
	for i := start; i < len(source); i++ {
		character := source[i]
		if quote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if character == '\\' {
				escaped = true
				continue
			}
			if character == quote {
				quote = 0
			}
			continue
		}
		if character == '/' && i+1 < len(source) && source[i+1] == '/' {
			if newline := strings.IndexByte(source[i+2:], '\n'); newline >= 0 {
				i += newline + 2
				continue
			}
			break
		}
		if character == '"' || character == '\'' {
			quote = character
			continue
		}
		switch character {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	t.Fatalf("unterminated source object at byte %d", start)
	return -1
}

func readWorkflow(t *testing.T, path string) workflow {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Ext(path) != ".json" {
		plutil, err := exec.LookPath("plutil")
		if err != nil {
			t.Fatal("plutil is required to inspect an unsigned Shortcut plist")
		}
		command := exec.Command(plutil, "-convert", "json", "-o", "-", path)
		data, err = command.CombinedOutput()
		if err != nil {
			t.Fatalf("convert unsigned workflow to JSON: %v: %s", err, data)
		}
	}
	var result workflow
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decode workflow: %v", err)
	}
	return result
}

func assertDescriptor(t *testing.T, index int, parameters map[string]any, identifier string) {
	t.Helper()
	descriptor, _ := parameters["AppIntentDescriptor"].(map[string]any)
	if descriptor["AppIntentIdentifier"] != identifier || descriptor["BundleIdentifier"] != "com.culturedcode.ThingsMac" {
		t.Errorf("compiled action %d has wrong App Intent descriptor: %#v", index, descriptor)
	}
}

func assertSerialization(t *testing.T, index int, parameters map[string]any, key, want string) {
	t.Helper()
	value, ok := parameters[key].(map[string]any)
	if !ok || value["WFSerializationType"] != want {
		t.Errorf("compiled action %d parameter %q has serialization %#v, want %s", index, key, parameters[key], want)
	}
}

func assertAttachedVariable(t *testing.T, index int, value any, want string) {
	t.Helper()
	if got := attachedVariable(value); got != want {
		t.Errorf("compiled action %d attaches variable %q, want %q (value %#v)", index, got, want, value)
	}
}

func attachedVariable(value any) string {
	serialized, _ := value.(map[string]any)
	payload, _ := serialized["Value"].(map[string]any)
	if name, _ := payload["VariableName"].(string); name != "" {
		return name
	}
	attachments, _ := payload["attachmentsByRange"].(map[string]any)
	for _, attachment := range attachments {
		item, _ := attachment.(map[string]any)
		if name, _ := item["VariableName"].(string); name != "" {
			return name
		}
	}
	return ""
}

func attachedOutputUUID(value any) string {
	serialized, _ := value.(map[string]any)
	payload, _ := serialized["Value"].(map[string]any)
	uuid, _ := payload["OutputUUID"].(string)
	return uuid
}
