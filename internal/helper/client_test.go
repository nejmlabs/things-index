package helper

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/nejmlabs/things-index/internal/capture"
)

const testRequestID = "00000000000000000000000000000001"

type recordingRunner struct {
	t       *testing.T
	wantOp  string
	stdout  string
	request map[string]any
	args    []string
}

func TestClientCaptureEncodesThingsFields(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{
		t: t, wantOp: "capture-task",
		stdout: `{"schemaVersion":1,"ok":true,"id":"things-id"}`,
	}
	client := &Client{
		TempDir: t.TempDir(), Runner: runner, Location: time.FixedZone("BST", 60*60),
		Now: func() time.Time { return time.Date(2026, 8, 16, 12, 0, 0, 0, time.FixedZone("BST", 60*60)) },
	}
	task := capture.Request{
		Title:       "Buy milk",
		Notes:       "Use glass bottles",
		Destination: &capture.Destination{Kind: capture.DestinationProject, Name: "Shopping", Heading: "Groceries"},
		Schedule: &capture.Schedule{
			Start: capture.StartOnDate, Date: "2026-08-17", Evening: true,
			ReminderAt: "2026-08-17T18:30:00+01:00",
		},
		Deadline:  "2026-08-18",
		Tags:      []string{"Errand", "Quick"},
		Checklist: []string{"Check fridge", "Buy milk"},
	}
	if _, err := client.Capture(context.Background(), testRequestID, task); err != nil {
		t.Fatal(err)
	}
	wireTask, ok := runner.request["task"].(map[string]any)
	if !ok {
		t.Fatalf("task is not an object: %#v", runner.request["task"])
	}
	destination, ok := wireTask["destination"].(map[string]any)
	if !ok || destination["kind"] != "project" || destination["name"] != "Shopping" || destination["heading"] != "Groceries" {
		t.Fatalf("unexpected destination %#v", wireTask["destination"])
	}
	if wireTask["start"] != "on_date" || wireTask["startDate"] != "2026-08-17" || wireTask["evening"] != true || wireTask["reminderTime"] != "18:30" {
		t.Fatalf("unexpected schedule %#v", wireTask)
	}
	if wireTask["deadline"] != "2026-08-18" || wireTask["checklist"] != "Check fridge\nBuy milk" {
		t.Fatalf("unexpected deadline or checklist %#v", wireTask)
	}
	if wireTask["startDayOffset"] != float64(1) || wireTask["reminderMinuteOffset"] != float64(18*60+30) || wireTask["deadlineDayOffset"] != float64(2) {
		t.Fatalf("unexpected native date offsets %#v", wireTask)
	}
	if tags, ok := wireTask["tags"].([]any); !ok || len(tags) != 2 || tags[0] != "Errand" || tags[1] != "Quick" {
		t.Fatalf("unexpected tags %#v", wireTask["tags"])
	}
}

func TestClientCaptureConvertsReminderToWorkerLocalTime(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{t: t, wantOp: "capture-task", stdout: `{"schemaVersion":1,"ok":true,"id":"things-id"}`}
	client := &Client{
		TempDir: t.TempDir(), Runner: runner, Location: time.FixedZone("BST", 60*60),
		Now: func() time.Time { return time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC) },
	}
	_, err := client.Capture(context.Background(), testRequestID, capture.Request{
		Title:    "Buy milk",
		Schedule: &capture.Schedule{Start: capture.StartOnDate, Date: "2026-08-17", ReminderAt: "2026-08-17T17:30:00Z"},
	})
	if err != nil {
		t.Fatal(err)
	}
	wireTask := runner.request["task"].(map[string]any)
	if wireTask["reminderTime"] != "18:30" {
		t.Fatalf("reminderTime = %#v", wireTask["reminderTime"])
	}
	if wireTask["startDayOffset"] != float64(1) || wireTask["reminderMinuteOffset"] != float64(18*60+30) {
		t.Fatalf("unexpected native date offsets %#v", wireTask)
	}
}

func TestClientCaptureUsesCalendarOffsetsAcrossDSTChange(t *testing.T) {
	t.Parallel()

	location, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{t: t, wantOp: "capture-task", stdout: `{"schemaVersion":1,"ok":true,"id":"things-id"}`}
	client := &Client{
		TempDir: t.TempDir(), Runner: runner, Location: location,
		// The UK changes from BST to GMT between the base date and task date.
		// Calendar offsets must remain two and three days, not be derived from
		// a 49- or 73-hour elapsed duration.
		Now: func() time.Time { return time.Date(2026, 10, 24, 12, 0, 0, 0, location) },
	}
	_, err = client.Capture(context.Background(), testRequestID, capture.Request{
		Title: "DST-safe task",
		Schedule: &capture.Schedule{
			Start: capture.StartOnDate, Date: "2026-10-26",
			ReminderAt: "2026-10-26T09:15:00Z",
		},
		Deadline: "2026-10-27",
	})
	if err != nil {
		t.Fatal(err)
	}
	wireTask := runner.request["task"].(map[string]any)
	if wireTask["startDayOffset"] != float64(2) {
		t.Fatalf("startDayOffset = %#v, want 2", wireTask["startDayOffset"])
	}
	if wireTask["deadlineDayOffset"] != float64(3) {
		t.Fatalf("deadlineDayOffset = %#v, want 3", wireTask["deadlineDayOffset"])
	}
	if wireTask["reminderMinuteOffset"] != float64(9*60+15) {
		t.Fatalf("reminderMinuteOffset = %#v, want 555", wireTask["reminderMinuteOffset"])
	}
}

