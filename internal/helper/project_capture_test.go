package helper

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nejmlabs/things-index/internal/capture"
)

func projectCaptureFixture(t *testing.T) (*Client, *sql.DB, *[]url.Values) {
	t.Helper()
	path := setupTestThingsDB(t)
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var adds []url.Values
	runner := &scriptedRunner{handler: func(executable string, args []string) ([]byte, []byte, error) {
		if executable == "/usr/bin/pgrep" {
			return nil, nil, nil
		}
		if executable != "/usr/bin/open" {
			return nil, nil, fmt.Errorf("unexpected command: %s", executable)
		}
		u, err := url.Parse(args[len(args)-1])
		if err != nil {
			return nil, nil, err
		}
		q := u.Query()
		switch u.Path {
		case "/add-project":
			_, err = db.Exec(`INSERT INTO TMTask (uuid,type,title,notes,area,creationDate) VALUES ('new-project',1,?,?,?,?)`, q.Get("title"), q.Get("notes"), q.Get("area-id"), macEpochSeconds(time.Now()))
		case "/add":
			adds = append(adds, q)
			_, err = db.Exec(`INSERT INTO TMTask (uuid,type,title,notes,project,creationDate) VALUES ('new-task',0,?,?,?,?)`, q.Get("title"), q.Get("notes"), q.Get("list-id"), macEpochSeconds(time.Now()))
		case "/update":
			_, err = db.Exec(`UPDATE TMTask SET title = ? WHERE uuid = ?`, q.Get("title"), q.Get("id"))
		default:
			err = fmt.Errorf("unexpected URL operation: %s", u.Path)
		}
		return nil, nil, err
	}}
	return &Client{DBPath: path, Runner: runner, AuthToken: "test-token"}, db, &adds
}

func TestCaptureResolvesProjectWithoutChangingRequest(t *testing.T) {
	for _, tc := range []struct {
		name, id string
		warning  bool
	}{
		{"Kitchen renovation", "", false},
		{"kitchen", "", true},
		{"kichen renovation", "", true},
		{"Old project name", "kitchen-id", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, db, adds := projectCaptureFixture(t)
			if _, err := db.Exec(`INSERT INTO TMTask (uuid,type,title) VALUES ('kitchen-id',1,'Kitchen renovation')`); err != nil {
				t.Fatal(err)
			}
			destination := &capture.Destination{Kind: capture.DestinationProject, ID: tc.id, Name: tc.name}
			task := capture.Request{TaskFields: capture.TaskFields{Title: "Buy paint", Destination: destination}}
			before, _ := task.Hash()
			response, err := client.Capture(context.Background(), testRequestID, task)
			if err != nil {
				t.Fatal(err)
			}
			after, _ := task.Hash()
			if before != after || destination.Name != tc.name || destination.ID != tc.id {
				t.Fatal("capture mutated the original request")
			}
			if len(*adds) != 1 || (*adds)[0].Get("list-id") != "kitchen-id" || (*adds)[0].Get("list") != "" {
				t.Fatalf("incorrect project dispatch: %v", *adds)
			}
			if (len(response.Warnings) > 0) != tc.warning {
				t.Fatalf("unexpected matching warnings: %v", response.Warnings)
			}
			if tc.warning && !strings.Contains(response.Warnings[0], "Kitchen renovation") {
				t.Fatalf("warning omits selected project: %v", response.Warnings)
			}
		})
	}
}

