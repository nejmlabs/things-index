package helper

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/nejmlabs/things-index/internal/capture"
)

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
	DBPath    string
	AuthToken string
	Timeout   time.Duration
	Runner    CommandRunner
	Location  *time.Location
	Now       func() time.Time
}

type Response struct {
	OK          bool     `json:"ok"`
	ID          string   `json:"id,omitempty"`
	IDs         []string `json:"ids,omitempty"`
	AppliedTags []string `json:"appliedTags,omitempty"`
	Code        string   `json:"code,omitempty"`
}

type OperationError struct {
	Code string
}

func (e *OperationError) Error() string {
	if e.Code == "" {
		return "the Things operation failed"
	}
	return "the Things operation failed: " + e.Code
}

func NewClient(authToken string) *Client {
	return &Client{
		AuthToken: strings.TrimSpace(authToken),
		Timeout:   30 * time.Second,
		Runner:    ExecRunner{},
		Location:  time.Local,
		Now:       time.Now,
	}
}

func FindThingsDatabase() (string, error) {
	if env := strings.TrimSpace(os.Getenv("THINGS_INDEX_THINGS_DB_PATH")); env != "" {
		return env, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get user home directory: %w", err)
	}
	groupDir := filepath.Join(home, "Library", "Group Containers", "JLMPQHK86H.com.culturedcode.ThingsMac")
	patterns := []string{
		filepath.Join(groupDir, "ThingsData-*", "Things Database.thingsdatabase", "main.sqlite"),
		filepath.Join(groupDir, "ThingsDatabase.thingsdatabase", "main.sqlite"),
		filepath.Join(groupDir, "Things Database.thingsdatabase", "main.sqlite"),
	}
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err == nil && len(matches) > 0 {
			return matches[0], nil
		}
	}
	return "", errors.New("Things 3 SQLite database not found in Group Containers")
}

