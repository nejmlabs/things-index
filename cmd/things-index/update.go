package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	releaseRepo  = "nejmlabs/things-index"
	releaseAsset = "things-index-darwin-universal"
)

// releaseSource resolves and downloads GitHub release artifacts; the bases
// are parameters so tests can point it at a local server.
type releaseSource struct {
	apiBase      string
	downloadBase string
	client       *http.Client
}

func defaultReleaseSource() *releaseSource {
	return &releaseSource{
		apiBase:      "https://api.github.com",
		downloadBase: "https://github.com",
		client:       &http.Client{Timeout: 5 * time.Minute},
	}
}

// runUpdate replaces this binary with the latest released one — the
// self-update counterpart of deploy/mac-worker-install.sh, sharing its asset,
// provenance-verification, and install-location conventions.
func runUpdate(args []string) error {
	force := false
	for _, arg := range args {
		switch arg {
		case "--force":
			force = true
		case "--help", "-h":
			fmt.Println("Usage: things-index update [--force]")
			fmt.Println("  Replaces this binary with the latest GitHub release (macOS only).")
			fmt.Println("  Provenance is verified when an authenticated gh CLI is available.")
			fmt.Println("  --force reinstalls even when already on the latest version.")
			return nil
		default:
			return fmt.Errorf("unknown update option %q (supported: --force)", arg)
		}
	}
	if runtime.GOOS != "darwin" {
		return errors.New("update replaces the macOS worker binary; update a server deployment with deploy/proxmox-update.sh")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	source := defaultReleaseSource()
	latest, err := source.latestVersion(ctx)
	if err != nil {
		return fmt.Errorf("check latest release: %w", err)
	}
	if !shouldUpdate(version, latest, force) {
		fmt.Printf("✓ things-index %s is already the latest release.\n", version)
		return nil
	}

	executablePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate running binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(executablePath); err == nil {
		executablePath = resolved
	}
	fmt.Printf("• Updating things-index %s → %s\n", version, latest)
	fmt.Printf("  Binary: %s\n", executablePath)

	tempPath := executablePath + ".update"
	if err := source.downloadAsset(ctx, latest, tempPath); err != nil {
		return err
	}
	defer os.Remove(tempPath)

	if err := verifyProvenance(tempPath); err != nil {
		return err
	}
	if err := os.Chmod(tempPath, 0o755); err != nil {
		return fmt.Errorf("mark downloaded binary executable: %w", err)
	}

	// Smoke-test the downloaded binary before committing to it.
	smoke := exec.CommandContext(ctx, tempPath, "version")
	smokeOutput, err := smoke.Output()
	if err != nil {
		return fmt.Errorf("downloaded binary failed its version check - keeping the current binary: %w", err)
	}
	fmt.Printf("  ✓ Downloaded binary runs: %s", firstLine(smokeOutput))

	// Swap with a rollback path: keep the old binary until the new one is in
	// place, and restore it if the final rename fails.
	backupPath := executablePath + ".old"
	_ = os.Remove(backupPath)
	if err := os.Rename(executablePath, backupPath); err != nil {
		return fmt.Errorf("stage current binary for rollback: %w", err)
	}
	if err := os.Rename(tempPath, executablePath); err != nil {
		if restoreErr := os.Rename(backupPath, executablePath); restoreErr != nil {
			return fmt.Errorf("install failed (%v) AND rollback failed (%v); reinstall from %s", err, restoreErr, "https://github.com/"+releaseRepo)
		}
		return fmt.Errorf("install downloaded binary (previous binary restored): %w", err)
	}
	_ = os.Remove(backupPath)
	fmt.Printf("  ✓ Installed things-index %s\n", latest)

	restartWorkerAgentIfInstalled()
	return nil
}

// shouldUpdate reports whether an update should proceed. Comparison is
// equality, not ordering: a locally built pre-release differs from the
// latest tag and the printed old → new versions make any rollback visible.
func shouldUpdate(current, latest string, force bool) bool {
	return force || current != latest
}

// latestVersion asks the GitHub API for the newest release tag and
// normalizes it to the bare version scheme the version constant uses.
func (r *releaseSource) latestVersion(ctx context.Context) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		r.apiBase+"/repos/"+releaseRepo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "things-index/"+version)
	response, err := r.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusTooManyRequests {
		return "", fmt.Errorf("GitHub API returned %s (unauthenticated rate limit is 60 requests/hour per IP; try again later)", response.Status)
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub API returned %s", response.Status)
	}
	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&release); err != nil {
		return "", err
	}
	normalized := normalizeReleaseTag(release.TagName)
	if normalized == "" {
		return "", fmt.Errorf("latest release has unusable tag %q", release.TagName)
	}
	return normalized, nil
}

