package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nejmlabs/things-index/internal/capture"
	"github.com/nejmlabs/things-index/internal/helper"
	"github.com/nejmlabs/things-index/internal/journal"
	"github.com/nejmlabs/things-index/internal/queue"
	"github.com/nejmlabs/things-index/internal/server"
	"github.com/nejmlabs/things-index/internal/serverapp"
	"github.com/nejmlabs/things-index/internal/toolschema"
	"github.com/nejmlabs/things-index/internal/worker"
	"github.com/nejmlabs/things-index/internal/workerapp"
	shortcutasset "github.com/nejmlabs/things-index/shortcuts"
)

const version = "0.2.2"

func main() {
	if len(os.Args) < 2 {
		if err := runDefault(); err != nil && !errors.Is(err, context.Canceled) {
			log.Fatal(err)
		}
		return
	}

	command := os.Args[1]
	var err error
	switch command {
	case "start", "run", "local":
		err = runStandaloneHTTP()
	case "stdio":
		err = runStdio()
	case "server":
		err = runDedicatedServer()
	case "worker":
		if len(os.Args) >= 3 && (os.Args[2] == "--setup" || os.Args[2] == "setup" || os.Args[2] == "-s") {
			err = runWorkerSetup()
		} else {
			err = runDedicatedWorker()
		}
	case "config":
		err = printConfig()
	case "update":
		err = runUpdate(os.Args[2:])
	case "uninstall", "teardown":
		err = runUninstall()
	case "--version", "-v", "version":
		fmt.Printf("things-index v%s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
	case "--help", "-h", "help":
		printHelp()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", command)
		printHelp()
		os.Exit(1)
	}

	if err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

func runDefault() error {
	if runtime.GOOS == "darwin" {
		return runStandaloneHTTP()
	}
	return runDedicatedServer()
}

func runStandaloneHTTP() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	listenAddr := os.Getenv("THINGS_INDEX_LISTEN_ADDR")
	if listenAddr == "" {
		listenAddr = "127.0.0.1:8080"
	}
	if err := serverapp.ValidateListenAddress(listenAddr); err != nil {
		return err
	}

	stateDir := os.Getenv("THINGS_INDEX_STATE_DIR")
	if stateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		stateDir = filepath.Join(home, "Library", "Application Support", "ThingsIndex")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}

	dbPath := os.Getenv("THINGS_INDEX_DB_PATH")
	if dbPath == "" {
		dbPath = filepath.Join(stateDir, "local-queue.sqlite")
	}

	publicToken := os.Getenv("THINGS_INDEX_PUBLIC_TOKEN")
	if publicToken == "" {
		var err error
		publicToken, err = generateToken()
		if err != nil {
			return fmt.Errorf("generate public token: %w", err)
		}
	}

	workerToken, err := generateToken()
	if err != nil {
		return fmt.Errorf("generate worker token: %w", err)
	}

	queueStore, err := queue.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open queue: %w", err)
	}
	defer queueStore.Close()

	srvHandler, err := server.NewHandler(queueStore, server.Config{
		PublicToken: publicToken,
		WorkerToken: workerToken,
		Version:     version,
	})
	if err != nil {
		return fmt.Errorf("create server: %w", err)
	}

	httpServer := &http.Server{
		Addr:              listenAddr,
		Handler:           srvHandler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       35 * time.Second,
		WriteTimeout:      35 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serverURL := "http://" + displayHost(listenAddr)

	captureAdapter := helper.NewClient(os.Getenv("THINGS_INDEX_THINGS_AUTH_TOKEN"))
	if thingsDB := os.Getenv("THINGS_INDEX_THINGS_DB_PATH"); thingsDB != "" {
		captureAdapter.DBPath = thingsDB
	}
	if err := captureAdapter.Ping(ctx); err != nil {
		log.Printf("⚠️  Warning: Could not connect to Things 3 database: %v", err)
	}

	journalPath := filepath.Join(stateDir, "journal.sqlite")
	journalStore, err := journal.Open(journalPath)
	if err != nil {
		return fmt.Errorf("open journal: %w", err)
	}
	defer journalStore.Close()

	processor := &worker.Processor{Helper: captureAdapter, Journal: journalStore}

	// Start the embedded worker loop in the background. It shares the process
	// with the queue, so it leases directly from the store instead of going
	// through the worker HTTP API.
	go func() {
		const maxAttempts = 5
		const leaseDuration = 90 * time.Second
		for ctx.Err() == nil {
			job, found := leaseNextJob(ctx, queueStore, maxAttempts, leaseDuration)
			if !found {
				select {
				case <-time.After(500 * time.Millisecond):
				case <-ctx.Done():
					return
				}
				continue
			}
			outcome, processErr := processor.Process(ctx, worker.Job{ID: job.ID, Task: job.Task})
			if processErr != nil {
				_ = queueStore.Fail(ctx, job.ID, job.LeaseToken, processErr.Error(), worker.IsRetryable(processErr))
			} else {
				_ = queueStore.Complete(ctx, job.ID, job.LeaseToken, outcome.ThingsID, outcome.Warnings)
				if worker.UsesJournal(job.Task) {
					_ = journalStore.MarkReported(ctx, job.ID)
				}
			}
		}
	}()

	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  🚀 ThingsIndex Standalone Mode (All-in-One Local Mac)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Printf("  • Server Endpoint: %s/mcp\n", serverURL)
	fmt.Printf("  • Bearer Token:    %s\n", publicToken)
	fmt.Println("─────────────────────────────────────────────────────────────────")
	fmt.Println("  Paste into Claude Desktop / Pebble Index config:")
	fmt.Println()
	fmt.Printf(`  {
    "mcpServers": {
      "things": {
        "url": "%s/mcp",
        "headers": {
          "Authorization": "Bearer %s"
        }
      }
    }
  }`, serverURL, publicToken)
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Printf("ThingsIndex listening on %s... (Press Ctrl+C to stop)\n", listenAddr)

	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return err
	}
	defer listener.Close()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// mustAddTool registers a tool through the shared schema normalization so
