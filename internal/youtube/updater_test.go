package youtube

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// releaseServer serves a yt-dlp-like "latest release" for the current
// platform: JSON with the platform asset and SHA2-256SUMS, the binary body
// and the checksums file. assetBody nil serves this test binary, which
// passes the self-test as long as KS_FAKE_YTDLP names a mode. badHash flips
// the checksum; selfTestFail serves a body that runs but prints the wrong
// version.
func releaseServer(t *testing.T, tag string, assetBody []byte, badHash, selfTestFail bool) *httptest.Server {
	t.Helper()
	asset, err := assetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	if assetBody == nil {
		b, err := os.ReadFile(os.Args[0])
		if err != nil {
			t.Fatalf("read test binary: %v", err)
		}
		assetBody = b
	}
	if selfTestFail {
		assetBody = []byte("#!/bin/sh\necho wrong-version\n")
	}
	sum := sha256.Sum256(assetBody)
	digest := hex.EncodeToString(sum[:])
	if badHash {
		digest = strings.Repeat("0", sha256.Size*2)
	}
	sums := digest + "  " + asset + "\n" +
		strings.Repeat("1", sha256.Size*2) + " *yt-dlp\n" + // other entries are ignored
		strings.Repeat("2", sha256.Size*2) + "  yt-dlp_macos\n"
	var ts *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":%q,"assets":[`+
			`{"name":%q,"browser_download_url":%q},`+
			`{"name":"SHA2-256SUMS","browser_download_url":%q}]}`,
			tag, asset, ts.URL+"/download/"+asset, ts.URL+"/download/SHA2-256SUMS")
	})
	mux.HandleFunc("/download/"+asset, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(assetBody)
	})
	mux.HandleFunc("/download/SHA2-256SUMS", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, sums)
	})
	ts = httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// newCountServer is an httptest server that calls onHit per request and