func normalizeReleaseTag(tag string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(tag), "v"))
}

// downloadAsset fetches the asset for the exact version the update decision
// was made against — never "latest", which could change between the check
// and the download.
func (r *releaseSource) downloadAsset(ctx context.Context, assetVersion, destination string) error {
	fmt.Printf("• Downloading the v%s release binary...\n", assetVersion)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		r.downloadBase+"/"+releaseRepo+"/releases/download/v"+assetVersion+"/"+releaseAsset, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "things-index/"+version)
	response, err := r.client.Do(request)
	if err != nil {
		return fmt.Errorf("download release binary: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("release download returned %s", response.Status)
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("stage downloaded binary: %w", err)
	}
	if _, err := io.Copy(file, response.Body); err != nil {
		file.Close()
		os.Remove(destination)
		return fmt.Errorf("write downloaded binary: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(destination)
		return fmt.Errorf("sync downloaded binary: %w", err)
	}
	return file.Close()
}

func firstLine(output []byte) string {
	line, _, _ := strings.Cut(string(output), "\n")
	return line + "\n"
}

// verifyProvenance mirrors the installer: hard-fail a bad attestation when an
// authenticated gh CLI is available, otherwise continue with a note.
func verifyProvenance(path string) error {
	ghPath, err := exec.LookPath("gh")
	if err != nil || exec.Command(ghPath, "auth", "status").Run() != nil {
		fmt.Println("  • Skipping provenance verification (no authenticated gh CLI).")
		fmt.Printf("    To verify by hand later: gh attestation verify <binary> --repo %s\n", releaseRepo)
		return nil
	}
	fmt.Println("• Verifying build provenance attestation...")
	verify := exec.Command(ghPath, "attestation", "verify", path, "--repo", releaseRepo)
	verify.Stdout = io.Discard
	verify.Stderr = os.Stderr
	if err := verify.Run(); err != nil {
		return fmt.Errorf("provenance verification FAILED - keeping the current binary: %w", err)
	}
	fmt.Printf("  ✓ Provenance verified: built by GitHub Actions from %s\n", releaseRepo)
	return nil
}

// restartWorkerAgentIfInstalled kicks the launchd agent so a running worker
// picks up the new binary; a Mac without the agent installed is left alone.
func restartWorkerAgentIfInstalled() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	plistPath := filepath.Join(home, "Library", "LaunchAgents", workerLaunchAgentLabel+".plist")
	if _, err := os.Stat(plistPath); err != nil {
		return
	}
	fmt.Println("• Restarting the worker launch agent...")
	target := fmt.Sprintf("gui/%d/%s", os.Getuid(), workerLaunchAgentLabel)
	if err := exec.Command("launchctl", "kickstart", "-k", target).Run(); err != nil {
		fmt.Printf("  • Could not restart the agent (%v); it picks up the new binary on next login.\n", err)
		return
	}
	// The agent's launcher records the binary path chosen at setup time, so
	// claim only the restart, not which binary it now runs.
	fmt.Println("  ✓ Worker launch agent restarted.")
}
