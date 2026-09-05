package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Exercise an isolated copy of the shipped wrapper. Only ordinary filesystem
// utilities are real: downloads, signing, metadata, provenance, and the child
// installer are stubs, so these tests never touch macOS services or the network.
type workerBootstrapFixture struct {
	home, script, tools, calls, candidate string
}

func newWorkerBootstrapFixture(t *testing.T) workerBootstrapFixture {
	t.Helper()
	directory := t.TempDir()
	fixture := workerBootstrapFixture{
		home: filepath.Join(directory, "test home"), script: filepath.Join(directory, "install.sh"),
		tools: filepath.Join(directory, "tools"), calls: filepath.Join(directory, "calls"),
		candidate: filepath.Join(directory, "candidate"),
	}
	if err := os.Mkdir(fixture.tools, 0o700); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join("..", "..", "deploy", "mac-worker-install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(source)
	// Redirect the copy's installation root without changing the test runner's
	// HOME. Exercise spaces in paths as well as the real wrapper's quoting.
	script = strings.ReplaceAll(script, "${HOME}", "${THINGS_INDEX_BOOTSTRAP_TEST_HOME}")
	for _, name := range []string{"xattr", "codesign", "python", "python3", "xcode-select", "lipo"} {
		path := "'" + strings.ReplaceAll(filepath.Join(fixture.tools, name), "'", "'\\''") + "'"
		script = strings.ReplaceAll(script, "/usr/bin/"+name, path)
	}
	if err := os.WriteFile(fixture.script, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mkdir", "mktemp", "rm", "chmod", "cp"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(fixture.tools, name)); err != nil {
			t.Fatal(err)
		}
	}
	stub := `#!/bin/bash
name=${0##*/}
printf '%s\0' "$name" "$@" '' >> "$THINGS_INDEX_BOOTSTRAP_TEST_CALLS"
case "$name" in
  uname) printf 'Darwin\n' ;;
  curl)
    destination=''
    while [ "$#" -gt 0 ]; do
      if [ "$1" = '-o' ]; then shift; destination=$1; fi
      shift
    done
    [ -n "$destination" ] || exit 90
    cp "$THINGS_INDEX_BOOTSTRAP_TEST_CANDIDATE" "$destination"
    ;;
  gh)
    case "$1" in
      auth) exit "${THINGS_INDEX_BOOTSTRAP_TEST_AUTH_EXIT:-0}" ;;
      attestation) exit "${THINGS_INDEX_BOOTSTRAP_TEST_ATTESTATION_EXIT:-0}" ;;
    esac
    ;;
  xattr)
    if [ "${THINGS_INDEX_BOOTSTRAP_TEST_QUARANTINE:-}" = yes ]; then
      printf 'com.apple.metadata:kMDItemWhereFroms\ncom.apple.quarantine\n'
    fi
    exit "${THINGS_INDEX_BOOTSTRAP_TEST_XATTR_EXIT:-0}"
    ;;
  codesign) exit "${THINGS_INDEX_BOOTSTRAP_TEST_CODESIGN_EXIT:-0}" ;;
  *) exit 91 ;;
esac
`
	for _, name := range []string{"uname", "curl", "gh", "xattr", "codesign", "python", "python3", "xcode-select", "lipo"} {
		if err := os.WriteFile(filepath.Join(fixture.tools, name), []byte(stub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	candidate := `#!/bin/bash
printf '%s\0' candidate "$0" "$@" '' >> "$THINGS_INDEX_BOOTSTRAP_TEST_CALLS"
IFS= read -r first_line < "$0" || exit 92
[ "$first_line" = '#!/bin/bash' ] || exit 93
printf '%s\0' candidate-readable "$0" '' >> "$THINGS_INDEX_BOOTSTRAP_TEST_CALLS"
exit "${THINGS_INDEX_BOOTSTRAP_TEST_CHILD_EXIT:-0}"
`
	if err := os.WriteFile(fixture.candidate, []byte(candidate), 0o600); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f workerBootstrapFixture) run(t *testing.T, environment []string, args ...string) (int, [][]string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/bash", append([]string{f.script}, args...)...)
	command.Env = []string{
		"PATH=" + f.tools,
		"THINGS_INDEX_BOOTSTRAP_TEST_HOME=" + f.home,
		"THINGS_INDEX_BOOTSTRAP_TEST_CALLS=" + f.calls,
		"THINGS_INDEX_BOOTSTRAP_TEST_CANDIDATE=" + f.candidate,
	}
	command.Env = append(command.Env, environment...)
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("bootstrap did not finish: %v\n%s", ctx.Err(), output)
	}
	status := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run bootstrap: %v", err)
		}
		status = exit.ExitCode()
	}
	data, err := os.ReadFile(f.calls)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var calls [][]string
	for _, record := range strings.Split(string(data), "\x00\x00") {
		if record != "" {
			calls = append(calls, strings.Split(record, "\x00"))
		}
	}
	staging, err := filepath.Glob(filepath.Join(f.home, ".local", "bin", ".things-index-download.*"))
	if err != nil || len(staging) != 0 {
		t.Fatalf("staging was not cleaned after exit %d: %v, %v", status, staging, err)
	}
	for _, call := range calls {
		switch call[0] {
		case "python", "python3", "xcode-select", "lipo":
			t.Fatalf("bootstrap invoked a developer tool: %v", call)
		}
	}
	return status, calls, string(output)
}

