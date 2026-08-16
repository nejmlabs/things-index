package main

import (
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
