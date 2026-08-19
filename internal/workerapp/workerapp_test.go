package workerapp

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestConfiguredJournalRetention(t *testing.T) {
	t.Setenv("THINGS_INDEX_JOURNAL_RETENTION_DAYS", "14")
	duration, err := configuredJournalRetention()
	if err != nil {
		t.Fatal(err)
	}
	if duration != 14*24*time.Hour {
		t.Fatalf("retention duration = %s", duration)
	}
}

func TestConfiguredJournalRetentionRejectsInvalid(t *testing.T) {
	t.Setenv("THINGS_INDEX_JOURNAL_RETENTION_DAYS", "soon")
	if _, err := configuredJournalRetention(); err == nil {
		t.Fatal("expected error for non-numeric retention")
	}
}

func TestStateDirectoryOverride(t *testing.T) {
	t.Setenv("THINGS_INDEX_STATE_DIR", "/tmp/custom-state")
	directory, err := StateDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if directory != "/tmp/custom-state" {
		t.Fatalf("state directory = %s", directory)
	}
}

func TestStateDirectoryDefault(t *testing.T) {
	t.Setenv("THINGS_INDEX_STATE_DIR", "")
	directory, err := StateDirectory()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(".local", "state", "things-index")
	if runtime.GOOS == "darwin" {
		want = filepath.Join("Library", "Application Support", "ThingsIndex")
	}
	if !strings.HasSuffix(directory, want) {
		t.Fatalf("state directory = %s, want suffix %s", directory, want)
	}
}

func TestJournalPathOverride(t *testing.T) {
	t.Setenv("THINGS_INDEX_JOURNAL_PATH", "/tmp/custom-journal.sqlite")
	path, err := JournalPath()
	if err != nil {
		t.Fatal(err)
	}
	if path != "/tmp/custom-journal.sqlite" {
		t.Fatalf("journal path = %s", path)
	}
}
