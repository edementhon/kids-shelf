package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/edementhon/kids-shelf/internal/core"
	"github.com/edementhon/kids-shelf/internal/disk"
	"github.com/edementhon/kids-shelf/internal/jobs"
	"github.com/edementhon/kids-shelf/internal/store"
	"github.com/edementhon/kids-shelf/internal/tools"
	"github.com/edementhon/kids-shelf/internal/update"
)

// testConfig points every directory at temp dirs, so Open never touches the
// real per-user data or media locations and Kiwix creates its library under
// the temp media root.
func testConfig(t *testing.T, dataDir string) Config {
	t.Helper()
	return Config{
		DataDir:  dataDir,
		MediaDir: filepath.Join(t.TempDir(), "media"),
		Version:  "test",
	}
}

// openTest opens the app and makes sure it is closed again.
func openTest(t *testing.T, dataDir string) *App {
	t.Helper()
	a, err := Open(context.Background(), testConfig(t, dataDir))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return a
}

// seedYtDlpInstalled writes the once-a-day gate state of an installed
// yt-dlp (internal/youtube/updater.go: relHelperDir, pointerName and
// lastCheckName), so EnsureLatest returns before it fetches the release
// endpoint: an "installed and checked today" install never touches the
// network, which keeps the Start/Close test deterministic.
func seedYtDlpInstalled(t *testing.T, dataDir string) {
	t.Helper()
	dir := filepath.Join(dataDir, "helpers", "yt-dlp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", dir, err)
	}
	today := time.Now().UTC().Format("2006-01-02")
	files := map[string]string{
		"current":   filepath.Join(dir, "v1", "yt-dlp"), // path need not exist
		"lastcheck": today,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", name, err)
		}
	}
}