func (c *Client) openDB(ctx context.Context) (*sql.DB, error) {
	dbPath := c.DBPath
	if dbPath == "" {
		var err error
		dbPath, err = FindThingsDatabase()
		if err != nil {
			return nil, err
		}
	}
	dsn := dbPath + "?_query_only=1&_busy_timeout=5000"
	if strings.HasPrefix(dbPath, "file:") {
		separator := "?"
		if strings.Contains(dbPath, "?") {
			separator = "&"
		}
		dsn = dbPath
		if !strings.Contains(dbPath, "_query_only") {
			dsn += separator + "_query_only=1&_busy_timeout=5000"
		}
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open Things database: %w", err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func (c *Client) Ping(ctx context.Context) error {
	db, err := c.openDB(ctx)
	if err != nil {
		return fmt.Errorf("connect to Things 3 database: %w", err)
	}
	defer db.Close()

	var count int
	err = db.QueryRowContext(ctx, `SELECT count(*) FROM TMTask WHERE trashed = 0 LIMIT 1`).Scan(&count)
	if err != nil {
		return fmt.Errorf("query Things 3 database: %w", err)
	}
	return nil
}

func (c *Client) FindCapture(ctx context.Context, requestID string) ([]string, error) {
	if !validRequestID(requestID) {
		return nil, errors.New("request identifier must be a 32-character lowercase hexadecimal value")
	}
	db, err := c.openDB(ctx)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	pendingTitle := fmt.Sprintf("ThingsIndex pending [%s]", requestID)
	rows, err := db.QueryContext(ctx, `
		SELECT uuid FROM TMTask
		WHERE (title = ? OR notes LIKE '%' || ? || '%') AND trashed = 0
		ORDER BY creationDate DESC
		LIMIT 2`, pendingTitle, requestID)
	if err != nil {
		return nil, fmt.Errorf("find pending Things capture: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan pending Things task ID: %w", err)
		}
		if id != "" {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending Things captures: %w", err)
	}
	return ids, nil
}

func (c *Client) Capture(ctx context.Context, requestID string, task capture.Request) (Response, error) {
	if !validRequestID(requestID) {
		return Response{}, errors.New("request identifier must be a 32-character lowercase hexadecimal value")
	}
	if err := task.Validate(); err != nil {
		return Response{}, err
	}
	db, err := c.openDB(ctx)
	if err != nil {
		return Response{}, err
	}
	defer db.Close()

	location := c.Location
	if location == nil {
		location = time.Local
	}

	// 1. Preflight destination existence and ambiguity
	if task.Destination != nil && task.Destination.Name != "" {
		switch task.Destination.Kind {
		case capture.DestinationProject:
			projectUUID, err := findProjectUUID(ctx, db, task.Destination.Name)
			if err != nil {
				return Response{}, err
			}

			if task.Destination.Heading != "" {
				headingRows, err := db.QueryContext(ctx, `SELECT uuid FROM TMTask WHERE type = 2 AND LOWER(title) = LOWER(?) AND project = ? AND trashed = 0`, task.Destination.Heading, projectUUID)
				if err != nil {
					return Response{}, fmt.Errorf("query destination heading: %w", err)
				}
				var headingCount int
				for headingRows.Next() {
					headingCount++
				}
				headingRows.Close()
				if headingCount == 0 {
					return Response{}, &OperationError{Code: "heading_not_found"}
				}
				if headingCount > 1 {
					return Response{}, &OperationError{Code: "heading_ambiguous"}
				}
			}

		case capture.DestinationArea:
			areaRows, err := db.QueryContext(ctx, `SELECT uuid FROM TMArea WHERE LOWER(title) = LOWER(?) AND (visible IS NULL OR visible != 0)`, task.Destination.Name)
			if err != nil {
				return Response{}, fmt.Errorf("query destination area: %w", err)
			}
			var areaCount int
			for areaRows.Next() {
				areaCount++
			}
			areaRows.Close()
			if areaCount == 0 {
				return Response{}, &OperationError{Code: "destination_not_found"}
			}
			if areaCount > 1 {
				return Response{}, &OperationError{Code: "destination_ambiguous"}
			}
		}
	}

	// 2. Query available tags
	var appliedTags []string
	if len(task.Tags) > 0 {
		tagRows, err := db.QueryContext(ctx, `SELECT title FROM TMTag`)
		if err == nil {
			defer tagRows.Close()
			existingTags := make(map[string]string)
			for tagRows.Next() {
				var tagTitle string
				if err := tagRows.Scan(&tagTitle); err == nil {
					existingTags[strings.ToLower(tagTitle)] = tagTitle
				}
			}
			for _, requested := range task.Tags {
				if canonical, found := existingTags[strings.ToLower(requested)]; found {
					appliedTags = append(appliedTags, canonical)
				}
			}
		}
	}

	runner := c.Runner
	if runner == nil {
		runner = ExecRunner{}
	}

	wasRunning := false
	if _, _, err := runner.Run(ctx, "/usr/bin/pgrep", []string{"-x", "Things3"}); err == nil {
		wasRunning = true
	}

	// 3. Construct Things URL with the pending marker title. The marker is what
	// lets FindCapture reconcile an uncertain outcome after a crash or retry
	// without creating a duplicate; FinaliseCapture renames it afterwards.
	beforeDispatch := time.Now().Add(-1 * time.Second)
	macEpoch := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	minCreationDate := beforeDispatch.UTC().Sub(macEpoch).Seconds()

	pendingTitle := fmt.Sprintf("ThingsIndex pending [%s]", requestID)
	addURL := buildAddURL(pendingTitle, task, appliedTags, c.AuthToken, location)

	// 4. Dispatch URL via open -g -j (do not bring forward, launch hidden)
	if _, _, err := runner.Run(ctx, "/usr/bin/open", []string{"-g", "-j", "-a", "/Applications/Things3.app", addURL}); err != nil {
		if _, _, err := runner.Run(ctx, "/usr/bin/open", []string{"-g", addURL}); err != nil {
			return Response{}, fmt.Errorf("dispatch Things add URL: %w", err)
		}
	}

	// 5. Poll SQLite for created task UUID
	var taskUUID string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		row := db.QueryRowContext(ctx, `SELECT uuid FROM TMTask WHERE title = ? AND creationDate >= ? AND trashed = 0 ORDER BY creationDate DESC LIMIT 1`, pendingTitle, minCreationDate)
		if err := row.Scan(&taskUUID); err == nil && taskUUID != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if taskUUID == "" {
		row := db.QueryRowContext(ctx, `SELECT uuid FROM TMTask WHERE title = ? AND trashed = 0 ORDER BY creationDate DESC LIMIT 1`, pendingTitle)
		_ = row.Scan(&taskUUID)
	}
	if taskUUID == "" {
		return Response{}, &OperationError{Code: "create_failed"}
	}

	// 6. Rename the pending marker to the final title while Things is still
	// open, so the later FinaliseCapture step is a no-op and never has to
	// relaunch Things. If this fails, the caller's retry reconciles via
	// FindCapture on the pending title instead of creating a duplicate.
	if err := c.FinaliseCapture(ctx, taskUUID, task.Title); err != nil {
		return Response{}, fmt.Errorf("finalise Things capture title: %w", err)
	}

	// If Things 3 was not already running, quit it cleanly so no dock dot remains
	if !wasRunning {
		_, _, _ = runner.Run(ctx, "/usr/bin/osascript", []string{"-e", `tell application "Things3" to quit`})
	}

	return Response{
		OK:          true,
		ID:          taskUUID,
		AppliedTags: appliedTags,
	}, nil
}

func (c *Client) FinaliseCapture(ctx context.Context, id, title string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("Things identifier is required")
	}
	if strings.TrimSpace(title) == "" {
		return errors.New("final Things title is required")
	}
	db, err := c.openDB(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	var currentTitle string
	err = db.QueryRowContext(ctx, `SELECT title FROM TMTask WHERE uuid = ? AND trashed = 0`, id).Scan(&currentTitle)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &OperationError{Code: "finalise_not_found"}
		}
		return fmt.Errorf("read task for finalisation: %w", err)
	}
	if currentTitle == title {
		return nil
	}

	runner := c.Runner
	if runner == nil {
		runner = ExecRunner{}
	}

	if c.AuthToken != "" {
		values := url.Values{
			"id":         {id},
			"title":      {title},
			"auth-token": {c.AuthToken},
		}
		updateURL := "things:///update?" + strings.ReplaceAll(values.Encode(), "+", "%20")
		if _, _, err := runner.Run(ctx, "/usr/bin/open", []string{"-g", updateURL}); err != nil {
			return fmt.Errorf("run Things update URL: %w", err)
		}
	} else {
		script := fmt.Sprintf(`tell application "Things3" to set name of (first to do whose id is "%s") to "%s"`, escapeAppleScriptString(id), escapeAppleScriptString(title))
		if _, _, err := runner.Run(ctx, "/usr/bin/osascript", []string{"-e", script}); err != nil {
			return fmt.Errorf("run Things AppleScript finalise: %w", err)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var updatedTitle string
		if err := db.QueryRowContext(ctx, `SELECT title FROM TMTask WHERE uuid = ? AND trashed = 0`, id).Scan(&updatedTitle); err == nil && updatedTitle == title {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}

func buildAddURL(pendingTitle string, task capture.Request, appliedTags []string, authToken string, location *time.Location) string {
	values := url.Values{}
	values.Set("title", pendingTitle)
	if task.Notes != "" {
		values.Set("notes", task.Notes)
	}
	values.Set("reveal", "false")
	if task.Destination != nil && task.Destination.Name != "" {
		values.Set("list", task.Destination.Name)
		if task.Destination.Heading != "" {
			values.Set("heading", task.Destination.Heading)
		}
	}
	if task.Schedule != nil {
		if task.Schedule.Evening {
			// Things treats "evening" as today's This Evening list; a reminder
			// time can ride along as evening@HH:MM.
			if task.Schedule.ReminderAt != "" {
				if reminderTime, err := time.Parse(time.RFC3339, task.Schedule.ReminderAt); err == nil {
					localReminder := reminderTime.In(location)
					values.Set("when", "evening@"+localReminder.Format("15:04"))
				} else {
					values.Set("when", "evening")
				}
			} else {
				values.Set("when", "evening")
			}
		} else if task.Schedule.Start == capture.StartOnDate {
			if task.Schedule.ReminderAt != "" {
				if reminderTime, err := time.Parse(time.RFC3339, task.Schedule.ReminderAt); err == nil {
					localReminder := reminderTime.In(location)
					values.Set("when", task.Schedule.Date+"@"+localReminder.Format("15:04"))
				} else {
					values.Set("when", task.Schedule.Date)
				}
			} else {
				values.Set("when", task.Schedule.Date)
			}
		} else if task.Schedule.Start == capture.StartSomeday {
			values.Set("when", "someday")
		} else if task.Schedule.Start == capture.StartAnytime {
			values.Set("when", "anytime")
		}
	}
	if task.Deadline != "" {
		values.Set("deadline", task.Deadline)
	}
	if len(appliedTags) > 0 {
		values.Set("tags", strings.Join(appliedTags, ","))
	}
	if len(task.Checklist) > 0 {
		values.Set("checklist-items", strings.Join(task.Checklist, "\n"))
	}
	if authToken != "" {
		values.Set("auth-token", authToken)
	}
	return "things:///add?" + strings.ReplaceAll(values.Encode(), "+", "%20")
}

func escapeAppleScriptString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	return s
}

func validRequestID(value string) bool {
	if len(value) != 32 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (c *Client) CreateHeading(ctx context.Context, project, headingTitle string) (Response, error) {
	if strings.TrimSpace(project) == "" {
		return Response{}, errors.New("project name is required")
	}
	if strings.TrimSpace(headingTitle) == "" {
		return Response{}, errors.New("heading title is required")
	}
	if c.AuthToken == "" {
		return Response{}, errors.New("Things authorization token is required for heading operations (set THINGS_INDEX_THINGS_AUTH_TOKEN)")
	}
	db, err := c.openDB(ctx)
	if err != nil {
		return Response{}, err
	}
	defer db.Close()

	// 1. Look up project
	projectUUID, err := findProjectUUID(ctx, db, project)
	if err != nil {
		return Response{}, err
	}

	// 2. Check if heading already exists
	var existingHeadingUUID string
	err = db.QueryRowContext(ctx, `SELECT uuid FROM TMTask WHERE type = 2 AND LOWER(title) = LOWER(?) AND project = ? AND trashed = 0`, headingTitle, projectUUID).Scan(&existingHeadingUUID)
	if err == nil && existingHeadingUUID != "" {
		return Response{OK: true, ID: existingHeadingUUID}, nil
	}

	runner := c.Runner
	if runner == nil {
		runner = ExecRunner{}
	}

	wasRunning := false
	if _, _, err := runner.Run(ctx, "/usr/bin/pgrep", []string{"-x", "Things3"}); err == nil {
		wasRunning = true
	}

	// 3. Construct things:///json URL payload. The update operation targets the
	// existing project by UUID and appends the heading to its items; without
	// "operation":"update" Things would create a duplicate project instead.
	jsonPayload := fmt.Sprintf(`[{"type":"project","operation":"update","id":%q,"attributes":{"items":[{"type":"heading","attributes":{"title":%q}}]}}]`, projectUUID, headingTitle)
	encodedData := strings.ReplaceAll(url.QueryEscape(jsonPayload), "+", "%20")
	addURL := fmt.Sprintf("things:///json?data=%s&reveal=false", encodedData)
	if c.AuthToken != "" {
		addURL += fmt.Sprintf("&auth-token=%s", url.QueryEscape(c.AuthToken))
	}

	// 4. Dispatch URL
	if _, _, err := runner.Run(ctx, "/usr/bin/open", []string{"-g", addURL}); err != nil {
		return Response{}, fmt.Errorf("dispatch Things add-heading URL: %w", err)
	}

	// 5. Poll for created heading UUID
	var headingUUID string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		row := db.QueryRowContext(ctx, `SELECT uuid FROM TMTask WHERE type = 2 AND LOWER(title) = LOWER(?) AND project = ? AND trashed = 0 ORDER BY creationDate DESC LIMIT 1`, headingTitle, projectUUID)
		if err := row.Scan(&headingUUID); err == nil && headingUUID != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if headingUUID == "" {
		return Response{}, &OperationError{Code: "create_failed"}
	}

	if !wasRunning {
		time.Sleep(100 * time.Millisecond)
		_, _, _ = runner.Run(ctx, "/usr/bin/osascript", []string{"-e", `tell application "Things3" to quit`})
	}

	return Response{OK: true, ID: headingUUID}, nil
}

func (c *Client) ArchiveHeading(ctx context.Context, project, headingTitle string) (Response, error) {
	if strings.TrimSpace(project) == "" {
		return Response{}, errors.New("project name is required")
	}
	if strings.TrimSpace(headingTitle) == "" {
		return Response{}, errors.New("heading title is required")
	}
	if c.AuthToken == "" {
		return Response{}, errors.New("Things authorization token is required for archiving headings (set THINGS_INDEX_THINGS_AUTH_TOKEN)")
	}
	db, err := c.openDB(ctx)
	if err != nil {
		return Response{}, err
	}
	defer db.Close()

	projectUUID, err := findProjectUUID(ctx, db, project)
	if err != nil {
		return Response{}, err
	}

	var headingUUID string
	err = db.QueryRowContext(ctx, `
		SELECT uuid FROM TMTask
		WHERE type = 2 AND LOWER(title) = LOWER(?) AND project = ? AND trashed = 0
	`, headingTitle, projectUUID).Scan(&headingUUID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Response{}, &OperationError{Code: "heading_not_found"}
		}
		return Response{}, fmt.Errorf("query heading: %w", err)
	}

	runner := c.Runner
	if runner == nil {
		runner = ExecRunner{}
	}

	wasRunning := false
	if _, _, err := runner.Run(ctx, "/usr/bin/pgrep", []string{"-x", "Things3"}); err == nil {
		wasRunning = true
	}

	// Headings share the to-do model, so the documented update command with
	// completed=true archives the heading row into the Logbook.
	values := url.Values{}
	values.Set("id", headingUUID)
	values.Set("completed", "true")
	values.Set("auth-token", c.AuthToken)
	values.Set("reveal", "false")
	updateURL := "things:///update?" + strings.ReplaceAll(values.Encode(), "+", "%20")

	if _, _, err := runner.Run(ctx, "/usr/bin/open", []string{"-g", updateURL}); err != nil {
		return Response{}, fmt.Errorf("dispatch Things update URL: %w", err)
	}

	// Verify the heading actually left the active project; open(1) exits 0
	// even when Things rejects the command, so success must come from SQLite.
	archived := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var status, trashed int
		err := db.QueryRowContext(ctx, `SELECT status, trashed FROM TMTask WHERE uuid = ?`, headingUUID).Scan(&status, &trashed)
		if err == nil && (status != 0 || trashed != 0) {
			archived = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !wasRunning {
		time.Sleep(100 * time.Millisecond)
		_, _, _ = runner.Run(ctx, "/usr/bin/osascript", []string{"-e", `tell application "Things3" to quit`})
	}

	if !archived {
		return Response{}, &OperationError{Code: "archive_failed"}
	}
	return Response{OK: true, ID: headingUUID}, nil
}

func (c *Client) RenameHeading(ctx context.Context, project, oldHeadingTitle, newHeadingTitle string) (Response, error) {
	if strings.TrimSpace(project) == "" {
		return Response{}, errors.New("project name is required")
	}
	if strings.TrimSpace(oldHeadingTitle) == "" {
		return Response{}, errors.New("current heading title is required")
	}
	if strings.TrimSpace(newHeadingTitle) == "" {
		return Response{}, errors.New("new heading title is required")
	}
	if c.AuthToken == "" {
		return Response{}, errors.New("Things authorization token is required for renaming headings (set THINGS_INDEX_THINGS_AUTH_TOKEN)")
	}
	db, err := c.openDB(ctx)
	if err != nil {
		return Response{}, err
	}
	defer db.Close()

	projectUUID, err := findProjectUUID(ctx, db, project)
	if err != nil {
		return Response{}, err
	}

	var headingUUID string
	err = db.QueryRowContext(ctx, `
		SELECT uuid FROM TMTask
		WHERE type = 2 AND LOWER(title) = LOWER(?) AND project = ? AND trashed = 0
	`, oldHeadingTitle, projectUUID).Scan(&headingUUID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Response{}, &OperationError{Code: "heading_not_found"}
		}
		return Response{}, fmt.Errorf("query heading: %w", err)
	}

	runner := c.Runner
	if runner == nil {
		runner = ExecRunner{}
	}

	wasRunning := false
	if _, _, err := runner.Run(ctx, "/usr/bin/pgrep", []string{"-x", "Things3"}); err == nil {
		wasRunning = true
	}

	// Headings share the to-do model, so the documented update command renames
	// the heading row by UUID.
	values := url.Values{}
	values.Set("id", headingUUID)
	values.Set("title", newHeadingTitle)
	values.Set("auth-token", c.AuthToken)
	values.Set("reveal", "false")
	updateURL := "things:///update?" + strings.ReplaceAll(values.Encode(), "+", "%20")

	if _, _, err := runner.Run(ctx, "/usr/bin/open", []string{"-g", updateURL}); err != nil {
		return Response{}, fmt.Errorf("dispatch Things update URL: %w", err)
	}

	// Verify the rename took effect; open(1) exits 0 even when Things rejects
	// the command, so success must come from SQLite.
	renamed := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var currentTitle string
		err := db.QueryRowContext(ctx, `SELECT title FROM TMTask WHERE uuid = ? AND trashed = 0`, headingUUID).Scan(&currentTitle)
		if err == nil && strings.EqualFold(currentTitle, newHeadingTitle) {
			renamed = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !wasRunning {
		time.Sleep(100 * time.Millisecond)
		_, _, _ = runner.Run(ctx, "/usr/bin/osascript", []string{"-e", `tell application "Things3" to quit`})
	}

	if !renamed {
		return Response{}, &OperationError{Code: "rename_failed"}
	}
	return Response{OK: true, ID: headingUUID}, nil
}

func findProjectUUID(ctx context.Context, db *sql.DB, project string) (string, error) {
	rows, err := db.QueryContext(ctx, `SELECT uuid FROM TMTask WHERE type = 1 AND LOWER(title) = LOWER(?) AND trashed = 0 AND status = 0`, project)
	if err != nil {
		return "", fmt.Errorf("query active project: %w", err)
	}
	var uuids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			uuids = append(uuids, id)
		}
	}
	rows.Close()
	if len(uuids) == 0 {
		fallbackRows, err := db.QueryContext(ctx, `SELECT uuid FROM TMTask WHERE type = 1 AND LOWER(title) = LOWER(?) AND trashed = 0`, project)
		if err != nil {
			return "", fmt.Errorf("query fallback project: %w", err)
		}
		for fallbackRows.Next() {
			var id string
			if err := fallbackRows.Scan(&id); err == nil {
				uuids = append(uuids, id)
			}
		}
		fallbackRows.Close()
	}
	if len(uuids) == 0 {
		return "", &OperationError{Code: "destination_not_found"}
	}
	if len(uuids) > 1 {
		return "", &OperationError{Code: "destination_ambiguous"}
	}
	return uuids[0], nil
}

