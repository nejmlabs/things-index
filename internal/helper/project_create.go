package helper

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/nejmlabs/things-index/internal/capture"
)

func (c *Client) CreateProject(ctx context.Context, req capture.CreateProjectRequest) (Response, error) {
	if strings.TrimSpace(req.Title) == "" {
		return Response{}, errors.New("project title is required")
	}
	db, err := c.openDB(ctx)
	if err != nil {
		return Response{}, err
	}
	defer db.Close()

	areaUUID, err := projectCreationArea(ctx, db, req.Area)
	if err != nil {
		return Response{}, err
	}

	// A retry may reuse a project only in the requested area. Omitting the
	// area means an unfiled project, not a match from any area.
	existingUUID, err := projectCreationMatch(ctx, db, req.Title, areaUUID, nil)
	if err != nil {
		return Response{}, err
	}
	if existingUUID != "" {
		return Response{OK: true, ID: existingUUID}, nil
	}

	values := url.Values{"title": {req.Title}, "reveal": {"false"}}
	if areaUUID != "" {
		values.Set("area-id", areaUUID)
	}
	if req.Notes != "" {
		values.Set("notes", req.Notes)
	}
	if req.Deadline != "" {
		values.Set("deadline", req.Deadline)
	}
	if req.When != "" {
		values.Set("when", req.When)
	}
	if len(req.Tags) > 0 {
		values.Set("tags", strings.Join(req.Tags, ","))
	}
	if c.AuthToken != "" {
		values.Set("auth-token", c.AuthToken)
	}
	addURL := "things:///add-project?" + strings.ReplaceAll(values.Encode(), "+", "%20")

	runner := c.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	_, _, runningErr := runner.Run(ctx, "/usr/bin/pgrep", []string{"-x", "Things3"})
	minCreationDate := macEpochSeconds(time.Now().Add(-time.Second))
	if _, _, err := runner.Run(ctx, "/usr/bin/open", []string{"-g", "-j", addURL}); err != nil {
		return Response{}, fmt.Errorf("dispatch add-project URL: %w", err)
	}
	if runningErr != nil {
		defer func() {
			// Cleanup still runs if verification fails or the request is canceled.
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer cancel()
			_, _, _ = runner.Run(cleanupCtx, "/usr/bin/osascript", []string{"-e", `tell application "Things3" to quit`})
		}()
	}

	// Confirm the new active project is in the resolved area, rather than
	// accepting a same-title row elsewhere or an archived project.
	deadline := c.verifyDeadline()
	for {
		projectUUID, err := projectCreationMatch(ctx, db, req.Title, areaUUID, &minCreationDate)
		if err != nil {
			return Response{}, err
		}
		if projectUUID != "" {
			return Response{OK: true, ID: projectUUID}, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return Response{}, &OperationError{Code: "create_failed"}
		}
		timer := time.NewTimer(min(100*time.Millisecond, remaining))
		select {
		case <-ctx.Done():
			timer.Stop()
			return Response{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func projectCreationArea(ctx context.Context, db *sql.DB, area string) (string, error) {
	if area == "" {
		return "", nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT uuid FROM TMArea
		WHERE LOWER(title) = LOWER(?) AND (visible IS NULL OR visible != 0)
		LIMIT 2`, area)
	if err != nil {
		return "", fmt.Errorf("query project area: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", fmt.Errorf("read project area: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate project areas: %w", err)
	}
	switch len(ids) {
	case 0:
		return "", fmt.Errorf("project area %q: %w", area, &OperationError{Code: "destination_not_found"})
	case 1:
		return ids[0], nil
	default:
		return "", fmt.Errorf("project area %q: %w", area, &OperationError{Code: "destination_ambiguous"})
	}
}

func projectCreationMatch(ctx context.Context, db *sql.DB, title, areaUUID string, minCreationDate *float64) (string, error) {
	query := `SELECT uuid FROM TMTask
		WHERE type = 1 AND LOWER(title) = LOWER(?) AND COALESCE(area, '') = ?
		AND trashed = 0 AND status = 0`
	args := []any{title, areaUUID}
	if minCreationDate != nil {
		query += " AND creationDate >= ?"
		args = append(args, *minCreationDate)
	}
	rows, err := db.QueryContext(ctx, query+" LIMIT 2", args...)
	if err != nil {
		return "", fmt.Errorf("query project in requested area: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", fmt.Errorf("read project in requested area: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate projects in requested area: %w", err)
	}
	if len(ids) > 1 {
		return "", fmt.Errorf("project %q in requested area: %w", title, &OperationError{Code: "destination_ambiguous"})
	}
	if len(ids) == 1 {
		return ids[0], nil
	}
	return "", nil
}