// answers 404, for counting updater attempts that must fail.
func newCountServer(t *testing.T, onHit func()) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		onHit()
		http.NotFound(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// retiredPointer returns the "current" pointer content, "" when absent.
func retiredPointer(dataDir string) string {
	b, err := os.ReadFile(filepath.Join(dataDir, "helpers", "yt-dlp", "current"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func TestUpdaterDailyGate(t *testing.T) {
	t.Setenv("KS_FAKE_YTDLP", "single")
	dataDir := t.TempDir()
	installFakeYtDlp(t, dataDir, "2026.09.01")
	var mu sync.Mutex
	hits := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		http.NotFound(w, r)
	}))
	t.Cleanup(ts.Close)

	u := NewUpdater(dataDir, ts.URL+"/releases/latest")
	u.Now = func() time.Time { return time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC) }
	ctx := context.Background()
	dir := filepath.Join(dataDir, "helpers", "yt-dlp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// A check earlier today suppresses the network call entirely.
	if err := os.WriteFile(filepath.Join(dir, "lastcheck"), []byte("2026-10-26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ok, err := u.EnsureLatest(ctx, false)
	if err != nil {
		t.Fatalf("EnsureLatest: %v", err)
	}
	if ok {
		t.Error("EnsureLatest installed with no network, want no-op")
	}
	if hits != 0 {
		t.Errorf("server hits = %d, want 0 (checked today already)", hits)
	}

	// A stale stamp refetches (and installs, since the 404 check fails).
	if err := os.WriteFile(filepath.Join(dir, "lastcheck"), []byte("2026-10-25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := u.EnsureLatest(ctx, false); err == nil {
		t.Error("EnsureLatest succeeded against a 404 server, want error")
	}
	if hits != 1 {
		t.Errorf("server hits = %d, want 1", hits)
	}
	// The failed check is not stamped: the next run retries.
	if _, err := u.EnsureLatest(ctx, false); err == nil {
		t.Error("EnsureLatest succeeded against a 404 server, want error")
	}
	if hits != 2 {
		t.Errorf("server hits = %d, want 2 (failed checks are not skipped)", hits)
	}
}

func TestUpdaterInstallsNewVersion(t *testing.T) {
	t.Setenv("KS_FAKE_YTDLP", "single")
	dataDir := t.TempDir()
	installFakeYtDlp(t, dataDir, "2026.09.01")
	oldPointer := retiredPointer(dataDir)
	ts := releaseServer(t, "2026.10.21", nil, false, false)
	u := NewUpdater(dataDir, ts.URL+"/releases/latest")
	u.Now = func() time.Time { return time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC) }

	ok, err := u.EnsureLatest(context.Background(), true)
	if err != nil {
		t.Fatalf("EnsureLatest: %v", err)
	}
	if !ok {
		t.Fatal("EnsureLatest reported no install")
	}
	cur := retiredPointer(dataDir)
	if !strings.Contains(cur, "2026.10.21") {
		t.Errorf("pointer = %q, want the new version", cur)
	}
	if cur == oldPointer {
		t.Error("pointer unchanged")
	}
	for _, f := range []string{
		filepath.Join(dataDir, "helpers", "yt-dlp", "2026.10.21", assetFor(t)),
		filepath.Join(dataDir, "helpers", "yt-dlp", "2026.10.21", "SHA2-256SUMS"),
	} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("installed file missing: %v", err)
		}
	}
	// The previous version directory is kept.
	if _, err := os.Stat(filepath.Join(dataDir, "helpers", "yt-dlp", "2026.09.01")); err != nil {
		t.Errorf("previous version removed: %v", err)
	}
	// The daily stamp reflects the check date.
	b, err := os.ReadFile(filepath.Join(dataDir, "helpers", "yt-dlp", "lastcheck"))
	if err != nil {
		t.Fatalf("lastcheck: %v", err)
	}
	if strings.TrimSpace(string(b)) != "2026-10-26" {
		t.Errorf("lastcheck = %q, want 2026-10-26", strings.TrimSpace(string(b)))
	}
	// An already-current version is not reinstalled.
	ok, err = u.EnsureLatest(context.Background(), true)
	if err != nil {
		t.Fatalf("EnsureLatest: %v", err)
	}
	if ok {
		t.Error("EnsureLatest reinstalled the current version")
	}
}

func TestUpdaterCurrentSurvivesBadChecksum(t *testing.T) {
	t.Setenv("KS_FAKE_YTDLP", "single")
	dataDir := t.TempDir()
	installFakeYtDlp(t, dataDir, "2026.09.01")
	oldPointer := retiredPointer(dataDir)
	ts := releaseServer(t, "2026.10.21", nil, true, false)
	u := NewUpdater(dataDir, ts.URL+"/releases/latest")

	if _, err := u.EnsureLatest(context.Background(), true); err == nil {
		t.Fatal("EnsureLatest accepted a bad checksum")
	}
	if cur := retiredPointer(dataDir); cur != oldPointer {
		t.Errorf("pointer changed to %q on checksum failure", cur)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "helpers", "yt-dlp", "2026.09.01", assetFor(t))); err != nil {
		t.Errorf("working binary gone: %v", err)
	}
	// The failed install left no stray version binary behind.
	if _, err := os.Stat(filepath.Join(dataDir, "helpers", "yt-dlp", "2026.10.21", assetFor(t))); !os.IsNotExist(err) {
		t.Errorf("failed install left a version binary (err = %v)", err)
	}
}

