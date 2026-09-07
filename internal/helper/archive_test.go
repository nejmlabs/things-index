package helper

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func archiveFixture(t *testing.T) (*Client, *sql.DB) {
	t.Helper()
	path := setupTestThingsDB(t)
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`INSERT INTO TMTask (uuid,type,title,status,trashed) VALUES ('task-id',0,'Task',0,0),('project-id',1,'Project',0,0),('heading-id',2,'Heading',0,0)`); err != nil {
		t.Fatal(err)
	}
	return &Client{DBPath: path, VerifyWindow: 10 * time.Millisecond}, db
}

func callArchive(ctx context.Context, client *Client, kind, id, action string) (Response, error) {
	if kind == "task" {
		return client.ArchiveTask(ctx, id, "", "", action)
	}
	return client.ArchiveProject(ctx, id, "", action)
}

func TestArchiveRequiresExactVerifiedResult(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		kind   string
		action string
		status int
		trash  int
	}{
		{"task", "", 3, 0},
		{"task", "complete", 3, 0},
		{"task", "cancel", 2, 0},
		{"task", "trash", 0, 1},
		{"project", "complete", 3, 0},
		{"project", "cancel", 2, 0},
	} {
		t.Run(tc.kind+"/"+tc.action, func(t *testing.T) {
			client, db := archiveFixture(t)
			calls := 0
			client.Runner = captureContextRunner(func(ctx context.Context, executable string, args []string) ([]byte, []byte, error) {
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("native command has no deadline")
				}
				if executable == "/usr/bin/pgrep" {
					return nil, nil, nil
				}
				calls++
				action := tc.action
				if action == "" {
					action = "complete"
				}
				wantArgs := []string{"-e", archiveScript, "--", tc.kind, tc.kind + "-id", action}
				if executable != "/usr/bin/osascript" || !reflect.DeepEqual(args, wantArgs) {
					t.Fatalf("unexpected native call %s %q", executable, args)
				}
				_, err := db.Exec(`UPDATE TMTask SET status=?,trashed=? WHERE uuid=?`, tc.status, tc.trash, tc.kind+"-id")
				return nil, nil, err
			})
			response, err := callArchive(context.Background(), client, tc.kind, tc.kind+"-id", tc.action)
			if err != nil || !response.OK || response.ID != tc.kind+"-id" || calls != 1 {
				t.Fatalf("response=%+v, error=%v, calls=%d", response, err, calls)
			}
		})
	}
}

func TestArchiveRejectsWrongTypeAndUnavailableIDBeforeDispatch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ kind, id, code string }{
		{"task", "project-id", "item_type_mismatch"},
		{"task", "heading-id", "item_type_mismatch"},
		{"project", "task-id", "item_type_mismatch"},
		{"task", "missing-id", "task_not_found"},
		{"project", "missing-id", "project_not_found"},
	} {
		t.Run(tc.kind+"/"+tc.id, func(t *testing.T) {
			client, _ := archiveFixture(t)
			client.Runner = &mockRunner{onRun: func(string, []string) error { t.Fatal("invalid target dispatched"); return nil }}
			_, err := callArchive(context.Background(), client, tc.kind, tc.id, "complete")
			var operationError *OperationError
			if !errors.As(err, &operationError) || operationError.Code != tc.code {
				t.Fatalf("error=%v, want %s", err, tc.code)
			}
		})
	}
}

