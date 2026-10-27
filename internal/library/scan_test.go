package library

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/edementhon/kids-shelf/internal/core"
)

func writeFile(t *testing.T, path string, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

// bumpMtime pushes a directory's mtime clearly into the future so a change
// to it is detected even under coarse filesystem timestamps.
func bumpMtime(t *testing.T, dir string) {
	t.Helper()
	future := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(dir, future, future); err != nil {
		t.Fatalf("Chtimes(%s): %v", dir, err)
	}
}

func scanTree(t *testing.T, root string) {
	t.Helper()
	writeFile(t, filepath.Join(root, "Bluey", "ep1.mp4"), "v")
	writeFile(t, filepath.Join(root, "Bluey", "Season 1", "ep2.mkv"), "v")
	writeFile(t, filepath.Join(root, "Bluey", "notes.txt"), "not a video")
	writeFile(t, filepath.Join(root, "Other Show", "ep3.webm"), "v")
}

func TestScannerEnqueuesNewFilesOnce(t *testing.T) {
	lib := newTestLibrary(t)
	root := t.TempDir()
	scanTree(t, root)
	if err := lib.AddSource(context.Background(), root, core.SourceFolder, nil); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	q := core.NewFakeIngestQueue(nil)

	if err := lib.Scan(context.Background(), q); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	if got := q.Enqueued(); got != 3 {
		t.Fatalf("first scan enqueued %d files, want 3", got)
	}

	calls := q.Calls()
	wantSrcs := []struct{ path, title, show string }{
		{
			filepath.Join(root, "Bluey", "Season 1", "ep2.mkv"),
			"ep2", "Bluey",
		},
		{
			filepath.Join(root, "Bluey", "ep1.mp4"),
			"ep1", "Bluey",
		},
		{
			filepath.Join(root, "Other Show", "ep3.webm"),
			"ep3", "Other Show",
		},
	}
	for i, want := range wantSrcs {
		c := calls[i]
		if c.Source.Kind != core.SourceFolder {
			t.Errorf("call %d kind = %q, want folder", i, c.Source.Kind)
		}
		// Path is the original file's canonical absolute path: this is what
		// BySource resolves and what scan_enqueued dedupes on, so two roots
		// holding the same Show/ep.mp4 do not collide.
		if c.Source.Path != filepath.ToSlash(want.path) {
			t.Errorf("call %d path = %q, want %q", i, c.Source.Path, want.path)
		}
		if c.Source.Title != want.title || c.Source.Show != want.show {
			t.Errorf("call %d title/show = %q/%q, want %q/%q", i, c.Source.Title, c.Source.Show, want.title, want.show)
		}
		wantFile := []string{
			filepath.Join(root, "Bluey", "Season 1", "ep2.mkv"),
			filepath.Join(root, "Bluey", "ep1.mp4"),
			filepath.Join(root, "Other Show", "ep3.webm"),
		}[i]
		if c.File != wantFile {
			t.Errorf("call %d file = %q, want %q", i, c.File, wantFile)
		}
		if !filepath.IsAbs(c.File) {
			t.Errorf("call %d file = %q, want absolute", i, c.File)
		}
	}

	// A second scan with nothing changed must enqueue nothing: the files are
	// neither published (BySource misses) nor re-enqueued (markers).
	if err := lib.Scan(context.Background(), q); err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if got := q.Enqueued(); got != 3 {
		t.Fatalf("second scan enqueued %d files (want still 3)", got)
	}
}

func TestScannerEnqueuesNewFileAfterDirectoryChange(t *testing.T) {
	lib := newTestLibrary(t)
	root := t.TempDir()
	scanTree(t, root)
	if err := lib.AddSource(context.Background(), root, core.SourceFolder, nil); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	q := core.NewFakeIngestQueue(nil)
	if err := lib.Scan(context.Background(), q); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	if q.Enqueued() != 3 {
		t.Fatalf("first scan enqueued %d, want 3", q.Enqueued())
	}

	// A file added later is picked up because its show directory's mtime
	// changed; the unchanged Season 1 subtree keeps its own mtime.
	writeFile(t, filepath.Join(root, "Bluey", "ep4.avi"), "v")
	bumpMtime(t, filepath.Join(root, "Bluey"))
	if err := lib.Scan(context.Background(), q); err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if got := q.Enqueued(); got != 4 {
		t.Fatalf("after adding a file, %d enqueued, want 4", got)
	}
	last := q.Calls()[3]
	if last.Source.Path != filepath.ToSlash(filepath.Join(root, "Bluey", "ep4.avi")) || last.Source.Title != "ep4" {
		t.Errorf("new call = %+v, want %s titled ep4",
			last, filepath.Join(root, "Bluey", "ep4.avi"))
	}

	// And again with no changes: nothing new.
	if err := lib.Scan(context.Background(), q); err != nil {
		t.Fatalf("third Scan: %v", err)
	}
	if got := q.Enqueued(); got != 4 {
		t.Fatalf("third scan enqueued %d, want still 4", got)
	}
}

func TestScannerFindsDeeplyAddedFileWithoutShowDirChange(t *testing.T) {
	lib := newTestLibrary(t)
	root := t.TempDir()
	scanTree(t, root)
	if err := lib.AddSource(context.Background(), root, core.SourceFolder, nil); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	q := core.NewFakeIngestQueue(nil)
	if err := lib.Scan(context.Background(), q); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	if q.Enqueued() != 3 {
		t.Fatalf("first scan enqueued %d, want 3", q.Enqueued())
	}

	// A file two levels below the show's root appears. Only Season 1's mtime
	// changes — the show directory's does not — so a shortcut that returns at
	// the unchanged show directory would miss it. Restore the show directory's
	// mtime explicitly so the test does not depend on filesystem behavior.
	showDir := filepath.Join(root, "Bluey")
	before, err := os.Stat(showDir)
	if err != nil {
		t.Fatalf("Stat(%s): %v", showDir, err)
	}
	writeFile(t, filepath.Join(showDir, "Season 1", "new.mp4"), "v")
	bumpMtime(t, filepath.Join(showDir, "Season 1"))
	at := before.ModTime()
	if err := os.Chtimes(showDir, at, at); err != nil {
		t.Fatalf("Chtimes(%s): %v", showDir, err)
	}

	if err := lib.Scan(context.Background(), q); err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if got := q.Enqueued(); got != 4 {
		t.Fatalf("after adding a deep file, %d enqueued, want 4", got)
	}
	last := q.Calls()[3]
	want := filepath.ToSlash(filepath.Join(showDir, "Season 1", "new.mp4"))
	if last.Source.Path != want || last.Source.Title != "new" || last.Source.Show != "Bluey" {
		t.Errorf("new call = %+v, want %s titled new in show Bluey", last, want)
	}

	// And again with no changes: nothing new.
	if err := lib.Scan(context.Background(), q); err != nil {
		t.Fatalf("third Scan: %v", err)
	}
	if got := q.Enqueued(); got != 4 {
		t.Fatalf("third scan enqueued %d, want still 4", got)
	}
}

func TestScannerHonorsAllowlist(t *testing.T) {
	lib := newTestLibrary(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "A", "ep1.mp4"), "v")
	writeFile(t, filepath.Join(root, "B", "ep2.mp4"), "v")
	writeFile(t, filepath.Join(root, "C", "Season 1", "ep3.mp4"), "v")
	if err := lib.AddSource(context.Background(), root, core.SourceFolder, []string{"A", "C"}); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	q := core.NewFakeIngestQueue(nil)
	if err := lib.Scan(context.Background(), q); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got := q.Enqueued(); got != 2 {
		t.Fatalf("allowlisted scan enqueued %d, want 2 (A and C only)", got)
	}
	for _, c := range q.Calls() {
		if c.Source.Path == filepath.ToSlash(filepath.Join(root, "B", "ep2.mp4")) {
			t.Errorf("show outside the allowlist was enqueued: %+v", c)
		}
	}
}

