package library

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/edementhon/kids-shelf/internal/core"
	"github.com/edementhon/kids-shelf/internal/store"
)

func newTestLibrary(t *testing.T) *Library {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	lib, err := New(s)
	if err != nil {
		t.Fatalf("library.New: %v", err)
	}
	return lib
}

func mustPublish(t *testing.T, lib *Library, ep core.Episode) {
	t.Helper()
	if err := lib.Publish(context.Background(), ep); err != nil {
		t.Fatalf("Publish: %v", err)
	}
}

func tmpEpisode(id, showID, show, title, rel string) core.Episode {
	return core.Episode{
		ID:          core.EpisodeID(id),
		ShowID:      showID,
		Title:       title,
		File:        "/out/" + id + ".mp4",
		DurationSec: 180,
		Thumb:       "/out/" + id + ".jpg",
		Source: core.Source{
			Kind:  core.SourceFolder,
			Path:  show + "/" + rel,
			Title: title,
			Show:  show,
		},
	}
}

func TestPublishBySourceRoundTrip(t *testing.T) {
	lib := newTestLibrary(t)
	ep := tmpEpisode("ep-1", "show-bluey", "Bluey", "Keepy Uppy", "Season 1/ep1.mp4")
	mustPublish(t, lib, ep)

	ref, found, err := lib.BySource(context.Background(), ep.Source)
	if err != nil || !found {
		t.Fatalf("BySource = (%+v, %v, %v), want found", ref, found, err)
	}
	if ref.Episode != "ep-1" {
		t.Errorf("BySource episode = %q, want ep-1", ref.Episode)
	}
	if ref.Show != "show-bluey" {
		t.Errorf("BySource show = %q, want show-bluey", ref.Show)
	}

	// Same path under another kind must not resolve.
	if _, found, err := lib.BySource(context.Background(), core.Source{
		Kind: core.SourceNAS, Path: ep.Source.Path,
	}); err != nil || found {
		t.Errorf("BySource with wrong kind = found %v, err %v; want not found", found, err)
	}
	// Same kind under another path must not resolve.
	if _, found, err := lib.BySource(context.Background(), core.Source{
		Kind: ep.Source.Kind, Path: "Other Show/file.mp4",
	}); err != nil || found {
		t.Errorf("BySource with wrong path = found %v, err %v; want not found", found, err)
	}

	got, err := lib.Episode(context.Background(), "ep-1")
	if err != nil {
		t.Fatalf("Episode: %v", err)
	}
	if got.ShowName != "Bluey" || got.Title != "Keepy Uppy" || got.File != "/out/ep-1.mp4" {
		t.Errorf("Episode = %+v, want show Bluey, title Keepy Uppy", got)
	}
	if got.Added.IsZero() {
		t.Error("Episode.Added is zero, want the publish time")
	}

	shows, err := lib.ListShows(context.Background())
	if err != nil {
		t.Fatalf("ListShows: %v", err)
	}
	want := []Show{{ID: "show-bluey", Name: "Bluey", Bin: BinFun, Episodes: 1, Poster: "ep-1"}}
	if !reflect.DeepEqual(shows, want) {
		t.Errorf("ListShows = %+v, want %+v", shows, want)
	}

	eps, err := lib.ShowEpisodes(context.Background(), "show-bluey")
	if err != nil {
		t.Fatalf("ShowEpisodes: %v", err)
	}
	if len(eps) != 1 || eps[0].ID != "ep-1" {
		t.Errorf("ShowEpisodes = %+v, want [ep-1]", eps)
	}
}