// the stdio server advertises exactly the schemas the HTTP server does. A
// registration error is a programmer error caught on first run.
func mustAddTool[In, Out any](server *mcp.Server, tool *mcp.Tool, handler mcp.ToolHandlerFor[In, Out]) {
	if err := toolschema.AddTool(server, tool, handler); err != nil {
		log.Fatalf("register tool: %v", err)
	}
}

func runStdio() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	captureAdapter := helper.NewClient(os.Getenv("THINGS_INDEX_THINGS_AUTH_TOKEN"))
	if thingsDB := os.Getenv("THINGS_INDEX_THINGS_DB_PATH"); thingsDB != "" {
		captureAdapter.DBPath = thingsDB
	}

	mcpServer := mcp.NewServer(&mcp.Implementation{
		Name:    "things-index",
		Version: version,
	}, &mcp.ServerOptions{
		Instructions: "Capture tasks directly in Things 3 on this Mac.",
	})

	mustAddTool(mcpServer, &mcp.Tool{
		Name:        "capture_things_task",
		Description: "Create one task in Things 3 on this Mac with zero prompts and zero window focus steal.",
	}, func(callCtx context.Context, _ *mcp.CallToolRequest, input capture.TaskFields) (*mcp.CallToolResult, struct {
		RequestID string `json:"request_id"`
		Status    string `json:"status"`
		ThingsID  string `json:"things_id,omitempty"`
	}, error) {
		task := capture.Request{TaskFields: input}
		if err := task.Validate(); err != nil {
			return nil, struct {
				RequestID string `json:"request_id"`
				Status    string `json:"status"`
				ThingsID  string `json:"things_id,omitempty"`
			}{}, fmt.Errorf("invalid Things task: %w", err)
		}

		reqID := randomHex(16)
		resp, err := captureAdapter.Capture(callCtx, reqID, task)
		if err != nil {
			return nil, struct {
				RequestID string `json:"request_id"`
				Status    string `json:"status"`
				ThingsID  string `json:"things_id,omitempty"`
			}{}, fmt.Errorf("capture task in Things 3: %w", err)
		}

		return nil, struct {
			RequestID string `json:"request_id"`
			Status    string `json:"status"`
			ThingsID  string `json:"things_id,omitempty"`
		}{
			RequestID: reqID,
			Status:    "created",
			ThingsID:  resp.ID,
		}, nil
	})

	mustAddTool(mcpServer, &mcp.Tool{
		Name:        "create_things_heading",
		Description: "Create a new section heading inside a Things 3 project.",
	}, func(callCtx context.Context, _ *mcp.CallToolRequest, input capture.HeadingRequest) (*mcp.CallToolResult, struct {
		Status   string `json:"status"`
		ThingsID string `json:"things_id,omitempty"`
	}, error) {
		if err := input.Validate(); err != nil {
			return nil, struct {
				Status   string `json:"status"`
				ThingsID string `json:"things_id,omitempty"`
			}{}, fmt.Errorf("invalid heading request: %w", err)
		}
		resp, err := captureAdapter.CreateHeading(callCtx, input.Project, input.Heading)
		if err != nil {
			return nil, struct {
				Status   string `json:"status"`
				ThingsID string `json:"things_id,omitempty"`
			}{}, fmt.Errorf("create heading: %w", err)
		}
		return nil, struct {
			Status   string `json:"status"`
			ThingsID string `json:"things_id,omitempty"`
		}{
			Status:   "created",
			ThingsID: resp.ID,
		}, nil
	})

	mustAddTool(mcpServer, &mcp.Tool{
		Name:        "archive_things_heading",
		Description: "Archive/hide a section heading from an active Things 3 project.",
	}, func(callCtx context.Context, _ *mcp.CallToolRequest, input capture.HeadingRequest) (*mcp.CallToolResult, struct {
		Status   string `json:"status"`
		ThingsID string `json:"things_id,omitempty"`
	}, error) {
		if err := input.Validate(); err != nil {
			return nil, struct {
				Status   string `json:"status"`
				ThingsID string `json:"things_id,omitempty"`
			}{}, fmt.Errorf("invalid heading request: %w", err)
		}
		resp, err := captureAdapter.ArchiveHeading(callCtx, input.Project, input.Heading)
		if err != nil {
			return nil, struct {
				Status   string `json:"status"`
				ThingsID string `json:"things_id,omitempty"`
			}{}, fmt.Errorf("archive heading: %w", err)
		}
		return nil, struct {
			Status   string `json:"status"`
			ThingsID string `json:"things_id,omitempty"`
		}{
			Status:   "archived",
			ThingsID: resp.ID,
		}, nil
	})

	mustAddTool(mcpServer, &mcp.Tool{
		Name:        "rename_things_heading",
		Description: "Rename an existing section heading inside a Things 3 project.",
	}, func(callCtx context.Context, _ *mcp.CallToolRequest, input capture.HeadingRequest) (*mcp.CallToolResult, struct {
		Status   string `json:"status"`
		ThingsID string `json:"things_id,omitempty"`
	}, error) {
		if err := input.Validate(); err != nil {
			return nil, struct {
				Status   string `json:"status"`
				ThingsID string `json:"things_id,omitempty"`
			}{}, fmt.Errorf("invalid heading request: %w", err)
		}
		if strings.TrimSpace(input.NewTitle) == "" {
			return nil, struct {
				Status   string `json:"status"`
				ThingsID string `json:"things_id,omitempty"`
			}{}, errors.New("new_title is required when renaming a heading")
		}
		resp, err := captureAdapter.RenameHeading(callCtx, input.Project, input.Heading, input.NewTitle)
		if err != nil {
			return nil, struct {
				Status   string `json:"status"`
				ThingsID string `json:"things_id,omitempty"`
			}{}, fmt.Errorf("rename heading: %w", err)
		}
		return nil, struct {
			Status   string `json:"status"`
			ThingsID string `json:"things_id,omitempty"`
		}{
			Status:   "renamed",
			ThingsID: resp.ID,
		}, nil
	})

	mustAddTool(mcpServer, &mcp.Tool{
		Name:        "archive_things_task",
		Description: "Archive a task in Things 3 (mark completed, canceled, or move to trash).",
	}, func(callCtx context.Context, _ *mcp.CallToolRequest, input capture.ArchiveTaskRequest) (*mcp.CallToolResult, struct {
		Status   string `json:"status"`
		ThingsID string `json:"things_id,omitempty"`
	}, error) {
		if err := input.Validate(); err != nil {
			return nil, struct {
				Status   string `json:"status"`
				ThingsID string `json:"things_id,omitempty"`
			}{}, fmt.Errorf("invalid archive task request: %w", err)
		}
		resp, err := captureAdapter.ArchiveTask(callCtx, input.ID, input.Title, input.Project, input.Action)
		if err != nil {
			return nil, struct {
				Status   string `json:"status"`
				ThingsID string `json:"things_id,omitempty"`
			}{}, fmt.Errorf("archive task: %w", err)
		}
		return nil, struct {
			Status   string `json:"status"`
			ThingsID string `json:"things_id,omitempty"`
		}{
			Status:   "archived",
			ThingsID: resp.ID,
		}, nil
	})

	mustAddTool(mcpServer, &mcp.Tool{
		Name:        "archive_things_project",
		Description: "Archive an entire project in Things 3 (mark completed or canceled).",
	}, func(callCtx context.Context, _ *mcp.CallToolRequest, input capture.ArchiveProjectRequest) (*mcp.CallToolResult, struct {
		Status   string `json:"status"`
		ThingsID string `json:"things_id,omitempty"`
	}, error) {
		if err := input.Validate(); err != nil {
			return nil, struct {
				Status   string `json:"status"`
				ThingsID string `json:"things_id,omitempty"`
			}{}, fmt.Errorf("invalid archive project request: %w", err)
		}
		resp, err := captureAdapter.ArchiveProject(callCtx, input.ID, input.Name, input.Action)
		if err != nil {
			return nil, struct {
				Status   string `json:"status"`
				ThingsID string `json:"things_id,omitempty"`
			}{}, fmt.Errorf("archive project: %w", err)
		}
		return nil, struct {
			Status   string `json:"status"`
			ThingsID string `json:"things_id,omitempty"`
		}{
			Status:   "archived",
			ThingsID: resp.ID,
		}, nil
	})

	mustAddTool(mcpServer, &mcp.Tool{
		Name:        "get_things_today",
		Description: "Get all tasks scheduled for Today in Things 3.",
	}, func(callCtx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		resp, err := captureAdapter.QueryTasks(callCtx, capture.QueryTasksRequest{Scope: "today"})
		if err != nil {
			return nil, nil, fmt.Errorf("get today: %w", err)
		}
		var items any
		_ = json.Unmarshal([]byte(resp.ID), &items)
		return nil, items, nil
	})

	mustAddTool(mcpServer, &mcp.Tool{
		Name:        "get_things_inbox",
		Description: "Get all unorganized tasks in Things 3 Inbox.",
	}, func(callCtx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		resp, err := captureAdapter.QueryTasks(callCtx, capture.QueryTasksRequest{Scope: "inbox"})
		if err != nil {
			return nil, nil, fmt.Errorf("get inbox: %w", err)
		}
		var items any
		_ = json.Unmarshal([]byte(resp.ID), &items)
		return nil, items, nil
	})

	mustAddTool(mcpServer, &mcp.Tool{
		Name:        "list_things_projects",
		Description: "List all active projects and their areas in Things 3.",
	}, func(callCtx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		resp, err := captureAdapter.QueryTasks(callCtx, capture.QueryTasksRequest{Scope: "projects"})
		if err != nil {
			return nil, nil, fmt.Errorf("list projects: %w", err)
		}
		var items any
		_ = json.Unmarshal([]byte(resp.ID), &items)
		return nil, items, nil
	})

	mustAddTool(mcpServer, &mcp.Tool{
		Name:        "search_things_tasks",
		Description: "Search tasks in Things 3 across any scope (today, inbox, anytime, someday, all) by title, project, area, or tag.",
	}, func(callCtx context.Context, _ *mcp.CallToolRequest, input capture.QueryTasksRequest) (*mcp.CallToolResult, any, error) {
		resp, err := captureAdapter.QueryTasks(callCtx, input)
		if err != nil {
			return nil, nil, fmt.Errorf("search tasks: %w", err)
		}
		var items any
		_ = json.Unmarshal([]byte(resp.ID), &items)
		return nil, items, nil
	})

	mustAddTool(mcpServer, &mcp.Tool{
		Name:        "create_things_project",
		Description: "Create a new project in Things 3.",
	}, func(callCtx context.Context, _ *mcp.CallToolRequest, input capture.CreateProjectRequest) (*mcp.CallToolResult, struct {
		Status   string `json:"status"`
		ThingsID string `json:"things_id,omitempty"`
	}, error) {
		if err := input.Validate(); err != nil {
			return nil, struct {
				Status   string `json:"status"`
				ThingsID string `json:"things_id,omitempty"`
			}{}, fmt.Errorf("invalid create project request: %w", err)
		}
		resp, err := captureAdapter.CreateProject(callCtx, input)
		if err != nil {
			return nil, struct {
				Status   string `json:"status"`
				ThingsID string `json:"things_id,omitempty"`
			}{}, fmt.Errorf("create project: %w", err)
		}
		return nil, struct {
			Status   string `json:"status"`
			ThingsID string `json:"things_id,omitempty"`
		}{
			Status:   "created",
			ThingsID: resp.ID,
		}, nil
	})

	mustAddTool(mcpServer, &mcp.Tool{
		Name:        "update_things_task",
		Description: "Update, reschedule, or add notes/checklists to an existing task in Things 3.",
	}, func(callCtx context.Context, _ *mcp.CallToolRequest, input capture.UpdateTaskRequest) (*mcp.CallToolResult, struct {
		Status   string `json:"status"`
		ThingsID string `json:"things_id,omitempty"`
	}, error) {
		if err := input.Validate(); err != nil {
			return nil, struct {
				Status   string `json:"status"`
				ThingsID string `json:"things_id,omitempty"`
			}{}, fmt.Errorf("invalid update task request: %w", err)
		}
		resp, err := captureAdapter.UpdateTask(callCtx, input)
		if err != nil {
			return nil, struct {
				Status   string `json:"status"`
				ThingsID string `json:"things_id,omitempty"`
			}{}, fmt.Errorf("update task: %w", err)
		}
		return nil, struct {
			Status   string `json:"status"`
			ThingsID string `json:"things_id,omitempty"`
		}{
			Status:   "updated",
			ThingsID: resp.ID,
		}, nil
	})

	return mcpServer.Run(ctx, &mcp.StdioTransport{})
}

