package helper

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/nejmlabs/things-index/internal/capture"
)

func (c *Client) ArchiveTask(ctx context.Context, id, title, project, action string) (Response, error) {
	if err := (capture.ArchiveTaskRequest{ID: id, Title: title, Project: project, Action: action}).Validate(); err != nil {
		return Response{}, err
	}
	db, err := c.openDB(ctx)
	if err != nil {
		return Response{}, err
	}
	defer db.Close()
	if id == "" {
		query := `SELECT uuid FROM TMTask WHERE type = 0 AND LOWER(title) = LOWER(?) AND trashed = 0 AND status = 0`
		args := []any{title}
		if project != "" {
			projectID, err := findProjectUUID(ctx, db, project)
			if err != nil {
				return Response{}, err
			}
			query += ` AND project = ?`
			args = append(args, projectID)
		}
		rows, err := db.QueryContext(ctx, query+" LIMIT 2", args...)
		if err != nil {
			return Response{}, fmt.Errorf("query archive task: %w", err)
		}
		var matches []string
		for rows.Next() {
			var match string
			if err := rows.Scan(&match); err != nil {
				rows.Close()
				return Response{}, fmt.Errorf("read archive task: %w", err)
			}
			matches = append(matches, match)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return Response{}, fmt.Errorf("iterate archive task matches: %w", err)
		}
		if len(matches) == 0 {
			return Response{}, &OperationError{Code: "task_not_found"}
		}
		if len(matches) != 1 {
			return Response{}, &OperationError{Code: "task_ambiguous"}
		}
		id = matches[0]
	}
	return c.archiveItem(ctx, db, id, "task", action, 0)
}

func (c *Client) ArchiveProject(ctx context.Context, id, name, action string) (Response, error) {
	if err := (capture.ArchiveProjectRequest{ID: id, Name: name, Action: action}).Validate(); err != nil {
		return Response{}, err
	}
	db, err := c.openDB(ctx)
	if err != nil {
		return Response{}, err
	}
	defer db.Close()
	if id == "" {
		id, err = findProjectUUID(ctx, db, name)
		if err != nil {
			return Response{}, err
		}
	}
	return c.archiveItem(ctx, db, id, "project", action, 1)
}

func (c *Client) archiveItem(ctx context.Context, db *sql.DB, id, kind, action string, expectedType int) (Response, error) {
	if action == "" {
		action = "complete"
	}
	desiredStatus := 3
	if action == "cancel" {
		desiredStatus = 2
	}
	readState := func() (status, trashed int, err error) {
		var itemType int
		err = db.QueryRowContext(ctx, `SELECT type,status,trashed FROM TMTask WHERE uuid = ?`, id).Scan(&itemType, &status, &trashed)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, &OperationError{Code: kind + "_not_found"}
		}
		if err != nil {
			return 0, 0, fmt.Errorf("read Things archive state: %w", err)
		}
		if itemType != expectedType {
			return 0, 0, &OperationError{Code: "item_type_mismatch"}
		}
		if (status != 0 && status != 2 && status != 3) || (trashed != 0 && trashed != 1) {
			return 0, 0, &OperationError{Code: "archive_state_unsupported"}
		}
		return status, trashed, nil
	}
	matches := func(status, trashed int) bool {
		if action == "trash" {
			return trashed == 1
		}
		return status == desiredStatus && trashed == 0
	}
	status, trashed, err := readState()
	if err != nil {
		return Response{}, err
	}
	if matches(status, trashed) {
		return Response{OK: true, ID: id}, nil
	}
	if trashed != 0 {
		return Response{}, &OperationError{Code: kind + "_not_found"}
	}
	runner := c.commandRunner()
	_, _, runningErr := runner.Run(ctx, "/usr/bin/pgrep", []string{"-x", "Things3"})
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	if runningErr != nil {
		defer restoreThingsStoppedState(ctx, runner)
	}
	if _, _, err := runner.Run(ctx, "/usr/bin/osascript", []string{"-e", archiveScript, "--", kind, id, action}); err != nil {
		return Response{}, fmt.Errorf("archive Things %s: %w", kind, err)
	}
	deadline := c.verifyDeadline()
	for {
		status, trashed, err := readState()
		if err != nil {
			return Response{}, err
		}
		if matches(status, trashed) {
			return Response{OK: true, ID: id}, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return Response{}, &OperationError{Code: "archive_unverified"}
		}
		if err := waitForPoll(ctx, min(50*time.Millisecond, remaining)); err != nil {
			return Response{}, err
		}
	}
}

const archiveScript = `on run argv
    if (count of argv) is not 3 then error "Invalid archive arguments"
    set itemKind to item 1 of argv
    set itemID to item 2 of argv
    set archiveAction to item 3 of argv
    tell application "Things3"
        if itemKind is "task" then
            set selectedItem to to do id itemID
        else if itemKind is "project" then
            set selectedItem to project id itemID
        else
            error "Invalid archive kind"
        end if
        if archiveAction is "cancel" then
            set status of selectedItem to canceled
        else if archiveAction is "complete" then
            set status of selectedItem to completed
        else if archiveAction is "trash" and itemKind is "task" then
            move selectedItem to list "Trash"
        else
            error "Invalid archive action"
        end if
    end tell
end run
`