func (c *Client) ArchiveTask(ctx context.Context, id, title, project, action string) (Response, error) {
	if strings.TrimSpace(id) == "" && strings.TrimSpace(title) == "" {
		return Response{}, errors.New("task id or title is required")
	}
	db, err := c.openDB(ctx)
	if err != nil {
		return Response{}, err
	}
	defer db.Close()

	taskUUID := id
	if taskUUID == "" {
		query := `SELECT uuid FROM TMTask WHERE type = 0 AND LOWER(title) = LOWER(?) AND trashed = 0 AND status = 0`
		args := []any{title}
		if project != "" {
			projUUID, err := findProjectUUID(ctx, db, project)
			if err != nil {
				return Response{}, err
			}
			query += ` AND project = ?`
			args = append(args, projUUID)
		}
		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			return Response{}, fmt.Errorf("query task: %w", err)
		}
		var found []string
		for rows.Next() {
			var uid string
			if err := rows.Scan(&uid); err == nil {
				found = append(found, uid)
			}
		}
		rows.Close()
		if len(found) == 0 {
			return Response{}, &OperationError{Code: "task_not_found"}
		}
		if len(found) > 1 {
			return Response{}, &OperationError{Code: "task_ambiguous"}
		}
		taskUUID = found[0]
	}

	runner := c.Runner
	if runner == nil {
		runner = ExecRunner{}
	}

	wasRunning := false
	if _, _, err := runner.Run(ctx, "/usr/bin/pgrep", []string{"-x", "Things3"}); err == nil {
		wasRunning = true
	}

	var script string
	switch action {
	case "cancel":
		script = fmt.Sprintf(`tell application "Things3"
  set aTask to (to do id %q)
  set status of aTask to canceled
end tell`, taskUUID)
	case "trash":
		script = fmt.Sprintf(`tell application "Things3"
  set aTask to (to do id %q)
  move aTask to list "Trash"
end tell`, taskUUID)
	default: // "complete"
		script = fmt.Sprintf(`tell application "Things3"
  set aTask to (to do id %q)
  set status of aTask to completed
end tell`, taskUUID)
	}

	if _, _, err := runner.Run(ctx, "/usr/bin/osascript", []string{"-e", script}); err != nil {
		return Response{}, fmt.Errorf("archive task via AppleScript: %w", err)
	}

	if !wasRunning {
		time.Sleep(50 * time.Millisecond)
		_, _, _ = runner.Run(ctx, "/usr/bin/osascript", []string{"-e", `tell application "Things3" to quit`})
	}

	return Response{OK: true, ID: taskUUID}, nil
}

