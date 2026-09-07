package helper

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
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
			creationDate REAL,
			userModificationDate REAL DEFAULT 0
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
	task := capture.Request{TaskFields: capture.TaskFields{
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
	}}

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
		return capture.Request{TaskFields: capture.TaskFields{
			Title: "Water plants",
			Schedule: &capture.Schedule{
				Start:      capture.StartOnDate,
				Date:       date,
				Evening:    true,
				ReminderAt: date + "T17:30:00Z", // 18:30 in BST
			},
		}}
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
				parsed, parseErr := url.Parse(args[len(args)-1])
				if parseErr != nil {
					return parseErr
				}
				if parsed.Path != "/add" {
					return nil // Hidden app launch during explicit finalisation.
				}
				query := parsed.Query()
				_, execErr := db.Exec(`INSERT INTO TMTask (uuid, type, title, notes, project, creationDate, trashed) VALUES ('task-1', 0, ?, ?, 'proj-1', ?, 0)`, query.Get("title"), query.Get("notes"), macEpochSeconds(time.Now()))
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

	task := capture.Request{TaskFields: capture.TaskFields{
		Title:       "Buy milk",
		Notes:       "Glass bottles",
		Destination: &capture.Destination{Kind: capture.DestinationProject, Name: "Shopping"},
		Tags:        []string{"Errand", "UnknownTag"},
	}}

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

	// Capture can finish before the worker durably records the returned UUID.
	// Its pending marker must survive that crash window, together with warnings.
	var pendingTitle string
	if err := db.QueryRow(`SELECT title FROM TMTask WHERE uuid = 'task-1'`).Scan(&pendingTitle); err != nil {
		t.Fatal(err)
	}
	if pendingTitle != "ThingsIndex pending ["+testRequestID+"]" {
		t.Fatalf("capture removed its recovery marker: %q", pendingTitle)
	}
	ids, err := client.FindCapture(context.Background(), testRequestID)
	if err != nil || len(ids) != 1 || ids[0] != resp.ID {
		t.Fatalf("cannot recover returned task: IDs=%v error=%v", ids, err)
	}
	notes, err := client.ReadCaptureNotes(context.Background(), resp.ID)
	warning := `ThingsIndex warning: tag "UnknownTag" did not exist and was not applied.`
	if err != nil || notes != "Glass bottles\n\n"+warning || len(resp.Warnings) != 1 || resp.Warnings[0] != warning {
		t.Fatalf("capture lost warning: notes=%q response=%+v error=%v", notes, resp, err)
	}
	if task.Notes != "Glass bottles" {
		t.Fatalf("capture changed caller's notes: %q", task.Notes)
	}

	if err := client.FinaliseCapture(context.Background(), resp.ID, task.Title); err != nil {
		t.Fatal(err)
	}
	var finalTitle string
	if err := db.QueryRow(`SELECT title FROM TMTask WHERE uuid = 'task-1'`).Scan(&finalTitle); err != nil {
		t.Fatal(err)
	}
	ids, err = client.FindCapture(context.Background(), testRequestID)
	if err != nil || len(ids) != 0 || finalTitle != task.Title {
		t.Fatalf("explicit finalisation failed: title=%q IDs=%v error=%v", finalTitle, ids, err)
	}
}

func TestClientPreflightDestinationErrors(t *testing.T) {
	t.Parallel()

	dbPath := setupTestThingsDB(t)
	client := &Client{
		DBPath: dbPath,
		Runner: &mockRunner{},
	}

	// Area not found
	var opErr *OperationError
	_, err := client.Capture(context.Background(), testRequestID, capture.Request{TaskFields: capture.TaskFields{
		Title:       "Task",
		Destination: &capture.Destination{Kind: capture.DestinationArea, Name: "NonExistent"},
	}})
	if !errors.As(err, &opErr) || opErr.Code != "destination_not_found" {
		t.Fatalf("expected destination_not_found for area, got %v", err)
	}
}

