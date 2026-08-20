package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nejmlabs/things-index/internal/capture"
	"github.com/nejmlabs/things-index/internal/queue"
	"github.com/nejmlabs/things-index/internal/toolschema"
	"github.com/nejmlabs/things-index/internal/worker"
)

const maxRequestBodyBytes = 64 << 10

type Queue interface {
	Enqueue(context.Context, capture.Request) (queue.Job, error)
	Lease(context.Context, time.Time, time.Duration) (queue.Job, bool, error)
	ExpireLeases(context.Context, time.Time, int) (int64, error)
	Complete(context.Context, string, string, string, []string) error
	Fail(context.Context, string, string, string, bool) error
	Get(context.Context, string) (queue.Job, error)
	ListRecent(context.Context, int) ([]queue.Job, error)
}

type Config struct {
	PublicToken    string
	WorkerToken    string
	AllowedOrigins []string
	WaitForResult  time.Duration
	WorkerLongPoll time.Duration
	LeaseDuration  time.Duration
	PollInterval   time.Duration
	MaxAttempts    int
	DashboardToken string
	DashboardLimit int
	// Version is advertised to MCP clients in the initialize response;
	// empty means "dev" (source builds without release metadata).
	Version string
}

type CaptureResult struct {
	Status    string   `json:"status" jsonschema:"Operation state: queued, created, succeeded, or failed."`
	RequestID string   `json:"request_id" jsonschema:"Stable request identifier for status checks."`
	ThingsID  string   `json:"things_id,omitempty" jsonschema:"Things task/project identifier, once created."`
	Data      any      `json:"data,omitempty" jsonschema:"Structured query results."`
	Warnings  []string `json:"warnings,omitempty" jsonschema:"Non-fatal capture warnings."`
	Error     string   `json:"error,omitempty" jsonschema:"Failure description, if capture failed."`
}

type statusInput struct {
	RequestID string `json:"request_id" jsonschema:"Request identifier returned by capture_things_task."`
}

