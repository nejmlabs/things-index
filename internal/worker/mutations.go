package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nejmlabs/things-index/internal/capture"
	"github.com/nejmlabs/things-index/internal/helper"
	"github.com/nejmlabs/things-index/internal/journal"
)

type OperationJournal interface {
	EnsureOperation(context.Context, string, string) (journal.Operation, error)
	GetOperation(context.Context, string) (journal.Operation, error)
	PrepareOperation(context.Context, string, json.RawMessage) error
	MarkOperationDispatched(context.Context, string) error
	CompleteOperation(context.Context, string, json.RawMessage) error
	MarkOperationReported(context.Context, string) error
}

type preparedTaskUpdater interface {
	PrepareTaskUpdate(context.Context, capture.UpdateTaskRequest) (helper.TaskUpdatePlan, error)
	ApplyTaskUpdate(context.Context, helper.TaskUpdatePlan) (helper.Response, error)
	CheckTaskUpdate(context.Context, helper.TaskUpdatePlan) (bool, error)
}

type organizationCreator interface {
	CreateArea(context.Context, capture.CreateAreaRequest) (helper.Response, error)
	CreateTag(context.Context, capture.CreateTagRequest) (helper.Response, error)
}

// processMutation records intent before invoking Things. Cached outcomes make
// lost completion acknowledgements safe. A crash with an unknown native result
// is reconciled read-only where possible; otherwise it is never re-dispatched.
func (p *Processor) processMutation(ctx context.Context, job Job) (Outcome, error) {
	store, ok := p.Journal.(OperationJournal)
	if !ok {
		return Outcome{}, permanentError(errors.New("write recovery journal is required"))
	}
	hash, err := job.Task.Hash()
	if err != nil {
		return Outcome{}, err
	}
	op, err := store.EnsureOperation(ctx, job.ID, hash)
	if err != nil {
		if errors.Is(err, journal.ErrPayloadMismatch) {
			err = permanentError(err)
		}
		return Outcome{}, err
	}
	if op.State == journal.OperationApplied || op.State == journal.OperationReported {
		var result Outcome
		if err := json.Unmarshal(op.Result, &result); err != nil || result.ThingsID == "" {
			return Outcome{}, permanentError(errors.New("saved write result is invalid; operation was not repeated"))
		}
		return result, nil
	}
	var updater preparedTaskUpdater
	var plan helper.TaskUpdatePlan
	if job.Task.UpdateTaskRequest != nil {
		updater, ok = p.Helper.(preparedTaskUpdater)
		if !ok {
			return Outcome{}, permanentError(errors.New("prepared task updates are required"))
		}
	}
	if op.State == journal.OperationReceived {
		encoded := json.RawMessage(`{}`)
		if updater != nil {
			plan, err = updater.PrepareTaskUpdate(ctx, *job.Task.UpdateTaskRequest)
			if err != nil {
				return Outcome{}, err
			}
			encoded, err = json.Marshal(plan)
			if err != nil {
				return Outcome{}, err
			}
		}
		if err = store.PrepareOperation(ctx, job.ID, encoded); err != nil {
			return Outcome{}, err
		}
		op.State, op.Plan = journal.OperationPrepared, encoded
	}
	if updater != nil {
		if err = json.Unmarshal(op.Plan, &plan); err != nil {
			return Outcome{}, permanentError(errors.New("saved update plan is invalid"))
		}
	}
	if op.State == journal.OperationDispatched {
		if updater == nil {
			return Outcome{}, uncertainWriteError()
		}
		verified, err := updater.CheckTaskUpdate(ctx, plan)
		if err != nil {
			return Outcome{}, err
		}
		if verified {
			return finishMutation(ctx, store, job.ID, helper.Response{OK: true, ID: plan.ID})
		}
		if !plan.ReplaySafe {
			return Outcome{}, uncertainWriteError()
		}
	} else {
		if op.State != journal.OperationPrepared {
			return Outcome{}, permanentError(errors.New("unexpected write journal state"))
		}
		if err := ctx.Err(); err != nil {
			return Outcome{}, err
		}
		if err = store.MarkOperationDispatched(ctx, job.ID); err != nil {
			return Outcome{}, err
		}
	}
	var response helper.Response
	if updater != nil {
		response, err = updater.ApplyTaskUpdate(ctx, plan)
	} else {
		response, err = p.dispatchMutation(ctx, job.Task)
	}
	if err != nil {
		return Outcome{}, err
	}
	return finishMutation(ctx, store, job.ID, response)
}

func uncertainWriteError() error {
	return permanentError(errors.New("the previous write outcome could not be verified; it was not repeated to avoid duplicate changes; review the Things item before submitting a new request"))
}

func finishMutation(ctx context.Context, store OperationJournal, jobID string, response helper.Response) (Outcome, error) {
	if !response.OK || response.ID == "" {
		return Outcome{}, permanentError(errors.New("Things write returned no verified identifier"))
	}
	outcome := Outcome{ThingsID: response.ID, Warnings: response.Warnings}
	encoded, err := json.Marshal(outcome)
	if err != nil {
		return Outcome{}, err
	}
	if err = store.CompleteOperation(ctx, jobID, encoded); err != nil {
		return Outcome{}, err
	}
	return outcome, nil
}

func (p *Processor) dispatchMutation(ctx context.Context, task capture.Request) (helper.Response, error) {
	switch {
	case task.CreateProjectRequest != nil:
		return p.Helper.CreateProject(ctx, *task.CreateProjectRequest)
	case task.ArchiveTaskRequest != nil:
		r := task.ArchiveTaskRequest
		return p.Helper.ArchiveTask(ctx, r.ID, r.Title, r.Project, r.Action)
	case task.ArchiveProjectRequest != nil:
		r := task.ArchiveProjectRequest
		return p.Helper.ArchiveProject(ctx, r.ID, r.Name, r.Action)
	case task.CreateAreaRequest != nil || task.CreateTagRequest != nil:
		creator, ok := p.Helper.(organizationCreator)
		if !ok {
			return helper.Response{}, permanentError(errors.New("area and tag creation are unavailable in this worker"))
		}
		if task.CreateAreaRequest != nil {
			return creator.CreateArea(ctx, *task.CreateAreaRequest)
		}
		return creator.CreateTag(ctx, *task.CreateTagRequest)
	case task.HeadingRequest != nil:
		r := task.HeadingRequest
		switch task.HeadingOperation {
		case "create":
			return p.Helper.CreateHeading(ctx, r.Project, r.Heading)
		case "rename":
			return p.Helper.RenameHeading(ctx, r.Project, r.Heading, r.NewTitle)
		case "archive":
			return p.Helper.ArchiveHeading(ctx, r.Project, r.Heading)
		}
	}
	return helper.Response{}, permanentError(fmt.Errorf("unsupported write operation"))
}