// TestOpenTwiceIdempotent opens the same data dir twice: every package's
// migrations record their (owner, version) pairs in schema_migrations, so
// the second Open applies nothing and must not fail.
func TestOpenTwiceIdempotent(t *testing.T) {
	dataDir := t.TempDir()

	first, err := Open(context.Background(), testConfig(t, dataDir))
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	dbPath := filepath.Join(dataDir, "kids-shelf.db")
	if st, err := os.Stat(dbPath); err != nil || st.Size() == 0 {
		t.Fatalf("database %s: %v", dbPath, err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	second, err := Open(context.Background(), testConfig(t, dataDir))
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	// A clean close never leaves a running job, so Recover after the
	// second Open must find nothing to requeue.
	n, err := second.queue.Recover()
	if err != nil {
		t.Fatalf("Recover after second Open: %v", err)
	}
	if n != 0 {
		t.Fatalf("Recover after clean close requeued %d jobs, want 0", n)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestDepsFilled checks that Deps carries every running service and every
// directory, and leaves Updates and Quit to cmd/kids-shelf.
func TestDepsFilled(t *testing.T) {
	a := openTest(t, t.TempDir())
	d := a.Deps()
	if d.Budget == nil || d.Library == nil || d.Ingest == nil ||
		d.YouTube == nil || d.Catalog == nil || d.Kiwix == nil || d.Practice == nil {
		t.Fatalf("Deps missing a service: %+v", d)
	}
	if d.MediaDir == "" || d.ClipsDir == "" || d.DataDir == "" {
		t.Fatalf("Deps missing a directory: %+v", d)
	}
	if d.Now == nil {
		t.Fatal("Deps.Now is nil")
	}
	if d.InTrial == nil {
		t.Fatal("Deps.InTrial is nil; the web server refuses Quit and Remove during a trial through it")
	}
	if d.Updates != nil || d.Quit != nil {
		t.Fatalf("Deps must leave Updates and Quit to cmd/kids-shelf, got %+v", d)
	}
	if got := filepath.Join(a.dataDir, "spelling-clips"); d.ClipsDir != got {
		t.Fatalf("ClipsDir = %s, want %s", d.ClipsDir, got)
	}
}

// TestAppReservationsBook: Open exposes the queue-backed reservation book
// that cmd/kids-shelf wires the updater to; a standalone hold through it is
// visible to the jobs queue, and releasing clears it.
func TestAppReservationsBook(t *testing.T) {
	a := openTest(t, t.TempDir())
	r := a.Reservations()
	if r == nil {
		t.Fatal("Reservations() = nil")
	}
	// The updater holds a staged release's snapshot reservation through
	// this book; the queue's sums must see it.
	if err := r.Hold("update:snapshot:v9.9.9", 1234, a.dataDir); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	sum, err := a.queue.SumActiveReservedBytes("")
	if err != nil {
		t.Fatal(err)
	}
	if sum != 1234 {
		t.Errorf("queue sees %d reserved bytes, want 1234 (the update hold)", sum)
	}
	if err := r.Release("update:snapshot:v9.9.9"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	sum, err = a.queue.SumActiveReservedBytes("")
	if err != nil {
		t.Fatal(err)
	}
	if sum != 0 {
		t.Errorf("queue sees %d reserved bytes after the release, want 0", sum)
	}
}

// TestStartCloseChecks work stops cleanly: Close cancels the workers and
// returns within 5 seconds, and no goroutine outlives the app (workers exit
// on the canceled context, so the count returns to what it was before).
func TestStartClose(t *testing.T) {
	dataDir := t.TempDir()
	a := openTest(t, dataDir)
	seedYtDlpInstalled(t, dataDir) // keep the workers off the network
	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := a.Start(ctx); err == nil {
		t.Fatal("second Start succeeded, want an error")
	}
	// Give every worker a moment to spawn and run one pass.
	time.Sleep(100 * time.Millisecond)

	started := time.Now()
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if dt := time.Since(started); dt > 5*time.Second {
		t.Fatalf("Close took %s, want under 5s", dt)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("%d goroutines before Start, %d after Close", before, after)
	}
}

// TestDepsInTrialFromJournal: Deps.InTrial reads the durable update
// journal, so the web server refuses Quit and Remove exactly while the
// journal carries an unconfirmed trial (Trying), never on a timer.
func TestDepsInTrialFromJournal(t *testing.T) {
	a := openTest(t, t.TempDir())
	d := a.Deps()
	if d.InTrial == nil {
		t.Fatal("Deps.InTrial is nil")
	}
	if d.InTrial() {
		t.Error("InTrial() = true with no journal, want false")
	}
	path := update.StatePath(a.dataDir)
	b, err := json.Marshal(update.State{
		Current: "v1",
		Trying:  &update.Trial{Version: "v2", Snapshot: "snap"},
	})
	if err != nil {
		t.Fatalf("marshal journal: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("WriteFile journal: %v", err)
	}
	if !d.InTrial() {
		t.Error("InTrial() = false while the journal has Trying, want true")
	}
}

// digestHex returns the sha-256 hex digest of s, the shape the tools.Manifest
// expects.
func digestHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestSetupHelpersReusesValidExtractedSetOnLowSpace: a restart on a nearly
// full disk keeps the helper set working when it is already extracted and
// hash-valid, because setupHelpers reserves only the bytes extraction will
// actually write (none), not the whole set again. A helper that is really
// missing still needs its bytes, so on a full disk the app refuses rather
// than run with a broken set.
func TestSetupHelpersReusesValidExtractedSetOnLowSpace(t *testing.T) {
	dataDir := t.TempDir()
	fsys := fstest.MapFS{
		"ffmpeg": &fstest.MapFile{Data: []byte("ffmpeg-binary"), Mode: 0o755},
		"qjs":    &fstest.MapFile{Data: []byte("qjs-binary"), Mode: 0o755},
	}
	m := tools.Manifest{Version: "v1", Files: []tools.ManifestFile{
		{Name: "ffmpeg", SHA256: digestHex("ffmpeg-binary")},
		{Name: "qjs", SHA256: digestHex("qjs-binary")},
	}}
	// The first launch had room: extract the full set, exactly as a fresh
	// install would.
	if _, err := tools.Extract(fsys, m, dataDir); err != nil {
		t.Fatalf("first Extract: %v", err)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "reservations.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	q, err := jobs.New(st)
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	// The disk is now full, so any Hold fails; extraction of nothing must
	// still succeed.
	reservations := disk.NewReservations(q).WithFree(func(string) (uint64, error) {
		return 0, nil
	})
	t.Setenv(tools.EnvHelpersDir, "") // the helpers come from the bundle path only

	a := &App{cfg: Config{}}
	if helpers := a.setupHelpers(context.Background(), dataDir, reservations, fsys, m); helpers == nil {
		t.Fatal("setupHelpers failed on a full disk with a valid cached set; want the cached helpers reused")
	} else if got := filepath.Dir(helpers.FFmpeg); got != filepath.Join(dataDir, "helpers", "v1") {
		t.Errorf("helpers resolved in %s, want the cached extraction %s", got, filepath.Join(dataDir, "helpers", "v1"))
	}

	// A helper is now really missing: its write needs the freed space, so
	// the full disk refuses instead of shipping a broken set.
	if err := os.Remove(filepath.Join(dataDir, "helpers", "v1", "qjs")); err != nil {
		t.Fatalf("Remove qjs: %v", err)
	}
	if helpers := a.setupHelpers(context.Background(), dataDir, reservations, fsys, m); helpers != nil {
		t.Fatal("setupHelpers succeeded on a full disk with a missing helper; want a refusal")
	}
}

// TestIngestWorkerHonorsSafariSettingFromBudget proves the wiring from
// internal/app: the ingest worker reads the household Safari setting from
// the budget store. With it off, a folder MKV with H.264+AAC is shown in
// place and the job is done with no conversion. The helpers and the fixture
// ffmpeg come from the same environment the ingest pipeline tests use.
func TestIngestWorkerHonorsSafariSettingFromBudget(t *testing.T) {
	if os.Getenv(tools.EnvHelpersDir) == "" || os.Getenv("KS_FIXTURE_FFMPEG") == "" {
		t.Skip("needs KS_HELPERS_DIR and KS_FIXTURE_FFMPEG (ffmpeg/ffprobe plus a full ffmpeg)")
	}
	full := os.Getenv("KS_FIXTURE_FFMPEG")
	src := filepath.Join(t.TempDir(), "clip.mkv")
	cmd := exec.Command(full,
		"-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-c:v", "libx264", "-profile:v", "high", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "128k", "-t", "4", "-f", "matroska", src)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v\n%s", err, b)
	}

	dataDir := t.TempDir()
	a := openTest(t, dataDir)
	if a.worker == nil {
		t.Fatal("Open wired no ingest worker")
	}
	ctx := context.Background()
	if err := a.budget.SetSafariScreens(ctx, false); err != nil {
		t.Fatalf("SetSafariScreens(false): %v", err)
	}
	// openTest points the media dir at a temp dir; helpers resolve from
	// KS_HELPERS_DIR, so the worker can really run.
	id, err := a.ingestQ.Enqueue(ctx, src, core.Source{
		Kind:  core.SourceFolder,
		Path:  src,
		Title: "Clip",
		Show:  "Clips",
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	runDone := make(chan struct{})
	go func() {
		a.worker.Run(runCtx)
		close(runDone)
	}()
	deadline := time.After(120 * time.Second)
	for {
		j, gerr := a.queue.Get(id)
		if gerr == nil && (j.State == jobs.StateDone || j.State == jobs.StateFailed) {
			break
		}
		select {
		case <-deadline:
			cancel()
			<-runDone
			t.Fatal("job did not finish in time")
		case <-time.After(50 * time.Millisecond):
		}
	}
	cancel()
	<-runDone
	job, err := a.queue.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateDone {
		t.Fatalf("state = %s (error %q), want done", job.State, job.Error)
	}
	ep, err := a.library.Episode(ctx, mustEpisodeID(t, job))
	if err != nil {
		t.Fatalf("Episode: %v", err)
	}
	if ep.Owned || ep.File != src {
		t.Errorf("episode = %+v, want the original %q, unowned (Safari off means no conversion)", ep, src)
	}
	if entries, err := os.ReadDir(filepath.Join(a.mediaDir, "library")); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".mp4") {
				t.Errorf("converted copy %s exists with the Safari setting off", e.Name())
			}
		}
	}
}

func mustEpisodeID(t *testing.T, job *jobs.Job) core.EpisodeID {
	t.Helper()
	var p struct {
		EpisodeID core.EpisodeID `json:"episode_id"`
	}
	if err := job.DecodePayload(&p); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	return p.EpisodeID
}
