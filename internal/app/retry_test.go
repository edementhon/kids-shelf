package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/edementhon/kids-shelf/internal/core"
	"github.com/edementhon/kids-shelf/internal/jobs"
)

// TestRetryFailedAfterUpgrade: the first Open of a new release version puts
// failed folder and NAS videos back in the queue (the update may carry the
// fix), once per version; failed YouTube downloads keep their own Retry, and
// unversioned builds never retry.
func TestRetryFailedAfterUpgrade(t *testing.T) {
	dataDir := t.TempDir()
	open := func(version string) *App {
		t.Helper()
		cfg := testConfig(t, dataDir)
		cfg.Version = version
		a, err := Open(context.Background(), cfg)
		if err != nil {
			t.Fatalf("Open %s: %v", version, err)
		}
		return a
	}
	fail := func(a *App, id string, kind core.SourceKind) {
		t.Helper()
		// The payload shape ingest writes: core.Source embedded untagged.
		payload, err := json.Marshal(struct {
			File   string      `json:"file"`
			Source core.Source `json:"source"`
		}{"/videos/" + id, core.Source{Kind: kind, Path: "/videos/" + id}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.store.DB().Exec(`INSERT INTO jobs (id, kind, stage, state, payload, error, created, updated, seq, priority)
			VALUES (?, 'ingest', 'probe', 'failed', ?, 'This file is damaged or incomplete and can''t be played.',
			'2026-10-27T00:00:00Z', '2026-10-27T00:00:00Z', (SELECT COALESCE(MAX(seq), 0) + 1 FROM jobs), 0)`,
			id, string(payload)); err != nil {
			t.Fatal(err)
		}
	}
	state := func(a *App, id string) string {
		t.Helper()
		j, err := a.queue.Get(id)
		if err != nil {
			t.Fatalf("Get %s: %v", id, err)
		}
		return j.State
	}

	a := open("dev") // unversioned: never retries
	fail(a, "folder.mkv", core.SourceFolder)
	fail(a, "nas.mkv", core.SourceNAS)
	fail(a, "youtube", core.SourceYouTube)
	a.Close()

	a = open("dev")
	if got := state(a, "folder.mkv"); got != jobs.StateFailed {
		t.Errorf("dev build retried a failed job (state %s)", got)
	}
	a.Close()

	a = open("v1.2.0")
	for id, want := range map[string]string{"folder.mkv": jobs.StateQueued, "nas.mkv": jobs.StateQueued, "youtube": jobs.StateFailed} {
		if got := state(a, id); got != want {
			t.Errorf("after the update: %s is %s, want %s", id, got, want)
		}
	}
	// It fails again under this version: no second retry for the same one.
	if _, err := a.store.DB().Exec(`UPDATE jobs SET state = 'failed' WHERE id = 'folder.mkv'`); err != nil {
		t.Fatal(err)
	}
	a.Close()

	a = open("v1.2.0")
	if got := state(a, "folder.mkv"); got != jobs.StateFailed {
		t.Errorf("same version reopened: folder.mkv is %s, want it left failed", got)
	}
	a.Close()

	a = open("v1.2.1")
	if got := state(a, "folder.mkv"); got != jobs.StateQueued {
		t.Errorf("next version: folder.mkv is %s, want queued again", got)
	}
	a.Close()
}
