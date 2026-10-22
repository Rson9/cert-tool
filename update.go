package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ==========================================
// Self update: cert update
// ==========================================

const defaultReleaseRepo = "Rson9/cert-tool"

// releaseRepo returns the GitHub repository hosting releases.
func releaseRepo() string {
	if repo := os.Getenv("CERT_TOOL_REPO"); repo != "" {
		return repo
	}
	return defaultReleaseRepo
}

// releaseBaseURL returns the release download base URL (mirror-friendly).
func releaseBaseURL() string {
	if base := os.Getenv("CERT_TOOL_BASE_URL"); base != "" {
		return strings.TrimRight(base, "/")
	}
	return "https://github.com/" + releaseRepo() + "/releases"
}

func httpClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

// handleUpdate updates the cert binary to the latest release.
//
//	cert update            update to the latest release
//	cert update --check    only check, do not change anything
//	cert update --force    re-download even when already up to date
func handleUpdate(args []string) {
	checkOnly := false
	force := false
	for _, arg := range args {
		switch arg {
		case "--check":
			checkOnly = true
		case "--force":
			force = true
		}
	}

	fmt.Printf("Current version: %s\n", cyan(version))

	// 1. Determine the latest release (best effort — blind download fallback
	// keeps mirrors without an API working).
	latestTag := latestReleaseTag()
	if latestTag != "" {
		fmt.Printf("Latest release:  %s\n", cyan(latestTag))
	}

	if !force && latestTag != "" && !isNewerVersion(latestTag, version) {
		fmt.Println(green("✅ Already up to date."))
		return
	}

	if checkOnly {
		if latestTag == "" {
			fmt.Printf("%s could not determine the latest version (no release API reachable; set CERT_TOOL_BASE_URL for mirrors)\n", red("Error:"))
			fmt.Println("Hint: plain 'cert update' also works without an API — it downloads and compares instead.")
			os.Exit(1)
		}
		fmt.Printf("%s update available: %s -> %s (run: cert update)\n", yellow("⬆"), version, latestTag)
		return
	}

	// 2. Download the matching binary.
	asset := fmt.Sprintf("cert-%s-%s", runtime.GOOS, runtime.GOARCH)
	var url string
	if latestTag != "" {
		url = fmt.Sprintf("%s/download/%s/%s", releaseBaseURL(), latestTag, asset)
	} else {
		url = fmt.Sprintf("%s/latest/download/%s", releaseBaseURL(), asset)
	}
	fmt.Printf("Downloading %s...\n", cyan(url))

	tmpDir, err := os.MkdirTemp("", "cert-update-*")
	if err != nil {
		fmt.Printf("%s %v\n", red("Error:"), err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmpDir)

	binaryPath := filepath.Join(tmpDir, asset)
	if err := downloadFile(url, binaryPath); err != nil {
		fmt.Printf("%s download failed: %v\n", red("Error:"), err)
		os.Exit(1)
	}

	// 3. Verify the checksum when the release publishes one.
	verifyChecksum(binaryPath, asset, url)

	// 4. Smoke test the downloaded binary before replacing the running one.
	out, err := runBinary(binaryPath, "version")
	if err != nil {
		fmt.Printf("%s the downloaded binary does not run on this platform: %v\n", red("Error:"), err)
		os.Exit(1)
	}
	newVersion := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out), "cert "))
	fmt.Printf("Downloaded: %s\n", cyan(newVersion))

	// 5. Replace the running binary atomically.
	target := certBinaryPath()
	if err := replaceBinary(binaryPath, target); err != nil {
		fmt.Printf("%s could not replace %s: %v\n", red("Error:"), target, err)
		os.Exit(1)
	}

	fmt.Println()
	fmt.Printf("%s cert updated: %s -> %s (%s)\n", green(bold("✅")), version, newVersion, target)
}

// latestReleaseTag resolves the newest release tag, trying the GitHub API
// first and falling back to the /releases/latest redirect (works for mirrors).
func latestReleaseTag() string {
	if strings.Contains(releaseBaseURL(), "github.com") {
		req, err := http.NewRequest(http.MethodGet, "https://api.github.com/repos/"+releaseRepo()+"/releases/latest", nil)
		if err == nil {
			req.Header.Set("User-Agent", "cert-tool/"+version)
			resp, err := httpClient().Do(req)
			if err == nil {
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					var body struct {
						TagName string `json:"tag_name"`
					}
					if err := json.NewDecoder(resp.Body).Decode(&body); err == nil && body.TagName != "" {
						return body.TagName
					}
				}
			}
		}
	}

	// Fallback: read the tag out of the /releases/latest redirect target.
	client := httpClient()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := client.Get(releaseBaseURL() + "/latest")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	location := resp.Header.Get("Location")
	if location == "" {
		return ""
	}
	parts := strings.Split(strings.TrimRight(location, "/"), "/")
	tag := parts[len(parts)-1]
	if tag == "latest" {
		return ""
	}
	return tag
}

