package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	updateRepoOwner = "Borgels"
	updateRepoName  = "clawcontrol-agent"
)

var (
	updateVersion string
	updateYes     bool
)

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Check for and apply CLI updates",
	Long: `Manage clawcontrol CLI updates.

Use "clawcontrol update check" to check for newer releases.
Use "clawcontrol update apply" to download and install an update.`,
}

var updateCheckCmd = &cobra.Command{
	Use:   "check",
	Short: "Check for a newer release",
	RunE:  runUpdateCheck,
}

var updateApplyCmd = &cobra.Command{
	Use:   "apply",
	Short: "Download and install an update",
	Long: `Download and install a clawcontrol-agent release binary.

By default this installs the latest release. Use --version to install
a specific version tag such as v0.2.12.`,
	RunE: runUpdateApply,
}

func init() {
	rootCmd.AddCommand(updateCmd)
	updateCmd.AddCommand(updateCheckCmd)
	updateCmd.AddCommand(updateApplyCmd)

	updateApplyCmd.Flags().StringVar(&updateVersion, "version", "", "Version tag to install (default: latest release)")
	updateApplyCmd.Flags().BoolVarP(&updateYes, "yes", "y", false, "Skip confirmation prompt")
}

type githubRelease struct {
	TagName string `json:"tag_name"`
	Name    string `json:"name"`
	HTMLURL string `json:"html_url"`
}

type parsedVersion struct {
	major int
	minor int
	patch int
	pre   string
	valid bool
}

func runUpdateCheck(cmd *cobra.Command, args []string) error {
	latest, err := fetchLatestRelease()
	if err != nil {
		return err
	}
	current := normalizeVersion(agentVersion)
	latestTag := normalizeVersion(latest.TagName)

	cmp := compareVersionStrings(current, latestTag)
	fmt.Printf("Current version: %s\n", current)
	fmt.Printf("Latest version:  %s\n", latestTag)
	fmt.Printf("Release page:    %s\n", latest.HTMLURL)

	switch {
	case cmp < 0:
		fmt.Println("Update available.")
	case cmp == 0:
		fmt.Println("You are up to date.")
	default:
		fmt.Println("Running a newer version than the latest published release.")
	}
	return nil
}

func runUpdateApply(cmd *cobra.Command, args []string) error {
	targetVersion := strings.TrimSpace(updateVersion)
	if targetVersion == "" {
		latest, err := fetchLatestRelease()
		if err != nil {
			return err
		}
		targetVersion = latest.TagName
	}
	targetVersion = normalizeVersion(targetVersion)
	if !strings.HasPrefix(targetVersion, "v") {
		return fmt.Errorf("invalid version %q: expected v-prefixed semantic version (e.g. v0.2.12)", targetVersion)
	}

	current := normalizeVersion(agentVersion)
	if compareVersionStrings(current, targetVersion) == 0 {
		fmt.Printf("Already running %s.\n", current)
		return nil
	}

	targetPath, err := resolveExecutablePath()
	if err != nil {
		return err
	}
	if err := ensureWritableTarget(targetPath); err != nil {
		return err
	}

	osName, archName, err := releaseAssetSuffix()
	if err != nil {
		return err
	}
	assetName := fmt.Sprintf("clawcontrol-agent-%s-%s", osName, archName)
	assetURL := fmt.Sprintf("https://github.com/%s/%s/releases/download/%s/%s", updateRepoOwner, updateRepoName, targetVersion, assetName)
	checksumURL := assetURL + ".sha256"

	if !updateYes {
		fmt.Printf("Current: %s\n", current)
		fmt.Printf("Target:  %s\n", targetVersion)
		fmt.Printf("Asset:   %s\n", assetURL)
		fmt.Print("Proceed with update? [y/N]: ")
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		answer := strings.ToLower(strings.TrimSpace(line))
		if answer != "y" && answer != "yes" {
			fmt.Println("Update cancelled.")
			return nil
		}
	}

	expectedChecksum, err := fetchChecksum(checksumURL)
	if err != nil {
		return err
	}

	targetDir := filepath.Dir(targetPath)
	tempFile, err := os.CreateTemp(targetDir, ".clawcontrol-agent-update-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tempPath := tempFile.Name()
	_ = tempFile.Close()
	defer os.Remove(tempPath)

	if err := downloadFile(assetURL, tempPath); err != nil {
		return err
	}
	if err := os.Chmod(tempPath, 0o755); err != nil {
		return fmt.Errorf("chmod temp binary: %w", err)
	}

	actualChecksum, err := fileSHA256(tempPath)
	if err != nil {
		return err
	}
	if !strings.EqualFold(actualChecksum, expectedChecksum) {
		return fmt.Errorf("checksum mismatch: expected %s got %s", expectedChecksum, actualChecksum)
	}

	backupPath := targetPath + ".bak"
	_ = os.Remove(backupPath)
	if err := os.Rename(targetPath, backupPath); err != nil {
		return fmt.Errorf("backup current binary: %w", err)
	}
	if err := os.Rename(tempPath, targetPath); err != nil {
		_ = os.Rename(backupPath, targetPath)
		return fmt.Errorf("install updated binary: %w", err)
	}
	_ = os.Remove(backupPath)

	fmt.Printf("Updated successfully to %s.\n", targetVersion)
	fmt.Printf("Binary path: %s\n", targetPath)
	fmt.Println("If running as a systemd service, restart it: sudo systemctl restart clawcontrol-agent")
	return nil
}

func fetchLatestRelease() (githubRelease, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", updateRepoOwner, updateRepoName)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return githubRelease{}, fmt.Errorf("create latest release request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "clawcontrol-agent-updater")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return githubRelease{}, fmt.Errorf("request latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return githubRelease{}, fmt.Errorf("latest release request failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var rel githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return githubRelease{}, fmt.Errorf("decode latest release response: %w", err)
	}
	if strings.TrimSpace(rel.TagName) == "" {
		return githubRelease{}, fmt.Errorf("latest release response missing tag_name")
	}
	return rel, nil
}

