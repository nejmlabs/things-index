package worker

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nejmlabs/things-index/internal/capture"
	"github.com/nejmlabs/things-index/internal/helper"
	"github.com/nejmlabs/things-index/internal/journal"
)

type projectWarningHelper struct {
	fakeHelper
	warning string
}

func (f *projectWarningHelper) Capture(ctx context.Context, id string, task capture.Request) (helper.Response, error) {
	response, err := f.fakeHelper.Capture(ctx, id, task)
	response.Warnings = []string{f.warning}
	return response, err
}

func TestProjectCaptureNoticesSurviveReplay(t *testing.T) {
	for _, tc := range []struct {
		name        string
		destination capture.Destination
		warning     string
	}{
		{
			name:        "matched project",
			destination: capture.Destination{Kind: capture.DestinationProject, Name: "kitchen"},
			warning:     `ThingsIndex warning: matched project "kitchen" to "Kitchen renovation".`,
		},
		{
			name:        "ambiguous project captured in Inbox",
			destination: capture.Destination{Kind: capture.DestinationProject, Name: "kitchen"},
			warning:     `ThingsIndex warning: project "kitchen" matched multiple active projects; captured in Inbox.`,
		},
		{
			name:        "missing project captured in Inbox",
			destination: capture.Destination{Kind: capture.DestinationProject, Name: "garden"},
			warning:     `ThingsIndex warning: no active project matched "garden"; captured in Inbox.`,
		},
		{
			name:        "stale project ID captured in Inbox",
			destination: capture.Destination{Kind: capture.DestinationProject, ID: "old-project", Name: "Kitchen renovation"},
			warning:     `ThingsIndex warning: requested project "Kitchen renovation" (ID: old-project) was unavailable; captured in Inbox.`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := journal.Open(filepath.Join(t.TempDir(), "journal.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			// The helper has already handled the destination and created one task.
			// The worker must preserve its notice without starting another capture.
			fake := &projectWarningHelper{fakeHelper: fakeHelper{createdID: "task-one"}, warning: tc.warning}
			processor := &Processor{Helper: fake, Journal: store}
			job := Job{ID: "00000000000000000000000000000001", Task: capture.Request{TaskFields: capture.TaskFields{
				Title: "Buy paint", Notes: "Original notes", Destination: &tc.destination,
			}}}
			originalHash, err := job.Task.Hash()
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 3; i++ {
				if i == 2 {
					if err := processor.MarkReported(context.Background(), job.ID); err != nil {
						t.Fatal(err)
					}
				}
				outcome, err := processor.Process(context.Background(), job)
				if err != nil {
					t.Fatal(err)
				}
				if outcome.ThingsID != "task-one" || len(outcome.Warnings) != 1 || outcome.Warnings[0] != tc.warning {
					t.Fatalf("notice or created task lost on attempt %d: %+v", i, outcome)
				}
				entry, err := store.Get(context.Background(), job.ID)
				if err != nil {
					t.Fatal(err)
				}
				if entry.Notes != "Original notes\n\n"+tc.warning || entry.PayloadHash != originalHash {
					t.Fatalf("journal lost the notice or original request hash: %+v", entry)
				}
				if fake.captureCalls != 1 {
					t.Fatalf("worker recaptured a successful task: %d calls", fake.captureCalls)
				}
			}
			finalHash, err := job.Task.Hash()
			if err != nil {
				t.Fatal(err)
			}
			if finalHash != originalHash {
				t.Fatal("worker mutated the original request")
			}
		})
	}
}