// TestEpisodeOwnedRoundTrip pins the app-ownership marker: an in-place
// original (Owned false) and a converted or YouTube file (Owned true) both
// survive Publish and Episode, and republishing flips the marker.
func TestEpisodeOwnedRoundTrip(t *testing.T) {
	lib := newTestLibrary(t)
	inplace := tmpEpisode("ep-inplace", "show-bluey", "Bluey", "In Place", "S01E01.mp4")
	inplace.Owned = false
	inplace.File = "/nas/Bluey/S01E01.mp4"
	mustPublish(t, lib, inplace)
	got, err := lib.Episode(context.Background(), "ep-inplace")
	if err != nil {
		t.Fatalf("Episode: %v", err)
	}
	if got.Owned || got.File != "/nas/Bluey/S01E01.mp4" {
		t.Errorf("in-place episode = %+v, want Owned false with the original path", got)
	}

	owned := tmpEpisode("ep-owned", "show-bluey", "Bluey", "Converted", "S01E02.mkv")
	owned.Owned = true
	mustPublish(t, lib, owned)
	got, err = lib.Episode(context.Background(), "ep-owned")
	if err != nil {
		t.Fatalf("Episode: %v", err)
	}
	if !got.Owned {
		t.Errorf("converted episode Owned = false, want true")
	}

	// Republishing the in-place episode as converted flips the marker,
	// which is what a re-ingest after a format change would do.
	inplace.Owned = true
	inplace.File = "/out/ep-inplace.mp4"
	mustPublish(t, lib, inplace)
	got, err = lib.Episode(context.Background(), "ep-inplace")
	if err != nil {
		t.Fatalf("Episode after republish: %v", err)
	}
	if !got.Owned || got.File != "/out/ep-inplace.mp4" {
		t.Errorf("republished episode = %+v, want Owned true with the converted path", got)
	}
}

func TestPublishResolvesCanonicalPathKey(t *testing.T) {
	lib := newTestLibrary(t)
	// migrate maps every legacy show/file key to the original file's
	// canonical absolute path (core.CanonicalPath); the source kind travels
	// with the episode the way ingest published it.
	const canonical = "/media/tv/The Magic School Bus/Season 1/01.mkv"
	ep := core.Episode{
		ID:     "ep-legacy",
		ShowID: "show-bus",
		Title:  "Ralphie",
		File:   "/out/ep-legacy.mp4",
		Source: core.Source{Kind: core.SourceFolder, Path: canonical, Show: "The Magic School Bus"},
	}
	mustPublish(t, lib, ep)
	ref, found, err := lib.BySource(context.Background(), core.Source{Kind: core.SourceFolder, Path: canonical})
	if err != nil || !found || ref.Episode != "ep-legacy" {
		t.Fatalf("BySource = (%+v, %v, %v), want ep-legacy", ref, found, err)
	}
}

func TestPublishIdempotentPerEpisode(t *testing.T) {
	lib := newTestLibrary(t)
	ep1 := tmpEpisode("ep-1", "show-bluey", "Bluey", "Keepy Uppy", "Season 1/ep1.mp4")
	mustPublish(t, lib, ep1)
	first, _ := lib.Episode(context.Background(), "ep-1")

	// Republishing the same id updates fields but not the count or the
	// added time; the show is reused, not duplicated.
	again := ep1
	again.Title = "Keepy Uppy (remuxed)"
	again.File = "/out/ep-1-new.mp4"
	mustPublish(t, lib, again)

	eps, err := lib.ShowEpisodes(context.Background(), "show-bluey")
	if err != nil {
		t.Fatalf("ShowEpisodes: %v", err)
	}
	if len(eps) != 1 {
		t.Fatalf("after republish ShowEpisodes has %d episodes, want 1", len(eps))
	}
	if eps[0].Title != "Keepy Uppy (remuxed)" || eps[0].File != "/out/ep-1-new.mp4" {
		t.Errorf("republished episode = %+v, want updated title/file", eps[0])
	}
	if !eps[0].Added.Equal(first.Added) {
		t.Errorf("republish changed added: %v -> %v", first.Added, eps[0].Added)
	}

	// A second episode of the same show shares the show and its id.
	ep2 := tmpEpisode("ep-2", "show-bluey", "Bluey", "Shadowlands", "Season 1/ep2.mp4")
	mustPublish(t, lib, ep2)
	shows, err := lib.ListShows(context.Background())
	if err != nil {
		t.Fatalf("ListShows: %v", err)
	}
	if len(shows) != 1 || shows[0].Episodes != 2 {
		t.Errorf("ListShows = %+v, want one show with 2 episodes", shows)
	}
	bySrc, found, err := lib.BySource(context.Background(), ep2.Source)
	if err != nil || !found {
		t.Fatalf("BySource(ep2) = (%+v, %v, %v)", bySrc, found, err)
	}
	if bySrc.Show != "show-bluey" || bySrc.Episode != "ep-2" {
		t.Errorf("BySource(ep2) = %+v, want show-bluey/ep-2", bySrc)
	}
}

