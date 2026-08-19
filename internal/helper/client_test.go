package helper

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/nejmlabs/things-index/internal/capture"
)

const testRequestID = "00000000000000000000000000000001"

type mockRunner struct {
	lastExecutable string
	lastArgs       []string
	onRun          func(executable string, args []string) error
}

func (m *mockRunner) Run(_ context.Context, executable string, args []string) ([]byte, []byte, error) {
	m.lastExecutable = executable
	m.lastArgs = args
	if m.onRun != nil {
		if err := m.onRun(executable, args); err != nil {
			return nil, nil, err
		}
	}
	return nil, nil, nil
}

func setupTestThingsDB(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "main.sqlite")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	statements := []string{
		`CREATE TABLE TMTask (
			uuid TEXT PRIMARY KEY,
			type INTEGER,
			title TEXT,
			notes TEXT,
			status INTEGER DEFAULT 0,
			trashed INTEGER DEFAULT 0,
			project TEXT DEFAULT '',
			area TEXT DEFAULT '',
			heading TEXT DEFAULT '',
			creationDate REAL
		)`,
		`CREATE TABLE TMArea (
			uuid TEXT PRIMARY KEY,
			title TEXT,
			visible INTEGER DEFAULT 1
		)`,
		`CREATE TABLE TMTag (
			uuid TEXT PRIMARY KEY,
			title TEXT
		)`,
		`CREATE TABLE TMTaskTag (
			tasks TEXT,
			tags TEXT
		)`,
	}
	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	return dbPath
}

func TestBuildAddURL(t *testing.T) {
	t.Parallel()

	location := time.FixedZone("BST", 3600)
	task := capture.Request{
		Title:       "Buy milk",
		Notes:       "Glass bottles",
		Destination: &capture.Destination{Kind: capture.DestinationProject, Name: "Shopping", Heading: "Groceries"},
		Schedule: &capture.Schedule{
			Start:      capture.StartOnDate,
			Date:       "2026-08-18",
			ReminderAt: "2026-08-18T17:30:00Z", // 18:30 in BST
		},
		Deadline:  "2026-08-19",
		Tags:      []string{"Errand"},
		Checklist: []string{"Item 1", "Item 2"},
	}

	rawURL := buildAddURL("ThingsIndex pending [123]", task, []string{"Errand"}, "secret-token", location, time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC))
	if !strings.HasPrefix(rawURL, "things:///add?") {
		t.Fatalf("unexpected URL prefix: %s", rawURL)
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()

	if query.Get("title") != "ThingsIndex pending [123]" {
		t.Errorf("title = %q", query.Get("title"))
	}
	if query.Get("notes") != "Glass bottles" {
		t.Errorf("notes = %q", query.Get("notes"))
	}
	if query.Get("list") != "Shopping" {
		t.Errorf("list = %q", query.Get("list"))
	}
	if query.Get("heading") != "Groceries" {
		t.Errorf("heading = %q", query.Get("heading"))
	}
	if query.Get("when") != "2026-08-18@18:30" {
		t.Errorf("when = %q, want 2026-08-18@18:30", query.Get("when"))
	}
	if query.Get("deadline") != "2026-08-19" {
		t.Errorf("deadline = %q", query.Get("deadline"))
	}
	if query.Get("tags") != "Errand" {
		t.Errorf("tags = %q", query.Get("tags"))
	}
	if query.Get("checklist-items") != "Item 1\nItem 2" {
		t.Errorf("checklist-items = %q", query.Get("checklist-items"))
	}
	if query.Get("auth-token") != "secret-token" {
		t.Errorf("auth-token = %q", query.Get("auth-token"))
	}
	if query.Get("reveal") != "false" {
		t.Errorf("reveal = %q", query.Get("reveal"))
	}
}

func TestBuildAddURLEvening(t *testing.T) {
	t.Parallel()

	location := time.FixedZone("BST", 3600)
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC) // 2026-08-18 in BST
	newTask := func(date string) capture.Request {
		return capture.Request{
			Title: "Water plants",
			Schedule: &capture.Schedule{
				Start:      capture.StartOnDate,
				Date:       date,
				Evening:    true,
				ReminderAt: date + "T17:30:00Z", // 18:30 in BST
			},
		}
	}

	todayURL, err := url.Parse(buildAddURL("t", newTask("2026-08-18"), nil, "", location, now))
	if err != nil {
		t.Fatal(err)
	}
	if got := todayURL.Query().Get("when"); got != "evening@18:30" {
		t.Errorf("when for today = %q, want evening@18:30", got)
	}

	futureURL, err := url.Parse(buildAddURL("t", newTask("2026-08-25"), nil, "", location, now))
	if err != nil {
		t.Fatal(err)
	}
	if got := futureURL.Query().Get("when"); got != "2026-08-25@18:30" {
		t.Errorf("when for future date = %q, want 2026-08-25@18:30", got)
	}
}

