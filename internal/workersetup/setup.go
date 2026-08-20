// Package workersetup provides a short-lived, loopback-only setup UI for the
// macOS worker. It is not used during normal capture processing.
package workersetup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/nejmlabs/things-index/internal/capture"
	"github.com/nejmlabs/things-index/internal/helper"
)

const maxFormBytes = 4 << 10

type Verifier interface {
	Ping(context.Context) error
	Capture(context.Context, string, capture.Request) (helper.Response, error)
	FinaliseCapture(context.Context, string, string) error
}

type OpenFunc func(context.Context, string) error

type Config struct {
	StateDir    string
	Verifier    Verifier
	OpenFile    OpenFunc
	OpenBrowser OpenFunc
}

type application struct {
	config   Config
	token    string
	finish   chan struct{}
	finishMu sync.Once
	testMu   sync.Mutex
	stateMu  sync.RWMutex
	state    pageState
	testID   string
	thingsID string
}

type pageState struct {
	Kind    string
	Heading string
	Detail  string
	Access  bool
	Ready   bool
}

type pageData struct {
	Token string
	State pageState
}

func Run(ctx context.Context, config Config) error {
	if err := validateConfig(config); err != nil {
		return err
	}
	token, err := randomToken()
	if err != nil {
		return fmt.Errorf("create setup token: %w", err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen for setup UI: %w", err)
	}
	defer listener.Close()

	app := newApplication(config, token)
	server := &http.Server{
		Handler:           app.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       35 * time.Second,
		WriteTimeout:      35 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
	serveResult := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveResult <- err
	}()

	url := fmt.Sprintf("http://%s/?token=%s", listener.Addr(), token)
	if err := config.OpenBrowser(ctx, url); err != nil {
		_ = server.Close()
		<-serveResult
		return fmt.Errorf("open setup UI: %w", err)
	}
	log.Printf("ThingsIndex setup opened at %s", url)

	select {
	case <-ctx.Done():
	case <-app.finish:
	case err := <-serveResult:
		return err
	}

	shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		return fmt.Errorf("close setup UI: %w", err)
	}
	return <-serveResult
}

func validateConfig(config Config) error {
	if strings.TrimSpace(config.StateDir) == "" {
		return errors.New("setup state directory is required")
	}
	if config.Verifier == nil {
		return errors.New("Things verifier is required")
	}
	if config.OpenBrowser == nil {
		return errors.New("browser opener is required")
	}
	return nil
}

func newApplication(config Config, token string) *application {
	testHash := sha256.Sum256([]byte(token))
	return &application{
		config: config,
		token:  token,
		finish: make(chan struct{}),
		testID: hex.EncodeToString(testHash[:16]),
		state: pageState{
			Kind:    "pending",
			Heading: "Things 3 access not yet verified",
			Detail:  "Verify Things 3 connectivity, then run the test capture.",
		},
	}
}

func (a *application) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", a.show)
	mux.HandleFunc("POST /verify", a.verify)
	mux.HandleFunc("POST /test-capture", a.testCapture)
	mux.HandleFunc("POST /finish", a.complete)
	return securityHeaders(mux)
}

func (a *application) show(response http.ResponseWriter, request *http.Request) {
	if !a.authorised(request) {
		http.NotFound(response, request)
		return
	}
	a.render(response)
}

func (a *application) verify(response http.ResponseWriter, request *http.Request) {
	if !a.authoriseForm(response, request) {
		return
	}
	if err := a.config.Verifier.Ping(request.Context()); err != nil {
		a.setState(
			"failed",
			"Things 3 connection failed",
			"Ensure Things 3 is installed and has been opened at least once. "+err.Error(),
			false,
			false,
		)
	} else {
		a.setState(
			"progress",
			"Things 3 connected",
			"Things 3 database verified. Run the test capture to confirm background capture.",
			true,
			false,
		)
	}
	a.render(response)
}