func TestScannerSkipRules(t *testing.T) {
	lib := newTestLibrary(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "Bluey", "normal.mp4"), "v")
	writeFile(t, filepath.Join(root, "Bluey", "#intro.mp4"), "v")
	writeFile(t, filepath.Join(root, "Bluey", ".DS_Store"), "v")
	writeFile(t, filepath.Join(root, "Bluey", "subtitles.srt"), "v")
	writeFile(t, filepath.Join(root, "Bluey", "#extra", "epx.mkv"), "v")
	writeFile(t, filepath.Join(root, "Bluey", ".hidden", "epy.mkv"), "v")
	writeFile(t, filepath.Join(root, "#hidden-show", "epz.mp4"), "v")
	writeFile(t, filepath.Join(root, "@eaDir", "syno.mp4"), "v")
	writeFile(t, filepath.Join(root, ".dotdir", "epw.mp4"), "v")
	if err := lib.AddSource(context.Background(), root, core.SourceFolder, nil); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	q := core.NewFakeIngestQueue(nil)
	if err := lib.Scan(context.Background(), q); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got := q.Enqueued(); got != 1 {
		for _, c := range q.Calls() {
			t.Logf("enqueued %s/%s", c.Source.Path, c.Source.Title)
		}
		t.Fatalf("skip-rule scan enqueued %d, want 1 (only Bluey/normal.mp4)", got)
	}
	if c := q.Calls()[0]; c.Source.Path != filepath.ToSlash(filepath.Join(root, "Bluey", "normal.mp4")) {
		t.Errorf("only enqueue = %+v, want %s",
			c, filepath.Join(root, "Bluey", "normal.mp4"))
	}
}