func (c *Client) ArchiveProject(ctx context.Context, id, name, action string) (Response, error) {
	if strings.TrimSpace(id) == "" && strings.TrimSpace(name) == "" {
		return Response{}, errors.New("project id or name is required")
	}
	db, err := c.openDB(ctx)
	if err != nil {
		return Response{}, err
	}
	defer db.Close()

	projUUID := id
	if projUUID == "" {
		var err error
		projUUID, err = findProjectUUID(ctx, db, name)
		if err != nil {
			return Response{}, err
		}
	}

	runner := c.Runner
	if runner == nil {
		runner = ExecRunner{}
	}

	wasRunning := false
	if _, _, err := runner.Run(ctx, "/usr/bin/pgrep", []string{"-x", "Things3"}); err == nil {
		wasRunning = true
	}

	var script string
	switch action {
	case "cancel":
		script = fmt.Sprintf(`tell application "Things3"
  set aProj to (project id %q)
  set status of aProj to canceled
end tell`, projUUID)
	default: // "complete"
		script = fmt.Sprintf(`tell application "Things3"
  set aProj to (project id %q)
  set status of aProj to completed
end tell`, projUUID)
	}

	if _, _, err := runner.Run(ctx, "/usr/bin/osascript", []string{"-e", script}); err != nil {
		return Response{}, fmt.Errorf("archive project via AppleScript: %w", err)
	}

	if !wasRunning {
		time.Sleep(50 * time.Millisecond)
		_, _, _ = runner.Run(ctx, "/usr/bin/osascript", []string{"-e", `tell application "Things3" to quit`})
	}

	return Response{OK: true, ID: projUUID}, nil
}