func (r *recordingRunner) Run(_ context.Context, executable string, args []string) ([]byte, []byte, error) {
	r.t.Helper()
	if executable != "/usr/bin/shortcuts" {
		r.t.Fatalf("unexpected executable %q", executable)
	}
	r.args = append([]string(nil), args...)
	data, err := os.ReadFile(args[3])
	if err != nil {
		r.t.Fatal(err)
	}
	if err := json.Unmarshal(data, &r.request); err != nil {
		r.t.Fatal(err)
	}
	if r.request["operation"] != r.wantOp {
		r.t.Fatalf("unexpected operation %#v", r.request["operation"])
	}
	info, err := os.Stat(args[3])
	if err != nil {
		r.t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		r.t.Fatalf("request permissions are %o", info.Mode().Perm())
	}
	return []byte(r.stdout), nil, nil
}

func TestClientCaptureUsesFixedShortcutCommand(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{
		t:      t,
		wantOp: "capture-task",
		stdout: `{"schemaVersion":1,"ok":true,"id":"things-id","appliedTags":["Errand"]}`,
	}
	client := &Client{TempDir: t.TempDir(), Runner: runner}
	response, err := client.Capture(context.Background(), testRequestID, capture.Request{Title: "Buy milk"})
	if err != nil {
		t.Fatal(err)
	}
	if response.ID != "things-id" {
		t.Fatalf("unexpected id %q", response.ID)
	}
	wantPrefix := []string{"run", ShortcutName, "--input-path"}
	if !reflect.DeepEqual(runner.args[:3], wantPrefix) {
		t.Fatalf("unexpected arguments %#v", runner.args)
	}
	if filepath.Dir(runner.args[3]) != client.TempDir {
		t.Fatalf("request escaped temporary directory: %q", runner.args[3])
	}
	if wantSuffix := []string{"--output-type", "public.json"}; !reflect.DeepEqual(runner.args[len(runner.args)-2:], wantSuffix) {
		t.Fatalf("unexpected output arguments %#v", runner.args)
	}
	if _, err := os.Stat(runner.args[3]); !os.IsNotExist(err) {
		t.Fatalf("request file was not removed: %v", err)
	}
}

func TestClientPingRequiresCaptureCapabilities(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{t: t, wantOp: "ping", stdout: `{"schemaVersion":1,"ok":true,"capabilities":[]}`}
	client := &Client{TempDir: t.TempDir(), Runner: runner}
	if err := client.Ping(context.Background()); err == nil {
		t.Fatal("expected missing capabilities to fail")
	}
}

func TestClientPingAcceptsCurrentCapabilities(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{
		t: t, wantOp: "ping",
		stdout: `{"schemaVersion":1,"ok":true,"capabilities":["capture-task-v5","find-capture-v1","finalise-capture-v1"]}`,
	}
	client := &Client{TempDir: t.TempDir(), Runner: runner}
	if err := client.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestClientFindCaptureUsesShortcutProtocol(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{
		t: t, wantOp: "find-capture",
		stdout: `{"schemaVersion":1,"ok":true,"ids":["things-1","things-2"]}`,
	}
	client := &Client{TempDir: t.TempDir(), Runner: runner}
	ids, err := client.FindCapture(context.Background(), testRequestID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids, []string{"things-1", "things-2"}) {
		t.Fatalf("unexpected ids %#v", ids)
	}
	if runner.request["requestId"] != testRequestID {
		t.Fatalf("unexpected request %#v", runner.request)
	}
}

func TestClientFindCaptureRejectsDuplicateIDs(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{
		t: t, wantOp: "find-capture",
		stdout: `{"schemaVersion":1,"ok":true,"ids":["things-1","things-1"]}`,
	}
	client := &Client{TempDir: t.TempDir(), Runner: runner}
	if _, err := client.FindCapture(context.Background(), testRequestID); err == nil {
		t.Fatal("expected duplicate Things identifiers to fail")
	}
}

func TestClientReturnsStructuredOperationError(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{
		t: t, wantOp: "capture-task",
		stdout: `{"schemaVersion":1,"ok":false,"code":"destination_not_found"}`,
	}
	client := &Client{TempDir: t.TempDir(), Runner: runner}
	_, err := client.Capture(context.Background(), testRequestID, capture.Request{Title: "Buy milk"})
	var operationError *OperationError
	if !errors.As(err, &operationError) || operationError.Code != "destination_not_found" {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestClientFinaliseUsesShortcutProtocol(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{t: t, wantOp: "finalise-capture", stdout: `{"schemaVersion":1,"ok":true}`}
	client := &Client{TempDir: t.TempDir(), Runner: runner}
	if err := client.FinaliseCapture(context.Background(), "things-1", "title with 'quotes'"); err != nil {
		t.Fatal(err)
	}
	if runner.request["id"] != "things-1" || runner.request["title"] != "title with 'quotes'" {
		t.Fatalf("unexpected request %#v", runner.request)
	}
	if _, exists := runner.request["notes"]; exists {
		t.Fatalf("finalisation must not resend notes: %#v", runner.request)
	}
}
