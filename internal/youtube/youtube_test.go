package youtube

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edementhon/kids-shelf/internal/core"
	"github.com/edementhon/kids-shelf/internal/disk"
	"github.com/edementhon/kids-shelf/internal/jobs"
	"github.com/edementhon/kids-shelf/internal/store"
)

// newFakeEnv wires a Manager to a real jobs queue and store, a fake ingest
// queue, and a copy of this test binary acting as yt-dlp in the given mode
// (see TestMain). The daily update check is stamped as done so Run never
// touches the network, and the updater's base URL points at a dead port for
// the forced retry path.
func newFakeEnv(t *testing.T, mode string) (m *Manager, fq *core.FakeIngestQueue, q *jobs.Queue, dataDir string) {
	t.Helper()
	t.Setenv("KS_FAKE_YTDLP", mode)
	dataDir = t.TempDir()
	installFakeYtDlp(t, dataDir, "2026.09.01")
	markDailyCheck(t, dataDir)
	s, err := store.Open(filepath.Join(dataDir, "test.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	q, err = jobs.New(s)
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	fq = &core.FakeIngestQueue{}
	up := NewUpdater(dataDir, "http://127.0.0.1:1/releases/latest")
	m = NewManager(q, fq, &PointerHelpers{
		DataDir: dataDir, QuickJSPath: "qjs-fake", FFmpegLocation: "ffmpeg-fake",
	}, up, dataDir)
	// Downloads reserve their worst-case space before starting; most tests
	// do not exercise the reservation, so probe a comfortably large free
	// figure instead of the test machine's real disk.
	m.WithReservations(disk.NewReservations(q).WithFree(func(string) (uint64, error) { return 64 << 30, nil }))
	return m, fq, q, dataDir
}

// installFakeYtDlp copies this test binary into
// <data>/helpers/yt-dlp/<tag>/<asset> as the fake yt-dlp and points the
// "current" pointer at it.
func installFakeYtDlp(t *testing.T, dataDir, tag string) {
	t.Helper()
	asset, err := assetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(dataDir, "helpers", "yt-dlp", tag)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, asset)
	if err := os.WriteFile(bin, src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "helpers", "yt-dlp", "current"), []byte(bin+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// markDailyCheck stamps the yt-dlp check as done for today so Run's daily
// update check is a no-op.
func markDailyCheck(t *testing.T, dataDir string) {
	t.Helper()
	dir := filepath.Join(dataDir, "helpers", "yt-dlp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UTC().Format("2006-01-02")
	if err := os.WriteFile(filepath.Join(dir, "lastcheck"), []byte(stamp+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readPointer(t *testing.T, dataDir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dataDir, "helpers", "yt-dlp", "current"))
	if err != nil {
		t.Fatalf("read pointer: %v", err)
	}
	return strings.TrimSpace(string(b))
}

// cancelAt returns a ctx the manager's test hook cancels the moment the
// pipeline reaches phase ("downloaded" or "enqueued"), simulating a kill at
// exactly that point.
func cancelAt(ctx context.Context, m *Manager, phase string) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	m.hook = func(got string) {
		if got == phase {
			cancel()
		}
	}
	return ctx, cancel
}

// downloadAttempts counts the fake yt-dlp invocations that ran a download
// (the ones with an -o argument), across all binary copies, from the call
// log the fakes append to.
func downloadAttempts(t *testing.T, log string) int {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, " -o ") {
			n++
		}
	}
	return n
}

func payloadOf(t *testing.T, q *jobs.Queue, jobID string) Payload {
	t.Helper()
	job, err := q.Get(jobID)
	if err != nil {
		t.Fatalf("Get(%s): %v", jobID, err)
	}
	var p Payload
	if err := job.DecodePayload(&p); err != nil {
		t.Fatalf("DecodePayload(%s): %v", jobID, err)
	}
	return p
}

func TestSingleVideoDownload(t *testing.T) {
	m, fq, q, dataDir := newFakeEnv(t, "single")
	const url = "https://www.youtube.com/watch?v=abc123"
	jobID, err := m.Enqueue(context.Background(), url)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	job, err := q.Get(jobID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateDone {
		t.Fatalf("state = %q, want done (error %q)", job.State, job.Error)
	}
	if job.Stage != stageHandoff {
		t.Errorf("stage = %q, want %q", job.Stage, stageHandoff)
	}
	if got := fq.Enqueued(); got != 1 {
		t.Fatalf("ingest enqueues = %d, want 1", got)
	}
	call := fq.Calls()[0]
	wantSrc := core.Source{Kind: core.SourceYouTube, Path: url, Title: "Fake Title abc123", Show: "Fake Channel"}
	if call.Source != wantSrc {
		t.Errorf("source = %+v, want %+v", call.Source, wantSrc)
	}
	if base := filepath.Base(call.File); base != "abc123.mp4" {
		t.Errorf("file = %q, want abc123.mp4", call.File)
	}
	wantDir := filepath.Join(dataDir, "ytdlp-tmp", jobID)
	if filepath.Dir(call.File) != wantDir {
		t.Errorf("file %q not under per-job temp dir %q", call.File, wantDir)
	}
	p := payloadOf(t, q, jobID)
	if !p.Downloaded {
		t.Error("payload.Downloaded not set")
	}
	if p.IngestID == "" {
		t.Error("payload.IngestID not set")
	}
	if p.Progress != 100 {
		t.Errorf("payload.Progress = %v, want 100", p.Progress)
	}
}

func TestPlaylistExpandsIntoOneJobPerVideo(t *testing.T) {
	m, fq, q, _ := newFakeEnv(t, "playlist3")
	const url = "https://www.youtube.com/playlist?list=PLfake"
	if _, err := m.Enqueue(context.Background(), url); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := fq.Enqueued(); got != 3 {
		t.Fatalf("ingest enqueues = %d, want 3", got)
	}
	titles := map[string]string{}
	for _, c := range fq.Calls() {
		if c.Source.Show != "Fake Channel" {
			t.Errorf("show = %q, want Fake Channel", c.Source.Show)
		}
		titles[c.Source.Path] = c.Source.Title
	}
	want := map[string]string{
		"https://www.youtube.com/watch?v=vid-aaa": "Fake A",
		"https://www.youtube.com/watch?v=vid-bbb": "Fake B",
		"https://www.youtube.com/watch?v=vid-ccc": "Fake C",
	}
	for path, title := range want {
		if titles[path] != title {
			t.Errorf("title for %s = %q, want %q", path, titles[path], title)
		}
	}
	all, err := q.List(Kind)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("jobs = %d, want 4 (1 playlist + 3 videos)", len(all))
	}
	// Children carry the resolved identity, including the playlist title.
	children := 0
	for i := range all {
		var p Payload
		if err := all[i].DecodePayload(&p); err != nil {
			continue
		}
		if p.VideoID == "" {
			continue
		}
		children++
		if !strings.HasPrefix(p.URL, "https://www.youtube.com/watch?v=") {
			t.Errorf("child URL = %q, want watch URL", p.URL)
		}
		if p.PlaylistTitle != "Fake Playlist" {
			t.Errorf("child playlist title = %q, want Fake Playlist", p.PlaylistTitle)
		}
		if p.IngestID == "" {
			t.Errorf("child %s handed off without an ingest id", p.VideoID)
		}
	}
	if children != 3 {
		t.Errorf("child jobs = %d, want 3", children)
	}
}

func TestPlaylistDeduplicatesByVideoID(t *testing.T) {
	m, fq, q, _ := newFakeEnv(t, "playlist3")
	const url = "https://www.youtube.com/playlist?list=PLfake"
	if _, err := m.Enqueue(context.Background(), url); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := fq.Enqueued(); got != 3 {
		t.Fatalf("ingest enqueues after first add = %d, want 3", got)
	}
	// The same playlist added again must not spawn new children: every
	// video id already exists across jobs.
	if _, err := m.Enqueue(context.Background(), url); err != nil {
		t.Fatalf("Enqueue second time: %v", err)
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := fq.Enqueued(); got != 3 {
		t.Fatalf("ingest enqueues after duplicate add = %d, want still 3", got)
	}
	all, err := q.List(Kind)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 5 {
		t.Errorf("jobs = %d, want 5 (2 parents + 3 videos)", len(all))
	}
}

func TestDownloadFailureRunsUpdaterThenRetries(t *testing.T) {
	const oldTag = "2026.09.01"
	const newTag = "2026.10.21"
	t.Setenv("KS_FAKE_YTDLP_NEWTAG", newTag)
	m, fq, q, dataDir := newFakeEnv(t, "update")
	ts := releaseServer(t, newTag, nil, false, false)
	m.updater.BaseURL = ts.URL + "/releases/latest"
	log := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("KS_FAKE_YTDLP_LOG", log)

	jobID, err := m.Enqueue(context.Background(), "https://www.youtube.com/watch?v=xyz789")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	job, err := q.Get(jobID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateDone {
		t.Fatalf("state = %q, want done (error %q)", job.State, job.Error)
	}
	if got := fq.Enqueued(); got != 1 {
		t.Fatalf("ingest enqueues = %d, want 1", got)
	}
	// Exactly two download attempts: once with the old binary, once with
	// the newly installed one.
	attempts := downloadAttempts(t, log)
	if attempts != 2 {
		t.Fatalf("download attempts = %d, want 2", attempts)
	}
	lines := strings.Split(readLog(t, log), "\n")
	var first, second string
	for _, l := range lines {
		if strings.Contains(l, " -o ") {
			if first == "" {
				first = l
			} else {
				second = l
			}
		}
	}
	if !strings.Contains(first, oldTag) || strings.Contains(first, newTag) {
		t.Errorf("first attempt ran %q, want the %s binary", first, oldTag)
	}
	if !strings.Contains(second, newTag) {
		t.Errorf("retry ran %q, want the %s binary", second, newTag)
	}
	// The pointer now names the new version, and the old one is kept.
	cur := readPointer(t, dataDir)
	if !strings.Contains(cur, newTag) {
		t.Errorf("pointer = %q, want it to name %s", cur, newTag)
	}
	oldDir := filepath.Join(dataDir, "helpers", "yt-dlp", oldTag)
	if _, err := os.Stat(oldDir); err != nil {
		t.Errorf("previous version dir removed: %v", err)
	}
}

func TestPermanentDownloadFailureFailsPlainly(t *testing.T) {
	m, fq, q, _ := newFakeEnv(t, "fail")
	log := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("KS_FAKE_YTDLP_LOG", log)
	// The forced update after the first failure hits this 404 server and
	// fails; the retry still runs with the unchanged binary.
	var hits atomic.Int32
	ts := newCountServer(t, func() { hits.Add(1) })
	m.updater.BaseURL = ts.URL + "/releases/latest"

	jobID, err := m.Enqueue(context.Background(), "https://www.youtube.com/watch?v=abc123")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	job, err := q.Get(jobID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateFailed {
		t.Fatalf("state = %q, want failed", job.State)
	}
	if !strings.Contains(job.Error, "couldn't download") || !strings.Contains(job.Error, "twice") {
		t.Errorf("error = %q, want a plain twice-tried message", job.Error)
	}
	for _, jargon := range []string{"ERROR:", "yt-dlp", "exit status"} {
		if strings.Contains(job.Error, jargon) {
			t.Errorf("error %q exposes %q to parents", job.Error, jargon)
		}
	}
	if got := fq.Enqueued(); got != 0 {
		t.Errorf("ingest enqueues = %d, want 0", got)
	}
	if attempts := downloadAttempts(t, log); attempts != 2 {
		t.Errorf("download attempts = %d, want 2", attempts)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("forced update checks = %d, want 1", got)
	}
}

func TestResolveFailureFailsPlainly(t *testing.T) {
	m, fq, q, _ := newFakeEnv(t, "badresolve")
	jobID, err := m.Enqueue(context.Background(), "https://www.youtube.com/watch?v=abc123")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	job, err := q.Get(jobID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateFailed {
		t.Fatalf("state = %q, want failed", job.State)
	}
	if !strings.Contains(job.Error, "doesn't look like a YouTube video link") {
		t.Errorf("error = %q, want the plain link message", job.Error)
	}
	if got := fq.Enqueued(); got != 0 {
		t.Errorf("ingest enqueues = %d, want 0", got)
	}
}

// TestResolvePassesJSRuntime is the regression test for the fresh-install
// failure "We couldn't read that link from YouTube": the resolve command
// lacked the --js-runtimes flag the download already used, and current
// yt-dlp refuses to extract YouTube without a JavaScript runtime, reporting
// a misleading "video is not available". The fake fails any resolve run that
// omits the flag; a resolved video must still reach ingest.
func TestResolvePassesJSRuntime(t *testing.T) {
	m, fq, q, _ := newFakeEnv(t, "nojsresolve")
	jobID, err := m.Enqueue(context.Background(), "https://www.youtube.com/watch?v=abc123")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	job, err := q.Get(jobID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateDone {
		t.Fatalf("state = %q, want done (error %q)", job.State, job.Error)
	}
	if got := fq.Enqueued(); got != 1 {
		t.Fatalf("ingest enqueues = %d, want 1", got)
	}
}

// TestFailureMessagesNameTheCause checks the parent-facing texts against each
// cause yt-dlp reports: a bot block or a network failure must never blame
// the link, and a real block must be named plainly.
func TestFailureMessagesNameTheCause(t *testing.T) {
	resolveCases := []struct {
		name string
		err  error
		want string
	}{
		{"bot", errors.New("ERROR: [youtube] XqZsoesa55w: Sign in to confirm you're not a bot. Use --cookies-from-browser chrome"), msgResolveBot},
		{"bot datacenter", errors.New("ERROR: [youtube] XqZsoesa55w: The link between YouTube and this computer is blocked. It may be because YouTube blocked your IP address"), msgResolveBot},
		{"network dns", errors.New("ERROR: Unable to download webpage: <urlopen error [Errno -2] Name or service not known>"), msgResolveNetwork},
		{"network timeout", errors.New("ERROR: [youtube] XqZsoesa55w: Unable to download API page: <urlopen error timed out>"), msgResolveNetwork},
		{"network refused", errors.New("ERROR: unable to connect to youtube.com:443: connection refused"), msgResolveNetwork},
		{"unavailable", errors.New("ERROR: [youtube] XqZsoesa55w: This video is unavailable"), msgResolveUnavailable},
		{"unavailable private or removed", errors.New("ERROR: [youtube] XqZsoesa55w: This video is unavailable. This video may be private or removed."), msgResolveUnavailable},
		{"removed", errors.New("ERROR: [youtube] XqZsoesa55w: Video unavailable. This video has been removed by the uploader"), msgResolveUnavailable},
		{"geo", errors.New("ERROR: [youtube] XqZsoesa55w: This video is not available in your country"), msgResolveUnavailable},
		{"link", errors.New("ERROR: [youtube] Unsupported URL: https://example.com/nope"), msgResolveLink},
		{"unknown", errors.New("ERROR: [youtube] XqZsoesa55w: something else went wrong"), msgResolveFallback},
	}
	for _, tc := range resolveCases {
		t.Run("resolve "+tc.name, func(t *testing.T) {
			if got := resolveFailureMessage(tc.err); got != tc.want {
				t.Errorf("resolveFailureMessage(%q) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}

	// The link-blame wording never appears when YouTube or the network is
	// the cause, in either the resolve or the download text.
	for _, cause := range []error{
		errors.New("ERROR: [youtube] abc: Sign in to confirm you're not a bot"),
		errors.New("ERROR: Unable to download webpage: <urlopen error timed out>"),
		errors.New("ERROR: [youtube] abc: This video is unavailable. This video may be private or removed."),
	} {
		for _, msg := range []string{resolveFailureMessage(cause), downloadMessage(Payload{Title: "Some Video"}, cause)} {
			if strings.Contains(msg, "may be wrong") || strings.Contains(msg, "address") {
				t.Errorf("message %q blames the link for cause %q", msg, cause)
			}
		}
	}

	bot := errors.New("ERROR: [youtube] abc: Sign in to confirm you're not a bot")
	if got := downloadMessage(Payload{Title: "Baby Shark"}, bot); !strings.Contains(got, "robot") || !strings.Contains(got, "Baby Shark") {
		t.Errorf("downloadMessage for a bot block = %q, want the robot-block text naming the video", got)
	}
	if got := downloadMessage(Payload{Title: "Baby Shark"}, errors.New("ERROR: unable to download video data: HTTP Error 403: Forbidden")); !strings.Contains(got, "twice") {
		t.Errorf("downloadMessage fallback = %q, want the plain twice-tried text", got)
	}
}

func TestKillAfterDownloadResumesWithoutRedownload(t *testing.T) {
	m, fq, q, _ := newFakeEnv(t, "single")
	log := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("KS_FAKE_YTDLP_LOG", log)
	jobID, err := m.Enqueue(context.Background(), "https://www.youtube.com/watch?v=abc123")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	ctx, cancel := cancelAt(context.Background(), m, "downloaded")
	err = m.Run(ctx)
	cancel()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	job, err := q.Get(jobID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateRunning {
		t.Fatalf("state after kill = %q, want running", job.State)
	}
	if got := fq.Enqueued(); got != 0 {
		t.Fatalf("enqueues after kill = %d, want 0", got)
	}
	if attempts := downloadAttempts(t, log); attempts != 1 {
		t.Fatalf("download attempts after kill = %d, want 1", attempts)
	}
	// Relaunch: Recover requeues the running job; the finished download
	// is reused and the ingest enqueue happens exactly once.
	if n, err := q.Recover(); err != nil || n != 1 {
		t.Fatalf("Recover = %d, %v; want 1", n, err)
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run after Recover: %v", err)
	}
	job, err = q.Get(jobID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateDone {
		t.Fatalf("state = %q, want done (error %q)", job.State, job.Error)
	}
	if got := fq.Enqueued(); got != 1 {
		t.Fatalf("enqueues = %d, want exactly 1", got)
	}
	if attempts := downloadAttempts(t, log); attempts != 1 {
		t.Errorf("download attempts = %d, want 1 (no re-download)", attempts)
	}
}

func TestKillAfterEnqueueDoesNotEnqueueTwice(t *testing.T) {
	m, fq, q, _ := newFakeEnv(t, "single")
	jobID, err := m.Enqueue(context.Background(), "https://www.youtube.com/watch?v=abc123")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	ctx, cancel := cancelAt(context.Background(), m, "enqueued")
	err = m.Run(ctx)
	cancel()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	job, err := q.Get(jobID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateRunning {
		t.Fatalf("state after kill = %q, want running", job.State)
	}
	if got := fq.Enqueued(); got != 1 {
		t.Fatalf("enqueues after kill = %d, want 1", got)
	}
	// The ingest job id was persisted before the kill point.
	if p := payloadOf(t, q, jobID); p.IngestID == "" {
		t.Fatal("IngestID not persisted before the kill point")
	}
	// Relaunch: the handoff is skipped entirely, so the second enqueue
	// never happens.
	if n, err := q.Recover(); err != nil || n != 1 {
		t.Fatalf("Recover = %d, %v; want 1", n, err)
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run after Recover: %v", err)
	}
	job, err = q.Get(jobID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateDone {
		t.Fatalf("state = %q, want done (error %q)", job.State, job.Error)
	}
	if got := fq.Enqueued(); got != 1 {
		t.Fatalf("enqueues = %d, want exactly 1", got)
	}
}

func TestTempDirsCleanUpOnceIngestFinishes(t *testing.T) {
	m, fq, q, dataDir := newFakeEnv(t, "single")
	jobID, err := m.Enqueue(context.Background(), "https://www.youtube.com/watch?v=abc123")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	dir := filepath.Join(dataDir, "ytdlp-tmp", jobID)
	if _, err := os.Stat(filepath.Join(dir, "abc123.mp4")); err != nil {
		t.Fatalf("downloaded file missing: %v", err)
	}
	// Ingest has not finished, so a second Run keeps the temp dir.
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("temp dir removed while ingest still runs: %v", err)
	}
	// Once ingest finishes with the handed-over file, the next Run sweeps.
	p := payloadOf(t, q, jobID)
	if err := fq.Complete(p.IngestID, "ep-1"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("temp dir still present after ingest finished (err = %v)", err)
	}
}

func TestFailedJobTempDirIsSwept(t *testing.T) {
	m, fq, q, dataDir := newFakeEnv(t, "fail")
	jobID, err := m.Enqueue(context.Background(), "https://www.youtube.com/watch?v=abc123")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	job, err := q.Get(jobID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateFailed {
		t.Fatalf("state = %q, want failed", job.State)
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	dir := filepath.Join(dataDir, "ytdlp-tmp", jobID)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("failed job's temp dir still present (err = %v)", err)
	}
	if got := fq.Enqueued(); got != 0 {
		t.Errorf("ingest enqueues = %d, want 0", got)
	}
}

func TestEnqueueRejectsUnusableAddresses(t *testing.T) {
	m, _, _, _ := newFakeEnv(t, "single")
	for _, bad := range []string{"not a url", "ftp://example.com/v", "youtube.com/watch?v=abc"} {
		if _, err := m.Enqueue(context.Background(), bad); err == nil {
			t.Errorf("Enqueue(%q) accepted, want error", bad)
		}
	}
}

func TestKillBetweenEnqueueAndPersistReusesIngestJob(t *testing.T) {
	m, fq, q, _ := newFakeEnv(t, "single")
	jobID, err := m.Enqueue(context.Background(), "https://www.youtube.com/watch?v=abc123")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	ctx, cancel := cancelAt(context.Background(), m, "ingest-enqueued")
	err = m.Run(ctx)
	cancel()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	job, err := q.Get(jobID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateRunning {
		t.Fatalf("state after kill = %q, want running", job.State)
	}
	if got := fq.Enqueued(); got != 1 {
		t.Fatalf("enqueues after kill = %d, want 1", got)
	}
	// The kill landed after Enqueue returned but before the ingest job id
	// was persisted: the payload has no id, so the resumed job re-runs the
	// handoff.
	if p := payloadOf(t, q, jobID); p.IngestID != "" {
		t.Fatal("IngestID persisted before the kill point, want it lost")
	}
	// Relaunch: the handoff enqueues the same source again. The ingest
	// queue is idempotent per (kind, path) - the canonical watch URL is the
	// durable key - so it returns the id of the first enqueue instead of
	// creating a second job. The fake numbers ids job-1, job-2, ... in
	// call order, so job-1 is the id the crash-run's enqueue received.
	if n, err := q.Recover(); err != nil || n != 1 {
		t.Fatalf("Recover = %d, %v; want 1", n, err)
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run after Recover: %v", err)
	}
	job, err = q.Get(jobID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateDone {
		t.Fatalf("state = %q, want done (error %q)", job.State, job.Error)
	}
	if got := fq.Enqueued(); got != 2 {
		t.Fatalf("enqueues = %d, want 2 (both handoffs recorded)", got)
	}
	if p := payloadOf(t, q, jobID); p.IngestID != "job-1" {
		t.Errorf("IngestID = %q, want %q: the resumed handoff must get the first enqueue's id", p.IngestID, "job-1")
	}
	calls := fq.Calls()
	if calls[0].Source != calls[1].Source || calls[0].File != calls[1].File {
		t.Errorf("resumed handoff enqueued a different source: %+v vs %+v", calls[0], calls[1])
	}
}

func TestChannelExpandsNestedTabs(t *testing.T) {
	m, fq, q, _ := newFakeEnv(t, "channel")
	const url = "https://www.youtube.com/@fakelab"
	if _, err := m.Enqueue(context.Background(), url); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Two tabs of two videos each: exactly four videos downloaded and
	// handed to ingest.
	if got := fq.Enqueued(); got != 4 {
		t.Fatalf("ingest enqueues = %d, want 4", got)
	}
	want := map[string]int{
		"https://www.youtube.com/watch?v=chan-1a": 0,
		"https://www.youtube.com/watch?v=chan-1b": 0,
		"https://www.youtube.com/watch?v=chan-2a": 0,
		"https://www.youtube.com/watch?v=chan-2b": 0,
	}
	for _, c := range fq.Calls() {
		if _, ok := want[c.Source.Path]; !ok {
			t.Errorf("ingested path %q is not one of the channel's videos", c.Source.Path)
			continue
		}
		want[c.Source.Path]++
		if c.Source.Show != "Fake Channel" {
			t.Errorf("show for %s = %q, want Fake Channel", c.Source.Path, c.Source.Show)
		}
	}
	for path, n := range want {
		if n != 1 {
			t.Errorf("video %s ingested %d times, want 1", path, n)
		}
	}
	// Every child job is built from a video id, never from the channel id
	// or a tab id.
	all, err := q.List(Kind)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("jobs = %d, want 5 (1 channel parent + 4 videos)", len(all))
	}
	children := 0
	for i := range all {
		var p Payload
		if err := all[i].DecodePayload(&p); err != nil {
			continue
		}
		if p.VideoID == "" {
			continue
		}
		children++
		if !strings.HasPrefix(p.URL, "https://www.youtube.com/watch?v=") {
			t.Errorf("child URL = %q, want watch URL", p.URL)
		}
		if _, ok := want[p.URL]; !ok {
			t.Errorf("child job %s built from a non-video id (%q)", all[i].ID, p.VideoID)
		}
	}
	if children != 4 {
		t.Errorf("child jobs = %d, want 4", children)
	}
}

func TestReservationDeniedPausesBeforeDownload(t *testing.T) {
	m, fq, q, _ := newFakeEnv(t, "single")
	// Zero free bytes: the reservation must be denied before any download.
	m.WithReservations(disk.NewReservations(q).WithFree(func(string) (uint64, error) { return 0, nil }).WithFloor(0))
	log := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("KS_FAKE_YTDLP_LOG", log)
	if err := os.WriteFile(log, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	jobID, err := m.Enqueue(context.Background(), "https://www.youtube.com/watch?v=abc123")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	job, err := q.Get(jobID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StatePaused {
		t.Fatalf("state = %q, want paused", job.State)
	}
	if !strings.Contains(job.Error, "disk is full") {
		t.Errorf("pause reason = %q, want the disk-full message", job.Error)
	}
	if got := fq.Enqueued(); got != 0 {
		t.Errorf("ingest enqueues = %d, want 0", got)
	}
	if attempts := downloadAttempts(t, log); attempts != 0 {
		t.Errorf("download attempts = %d, want 0 (denied before download)", attempts)
	}
}

func TestDownloadDiskFullPausesWithoutUpdater(t *testing.T) {
	m, fq, q, dataDir := newFakeEnv(t, "nospace")
	log := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("KS_FAKE_YTDLP_LOG", log)
	var hits atomic.Int32
	ts := newCountServer(t, func() { hits.Add(1) })
	m.updater.BaseURL = ts.URL + "/releases/latest"
	jobID, err := m.Enqueue(context.Background(), "https://www.youtube.com/watch?v=abc123")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	job, err := q.Get(jobID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StatePaused {
		t.Fatalf("state = %q, want paused", job.State)
	}
	if !strings.Contains(job.Error, "disk is full") {
		t.Errorf("pause reason = %q, want the disk-full message", job.Error)
	}
	if got := fq.Enqueued(); got != 0 {
		t.Errorf("ingest enqueues = %d, want 0", got)
	}
	// A full disk is not a yt-dlp problem: no forced update, no retry.
	if got := hits.Load(); got != 0 {
		t.Errorf("updater checks = %d, want 0", got)
	}
	if attempts := downloadAttempts(t, log); attempts != 1 {
		t.Errorf("download attempts = %d, want 1 (no retry)", attempts)
	}
	// The temp dir stays so a resume can continue once space is freed.
	if _, err := os.Stat(filepath.Join(dataDir, "ytdlp-tmp", jobID)); err != nil {
		t.Errorf("temp dir removed after disk-full pause: %v", err)
	}
}
func TestPausedDownloadResumesPartialFile(t *testing.T) {
	// Run 1: downloads fail with a disk-full error after writing a
	// partial .part file; the job pauses and the temp dir keeps the
	// partial file. Run 2 (space freed, mode "single"): the resumed job
	// must find the temp dir and its partial file intact and continue
	// the download from it, not wipe it and start from zero.
	m, fq, q, dataDir := newFakeEnv(t, "nospace-part")
	log := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("KS_FAKE_YTDLP_LOG", log)
	jobID, err := m.Enqueue(context.Background(), "https://www.youtube.com/watch?v=abc123")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run after pause: %v", err)
	}
	job, err := q.Get(jobID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StatePaused {
		t.Fatalf("state after nospace = %q, want paused (error %q)", job.State, job.Error)
	}
	if got := fq.Enqueued(); got != 0 {
		t.Fatalf("ingest enqueues after pause = %d, want 0", got)
	}
	if attempts := downloadAttempts(t, log); attempts != 1 {
		t.Fatalf("download attempts after pause = %d, want 1", attempts)
	}
	// The paused job's temp dir and its partial file survive the pause.
	tmpDir := filepath.Join(dataDir, "ytdlp-tmp", jobID)
	if _, err := os.Stat(filepath.Join(tmpDir, "abc123.mp4.part")); err != nil {
		t.Fatalf("partial file missing after disk-full pause: %v", err)
	}
	// The parent freed disk space; the paused job is put back on the
	// queue (Advance is how a paused job resumes; Recover requeues the
	// running job for the next pass) with the disk working again (the
	// mode switch makes the fake the working yt-dlp). The temp dir and
	// the partial file must still be there when the second download
	// starts.
	t.Setenv("KS_FAKE_YTDLP", "single")
	if err := q.Advance(jobID, stageDownload, payloadOf(t, q, jobID)); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if n, err := q.Recover(); err != nil || n != 1 {
		t.Fatalf("Recover = %d, %v; want 1", n, err)
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run after resume: %v", err)
	}
	job, err = q.Get(jobID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateDone {
		t.Fatalf("state after resume = %q, want done (error %q)", job.State, job.Error)
	}
	if got := fq.Enqueued(); got != 1 {
		t.Fatalf("ingest enqueues = %d, want 1", got)
	}
	if len(fq.Calls()) != 1 || filepath.Base(fq.Calls()[0].File) != "abc123.mp4" {
		t.Errorf("ingested file = %v, want abc123.mp4", fq.Calls())
	}
	// The resumed download continued the .part file: exactly two
	// download invocations in total, and the second one ran with
	// --continue (and without --no-part).
	if attempts := downloadAttempts(t, log); attempts != 2 {
		t.Errorf("download attempts = %d, want 2 (first attempt + resume)", attempts)
	}
	lines := strings.Split(readLog(t, log), "\n")
	var second string
	for _, l := range lines {
		if strings.Contains(l, " -o ") {
			second = l
		}
	}
	if !strings.Contains(second, "--continue") {
		t.Errorf("resume invocation %q missing --continue", second)
	}
	if strings.Contains(second, "--no-part") {
		t.Errorf("resume invocation %q kept --no-part, which would discard the partial file", second)
	}
}

func TestDownloadGrowthBeyondReservationPauses(t *testing.T) {
	// The video's size is unknown (the fake's size probe says so), so the
	// job reserves the 2 GiB default. The download then grows past it:
	// the manager tries to extend the reservation into 2 GiB of free
	// space - which is exactly the default, leaving nothing to extend
	// into - so yt-dlp is stopped and the job pauses with the disk-full
	// message, keeping the partial file for the resume.
	m, fq, q, dataDir := newFakeEnv(t, "nospace-part")
	free := int64(2 << 30)
	m.WithReservations(disk.NewReservations(q).WithFree(func(string) (uint64, error) {
		return uint64(free), nil
	}).WithFloor(0))
	log := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("KS_FAKE_YTDLP_LOG", log)
	jobID, err := m.Enqueue(context.Background(), "https://www.youtube.com/watch?v=abc123")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	job, err := q.Get(jobID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StatePaused {
		t.Fatalf("state = %q, want paused after the reservation ran out (error %q)", job.State, job.Error)
	}
	if !strings.Contains(job.Error, "disk is full") {
		t.Errorf("pause reason = %q, want the disk-full message", job.Error)
	}
	if got := fq.Enqueued(); got != 0 {
		t.Errorf("ingest enqueues = %d, want 0", got)
	}
	if attempts := downloadAttempts(t, log); attempts != 1 {
		t.Errorf("download attempts = %d, want 1 (stopped mid-download, no retry)", attempts)
	}
	// The reservation and the partial download survive for the resume.
	tmpDir := filepath.Join(dataDir, "ytdlp-tmp", jobID)
	if _, err := os.Stat(filepath.Join(tmpDir, "abc123.mp4.part")); err != nil {
		t.Errorf("partial file missing after paused download: %v", err)
	}
	reserved := payloadBytes(t, q, jobID)
	if reserved != defaultReserveBytes {
		t.Errorf("reservation after pause = %d, want the 2 GiB default %d", reserved, defaultReserveBytes)
	}
	// Freeing the disk (a bigger probe) lets the very same job resume
	// and finish from its partial file.
	free = 2 << 32 // 8 GiB: enough for the extended reservation
	if err := q.Advance(jobID, stageDownload, payloadOf(t, q, jobID)); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if n, err := q.Recover(); err != nil || n != 1 {
		t.Fatalf("Recover = %d, %v; want 1", n, err)
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run after freeing disk: %v", err)
	}
	job, err = q.Get(jobID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateDone {
		t.Fatalf("state after resume = %q, want done (error %q)", job.State, job.Error)
	}
	if attempts := downloadAttempts(t, log); attempts != 2 {
		t.Errorf("download attempts = %d, want 2 (paused run + resumed run)", attempts)
	}
	if got := fq.Enqueued(); got != 1 {
		t.Errorf("ingest enqueues = %d, want 1", got)
	}
}

// payloadBytes reads the reserved_bytes column the jobs table stores for a
// job, the number the reservations manager accounted against free space.
func payloadBytes(t *testing.T, q *jobs.Queue, jobID string) uint64 {
	t.Helper()
	job, err := q.Get(jobID)
	if err != nil {
		t.Fatalf("Get(%s): %v", jobID, err)
	}
	return uint64(job.ReservedBytes)
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	return string(b)
}
func TestEnqueueRejectsNonYouTubeLinks(t *testing.T) {
	m, _, q, _ := newFakeEnv(t, "single")
	for _, link := range []string{
		"http://127.0.0.1/x",                         // arbitrary host, http
		"http://www.youtube.com/watch?v=abc123",      // right host, wrong scheme
		"https://evil.example/watch?v=x",             // arbitrary host, https
		"https://youtube.com.evil.example/watch?v=x", // hostname trick
		"https://youtu.be@evil.example/watch?v=x",    // userinfo trick
		"ftp://youtube.com/watch?v=x",                // wrong scheme entirely
		"not a url",
	} {
		if _, err := m.Enqueue(context.Background(), link); err == nil {
			t.Errorf("Enqueue(%q) succeeded, want a refusal", link)
		}
	}
	if n, err := lenAll(q); err != nil || n != 0 {
		t.Errorf("jobs after refused enqueues = %d, %v; want 0", n, err)
	}
}

func TestEnqueueAcceptsYouTubeHosts(t *testing.T) {
	m, _, _, _ := newFakeEnv(t, "single")
	for _, link := range []string{
		"https://youtube.com/watch?v=abc123",
		"https://www.youtube.com/watch?v=abc123",
		"https://m.youtube.com/watch?v=abc123",
		"https://music.youtube.com/watch?v=abc123",
		"https://youtu.be/abc123",
	} {
		if _, err := m.Enqueue(context.Background(), link); err != nil {
			t.Errorf("Enqueue(%q): %v", link, err)
		}
	}
}

func lenAll(q *jobs.Queue) (int, error) {
	all, err := q.List(Kind)
	if err != nil {
		return 0, err
	}
	return len(all), nil
}
