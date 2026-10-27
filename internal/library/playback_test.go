package library

import (
	"context"
	"testing"
	"time"

	"github.com/edementhon/kids-shelf/internal/core"
)

// clock returns a fake clock that advances one second per call, so every
// library write gets a strictly increasing timestamp.
func clock(lib *Library) func() time.Time {
	var t0 int64
	lib.now = func() time.Time {
		t0++
		return time.Unix(1_700_000_000+t0, 0).UTC()
	}
	return lib.now
}

func mustBlueprint(t *testing.T, lib *Library, n int) []core.Episode {
	t.Helper()
	eps := make([]core.Episode, n)
	for i := range n {
		eps[i] = core.Episode{
			ID:          core.EpisodeID("ep-" + string(rune('a'+i))),
			ShowID:      "show-1",
			Title:       "Episode " + string(rune('a'+i)),
			File:        "/out/ep-" + string(rune('a'+i)) + ".mp4",
			DurationSec: int64(600 + i),
			Thumb:       "/out/ep-" + string(rune('a'+i)) + ".jpg",
			Source:      core.Source{Kind: core.SourceFolder, Path: "Bluey/Season 1/ep" + string(rune('a'+i)) + ".mp4", Show: "Bluey"},
		}
		mustPublish(t, lib, eps[i])
	}
	return eps
}

func TestSaveResumeAndClear(t *testing.T) {
	lib := newTestLibrary(t)
	clock(lib)
	eps := mustBlueprint(t, lib, 1)
	ep := eps[0].ID

	if pos, err := lib.Resume(context.Background(), "alex", ep); err != nil || pos != 0 {
		t.Fatalf("Resume before any save = %v, %v; want 0", pos, err)
	}
	if err := lib.SavePosition(context.Background(), "alex", ep, 120); err != nil {
		t.Fatalf("SavePosition: %v", err)
	}
	if pos, err := lib.Resume(context.Background(), "alex", ep); err != nil || pos != 120 {
		t.Fatalf("Resume after save = %v, %v; want 120", pos, err)
	}
	// Positions are per kid.
	if pos, _ := lib.Resume(context.Background(), "sam", ep); pos != 0 {
		t.Errorf("Resume for other kid = %v, want 0", pos)
	}
	if err := lib.SavePosition(context.Background(), "alex", ep, 300); err != nil {
		t.Fatalf("SavePosition: %v", err)
	}
	if pos, _ := lib.Resume(context.Background(), "alex", ep); pos != 300 {
		t.Errorf("Resume after update = %v, want 300", pos)
	}
	// Zero clears, like ImportResume's contract.
	if err := lib.SavePosition(context.Background(), "alex", ep, 0); err != nil {
		t.Fatalf("SavePosition(0): %v", err)
	}
	if pos, _ := lib.Resume(context.Background(), "alex", ep); pos != 0 {
		t.Errorf("Resume after clear = %v, want 0", pos)
	}
}

func TestKeepWatchingOrderingAndCutoff(t *testing.T) {
	lib := newTestLibrary(t)
	clock(lib)
	eps := mustBlueprint(t, lib, 3)
	alex := core.KidID("alex")

	save := func(ep core.EpisodeID, pos float64) {
		t.Helper()
		if err := lib.SavePosition(context.Background(), alex, ep, pos); err != nil {
			t.Fatalf("SavePosition: %v", err)
		}
	}
	list := func() []KeepWatchingItem {
		t.Helper()
		items, err := lib.KeepWatching(context.Background(), alex)
		if err != nil {
			t.Fatalf("KeepWatching: %v", err)
		}
		return items
	}

	save(eps[1].ID, 4)   // barely started: never listed
	save(eps[2].ID, 100) // older entry
	save(eps[0].ID, 60)  // newest entry
	save(eps[1].ID, 5)   // exactly at the cutoff: still not listed

	items := list()
	if len(items) != 2 {
		t.Fatalf("KeepWatching has %d items, want 2", len(items))
	}
	if items[0].Episode != eps[0].ID || items[1].Episode != eps[2].ID {
		t.Errorf("KeepWatching order = [%s %s], want [%s %s] (updated desc)",
			items[0].Episode, items[1].Episode, eps[0].ID, eps[2].ID)
	}
	if items[0].Pos != 60 || items[1].Pos != 100 {
		t.Errorf("KeepWatching pos = [%v %v], want [60 100]", items[0].Pos, items[1].Pos)
	}
	if items[1].Updated.After(items[0].Updated) {
		t.Error("KeepWatching updated not descending")
	}
	// Entries are enriched with episode and show metadata.
	if items[0].Title != "Episode a" || items[0].DurationSec != int64(600) ||
		items[0].Show != "show-1" || items[0].ShowName != "Bluey" || items[0].Kid != alex {
		t.Errorf("KeepWatching enrichment = %+v", items[0])
	}

	// The other kid sees nothing.
	other, err := lib.KeepWatching(context.Background(), "sam")
	if err != nil {
		t.Fatalf("KeepWatching(sam): %v", err)
	}
	if len(other) != 0 {
		t.Errorf("KeepWatching(sam) = %+v, want empty", other)
	}
}

