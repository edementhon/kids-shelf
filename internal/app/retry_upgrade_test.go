package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/edementhon/kids-shelf/internal/core"
	"github.com/edementhon/kids-shelf/internal/jobs"
	"github.com/edementhon/kids-shelf/internal/library"
	"github.com/edementhon/kids-shelf/internal/store"
)

// retryFixture is a fully migrated store with its queue and library.
type retryFixture struct {
	st  *store.Store
	q   *jobs.Queue
	lib *library.Library
	seq int
}

func newRetryFixture(t *testing.T) *retryFixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "kids-shelf.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(allMigrations...); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	q, err := jobs.New(st)
	if err != nil {
		t.Fatal(err)
	}
	lib, err := library.New(st)
	if err != nil {
		t.Fatal(err)
	}
	return &retryFixture{st: st, q: q, lib: lib}
}

// job inserts an ingest job with the payload ingest really writes: its own
// JSON tags, core.Source embedded untagged ($.source.Kind, $.source.Path).
func (f *retryFixture) job(t *testing.T, id, state, path, episode string, shown bool) {
	t.Helper()
	f.seq++
	payload, err := json.Marshal(struct {
		File        string         `json:"file"`
		Source      core.Source    `json:"source"`
		EpisodeID   core.EpisodeID `json:"episode_id"`
		DurationSec int64          `json:"duration_sec"`
		Action      string         `json:"action,omitempty"`
		Output      string         `json:"output,omitempty"`
		Shown       bool           `json:"shown"`
		Published   bool           `json:"published"`
	}{path, core.Source{Kind: core.SourceFolder, Path: path, Title: id, Show: "Show"},
		core.EpisodeID(episode), 600, "full", path, shown, shown})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.DB().Exec(`INSERT INTO jobs (id, kind, stage, state, payload, error, created, updated, seq, priority)
		VALUES (?, 'ingest', 'convert', ?, ?, 'It broke.', '2026-10-27T00:00:00Z', '2026-10-27T00:00:00Z', ?, 3000600)`,
		id, state, string(payload), f.seq); err != nil {
		t.Fatal(err)
	}
}

func (f *retryFixture) publish(t *testing.T, episode, path, issue, fix string) {
	t.Helper()
	if err := f.lib.Publish(context.Background(), core.Episode{
		ID: core.EpisodeID(episode), ShowID: "show-1", Title: episode, File: path, DurationSec: 600,
		Source: core.Source{Kind: core.SourceFolder, Path: path, Title: episode, Show: "Show"},
		Issue:  issue, Fix: fix,
	}); err != nil {
		t.Fatalf("Publish %s: %v", episode, err)
	}
}

func (f *retryFixture) get(t *testing.T, id string) *jobs.Job {
	t.Helper()
	j, err := f.q.Get(id)
	if err != nil {
		t.Fatalf("Get %s: %v", id, err)
	}
	return j
}

// TestRetryAfterUpgradeCurrentJobsOnly: the per-version retry requeues only
// a source's current failed job. A failed job superseded by a later job of
// the same source, or whose source another published episode already
// plays, keeps its failure. A retried failed improvement keeps its episode
// id and goes back through the show phase (its publish marker cleared, its
// shown marker kept), which republishes it "pending" for the retry.
func TestRetryAfterUpgradeCurrentJobsOnly(t *testing.T) {
	f := newRetryFixture(t)
	f.job(t, "old", jobs.StateFailed, "/media/a.mkv", "ep-a-old", false)
	f.job(t, "new", jobs.StateDone, "/media/a.mkv", "ep-a-new", false)
	f.publish(t, "ep-a-new", "/media/a.mkv", "", "")
	f.job(t, "orphan", jobs.StateFailed, "/media/b.mkv", "ep-b-job", false)
	f.publish(t, "ep-b-other", "/media/b.mkv", "", "")
	f.job(t, "shown", jobs.StateFailed, "/media/c.mkv", "ep-c", true)
	f.publish(t, "ep-c", "/media/c.mkv", core.IssuePicture, core.FixFailed)

	n, err := retryFailedAfterUpgrade(f.st, f.q, "v1.2.0")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if n != 1 {
		t.Errorf("retried %d, want 1 (the shown improvement)", n)
	}
	for id, want := range map[string]string{"old": jobs.StateFailed, "new": jobs.StateDone, "orphan": jobs.StateFailed, "shown": jobs.StateQueued} {
		if got := f.get(t, id).State; got != want {
			t.Errorf("%s is %s, want %s", id, got, want)
		}
	}
	var pl struct {
		EpisodeID core.EpisodeID `json:"episode_id"`
		Shown     bool           `json:"shown"`
		Published bool           `json:"published"`
		Action    string         `json:"action"`
		Source    core.Source    `json:"source"`
	}
	if err := json.Unmarshal(f.get(t, "shown").Payload, &pl); err != nil {
		t.Fatal(err)
	}
	if pl.EpisodeID != "ep-c" || !pl.Shown || pl.Published || pl.Action != "" || pl.Source.Path != "/media/c.mkv" {
		t.Errorf("retried payload = %+v, want episode ep-c, shown, not published, no stale plan", pl)
	}
}

// TestRetryAfterUpgradeIsAtomic: the requeue and the retried_version marker
// commit together. When the marker cannot be written, nothing is requeued,
// so the next launch of the same version tries again instead of retrying
// jobs that fail again on every launch.
func TestRetryAfterUpgradeIsAtomic(t *testing.T) {
	f := newRetryFixture(t)
	f.job(t, "folder", jobs.StateFailed, "/media/a.mkv", "ep-a", false)
	if _, err := f.st.DB().Exec(`CREATE TRIGGER block_marker BEFORE INSERT ON app_state
		BEGIN SELECT RAISE(ABORT, 'marker write refused'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := retryFailedAfterUpgrade(f.st, f.q, "v1.2.0"); err == nil {
		t.Fatal("retry succeeded although the marker write failed")
	}
	if got := f.get(t, "folder").State; got != jobs.StateFailed {
		t.Fatalf("after the failed marker write the job is %s, want still failed", got)
	}
	if _, err := f.st.DB().Exec(`DROP TRIGGER block_marker`); err != nil {
		t.Fatal(err)
	}
	n, err := retryFailedAfterUpgrade(f.st, f.q, "v1.2.0")
	if err != nil || n != 1 {
		t.Fatalf("retry = %d, %v; want 1", n, err)
	}
	if got := f.get(t, "folder").State; got != jobs.StateQueued {
		t.Errorf("job is %s, want queued", got)
	}
	var marker string
	if err := f.st.DB().QueryRow(`SELECT value FROM app_state WHERE key = 'retried_version'`).Scan(&marker); err != nil || marker != "v1.2.0" {
		t.Errorf("marker = %q, %v; want v1.2.0", marker, err)
	}
}