func TestUpdaterCurrentSurvivesFailingSelfTest(t *testing.T) {
	t.Setenv("KS_FAKE_YTDLP", "single")
	dataDir := t.TempDir()
	installFakeYtDlp(t, dataDir, "2026.09.01")
	oldPointer := retiredPointer(dataDir)
	// The asset passes the checksum but its --version prints a wrong tag.
	ts := releaseServer(t, "2026.10.21", nil, false, true)
	u := NewUpdater(dataDir, ts.URL+"/releases/latest")

	_, err := u.EnsureLatest(context.Background(), true)
	if err == nil {
		t.Fatal("EnsureLatest accepted a build that fails its self-test")
	}
	if !strings.Contains(err.Error(), "self-test") {
		t.Errorf("error %q, want it to mention the self-test", err.Error())
	}
	if cur := retiredPointer(dataDir); cur != oldPointer {
		t.Errorf("pointer changed to %q on self-test failure", cur)
	}
	if _, err := os.Stat(oldPointer); err != nil {
		t.Errorf("working binary gone: %v", err)
	}
}

func TestUpdaterCurrentSurvivesNetworkError(t *testing.T) {
	t.Setenv("KS_FAKE_YTDLP", "single")
	dataDir := t.TempDir()
	installFakeYtDlp(t, dataDir, "2026.09.01")
	oldPointer := retiredPointer(dataDir)
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close() // connection refused from here on
	u := NewUpdater(dataDir, deadURL+"/releases/latest")

	if _, err := u.EnsureLatest(context.Background(), true); err == nil {
		t.Fatal("EnsureLatest succeeded against a dead server")
	}
	if cur := retiredPointer(dataDir); cur != oldPointer {
		t.Errorf("pointer changed to %q on network failure", cur)
	}
	if _, err := os.Stat(oldPointer); err != nil {
		t.Errorf("working binary gone: %v", err)
	}
}

