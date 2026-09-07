package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseDisabledOverride(t *testing.T) {
	for _, test := range []struct {
		value          string
		disabled, fail bool
	}{{"true", true, false}, {"disabled", true, false}, {"false", false, false}, {"enabled", false, false}, {"unknown", false, true}, {"disabled-extra", false, true}, {"disabled extra words", false, true}} {
		t.Run(test.value, func(t *testing.T) {
			got, err := parseDisabledOverride([]byte("disabled services = {\n \""+workerLaunchAgentLabel+"\" => "+test.value+"\n}"), workerLaunchAgentLabel)
			if got != test.disabled || (err != nil) != test.fail {
				t.Fatalf("got %v,%v", got, err)
			}
		})
	}
	if disabled, err := parseDisabledOverride([]byte(`"another.worker" => disabled`), workerLaunchAgentLabel); err != nil || disabled {
		t.Fatalf("unrelated override matched: %v,%v", disabled, err)
	}
	duplicate := strings.Repeat(`"`+workerLaunchAgentLabel+`" => disabled`+"\n", 2)
	if _, err := parseDisabledOverride([]byte(duplicate), workerLaunchAgentLabel); err == nil {
		t.Fatal("duplicate override accepted")
	}
}

func TestSetupInputRequiresCompletedLine(t *testing.T) {
	for _, input := range []string{"", "done", "yes"} {
		if _, err := readSetupLine(bufio.NewReader(strings.NewReader(input))); err == nil {
			t.Fatalf("EOF accepted for %q", input)
		}
	}
	got, err := readSetupLine(bufio.NewReader(strings.NewReader("done\n")))
	if err != nil || got != "done" {
		t.Fatalf("explicit line rejected: %q,%v", got, err)
	}
}

func TestInspectSetupFDA(t *testing.T) {
	for _, test := range []struct {
		name        string
		insert      bool
		grant       int
		requirement []byte
		verifyErr   error
		want        fdaStatus
	}{
		{name: "missing", want: fdaMissing},
		{name: "disabled", insert: true, grant: 0, requirement: []byte("old"), want: fdaStale},
		{name: "empty requirement", insert: true, grant: 2, want: fdaStale},
		{name: "stale identity", insert: true, grant: 2, requirement: []byte("old identity"), verifyErr: errors.New("does not satisfy requirement"), want: fdaStale},
		{name: "matching", insert: true, grant: 2, requirement: []byte("stored requirement"), want: fdaMatched},
		{name: "verification timed out", insert: true, grant: 2, requirement: []byte("stored requirement"), verifyErr: context.DeadlineExceeded, want: fdaUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "TCC.db")
			db, err := sql.Open("sqlite3", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(`CREATE TABLE access(service TEXT,client TEXT,client_type INTEGER,auth_value INTEGER,csreq BLOB)`); err != nil {
				t.Fatal(err)
			}
			if test.insert {
				if _, err = db.Exec(`INSERT INTO access VALUES(?,?,?,?,?)`, "kTCCServiceSystemPolicyAllFiles", "/worker", 1, test.grant, test.requirement); err != nil {
					t.Fatal(err)
				}
			}
			db.Close()
			calls := 0
			var extracted string
			// Exactly seven arguments: the stored requirement is a binary file, not
			// literal source and not the current executable's own requirement.
			run := func(_ context.Context, args ...string) ([]byte, error) {
				calls++
				if len(args) != 7 || !reflect.DeepEqual(args[:5], []string{"--verify", "--strict", "--architecture", "arm64", "--test-requirement"}) || args[6] != "/worker" {
					t.Fatalf("unexpected verification args: %v", args)
				}
				extracted = args[5]
				got, err := os.ReadFile(extracted)
				if err != nil || !bytes.Equal(got, test.requirement) {
					t.Fatalf("stored requirement changed: %q,%v", got, err)
				}
				return nil, test.verifyErr
			}
			if got := inspectSetupFDA(context.Background(), path, "/worker", "arm64", run); got != test.want {
				t.Fatalf("got %s; want %s", got, test.want)
			}
			if calls > 0 {
				if _, err := os.Stat(extracted); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("temporary TCC requirement retained: %v", err)
				}
			}
		})
	}
	t.Run("unreadable or absent database", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing.db")
		got := inspectSetupFDA(context.Background(), path, "/worker", "arm64", nil)
		if got != fdaUnknown {
			t.Fatalf("got %s", got)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("read-only check created a database")
		}
	})
}

type setupFixture struct {
	lifecycle        *workerSetupLifecycle
	loaded, disabled bool
	pid              int
	calls            []string
	onBootstrap      func()
	onStatus         func()
	waits            int
	maxWaits         int
}

