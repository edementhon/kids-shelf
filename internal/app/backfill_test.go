package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/edementhon/kids-shelf/internal/core"
	"github.com/edementhon/kids-shelf/internal/library"
	"github.com/edementhon/kids-shelf/internal/store"
)

// TestBackfillIssueFixFromJobs: a database that ran the show-first ingest
// before episodes had issue/fix columns has shown episodes with empty fields
// although their better copies are queued or failed. app v1 must mark them
// (pending: "Getting ready" instead of trying the unplayable original;
// failed: plays the original with a label) and leave everything else alone.
func TestBackfillIssueFixFromJobs(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "kids-shelf.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	before := allMigrations[:len(allMigrations)-len(appMigrations)]
	if err := s.Migrate(before...); err != nil {
		t.Fatalf("migrate (before the backfill): %v", err)
	}
	lib, err := library.New(s)
	if err != nil {
		t.Fatal(err)
	}
	publish := func(id string, owned bool) {
		t.Helper()
		if err := lib.Publish(ctx, core.Episode{
			ID: core.EpisodeID(id), ShowID: "show-1", Title: id, File: "/media/" + id + ".mkv",
			DurationSec: 600, Owned: owned,
			Source: core.Source{Kind: core.SourceFolder, Path: "/media/" + id + ".mkv", Title: id, Show: "Show"},
		}); err != nil {
			t.Fatalf("Publish %s: %v", id, err)
		}
	}
	seq := 0
	job := func(episode, action, state string, priority int, shown bool) {
		t.Helper()
		seq++
		// The ingest payload's own JSON tags (internal/ingest payload).
		payload, err := json.Marshal(struct {
			File        string         `json:"file"`
			Source      core.Source    `json:"source"`
			EpisodeID   core.EpisodeID `json:"episode_id"`
			DurationSec int64          `json:"duration_sec"`
			Action      string         `json:"action,omitempty"`
			Shown       bool           `json:"shown"`
		}{"/media/" + episode + ".mkv", core.Source{Kind: core.SourceFolder, Path: "/media/" + episode + ".mkv"},
			core.EpisodeID(episode), 600, action, shown})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB().Exec(`INSERT INTO jobs (id, kind, stage, state, payload, error, created, updated, seq, priority)
			VALUES (?, 'ingest', 'convert', ?, ?, '', '2026-10-27T00:00:00Z', '2026-10-27T00:00:00Z', ?, ?)`,
			"job-"+episode, state, string(payload), seq, priority); err != nil {
			t.Fatal(err)
		}
	}
	publish("ep-sound", false)
	job("ep-sound", "audio", "queued", 2_000_600, true)
	publish("ep-picture", false)
	job("ep-picture", "full", "running", 3_000_600, true)
	publish("ep-failed", false)
	job("ep-failed", "full", "failed", 3_000_600, true)
	publish("ep-inplace", false)
	job("ep-inplace", "inplace", "done", 0, true)
	publish("ep-converted", true) // improvement already landed
	job("ep-converted", "audio", "done", 2_000_600, true)

	if err := s.Migrate(allMigrations...); err != nil {
		t.Fatalf("migrate (with the backfill): %v", err)
	}
	if err := s.Migrate(allMigrations...); err != nil {
		t.Fatalf("migrate again: %v", err)
	}
	for _, tc := range []struct{ id, issue, fix string }{
		{"ep-sound", "sound", "pending"},
		{"ep-picture", "picture", "pending"},
		{"ep-failed", "picture", "failed"},
		{"ep-inplace", "", ""},
		{"ep-converted", "", ""},
	} {
		ep, err := lib.Episode(ctx, core.EpisodeID(tc.id))
		if err != nil {
			t.Fatalf("Episode %s: %v", tc.id, err)
		}
		if ep.Issue != tc.issue || ep.Fix != tc.fix {
			t.Errorf("%s: issue=%q fix=%q, want %q %q", tc.id, ep.Issue, ep.Fix, tc.issue, tc.fix)
		}
	}
}
