package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nejmlabs/things-index/internal/capture"
	"github.com/nejmlabs/things-index/internal/queue"
)

const testDashboardToken = "dashboard-token-000000000000000000"

func TestDashboardIsOptionalAndSeparatelyAuthenticated(t *testing.T) {
	store, err := queue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	disabled, err := NewHandler(store, Config{PublicToken: testPublicToken, WorkerToken: testWorkerToken})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	response := httptest.NewRecorder()
	disabled.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("disabled dashboard returned status %d", response.Code)
	}

	enabled, err := NewHandler(store, Config{
		PublicToken: testPublicToken, WorkerToken: testWorkerToken, DashboardToken: testDashboardToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	response = httptest.NewRecorder()
	enabled.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated dashboard returned status %d", response.Code)
	}
	if !strings.HasPrefix(response.Header().Get("WWW-Authenticate"), "Basic ") {
		t.Fatal("dashboard did not request Basic authentication")
	}

	request = httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	request.SetBasicAuth(dashboardUsername, testDashboardToken)
	response = httptest.NewRecorder()
	enabled.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("authenticated dashboard returned status %d", response.Code)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("dashboard response may be cached")
	}
	if response.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("dashboard omitted its content security policy")
	}
}

func TestDashboardRendersAuthoritativeJobStates(t *testing.T) {
	store, err := queue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	succeeded := enqueueAndLease(t, ctx, store, "Confirmed task")
	if err := store.Complete(ctx, succeeded.ID, succeeded.LeaseToken, "things-confirmed", nil); err != nil {
		t.Fatal(err)
	}
	failed := enqueueAndLease(t, ctx, store, "Failed task")
	if err := store.Fail(ctx, failed.ID, failed.LeaseToken, "Things rejected the task", false); err != nil {
		t.Fatal(err)
	}
	enqueueAndLease(t, ctx, store, "Processing task")
	retrying := enqueueAndLease(t, ctx, store, "Retrying task")
	if err := store.Fail(ctx, retrying.ID, retrying.LeaseToken, "temporary failure", true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Enqueue(ctx, capture.Request{Title: "Pending task"}); err != nil {
		t.Fatal(err)
	}

	handler, err := NewHandler(store, Config{
		PublicToken: testPublicToken, WorkerToken: testWorkerToken, DashboardToken: testDashboardToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	request.SetBasicAuth(dashboardUsername, testDashboardToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	body, err := io.ReadAll(response.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)
	for _, expected := range []string{
		"Confirmed task", "Confirmed in Things",
		"Failed task", "Failed",
		"Retrying task", "Retrying",
		"Processing task", "Processing on Mac",
		"Pending task", "Pending",
	} {
		if !strings.Contains(page, expected) {
			t.Fatalf("dashboard omitted %q", expected)
		}
	}
}

func TestDashboardRejectsWeakOrSharedToken(t *testing.T) {
	store, err := queue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := NewHandler(store, Config{
		PublicToken: testPublicToken, WorkerToken: testWorkerToken, DashboardToken: "short",
	}); err == nil {
		t.Fatal("weak dashboard token was accepted")
	}
	if _, err := NewHandler(store, Config{
		PublicToken: testPublicToken, WorkerToken: testWorkerToken, DashboardToken: testPublicToken,
	}); err == nil {
		t.Fatal("shared dashboard token was accepted")
	}
}

func TestDashboardTreatsExpiredLeaseAsRetryPending(t *testing.T) {
	job := queue.Job{State: queue.StateLeased, Attempts: 1, LeaseUntil: time.Now().Add(-time.Second)}
	sent, confirmed := dashboardIndicators(job, time.Now())
	if sent.Symbol != "✓" || confirmed.Symbol != "↻" {
		t.Fatalf("unexpected expired lease indicators: sent=%#v confirmed=%#v", sent, confirmed)
	}
}

func enqueueAndLease(t *testing.T, ctx context.Context, store *queue.Store, title string) queue.Job {
	t.Helper()
	queued, err := store.Enqueue(ctx, capture.Request{Title: title})
	if err != nil {
		t.Fatal(err)
	}
	leased, ok, err := store.Lease(ctx, time.Now(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("lease %q: ok=%v err=%v", title, ok, err)
	}
	if leased.ID != queued.ID {
		t.Fatalf("leased job %q, want %q", leased.ID, queued.ID)
	}
	return leased
}
