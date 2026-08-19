package capture

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxTitleBytes     = 4_000
	MaxNotesBytes     = 9_000
	MaxDestinationLen = 400
	MaxTagCount       = 100
	MaxChecklistCount = 100
)

type DestinationKind string

const (
	DestinationInbox   DestinationKind = "inbox"
	DestinationProject DestinationKind = "project"
	DestinationArea    DestinationKind = "area"
)

type Destination struct {
	Kind    DestinationKind `json:"kind" jsonschema:"Where to place the task: inbox, project, or area."`
	Name    string          `json:"name,omitempty" jsonschema:"Exact project or area name. Omit for Inbox."`
	Heading string          `json:"heading,omitempty" jsonschema:"Optional exact heading name inside the selected project."`
}

type StartKind string

const (
	StartAnytime StartKind = "anytime"
	StartSomeday StartKind = "someday"
	StartOnDate  StartKind = "on_date"
)

type Schedule struct {
	Start      StartKind `json:"start,omitempty" jsonschema:"When the task starts: anytime, someday, or on_date."`
	Date       string    `json:"date,omitempty" jsonschema:"Start date in YYYY-MM-DD form; required for on_date."`
	Evening    bool      `json:"evening,omitempty" jsonschema:"Place the task in This Evening when its start date is today; for any other date the task is scheduled for that date instead."`
	ReminderAt string    `json:"reminder_at,omitempty" jsonschema:"Reminder timestamp with timezone in RFC3339 form."`
}

type HeadingRequest struct {
	Project  string `json:"project" jsonschema:"Required exact name of the project."`
	Heading  string `json:"heading" jsonschema:"Required heading title."`
	NewTitle string `json:"new_title,omitempty" jsonschema:"Optional new heading title (when renaming)."`
}

func (h HeadingRequest) Validate() error {
	if strings.TrimSpace(h.Project) == "" {
		return errors.New("project name is required")
	}
	if strings.TrimSpace(h.Heading) == "" {
		return errors.New("heading title is required")
	}
	return nil
}

type ArchiveTaskRequest struct {
	ID      string `json:"id,omitempty" jsonschema:"Optional Things task UUID."`
	Title   string `json:"title,omitempty" jsonschema:"Optional task title to look up if ID is omitted."`
	Project string `json:"project,omitempty" jsonschema:"Optional project name to disambiguate the task title."`
	Action  string `json:"action,omitempty" jsonschema:"Archive action: complete (default), cancel, or trash."`
}

func (r ArchiveTaskRequest) Validate() error {
	if strings.TrimSpace(r.ID) == "" && strings.TrimSpace(r.Title) == "" {
		return errors.New("either task id or title is required")
	}
	switch r.Action {
	case "", "complete", "cancel", "trash":
	default:
		return errors.New("action must be 'complete', 'cancel', or 'trash'")
	}
	return nil
}

type ArchiveProjectRequest struct {
	ID     string `json:"id,omitempty" jsonschema:"Optional Things project UUID."`
	Name   string `json:"name,omitempty" jsonschema:"Optional project name to look up if ID is omitted."`
	Action string `json:"action,omitempty" jsonschema:"Archive action: complete (default) or cancel."`
}

func (r ArchiveProjectRequest) Validate() error {
	if strings.TrimSpace(r.ID) == "" && strings.TrimSpace(r.Name) == "" {
		return errors.New("either project id or name is required")
	}
	switch r.Action {
	case "", "complete", "cancel":
	default:
		return errors.New("action must be 'complete' or 'cancel'")
	}
	return nil
}

type QueryTasksRequest struct {
	Scope            string `json:"scope,omitempty" jsonschema:"Scope: 'today', 'inbox', 'anytime', 'someday', 'projects', or 'all' (default: 'today')."`
	Query            string `json:"query,omitempty" jsonschema:"Optional search text to filter by title and notes."`
	Project          string `json:"project,omitempty" jsonschema:"Optional project name filter."`
	Area             string `json:"area,omitempty" jsonschema:"Optional area name filter."`
	Tag              string `json:"tag,omitempty" jsonschema:"Optional tag name filter."`
	IncludeCompleted bool   `json:"include_completed,omitempty" jsonschema:"Include completed tasks (default: false)."`
	Limit            int    `json:"limit,omitempty" jsonschema:"Max results to return (default: 50)."`
}

func (q QueryTasksRequest) Validate() error {
	return nil
}