func fetchChecksum(url string) (string, error) {
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Get(url)
	if err != nil {
		return "", fmt.Errorf("download checksum: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("checksum request failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read checksum response: %w", err)
	}
	fields := strings.Fields(string(body))
	if len(fields) == 0 {
		return "", fmt.Errorf("checksum file was empty")
	}
	return strings.TrimSpace(fields[0]), nil
}

func downloadFile(url string, path string) error {
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Get(url)
	if err != nil {
		return fmt.Errorf("download binary: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("binary download failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	out, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create temp binary: %w", err)
	}
	defer out.Close()
	if _, err := io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("write temp binary: %w", err)
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open file for checksum: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("compute checksum: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func resolveExecutablePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err == nil && strings.TrimSpace(resolved) != "" {
		return resolved, nil
	}
	return exe, nil
}

func ensureWritableTarget(targetPath string) error {
	dir := filepath.Dir(targetPath)
	f, err := os.CreateTemp(dir, ".clawcontrol-write-check-*")
	if err != nil {
		return fmt.Errorf("cannot write to %s (try running with sudo): %w", dir, err)
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return nil
}

func releaseAssetSuffix() (string, string, error) {
	var osName string
	switch runtime.GOOS {
	case "linux":
		osName = "linux"
	case "darwin":
		osName = "darwin"
	default:
		return "", "", fmt.Errorf("unsupported OS for self-update: %s", runtime.GOOS)
	}

	var archName string
	switch runtime.GOARCH {
	case "amd64":
		archName = "amd64"
	case "arm64":
		archName = "arm64"
	default:
		return "", "", fmt.Errorf("unsupported architecture for self-update: %s", runtime.GOARCH)
	}
	return osName, archName, nil
}

func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return v
	}
	if !strings.HasPrefix(strings.ToLower(v), "v") {
		return "v" + v
	}
	return "v" + strings.TrimPrefix(strings.TrimPrefix(v, "v"), "V")
}

func compareVersionStrings(a string, b string) int {
	pa := parseVersion(a)
	pb := parseVersion(b)
	if !pa.valid || !pb.valid {
		return strings.Compare(a, b)
	}
	if pa.major != pb.major {
		if pa.major < pb.major {
			return -1
		}
		return 1
	}
	if pa.minor != pb.minor {
		if pa.minor < pb.minor {
			return -1
		}
		return 1
	}
	if pa.patch != pb.patch {
		if pa.patch < pb.patch {
			return -1
		}
		return 1
	}
	if pa.pre == pb.pre {
		return 0
	}
	if pa.pre == "" {
		return 1
	}
	if pb.pre == "" {
		return -1
	}
	if pa.pre < pb.pre {
		return -1
	}
	return 1
}

func parseVersion(v string) parsedVersion {
	n := strings.TrimSpace(strings.TrimPrefix(strings.ToLower(v), "v"))
	if n == "" {
		return parsedVersion{}
	}
	parts := strings.SplitN(n, "-", 2)
	core := parts[0]
	pre := ""
	if len(parts) == 2 {
		pre = parts[1]
	}
	nums := strings.Split(core, ".")
	if len(nums) < 2 || len(nums) > 3 {
		return parsedVersion{}
	}
	major, err := strconv.Atoi(nums[0])
	if err != nil {
		return parsedVersion{}
	}
	minor, err := strconv.Atoi(nums[1])
	if err != nil {
		return parsedVersion{}
	}
	patch := 0
	if len(nums) == 3 {
		patch, err = strconv.Atoi(nums[2])
		if err != nil {
			return parsedVersion{}
		}
	}
	return parsedVersion{
		major: major,
		minor: minor,
		patch: patch,
		pre:   pre,
		valid: true,
	}
}
