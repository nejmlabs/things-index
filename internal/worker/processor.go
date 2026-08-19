package worker

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/nejmlabs/things-index/internal/capture"
	"github.com/nejmlabs/things-index/internal/helper"
	"github.com/nejmlabs/things-index/internal/journal"
)

type PermanentError struct {
	Err error
}

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	var permanent *PermanentError
	if errors.As(err, &permanent) {
		return false
	}
	var operationError *helper.OperationError
	if errors.As(err, &operationError) {
		return operationError.Code == "create_failed" ||
			operationError.Code == "finalise_not_found" ||
			operationError.Code == "finalise_unverified"
	}
	return true
}

type Helper interface {
	Capture(ctx context.Context, requestID string, task capture.Request) (helper.Response, error)
	FindCapture(ctx context.Context, requestID string) ([]string, error)
	FinaliseCapture(ctx context.Context, id, title string) error
	CreateHeading(ctx context.Context, project, headingTitle string) (helper.Response, error)
	ArchiveHeading(ctx context.Context, project, headingTitle string) (helper.Response, error)
	RenameHeading(ctx context.Context, project, oldHeadingTitle, newHeadingTitle string) (helper.Response, error)
	ArchiveTask(ctx context.Context, id, title, project, action string) (helper.Response, error)
	ArchiveProject(ctx context.Context, id, name, action string) (helper.Response, error)
	QueryTasks(ctx context.Context, req capture.QueryTasksRequest) (helper.Response, error)
	CreateProject(ctx context.Context, req capture.CreateProjectRequest) (helper.Response, error)
	UpdateTask(ctx context.Context, req capture.UpdateTaskRequest) (helper.Response, error)
}

type Journal interface {
	Ensure(ctx context.Context, jobID, payloadHash string) (journal.Entry, bool, error)
	Get(ctx context.Context, jobID string) (journal.Entry, error)
	MarkCreating(ctx context.Context, jobID string) error
	MarkCreated(ctx context.Context, jobID, thingsID, finalNotes string) error
	MarkFinalised(ctx context.Context, jobID string) error
	MarkReported(ctx context.Context, jobID string) error
}

type Job struct {
	ID   string          `json:"id"`
	Task capture.Request `json:"task"`
}

type Outcome struct {
	ThingsID string   `json:"thingsId"`
	Warnings []string `json:"warnings,omitempty"`
}

type Processor struct {
	Helper  Helper
	Journal Journal
}