type TaskItem struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Notes   string `json:"notes,omitempty"`
	Status  string `json:"status"`
	Project string `json:"project,omitempty"`
	Area    string `json:"area,omitempty"`
	Heading string `json:"heading,omitempty"`
}

type ProjectItem struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Area      string `json:"area,omitempty"`
	Notes     string `json:"notes,omitempty"`
	OpenCount int    `json:"open_count"`
}

func (c *Client) QueryTasks(ctx context.Context, req capture.QueryTasksRequest) (Response, error) {
	db, err := c.openDB(ctx)
	if err != nil {
		return Response{}, err
	}
	defer db.Close()

	if req.Limit <= 0 || req.Limit > 200 {
		req.Limit = 50
	}

	if req.Scope == "projects" {
		rows, err := db.QueryContext(ctx, `
			SELECT p.uuid, p.title, COALESCE(a.title, ''), COALESCE(p.notes, ''), p.openUntrashedLeafActionsCount
			FROM TMTask p
			LEFT JOIN TMArea a ON p.area = a.uuid
			WHERE p.type = 1 AND p.status = 0 AND p.trashed = 0
			ORDER BY p.`+"`index`"+` ASC LIMIT ?
		`, req.Limit)
		if err != nil {
			return Response{}, fmt.Errorf("query projects: %w", err)
		}
		defer rows.Close()
		projects := make([]ProjectItem, 0)
		for rows.Next() {
			var p ProjectItem
			if err := rows.Scan(&p.ID, &p.Title, &p.Area, &p.Notes, &p.OpenCount); err == nil {
				projects = append(projects, p)
			}
		}
		dataBytes, _ := json.Marshal(projects)
		return Response{OK: true, ID: string(dataBytes)}, nil
	}

	baseQuery := `
		SELECT t.uuid, t.title, COALESCE(t.notes, ''), t.status, COALESCE(p.title, ''), COALESCE(a.title, ''), COALESCE(h.title, '')
		FROM TMTask t
		LEFT JOIN TMTask p ON t.project = p.uuid
		LEFT JOIN TMArea a ON t.area = a.uuid OR p.area = a.uuid
		LEFT JOIN TMTask h ON t.heading = h.uuid
		WHERE t.type = 0 AND t.trashed = 0
	`
	var whereClauses []string
	var args []any

	if !req.IncludeCompleted {
		whereClauses = append(whereClauses, "t.status = 0")
	}

	switch req.Scope {
	case "inbox":
		whereClauses = append(whereClauses, "t.start = 0 AND t.project IS NULL AND t.area IS NULL")
	case "today":
		whereClauses = append(whereClauses, "(t.start = 1 AND t.todayIndex IS NOT NULL)")
	case "anytime":
		whereClauses = append(whereClauses, "t.start = 1 AND t.todayIndex IS NULL")
	case "someday":
		whereClauses = append(whereClauses, "t.start = 2")
	}

	if req.Query != "" {
		whereClauses = append(whereClauses, "(LOWER(t.title) LIKE LOWER(?) OR LOWER(t.notes) LIKE LOWER(?) OR LOWER(p.title) LIKE LOWER(?))")
		q := "%" + req.Query + "%"
		args = append(args, q, q, q)
	}

	if req.Project != "" {
		whereClauses = append(whereClauses, "LOWER(p.title) = LOWER(?)")
		args = append(args, req.Project)
	}

	if req.Area != "" {
		whereClauses = append(whereClauses, "LOWER(a.title) = LOWER(?)")
		args = append(args, req.Area)
	}

	if req.Tag != "" {
		whereClauses = append(whereClauses, `t.uuid IN (
			SELECT tt.tasks FROM TMTaskTag tt
			JOIN TMTag tg ON tt.tags = tg.uuid
			WHERE LOWER(tg.title) = LOWER(?)
		)`)
		args = append(args, req.Tag)
	}

	if len(whereClauses) > 0 {
		baseQuery += " AND " + strings.Join(whereClauses, " AND ")
	}

	baseQuery += " ORDER BY t.status ASC, t.`index` ASC, t.creationDate DESC LIMIT ?"
	args = append(args, req.Limit)

	rows, err := db.QueryContext(ctx, baseQuery, args...)
	if err != nil {
		return Response{}, fmt.Errorf("query tasks: %w", err)
	}
	defer rows.Close()

	tasks := make([]TaskItem, 0)
	for rows.Next() {
		var item TaskItem
		var statusInt int
		if err := rows.Scan(&item.ID, &item.Title, &item.Notes, &statusInt, &item.Project, &item.Area, &item.Heading); err == nil {
			switch statusInt {
			case 3:
				item.Status = "completed"
			case 2:
				item.Status = "canceled"
			default:
				item.Status = "open"
			}
			tasks = append(tasks, item)
		}
	}

	dataBytes, _ := json.Marshal(tasks)
	return Response{OK: true, ID: string(dataBytes)}, nil
}

