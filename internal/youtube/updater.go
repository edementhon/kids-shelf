package youtube

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edementhon/kids-shelf/internal/proc"
)

// DefaultBaseURL is where the updater looks for yt-dlp releases: the GitHub
// API's "latest release" endpoint for yt-dlp/yt-dlp.
const DefaultBaseURL = "https://api.github.com/repos/yt-dlp/yt-dlp/releases/latest"

// The helper layout under the data dir. relHelperDir holds one directory
// per installed version plus the "current" pointer file (versions older
// than the current one are removed after each check, pruneOlder);
// PointerHelpers resolves the binary through it.
const (
	relHelperDir  = "helpers/yt-dlp"
	pointerName   = "current"
	lastCheckName = "lastcheck"
	sumsAssetName = "SHA2-256SUMS"
)

// Updater installs new yt-dlp releases under <data>/helpers/yt-dlp/<tag>/
// and switches the "current" pointer file to the new binary only after the
// download has been verified against the release's SHA2-256SUMS and passed
// a self-test (--version must print the release tag). A failure at any
// step leaves the current pointer untouched, so the working version always
// stays in place; older versions go only after the pointer has moved past
// them.
type Updater struct {
	DataDir string
	// BaseURL is the release-check endpoint; Empty means DefaultBaseURL.
	BaseURL string
	// Now supplies the clock for the once-a-day gate (tests override it).
	Now func() time.Time
	// Client is the HTTP client for the release check and downloads.
	Client *http.Client
	// Log receives non-fatal diagnostics.
	Log func(format string, args ...any)

	// mu serializes EnsureLatest: the app's daily worker and the YouTube
	// queue both call it at launch, and two installs of one version share
	// the same temp file names.
	mu sync.Mutex
}

// NewUpdater returns an Updater for the given data dir; an empty baseURL
// selects DefaultBaseURL. It is safe to call more than once.
func NewUpdater(dataDir, baseURL string) *Updater {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Updater{DataDir: dataDir, BaseURL: baseURL}
}

func (u *Updater) dir() string { return filepath.Join(u.DataDir, relHelperDir) }
func (u *Updater) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}
func (u *Updater) client() *http.Client {
	if u.Client != nil {
		return u.Client
	}
	return &http.Client{Timeout: 5 * time.Minute}
}
func (u *Updater) logf(format string, args ...any) {
	if u.Log != nil {
		u.Log(format, args...)
	}
}

// EnsureLatest installs the newest yt-dlp release when the once-a-day gate
// allows (or force is set) and the latest release is newer than the current
// binary. It reports whether it installed a new version. The "checked
// today" stamp is written only after a successful install or a confirmed
// up-to-date check, so a failed download is retried on the next call
// instead of being skipped until tomorrow. When no binary is installed
// yet, the gate does not apply at all: the first launch must always try to
// provide one.
func (u *Updater) EnsureLatest(ctx context.Context, force bool) (bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	today := u.now().UTC().Format("2006-01-02")
	cur, err := u.currentBinaryPath()
	if err != nil {
		return false, err
	}
	if !force && cur != "" {
		if b, err := os.ReadFile(filepath.Join(u.dir(), lastCheckName)); err == nil && strings.TrimSpace(string(b)) == today {
			return false, nil
		}
	}
	rel, err := u.fetchLatest(ctx)
	if err != nil {
		return false, err
	}
	tag := strings.TrimSpace(rel.Tag)
	if tag == "" {
		return false, errors.New("youtube: release has no tag")
	}
	asset, err := assetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return false, err
	}
	if cur != "" {
		curTag := filepath.Base(filepath.Dir(cur))
		if compareTags(tag, curTag) <= 0 {
			// Confirmed up to date: the check counts as done for today.
			u.stampCheck(today)
			u.pruneOlder()
			return false, nil
		}
	}
	if err := u.install(ctx, tag, rel, asset); err != nil {
		return false, err
	}
	u.stampCheck(today)
	u.pruneOlder()
	return true, nil
}

// ytdlpTag matches the version directories install creates: yt-dlp's
// release tags, YYYY.MM.DD with an optional .N for a same-day release.
var ytdlpTag = regexp.MustCompile(`^[0-9]{4}\.[0-9]{2}\.[0-9]{2}(\.[0-9]+)?$`)