func TestWatchLogAndRecent(t *testing.T) {
	lib := newTestLibrary(t)
	clock(lib)
	eps := mustBlueprint(t, lib, 14)
	alex := core.KidID("alex")

	for i, ep := range eps {
		if err := lib.RecordWatch(context.Background(), alex, ep.ID,
			"Episode "+string(rune('a'+i)), float64(i), i%2 == 0); err != nil {
			t.Fatalf("RecordWatch: %v", err)
		}
	}

	recent, err := lib.Recent(context.Background())
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(recent) != recentMax {
		t.Fatalf("Recent has %d entries, want %d", len(recent), recentMax)
	}
	if recent[0].Episode != eps[13].ID {
		t.Errorf("Recent[0] = %s, want the newest episode", recent[0].Episode)
	}
	if recent[0].Title != "Episode n" || recent[0].Kid != alex || recent[0].ShowName != "Bluey" {
		t.Errorf("Recent[0] enrichment = %+v", recent[0])
	}
	for i := 1; i < len(recent); i++ {
		if recent[i].At.After(recent[i-1].At) {
			t.Fatalf("Recent not newest-first at %d", i)
		}
	}
	// The two oldest episodes fell off the cap.
	for _, gone := range []core.EpisodeID{eps[0].ID, eps[1].ID} {
		for _, r := range recent {
			if r.Episode == gone {
				t.Errorf("episode %s still in recent after cap prune", gone)
			}
		}
	}

	// Re-watching an episode moves it to the front without duplicating it.
	if err := lib.RecordWatch(context.Background(), alex, eps[0].ID, "Episode a", 1, false); err != nil {
		t.Fatalf("RecordWatch: %v", err)
	}
	recent, err = lib.Recent(context.Background())
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(recent) != recentMax {
		t.Fatalf("Recent after rewatch has %d entries, want %d", len(recent), recentMax)
	}
	if recent[0].Episode != eps[0].ID {
		t.Errorf("Recent[0] = %s, want the rewatched episode at the front", recent[0].Episode)
	}
	seen := map[core.EpisodeID]bool{}
	for _, r := range recent {
		if seen[r.Episode] {
			t.Fatalf("episode %s duplicated in recent", r.Episode)
		}
		seen[r.Episode] = true
	}

	// The watch log has every record, newest first, with the recorded fields.
	log, err := lib.WatchLog(context.Background())
	if err != nil {
		t.Fatalf("WatchLog: %v", err)
	}
	if len(log) != 15 {
		t.Fatalf("WatchLog has %d entries, want 15", len(log))
	}
	if log[0].Episode != eps[0].ID || log[0].Title != "Episode a" || log[0].Cost != 1 || log[0].Free {
		t.Errorf("WatchLog[0] = %+v, want the rewatch entry", log[0])
	}
	if log[1].Free || log[1].Cost != 13 {
		t.Errorf("WatchLog[1] = %+v, want the i=13 entry (odd: not free, cost 13)", log[1])
	}
	if !log[2].Free || log[2].Cost != 12 {
		t.Errorf("WatchLog[2] = %+v, want the i=12 entry (even: free, cost 12)", log[2])
	}
}

