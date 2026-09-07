package journal

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestOperationPersistsPlanAndResultAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "journal.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { store.Close() }()
	op, err := store.EnsureOperation(ctx, "job", "hash")
	if err != nil || op.State != OperationReceived {
		t.Fatalf("unexpected initial operation: %+v %v", op, err)
	}
	if err := store.MarkOperationDispatched(ctx, "job"); err == nil {
		t.Fatal("dispatch before preparation was accepted")
	}
	if err := store.PrepareOperation(ctx, "job", json.RawMessage(`{`)); err == nil {
		t.Fatal("invalid plan was accepted")
	}
	plan := json.RawMessage(`{"id":"resolved-task","notes":{"before":"Old","after":"Old\nNew"}}`)
	if err := store.PrepareOperation(ctx, "job", plan); err != nil {
		t.Fatal(err)
	}
	if err := store.PrepareOperation(ctx, "job", json.RawMessage(`{"id":"different"}`)); err == nil {
		t.Fatal("prepared intent was overwritten")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	op, err = store.EnsureOperation(ctx, "job", "hash")
	if err != nil || op.State != OperationPrepared || string(op.Plan) != string(plan) {
		t.Fatalf("restart lost prepared intent: %+v %v", op, err)
	}
	if _, err := store.EnsureOperation(ctx, "job", "changed hash"); !errors.Is(err, ErrPayloadMismatch) {
		t.Fatalf("changed operation payload was accepted: %v", err)
	}
	if err := store.MarkOperationReported(ctx, "job"); err == nil {
		t.Fatal("unapplied operation was reported")
	}
	if err := store.MarkOperationDispatched(ctx, "job"); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteOperation(ctx, "job", json.RawMessage(`invalid`)); err == nil {
		t.Fatal("invalid completion result was accepted")
	}
	result := json.RawMessage(`{"thingsId":"resolved-task","warnings":["saved warning"]}`)
	if err := store.CompleteOperation(ctx, "job", result); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkOperationDispatched(ctx, "job"); err == nil {
		t.Fatal("applied operation returned to dispatch")
	}
	for range 2 {
		if err := store.MarkOperationReported(ctx, "job"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	op, err = store.GetOperation(ctx, "job")
	if err != nil || op.State != OperationReported || string(op.Result) != string(result) || string(op.Plan) != string(plan) {
		t.Fatalf("restart lost completed outcome: %+v %v", op, err)
	}
}

func TestJournalRejectsJobIDReusedAcrossOperationKinds(t *testing.T) {
	t.Parallel()
	for _, operationFirst := range []bool{false, true} {
		store, err := Open(filepath.Join(t.TempDir(), "journal.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		ctx := context.Background()
		if operationFirst {
			if _, err := store.EnsureOperation(ctx, "same-job", "same-hash"); err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.Ensure(ctx, "same-job", "same-hash"); !errors.Is(err, ErrPayloadMismatch) {
				t.Fatalf("capture reused a mutation key: %v", err)
			}
		} else {
			if _, _, err := store.Ensure(ctx, "same-job", "same-hash"); err != nil {
				t.Fatal(err)
			}
			if _, err := store.EnsureOperation(ctx, "same-job", "same-hash"); !errors.Is(err, ErrPayloadMismatch) {
				t.Fatalf("mutation reused a capture key: %v", err)
			}
		}
	}
}

func TestPruneOperationsRetainsUnacknowledgedWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, state := range []OperationState{OperationReceived, OperationPrepared, OperationDispatched, OperationApplied, OperationReported} {
		id := string(state)
		if _, err := store.EnsureOperation(ctx, id, "hash"); err != nil {
			t.Fatal(err)
		}
		if state == OperationReceived {
			continue
		}
		if err := store.PrepareOperation(ctx, id, json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		if state == OperationPrepared {
			continue
		}
		if err := store.MarkOperationDispatched(ctx, id); err != nil {
			t.Fatal(err)
		}
		if state == OperationDispatched {
			continue
		}
		if err := store.CompleteOperation(ctx, id, json.RawMessage(`{"thingsId":"created"}`)); err != nil {
			t.Fatal(err)
		}
		if state == OperationReported {
			if err := store.MarkOperationReported(ctx, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE operations SET updated_at=?`, time.Now().Add(-48*time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	count, err := store.PruneReported(ctx, time.Now().Add(-24*time.Hour))
	if err != nil || count != 1 {
		t.Fatalf("unexpected pruning: count=%d error=%v", count, err)
	}
	if _, err := store.GetOperation(ctx, string(OperationReported)); !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("acknowledged operation was retained: %v", err)
	}
	for _, state := range []OperationState{OperationReceived, OperationPrepared, OperationDispatched, OperationApplied} {
		op, err := store.GetOperation(ctx, string(state))
		if err != nil || op.State != state {
			t.Fatalf("unacknowledged operation was pruned: state=%s error=%v", state, err)
		}
	}
}
