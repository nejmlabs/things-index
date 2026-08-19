package helper

// Live test against the real local Things 3 installation. It mutates the
// Things database (creates one throwaway project and headings inside it), so
// it only runs when explicitly requested:
//
//	THINGS_INDEX_LIVE_TEST=1 THINGS_INDEX_THINGS_AUTH_TOKEN=<token> \
//	  go test ./internal/helper -run TestLiveHeadingOperations -v
//
// Its purpose is to settle whether Things honors the undocumented heading
// mutations (see docs: the update command is specified for to-dos only, and a
// project's items array is create-specific). The heading used for the rename
// and archive probes is created the documented way: nested in the items array
// of a brand-new project.

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLiveHeadingOperations(t *testing.T) {
	if os.Getenv("THINGS_INDEX_LIVE_TEST") != "1" {
		t.Skip("set THINGS_INDEX_LIVE_TEST=1 to run against the local Things 3 installation")
	}
	token := os.Getenv("THINGS_INDEX_THINGS_AUTH_TOKEN")
	if token == "" {
		t.Skip("set THINGS_INDEX_THINGS_AUTH_TOKEN (Things > Settings > General > Enable Things URLs > Manage)")
	}

	client := NewClient(token)
	ctx := context.Background()

	if err := client.Ping(ctx); err != nil {
		t.Fatalf("Things database not reachable: %v", err)
	}

	// 1. Create the throwaway project WITH a heading nested in items — the
	// one documented way to create a heading.
	projectTitle := fmt.Sprintf("ThingsIndex Live Test %d", time.Now().Unix())
	jsonPayload := fmt.Sprintf(`[{"type":"project","attributes":{"title":%q,"items":[{"type":"heading","attributes":{"title":"Alpha"}}]}}]`, projectTitle)
	createURL := fmt.Sprintf("things:///json?data=%s&reveal=false&auth-token=%s",
		strings.ReplaceAll(url.QueryEscape(jsonPayload), "+", "%20"), url.QueryEscape(token))
	if _, _, err := (ExecRunner{}).Run(ctx, "/usr/bin/open", []string{"-g", createURL}); err != nil {
		t.Fatalf("dispatch project-with-heading create URL: %v", err)
	}

	db, err := client.openDB(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var projectUUID, headingUUID string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if projectUUID == "" {
			_ = db.QueryRowContext(ctx, `SELECT uuid FROM TMTask WHERE type = 1 AND title = ? AND trashed = 0`, projectTitle).Scan(&projectUUID)
		}
		if projectUUID != "" {
			if err := db.QueryRowContext(ctx, `SELECT uuid FROM TMTask WHERE type = 2 AND title = 'Alpha' AND project = ? AND trashed = 0`, projectUUID).Scan(&headingUUID); err == nil && headingUUID != "" {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if projectUUID == "" || headingUUID == "" {
		t.Fatalf("documented project+heading create did not appear (project=%q heading=%q)", projectUUID, headingUUID)
	}
	t.Logf("created project %q (%s) with nested heading Alpha (%s)", projectTitle, projectUUID, headingUUID)
	defer func() {
		if _, err := client.ArchiveProject(ctx, projectUUID, "", "cancel"); err != nil {
			t.Logf("cleanup: cancel project failed: %v — delete %q from Things manually", err, projectTitle)
		} else {
			t.Logf("cleanup: canceled project %q; it now sits in the Logbook", projectTitle)
		}
	}()

	// 2. Probe: add a second heading to the now-existing project via the
	// update+items payload (docs mark items create-specific, so expect fail).
	if created, err := client.CreateHeading(ctx, projectTitle, "Second"); err != nil {
		t.Logf("RESULT create_things_heading (existing project): FAILED: %v", err)
	} else {
		t.Logf("RESULT create_things_heading (existing project): OK (%s)", created.ID)
	}

	// 3. Probe: rename the heading via things:///update on its UUID.
	headingName := "Alpha"
	if renamed, err := client.RenameHeading(ctx, projectTitle, "Alpha", "Beta"); err != nil {
		t.Logf("RESULT rename_things_heading: FAILED: %v", err)
	} else {
		t.Logf("RESULT rename_things_heading: OK (%s)", renamed.ID)
		headingName = "Beta"
	}

	// 4. Probe: archive the heading via things:///update completed=true.
	if archived, err := client.ArchiveHeading(ctx, projectTitle, headingName); err != nil {
		t.Logf("RESULT archive_things_heading: FAILED: %v", err)
	} else {
		t.Logf("RESULT archive_things_heading: OK (%s)", archived.ID)
	}
}