func runDedicatedServer() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serverapp.Run(ctx)
}

func runDedicatedWorker() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return workerapp.Run(ctx)
}

// workerLaunchAgentLabel matches the plist name the uninstaller removes and
// the deploy/launchd example documents.
const workerLaunchAgentLabel = "com.nejmlabs.things-index-worker"

func runWorkerSetup() error {
	if runtime.GOOS != "darwin" {
		return errors.New("the Mac worker setup wizard must be run on macOS")
	}

	reader := bufio.NewReader(os.Stdin)
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  🛠️  ThingsIndex Mac Worker Setup Wizard")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// 1. Prompt for Server URL
	defaultServer := os.Getenv("THINGS_INDEX_SERVER_URL")
	if defaultServer == "" {
		defaultServer = "http://127.0.0.1:8080"
	}
	fmt.Printf("• Enter Server URL [%s]: ", defaultServer)
	serverURLInput, _ := reader.ReadString('\n')
	serverURL := strings.TrimSpace(serverURLInput)
	if serverURL == "" {
		serverURL = defaultServer
	}

	// 2. Prompt for Worker Token
	defaultToken := os.Getenv("THINGS_INDEX_WORKER_TOKEN")
	fmt.Print("• Enter Worker Token: ")
	tokenInput, _ := reader.ReadString('\n')
	workerToken := strings.TrimSpace(tokenInput)
	if workerToken == "" {
		workerToken = defaultToken
	}
	if len(workerToken) < 32 {
		return errors.New("worker token must be at least 32 characters long")
	}

	// 3. Validate the URL/token against the same rules the worker daemon
	// enforces at startup, so the wizard cannot bless a configuration the
	// installed worker will refuse.
	serverClient, err := worker.NewClient(worker.ClientConfig{BaseURL: serverURL, Token: workerToken})
	if err != nil {
		fmt.Println()
		fmt.Println("  ✗ This server URL cannot be used by the worker daemon:")
		fmt.Printf("    %v\n", err)
		fmt.Println()
		fmt.Println("  The worker requires HTTPS unless the server is reached via a literal")
		fmt.Println("  loopback IP. For a homelab server on plain HTTP, either:")
		fmt.Println("    • put it behind an HTTPS reverse proxy or tunnel (worked examples in deploy/), or")
		fmt.Println("    • keep an SSH tunnel open: ssh -N -L 8080:<server-ip>:8080 <user>@<server-host>")
		fmt.Println("      and enter http://127.0.0.1:8080 here instead.")
		return errors.New("worker setup aborted: server URL rejected")
	}

	// 4. Verify the connection and the token against the authenticated worker
	// API, so a mistyped token fails here instead of invisibly at boot.
	fmt.Printf("• Checking server connection to %s...\n", serverURL)
	pingCtx, cancelPing := context.WithTimeout(context.Background(), 5*time.Second)
	pingErr := serverClient.Ping(pingCtx)
	cancelPing()
	switch {
	case pingErr == nil:
		fmt.Println("  ✓ Server connection and worker token verified!")
	case errors.Is(pingErr, worker.ErrUnauthorized):
		return errors.New("the server rejected this worker token; copy THINGS_INDEX_WORKER_TOKEN from the server's configuration and run the wizard again")
	default:
		// Fall back to the unauthenticated health endpoint: an older server
		// build has no ping route, and an offline server should not block
		// setup — queued jobs simply wait for it to come back.
		httpClient := &http.Client{Timeout: 5 * time.Second}
		healthURL := strings.TrimRight(serverURL, "/") + "/healthz"
		if resp, healthErr := httpClient.Get(healthURL); healthErr == nil {
			_ = resp.Body.Close()
			fmt.Println("  ⚠️  Server reachable, but it could not verify the worker token (older server build?). Continuing.")
		} else {
			fmt.Printf("  ⚠️  Could not reach the server: %v (Continuing anyway)\n", pingErr)
		}
	}

	// 5. Optional Things URL-scheme auth token, which unlocks the update
	// operations the URL scheme gates behind it.
	defaultThingsToken := os.Getenv("THINGS_INDEX_THINGS_AUTH_TOKEN")
	thingsTokenPrompt := "• Things auth token (optional, unlocks deadline/tag/checklist updates)"
	if defaultThingsToken != "" {
		thingsTokenPrompt += " [detected in environment]"
	}
	fmt.Print(thingsTokenPrompt + ": ")
	thingsTokenInput, _ := reader.ReadString('\n')
	thingsAuthToken := strings.TrimSpace(thingsTokenInput)
	if thingsAuthToken == "" {
		thingsAuthToken = defaultThingsToken
	}
	if strings.ContainsAny(thingsAuthToken, " \t") {
		return errors.New("the Things auth token must not contain whitespace; copy it from Things > Settings > General > Enable Things URLs > Manage")
	}
	if thingsAuthToken == "" {
		fmt.Println("  • Skipped. update_things_task stays limited to title/notes/today;")
		fmt.Println("    rerun the wizard anytime with the token from Things > Settings >")
		fmt.Println("    General > Enable Things URLs > Manage to unlock the rest.")
	}

	// 6. Auto-detect Things 3 SQLite Database
	fmt.Println("• Detecting Things 3 SQLite database...")
	thingsDB, err := helper.FindThingsDatabase()
	if err != nil {
		return fmt.Errorf("Things 3 database not found: %w", err)
	}
	fmt.Printf("  ✓ Found database: %s\n", thingsDB)

	// 7. Test Things 3 Access
	captureAdapter := helper.NewClient("")
	captureAdapter.DBPath = thingsDB
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := captureAdapter.Ping(ctx); err != nil {
		return fmt.Errorf("failed to query Things 3 database: %w", err)
	}
	fmt.Println("  ✓ Things 3 database query verified (Read-Only OK)")

	// 8. Validate the Things auth token by creating and finalising one
	// disposable task through the URL scheme; a bad token fails here
	// (finalise_unverified) instead of raising Things error dialogs during
	// background operation. Things is pre-launched hidden so the capture path
	// never reaches its quit-on-exit AppleScript — that automation dialog
	// must belong to the daemon, not this terminal.
	if thingsAuthToken != "" {
		fmt.Println("• Validating the Things auth token with a disposable test task...")
		_ = exec.Command("/usr/bin/open", "-g", "-j", "-a", "/Applications/Things3.app").Run()
		for attempt := 0; attempt < 20; attempt++ {
			if exec.Command("/usr/bin/pgrep", "-x", "Things3").Run() == nil {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		time.Sleep(time.Second)

		verifier := helper.NewClient(thingsAuthToken)
		verifier.DBPath = thingsDB
		testCtx, cancelTest := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancelTest()
		const testTitle = "ThingsIndex setup test — safe to delete"
		testID := randomHex(16)
		_, err := verifier.Capture(testCtx, testID, capture.Request{TaskFields: capture.TaskFields{
			Title: testTitle,
			Notes: "Created by things-index worker --setup to validate the Things auth token.",
		}})
		if err != nil {
			// A slow first launch can outlast the capture poll; reconcile the
			// pending task the same way the worker does before giving up.
			time.Sleep(3 * time.Second)
			if ids, findErr := verifier.FindCapture(testCtx, testID); findErr == nil && len(ids) == 1 {
				err = verifier.FinaliseCapture(testCtx, ids[0], testTitle)
			}
		}
		if err != nil {
			return fmt.Errorf("the Things auth token failed validation (copy it from Things > Settings > General > Enable Things URLs > Manage): %w", err)
		}
		fmt.Printf("  ✓ Token verified; delete the Inbox task %q when convenient.\n", testTitle)
	}

	// 9. Install the bundled ThingsIndex Helper shortcut — heading operations
	// run through it, and Apple's CLI cannot install shortcuts silently, so
	// this needs one click in the Shortcuts app.
	if err := installHelperShortcut(); err != nil {
		return err
	}

	// 10. Settle the Shortcut's one-time privacy dialogs now via its harmless
	// ping; the grants are stored per shortcut, so they cover the daemon's
	// runs too.
	fmt.Println("• Verifying the helper shortcut (choose “Always Allow” on any privacy dialogs)...")
	shortcutCtx, cancelShortcut := context.WithTimeout(context.Background(), 3*time.Minute)
	err = captureAdapter.PingHelperShortcut(shortcutCtx)
	cancelShortcut()
	if err != nil {
		return fmt.Errorf("the helper shortcut did not answer its ping (approve its privacy dialogs and rerun the wizard): %w", err)
	}
	fmt.Println("  ✓ Helper shortcut verified; its privacy grants are settled.")

	// 11. Install Background Launcher Script (carries the secrets, hence 0700)
	home, _ := os.UserHomeDir()
	binDir := filepath.Join(home, ".local", "bin")
	_ = os.MkdirAll(binDir, 0o755)

	exePath, err := os.Executable()
	if err != nil {
		exePath = "/usr/local/bin/things-index"
	}

	launcherScript := filepath.Join(binDir, "run-things-worker.sh")
	scriptContent := buildLauncherScript(home, serverURL, workerToken, thingsDB, thingsAuthToken, exePath)
	if err := os.WriteFile(launcherScript, []byte(scriptContent), 0o700); err != nil {
		return fmt.Errorf("write launcher script: %w", err)
	}
	fmt.Printf("  ✓ Created launcher script: %s\n", launcherScript)

	// 12. Install the LaunchAgent: starts at login, KeepAlive restarts the
	// worker if it ever crashes, and logs land in ~/Library/Logs/ThingsIndex.
	logDir := filepath.Join(home, "Library", "Logs", "ThingsIndex")
	_ = os.MkdirAll(logDir, 0o755)
	agentsDir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		return fmt.Errorf("create LaunchAgents directory: %w", err)
	}
	plistPath := filepath.Join(agentsDir, workerLaunchAgentLabel+".plist")
	if err := os.WriteFile(plistPath, []byte(buildLaunchAgentPlist(launcherScript, logDir)), 0o644); err != nil {
		return fmt.Errorf("write LaunchAgent: %w", err)
	}
	fmt.Printf("  ✓ Created LaunchAgent: %s\n", plistPath)

	// 13. Replace any previous install (LaunchAgent or the cron+screen
	// mechanism earlier wizard versions used) and start the agent. Deleting
	// the consent marker forces the daemon to re-run its automation
	// preflight, so the Things 3 grant lands on this (possibly rebuilt)
	// binary rather than being assumed from an older install.
	if markerPath, err := workerapp.AutomationConsentMarkerPath(); err == nil {
		_ = os.Remove(markerPath)
	}
	fmt.Println("• Starting background worker via launchd...")
	fmt.Println("  macOS will show up to two permission dialogs for “things-index” —")
	fmt.Println("  “access data from other apps” and “control Things3”. Approve both.")
	domainTarget := fmt.Sprintf("gui/%d", os.Getuid())
	_ = exec.Command("launchctl", "bootout", domainTarget+"/"+workerLaunchAgentLabel).Run()
	_ = exec.Command("/bin/sh", "-c", `(crontab -l 2>/dev/null | grep -v "things-worker") | crontab - 2>/dev/null || true`).Run()
	_ = exec.Command("/bin/sh", "-c", `screen -S things-worker -X quit 2>/dev/null || true`).Run()
	_ = exec.Command("/bin/sh", "-c", `pkill -f "things-index worker" 2>/dev/null || true`).Run()
	if output, err := exec.Command("launchctl", "bootstrap", domainTarget, plistPath).CombinedOutput(); err != nil {
		return fmt.Errorf("start LaunchAgent in %s: %w: %s", domainTarget, err, strings.TrimSpace(string(output)))
	}

	// 14. Confirm the worker is running AND has recorded its automation
	// consent before declaring success. macOS pgrep excludes this wizard (an
	// ancestor), so only the daemon matches.
	fmt.Println("• Waiting for the worker to start and record its automation consent...")
	markerPath, markerErr := workerapp.AutomationConsentMarkerPath()
	running := false
	consented := false
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		running = exec.Command("/usr/bin/pgrep", "-f", "things-index worker").Run() == nil
		if markerErr == nil {
			if _, err := os.Stat(markerPath); err == nil {
				consented = true
			}
		}
		if running && consented {
			break
		}
	}

	fmt.Println()
	if !running || !consented {
		fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		fmt.Println("  ⚠️  Setup finished, but the worker is not fully confirmed yet.")
		if !running {
			fmt.Println("  • The worker process has not been seen running.")
		}
		if !consented {
			fmt.Println("  • Things 3 automation consent has not been recorded — approve")
			fmt.Println("    the “control Things3” dialog if it is still on screen.")
		}
		fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		fmt.Printf("  • Check logs:   %s\n", filepath.Join(logDir, "worker-error.log"))
		fmt.Printf("  • Check status: launchctl print %s/%s\n", domainTarget, workerLaunchAgentLabel)
		fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		return nil
	}
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  🎉 ThingsIndex Mac Worker Successfully Configured & Active!")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Printf("  • Server Target:   %s\n", serverURL)
	fmt.Printf("  • Things Database: %s\n", thingsDB)
	fmt.Printf("  • LaunchAgent:     %s (starts at login, auto-restarts)\n", workerLaunchAgentLabel)
	fmt.Printf("  • Logs:            %s\n", logDir)
	fmt.Println("  • Permissions:     data access, Things automation, and Shortcut")
	fmt.Println("                     privacy grants are all settled.")
	fmt.Println("────────────────────────────────────────────────────────────")
	fmt.Println("  The worker is now actively listening in the background")
	fmt.Println("  and processes jobs prompt-free.")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	return nil
}

