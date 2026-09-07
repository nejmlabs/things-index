package worker

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nejmlabs/things-index/internal/capture"
	"github.com/nejmlabs/things-index/internal/helper"
	"github.com/nejmlabs/things-index/internal/journal"
)

type fakeHelper struct {
	captureCalls   int
	capturedTasks  []capture.Request
	captureErrors  []error
	findIDs        []string
	createdID      string
	appliedTags    []string
	finalisedID    string
	finalisedTitle string
}

func (f *fakeHelper) Capture(_ context.Context, _ string, task capture.Request) (helper.Response, error) {
	call := f.captureCalls
	f.captureCalls++
	f.capturedTasks = append(f.capturedTasks, task)
	if call < len(f.captureErrors) && f.captureErrors[call] != nil {
		return helper.Response{}, f.captureErrors[call]
	}
	return helper.Response{OK: true, ID: f.createdID, AppliedTags: f.appliedTags}, nil
}

func (f *fakeHelper) FindCapture(_ context.Context, _ string) ([]string, error) {
	return f.findIDs, nil
}

func (f *fakeHelper) FinaliseCapture(_ context.Context, id, title string) error {
	f.finalisedID = id
	f.finalisedTitle = title
	return nil
}

func (f *fakeHelper) CreateHeading(_ context.Context, _, _ string) (helper.Response, error) {
	return helper.Response{OK: true, ID: "fake-heading-id"}, nil
}

func (f *fakeHelper) ArchiveHeading(_ context.Context, _, _ string) (helper.Response, error) {
	return helper.Response{OK: true, ID: "fake-heading-id"}, nil
}

func (f *fakeHelper) RenameHeading(_ context.Context, _, _, _ string) (helper.Response, error) {
	return helper.Response{OK: true, ID: "fake-heading-id"}, nil
}

func (f *fakeHelper) ArchiveTask(_ context.Context, _, _, _, _ string) (helper.Response, error) {
	return helper.Response{OK: true, ID: "fake-task-id"}, nil
}

func (f *fakeHelper) ArchiveProject(_ context.Context, _, _, _ string) (helper.Response, error) {
	return helper.Response{OK: true, ID: "fake-project-id"}, nil
}

func (f *fakeHelper) QueryTasks(_ context.Context, _ capture.QueryTasksRequest) (helper.Response, error) {
	return helper.Response{OK: true, ID: `[]`}, nil
}

func (f *fakeHelper) CreateProject(_ context.Context, _ capture.CreateProjectRequest) (helper.Response, error) {
	return helper.Response{OK: true, ID: "fake-project-id"}, nil
}

func (f *fakeHelper) UpdateTask(_ context.Context, _ capture.UpdateTaskRequest) (helper.Response, error) {
	return helper.Response{OK: true, ID: "fake-task-id"}, nil
}

