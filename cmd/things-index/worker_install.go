package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// The download wrapper verifies the fixed release certificate before invoking
// this command. All filesystem, identity-continuity and launchd work then runs
// in Go; setup is executed from the final path so its TCC identity is correct.
func runWorkerInstall(arguments []string) error {
	if runtime.GOOS != "darwin" {
		return errors.New("worker installation requires macOS")
	}
	noSetup, err := parseWorkerInstallArguments(arguments)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()
	lifecycle, err := newWorkerSetupLifecycle()
	if err != nil {
		return err
	}
	target := filepath.Join(lifecycle.home, ".local", "bin", "things-index")
	installer := workerInstaller{
		source: lifecycle.executable, target: target,
		verify: func(ctx context.Context, installed, candidate string) error {
			_, err := verifyUpdateSigning(ctx, installed, candidate, runCodeSign)
			return err
		},
		quarantine: func(ctx context.Context, path string) error {
			output, err := runSetupCommand(ctx, "/usr/bin/xattr", path)
			if err != nil {
				return errors.New("could not inspect executable quarantine metadata")
			}
			for _, attribute := range strings.Split(string(output), "\n") {
				if attribute == "com.apple.quarantine" {
					return errors.New("executable is quarantined; installation will not remove quarantine or execute it")
				}
			}
			return nil
		},
		copy: func(ctx context.Context, source, destination string) error {
			_, err := runSetupCommand(ctx, "/usr/bin/ditto", "--rsrc", "--extattr", source, destination)
			if err != nil {
				return errors.New("could not copy executable with its metadata preserved")
			}
			return nil
		},
		stop: lifecycle.stop,
		setup: func(ctx context.Context, installed string) error {
			command := exec.CommandContext(ctx, installed, "worker", "--setup")
			command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
			command.WaitDelay = 2 * time.Second
			if err := command.Run(); err != nil {
				return errors.New("worker setup did not complete; the installed executable and backup were retained")
			}
			return ctx.Err()
		},
	}
	result, err := installer.install(ctx, noSetup)
	if result.backup != "" {
		fmt.Printf("• Original executable backup retained: %s\n", result.backup)
	}
	if err != nil {
		return err
	}
	fmt.Printf("✓ Installed things-index v%s at %s\n", version, target)
	if noSetup {
		fmt.Println("• The worker is stopped and disabled.")
		fmt.Printf("  Run %s worker --setup interactively to review permissions and start it.\n", target)
	}
	return nil
}

func parseWorkerInstallArguments(arguments []string) (bool, error) {
	if len(arguments) == 0 {
		return false, nil
	}
	if len(arguments) == 1 && arguments[0] == "--no-setup" {
		return true, nil
	}
	return false, errors.New("usage: install-worker [--no-setup]")
}

type workerInstallResult struct{ backup string }
type workerInstaller struct {
	source, target string
	verify         func(context.Context, string, string) error
	quarantine     func(context.Context, string) error
	copy           func(context.Context, string, string) error
	stop           func(context.Context) error
	setup          func(context.Context, string) error
}

func installFileHash(path string) ([sha256.Size]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		return [sha256.Size]byte{}, err
	}
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result, nil
}

