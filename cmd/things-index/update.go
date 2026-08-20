package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	releaseRepo  = "nejmlabs/things-index"
	releaseAsset = "things-index-darwin-universal"
)

// runUpdate replaces this binary with the latest released one — the
// self-update counterpart of deploy/mac-worker-install.sh, sharing its asset,
// provenance-verification, and install-location conventions.
func runUpdate(args []string) error {
	force := false
	for _, arg := range args {
		switch arg {
		case "--force":
			force = true
		default:
			return fmt.Errorf("unknown update option %q (supported: --force)", arg)
		}
	}
	if runtime.GOOS != "darwin" {
		return errors.New("update replaces the macOS worker binary; update a server deployment with deploy/proxmox-update.sh")
	}

	latest, err := latestReleaseVersion()
	if err != nil {
		return fmt.Errorf("check latest release: %w", err)
	}
	if latest == version && !force {
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
	if err := downloadReleaseAsset(tempPath); err != nil {
		return err
	}
	defer os.Remove(tempPath)

	if err := verifyProvenance(tempPath); err != nil {
		return err
	}
	if err := os.Chmod(tempPath, 0o755); err != nil {
		return fmt.Errorf("mark downloaded binary executable: %w", err)
	}
	if err := os.Rename(tempPath, executablePath); err != nil {
		return fmt.Errorf("install downloaded binary: %w", err)
	}
	fmt.Printf("  ✓ Installed things-index %s\n", latest)

	restartWorkerAgentIfInstalled()
	return nil
}

// latestReleaseVersion asks the GitHub API for the newest release tag and
// normalizes it to the bare version scheme the version constant uses.
func latestReleaseVersion() (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	request, err := http.NewRequest(http.MethodGet,
		"https://api.github.com/repos/"+releaseRepo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "things-index/"+version)
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
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

func downloadReleaseAsset(destination string) error {
	fmt.Println("• Downloading the latest release binary...")
	client := &http.Client{Timeout: 5 * time.Minute}
	response, err := client.Get("https://github.com/" + releaseRepo + "/releases/latest/download/" + releaseAsset)
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
	return file.Close()
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
	fmt.Println("  ✓ Worker restarted on the new binary.")
}