func TestScannerSkipsPublishedEpisodes(t *testing.T) {
	lib := newTestLibrary(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "Bluey", "ep1.mp4"), "v")
	// The episode is already in the catalog with this provenance.
	mustPublish(t, lib, core.Episode{
		ID:     "ep-1",
		ShowID: "show-bluey",
		Title:  "ep1",
		File:   "/out/ep-1.mp4",
		Source: core.Source{Kind: core.SourceFolder, Path: filepath.ToSlash(filepath.Join(root, "Bluey", "ep1.mp4")), Show: "Bluey"},
	})
	if err := lib.AddSource(context.Background(), root, core.SourceFolder, nil); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	q := core.NewFakeIngestQueue(nil)
	if err := lib.Scan(context.Background(), q); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got := q.Enqueued(); got != 0 {
		t.Fatalf("scan enqueued %d files for an already-published source, want 0", got)
	}
}

// TestScannerKeepsEpisodesOfVanishedOriginals pins the in-place rule: when
// the source file of a published episode disappears (a NAS that is simply
// unmounted is the canonical case), the scan must not delete the episode -
// the catalog row and its playback history survive, and nothing is
// re-enqueued. The decision (2026-10-27) leaves re-probing a changed
// original to a future pass that records size/mtime at publish.
func TestScannerKeepsEpisodesOfVanishedOriginals(t *testing.T) {
	lib := newTestLibrary(t)
	root := t.TempDir()
	orig := filepath.Join(root, "Bluey", "ep1.mp4")
	writeFile(t, orig, "v")
	mustPublish(t, lib, core.Episode{
		ID:          "ep-1",
		ShowID:      "show-bluey",
		Title:       "ep1",
		File:        orig, // an in-place episode records the original's path
		Owned:       false,
		DurationSec: 60,
		Source: core.Source{
			Kind:  core.SourceFolder,
			Path:  filepath.ToSlash(orig),
			Title: "ep1",
			Show:  "Bluey",
		},
	})
	if err := lib.AddSource(context.Background(), root, core.SourceFolder, nil); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	if err := os.Remove(orig); err != nil {
		t.Fatalf("remove the original: %v", err)
	}
	q := core.NewFakeIngestQueue(nil)
	if err := lib.Scan(context.Background(), q); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got := q.Enqueued(); got != 0 {
		t.Fatalf("scan enqueued %d files for a vanished original, want 0", got)
	}
	if _, found, err := lib.BySource(context.Background(), core.Source{
		Kind: core.SourceFolder, Path: filepath.ToSlash(orig),
	}); err != nil || !found {
		t.Fatalf("BySource after the original vanished = found %v, err %v; want the episode kept", found, err)
	}
	if _, err := lib.Episode(context.Background(), "ep-1"); err != nil {
		t.Errorf("episode deleted with its original: %v", err)
	}
}

