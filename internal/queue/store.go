package queue

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/nejmlabs/things-index/internal/capture"
)

type State string

const (
	StateQueued    State = "queued"
	StateLeased    State = "leased"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
)

type Job struct {
	ID          string
	Task        capture.Request
	State       State
	LeaseToken  string
	LeaseUntil  time.Time
	ThingsID    string
	Warnings    []string
	LastError   string
	Attempts    int
	CreatedAt   time.Time
	CompletedAt time.Time
}

type Store struct {
	db *sql.DB
}

type PruneResult struct {
	Succeeded int64
	Failed    int64
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("queue path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create queue directory: %w", err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, fmt.Errorf("open queue: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.configure(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) configure(ctx context.Context) error {
	statements := []string{
		`PRAGMA journal_mode = WAL`,
		`PRAGMA synchronous = FULL`,
		`PRAGMA busy_timeout = 5000`,
		`CREATE TABLE IF NOT EXISTS jobs (
			id TEXT PRIMARY KEY,
			payload TEXT NOT NULL,
			state TEXT NOT NULL CHECK (state IN ('queued','leased','succeeded','failed')),
			lease_token TEXT NOT NULL DEFAULT '',
			lease_until INTEGER NOT NULL DEFAULT 0,
			things_id TEXT NOT NULL DEFAULT '',
			warnings TEXT NOT NULL DEFAULT '[]',
			last_error TEXT NOT NULL DEFAULT '',
			attempts INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			completed_at INTEGER NOT NULL DEFAULT 0
		) STRICT`,
		`CREATE INDEX IF NOT EXISTS jobs_lease_order ON jobs (state, lease_until, created_at)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure queue: %w", err)
		}
	}
	return nil
}

func (s *Store) Enqueue(ctx context.Context, task capture.Request) (Job, error) {
	if err := task.Validate(); err != nil {
		return Job{}, err
	}
	payload, err := json.Marshal(task)
	if err != nil {
		return Job{}, fmt.Errorf("encode capture task: %w", err)
	}
	id, err := randomID()
	if err != nil {
		return Job{}, err
	}
	now := time.Now().UTC()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO jobs (id, payload, state, created_at)
		VALUES (?, ?, 'queued', ?)`, id, string(payload), now.UnixMilli())
	if err != nil {
		return Job{}, fmt.Errorf("enqueue capture task: %w", err)
	}
	return Job{ID: id, Task: task, State: StateQueued, CreatedAt: now}, nil
}

func (s *Store) Lease(ctx context.Context, now time.Time, duration time.Duration) (Job, bool, error) {
	if duration <= 0 {
		return Job{}, false, errors.New("lease duration must be positive")
	}
	token, err := randomID()
	if err != nil {
		return Job{}, false, err
	}
	leaseUntil := now.UTC().Add(duration)
	row := s.db.QueryRowContext(ctx, `
		UPDATE jobs
		SET state = 'leased', lease_token = ?, lease_until = ?, attempts = attempts + 1
		WHERE id = (
			SELECT id FROM jobs
			WHERE state = 'queued' OR (state = 'leased' AND lease_until <= ?)
			ORDER BY created_at
			LIMIT 1
		)
		RETURNING id, payload, state, lease_token, lease_until, things_id, warnings,
		          last_error, attempts, created_at, completed_at`,
		token, leaseUntil.UnixMilli(), now.UTC().UnixMilli())
	job, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, fmt.Errorf("lease capture task: %w", err)
	}
	return job, true, nil
}

func (s *Store) Complete(ctx context.Context, jobID, leaseToken, thingsID string, warnings []string) error {
	if jobID == "" || leaseToken == "" || thingsID == "" {
		return errors.New("job id, lease token, and Things id are required")
	}
	warningsJSON, err := json.Marshal(warnings)
	if err != nil {
		return fmt.Errorf("encode completion warnings: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE jobs
		SET state = 'succeeded', things_id = ?, warnings = ?, completed_at = ?,
		    lease_token = '', lease_until = 0, last_error = ''
		WHERE id = ? AND state = 'leased' AND lease_token = ?`,
		thingsID, string(warningsJSON), time.Now().UTC().UnixMilli(), jobID, leaseToken)
	if err != nil {
		return fmt.Errorf("complete capture task: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect capture completion: %w", err)
	}
	if affected == 1 {
		return nil
	}
	job, getErr := s.Get(ctx, jobID)
	if getErr == nil && job.State == StateSucceeded && job.ThingsID == thingsID {
		return nil
	}
	return errors.New("capture lease is no longer current")
}

func (s *Store) Fail(ctx context.Context, jobID, leaseToken, message string, retryable bool) error {
	state := StateFailed
	if retryable {
		state = StateQueued
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE jobs
		SET state = ?, last_error = ?, lease_token = '', lease_until = 0,
		    completed_at = CASE WHEN ? = 'failed' THEN ? ELSE 0 END
		WHERE id = ? AND state = 'leased' AND lease_token = ?`,
		state, message, state, time.Now().UTC().UnixMilli(), jobID, leaseToken)
	if err != nil {
		return fmt.Errorf("fail capture task: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect capture failure: %w", err)
	}
	if affected != 1 {
		return errors.New("capture lease is no longer current")
	}
	return nil
}

func (s *Store) Get(ctx context.Context, jobID string) (Job, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, payload, state, lease_token, lease_until, things_id, warnings,
		       last_error, attempts, created_at, completed_at
		FROM jobs WHERE id = ?`, jobID)
	job, err := scanJob(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, fmt.Errorf("capture job %q was not found", jobID)
		}
		return Job{}, fmt.Errorf("read capture job: %w", err)
	}
	return job, nil
}

// ListRecent returns newest-first jobs for the read-only status dashboard.
func (s *Store) ListRecent(ctx context.Context, limit int) ([]Job, error) {
	if limit < 1 || limit > 500 {
		return nil, errors.New("recent job limit must be between 1 and 500")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, payload, state, lease_token, lease_until, things_id, warnings,
		       last_error, attempts, created_at, completed_at
		FROM jobs
		ORDER BY created_at DESC, id DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list recent capture jobs: %w", err)
	}
	defer rows.Close()

	jobs := make([]Job, 0, limit)
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("scan recent capture job: %w", err)
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recent capture jobs: %w", err)
	}
	return jobs, nil
}

// PruneTerminal removes completed jobs older than their respective cutoffs.
// A zero cutoff disables pruning for that state. Queued and leased jobs are
// never removed.
func (s *Store) PruneTerminal(ctx context.Context, succeededBefore, failedBefore time.Time) (PruneResult, error) {
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PruneResult{}, fmt.Errorf("begin terminal job cleanup: %w", err)
	}
	defer transaction.Rollback()

	var pruned PruneResult
	if !succeededBefore.IsZero() {
		pruned.Succeeded, err = deleteTerminalBefore(ctx, transaction, StateSucceeded, succeededBefore)
		if err != nil {
			return PruneResult{}, err
		}
	}
	if !failedBefore.IsZero() {
		pruned.Failed, err = deleteTerminalBefore(ctx, transaction, StateFailed, failedBefore)
		if err != nil {
			return PruneResult{}, err
		}
	}
	if err := transaction.Commit(); err != nil {
		return PruneResult{}, fmt.Errorf("commit terminal job cleanup: %w", err)
	}
	return pruned, nil
}

func deleteTerminalBefore(ctx context.Context, transaction *sql.Tx, state State, before time.Time) (int64, error) {
	result, err := transaction.ExecContext(ctx, `
		DELETE FROM jobs
		WHERE state = ? AND completed_at > 0 AND completed_at < ?`,
		state, before.UTC().UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("prune %s capture jobs: %w", state, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("inspect pruned %s capture jobs: %w", state, err)
	}
	return count, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(row rowScanner) (Job, error) {
	var job Job
	var payload string
	var warnings string
	var leaseUntil int64
	var createdAt int64
	var completedAt int64
	if err := row.Scan(
		&job.ID, &payload, &job.State, &job.LeaseToken, &leaseUntil, &job.ThingsID,
		&warnings, &job.LastError, &job.Attempts, &createdAt, &completedAt,
	); err != nil {
		return Job{}, err
	}
	if err := json.Unmarshal([]byte(payload), &job.Task); err != nil {
		return Job{}, fmt.Errorf("decode queued task: %w", err)
	}
	if err := json.Unmarshal([]byte(warnings), &job.Warnings); err != nil {
		return Job{}, fmt.Errorf("decode queued warnings: %w", err)
	}
	if leaseUntil > 0 {
		job.LeaseUntil = time.UnixMilli(leaseUntil).UTC()
	}
	job.CreatedAt = time.UnixMilli(createdAt).UTC()
	if completedAt > 0 {
		job.CompletedAt = time.UnixMilli(completedAt).UTC()
	}
	return job, nil
}

func randomID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate identifier: %w", err)
	}
	return hex.EncodeToString(bytes[:]), nil
}