func TestPublishRejectsIncompleteEpisodes(t *testing.T) {
	lib := newTestLibrary(t)
	err := lib.Publish(context.Background(), core.Episode{
		ID:     "x",
		Title:  "No show",
		Source: core.Source{Kind: core.SourceFolder, Path: "x/y.mp4"},
	})
	if err == nil {
		t.Error("Publish without a show name succeeded, want error")
	}
	err = lib.Publish(context.Background(), core.Episode{
		Title:  "No id",
		Source: core.Source{Kind: core.SourceFolder, Path: "x/y.mp4", Show: "x"},
	})
	if err == nil {
		t.Error("Publish without an episode id succeeded, want error")
	}
}

func TestBinsAndBinRate(t *testing.T) {
	lib := newTestLibrary(t)
	mustPublish(t, lib, tmpEpisode("e-fun", "s-fun", "Fun Show", "A", "a.mp4"))
	mustPublish(t, lib, tmpEpisode("e-learn", "s-learn", "Learn Show", "B", "b.mp4"))
	mustPublish(t, lib, tmpEpisode("e-free", "s-free", "Free Show", "C", "c.mp4"))

	// New shows default to fun.
	if rate, err := lib.BinRate(context.Background(), "e-fun"); err != nil || rate != 1.0 {
		t.Errorf("BinRate(fun default) = %v, %v; want 1.0", rate, err)
	}

	if err := lib.SetBin(context.Background(), "s-learn", BinLearning); err != nil {
		t.Fatalf("SetBin learning: %v", err)
	}
	if err := lib.SetBin(context.Background(), "s-free", BinFree); err != nil {
		t.Fatalf("SetBin free: %v", err)
	}

	for _, tc := range []struct {
		ep   core.EpisodeID
		want float64
	}{
		{"e-fun", 1.0},
		{"e-learn", 0.5},
		{"e-free", 0.0},
	} {
		rate, err := lib.BinRate(context.Background(), tc.ep)
		if err != nil || rate != tc.want {
			t.Errorf("BinRate(%s) = %v, %v; want %v", tc.ep, rate, err, tc.want)
		}
	}

	// BinRateOf is the same mapping, standalone.
	if BinRateOf(BinFun) != 1.0 || BinRateOf(BinLearning) != 0.5 || BinRateOf(BinFree) != 0.0 {
		t.Error("BinRateOf rates wrong")
	}

	if err := lib.SetBin(context.Background(), "s-fun", "premium"); err == nil {
		t.Error("SetBin with an invalid bin succeeded, want error")
	}
	if err := lib.SetBin(context.Background(), "no-such-show", BinFree); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetBin on unknown show = %v, want ErrNotFound", err)
	}
	if _, err := lib.BinRate(context.Background(), "no-such-ep"); !errors.Is(err, ErrNotFound) {
		t.Errorf("BinRate on unknown episode = %v, want ErrNotFound", err)
	}

	shows, err := lib.ListShows(context.Background())
	if err != nil {
		t.Fatalf("ListShows: %v", err)
	}
	bins := map[string]string{}
	for _, s := range shows {
		bins[s.Name] = s.Bin
	}
	want := map[string]string{"Fun Show": BinFun, "Learn Show": BinLearning, "Free Show": BinFree}
	if !reflect.DeepEqual(bins, want) {
		t.Errorf("ListShows bins = %v, want %v", bins, want)
	}
}

func TestSetShowBin(t *testing.T) {
	lib := newTestLibrary(t)
	ctx := context.Background()
	binsOf := func() map[string]string {
		t.Helper()
		shows, err := lib.ListShows(ctx)
		if err != nil {
			t.Fatalf("ListShows: %v", err)
		}
		bins := map[string]string{}
		for _, s := range shows {
			bins[s.Name] = s.Bin
		}
		return bins
	}

	// A bin set before the show exists is applied when the show is first
	// published; a show without a configured bin still defaults to fun.
	if err := lib.SetShowBin(ctx, "Scheduled Show", BinLearning); err != nil {
		t.Fatalf("SetShowBin before publish: %v", err)
	}
	mustPublish(t, lib, tmpEpisode("e-sched", "s-sched", "Scheduled Show", "A", "a.mp4"))
	mustPublish(t, lib, tmpEpisode("e-plain", "s-plain", "Plain Show", "B", "b.mp4"))
	want := map[string]string{"Scheduled Show": BinLearning, "Plain Show": BinFun}
	if bins := binsOf(); !reflect.DeepEqual(bins, want) {
		t.Errorf("bins after publish = %v, want %v", bins, want)
	}

	// SetShowBin on an existing show changes its bin immediately.
	if err := lib.SetShowBin(ctx, "Scheduled Show", BinFree); err != nil {
		t.Fatalf("SetShowBin on existing show: %v", err)
	}
	if bins := binsOf(); bins["Scheduled Show"] != BinFree {
		t.Errorf("bin after SetShowBin on existing show = %q, want %q", bins["Scheduled Show"], BinFree)
	}

	// Invalid bins are rejected.
	if err := lib.SetShowBin(ctx, "Scheduled Show", "premium"); err == nil {
		t.Error("SetShowBin with an invalid bin succeeded, want error")
	}
}

