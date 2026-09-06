package helper

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nejmlabs/things-index/internal/capture"
)

// TaskUpdatePlan is an immutable, JSON-serializable intent. Persist it before
// dispatch; never prepare it again when retrying the same job. It contains no
// authorization token. Version changes require an explicit migration.
type TaskUpdatePlan struct {
	Version    int                        `json:"version"`
	ID         string                     `json:"id"`
	ReplaySafe bool                       `json:"replay_safe"`
	Title      *TaskUpdateTextChange      `json:"title,omitempty"`
	Notes      *TaskUpdateTextChange      `json:"notes,omitempty"`
	Tags       *TaskUpdateTagChange       `json:"tags,omitempty"`
	Checklist  *TaskUpdateChecklistChange `json:"checklist,omitempty"`
	Deadline   *TaskUpdateTextChange      `json:"deadline,omitempty"`
	Schedule   *TaskUpdateScheduleChange  `json:"schedule,omitempty"`
}

type TaskUpdateTextChange struct {
	Before string `json:"before"`
	After  string `json:"after"`
}

type TaskUpdateTag struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type TaskUpdateTagChange struct {
	Before []TaskUpdateTag `json:"before"`
	After  []TaskUpdateTag `json:"after"`
}

type TaskUpdateChecklistItem struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status int    `json:"status"`
}

type TaskUpdateChecklistChange struct {
	Before []TaskUpdateChecklistItem `json:"before"`
	Append []string                  `json:"append"`
}

// Packed startDate values are deliberately opaque. Public AppleScript reads
// supply calendar dates and Today membership; SQLite supplies buckets and
// snapshots for detecting conflicting edits.
type TaskUpdateScheduleState struct {
	Start           int      `json:"start"`
	Today           bool     `json:"today"` // Legacy raw index presence; only a conflict snapshot.
	TodayMembership *bool    `json:"today_membership,omitempty"`
	StartDate       *float64 `json:"start_date,omitempty"`
	StartBucket     *int64   `json:"start_bucket,omitempty"`
	ActivationDate  string   `json:"activation_date"`
}

type TaskUpdateScheduleChange struct {
	Before TaskUpdateScheduleState `json:"before"`
	When   string                  `json:"when"`
	Date   string                  `json:"date,omitempty"`
}

type taskUpdateState struct {
	title, notes, deadline string
	tags                   []TaskUpdateTag
	checklist              []TaskUpdateChecklistItem
	schedule               TaskUpdateScheduleState
}

func taskUpdateError(code, message string) error {
	return fmt.Errorf("%s: %w", message, &OperationError{Code: code})
}

func validUpdateText(value string, maximum int) bool {
	return utf8.ValidString(value) && len(value) <= maximum && !strings.ContainsRune(value, '\x00')
}

func (c *Client) updateToday() string {
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	location := c.Location
	if location == nil {
		location = time.Local
	}
	return now.In(location).Format("2006-01-02")
}