func newSetupFixture(t *testing.T) *setupFixture {
	t.Helper()
	directory := t.TempDir()
	now := time.Now().Add(-time.Hour)
	fixture := &setupFixture{pid: 100, loaded: true, maxWaits: 20}
	fixture.lifecycle = &workerSetupLifecycle{executable: "/worker", home: directory, domain: "gui/501", logPath: filepath.Join(directory, "worker-error.log"), markerPath: filepath.Join(directory, "automation-consent-granted"),
		checkFDA: func(context.Context) fdaStatus { return fdaMatched }, now: func() time.Time { return now }, alive: func(int) bool { return false },
		wait: func(ctx context.Context, d time.Duration) error {
			fixture.waits++
			now = now.Add(d)
			if fixture.waits > fixture.maxWaits {
				return context.DeadlineExceeded
			}
			return ctx.Err()
		},
	}
	fixture.lifecycle.run = func(_ context.Context, executable string, args ...string) ([]byte, error) {
		if executable != "/bin/launchctl" {
			fixture.calls = append(fixture.calls, executable)
			return nil, nil
		}
		fixture.calls = append(fixture.calls, strings.Join(args, " "))
		switch args[0] {
		case "print":
			if fixture.onStatus != nil {
				fixture.onStatus()
			}
			if !fixture.loaded {
				return []byte("Could not find service in domain"), errors.New("not found")
			}
			return []byte(fmt.Sprintf("state = running\n pid = %d\n", fixture.pid)), nil
		case "print-disabled":
			value := "enabled"
			if fixture.disabled {
				value = "disabled"
			}
			return []byte(fmt.Sprintf("disabled services = {\n \"%s\" => %s\n}", workerLaunchAgentLabel, value)), nil
		case "disable":
			fixture.disabled = true
		case "enable":
			fixture.disabled = false
		case "bootout":
			fixture.loaded = false
		case "bootstrap":
			fixture.loaded = true
			fixture.pid++
			if fixture.onBootstrap != nil {
				fixture.onBootstrap()
			}
		default:
			t.Fatalf("unexpected launchctl command: %v", args)
		}
		return nil, nil
	}
	return fixture
}
func writeReadyEvidence(t *testing.T, fixture *setupFixture) {
	t.Helper()
	f, err := os.OpenFile(fixture.lifecycle.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("Starting ThingsIndex worker...\nThings 3 database OK\nThings 3 automation consent granted\nThingsIndex worker ready\n")
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(fixture.lifecycle.markerPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	now := fixture.lifecycle.now()
	if err = os.Chtimes(fixture.lifecycle.markerPath, now, now); err != nil {
		t.Fatal(err)
	}
}

func TestSetupFDAGuidanceStopsBeforePromptAndFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name                string
		before, after       fdaStatus
		input               string
		wantErr, expectOpen bool
	}{
		{"matching skips prompt", fdaMatched, fdaMatched, "", false, false},
		{"stale requires refresh", fdaStale, fdaMatched, "done\n", false, true},
		{"unknown requires confirmation", fdaUnknown, fdaUnknown, "done\n", false, true},
		{"unknown EOF", fdaUnknown, fdaUnknown, "", true, true},
		{"stale remains stale", fdaStale, fdaStale, "done\n", true, true},
		{"missing remains missing", fdaMissing, fdaMissing, "done\n", true, true},
		{"cancel", fdaMissing, fdaMatched, "cancel\n", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSetupFixture(t)
			checks := 0
			fixture.lifecycle.checkFDA = func(context.Context) fdaStatus {
				if fixture.loaded || !fixture.disabled {
					t.Fatal("FDA check ran before worker stopped")
				}
				checks++
				if checks == 1 {
					return test.before
				}
				return test.after
			}
			var output bytes.Buffer
			err := fixture.lifecycle.prepareFDA(context.Background(), bufio.NewReader(strings.NewReader(test.input)), &output)
			if (err != nil) != test.wantErr {
				t.Fatalf("unexpected error: %v", err)
			}
			if fixture.loaded || !fixture.disabled {
				t.Fatal("worker did not stay stopped")
			}
			opened := false
			for _, call := range fixture.calls {
				if call == "/usr/bin/open" {
					opened = true
				}
				if strings.Contains(call, "pkill") || strings.Contains(call, "pgrep") {
					t.Fatal("broad process command used")
				}
			}
			if opened != test.expectOpen {
				t.Fatalf("opened settings=%v", opened)
			}
			if !test.wantErr && test.after == fdaUnknown && !strings.Contains(output.String(), "stored grant remains unverified") {
				t.Fatal("unknown grant was presented as verified")
			}
		})
	}
}