func TestScannerTwoSourcesKeepKindsDistinct(t *testing.T) {
	lib := newTestLibrary(t)
	root1, root2 := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(root1, "Show", "x.mp4"), "v")
	writeFile(t, filepath.Join(root2, "Show", "x.mp4"), "v")
	if err := lib.AddSource(context.Background(), root1, core.SourceFolder, nil); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	if err := lib.AddSource(context.Background(), root2, core.SourceNAS, nil); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	q := core.NewFakeIngestQueue(nil)
	if err := lib.Scan(context.Background(), q); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got := q.Enqueued(); got != 2 {
		t.Fatalf("two sources enqueued %d, want 2", got)
	}
	calls := q.Calls()
	kinds := map[core.SourceKind]bool{}
	paths := map[string]bool{}
	for _, c := range calls {
		kinds[c.Source.Kind] = true
		paths[c.Source.Path] = true
	}
	if !kinds[core.SourceFolder] || !kinds[core.SourceNAS] {
		t.Errorf("kinds seen = %v, want both folder and nas", kinds)
	}
	if !paths[filepath.ToSlash(filepath.Join(root1, "Show", "x.mp4"))] || !paths[filepath.ToSlash(filepath.Join(root2, "Show", "x.mp4"))] {
		t.Errorf("paths seen = %v, want both roots' canonical paths", paths)
	}
	if calls[0].File == calls[1].File {
		t.Errorf("both sources used file %q, want distinct absolute paths", calls[0].File)
	}
}

func TestScannerSameShowInTwoRootsDoesNotCollide(t *testing.T) {
	lib := newTestLibrary(t)
	root1, root2 := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(root1, "Show", "ep.mp4"), "v")
	writeFile(t, filepath.Join(root2, "Show", "ep.mp4"), "v")
	// Both roots use the same kind, so only the canonical path — not the
	// "Show/ep.mp4" key — tells the two files apart.
	if err := lib.AddSource(context.Background(), root1, core.SourceFolder, nil); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	if err := lib.AddSource(context.Background(), root2, core.SourceFolder, nil); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	q := core.NewFakeIngestQueue(nil)
	if err := lib.Scan(context.Background(), q); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got := q.Enqueued(); got != 2 {
		t.Fatalf("two same-kind roots enqueued %d, want 2 (no cross-root collision)", got)
	}
	seen := map[string]bool{}
	for _, c := range q.Calls() {
		seen[c.Source.Path] = true
	}
	if !seen[filepath.ToSlash(filepath.Join(root1, "Show", "ep.mp4"))] || !seen[filepath.ToSlash(filepath.Join(root2, "Show", "ep.mp4"))] {
		t.Errorf("enqueued paths = %v, want both roots' canonical paths", seen)
	}
}

func TestScannerToleratesMissingRoot(t *testing.T) {
	lib := newTestLibrary(t)
	if err := lib.AddSource(context.Background(), filepath.Join(t.TempDir(), "gone"), core.SourceFolder, nil); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	q := core.NewFakeIngestQueue(nil)
	if err := lib.Scan(context.Background(), q); err != nil {
		t.Fatalf("Scan on a missing root = %v, want nil (unmounted NAS tolerated)", err)
	}
	if q.Enqueued() != 0 {
		t.Fatalf("missing root enqueued %d, want 0", q.Enqueued())
	}
}

func TestSourcesCRUD(t *testing.T) {
	lib := newTestLibrary(t)
	ctx := context.Background()
	root := t.TempDir()

	if err := lib.AddSource(ctx, root, "weird", nil); err == nil {
		t.Error("AddSource with an invalid kind succeeded, want error")
	}

	if err := lib.AddSource(ctx, root, core.SourceNAS, []string{"b", "a", "b"}); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	srcs, err := lib.Sources(ctx)
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	if len(srcs) != 1 || srcs[0].Path != root || srcs[0].Kind != core.SourceNAS {
		t.Fatalf("Sources = %+v, want one nas source", srcs)
	}
	if want := []string{"a", "b"}; !reflect.DeepEqual(srcs[0].Allowlist, want) {
		t.Errorf("Allowlist = %v, want deduplicated and sorted %v", srcs[0].Allowlist, want)
	}

	// Re-adding the same path replaces kind and allowlist.
	if err := lib.AddSource(ctx, root, core.SourceFolder, nil); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	srcs, err = lib.Sources(ctx)
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	if len(srcs) != 1 || srcs[0].Kind != core.SourceFolder || len(srcs[0].Allowlist) != 0 {
		t.Errorf("Sources after re-add = %+v, want one folder source with empty allowlist", srcs)
	}

	if err := lib.RemoveSource(ctx, root); err != nil {
		t.Fatalf("RemoveSource: %v", err)
	}
	if err := lib.RemoveSource(ctx, root); err != nil {
		t.Fatalf("RemoveSource of a missing path = %v, want no-op", err)
	}
	srcs, err = lib.Sources(ctx)
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	if len(srcs) != 0 {
		t.Errorf("Sources after remove = %+v, want empty", srcs)
	}
}