// pruneOlder removes the version directories older than the one the
// "current" pointer names: nothing uses them once the pointer has moved.
// Only directories named like a yt-dlp tag and older than current are
// touched; the current one, anything newer, the pointer and the daily
// stamp stay. Best effort: a yt-dlp still running from an old directory
// (Windows) keeps it until a later check. Callers hold u.mu.
func (u *Updater) pruneOlder() {
	cur, err := u.currentBinaryPath()
	if err != nil || cur == "" {
		return
	}
	curTag := filepath.Base(filepath.Dir(cur))
	entries, err := os.ReadDir(u.dir())
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !ytdlpTag.MatchString(name) || compareTags(name, curTag) >= 0 {
			continue
		}
		if err := os.RemoveAll(filepath.Join(u.dir(), name)); err != nil {
			u.logf("youtube: removing the old yt-dlp %s: %v", name, err)
			continue
		}
		u.logf("youtube: removed the old yt-dlp %s", name)
	}
}

// stampCheck records that the release check ran today (UTC date).
func (u *Updater) stampCheck(today string) {
	if err := os.MkdirAll(u.dir(), 0o755); err != nil {
		u.logf("youtube: create helper dir: %v", err)
		return
	}
	if err := os.WriteFile(filepath.Join(u.dir(), lastCheckName), []byte(today+"\n"), 0o644); err != nil {
		u.logf("youtube: write last check stamp: %v", err)
	}
}

// currentBinaryPath returns the binary the "current" pointer names, or ""
// when no version is installed yet.
func (u *Updater) currentBinaryPath() (string, error) {
	b, err := os.ReadFile(filepath.Join(u.dir(), pointerName))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

type releaseInfo struct {
	Tag    string         `json:"tag_name"`
	Assets []releaseAsset `json:"assets"`
}

type releaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// fetchLatest asks the release endpoint (the GitHub API by default) for the
// newest yt-dlp release.
func (u *Updater) fetchLatest(ctx context.Context) (*releaseInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.BaseURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "kids-shelf/1.0 yt-dlp-updater")
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := u.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("youtube: release check: HTTP %s", resp.Status)
	}
	var rel releaseInfo
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("youtube: release check: %w", err)
	}
	return &rel, nil
}

// install downloads the platform's binary and the release's SHA2-256SUMS
// into a fresh version dir, verifies, self-tests, and only then renames
// them into place and flips the "current" pointer. Nothing is deleted
// here, so a failure at any step leaves the previous version fully in place.
func (u *Updater) install(ctx context.Context, tag string, rel *releaseInfo, asset string) error {
	dir := filepath.Join(u.dir(), tag)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	binURL := findAsset(rel, asset)
	if binURL == "" {
		return fmt.Errorf("youtube: release %s has no %s asset", tag, asset)
	}
	sumsURL := findAsset(rel, sumsAssetName)
	if sumsURL == "" {
		return fmt.Errorf("youtube: release %s has no %s asset", tag, sumsAssetName)
	}
	binTmp := filepath.Join(dir, asset+".download")
	sumsTmp := filepath.Join(dir, sumsAssetName+".download")
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(binTmp)
			_ = os.Remove(sumsTmp)
		}
	}()
	if err := u.download(ctx, binURL, binTmp); err != nil {
		return err
	}
	if err := u.download(ctx, sumsURL, sumsTmp); err != nil {
		return err
	}
	if err := verifyChecksum(binTmp, sumsTmp, asset); err != nil {
		return fmt.Errorf("youtube: verify %s: %w", asset, err)
	}
	if err := os.Chmod(binTmp, 0o755); err != nil {
		return err
	}
	out, err := u.runVersion(ctx, binTmp)
	if err != nil {
		return fmt.Errorf("youtube: new yt-dlp failed its self-test: %w", err)
	}
	if got := strings.TrimSpace(out); got != tag {
		return fmt.Errorf("youtube: self-test printed %q, want %q", got, tag)
	}
	binFinal := filepath.Join(dir, asset)
	sumsFinal := filepath.Join(dir, sumsAssetName)
	if err := os.Rename(binTmp, binFinal); err != nil {
		return err
	}
	if err := os.Rename(sumsTmp, sumsFinal); err != nil {
		return err
	}
	keep = true
	return u.switchPointer(binFinal)
}