func (a *application) testCapture(response http.ResponseWriter, request *http.Request) {
	if !a.authoriseForm(response, request) {
		return
	}
	if !a.currentState().Access {
		http.Error(response, "verify Things 3 access before testing capture", http.StatusConflict)
		return
	}

	a.testMu.Lock()
	defer a.testMu.Unlock()

	if a.thingsID == "" {
		today := time.Now()
		created, err := a.config.Verifier.Capture(request.Context(), a.testID, capture.Request{TaskFields: capture.TaskFields{
			Title: "ThingsIndex setup test — safe to delete",
			Notes: "Created by the ThingsIndex setup GUI to verify background capture permissions.",
			Schedule: &capture.Schedule{
				Start:      capture.StartOnDate,
				Date:       today.Format("2006-01-02"),
				ReminderAt: today.Add(time.Hour).Format(time.RFC3339),
			},
			Deadline:  today.Add(24 * time.Hour).Format("2006-01-02"),
			Checklist: []string{"Verify permissions", "Safe to delete"},
		}})
		if err != nil {
			a.setState("failed", "Capture test could not create a task", err.Error(), true, false)
			a.render(response)
			return
		}
		a.thingsID = created.ID
	}

	if err := a.config.Verifier.FinaliseCapture(request.Context(), a.thingsID, "ThingsIndex setup test — safe to delete"); err != nil {
		a.setState(
			"failed",
			"Capture test needs attention",
			err.Error(),
			true,
			false,
		)
	} else {
		a.setState(
			"ready",
			"Capture path ready",
			"Task created and confirmed in Things 3. Delete “ThingsIndex setup test — safe to delete” when convenient.",
			true,
			true,
		)
	}
	a.render(response)
}

func (a *application) complete(response http.ResponseWriter, request *http.Request) {
	if !a.authoriseForm(response, request) {
		return
	}
	if !a.currentState().Ready {
		http.Error(response, "complete verification before finishing setup", http.StatusConflict)
		return
	}
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = response.Write([]byte(finishedPage))
	a.finishMu.Do(func() { close(a.finish) })
}

func (a *application) authorised(request *http.Request) bool {
	return subtle.ConstantTimeCompare([]byte(request.URL.Query().Get("token")), []byte(a.token)) == 1
}

func (a *application) authoriseForm(response http.ResponseWriter, request *http.Request) bool {
	request.Body = http.MaxBytesReader(response, request.Body, maxFormBytes)
	if err := request.ParseForm(); err != nil || subtle.ConstantTimeCompare([]byte(request.Form.Get("token")), []byte(a.token)) != 1 {
		http.Error(response, "invalid setup request", http.StatusForbidden)
		return false
	}
	return true
}

func (a *application) render(response http.ResponseWriter) {
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := setupTemplate.Execute(response, pageData{Token: a.token, State: a.currentState()}); err != nil {
		return
	}
}

func (a *application) setState(kind, heading, detail string, access, ready bool) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.state = pageState{Kind: kind, Heading: heading, Detail: detail, Access: access, Ready: ready}
}

func (a *application) currentState() pageState {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.state
}

func randomToken() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func Open(ctx context.Context, target string) error {
	return exec.CommandContext(ctx, "/usr/bin/open", target).Run()
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Cache-Control", "no-store")
		response.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		response.Header().Set("Referrer-Policy", "no-referrer")
		response.Header().Set("X-Content-Type-Options", "nosniff")
		response.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(response, request)
	})
}