func TestShowEpisodesNaturalOrdering(t *testing.T) {
	lib := newTestLibrary(t)
	titles := []string{
		"Episode 10",
		"Episode 100",
		"Episode 2",
		"Episode 1",
		"S1E12",
		"S1E2",
		"Ep 9",
	}
	want := []string{
		"Ep 9",
		"Episode 1",
		"Episode 2",
		"Episode 10",
		"Episode 100",
		"S1E2",
		"S1E12",
	}
	for i, tt := range titles {
		mustPublish(t, lib, core.Episode{
			ID:     core.EpisodeID("ep-" + string(rune('a'+i))),
			ShowID: "s",
			Title:  tt,
			File:   "/out/" + tt + ".mp4",
			Source: core.Source{Kind: core.SourceFolder, Path: "Show/" + tt + ".mp4", Show: "Show"},
		})
	}

	eps, err := lib.ShowEpisodes(context.Background(), "s")
	if err != nil {
		t.Fatalf("ShowEpisodes: %v", err)
	}
	got := make([]string, len(eps))
	for i, e := range eps {
		got[i] = e.Title
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ShowEpisodes order = %v, want %v", got, want)
	}
}

func TestListShowsPosterFromFirstThumbedEpisode(t *testing.T) {
	lib := newTestLibrary(t)
	// "Shelf" publishes out of order; only Episode 2 and Episode 3 carry a
	// thumbnail, so the poster must be Episode 2 (the first thumbed one in
	// natural title order, not the first published).
	mustPublish(t, lib, core.Episode{
		ID: "ep-2", ShowID: "shelf", Title: "Episode 2", File: "/out/ep-2.mp4",
		Thumb:  "/out/ep-2.jpg",
		Source: core.Source{Kind: core.SourceFolder, Path: "Shelf/Episode 2.mp4", Show: "Shelf"},
	})
	mustPublish(t, lib, core.Episode{
		ID: "ep-1", ShowID: "shelf", Title: "Episode 1", File: "/out/ep-1.mp4",
		Source: core.Source{Kind: core.SourceFolder, Path: "Shelf/Episode 1.mp4", Show: "Shelf"},
	})
	mustPublish(t, lib, core.Episode{
		ID: "ep-3", ShowID: "shelf", Title: "Episode 3", File: "/out/ep-3.mp4",
		Thumb:  "/out/ep-3.jpg",
		Source: core.Source{Kind: core.SourceFolder, Path: "Shelf/Episode 3.mp4", Show: "Shelf"},
	})
	// "No Thumbs" has episodes but none with a thumbnail: no poster.
	mustPublish(t, lib, core.Episode{
		ID: "nt-1", ShowID: "nothumbs", Title: "Only One", File: "/out/nt-1.mp4",
		Source: core.Source{Kind: core.SourceFolder, Path: "No Thumbs/Only One.mp4", Show: "No Thumbs"},
	})

	shows, err := lib.ListShows(context.Background())
	if err != nil {
		t.Fatalf("ListShows: %v", err)
	}
	got := map[core.ShowID]Show{}
	for _, sh := range shows {
		got[sh.ID] = sh
	}
	if p := got["shelf"].Poster; p != "ep-2" {
		t.Errorf("Shelf poster = %q, want ep-2 (first thumbed in natural title order)", p)
	}
	if got["shelf"].Episodes != 3 {
		t.Errorf("Shelf episodes = %d, want 3", got["shelf"].Episodes)
	}
	if p := got["nothumbs"].Poster; p != "" {
		t.Errorf("No Thumbs poster = %q, want \"\" (no thumbed episode)", p)
	}
}

