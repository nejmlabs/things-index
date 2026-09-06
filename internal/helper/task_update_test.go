package helper

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	t                *testing.T
	db               *sql.DB
	due              string
	activation       string
	dateOutput       *string
	dispatches       []url.Values
	onDispatch       func(url.Values) error
	scripts          []string
	onScript         func(string) error
	readError        error
	todayMember      bool
	membershipReads  int
	membershipError  error
	membershipOutput *string
}

func (r *taskUpdateTestRunner) Run(_ context.Context, executable string, args []string) ([]byte, []byte, error) {
	if executable == "/usr/bin/pgrep" {
		return nil, nil, nil
	}
	if executable == "/usr/bin/osascript" && len(args) == 4 && args[1] == taskUpdateTodayMembershipScript {
		r.membershipReads++
		if args[2] != "--" || args[3] == "" {
			r.t.Fatalf("invalid membership argument: %v", args)
		}
		if r.membershipError != nil {
			return nil, nil, r.membershipError
		}
		if r.membershipOutput != nil {
			return []byte(*r.membershipOutput), nil, nil
		}
		return []byte(fmt.Sprintf("%t\n", r.todayMember)), nil, nil
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
		if c.scheduleUpdateMatches(plan.Schedule, state, "") {
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
		if c.scheduleUpdateMatches(change, state, "") {
			t.Fatalf("%s accepted a wrong nonempty activation date with stale todayIndex", when)
		}
		state.ActivationDate = ""
		if c.scheduleUpdateMatches(change, state, "") {
			t.Fatalf("%s accepted stale todayIndex without public membership", when)
		}
		member := false
		state.TodayMembership = &member
		if c.scheduleUpdateMatches(change, state, "") {
			t.Fatalf("%s accepted an item absent from public Today", when)
		}
		member = true
		if !c.scheduleUpdateMatches(change, state, "") {
			t.Fatalf("%s did not accept verified current-day membership", when)
		}
		otherBucket := int64(1) - bucket
		state.StartBucket = &otherBucket
		state.ActivationDate = change.Date
		if c.scheduleUpdateMatches(change, state, "") {
			t.Fatalf("%s accepted the other Today/Evening bucket", when)
		}
		state.StartBucket = nil
		if c.scheduleUpdateMatches(change, state, "") {
			t.Fatalf("%s accepted an unknown bucket", when)
		}
	}
}

func TestFutureTaskUpdateReconcilesObservedUpcomingState(t *testing.T) {
	t.Parallel()
	client, runner := newTaskUpdateTestClient(t)
	plan := prepareTestUpdate(t, client, capture.UpdateTaskRequest{When: "2026-09-13", Deadline: "2026-09-20"})
	runner.onDispatch = func(q url.Values) error {
		if q.Get("when") != "2026-09-13" || q.Get("deadline") != "2026-09-20" {
			t.Fatalf("requested calendar dates changed: %v", q)
		}
		// Mirror the actual native result, including the misleading non-null
		// zero todayIndex. Never decode the packed startDate to verify it.
		updateTestExec(t, runner.db, `UPDATE TMTask SET start=2,todayIndex=0,startDate=132814464,startBucket=0 WHERE uuid='task-1'`)
		runner.activation, runner.due = "2026-09-13", "2026-09-20"
		return context.DeadlineExceeded // Native write landed; its reply was lost.
	}
	if _, err := client.ApplyTaskUpdate(context.Background(), plan); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected an interrupted native result, got %v", err)
	}
	done, err := client.CheckTaskUpdate(context.Background(), plan)
	if err != nil || !done || len(runner.dispatches) != 1 {
		t.Fatalf("correct future schedule not reconciled: done=%t, err=%v, writes=%d", done, err, len(runner.dispatches))
	}
	response, err := client.ApplyTaskUpdate(context.Background(), plan)
	if err != nil || !response.OK || response.ID != "task-1" || len(runner.dispatches) != 1 {
		t.Fatalf("already applied future schedule repeated: response=%+v, err=%v, writes=%d", response, err, len(runner.dispatches))
	}
	// The same raw database fields cannot prove a different or absent public
	// date. These must still fail reconciliation rather than claim success.
	for _, activation := range []string{"2026-09-14", ""} {
		runner.activation = activation
		done, err := client.CheckTaskUpdate(context.Background(), plan)
		if done {
			t.Fatalf("accepted incorrect public activation date %q", activation)
		}
		requireUpdateError(t, err, "update_conflict")
		if len(runner.dispatches) != 1 {
			t.Fatal("read-only reconciliation dispatched another update")
		}
	}
}

