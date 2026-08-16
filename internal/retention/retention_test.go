package retention

import (
	"testing"
	"time"
)

func TestParseDays(t *testing.T) {
	tests := []struct {
		name        string
		value       string
		defaultDays int64
		want        time.Duration
		valid       bool
	}{
		{name: "default", defaultDays: 7, want: 7 * 24 * time.Hour, valid: true},
		{name: "disabled", value: "0", defaultDays: 7, want: 0, valid: true},
		{name: "explicit", value: "30", want: 30 * 24 * time.Hour, valid: true},
		{name: "negative", value: "-1", valid: false},
		{name: "fraction", value: "1.5", valid: false},
		{name: "text", value: "forever", valid: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseDays("TEST_RETENTION_DAYS", test.value, test.defaultDays)
			if test.valid && err != nil {
				t.Fatal(err)
			}
			if !test.valid && err == nil {
				t.Fatal("expected invalid duration to be rejected")
			}
			if test.valid && got != test.want {
				t.Fatalf("duration = %s, want %s", got, test.want)
			}
		})
	}
}

func TestCutoff(t *testing.T) {
	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.FixedZone("BST", 3600))
	if got := Cutoff(now, 24*time.Hour); !got.Equal(now.UTC().Add(-24 * time.Hour)) {
		t.Fatalf("cutoff = %s", got)
	}
	if got := Cutoff(now, 0); !got.IsZero() {
		t.Fatalf("disabled cutoff = %s", got)
	}
}