type CreateProjectRequest struct {
	Title    string   `json:"title" jsonschema:"Required title of the new project."`
	Area     string   `json:"area,omitempty" jsonschema:"Optional name of the area to place the project in."`
	Notes    string   `json:"notes,omitempty" jsonschema:"Optional project notes."`
	Deadline string   `json:"deadline,omitempty" jsonschema:"Optional deadline in YYYY-MM-DD form."`
	When     string   `json:"when,omitempty" jsonschema:"Optional start time: today, someday, or YYYY-MM-DD."`
	Tags     []string `json:"tags,omitempty" jsonschema:"Optional tag names."`
}

func (c CreateProjectRequest) Validate() error {
	if strings.TrimSpace(c.Title) == "" {
		return errors.New("project title is required")
	}
	return nil
}

type UpdateTaskRequest struct {
	ID           string   `json:"id,omitempty" jsonschema:"Things task UUID (optional if title is provided)."`
	Title        string   `json:"title,omitempty" jsonschema:"Task title to find if ID is omitted."`
	Project      string   `json:"project,omitempty" jsonschema:"Optional project name to disambiguate the task title."`
	NewTitle     string   `json:"new_title,omitempty" jsonschema:"Optional new title for the task."`
	Notes        string   `json:"notes,omitempty" jsonschema:"Replace existing notes."`
	AppendNotes  string   `json:"append_notes,omitempty" jsonschema:"Append to existing notes."`
	Deadline     string   `json:"deadline,omitempty" jsonschema:"New deadline in YYYY-MM-DD form."`
	When         string   `json:"when,omitempty" jsonschema:"Reschedule task: today, evening, someday, anytime, or YYYY-MM-DD."`
	AddTags      []string `json:"add_tags,omitempty" jsonschema:"Tags to add."`
	AddChecklist []string `json:"add_checklist,omitempty" jsonschema:"Checklist lines to append."`
}

func (u UpdateTaskRequest) Validate() error {
	if strings.TrimSpace(u.ID) == "" && strings.TrimSpace(u.Title) == "" {
		return errors.New("either task id or title is required")
	}
	return nil
}

type Request struct {
	Title                 string                 `json:"title,omitempty" jsonschema:"Things task title."`
	Notes                 string                 `json:"notes,omitempty" jsonschema:"Optional task notes."`
	Destination           *Destination           `json:"destination,omitempty" jsonschema:"Optional exact destination; omitted tasks go to Inbox."`
	Schedule              *Schedule              `json:"schedule,omitempty" jsonschema:"Optional Things start date and reminder."`
	Deadline              string                 `json:"deadline,omitempty" jsonschema:"Optional deadline in YYYY-MM-DD form."`
	Tags                  []string               `json:"tags,omitempty" jsonschema:"Existing Things tag names to apply."`
	Checklist             []string               `json:"checklist,omitempty" jsonschema:"Checklist lines to create."`
	IdempotencyKey        string                 `json:"idempotency_key,omitempty" jsonschema:"Optional client-provided idempotency key for safe retries."`
	HeadingOperation      string                 `json:"heading_operation,omitempty"`
	HeadingRequest        *HeadingRequest        `json:"heading_request,omitempty"`
	ArchiveTaskRequest    *ArchiveTaskRequest    `json:"archive_task_request,omitempty"`
	ArchiveProjectRequest *ArchiveProjectRequest `json:"archive_project_request,omitempty"`
	QueryTasksRequest     *QueryTasksRequest     `json:"query_tasks_request,omitempty"`
	CreateProjectRequest  *CreateProjectRequest  `json:"create_project_request,omitempty"`
	UpdateTaskRequest     *UpdateTaskRequest     `json:"update_task_request,omitempty"`
}

