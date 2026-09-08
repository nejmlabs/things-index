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
	ID      string          `json:"id,omitempty" jsonschema:"Optional active project ID, for example the things_id from create_things_project. Takes precedence over name. An unavailable ID saves the new task in Inbox with requested placement in notes and a warning. Only supported for project destinations."`
	Name    string          `json:"name,omitempty" jsonschema:"Project name as spoken: exact matches first, then a clear partial name or typo match. Missing or ambiguous projects save the new task in Inbox with requested placement in notes and a warning. Area names must be exact. Optional with a project id; omit for Inbox."`
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
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"Optional key to reuse for retries of this exact write."`
	Project        string `json:"project" jsonschema:"Required exact name of the project."`
	Heading        string `json:"heading" jsonschema:"Required heading title."`
	NewTitle       string `json:"new_title,omitempty" jsonschema:"Optional new heading title (when renaming)."`
}

func (h HeadingRequest) Validate() error {
	if err := ValidateIdempotencyKey(h.IdempotencyKey); err != nil {
		return err
	}
	if strings.TrimSpace(h.Project) == "" {
		return errors.New("project name is required")
	}
	if strings.TrimSpace(h.Heading) == "" {
		return errors.New("heading title is required")
	}
	return nil
}

type ArchiveTaskRequest struct {
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"Optional key to reuse for retries of this exact write."`
	ID             string `json:"id,omitempty" jsonschema:"Optional Things task UUID."`
	Title          string `json:"title,omitempty" jsonschema:"Optional task title to look up if ID is omitted."`
	Project        string `json:"project,omitempty" jsonschema:"Optional project name to disambiguate the task title."`
	Action         string `json:"action,omitempty" jsonschema:"Archive action: complete (default), cancel, or trash."`
}