// TestRecentForKid: a kid's "Recently watched" comes from that kid's own
// watches. The household recent table keeps one row per episode stamped with
// its last watcher, so filtering it lost an episode from a kid's list as soon
// as a sibling watched it too, and a sibling's twelve newer watches emptied
// the list entirely.
func TestRecentForKid(t *testing.T) {
	ctx := context.Background()
	lib := newTestLibrary(t)
	clock(lib)
	eps := mustBlueprint(t, lib, 15)
	alex, sam := core.KidID("alex"), core.KidID("sam")
	ids := func(items []RecentItem) []core.EpisodeID {
		out := make([]core.EpisodeID, len(items))
		for i, it := range items {
			out[i] = it.Episode
		}
		return out
	}

	// An imported legacy recent row attributed to alex counts as his while
	// the household list still holds it.
	if err := lib.ImportHouseholdRecent(ctx, eps[14].ID, time.Unix(1_600_000_000, 0).UTC(), alex); err != nil {
		t.Fatal(err)
	}
	for _, ep := range []core.Episode{eps[0], eps[1], eps[0]} {
		if err := lib.RecordWatch(ctx, alex, ep.ID, ep.Title, 1, false); err != nil {
			t.Fatal(err)
		}
	}
	got, err := lib.RecentForKid(ctx, alex)
	if err != nil {
		t.Fatalf("RecentForKid: %v", err)
	}
	if g := ids(got); len(g) != 3 || g[0] != eps[0].ID || g[1] != eps[1].ID || g[2] != eps[14].ID {
		t.Fatalf("alex's recent = %v, want [%s %s %s] (newest first, once each, imported row kept)", g, eps[0].ID, eps[1].ID, eps[14].ID)
	}
	if got[0].Title != eps[0].Title || got[0].ShowName != "Bluey" || got[0].Kid != alex {
		t.Errorf("enrichment = %+v", got[0])
	}

	// Sam then watches alex's newest episode and twelve more, which also
	// pushes every older row out of the 12-row household list.
	if err := lib.RecordWatch(ctx, sam, eps[0].ID, eps[0].Title, 1, false); err != nil {
		t.Fatal(err)
	}
	for _, ep := range eps[2:14] {
		if err := lib.RecordWatch(ctx, sam, ep.ID, ep.Title, 1, false); err != nil {
			t.Fatal(err)
		}
	}
	got, err = lib.RecentForKid(ctx, alex)
	if err != nil {
		t.Fatalf("RecentForKid: %v", err)
	}
	if g := ids(got); len(g) < 2 || g[0] != eps[0].ID || g[1] != eps[1].ID {
		t.Fatalf("alex's recent after sam's watches = %v, want his own %s and %s first", g, eps[0].ID, eps[1].ID)
	}

	got, err = lib.RecentForKid(ctx, sam)
	if err != nil {
		t.Fatalf("RecentForKid: %v", err)
	}
	if len(got) != recentMax || got[0].Episode != eps[13].ID {
		t.Errorf("sam's recent = %v, want the newest %d of his own, newest first", ids(got), recentMax)
	}
}

func TestPlaybackImporterMethods(t *testing.T) {
	lib := newTestLibrary(t)
	clock(lib)
	eps := mustBlueprint(t, lib, 2)
	alex := core.KidID("alex")
	legacyUpdated := time.Date(2026, 10, 26, 12, 0, 0, 0, time.FixedZone("ET", -4*3600))

	// ImportResume replays the legacy position and time.
	if err := lib.ImportResume(context.Background(), alex, eps[0].ID, 42.5, legacyUpdated); err != nil {
		t.Fatalf("ImportResume: %v", err)
	}
	if pos, _ := lib.Resume(context.Background(), alex, eps[0].ID); pos != 42.5 {
		t.Errorf("Resume after import = %v, want 42.5", pos)
	}
	items, err := lib.KeepWatching(context.Background(), alex)
	if err != nil {
		t.Fatalf("KeepWatching: %v", err)
	}
	if len(items) != 1 || !items[0].Updated.Equal(legacyUpdated) {
		t.Errorf("KeepWatching after import = %+v, want updated %v", items, legacyUpdated)
	}
	if err := lib.ImportResume(context.Background(), alex, eps[0].ID, 0, legacyUpdated.Add(time.Minute)); err != nil {
		t.Fatalf("ImportResume(0): %v", err)
	}
	if items, _ := lib.KeepWatching(context.Background(), alex); len(items) != 0 {
		t.Errorf("KeepWatching after clearing import = %+v, want empty", items)
	}

	// ImportWatch appends a log entry and leaves the recent list alone.
	if err := lib.ImportWatch(context.Background(), alex, eps[1].ID, legacyUpdated, "Episode b", 2, true); err != nil {
		t.Fatalf("ImportWatch: %v", err)
	}
	log, err := lib.WatchLog(context.Background())
	if err != nil {
		t.Fatalf("WatchLog: %v", err)
	}
	if len(log) != 1 || log[0].Episode != eps[1].ID || log[0].Cost != 2 || !log[0].Free ||
		log[0].Kid != alex || !log[0].At.Equal(legacyUpdated) {
		t.Errorf("WatchLog after import = %+v", log)
	}
	recent, _ := lib.Recent(context.Background())
	if len(recent) != 0 {
		t.Errorf("Recent after ImportWatch = %+v, want empty (recent imported separately)", recent)
	}

	// ImportHouseholdRecent fills the recent list with its own timestamps.
	if err := lib.ImportHouseholdRecent(context.Background(), eps[0].ID, legacyUpdated.Add(2*time.Minute), alex); err != nil {
		t.Fatalf("ImportHouseholdRecent: %v", err)
	}
	if err := lib.ImportHouseholdRecent(context.Background(), eps[1].ID, legacyUpdated.Add(3*time.Minute), ""); err != nil {
		t.Fatalf("ImportHouseholdRecent: %v", err)
	}
	recent, err = lib.Recent(context.Background())
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(recent) != 2 || recent[0].Episode != eps[1].ID || recent[0].Kid != "" ||
		recent[1].Episode != eps[0].ID {
		t.Errorf("Recent after imports = %+v", recent)
	}
}

