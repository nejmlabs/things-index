package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nejmlabs/things-index/internal/queue"
	"github.com/nejmlabs/things-index/internal/worker"
)

const (
	testPublicToken = "public-token-00000000000000000000"
	testWorkerToken = "worker-token-00000000000000000000"
)

func TestMCPAndWorkerRoundTrip(t *testing.T) {
	store, err := queue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	handler, err := NewHandler(store, Config{
		PublicToken:    testPublicToken,
		WorkerToken:    testWorkerToken,
		WaitForResult:  5 * time.Millisecond,
		WorkerLongPoll: 5 * time.Millisecond,
		PollInterval:   time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	inMemoryTransport := handlerTransport{handler: handler}

	client := mcp.NewClient(&mcp.Implementation{Name: "things-index-test", Version: "1"}, nil)
	httpClient := &http.Client{Transport: authTransport{token: testPublicToken, base: inMemoryTransport}}
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             "http://things-index.test/mcp",
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	captureResult, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "capture_things_task",
		Arguments: map[string]any{"title": "Buy milk"},
	})
	if err != nil {
		t.Fatal(err)
	}
	queued := decodeCaptureResult(t, captureResult)
	if queued.Status != "queued" || !validID(queued.RequestID) {
		t.Fatalf("unexpected queued result: %+v", queued)
	}

	workerClient, err := worker.NewClient(worker.ClientConfig{
		BaseURL: "http://127.0.0.1", Token: testWorkerToken,
		HTTPClient: &http.Client{Transport: inMemoryTransport},
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := workerClient.Lease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if lease == nil || lease.ID != queued.RequestID || lease.Task.Title != "Buy milk" {
		t.Fatalf("unexpected worker lease: %+v", lease)
	}
	if err := workerClient.Complete(context.Background(), *lease, worker.Outcome{ThingsID: "things-id-1"}); err != nil {
		t.Fatal(err)
	}

	statusResult, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "things_capture_status", Arguments: map[string]any{"request_id": queued.RequestID},
	})
	if err != nil {
		t.Fatal(err)
	}
	created := decodeCaptureResult(t, statusResult)
	if created.Status != "created" || created.ThingsID != "things-id-1" {
		t.Fatalf("unexpected completed result: %+v", created)
	}
}

func TestBearerTokensAreSeparated(t *testing.T) {
	store, err := queue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	handler, err := NewHandler(store, Config{PublicToken: testPublicToken, WorkerToken: testWorkerToken})
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/worker/v1/lease", nil)
	request.Header.Set("Authorization", "Bearer "+testPublicToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("public token reached worker API: status %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer "+testWorkerToken)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("worker token reached MCP API: status %d", response.Code)
	}
}

func TestWorkerPing(t *testing.T) {
	store, err := queue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	handler, err := NewHandler(store, Config{PublicToken: testPublicToken, WorkerToken: testWorkerToken})
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/worker/v1/ping", nil)
	request.Header.Set("Authorization", "Bearer "+testWorkerToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("worker ping status = %d, want %d", response.Code, http.StatusNoContent)
	}

	request = httptest.NewRequest(http.MethodGet, "/worker/v1/ping", nil)
	request.Header.Set("Authorization", "Bearer "+testPublicToken)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("public token pinged worker API: status %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/worker/v1/ping", nil)
	request.Header.Set("Authorization", "Bearer "+testWorkerToken)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST ping status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
}

func TestNewHandlerRejectsWeakOrSharedTokens(t *testing.T) {
	store, err := queue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := NewHandler(store, Config{PublicToken: "short", WorkerToken: testWorkerToken}); err == nil {
		t.Fatal("weak public token was accepted")
	}
	if _, err := NewHandler(store, Config{PublicToken: testPublicToken, WorkerToken: testPublicToken}); err == nil {
		t.Fatal("shared public and worker token was accepted")
	}
}

func TestMCPIdempotency(t *testing.T) {
	store, err := queue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	handler, err := NewHandler(store, Config{
		PublicToken:    testPublicToken,
		WorkerToken:    testWorkerToken,
		WaitForResult:  5 * time.Millisecond,
		WorkerLongPoll: 5 * time.Millisecond,
		PollInterval:   time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	inMemoryTransport := handlerTransport{handler: handler}

	client := mcp.NewClient(&mcp.Implementation{Name: "things-index-test", Version: "1"}, nil)
	httpClient := &http.Client{Transport: authTransport{token: testPublicToken, base: inMemoryTransport}}
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             "http://things-index.test/mcp",
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	firstResult, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "capture_things_task",
		Arguments: map[string]any{"title": "Buy milk", "idempotency_key": "pebble-event-001"},
	})
	if err != nil {
		t.Fatal(err)
	}
	firstQueued := decodeCaptureResult(t, firstResult)

	secondResult, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "capture_things_task",
		Arguments: map[string]any{"title": "Buy milk", "idempotency_key": "pebble-event-001"},
	})
	if err != nil {
		t.Fatal(err)
	}
	secondQueued := decodeCaptureResult(t, secondResult)

	if firstQueued.RequestID != secondQueued.RequestID {
		t.Fatalf("expected identical request ID %q, got %q", firstQueued.RequestID, secondQueued.RequestID)
	}
}

