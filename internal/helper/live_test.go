package helper

// Live test against the real local Things 3 installation. It mutates the
// Things database (creates one throwaway project and headings inside it), so
// it only runs when explicitly requested:
//
//	THINGS_INDEX_LIVE_TEST=1 go test ./internal/helper -run TestLiveHeadingOperations -v
//
// It proves the Shortcut-backed heading operations end to end. Neither the
// Things URL scheme nor AppleScript can touch headings at all (the update
// command is specified for to-dos, a project's items array is
// create-specific, and the AppleScript dictionary has no heading class), so
// CreateHeading, RenameHeading, and ArchiveHeading run the bundled
// ThingsIndex Helper shortcut, which must be installed for the logged-in
// user, and verify every mutation against SQLite before reporting success.

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestLiveHeadingOperations(t *testing.T) {
	if os.Getenv("THINGS_INDEX_LIVE_TEST") != "1" {
		t.Skip("set THINGS_INDEX_LIVE_TEST=1 to run against the local Things 3 installation")
	}

	client := NewClient(os.Getenv("THINGS_INDEX_THINGS_AUTH_TOKEN"))
	ctx := context.Background()

	if err := client.Ping(ctx); err != nil {
		t.Fatalf("Things database not reachable: %v", err)
	}

	projectTitle := fmt.Sprintf("ThingsIndex Live Test %d", time.Now().Unix())
	script := fmt.Sprintf(`tell application "Things3" to make new project with properties {name:"%s"}`, escapeAppleScriptString(projectTitle))
	if _, _, err := (ExecRunner{}).Run(ctx, "/usr/bin/osascript", []string{"-e", script}); err != nil {
		t.Fatalf("create throwaway project: %v", err)
	}
	defer func() {
		if _, err := client.ArchiveProject(ctx, "", projectTitle, "cancel"); err != nil {
			t.Logf("cleanup: cancel project failed: %v — delete %q from Things manually", err, projectTitle)
		} else {
			t.Logf("cleanup: canceled project %q; it now sits in the Logbook", projectTitle)
		}
	}()

	created, err := client.CreateHeading(ctx, projectTitle, "Alpha")
	if err != nil {
		t.Fatalf("create heading in existing project: %v", err)
	}
	t.Logf("created heading Alpha (%s) in existing project %q", created.ID, projectTitle)

	// A retry must reuse the heading rather than duplicate it.
	retried, err := client.CreateHeading(ctx, projectTitle, "Alpha")
	if err != nil {
		t.Errorf("idempotent create retry: %v", err)
	} else if retried.ID != created.ID {
		t.Errorf("create retry returned %s, want %s", retried.ID, created.ID)
	}

	renamed, err := client.RenameHeading(ctx, projectTitle, "Alpha", "Beta")
	if err != nil {
		t.Fatalf("rename heading: %v", err)
	}
	t.Logf("renamed heading Alpha to Beta (%s)", renamed.ID)

	archived, err := client.ArchiveHeading(ctx, projectTitle, "Beta")
	if err != nil {
		t.Fatalf("archive heading: %v", err)
	}
	t.Logf("archived heading Beta (%s)", archived.ID)
}