// PrepareTaskUpdate resolves the target once, validates the complete request,
// and snapshots exact values before any write. Append notes become replacement
// notes. Tag additions become replacement sets. Checklist rows retain their IDs
// and checked states, so checklist additions remain a single append dispatch.
func (c *Client) PrepareTaskUpdate(ctx context.Context, req capture.UpdateTaskRequest) (TaskUpdatePlan, error) {
	plan := TaskUpdatePlan{Version: 1, ReplaySafe: len(req.AddChecklist) == 0 && req.When != "evening"}
	if err := req.Validate(); err != nil {
		return plan, taskUpdateError("invalid_request", err.Error())
	}
	if !validUpdateText(req.NewTitle, capture.MaxTitleBytes) || !validUpdateText(req.Notes, capture.MaxNotesBytes) ||
		!validUpdateText(req.AppendNotes, capture.MaxNotesBytes) || strings.ContainsAny(req.NewTitle, "\r\n") ||
		(req.NewTitle != "" && strings.TrimSpace(req.NewTitle) == "") || (req.Notes != "" && req.AppendNotes != "") {
		return plan, taskUpdateError("invalid_request", "invalid title or notes update")
	}
	if req.NewTitle == "" && req.Notes == "" && req.AppendNotes == "" && req.When == "" && req.Deadline == "" && len(req.AddTags) == 0 && len(req.AddChecklist) == 0 {
		return plan, taskUpdateError("invalid_request", "at least one task change is required")
	}
	for _, list := range []struct {
		values       []string
		limit, bytes int
	}{{req.AddTags, capture.MaxTagCount, capture.MaxDestinationLen}, {req.AddChecklist, capture.MaxChecklistCount, capture.MaxTitleBytes}} {
		if len(list.values) > list.limit {
			return plan, taskUpdateError("invalid_request", "too many tag or checklist values")
		}
		for _, value := range list.values {
			if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\r\n") || !validUpdateText(value, list.bytes) {
				return plan, taskUpdateError("invalid_request", "tag and checklist values must be bounded nonempty single lines")
			}
		}
	}
	if req.Deadline != "" {
		if _, err := time.Parse("2006-01-02", req.Deadline); err != nil {
			return plan, taskUpdateError("invalid_request", "deadline must be YYYY-MM-DD")
		}
		plan.Deadline = &TaskUpdateTextChange{After: req.Deadline}
	}
	if req.When != "" {
		plan.Schedule = &TaskUpdateScheduleChange{When: req.When}
		switch req.When {
		case "today", "evening":
			plan.Schedule.Date = c.updateToday()
		case "anytime", "someday":
		default:
			if _, err := time.Parse("2006-01-02", req.When); err != nil {
				return plan, taskUpdateError("invalid_request", "when must be today, evening, anytime, someday, or YYYY-MM-DD")
			}
			plan.Schedule.Date = req.When
		}
	}
	if c.AuthToken == "" && (plan.Deadline != nil || len(req.AddTags) > 0 || len(req.AddChecklist) > 0 || (req.When != "" && req.When != "today")) {
		return plan, taskUpdateError("update_auth_required", "this task update requires the Things authorization token")
	}
	db, err := c.openDB(ctx)
	if err != nil {
		return plan, err
	}
	defer db.Close()
	plan.ID, err = resolveTaskUpdateTarget(ctx, db, req)
	if err != nil {
		return plan, err
	}
	if req.NewTitle != "" {
		plan.Title = &TaskUpdateTextChange{After: req.NewTitle}
	}
	if req.Notes != "" || req.AppendNotes != "" {
		plan.Notes = &TaskUpdateTextChange{After: req.Notes}
	}
	if len(req.AddTags) != 0 {
		plan.Tags = &TaskUpdateTagChange{}
	}
	if len(req.AddChecklist) != 0 {
		plan.Checklist = &TaskUpdateChecklistChange{Append: append([]string(nil), req.AddChecklist...)}
	}
	before, err := c.readTaskUpdateState(ctx, db, plan)
	if err != nil {
		return plan, err
	}
	if plan.Title != nil {
		plan.Title.Before = before.title
	}
	if plan.Notes != nil {
		plan.Notes.Before = before.notes
		if req.AppendNotes != "" {
			plan.Notes.After = before.notes
			if plan.Notes.After != "" {
				plan.Notes.After += "\n"
			}
			plan.Notes.After += req.AppendNotes
		}
		if !validUpdateText(plan.Notes.After, capture.MaxNotesBytes) {
			return plan, taskUpdateError("invalid_request", "resulting notes exceed the supported size")
		}
	}
	if plan.Tags != nil {
		plan.Tags.Before = before.tags
		plan.Tags.After, err = desiredTaskUpdateTags(ctx, db, before.tags, req.AddTags)
		if err != nil {
			return plan, err
		}
	}
	if plan.Checklist != nil {
		plan.Checklist.Before = before.checklist
		if len(before.checklist)+len(req.AddChecklist) > capture.MaxChecklistCount {
			return plan, taskUpdateError("invalid_request", "resulting checklist exceeds 100 items")
		}
	}
	if plan.Deadline != nil {
		plan.Deadline.Before = before.deadline
	}
	if plan.Schedule != nil {
		plan.Schedule.Before = before.schedule
	}
	return plan, nil
}