func NewHandler(store Queue, config Config) (http.Handler, error) {
	if store == nil {
		return nil, errors.New("queue is required")
	}
	if len(config.PublicToken) < 32 || len(config.WorkerToken) < 32 {
		return nil, errors.New("public and worker tokens must each be at least 32 characters")
	}
	if secureEqual(config.PublicToken, config.WorkerToken) {
		return nil, errors.New("public and worker tokens must be different")
	}
	if config.DashboardToken != "" {
		if len(config.DashboardToken) < 32 {
			return nil, errors.New("dashboard token must be at least 32 characters")
		}
		if secureEqual(config.DashboardToken, config.PublicToken) || secureEqual(config.DashboardToken, config.WorkerToken) {
			return nil, errors.New("dashboard token must be different from public and worker tokens")
		}
	}
	applyDefaults(&config)

	mcpServer := mcp.NewServer(&mcp.Implementation{
		Name:    "things-index",
		Version: config.Version,
	}, &mcp.ServerOptions{
		Instructions: "Capture tasks in Things. Use the returned request_id to check a queued capture.",
		Capabilities: &mcp.ServerCapabilities{},
	})

	service := &service{queue: store, config: config}
	var registrationErr error
	register := func(err error) {
		if registrationErr == nil {
			registrationErr = err
		}
	}
	register(toolschema.AddTool(mcpServer, &mcp.Tool{
		Name:        "capture_things_task",
		Description: "Create one task in Things on the connected Mac. The call may return queued while the Mac is offline.",
	}, service.captureTask))
	register(toolschema.AddTool(mcpServer, &mcp.Tool{
		Name:        "things_capture_status",
		Description: "Check a Things capture using the request_id returned by capture_things_task.",
	}, service.captureStatus))
	register(toolschema.AddTool(mcpServer, &mcp.Tool{
		Name:        "create_things_heading",
		Description: "Create a new section heading inside a Things 3 project.",
	}, service.createHeading))
	register(toolschema.AddTool(mcpServer, &mcp.Tool{
		Name:        "archive_things_heading",
		Description: "Archive/hide a section heading from an active Things 3 project.",
	}, service.archiveHeading))
	register(toolschema.AddTool(mcpServer, &mcp.Tool{
		Name:        "rename_things_heading",
		Description: "Rename an existing section heading inside a Things 3 project.",
	}, service.renameHeading))
	register(toolschema.AddTool(mcpServer, &mcp.Tool{
		Name:        "archive_things_task",
		Description: "Archive a task in Things 3 (mark completed, canceled, or move to trash).",
	}, service.archiveTask))
	register(toolschema.AddTool(mcpServer, &mcp.Tool{
		Name:        "archive_things_project",
		Description: "Archive an entire project in Things 3 (mark completed or canceled).",
	}, service.archiveProject))
	register(toolschema.AddTool(mcpServer, &mcp.Tool{
		Name:        "get_things_today",
		Description: "Get all tasks scheduled for Today in Things 3.",
	}, service.getToday))
	register(toolschema.AddTool(mcpServer, &mcp.Tool{
		Name:        "get_things_inbox",
		Description: "Get all unorganized tasks in Things 3 Inbox.",
	}, service.getInbox))
	register(toolschema.AddTool(mcpServer, &mcp.Tool{
		Name:        "list_things_projects",
		Description: "List all active projects and their areas in Things 3.",
	}, service.listProjects))
	register(toolschema.AddTool(mcpServer, &mcp.Tool{
		Name:        "search_things_tasks",
		Description: "Search tasks in Things 3 across any scope (today, inbox, anytime, someday, all) by title, project, area, or tag.",
	}, service.searchTasks))
	register(toolschema.AddTool(mcpServer, &mcp.Tool{
		Name:        "create_things_project",
		Description: "Create a new project in Things 3.",
	}, service.createProject))
	register(toolschema.AddTool(mcpServer, &mcp.Tool{
		Name:        "update_things_task",
		Description: "Update, reschedule, or add notes/checklists to an existing task in Things 3.",
	}, service.updateTask))
	if registrationErr != nil {
		return nil, registrationErr
	}

	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return mcpServer
	}, &mcp.StreamableHTTPOptions{
		// Stateful: issue Mcp-Session-Id on initialize. Pebble Index's agent
		// runtime abandons and endlessly restarts a session-less handshake
		// (observed live), and stateful is what mainstream servers do anyway.
		JSONResponse:                 true,
		DisableLocalhostProtection:   true,
		MaxRequestBodyBytes:          maxRequestBodyBytes,
		PropagateRequestCancellation: true,
		// Without a timeout, sessions abandoned mid-handshake (the exact
		// client behavior that motivated stateful mode) accumulate forever
		// in a 24/7 daemon.
		SessionTimeout: 10 * time.Minute,
	})

	originProtection := http.NewCrossOriginProtection()
	for _, origin := range config.AllowedOrigins {
		if err := originProtection.AddTrustedOrigin(origin); err != nil {
			return nil, fmt.Errorf("add trusted origin %q: %w", origin, err)
		}
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", securityHeaders(bearerAuth(config.PublicToken, originProtection.Handler(standaloneSSE(lenientAccept(stripCacheExtensions(mcpHandler)))))))
	mux.Handle("/worker/", securityHeaders(bearerAuth(config.WorkerToken, http.HandlerFunc(service.workerAPI))))
	if config.DashboardToken != "" {
		dashboard := newDashboardHandler(store, config.DashboardLimit)
		mux.Handle("GET /dashboard", securityHeaders(dashboardBasicAuth(config.DashboardToken, dashboard)))
	}
	mux.HandleFunc("GET /healthz", func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"status":"ok"}`+"\n")
	})
	return mux, nil
}

// lenientAccept rewrites POST Accept headers the MCP SDK would reject with a
// 400. The spec says clients send both "application/json" and
// "text/event-stream", but common HTTP stacks default to "application/json"
// alone or omit the header entirely (observed live with Pebble Index); such
// clients wanted a JSON response, which the server produces anyway
// (JSONResponse mode), so upgrading their Accept to the compliant pair only
// enables the exchange. GETs are never rewritten: a GET's Accept selects
// between the SSE stream and a 405, and upgrading a JSON-only GET would turn
// it into a stream request the client never asked for.
func lenientAccept(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			next.ServeHTTP(response, request)
			return
		}
		accept := request.Header.Get("Accept")
		hasJSON := strings.Contains(accept, "application/json") ||
			strings.Contains(accept, "application/*") || strings.Contains(accept, "*/*")
		hasStream := strings.Contains(accept, "text/event-stream") ||
			strings.Contains(accept, "text/*") || strings.Contains(accept, "*/*")
		if !hasJSON || !hasStream {
			request.Header.Set("Accept", "application/json, text/event-stream")
		}
		next.ServeHTTP(response, request)
	})
}

// standaloneSSE serves a keep-alive event stream for session-less GETs. The
// stateful SDK answers those with 400 (no Mcp-Session-Id), but Pebble Index
// opens exactly such a stream right after initialize and treats its failure
// as a broken server (observed live: full handshake restarts). The stream
// never carries messages — real notification streams are session-bound and
// served by the SDK behind this middleware — so it only holds the connection
// open, capped at two hours as a leak backstop (the http.Server write
// timeout usually ends it much earlier).
func standaloneSSE(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || !strings.Contains(request.Header.Get("Accept"), "text/event-stream") ||
			request.Header.Get("Mcp-Session-Id") != "" {
			next.ServeHTTP(response, request)
			return
		}
		flusher, ok := response.(http.Flusher)
		if !ok {
			next.ServeHTTP(response, request)
			return
		}
		response.Header().Set("Content-Type", "text/event-stream")
		response.Header().Set("Cache-Control", "no-cache, no-transform")
		// Tell buffering reverse proxies to pass the stream through.
		response.Header().Set("X-Accel-Buffering", "no")
		response.WriteHeader(http.StatusOK)
		// An immediate comment gives clients a first byte to treat the
		// stream as established instead of a silent 25-second wait.
		if _, err := io.WriteString(response, ": connected\n\n"); err != nil {
			return
		}
		flusher.Flush()
		keepAlive := time.NewTicker(25 * time.Second)
		defer keepAlive.Stop()
		maxAge := time.NewTimer(2 * time.Hour)
		defer maxAge.Stop()
		for {
			select {
			case <-request.Context().Done():
				return
			case <-maxAge.C:
				return
			case <-keepAlive.C:
				if _, err := io.WriteString(response, ": keep-alive\n\n"); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	})
}

// stripCacheExtensions removes the SDK's draft ttlMs/cacheScope result
// extension fields, which the SDK (v1.7.0, no omitempty on either) emits on
// every list result and strict client deserializers reject as unknown keys
// (observed live with Pebble Index). JSON responses are buffered, rewritten,
// and sent with a corrected Content-Length; anything else — notably the
// SDK's session-bound SSE streams — passes through unbuffered with a working
// Flush, decided per response at WriteHeader time.
func stripCacheExtensions(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		rewriter := &jsonRewriter{destination: response, header: http.Header{}}
		next.ServeHTTP(rewriter, request)
		rewriter.finish()
	})
}

// jsonRewriter buffers JSON responses for post-processing and transparently
// streams everything else.
type jsonRewriter struct {
	destination http.ResponseWriter
	header      http.Header
	status      int
	buffer      bytes.Buffer
	streaming   bool
	decided     bool
}

func (j *jsonRewriter) Header() http.Header { return j.header }

func (j *jsonRewriter) WriteHeader(status int) {
	if j.decided {
		return
	}
	j.decided = true
	j.status = status
	if !strings.Contains(j.header.Get("Content-Type"), "application/json") {
		j.streaming = true
		copyHeadersReplacing(j.destination.Header(), j.header)
		j.destination.WriteHeader(status)
	}
}

func (j *jsonRewriter) Write(p []byte) (int, error) {
	if !j.decided {
		j.WriteHeader(http.StatusOK)
	}
	if j.streaming {
		return j.destination.Write(p)
	}
	return j.buffer.Write(p)
}

// Flush satisfies http.Flusher so the SDK's best-effort stream flushes reach
// the network; buffered JSON responses have nothing to flush early because
// the rewrite needs the complete body.
func (j *jsonRewriter) Flush() {
	if !j.streaming {
		return
	}
	if flusher, ok := j.destination.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (j *jsonRewriter) finish() {
	if j.streaming {
		return
	}
	if !j.decided {
		j.status = http.StatusOK
	}
	body := stripResultCacheKeys(j.buffer.Bytes())
	copyHeadersReplacing(j.destination.Header(), j.header)
	if len(body) > 0 {
		j.destination.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	j.destination.WriteHeader(j.status)
	if len(body) > 0 {
		_, _ = j.destination.Write(body)
	}
}

// copyHeadersReplacing overwrites same-named destination headers instead of
// appending: outer middleware (securityHeaders) pre-sets some of the same
// keys the SDK sets, and Add-semantics shipped duplicates.
func copyHeadersReplacing(destination, source http.Header) {
	for name, values := range source {
		destination[name] = values
	}
}

// stripResultCacheKeys deletes ttlMs/cacheScope from the top level of a
// JSON-RPC result object, returning the input unchanged when it is not one.
// Top-level is correct for SDK v1.7.0, where Cacheable is embedded only in
// list-style results.
func stripResultCacheKeys(body []byte) []byte {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return body
	}
	rawResult, ok := envelope["result"]
	if !ok {
		return body
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(rawResult, &result); err != nil {
		return body
	}
	_, hadTTL := result["ttlMs"]
	_, hadScope := result["cacheScope"]
	if !hadTTL && !hadScope {
		return body
	}
	delete(result, "ttlMs")
	delete(result, "cacheScope")
	newResult, err := json.Marshal(result)
	if err != nil {
		return body
	}
	envelope["result"] = newResult
	newBody, err := json.Marshal(envelope)
	if err != nil {
		return body
	}
	return newBody
}

type service struct {
	queue  Queue
	config Config
}

func (s *service) captureTask(ctx context.Context, _ *mcp.CallToolRequest, input capture.TaskFields) (*mcp.CallToolResult, CaptureResult, error) {
	task := capture.Request{TaskFields: input}
	if err := task.Validate(); err != nil {
		return nil, CaptureResult{}, fmt.Errorf("invalid Things capture: %w", err)
	}
	job, err := s.queue.Enqueue(ctx, task)
	if err != nil {
		return nil, CaptureResult{}, fmt.Errorf("queue Things capture: %w", err)
	}
	result, err := s.waitForResult(ctx, job.ID, s.config.WaitForResult)
	if err != nil {
		return nil, CaptureResult{}, err
	}
	return nil, result, nil
}

func (s *service) captureStatus(ctx context.Context, _ *mcp.CallToolRequest, input statusInput) (*mcp.CallToolResult, CaptureResult, error) {
	if !validID(input.RequestID) {
		return nil, CaptureResult{}, errors.New("request_id must be a 32-character lowercase hexadecimal identifier")
	}
	job, err := s.queue.Get(ctx, input.RequestID)
	if err != nil {
		return nil, CaptureResult{}, err
	}
	return nil, captureResult(job), nil
}

func (s *service) createHeading(ctx context.Context, _ *mcp.CallToolRequest, input capture.HeadingRequest) (*mcp.CallToolResult, CaptureResult, error) {
	if err := input.Validate(); err != nil {
		return nil, CaptureResult{}, fmt.Errorf("invalid heading request: %w", err)
	}
	task := capture.Request{
		HeadingOperation: "create",
		HeadingRequest:   &input,
	}
	job, err := s.queue.Enqueue(ctx, task)
	if err != nil {
		return nil, CaptureResult{}, fmt.Errorf("queue heading creation: %w", err)
	}
	result, err := s.waitForResult(ctx, job.ID, s.config.WaitForResult)
	if err != nil {
		return nil, CaptureResult{}, err
	}
	return nil, result, nil
}

func (s *service) archiveHeading(ctx context.Context, _ *mcp.CallToolRequest, input capture.HeadingRequest) (*mcp.CallToolResult, CaptureResult, error) {
	if err := input.Validate(); err != nil {
		return nil, CaptureResult{}, fmt.Errorf("invalid heading request: %w", err)
	}
	task := capture.Request{
		HeadingOperation: "archive",
		HeadingRequest:   &input,
	}
	job, err := s.queue.Enqueue(ctx, task)
	if err != nil {
		return nil, CaptureResult{}, fmt.Errorf("queue heading archive: %w", err)
	}
	result, err := s.waitForResult(ctx, job.ID, s.config.WaitForResult)
	if err != nil {
		return nil, CaptureResult{}, err
	}
	return nil, result, nil
}

func (s *service) renameHeading(ctx context.Context, _ *mcp.CallToolRequest, input capture.HeadingRequest) (*mcp.CallToolResult, CaptureResult, error) {
	if err := input.Validate(); err != nil {
		return nil, CaptureResult{}, fmt.Errorf("invalid heading request: %w", err)
	}
	if strings.TrimSpace(input.NewTitle) == "" {
		return nil, CaptureResult{}, errors.New("new_title is required when renaming a heading")
	}
	task := capture.Request{
		HeadingOperation: "rename",
		HeadingRequest:   &input,
	}
	job, err := s.queue.Enqueue(ctx, task)
	if err != nil {
		return nil, CaptureResult{}, fmt.Errorf("queue heading rename: %w", err)
	}
	result, err := s.waitForResult(ctx, job.ID, s.config.WaitForResult)
	if err != nil {
		return nil, CaptureResult{}, err
	}
	return nil, result, nil
}

func (s *service) archiveTask(ctx context.Context, _ *mcp.CallToolRequest, input capture.ArchiveTaskRequest) (*mcp.CallToolResult, CaptureResult, error) {
	if err := input.Validate(); err != nil {
		return nil, CaptureResult{}, fmt.Errorf("invalid archive task request: %w", err)
	}
	task := capture.Request{
		ArchiveTaskRequest: &input,
	}
	job, err := s.queue.Enqueue(ctx, task)
	if err != nil {
		return nil, CaptureResult{}, fmt.Errorf("queue task archive: %w", err)
	}
	result, err := s.waitForResult(ctx, job.ID, s.config.WaitForResult)
	if err != nil {
		return nil, CaptureResult{}, err
	}
	return nil, result, nil
}

func (s *service) archiveProject(ctx context.Context, _ *mcp.CallToolRequest, input capture.ArchiveProjectRequest) (*mcp.CallToolResult, CaptureResult, error) {
	if err := input.Validate(); err != nil {
		return nil, CaptureResult{}, fmt.Errorf("invalid archive project request: %w", err)
	}
	task := capture.Request{
		ArchiveProjectRequest: &input,
	}
	job, err := s.queue.Enqueue(ctx, task)
	if err != nil {
		return nil, CaptureResult{}, fmt.Errorf("queue project archive: %w", err)
	}
	result, err := s.waitForResult(ctx, job.ID, s.config.WaitForResult)
	if err != nil {
		return nil, CaptureResult{}, err
	}
	return nil, result, nil
}

func (s *service) getToday(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, CaptureResult, error) {
	task := capture.Request{
		QueryTasksRequest: &capture.QueryTasksRequest{Scope: "today"},
	}
	job, err := s.queue.Enqueue(ctx, task)
	if err != nil {
		return nil, CaptureResult{}, fmt.Errorf("queue get today: %w", err)
	}
	result, err := s.waitForResult(ctx, job.ID, s.config.WaitForResult)
	if err != nil {
		return nil, CaptureResult{}, err
	}
	return nil, result, nil
}

func (s *service) getInbox(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, CaptureResult, error) {
	task := capture.Request{
		QueryTasksRequest: &capture.QueryTasksRequest{Scope: "inbox"},
	}
	job, err := s.queue.Enqueue(ctx, task)
	if err != nil {
		return nil, CaptureResult{}, fmt.Errorf("queue get inbox: %w", err)
	}
	result, err := s.waitForResult(ctx, job.ID, s.config.WaitForResult)
	if err != nil {
		return nil, CaptureResult{}, err
	}
	return nil, result, nil
}

func (s *service) listProjects(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, CaptureResult, error) {
	task := capture.Request{
		QueryTasksRequest: &capture.QueryTasksRequest{Scope: "projects"},
	}
	job, err := s.queue.Enqueue(ctx, task)
	if err != nil {
		return nil, CaptureResult{}, fmt.Errorf("queue list projects: %w", err)
	}
	result, err := s.waitForResult(ctx, job.ID, s.config.WaitForResult)
	if err != nil {
		return nil, CaptureResult{}, err
	}
	return nil, result, nil
}

func (s *service) searchTasks(ctx context.Context, _ *mcp.CallToolRequest, input capture.QueryTasksRequest) (*mcp.CallToolResult, CaptureResult, error) {
	task := capture.Request{
		QueryTasksRequest: &input,
	}
	job, err := s.queue.Enqueue(ctx, task)
	if err != nil {
		return nil, CaptureResult{}, fmt.Errorf("queue search tasks: %w", err)
	}
	result, err := s.waitForResult(ctx, job.ID, s.config.WaitForResult)
	if err != nil {
		return nil, CaptureResult{}, err
	}
	return nil, result, nil
}

func (s *service) createProject(ctx context.Context, _ *mcp.CallToolRequest, input capture.CreateProjectRequest) (*mcp.CallToolResult, CaptureResult, error) {
	if err := input.Validate(); err != nil {
		return nil, CaptureResult{}, fmt.Errorf("invalid create project request: %w", err)
	}
	task := capture.Request{
		CreateProjectRequest: &input,
	}
	job, err := s.queue.Enqueue(ctx, task)
	if err != nil {
		return nil, CaptureResult{}, fmt.Errorf("queue create project: %w", err)
	}
	result, err := s.waitForResult(ctx, job.ID, s.config.WaitForResult)
	if err != nil {
		return nil, CaptureResult{}, err
	}
	return nil, result, nil
}

func (s *service) updateTask(ctx context.Context, _ *mcp.CallToolRequest, input capture.UpdateTaskRequest) (*mcp.CallToolResult, CaptureResult, error) {
	if err := input.Validate(); err != nil {
		return nil, CaptureResult{}, fmt.Errorf("invalid update task request: %w", err)
	}
	task := capture.Request{
		UpdateTaskRequest: &input,
	}
	job, err := s.queue.Enqueue(ctx, task)
	if err != nil {
		return nil, CaptureResult{}, fmt.Errorf("queue update task: %w", err)
	}
	result, err := s.waitForResult(ctx, job.ID, s.config.WaitForResult)
	if err != nil {
		return nil, CaptureResult{}, err
	}
	return nil, result, nil
}

func (s *service) waitForResult(ctx context.Context, jobID string, duration time.Duration) (CaptureResult, error) {
	deadline := time.NewTimer(duration)
	defer deadline.Stop()
	ticker := time.NewTicker(s.config.PollInterval)
	defer ticker.Stop()
	for {
		job, err := s.queue.Get(ctx, jobID)
		if err != nil {
			return CaptureResult{}, err
		}
		if job.State == queue.StateSucceeded || job.State == queue.StateFailed {
			return captureResult(job), nil
		}
		select {
		case <-ctx.Done():
			return CaptureResult{}, ctx.Err()
		case <-deadline.C:
			return captureResult(job), nil
		case <-ticker.C:
		}
	}
}

func captureResult(job queue.Job) CaptureResult {
	result := CaptureResult{RequestID: job.ID, ThingsID: job.ThingsID, Warnings: job.Warnings}
	if job.ThingsID != "" && (strings.HasPrefix(job.ThingsID, "[") || strings.HasPrefix(job.ThingsID, "{")) {
		var d any
		if err := json.Unmarshal([]byte(job.ThingsID), &d); err == nil {
			result.Data = d
			result.ThingsID = ""
		}
	}
	switch job.State {
	case queue.StateSucceeded:
		result.Status = "created"
	case queue.StateFailed:
		result.Status = "failed"
		result.Error = job.LastError
	default:
		result.Status = "queued"
	}
	return result
}

func (s *service) workerAPI(response http.ResponseWriter, request *http.Request) {
	// The setup wizard uses this authenticated no-op to verify the worker
	// token before installing anything; bearerAuth has already run.
	if request.URL.Path == "/worker/v1/ping" {
		if request.Method != http.MethodGet {
			response.Header().Set("Allow", http.MethodGet)
			http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		response.WriteHeader(http.StatusNoContent)
		return
	}
	if request.Method != http.MethodPost {
		response.Header().Set("Allow", http.MethodPost)
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if request.URL.Path == "/worker/v1/lease" {
		s.lease(response, request)
		return
	}
	jobID, operation, ok := parseJobPath(request.URL.Path)
	if !ok {
		http.NotFound(response, request)
		return
	}
	switch operation {
	case "complete":
		s.complete(response, request, jobID)
	case "fail":
		s.fail(response, request, jobID)
	default:
		http.NotFound(response, request)
	}
}

func (s *service) lease(response http.ResponseWriter, request *http.Request) {
	if _, err := s.queue.ExpireLeases(request.Context(), time.Now(), s.config.MaxAttempts); err != nil {
		http.Error(response, "expire capture leases", http.StatusInternalServerError)
		return
	}
	deadline := time.NewTimer(s.config.WorkerLongPoll)
	defer deadline.Stop()
	ticker := time.NewTicker(s.config.PollInterval)
	defer ticker.Stop()
	for {
		job, found, err := s.queue.Lease(request.Context(), time.Now(), s.config.LeaseDuration)
		if err != nil {
			http.Error(response, "lease capture job", http.StatusInternalServerError)
			return
		}
		if found {
			writeJSON(response, http.StatusOK, worker.Lease{
				Job: worker.Job{ID: job.ID, Task: job.Task}, LeaseToken: job.LeaseToken, Attempts: job.Attempts,
			})
			return
		}
		select {
		case <-request.Context().Done():
			return
		case <-deadline.C:
			response.WriteHeader(http.StatusNoContent)
			return
		case <-ticker.C:
		}
	}
}

func (s *service) complete(response http.ResponseWriter, request *http.Request, jobID string) {
	var input struct {
		LeaseToken string   `json:"leaseToken"`
		ThingsID   string   `json:"thingsId"`
		Warnings   []string `json:"warnings,omitempty"`
	}
	if err := decodeRequest(response, request, &input); err != nil {
		return
	}
	if err := s.queue.Complete(request.Context(), jobID, input.LeaseToken, input.ThingsID, input.Warnings); err != nil {
		http.Error(response, err.Error(), http.StatusConflict)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (s *service) fail(response http.ResponseWriter, request *http.Request, jobID string) {
	var input struct {
		LeaseToken string `json:"leaseToken"`
		Error      string `json:"error"`
		Retryable  bool   `json:"retryable"`
	}
	if err := decodeRequest(response, request, &input); err != nil {
		return
	}
	if input.LeaseToken == "" || strings.TrimSpace(input.Error) == "" {
		http.Error(response, "leaseToken and error are required", http.StatusBadRequest)
		return
	}
	if job, err := s.queue.Get(request.Context(), jobID); err == nil && job.Attempts >= s.config.MaxAttempts {
		input.Retryable = false
	}
	if err := s.queue.Fail(request.Context(), jobID, input.LeaseToken, input.Error, input.Retryable); err != nil {
		http.Error(response, err.Error(), http.StatusConflict)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func decodeRequest(response http.ResponseWriter, request *http.Request, target any) error {
	request.Body = http.MaxBytesReader(response, request.Body, maxRequestBodyBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		http.Error(response, "invalid JSON request", http.StatusBadRequest)
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		http.Error(response, "request must contain exactly one JSON value", http.StatusBadRequest)
		if err == nil {
			return errors.New("unexpected data after JSON value")
		}
		return err
	}
	return nil
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

func parseJobPath(path string) (string, string, bool) {
	remainder := strings.TrimPrefix(path, "/worker/v1/jobs/")
	if remainder == path {
		return "", "", false
	}
	jobID, operation, ok := strings.Cut(remainder, "/")
	return jobID, operation, ok && !strings.Contains(operation, "/") && validID(jobID)
}

func validID(value string) bool {
	if len(value) != 32 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func bearerAuth(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		authorization := request.Header.Get("Authorization")
		scheme, provided, found := strings.Cut(authorization, " ")
		// The auth-scheme token is case-insensitive (RFC 9110 §11.1).
		if !found || !strings.EqualFold(scheme, "Bearer") || !secureEqual(provided, token) {
			response.Header().Set("WWW-Authenticate", `Bearer realm="things-index"`)
			http.Error(response, "unauthorised", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(response, request)
	})
}

func secureEqual(left, right string) bool {
	leftHash := sha256.Sum256([]byte(left))
	rightHash := sha256.Sum256([]byte(right))
	return subtle.ConstantTimeCompare(leftHash[:], rightHash[:]) == 1
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Cache-Control", "no-store")
		response.Header().Set("X-Content-Type-Options", "nosniff")
		response.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(response, request)
	})
}

func applyDefaults(config *Config) {
	if config.Version == "" {
		config.Version = "dev"
	}
	if config.WaitForResult <= 0 {
		config.WaitForResult = 8 * time.Second
	}
	if config.WorkerLongPoll <= 0 {
		config.WorkerLongPoll = 25 * time.Second
	}
	if config.LeaseDuration <= 0 {
		config.LeaseDuration = 90 * time.Second
	}
	if config.PollInterval <= 0 {
		config.PollInterval = 100 * time.Millisecond
	}
	if config.MaxAttempts <= 0 {
		config.MaxAttempts = 5
	}
	if config.DashboardLimit <= 0 {
		config.DashboardLimit = 100
	}
	if config.DashboardLimit > 500 {
		config.DashboardLimit = 500
	}
}
