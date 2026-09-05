package helper

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nejmlabs/things-index/internal/capture"
)

func projectTestDatabase(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := setupTestThingsDB(t)
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, statement := range []string{
		`ALTER TABLE TMTask ADD COLUMN start INTEGER DEFAULT 1`,
		`ALTER TABLE TMTask ADD COLUMN todayIndex INTEGER`,
		`ALTER TABLE TMTask ADD COLUMN startDate REAL`,
		`ALTER TABLE TMTask ADD COLUMN startBucket INTEGER DEFAULT 0`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	return path, db
}

func insertProjectFixture(t *testing.T, db *sql.DB, statement string, args ...any) {
	t.Helper()
	if _, err := db.Exec(statement, args...); err != nil {
		t.Fatal(err)
	}
}

func TestCreateProjectResolvesAreaBeforeDispatch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		areas     string
		requested string
		wantCode  string
	}{
		{name: "missing", requested: "Personal", wantCode: "destination_not_found"},
		{name: "whole title required", areas: `('area-1', 'Personal Life', 1)`, requested: "Personal", wantCode: "destination_not_found"},
		{name: "hidden", areas: `('area-1', 'Personal', 0)`, requested: "Personal", wantCode: "destination_not_found"},
		{name: "duplicates", areas: `('area-1', 'Personal', 1), ('area-2', 'PERSONAL', 1)`, requested: "Personal", wantCode: "destination_ambiguous"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, db := projectTestDatabase(t)
			if tc.areas != "" {
				insertProjectFixture(t, db, `INSERT INTO TMArea (uuid, title, visible) VALUES `+tc.areas)
			}
			runner := &scriptedRunner{}
			client := &Client{DBPath: path, Runner: runner}
			_, err := client.CreateProject(context.Background(), capture.CreateProjectRequest{Title: "Trip", Area: tc.requested})
			var operationError *OperationError
			if !errors.As(err, &operationError) || operationError.Code != tc.wantCode {
				t.Fatalf("expected %s, got %v", tc.wantCode, err)
			}
			if len(runner.calls) != 0 {
				t.Fatalf("invalid area dispatched commands: %v", runner.calls)
			}
		})
	}
}

func TestCreateProjectCreatesInRequestedAreaDespiteSameTitleElsewhere(t *testing.T) {
	t.Parallel()
	path, db := projectTestDatabase(t)
	insertProjectFixture(t, db, `INSERT INTO TMArea (uuid, title, visible) VALUES ('personal', 'Personal', 1), ('hidden-personal', 'Personal', 0), ('work', 'Work', 1)`)
	insertProjectFixture(t, db, `INSERT INTO TMTask (uuid, type, title, area, creationDate) VALUES ('work-trip', 1, 'Trip', 'work', 1)`)
	insertProjectFixture(t, db, `INSERT INTO TMTask (uuid, type, title, area, status, creationDate) VALUES ('old-personal-trip', 1, 'Trip', 'personal', 3, 1)`)
	insertProjectFixture(t, db, `INSERT INTO TMTag VALUES ('holiday','Holiday')`)
	dispatches := 0
	runner := &scriptedRunner{handler: func(executable string, args []string) ([]byte, []byte, error) {
		if executable == "/usr/bin/osascript" && strings.Contains(args[1], "on isoDate") {
			return []byte("2026-12-01|2026-09-05"), nil, nil
		}
		if executable != "/usr/bin/open" {
			return nil, nil, nil
		}
		target, err := url.Parse(args[len(args)-1])
		if err != nil {
			t.Fatal(err)
		}
		query := target.Query()
		for key, want := range map[string]string{
			"area-id": "personal", "area": "", "title": "Trip", "notes": "Holiday plans",
			"when": "2026-09-05", "deadline": "2026-12-01", "tags": "Holiday", "reveal": "false",
		} {
			if got := query.Get(key); got != want {
				t.Errorf("URL %s = %q, want %q", key, got, want)
			}
		}
		insertProjectFixture(t, db, `INSERT INTO TMTask (uuid, type, title, area, notes, creationDate) VALUES ('personal-trip', 1, ?, ?, ?, ?)`, query.Get("title"), query.Get("area-id"), query.Get("notes"), macEpochSeconds(time.Now()))
		insertProjectFixture(t, db, `INSERT INTO TMTaskTag VALUES ('personal-trip','holiday')`)
		dispatches++
		return nil, nil, nil
	}}
	client := &Client{DBPath: path, Runner: runner, Location: time.UTC, Now: func() time.Time { return time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC) }}
	req := capture.CreateProjectRequest{Title: "Trip", Area: "personal", Notes: "Holiday plans", When: "today", Deadline: "2026-12-01", Tags: []string{"Holiday"}}
	got, err := client.CreateProject(context.Background(), req)
	if err != nil || !got.OK || got.ID != "personal-trip" {
		t.Fatalf("create returned %+v, %v", got, err)
	}
	var area, notes string
	if err := db.QueryRow(`SELECT area, notes FROM TMTask WHERE uuid = ?`, got.ID).Scan(&area, &notes); err != nil {
		t.Fatal(err)
	}
	if area != "personal" || notes != req.Notes {
		t.Fatalf("returned project has area=%q notes=%q", area, notes)
	}
	retried, err := client.CreateProject(context.Background(), req)
	if err != nil || retried.ID != got.ID || dispatches != 1 || len(retried.Warnings) != 0 {
		t.Fatalf("retry did not reuse the intended project: %+v, %v, calls=%v", retried, err, runner.calls)
	}
}