func TestSetupFreshEvidenceRejectsOldReadiness(t *testing.T) {
	fixture := newSetupFixture(t)
	writeReadyEvidence(t, fixture)
	start, err := setupLogSnapshot(fixture.lifecycle.logPath, fixture.lifecycle.now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if ready, err := freshSetupEvidence(fixture.lifecycle.logPath, fixture.lifecycle.markerPath, &start); err != nil || ready {
		t.Fatalf("stale log accepted: %v,%v", ready, err)
	}
	// A new marker alone cannot reuse the old ready lines.
	os.Chtimes(fixture.lifecycle.markerPath, start.started, start.started)
	if ready, err := freshSetupEvidence(fixture.lifecycle.logPath, fixture.lifecycle.markerPath, &start); err != nil || ready {
		t.Fatalf("fresh marker reused stale log: %v,%v", ready, err)
	}
	writeReadyEvidence(t, fixture)
	os.Chtimes(fixture.lifecycle.markerPath, start.started, start.started)
	if ready, err := freshSetupEvidence(fixture.lifecycle.logPath, fixture.lifecycle.markerPath, &start); err != nil || !ready {
		t.Fatalf("fresh startup rejected: %v,%v", ready, err)
	}
	writeReadyEvidence(t, fixture)
	if _, err := freshSetupEvidence(fixture.lifecycle.logPath, fixture.lifecycle.markerPath, &start); err == nil {
		t.Fatal("multiple fresh startups accepted")
	}
}

func TestSetupStartupFailureStopsOnlyItsAgent(t *testing.T) {
	for _, failure := range []string{"no fresh evidence", "PID restart", "bootstrap"} {
		t.Run(failure, func(t *testing.T) {
			fixture := newSetupFixture(t)
			if err := fixture.lifecycle.stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			fixture.onBootstrap = func() {
				if failure == "PID restart" {
					writeReadyEvidence(t, fixture)
				}
			}
			if failure == "PID restart" {
				reads := 0
				fixture.onStatus = func() {
					if fixture.loaded {
						reads++
						if reads == 2 {
							fixture.pid++
						}
					}
				}
			}
			if failure == "bootstrap" {
				original := fixture.lifecycle.run
				fixture.lifecycle.run = func(ctx context.Context, path string, args ...string) ([]byte, error) {
					if len(args) > 0 && args[0] == "bootstrap" {
						fixture.loaded = true
						return []byte("SECRET TOKEN IN PRIVATE LAUNCHCTL OUTPUT"), errors.New("exit 5")
					}
					return original(ctx, path, args...)
				}
			}
			_, err := fixture.lifecycle.startVerified(context.Background(), "/worker.plist")
			if err == nil {
				t.Fatal("unverified worker accepted")
			}
			if strings.Contains(err.Error(), "SECRET") {
				t.Fatal("raw launchctl output exposed")
			}
			if fixture.loaded || !fixture.disabled {
				t.Fatal("failed startup left worker running or enabled")
			}
			for _, call := range fixture.calls {
				if strings.HasPrefix(call, "disable ") || strings.HasPrefix(call, "bootout ") {
					if !strings.HasSuffix(call, "gui/501/"+workerLaunchAgentLabel) {
						t.Fatalf("stopped another agent: %s", call)
					}
				}
			}
		})
	}
}

func TestSetupControlledRestartRequiresNewAutomationEvidence(t *testing.T) {
	fixture := newSetupFixture(t)
	fixture.maxWaits = 50
	if err := os.WriteFile(fixture.lifecycle.markerPath, []byte("previous consent"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.onBootstrap = func() { writeReadyEvidence(t, fixture) }
	if err := fixture.lifecycle.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, err := fixture.lifecycle.startVerified(context.Background(), "/worker.plist")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.lifecycle.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	second, err := fixture.lifecycle.startVerified(context.Background(), "/worker.plist")
	if err != nil {
		t.Fatal(err)
	}
	if second == first || !fixture.loaded || fixture.disabled {
		t.Fatal("controlled restart was not verified")
	}
	backups, err := filepath.Glob(filepath.Join(filepath.Dir(fixture.lifecycle.markerPath), "automation-consent-granted.setup-backup-*"))
	if err != nil || len(backups) != 2 {
		t.Fatalf("consent markers not archived: %v,%v", backups, err)
	}
	preserved := false
	for _, path := range backups {
		data, _ := os.ReadFile(path)
		preserved = preserved || string(data) == "previous consent"
	}
	if !preserved {
		t.Fatal("old consent marker was discarded")
	}
}

func TestSetupBlockedInputCancellation(t *testing.T) {
	input, output := io.Pipe()
	defer input.Close()
	defer output.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := readSetupLineContext(ctx, bufio.NewReader(input)); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt blocked prompt")
	}
}

func TestSetupFDAPromptCancellationLeavesWorkerStopped(t *testing.T) {
	fixture := newSetupFixture(t)
	fixture.lifecycle.checkFDA = func(context.Context) fdaStatus { return fdaUnknown }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	original := fixture.lifecycle.run
	fixture.lifecycle.run = func(ctx context.Context, path string, args ...string) ([]byte, error) {
		result, err := original(ctx, path, args...)
		if path == "/usr/bin/open" {
			cancel()
		}
		return result, err
	}
	input, output := io.Pipe()
	defer input.Close()
	defer output.Close()
	var messages bytes.Buffer
	err := fixture.lifecycle.prepareFDA(ctx, bufio.NewReader(input), &messages)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if fixture.loaded || !fixture.disabled {
		t.Fatal("canceled permission setup left worker active")
	}
}