func TestUpdaterAssetNames(t *testing.T) {
	cases := []struct {
		goos, goarch, want string
	}{
		{"windows", "amd64", "yt-dlp.exe"},
		{"darwin", "amd64", "yt-dlp_macos"},
		{"darwin", "arm64", "yt-dlp_macos"},
		{"linux", "amd64", "yt-dlp_linux"},
		{"linux", "arm64", "yt-dlp_linux_aarch64"},
		{"linux", "386", ""},
		{"windows", "arm64", ""},
	}
	for _, c := range cases {
		got, err := assetName(c.goos, c.goarch)
		if c.want == "" {
			if err == nil {
				t.Errorf("assetName(%s, %s) = %q, want error", c.goos, c.goarch, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("assetName(%s, %s) = %q, %v; want %q", c.goos, c.goarch, got, err, c.want)
		}
	}
}

func TestCompareTags(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2026.10.21", "2026.09.01", 1},
		{"2026.9.1", "2026.10.21", -1},
		{"2026.10.21", "2026.10.21", 0},
		{"v0.17.0", "v0.12.0", 1},
		{"2026.10.21", "beta", 1},
	}
	for _, c := range cases {
		if got := compareTags(c.a, c.b); got != c.want {
			t.Errorf("compareTags(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func assetFor(t *testing.T) string {
	t.Helper()
	a, err := assetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestUpdaterFailedInstallRetriedSameDay(t *testing.T) {
	t.Setenv("KS_FAKE_YTDLP", "single")
	dataDir := t.TempDir()
	installFakeYtDlp(t, dataDir, "2026.09.01")
	asset, err := assetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatalf("read test binary: %v", err)
	}
	sum := sha256.Sum256(body)
	sums := hex.EncodeToString(sum[:]) + "  " + asset + "\n"
	var fail atomic.Bool
	fail.Store(true)
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			fmt.Fprintf(w, `{"tag_name":"2026.10.21","assets":[`+
				`{"name":%q,"browser_download_url":%q},`+
				`{"name":"SHA2-256SUMS","browser_download_url":%q}]}`,
				asset, ts.URL+"/download/"+asset, ts.URL+"/download/SHA2-256SUMS")
		case "/download/" + asset:
			if fail.Swap(false) {
				http.Error(w, "network error", http.StatusInternalServerError)
				return
			}
			_, _ = w.Write(body)
		case "/download/SHA2-256SUMS":
			_, _ = io.WriteString(w, sums)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	u := NewUpdater(dataDir, ts.URL+"/releases/latest")
	u.Now = func() time.Time { return time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC) }
	ctx := context.Background()

	// The first install fails mid-download (network error).
	if _, err := u.EnsureLatest(ctx, false); err == nil {
		t.Fatal("EnsureLatest succeeded, want the first install to fail")
	}
	// A failed install must not close the once-a-day gate: the next call
	// the same day tries again.
	if b, err := os.ReadFile(filepath.Join(dataDir, "helpers", "yt-dlp", "lastcheck")); err == nil && strings.TrimSpace(string(b)) == "2026-10-26" {
		t.Fatal("failed install stamped the daily check")
	}
	ok, err := u.EnsureLatest(ctx, false)
	if err != nil {
		t.Fatalf("EnsureLatest retry: %v", err)
	}
	if !ok {
		t.Error("EnsureLatest retry installed nothing")
	}
	// The successful install stamps the check.
	if b, err := os.ReadFile(filepath.Join(dataDir, "helpers", "yt-dlp", "lastcheck")); err != nil {
		t.Fatalf("lastcheck: %v", err)
	} else if strings.TrimSpace(string(b)) != "2026-10-26" {
		t.Errorf("lastcheck = %q, want 2026-10-26", strings.TrimSpace(string(b)))
	}
	cur := retiredPointer(dataDir)
	if !strings.Contains(cur, "2026.10.21") {
		t.Errorf("pointer = %q, want the newly installed version", cur)
	}
}

func TestUpdaterGateBypassedWithoutBinary(t *testing.T) {
	t.Setenv("KS_FAKE_YTDLP", "single")
	dataDir := t.TempDir()
	// No yt-dlp installed yet: the "checked today" stamp must not suppress
	// the first install.
	if err := os.MkdirAll(filepath.Join(dataDir, "helpers", "yt-dlp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "helpers", "yt-dlp", "lastcheck"), []byte("2026-10-26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := releaseServer(t, "2026.10.21", nil, false, false)
	u := NewUpdater(dataDir, ts.URL+"/releases/latest")
	u.Now = func() time.Time { return time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC) }
	ok, err := u.EnsureLatest(context.Background(), false)
	if err != nil {
		t.Fatalf("EnsureLatest: %v", err)
	}
	if !ok {
		t.Error("EnsureLatest installed nothing despite having no binary")
	}
	if cur := retiredPointer(dataDir); !strings.Contains(cur, "2026.10.21") {
		t.Errorf("pointer = %q, want the installed version", cur)
	}
}

// TestUpdaterConcurrentChecksBothSucceed: on a first launch the app's daily
// worker and the YouTube queue both call EnsureLatest at once, with no
// binary installed yet. Both downloads used the same temp names, so one
// call renamed the other's file away and failed ("rename ...download: no
// such file or directory"). Calls are serialized: neither may fail, and
// the version is installed once.
func TestUpdaterConcurrentChecksBothSucceed(t *testing.T) {
	t.Setenv("KS_FAKE_YTDLP", "single")
	dataDir := t.TempDir()
	ts := releaseServer(t, "2026.10.21", nil, false, false)
	u := NewUpdater(dataDir, ts.URL+"/releases/latest")
	u.Now = func() time.Time { return time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC) }

	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)
	installed := make([]bool, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			installed[i], errs[i] = u.EnsureLatest(context.Background(), false)
		}()
	}
	wg.Wait()
	count := 0
	for i := range n {
		if errs[i] != nil {
			t.Errorf("call %d: %v", i, errs[i])
		}
		if installed[i] {
			count++
		}
	}
	if count != 1 {
		t.Errorf("%d calls installed, want exactly 1", count)
	}
	if cur := retiredPointer(dataDir); !strings.Contains(cur, "2026.10.21") {
		t.Errorf("pointer = %q, want the new version", cur)
	}
}