func resolveTaskUpdateTarget(ctx context.Context, db *sql.DB, req capture.UpdateTaskRequest) (string, error) {
	query := `SELECT uuid FROM TMTask WHERE type=0 AND status=0 AND trashed=0`
	var args []any
	if req.ID != "" {
		query += ` AND uuid=?`
		args = append(args, req.ID)
	} else {
		query += ` AND LOWER(title)=LOWER(?)`
		args = append(args, req.Title)
		if req.Project != "" {
			id, err := findProjectUUID(ctx, db, req.Project)
			if err != nil {
				return "", err
			}
			query += ` AND project=?`
			args = append(args, id)
		}
	}
	rows, err := db.QueryContext(ctx, query+` LIMIT 2`, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(ids) == 0 {
		return "", &OperationError{Code: "task_not_found"}
	}
	if len(ids) > 1 {
		return "", &OperationError{Code: "task_ambiguous"}
	}
	return ids[0], nil
}

func (c *Client) readTaskUpdateState(ctx context.Context, db *sql.DB, plan TaskUpdatePlan) (taskUpdateState, error) {
	var state taskUpdateState
	err := db.QueryRowContext(ctx, `SELECT COALESCE(title,''), COALESCE(notes,'') FROM TMTask WHERE uuid=? AND type=0 AND status=0 AND trashed=0`, plan.ID).Scan(&state.title, &state.notes)
	if errors.Is(err, sql.ErrNoRows) {
		return state, taskUpdateError("update_conflict", "task is no longer an open, nontrashed to-do")
	}
	if err != nil {
		return state, err
	}
	if plan.Tags != nil {
		rows, err := db.QueryContext(ctx, `SELECT g.uuid,g.title FROM TMTaskTag t LEFT JOIN TMTag g ON g.uuid=t.tags WHERE t.tasks=? ORDER BY g.uuid`, plan.ID)
		if err != nil {
			return state, err
		}
		state.tags = make([]TaskUpdateTag, 0)
		for rows.Next() {
			var tag TaskUpdateTag
			if err = rows.Scan(&tag.ID, &tag.Title); err != nil {
				break
			}
			state.tags = append(state.tags, tag)
		}
		rowErr := rows.Err()
		rows.Close()
		if err != nil {
			return state, err
		}
		if rowErr != nil {
			return state, rowErr
		}
	}
	if plan.Checklist != nil {
		rows, err := db.QueryContext(ctx, `SELECT uuid,title,status FROM TMChecklistItem WHERE task=? ORDER BY "index",uuid`, plan.ID)
		if err != nil {
			return state, fmt.Errorf("read checklist schema: %w", err)
		}
		state.checklist = make([]TaskUpdateChecklistItem, 0)
		for rows.Next() {
			var item TaskUpdateChecklistItem
			if err = rows.Scan(&item.ID, &item.Title, &item.Status); err != nil {
				break
			}
			state.checklist = append(state.checklist, item)
		}
		rowErr := rows.Err()
		rows.Close()
		if err != nil {
			return state, err
		}
		if rowErr != nil {
			return state, rowErr
		}
	}
	if plan.Schedule != nil || plan.Deadline != nil {
		repeating, err := taskUpdateRepeats(ctx, db, plan.ID)
		if err != nil {
			return state, fmt.Errorf("read recurrence schema: %w", err)
		}
		if repeating {
			return state, taskUpdateError("invalid_request", "Things does not support schedule or deadline updates on repeating to-dos")
		}
		if plan.Schedule != nil {
			var startDate sql.NullFloat64
			var bucket sql.NullInt64
			if err := db.QueryRowContext(ctx, `SELECT COALESCE(start,0),todayIndex IS NOT NULL,startDate,startBucket FROM TMTask WHERE uuid=?`, plan.ID).Scan(&state.schedule.Start, &state.schedule.Today, &startDate, &bucket); err != nil {
				return state, fmt.Errorf("read schedule schema: %w", err)
			}
			if startDate.Valid {
				state.schedule.StartDate = &startDate.Float64
			}
			if bucket.Valid {
				state.schedule.StartBucket = &bucket.Int64
			}
		}
		due, activation, err := c.readTaskUpdateDates(ctx, plan.ID)
		if err != nil {
			return state, err
		}
		state.deadline, state.schedule.ActivationDate = due, activation
		if needsTodayMembership(plan.Schedule, activation) {
			member, err := c.readTaskUpdateTodayMembership(ctx, plan.ID)
			if err != nil {
				return state, err
			}
			state.schedule.TodayMembership = &member
		}
	}
	return state, nil
}

// Things has used several recurrence representations. Only inspect whether
// recognized fields contain data; their private contents are not decoded.
func taskUpdateRepeats(ctx context.Context, db *sql.DB, id string) (bool, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(TMTask)`)
	if err != nil {
		return false, err
	}
	columns := make(map[string]bool)
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue sql.NullString
		if err = rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			break
		}
		columns[name] = true
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	if rowErr != nil {
		return false, rowErr
	}
	var predicates []string
	for _, name := range []string{"recurrenceRule", "rt1_recurrenceRule", "repeater"} {
		if columns[name] {
			predicates = append(predicates, `COALESCE(length("`+name+`"),0)>0`)
		}
	}
	if len(predicates) == 0 {
		return false, errors.New("no recognized recurrence representation")
	}
	// A linked template also identifies an instance of a repeating to-do.
	if columns["rt1_repeatingTemplate"] {
		predicates = append(predicates, `COALESCE(length("rt1_repeatingTemplate"),0)>0`)
	}
	var repeating bool
	err = db.QueryRowContext(ctx, `SELECT `+strings.Join(predicates, " OR ")+` FROM TMTask WHERE uuid=?`, id).Scan(&repeating)
	return repeating, err
}

func desiredTaskUpdateTags(ctx context.Context, db *sql.DB, before []TaskUpdateTag, requested []string) ([]TaskUpdateTag, error) {
	rows, err := db.QueryContext(ctx, `SELECT uuid,title FROM TMTag`)
	if err != nil {
		return nil, err
	}
	var catalogue []TaskUpdateTag
	for rows.Next() {
		var tag TaskUpdateTag
		if err = rows.Scan(&tag.ID, &tag.Title); err != nil {
			break
		}
		catalogue = append(catalogue, tag)
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if rowErr != nil {
		return nil, rowErr
	}
	after := append([]TaskUpdateTag{}, before...)
	for _, title := range requested {
		var found []TaskUpdateTag
		for _, tag := range catalogue {
			if strings.EqualFold(tag.Title, title) {
				found = append(found, tag)
			}
		}
		if len(found) != 1 {
			return nil, taskUpdateError("tag_unavailable", "requested tag is missing or ambiguous")
		}
		present := false
		for _, tag := range after {
			if tag.ID == found[0].ID {
				present = true
			}
		}
		if !present {
			after = append(after, found[0])
		}
	}
	for _, tag := range after {
		if strings.ContainsAny(tag.Title, ",\r\n") {
			return nil, taskUpdateError("invalid_request", "tag names containing commas or line breaks cannot be safely replaced through this update URL")
		}
	}
	if len(after) > capture.MaxTagCount {
		return nil, taskUpdateError("invalid_request", "resulting tag set exceeds the supported size")
	}
	sort.Slice(after, func(i, j int) bool { return after[i].ID < after[j].ID })
	return after, nil
}

func (c *Client) readTaskUpdateDates(ctx context.Context, id string) (string, string, error) {
	script := `on isoDate(d)
  if d is missing value then return ""
  set yy to year of d as integer
  set mm to month of d as integer
  set dd to day of d as integer
  return (yy as text) & "-" & (text -2 thru -1 of ("0" & (mm as text))) & "-" & (text -2 thru -1 of ("0" & (dd as text)))
end isoDate
tell application "Things3"
  set aTask to (to do id "` + escapeAppleScriptString(id) + `")
  set dueValue to due date of aTask
  set activationValue to activation date of aTask
end tell
return my isoDate(dueValue) & "|" & my isoDate(activationValue)`
	output, _, err := c.commandRunner().Run(ctx, "/usr/bin/osascript", []string{"-e", script})
	if err != nil {
		return "", "", fmt.Errorf("read public Things calendar properties: %w", err)
	}
	parts := strings.Split(strings.TrimSpace(string(output)), "|")
	if len(parts) != 2 {
		return "", "", taskUpdateError("update_unverified", "Things calendar response is not a pair of dates")
	}
	for _, value := range parts {
		if value != "" {
			if _, err := time.Parse("2006-01-02", value); err != nil {
				return "", "", taskUpdateError("update_unverified", "Things calendar response contains an invalid date")
			}
		}
	}
	return parts[0], parts[1], nil
}

func needsTodayMembership(change *TaskUpdateScheduleChange, activation string) bool {
	return change != nil && (change.When == "today" || change.When == "evening" || change.When == "anytime") && activation == ""
}

func (c *Client) readTaskUpdateTodayMembership(ctx context.Context, id string) (bool, error) {
	output, _, err := c.commandRunner().Run(ctx, "/usr/bin/osascript", []string{"-e", taskUpdateTodayMembershipScript, "--", id})
	if err != nil {
		return false, fmt.Errorf("read public Things Today membership: %w", err)
	}
	switch strings.TrimSpace(string(output)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, taskUpdateError("update_unverified", "Things Today membership response is not a boolean")
	}
}

const taskUpdateTodayMembershipScript = `on run argv
  if (count of argv) is not 1 then error "Invalid membership arguments"
  set requestedID to item 1 of argv
  tell application "Things3" to return requestedID is in (get id of every to do of list "Today")
end run`

func (c *Client) scheduleUpdateMatches(change *TaskUpdateScheduleChange, state TaskUpdateScheduleState, due string) bool {
	matchesDay := state.ActivationDate == change.Date ||
		(state.ActivationDate == "" && change.Date == c.updateToday() && state.TodayMembership != nil && *state.TodayMembership)
	switch change.When {
	case "evening":
		// Things' bundled model/demo data and its native eveningBucket property
		// identify bucket 1 as This Evening (0 is ordinary Today). This is a
		// schema dependency, not a packed-date decoder: unknown buckets never
		// prove success. Public dates and Today membership establish the day.
		return state.StartBucket != nil && *state.StartBucket == 1 && state.Start == 1 && matchesDay
	case "someday":
		return state.Start == 2 && state.ActivationDate == ""
	case "anytime":
		if state.Start != 1 || state.ActivationDate != "" || state.TodayMembership == nil {
			return false
		}
		if !*state.TodayMembership {
			return true
		}
		// With no activation date, manual Today placement would otherwise look
		// identical to Anytime. A due/overdue deadline may independently keep a
		// cleared task in Today, so only that documented case accepts membership.
		// Due is separate from the schedule snapshot: a combined deadline change
		// must not itself create a schedule conflict.
		if _, err := time.Parse("2006-01-02", due); err != nil {
			return false
		}
		return due <= c.updateToday()
	case "today":
		return state.StartBucket != nil && *state.StartBucket == 0 && state.Start == 1 && matchesDay
	default:
		// Explicit dates are verified through Things' public calendar property.
		// Future tasks/projects can have start=2 (the same raw value used by
		// Someday), so the private start enum cannot establish this outcome.
		return state.ActivationDate == change.Date
	}
}

func checklistUpdateMatches(change *TaskUpdateChecklistChange, current []TaskUpdateChecklistItem) bool {
	if len(current) != len(change.Before)+len(change.Append) {
		return false
	}
	if !reflect.DeepEqual(current[:len(change.Before)], change.Before) {
		return false
	}
	for i, title := range change.Append {
		if current[len(change.Before)+i].Title != title || current[len(change.Before)+i].Status != 0 {
			return false
		}
	}
	return true
}

func (c *Client) taskUpdateStatus(plan TaskUpdatePlan, state taskUpdateState) (bool, error) {
	done := true
	for _, pair := range []struct {
		change  *TaskUpdateTextChange
		current string
	}{{plan.Title, state.title}, {plan.Notes, state.notes}, {plan.Deadline, state.deadline}} {
		if pair.change == nil || pair.current == pair.change.After {
			continue
		}
		done = false
		if pair.current != pair.change.Before {
			return false, taskUpdateError("update_conflict", "task fields changed after this update was prepared")
		}
	}
	if plan.Tags != nil && !reflect.DeepEqual(state.tags, plan.Tags.After) {
		done = false
		if !reflect.DeepEqual(state.tags, plan.Tags.Before) {
			return false, taskUpdateError("update_conflict", "task tags changed after this update was prepared")
		}
	}
	if plan.Checklist != nil && !checklistUpdateMatches(plan.Checklist, state.checklist) {
		done = false
		if !reflect.DeepEqual(state.checklist, plan.Checklist.Before) {
			return false, taskUpdateError("update_conflict", "task checklist changed or only part of the append is visible")
		}
	}
	if plan.Schedule != nil && !c.scheduleUpdateMatches(plan.Schedule, state.schedule, state.deadline) {
		done = false
		if !reflect.DeepEqual(state.schedule, plan.Schedule.Before) {
			return false, taskUpdateError("update_conflict", "task schedule changed after this update was prepared")
		}
	}
	return done, nil
}

func validateTaskUpdatePlan(plan TaskUpdatePlan) error {
	invalid := func() error { return taskUpdateError("invalid_request", "invalid prepared task update") }
	if plan.Version != 1 || strings.TrimSpace(plan.ID) == "" || !validUpdateText(plan.ID, capture.MaxDestinationLen) || (plan.Title == nil && plan.Notes == nil && plan.Tags == nil && plan.Checklist == nil && plan.Deadline == nil && plan.Schedule == nil) {
		return invalid()
	}
	if plan.Title != nil && (strings.TrimSpace(plan.Title.After) == "" || strings.ContainsAny(plan.Title.After, "\r\n") || !validUpdateText(plan.Title.After, capture.MaxTitleBytes)) {
		return invalid()
	}
	if plan.Notes != nil && !validUpdateText(plan.Notes.After, capture.MaxNotesBytes) {
		return invalid()
	}
	if plan.Checklist != nil {
		if plan.ReplaySafe || len(plan.Checklist.Append) == 0 || len(plan.Checklist.Before)+len(plan.Checklist.Append) > capture.MaxChecklistCount {
			return invalid()
		}
		for _, title := range plan.Checklist.Append {
			if strings.TrimSpace(title) == "" || strings.ContainsAny(title, "\r\n") || !validUpdateText(title, capture.MaxTitleBytes) {
				return invalid()
			}
		}
	}
	validDate := func(value string) bool { _, err := time.Parse("2006-01-02", value); return err == nil }
	if plan.Deadline != nil && !validDate(plan.Deadline.After) {
		return invalid()
	}
	if plan.Schedule != nil {
		switch plan.Schedule.When {
		case "today", "evening":
			if !validDate(plan.Schedule.Date) || (plan.Schedule.When == "evening" && plan.ReplaySafe) {
				return invalid()
			}
		case "anytime", "someday":
			if plan.Schedule.Date != "" {
				return invalid()
			}
		default:
			if !validDate(plan.Schedule.When) || plan.Schedule.Date != plan.Schedule.When {
				return invalid()
			}
		}
	}
	if plan.Tags != nil && (len(plan.Tags.After) == 0 || len(plan.Tags.After) > capture.MaxTagCount) {
		return taskUpdateError("invalid_request", "invalid prepared task update")
	}
	return nil
}

// CheckTaskUpdate never dispatches. Use it to reconcile an interrupted operation
// before deciding whether the persisted plan permits another attempt.
func (c *Client) CheckTaskUpdate(ctx context.Context, plan TaskUpdatePlan) (bool, error) {
	if err := validateTaskUpdatePlan(plan); err != nil {
		return false, err
	}
	db, err := c.openDB(ctx)
	if err != nil {
		return false, err
	}
	defer db.Close()
	state, err := c.readTaskUpdateState(ctx, db, plan)
	if err != nil {
		return false, err
	}
	return c.taskUpdateStatus(plan, state)
}

// ApplyTaskUpdate writes only a previously prepared intent. Baseline checks are
// optimistic: Things' URL API has no atomic compare-and-swap, so another writer
// must not intentionally edit these same fields concurrently. A checklist plan
// is append-once; after an uncertain dispatch call CheckTaskUpdate, not Apply.
func (c *Client) ApplyTaskUpdate(ctx context.Context, plan TaskUpdatePlan) (Response, error) {
	if err := validateTaskUpdatePlan(plan); err != nil {
		return Response{}, err
	}
	db, err := c.openDB(ctx)
	if err != nil {
		return Response{}, err
	}
	defer db.Close()
	state, err := c.readTaskUpdateState(ctx, db, plan)
	if err != nil {
		return Response{}, err
	}
	done, err := c.taskUpdateStatus(plan, state)
	if err != nil {
		return Response{}, err
	}
	if done {
		return Response{OK: true, ID: plan.ID}, nil
	}
	if plan.Tags != nil {
		names := make([]string, 0, len(plan.Tags.After))
		for _, tag := range plan.Tags.After {
			names = append(names, tag.Title)
		}
		resolved, err := desiredTaskUpdateTags(ctx, db, nil, names)
		if err != nil || !reflect.DeepEqual(resolved, plan.Tags.After) {
			return Response{}, taskUpdateError("update_conflict", "prepared tag identities have changed")
		}
	}
	if plan.Schedule != nil && plan.Schedule.When == "evening" && plan.Schedule.Date != c.updateToday() {
		return Response{}, taskUpdateError("update_conflict", "prepared evening date has passed")
	}
	values := url.Values{"id": {plan.ID}, "reveal": {"false"}}
	if plan.Title != nil && state.title != plan.Title.After {
		values.Set("title", plan.Title.After)
	}
	if plan.Notes != nil && state.notes != plan.Notes.After {
		values.Set("notes", plan.Notes.After)
	}
	if plan.Tags != nil && !reflect.DeepEqual(state.tags, plan.Tags.After) {
		var names []string
		for _, tag := range plan.Tags.After {
			names = append(names, tag.Title)
		}
		values.Set("tags", strings.Join(names, ","))
	}
	if plan.Checklist != nil && !checklistUpdateMatches(plan.Checklist, state.checklist) {
		values.Set("append-checklist-items", strings.Join(plan.Checklist.Append, "\n"))
	}
	if plan.Deadline != nil && state.deadline != plan.Deadline.After {
		values.Set("deadline", plan.Deadline.After)
	}
	scheduleDue := state.deadline
	if plan.Deadline != nil {
		// Moving a deadline into the future can remove the reason an Anytime
		// task was allowed in Today. Include the requested start-date clear in
		// the same update when that resulting deadline requires Today absence.
		scheduleDue = plan.Deadline.After
	}
	if plan.Schedule != nil && (!c.scheduleUpdateMatches(plan.Schedule, state.schedule, state.deadline) || !c.scheduleUpdateMatches(plan.Schedule, state.schedule, scheduleDue)) {
		when := plan.Schedule.When
		if when == "anytime" {
			// The documented update URL clears a field with a present empty
			// parameter; "anytime" is not a documented update.when value.
			when = ""
		} else if when != "evening" && plan.Schedule.Date != "" {
			when = plan.Schedule.Date
		}
		values.Set("when", when)
	}
	runner := c.commandRunner()
	if c.AuthToken != "" {
		values.Set("auth-token", c.AuthToken)
		updateURL := "things:///update?" + strings.ReplaceAll(values.Encode(), "+", "%20")
		if _, _, err := runner.Run(ctx, "/usr/bin/open", []string{"-g", "-j", updateURL}); err != nil {
			return Response{}, fmt.Errorf("dispatch prepared Things update: %w", err)
		}
	} else {
		if values.Has("tags") || values.Has("append-checklist-items") || values.Has("deadline") || (values.Has("when") && (plan.Schedule == nil || plan.Schedule.When != "today")) {
			return Response{}, taskUpdateError("update_auth_required", "prepared update requires the Things authorization token")
		}
		var lines []string
		lines = append(lines, `set aTask to (to do id "`+escapeAppleScriptString(plan.ID)+`")`)
		if values.Has("title") {
			lines = append(lines, `set name of aTask to "`+escapeAppleScriptString(values.Get("title"))+`"`)
		}
		if values.Has("notes") {
			lines = append(lines, `set notes of aTask to "`+escapeAppleScriptString(values.Get("notes"))+`"`)
		}
		if values.Has("when") {
			date, _ := time.Parse("2006-01-02", plan.Schedule.Date)
			lines = append(lines, "set targetDate to current date", "set day of targetDate to 1", fmt.Sprintf("set year of targetDate to %d", date.Year()), fmt.Sprintf("set month of targetDate to %d", int(date.Month())), fmt.Sprintf("set day of targetDate to %d", date.Day()), "set time of targetDate to 0", "schedule aTask for targetDate")
		}
		script := "tell application \"Things3\"\n" + strings.Join(lines, "\n") + "\nend tell"
		if _, _, err := runner.Run(ctx, "/usr/bin/osascript", []string{"-e", script}); err != nil {
			return Response{}, fmt.Errorf("apply prepared Things update: %w", err)
		}
	}
	deadline := c.verifyDeadline()
	for {
		current, err := c.readTaskUpdateState(ctx, db, plan)
		if err != nil {
			return Response{}, err
		}
		done, _ := c.taskUpdateStatus(plan, current)
		if done {
			return Response{OK: true, ID: plan.ID}, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return Response{}, taskUpdateError("update_unverified", "exact requested task changes were not verified")
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

// UpdateTask is the convenience entry point for a new, single attempt. Durable
// worker retries must persist PrepareTaskUpdate's result and use Apply/Check.
func (c *Client) UpdateTask(ctx context.Context, req capture.UpdateTaskRequest) (Response, error) {
	plan, err := c.PrepareTaskUpdate(ctx, req)
	if err != nil {
		return Response{}, err
	}
	return c.ApplyTaskUpdate(ctx, plan)
}