func (r ArchiveTaskRequest) Validate() error {
	if err := ValidateIdempotencyKey(r.IdempotencyKey); err != nil {
		return err
	}
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
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"Optional key to reuse for retries of this exact write."`
	ID             string `json:"id,omitempty" jsonschema:"Optional Things project UUID."`
	Name           string `json:"name,omitempty" jsonschema:"Optional project name to look up if ID is omitted."`
	Action         string `json:"action,omitempty" jsonschema:"Archive action: complete (default) or cancel."`
}

func (r ArchiveProjectRequest) Validate() error {
	if err := ValidateIdempotencyKey(r.IdempotencyKey); err != nil {
		return err
	}
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
	Scope            string `json:"scope,omitempty" jsonschema:"Scope: 'today', 'inbox', 'anytime', 'someday', 'projects', or 'all' (default: 'all'). The projects scope lists active projects and only applies limit."`
	Query            string `json:"query,omitempty" jsonschema:"Optional text search in task titles, notes, and project titles. Task scopes only."`
	Project          string `json:"project,omitempty" jsonschema:"Optional exact project name filter, ignoring case. Task scopes only."`
	Area             string `json:"area,omitempty" jsonschema:"Optional exact area name filter, ignoring case. Task scopes only."`
	Tag              string `json:"tag,omitempty" jsonschema:"Optional exact tag name filter, ignoring case. Task scopes only."`
	IncludeCompleted bool   `json:"include_completed,omitempty" jsonschema:"Also include completed and canceled tasks, subject to scope (default: false). Trash is always excluded; ignored for projects."`
	Limit            int    `json:"limit,omitempty" jsonschema:"Maximum results: 1 to 200. Omitted or out-of-range values use 50. No pagination."`
}

func (q QueryTasksRequest) Validate() error {
	return nil
}

type CreateProjectRequest struct {
	IdempotencyKey string   `json:"idempotency_key,omitempty" jsonschema:"Optional key to reuse for retries of this exact write."`
	Title          string   `json:"title" jsonschema:"Required title of the new project."`
	Area           string   `json:"area,omitempty" jsonschema:"Optional name of the area to place the project in."`
	Notes          string   `json:"notes,omitempty" jsonschema:"Optional project notes."`
	Deadline       string   `json:"deadline,omitempty" jsonschema:"Optional deadline in YYYY-MM-DD form."`
	When           string   `json:"when,omitempty" jsonschema:"Optional start time: today, evening, anytime, someday, or YYYY-MM-DD."`
	Tags           []string `json:"tags,omitempty" jsonschema:"Optional tag names."`
}

func (c CreateProjectRequest) Validate() error {
	if err := ValidateIdempotencyKey(c.IdempotencyKey); err != nil {
		return err
	}
	if err := validateUniqueStrings("project title", []string{c.Title}, MaxTitleBytes); err != nil {
		return err
	}
	if c.Title != strings.TrimSpace(c.Title) || strings.ContainsRune(c.Title, 0) {
		return errors.New("project title must not contain NUL or leading/trailing whitespace")
	}
	if c.Area != "" {
		if err := validateOrganizationName("area", c.Area); err != nil {
			return err
		}
	}
	if !utf8.ValidString(c.Notes) || len(c.Notes) > MaxNotesBytes || strings.ContainsRune(c.Notes, 0) {
		return errors.New("project notes must be valid UTF-8 without NUL and within the notes limit")
	}
	if c.Deadline != "" {
		if err := validateDate(c.Deadline); err != nil {
			return fmt.Errorf("invalid deadline: %w", err)
		}
	}
	switch c.When {
	case "", "today", "evening", "anytime", "someday":
	default:
		if err := validateDate(c.When); err != nil {
			return fmt.Errorf("invalid project start: %w", err)
		}
	}
	if len(c.Tags) > MaxTagCount {
		return fmt.Errorf("no more than %d tags are allowed", MaxTagCount)
	}
	if err := validateUniqueStrings("tag", c.Tags, MaxDestinationLen); err != nil {
		return err
	}
	for _, tag := range c.Tags {
		if strings.ContainsAny(tag, ",\x00") {
			return errors.New("tag names must not contain commas or NUL")
		}
	}
	return nil
}

type UpdateTaskRequest struct {
	IdempotencyKey string   `json:"idempotency_key,omitempty" jsonschema:"Optional key to reuse for retries of this exact write."`
	ID             string   `json:"id,omitempty" jsonschema:"Things task UUID (optional if title is provided)."`
	Title          string   `json:"title,omitempty" jsonschema:"Task title to find if ID is omitted."`
	Project        string   `json:"project,omitempty" jsonschema:"Optional project name to disambiguate the task title."`
	NewTitle       string   `json:"new_title,omitempty" jsonschema:"Optional new title for the task."`
	Notes          string   `json:"notes,omitempty" jsonschema:"Replace existing notes."`
	AppendNotes    string   `json:"append_notes,omitempty" jsonschema:"Append to existing notes."`
	Deadline       string   `json:"deadline,omitempty" jsonschema:"New deadline in YYYY-MM-DD form."`
	When           string   `json:"when,omitempty" jsonschema:"Reschedule task: today, evening, someday, anytime, or YYYY-MM-DD."`
	AddTags        []string `json:"add_tags,omitempty" jsonschema:"Tags to add."`
	AddChecklist   []string `json:"add_checklist,omitempty" jsonschema:"Checklist lines to append."`
}

func (u UpdateTaskRequest) Validate() error {
	if err := ValidateIdempotencyKey(u.IdempotencyKey); err != nil {
		return err
	}
	if strings.TrimSpace(u.ID) == "" && strings.TrimSpace(u.Title) == "" {
		return errors.New("either task id or title is required")
	}
	return nil
}

// TaskFields is the user-facing surface of a task capture and the input type
// of the capture_things_task tool on every server. Keeping it separate from
// Request means tool schemas built from it never advertise the internal
// job-envelope fields below, which bloated the advertised schema to 13KB and
// misled client-side LLMs into generating invalid calls.
type TaskFields struct {
	Title          string       `json:"title" jsonschema:"Things task title."`
	Notes          string       `json:"notes,omitempty" jsonschema:"Optional task notes."`
	Destination    *Destination `json:"destination,omitempty" jsonschema:"Optional destination. Projects accept a fuzzy name or an explicit id; missing or ambiguous projects save to Inbox with a warning. Omitted destinations go to Inbox."`
	Schedule       *Schedule    `json:"schedule,omitempty" jsonschema:"Optional Things start date and reminder."`
	Deadline       string       `json:"deadline,omitempty" jsonschema:"Optional deadline in YYYY-MM-DD form."`
	Tags           []string     `json:"tags,omitempty" jsonschema:"Existing Things tag names to apply."`
	Checklist      []string     `json:"checklist,omitempty" jsonschema:"Checklist lines to create."`
	IdempotencyKey string       `json:"idempotency_key,omitempty" jsonschema:"Optional client-provided idempotency key for safe retries."`
}

// Request is the internal job envelope: the task fields plus exactly one
// optional sub-request selecting a non-capture operation.
type Request struct {
	TaskFields
	HeadingOperation      string                 `json:"heading_operation,omitempty"`
	HeadingRequest        *HeadingRequest        `json:"heading_request,omitempty"`
	ArchiveTaskRequest    *ArchiveTaskRequest    `json:"archive_task_request,omitempty"`
	ArchiveProjectRequest *ArchiveProjectRequest `json:"archive_project_request,omitempty"`
	QueryTasksRequest     *QueryTasksRequest     `json:"query_tasks_request,omitempty"`
	CreateProjectRequest  *CreateProjectRequest  `json:"create_project_request,omitempty"`
	UpdateTaskRequest     *UpdateTaskRequest     `json:"update_task_request,omitempty"`
	CreateAreaRequest     *CreateAreaRequest     `json:"create_area_request,omitempty"`
	CreateTagRequest      *CreateTagRequest      `json:"create_tag_request,omitempty"`
}

func (r Request) Validate() error {
	if err := ValidateIdempotencyKey(r.IdempotencyKey); err != nil {
		return err
	}
	if key := r.OperationKey(); key != "" && r.IdempotencyKey != "" && key != r.IdempotencyKey {
		return errors.New("conflicting idempotency keys in job")
	}
	operations := 0
	for _, present := range []bool{r.QueryTasksRequest != nil, r.CreateProjectRequest != nil,
		r.UpdateTaskRequest != nil, r.ArchiveTaskRequest != nil, r.ArchiveProjectRequest != nil,
		r.HeadingOperation != "", r.CreateAreaRequest != nil, r.CreateTagRequest != nil} {
		if present {
			operations++
		}
	}
	if operations > 1 {
		return errors.New("a job must contain exactly one operation")
	}
	if r.HeadingRequest != nil && r.HeadingOperation == "" {
		return errors.New("heading_request requires a heading operation")
	}
	if r.HeadingOperation != "" && r.HeadingOperation != "create" && r.HeadingOperation != "rename" && r.HeadingOperation != "archive" {
		return errors.New("invalid heading operation")
	}
	if operations != 0 && (r.Title != "" || r.Notes != "" || r.Destination != nil || r.Schedule != nil || r.Deadline != "" || len(r.Tags) != 0 || len(r.Checklist) != 0) {
		return errors.New("task capture fields cannot be combined with another operation")
	}
	if r.CreateAreaRequest != nil {
		return r.CreateAreaRequest.Validate()
	}
	if r.CreateTagRequest != nil {
		return r.CreateTagRequest.Validate()
	}
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
		if r.HeadingOperation == "rename" && strings.TrimSpace(r.HeadingRequest.NewTitle) == "" {
			return errors.New("new_title is required when renaming a heading")
		}
		return r.HeadingRequest.Validate()
	}
	if strings.TrimSpace(r.Title) == "" {
		return errors.New("title is required")
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
			if r.Destination.ID != "" || r.Destination.Name != "" || r.Destination.Heading != "" {
				return errors.New("an Inbox destination must not have an id, name, or heading")
			}
		case DestinationProject:
			if r.Destination.ID == "" && strings.TrimSpace(r.Destination.Name) == "" {
				return errors.New("a project destination requires a name or id")
			}
			if r.Destination.ID != "" && (!utf8.ValidString(r.Destination.ID) || len(r.Destination.ID) > MaxDestinationLen || strings.TrimSpace(r.Destination.ID) != r.Destination.ID || strings.ContainsAny(r.Destination.ID, "\r\n\t")) {
				return errors.New("project id must be a nonblank single-line identifier of at most 400 bytes")
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
			if r.Destination.ID != "" {
				return errors.New("an explicit destination id is only supported for projects")
			}
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
