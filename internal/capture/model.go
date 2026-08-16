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
	Evening    bool      `json:"evening,omitempty" jsonschema:"Place a task starting today in This Evening."`
	ReminderAt string    `json:"reminder_at,omitempty" jsonschema:"Reminder timestamp with timezone in RFC3339 form."`
}

type Request struct {
	Title       string       `json:"title" jsonschema:"Required Things task title."`
	Notes       string       `json:"notes,omitempty" jsonschema:"Optional task notes."`
	Destination *Destination `json:"destination,omitempty" jsonschema:"Optional exact destination; omitted tasks go to Inbox."`
	Schedule    *Schedule    `json:"schedule,omitempty" jsonschema:"Optional Things start date and reminder."`
	Deadline    string       `json:"deadline,omitempty" jsonschema:"Optional deadline in YYYY-MM-DD form."`
	Tags        []string     `json:"tags,omitempty" jsonschema:"Existing Things tag names to apply."`
	Checklist   []string     `json:"checklist,omitempty" jsonschema:"Checklist lines to create."`
}

func (r Request) Validate() error {
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
