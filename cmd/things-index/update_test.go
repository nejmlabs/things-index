package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeReleaseTag(t *testing.T) {
	cases := map[string]string{
		"v0.2.1":   "0.2.1",
		"0.2.1":    "0.2.1",
		" v1.0.0 ": "1.0.0",
		"v":        "",
		"":         "",
	}
	for tag, want := range cases {
		if got := normalizeReleaseTag(tag); got != want {
			t.Errorf("normalizeReleaseTag(%q) = %q, want %q", tag, got, want)
		}
	}
}

func TestRunUpdateRejectsUnknownOption(t *testing.T) {
	if err := runUpdate([]string{"--bogus"}); err == nil {
		t.Fatal("unknown option accepted")
	}
}

func TestShouldUpdate(t *testing.T) {
	if shouldUpdate("0.2.2", "0.2.2", false) {
		t.Error("same version without force should not update")
	}
	if !shouldUpdate("0.2.2", "0.2.3", false) {
		t.Error("newer release should update")
	}
	if !shouldUpdate("0.2.2", "0.2.2", true) {
		t.Error("--force should always update")
	}
	if !shouldUpdate("0.3.0-dev", "0.2.2", false) {
		t.Error("differing versions should update (equality comparison, visible in output)")
	}
}

func TestReleaseSourceLatestVersion(t *testing.T) {
	newSource := func(status int, body string) (*releaseSource, *httptest.Server) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/repos/nejmlabs/things-index/releases/latest" {
				t.Errorf("unexpected path %s", r.URL.Path)
			}
			if r.Header.Get("User-Agent") == "" {
				t.Error("missing User-Agent")
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
		return &releaseSource{apiBase: server.URL, downloadBase: server.URL, client: server.Client()}, server
	}

	source, server := newSource(http.StatusOK, `{"tag_name":"v9.9.9"}`)
	defer server.Close()
	got, err := source.latestVersion(context.Background())
	if err != nil || got != "9.9.9" {
		t.Errorf("latestVersion = %q, %v; want 9.9.9", got, err)
	}

	source, server = newSource(http.StatusForbidden, `{}`)
	defer server.Close()
	if _, err := source.latestVersion(context.Background()); err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("403 error should mention the rate limit, got %v", err)
	}

	source, server = newSource(http.StatusInternalServerError, ``)
	defer server.Close()
	if _, err := source.latestVersion(context.Background()); err == nil {
		t.Error("500 should error")
	}

	source, server = newSource(http.StatusOK, `not-json`)
	defer server.Close()
	if _, err := source.latestVersion(context.Background()); err == nil {
		t.Error("malformed JSON should error")
	}

	source, server = newSource(http.StatusOK, `{"tag_name":"v"}`)
	defer server.Close()
	if _, err := source.latestVersion(context.Background()); err == nil {
		t.Error("unusable tag should error")
	}
}

func TestReleaseSourceDownloadPinsTag(t *testing.T) {
	payload := []byte("fake-binary-bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "/nejmlabs/things-index/releases/download/v1.2.3/things-index-darwin-universal"
		if r.URL.Path != want {
			t.Errorf("download path = %s, want %s (pinned tag, not latest)", r.URL.Path, want)
		}
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	source := &releaseSource{apiBase: server.URL, downloadBase: server.URL, client: server.Client()}

	destination := filepath.Join(t.TempDir(), "binary")
	if err := source.downloadAsset(context.Background(), "1.2.3", destination); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != string(payload) {
		t.Errorf("downloaded content mismatch")
	}
}

func TestRunUpdateHelpAndBadOptions(t *testing.T) {
	if err := runUpdate([]string{"--help"}); err != nil {
		t.Errorf("--help should succeed, got %v", err)
	}
	err := runUpdate([]string{"--bogus"})
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("unknown option error should mention --force, got %v", err)
	}
}
