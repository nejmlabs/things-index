package helper

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nejmlabs/things-index/internal/capture"
)

func newQueryTestClient(t *testing.T, membership string) (*Client, *sql.DB) {
	t.Helper()
	path := setupTestThingsDB(t)
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, statement := range []string{
		`ALTER TABLE TMTask ADD COLUMN "index" INTEGER DEFAULT 0`,
		`ALTER TABLE TMTask ADD COLUMN start INTEGER DEFAULT 1`,
		`ALTER TABLE TMTask ADD COLUMN todayIndex INTEGER`,
		`ALTER TABLE TMTask ADD COLUMN startDate REAL`,
		`ALTER TABLE TMTask ADD COLUMN startBucket INTEGER DEFAULT 0`,
	} {
		updateTestExec(t, db, statement)
	}
	runner := captureContextRunner(func(ctx context.Context, executable string, args []string) ([]byte, []byte, error) {
		if _, bounded := ctx.Deadline(); !bounded {
			t.Fatal("query native call must be bounded")
		}
		if executable == "/usr/bin/pgrep" {
			return nil, nil, nil
		}
		if executable != "/usr/bin/osascript" || len(args) != 4 || args[0] != "-e" || args[1] != queryTaskListIDsScript || args[2] != "--" {
			t.Fatalf("unexpected native query: %s %q", executable, args)
		}
		if args[3] != "today" && args[3] != "anytime" && args[3] != "someday" {
			t.Fatalf("unexpected list: %q", args[3])
		}
		return []byte(membership), nil, nil
	})
	return &Client{DBPath: path, Runner: runner}, db
}

func queryTestTasks(t *testing.T, client *Client, req capture.QueryTasksRequest) []TaskItem {
	t.Helper()
	response, err := client.QueryTasks(context.Background(), req)
	if err != nil || !response.OK {
		t.Fatalf("query response = %+v, %v", response, err)
	}
	var tasks []TaskItem
	if err := json.Unmarshal([]byte(response.ID), &tasks); err != nil {
		t.Fatal(err)
	}
	return tasks
}

func TestQueryTasksUsesPublicCalendarLists(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		scope string
		ids   string
		want  []string
	}{
		{"today", "evening\ntoday\n", []string{"evening", "today"}},
		// Lists are independent public oracles: a Today item may also be in
		// Anytime, but membership is not assumed (for example, project state).
		{"anytime", "today\nanytime\n", []string{"today", "anytime"}},
		{"someday", "someday\n", []string{"someday"}},
	} {
		t.Run(tc.scope, func(t *testing.T) {
			client, db := newQueryTestClient(t, tc.ids)
			updateTestExec(t, db, `INSERT INTO TMTask (uuid,type,title,"index",start,todayIndex,startDate,startBucket) VALUES
				('evening',0,'Evening',1,1,NULL,NULL,1),
				('today',0,'Today',2,1,0,NULL,0),
				('anytime',0,'Anytime',3,1,0,NULL,0),
				('future',0,'Upcoming',4,2,0,132814464,0),
				('someday',0,'Someday',5,2,0,NULL,0)`)
			tasks := queryTestTasks(t, client, capture.QueryTasksRequest{Scope: tc.scope})
			var got []string
			for _, task := range tasks {
				got = append(got, task.ID)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("scope %s IDs = %v, want %v", tc.scope, got, tc.want)
			}
		})
	}
}

func TestQueryTasksPublicMembershipPreservesFiltersAndLimit(t *testing.T) {
	t.Parallel()
	client, db := newQueryTestClient(t, "project\nheading\nwrong-type\ntrashed\ncompleted\nfiltered\nselected\nsecond\nselected\n")
	updateTestExec(t, db, `INSERT INTO TMArea (uuid,title) VALUES ('area','Home')`)
	updateTestExec(t, db, `INSERT INTO TMTag (uuid,title) VALUES ('tag','Errand')`)
	updateTestExec(t, db, `INSERT INTO TMTask (uuid,type,title,"index",area) VALUES
		('project',1,'Shopping',0,'area'),('heading',2,'Groceries',0,''),('wrong-type',1,'Needle',0,'')`)
	updateTestExec(t, db, `INSERT INTO TMTask (uuid,type,title,notes,"index",project,heading,status,trashed) VALUES
		('nonmember',0,'Needle','description',1,'project','heading',0,0),
		('trashed',0,'Needle','description',2,'project','heading',0,1),
		('completed',0,'Needle','description',3,'project','heading',3,0),
		('filtered',0,'Other title','description',4,'project','heading',0,0),
		('selected',0,'Needle','description',5,'project','heading',0,0),
		('second',0,'Needle','description',6,'project','heading',0,0)`)
	updateTestExec(t, db, `INSERT INTO TMTaskTag (tasks,tags) VALUES ('nonmember','tag'),('trashed','tag'),('completed','tag'),('filtered','tag'),('selected','tag'),('second','tag')`)
	req := capture.QueryTasksRequest{Scope: "today", Query: "needle", Project: "SHOPPING", Area: "home", Tag: "errand", Limit: 1}
	tasks := queryTestTasks(t, client, req)
	if len(tasks) != 1 || tasks[0] != (TaskItem{ID: "selected", Title: "Needle", Notes: "description", Status: "open", Project: "Shopping", Area: "Home", Heading: "Groceries"}) {
		t.Fatalf("filtered limited result = %+v", tasks)
	}
	req.Limit, req.IncludeCompleted = 20, true
	tasks = queryTestTasks(t, client, req)
	if len(tasks) != 3 || tasks[0].ID != "selected" || tasks[1].ID != "second" || tasks[2].ID != "completed" || tasks[2].Status != "completed" {
		t.Fatalf("completed/order/duplicate filtering = %+v", tasks)
	}
}

