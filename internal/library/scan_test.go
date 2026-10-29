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
	// changes (the show directory's does not), so a shortcut that returns at
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
	// Both roots use the same kind, so only the canonical path, not the
	// "Show/ep.mp4" key, tells the two files apart.
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

// TestScannerLooseFilesFormRootShow: videos placed straight in an added
// folder were silently ignored (one show had to be one subfolder). They now
// join a show named after the folder itself, with the usual skip rules and
// the allowlist naming that show.
func TestScannerLooseFilesFormRootShow(t *testing.T) {
	lib := newTestLibrary(t)
	root := filepath.Join(t.TempDir(), "Cartoons")
	writeFile(t, filepath.Join(root, "Cartoon.mp4"), "v")
	writeFile(t, filepath.Join(root, "#skip.mp4"), "v")
	writeFile(t, filepath.Join(root, ".hidden.mp4"), "v")
	writeFile(t, filepath.Join(root, "notes.txt"), "not a video")
	writeFile(t, filepath.Join(root, "Bluey", "ep1.mp4"), "v")
	if err := lib.AddSource(context.Background(), root, core.SourceFolder, nil); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	q := core.NewFakeIngestQueue(nil)
	n, err := lib.ScanNew(context.Background(), q)
	if err != nil {
		t.Fatalf("ScanNew: %v", err)
	}
	if n != 2 || q.Enqueued() != 2 {
		t.Fatalf("scan handed over %d (queue saw %d), want 2", n, q.Enqueued())
	}
	var loose *core.EnqueueCall
	for _, c := range q.Calls() {
		if c.Source.Path == filepath.ToSlash(filepath.Join(root, "Cartoon.mp4")) {
			loose = &c
		}
	}
	if loose == nil {
		t.Fatalf("the loose video was not enqueued: %+v", q.Calls())
	}
	if loose.Source.Show != "Cartoons" || loose.Source.Title != "Cartoon" {
		t.Errorf("loose video show/title = %q/%q, want Cartoons/Cartoon", loose.Source.Show, loose.Source.Title)
	}

	// Nothing changed: nothing new, and ScanNew says so.
	if n, err := lib.ScanNew(context.Background(), q); err != nil || n != 0 {
		t.Fatalf("second ScanNew = %d, %v; want 0 new", n, err)
	}

	// A later loose file is found once the root's mtime changes.
	writeFile(t, filepath.Join(root, "Second.mkv"), "v")
	bumpMtime(t, root)
	if n, err := lib.ScanNew(context.Background(), q); err != nil || n != 1 {
		t.Fatalf("ScanNew after a new loose file = %d, %v; want 1", n, err)
	}

	// An allowlist limits loose files by the root show's name.
	lib2 := newTestLibrary(t)
	if err := lib2.AddSource(context.Background(), root, core.SourceFolder, []string{"Bluey"}); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	q2 := core.NewFakeIngestQueue(nil)
	if err := lib2.Scan(context.Background(), q2); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if q2.Enqueued() != 1 || q2.Calls()[0].Source.Show != "Bluey" {
		t.Errorf("allowlist [Bluey] enqueued %+v, want only Bluey/ep1.mp4", q2.Calls())
	}
}

func TestLooseShowName(t *testing.T) {
	for in, want := range map[string]string{
		"/media/Kids":  "Kids",
		"/media/Kids/": "Kids",
		"/":            "Videos",
	} {
		if got := looseShowName(in); got != want {
			t.Errorf("looseShowName(%q) = %q, want %q", in, got, want)
		}
	}
}

// retryQueue is an ingest queue with the real queue's failure rule:
// Enqueue returns the live job of a source, and only a failed job is
// replaced by a new one.
type retryQueue struct {
	next   int
	src    map[string]core.Source
	failed map[string]bool
	ep     map[string]core.EpisodeID
	calls  []string // job ids handed out by Enqueue, in order
}

func newRetryQueue() *retryQueue {
	return &retryQueue{src: map[string]core.Source{}, failed: map[string]bool{}, ep: map[string]core.EpisodeID{}}
}

func (q *retryQueue) Enqueue(ctx context.Context, file string, src core.Source) (string, error) {
	for id, s := range q.src {
		if s.Kind == src.Kind && s.Path == src.Path && !q.failed[id] {
			q.calls = append(q.calls, id)
			return id, nil
		}
	}
	q.next++
	id := "job-" + string(rune('0'+q.next))
	q.src[id] = src
	q.calls = append(q.calls, id)
	return id, nil
}

func (q *retryQueue) IngestStatus(ctx context.Context, id string) (core.EpisodeID, bool, error) {
	if q.failed[id] {
		return "", true, nil
	}
	if ep, ok := q.ep[id]; ok {
		return ep, true, nil
	}
	return "", false, nil
}