// installHelperShortcut puts the embedded signed shortcut into the user's
// library. Apple's shortcuts CLI cannot install (only run/list/view/sign), so
// this opens the import dialog and waits for the user's one Add click.
func installHelperShortcut() error {
	if helperShortcutInstalled() {
		fmt.Printf("  ✓ %q shortcut already installed.\n", helper.HelperShortcutName)
		return nil
	}
	fmt.Println("• Installing the ThingsIndex Helper shortcut...")
	// Shortcuts names an import after the file's basename, so the staged file
	// must carry the exact library name — a CreateTemp-style random suffix
	// would import as "ThingsIndex Helper-123456789" and never match the poll.
	tempDir, err := os.MkdirTemp("", "things-index-shortcut")
	if err != nil {
		return fmt.Errorf("stage helper shortcut: %w", err)
	}
	defer os.RemoveAll(tempDir)
	tempPath := filepath.Join(tempDir, helper.HelperShortcutName+".shortcut")
	if err := os.WriteFile(tempPath, shortcutasset.Helper(), 0o600); err != nil {
		return fmt.Errorf("write helper shortcut: %w", err)
	}
	if err := exec.Command("/usr/bin/open", tempPath).Run(); err != nil {
		return fmt.Errorf("open helper shortcut in Shortcuts: %w", err)
	}
	fmt.Println("  Shortcuts opened an import dialog — click “Add Shortcut”. Waiting...")
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if helperShortcutInstalled() {
			fmt.Printf("  ✓ %q shortcut installed.\n", helper.HelperShortcutName)
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("the %q shortcut did not appear within 3 minutes; click “Add Shortcut” in the Shortcuts app and rerun the wizard (if it imported under a different name, rename it to %q first)", helper.HelperShortcutName, helper.HelperShortcutName)
}

func helperShortcutInstalled() bool {
	output, err := exec.Command("/usr/bin/shortcuts", "list").Output()
	return err == nil && slices.Contains(strings.Split(string(output), "\n"), helper.HelperShortcutName)
}

// buildLauncherScript renders the launcher launchd runs. It carries the
// worker's secrets, so callers must write it with mode 0700.
func buildLauncherScript(home, serverURL, workerToken, thingsDB, thingsAuthToken, exePath string) string {
	var builder strings.Builder
	builder.WriteString("#!/bin/zsh -l\n")
	fmt.Fprintf(&builder, "export HOME=%s\n", shellQuote(home))
	builder.WriteString(`export PATH="/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:$HOME/.local/bin"` + "\n")
	fmt.Fprintf(&builder, "export THINGS_INDEX_SERVER_URL=%s\n", shellQuote(serverURL))
	fmt.Fprintf(&builder, "export THINGS_INDEX_WORKER_TOKEN=%s\n", shellQuote(workerToken))
	builder.WriteString(`export THINGS_INDEX_JOURNAL_PATH="$HOME/Library/Application Support/ThingsIndex/journal.sqlite"` + "\n")
	fmt.Fprintf(&builder, "export THINGS_INDEX_THINGS_DB_PATH=%s\n", shellQuote(thingsDB))
	if thingsAuthToken != "" {
		fmt.Fprintf(&builder, "export THINGS_INDEX_THINGS_AUTH_TOKEN=%s\n", shellQuote(thingsAuthToken))
	}
	builder.WriteString("\n")
	fmt.Fprintf(&builder, "exec %s worker\n", shellQuote(exePath))
	return builder.String()
}

// buildLaunchAgentPlist renders the LaunchAgent. It intentionally contains no
// secrets — those live in the 0700 launcher script it points at.
func buildLaunchAgentPlist(launcherScript, logDir string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
    </array>
    <key>LimitLoadToSessionType</key>
    <string>Aqua</string>
    <key>ProcessType</key>
    <string>Background</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>ThrottleInterval</key>
    <integer>10</integer>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>
`, workerLaunchAgentLabel, xmlEscape(launcherScript), xmlEscape(filepath.Join(logDir, "worker.log")), xmlEscape(filepath.Join(logDir, "worker-error.log")))
}

var xmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")

func xmlEscape(value string) string { return xmlEscaper.Replace(value) }

func runUninstall() error {
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  🗑️  ThingsIndex Uninstaller")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	if runtime.GOOS == "darwin" {
		fmt.Println("• Stopping running worker processes and screen sessions...")
		_ = exec.Command("/bin/sh", "-c", `pkill -f "things-index" 2>/dev/null || true`).Run()
		_ = exec.Command("/bin/sh", "-c", `screen -S things-worker -X quit 2>/dev/null || true`).Run()

		home, _ := os.UserHomeDir()

		fmt.Println("• Removing @reboot crontab entries...")
		_ = exec.Command("/bin/sh", "-c", `(crontab -l 2>/dev/null | grep -v "things-worker") | crontab - 2>/dev/null || true`).Run()

		fmt.Println("• Removing LaunchAgents...")
		plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.nejmlabs.things-index-worker.plist")
		_ = exec.Command("/bin/sh", "-c", fmt.Sprintf(`launchctl bootout gui/$(id -u) "%s" 2>/dev/null || true; rm -f "%s"`, plistPath, plistPath)).Run()

		fmt.Println("• Removing launcher scripts...")
		_ = os.Remove(filepath.Join(home, ".local", "bin", "run-things-worker.sh"))
		_ = os.Remove(filepath.Join(home, ".local", "bin", "things-index-worker-launcher.sh"))

		fmt.Println("• Removing Application Support databases & state...")
		_ = os.RemoveAll(filepath.Join(home, "Library", "Application Support", "ThingsIndex"))
		_ = os.RemoveAll(filepath.Join(home, ".local", "state", "things-index"))

		fmt.Println("• Removing Logs...")
		_ = os.RemoveAll(filepath.Join(home, "Library", "Logs", "ThingsIndex"))

		// The pieces an uninstaller cannot remove itself: Apple's shortcuts
		// CLI has no delete command, TCC automation grants have no safe
		// per-app reset, and the binary is the program running right now.
		var manual []string
		if helperShortcutInstalled() {
			manual = append(manual, fmt.Sprintf("Delete %q in the Shortcuts app — Apple's shortcuts CLI cannot remove shortcuts.", helper.HelperShortcutName))
		}
		manual = append(manual, "If you granted automation access to Things 3, revoke it under System Settings > Privacy & Security > Automation.")
		if exePath, err := os.Executable(); err == nil {
			manual = append(manual, "Delete this binary when done: rm "+shellQuote(exePath))
		}

		fmt.Println()
		fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		fmt.Println("  ✓ ThingsIndex has been removed from this Mac.")
		fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		fmt.Println("  Final manual steps for a zero-trace machine:")
		for _, step := range manual {
			fmt.Printf("  • %s\n", step)
		}
		fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		return nil
	}

	// Linux / Server uninstaller
	fmt.Println("• Stopping and disabling systemd service...")
	_ = exec.Command("/bin/sh", "-c", "systemctl stop things-index-server 2>/dev/null || true; systemctl disable things-index-server 2>/dev/null || true").Run()
	_ = os.Remove("/etc/systemd/system/things-index-server.service")
	_ = exec.Command("systemctl", "daemon-reload").Run()

	fmt.Println("• Removing configuration and database directories...")
	_ = os.RemoveAll("/etc/things-index")
	_ = os.RemoveAll("/var/lib/things-index")
	_ = os.Remove("/usr/local/bin/things-index-server")

	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  ✓ ThingsIndex Server has been completely removed.")
	if exePath, err := os.Executable(); err == nil {
		fmt.Printf("  • Delete this binary when done: rm %s\n", shellQuote(exePath))
	}
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	return nil
}

func printConfig() error {
	exe, err := os.Executable()
	if err != nil {
		exe = "things-index"
	}
	fmt.Printf(`{
  "mcpServers": {
    "things": {
      "command": "%s",
      "args": ["stdio"]
    }
  }
}
`, exe)
	return nil
}

func printHelp() {
	fmt.Println(`things-index - Native Things 3 Model Context Protocol (MCP) Server

Usage:
  things-index [command] [options]

Commands:
  start, run      Start All-in-One local MCP HTTP server & background worker (default on macOS)
  stdio           Run as a direct stdio MCP server for Claude Desktop / Cursor
  server          Run headless server queue (for Linux / Docker / Proxmox)
  worker          Run dedicated background worker connecting to a remote server
  worker --setup  Interactive Mac worker setup wizard (verifies server & token, installs launchd agent)
  config          Print ready-to-paste Claude Desktop stdio JSON configuration
  update          Replace this binary with the latest release (verifies provenance; --force reinstalls)
  uninstall       Stop and cleanly remove all daemons, databases, crontab, and scripts
  version         Print things-index version

Environment Variables:
  THINGS_INDEX_LISTEN_ADDR            HTTP listen address (default: 127.0.0.1:8080)
  THINGS_INDEX_ALLOW_UNSPECIFIED_BIND Set to 1 to allow binding 0.0.0.0 / [::] (containers)
  THINGS_INDEX_PUBLIC_TOKEN           Bearer token for MCP clients (auto-generated if omitted)
  THINGS_INDEX_WORKER_TOKEN           Bearer token for Mac worker authentication
  THINGS_INDEX_SERVER_URL             Server URL for worker daemon (e.g. https://...)
  THINGS_INDEX_THINGS_AUTH_TOKEN      Things 3 URL-scheme auth token (Things settings)
  THINGS_INDEX_THINGS_DB_PATH         Custom path to Things 3 SQLite database
  THINGS_INDEX_STATE_DIR              State directory for queue and journal databases`)
}

func generateToken() (string, error) {
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(tokenBytes), nil
}

// displayHost rewrites wildcard listen addresses to a loopback host that MCP
// clients on this machine can actually connect to.
func displayHost(listenAddr string) string {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return listenAddr
	}
	ip := net.ParseIP(host)
	if host == "" || (ip != nil && ip.IsUnspecified()) {
		return net.JoinHostPort("127.0.0.1", port)
	}
	return listenAddr
}

func leaseNextJob(ctx context.Context, store *queue.Store, maxAttempts int, leaseDuration time.Duration) (queue.Job, bool) {
	if _, err := store.ExpireLeases(ctx, time.Now(), maxAttempts); err != nil {
		return queue.Job{}, false
	}
	job, found, err := store.Lease(ctx, time.Now(), leaseDuration)
	if err != nil || !found {
		return queue.Job{}, false
	}
	return job, true
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func randomHex(bytesLen int) string {
	b := make([]byte, bytesLen)
	if _, err := rand.Read(b); err != nil {
		return hex.EncodeToString([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))[:bytesLen*2]
	}
	return hex.EncodeToString(b)
}
