package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nejmlabs/things-index/internal/capture"
	"github.com/nejmlabs/things-index/internal/worker"
)

// Local MCP calls use the same durable write protocol as leased worker jobs.
// The gate keeps separate calls in one session from modifying Things together.
type localWriter struct {
	processor *worker.Processor
	gate      chan struct{}
}

func newLocalWriter(processor *worker.Processor) *localWriter {
	return &localWriter{processor: processor, gate: make(chan struct{}, 1)}
}

func (w *localWriter) write(ctx context.Context, task capture.Request) (stdioCaptureResult, error) {
	ctx, cancel := context.WithTimeout(ctx, worker.JobTimeout)
	defer cancel()
	if task.IdempotencyKey == "" {
		task.IdempotencyKey = task.OperationKey()
	}
	if err := task.Validate(); err != nil {
		return stdioCaptureResult{}, fmt.Errorf("invalid Things request: %w", err)
	}
	select {
	case w.gate <- struct{}{}:
		defer func() { <-w.gate }()
	case <-ctx.Done():
		return stdioCaptureResult{}, ctx.Err()
	}
	id := randomHex(16)
	if task.IdempotencyKey != "" {
		// Separate local calls from jobs delivered by a remote queue sharing
		// this Mac's journal; keys remain stable across process restarts.
		sum := sha256.Sum256([]byte("things-index-stdio-idempotency:" + task.IdempotencyKey))
		id = hex.EncodeToString(sum[:16])
	}
	outcome, err := w.processor.Process(ctx, worker.Job{ID: id, Task: task})
	if err != nil {
		return stdioCaptureResult{}, err
	}
	// Stdio has no durable acknowledgement of delivery to the caller. Keep
	// the finalised/applied result so pruning cannot erase retry protection.
	return stdioCaptureResult{RequestID: id, ThingsID: outcome.ThingsID, Warnings: outcome.Warnings}, nil
}

func addLocalWriteTool[Input any](server *mcp.Server, writer *localWriter, name, description, status string, request func(Input) capture.Request) {
	mustAddTool(server, &mcp.Tool{Name: name, Description: description},
		func(ctx context.Context, _ *mcp.CallToolRequest, input Input) (*mcp.CallToolResult, stdioCaptureResult, error) {
			result, err := writer.write(ctx, request(input))
			if err == nil {
				result.Status = status
			}
			return nil, result, err
		})
}