func TestCreateProjectWithoutAreaCreatesUnfiledProject(t *testing.T) {
	t.Parallel()
	path, db := projectTestDatabase(t)
	insertProjectFixture(t, db, `INSERT INTO TMTask (uuid, type, title, area, creationDate) VALUES ('work-trip', 1, 'Trip', 'work', 1)`)
	runner := &scriptedRunner{handler: func(executable string, args []string) ([]byte, []byte, error) {
		if executable == "/usr/bin/open" {
			target, err := url.Parse(args[len(args)-1])
			if err != nil {
				t.Fatal(err)
			}
			if target.Query().Has("area-id") || target.Query().Has("area") {
				t.Fatalf("unfiled project has an area parameter: %s", target)
			}
			insertProjectFixture(t, db, `INSERT INTO TMTask (uuid, type, title, area, creationDate) VALUES ('unfiled-trip', 1, 'Trip', NULL, ?)`, macEpochSeconds(time.Now()))
		}
		return nil, nil, nil
	}}
	client := &Client{DBPath: path, Runner: runner}
	got, err := client.CreateProject(context.Background(), capture.CreateProjectRequest{Title: "Trip"})
	if err != nil || got.ID != "unfiled-trip" {
		t.Fatalf("create returned %+v, %v", got, err)
	}
	callsBeforeRetry := len(runner.calls)
	got, err = client.CreateProject(context.Background(), capture.CreateProjectRequest{Title: "Trip"})
	if err != nil || got.ID != "unfiled-trip" || len(runner.calls) != callsBeforeRetry {
		t.Fatalf("unfiled retry returned %+v, %v, calls=%v", got, err, runner.calls)
	}
}

func TestCreateProjectRejectsDuplicateProjectsInRequestedArea(t *testing.T) {
	t.Parallel()
	for _, area := range []string{"", "Personal"} {
		t.Run("area="+area, func(t *testing.T) {
			path, db := projectTestDatabase(t)
			if area != "" {
				insertProjectFixture(t, db, `INSERT INTO TMArea (uuid, title) VALUES (?, ?)`, area, area)
			}
			insertProjectFixture(t, db, `INSERT INTO TMTask (uuid, type, title, area, creationDate) VALUES ('trip-1', 1, 'Trip', ?, 1), ('trip-2', 1, 'TRIP', ?, 2)`, area, area)
			runner := &scriptedRunner{}
			client := &Client{DBPath: path, Runner: runner}
			_, err := client.CreateProject(context.Background(), capture.CreateProjectRequest{Title: "Trip", Area: area})
			var operationError *OperationError
			if !errors.As(err, &operationError) || operationError.Code != "destination_ambiguous" {
				t.Fatalf("expected destination_ambiguous, got %v", err)
			}
			if len(runner.calls) != 0 {
				t.Fatalf("ambiguous projects dispatched commands: %v", runner.calls)
			}
		})
	}
}

func TestCreateProjectRejectsWrongDestinationAndRestoresRunningState(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		createdArea   string
		createdStatus int
	}{
		{name: "wrong area", createdArea: "work"},
		{name: "unexpected unfiled project"},
		{name: "archived project", createdArea: "personal", createdStatus: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, db := projectTestDatabase(t)
			insertProjectFixture(t, db, `INSERT INTO TMArea (uuid, title) VALUES ('personal', 'Personal')`)
			quit := false
			runner := &scriptedRunner{handler: func(executable string, args []string) ([]byte, []byte, error) {
				switch executable {
				case "/usr/bin/pgrep":
					return nil, nil, errors.New("Things was closed")
				case "/usr/bin/open":
					insertProjectFixture(t, db, `INSERT INTO TMTask (uuid, type, title, area, status, creationDate) VALUES ('wrong-trip', 1, 'Trip', ?, ?, ?)`, tc.createdArea, tc.createdStatus, macEpochSeconds(time.Now()))
				case "/usr/bin/osascript":
					quit = strings.Contains(strings.Join(args, " "), "to quit")
				}
				return nil, nil, nil
			}}
			client := &Client{DBPath: path, Runner: runner, VerifyWindow: 10 * time.Millisecond}
			_, err := client.CreateProject(context.Background(), capture.CreateProjectRequest{Title: "Trip", Area: "Personal"})
			var operationError *OperationError
			if !errors.As(err, &operationError) || operationError.Code != "create_failed" {
				t.Fatalf("expected create_failed for incorrect placement/status, got %v", err)
			}
			if !quit {
				t.Fatalf("verification failure left Things running: %v", runner.calls)
			}
		})
	}
}