func TestProcessorCreatesAndFinalisesOnce(t *testing.T) {
	t.Parallel()

	store, err := journal.Open(filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fake := &fakeHelper{createdID: "things-1", appliedTags: []string{"Errand"}}
	processor := &Processor{Helper: fake, Journal: store}
	job := Job{ID: "00000000000000000000000000000001", Task: capture.Request{TaskFields: capture.TaskFields{Title: "Buy milk", Notes: "note", Tags: []string{"Errand"}}}}
	outcome, err := processor.Process(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.ThingsID != "things-1" || fake.captureCalls != 1 || fake.finalisedID != "things-1" {
		t.Fatalf("unexpected outcome %#v and helper %#v", outcome, fake)
	}
	if fake.finalisedTitle != job.Task.Title {
		t.Fatalf("finalised title = %q, want %q", fake.finalisedTitle, job.Task.Title)
	}
	if _, err := processor.Process(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if fake.captureCalls != 1 {
		t.Fatalf("task was created %d times", fake.captureCalls)
	}
}

func TestProcessorRecoversCreatedTaskFromPendingTitle(t *testing.T) {
	t.Parallel()

	store, err := journal.Open(filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	job := Job{ID: "00000000000000000000000000000001", Task: capture.Request{TaskFields: capture.TaskFields{Title: "Buy milk"}}}
	hash, _ := job.Task.Hash()
	if _, _, err := store.Ensure(context.Background(), job.ID, hash); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCreating(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	fake := &fakeHelper{findIDs: []string{"things-recovered"}}
	processor := &Processor{Helper: fake, Journal: store}
	outcome, err := processor.Process(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.ThingsID != "things-recovered" || fake.captureCalls != 0 || fake.finalisedID != "things-recovered" {
		t.Fatalf("unexpected recovery outcome %#v and helper %#v", outcome, fake)
	}
}

func TestProcessorFallsBackToInboxWhenHeadingIsMissing(t *testing.T) {
	t.Parallel()

	store, err := journal.Open(filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fake := &fakeHelper{
		createdID:     "things-1",
		captureErrors: []error{&helper.OperationError{Code: "heading_not_found"}},
	}
	processor := &Processor{Helper: fake, Journal: store}
	outcome, err := processor.Process(context.Background(), Job{
		ID: "00000000000000000000000000000001",
		Task: capture.Request{TaskFields: capture.TaskFields{
			Title:       "Buy milk",
			Destination: &capture.Destination{Kind: capture.DestinationProject, Name: "Shopping", Heading: "Groceries"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fake.captureCalls != 2 || fake.capturedTasks[1].Destination.Kind != capture.DestinationInbox {
		t.Fatalf("unexpected fallback calls %#v", fake.capturedTasks)
	}
	if len(outcome.Warnings) != 1 || outcome.Warnings[0] == "" {
		t.Fatalf("unexpected warnings %#v", outcome.Warnings)
	}
}

func TestProcessorTreatsOmittedAppliedTagsAsUnverified(t *testing.T) {
	t.Parallel()

	store, err := journal.Open(filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fake := &fakeHelper{createdID: "things-1", appliedTags: nil}
	processor := &Processor{Helper: fake, Journal: store}
	outcome, err := processor.Process(context.Background(), Job{
		ID: "00000000000000000000000000000001", Task: capture.Request{TaskFields: capture.TaskFields{Title: "Buy milk", Tags: []string{"Errand"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.Warnings) != 0 {
		t.Fatalf("unverified tags produced warnings: %#v", outcome.Warnings)
	}
}

func TestIsRetryable(t *testing.T) {
	t.Parallel()

	if IsRetryable(nil) {
		t.Fatal("nil error should not be retryable")
	}
	if IsRetryable(permanentError(errors.New("invalid job"))) {
		t.Fatal("permanentError should not be retryable")
	}
	if !IsRetryable(&helper.OperationError{Code: "create_failed"}) {
		t.Fatal("create_failed should be retryable")
	}
	if !IsRetryable(&helper.OperationError{Code: "finalise_not_found"}) {
		t.Fatal("finalise_not_found should be retryable")
	}
	if !IsRetryable(&helper.OperationError{Code: "finalise_unverified"}) {
		t.Fatal("finalise_unverified should be retryable")
	}
	if !IsRetryable(&helper.OperationError{Code: "update_unverified"}) {
		t.Fatal("update_unverified must allow reconciliation using the durable prepared plan")
	}
	if IsRetryable(&helper.OperationError{Code: "invalid_request"}) {
		t.Fatal("invalid_request should not be retryable")
	}
	if IsRetryable(&helper.OperationError{Code: "destination_not_found"}) {
		t.Fatal("destination_not_found should not be retryable")
	}
}

type captureJournalFailure struct {
	*journal.Store
	failCreated bool
}

func (s *captureJournalFailure) MarkCreated(ctx context.Context, jobID, thingsID, notes string) error {
	if s.failCreated {
		s.failCreated = false
		return errors.New("interrupted before saving returned UUID")
	}
	return s.Store.MarkCreated(ctx, jobID, thingsID, notes)
}

type recoverableCaptureHelper struct {
	fakeHelper
	actualNotes string
	warnings    []string
	readError   error
	finalise    func(context.Context, string, string) error
}

func (h *recoverableCaptureHelper) Capture(ctx context.Context, id string, task capture.Request) (helper.Response, error) {
	response, err := h.fakeHelper.Capture(ctx, id, task)
	if err == nil {
		h.findIDs = []string{response.ID}
		response.Warnings = h.warnings
	}
	return response, err
}

func (h *recoverableCaptureHelper) ReadCaptureNotes(context.Context, string) (string, error) {
	return h.actualNotes, h.readError
}

func (h *recoverableCaptureHelper) FinaliseCapture(ctx context.Context, id, title string) error {
	if h.finalise != nil {
		if err := h.finalise(ctx, id, title); err != nil {
			return err
		}
	}
	return h.fakeHelper.FinaliseCapture(ctx, id, title)
}

func TestCaptureRecoveryKeepsWarningsAndSavesUUIDBeforeFinalising(t *testing.T) {
	t.Parallel()
	store, _ := mutationStore(t)
	wrapped := &captureJournalFailure{Store: store, failCreated: true}
	warnings := []string{
		`ThingsIndex warning: requested project "Kitchen" did not match an active project; captured in Inbox.`,
		`ThingsIndex warning: tag "Missing" did not exist and was not applied.`,
	}
	notes := "Original notes\n\n" + strings.Join(warnings, "\n\n")
	h := &recoverableCaptureHelper{fakeHelper: fakeHelper{createdID: "created-once", appliedTags: []string{}}, actualNotes: notes, warnings: warnings}
	job := Job{ID: projectJob().ID, Task: capture.Request{TaskFields: capture.TaskFields{Title: "Task", Notes: "Original notes", Tags: []string{"Missing"}}}}
	h.finalise = func(ctx context.Context, id, _ string) error {
		entry, err := store.Get(ctx, job.ID)
		if err != nil || entry.State != journal.StateCreated || entry.ThingsID != id || entry.Notes != notes {
			t.Fatalf("marker removed before durable identity/warnings: entry=%+v error=%v", entry, err)
		}
		return nil
	}
	p := &Processor{Helper: h, Journal: wrapped}
	if _, err := p.Process(context.Background(), job); err == nil || h.finalisedID != "" {
		t.Fatalf("capture was finalised despite journal failure: error=%v helper=%+v", err, h)
	}
	// A protected-database read failure must not discard the actual warnings.
	h.readError = errors.New("notes read failed")
	if _, err := p.Process(context.Background(), job); !errors.Is(err, h.readError) || h.finalisedID != "" {
		t.Fatalf("recovery ignored notes read failure: %v", err)
	}
	h.readError = nil
	result, err := p.Process(context.Background(), job)
	if err != nil || result.ThingsID != h.createdID || len(result.Warnings) != 2 || h.captureCalls != 1 || h.finalisedID != h.createdID {
		t.Fatalf("capture recovery failed: outcome=%+v error=%v helper=%+v", result, err, h)
	}
	for i, warning := range warnings {
		if result.Warnings[i] != warning {
			t.Fatalf("recovery lost original warning: %v", result.Warnings)
		}
	}
}

func TestCaptureResponseAndMissingTagWarningsAreDeduplicated(t *testing.T) {
	t.Parallel()
	store, _ := mutationStore(t)
	warning := `ThingsIndex warning: tag "Missing" did not exist and was not applied.`
	h := &recoverableCaptureHelper{fakeHelper: fakeHelper{createdID: "created-once", appliedTags: []string{}}, warnings: []string{warning}}
	p := &Processor{Helper: h, Journal: store}
	job := Job{ID: projectJob().ID, Task: capture.Request{TaskFields: capture.TaskFields{Title: "Task", Tags: []string{"Missing"}}}}
	result, err := p.Process(context.Background(), job)
	if err != nil || len(result.Warnings) != 1 || result.Warnings[0] != warning {
		t.Fatalf("duplicate/lost warning: result=%+v error=%v", result, err)
	}
	entry, err := store.Get(context.Background(), job.ID)
	if err != nil || entry.Notes != warning {
		t.Fatalf("journal has duplicate/lost warning: entry=%+v error=%v", entry, err)
	}
}

func TestUncertainCaptureRequiresOneExactRecoveryMatch(t *testing.T) {
	t.Parallel()
	for _, ids := range [][]string{nil, {"duplicate-1", "duplicate-2"}} {
		store, _ := mutationStore(t)
		job := Job{ID: projectJob().ID, Task: capture.Request{TaskFields: capture.TaskFields{Title: "Task"}}}
		hash, err := job.Task.Hash()
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Ensure(context.Background(), job.ID, hash); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkCreating(context.Background(), job.ID); err != nil {
			t.Fatal(err)
		}
		h := &fakeHelper{createdID: "must-not-create", findIDs: ids}
		p := &Processor{Helper: h, Journal: store}
		if _, err := p.Process(context.Background(), job); err == nil || IsRetryable(err) || h.captureCalls != 0 || h.finalisedID != "" {
			t.Fatalf("uncertain match caused mutation: matches=%v error=%v helper=%+v", ids, err, h)
		}
	}
}