func (c *Client) CreateProject(ctx context.Context, req capture.CreateProjectRequest) (Response, error) {
	if strings.TrimSpace(req.Title) == "" {
		return Response{}, errors.New("project title is required")
	}
	db, err := c.openDB(ctx)
	if err != nil {
		return Response{}, err
	}
	defer db.Close()

	values := url.Values{}
	values.Set("title", req.Title)
	values.Set("reveal", "false")
	if req.Area != "" {
		values.Set("area", req.Area)
	}
	if req.Notes != "" {
		values.Set("notes", req.Notes)
	}
	if req.Deadline != "" {
		values.Set("deadline", req.Deadline)
	}
	if req.When != "" {
		values.Set("when", req.When)
	}
	if len(req.Tags) > 0 {
		values.Set("tags", strings.Join(req.Tags, ","))
	}
	if c.AuthToken != "" {
		values.Set("auth-token", c.AuthToken)
	}

	addURL := "things:///add-project?" + strings.ReplaceAll(values.Encode(), "+", "%20")

	runner := c.Runner
	if runner == nil {
		runner = ExecRunner{}
	}

	wasRunning := false
	if _, _, err := runner.Run(ctx, "/usr/bin/pgrep", []string{"-x", "Things3"}); err == nil {
		wasRunning = true
	}

	if _, _, err := runner.Run(ctx, "/usr/bin/open", []string{"-g", addURL}); err != nil {
		return Response{}, fmt.Errorf("dispatch add-project URL: %w", err)
	}

	var projectUUID string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		row := db.QueryRowContext(ctx, `SELECT uuid FROM TMTask WHERE type = 1 AND LOWER(title) = LOWER(?) AND trashed = 0 ORDER BY creationDate DESC LIMIT 1`, req.Title)
		if err := row.Scan(&projectUUID); err == nil && projectUUID != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if projectUUID == "" {
		return Response{}, &OperationError{Code: "create_failed"}
	}

	if !wasRunning {
		time.Sleep(50 * time.Millisecond)
		_, _, _ = runner.Run(ctx, "/usr/bin/osascript", []string{"-e", `tell application "Things3" to quit`})
	}

	return Response{OK: true, ID: projectUUID}, nil
}

