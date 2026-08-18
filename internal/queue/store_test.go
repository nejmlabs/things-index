package queue

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nejmlabs/things-index/internal/capture"
)

func TestQueueLeaseAndComplete(t *testing.T) {
	t.Parallel()

	store, err := Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	queued, err := store.Enqueue(ctx, capture.Request{Title: "Buy milk"})
	if err != nil {
		t.Fatal(err)
	}
	leased, ok, err := store.Lease(ctx, time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || leased.ID != queued.ID || leased.LeaseToken == "" {
		t.Fatalf("unexpected lease %#v, ok=%v", leased, ok)
	}
	if err := store.Complete(ctx, leased.ID, leased.LeaseToken, "things-1", []string{"warning"}); err != nil {
		t.Fatal(err)
	}
	completed, err := store.Get(ctx, queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != StateSucceeded || completed.ThingsID != "things-1" || len(completed.Warnings) != 1 {
		t.Fatalf("unexpected completed job %#v", completed)
	}
}

func TestQueueReleasesExpiredLease(t *testing.T) {
	t.Parallel()

	store, err := Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err := store.Enqueue(ctx, capture.Request{Title: "Buy milk"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	first, ok, err := store.Lease(ctx, now, time.Second)
	if err != nil || !ok {
		t.Fatalf("first lease failed: %#v, %v", first, err)
	}
	second, ok, err := store.Lease(ctx, now.Add(2*time.Second), time.Minute)
	if err != nil || !ok {
		t.Fatalf("second lease failed: %#v, %v", second, err)
	}
	if first.ID != second.ID || first.LeaseToken == second.LeaseToken {
		t.Fatalf("job was not safely re-leased: %#v %#v", first, second)
	}
}

func TestPruneTerminalRemovesOnlyExpiredTerminalJobs(t *testing.T) {
	t.Parallel()

	store, err := Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	succeeded, err := store.Enqueue(ctx, capture.Request{Title: "Succeeded"})
	if err != nil {
		t.Fatal(err)
	}
	succeededLease, ok, err := store.Lease(ctx, time.Now(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("lease succeeded job: ok=%v err=%v", ok, err)
	}
	if err := store.Complete(ctx, succeededLease.ID, succeededLease.LeaseToken, "things-1", nil); err != nil {
		t.Fatal(err)
	}

	failed, err := store.Enqueue(ctx, capture.Request{Title: "Failed"})
	if err != nil {
		t.Fatal(err)
	}
	failedLease, ok, err := store.Lease(ctx, time.Now(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("lease failed job: ok=%v err=%v", ok, err)
	}
	if err := store.Fail(ctx, failedLease.ID, failedLease.LeaseToken, "permanent failure", false); err != nil {
		t.Fatal(err)
	}

	queued, err := store.Enqueue(ctx, capture.Request{Title: "Still queued"})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour).UnixMilli()
	if _, err := store.db.ExecContext(ctx,
		`UPDATE jobs SET completed_at = ? WHERE id IN (?, ?)`, old, succeeded.ID, failed.ID,
	); err != nil {
		t.Fatal(err)
	}

	pruned, err := store.PruneTerminal(ctx, time.Now().Add(-24*time.Hour), time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if pruned.Succeeded != 1 || pruned.Failed != 1 {
		t.Fatalf("unexpected prune result: %#v", pruned)
	}
	if _, err := store.Get(ctx, succeeded.ID); err == nil {
		t.Fatal("expired succeeded job was retained")
	}
	if _, err := store.Get(ctx, failed.ID); err == nil {
		t.Fatal("expired failed job was retained")
	}
	if _, err := store.Get(ctx, queued.ID); err != nil {
		t.Fatalf("queued job was pruned: %v", err)
	}
}

func TestListRecentOrdersAndLimitsJobs(t *testing.T) {
	t.Parallel()

	store, err := Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	var jobs []Job
	for _, title := range []string{"Oldest", "Middle", "Newest"} {
		job, err := store.Enqueue(ctx, capture.Request{Title: title})
		if err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, job)
	}
	for index, job := range jobs {
		if _, err := store.db.ExecContext(ctx,
			`UPDATE jobs SET created_at = ? WHERE id = ?`, int64(index+1), job.ID,
		); err != nil {
			t.Fatal(err)
		}
	}

	recent, err := store.ListRecent(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 2 || recent[0].ID != jobs[2].ID || recent[1].ID != jobs[1].ID {
		t.Fatalf("unexpected recent jobs: %#v", recent)
	}
	if _, err := store.ListRecent(ctx, 0); err == nil {
		t.Fatal("invalid recent job limit was accepted")
	}
}

func TestQueueIdempotency(t *testing.T) {
	t.Parallel()

	store, err := Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	first, err := store.Enqueue(ctx, capture.Request{Title: "Buy milk", IdempotencyKey: "key-123"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Enqueue(ctx, capture.Request{Title: "Buy milk", IdempotencyKey: "key-123"})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("expected identical job ID %q, got %q", first.ID, second.ID)
	}

	_, err = store.Enqueue(ctx, capture.Request{Title: "Buy tea", IdempotencyKey: "key-123"})
	if err == nil {
		t.Fatal("expected conflicting payload with same idempotency key to be rejected")
	}
}

func TestQueueExpireLeasesCapsMaxAttempts(t *testing.T) {
	t.Parallel()

	store, err := Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	job, err := store.Enqueue(ctx, capture.Request{Title: "Crash-prone task"})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	// Simulate 3 expired attempts with maxAttempts = 3
	for i := 0; i < 3; i++ {
		leased, ok, err := store.Lease(ctx, now, time.Second)
		if err != nil || !ok {
			t.Fatalf("lease attempt %d failed: ok=%v err=%v", i, ok, err)
		}
		if leased.ID != job.ID {
			t.Fatalf("unexpected leased ID: %q", leased.ID)
		}
		now = now.Add(2 * time.Second)
	}

	expiredCount, err := store.ExpireLeases(ctx, now, 3)
	if err != nil {
		t.Fatal(err)
	}
	if expiredCount != 1 {
		t.Fatalf("expiredCount = %d, want 1", expiredCount)
	}

	failedJob, err := store.Get(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failedJob.State != StateFailed || failedJob.LastError == "" {
		t.Fatalf("expected job to be in failed state: %#v", failedJob)
	}

	// Should no longer be leaseable
	_, ok, err := store.Lease(ctx, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("failed job should not be leaseable")
	}
}
