package server

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nejmlabs/things-index/internal/worker"
)

func TestOrganizationToolSchemasRemainFocused(t *testing.T) {
	t.Parallel()
	session, _, _ := newProjectWorkflowSession(t)
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		properties []string
	}{
		{"create_things_area", []string{"idempotency_key", "title"}},
		{"create_things_tag", []string{"idempotency_key", "parent", "title"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var found *mcp.Tool
			for _, tool := range listed.Tools {
				if tool.Name == tc.name {
					found = tool
				}
			}
			if found == nil {
				t.Fatal("tool is not registered")
			}
			encoded, err := json.Marshal(found.InputSchema)
			if err != nil {
				t.Fatal(err)
			}
			var schema struct {
				Properties map[string]any `json:"properties"`
				Required   []string       `json:"required"`
			}
			if err := json.Unmarshal(encoded, &schema); err != nil {
				t.Fatal(err)
			}
			var properties []string
			for property := range schema.Properties {
				properties = append(properties, property)
			}
			slices.Sort(properties)
			if !reflect.DeepEqual(properties, tc.properties) || !reflect.DeepEqual(schema.Required, []string{"title"}) {
				t.Fatalf("properties=%q, required=%q", properties, schema.Required)
			}
			for _, term := range []string{"explicitly", "exact", "warning", "ambiguous", "choices"} {
				if !strings.Contains(strings.ToLower(found.Description), term) {
					t.Errorf("description omits %q", term)
				}
			}
		})
	}
}

func TestOrganizationQueuePreservesFieldsIdentityAndWarnings(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"area", "tag"} {
		t.Run(kind, func(t *testing.T) {
			session, store, workerClient := newProjectWorkflowSession(t)
			ctx := context.Background()
			arguments := map[string]any{"title": "Garden", "idempotency_key": "create-" + kind}
			if kind == "tag" {
				arguments["parent"] = "Home"
			}
			params := &mcp.CallToolParams{Name: "create_things_" + kind, Arguments: arguments}
			first, err := session.CallTool(ctx, params)
			if err != nil {
				t.Fatal(err)
			}
			queued := decodeCaptureResult(t, first)
			retry, err := session.CallTool(ctx, params)
			if err != nil {
				t.Fatal(err)
			}
			if got := decodeCaptureResult(t, retry); got.RequestID != queued.RequestID {
				t.Fatalf("retry created another request: %+v", got)
			}
			lease, err := workerClient.Lease(ctx)
			if err != nil || lease == nil || lease.ID != queued.RequestID {
				t.Fatalf("lease=%+v, error=%v", lease, err)
			}
			if lease.Task.IdempotencyKey != "create-"+kind {
				t.Fatalf("idempotency key not copied to job: %+v", lease.Task)
			}
			if kind == "area" {
				request := lease.Task.CreateAreaRequest
				if request == nil || request.Title != "Garden" || request.IdempotencyKey != "create-area" || lease.Task.CreateTagRequest != nil {
					t.Fatalf("area request changed in transit: %+v", lease.Task)
				}
			} else {
				request := lease.Task.CreateTagRequest
				if request == nil || request.Title != "Garden" || request.Parent != "Home" || request.IdempotencyKey != "create-tag" || lease.Task.CreateAreaRequest != nil {
					t.Fatalf("tag request changed in transit: %+v", lease.Task)
				}
			}
			warnings := []string{"ThingsIndex warning: reused existing " + kind + "."}
			if err := workerClient.Complete(ctx, *lease, worker.Outcome{ThingsID: kind + "-id", Warnings: warnings}); err != nil {
				t.Fatal(err)
			}
			completed, err := session.CallTool(ctx, params)
			if err != nil {
				t.Fatal(err)
			}
			got := decodeCaptureResult(t, completed)
			if got.RequestID != queued.RequestID || got.Status != "created" || got.ThingsID != kind+"-id" || !reflect.DeepEqual(got.Warnings, warnings) {
				t.Fatalf("completed retry lost result: %+v", got)
			}
			jobs, err := store.ListRecent(ctx, 10)
			if err != nil || len(jobs) != 1 {
				t.Fatalf("jobs=%d, error=%v", len(jobs), err)
			}
		})
	}
}

func TestOrganizationInvalidRequestsDoNotReachQueue(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"create_things_area", map[string]any{"title": " "}},
		{"create_things_area", map[string]any{"title": "Home", "idempotency_key": "invalid\nkey"}},
		{"create_things_tag", map[string]any{"title": "Garden", "parent": " "}},
		{"create_things_tag", map[string]any{"title": "Home", "parent": "home"}},
		{"create_things_tag", map[string]any{"title": "Home, Garden"}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			session, store, _ := newProjectWorkflowSession(t)
			result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
			if err == nil && (result == nil || !result.IsError) {
				t.Fatalf("invalid request accepted: %+v", result)
			}
			jobs, err := store.ListRecent(context.Background(), 10)
			if err != nil || len(jobs) != 0 {
				t.Fatalf("invalid request enqueued: jobs=%d, error=%v", len(jobs), err)
			}
		})
	}
}

func TestExistingWriteToolsPreserveIdempotencyKeys(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"create_things_heading", map[string]any{"project": "Home", "heading": "Garden"}},
		{"archive_things_heading", map[string]any{"project": "Home", "heading": "Garden"}},
		{"rename_things_heading", map[string]any{"project": "Home", "heading": "Garden", "new_title": "Outside"}},
		{"archive_things_task", map[string]any{"id": "task-id"}},
		{"archive_things_project", map[string]any{"id": "project-id"}},
		{"create_things_project", map[string]any{"title": "Garden"}},
		{"update_things_task", map[string]any{"id": "task-id", "new_title": "Garden"}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			session, store, _ := newProjectWorkflowSession(t)
			tc.args["idempotency_key"] = "one-command"
			params := &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args}
			first, err := session.CallTool(context.Background(), params)
			if err != nil {
				t.Fatal(err)
			}
			second, err := session.CallTool(context.Background(), params)
			if err != nil {
				t.Fatal(err)
			}
			if decodeCaptureResult(t, first).RequestID != decodeCaptureResult(t, second).RequestID {
				t.Fatal("same-key retry created another request")
			}
			jobs, err := store.ListRecent(context.Background(), 10)
			if err != nil || len(jobs) != 1 || jobs[0].Task.IdempotencyKey != "one-command" {
				t.Fatalf("jobs=%+v, error=%v", jobs, err)
			}
		})
	}
}
