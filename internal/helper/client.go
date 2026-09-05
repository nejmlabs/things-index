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
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/nejmlabs/things-index/internal/capture"
)

type CommandRunner interface {
	Run(ctx context.Context, executable string, args []string) (stdout []byte, stderr []byte, err error)
}

type Client struct {
	DBPath    string
	AuthToken string
	Timeout   time.Duration
	// VerifyWindow bounds the SQLite polls that confirm a dispatched Things
	// mutation actually landed; zero means the 5-second default.
	VerifyWindow time.Duration
	Runner       CommandRunner
	Location     *time.Location
	Now          func() time.Time
}

func (c *Client) verifyDeadline() time.Time {
	window := c.VerifyWindow
	if window <= 0 {
		window = 5 * time.Second
	}
	return time.Now().Add(window)
}

type Response struct {
	OK          bool     `json:"ok"`
	ID          string   `json:"id,omitempty"`
	IDs         []string `json:"ids,omitempty"`
	AppliedTags []string `json:"appliedTags,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
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
		WHERE type = 0 AND title = ? AND trashed = 0
		ORDER BY creationDate DESC
		LIMIT 2`, pendingTitle)
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

// ReadCaptureNotes preserves the actual notes, including placement/tag warnings,
// when the worker recovers a pending task after an interrupted capture.
func (c *Client) ReadCaptureNotes(ctx context.Context, id string) (string, error) {
	if strings.TrimSpace(id) == "" {
		return "", errors.New("Things identifier is required")
	}
	db, err := c.openDB(ctx)
	if err != nil {
		return "", err
	}
	defer db.Close()
	var notes string
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(notes, '') FROM TMTask WHERE uuid = ? AND type = 0 AND trashed = 0`, id).Scan(&notes); err != nil {
		return "", fmt.Errorf("read captured Things notes: %w", err)
	}
	return notes, nil
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

	// Resolve a copy: the caller's original destination remains part of its
	// durable request hash. Dispatch the selected project by ID, not its title.
	var warnings []string
	if task.Destination != nil {
		destination := *task.Destination
		task.Destination = &destination
		switch task.Destination.Kind {
		case capture.DestinationProject:
			project, err := resolveCaptureProject(ctx, db, destination.Name, destination.ID)
			if err != nil {
				var matchErr *ProjectMatchError
				if !errors.As(err, &matchErr) {
					return Response{}, err
				}
				// Index 01 cannot answer a clarification question. Preserve the
				// task and requested placement without guessing between projects.
				warning := projectInboxWarning(destination, matchErr)
				warnings = append(warnings, warning)
				destination = capture.Destination{Kind: capture.DestinationInbox}
				break
			}
			if destination.ID == "" && !strings.EqualFold(strings.TrimSpace(destination.Name), project.Title) {
				warnings = append(warnings, fmt.Sprintf("ThingsIndex warning: matched project %q to %q.", destination.Name, project.Title))
			}
			destination.ID = project.ID
			destination.Name = project.Title

			if task.Destination.Heading != "" {
				var headingCount int
				if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM TMTask WHERE type = 2 AND LOWER(title) = LOWER(?) AND project = ? AND trashed = 0`, task.Destination.Heading, project.ID).Scan(&headingCount); err != nil {
					return Response{}, fmt.Errorf("query destination heading: %w", err)
				}
				if headingCount == 0 {
					return Response{}, &OperationError{Code: "heading_not_found"}
				}
				if headingCount > 1 {
					return Response{}, &OperationError{Code: "heading_ambiguous"}
				}
			}

		case capture.DestinationArea:
			var areaCount int
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM TMArea WHERE LOWER(title) = LOWER(?) AND (visible IS NULL OR visible != 0)`, task.Destination.Name).Scan(&areaCount); err != nil {
				return Response{}, fmt.Errorf("query destination area: %w", err)
			}
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
		if err != nil {
			return Response{}, fmt.Errorf("query available Things tags: %w", err)
		}
		existingTags := make(map[string]string)
		for tagRows.Next() {
			var tagTitle string
			if err := tagRows.Scan(&tagTitle); err != nil {
				tagRows.Close()
				return Response{}, fmt.Errorf("read available Things tag: %w", err)
			}
			existingTags[strings.ToLower(tagTitle)] = tagTitle
		}
		rowErr := tagRows.Err()
		tagRows.Close()
		if rowErr != nil {
			return Response{}, fmt.Errorf("iterate available Things tags: %w", rowErr)
		}
		appliedTags = make([]string, 0, len(task.Tags))
		for _, requested := range task.Tags {
			if canonical, found := existingTags[strings.ToLower(requested)]; found {
				appliedTags = append(appliedTags, canonical)
			} else {
				warnings = append(warnings, fmt.Sprintf("ThingsIndex warning: tag %q did not exist and was not applied.", requested))
			}
		}
	}
	for _, warning := range warnings {
		if !strings.Contains("\n\n"+task.Notes+"\n\n", "\n\n"+warning+"\n\n") {
			if task.Notes != "" {
				task.Notes += "\n\n"
			}
			task.Notes += warning
		}
	}

	runner := c.commandRunner()

	_, _, runningErr := runner.Run(ctx, "/usr/bin/pgrep", []string{"-x", "Things3"})
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	if errors.Is(runningErr, context.DeadlineExceeded) || errors.Is(runningErr, context.Canceled) {
		return Response{}, fmt.Errorf("check Things running state: %w", runningErr)
	}
	if runningErr != nil {
		defer restoreThingsStoppedState(ctx, runner)
	}

	// 3. Construct Things URL with the pending marker title. The marker is what
	// lets FindCapture reconcile an uncertain outcome after a crash or retry
	// without creating a duplicate; FinaliseCapture renames it afterwards.
	minCreationDate := macEpochSeconds(time.Now().Add(-1 * time.Second))

	pendingTitle := fmt.Sprintf("ThingsIndex pending [%s]", requestID)
	addURL := buildAddURL(pendingTitle, task, appliedTags, c.AuthToken, location, time.Now())

	// 4. Dispatch URL via open -g -j (do not bring forward, launch hidden)
	if _, _, err := runner.Run(ctx, "/usr/bin/open", []string{"-g", "-j", "-a", "/Applications/Things3.app", addURL}); err != nil {
		if err := ctx.Err(); err != nil {
			return Response{}, err
		}
		// A timed-out command may already have delivered the URL. Its pending
		// marker must be reconciled instead of dispatching a second creation.
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return Response{}, fmt.Errorf("dispatch Things add URL: %w", err)
		}
		if _, _, err := runner.Run(ctx, "/usr/bin/open", []string{"-g", "-j", addURL}); err != nil {
			return Response{}, fmt.Errorf("dispatch Things add URL: %w", err)
		}
	}

	// 5. Poll SQLite for created task UUID
	var taskUUID string
	deadline := c.verifyDeadline()
	for time.Now().Before(deadline) {
		row := db.QueryRowContext(ctx, `SELECT uuid FROM TMTask WHERE type = 0 AND title = ? AND creationDate >= ? AND trashed = 0 ORDER BY creationDate DESC LIMIT 1`, pendingTitle, minCreationDate)
		err := row.Scan(&taskUUID)
		if err == nil && taskUUID != "" {
			break
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Response{}, fmt.Errorf("verify created Things task: %w", err)
		}
		if err := waitForPoll(ctx, min(50*time.Millisecond, time.Until(deadline))); err != nil {
			return Response{}, err
		}
	}
	if taskUUID == "" {
		row := db.QueryRowContext(ctx, `SELECT uuid FROM TMTask WHERE type = 0 AND title = ? AND trashed = 0 ORDER BY creationDate DESC LIMIT 1`, pendingTitle)
		if err := row.Scan(&taskUUID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Response{}, fmt.Errorf("find created Things task: %w", err)
		}
	}
	if taskUUID == "" {
		return Response{}, &OperationError{Code: "create_failed"}
	}

	// Keep the request marker until the caller has durably saved this UUID.
	// FinaliseCapture is a separate step, matching the bundled Shortcut's
	// capture/find/finalise contract. Renaming here loses crash recovery.

	return Response{
		OK:          true,
		ID:          taskUUID,
		AppliedTags: appliedTags,
		Warnings:    warnings,
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
	err = db.QueryRowContext(ctx, `SELECT title FROM TMTask WHERE uuid = ? AND type = 0 AND trashed = 0`, id).Scan(&currentTitle)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &OperationError{Code: "finalise_not_found"}
		}
		return fmt.Errorf("read task for finalisation: %w", err)
	}
	if currentTitle == title {
		return nil
	}
	const pendingPrefix = "ThingsIndex pending ["
	requestID := strings.TrimSuffix(strings.TrimPrefix(currentTitle, pendingPrefix), "]")
	if !validRequestID(requestID) || currentTitle != pendingPrefix+requestID+"]" {
		// A user may have renamed the task while the worker was stopped.
		// A retry must not overwrite that change with the original request.
		return &OperationError{Code: "finalise_conflict"}
	}

	runner := c.commandRunner()
	_, _, runningErr := runner.Run(ctx, "/usr/bin/pgrep", []string{"-x", "Things3"})
	if err := ctx.Err(); err != nil {
		return err
	}
	if errors.Is(runningErr, context.DeadlineExceeded) || errors.Is(runningErr, context.Canceled) {
		return fmt.Errorf("check Things running state: %w", runningErr)
	}
	if runningErr != nil {
		defer restoreThingsStoppedState(ctx, runner)
		if c.AuthToken == "" {
			if _, _, err := runner.Run(ctx, "/usr/bin/open", []string{"-g", "-j", "-a", "/Applications/Things3.app"}); err != nil {
				return fmt.Errorf("launch Things hidden for finalisation: %w", err)
			}
		}
	}

	if c.AuthToken != "" {
		values := url.Values{
			"id":         {id},
			"title":      {title},
			"auth-token": {c.AuthToken},
			"reveal":     {"false"},
		}
		updateURL := "things:///update?" + strings.ReplaceAll(values.Encode(), "+", "%20")
		if _, _, err := runner.Run(ctx, "/usr/bin/open", []string{"-g", "-j", updateURL}); err != nil {
			return fmt.Errorf("run Things update URL: %w", err)
		}
	} else {
		script := fmt.Sprintf(`tell application "Things3" to set name of (first to do whose id is "%s") to "%s"`, escapeAppleScriptString(id), escapeAppleScriptString(title))
		if _, _, err := runner.Run(ctx, "/usr/bin/osascript", []string{"-e", script}); err != nil {
			return fmt.Errorf("run Things AppleScript finalise: %w", err)
		}
	}

	// open exits 0 even when Things rejects the update (for example on a
	// revoked auth token), so an unverified rename must fail loudly; the
	// caller's retry reconciles through the pending marker title.
	deadline := c.verifyDeadline()
	for time.Now().Before(deadline) {
		var updatedTitle string
		err := db.QueryRowContext(ctx, `SELECT title FROM TMTask WHERE uuid = ? AND trashed = 0`, id).Scan(&updatedTitle)
		if err == nil && updatedTitle == title {
			return nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("verify Things finalisation: %w", err)
		}
		if err := waitForPoll(ctx, min(50*time.Millisecond, time.Until(deadline))); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return &OperationError{Code: "finalise_unverified"}
}

// Cleanup must still run after a deadline, but must not launch Things merely
// to quit it when a failed dispatch never started the app.
func restoreThingsStoppedState(ctx context.Context, runner CommandRunner) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if _, _, err := runner.Run(cleanupCtx, "/usr/bin/pgrep", []string{"-x", "Things3"}); err == nil {
		_, _, _ = runner.Run(cleanupCtx, "/usr/bin/osascript", []string{"-e", `tell application "Things3" to quit`})
	}
}

func buildAddURL(pendingTitle string, task capture.Request, appliedTags []string, authToken string, location *time.Location, now time.Time) string {
	values := url.Values{}
	values.Set("title", pendingTitle)
	if task.Notes != "" {
		values.Set("notes", task.Notes)
	}
	values.Set("reveal", "false")
	if task.Destination != nil && (task.Destination.Name != "" || task.Destination.ID != "") {
		if task.Destination.ID != "" {
			values.Set("list-id", task.Destination.ID)
		} else {
			values.Set("list", task.Destination.Name)
		}
		if task.Destination.Heading != "" {
			values.Set("heading", task.Destination.Heading)
		}
	}
	if task.Schedule != nil {
		var when string
		if task.Schedule.Evening && task.Schedule.Date == now.In(location).Format("2006-01-02") {
			// Things can only place a task in This Evening for today; for any
			// other start date fall through to on_date handling so the date
			// (and reminder) are preserved instead of silently rescheduling
			// the task to today.
			when = "evening"
		} else if task.Schedule.Start == capture.StartOnDate {
			when = task.Schedule.Date
		} else if task.Schedule.Start == capture.StartSomeday {
			when = "someday"
		} else if task.Schedule.Start == capture.StartAnytime {
			when = "anytime"
		}
		if when != "" {
			if task.Schedule.Start == capture.StartOnDate && task.Schedule.ReminderAt != "" {
				if reminderTime, err := time.Parse(time.RFC3339, task.Schedule.ReminderAt); err == nil {
					when += "@" + reminderTime.In(location).Format("15:04")
				}
			}
			values.Set("when", when)
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

// macEpochSeconds converts t to seconds since Things' Cocoa reference date
// (2001-01-01 UTC), the encoding TMTask.creationDate uses.
func macEpochSeconds(t time.Time) float64 {
	return t.UTC().Sub(time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)).Seconds()
}

// HelperShortcutName is the required library name of the bundled Shortcut;
// the worker intentionally does not guess among renamed copies.
const HelperShortcutName = "ThingsIndex Helper"

type shortcutResponse struct {
	SchemaVersion int    `json:"schemaVersion"`
	OK            bool   `json:"ok"`
	ID            string `json:"id"`
	Code          string `json:"code"`
}

// runHelperShortcut performs one JSON exchange with the ThingsIndex Helper
// shortcut through Apple's shortcuts CLI. Heading mutations have no URL
// scheme or AppleScript equivalent; the Shortcut's native Things App Intents
// are the only automation surface that reaches them.
func runHelperShortcut(ctx context.Context, runner CommandRunner, request map[string]any) (shortcutResponse, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return shortcutResponse{}, fmt.Errorf("encode helper shortcut request: %w", err)
	}
	tempFile, err := os.CreateTemp("", "things-index-shortcut-*.json")
	if err != nil {
		return shortcutResponse{}, fmt.Errorf("create helper shortcut input file: %w", err)
	}
	tempPath := tempFile.Name()
	defer os.Remove(tempPath)
	if _, err := tempFile.Write(payload); err != nil {
		tempFile.Close()
		return shortcutResponse{}, fmt.Errorf("write helper shortcut input file: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return shortcutResponse{}, fmt.Errorf("close helper shortcut input file: %w", err)
	}

	stdout, stderr, err := runner.Run(ctx, "/usr/bin/shortcuts", []string{
		"run", HelperShortcutName, "--input-path", tempPath, "--output-type", "public.json",
	})
	if err != nil {
		detail := strings.TrimSpace(string(stderr))
		if detail == "" {
			detail = err.Error()
		}
		return shortcutResponse{}, fmt.Errorf("run the %q shortcut (install it with things-index install-shortcut, or open shortcuts/ThingsIndex Helper.shortcut): %s", HelperShortcutName, detail)
	}
	var response shortcutResponse
	if err := json.Unmarshal(bytes.TrimSpace(stdout), &response); err != nil {
		return shortcutResponse{}, fmt.Errorf("decode %q shortcut response %q: %w", HelperShortcutName, strings.TrimSpace(string(stdout)), err)
	}
	if response.SchemaVersion != 1 {
		return shortcutResponse{}, fmt.Errorf("unsupported %q shortcut response version %d", HelperShortcutName, response.SchemaVersion)
	}
	if !response.OK {
		code := response.Code
		if code == "" {
			code = "shortcut_failed"
		}
		return shortcutResponse{}, &OperationError{Code: code}
	}
	return response, nil
}

// PingHelperShortcut runs the bundled Shortcut's harmless ping operation (it
// looks up one impossible Things ID) so setup can settle the Shortcut's
// one-time privacy dialogs before the background worker needs it. Shortcut
// privacy grants persist per shortcut, not per invoking process, so a grant
// earned here covers later daemon runs.
func (c *Client) PingHelperShortcut(ctx context.Context) error {
	runner := c.commandRunner()
	_, err := runHelperShortcut(ctx, runner, map[string]any{
		"schemaVersion": 1,
		"operation":     "ping",
	})
	return err
}

// AutomationPreflight sends one benign Apple Event to Things 3 (a read-only
// list count) so macOS raises the Automation consent dialog during a
// deliberate setup moment instead of during the first background archive.
// TCC attributes the grant to the calling process, so the worker daemon must
// run this itself; a wizard-run preflight would only grant the terminal.
func (c *Client) AutomationPreflight(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	preflightClient := *c
	preflightClient.Timeout = 2 * time.Minute
	runner := preflightClient.commandRunner()
	if _, _, err := runner.Run(ctx, "/usr/bin/pgrep", []string{"-x", "Things3"}); err != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		defer restoreThingsStoppedState(ctx, runner)
		// Launch hidden first so the Apple Event below does not surface a
		// Things window.
		if _, _, err := runner.Run(ctx, "/usr/bin/open", []string{"-g", "-j", "-a", "/Applications/Things3.app"}); err != nil {
			return fmt.Errorf("launch Things hidden for Automation preflight: %w", err)
		}
	}
	// Counting lists is answered by the running app, unlike properties such
	// as name or version that AppleScript resolves from the bundle without
	// ever sending an Apple Event (which would not raise the consent dialog).
	if _, _, err := runner.Run(ctx, "/usr/bin/osascript", []string{"-e", `tell application "Things3" to count of lists`}); err != nil {
		return fmt.Errorf("Things 3 automation consent is missing or was denied (System Settings > Privacy & Security > Automation): %w", err)
	}
	return nil
}

// canonicalTitle returns the exact stored title for a TMTask row so the
// Shortcut's exact-match queries agree with our case-insensitive lookups.
func canonicalTitle(ctx context.Context, db *sql.DB, uuid, fallback string) string {
	var title string
	if err := db.QueryRowContext(ctx, `SELECT title FROM TMTask WHERE uuid = ?`, uuid).Scan(&title); err == nil && title != "" {
		return title
	}
	return fallback
}

func (c *Client) CreateHeading(ctx context.Context, project, headingTitle string) (Response, error) {
	if strings.TrimSpace(project) == "" {
		return Response{}, errors.New("project name is required")
	}
	if strings.TrimSpace(headingTitle) == "" {
		return Response{}, errors.New("heading title is required")
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

	runner := c.commandRunner()

	wasRunning := false
	if _, _, err := runner.Run(ctx, "/usr/bin/pgrep", []string{"-x", "Things3"}); err == nil {
		wasRunning = true
	}

	// 3. Create the heading through the helper Shortcut's native Create
	// Heading intent. The URL scheme cannot reach existing projects (a
	// project's items array is create-specific), so the Shortcut is the only
	// working path.
	if _, err := runHelperShortcut(ctx, runner, map[string]any{
		"schemaVersion": 1,
		"operation":     "create-heading",
		"project":       canonicalTitle(ctx, db, projectUUID, project),
		"title":         headingTitle,
	}); err != nil {
		return Response{}, err
	}

	// 4. Poll for created heading UUID; the Shortcut's claim is only trusted
	// once the row is visible in the Things database.
	var headingUUID string
	deadline := c.verifyDeadline()
	for time.Now().Before(deadline) {
		row := db.QueryRowContext(ctx, `SELECT uuid FROM TMTask WHERE type = 2 AND LOWER(title) = LOWER(?) AND project = ? AND trashed = 0 ORDER BY creationDate DESC LIMIT 1`, headingTitle, projectUUID)
		if err := row.Scan(&headingUUID); err == nil && headingUUID != "" {
			break
		}
		if err := waitForPoll(ctx, 100*time.Millisecond); err != nil {
			return Response{}, err
		}
	}
	if headingUUID == "" {
		return Response{}, &OperationError{Code: "create_failed"}
	}

	if !wasRunning {
		if err := waitForPoll(ctx, 100*time.Millisecond); err != nil {
			return Response{}, err
		}
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

	runner := c.commandRunner()

	wasRunning := false
	if _, _, err := runner.Run(ctx, "/usr/bin/pgrep", []string{"-x", "Things3"}); err == nil {
		wasRunning = true
	}

	// Complete the heading through the helper Shortcut's Edit Items status
	// detail; the update URL command is specified for to-dos only and Things
	// silently ignores it on heading rows.
	if _, err := runHelperShortcut(ctx, runner, map[string]any{
		"schemaVersion": 1,
		"operation":     "archive-heading",
		"project":       canonicalTitle(ctx, db, projectUUID, project),
		"heading":       canonicalTitle(ctx, db, headingUUID, headingTitle),
	}); err != nil {
		return Response{}, err
	}

	// Verify the heading actually left the active project; the Shortcut's
	// claim is only trusted once the change is visible in SQLite.
	archived := false
	deadline := c.verifyDeadline()
	for time.Now().Before(deadline) {
		var status, trashed int
		err := db.QueryRowContext(ctx, `SELECT status, trashed FROM TMTask WHERE uuid = ?`, headingUUID).Scan(&status, &trashed)
		if err == nil && (status != 0 || trashed != 0) {
			archived = true
			break
		}
		if err := waitForPoll(ctx, 100*time.Millisecond); err != nil {
			return Response{}, err
		}
	}

	if !wasRunning {
		if err := waitForPoll(ctx, 100*time.Millisecond); err != nil {
			return Response{}, err
		}
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

	runner := c.commandRunner()

	wasRunning := false
	if _, _, err := runner.Run(ctx, "/usr/bin/pgrep", []string{"-x", "Things3"}); err == nil {
		wasRunning = true
	}

	// Rename the heading through the helper Shortcut's Edit Items title
	// detail; the update URL command is specified for to-dos only and Things
	// silently ignores it on heading rows.
	if _, err := runHelperShortcut(ctx, runner, map[string]any{
		"schemaVersion": 1,
		"operation":     "rename-heading",
		"project":       canonicalTitle(ctx, db, projectUUID, project),
		"heading":       canonicalTitle(ctx, db, headingUUID, oldHeadingTitle),
		"title":         newHeadingTitle,
	}); err != nil {
		return Response{}, err
	}

	// Verify the rename took effect; the Shortcut's claim is only trusted
	// once the change is visible in SQLite.
	renamed := false
	deadline := c.verifyDeadline()
	for time.Now().Before(deadline) {
		var currentTitle string
		err := db.QueryRowContext(ctx, `SELECT title FROM TMTask WHERE uuid = ? AND trashed = 0`, headingUUID).Scan(&currentTitle)
		if err == nil && strings.EqualFold(currentTitle, newHeadingTitle) {
			renamed = true
			break
		}
		if err := waitForPoll(ctx, 100*time.Millisecond); err != nil {
			return Response{}, err
		}
	}

	if !wasRunning {
		if err := waitForPoll(ctx, 100*time.Millisecond); err != nil {
			return Response{}, err
		}
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