func TestAnytimeTaskUpdateClearsDateWithoutRequiringEmptyTodayIndex(t *testing.T) {
	t.Parallel()
	for _, todayIndex := range []int{0, 17, -17} {
		t.Run(fmt.Sprintf("todayIndex=%d", todayIndex), func(t *testing.T) {
			client, runner := newTaskUpdateTestClient(t)
			runner.activation, runner.due = "2026-09-13", "2026-09-05"
			runner.todayMember = true // Deadline independently retains Today membership.
			updateTestExec(t, runner.db, `UPDATE TMTask SET start=2,todayIndex=?,startDate=132814464 WHERE uuid='task-1'`, todayIndex)
			plan := prepareTestUpdate(t, client, capture.UpdateTaskRequest{When: "anytime"})
			runner.onDispatch = func(q url.Values) error {
				if !q.Has("when") || q.Get("when") != "" || q.Has("deadline") {
					t.Fatalf("clear-start request changed: %v", q)
				}
				runner.activation = ""
				// Keep the ordering index and today's deadline: neither should
				// prevent verification that Things cleared the start date.
				updateTestExec(t, runner.db, `UPDATE TMTask SET start=1,startDate=NULL WHERE uuid='task-1'`)
				return nil
			}
			response, err := client.ApplyTaskUpdate(context.Background(), plan)
			if err != nil || !response.OK || response.ID != "task-1" {
				t.Fatalf("Anytime update was not verified: response=%+v, err=%v", response, err)
			}
			done, err := client.CheckTaskUpdate(context.Background(), plan)
			if err != nil || !done || len(runner.dispatches) != 1 {
				t.Fatalf("Anytime reconciliation failed: done=%t, err=%v, writes=%d", done, err, len(runner.dispatches))
			}
			if runner.due != "2026-09-05" {
				t.Fatal("Anytime update changed the deadline")
			}
			// A nonempty date or a different start mode must still fail.
			runner.activation = "2026-09-14"
			if done, _ := client.CheckTaskUpdate(context.Background(), plan); done {
				t.Fatal("Anytime accepted a scheduled activation date")
			}
			runner.activation = ""
			for _, start := range []int{0, 2} {
				updateTestExec(t, runner.db, `UPDATE TMTask SET start=? WHERE uuid='task-1'`, start)
				if done, _ := client.CheckTaskUpdate(context.Background(), plan); done {
					t.Fatalf("Anytime accepted start mode %d", start)
				}
			}
		})
	}
}

func TestTodayAndEveningRequirePublicMembershipWhenActivationIsEmpty(t *testing.T) {
	t.Parallel()
	for _, when := range []string{"today", "evening"} {
		for _, memberAfter := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/member=%t", when, memberAfter), func(t *testing.T) {
				client, runner := newTaskUpdateTestClient(t)
				bucket := 0
				if when == "evening" {
					bucket = 1
				}
				// An Anytime task may retain both an ordering index and an old
				// evening bucket. Neither proves membership in the public list.
				updateTestExec(t, runner.db, `UPDATE TMTask SET start=1,todayIndex=0,startBucket=? WHERE uuid='task-1'`, bucket)
				plan := prepareTestUpdate(t, client, capture.UpdateTaskRequest{When: when})
				if plan.Schedule.Before.TodayMembership == nil || *plan.Schedule.Before.TodayMembership || runner.membershipReads != 1 {
					t.Fatalf("before state lacks explicit public membership: %+v", plan.Schedule.Before)
				}
				runner.onDispatch = func(url.Values) error {
					runner.todayMember = memberAfter
					return nil
				}
				response, err := client.ApplyTaskUpdate(context.Background(), plan)
				if memberAfter {
					if err != nil || !response.OK || response.ID != "task-1" {
						t.Fatalf("public Today result was not verified: %+v, %v", response, err)
					}
				} else {
					requireUpdateError(t, err, "update_unverified")
					if response.OK {
						t.Fatal("stale index falsely proved the requested result")
					}
				}
				done, err := client.CheckTaskUpdate(context.Background(), plan)
				if err != nil || done != memberAfter || len(runner.dispatches) != 1 {
					t.Fatalf("membership reconciliation = %t, %v, writes=%d", done, err, len(runner.dispatches))
				}
			})
		}
	}
}

