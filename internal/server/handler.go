package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nejmlabs/things-index/internal/capture"
	"github.com/nejmlabs/things-index/internal/queue"
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
		Version: "0.1.0",
	}, &mcp.ServerOptions{
		Instructions: "Capture tasks in Things. Use the returned request_id to check a queued capture.",
		Capabilities: &mcp.ServerCapabilities{},
	})

	service := &service{queue: store, config: config}
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "capture_things_task",
		Description: "Create one task in Things on the connected Mac. The call may return queued while the Mac is offline.",
	}, service.captureTask)
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "things_capture_status",
		Description: "Check a Things capture using the request_id returned by capture_things_task.",
	}, service.captureStatus)
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "create_things_heading",
		Description: "Create a new section heading inside a Things 3 project.",
	}, service.createHeading)
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "archive_things_heading",
		Description: "Archive/hide a section heading from an active Things 3 project.",
	}, service.archiveHeading)
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "rename_things_heading",
		Description: "Rename an existing section heading inside a Things 3 project.",
	}, service.renameHeading)
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "archive_things_task",
		Description: "Archive a task in Things 3 (mark completed, canceled, or move to trash).",
	}, service.archiveTask)
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "archive_things_project",
		Description: "Archive an entire project in Things 3 (mark completed or canceled).",
	}, service.archiveProject)
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "get_things_today",
		Description: "Get all tasks scheduled for Today in Things 3.",
	}, service.getToday)
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "get_things_inbox",
		Description: "Get all unorganized tasks in Things 3 Inbox.",
	}, service.getInbox)
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "list_things_projects",
		Description: "List all active projects and their areas in Things 3.",
	}, service.listProjects)
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "search_things_tasks",
		Description: "Search tasks in Things 3 across any scope (today, inbox, anytime, someday, all) by title, project, area, or tag.",
	}, service.searchTasks)
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "create_things_project",
		Description: "Create a new project in Things 3.",
	}, service.createProject)
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "update_things_task",
		Description: "Update, reschedule, or add notes/checklists to an existing task in Things 3.",
	}, service.updateTask)

	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return mcpServer
	}, &mcp.StreamableHTTPOptions{
		Stateless:                    true,
		JSONResponse:                 true,
		DisableLocalhostProtection:   true,
		MaxRequestBodyBytes:          maxRequestBodyBytes,
		PropagateRequestCancellation: true,
	})

	originProtection := http.NewCrossOriginProtection()
	for _, origin := range config.AllowedOrigins {
		if err := originProtection.AddTrustedOrigin(origin); err != nil {
			return nil, fmt.Errorf("add trusted origin %q: %w", origin, err)
		}
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", securityHeaders(bearerAuth(config.PublicToken, originProtection.Handler(mcpHandler))))
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

type service struct {
	queue  Queue
	config Config
}

func (s *service) captureTask(ctx context.Context, _ *mcp.CallToolRequest, task capture.Request) (*mcp.CallToolResult, CaptureResult, error) {
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
		provided := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		if provided == request.Header.Get("Authorization") || !secureEqual(provided, token) {
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
