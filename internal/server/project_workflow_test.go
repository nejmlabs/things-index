package server

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nejmlabs/things-index/internal/capture"
	"github.com/nejmlabs/things-index/internal/queue"
	"github.com/nejmlabs/things-index/internal/worker"
)

func TestProjectWorkflowToolSchemas(t *testing.T) {
	t.Parallel()
	session, _, _ := newProjectWorkflowSession(t)
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	tools := make(map[string]*mcp.Tool, len(listed.Tools))
	for _, tool := range listed.Tools {
		tools[tool.Name] = tool
	}
	for _, name := range []string{"create_things_project", "capture_things_task"} {
		if tools[name] == nil {
			t.Fatalf("project workflow is missing tool %q", name)
		}
	}

	type schema struct {
		Description string            `json:"description"`
		Properties  map[string]schema `json:"properties"`
		Required    []string          `json:"required"`
	}
	readSchema := func(tool *mcp.Tool) schema {
		t.Helper()
		encoded, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var result schema
		if err := json.Unmarshal(encoded, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}

	project := readSchema(tools["create_things_project"])
	for _, field := range []string{"title", "area"} {
		if _, ok := project.Properties[field]; !ok {
			t.Errorf("create_things_project does not advertise %q", field)
		}
	}
	destination := readSchema(tools["capture_things_task"]).Properties["destination"]
	for _, field := range []string{"kind", "id", "name", "heading"} {
		if _, ok := destination.Properties[field]; !ok {
			t.Errorf("capture destination does not advertise %q", field)
		}
	}
	for _, required := range destination.Required {
		if required == "name" || required == "id" {
			t.Errorf("destination requires %q, preventing selection by either name or ID", required)
		}
	}
	nameDescription := strings.ToLower(destination.Properties["name"].Description)
	if !strings.Contains(nameDescription, "fuzzy") && !strings.Contains(nameDescription, "approximate") && !strings.Contains(nameDescription, "partial") {
		t.Errorf("destination name does not describe approximate project matching: %q", nameDescription)
	}
	for _, term := range []string{"exact", "missing", "ambiguous", "inbox", "notes", "warning"} {
		if !strings.Contains(nameDescription, term) {
			t.Errorf("destination name does not explain %q: %q", term, nameDescription)
		}
	}
	if strings.Contains(nameDescription, "require clarification") {
		t.Errorf("destination schema asks Index 01 for clarification: %q", nameDescription)
	}
	idDescription := strings.ToLower(destination.Properties["id"].Description)
	if !strings.Contains(idDescription, "unavailable") || !strings.Contains(idDescription, "inbox") {
		t.Errorf("destination ID does not explain unavailable-ID fallback: %q", idDescription)
	}
	captureDescription := strings.ToLower(tools["capture_things_task"].Description)
	if !strings.Contains(captureDescription, "inbox") || !strings.Contains(captureDescription, "warning") {
		t.Errorf("capture tool does not advertise Inbox fallback: %q", captureDescription)
	}
}

func TestProjectWorkflowInstructionsSupportOneCommandCapture(t *testing.T) {
	t.Parallel()
	session, _, _ := newProjectWorkflowSession(t)
	instructions := strings.ToLower(session.InitializeResult().Instructions)
	for _, expected := range []string{
		"cannot answer clarification questions",
		"missing or ambiguous projects",
		"unavailable explicit ids",
		"save the task in inbox",
		"requested project and heading in its notes",
		"do not ask a follow-up question or offer project choices",
		"only when the user explicitly asks for a new project",
		"if project creation fails, report the failure",
	} {
		if !strings.Contains(instructions, expected) {
			t.Errorf("server instructions omit %q: %q", expected, instructions)
		}
	}
}

func TestProjectWorkflowCreateThenCaptureByID(t *testing.T) {
	t.Parallel()
	session, _, workerClient := newProjectWorkflowSession(t)
	ctx := context.Background()
	created, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "create_things_project",
		Arguments: map[string]any{
			"title": "Kitchen Renovation",
			"area":  "Home",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	queuedProject := decodeCaptureResult(t, created)
	projectLease, err := workerClient.Lease(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if projectLease == nil || projectLease.ID != queuedProject.RequestID {
		t.Fatalf("unexpected project lease: %+v", projectLease)
	}
	project := projectLease.Task.CreateProjectRequest
	if project == nil || project.Title != "Kitchen Renovation" || project.Area != "Home" {
		t.Fatalf("project fields changed in transit: %+v", project)
	}
	if err := workerClient.Complete(ctx, *projectLease, worker.Outcome{ThingsID: "project-kitchen"}); err != nil {
		t.Fatal(err)
	}
	status, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "things_capture_status",
		Arguments: map[string]any{"request_id": queuedProject.RequestID},
	})
	if err != nil {
		t.Fatal(err)
	}
	projectResult := decodeCaptureResult(t, status)
	if projectResult.Status != "created" || projectResult.ThingsID != "project-kitchen" {
		t.Fatalf("project ID was not available for follow-up capture: %+v", projectResult)
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "capture_things_task",
		Arguments: map[string]any{
			"title": "Order cabinet handles",
			"destination": map[string]any{
				"kind": "project",
				"id":   projectResult.ThingsID,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	queuedTask := decodeCaptureResult(t, result)
	taskLease, err := workerClient.Lease(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if taskLease == nil || taskLease.ID != queuedTask.RequestID {
		t.Fatalf("unexpected follow-up task lease: %+v", taskLease)
	}
	destination := taskLease.Task.Destination
	if destination == nil || destination.Kind != capture.DestinationProject || destination.ID != projectResult.ThingsID || destination.Name != "" {
		t.Fatalf("ID-only project selection changed in transit: %+v", destination)
	}
}

func TestProjectWorkflowCapturePreservesDestination(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		destination capture.Destination
		notice      string
	}{
		{
			name:        "matched project name",
			destination: capture.Destination{Kind: capture.DestinationProject, Name: "kitchen reno", Heading: "Purchases"},
			notice:      `Matched project "Kitchen Renovation".`,
		},
		{
			name:        "explicit project ID",
			destination: capture.Destination{Kind: capture.DestinationProject, ID: "project-kitchen", Name: "kitchen reno", Heading: "Purchases"},
			notice:      `Matched project "Kitchen Renovation".`,
		},
		{
			name:        "ambiguous project Inbox fallback",
			destination: capture.Destination{Kind: capture.DestinationProject, Name: "kitchen", Heading: "Purchases"},
			notice:      `Saved to Inbox because project "kitchen" is ambiguous. Requested project and heading are in the task notes.`,
		},
		{
			name:        "missing project Inbox fallback",
			destination: capture.Destination{Kind: capture.DestinationProject, Name: "unknown project", Heading: "Purchases"},
			notice:      `Saved to Inbox because project "unknown project" was not found. Requested project and heading are in the task notes.`,
		},
		{
			name:        "unavailable project ID Inbox fallback",
			destination: capture.Destination{Kind: capture.DestinationProject, ID: "deleted-project", Name: "kitchen", Heading: "Purchases"},
			notice:      `Saved to Inbox because project ID "deleted-project" is unavailable. Requested project and heading are in the task notes.`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			destination := tc.destination
			session, store, workerClient := newProjectWorkflowSession(t)
			ctx := context.Background()
			result, err := session.CallTool(ctx, &mcp.CallToolParams{
				Name: "capture_things_task",
				Arguments: map[string]any{
					"title":           "Order cabinet handles",
					"destination":     destination,
					"idempotency_key": "index-project-capture",
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			queued := decodeCaptureResult(t, result)
			if queued.Status != "queued" {
				t.Fatalf("capture was not queued: %+v", queued)
			}
			job, err := store.Get(ctx, queued.RequestID)
			if err != nil {
				t.Fatal(err)
			}
			if job.Task.Destination == nil || *job.Task.Destination != destination {
				t.Fatalf("queued destination = %+v, want %+v", job.Task.Destination, destination)
			}
			lease, err := workerClient.Lease(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if lease == nil || lease.ID != queued.RequestID || lease.Task.Destination == nil || *lease.Task.Destination != destination {
				t.Fatalf("leased destination was not preserved: %+v", lease)
			}
			if err := workerClient.Complete(ctx, *lease, worker.Outcome{
				ThingsID: "task-cabinet-handles",
				Warnings: []string{tc.notice},
			}); err != nil {
				t.Fatal(err)
			}
			status, err := session.CallTool(ctx, &mcp.CallToolParams{
				Name:      "things_capture_status",
				Arguments: map[string]any{"request_id": queued.RequestID},
			})
			if err != nil {
				t.Fatal(err)
			}
			completed := decodeCaptureResult(t, status)
			if completed.Status != "created" || len(completed.Warnings) != 1 || completed.Warnings[0] != tc.notice {
				t.Fatalf("capture destination notice did not reach the MCP client: %+v", completed)
			}
		})
	}
}

func newProjectWorkflowSession(t *testing.T) (*mcp.ClientSession, *queue.Store, *worker.Client) {
	t.Helper()
	store, err := queue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	handler, err := NewHandler(store, Config{
		PublicToken:    testPublicToken,
		WorkerToken:    testWorkerToken,
		WaitForResult:  time.Millisecond,
		WorkerLongPoll: time.Millisecond,
		PollInterval:   time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	transport := handlerTransport{handler: handler}
	client := mcp.NewClient(&mcp.Implementation{Name: "project-workflow-test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             "http://things-index.test/mcp",
		HTTPClient:           &http.Client{Transport: authTransport{token: testPublicToken, base: transport}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	workerClient, err := worker.NewClient(worker.ClientConfig{
		BaseURL:    "http://127.0.0.1",
		Token:      testWorkerToken,
		HTTPClient: &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatal(err)
	}
	return session, store, workerClient
}