func (p *Processor) Process(ctx context.Context, job Job) (Outcome, error) {
	if p.Helper == nil || p.Journal == nil {
		return Outcome{}, errors.New("worker helper and journal are required")
	}
	if !validJobID(job.ID) {
		return Outcome{}, permanentError(errors.New("job identifier must be a 32-character lowercase hexadecimal value"))
	}
	if err := job.Task.Validate(); err != nil {
		return Outcome{}, permanentError(fmt.Errorf("invalid capture job: %w", err))
	}

	if job.Task.QueryTasksRequest != nil {
		resp, err := p.Helper.QueryTasks(ctx, *job.Task.QueryTasksRequest)
		if err != nil {
			return Outcome{}, err
		}
		return Outcome{ThingsID: resp.ID}, nil
	}

	if job.Task.CreateProjectRequest != nil {
		resp, err := p.Helper.CreateProject(ctx, *job.Task.CreateProjectRequest)
		if err != nil {
			return Outcome{}, err
		}
		return Outcome{ThingsID: resp.ID}, nil
	}

	if job.Task.UpdateTaskRequest != nil {
		resp, err := p.Helper.UpdateTask(ctx, *job.Task.UpdateTaskRequest)
		if err != nil {
			return Outcome{}, err
		}
		return Outcome{ThingsID: resp.ID}, nil
	}

	if job.Task.ArchiveTaskRequest != nil {
		resp, err := p.Helper.ArchiveTask(ctx, job.Task.ArchiveTaskRequest.ID, job.Task.ArchiveTaskRequest.Title, job.Task.ArchiveTaskRequest.Project, job.Task.ArchiveTaskRequest.Action)
		if err != nil {
			return Outcome{}, err
		}
		return Outcome{ThingsID: resp.ID}, nil
	}

	if job.Task.ArchiveProjectRequest != nil {
		resp, err := p.Helper.ArchiveProject(ctx, job.Task.ArchiveProjectRequest.ID, job.Task.ArchiveProjectRequest.Name, job.Task.ArchiveProjectRequest.Action)
		if err != nil {
			return Outcome{}, err
		}
		return Outcome{ThingsID: resp.ID}, nil
	}

	if job.Task.HeadingOperation != "" && job.Task.HeadingRequest != nil {
		switch job.Task.HeadingOperation {
		case "create":
			resp, err := p.Helper.CreateHeading(ctx, job.Task.HeadingRequest.Project, job.Task.HeadingRequest.Heading)
			if err != nil {
				return Outcome{}, err
			}
			return Outcome{ThingsID: resp.ID}, nil
		case "archive":
			resp, err := p.Helper.ArchiveHeading(ctx, job.Task.HeadingRequest.Project, job.Task.HeadingRequest.Heading)
			if err != nil {
				return Outcome{}, err
			}
			return Outcome{ThingsID: resp.ID}, nil
		case "rename":
			resp, err := p.Helper.RenameHeading(ctx, job.Task.HeadingRequest.Project, job.Task.HeadingRequest.Heading, job.Task.HeadingRequest.NewTitle)
			if err != nil {
				return Outcome{}, err
			}
			return Outcome{ThingsID: resp.ID}, nil
		}
	}

	payloadHash, err := job.Task.Hash()
	if err != nil {
		return Outcome{}, fmt.Errorf("hash capture job: %w", err)
	}
	entry, fresh, err := p.Journal.Ensure(ctx, job.ID, payloadHash)
	if err != nil {
		if errors.Is(err, journal.ErrPayloadMismatch) {
			return Outcome{}, permanentError(err)
		}
		return Outcome{}, err
	}

	if entry.State == journal.StateReported || entry.State == journal.StateFinalised {
		return Outcome{ThingsID: entry.ThingsID, Warnings: warningsFromNotes(job.Task.Notes, entry.Notes)}, nil
	}

	if entry.State == journal.StateCreating && !fresh {
		ids, findErr := p.Helper.FindCapture(ctx, job.ID)
		if findErr != nil {
			return Outcome{}, fmt.Errorf("reconcile uncertain Things capture: %w", findErr)
		}
		switch len(ids) {
		case 0:
			// The helper did not create the task, so the same stable request may be attempted again.
		case 1:
			finalNotes := job.Task.Notes
			if err := p.Journal.MarkCreated(ctx, job.ID, ids[0], finalNotes); err != nil {
				return Outcome{}, err
			}
			entry, err = p.Journal.Get(ctx, job.ID)
			if err != nil {
				return Outcome{}, err
			}
		default:
			return Outcome{}, permanentError(fmt.Errorf("manual review required: %d Things tasks use the pending title for request %q", len(ids), job.ID))
		}
	}

	if entry.State == journal.StateReceived {
		if err := p.Journal.MarkCreating(ctx, job.ID); err != nil {
			return Outcome{}, err
		}
		entry.State = journal.StateCreating
	}

	if entry.State == journal.StateCreating && entry.ThingsID == "" {
		task := job.Task
		response, captureErr := p.Helper.Capture(ctx, job.ID, task)
		if isDestinationError(captureErr) && task.Destination != nil && task.Destination.Kind != capture.DestinationInbox {
			warning := fmt.Sprintf("ThingsIndex warning: destination %s was unavailable; captured in Inbox.", destinationLabel(*task.Destination))
			task.Destination = &capture.Destination{Kind: capture.DestinationInbox}
			task.Notes = appendWarning(task.Notes, warning)
			response, captureErr = p.Helper.Capture(ctx, job.ID, task)
		}
		if captureErr != nil {
			return Outcome{}, fmt.Errorf("create Things task: %w", captureErr)
		}
		finalNotes := task.Notes
		// The helper may omit appliedTags when it cannot verify Things' resulting
		// tag array. Omitted means "not verified", not "none applied".
		if response.AppliedTags != nil {
			for _, tag := range missingTags(task.Tags, response.AppliedTags) {
				finalNotes = appendWarning(finalNotes, fmt.Sprintf("ThingsIndex warning: tag %q did not exist and was not applied.", tag))
			}
		}
		if err := p.Journal.MarkCreated(ctx, job.ID, response.ID, finalNotes); err != nil {
			return Outcome{}, err
		}
		entry, err = p.Journal.Get(ctx, job.ID)
		if err != nil {
			return Outcome{}, err
		}
	}

	if entry.State == journal.StateCreated {
		if err := p.Helper.FinaliseCapture(ctx, entry.ThingsID, job.Task.Title); err != nil {
			return Outcome{}, fmt.Errorf("finalise Things capture: %w", err)
		}
		if err := p.Journal.MarkFinalised(ctx, job.ID); err != nil {
			return Outcome{}, err
		}
		entry.State = journal.StateFinalised
	}

	if entry.State != journal.StateFinalised {
		return Outcome{}, fmt.Errorf("delivery %q stopped in unexpected state %s", job.ID, entry.State)
	}
	return Outcome{ThingsID: entry.ThingsID, Warnings: warningsFromNotes(job.Task.Notes, entry.Notes)}, nil
}