func TestNaturalLess(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"Episode 2", "Episode 10", true},
		{"Episode 10", "Episode 2", false},
		{"S1E2", "S1E12", true},
		{"S1E12", "S1E2", false},
		{"ep 2", "EP 10", true}, // ASCII case-insensitive
		{"Episode 2", "episode 2", false},
		{"a", "b", true},
		{"", "a", true},
		{"a", "", false},
		{"abc", "abc", false},
	} {
		if got := naturalLess(tc.a, tc.b); got != tc.want {
			t.Errorf("naturalLess(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestSearch(t *testing.T) {
	lib := newTestLibrary(t)
	mustPublish(t, lib, tmpEpisode("e1", "s-bluey", "Bluey", "Keepy Uppy", "a.mp4"))
	mustPublish(t, lib, tmpEpisode("e2", "s-bluey", "Bluey", "Shadowlands", "a.mp4"))
	mustPublish(t, lib, tmpEpisode("e3", "s-sesame", "Sesame Street", "Episode 10", "a.mp4"))

	res, err := lib.Search(context.Background(), "blue")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Shows) != 1 || res.Shows[0].Name != "Bluey" {
		t.Errorf("Search(blue) shows = %+v, want [Bluey]", res.Shows)
	}
	if len(res.Episodes) != 0 {
		t.Errorf("Search(blue) episodes = %+v, want none (titles do not match)", res.Episodes)
	}

	// Episode titles match case-insensitively too.
	res, err = lib.Search(context.Background(), "SHADOW")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Episodes) != 1 || res.Episodes[0].ID != "e2" {
		t.Errorf("Search(SHADOW) episodes = %+v, want [e2]", res.Episodes)
	}

	// Numeric substrings are plain text here.
	res, err = lib.Search(context.Background(), "10")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Episodes) != 1 || res.Episodes[0].ID != "e3" {
		t.Errorf("Search(10) episodes = %+v, want [e3]", res.Episodes)
	}

	// LIKE wildcards in the query match literally.
	res, err = lib.Search(context.Background(), "%")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Shows) != 0 || len(res.Episodes) != 0 {
		t.Errorf("Search(%q) = %+v, want nothing (%% is not a wildcard)", "%", res)
	}

	empty, err := lib.Search(context.Background(), "   ")
	if err != nil || len(empty.Shows) != 0 || len(empty.Episodes) != 0 {
		t.Errorf("Search(blank) = %+v, %v; want nothing", empty, err)
	}
}

func TestEpisodeUnknown(t *testing.T) {
	lib := newTestLibrary(t)
	if _, err := lib.Episode(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Episode(unknown) = %v, want ErrNotFound", err)
	}
	if _, err := lib.Episode(context.Background(), ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("Episode(empty) = %v, want ErrNotFound", err)
	}
}

func TestNewAppliesMigrationsIdempotently(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := New(s); err != nil {
		t.Fatalf("first New: %v", err)
	}
	lib2, err := New(s)
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	mustPublish(t, lib2, tmpEpisode("e1", "s1", "Bluey", "Keepy Uppy", "a.mp4"))
	if _, err := lib2.ListShows(context.Background()); err != nil {
		t.Fatalf("ListShows after second New: %v", err)
	}
}

func TestClockTicksForAdded(t *testing.T) {
	lib := newTestLibrary(t)
	var t0 int64
	lib.now = func() time.Time {
		t0++
		return time.Unix(1_700_000_000+t0, 0).UTC()
	}
	mustPublish(t, lib, tmpEpisode("e1", "s1", "Bluey", "A", "a.mp4"))
	time.Sleep(time.Millisecond)
	mustPublish(t, lib, tmpEpisode("e2", "s1", "Bluey", "B", "b.mp4"))
	eps, err := lib.ShowEpisodes(context.Background(), "s1")
	if err != nil {
		t.Fatalf("ShowEpisodes: %v", err)
	}
	if len(eps) != 2 || !eps[0].Added.Before(eps[1].Added) {
		t.Errorf("added times not monotonic: %+v", eps)
	}
}