func TestCreateProjectThenCaptureByReturnedID(t *testing.T) {
	client, db, adds := projectCaptureFixture(t)
	if _, err := db.Exec(`INSERT INTO TMArea (uuid,title) VALUES ('personal','Personal'),('work','Work'); INSERT INTO TMTask (uuid,type,title,area) VALUES ('other-trip',1,'Trip','work')`); err != nil {
		t.Fatal(err)
	}
	project, err := client.CreateProject(context.Background(), capture.CreateProjectRequest{Title: "Trip", Area: "Personal"})
	if err != nil {
		t.Fatal(err)
	}
	if project.ID == "other-trip" {
		t.Fatal("creation reused the project from another area")
	}
	_, err = client.Capture(context.Background(), testRequestID, capture.Request{TaskFields: capture.TaskFields{Title: "Book hotel", Destination: &capture.Destination{Kind: capture.DestinationProject, ID: project.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(*adds) != 1 || (*adds)[0].Get("list-id") != project.ID {
		t.Fatalf("task missed newly created project: %v", *adds)
	}
}

func TestCaptureSavesUnclearProjectToInbox(t *testing.T) {
	for _, tc := range []struct {
		name, id, heading, reason string
	}{
		{name: "kitchen", reason: "matched multiple active projects"},
		{name: "completely unrelated", reason: "did not match an active project"},
		{id: "missing-id", reason: "did not match an active project"},
		{name: "Kitchen renovation", id: "missing-id", reason: "did not match an active project"},
		{name: "kitchen", heading: "Purchases", reason: "matched multiple active projects"},
		{name: "Old renovation", reason: "did not match an active project"},
	} {
		t.Run(tc.name+"/"+tc.id+"/"+tc.heading, func(t *testing.T) {
			client, db, adds := projectCaptureFixture(t)
			if _, err := db.Exec(`INSERT INTO TMTask (uuid,type,title,status) VALUES ('one',1,'Kitchen renovation',0),('two',1,'Kitchen supplies',0),('old',1,'Old renovation',3)`); err != nil {
				t.Fatal(err)
			}
			destination := capture.Destination{Kind: capture.DestinationProject, Name: tc.name, ID: tc.id, Heading: tc.heading}
			originalDestination := destination
			task := capture.Request{TaskFields: capture.TaskFields{Title: "Buy paint", Notes: "Original notes", Destination: &destination}}
			before, _ := task.Hash()
			response, err := client.Capture(context.Background(), testRequestID, task)
			if err != nil {
				t.Fatal(err)
			}
			if !response.OK || response.ID != "new-task" || len(*adds) != 1 {
				t.Fatalf("expected one successful capture: response=%+v adds=%v", response, *adds)
			}
			add := (*adds)[0]
			for _, field := range []string{"list", "list-id", "heading"} {
				if add.Get(field) != "" {
					t.Fatalf("Inbox capture retained destination %s=%q", field, add.Get(field))
				}
			}
			if len(response.Warnings) != 1 {
				t.Fatalf("expected one Inbox notice: %v", response.Warnings)
			}
			warning := response.Warnings[0]
			for _, want := range []string{tc.name, tc.id, tc.heading, tc.reason, "captured in Inbox"} {
				if want != "" && !strings.Contains(warning, want) {
					t.Fatalf("Inbox notice %q omits %q", warning, want)
				}
			}
			if add.Get("notes") != task.Notes+"\n\n"+warning {
				t.Fatalf("capture lost notes or requested placement: %q", add.Get("notes"))
			}
			var title, project, notes string
			if err := db.QueryRow(`SELECT title, project, notes FROM TMTask WHERE uuid = 'new-task'`).Scan(&title, &project, &notes); err != nil {
				t.Fatal(err)
			}
			if title != "ThingsIndex pending ["+testRequestID+"]" || project != "" || notes != add.Get("notes") {
				t.Fatalf("incorrect saved task: title=%q project=%q notes=%q", title, project, notes)
			}
			if err := client.FinaliseCapture(context.Background(), response.ID, task.Title); err != nil {
				t.Fatal(err)
			}
			after, _ := task.Hash()
			if after != before || destination != originalDestination || task.Notes != "Original notes" {
				t.Fatal("Inbox fallback mutated the original request")
			}
		})
	}
}

func TestCaptureDoesNotFallBackOnProjectDatabaseFailure(t *testing.T) {
	client, db, adds := projectCaptureFixture(t)
	if _, err := db.Exec(`DROP TABLE TMArea`); err != nil {
		t.Fatal(err)
	}
	_, err := client.Capture(context.Background(), testRequestID, capture.Request{TaskFields: capture.TaskFields{Title: "Buy paint", Destination: &capture.Destination{Kind: capture.DestinationProject, Name: "kitchen"}}})
	var matchErr *ProjectMatchError
	if err == nil || errors.As(err, &matchErr) {
		t.Fatalf("database failure became a matching result: %v", err)
	}
	if len(*adds) != 0 {
		t.Fatal("database failure dispatched a task")
	}
}

func TestCaptureHeadingUsesResolvedProject(t *testing.T) {
	client, db, adds := projectCaptureFixture(t)
	if _, err := db.Exec(`INSERT INTO TMTask (uuid,type,title,project) VALUES ('one',1,'Kitchen',''),('two',1,'Kitchen',''),('heading',2,'Supplies','two')`); err != nil {
		t.Fatal(err)
	}
	_, err := client.Capture(context.Background(), testRequestID, capture.Request{TaskFields: capture.TaskFields{Title: "Buy paint", Destination: &capture.Destination{Kind: capture.DestinationProject, ID: "two", Heading: "Supplies"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(*adds) != 1 || (*adds)[0].Get("list-id") != "two" || (*adds)[0].Get("heading") != "Supplies" {
		t.Fatalf("incorrect heading dispatch: %v", *adds)
	}
}
