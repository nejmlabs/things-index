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
	Status    string   `json:"status" jsonschema:"Capture state: queued, created, or failed."`
	RequestID string   `json:"request_id" jsonschema:"Stable request identifier for status checks."`
	ThingsID  string   `json:"things_id,omitempty" jsonschema:"Things task identifier, once created."`
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
