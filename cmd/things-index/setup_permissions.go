package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type setupCommand func(context.Context, string, ...string) ([]byte, error)

type fdaStatus string

const (
	fdaMatched fdaStatus = "matched"
	fdaMissing fdaStatus = "missing"
	fdaStale   fdaStatus = "stale"
	fdaUnknown fdaStatus = "unknown"
)

// Commands may return launchd configuration, including secrets. Never include
// their output (or arguments) in a setup error.
func runSetupCommand(ctx context.Context, executable string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, args...)
	command.Env = append(os.Environ(), "LC_ALL=C")
	command.WaitDelay = 2 * time.Second
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		return output, ctx.Err()
	}
	return output, err
}

func readSetupLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", errors.New("setup input ended before confirmation; run the wizard interactively")
	}
	return strings.TrimSpace(line), nil
}

// Only one prompt reads at a time. On cancellation the wizard exits, so a
// blocked stdin goroutine cannot compete with another reader or accept input.
func readSetupLineContext(ctx context.Context, reader *bufio.Reader) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	type result struct {
		line string
		err  error
	}
	ready := make(chan result, 1)
	go func() { line, err := readSetupLine(reader); ready <- result{line, err} }()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case value := <-ready:
		return value.line, value.err
	}
}

func waitSetupContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func parseDisabledOverride(output []byte, label string) (bool, error) {
	// Recognize the target line before interpreting its value. A changed or
	// malformed format must not be mistaken for an absent override.
	target := regexp.MustCompile(`(?m)^\s*"` + regexp.QuoteMeta(label) + `"([^\n]*)$`)
	matches := target.FindAllSubmatch(output, -1)
	if len(matches) == 0 {
		return false, nil
	}
	if len(matches) != 1 {
		return false, errors.New("ambiguous LaunchAgent enablement")
	}
	value := regexp.MustCompile(`^\s*=>\s*(true|false|enabled|disabled)\s*$`).FindSubmatch(matches[0][1])
	if len(value) != 2 {
		return false, errors.New("unrecognized LaunchAgent enablement")
	}
	return string(value[1]) == "true" || string(value[1]) == "disabled", nil
}

type setupAgentStatus struct {
	loaded bool
	pid    int
}

func parseSetupAgentStatus(output []byte, err error) (setupAgentStatus, error) {
	if err != nil {
		if bytes.Contains(bytes.ToLower(output), []byte("could not find service")) {
			return setupAgentStatus{}, nil
		}
		return setupAgentStatus{}, errors.New("could not inspect this LaunchAgent")
	}
	matches := regexp.MustCompile(`(?m)^\s*pid = ([0-9]+)\s*$`).FindAllSubmatch(output, -1)
	if len(matches) > 1 {
		return setupAgentStatus{}, errors.New("ambiguous LaunchAgent PID")
	}
	status := setupAgentStatus{loaded: true}
	if len(matches) == 1 {
		status.pid, _ = strconv.Atoi(string(matches[0][1]))
		if status.pid <= 0 {
			return setupAgentStatus{}, errors.New("invalid LaunchAgent PID")
		}
	}
	return status, nil
}

