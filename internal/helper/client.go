package helper

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nejmlabs/things-index/internal/capture"
)

const (
	SchemaVersion = 1
	ShortcutName  = "ThingsIndex Helper"
)

var requiredCapabilities = []string{"capture-task-v5", "find-capture-v1", "finalise-capture-v1"}

type CommandRunner interface {
	Run(ctx context.Context, executable string, args []string) (stdout []byte, stderr []byte, err error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, executable string, args []string) ([]byte, []byte, error) {
	command := exec.CommandContext(ctx, executable, args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

type Client struct {
	TempDir  string
	Timeout  time.Duration
	Runner   CommandRunner
	Location *time.Location
	Now      func() time.Time
}

type Response struct {
	SchemaVersion int      `json:"schemaVersion"`
	OK            bool     `json:"ok"`
	Capabilities  []string `json:"capabilities,omitempty"`
	ID            string   `json:"id,omitempty"`
	IDs           []string `json:"ids,omitempty"`
	AppliedTags   []string `json:"appliedTags,omitempty"`
	Code          string   `json:"code,omitempty"`
}

type OperationError struct {
	Code string
}

func (e *OperationError) Error() string {
	if e.Code == "" {
		return "the ThingsIndex helper rejected the operation"
	}
	return "the ThingsIndex helper rejected the operation: " + e.Code
}

type CaptureRequest struct {
	SchemaVersion int          `json:"schemaVersion"`
	Operation     string       `json:"operation"`
	RequestID     string       `json:"requestId"`
	Task          shortcutTask `json:"task"`
}

type FindCaptureRequest struct {
	SchemaVersion int    `json:"schemaVersion"`
	Operation     string `json:"operation"`
	RequestID     string `json:"requestId"`
}

type FinaliseCaptureRequest struct {
	SchemaVersion int    `json:"schemaVersion"`
	Operation     string `json:"operation"`
	ID            string `json:"id"`
	Title         string `json:"title"`
}

type shortcutTask struct {
	Title                string               `json:"title"`
	Notes                string               `json:"notes"`
	Destination          *capture.Destination `json:"destination,omitempty"`
	Start                capture.StartKind    `json:"start,omitempty"`
	StartDate            string               `json:"startDate,omitempty"`
	Evening              bool                 `json:"evening"`
	ReminderTime         string               `json:"reminderTime,omitempty"`
	Deadline             string               `json:"deadline,omitempty"`
	StartDayOffset       int                  `json:"startDayOffset"`
	ReminderMinuteOffset int                  `json:"reminderMinuteOffset"`
	DeadlineDayOffset    int                  `json:"deadlineDayOffset"`
	Tags                 []string             `json:"tags"`
	Checklist            string               `json:"checklist"`
}

func NewClient(tempDir string) *Client {
	return &Client{TempDir: tempDir, Timeout: 30 * time.Second, Runner: ExecRunner{}, Location: time.Local, Now: time.Now}
}

func (c *Client) Ping(ctx context.Context) error {
	response, err := c.run(ctx, map[string]any{"schemaVersion": SchemaVersion, "operation": "ping"})
	if err != nil {
		return err
	}
	for _, capability := range requiredCapabilities {
		if !slices.Contains(response.Capabilities, capability) {
			return fmt.Errorf("installed %s lacks required capability %q", ShortcutName, capability)
		}
	}
	return nil
}

func (c *Client) Capture(ctx context.Context, requestID string, task capture.Request) (Response, error) {
	if !validRequestID(requestID) {
		return Response{}, errors.New("request identifier must be a 32-character lowercase hexadecimal value")
	}
	if err := task.Validate(); err != nil {
		return Response{}, err
	}
	wireTask := shortcutTask{
		Title:       task.Title,
		Notes:       task.Notes,
		Destination: task.Destination,
		Deadline:    task.Deadline,
		Tags:        append([]string{}, task.Tags...),
		Checklist:   strings.Join(task.Checklist, "\n"),
	}
	location := c.Location
	if location == nil {
		location = time.Local
	}
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	today := calendarDay(now.In(location))
	if task.Schedule != nil {
		wireTask.Start = task.Schedule.Start
		wireTask.StartDate = task.Schedule.Date
		wireTask.Evening = task.Schedule.Evening
		if task.Schedule.Start == capture.StartOnDate {
			startDate, err := time.Parse("2006-01-02", task.Schedule.Date)
			if err != nil {
				return Response{}, fmt.Errorf("parse start date: %w", err)
			}
			wireTask.StartDayOffset = calendarDayOffset(today, startDate)
		}
		if task.Schedule.ReminderAt != "" {
			reminder, err := time.Parse(time.RFC3339, task.Schedule.ReminderAt)
			if err != nil {
				return Response{}, fmt.Errorf("parse reminder time: %w", err)
			}
			localReminder := reminder.In(location)
			wireTask.ReminderTime = localReminder.Format("15:04")
			wireTask.ReminderMinuteOffset = localReminder.Hour()*60 + localReminder.Minute()
		}
	}
	if task.Deadline != "" {
		deadline, err := time.Parse("2006-01-02", task.Deadline)
		if err != nil {
			return Response{}, fmt.Errorf("parse deadline: %w", err)
		}
		wireTask.DeadlineDayOffset = calendarDayOffset(today, deadline)
	}
	response, err := c.run(ctx, CaptureRequest{
		SchemaVersion: SchemaVersion,
		Operation:     "capture-task",
		RequestID:     requestID,
		Task:          wireTask,
	})
	if err != nil {
		return Response{}, err
	}
	if strings.TrimSpace(response.ID) == "" {
		return Response{}, errors.New("the ThingsIndex helper did not return the created Things identifier")
	}
	return response, nil
}

func calendarDay(value time.Time) time.Time {
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
}

func calendarDayOffset(from, to time.Time) int {
	return int(calendarDay(to).Sub(calendarDay(from)) / (24 * time.Hour))
}

func (c *Client) FindCapture(ctx context.Context, requestID string) ([]string, error) {
	if !validRequestID(requestID) {
		return nil, errors.New("request identifier must be a 32-character lowercase hexadecimal value")
	}
	response, err := c.run(ctx, FindCaptureRequest{
		SchemaVersion: SchemaVersion,
		Operation:     "find-capture",
		RequestID:     requestID,
	})
	if err != nil {
		return nil, err
	}
	if len(response.IDs) > 2 {
		return nil, errors.New("the ThingsIndex helper returned more than two recovery matches")
	}
	seen := make(map[string]struct{}, len(response.IDs))
	for _, id := range response.IDs {
		if strings.TrimSpace(id) == "" {
			return nil, errors.New("the ThingsIndex helper returned an empty Things identifier")
		}
		if _, exists := seen[id]; exists {
			return nil, errors.New("the ThingsIndex helper returned a duplicate Things identifier")
		}
		seen[id] = struct{}{}
	}
	return append([]string(nil), response.IDs...), nil
}

func (c *Client) FinaliseCapture(ctx context.Context, id, title string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("Things identifier is required")
	}
	if strings.TrimSpace(title) == "" {
		return errors.New("final Things title is required")
	}
	_, err := c.run(ctx, FinaliseCaptureRequest{
		SchemaVersion: SchemaVersion,
		Operation:     "finalise-capture",
		ID:            id,
		Title:         title,
	})
	return err
}

func (c *Client) run(ctx context.Context, request any) (Response, error) {
	if c.Runner == nil {
		return Response{}, errors.New("a helper command runner is required")
	}
	tempDir := c.TempDir
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		return Response{}, fmt.Errorf("prepare helper request directory: %w", err)
	}
	file, err := os.CreateTemp(tempDir, "things-index-request-*.json")
	if err != nil {
		return Response{}, fmt.Errorf("create helper request file: %w", err)
	}
	path := file.Name()
	defer os.Remove(path)
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return Response{}, fmt.Errorf("secure helper request file: %w", err)
	}
	encoder := json.NewEncoder(file)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(request); err != nil {
		file.Close()
		return Response{}, fmt.Errorf("encode helper request: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return Response{}, fmt.Errorf("flush helper request: %w", err)
	}
	if err := file.Close(); err != nil {
		return Response{}, fmt.Errorf("close helper request: %w", err)
	}

	stdout, stderr, err := c.runCommand(ctx, "/usr/bin/shortcuts", []string{
		"run", ShortcutName, "--input-path", filepath.Clean(path), "--output-type", "public.json",
	})
	if err != nil {
		return Response{}, fmt.Errorf("run ThingsIndex helper: %w (%s)", err, strings.TrimSpace(string(stderr)))
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout))
	decoder.DisallowUnknownFields()
	var response Response
	if err := decoder.Decode(&response); err != nil {
		return Response{}, errors.New("the ThingsIndex helper returned invalid JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Response{}, errors.New("the ThingsIndex helper returned more than one response")
	}
	if response.SchemaVersion != SchemaVersion {
		return Response{}, fmt.Errorf("unsupported ThingsIndex helper schema version %d", response.SchemaVersion)
	}
	if !response.OK {
		return Response{}, &OperationError{Code: response.Code}
	}
	return response, nil
}

func (c *Client) runCommand(ctx context.Context, executable string, args []string) ([]byte, []byte, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	commandContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stdout, stderr, err := c.Runner.Run(commandContext, executable, args)
	if err != nil && commandContext.Err() != nil {
		return nil, nil, errors.New("the ThingsIndex helper timed out")
	}
	return stdout, stderr, err
}

func validRequestID(value string) bool {
	if len(value) != 32 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
