package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/nejmlabs/things-index/internal/capture"
	"github.com/nejmlabs/things-index/internal/helper"
	"github.com/nejmlabs/things-index/internal/journal"
	"github.com/nejmlabs/things-index/internal/worker"
)

type localTestHelper struct {
	worker.Helper
	creates int
}

func (h *localTestHelper) CreateArea(context.Context, capture.CreateAreaRequest) (helper.Response, error) {
	h.creates++
	return helper.Response{OK: true, ID: "area-1", Warnings: []string{"saved warning"}}, nil
}

func (h *localTestHelper) CreateTag(context.Context, capture.CreateTagRequest) (helper.Response, error) {
	return helper.Response{}, errors.New("unexpected tag write")
}

func TestLocalWriteRetriesSurviveRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "journal.sqlite")
	helper := &localTestHelper{}
	var previous stdioCaptureResult
	for attempt := 0; attempt < 2; attempt++ {
		store, err := journal.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		writer := newLocalWriter(&worker.Processor{Helper: helper, Journal: store})
		request := capture.Request{CreateAreaRequest: &capture.CreateAreaRequest{Title: "Work", IdempotencyKey: "spoken-command-1"}}
		result, err := writer.write(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		if result.ThingsID != "area-1" || len(result.Warnings) != 1 || len(result.RequestID) != 32 {
			t.Fatalf("result = %#v", result)
		}
		if attempt > 0 && result.RequestID != previous.RequestID {
			t.Fatal("retry changed request ID")
		}
		previous = result
		request.CreateAreaRequest.Title = "Home"
		if _, err := writer.write(ctx, request); !errors.Is(err, journal.ErrPayloadMismatch) {
			t.Fatalf("conflicting key = %v", err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if helper.creates != 1 {
		t.Fatalf("native writes = %d, want 1", helper.creates)
	}
}

func TestLocalWriteCancellationWhileWaitingDoesNotDispatch(t *testing.T) {
	helper := &localTestHelper{}
	writer := newLocalWriter(&worker.Processor{Helper: helper})
	writer.gate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := writer.write(ctx, capture.Request{CreateAreaRequest: &capture.CreateAreaRequest{Title: "Work"}})
	if !errors.Is(err, context.Canceled) || helper.creates != 0 {
		t.Fatalf("err = %v, writes = %d", err, helper.creates)
	}
}
