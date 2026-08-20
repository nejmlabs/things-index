package main

import "testing"

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
