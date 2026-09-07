package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type workerInstallFixture struct {
	installer                    workerInstaller
	stops, setups, verifications int
}

func newWorkerInstallFixture(t *testing.T, existing bool) *workerInstallFixture {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(directory, "download", "things-index")
	target := filepath.Join(directory, "home", ".local", "bin", "things-index")
	if err = os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(source, []byte("verified signed release"), 0o755); err != nil {
		t.Fatal(err)
	}
	if existing {
		if err = os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(target, []byte("original signed worker"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fixture := &workerInstallFixture{}
	fixture.installer = workerInstaller{source: source, target: target,
		verify: func(_ context.Context, installed, candidate string) error {
			fixture.verifications++
			if _, err := os.Stat(installed); err != nil {
				return err
			}
			data, err := os.ReadFile(candidate)
			if err != nil {
				return err
			}
			if string(data) != "verified signed release" {
				return errors.New("not the trusted release")
			}
			return nil
		},
		quarantine: func(context.Context, string) error { return nil },
		copy: func(_ context.Context, source, destination string) error {
			contents, err := os.ReadFile(source)
			if err != nil {
				return err
			}
			return os.WriteFile(destination, contents, 0o755)
		},
		stop: func(context.Context) error { fixture.stops++; return nil },
		setup: func(_ context.Context, path string) error {
			fixture.setups++
			if path != target {
				t.Fatalf("setup executed from staging: %s", path)
			}
			contents, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if string(contents) != "verified signed release" {
				return errors.New("installed release differs")
			}
			return nil
		},
	}
	return fixture
}

func TestWorkerInstallArguments(t *testing.T) {
	for _, test := range []struct {
		args          []string
		noSetup, fail bool
	}{{nil, false, false}, {[]string{"--no-setup"}, true, false}, {[]string{"--setup"}, false, true}, {[]string{"--no-setup", "extra"}, false, true}} {
		got, err := parseWorkerInstallArguments(test.args)
		if got != test.noSetup || (err != nil) != test.fail {
			t.Fatalf("%v: got %v,%v", test.args, got, err)
		}
	}
}

func TestWorkerInstallRejectsUnverifiedSourceBeforeChanges(t *testing.T) {
	for _, failure := range []string{"identity", "quarantine", "copy changed"} {
		t.Run(failure, func(t *testing.T) {
			fixture := newWorkerInstallFixture(t, true)
			switch failure {
			case "identity":
				fixture.installer.verify = func(context.Context, string, string) error { return errors.New("different certificate") }
			case "quarantine":
				fixture.installer.quarantine = func(context.Context, string) error { return errors.New("quarantined") }
			case "copy changed":
				fixture.installer.copy = func(_ context.Context, _, destination string) error {
					return os.WriteFile(destination, []byte("changed"), 0o755)
				}
			}
			if _, err := fixture.installer.install(context.Background(), false); err == nil {
				t.Fatal("invalid release installed")
			}
			data, _ := os.ReadFile(fixture.installer.target)
			if string(data) != "original signed worker" || fixture.stops != 0 || fixture.setups != 0 {
				t.Fatal("validation failure changed the running installation")
			}
			backups, _ := filepath.Glob(filepath.Join(filepath.Dir(fixture.installer.target), ".things-index-backup-*"))
			if len(backups) != 0 {
				t.Fatal("backup created before release verification")
			}
		})
	}
}

func TestWorkerInstallBacksUpBeforeStoppingAndRunsInstalledPath(t *testing.T) {
	fixture := newWorkerInstallFixture(t, true)
	originalStop := fixture.installer.stop
	fixture.installer.stop = func(ctx context.Context) error {
		backups, _ := filepath.Glob(filepath.Join(filepath.Dir(fixture.installer.target), ".things-index-backup-*", "things-index"))
		if len(backups) != 1 {
			t.Fatal("worker stopped without a retained backup")
		}
		data, _ := os.ReadFile(fixture.installer.target)
		if string(data) != "original signed worker" {
			t.Fatal("binary replaced before worker stop")
		}
		return originalStop(ctx)
	}
	result, err := fixture.installer.install(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.stops != 1 || fixture.setups != 1 || fixture.verifications != 3 {
		t.Fatalf("unexpected lifecycle: stop=%d setup=%d verify=%d", fixture.stops, fixture.setups, fixture.verifications)
	}
	data, err := os.ReadFile(result.backup)
	if err != nil || string(data) != "original signed worker" {
		t.Fatalf("backup not retained: %q,%v", data, err)
	}
	info, err := os.Stat(filepath.Dir(result.backup))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatal("backup directory is not private")
	}
	staged, _ := filepath.Glob(filepath.Join(filepath.Dir(fixture.installer.target), ".things-index-install-*"))
	if len(staged) != 0 {
		t.Fatal("temporary replacement was retained")
	}
}

func TestWorkerInstallNoSetupLeavesStopped(t *testing.T) {
	fixture := newWorkerInstallFixture(t, false)
	result, err := fixture.installer.install(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if result.backup != "" || fixture.stops != 1 || fixture.setups != 0 {
		t.Fatalf("unexpected first-install no-setup lifecycle: %#v", result)
	}
	data, _ := os.ReadFile(fixture.installer.target)
	if string(data) != "verified signed release" {
		t.Fatal("release not installed")
	}
}

func TestWorkerInstallSetupFailureStopsAndPreservesBothBinaries(t *testing.T) {
	for _, failure := range []error{errors.New("setup failed"), context.Canceled} {
		t.Run(failure.Error(), func(t *testing.T) {
			fixture := newWorkerInstallFixture(t, true)
			fixture.installer.setup = func(context.Context, string) error { return failure }
			result, err := fixture.installer.install(context.Background(), false)
			if !errors.Is(err, failure) {
				t.Fatalf("unexpected error: %v", err)
			}
			if fixture.stops != 2 {
				t.Fatalf("setup failure did not stop worker again: %d", fixture.stops)
			}
			old, _ := os.ReadFile(result.backup)
			current, _ := os.ReadFile(fixture.installer.target)
			if string(old) != "original signed worker" || string(current) != "verified signed release" {
				t.Fatal("failure lost a retained executable")
			}
		})
	}
}

func TestWorkerInstallStopsAfterFinalVerificationFailure(t *testing.T) {
	fixture := newWorkerInstallFixture(t, true)
	originalVerify := fixture.installer.verify
	fixture.installer.verify = func(ctx context.Context, installed, candidate string) error {
		if candidate == fixture.installer.target {
			return errors.New("final signature verification failed")
		}
		return originalVerify(ctx, installed, candidate)
	}
	result, err := fixture.installer.install(context.Background(), false)
	if err == nil || fixture.stops != 2 || fixture.setups != 0 || result.backup == "" {
		t.Fatalf("unsafe final verification failure: %v", err)
	}
}

func TestWorkerInstallRejectsTargetChangeWhileStopping(t *testing.T) {
	fixture := newWorkerInstallFixture(t, true)
	fixture.installer.stop = func(context.Context) error {
		fixture.stops++
		return os.WriteFile(fixture.installer.target, []byte("concurrent update"), 0o755)
	}
	result, err := fixture.installer.install(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("concurrent target change accepted: %v", err)
	}
	current, _ := os.ReadFile(fixture.installer.target)
	old, _ := os.ReadFile(result.backup)
	if string(current) != "concurrent update" || string(old) != "original signed worker" {
		t.Fatal("overwrote concurrent change or lost backup")
	}
}

func TestWorkerInstallRejectsSymlinksAndSameFile(t *testing.T) {
	for _, kind := range []string{"target", "source", "same path", "same inode"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newWorkerInstallFixture(t, true)
			switch kind {
			case "target":
				os.Remove(fixture.installer.target)
				if err := os.Symlink(fixture.installer.source, fixture.installer.target); err != nil {
					t.Fatal(err)
				}
			case "source":
				link := fixture.installer.source + "-link"
				if err := os.Symlink(fixture.installer.source, link); err != nil {
					t.Fatal(err)
				}
				fixture.installer.source = link
			case "same path":
				fixture.installer.source = fixture.installer.target
			case "same inode":
				os.Remove(fixture.installer.target)
				if err := os.Link(fixture.installer.source, fixture.installer.target); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := fixture.installer.install(context.Background(), false); err == nil {
				t.Fatal("unsafe path accepted")
			}
			if fixture.stops != 0 || fixture.setups != 0 {
				t.Fatal("unsafe path changed worker state")
			}
		})
	}
}
