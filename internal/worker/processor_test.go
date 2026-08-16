package worker

import (
	"context"
	"path/filepath"
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
	return helper.Response{SchemaVersion: 1, OK: true, ID: f.createdID, AppliedTags: f.appliedTags}, nil
}

func (f *fakeHelper) FindCapture(_ context.Context, _ string) ([]string, error) {
	return f.findIDs, nil
}

func (f *fakeHelper) FinaliseCapture(_ context.Context, id, title string) error {
	f.finalisedID = id
	f.finalisedTitle = title
	return nil
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
	job := Job{ID: "00000000000000000000000000000001", Task: capture.Request{Title: "Buy milk", Notes: "note", Tags: []string{"Errand"}}}
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
	job := Job{ID: "00000000000000000000000000000001", Task: capture.Request{Title: "Buy milk"}}
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
		Task: capture.Request{
			Title:       "Buy milk",
			Destination: &capture.Destination{Kind: capture.DestinationProject, Name: "Shopping", Heading: "Groceries"},
		},
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
		ID: "00000000000000000000000000000001", Task: capture.Request{Title: "Buy milk", Tags: []string{"Errand"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.Warnings) != 0 {
		t.Fatalf("unverified tags produced warnings: %#v", outcome.Warnings)
	}
}