func TestTodayMembershipReadIsBoundedAndPassesIdentityAsData(t *testing.T) {
	t.Parallel()
	requestedID := `id " & do shell script "unexpected"`
	client := &Client{Runner: captureContextRunner(func(ctx context.Context, executable string, args []string) ([]byte, []byte, error) {
		if executable != "/usr/bin/osascript" || !reflect.DeepEqual(args, []string{"-e", taskUpdateTodayMembershipScript, "--", requestedID}) {
			t.Fatalf("unexpected membership read: %s %q", executable, args)
		}
		if _, bounded := ctx.Deadline(); !bounded || strings.Contains(args[1], requestedID) {
			t.Fatal("membership read is unbounded or interpolates request data")
		}
		return []byte("true\n"), nil, nil
	})}
	member, err := client.readTaskUpdateTodayMembership(context.Background(), requestedID)
	if err != nil || !member {
		t.Fatalf("membership read = %t, %v", member, err)
	}
}

func TestTodayMembershipInvalidReadStopsBeforeDispatch(t *testing.T) {
	t.Parallel()
	for _, output := range []string{"", "1", "TRUE", "true\nfalse", "private malformed response"} {
		t.Run(output, func(t *testing.T) {
			client, runner := newTaskUpdateTestClient(t)
			runner.membershipOutput = &output
			_, err := client.PrepareTaskUpdate(context.Background(), capture.UpdateTaskRequest{ID: "task-1", When: "today"})
			requireUpdateError(t, err, "update_unverified")
			if strings.Contains(err.Error(), "private malformed response") || len(runner.dispatches) != 0 {
				t.Fatalf("invalid native output leaked or dispatched: %v", err)
			}
		})
	}
	for _, readError := range []error{errors.New("native read failed"), context.DeadlineExceeded, context.Canceled} {
		t.Run(readError.Error(), func(t *testing.T) {
			client, runner := newTaskUpdateTestClient(t)
			runner.membershipError = readError
			_, err := client.PrepareTaskUpdate(context.Background(), capture.UpdateTaskRequest{ID: "task-1", When: "today"})
			if !errors.Is(err, readError) || len(runner.dispatches) != 0 {
				t.Fatalf("failed membership read = %v, writes=%d", err, len(runner.dispatches))
			}
		})
	}
}

func TestUnrelatedCalendarReadsDoNotRequireTodayMembership(t *testing.T) {
	t.Parallel()
	for _, req := range []capture.UpdateTaskRequest{
		{When: "2026-09-13"}, {When: "someday"}, {Deadline: "2026-09-20"},
	} {
		t.Run(req.When+req.Deadline, func(t *testing.T) {
			client, runner := newTaskUpdateTestClient(t)
			runner.membershipError = errors.New("must not read Today")
			prepareTestUpdate(t, client, req)
			if runner.membershipReads != 0 {
				t.Fatal("unrelated date operation read Today membership")
			}
		})
	}
}

func TestLegacyBlankDatePlanDoesNotReinterpretTodayIndexAsMembership(t *testing.T) {
	t.Parallel()
	client, runner := newTaskUpdateTestClient(t)
	updateTestExec(t, runner.db, `UPDATE TMTask SET start=1,todayIndex=0,startBucket=0 WHERE uuid='task-1'`)
	plan := prepareTestUpdate(t, client, capture.UpdateTaskRequest{When: "today"})
	plan.Schedule.Before.TodayMembership = nil // Saved by the older verifier.
	done, err := client.CheckTaskUpdate(context.Background(), plan)
	if done {
		t.Fatal("legacy non-null index was silently accepted as public membership")
	}
	requireUpdateError(t, err, "update_conflict")
	if len(runner.dispatches) != 0 {
		t.Fatal("legacy ambiguous state caused a new write")
	}
}