func TestCreateProjectVerifiesEveryRequestedField(t *testing.T) {
	t.Parallel()
	for _, missing := range []string{"none", "notes", "tags", "deadline", "schedule"} {
		t.Run(missing, func(t *testing.T) {
			client, runner := newTaskUpdateTestClient(t)
			insertProjectFixture(t, runner.db, `INSERT INTO TMTag VALUES ('tag','Holiday')`)
			req := capture.CreateProjectRequest{Title: "Trip", Notes: "Itinerary", Tags: []string{"Holiday"}, Deadline: "2026-09-10", When: "2026-09-09"}
			runner.onDispatch = func(q url.Values) error {
				notes := q.Get("notes")
				if missing == "notes" {
					notes = "Wrong notes"
				}
				insertProjectFixture(t, runner.db, `INSERT INTO TMTask (uuid,type,title,notes,start,creationDate) VALUES ('project',1,?,?,1,?)`, q.Get("title"), notes, macEpochSeconds(time.Now()))
				if missing != "tags" {
					insertProjectFixture(t, runner.db, `INSERT INTO TMTaskTag VALUES ('project','tag')`)
				}
				if missing != "deadline" {
					runner.due = q.Get("deadline")
				}
				if missing != "schedule" {
					runner.activation = q.Get("when")
				}
				return nil
			}
			response, err := client.CreateProject(context.Background(), req)
			if missing == "none" {
				if err != nil || !response.OK || response.ID != "project" {
					t.Fatalf("correct project = %+v, %v", response, err)
				}
			} else {
				requireUpdateError(t, err, "create_failed")
			}
			if len(runner.dispatches) != 1 {
				t.Fatalf("verification repeated creation: %d", len(runner.dispatches))
			}
		})
	}
}

func TestCreateProjectReuseReportsUnappliedFields(t *testing.T) {
	t.Parallel()
	client, runner := newTaskUpdateTestClient(t)
	insertProjectFixture(t, runner.db, `INSERT INTO TMTag VALUES ('tag','Holiday')`)
	insertProjectFixture(t, runner.db, `INSERT INTO TMTask (uuid,type,title,notes) VALUES ('existing',1,'Trip','Existing notes')`)
	response, err := client.CreateProject(context.Background(), capture.CreateProjectRequest{Title: "Trip", Notes: "Requested notes", Tags: []string{"Holiday"}, Deadline: "2026-09-10", When: "2026-09-09"})
	if err != nil || !response.OK || response.ID != "existing" || len(response.Warnings) != 1 {
		t.Fatalf("reuse = %+v, %v", response, err)
	}
	for _, field := range []string{"notes", "tags", "deadline", "schedule", "were not applied"} {
		if !strings.Contains(response.Warnings[0], field) {
			t.Fatalf("unapplied %s was not disclosed: %v", field, response.Warnings)
		}
	}
	if len(runner.dispatches) != 0 {
		t.Fatal("reuse mutated the existing project")
	}
}

func TestCreateProjectPreflightRejectsUnknownTagsAndSchema(t *testing.T) {
	t.Parallel()
	for _, problem := range []string{"missing tag", "ambiguous tag", "missing schedule column", "invalid date", "invalid title"} {
		t.Run(problem, func(t *testing.T) {
			client, runner := newTaskUpdateTestClient(t)
			req := capture.CreateProjectRequest{Title: "Trip", Tags: []string{"Holiday"}}
			switch problem {
			case "ambiguous tag":
				insertProjectFixture(t, runner.db, `INSERT INTO TMTag VALUES ('a','Holiday'),('b','HOLIDAY')`)
			case "missing schedule column":
				req.Tags, req.When = nil, "today"
				insertProjectFixture(t, runner.db, `ALTER TABLE TMTask DROP COLUMN startBucket`)
			case "invalid date":
				req.Tags, req.Deadline = nil, "2026-02-30"
			case "invalid title":
				req.Tags, req.Title = nil, "Bad\nTitle"
			}
			_, err := client.CreateProject(context.Background(), req)
			if err == nil || len(runner.dispatches) != 0 {
				t.Fatalf("invalid request/schema reached dispatch: error=%v writes=%d", err, len(runner.dispatches))
			}
		})
	}
}