func TestWorkerBootstrapVerifiesBeforeHandoff(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		exit int
	}{
		{name: "interactive setup"},
		{name: "no setup", args: []string{"--no-setup"}},
		{name: "child failure", args: []string{"--no-setup"}, exit: 37},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newWorkerBootstrapFixture(t)
			environment := []string{}
			if test.exit != 0 {
				environment = append(environment, "THINGS_INDEX_BOOTSTRAP_TEST_CHILD_EXIT=37")
			}
			status, calls, output := fixture.run(t, environment, test.args...)
			if status != test.exit {
				t.Fatalf("exit %d; want %d\n%s", status, test.exit, output)
			}
			var names []string
			for _, call := range calls {
				names = append(names, call[0])
			}
			wantNames := []string{"uname", "curl", "gh", "gh", "xattr", "codesign", "candidate", "candidate-readable"}
			if !reflect.DeepEqual(names, wantNames) {
				t.Fatalf("unexpected verification/handoff order: %v", calls)
			}
			candidate := calls[4][1]
			if !strings.HasPrefix(candidate, filepath.Join(fixture.home, ".local", "bin", ".things-index-download.")) || filepath.Base(candidate) != "things-index" {
				t.Fatalf("candidate not staged in a fresh installation directory: %q", candidate)
			}
			wantSign := []string{"codesign", "--verify", "--strict", "--all-architectures", "--test-requirement", `=identifier "com.nejmlabs.things-index" and certificate leaf = H"411458A567FC772FF286B076E379703960D71231"`, candidate}
			if !reflect.DeepEqual(calls[5], wantSign) {
				t.Fatalf("signature verification arguments: %q; want %q", calls[5], wantSign)
			}
			wantChild := append([]string{"candidate", candidate, "install-worker"}, test.args...)
			if !reflect.DeepEqual(calls[6], wantChild) || !reflect.DeepEqual(calls[7], []string{"candidate-readable", candidate}) {
				t.Fatalf("child did not receive the intended command and readable source: %v", calls[6:])
			}
			if !reflect.DeepEqual(calls[3], []string{"gh", "attestation", "verify", candidate, "--repo", "nejmlabs/things-index"}) {
				t.Fatalf("provenance did not verify the executed candidate: %v", calls[3])
			}
		})
	}
}

func TestWorkerBootstrapRejectsBeforeExecution(t *testing.T) {
	for _, test := range []struct {
		name, environment, lastCall, message string
	}{
		{"quarantine", "THINGS_INDEX_BOOTSTRAP_TEST_QUARANTINE=yes", "xattr", "quarantined"},
		{"metadata unreadable", "THINGS_INDEX_BOOTSTRAP_TEST_XATTR_EXIT=1", "xattr", "Could not inspect"},
		{"signature mismatch", "THINGS_INDEX_BOOTSTRAP_TEST_CODESIGN_EXIT=1", "codesign", "does not match"},
		{"provenance failure", "THINGS_INDEX_BOOTSTRAP_TEST_ATTESTATION_EXIT=1", "gh", "Verifying build provenance"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newWorkerBootstrapFixture(t)
			status, calls, output := fixture.run(t, []string{test.environment}, "--no-setup")
			if status == 0 || !strings.Contains(output, test.message) {
				t.Fatalf("verification failure was not reported: exit %d\n%s", status, output)
			}
			if len(calls) == 0 || calls[len(calls)-1][0] != test.lastCall {
				t.Fatalf("did not stop at failed verification: %v", calls)
			}
			for _, call := range calls {
				if call[0] == "candidate" || call[0] == "candidate-readable" {
					t.Fatalf("unverified candidate executed: %v", calls)
				}
			}
		})
	}
}

func TestWorkerBootstrapProvenanceIsOptional(t *testing.T) {
	fixture := newWorkerBootstrapFixture(t)
	status, calls, output := fixture.run(t, []string{"THINGS_INDEX_BOOTSTRAP_TEST_AUTH_EXIT=1"}, "--no-setup")
	if status != 0 || !strings.Contains(output, "Skipping provenance verification") {
		t.Fatalf("unauthenticated gh blocked installation: exit %d\n%s", status, output)
	}
	var names []string
	for _, call := range calls {
		names = append(names, call[0])
	}
	// Skipping optional provenance must retain mandatory metadata and pinned
	// signature checks before the child executes.
	want := []string{"uname", "curl", "gh", "xattr", "codesign", "candidate", "candidate-readable"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("unexpected handoff without authenticated gh: %v", calls)
	}
}