func TestAnytimeClearsManualTodayWithNoActivationDate(t *testing.T) {
	t.Parallel()
	for _, due := range []string{"", "2026-09-13"} {
		t.Run("due="+due, func(t *testing.T) {
			client, runner := newTaskUpdateTestClient(t)
			runner.todayMember, runner.due = true, due
			updateTestExec(t, runner.db, `UPDATE TMTask SET start=1,todayIndex=0,startBucket=0 WHERE uuid='task-1'`)
			plan := prepareTestUpdate(t, client, capture.UpdateTaskRequest{When: "anytime"})
			if done, err := client.CheckTaskUpdate(context.Background(), plan); err != nil || done {
				t.Fatalf("manual Today falsely satisfied Anytime before dispatch: %t, %v", done, err)
			}
			runner.onDispatch = func(q url.Values) error {
				if !q.Has("when") || q.Get("when") != "" || q.Has("deadline") {
					t.Fatalf("Anytime must send the documented present-empty clear: %v", q)
				}
				runner.todayMember = false
				return nil
			}
			response, err := client.ApplyTaskUpdate(context.Background(), plan)
			if err != nil || !response.OK || len(runner.dispatches) != 1 {
				t.Fatalf("manual Today was not cleared: %+v, %v, writes=%d", response, err, len(runner.dispatches))
			}
			if _, err := client.ApplyTaskUpdate(context.Background(), plan); err != nil || len(runner.dispatches) != 1 {
				t.Fatalf("verified clear was repeated: %v, writes=%d", err, len(runner.dispatches))
			}
		})
	}
}

func TestAnytimeAllowsVerifiedTodayMembershipFromDueDeadline(t *testing.T) {
	t.Parallel()
	for _, due := range []string{"2026-09-04", "2026-09-05"} {
		t.Run(due, func(t *testing.T) {
			client, runner := newTaskUpdateTestClient(t)
			runner.todayMember, runner.due = true, due
			plan := prepareTestUpdate(t, client, capture.UpdateTaskRequest{When: "anytime"})
			response, err := client.ApplyTaskUpdate(context.Background(), plan)
			if err != nil || !response.OK || len(runner.dispatches) != 0 {
				t.Fatalf("deadline-driven Today prevented Anytime verification: %+v, %v, writes=%d", response, err, len(runner.dispatches))
			}
		})
	}
}

func TestCombinedAnytimeAndDeadlineUsesCurrentPublicDatesWithoutFalseConflict(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, activation, beforeDue, afterDue string
		start                                 int
		memberBefore, memberAfter             bool
	}{
		{"future start and deadline to Today", "2026-09-13", "2026-09-20", "2026-09-05", 2, false, true},
		{"manual Today with new future deadline", "", "", "2026-09-20", 1, true, false},
		{"manual Today with new due deadline", "", "", "2026-09-05", 1, true, true},
		{"move due deadline into future while clearing Today", "", "2026-09-05", "2026-09-20", 1, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, runner := newTaskUpdateTestClient(t)
			runner.activation, runner.due, runner.todayMember = tc.activation, tc.beforeDue, tc.memberBefore
			updateTestExec(t, runner.db, `UPDATE TMTask SET start=?,todayIndex=0,startBucket=0 WHERE uuid='task-1'`, tc.start)
			plan := prepareTestUpdate(t, client, capture.UpdateTaskRequest{When: "anytime", Deadline: tc.afterDue})
			runner.onDispatch = func(q url.Values) error {
				if !q.Has("when") || q.Get("when") != "" || q.Get("deadline") != tc.afterDue {
					t.Fatalf("combined update omitted date clear or changed deadline: %v", q)
				}
				runner.activation, runner.due, runner.todayMember = "", tc.afterDue, tc.memberAfter
				updateTestExec(t, runner.db, `UPDATE TMTask SET start=1,startDate=NULL WHERE uuid='task-1'`)
				return context.DeadlineExceeded
			}
			if _, err := client.ApplyTaskUpdate(context.Background(), plan); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expected interrupted native reply: %v", err)
			}
			done, err := client.CheckTaskUpdate(context.Background(), plan)
			if err != nil || !done || len(runner.dispatches) != 1 {
				t.Fatalf("requested deadline change became a schedule conflict: %t, %v, writes=%d", done, err, len(runner.dispatches))
			}
			if _, err := client.ApplyTaskUpdate(context.Background(), plan); err != nil || len(runner.dispatches) != 1 {
				t.Fatalf("combined update repeated after reconciliation: %v, writes=%d", err, len(runner.dispatches))
			}
		})
	}
}
