package helper

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nejmlabs/things-index/internal/capture"
)

// This runner only reads/writes each test's temporary SQLite database. No
// installed Things application, automation command, or user database is used.
type taskUpdateTestRunner struct {
	t          *testing.T
	db         *sql.DB
	due        string
	activation string
	dateOutput *string
	dispatches []url.Values
	onDispatch func(url.Values) error
	scripts    []string
	onScript   func(string) error
	readError  error
}

func (r *taskUpdateTestRunner) Run(_ context.Context, executable string, args []string) ([]byte, []byte, error) {
	if executable == "/usr/bin/pgrep" {
		return nil, nil, nil
	}
	if executable == "/usr/bin/osascript" && len(args) == 2 && strings.Contains(args[1], "on isoDate(d)") {
		if r.readError != nil {
			return nil, nil, r.readError
		}
		if r.dateOutput != nil {
			return []byte(*r.dateOutput), nil, nil
		}
		return []byte(r.due + "|" + r.activation + "\n"), nil, nil
	}
	if executable == "/usr/bin/osascript" && len(args) == 2 && r.onScript != nil {
		r.scripts = append(r.scripts, args[1])
		return nil, nil, r.onScript(args[1])
	}
	if executable != "/usr/bin/open" || len(args) != 3 || args[0] != "-g" || args[1] != "-j" {
		r.t.Fatalf("unexpected native command: %s %v", executable, args)
	}
	u, err := url.Parse(args[2])
	if err != nil {
		r.t.Fatal(err)
	}
	q := u.Query()
	r.dispatches = append(r.dispatches, q)
	if r.onDispatch != nil {
		return nil, nil, r.onDispatch(q)
	}
	return nil, nil, nil
}