// switchPointer atomically replaces the "current" pointer file: write to a
// temp name in the same directory, then rename over the pointer.
func (u *Updater) switchPointer(binPath string) error {
	tmp := filepath.Join(u.dir(), pointerName+".tmp")
	if err := os.WriteFile(tmp, []byte(binPath+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(u.dir(), pointerName))
}

// download fetches url into dst.
func (u *Updater) download(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := u.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("youtube: download %s: HTTP %s", url, resp.Status)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// verifyChecksum requires the release's SHA2-256SUMS to carry an entry for
// asset whose digest matches the downloaded binary.
func verifyChecksum(binPath, sumsPath, asset string) error {
	bin, err := os.ReadFile(binPath)
	if err != nil {
		return err
	}
	sums, err := os.ReadFile(sumsPath)
	if err != nil {
		return err
	}
	want, ok := findSumsLine(string(sums), asset)
	if !ok {
		return fmt.Errorf("%s has no entry for %s", sumsAssetName, asset)
	}
	got := sha256.Sum256(bin)
	if !strings.EqualFold(hex.EncodeToString(got[:]), want) {
		return fmt.Errorf("checksum mismatch for %s", asset)
	}
	return nil
}

// findSumsLine extracts the hex digest for asset from a "SHA2-256SUMS"
// file whose lines look like "<hex>  <name>" or "<hex> *<name>".
func findSumsLine(sums, asset string) (string, bool) {
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == asset && isHexDigest(fields[0]) {
			return fields[0], true
		}
	}
	return "", false
}

func isHexDigest(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// runVersion runs the binary's self-test: --version must succeed and print
// the release tag.
func (u *Updater) runVersion(ctx context.Context, binPath string) (string, error) {
	cmd := proc.CommandContext(ctx, binPath, "--version")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out.String(), nil
}

// assetName maps the build platform to the asset name yt-dlp publishes for
// it. Only the five supported targets have builds.
func assetName(goos, goarch string) (string, error) {
	switch goos {
	case "windows":
		if goarch == "amd64" {
			return "yt-dlp.exe", nil
		}
	case "darwin":
		if goarch == "arm64" || goarch == "amd64" {
			return "yt-dlp_macos", nil
		}
	case "linux":
		switch goarch {
		case "amd64":
			return "yt-dlp_linux", nil
		case "arm64":
			return "yt-dlp_linux_aarch64", nil
		}
	}
	return "", fmt.Errorf("youtube: no yt-dlp build for %s/%s", goos, goarch)
}

// findAsset returns the download URL of the named asset, or "".
func findAsset(rel *releaseInfo, name string) string {
	for _, a := range rel.Assets {
		if a.Name == name {
			return a.URL
		}
	}
	return ""
}

// compareTags orders release tags numerically by dot-separated components
// (yt-dlp tags are dates like 2026.10.21, where a plain string compare
// would mis-order 2026.9.1 against 2026.10.21). A leading "v" is ignored;
// a fully numeric tag outranks a non-numeric one, and two non-numeric tags
// fall back to a string compare.
func compareTags(a, b string) int {
	a, b = strings.TrimPrefix(a, "v"), strings.TrimPrefix(b, "v")
	if an, bn := numericTag(a), numericTag(b); an != bn {
		if an {
			return 1
		}
		return -1
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int64
		var xe, ye error
		if i < len(as) {
			x, xe = strconv.ParseInt(as[i], 10, 64)
		}
		if i < len(bs) {
			y, ye = strconv.ParseInt(bs[i], 10, 64)
		}
		if xe != nil || ye != nil {
			return strings.Compare(a, b)
		}
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
	}
	return 0
}

// numericTag reports whether every dot-separated part of tag is an integer.
func numericTag(tag string) bool {
	for _, p := range strings.Split(tag, ".") {
		if _, err := strconv.ParseInt(p, 10, 64); err != nil {
			return false
		}
	}
	return true
}