func (r Request) Validate() error {
	if r.QueryTasksRequest != nil {
		return r.QueryTasksRequest.Validate()
	}
	if r.CreateProjectRequest != nil {
		return r.CreateProjectRequest.Validate()
	}
	if r.UpdateTaskRequest != nil {
		return r.UpdateTaskRequest.Validate()
	}
	if r.ArchiveTaskRequest != nil {
		return r.ArchiveTaskRequest.Validate()
	}
	if r.ArchiveProjectRequest != nil {
		return r.ArchiveProjectRequest.Validate()
	}
	if r.HeadingOperation != "" {
		if r.HeadingRequest == nil {
			return errors.New("heading_request is required when heading_operation is set")
		}
		return r.HeadingRequest.Validate()
	}
	if strings.TrimSpace(r.Title) == "" {
		return errors.New("title is required")
	}
	if r.IdempotencyKey != "" {
		if !utf8.ValidString(r.IdempotencyKey) || len(r.IdempotencyKey) > 128 {
			return errors.New("idempotency_key must be valid UTF-8 and at most 128 bytes")
		}
		if strings.ContainsAny(r.IdempotencyKey, "\r\n") {
			return errors.New("idempotency_key must be a single line")
		}
	}
	if !utf8.ValidString(r.Title) || len(r.Title) > MaxTitleBytes {
		return fmt.Errorf("title must be valid UTF-8 and at most %d bytes", MaxTitleBytes)
	}
	if !utf8.ValidString(r.Notes) || len(r.Notes) > MaxNotesBytes {
		return fmt.Errorf("notes must be valid UTF-8 and at most %d bytes", MaxNotesBytes)
	}
	if r.Destination != nil {
		switch r.Destination.Kind {
		case DestinationInbox:
			if r.Destination.Name != "" || r.Destination.Heading != "" {
				return errors.New("an Inbox destination must not have a name or heading")
			}
		case DestinationProject:
			if strings.TrimSpace(r.Destination.Name) == "" {
				return errors.New("a project destination requires an exact name")
			}
			if !utf8.ValidString(r.Destination.Name) || len(r.Destination.Name) > MaxDestinationLen {
				return fmt.Errorf("destination name must be valid UTF-8 and at most %d bytes", MaxDestinationLen)
			}
			if !utf8.ValidString(r.Destination.Heading) || len(r.Destination.Heading) > MaxDestinationLen {
				return fmt.Errorf("heading must be valid UTF-8 and at most %d bytes", MaxDestinationLen)
			}
			if r.Destination.Heading != "" && strings.TrimSpace(r.Destination.Heading) == "" {
				return errors.New("heading must not be blank")
			}
		case DestinationArea:
			if strings.TrimSpace(r.Destination.Name) == "" {
				return errors.New("an area destination requires an exact name")
			}
			if !utf8.ValidString(r.Destination.Name) || len(r.Destination.Name) > MaxDestinationLen {
				return fmt.Errorf("destination name must be valid UTF-8 and at most %d bytes", MaxDestinationLen)
			}
			if r.Destination.Heading != "" {
				return errors.New("a heading is valid only inside a project")
			}
		default:
			return fmt.Errorf("unsupported destination kind %q", r.Destination.Kind)
		}
	}
	if r.Schedule != nil {
		switch r.Schedule.Start {
		case "", StartAnytime, StartSomeday:
			if r.Schedule.Date != "" {
				return errors.New("a start date is only valid with on_date")
			}
		case StartOnDate:
			if err := validateDate(r.Schedule.Date); err != nil {
				return fmt.Errorf("invalid start date: %w", err)
			}
		default:
			return fmt.Errorf("unsupported start kind %q", r.Schedule.Start)
		}
		if r.Schedule.Evening && r.Schedule.Start != StartOnDate {
			return errors.New("evening requires an on_date schedule")
		}
		if r.Schedule.ReminderAt != "" {
			if r.Schedule.Start != StartOnDate {
				return errors.New("a reminder requires an on_date schedule")
			}
			if _, err := time.Parse(time.RFC3339, r.Schedule.ReminderAt); err != nil {
				return errors.New("reminder_at must be an RFC3339 timestamp with timezone")
			}
		}
	}
	if r.Deadline != "" {
		if err := validateDate(r.Deadline); err != nil {
			return fmt.Errorf("invalid deadline: %w", err)
		}
	}
	if len(r.Tags) > MaxTagCount {
		return fmt.Errorf("no more than %d tags are allowed", MaxTagCount)
	}
	if err := validateUniqueStrings("tag", r.Tags, MaxDestinationLen); err != nil {
		return err
	}
	if len(r.Checklist) > MaxChecklistCount {
		return fmt.Errorf("no more than %d checklist items are allowed", MaxChecklistCount)
	}
	if err := validateUniqueStrings("checklist item", r.Checklist, MaxTitleBytes); err != nil {
		return err
	}
	return nil
}

func (r Request) Hash() (string, error) {
	data, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func validateDate(value string) error {
	if value == "" {
		return errors.New("date is required")
	}
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil || parsed.Format("2006-01-02") != value {
		return errors.New("expected YYYY-MM-DD")
	}
	return nil
}

func validateUniqueStrings(label string, values []string, maxBytes int) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return fmt.Errorf("%s must not be empty", label)
		}
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("%s must be a single line", label)
		}
		if !utf8.ValidString(value) || len(value) > maxBytes {
			return fmt.Errorf("%s must be valid UTF-8 and at most %d bytes", label, maxBytes)
		}
		key := strings.ToLower(trimmed)
		if _, ok := seen[key]; ok {
			return fmt.Errorf("duplicate %s %q", label, value)
		}
		seen[key] = struct{}{}
	}
	return nil
}
