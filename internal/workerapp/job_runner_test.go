package workerapp

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nejmlabs/things-index/internal/capture"
	"github.com/nejmlabs/things-index/internal/worker"
)

func TestJobRunnerReportsTimeoutAndContinues(t *testing.T) {
	var processed, failed, completed, marked []string
	var processingCtx context.Context
	runner := jobRunner{jobTimeout: 20 * time.Millisecond, reportTimeout: time.Second}
	runner.process = func(ctx context.Context, job worker.Job) (worker.Outcome, error) {
		processingCtx = ctx
		processed = append(processed, job.ID)
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > runner.jobTimeout {
			t.Fatal("processing context has no job deadline")
		}
		if job.ID == "slow" {
			<-ctx.Done()
			return worker.Outcome{}, fmt.Errorf("native command stopped: %w", ctx.Err())
		}
		if err := ctx.Err(); err != nil {
			t.Fatalf("next job inherited an expired context: %v", err)
		}
		return worker.Outcome{ThingsID: "created-task"}, nil
	}
	checkReportContext := func(ctx context.Context) {
		t.Helper()
		if processingCtx.Err() == nil {
			t.Fatal("processing context was not released before reporting")
		}
		if err := ctx.Err(); err != nil {
			t.Fatalf("reporting inherited processing cancellation: %v", err)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > runner.reportTimeout {
			t.Fatal("reporting context has no independent deadline")
		}
	}
	runner.fail = func(ctx context.Context, lease worker.Lease, err error, retryable bool) error {
		checkReportContext(ctx)
		if !errors.Is(err, context.DeadlineExceeded) || !retryable {
			t.Fatalf("timeout reported as %v, retryable=%v", err, retryable)
		}
		failed = append(failed, lease.ID)
		return nil
	}
	runner.complete = func(ctx context.Context, lease worker.Lease, outcome worker.Outcome) error {
		checkReportContext(ctx)
		if outcome.ThingsID != "created-task" {
			t.Fatalf("unexpected outcome: %#v", outcome)
		}
		completed = append(completed, lease.ID)
		return nil
	}
	runner.markReported = func(ctx context.Context, id string) error {
		checkReportContext(ctx)
		marked = append(marked, id)
		return nil
	}
	for _, id := range []string{"slow", "next"} {
		lease := worker.Lease{Job: worker.Job{ID: id, Task: capture.Request{TaskFields: capture.TaskFields{Title: "Task"}}}}
		if err := runner.run(context.Background(), lease); err != nil {
			t.Fatal(err)
		}
	}
	if fmt.Sprint(processed) != "[slow next]" || fmt.Sprint(failed) != "[slow]" ||
		fmt.Sprint(completed) != "[next]" || fmt.Sprint(marked) != "[next]" {
		t.Fatalf("unexpected lifecycle: processed=%v failed=%v completed=%v marked=%v", processed, failed, completed, marked)
	}
}

func TestJobRunnerBoundsSuccessAndFailureReporting(t *testing.T) {
	for _, success := range []bool{true, false} {
		t.Run(fmt.Sprintf("success=%v", success), func(t *testing.T) {
			runner := jobRunner{jobTimeout: time.Second, reportTimeout: 20 * time.Millisecond}
			runner.process = func(context.Context, worker.Job) (worker.Outcome, error) {
				if !success {
					return worker.Outcome{}, errors.New("processing failed")
				}
				return worker.Outcome{ThingsID: "task"}, nil
			}
			reportCalls := 0
			stall := func(ctx context.Context) error {
				reportCalls++
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("reporting context has no deadline")
				}
				<-ctx.Done()
				return ctx.Err()
			}
			runner.complete = func(ctx context.Context, _ worker.Lease, _ worker.Outcome) error {
				if !success {
					t.Fatal("failure reported as success")
				}
				return stall(ctx)
			}
			runner.fail = func(ctx context.Context, _ worker.Lease, _ error, _ bool) error {
				if success {
					t.Fatal("success reported as failure")
				}
				return stall(ctx)
			}
			runner.markReported = func(context.Context, string) error {
				t.Fatal("unacknowledged result marked as reported")
				return nil
			}
			if err := runner.run(context.Background(), worker.Lease{}); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("reporting timeout = %v", err)
			}
			if reportCalls != 1 {
				t.Fatalf("reported %d times", reportCalls)
			}
		})
	}
}

func TestJobRunnerShutdownCancelsProcessingAndReporting(t *testing.T) {
	for _, stage := range []string{"processing", "reporting"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runner := jobRunner{jobTimeout: time.Second, reportTimeout: time.Second}
			runner.process = func(jobCtx context.Context, _ worker.Job) (worker.Outcome, error) {
				if stage == "processing" {
					cancel()
					<-jobCtx.Done()
					return worker.Outcome{}, jobCtx.Err()
				}
				return worker.Outcome{ThingsID: "task"}, nil
			}
			reported := false
			runner.complete = func(reportCtx context.Context, _ worker.Lease, _ worker.Outcome) error {
				if stage != "reporting" {
					t.Fatal("processing cancellation did not stop reporting")
				}
				reported = true
				cancel()
				<-reportCtx.Done()
				return reportCtx.Err()
			}
			runner.fail = func(context.Context, worker.Lease, error, bool) error {
				t.Fatal("shutdown was reported as a job failure")
				return nil
			}
			runner.markReported = func(context.Context, string) error {
				t.Fatal("canceled report was marked as acknowledged")
				return nil
			}
			if err := runner.run(ctx, worker.Lease{}); !errors.Is(err, context.Canceled) {
				t.Fatalf("shutdown error = %v", err)
			}
			if reported != (stage == "reporting") {
				t.Fatalf("report attempted=%v during %s shutdown", reported, stage)
			}
		})
	}
}
