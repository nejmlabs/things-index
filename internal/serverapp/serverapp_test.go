package serverapp

import (
	"path/filepath"
	"testing"
	"time"
)

func TestValidateListenAddress(t *testing.T) {
	tests := []struct {
		address          string
		allowUnspecified bool
		valid            bool
	}{
		{"127.0.0.1:8080", false, true},
		{"localhost:8080", false, true},
		{"192.168.1.50:8080", false, true},
		{"10.20.30.40:8080", false, true},
		{"[fd12:3456::10]:8080", false, true},
		{"0.0.0.0:8080", false, false},
		{"[::]:8080", false, false},
		{"0.0.0.0:8080", true, true},
		{"[::]:8080", true, true},
		{"8.8.8.8:8080", false, false},
		{"8.8.8.8:8080", true, false},
		{"things-index.example:8080", false, false},
		{"things-index.example:8080", true, false},
		{"missing-port", false, false},
	}
	for _, test := range tests {
		name := test.address
		if test.allowUnspecified {
			name += "+unspecified"
		}
		t.Run(name, func(t *testing.T) {
			err := validateListenAddress(test.address, test.allowUnspecified)
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