func TestFindCaptureRequiresExactLiveTaskMarker(t *testing.T) {
	t.Parallel()
	client, db, _ := projectCaptureFixture(t)
	marker := "ThingsIndex pending [" + testRequestID + "]"
	for _, row := range []struct {
		id, title, notes string
		kind, trashed    int
	}{
		{"pending", marker, "", 0, 0},
		{"project", marker, "", 1, 0},
		{"heading", marker, "", 2, 0},
		{"trash", marker, "", 0, 1},
		{"unrelated", "Existing task", "Reference " + testRequestID, 0, 0},
	} {
		if _, err := db.Exec(`INSERT INTO TMTask (uuid,type,title,notes,trashed) VALUES (?,?,?,?,?)`, row.id, row.kind, row.title, row.notes, row.trashed); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := client.FindCapture(context.Background(), testRequestID)
	if err != nil || len(ids) != 1 || ids[0] != "pending" {
		t.Fatalf("recovery matched unrelated records: IDs=%v error=%v", ids, err)
	}
}

func TestCaptureUnknownTagsRemainExplicit(t *testing.T) {
	t.Parallel()
	client, _, adds := projectCaptureFixture(t)
	warning := `ThingsIndex warning: tag "Missing" did not exist and was not applied.`
	response, err := client.Capture(context.Background(), testRequestID, capture.Request{TaskFields: capture.TaskFields{
		Title: "Task", Notes: "Original\n\n" + warning, Tags: []string{"Missing"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if response.AppliedTags == nil || len(response.AppliedTags) != 0 || len(response.Warnings) != 1 || response.Warnings[0] != warning {
		t.Fatalf("unknown tags were not reported explicitly: %+v", response)
	}
	if len(*adds) != 1 || (*adds)[0].Get("tags") != "" || strings.Count((*adds)[0].Get("notes"), warning) != 1 {
		t.Fatalf("unexpected warning or tag dispatch: %v", *adds)
	}
}

func TestCaptureTagReadFailurePreventsDispatch(t *testing.T) {
	t.Parallel()
	for _, setup := range []string{`DROP TABLE TMTag`, `INSERT INTO TMTag (uuid,title) VALUES ('invalid',NULL)`} {
		t.Run(setup, func(t *testing.T) {
			client, db, _ := projectCaptureFixture(t)
			if _, err := db.Exec(setup); err != nil {
				t.Fatal(err)
			}
			runner := &scriptedRunner{}
			client.Runner = runner
			_, err := client.Capture(context.Background(), testRequestID, capture.Request{TaskFields: capture.TaskFields{Title: "Task", Tags: []string{"Important"}}})
			if err == nil || !strings.Contains(err.Error(), "Things tag") || len(runner.calls) != 0 {
				t.Fatalf("tag read failure allowed mutation: error=%v calls=%v", err, runner.calls)
			}
		})
	}
}

type captureContextRunner func(context.Context, string, []string) ([]byte, []byte, error)

func (run captureContextRunner) Run(ctx context.Context, executable string, args []string) ([]byte, []byte, error) {
	return run(ctx, executable, args)
}

func TestCaptureInterruptedDispatchRemainsRecoverableAndRestoresApp(t *testing.T) {
	t.Parallel()
	for _, timeout := range []bool{false, true} {
		name := "cancelled"
		if timeout {
			name = "command deadline"
		}
		t.Run(name, func(t *testing.T) {
			client, db, _ := projectCaptureFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			running, openCalls, quits := false, 0, 0
			client.Runner = captureContextRunner(func(commandCtx context.Context, executable string, args []string) ([]byte, []byte, error) {
				switch executable {
				case "/usr/bin/pgrep":
					if !running {
						return nil, nil, errors.New("not running")
					}
				case "/usr/bin/open":
					openCalls++
					running = true
					if _, err := db.Exec(`INSERT INTO TMTask (uuid,type,title,notes,creationDate) VALUES ('created',0,?,'Saved notes',?)`, "ThingsIndex pending ["+testRequestID+"]", macEpochSeconds(time.Now())); err != nil {
						t.Fatal(err)
					}
					if timeout {
						return nil, nil, context.DeadlineExceeded
					}
					cancel()
				case "/usr/bin/osascript":
					if commandCtx.Err() != nil || !strings.Contains(args[1], "quit") {
						t.Fatalf("cleanup did not receive a usable context: %v %v", commandCtx.Err(), args)
					}
					quits++
					running = false
				default:
					t.Fatalf("unexpected command %s", executable)
				}
				return nil, nil, nil
			})
			_, err := client.Capture(ctx, testRequestID, capture.Request{TaskFields: capture.TaskFields{Title: "Task"}})
			want := context.Canceled
			if timeout {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, want) || openCalls != 1 || running || quits != 1 {
				t.Fatalf("interruption was mishandled: error=%v dispatches=%d running=%v quits=%d", err, openCalls, running, quits)
			}
			ids, err := client.FindCapture(context.Background(), testRequestID)
			if err != nil || len(ids) != 1 || ids[0] != "created" {
				t.Fatalf("dispatched task cannot be recovered: IDs=%v error=%v", ids, err)
			}
		})
	}
}

// Older fixtures used Cocoa timestamps. Retain them to prove that capture
// identity does not depend on interpreting Things' private timestamp format.
func macEpochSeconds(t time.Time) float64 {
	return t.UTC().Sub(time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)).Seconds()
}

func TestCaptureRejectsDuplicatePendingMarkers(t *testing.T) {
	t.Parallel()
	client, db, _ := projectCaptureFixture(t)
	dispatches := 0
	client.Runner = &scriptedRunner{handler: func(executable string, args []string) ([]byte, []byte, error) {
		if executable == "/usr/bin/open" {
			dispatches++
			_, err := db.Exec(`INSERT INTO TMTask (uuid,type,title,creationDate) VALUES ('one',0,?,1),('two',0,?,1788662741)`, "ThingsIndex pending ["+testRequestID+"]", "ThingsIndex pending ["+testRequestID+"]")
			return nil, nil, err
		}
		return nil, nil, nil
	}}
	response, err := client.Capture(context.Background(), testRequestID, capture.Request{TaskFields: capture.TaskFields{Title: "Task"}})
	requireUpdateError(t, err, "capture_ambiguous")
	if response.ID != "" || dispatches != 1 {
		t.Fatalf("duplicate marker selected or creation repeated: response=%+v dispatches=%d", response, dispatches)
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

	if _, err := db.Exec(`INSERT INTO TMTask (uuid, type, title) VALUES ('task-1', 0, ?)`, "ThingsIndex pending ["+testRequestID+"]"); err != nil {
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

func TestClientFinaliseCaptureFailsWhenRenameNeverLands(t *testing.T) {
	t.Parallel()

	dbPath := setupTestThingsDB(t)
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO TMTask (uuid, type, title) VALUES ('task-1', 0, ?)`, "ThingsIndex pending ["+testRequestID+"]"); err != nil {
		t.Fatal(err)
	}

	// The dispatch succeeds (open exits 0) but Things never applies the
	// rename, as happens with a revoked auth token.
	client := &Client{
		DBPath:       dbPath,
		AuthToken:    "token-123",
		Runner:       &mockRunner{},
		VerifyWindow: 300 * time.Millisecond,
	}

	err = client.FinaliseCapture(context.Background(), "task-1", "Final Title")
	var opErr *OperationError
	if !errors.As(err, &opErr) || opErr.Code != "finalise_unverified" {
		t.Fatalf("expected finalise_unverified, got %v", err)
	}
}

func TestFinaliseCapturePreservesManualChangesAndRejectsNonTasks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, storedTitle, wantCode string
		kind, trashed               int
	}{
		{name: "manual rename", storedTitle: "User revised this task", wantCode: "finalise_conflict"},
		{name: "short marker", storedTitle: "ThingsIndex pending [123]", wantCode: "finalise_conflict"},
		{name: "marker prefix", storedTitle: "Edited ThingsIndex pending [" + testRequestID + "]", wantCode: "finalise_conflict"},
		{name: "marker suffix", storedTitle: "ThingsIndex pending [" + testRequestID + "] edited", wantCode: "finalise_conflict"},
		{name: "project", storedTitle: "ThingsIndex pending [" + testRequestID + "]", kind: 1, wantCode: "finalise_not_found"},
		{name: "heading already titled", storedTitle: "Final", kind: 2, wantCode: "finalise_not_found"},
		{name: "trashed", storedTitle: "ThingsIndex pending [" + testRequestID + "]", trashed: 1, wantCode: "finalise_not_found"},
		{name: "already finalised", storedTitle: "Final"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, db, _ := projectCaptureFixture(t)
			if _, err := db.Exec(`INSERT INTO TMTask (uuid,type,title,trashed) VALUES ('target',?,?,?)`, tc.kind, tc.storedTitle, tc.trashed); err != nil {
				t.Fatal(err)
			}
			runner := &scriptedRunner{}
			client.Runner = runner
			err := client.FinaliseCapture(context.Background(), "target", "Final")
			var opErr *OperationError
			if tc.wantCode == "" && err != nil || tc.wantCode != "" && (!errors.As(err, &opErr) || opErr.Code != tc.wantCode) {
				t.Fatalf("unexpected finalisation result: %v", err)
			}
			var storedTitle string
			if err := db.QueryRow(`SELECT title FROM TMTask WHERE uuid='target'`).Scan(&storedTitle); err != nil {
				t.Fatal(err)
			}
			if len(runner.calls) != 0 || storedTitle != tc.storedTitle {
				t.Fatalf("finalisation changed unrelated/user-edited task: calls=%v title=%q", runner.calls, storedTitle)
			}
		})
	}
}

func TestFinaliseCaptureRestoresRunningStateOnSuccessAndDeadline(t *testing.T) {
	t.Parallel()
	for _, token := range []string{"", "auth-token"} {
		for _, initiallyRunning := range []bool{false, true} {
			for _, applyRename := range []bool{false, true} {
				name := "AppleScript"
				if token != "" {
					name = "URL"
				}
				if initiallyRunning {
					name += "/running"
				}
				if applyRename {
					name += "/success"
				}
				t.Run(name, func(t *testing.T) {
					client, db, _ := projectCaptureFixture(t)
					if _, err := db.Exec(`INSERT INTO TMTask (uuid,type,title) VALUES ('pending',0,?)`, "ThingsIndex pending ["+testRequestID+"]"); err != nil {
						t.Fatal(err)
					}
					running, quits := initiallyRunning, 0
					client.AuthToken = token
					client.VerifyWindow = time.Second
					client.Runner = captureContextRunner(func(commandCtx context.Context, executable string, args []string) ([]byte, []byte, error) {
						rename := false
						switch executable {
						case "/usr/bin/pgrep":
							if !running {
								return nil, nil, errors.New("not running")
							}
						case "/usr/bin/open":
							if len(args) < 3 || args[0] != "-g" || args[1] != "-j" {
								t.Fatalf("launch was not hidden: %v", args)
							}
							running = true
							rename = strings.HasPrefix(args[len(args)-1], "things:///update?")
						case "/usr/bin/osascript":
							if !running || commandCtx.Err() != nil {
								t.Fatalf("Apple Event without a running app and live context: running=%v error=%v", running, commandCtx.Err())
							}
							if strings.Contains(args[1], "quit") {
								running = false
								quits++
							}
							rename = strings.Contains(args[1], "set name of")
						default:
							t.Fatalf("unexpected command: %s", executable)
						}
						if rename && applyRename {
							_, err := db.Exec(`UPDATE TMTask SET title = 'Final' WHERE uuid = 'pending'`)
							return nil, nil, err
						}
						return nil, nil, nil
					})
					ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
					defer cancel()
					err := client.FinaliseCapture(ctx, "pending", "Final")
					if applyRename && err != nil || !applyRename && !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("unexpected finalisation result: %v", err)
					}
					wantQuits := 1
					if initiallyRunning {
						wantQuits = 0
					}
					if running != initiallyRunning || quits != wantQuits {
						t.Fatalf("finalisation changed running state: running=%v quits=%d", running, quits)
					}
				})
			}
		}
	}
}

func TestUpdateTaskVerifiesAppliedChanges(t *testing.T) {
	t.Parallel()
	client, runner := newTaskUpdateTestClient(t)
	runner.onDispatch = func(values url.Values) error {
		runner.activation = values.Get("when")
		_, err := runner.db.Exec(`UPDATE TMTask SET title=?,todayIndex=0,startBucket=0,userModificationDate=200 WHERE uuid='task-1'`, values.Get("title"))
		return err
	}

	resp, err := client.UpdateTask(context.Background(), capture.UpdateTaskRequest{
		ID:       "task-1",
		NewTitle: "New title",
		When:     "today",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.ID != "task-1" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if len(runner.dispatches) != 1 || runner.dispatches[0].Get("when") != "2026-09-05" {
		t.Fatalf("dispatched URL did not preserve prepared today date: %v", runner.dispatches)
	}
}

func TestUpdateTaskFailsHonestlyWhenDatabaseUnchanged(t *testing.T) {
	t.Parallel()

	dbPath := setupTestThingsDB(t)
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO TMTask (uuid, type, title, userModificationDate) VALUES ('task-1', 0, 'Old title', 100)`); err != nil {
		t.Fatal(err)
	}

	// open exits 0 but Things never applies the update.
	client := &Client{
		DBPath:       dbPath,
		AuthToken:    "token-123",
		Runner:       &mockRunner{},
		VerifyWindow: 300 * time.Millisecond,
	}

	_, err = client.UpdateTask(context.Background(), capture.UpdateTaskRequest{
		ID:       "task-1",
		NewTitle: "New title",
	})
	var opErr *OperationError
	if !errors.As(err, &opErr) || opErr.Code != "update_unverified" {
		t.Fatalf("expected update_unverified, got %v", err)
	}
}

func TestCreateProjectReusesExistingActiveProject(t *testing.T) {
	t.Parallel()

	dbPath := setupTestThingsDB(t)
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO TMTask (uuid, type, title, creationDate) VALUES ('proj-1', 1, 'Shopping', 1)`); err != nil {
		t.Fatal(err)
	}

	runner := &scriptedRunner{}
	client := &Client{DBPath: dbPath, Runner: runner, VerifyWindow: 300 * time.Millisecond}

	resp, err := client.CreateProject(context.Background(), capture.CreateProjectRequest{Title: "shopping"})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.ID != "proj-1" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("expected no dispatch for an existing project, ran %v", runner.calls)
	}
}

func TestCreateProjectIgnoresArchivedSameTitleProject(t *testing.T) {
	t.Parallel()

	dbPath := setupTestThingsDB(t)
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// A completed project with the same title must be matched neither by the
	// reuse pre-check nor by the post-dispatch poll.
	if _, err := db.Exec(`INSERT INTO TMTask (uuid, type, title, status, creationDate) VALUES ('proj-old', 1, 'Shopping', 3, 1)`); err != nil {
		t.Fatal(err)
	}

	runner := &mockRunner{
		onRun: func(executable string, args []string) error {
			if executable == "/usr/bin/open" {
				macEpoch := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
				nowEpoch := time.Now().UTC().Sub(macEpoch).Seconds()
				_, execErr := db.Exec(`INSERT INTO TMTask (uuid, type, title, creationDate) VALUES ('proj-new', 1, 'Shopping', ?)`, nowEpoch)
				return execErr
			}
			return nil
		},
	}
	client := &Client{DBPath: dbPath, Runner: runner, VerifyWindow: 300 * time.Millisecond}

	resp, err := client.CreateProject(context.Background(), capture.CreateProjectRequest{Title: "Shopping"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "proj-new" {
		t.Fatalf("poll matched the wrong project: %+v", resp)
	}
}

func TestCreateProjectFailsWhenNothingAppears(t *testing.T) {
	t.Parallel()

	dbPath := setupTestThingsDB(t)
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO TMTask (uuid, type, title, status, creationDate) VALUES ('proj-old', 1, 'Shopping', 3, 1)`); err != nil {
		t.Fatal(err)
	}

	// open exits 0 but no new project ever lands; the stale archived project
	// must not satisfy the poll.
	client := &Client{DBPath: dbPath, Runner: &mockRunner{}, VerifyWindow: 300 * time.Millisecond}

	_, err = client.CreateProject(context.Background(), capture.CreateProjectRequest{Title: "Shopping"})
	var opErr *OperationError
	if !errors.As(err, &opErr) || opErr.Code != "create_failed" {
		t.Fatalf("expected create_failed, got %v", err)
	}
}

// scriptedRunner returns canned stdout per executable, so tests can exercise
// the JSON exchange with the ThingsIndex Helper shortcut.
type scriptedRunner struct {
	calls   [][]string
	handler func(executable string, args []string) ([]byte, []byte, error)
}

func (s *scriptedRunner) Run(_ context.Context, executable string, args []string) ([]byte, []byte, error) {
	s.calls = append(s.calls, append([]string{executable}, args...))
	if s.handler != nil {
		return s.handler(executable, args)
	}
	return nil, nil, nil
}

func readShortcutInput(t *testing.T, args []string) map[string]any {
	t.Helper()
	if len(args) < 6 || args[0] != "run" || args[1] != HelperShortcutName || args[2] != "--input-path" || args[4] != "--output-type" || args[5] != "public.json" {
		t.Fatalf("unexpected shortcuts arguments: %v", args)
	}
	data, err := os.ReadFile(args[3])
	if err != nil {
		t.Fatal(err)
	}
	request := map[string]any{}
	if err := json.Unmarshal(data, &request); err != nil {
		t.Fatal(err)
	}
	return request
}

func headingTestClient(t *testing.T, runner CommandRunner) (*Client, *sql.DB) {
	t.Helper()
	dbPath := setupTestThingsDB(t)
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`INSERT INTO TMTask (uuid, type, title) VALUES ('proj-1', 1, 'Shopping')`); err != nil {
		t.Fatal(err)
	}
	client := &Client{DBPath: dbPath, Runner: runner, VerifyWindow: 300 * time.Millisecond}
	return client, db
}

func TestCreateHeadingRunsHelperShortcut(t *testing.T) {
	t.Parallel()

	var runner *scriptedRunner
	var db *sql.DB
	runner = &scriptedRunner{handler: func(executable string, args []string) ([]byte, []byte, error) {
		switch executable {
		case "/usr/bin/pgrep":
			return nil, nil, nil // Things is running, so no quit follows
		case "/usr/bin/shortcuts":
			request := readShortcutInput(t, args)
			if request["operation"] != "create-heading" || request["project"] != "Shopping" || request["title"] != "Groceries" {
				t.Fatalf("unexpected shortcut request: %v", request)
			}
			if _, err := db.Exec(`INSERT INTO TMTask (uuid, type, title, project, creationDate) VALUES ('head-1', 2, 'Groceries', 'proj-1', 1)`); err != nil {
				return nil, nil, err
			}
			return []byte(`{"schemaVersion":1,"ok":true,"id":"head-1"}`), nil, nil
		}
		t.Fatalf("unexpected executable %s", executable)
		return nil, nil, nil
	}}
	client, database := headingTestClient(t, runner)
	db = database

	resp, err := client.CreateHeading(context.Background(), "shopping", "Groceries")
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.ID != "head-1" {
		t.Fatalf("unexpected response: %+v", resp)
	}

	// A retry reuses the existing heading without touching Shortcuts again.
	shortcutCalls := len(runner.calls)
	resp, err = client.CreateHeading(context.Background(), "Shopping", "groceries")
	if err != nil || resp.ID != "head-1" {
		t.Fatalf("idempotent retry: resp=%+v err=%v", resp, err)
	}
	if len(runner.calls) != shortcutCalls {
		t.Fatalf("retry ran %d extra commands", len(runner.calls)-shortcutCalls)
	}
}

func TestCreateHeadingFailsHonestlyWhenDatabaseUnchanged(t *testing.T) {
	t.Parallel()

	runner := &scriptedRunner{handler: func(executable string, args []string) ([]byte, []byte, error) {
		if executable == "/usr/bin/shortcuts" {
			// The shortcut claims success but nothing lands in the database.
			return []byte(`{"schemaVersion":1,"ok":true,"id":"ghost"}`), nil, nil
		}
		return nil, nil, nil
	}}
	client, _ := headingTestClient(t, runner)

	_, err := client.CreateHeading(context.Background(), "Shopping", "Groceries")
	var opErr *OperationError
	if !errors.As(err, &opErr) || opErr.Code != "create_failed" {
		t.Fatalf("expected create_failed, got %v", err)
	}
}

func TestRenameHeadingRunsHelperShortcut(t *testing.T) {
	t.Parallel()

	var runner *scriptedRunner
	var db *sql.DB
	runner = &scriptedRunner{handler: func(executable string, args []string) ([]byte, []byte, error) {
		switch executable {
		case "/usr/bin/pgrep":
			return nil, nil, nil
		case "/usr/bin/shortcuts":
			request := readShortcutInput(t, args)
			// Canonical stored titles are sent even when the caller used
			// different letter case.
			if request["operation"] != "rename-heading" || request["project"] != "Shopping" || request["heading"] != "Alpha" || request["title"] != "Beta" {
				t.Fatalf("unexpected shortcut request: %v", request)
			}
			if _, err := db.Exec(`UPDATE TMTask SET title = 'Beta' WHERE uuid = 'head-1'`); err != nil {
				return nil, nil, err
			}
			return []byte(`{"schemaVersion":1,"ok":true,"id":"head-1"}`), nil, nil
		}
		t.Fatalf("unexpected executable %s", executable)
		return nil, nil, nil
	}}
	client, database := headingTestClient(t, runner)
	db = database
	if _, err := db.Exec(`INSERT INTO TMTask (uuid, type, title, project) VALUES ('head-1', 2, 'Alpha', 'proj-1')`); err != nil {
		t.Fatal(err)
	}

	resp, err := client.RenameHeading(context.Background(), "shopping", "alpha", "Beta")
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.ID != "head-1" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestRenameHeadingFailsHonestlyWhenDatabaseUnchanged(t *testing.T) {
	t.Parallel()

	runner := &scriptedRunner{handler: func(executable string, args []string) ([]byte, []byte, error) {
		if executable == "/usr/bin/shortcuts" {
			return []byte(`{"schemaVersion":1,"ok":true,"id":"head-1"}`), nil, nil
		}
		return nil, nil, nil
	}}
	client, db := headingTestClient(t, runner)
	if _, err := db.Exec(`INSERT INTO TMTask (uuid, type, title, project) VALUES ('head-1', 2, 'Alpha', 'proj-1')`); err != nil {
		t.Fatal(err)
	}

	_, err := client.RenameHeading(context.Background(), "Shopping", "Alpha", "Beta")
	var opErr *OperationError
	if !errors.As(err, &opErr) || opErr.Code != "rename_failed" {
		t.Fatalf("expected rename_failed, got %v", err)
	}
}

func TestArchiveHeadingRunsHelperShortcut(t *testing.T) {
	t.Parallel()

	var runner *scriptedRunner
	var db *sql.DB
	runner = &scriptedRunner{handler: func(executable string, args []string) ([]byte, []byte, error) {
		switch executable {
		case "/usr/bin/pgrep":
			return nil, nil, nil
		case "/usr/bin/shortcuts":
			request := readShortcutInput(t, args)
			if request["operation"] != "archive-heading" || request["project"] != "Shopping" || request["heading"] != "Alpha" {
				t.Fatalf("unexpected shortcut request: %v", request)
			}
			if _, err := db.Exec(`UPDATE TMTask SET status = 3 WHERE uuid = 'head-1'`); err != nil {
				return nil, nil, err
			}
			return []byte(`{"schemaVersion":1,"ok":true,"id":"head-1"}`), nil, nil
		}
		t.Fatalf("unexpected executable %s", executable)
		return nil, nil, nil
	}}
	client, database := headingTestClient(t, runner)
	db = database
	if _, err := db.Exec(`INSERT INTO TMTask (uuid, type, title, project) VALUES ('head-1', 2, 'Alpha', 'proj-1')`); err != nil {
		t.Fatal(err)
	}

	resp, err := client.ArchiveHeading(context.Background(), "Shopping", "Alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.ID != "head-1" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestHeadingOperationsSurfaceShortcutErrors(t *testing.T) {
	t.Parallel()

	runner := &scriptedRunner{handler: func(executable string, args []string) ([]byte, []byte, error) {
		if executable == "/usr/bin/shortcuts" {
			return []byte(`{"schemaVersion":1,"ok":false,"code":"heading_ambiguous"}`), nil, nil
		}
		return nil, nil, nil
	}}
	client, _ := headingTestClient(t, runner)

	_, err := client.CreateHeading(context.Background(), "Shopping", "Groceries")
	var opErr *OperationError
	if !errors.As(err, &opErr) || opErr.Code != "heading_ambiguous" {
		t.Fatalf("expected heading_ambiguous passthrough, got %v", err)
	}
}

func TestPingHelperShortcut(t *testing.T) {
	t.Parallel()

	runner := &scriptedRunner{handler: func(executable string, args []string) ([]byte, []byte, error) {
		if executable != "/usr/bin/shortcuts" {
			t.Fatalf("unexpected executable %s", executable)
		}
		request := readShortcutInput(t, args)
		if request["operation"] != "ping" || request["schemaVersion"] != float64(1) {
			t.Fatalf("unexpected ping request: %v", request)
		}
		// The real ping response carries a capabilities array the client must
		// tolerate without error.
		return []byte(`{"schemaVersion":1,"ok":true,"capabilities":["create-heading-v1"]}`), nil, nil
	}}

	client := &Client{Runner: runner}
	if err := client.PingHelperShortcut(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected exactly one shortcuts run, got %v", runner.calls)
	}
}

func TestAutomationPreflight(t *testing.T) {
	t.Parallel()

	running := false
	var runner *scriptedRunner
	runner = &scriptedRunner{handler: func(executable string, args []string) ([]byte, []byte, error) {
		switch executable {
		case "/usr/bin/pgrep":
			if running {
				return nil, nil, nil
			}
			return nil, nil, errors.New("not running")
		case "/usr/bin/open":
			running = true
			return nil, nil, nil
		case "/usr/bin/osascript":
			if strings.Contains(args[1], "quit") {
				running = false
			}
			return nil, nil, nil
		}
		t.Fatalf("unexpected executable %s", executable)
		return nil, nil, nil
	}}

	client := &Client{Runner: runner}
	if err := client.AutomationPreflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Things was not running: expect launch, the consent-raising Apple Event,
	// and the quit that restores the no-Dock-icon state.
	if len(runner.calls) != 5 || running {
		t.Fatalf("unexpected command sequence: %v", runner.calls)
	}
	if runner.calls[1][0] != "/usr/bin/open" || !strings.Contains(strings.Join(runner.calls[2], " "), "count of lists") ||
		!strings.Contains(strings.Join(runner.calls[4], " "), "quit") {
		t.Fatalf("unexpected command sequence: %v", runner.calls)
	}
}

func TestAutomationPreflightSurfacesDenial(t *testing.T) {
	t.Parallel()

	runner := &scriptedRunner{handler: func(executable string, args []string) ([]byte, []byte, error) {
		if executable == "/usr/bin/osascript" {
			return nil, nil, errors.New("execution error: Not authorized to send Apple events to Things3. (-1743)")
		}
		return nil, nil, nil
	}}

	client := &Client{Runner: runner}
	err := client.AutomationPreflight(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Automation") {
		t.Fatalf("expected consent-denial guidance, got %v", err)
	}
}

func TestAutomationPreflightAllowsAttendedConsentWithinCallerDeadline(t *testing.T) {
	t.Parallel()
	for _, callerTimeout := range []time.Duration{0, time.Second} {
		ctx := context.Background()
		if callerTimeout != 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, callerTimeout)
			defer cancel()
		}
		calls := 0
		client := &Client{Timeout: time.Millisecond, Runner: captureContextRunner(func(commandCtx context.Context, _ string, _ []string) ([]byte, []byte, error) {
			calls++
			deadline, ok := commandCtx.Deadline()
			remaining := time.Until(deadline)
			maximum := 2 * time.Minute
			if callerTimeout != 0 {
				maximum = callerTimeout
			}
			if !ok || remaining <= maximum/2 || remaining > maximum {
				t.Fatalf("unexpected consent command allowance: %v", remaining)
			}
			return nil, nil, nil // Things is already running.
		})}
		if err := client.AutomationPreflight(ctx); err != nil || calls != 2 {
			t.Fatalf("unexpected preflight result: error=%v calls=%d", err, calls)
		}
	}
}

func TestHeadingOperationsExplainMissingShortcut(t *testing.T) {
	t.Parallel()

	runner := &scriptedRunner{handler: func(executable string, args []string) ([]byte, []byte, error) {
		if executable == "/usr/bin/shortcuts" {
			return nil, []byte(`shortcut "ThingsIndex Helper" does not exist`), errors.New("exit status 1")
		}
		return nil, nil, nil
	}}
	client, _ := headingTestClient(t, runner)

	_, err := client.CreateHeading(context.Background(), "Shopping", "Groceries")
	if err == nil || !strings.Contains(err.Error(), "install-shortcut") {
		t.Fatalf("expected install remediation in error, got %v", err)
	}
}
