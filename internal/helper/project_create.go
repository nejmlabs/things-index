package helper

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/nejmlabs/things-index/internal/capture"
)

func (c *Client) CreateProject(ctx context.Context, req capture.CreateProjectRequest) (Response, error) {
	if err := req.Validate(); err != nil {
		return Response{}, taskUpdateError("invalid_request", err.Error())
	}
	db, err := c.openDB(ctx)
	if err != nil {
		return Response{}, err
	}
	defer db.Close()
	var tags []TaskUpdateTag
	if len(req.Tags) > 0 {
		tags, err = desiredTaskUpdateTags(ctx, db, nil, req.Tags)
		if err != nil {
			return Response{}, err
		}
		// The relation must also be readable before dispatch, not merely the
		// catalogue. Unsupported schemas cannot produce a verified result.
		rows, err := db.QueryContext(ctx, `SELECT tasks,tags FROM TMTaskTag LIMIT 0`)
		if err != nil {
			return Response{}, fmt.Errorf("read project tag schema: %w", err)
		}
		rows.Close()
	}
	var schedule *TaskUpdateScheduleChange
	if req.When != "" {
		schedule = &TaskUpdateScheduleChange{When: req.When}
		switch req.When {
		case "today", "evening":
			schedule.Date = c.updateToday()
		case "anytime", "someday":
		default:
			schedule.Date = req.When
		}
		rows, err := db.QueryContext(ctx, `SELECT start,todayIndex,startDate,startBucket FROM TMTask LIMIT 0`)
		if err != nil {
			return Response{}, fmt.Errorf("read project schedule schema: %w", err)
		}
		rows.Close()
	}
	rows, err := db.QueryContext(ctx, `SELECT title,notes FROM TMTask LIMIT 0`)
	if err != nil {
		return Response{}, fmt.Errorf("read project fields schema: %w", err)
	}
	rows.Close()

	areaUUID, err := projectCreationArea(ctx, db, req.Area)
	if err != nil {
		return Response{}, err
	}

	// A retry may reuse a project only in the requested area. Omitting the
	// area means an unfiled project, not a match from any area.
	existingUUID, err := projectCreationMatch(ctx, db, req.Title, areaUUID)
	if err != nil {
		return Response{}, err
	}
	runner := c.commandRunner()
	if existingUUID == "" || schedule != nil || req.Deadline != "" {
		_, _, runningErr := runner.Run(ctx, "/usr/bin/pgrep", []string{"-x", "Things3"})
		if runningErr != nil {
			defer func() {
				// Register before dispatch: a timed-out command may have opened Things.
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
				defer cancel()
				_, _, _ = runner.Run(cleanupCtx, "/usr/bin/osascript", []string{"-e", `tell application "Things3" to quit`})
			}()
		}
	}
	if existingUUID != "" {
		mismatches, err := c.projectCreationFields(ctx, db, existingUUID, areaUUID, req, tags, schedule, false)
		if err != nil {
			return Response{}, err
		}
		response := Response{OK: true, ID: existingUUID}
		if len(mismatches) != 0 {
			response.Warnings = []string{"Reused the existing project; requested " + strings.Join(mismatches, ", ") + " differ and were not applied."}
		}
		return response, nil
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
	if schedule != nil {
		when := schedule.When
		if schedule.Date != "" && when != "evening" {
			when = schedule.Date
		}
		values.Set("when", when)
	}
	if len(req.Tags) > 0 {
		var names []string
		for _, tag := range tags {
			names = append(names, tag.Title)
		}
		values.Set("tags", strings.Join(names, ","))
	}
	if c.AuthToken != "" {
		values.Set("auth-token", c.AuthToken)
	}
	addURL := "things:///add-project?" + strings.ReplaceAll(values.Encode(), "+", "%20")

	if _, _, err := runner.Run(ctx, "/usr/bin/open", []string{"-g", "-j", addURL}); err != nil {
		return Response{}, fmt.Errorf("dispatch add-project URL: %w", err)
	}

	// Confirm the new active project is in the resolved area, rather than
	// accepting a same-title row elsewhere or an archived project. The exact
	// match was absent before dispatch; creationDate's private encoding and
	// another device's clock cannot establish the identity of this write.
	deadline := c.verifyDeadline()
	for {
		projectUUID, err := projectCreationMatch(ctx, db, req.Title, areaUUID)
		if err != nil {
			return Response{}, err
		}
		if projectUUID != "" {
			mismatches, err := c.projectCreationFields(ctx, db, projectUUID, areaUUID, req, tags, schedule, true)
			if err != nil {
				return Response{}, err
			}
			if len(mismatches) == 0 {
				return Response{OK: true, ID: projectUUID}, nil
			}
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

// projectCreationFields verifies only fields explicitly requested by callers.
// Reusing a same-title project never silently mutates the existing project.
func (c *Client) projectCreationFields(ctx context.Context, db *sql.DB, id, area string, req capture.CreateProjectRequest, tags []TaskUpdateTag, schedule *TaskUpdateScheduleChange, newProject bool) ([]string, error) {
	var title, notes string
	err := db.QueryRowContext(ctx, `SELECT COALESCE(title,''),COALESCE(notes,'') FROM TMTask WHERE uuid=? AND type=1 AND status=0 AND trashed=0 AND COALESCE(area,'')=?`, id, area).Scan(&title, &notes)
	if err == sql.ErrNoRows {
		if !newProject {
			return nil, taskUpdateError("update_conflict", "existing project changed placement or active status during verification")
		}
		return []string{"placement or active status"}, nil
	}
	if err != nil {
		return nil, err
	}
	var mismatches []string
	if (newProject && title != req.Title) || (!newProject && !strings.EqualFold(title, req.Title)) {
		mismatches = append(mismatches, "title")
	}
	if (newProject || req.Notes != "") && notes != req.Notes {
		mismatches = append(mismatches, "notes")
	}
	if len(req.Tags) != 0 {
		rows, err := db.QueryContext(ctx, `SELECT g.uuid,g.title FROM TMTaskTag t LEFT JOIN TMTag g ON g.uuid=t.tags WHERE t.tasks=? ORDER BY g.uuid`, id)
		if err != nil {
			return nil, err
		}
		actual := make([]TaskUpdateTag, 0)
		for rows.Next() {
			var tag TaskUpdateTag
			if err = rows.Scan(&tag.ID, &tag.Title); err != nil {
				break
			}
			actual = append(actual, tag)
		}
		rowErr := rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if rowErr != nil {
			return nil, rowErr
		}
		if !reflect.DeepEqual(actual, tags) {
			mismatches = append(mismatches, "tags")
		}
	}
	if schedule != nil || req.Deadline != "" {
		due, activation, err := c.readTaskUpdateDates(ctx, id)
		if err != nil {
			return nil, err
		}
		if req.Deadline != "" && due != req.Deadline {
			mismatches = append(mismatches, "deadline")
		}
		if schedule != nil {
			state := TaskUpdateScheduleState{ActivationDate: activation}
			if needsTodayMembership(schedule, activation) {
				inToday, err := c.readTaskUpdateTodayMembership(ctx, id)
				if err != nil {
					return nil, err
				}
				state.TodayMembership = &inToday
			}
			var startDate sql.NullFloat64
			var bucket sql.NullInt64
			if err := db.QueryRowContext(ctx, `SELECT COALESCE(start,0),todayIndex IS NOT NULL,startDate,startBucket FROM TMTask WHERE uuid=?`, id).Scan(&state.Start, &state.Today, &startDate, &bucket); err != nil {
				return nil, err
			}
			if startDate.Valid {
				state.StartDate = &startDate.Float64
			}
			if bucket.Valid {
				state.StartBucket = &bucket.Int64
			}
			if !c.scheduleUpdateMatches(schedule, state, due) {
				mismatches = append(mismatches, "schedule")
			}
		}
	}
	return mismatches, nil
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

func projectCreationMatch(ctx context.Context, db *sql.DB, title, areaUUID string) (string, error) {
	query := `SELECT uuid FROM TMTask
		WHERE type = 1 AND LOWER(title) = LOWER(?) AND COALESCE(area, '') = ?
		AND trashed = 0 AND status = 0`
	args := []any{title, areaUUID}
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
