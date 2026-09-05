package worker

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nejmlabs/things-index/internal/capture"
	"github.com/nejmlabs/things-index/internal/helper"
	"github.com/nejmlabs/things-index/internal/journal"
)

type mutationHelper struct {
	fakeHelper
	plan                             helper.TaskUpdatePlan
	prepareCalls, applyCalls, checks int
	projectCalls                     int
	applyError, checkError           error
	projectError                     error
	verified                         bool
	response                         helper.Response
	appliedPlans                     []helper.TaskUpdatePlan
}

func (h *mutationHelper) PrepareTaskUpdate(context.Context, capture.UpdateTaskRequest) (helper.TaskUpdatePlan, error) {
	h.prepareCalls++
	return h.plan, nil
}

func (h *mutationHelper) ApplyTaskUpdate(_ context.Context, plan helper.TaskUpdatePlan) (helper.Response, error) {
	h.applyCalls++
	h.appliedPlans = append(h.appliedPlans, plan)
	if h.applyError != nil {
		return helper.Response{}, h.applyError
	}
	return helper.Response{OK: true, ID: plan.ID}, nil
}

func (h *mutationHelper) CheckTaskUpdate(context.Context, helper.TaskUpdatePlan) (bool, error) {
	h.checks++
	return h.verified, h.checkError
}

func (h *mutationHelper) CreateProject(context.Context, capture.CreateProjectRequest) (helper.Response, error) {
	h.projectCalls++
	return h.response, h.projectError
}

func mutationStore(t *testing.T) (*journal.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal.db")
	store, err := journal.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, path
}

func projectJob() Job {
	return Job{ID: "00000000000000000000000000000001", Task: capture.Request{CreateProjectRequest: &capture.CreateProjectRequest{Title: "New project"}}}
}

func TestMutationLostAcknowledgementReturnsPersistedResult(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, path := mutationStore(t)
	h := &mutationHelper{response: helper.Response{OK: true, ID: "project-created", Warnings: []string{"Saved warning"}}}
	p := &Processor{Helper: h, Journal: store}
	job := projectJob()
	first, err := p.Process(ctx, job)
	if err != nil || h.projectCalls != 1 {
		t.Fatalf("initial write failed: outcome=%+v error=%v calls=%d", first, err, h.projectCalls)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := journal.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	p.Journal = reopened
	h.projectError = errors.New("must not invoke Things after restart")
	for _, report := range []bool{false, true} {
		if report {
			if err := p.MarkReported(ctx, job.ID); err != nil {
				t.Fatal(err)
			}
		}
		result, err := p.Process(ctx, job)
		if err != nil || !reflect.DeepEqual(result, first) || h.projectCalls != 1 {
			t.Fatalf("redelivery repeated or lost result: outcome=%+v error=%v calls=%d", result, err, h.projectCalls)
		}
	}
	op, err := reopened.GetOperation(ctx, job.ID)
	if err != nil || op.State != journal.OperationReported {
		t.Fatalf("completion acknowledgement was not persisted: %+v %v", op, err)
	}
	job.Task.CreateProjectRequest.Title = "Different project"
	if _, err := p.Process(ctx, job); !errors.Is(err, journal.ErrPayloadMismatch) || IsRetryable(err) || h.projectCalls != 1 {
		t.Fatalf("changed payload reused cached outcome: %v", err)
	}
}

type failingMutationJournal struct {
	*journal.Store
	completeError error
}

func (s *failingMutationJournal) CompleteOperation(ctx context.Context, id string, result json.RawMessage) error {
	if s.completeError != nil {
		return s.completeError
	}
	return s.Store.CompleteOperation(ctx, id, result)
}

func TestUnknownGenericWriteIsNeverDispatchedAgain(t *testing.T) {
	t.Parallel()
	for _, loseJournalResult := range []bool{false, true} {
		name := "native timeout"
		if loseJournalResult {
			name = "journal failure after native success"
		}
		t.Run(name, func(t *testing.T) {
			store, _ := mutationStore(t)
			fault := errors.New("simulated interrupted result")
			wrapped := &failingMutationJournal{Store: store}
			h := &mutationHelper{response: helper.Response{OK: true, ID: "created"}}
			if loseJournalResult {
				wrapped.completeError = fault
			} else {
				h.projectError = fault
			}
			p := &Processor{Helper: h, Journal: wrapped}
			job := projectJob()
			if _, err := p.Process(context.Background(), job); !errors.Is(err, fault) {
				t.Fatalf("initial failure was lost: %v", err)
			}
			wrapped.completeError, h.projectError = nil, nil
			_, err := p.Process(context.Background(), job)
			if err == nil || IsRetryable(err) || !strings.Contains(err.Error(), "not repeated") || h.projectCalls != 1 {
				t.Fatalf("uncertain write was repeated: error=%v calls=%d", err, h.projectCalls)
			}
			op, err := store.GetOperation(context.Background(), job.ID)
			if err != nil || op.State != journal.OperationDispatched {
				t.Fatalf("uncertain intent was lost: %+v %v", op, err)
			}
		})
	}
}

func TestSafeUpdateReplayUsesOriginalPreparedValues(t *testing.T) {
	t.Parallel()
	store, _ := mutationStore(t)
	plan := helper.TaskUpdatePlan{Version: 1, ID: "resolved-once", ReplaySafe: true,
		Notes: &helper.TaskUpdateTextChange{Before: "Original", After: "Original\nAppend once"}}
	h := &mutationHelper{plan: plan, applyError: context.DeadlineExceeded}
	p := &Processor{Helper: h, Journal: store}
	job := Job{ID: projectJob().ID, Task: capture.Request{UpdateTaskRequest: &capture.UpdateTaskRequest{ID: plan.ID, AppendNotes: "Append once"}}}
	if _, err := p.Process(context.Background(), job); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected uncertain dispatch: %v", err)
	}
	// Resolving or reading the task again must not prepare a second append.
	h.plan = helper.TaskUpdatePlan{Version: 1, ID: "wrong-new-target", ReplaySafe: true}
	h.applyError = nil
	result, err := p.Process(context.Background(), job)
	if err != nil || result.ThingsID != plan.ID || h.prepareCalls != 1 || h.applyCalls != 2 || h.checks != 1 {
		t.Fatalf("incorrect replay: result=%+v error=%v helper=%+v", result, err, h)
	}
	if !reflect.DeepEqual(h.appliedPlans, []helper.TaskUpdatePlan{plan, plan}) {
		t.Fatalf("replay changed immutable intent: %+v", h.appliedPlans)
	}
}