func (c *Client) UpdateTask(ctx context.Context, req capture.UpdateTaskRequest) (Response, error) {
	db, err := c.openDB(ctx)
	if err != nil {
		return Response{}, err
	}
	defer db.Close()

	taskUUID := req.ID
	if taskUUID == "" {
		query := `SELECT uuid FROM TMTask WHERE type = 0 AND LOWER(title) = LOWER(?) AND trashed = 0 AND status = 0`
		args := []any{req.Title}
		if req.Project != "" {
			projUUID, err := findProjectUUID(ctx, db, req.Project)
			if err != nil {
				return Response{}, err
			}
			query += ` AND project = ?`
			args = append(args, projUUID)
		}
		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			return Response{}, fmt.Errorf("query task: %w", err)
		}
		var found []string
		for rows.Next() {
			var uid string
			if err := rows.Scan(&uid); err == nil {
				found = append(found, uid)
			}
		}
		rows.Close()
		if len(found) == 0 {
			return Response{}, &OperationError{Code: "task_not_found"}
		}
		if len(found) > 1 {
			return Response{}, &OperationError{Code: "task_ambiguous"}
		}
		taskUUID = found[0]
	}

	// AppleScript can only rename, edit notes, and reschedule to today; every
	// other field needs the Things update URL, which requires the auth token.
	needsURLScheme := req.Deadline != "" || len(req.AddTags) > 0 || len(req.AddChecklist) > 0 ||
		(req.When != "" && req.When != "today")
	if c.AuthToken == "" && needsURLScheme {
		return Response{}, errors.New("updating deadline, tags, checklist, or non-today schedules requires the Things authorization token (set THINGS_INDEX_THINGS_AUTH_TOKEN)")
	}

	runner := c.Runner
	if runner == nil {
		runner = ExecRunner{}
	}

	wasRunning := false
	if _, _, err := runner.Run(ctx, "/usr/bin/pgrep", []string{"-x", "Things3"}); err == nil {
		wasRunning = true
	}

	if c.AuthToken != "" {
		values := url.Values{}
		values.Set("id", taskUUID)
		values.Set("auth-token", c.AuthToken)
		values.Set("reveal", "false")
		if req.NewTitle != "" {
			values.Set("title", req.NewTitle)
		}
		if req.Notes != "" {
			values.Set("notes", req.Notes)
		} else if req.AppendNotes != "" {
			values.Set("append-notes", req.AppendNotes)
		}
		if req.When != "" {
			values.Set("when", req.When)
		}
		if req.Deadline != "" {
			values.Set("deadline", req.Deadline)
		}
		if len(req.AddTags) > 0 {
			values.Set("add-tags", strings.Join(req.AddTags, ","))
		}
		if len(req.AddChecklist) > 0 {
			values.Set("append-checklist-items", strings.Join(req.AddChecklist, "\n"))
		}
		updateURL := "things:///update?" + strings.ReplaceAll(values.Encode(), "+", "%20")
		if _, _, err := runner.Run(ctx, "/usr/bin/open", []string{"-g", updateURL}); err != nil {
			return Response{}, fmt.Errorf("dispatch Things update URL: %w", err)
		}
	} else {
		var scriptLines []string
		scriptLines = append(scriptLines, fmt.Sprintf(`set aTask to (to do id "%s")`, escapeAppleScriptString(taskUUID)))
		if req.NewTitle != "" {
			scriptLines = append(scriptLines, fmt.Sprintf(`set name of aTask to "%s"`, escapeAppleScriptString(req.NewTitle)))
		}
		if req.Notes != "" {
			scriptLines = append(scriptLines, fmt.Sprintf(`set notes of aTask to "%s"`, escapeAppleScriptString(req.Notes)))
		} else if req.AppendNotes != "" {
			scriptLines = append(scriptLines, fmt.Sprintf(`set notes of aTask to (notes of aTask & "\n" & "%s")`, escapeAppleScriptString(req.AppendNotes)))
		}
		if req.When == "today" {
			scriptLines = append(scriptLines, `schedule aTask for (current date)`)
		}

		fullScript := fmt.Sprintf("tell application \"Things3\"\n  %s\nend tell", strings.Join(scriptLines, "\n  "))

		if _, _, err := runner.Run(ctx, "/usr/bin/osascript", []string{"-e", fullScript}); err != nil {
			return Response{}, fmt.Errorf("update task via AppleScript: %w", err)
		}
	}

	if !wasRunning {
		time.Sleep(50 * time.Millisecond)
		_, _, _ = runner.Run(ctx, "/usr/bin/osascript", []string{"-e", `tell application "Things3" to quit`})
	}

	return Response{OK: true, ID: taskUUID}, nil
}
