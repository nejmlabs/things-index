package journal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var ErrPayloadMismatch = errors.New("the server reused a job identifier with different task content")

type State string

const (
	StateReceived  State = "received"
	StateCreating  State = "creating"
	StateCreated   State = "created"
	StateFinalised State = "finalised"
	StateReported  State = "reported"
)

type Entry struct {
	JobID       string
	PayloadHash string
	State       State
	ThingsID    string
	Notes       string
	UpdatedAt   time.Time
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("journal path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create journal directory: %w", err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, fmt.Errorf("open journal: %w", err)
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
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE IF NOT EXISTS deliveries (
			job_id TEXT PRIMARY KEY,
			payload_hash TEXT NOT NULL,
			state TEXT NOT NULL CHECK (state IN ('received','creating','created','finalised','reported')),
			things_id TEXT NOT NULL DEFAULT '',
			final_notes TEXT NOT NULL DEFAULT '',
			updated_at INTEGER NOT NULL
		) STRICT`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure journal: %w", err)
		}
	}
	return nil
}

func (s *Store) Ensure(ctx context.Context, jobID, payloadHash string) (Entry, bool, error) {
	if jobID == "" || payloadHash == "" {
		return Entry{}, false, errors.New("job id and payload hash are required")
	}
	now := time.Now().UTC().UnixMilli()
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO deliveries (job_id, payload_hash, state, updated_at)
		VALUES (?, ?, 'received', ?)
		ON CONFLICT(job_id) DO NOTHING`, jobID, payloadHash, now)
	if err != nil {
		return Entry{}, false, fmt.Errorf("record delivery: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return Entry{}, false, fmt.Errorf("inspect delivery insert: %w", err)
	}
	entry, err := s.Get(ctx, jobID)
	if err != nil {
		return Entry{}, false, err
	}
	if entry.PayloadHash != payloadHash {
		return Entry{}, false, ErrPayloadMismatch
	}
	return entry, affected == 1, nil
}

func (s *Store) Get(ctx context.Context, jobID string) (Entry, error) {
	var entry Entry
	var updatedAt int64
	err := s.db.QueryRowContext(ctx, `
		SELECT job_id, payload_hash, state, things_id, final_notes, updated_at
		FROM deliveries WHERE job_id = ?`, jobID).Scan(
		&entry.JobID, &entry.PayloadHash, &entry.State, &entry.ThingsID, &entry.Notes, &updatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Entry{}, fmt.Errorf("delivery %q was not found", jobID)
		}
		return Entry{}, fmt.Errorf("read delivery: %w", err)
	}
	entry.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	return entry, nil
}

// PruneReported removes delivery records that the server has already
// acknowledged. Incomplete records are retained for crash recovery.
func (s *Store) PruneReported(ctx context.Context, before time.Time) (int64, error) {
	if before.IsZero() {
		return 0, nil
	}
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM deliveries
		WHERE state = 'reported' AND updated_at < ?`, before.UTC().UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("prune reported deliveries: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("inspect pruned reported deliveries: %w", err)
	}
	return count, nil
}

func (s *Store) MarkCreating(ctx context.Context, jobID string) error {
	return s.transition(ctx, jobID, []State{StateReceived}, StateCreating, "", "")
}

func (s *Store) MarkCreated(ctx context.Context, jobID, thingsID, finalNotes string) error {
	if thingsID == "" {
		return errors.New("Things identifier is required")
	}
	return s.transition(ctx, jobID, []State{StateCreating}, StateCreated, thingsID, finalNotes)
}

func (s *Store) MarkFinalised(ctx context.Context, jobID string) error {
	return s.transition(ctx, jobID, []State{StateCreated}, StateFinalised, "", "")
}

func (s *Store) MarkReported(ctx context.Context, jobID string) error {
	return s.transition(ctx, jobID, []State{StateFinalised, StateReported}, StateReported, "", "")
}

func (s *Store) transition(
	ctx context.Context,
	jobID string,
	from []State,
	to State,
	thingsID string,
	finalNotes string,
) error {
	if len(from) == 0 {
		return errors.New("at least one source state is required")
	}
	query := `UPDATE deliveries SET state = ?, updated_at = ?`
	args := []any{to, time.Now().UTC().UnixMilli()}
	if thingsID != "" {
		query += `, things_id = ?`
		args = append(args, thingsID)
	}
	if finalNotes != "" {
		query += `, final_notes = ?`
		args = append(args, finalNotes)
	}
	query += ` WHERE job_id = ? AND state IN (`
	args = append(args, jobID)
	for index, state := range from {
		if index > 0 {
			query += `,`
		}
		query += `?`
		args = append(args, state)
	}
	query += `)`
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("advance delivery to %s: %w", to, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect delivery transition: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("delivery %q cannot transition to %s", jobID, to)
	}
	return nil
}