// TestScannerRetriesChangedFailedFile: a file whose conversion failed was
// remembered forever, so replacing it with a good copy never brought it
// back. Now a changed size or modification time gets a new job once the
// old one failed, even when the folder's own mtime did not change (a copy
// over the old file), and an unchanged broken file is left alone.
func TestScannerRetriesChangedFailedFile(t *testing.T) {
	ctx := context.Background()
	lib := newTestLibrary(t)
	root := t.TempDir()
	show := filepath.Join(root, "Broken")
	file := filepath.Join(show, "Bad.mp4")
	writeFile(t, file, "broken")
	if err := lib.AddSource(ctx, root, core.SourceFolder, nil); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	q := newRetryQueue()
	if n, err := lib.ScanNew(ctx, q); err != nil || n != 1 {
		t.Fatalf("first ScanNew = %d, %v; want 1", n, err)
	}
	q.failed["job-1"] = true

	// Unchanged and failed: not retried.
	if n, err := lib.ScanNew(ctx, q); err != nil || n != 0 {
		t.Fatalf("ScanNew of an unchanged failed file = %d, %v; want 0", n, err)
	}

	// The parent copies a good file over it; the folder's mtime stays.
	dirInfo, err := os.Stat(show)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, file, "a good copy")
	if err := os.Chtimes(show, dirInfo.ModTime(), dirInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	if n, err := lib.ScanNew(ctx, q); err != nil || n != 1 {
		t.Fatalf("ScanNew of a changed failed file = %d, %v; want 1", n, err)
	}
	if last := q.calls[len(q.calls)-1]; last != "job-2" {
		t.Fatalf("changed file got job %q, want a new job-2", last)
	}

	// Changed while its job still runs: left alone, and retried once
	// that job fails.
	future := time.Now().Add(3 * time.Hour)
	if err := os.Chtimes(file, future, future); err != nil {
		t.Fatal(err)
	}
	if n, err := lib.ScanNew(ctx, q); err != nil || n != 0 {
		t.Fatalf("ScanNew while the job runs = %d, %v; want 0", n, err)
	}
	q.failed["job-2"] = true
	if n, err := lib.ScanNew(ctx, q); err != nil || n != 1 {
		t.Fatalf("ScanNew after the running job failed = %d, %v; want 1", n, err)
	}
	if last := q.calls[len(q.calls)-1]; last != "job-3" {
		t.Fatalf("retry got job %q, want job-3", last)
	}
}

// TestScannerNeverReaddsRemovedEpisode: a file whose job finished with an
// episode the parent later removed is not enqueued again when it changes.
func TestScannerNeverReaddsRemovedEpisode(t *testing.T) {
	ctx := context.Background()
	lib := newTestLibrary(t)
	root := t.TempDir()
	file := filepath.Join(root, "Show", "ep.mp4")
	writeFile(t, file, "v")
	if err := lib.AddSource(ctx, root, core.SourceFolder, nil); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	q := newRetryQueue()
	if n, err := lib.ScanNew(ctx, q); err != nil || n != 1 {
		t.Fatalf("first ScanNew = %d, %v; want 1", n, err)
	}
	q.ep["job-1"] = "ep-1" // finished; no episode row (removed since)
	writeFile(t, file, "changed")
	future := time.Now().Add(3 * time.Hour)
	if err := os.Chtimes(file, future, future); err != nil {
		t.Fatal(err)
	}
	if n, err := lib.ScanNew(ctx, q); err != nil || n != 0 {
		t.Fatalf("ScanNew of a changed, finished file = %d, %v; want 0", n, err)
	}
}

func TestCountVideos(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "loose.mp4"), "v")
	writeFile(t, filepath.Join(root, "Show", "a.mkv"), "v")
	writeFile(t, filepath.Join(root, "Show", "Season 1", "b.MP4"), "v")
	writeFile(t, filepath.Join(root, "Show", "notes.txt"), "v")
	writeFile(t, filepath.Join(root, "#old", "c.mp4"), "v")
	writeFile(t, filepath.Join(root, ".hidden", "d.mp4"), "v")
	if n, err := CountVideos(context.Background(), root); err != nil || n != 3 {
		t.Fatalf("CountVideos = %d, %v; want 3", n, err)
	}
	if n, err := CountVideos(context.Background(), t.TempDir()); err != nil || n != 0 {
		t.Fatalf("CountVideos(empty) = %d, %v; want 0", n, err)
	}
	if _, err := CountVideos(context.Background(), filepath.Join(root, "nope")); err == nil {
		t.Fatal("CountVideos of a missing folder: want an error")
	}
}
