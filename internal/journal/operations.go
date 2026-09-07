package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var ErrOperationNotFound = errors.New("write operation was not found")

type OperationState string

const (
	OperationReceived   OperationState = "received"
	OperationPrepared   OperationState = "prepared"
	OperationDispatched OperationState = "dispatched"
	OperationApplied    OperationState = "applied"
	OperationReported   OperationState = "reported"
)

type Operation struct {
	JobID       string
	PayloadHash string
	State       OperationState
	Plan        json.RawMessage
	Result      json.RawMessage
}

func (s *Store) configureOperations(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS operations (
		job_id TEXT PRIMARY KEY, payload_hash TEXT NOT NULL,
		state TEXT NOT NULL CHECK(state IN ('received','prepared','dispatched','applied','reported')),
		plan TEXT NOT NULL DEFAULT '{}', result TEXT NOT NULL DEFAULT '{}', updated_at INTEGER NOT NULL
	) STRICT`)
	if err != nil {
		return err
	}
	// One identifier belongs to one operation kind, even if a caller retries
	// a key with a changed request. Triggers make this check atomic across
	// separate worker/stdio processes sharing the same journal.
	for _, statement := range []string{
		`CREATE TRIGGER IF NOT EXISTS deliveries_unique_job BEFORE INSERT ON deliveries
		WHEN EXISTS(SELECT 1 FROM operations WHERE job_id=NEW.job_id)
		BEGIN SELECT RAISE(ABORT, 'journal job kind conflict'); END`,
		`CREATE TRIGGER IF NOT EXISTS operations_unique_job BEFORE INSERT ON operations
		WHEN EXISTS(SELECT 1 FROM deliveries WHERE job_id=NEW.job_id)
		BEGIN SELECT RAISE(ABORT, 'journal job kind conflict'); END`,
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) EnsureOperation(ctx context.Context, jobID, payloadHash string) (Operation, error) {
	if jobID == "" || payloadHash == "" {
		return Operation{}, errors.New("operation id and payload hash are required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO operations(job_id,payload_hash,state,updated_at)
		VALUES(?,?,'received',?) ON CONFLICT(job_id) DO NOTHING`, jobID, payloadHash, time.Now().UTC().UnixMilli())
	if err != nil {
		if err.Error() == "journal job kind conflict" {
			return Operation{}, ErrPayloadMismatch
		}
		return Operation{}, fmt.Errorf("record write operation: %w", err)
	}
	op, err := s.GetOperation(ctx, jobID)
	if err != nil {
		return Operation{}, err
	}
	if op.PayloadHash != payloadHash {
		return Operation{}, ErrPayloadMismatch
	}
	return op, nil
}

func (s *Store) GetOperation(ctx context.Context, jobID string) (Operation, error) {
	var op Operation
	var plan, result string
	err := s.db.QueryRowContext(ctx, `SELECT job_id,payload_hash,state,plan,result FROM operations WHERE job_id=?`, jobID).
		Scan(&op.JobID, &op.PayloadHash, &op.State, &plan, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return Operation{}, ErrOperationNotFound
	}
	if err != nil {
		return Operation{}, fmt.Errorf("read write operation: %w", err)
	}
	op.Plan, op.Result = json.RawMessage(plan), json.RawMessage(result)
	return op, nil
}

func (s *Store) PrepareOperation(ctx context.Context, jobID string, plan json.RawMessage) error {
	if !json.Valid(plan) {
		return errors.New("write plan must be valid JSON")
	}
	return s.operationTransition(ctx, jobID, OperationReceived, OperationPrepared, "plan", plan)
}

func (s *Store) MarkOperationDispatched(ctx context.Context, jobID string) error {
	return s.operationTransition(ctx, jobID, OperationPrepared, OperationDispatched, "", nil)
}

func (s *Store) CompleteOperation(ctx context.Context, jobID string, result json.RawMessage) error {
	if !json.Valid(result) {
		return errors.New("write result must be valid JSON")
	}
	return s.operationTransition(ctx, jobID, OperationDispatched, OperationApplied, "result", result)
}

func (s *Store) MarkOperationReported(ctx context.Context, jobID string) error {
	op, err := s.GetOperation(ctx, jobID)
	if err != nil {
		return err
	}
	if op.State == OperationReported {
		return nil
	}
	return s.operationTransition(ctx, jobID, OperationApplied, OperationReported, "", nil)
}

func (s *Store) operationTransition(ctx context.Context, jobID string, from, to OperationState, column string, value []byte) error {
	query := `UPDATE operations SET state=?,updated_at=?`
	args := []any{to, time.Now().UTC().UnixMilli()}
	if column != "" {
		if column != "plan" && column != "result" {
			return errors.New("invalid write journal column")
		}
		query += `, ` + column + `=?`
		args = append(args, string(value))
	}
	query += ` WHERE job_id=? AND state=?`
	args = append(args, jobID, from)
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("advance write operation: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("write operation cannot transition to %s", to)
	}
	return nil
}