var setupTemplate = template.Must(template.New("setup").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>ThingsIndex Setup</title>
  <style>
    :root { color-scheme: light dark; font-family: ui-sans-serif, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
    * { box-sizing: border-box; }
    body { margin: 0; min-height: 100vh; display: grid; place-items: center; padding: 2rem 1rem; background: #74809518; color: CanvasText; }
    main { width: min(42rem, 100%); padding: 2rem; border: 1px solid #7a849455; border-radius: 1.2rem; background: Canvas; box-shadow: 0 1.2rem 4rem #0002; }
    .eyebrow { margin: 0 0 .5rem; color: #567; font-size: .75rem; font-weight: 750; letter-spacing: .12em; text-transform: uppercase; }
    h1 { margin: 0; font-size: clamp(1.8rem, 5vw, 2.6rem); letter-spacing: -.035em; }
    .intro { margin: .7rem 0 1.7rem; color: GrayText; line-height: 1.55; }
    .status { display: grid; grid-template-columns: 2.2rem 1fr; gap: .85rem; padding: 1rem; border-radius: .85rem; background: #74809512; border: 1px solid #74809530; }
    .dot { width: 2.1rem; height: 2.1rem; display: grid; place-items: center; border-radius: 50%; font-weight: 850; }
    .pending .dot { background: #74809525; color: GrayText; }
    .progress .dot { background: #dcecff; color: #1d4f91; }
    .failed .dot { background: #ffe0e0; color: #9b1c1c; }
    .ready .dot { background: #d9f7e7; color: #0b6b3a; }
    .status h2 { margin: .05rem 0 .3rem; font-size: 1rem; }
    .status p { margin: 0; color: GrayText; line-height: 1.45; font-size: .9rem; overflow-wrap: anywhere; }
    .steps { margin: 1.5rem 0; padding-left: 1.3rem; line-height: 1.55; }
    .steps li + li { margin-top: .55rem; }
    .actions { display: flex; gap: .7rem; flex-wrap: wrap; }
    form { margin: 0; }
    button { appearance: none; border: 1px solid #74809555; border-radius: .65rem; padding: .7rem 1rem; background: Canvas; color: CanvasText; font: inherit; font-weight: 680; cursor: pointer; }
    button:hover { background: #74809518; }
    .primary { border-color: #276bd6; background: #276bd6; color: white; }
    .primary:hover { background: #1f5cbb; }
    button:disabled { cursor: not-allowed; opacity: .45; }
    footer { margin-top: 1.5rem; color: GrayText; font-size: .78rem; line-height: 1.45; }
  </style>
</head>
<body>
<main>
  <p class="eyebrow">Mac worker onboarding</p>
  <h1>ThingsIndex Setup</h1>
  <p class="intro">Verify Things 3 connectivity and test instantaneous background capture.</p>

  <section class="status {{.State.Kind}}" aria-live="polite">
    <span class="dot" aria-hidden="true">{{if .State.Ready}}✓{{else if eq .State.Kind "failed"}}!{{else}}…{{end}}</span>
    <div><h2>{{.State.Heading}}</h2><p>{{.State.Detail}}</p></div>
  </section>

  <ol class="steps">
    <li>Select <strong>Verify Things 3</strong> to confirm database connectivity.</li>
    <li>Select <strong>Test Capture</strong> to create and finalise a test task.</li>
    <li>Select <strong>Finish Setup</strong> to exit onboarding.</li>
  </ol>

  <div class="actions">
    <form method="post" action="/verify"><input type="hidden" name="token" value="{{.Token}}"><button class="primary" type="submit">Verify Things 3</button></form>
    <form method="post" action="/test-capture"><input type="hidden" name="token" value="{{.Token}}"><button type="submit" {{if not .State.Access}}disabled{{end}}>Test Capture</button></form>
    <form method="post" action="/finish"><input type="hidden" name="token" value="{{.Token}}"><button type="submit" {{if not .State.Ready}}disabled{{end}}>Finish Setup</button></form>
  </div>

  <footer>This page is available only on this Mac at 127.0.0.1 and stops when setup finishes. It does not expose the worker API or remain running afterward.</footer>
</main>
</body>
</html>`))

const finishedPage = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>ThingsIndex Setup Complete</title><style>:root{color-scheme:light dark;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}body{min-height:100vh;margin:0;display:grid;place-items:center;background:#74809518}main{max-width:34rem;margin:1rem;padding:2rem;border:1px solid #74809555;border-radius:1rem;background:Canvas;text-align:center}span{display:inline-grid;place-items:center;width:3rem;height:3rem;border-radius:50%;background:#d9f7e7;color:#0b6b3a;font-size:1.5rem;font-weight:900}h1{margin:1rem 0 .5rem}p{color:GrayText;line-height:1.5}</style></head><body><main><span>✓</span><h1>Setup complete</h1><p>Things 3 connection is verified and ready. You can close this tab and start the worker.</p></main></body></html>`
