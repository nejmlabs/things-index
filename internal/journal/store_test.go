package journal

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestJournalPersistsDeliveryState(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "journal.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	entry, created, err := store.Ensure(ctx, "job-1", "hash-1")
	if err != nil {
		t.Fatal(err)
	}
	if !created || entry.State != StateReceived {
		t.Fatalf("unexpected initial entry: %#v, created=%v", entry, created)
	}
	if err := store.MarkCreating(ctx, "job-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCreated(ctx, "job-1", "things-1", "final notes"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkFinalised(ctx, "job-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	entry, err = reopened.Get(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if entry.State != StateFinalised || entry.ThingsID != "things-1" || entry.Notes != "final notes" {
		t.Fatalf("unexpected persisted entry: %#v", entry)
	}
}

func TestJournalRejectsReusedJobWithDifferentPayload(t *testing.T) {
	t.Parallel()

	store, err := Open(filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, _, err := store.Ensure(ctx, "job-1", "hash-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Ensure(ctx, "job-1", "hash-2"); err == nil {
		t.Fatal("expected changed payload to be rejected")
	}
}

func TestPruneReportedRetainsIncompleteDeliveries(t *testing.T) {
	t.Parallel()

	store, err := Open(filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	if _, _, err := store.Ensure(ctx, "reported", "hash-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCreating(ctx, "reported"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCreated(ctx, "reported", "things-1", "notes"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkFinalised(ctx, "reported"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkReported(ctx, "reported"); err != nil {
		t.Fatal(err)
	}

	if _, _, err := store.Ensure(ctx, "incomplete", "hash-2"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCreating(ctx, "incomplete"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCreated(ctx, "incomplete", "things-2", "notes"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkFinalised(ctx, "incomplete"); err != nil {
		t.Fatal(err)
	}

	old := time.Now().Add(-48 * time.Hour).UnixMilli()
	if _, err := store.db.ExecContext(ctx, `UPDATE deliveries SET updated_at = ?`, old); err != nil {
		t.Fatal(err)
	}
	count, err := store.PruneReported(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("pruned %d deliveries, want 1", count)
	}
	if _, err := store.Get(ctx, "reported"); err == nil {
		t.Fatal("expired reported delivery was retained")
	}
	if _, err := store.Get(ctx, "incomplete"); err != nil {
		t.Fatalf("incomplete delivery was pruned: %v", err)
	}
}