func TestQueryTasksLargePublicListDoesNotExceedSQLiteBindings(t *testing.T) {
	t.Parallel()
	var membership strings.Builder
	for i := 0; i < 40000; i++ {
		fmt.Fprintf(&membership, "member-%d\n", i)
	}
	client, db := newQueryTestClient(t, membership.String())
	updateTestExec(t, db, `INSERT INTO TMTask (uuid,type,title,"index") VALUES ('nonmember',0,'Skip',0),('member-39999',0,'Last ID',1)`)
	tasks := queryTestTasks(t, client, capture.QueryTasksRequest{Scope: "today", Limit: 1})
	if len(tasks) != 1 || tasks[0].ID != "member-39999" {
		t.Fatalf("large list result = %+v", tasks)
	}
}

func TestQueryTasksPublicListProtocol(t *testing.T) {
	t.Parallel()
	for _, output := range []string{"", "\n", "\r\n"} {
		client, _ := newQueryTestClient(t, output)
		if tasks := queryTestTasks(t, client, capture.QueryTasksRequest{Scope: "today"}); len(tasks) != 0 {
			t.Fatalf("empty list returned %+v", tasks)
		}
	}
	for _, output := range []string{"id with spaces\n", "id\n\nother\n", "id\tother\n", "id, other\n", "id\x00\n"} {
		client, _ := newQueryTestClient(t, output)
		if response, err := client.QueryTasks(context.Background(), capture.QueryTasksRequest{Scope: "today"}); err == nil || response.OK {
			t.Fatalf("accepted malformed list output %q: %+v, %v", output, response, err)
		}
	}
	client, db := newQueryTestClient(t, "one\r\none\r\n")
	updateTestExec(t, db, `INSERT INTO TMTask (uuid,type,title) VALUES ('one',0,'One')`)
	if tasks := queryTestTasks(t, client, capture.QueryTasksRequest{Scope: "today"}); len(tasks) != 1 {
		t.Fatalf("CRLF/duplicate list returned %+v", tasks)
	}
}

func TestQueryTasksListLifecycleAndCancellation(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"already running", "stopped", "native error", "cancel", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			client, _ := newQueryTestClient(t, "")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "timeout" {
				client.Timeout = 10 * time.Millisecond
			}
			var calls []string
			client.Runner = captureContextRunner(func(callCtx context.Context, executable string, args []string) ([]byte, []byte, error) {
				switch executable {
				case "/usr/bin/pgrep":
					calls = append(calls, "pgrep")
					if len(calls) == 1 && mode != "already running" {
						return nil, nil, errors.New("not running")
					}
					if callCtx.Err() != nil {
						t.Fatal("cleanup context was canceled")
					}
					return nil, nil, nil
				case "/usr/bin/open":
					calls = append(calls, "open hidden")
					if !reflect.DeepEqual(args, []string{"-g", "-j", "-a", "/Applications/Things3.app"}) {
						t.Fatalf("visible launch: %q", args)
					}
					return nil, nil, nil
				case "/usr/bin/osascript":
					if len(args) == 2 && args[1] == `tell application "Things3" to quit` {
						calls = append(calls, "quit")
						if callCtx.Err() != nil {
							t.Fatal("quit context was canceled")
						}
						return nil, nil, nil
					}
					calls = append(calls, "membership")
					switch mode {
					case "native error":
						return nil, []byte("private stderr"), errors.New("read failed")
					case "cancel":
						cancel()
						return nil, nil, callCtx.Err()
					case "timeout":
						<-callCtx.Done()
						return nil, nil, callCtx.Err()
					}
					return []byte("\n"), nil, nil
				default:
					t.Fatalf("unexpected executable: %s", executable)
					return nil, nil, nil
				}
			})
			response, err := client.QueryTasks(ctx, capture.QueryTasksRequest{Scope: "today"})
			if mode == "already running" || mode == "stopped" {
				if err != nil || !response.OK {
					t.Fatalf("query = %+v, %v", response, err)
				}
			} else if err == nil || response.OK || strings.Contains(err.Error(), "private stderr") {
				t.Fatalf("failed query = %+v, %v", response, err)
			}
			want := []string{"pgrep", "open hidden", "membership", "pgrep", "quit"}
			if mode == "already running" {
				want = []string{"pgrep", "membership"}
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("lifecycle calls = %v, want %v", calls, want)
			}
		})
	}
}

func TestQueryTasksOtherScopesDoNotUseAutomation(t *testing.T) {
	t.Parallel()
	client, db := newQueryTestClient(t, "")
	updateTestExec(t, db, `INSERT INTO TMTask (uuid,type,title,start,project,area) VALUES ('inbox',0,'Inbox',0,NULL,NULL)`)
	client.Runner = captureContextRunner(func(context.Context, string, []string) ([]byte, []byte, error) {
		t.Fatal("unscoped/inbox query used automation")
		return nil, nil, nil
	})
	for _, scope := range []string{"all", "", "inbox"} {
		if tasks := queryTestTasks(t, client, capture.QueryTasksRequest{Scope: scope}); len(tasks) != 1 || tasks[0].ID != "inbox" {
			t.Fatalf("scope %s = %+v", scope, tasks)
		}
	}
}
