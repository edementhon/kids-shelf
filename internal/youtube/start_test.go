package youtube

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edementhon/kids-shelf/internal/jobs"
)

// A yt-dlp that never runs (deleted by an antivirus, or refused by the
// operating system) must not look like a YouTube problem: the job fails
// with words naming the downloader, the reason reaches the log, a missing
// file is downloaded again, and no job is left stuck "running".

// blockYtDlp leaves the installed fake in place but makes it impossible to
// start (no execute permission), like a file an antivirus refuses to run.
func blockYtDlp(t *testing.T, dataDir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("execute permission does not stop a Windows program from starting")
	}
	if err := os.Chmod(readPointer(t, dataDir), 0o644); err != nil {
		t.Fatal(err)
	}
}

// captureLog records the manager's log lines.
func captureLog(m *Manager) func() string {
	var mu sync.Mutex
	var b strings.Builder
	m.WithLog(func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(&b, format+"\n", args...)
	})
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return b.String()
	}
}

func runOne(t *testing.T, m *Manager, q *jobs.Queue, jobID string) *jobs.Job {
	t.Helper()
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	job, err := q.Get(jobID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return job
}

func TestYtDlpThatCannotStartFailsWithItsOwnWords(t *testing.T) {
	m, fq, q, dataDir := newFakeEnv(t, "single")
	blockYtDlp(t, dataDir)
	logged := captureLog(m)
	jobID, err := m.Enqueue(context.Background(), "https://www.youtube.com/watch?v=abc123")
	if err != nil {
		t.Fatal(err)
	}
	job := runOne(t, m, q, jobID)
	if job.State != jobs.StateFailed || job.Error != blockedMessage(runtime.GOOS) {
		t.Fatalf("state %s, error %q; want failed with the blocked-downloader words", job.State, job.Error)
	}
	if !strings.Contains(logged(), "could not start") {
		t.Errorf("log = %q, want the start failure", logged())
	}
	if fq.Enqueued() != 0 {
		t.Errorf("ingest enqueues = %d, want 0", fq.Enqueued())
	}
}

// TestPlaylistVideoWithBlockedYtDlpFailsNotStuck: a video job made by a
// playlist expansion already has its id, so its first yt-dlp run is the
// size probe. A probe that cannot run must not leave the job "running"
// (only a restart would recover it); the job fails with the blocked words.
func TestPlaylistVideoWithBlockedYtDlpFailsNotStuck(t *testing.T) {
	m, _, q, dataDir := newFakeEnv(t, "single")
	blockYtDlp(t, dataDir)
	jobID, err := q.Enqueue(Kind, Payload{URL: watchURL("abc123"), VideoID: "abc123", Title: "Video", PlaylistTitle: "List"})
	if err != nil {
		t.Fatal(err)
	}
	job := runOne(t, m, q, jobID)
	if job.State != jobs.StateFailed || job.Error != blockedMessage(runtime.GOOS) {
		t.Fatalf("state %s, error %q; want failed with the blocked-downloader words", job.State, job.Error)
	}
}

// TestMissingYtDlpIsDownloadedAgain: the file the pointer names is gone
// (today's check already ran, and the release is not newer). The next run
// downloads it again and the link works.
func TestMissingYtDlpIsDownloadedAgain(t *testing.T) {
	m, fq, q, dataDir := newFakeEnv(t, "single")
	bin := readPointer(t, dataDir)
	if err := os.Remove(bin); err != nil {
		t.Fatal(err)
	}
	ts := releaseServer(t, "2026.09.01", nil, false, false) // the same version
	m.updater.BaseURL = ts.URL + "/releases/latest"
	jobID, err := m.Enqueue(context.Background(), "https://www.youtube.com/watch?v=abc123")
	if err != nil {
		t.Fatal(err)
	}
	job := runOne(t, m, q, jobID)
	if job.State != jobs.StateDone {
		t.Fatalf("state %s (error %q), want done after yt-dlp was downloaded again", job.State, job.Error)
	}
	if _, err := os.Stat(readPointer(t, dataDir)); err != nil {
		t.Errorf("yt-dlp still missing: %v", err)
	}
	if fq.Enqueued() != 1 {
		t.Errorf("ingest enqueues = %d, want 1", fq.Enqueued())
	}
}

// TestMissingYtDlpWithNoInternetSaysSo: the file is gone and cannot be
// fetched again; the job names the missing downloader, not YouTube.
func TestMissingYtDlpWithNoInternetSaysSo(t *testing.T) {
	m, _, q, dataDir := newFakeEnv(t, "single")
	if err := os.Remove(readPointer(t, dataDir)); err != nil {
		t.Fatal(err)
	}
	jobID, err := m.Enqueue(context.Background(), "https://www.youtube.com/watch?v=abc123")
	if err != nil {
		t.Fatal(err)
	}
	job := runOne(t, m, q, jobID)
	if job.State != jobs.StateFailed || job.Error != msgYtDlpMissing {
		t.Fatalf("state %s, error %q; want failed with the missing-downloader words", job.State, job.Error)
	}
}

// TestUpdaterRepairsMissingBinaryAtMostEvery30Minutes: a file that keeps
// disappearing (an antivirus deleting it) is fetched again, but not on
// every call: the worker loop calls EnsureLatest every second.
func TestUpdaterRepairsMissingBinaryAtMostEvery30Minutes(t *testing.T) {
	t.Setenv("KS_FAKE_YTDLP", "single")
	dataDir := t.TempDir()
	installFakeYtDlp(t, dataDir, "2026.09.01")
	markDailyCheck(t, dataDir)
	ts := releaseServer(t, "2026.09.01", nil, false, false)
	now := time.Now()
	u := NewUpdater(dataDir, ts.URL+"/releases/latest")
	u.Now = func() time.Time { return now }
	bin := readPointer(t, dataDir)

	present := func() bool { _, err := os.Stat(bin); return err == nil }
	ensure := func() {
		t.Helper()
		if _, err := u.EnsureLatest(context.Background(), false); err != nil {
			t.Fatalf("EnsureLatest: %v", err)
		}
	}
	os.Remove(bin)
	ensure()
	if !present() {
		t.Fatal("a missing yt-dlp was not downloaded again")
	}
	os.Remove(bin)
	now = now.Add(29 * time.Minute)
	ensure()
	if present() {
		t.Fatal("yt-dlp was downloaded again within 30 minutes of the last time")
	}
	now = now.Add(2 * time.Minute)
	ensure()
	if !present() {
		t.Fatal("yt-dlp was not downloaded again after 30 minutes")
	}
	if got := filepath.Base(filepath.Dir(bin)); got != "2026.09.01" {
		t.Errorf("reinstalled under %s, want the same version", got)
	}
}