// The TCC database is an optional, read-only diagnostic. An inaccessible or
// unfamiliar schema means unknown, never an inferred grant or a request to
// grant Terminal Full Disk Access.
func inspectSetupFDA(ctx context.Context, databasePath, executable, architecture string, run codeSignCommand) fdaStatus {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	uri := (&url.URL{Scheme: "file", Path: databasePath}).String() + "?mode=ro&_query_only=1&_busy_timeout=1000"
	db, err := sql.Open("sqlite3", uri)
	if err != nil {
		return fdaUnknown
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT auth_value, csreq FROM access WHERE service=? AND client=? AND client_type=1`, "kTCCServiceSystemPolicyAllFiles", executable)
	if err != nil {
		return fdaUnknown
	}
	var grant int
	var requirement []byte
	count := 0
	for rows.Next() {
		count++
		if rows.Scan(&grant, &requirement) != nil {
			rows.Close()
			return fdaUnknown
		}
	}
	rowErr := rows.Err()
	rows.Close()
	if rowErr != nil {
		return fdaUnknown
	}
	if count == 0 {
		return fdaMissing
	}
	if count != 1 || grant != 2 || len(requirement) == 0 {
		return fdaStale
	}
	directory, err := os.MkdirTemp("", "things-index-fda-")
	if err != nil {
		return fdaUnknown
	}
	defer os.RemoveAll(directory)
	path := filepath.Join(directory, "requirement.bin")
	if os.WriteFile(path, requirement, 0o600) != nil {
		return fdaUnknown
	}
	_, err = run(ctx, "--verify", "--strict", "--architecture", architecture, "--test-requirement", path, executable)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fdaUnknown
	}
	if err != nil {
		return fdaStale
	}
	return fdaMatched
}

type workerSetupLifecycle struct {
	executable, home, domain, logPath, markerPath string
	run                                           setupCommand
	checkFDA                                      func(context.Context) fdaStatus
	now                                           func() time.Time
	wait                                          func(context.Context, time.Duration) error
	alive                                         func(int) bool
	touched                                       bool
	grant                                         fdaStatus
	previousPID                                   int
}

func newWorkerSetupLifecycle() (*workerSetupLifecycle, error) {
	if os.Getuid() == 0 {
		return nil, errors.New("run worker setup as the signed-in Mac user, without sudo")
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	if _, err := runSetupCommand(context.Background(), "/bin/launchctl", "print", domain); err != nil {
		return nil, errors.New("worker setup requires this user's logged-in macOS desktop session")
	}
	path, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve setup executable: %w", err)
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("resolve actual executable: %w", err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	state := filepath.Join(home, "Library", "Application Support", "ThingsIndex")
	if custom := os.Getenv("THINGS_INDEX_STATE_DIR"); custom != "" && custom != state {
		return nil, errors.New("custom THINGS_INDEX_STATE_DIR requires a matching manual LaunchAgent configuration")
	}
	lifecycle := &workerSetupLifecycle{executable: path, home: home, domain: domain,
		logPath: filepath.Join(home, "Library", "Logs", "ThingsIndex", "worker-error.log"), markerPath: filepath.Join(state, "automation-consent-granted"),
		run: runSetupCommand, now: time.Now,
		wait:  waitSetupContext,
		alive: func(pid int) bool { err := syscall.Kill(pid, 0); return err == nil || errors.Is(err, syscall.EPERM) },
	}
	architecture := runtime.GOARCH
	if architecture == "amd64" {
		architecture = "x86_64"
	}
	lifecycle.checkFDA = func(ctx context.Context) fdaStatus {
		return inspectSetupFDA(ctx, "/Library/Application Support/com.apple.TCC/TCC.db", path, architecture, runCodeSign)
	}
	return lifecycle, nil
}

func (s *workerSetupLifecycle) service() string { return s.domain + "/" + workerLaunchAgentLabel }
func (s *workerSetupLifecycle) status(ctx context.Context) (setupAgentStatus, error) {
	output, err := s.run(ctx, "/bin/launchctl", "print", s.service())
	return parseSetupAgentStatus(output, err)
}
func (s *workerSetupLifecycle) disabled(ctx context.Context) (bool, error) {
	output, err := s.run(ctx, "/bin/launchctl", "print-disabled", s.domain)
	if err != nil {
		return false, errors.New("could not inspect LaunchAgent enablement")
	}
	return parseDisabledOverride(output, workerLaunchAgentLabel)
}
func (s *workerSetupLifecycle) command(ctx context.Context, args ...string) error {
	_, err := s.run(ctx, "/bin/launchctl", args...)
	if err != nil {
		return errors.New("LaunchAgent " + args[0] + " failed; inspect its status privately")
	}
	return nil
}

func (s *workerSetupLifecycle) stop(ctx context.Context) error {
	status, err := s.status(ctx)
	if err != nil {
		return err
	}
	s.touched = true
	if err = s.command(ctx, "disable", s.service()); err != nil {
		return err
	}
	if status.loaded {
		if err = s.command(ctx, "bootout", s.service()); err != nil {
			return err
		}
	}
	deadline := s.now().Add(12 * time.Second)
	for {
		current, err := s.status(ctx)
		if err != nil {
			return err
		}
		if !current.loaded && (status.pid == 0 || !s.alive(status.pid)) {
			break
		}
		if !s.now().Before(deadline) {
			return errors.New("worker did not stop; setup will not continue")
		}
		if err = s.wait(ctx, 200*time.Millisecond); err != nil {
			return err
		}
	}
	disabled, err := s.disabled(ctx)
	if err != nil {
		return err
	}
	if !disabled {
		return errors.New("worker did not remain disabled")
	}
	if status.pid != 0 {
		s.previousPID = status.pid
	}
	return nil
}

func (s *workerSetupLifecycle) prepareFDA(ctx context.Context, reader *bufio.Reader, output io.Writer) error {
	if err := s.stop(ctx); err != nil {
		return err
	}
	s.grant = s.checkFDA(ctx)
	if s.grant == fdaMatched {
		fmt.Fprintln(output, "  ✓ The stored Full Disk Access grant matches this signed executable.")
		return nil
	}
	fmt.Fprintln(output, "• The worker is stopped while you review Full Disk Access.")
	if s.grant == fdaUnknown {
		fmt.Fprintln(output, "  The stored grant is not readable here; setup will verify the worker's actual access after launch.")
	}
	fmt.Fprintln(output, "  In System Settings > Privacy & Security > Full Disk Access, remove the old ThingsIndex entry,")
	fmt.Fprintln(output, "  then add and enable this exact executable (toggling an old entry can retain its old identity):")
	fmt.Fprintln(output, "  "+s.executable)
	fmt.Fprintln(output, "  This is the worker executable. No Terminal Full Disk Access grant is needed for this step.")
	_, _ = s.run(ctx, "/usr/bin/open", "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles")
	_, _ = s.run(ctx, "/usr/bin/open", "-R", s.executable)
	fmt.Fprint(output, "• Type done after adding and enabling this executable, or cancel to stop: ")
	answer, err := readSetupLineContext(ctx, reader)
	if err != nil {
		return err
	}
	if !strings.EqualFold(answer, "done") {
		return errors.New("Full Disk Access confirmation canceled; worker remains stopped")
	}
	s.grant = s.checkFDA(ctx)
	if s.grant == fdaMissing || s.grant == fdaStale {
		return errors.New("the stored Full Disk Access grant still does not match; worker remains stopped")
	}
	if s.grant == fdaUnknown {
		fmt.Fprintln(output, "  • Full Disk Access was confirmed by you; its stored grant remains unverified.")
	} else {
		fmt.Fprintln(output, "  ✓ The stored Full Disk Access grant now matches this executable.")
	}
	return nil
}

func verifySetupSigning(ctx context.Context, executable string) error {
	if _, err := verifyUpdateSigning(ctx, executable, executable, runCodeSign); err != nil {
		return fmt.Errorf("setup requires the certificate-signed universal release for stable worker permissions; install a signed release first: %w", err)
	}
	for _, architecture := range []string{"arm64", "x86_64"} {
		signature, err := inspectMacSignature(ctx, executable, architecture, runCodeSign)
		if err != nil {
			return err
		}
		if strings.Contains(strings.ToLower(signature.requirement), "cdhash") {
			return errors.New("this executable's signing requirement changes with its code; install the stable signed release")
		}
	}
	return nil
}

type setupLogPosition struct {
	info    os.FileInfo
	offset  int64
	started time.Time
}

func setupLogSnapshot(path string, now time.Time) (setupLogPosition, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return setupLogPosition{started: now}, nil
	}
	if err != nil {
		return setupLogPosition{}, err
	}
	if !info.Mode().IsRegular() {
		return setupLogPosition{}, errors.New("worker log must be a regular file")
	}
	return setupLogPosition{info: info, offset: info.Size(), started: now}, nil
}
func freshSetupEvidence(logPath, markerPath string, start *setupLogPosition) (bool, error) {
	info, err := os.Lstat(logPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("worker log is not a regular file")
	}
	if start.info == nil {
		start.info = info
	} else if !os.SameFile(start.info, info) {
		return false, errors.New("worker log rotated during setup verification")
	}
	if info.Size() < start.offset || info.Size()-start.offset > 2*1024*1024 {
		return false, errors.New("worker log changed unexpectedly during setup verification")
	}
	file, err := os.Open(logPath)
	if err != nil {
		return false, err
	}
	defer file.Close()
	if _, err = file.Seek(start.offset, io.SeekStart); err != nil {
		return false, err
	}
	fresh, err := io.ReadAll(io.LimitReader(file, 2*1024*1024+1))
	if err != nil {
		return false, err
	}
	if len(fresh) > 2*1024*1024 {
		return false, errors.New("worker startup log exceeded its limit")
	}
	startText := []byte("Starting ThingsIndex worker...")
	count := bytes.Count(fresh, startText)
	if count > 1 {
		return false, errors.New("worker restarted during setup verification")
	}
	if count == 0 {
		return false, nil
	}
	segment := fresh[bytes.Index(fresh, startText):]
	for _, text := range []string{"Things 3 database OK", "Things 3 automation consent granted", "ThingsIndex worker ready"} {
		if !bytes.Contains(segment, []byte(text)) {
			return false, nil
		}
	}
	marker, err := os.Lstat(markerPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return marker.Mode().IsRegular() && !marker.ModTime().Before(start.started), nil
}

func archiveSetupMarker(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("Automation consent marker must be a regular file")
	}
	backup, err := os.CreateTemp(filepath.Dir(path), "automation-consent-granted.setup-backup-*")
	if err != nil {
		return err
	}
	name := backup.Name()
	if err = backup.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err = os.Rename(path, name); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

func (s *workerSetupLifecycle) waitReady(ctx context.Context, start *setupLogPosition) (int, error) {
	pid := 0
	var stableSince time.Time
	for {
		status, err := s.status(ctx)
		if err != nil {
			return 0, err
		}
		if pid != 0 && status.pid != pid {
			return 0, errors.New("worker PID changed during setup verification")
		}
		if status.pid != 0 {
			if status.pid == s.previousPID {
				return 0, errors.New("worker did not start with a new PID")
			}
			pid = status.pid
			ready, err := freshSetupEvidence(s.logPath, s.markerPath, start)
			if err != nil {
				return 0, err
			}
			if ready {
				if stableSince.IsZero() {
					stableSince = s.now()
				} else if s.now().Sub(stableSince) >= 2*time.Second {
					confirmed, err := s.status(ctx)
					if err != nil {
						return 0, err
					}
					if confirmed.pid != pid {
						return 0, errors.New("worker PID changed at the end of setup verification")
					}
					return pid, nil
				}
			} else {
				stableSince = time.Time{}
			}
		}
		if err = s.wait(ctx, 250*time.Millisecond); err != nil {
			return 0, fmt.Errorf("fresh database, Automation and worker startup were not verified: %w", err)
		}
	}
}

func (s *workerSetupLifecycle) startVerified(ctx context.Context, plistPath string) (pid int, resultErr error) {
	// Every failure, including an interrupted bootstrap, leaves only this agent
	// stopped/disabled. Cleanup gets its own deadline after the caller expires.
	defer func() {
		if resultErr != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := s.stop(cleanup); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("worker stop cleanup failed: %w", err))
			}
		}
	}()
	status, err := s.status(ctx)
	if err != nil {
		return 0, err
	}
	if status.loaded {
		return 0, errors.New("worker must be stopped before verification")
	}
	grant := s.checkFDA(ctx)
	if grant == fdaMissing || grant == fdaStale {
		return 0, errors.New("Full Disk Access no longer matches this executable; worker remains stopped")
	}
	s.grant = grant
	if err = archiveSetupMarker(s.markerPath); err != nil {
		return 0, fmt.Errorf("archive Automation marker: %w", err)
	}
	start, err := setupLogSnapshot(s.logPath, s.now())
	if err != nil {
		return 0, err
	}
	if err = s.command(ctx, "enable", s.service()); err != nil {
		return 0, err
	}
	if err = s.command(ctx, "bootstrap", s.domain, plistPath); err != nil {
		return 0, err
	}
	return s.waitReady(ctx, &start)
}

// Replace secret-bearing files atomically so a prior permissive mode or
// interrupted write cannot expose or truncate the launcher's credentials.
func writeSetupFile(path string, contents []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".things-index-setup-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.Write(contents); err != nil {
		file.Close()
		return err
	}
	if err = file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
