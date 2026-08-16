package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestRequireNonPublicAddress(t *testing.T) {
	tests := []struct {
		address string
		valid   bool
	}{
		{"127.0.0.1:8080", true},
		{"localhost:8080", true},
		{"192.168.1.50:8080", true},
		{"10.20.30.40:8080", true},
		{"[fd12:3456::10]:8080", true},
		{"0.0.0.0:8080", false},
		{"[::]:8080", false},
		{"8.8.8.8:8080", false},
		{"things-index.example:8080", false},
		{"missing-port", false},
	}
	for _, test := range tests {
		t.Run(test.address, func(t *testing.T) {
			err := requireNonPublicAddress(test.address)
			if test.valid && err != nil {
				t.Fatalf("expected address to be accepted: %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("expected address to be rejected")
			}
		})
	}
}

func TestConfiguredQueuePath(t *testing.T) {
	want := filepath.Join(t.TempDir(), "custom.db")
	t.Setenv("THINGS_INDEX_DB_PATH", want)

	path, err := configuredQueuePath()
	if err != nil {
		t.Fatal(err)
	}
	if path != want {
		t.Fatalf("configured queue path = %q, want %q", path, want)
	}
}

func TestConfiguredTerminalRetention(t *testing.T) {
	t.Setenv("THINGS_INDEX_SUCCEEDED_RETENTION_DAYS", "2")
	t.Setenv("THINGS_INDEX_FAILED_RETENTION_DAYS", "0")
	policy, err := configuredTerminalRetention()
	if err != nil {
		t.Fatal(err)
	}
	if policy.succeeded != 48*time.Hour || policy.failed != 0 {
		t.Fatalf("unexpected retention policy: %#v", policy)
	}
}