func TestFormatTimeFixedWidth(t *testing.T) {
	// Sub-second times must keep lexicographic order identical to
	// chronological order; RFC3339Nano breaks this by dropping the trailing
	// zeros ("12:00:00Z" sorts after "12:00:00.1Z").
	onBoundary := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	sub := time.Date(2026, 10, 26, 12, 0, 0, 123456789, time.UTC)
	on := formatTime(onBoundary)
	frac := formatTime(sub)
	if on != "2026-10-26T12:00:00.000000000Z" {
		t.Errorf("formatTime(whole second) = %q, want fixed 9-digit fraction", on)
	}
	if frac != "2026-10-26T12:00:00.123456789Z" {
		t.Errorf("formatTime(sub-second) = %q, want fixed 9-digit fraction", frac)
	}
	if len(on) != len(frac) || len(on) != len("2006-01-02T15:04:05.000000000Z") {
		t.Errorf("formatTime not fixed width: %q (%d) vs %q (%d)", on, len(on), frac, len(frac))
	}
	if on >= frac {
		t.Errorf("12:00:00 sorts after 12:00:00.123456789: %q >= %q", on, frac)
	}
	// Round-trips through parseTime keep the exact instant.
	for _, tt := range []time.Time{onBoundary, sub} {
		got, err := parseTime(formatTime(tt))
		if err != nil || !got.Equal(tt) {
			t.Errorf("parseTime(formatTime(%v)) = %v, %v; want the same instant", tt, got, err)
		}
	}
	// Old variable-width rows (written before migration 3) still parse.
	old := time.Date(2026, 10, 26, 12, 0, 0, 100_000_000, time.UTC)
	got, err := parseTime("2026-10-26T12:00:00.1Z")
	if err != nil || !got.Equal(old) {
		t.Errorf("parseTime(old form) = %v, %v; want %v", got, err, old)
	}
}

func TestMigrationConvertsLegacyTimesAndDedupesWatchLog(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	// Apply only version 1, then seed rows exactly as an old release wrote
	// them: RFC3339Nano timestamps and duplicate watch-log entries.
	if err := s.Migrate(Migrations[:1]...); err != nil {
		t.Fatalf("migrate v1: %v", err)
	}
	db := s.DB()
	seed := []string{
		`INSERT INTO shows (id, name) VALUES ('s1', 'Bluey')`,
		`INSERT INTO episodes (id, show_id, title, file, src_kind, src_path, added)
		 VALUES ('e1', 's1', 'ep1', '/out/e1.mp4', 'folder', '/m/Bluey/ep1.mp4', '2026-10-26T12:00:00.1Z')`,
		`INSERT INTO resume (kid, episode, pos, updated) VALUES ('alex', 'e1', 10, '2026-10-26T12:00:00.5Z')`,
		`INSERT INTO recent (episode, at, kid) VALUES ('e1', '2026-10-26T12:00:00.05Z', 'alex')`,
	}
	for _, q := range seed {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	dup := `INSERT INTO watch_log (kid, episode, at, title, cost, free)
	        VALUES ('alex', 'e1', '2026-10-26T12:00:00Z', 'ep1', 1, 0)`
	for range 2 {
		if _, err := db.Exec(dup); err != nil {
			t.Fatalf("seed watch_log: %v", err)
		}
	}

	// New applies migrations 2 and 3: the duplicate watch entry collapses to
	// one row and every time column is re-emitted fixed-width.
	lib, err := New(s)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var raw string
	if err := db.QueryRow(`SELECT at FROM watch_log`).Scan(&raw); err != nil {
		t.Fatalf("read watch_log: %v", err)
	}
	if raw != "2026-10-26T12:00:00.000000000Z" {
		t.Errorf("watch_log.at after migration = %q, want fixed width", raw)
	}
	log, err := lib.WatchLog(context.Background())
	if err != nil {
		t.Fatalf("WatchLog: %v", err)
	}
	if len(log) != 1 || log[0].Kid != "alex" {
		t.Errorf("WatchLog after migration = %+v, want the single deduped entry", log)
	}

	ep, err := lib.Episode(context.Background(), "e1")
	if err != nil {
		t.Fatalf("Episode: %v", err)
	}
	if want := time.Date(2026, 10, 26, 12, 0, 0, 100_000_000, time.UTC); !ep.Added.Equal(want) {
		t.Errorf("episodes.added after migration = %v, want %v", ep.Added, want)
	}
	if err := db.QueryRow(`SELECT updated FROM resume`).Scan(&raw); err != nil {
		t.Fatalf("read resume: %v", err)
	}
	if raw != "2026-10-26T12:00:00.500000000Z" {
		t.Errorf("resume.updated after migration = %q, want fixed width", raw)
	}
	if err := db.QueryRow(`SELECT at FROM recent`).Scan(&raw); err != nil {
		t.Fatalf("read recent: %v", err)
	}
	if raw != "2026-10-26T12:00:00.050000000Z" {
		t.Errorf("recent.at after migration = %q, want fixed width", raw)
	}
}