func newTaskUpdateTestClient(t *testing.T) (*Client, *taskUpdateTestRunner) {
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
		`ALTER TABLE TMTask ADD COLUMN recurrenceRule BLOB`,
		`CREATE TABLE TMChecklistItem (uuid TEXT PRIMARY KEY,title TEXT,status INTEGER,"index" INTEGER,task TEXT)`,
		`INSERT INTO TMTask (uuid,type,title,notes) VALUES ('task-1',0,'Original','Existing notes')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	runner := &taskUpdateTestRunner{t: t, db: db}
	return &Client{DBPath: path, AuthToken: "test-token", Runner: runner, VerifyWindow: time.Millisecond,
		Now: func() time.Time { return time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC) }, Location: time.UTC}, runner
}

func updateTestExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func requireUpdateError(t *testing.T, err error, code string) {
	t.Helper()
	var operation *OperationError
	if !errors.As(err, &operation) || operation.Code != code {
		t.Fatalf("error = %v; want %s", err, code)
	}
}

func prepareTestUpdate(t *testing.T, client *Client, req capture.UpdateTaskRequest) TaskUpdatePlan {
	t.Helper()
	req.ID = "task-1"
	plan, err := client.PrepareTaskUpdate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestPreparedTaskUpdateAppendNotesSurvivesSerializationAndLostAcknowledgement(t *testing.T) {
	t.Parallel()
	c, runner := newTaskUpdateTestClient(t)
	updateTestExec(t, runner.db, `UPDATE TMTask SET notes='Already mentioned: hello' WHERE uuid='task-1'`)
	plan := prepareTestUpdate(t, c, capture.UpdateTaskRequest{AppendNotes: "hello"})
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), c.AuthToken) || !plan.ReplaySafe {
		t.Fatal("replacement plan must be replay-safe and contain no authorization token")
	}
	var restored TaskUpdatePlan
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	runner.onDispatch = func(q url.Values) error {
		if q.Has("append-notes") || q.Get("notes") != "Already mentioned: hello\nhello" {
			t.Fatalf("wrong replacement: %v", q)
		}
		_, err := runner.db.Exec(`UPDATE TMTask SET notes=? WHERE uuid=?`, q.Get("notes"), q.Get("id"))
		return err
	}
	if _, err := c.ApplyTaskUpdate(context.Background(), restored); err != nil {
		t.Fatal(err)
	}
	if done, err := c.CheckTaskUpdate(context.Background(), restored); err != nil || !done {
		t.Fatalf("reconciliation = %v, %v", done, err)
	}
	if _, err := c.ApplyTaskUpdate(context.Background(), restored); err != nil {
		t.Fatal(err)
	}
	if len(runner.dispatches) != 1 {
		t.Fatalf("lost acknowledgement repeated append: %d dispatches", len(runner.dispatches))
	}
}

func TestPreparedTaskUpdateConflictAndPartialReplacement(t *testing.T) {
	t.Parallel()
	t.Run("conflicting field blocks every mutation", func(t *testing.T) {
		c, runner := newTaskUpdateTestClient(t)
		plan := prepareTestUpdate(t, c, capture.UpdateTaskRequest{NewTitle: "New", AppendNotes: "extra"})
		updateTestExec(t, runner.db, `UPDATE TMTask SET notes='User edited' WHERE uuid='task-1'`)
		_, err := c.ApplyTaskUpdate(context.Background(), plan)
		requireUpdateError(t, err, "update_conflict")
		if len(runner.dispatches) != 0 {
			t.Fatal("conflict dispatched a write")
		}
	})
	t.Run("already applied field is not rewritten", func(t *testing.T) {
		c, runner := newTaskUpdateTestClient(t)
		plan := prepareTestUpdate(t, c, capture.UpdateTaskRequest{NewTitle: "New", AppendNotes: "extra"})
		updateTestExec(t, runner.db, `UPDATE TMTask SET title='New',area='user-area' WHERE uuid='task-1'`)
		runner.onDispatch = func(q url.Values) error {
			if q.Has("title") || q.Has("list-id") {
				t.Fatalf("rewrote a satisfied or unrelated field: %v", q)
			}
			_, err := runner.db.Exec(`UPDATE TMTask SET notes=? WHERE uuid='task-1'`, q.Get("notes"))
			return err
		}
		if _, err := c.ApplyTaskUpdate(context.Background(), plan); err != nil {
			t.Fatal(err)
		}
	})
}

func TestPreparedTaskUpdateRejectsUnverifiedChanges(t *testing.T) {
	t.Parallel()
	c, runner := newTaskUpdateTestClient(t)
	plan := prepareTestUpdate(t, c, capture.UpdateTaskRequest{AppendNotes: "Existing"})
	runner.onDispatch = func(q url.Values) error {
		// Neither a modification timestamp nor the presence of the appended
		// substring in old notes proves that the exact replacement was applied.
		_, err := runner.db.Exec(`UPDATE TMTask SET userModificationDate=1234 WHERE uuid='task-1'`)
		return err
	}
	_, err := c.ApplyTaskUpdate(context.Background(), plan)
	requireUpdateError(t, err, "update_unverified")
	if done, err := c.CheckTaskUpdate(context.Background(), plan); done || err != nil {
		t.Fatalf("unchanged baseline should be pending: %v, %v", done, err)
	}
}

func TestPreparedTaskUpdateTagIdentitiesAndReplacement(t *testing.T) {
	t.Parallel()
	c, runner := newTaskUpdateTestClient(t)
	updateTestExec(t, runner.db, `INSERT INTO TMTag VALUES ('a','Home'),('b','Work')`)
	updateTestExec(t, runner.db, `INSERT INTO TMTaskTag VALUES ('task-1','a')`)
	plan := prepareTestUpdate(t, c, capture.UpdateTaskRequest{AddTags: []string{"work", "Home"}})
	if !reflect.DeepEqual(plan.Tags.After, []TaskUpdateTag{{ID: "a", Title: "Home"}, {ID: "b", Title: "Work"}}) {
		t.Fatalf("wrong canonical union: %+v", plan.Tags)
	}
	runner.onDispatch = func(q url.Values) error {
		if q.Get("tags") != "Home,Work" || q.Has("add-tags") {
			t.Fatalf("tags were not replaced: %v", q)
		}
		_, err := runner.db.Exec(`INSERT INTO TMTaskTag VALUES ('task-1','b')`)
		return err
	}
	if _, err := c.ApplyTaskUpdate(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ApplyTaskUpdate(context.Background(), plan); err != nil || len(runner.dispatches) != 1 {
		t.Fatalf("tag replay: %v, %d dispatches", err, len(runner.dispatches))
	}
}

func TestPreparedTaskUpdateTagsFailBeforeMutation(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"missing", "ambiguous", "renamed", "comma"} {
		t.Run(mode, func(t *testing.T) {
			c, runner := newTaskUpdateTestClient(t)
			name := "Work"
			if mode != "missing" {
				updateTestExec(t, runner.db, `INSERT INTO TMTag VALUES ('a','Work')`)
			}
			if mode == "ambiguous" {
				updateTestExec(t, runner.db, `INSERT INTO TMTag VALUES ('b','WORK')`)
			}
			if mode == "comma" {
				name = "Work, home"
				updateTestExec(t, runner.db, `UPDATE TMTag SET title=?`, name)
			}
			plan, err := c.PrepareTaskUpdate(context.Background(), capture.UpdateTaskRequest{ID: "task-1", AddTags: []string{name}})
			if mode == "renamed" {
				if err != nil {
					t.Fatal(err)
				}
				updateTestExec(t, runner.db, `UPDATE TMTag SET title='Renamed'`)
				_, err = c.ApplyTaskUpdate(context.Background(), plan)
				requireUpdateError(t, err, "update_conflict")
			} else if mode == "comma" {
				requireUpdateError(t, err, "invalid_request")
			} else {
				requireUpdateError(t, err, "tag_unavailable")
			}
			if len(runner.dispatches) != 0 {
				t.Fatal("invalid tag dispatched a mutation")
			}
		})
	}
}

func TestPreparedTaskUpdateChecklistPreservesExistingRows(t *testing.T) {
	t.Parallel()
	c, runner := newTaskUpdateTestClient(t)
	updateTestExec(t, runner.db, `INSERT INTO TMChecklistItem VALUES ('old','Done item',3,0,'task-1')`)
	plan := prepareTestUpdate(t, c, capture.UpdateTaskRequest{AddChecklist: []string{"New item", "New item"}})
	if plan.ReplaySafe {
		t.Fatal("append dispatch must not be repeated after an uncertain outcome")
	}
	runner.onDispatch = func(q url.Values) error {
		if q.Get("append-checklist-items") != "New item\nNew item" || q.Has("checklist-items") {
			t.Fatalf("wrong checklist operation: %v", q)
		}
		_, err := runner.db.Exec(`INSERT INTO TMChecklistItem VALUES ('new1','New item',0,1,'task-1'),('new2','New item',0,2,'task-1')`)
		return err
	}
	if _, err := c.ApplyTaskUpdate(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if done, err := c.CheckTaskUpdate(context.Background(), plan); !done || err != nil {
		t.Fatalf("exact append not reconciled: %v, %v", done, err)
	}
	updateTestExec(t, runner.db, `UPDATE TMChecklistItem SET uuid='recreated' WHERE uuid='old'`)
	_, err := c.CheckTaskUpdate(context.Background(), plan)
	requireUpdateError(t, err, "update_conflict")
	if len(runner.dispatches) != 1 {
		t.Fatal("read-only reconciliation dispatched a command")
	}
}

func TestPreparedTaskUpdateChecklistPartialAppendConflicts(t *testing.T) {
	t.Parallel()
	c, runner := newTaskUpdateTestClient(t)
	plan := prepareTestUpdate(t, c, capture.UpdateTaskRequest{AddChecklist: []string{"One", "Two"}})
	updateTestExec(t, runner.db, `INSERT INTO TMChecklistItem VALUES ('new1','One',0,0,'task-1')`)
	_, err := c.CheckTaskUpdate(context.Background(), plan)
	requireUpdateError(t, err, "update_conflict")
	_, err = c.ApplyTaskUpdate(context.Background(), plan)
	requireUpdateError(t, err, "update_conflict")
	if len(runner.dispatches) != 0 {
		t.Fatal("partial append was repeated")
	}
}

func TestPreparedTaskUpdateCalendarUsesPublicDatesAndFreezesToday(t *testing.T) {
	t.Parallel()
	c, runner := newTaskUpdateTestClient(t)
	runner.due = "2026-09-08"
	updateTestExec(t, runner.db, `UPDATE TMTask SET startDate=999999,startBucket=987 WHERE uuid='task-1'`)
	plan := prepareTestUpdate(t, c, capture.UpdateTaskRequest{When: "today", Deadline: "2026-09-10"})
	if plan.Schedule.Date != "2026-09-05" || plan.Deadline.Before != "2026-09-08" || plan.Schedule.Before.StartDate == nil || *plan.Schedule.Before.StartDate != 999999 {
		t.Fatalf("calendar snapshot = %+v", plan)
	}
	c.Now = func() time.Time { return time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC) }
	runner.onDispatch = func(q url.Values) error {
		if q.Get("when") != "2026-09-05" || q.Get("deadline") != "2026-09-10" {
			t.Fatalf("relative dates drifted on retry: %v", q)
		}
		runner.activation, runner.due = q.Get("when"), q.Get("deadline")
		_, err := runner.db.Exec(`UPDATE TMTask SET startBucket=0 WHERE uuid='task-1'`)
		return err
	}
	if _, err := c.ApplyTaskUpdate(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if done, err := c.CheckTaskUpdate(context.Background(), plan); !done || err != nil {
		t.Fatalf("calendar reconciliation = %v, %v", done, err)
	}
}

func TestPreparedTaskUpdateCalendarRefusesFalseProof(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"unchanged", "malformed", "recurring", "missing schema", "conflict", "evening"} {
		t.Run(mode, func(t *testing.T) {
			c, runner := newTaskUpdateTestClient(t)
			req := capture.UpdateTaskRequest{ID: "task-1", When: "2026-09-09", Deadline: "2026-09-10"}
			switch mode {
			case "malformed":
				value := "9/10/2026|not a date"
				runner.dateOutput = &value
			case "recurring":
				updateTestExec(t, runner.db, `UPDATE TMTask SET recurrenceRule=X'01'`)
			case "missing schema":
				updateTestExec(t, runner.db, `ALTER TABLE TMTask DROP COLUMN recurrenceRule`)
			case "evening":
				req.When, req.Deadline = "evening", ""
			}
			plan, err := c.PrepareTaskUpdate(context.Background(), req)
			if mode == "malformed" || mode == "recurring" || mode == "missing schema" {
				if err == nil || len(runner.dispatches) != 0 {
					t.Fatalf("unsupported calendar read did not fail before mutation: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "conflict" {
				runner.due = "2026-12-01"
				_, err := c.ApplyTaskUpdate(context.Background(), plan)
				requireUpdateError(t, err, "update_conflict")
				if len(runner.dispatches) != 0 {
					t.Fatal("calendar conflict dispatched")
				}
				return
			}
			if mode == "evening" && plan.ReplaySafe {
				t.Fatal("unverifiable evening plan must not be replayed")
			}
			runner.onDispatch = func(q url.Values) error {
				// An arbitrary bucket change or modification timestamp is not a
				// substitute for exact calendar/list verification.
				_, err := runner.db.Exec(`UPDATE TMTask SET startBucket=456,userModificationDate=678`)
				return err
			}
			_, err = c.ApplyTaskUpdate(context.Background(), plan)
			requireUpdateError(t, err, "update_unverified")
		})
	}
}

func TestPreparedTaskUpdateValidationAndTargetResolution(t *testing.T) {
	t.Parallel()
	c, runner := newTaskUpdateTestClient(t)
	updateTestExec(t, runner.db, `INSERT INTO TMTask (uuid,type,title,notes) VALUES ('task-2',0,'Original','')`)
	_, err := c.PrepareTaskUpdate(context.Background(), capture.UpdateTaskRequest{Title: "Original", NewTitle: "New"})
	requireUpdateError(t, err, "task_ambiguous")
	plan := prepareTestUpdate(t, c, capture.UpdateTaskRequest{NewTitle: "New"})
	plan.Version = 99
	_, err = c.ApplyTaskUpdate(context.Background(), plan)
	requireUpdateError(t, err, "invalid_request")
	checklist := prepareTestUpdate(t, c, capture.UpdateTaskRequest{AddChecklist: []string{"One"}})
	checklist.ReplaySafe = true
	_, err = c.CheckTaskUpdate(context.Background(), checklist)
	requireUpdateError(t, err, "invalid_request")
	if len(runner.dispatches) != 0 {
		t.Fatal("invalid plan dispatched")
	}
}

func TestPreparedTaskUpdateAppleScriptFallbackReplacesAndVerifies(t *testing.T) {
	t.Parallel()
	c, runner := newTaskUpdateTestClient(t)
	c.AuthToken = ""
	plan := prepareTestUpdate(t, c, capture.UpdateTaskRequest{NewTitle: `A "quoted" task`, AppendNotes: "A new line\nwith another", When: "today"})
	runner.onScript = func(script string) error {
		for _, expected := range []string{`set aTask to (to do id "task-1")`, `set name of aTask to "A \"quoted\" task"`, `set notes of aTask to "Existing notes\nA new line\nwith another"`, "set year of targetDate to 2026", "set month of targetDate to 9", "set day of targetDate to 5", "schedule aTask for targetDate"} {
			if !strings.Contains(script, expected) {
				t.Fatalf("AppleScript lacks %q: %s", expected, script)
			}
		}
		runner.activation = "2026-09-05"
		_, err := runner.db.Exec(`UPDATE TMTask SET title=?,notes=?,todayIndex=0,startBucket=0 WHERE uuid='task-1'`, plan.Title.After, plan.Notes.After)
		return err
	}
	if _, err := c.ApplyTaskUpdate(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ApplyTaskUpdate(context.Background(), plan); err != nil || len(runner.scripts) != 1 {
		t.Fatalf("AppleScript replay = %v, %d writes", err, len(runner.scripts))
	}
	if len(runner.dispatches) != 0 {
		t.Fatal("unauthorized URL dispatch")
	}
}

func TestPreparedTaskUpdateChecklistWaitsForCompleteAsyncReadback(t *testing.T) {
	t.Parallel()
	c, runner := newTaskUpdateTestClient(t)
	c.VerifyWindow = time.Second
	plan := prepareTestUpdate(t, c, capture.UpdateTaskRequest{AddChecklist: []string{"One", "Two"}})
	finished := make(chan error, 1)
	runner.onDispatch = func(q url.Values) error {
		if _, err := runner.db.Exec(`INSERT INTO TMChecklistItem VALUES ('new1','One',0,0,'task-1')`); err != nil {
			return err
		}
		go func() {
			time.Sleep(10 * time.Millisecond)
			_, err := runner.db.Exec(`INSERT INTO TMChecklistItem VALUES ('new2','Two',0,1,'task-1')`)
			finished <- err
		}()
		return nil
	}
	_, err := c.ApplyTaskUpdate(context.Background(), plan)
	if insertErr := <-finished; insertErr != nil {
		t.Fatal(insertErr)
	}
	if err != nil || len(runner.dispatches) != 1 {
		t.Fatalf("async complete append = %v, %d writes", err, len(runner.dispatches))
	}
}

func TestPreparedTaskUpdateEveningRequiresKnownBucketAndIntendedDay(t *testing.T) {
	t.Parallel()
	c, runner := newTaskUpdateTestClient(t)
	updateTestExec(t, runner.db, `UPDATE TMTask SET startBucket=0`)
	plan := prepareTestUpdate(t, c, capture.UpdateTaskRequest{When: "evening"})
	for _, bucket := range []int64{0, 2, 987} {
		state := TaskUpdateScheduleState{Start: 1, Today: true, StartBucket: &bucket, ActivationDate: plan.Schedule.Date}
		if c.scheduleUpdateMatches(plan.Schedule, state) {
			t.Fatalf("bucket %d falsely proved evening", bucket)
		}
	}
	if plan.ReplaySafe {
		t.Fatal("relative evening dispatch must remain single-attempt")
	}
	runner.onDispatch = func(q url.Values) error {
		if q.Get("when") != "evening" {
			t.Fatalf("evening capability was removed: %v", q)
		}
		runner.activation = plan.Schedule.Date
		_, err := runner.db.Exec(`UPDATE TMTask SET todayIndex=0,startBucket=1 WHERE uuid='task-1'`)
		return err
	}
	if _, err := c.ApplyTaskUpdate(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if done, err := c.CheckTaskUpdate(context.Background(), plan); !done || err != nil {
		t.Fatalf("evening outcome not reconciled: %v, %v", done, err)
	}
	if len(runner.dispatches) != 1 {
		t.Fatal("evening reconciliation dispatched")
	}
}

func TestPreparedTaskUpdateTodayDoesNotAcceptEveningOrStaleTodayIndex(t *testing.T) {
	t.Parallel()
	c, _ := newTaskUpdateTestClient(t)
	for _, when := range []string{"today", "evening"} {
		change := &TaskUpdateScheduleChange{When: when, Date: "2026-09-05"}
		bucket := int64(0)
		if when == "evening" {
			bucket = 1
		}
		state := TaskUpdateScheduleState{Start: 1, Today: true, StartBucket: &bucket, ActivationDate: "2026-10-01"}
		if c.scheduleUpdateMatches(change, state) {
			t.Fatalf("%s accepted a wrong nonempty activation date with stale todayIndex", when)
		}
		state.ActivationDate = ""
		if !c.scheduleUpdateMatches(change, state) {
			t.Fatalf("%s did not accept verified current-day membership", when)
		}
		otherBucket := int64(1) - bucket
		state.StartBucket = &otherBucket
		state.ActivationDate = change.Date
		if c.scheduleUpdateMatches(change, state) {
			t.Fatalf("%s accepted the other Today/Evening bucket", when)
		}
		state.StartBucket = nil
		if c.scheduleUpdateMatches(change, state) {
			t.Fatalf("%s accepted an unknown bucket", when)
		}
	}
}