func installRegularPath(path string, allowMissing bool) (os.FileInfo, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("installation requires clean absolute executable paths")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && allowMissing {
		// Evaluate the closest existing parent so missing directories do not hide
		// a symlink in ~/.local or its ancestry.
		parent := filepath.Dir(path)
		for {
			resolved, err := filepath.EvalSymlinks(parent)
			if err == nil {
				if resolved != parent {
					return nil, errors.New("installation path contains a symlink")
				}
				return nil, nil
			}
			if !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
			next := filepath.Dir(parent)
			if next == parent {
				return nil, errors.New("installation parent cannot be resolved")
			}
			parent = next
		}
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("installation requires regular executable files without symlinks")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	if resolved != path {
		return nil, errors.New("installation path contains a symlink")
	}
	return info, nil
}

func (i workerInstaller) install(ctx context.Context, noSetup bool) (result workerInstallResult, resultErr error) {
	source, err := installRegularPath(i.source, false)
	if err != nil {
		return result, err
	}
	existing, err := installRegularPath(i.target, true)
	if err != nil {
		return result, err
	}
	if i.source == i.target || (existing != nil && os.SameFile(source, existing)) {
		return result, errors.New("install-worker must run from the verified download; this executable is already at the target path")
	}
	if err = i.quarantine(ctx, i.source); err != nil {
		return result, err
	}
	installed := i.target
	if existing == nil {
		installed = i.source
	}
	// Complete signing continuity checks before touching launchd, creating an
	// installation directory, backing up, or replacing the existing executable.
	if err = i.verify(ctx, installed, i.source); err != nil {
		return result, fmt.Errorf("release signing verification failed: %w", err)
	}
	sourceHash, err := installFileHash(i.source)
	if err != nil {
		return result, err
	}
	var originalHash [sha256.Size]byte
	if existing != nil {
		originalHash, err = installFileHash(i.target)
		if err != nil {
			return result, err
		}
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	directory := filepath.Dir(i.target)
	if err = os.MkdirAll(directory, 0o755); err != nil {
		return result, err
	}
	if _, err = installRegularPath(i.target, true); err != nil {
		return result, err
	}
	staged, err := os.CreateTemp(directory, ".things-index-install-*")
	if err != nil {
		return result, err
	}
	stagedPath := staged.Name()
	if err = staged.Close(); err != nil {
		os.Remove(stagedPath)
		return result, err
	}
	defer os.Remove(stagedPath)
	if err = i.copy(ctx, i.source, stagedPath); err != nil {
		return result, err
	}
	if err = os.Chmod(stagedPath, 0o755); err != nil {
		return result, err
	}
	if err = i.checkCopy(ctx, stagedPath, sourceHash, installed); err != nil {
		return result, err
	}
	if existing != nil {
		backupDirectory, err := os.MkdirTemp(directory, ".things-index-backup-*")
		if err != nil {
			return result, err
		}
		backup := filepath.Join(backupDirectory, "things-index")
		if err = i.copy(ctx, i.target, backup); err != nil {
			return result, err
		}
		copied, err := installFileHash(backup)
		if err != nil {
			return result, err
		}
		if copied != originalHash {
			return result, errors.New("original executable backup failed hash verification")
		}
		result.backup = backup
		if err = syncInstallFile(backup); err != nil {
			return result, fmt.Errorf("flush original executable backup: %w", err)
		}
	}
	// Once stopping begins, any subsequent failure must leave this same agent
	// disabled. Cleanup has its own deadline, including after Ctrl-C or timeout.
	stopping := false
	defer func() {
		if resultErr != nil && stopping {
			cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := i.stop(cleanup); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("worker stop cleanup failed: %w", err))
			}
		}
	}()
	if err = i.unchangedTarget(existing, originalHash); err != nil {
		return result, err
	}
	stopping = true
	if err = i.stop(ctx); err != nil {
		return result, err
	}
	if err = i.unchangedTarget(existing, originalHash); err != nil {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if err = os.Rename(stagedPath, i.target); err != nil {
		return result, fmt.Errorf("replace worker executable: %w", err)
	}
	previous := result.backup
	if previous == "" {
		previous = i.source
	}
	if err = i.checkCopy(ctx, i.target, sourceHash, previous); err != nil {
		return result, err
	}
	if noSetup {
		return result, nil
	}
	if err = i.setup(ctx, i.target); err != nil {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	return result, nil
}

func (i workerInstaller) checkCopy(ctx context.Context, path string, expected [sha256.Size]byte, installed string) error {
	if _, err := installRegularPath(path, false); err != nil {
		return err
	}
	actual, err := installFileHash(path)
	if err != nil {
		return err
	}
	if actual != expected {
		return errors.New("copied executable failed SHA-256 verification")
	}
	if err = i.quarantine(ctx, path); err != nil {
		return err
	}
	if err = i.verify(ctx, installed, path); err != nil {
		return fmt.Errorf("copied executable failed signing verification: %w", err)
	}
	return syncInstallFile(path)
}

func syncInstallFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
func (i workerInstaller) unchangedTarget(original os.FileInfo, expected [sha256.Size]byte) error {
	current, err := installRegularPath(i.target, true)
	if err != nil {
		return err
	}
	if original == nil {
		if current != nil {
			return errors.New("an executable appeared at the target during installation")
		}
		return nil
	}
	if current == nil || !os.SameFile(original, current) {
		return errors.New("installed executable changed during installation")
	}
	hash, err := installFileHash(i.target)
	if err != nil {
		return err
	}
	if hash != expected {
		return errors.New("installed executable changed during installation")
	}
	return nil
}