func (p *Processor) MarkReported(ctx context.Context, jobID string) error {
	return p.Journal.MarkReported(ctx, jobID)
}

// UsesJournal reports whether processing this task records idempotency state
// in the journal. Query, update, archive, and heading operations execute
// directly and leave no journal entry to mark reported.
func UsesJournal(task capture.Request) bool {
	return task.QueryTasksRequest == nil &&
		task.CreateProjectRequest == nil &&
		task.UpdateTaskRequest == nil &&
		task.ArchiveTaskRequest == nil &&
		task.ArchiveProjectRequest == nil &&
		task.HeadingOperation == ""
}

func isDestinationError(err error) bool {
	var operationError *helper.OperationError
	if !errors.As(err, &operationError) {
		return false
	}
	return operationError.Code == "destination_not_found" ||
		operationError.Code == "destination_ambiguous" ||
		operationError.Code == "heading_not_found" ||
		operationError.Code == "heading_ambiguous"
}

func destinationLabel(destination capture.Destination) string {
	label := fmt.Sprintf("%s %q", destination.Kind, destination.Name)
	if destination.Heading != "" {
		label += fmt.Sprintf(" / heading %q", destination.Heading)
	}
	return label
}

func missingTags(requested, applied []string) []string {
	var missing []string
	for _, tag := range requested {
		if !slices.ContainsFunc(applied, func(candidate string) bool {
			return strings.EqualFold(candidate, tag)
		}) {
			missing = append(missing, tag)
		}
	}
	return missing
}

func appendWarning(notes, warning string) string {
	if notes == "" {
		return warning
	}
	return notes + "\n\n" + warning
}

func warningsFromNotes(original, final string) []string {
	if original == final {
		return nil
	}
	remaining := strings.TrimPrefix(final, original)
	remaining = strings.TrimSpace(remaining)
	if remaining == "" {
		return nil
	}
	parts := strings.Split(remaining, "\n\n")
	warnings := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.HasPrefix(part, "ThingsIndex warning:") {
			warnings = append(warnings, part)
		}
	}
	return warnings
}

func permanentError(err error) error {
	return &PermanentError{Err: err}
}

func validJobID(value string) bool {
	if len(value) != 32 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