type authTransport struct {
	token string
	base  http.RoundTripper
}

func (transport authTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	copy := request.Clone(request.Context())
	copy.Header.Set("Authorization", "Bearer "+transport.token)
	return transport.base.RoundTrip(copy)
}

type handlerTransport struct {
	handler http.Handler
}

func (transport handlerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	transport.handler.ServeHTTP(recorder, request)
	response := recorder.Result()
	response.Request = request
	return response, nil
}

func decodeCaptureResult(t *testing.T, result *mcp.CallToolResult) CaptureResult {
	t.Helper()
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var decoded CaptureResult
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

// Strict MCP client runtimes reject tools whose schemas use JSON Schema type
// arrays (e.g. ["null","object"]) when converting to LLM function-call
// formats, so every registered tool must expose single-type schemas only.
func TestToolSchemasUseSingleTypes(t *testing.T) {
	store, err := queue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	handler, err := NewHandler(store, Config{
		PublicToken: testPublicToken,
		WorkerToken: testWorkerToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	inMemoryTransport := handlerTransport{handler: handler}

	client := mcp.NewClient(&mcp.Implementation{Name: "things-index-test", Version: "1"}, nil)
	httpClient := &http.Client{Transport: authTransport{token: testPublicToken, base: inMemoryTransport}}
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             "http://things-index.test/mcp",
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) == 0 {
		t.Fatal("no tools registered")
	}
	var walk func(toolName string, node any)
	walk = func(toolName string, node any) {
		switch value := node.(type) {
		case map[string]any:
			if listed, ok := value["type"].([]any); ok {
				t.Errorf("tool %s schema uses type array %v", toolName, listed)
			}
			for _, child := range value {
				walk(toolName, child)
			}
		case []any:
			for _, child := range value {
				walk(toolName, child)
			}
		}
	}
	for _, tool := range tools.Tools {
		for _, schema := range []any{tool.InputSchema, tool.OutputSchema} {
			raw, err := json.Marshal(schema)
			if err != nil {
				t.Fatal(err)
			}
			var decoded any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			walk(tool.Name, decoded)
		}
	}
}

// Clients like Pebble Index send "Accept: application/json" without the
// event-stream type the SDK insists on; the endpoint must serve them anyway.
func TestPlainJSONAcceptHeaderServed(t *testing.T) {
	store, err := queue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	handler, err := NewHandler(store, Config{PublicToken: testPublicToken, WorkerToken: testWorkerToken})
	if err != nil {
		t.Fatal(err)
	}

	for _, accept := range []string{"application/json", "application/json, text/event-stream", "*/*", ""} {
		request := httptest.NewRequest(http.MethodPost, "http://things-index.test/mcp",
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		request.Header.Set("Authorization", "Bearer "+testPublicToken)
		request.Header.Set("Content-Type", "application/json")
		if accept != "" {
			request.Header.Set("Accept", accept)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Errorf("Accept %q: got status %d, body %s", accept, recorder.Code, recorder.Body.String())
		}
	}
}

// Clients that open the standalone GET event stream (Pebble Index does,
// immediately after initialize) must get a live stream, not the stateless
// handler's 405 - they treat that as a broken server and restart the
// handshake.
func TestStandaloneSSEStreamServed(t *testing.T) {
	store, err := queue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	handler, err := NewHandler(store, Config{PublicToken: testPublicToken, WorkerToken: testWorkerToken})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testPublicToken)
	request.Header.Set("Accept", "application/json,text/event-stream")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200", response.StatusCode)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.Contains(contentType, "text/event-stream") {
		t.Fatalf("got content type %q, want text/event-stream", contentType)
	}
}

// The SDK stamps draft ttlMs/cacheScope extension fields onto list results;
// strict client deserializers reject unknown keys, so they must be stripped.
func TestResultsCarryNoCacheExtensions(t *testing.T) {
	store, err := queue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	handler, err := NewHandler(store, Config{PublicToken: testPublicToken, WorkerToken: testWorkerToken})
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "http://things-index.test/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	request.Header.Set("Authorization", "Bearer "+testPublicToken)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if strings.Contains(body, "ttlMs") || strings.Contains(body, "cacheScope") {
		t.Fatalf("cache extension fields still present: %s", body[:200])
	}
	if !strings.Contains(body, "capture_things_task") {
		t.Fatal("tools missing from rewritten response")
	}
}