func TestArchiveDoesNotTrustNativeExitOrUnrelatedChanges(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		kind   string
		action string
		update string
	}{
		{"native did nothing", "task", "complete", ""},
		{"wrong status", "task", "complete", `UPDATE TMTask SET status=2 WHERE uuid='task-id'`},
		{"wrong row", "task", "complete", `UPDATE TMTask SET status=3 WHERE uuid='project-id'`},
		{"trash is not completion", "task", "complete", `UPDATE TMTask SET status=3,trashed=1 WHERE uuid='task-id'`},
		{"completion is not trash", "task", "trash", `UPDATE TMTask SET status=3 WHERE uuid='task-id'`},
		{"project wrong status", "project", "cancel", `UPDATE TMTask SET status=3 WHERE uuid='project-id'`},
		{"type changed", "project", "cancel", `UPDATE TMTask SET status=2,type=0 WHERE uuid='project-id'`},
		{"row disappeared", "task", "complete", `DELETE FROM TMTask WHERE uuid='task-id'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, db := archiveFixture(t)
			client.Runner = &mockRunner{onRun: func(executable string, args []string) error {
				if executable == "/usr/bin/osascript" && tc.update != "" {
					_, err := db.Exec(tc.update)
					return err
				}
				return nil
			}}
			response, err := callArchive(context.Background(), client, tc.kind, tc.kind+"-id", tc.action)
			if err == nil || response.OK || response.ID != "" {
				t.Fatalf("unverified result accepted: response=%+v, error=%v", response, err)
			}
		})
	}
}

func TestArchiveAlreadyAppliedDoesNotDispatchAgain(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"task", "project"} {
		t.Run(kind, func(t *testing.T) {
			client, db := archiveFixture(t)
			if _, err := db.Exec(`UPDATE TMTask SET status=3 WHERE uuid=?`, kind+"-id"); err != nil {
				t.Fatal(err)
			}
			client.Runner = &mockRunner{onRun: func(string, []string) error { t.Fatal("already completed item dispatched"); return nil }}
			response, err := callArchive(context.Background(), client, kind, kind+"-id", "complete")
			if err != nil || !response.OK || response.ID != kind+"-id" {
				t.Fatalf("response=%+v, error=%v", response, err)
			}
		})
	}
}

func TestArchiveFailureRestoresOnlyOriginallyStoppedApp(t *testing.T) {
	t.Parallel()
	for _, nativeFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "canceled verification", true: "native failure"}[nativeFailure], func(t *testing.T) {
			client, _ := archiveFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			pgrepCalls, quitCalls, mutationCalls := 0, 0, 0
			client.Runner = captureContextRunner(func(callCtx context.Context, executable string, args []string) ([]byte, []byte, error) {
				if executable == "/usr/bin/pgrep" {
					pgrepCalls++
					if pgrepCalls == 1 {
						return nil, nil, errors.New("not running")
					}
					return nil, nil, nil
				}
				if len(args) == 2 && strings.Contains(args[1], "to quit") {
					quitCalls++
					if callCtx.Err() != nil {
						t.Fatal("cleanup inherited canceled context")
					}
					if _, ok := callCtx.Deadline(); !ok {
						t.Fatal("cleanup lacks a deadline")
					}
					return nil, nil, nil
				}
				mutationCalls++
				if nativeFailure {
					return nil, nil, errors.New("native failure")
				}
				cancel()
				return nil, nil, nil
			})
			response, err := client.ArchiveTask(ctx, "task-id", "", "", "complete")
			if err == nil || response.OK || pgrepCalls != 2 || quitCalls != 1 || mutationCalls != 1 {
				t.Fatalf("response=%+v, error=%v, pgrep=%d quit=%d mutations=%d", response, err, pgrepCalls, quitCalls, mutationCalls)
			}
			if !nativeFailure && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation was not preserved: %v", err)
			}
		})
	}
}

func TestArchiveRejectsInvalidActionsBeforeOpeningDatabase(t *testing.T) {
	t.Parallel()
	client := &Client{DBPath: "/does-not-exist"}
	if _, err := client.ArchiveTask(context.Background(), "task-id", "", "", "invalid"); err == nil || !strings.Contains(err.Error(), "action must") {
		t.Fatalf("invalid task action error=%v", err)
	}
	if _, err := client.ArchiveProject(context.Background(), "project-id", "", "trash"); err == nil || !strings.Contains(err.Error(), "action must") {
		t.Fatalf("invalid project action error=%v", err)
	}
}