func TestClientCaptureAndFind(t *testing.T) {
	t.Parallel()

	dbPath := setupTestThingsDB(t)
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Seed Project and Tag
	if _, err := db.Exec(`INSERT INTO TMTask (uuid, type, title, trashed) VALUES ('proj-1', 1, 'Shopping', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO TMTag (uuid, title) VALUES ('tag-1', 'Errand')`); err != nil {
		t.Fatal(err)
	}

	runner := &mockRunner{
		onRun: func(executable string, args []string) error {
			switch executable {
			case "/usr/bin/pgrep":
				return errors.New("not running")
			case "/usr/bin/osascript":
				// Simulate Things renaming the pending marker when finalised.
				if len(args) == 2 && strings.Contains(args[1], "set name of") {
					_, execErr := db.Exec(`UPDATE TMTask SET title = 'Buy milk' WHERE uuid = 'task-1'`)
					return execErr
				}
				return nil
			case "/usr/bin/open":
				// Simulate Things creating the task with the pending marker
				// title and no request reference in the notes, matching what
				// the real add URL produces.
				macEpoch := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
				nowEpoch := time.Now().UTC().Sub(macEpoch).Seconds()
				_, execErr := db.Exec(`INSERT INTO TMTask (uuid, type, title, notes, project, creationDate, trashed) VALUES ('task-1', 0, 'ThingsIndex pending [`+testRequestID+`]', '', 'proj-1', ?, 0)`, nowEpoch)
				return execErr
			default:
				t.Fatalf("unexpected executable: %s", executable)
				return nil
			}
		},
	}

	client := &Client{
		DBPath:  dbPath,
		Runner:  runner,
		Timeout: 5 * time.Second,
	}

	task := capture.Request{
		Title:       "Buy milk",
		Destination: &capture.Destination{Kind: capture.DestinationProject, Name: "Shopping"},
		Tags:        []string{"Errand", "UnknownTag"},
	}

	resp, err := client.Capture(context.Background(), testRequestID, task)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.ID != "task-1" {
		t.Fatalf("unexpected capture response: %#v", resp)
	}
	if len(resp.AppliedTags) != 1 || resp.AppliedTags[0] != "Errand" {
		t.Fatalf("unexpected applied tags: %#v", resp.AppliedTags)
	}

	// The pending marker must have been renamed to the final title inline.
	var finalTitle string
	if err := db.QueryRow(`SELECT title FROM TMTask WHERE uuid = 'task-1'`).Scan(&finalTitle); err != nil {
		t.Fatal(err)
	}
	if finalTitle != "Buy milk" {
		t.Fatalf("title after capture = %q, want %q", finalTitle, "Buy milk")
	}

	// FindCapture reconciles in-flight captures by their pending marker title.
	const otherRequestID = "00000000000000000000000000000002"
	if _, err := db.Exec(`INSERT INTO TMTask (uuid, type, title, notes, trashed) VALUES ('task-2', 0, 'ThingsIndex pending [` + otherRequestID + `]', '', 0)`); err != nil {
		t.Fatal(err)
	}
	ids, err := client.FindCapture(context.Background(), otherRequestID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "task-2" {
		t.Fatalf("unexpected found IDs: %#v", ids)
	}
}

func TestClientPreflightDestinationErrors(t *testing.T) {
	t.Parallel()

	dbPath := setupTestThingsDB(t)
	client := &Client{
		DBPath: dbPath,
		Runner: &mockRunner{},
	}

	// Project not found
	_, err := client.Capture(context.Background(), testRequestID, capture.Request{
		Title:       "Task",
		Destination: &capture.Destination{Kind: capture.DestinationProject, Name: "NonExistent"},
	})
	var opErr *OperationError
	if !errors.As(err, &opErr) || opErr.Code != "destination_not_found" {
		t.Fatalf("expected destination_not_found, got %v", err)
	}

	// Area not found
	_, err = client.Capture(context.Background(), testRequestID, capture.Request{
		Title:       "Task",
		Destination: &capture.Destination{Kind: capture.DestinationArea, Name: "NonExistent"},
	})
	if !errors.As(err, &opErr) || opErr.Code != "destination_not_found" {
		t.Fatalf("expected destination_not_found for area, got %v", err)
	}
}

func TestClientFinaliseCapture(t *testing.T) {
	t.Parallel()

	dbPath := setupTestThingsDB(t)
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO TMTask (uuid, type, title) VALUES ('task-1', 0, 'ThingsIndex pending [123]')`); err != nil {
		t.Fatal(err)
	}

	runner := &mockRunner{
		onRun: func(executable string, args []string) error {
			// Simulate title update
			_, execErr := db.Exec(`UPDATE TMTask SET title = 'Final Title' WHERE uuid = 'task-1'`)
			return execErr
		},
	}

	client := &Client{
		DBPath:    dbPath,
		AuthToken: "token-123",
		Runner:    runner,
	}

	if err := client.FinaliseCapture(context.Background(), "task-1", "Final Title"); err != nil {
		t.Fatal(err)
	}

	// Verify not found error for nonexistent ID
	err = client.FinaliseCapture(context.Background(), "nonexistent", "Final Title")
	var opErr *OperationError
	if !errors.As(err, &opErr) || opErr.Code != "finalise_not_found" {
		t.Fatalf("expected finalise_not_found, got %v", err)
	}
}