// downloadFile fetches url into path.
func downloadFile(url, path string) error {
	resp, err := httpClient().Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected HTTP status %s", resp.Status)
	}
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return err
	}
	return os.Chmod(path, 0755)
}

// verifyChecksum compares the downloaded asset against the release
// checksums.txt (best effort: skipped when the release has none).
func verifyChecksum(binaryPath, asset, assetURL string) {
	sumArgs := []string{"sha256sum"}
	if _, err := exec.LookPath("sha256sum"); err != nil {
		if _, err := exec.LookPath("shasum"); err != nil {
			fmt.Printf("%s no sha256 tool available, skipping checksum verification\n", yellow("Note:"))
			return
		}
		sumArgs = []string{"shasum", "-a", "256"}
	}

	sumsURL := strings.TrimSuffix(assetURL, asset) + "checksums.txt"
	sumsPath := binaryPath + ".sums"
	if err := downloadFile(sumsURL, sumsPath); err != nil {
		fmt.Printf("%s checksums.txt not published, skipping verification\n", yellow("Note:"))
		return
	}

	expected := ""
	if data, err := os.ReadFile(sumsPath); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[1] == asset {
				expected = fields[0]
				break
			}
		}
	}
	if expected == "" {
		fmt.Printf("%s no checksum entry for %s, skipping verification\n", yellow("Note:"), asset)
		return
	}

	out, err := runBinary(sumArgs[0], append(sumArgs[1:], binaryPath)...)
	if err != nil {
		fmt.Printf("%s could not compute checksum, skipping verification\n", yellow("Note:"))
		return
	}
	fields := strings.Fields(out)
	if len(fields) == 0 || fields[0] != expected {
		fmt.Printf("%s checksum mismatch for %s\n  expected: %s\n  actual:   %s\n", red("Error:"), asset, expected, fields[0])
		os.Exit(1)
	}
	fmt.Println(green("Checksum verified"))
}

// replaceBinary atomically replaces the running binary with newPath.
// A running executable cannot be overwritten in place on Linux (ETXTBSY),
// so the new binary is staged next to the target and renamed over it.
func replaceBinary(newPath, targetPath string) error {
	staging := filepath.Join(filepath.Dir(targetPath), ".cert-update-staging")

	data, err := os.ReadFile(newPath)
	if err != nil {
		return err
	}
	if err := writePrivilegedFile(staging, data, 0755); err != nil {
		return err
	}
	if err := os.Rename(staging, targetPath); err == nil {
		return nil
	}
	if err := runPrivileged("mv", staging, targetPath); err != nil {
		_ = removePrivilegedFile(staging)
		return err
	}
	return nil
}

// runBinary executes a binary and returns its combined output.
func runBinary(path string, args ...string) (string, error) {
	out, err := exec.Command(path, args...).CombinedOutput()
	return string(out), err
}

// parseVersion extracts (major, minor, patch) from strings like
// "v1.2.3", "1.2.3", "v0.4.0-test" or "v1.2.3-5-g75df0f1".
func parseVersion(s string) (major, minor, patch int, ok bool) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "v"))
	if s == "" {
		return 0, 0, 0, false
	}
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '.' || r == '-' })
	nums := make([]int, 3)
	for i := 0; i < 3 && i < len(parts); i++ {
		digits := ""
		for _, r := range parts[i] {
			if r >= '0' && r <= '9' {
				digits += string(r)
			} else {
				break
			}
		}
		if digits == "" {
			if i == 0 {
				return 0, 0, 0, false // not a version at all ("dev", "main", …)
			}
			break
		}
		nums[i], _ = strconv.Atoi(digits)
	}
	return nums[0], nums[1], nums[2], true
}

// isNewerVersion reports whether latest is newer than current. Unknown
// versions (dev builds, odd tags) always allow an update.
func isNewerVersion(latest, current string) bool {
	lm, lmi, lp, lok := parseVersion(latest)
	cm, cmi, cp, cok := parseVersion(current)
	if !lok || !cok {
		return true
	}
	if lm != cm {
		return lm > cm
	}
	if lmi != cmi {
		return lmi > cmi
	}
	return lp > cp
}