func TestImportHouseholdRecentCapAndDedup(t *testing.T) {
	lib := newTestLibrary(t)
	clock(lib)
	eps := mustBlueprint(t, lib, 15)
	base := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	for i, ep := range eps {
		if err := lib.ImportHouseholdRecent(context.Background(), ep.ID, base.Add(time.Duration(i)*time.Minute), ""); err != nil {
			t.Fatalf("ImportHouseholdRecent: %v", err)
		}
	}
	recent, err := lib.Recent(context.Background())
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(recent) != recentMax {
		t.Fatalf("Recent has %d entries, want %d", len(recent), recentMax)
	}
	if recent[0].Episode != eps[14].ID {
		t.Errorf("Recent[0] = %s, want newest %s", recent[0].Episode, eps[14].ID)
	}
	// Re-import of an old episode with a newer time moves it to the front,
	// still one row.
	if err := lib.ImportHouseholdRecent(context.Background(), eps[0].ID,
		base.Add(15*time.Minute), "alex"); err != nil {
		t.Fatalf("ImportHouseholdRecent: %v", err)
	}
	recent, err = lib.Recent(context.Background())
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(recent) != recentMax {
		t.Fatalf("Recent after bump has %d entries, want %d", len(recent), recentMax)
	}
	if recent[0].Episode != eps[0].ID || recent[0].Kid != "alex" {
		t.Errorf("Recent[0] = %+v, want the bumped episode with kid", recent[0])
	}
	count := 0
	for _, r := range recent {
		if r.Episode == eps[0].ID {
			count++
		}
	}
	if count != 1 {
		t.Errorf("episode %s appears %d times, want 1", eps[0].ID, count)
	}
}

func TestImportWatchIdempotent(t *testing.T) {
	lib := newTestLibrary(t)
	clock(lib)
	ep := mustBlueprint(t, lib, 1)[0].ID
	alex := core.KidID("alex")
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

	// Importing the same legacy watch-log entry twice (a rerun of migrate)
	// keeps one copy: the (kid, episode, watched-at) identity is UNIQUE.
	for range 2 {
		if err := lib.ImportWatch(context.Background(), alex, ep, at, "Episode a", 2, true); err != nil {
			t.Fatalf("ImportWatch: %v", err)
		}
	}
	log, err := lib.WatchLog(context.Background())
	if err != nil {
		t.Fatalf("WatchLog: %v", err)
	}
	if len(log) != 1 || log[0].Episode != ep || log[0].Cost != 2 || !log[0].Free {
		t.Errorf("WatchLog after double import = %+v, want the single entry", log)
	}

	// Distinct watched-at times are separate entries, not collapsed.
	later := at.Add(30 * time.Second)
	if err := lib.ImportWatch(context.Background(), alex, ep, later, "Episode a", 2, true); err != nil {
		t.Fatalf("ImportWatch(later): %v", err)
	}
	log, err = lib.WatchLog(context.Background())
	if err != nil {
		t.Fatalf("WatchLog: %v", err)
	}
	if len(log) != 2 {
		t.Errorf("WatchLog after a second distinct entry = %d entries, want 2", len(log))
	}
}

func TestWatchLogOrdersSubSecondTimesChronologically(t *testing.T) {
	lib := newTestLibrary(t)
	clock(lib)
	eps := mustBlueprint(t, lib, 2)
	alex := core.KidID("alex")
	// The later watch carries a sub-second fraction; the earlier one sits
	// exactly on a second boundary. RFC3339Nano renders them as
	// "12:00:00.1Z" and "12:00:00Z", which sort the wrong way; the
	// fixed-width format keeps them chronological.
	onBoundary := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	subSecond := time.Date(2026, 10, 26, 12, 0, 0, 100_000_000, time.UTC)
	for _, w := range []struct {
		ep core.EpisodeID
		at time.Time
	}{
		{eps[0].ID, onBoundary},
		{eps[1].ID, subSecond},
	} {
		if err := lib.ImportWatch(context.Background(), alex, w.ep, w.at, "Episode", 1, false); err != nil {
			t.Fatalf("ImportWatch: %v", err)
		}
	}
	log, err := lib.WatchLog(context.Background())
	if err != nil {
		t.Fatalf("WatchLog: %v", err)
	}
	if len(log) != 2 || log[0].Episode != eps[1].ID {
		t.Fatalf("WatchLog order = %+v, want the sub-second watch first", log)
	}
	if !log[0].At.Equal(subSecond) || !log[1].At.Equal(onBoundary) {
		t.Errorf("WatchLog times = %v, %v; want %v then %v",
			log[0].At, log[1].At, subSecond, onBoundary)
	}
}
