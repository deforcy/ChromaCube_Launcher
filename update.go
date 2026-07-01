package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Temp names used by the in-place self-update swap. They sit next to the running
// exe (same volume) so the renames are atomic.
const (
	newUpdateName = ".chromacube-update.exe"
	oldUpdateName = ".chromacube-old.exe"
)

// How often a running app re-checks the published version (it also re-checks
// whenever the window is brought back from the tray).
const updateCheckInterval = 15 * time.Minute

// updateInfo is the version manifest the launcher Worker serves at
// remoteConfigURL + "version" (set from the admin panel). When the running
// build is older than Latest, the app locks itself behind the update screen.
type updateInfo struct {
	Latest string `json:"latest"`
	URL    string `json:"url"`
	Notes  string `json:"notes"`
	SHA256 string `json:"sha256"` // optional integrity check for the download
}

// fetchUpdateInfo asks the Worker for the latest published version. Best effort:
// any error leaves the app usable (we never lock on a failed/blank check).
func fetchUpdateInfo() (updateInfo, error) {
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get(remoteConfigURL + "version")
	if err != nil {
		return updateInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return updateInfo{}, fmt.Errorf("version endpoint HTTP %d", resp.StatusCode)
	}
	var info updateInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return updateInfo{}, err
	}
	return info, nil
}

// isUpdateRequired reports whether latest is strictly newer than the running
// build. Developer builds (appVersion "dev"/empty) and a blank manifest are
// never gated.
func isUpdateRequired(current, latest string) bool {
	if latest == "" || current == "" || current == "dev" {
		return false
	}
	return compareVersions(current, latest) < 0
}

// compareVersions compares dotted numeric versions (e.g. "1.2.0" vs "1.10.0").
// Returns -1 if a<b, 0 if equal, 1 if a>b; missing parts count as 0.
func compareVersions(a, b string) int {
	pa, pb := versionParts(a), versionParts(b)
	n := len(pa)
	if len(pb) > n {
		n = len(pb)
	}
	for i := 0; i < n; i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func versionParts(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 { // drop any pre-release/build suffix
		v = v[:i]
	}
	fields := strings.Split(v, ".")
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil {
			n = 0
		}
		out = append(out, n)
	}
	return out
}

// ----- self-update mechanics -------------------------------------------------

// downloadFile streams url to dest (overwriting). A generous timeout allows for
// a multi-megabyte binary over a slow link.
func downloadFile(url, dest string) error {
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, resp.Body)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// verifySHA256 checks that the file at path hashes to want (hex). An empty want
// skips the check (no hash published).
func verifySHA256(path, want string) error {
	want = strings.ToLower(strings.TrimSpace(want))
	if want == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return fmt.Errorf("checksum mismatch (expected %s, got %s)", want, got)
	}
	return nil
}

// relaunch starts a fresh copy of the binary at path and returns immediately.
// The current process (already elevated) hands its token to the child, so no new
// UAC prompt appears.
func relaunch(path string, args ...string) error {
	cmd := exec.Command(path, args...)
	cmd.Dir = filepath.Dir(path)
	return cmd.Start()
}

// fetchLatestRelease returns the tag and body of the repo's latest GitHub
// release, used to show "what changed" after a self-update. Best effort.
func fetchLatestRelease() (tag, body string) {
	if githubRepo == "" {
		return "", ""
	}
	client := &http.Client{Timeout: 8 * time.Second}
	req, err := http.NewRequest("GET", "https://api.github.com/repos/"+githubRepo+"/releases/latest", nil)
	if err != nil {
		return "", ""
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", ""
	}
	var rel struct {
		TagName string `json:"tag_name"`
		Body    string `json:"body"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", ""
	}
	return rel.TagName, rel.Body
}

// cleanupOldUpdate removes the previous binary left over from a self-update swap.
// It cannot be deleted while it is the running process, so we clear it on the
// next launch.
func cleanupOldUpdate() {
	self, err := os.Executable()
	if err != nil {
		return
	}
	_ = os.Remove(filepath.Join(filepath.Dir(self), oldUpdateName))
}