func TestUnsafeChecklistUpdateOnlyReconcilesAfterDispatch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		verified   bool
		checkError error
	}{
		{name: "already applied", verified: true},
		{name: "not verified"},
		{name: "read failure", checkError: errors.New("database unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, _ := mutationStore(t)
			plan := helper.TaskUpdatePlan{Version: 1, ID: "task", ReplaySafe: false,
				Checklist: &helper.TaskUpdateChecklistChange{Append: []string{"One new row"}}}
			h := &mutationHelper{plan: plan, applyError: context.DeadlineExceeded, verified: tc.verified, checkError: tc.checkError}
			p := &Processor{Helper: h, Journal: store}
			job := Job{ID: projectJob().ID, Task: capture.Request{UpdateTaskRequest: &capture.UpdateTaskRequest{ID: plan.ID, AddChecklist: []string{"One new row"}}}}
			if _, err := p.Process(context.Background(), job); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expected interrupted dispatch: %v", err)
			}
			h.applyError = nil
			outcome, err := p.Process(context.Background(), job)
			switch {
			case tc.checkError != nil:
				if !errors.Is(err, tc.checkError) || !IsRetryable(err) {
					t.Fatalf("read failure became a write decision: %v", err)
				}
			case tc.verified:
				if err != nil || outcome.ThingsID != plan.ID {
					t.Fatalf("confirmed update was not recovered: %+v %v", outcome, err)
				}
			default:
				if err == nil || IsRetryable(err) {
					t.Fatalf("unconfirmed checklist append should stop permanently: %v", err)
				}
			}
			if h.prepareCalls != 1 || h.applyCalls != 1 || h.checks != 1 {
				t.Fatalf("unsafe append was repeated: %+v", h)
			}
		})
	}
}

func TestDelayedChecklistVerificationRecoversWithoutAnotherAppend(t *testing.T) {
	store, _ := mutationStore(t)
	h := &mutationHelper{
		plan: helper.TaskUpdatePlan{Version: 1, ID: "task", ReplaySafe: false,
			Checklist: &helper.TaskUpdateChecklistChange{Append: []string{"One new row"}}},
		applyError: &helper.OperationError{Code: "update_unverified"},
	}
	p := &Processor{Helper: h, Journal: store}
	job := Job{ID: projectJob().ID, Task: capture.Request{UpdateTaskRequest: &capture.UpdateTaskRequest{ID: "task", AddChecklist: []string{"One new row"}}}}
	if _, err := p.Process(context.Background(), job); !IsRetryable(err) {
		t.Fatalf("verification timeout must allow reconciliation: %v", err)
	}
	// The native append arrived after the verification window expired.
	h.verified = true
	result, err := p.Process(context.Background(), job)
	if err != nil || result.ThingsID != "task" || h.applyCalls != 1 || h.checks != 1 {
		t.Fatalf("late result was not recovered safely: %+v, %v, applies=%d checks=%d", result, err, h.applyCalls, h.checks)
	}
}
