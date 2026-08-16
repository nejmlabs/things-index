package workersetup

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nejmlabs/things-index/internal/capture"
	"github.com/nejmlabs/things-index/internal/helper"
)

type fakeVerifier struct {
	pingErr       error
	captureErr    error
	finaliseErr   error
	pingCalls     int
	captureCalls  int
	finaliseCalls int
	capturedID    string
}

func (f *fakeVerifier) Ping(context.Context) error {
	f.pingCalls++
	return f.pingErr

}

func (f *fakeVerifier) Capture(_ context.Context, _ string, _ capture.Request) (helper.Response, error) {
	f.captureCalls++
	if f.captureErr != nil {
		return helper.Response{}, f.captureErr
	}
	id := f.capturedID
	if id == "" {
		id = "setup-test-id"
	}
	return helper.Response{SchemaVersion: 1, OK: true, ID: id}, nil
}

func (f *fakeVerifier) FinaliseCapture(context.Context, string, string) error {
	f.finaliseCalls++
	return f.finaliseErr
}

func TestInstallWritesExactNamedShortcutAndOpensIt(t *testing.T) {
	t.Parallel()

	stateDirectory := t.TempDir()
	var opened string
	app := newApplication(Config{
		Shortcut: []byte("signed shortcut"),
		StateDir: stateDirectory,
		Verifier: &fakeVerifier{},
		OpenFile: func(_ context.Context, path string) error {
			opened = path
			return nil
		},
		OpenBrowser: func(context.Context, string) error { return nil },
	}, "secret")

	response := postForm(app.handler(), "/install", url.Values{"token": {"secret"}})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if filepath.Base(opened) != shortcutFilename {
		t.Fatalf("opened %q", opened)
	}
	data, err := os.ReadFile(opened)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "signed shortcut" {
		t.Fatalf("unexpected helper content %q", data)
	}
	info, err := os.Stat(opened)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("helper permissions are %o", info.Mode().Perm())
	}
}

func TestVerifyEnablesCaptureTestWithoutFinishing(t *testing.T) {
	t.Parallel()

	verifier := &fakeVerifier{}
	app := testApplication(t, verifier)
	response := postForm(app.handler(), "/verify", url.Values{"token": {"secret"}})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if verifier.pingCalls != 1 || !app.currentState().Access || app.currentState().Ready {
		t.Fatalf("unexpected verifier calls/state: %d %#v", verifier.pingCalls, app.currentState())
	}
	if !strings.Contains(response.Body.String(), "Access verified") {
		t.Fatalf("access status missing: %s", response.Body.String())
	}
}

func TestVerifyShowsFailureWithoutFinishing(t *testing.T) {
	t.Parallel()

	app := testApplication(t, &fakeVerifier{pingErr: errors.New("not installed")})
	response := postForm(app.handler(), "/verify", url.Values{"token": {"secret"}})
	if response.Code != http.StatusOK || app.currentState().Ready {
		t.Fatalf("unexpected status/state: %d %#v", response.Code, app.currentState())
	}
	if !strings.Contains(response.Body.String(), "not installed") {
		t.Fatalf("failure page missing detail: %s", response.Body.String())
	}
}

func TestFinishRequiresSuccessfulVerification(t *testing.T) {
	t.Parallel()

	app := testApplication(t, &fakeVerifier{})
	response := postForm(app.handler(), "/finish", url.Values{"token": {"secret"}})
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d", response.Code)
	}

	postForm(app.handler(), "/verify", url.Values{"token": {"secret"}})
	response = postForm(app.handler(), "/finish", url.Values{"token": {"secret"}})
	if response.Code != http.StatusConflict {
		t.Fatalf("finish after access-only status = %d", response.Code)
	}
	postForm(app.handler(), "/test-capture", url.Values{"token": {"secret"}})
	response = postForm(app.handler(), "/finish", url.Values{"token": {"secret"}})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	select {
	case <-app.finish:
	default:
		t.Fatal("finish channel was not closed")
	}
}

func TestCaptureTestRequiresAccessAndResumesFinalisation(t *testing.T) {
	t.Parallel()

	verifier := &fakeVerifier{finaliseErr: errors.New("edit permission pending")}
	app := testApplication(t, verifier)
	response := postForm(app.handler(), "/test-capture", url.Values{"token": {"secret"}})
	if response.Code != http.StatusConflict {
		t.Fatalf("capture before access status = %d", response.Code)
	}

	postForm(app.handler(), "/verify", url.Values{"token": {"secret"}})
	response = postForm(app.handler(), "/test-capture", url.Values{"token": {"secret"}})
	if response.Code != http.StatusOK || verifier.captureCalls != 1 || verifier.finaliseCalls != 1 || app.currentState().Ready {
		t.Fatalf("unexpected first test result: status=%d helper=%#v state=%#v", response.Code, verifier, app.currentState())
	}

	verifier.finaliseErr = nil
	response = postForm(app.handler(), "/test-capture", url.Values{"token": {"secret"}})
	if response.Code != http.StatusOK || verifier.captureCalls != 1 || verifier.finaliseCalls != 2 || !app.currentState().Ready {
		t.Fatalf("unexpected resumed test result: status=%d helper=%#v state=%#v", response.Code, verifier, app.currentState())
	}
}

func TestSetupRejectsWrongToken(t *testing.T) {
	t.Parallel()

	app := testApplication(t, &fakeVerifier{})
	request := httptest.NewRequest(http.MethodGet, "/?token=wrong", nil)
	response := httptest.NewRecorder()
	app.handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("GET status = %d", response.Code)
	}

	response = postForm(app.handler(), "/install", url.Values{"token": {"wrong"}})
	if response.Code != http.StatusForbidden {
		t.Fatalf("POST status = %d", response.Code)
	}
}

func TestSetupAddsBrowserSecurityHeaders(t *testing.T) {
	t.Parallel()

	app := testApplication(t, &fakeVerifier{})
	request := httptest.NewRequest(http.MethodGet, "/?token=secret", nil)
	response := httptest.NewRecorder()
	app.handler().ServeHTTP(response, request)
	if response.Header().Get("Content-Security-Policy") == "" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("missing security headers: %#v", response.Header())
	}
}

func testApplication(t *testing.T, verifier Verifier) *application {
	t.Helper()
	return newApplication(Config{
		Shortcut:    []byte("signed shortcut"),
		StateDir:    t.TempDir(),
		Verifier:    verifier,
		OpenFile:    func(context.Context, string) error { return nil },
		OpenBrowser: func(context.Context, string) error { return nil },
	}, "secret")
}

func postForm(handler http.Handler, target string, values url.Values) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	_, _ = io.Copy(io.Discard, response.Result().Body)
	return response
}
