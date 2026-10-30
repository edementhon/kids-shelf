package kiwix

// Tests for library folders (folders.go): ZIM files kids-shelf serves where
// they are, found by looking through a folder the parent added.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edementhon/kids-shelf/internal/disk"
)

// zimBytes returns size bytes that start like a ZIM file whose header
// carries uuid (8-4-4-4-12 hex; "" leaves it zero).
func zimBytes(uuid string, size int) []byte {
	b := make([]byte, max(size, 24))
	binary.LittleEndian.PutUint32(b, zimMagic)
	binary.LittleEndian.PutUint16(b[4:], 6)
	binary.LittleEndian.PutUint16(b[6:], 3)
	if raw, err := hex.DecodeString(strings.ReplaceAll(uuid, "-", "")); err == nil && len(raw) == 16 {
		copy(b[8:24], raw)
	}
	return b
}

// putFile writes data at dir/rel (slash-separated), making the folders on
// the way, and returns the file's path.
func putFile(t *testing.T, dir, rel string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	return p
}

// folderBooks returns the installed rows from folder, by file path.
func folderBooks(t *testing.T, m *Manager, folder string) map[string]Installed {
	t.Helper()
	all, err := m.Installed()
	if err != nil {
		t.Fatalf("Installed: %v", err)
	}
	out := make(map[string]Installed)
	for _, in := range all {
		if in.Folder == folder {
			out[in.File] = in
		}
	}
	return out
}

func sortedKeys(m map[string]Installed) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// TestFolderScanRules: every regular .zim file, in any case, in the folder
// and the folders inside it goes on the shelf; anything else is left out:
// other files, .zim.done markers, split parts, hidden files and folders,
// recycle bins, Synology's @eaDir, a folder named like a ZIM, and
// kids-shelf's own library folder with its partial downloads when it lies
// inside. A link to a ZIM counts; a link to a folder is not followed.
func TestFolderScanRules(t *testing.T) {
	m, _, lib := newTestManager(t)
	// The folder around kids-shelf's own library folder, like a media root.
	dir := filepath.Dir(lib)
	want := []string{
		putFile(t, dir, "top.zim", []byte("1")),
		putFile(t, dir, "UPPER.ZIM", []byte("22")),
		putFile(t, dir, "My Wiki (2026).Zim", []byte("333")),
		putFile(t, dir, "sub/wiki_for_kids.zim", []byte("4444")),
		putFile(t, dir, "sub/deeper/deep.zim", []byte("55555")),
	}
	for _, skip := range []string{
		"notes.txt", "book.zim.done", "book.zimaa", "zim", ".hidden/secret.zim", "._top.zim",
		"#recycle/old.zim", "@eaDir/thumb.zim", "sub/.cache/cached.zim",
	} {
		putFile(t, dir, skip, []byte("x"))
	}
	putFile(t, lib, "downloaded.zim", []byte("x"))
	putFile(t, lib, ".partial/next.zim.part", []byte("x"))
	if err := os.MkdirAll(filepath.Join(dir, "folder.zim"), 0o755); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		elsewhere := t.TempDir()
		target := putFile(t, elsewhere, "target.zim", []byte("666666"))
		link := filepath.Join(dir, "link.zim")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		want = append(want, link)
		putFile(t, elsewhere, "more/far.zim", []byte("x"))
		if err := os.Symlink(filepath.Join(elsewhere, "more"), filepath.Join(dir, "linked folder")); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	m.OnChange = func() { calls++ }

	n, err := m.AddFolder(context.Background(), dir)
	if err != nil {
		t.Fatalf("AddFolder: %v", err)
	}
	if n != len(want) {
		t.Errorf("AddFolder found %d libraries, want %d", n, len(want))
	}
	if calls != 1 {
		t.Errorf("OnChange ran %d times for adding the folder, want once", calls)
	}
	books := folderBooks(t, m, dir)
	slices.Sort(want)
	if got := sortedKeys(books); !slices.Equal(got, want) {
		t.Fatalf("library folder books =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for file, in := range books {
		sum := sha256.Sum256([]byte(file))
		if in.ID != "folder:"+hex.EncodeToString(sum[:]) {
			t.Errorf("%s: id %q, want folder: and the sha-256 of its path", file, in.ID)
		}
		if p, err := m.FilePath(in); err != nil || p != file {
			t.Errorf("%s: FilePath = %q, %v, want the file where it is", file, p, err)
		}
		if !m.OnDisk(in) {
			t.Errorf("%s: OnDisk = false for a file that is there", file)
		}
		st, err := os.Stat(file)
		if err != nil || in.Size != st.Size() {
			t.Errorf("%s: size %d, want the file's %v (%v)", file, in.Size, st, err)
		}
	}
	// Until kiwix-serve reports a ZIM's own title, its file name stands in.
	for rel, title := range map[string]string{
		"My Wiki (2026).Zim":    "My Wiki (2026)",
		"sub/wiki_for_kids.zim": "wiki for kids",
	} {
		if got := books[filepath.Join(dir, filepath.FromSlash(rel))].Title; got != title {
			t.Errorf("%s: title %q, want %q", rel, got, title)
		}
	}
	folders, err := m.Folders()
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	if len(folders) != 1 || folders[0].Path != dir || folders[0].Books != len(want) || !folders[0].Reachable() {
		t.Errorf("Folders = %+v, want %s with %d books, reachable", folders, dir, len(want))
	}
}

// TestFolderLookAgainFollowsTheFolder: a later look adds new files and
// drops the ones gone from a folder that is there, and tells OnChange once
// per look, however many files changed (and not at all when none did).
func TestFolderLookAgainFollowsTheFolder(t *testing.T) {
	m, _, _ := newTestManager(t)
	dir := t.TempDir()
	a := putFile(t, dir, "a.zim", []byte("a"))
	b := putFile(t, dir, "b.zim", []byte("b"))
	ctx := context.Background()
	if n, err := m.AddFolder(ctx, dir); err != nil || n != 2 {
		t.Fatalf("AddFolder = %d, %v, want 2", n, err)
	}
	calls := 0
	m.OnChange = func() { calls++ }

	if err := m.ScanFolders(ctx); err != nil {
		t.Fatalf("ScanFolders: %v", err)
	}
	if calls != 0 {
		t.Errorf("OnChange ran %d times for a look that found nothing new, want 0", calls)
	}

	if err := os.Remove(a); err != nil {
		t.Fatal(err)
	}
	c := putFile(t, dir, "c.zim", []byte("c"))
	d := putFile(t, dir, "new/d.zim", []byte("d"))
	if err := m.ScanFolders(ctx); err != nil {
		t.Fatalf("ScanFolders: %v", err)
	}
	if calls != 1 {
		t.Errorf("OnChange ran %d times for one look with three changes, want once", calls)
	}
	want := []string{b, c, d}
	slices.Sort(want)
	if got := sortedKeys(folderBooks(t, m, dir)); !slices.Equal(got, want) {
		t.Errorf("books after the look = %v, want %v", got, want)
	}

	// "Look again" reports what it found.
	res, err := m.ScanFolder(ctx, dir)
	if err != nil {
		t.Fatalf("ScanFolder: %v", err)
	}
	if !res.Reachable || res.Added != 0 || res.Dropped != 0 || res.Books != 3 {
		t.Errorf("ScanFolder = %+v, want reachable, nothing added or dropped, 3 books", res)
	}
	if err := os.Remove(c); err != nil {
		t.Fatal(err)
	}
	putFile(t, dir, "e.zim", []byte("e"))
	if res, err = m.ScanFolder(ctx, dir); err != nil || res.Added != 1 || res.Dropped != 1 || res.Books != 3 {
		t.Errorf("ScanFolder = %+v, %v, want 1 added, 1 dropped, 3 books", res, err)
	}
	if calls != 2 {
		t.Errorf("OnChange ran %d times after the second change, want 2", calls)
	}

	// A newer copy under the same name: kiwix-serve must open it afresh.
	putFile(t, dir, "b.zim", []byte("a newer b"))
	if _, err := m.ScanFolder(ctx, dir); err != nil {
		t.Fatalf("ScanFolder: %v", err)
	}
	if calls != 3 {
		t.Errorf("OnChange ran %d times after b.zim changed, want 3", calls)
	}
	if got := folderBooks(t, m, dir)[b].Size; got != int64(len("a newer b")) {
		t.Errorf("b.zim size = %d, want the new copy's %d", got, len("a newer b"))
	}

	if _, err := m.ScanFolder(ctx, filepath.Join(dir, "new")); !errors.Is(err, ErrNoSuchFolder) {
		t.Errorf("ScanFolder of a folder not on the list = %v, want ErrNoSuchFolder", err)
	}
}

// TestFolderOutOfReachKeepsBooks: a folder kids-shelf cannot read (a drive
// that is not plugged in, a NAS that is off) keeps its books, which show
// as missing; an empty folder left behind by an unmounted drive counts as
// out of reach too. When it is back, the same books open again.
func TestFolderOutOfReachKeepsBooks(t *testing.T) {
	m, _, _ := newTestManager(t)
	parent := t.TempDir()
	dir := filepath.Join(parent, "nas")
	putFile(t, dir, "a.zim", []byte("a"))
	putFile(t, dir, "sub/b.zim", []byte("b"))
	ctx := context.Background()
	if n, err := m.AddFolder(ctx, dir); err != nil || n != 2 {
		t.Fatalf("AddFolder = %d, %v, want 2", n, err)
	}
	before := folderBooks(t, m, dir)
	calls := 0
	m.OnChange = func() { calls++ }

	check := func(when string, wantOnDisk bool) {
		t.Helper()
		if err := m.ScanFolders(ctx); err != nil {
			t.Fatalf("%s: ScanFolders: %v", when, err)
		}
		books := folderBooks(t, m, dir)
		if !slices.Equal(sortedKeys(books), sortedKeys(before)) {
			t.Fatalf("%s: books = %v, want the same %v", when, sortedKeys(books), sortedKeys(before))
		}
		for file, in := range books {
			if in.ID != before[file].ID {
				t.Errorf("%s: %s has id %s, want the same %s", when, file, in.ID, before[file].ID)
			}
			if m.OnDisk(in) != wantOnDisk {
				t.Errorf("%s: %s OnDisk = %v, want %v", when, file, !wantOnDisk, wantOnDisk)
			}
		}
		folders, err := m.Folders()
		if err != nil || len(folders) != 1 || folders[0].Reachable() != wantOnDisk {
			t.Errorf("%s: Folders = %+v, %v, want reachable %v", when, folders, err, wantOnDisk)
		}
		res, err := m.ScanFolder(ctx, dir)
		if err != nil || res.Reachable != wantOnDisk || res.Dropped != 0 || res.Added != 0 || res.Books != 2 {
			t.Errorf("%s: ScanFolder = %+v, %v, want reachable %v and the 2 books kept", when, res, err, wantOnDisk)
		}
		if calls != 0 {
			t.Errorf("%s: OnChange ran %d times, want 0 (nothing changed)", when, calls)
		}
	}

	away := filepath.Join(parent, "away")
	if err := os.Rename(dir, away); err != nil {
		t.Fatal(err)
	}
	check("folder gone", false)

	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	check("empty mount folder", false)

	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(away, dir); err != nil {
		t.Fatal(err)
	}
	check("folder back", true)
}

// TestFolderUnreadableSubfolderKeepsBooks: a folder inside that cannot be
// read keeps its books rather than dropping them as gone.
func TestFolderUnreadableSubfolderKeepsBooks(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a folder permission the test user cannot get past")
	}
	m, _, _ := newTestManager(t)
	dir := t.TempDir()
	a := putFile(t, dir, "a.zim", []byte("a"))
	b := putFile(t, dir, "locked/b.zim", []byte("b"))
	ctx := context.Background()
	if n, err := m.AddFolder(ctx, dir); err != nil || n != 2 {
		t.Fatalf("AddFolder = %d, %v, want 2", n, err)
	}
	locked := filepath.Dir(b)
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	if err := m.ScanFolders(ctx); err != nil {
		t.Fatalf("ScanFolders: %v", err)
	}
	want := []string{a, b}
	slices.Sort(want)
	if got := sortedKeys(folderBooks(t, m, dir)); !slices.Equal(got, want) {
		t.Errorf("books = %v, want both kept %v", got, want)
	}
}

// TestFolderBookRemoveRefused: a library from a folder cannot be removed
// one by one, and its file is never touched: Remove refuses before
// stopping kiwix-serve or deleting anything.
func TestFolderBookRemoveRefused(t *testing.T) {
	m, _, _ := newTestManager(t)
	dir := t.TempDir()
	file := putFile(t, dir, "a.zim", []byte("the family's own file"))
	if _, err := m.AddFolder(context.Background(), dir); err != nil {
		t.Fatalf("AddFolder: %v", err)
	}
	quiesced := false
	m.Quiesce = func(context.Context) (func(), error) {
		quiesced = true
		return func() {}, nil
	}
	in := folderBooks(t, m, dir)[file]
	if err := m.Remove(in.ID); !errors.Is(err, ErrFolderBook) {
		t.Fatalf("Remove of a folder book = %v, want ErrFolderBook", err)
	}
	if quiesced {
		t.Error("Remove stopped kiwix-serve for a removal it refuses")
	}
	if got, err := os.ReadFile(file); err != nil || string(got) != "the family's own file" {
		t.Errorf("the folder's file after Remove = %q, %v, want it untouched", got, err)
	}
	if _, ok := folderBooks(t, m, dir)[file]; !ok {
		t.Error("Remove dropped the folder book's row")
	}
}

// TestStopUsingFolderKeepsFiles: stopping a folder takes its books off the
// shelf, leaves every file where it was and keeps the downloads.
func TestStopUsingFolderKeepsFiles(t *testing.T) {
	m, q, lib := newTestManager(t)
	_, download := publishTestZIM(t, m, q, lib)
	dir := t.TempDir()
	files := map[string]string{
		putFile(t, dir, "a.zim", []byte("a")):     "a",
		putFile(t, dir, "sub/b.zim", []byte("b")): "b",
	}
	ctx := context.Background()
	if _, err := m.AddFolder(ctx, dir); err != nil {
		t.Fatalf("AddFolder: %v", err)
	}
	calls := 0
	m.OnChange = func() { calls++ }

	if err := m.RemoveFolder(dir); err != nil {
		t.Fatalf("RemoveFolder: %v", err)
	}
	if calls != 1 {
		t.Errorf("OnChange ran %d times, want once", calls)
	}
	all, err := m.Installed()
	if err != nil {
		t.Fatalf("Installed: %v", err)
	}
	if len(all) != 1 || all[0].Folder != "" {
		t.Errorf("installed after stopping the folder = %+v, want only the download", all)
	}
	if _, err := os.Stat(download); err != nil {
		t.Errorf("the download's file: %v", err)
	}
	for file, data := range files {
		if got, err := os.ReadFile(file); err != nil || string(got) != data {
			t.Errorf("%s after stopping its folder = %q, %v, want it untouched", file, got, err)
		}
	}
	if folders, err := m.Folders(); err != nil || len(folders) != 0 {
		t.Errorf("Folders = %+v, %v, want none", folders, err)
	}
	if err := m.RemoveFolder(dir); !errors.Is(err, ErrNoSuchFolder) {
		t.Errorf("RemoveFolder again = %v, want ErrNoSuchFolder", err)
	}
	// The folder can be used again later.
	if n, err := m.AddFolder(ctx, dir); err != nil || n != 2 {
		t.Errorf("AddFolder again = %d, %v, want 2", n, err)
	}
}

// TestFilePathBothKinds: a download's stored name resolves inside the
// library folder and one that would escape it is refused; a library
// folder book is the absolute path it stores.
func TestFilePathBothKinds(t *testing.T) {
	m, _, lib := newTestManager(t)
	if p, err := m.FilePath(Installed{File: "a.zim"}); err != nil || p != filepath.Join(lib, "a.zim") {
		t.Errorf("FilePath(download a.zim) = %q, %v, want %s", p, err, filepath.Join(lib, "a.zim"))
	}
	for _, bad := range []string{"", "../outside.zim", filepath.Join("..", "..", "outside.zim"), ".."} {
		if p, err := m.FilePath(Installed{File: bad}); err == nil {
			t.Errorf("FilePath(download %q) = %q, want a refusal", bad, p)
		}
	}
	// A download's name that looks absolute still stays inside the library.
	abs := filepath.Join(t.TempDir(), "elsewhere.zim")
	if p, err := m.FilePath(Installed{File: abs}); err != nil || !within(lib, p) {
		t.Errorf("FilePath(download %q) = %q, %v, want a path inside %s", abs, p, err, lib)
	}

	dir := t.TempDir()
	stored := filepath.Join(dir, "sub", "..", "Book (1).ZIM")
	if p, err := m.FilePath(Installed{File: stored, Folder: dir}); err != nil || p != filepath.Join(dir, "Book (1).ZIM") {
		t.Errorf("FilePath(folder book %q) = %q, %v, want it cleaned", stored, p, err)
	}
	if p, err := m.FilePath(Installed{File: "relative.zim", Folder: dir}); err == nil {
		t.Errorf("FilePath(folder book with a relative path) = %q, want a refusal", p)
	}
}

// TestStopUsingFolderDoesNotWaitForALook: stopping a folder never waits
// for a look through the folders (one stuck on a NAS that stopped
// answering, say), and a look that ends after the folder left the list
// puts nothing back on the shelf.
func TestStopUsingFolderDoesNotWaitForALook(t *testing.T) {
	m, _, _ := newTestManager(t)
	dir := t.TempDir()
	putFile(t, dir, "a.zim", []byte("a"))
	ctx := context.Background()
	if _, err := m.AddFolder(ctx, dir); err != nil {
		t.Fatalf("AddFolder: %v", err)
	}
	m.folderMu.Lock() // a look that is still going
	done := make(chan error, 1)
	go func() { done <- m.RemoveFolder(dir) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("RemoveFolder: %v", err)
		}
	case <-time.After(10 * time.Second):
		m.folderMu.Unlock()
		t.Fatal("RemoveFolder waited for the look through the folders")
	}
	putFile(t, dir, "b.zim", []byte("b"))
	res, err := m.scanFolder(ctx, dir)
	m.folderMu.Unlock()
	if err != nil || res.changed() || res.Books != 0 {
		t.Errorf("the look that ended after the stop = %+v, %v, want nothing changed", res, err)
	}
	if books := folderBooks(t, m, dir); len(books) != 0 {
		t.Errorf("books from the stopped folder = %v, want none", sortedKeys(books))
	}
}

// TestMigrationFromV1KeepsDownloads: a database from before library
// folders (only migration 1) keeps its downloads working after the
// upgrade, and library folders work on it.
func TestMigrationFromV1KeepsDownloads(t *testing.T) {
	s, q, lib := newStoreQueue(t)
	if err := s.Migrate(Migrations[0]); err != nil {
		t.Fatalf("migration 1: %v", err)
	}
	const file = "wikipedia_en_100_maxi_2026-08.zim"
	if _, err := s.DB().Exec(`INSERT INTO installed (id, name, title, language, file, size, installed_at)
		VALUES ('urn:uuid:old', 'wikipedia_en_100', 'Wikipedia', 'eng', ?, 5, '2026-08-07T00:00:00Z')`, file); err != nil {
		t.Fatalf("seed a version 1 row: %v", err)
	}
	m, err := NewManager(s, q, disk.NewReservations(q), lib)
	if err != nil {
		t.Fatalf("NewManager on a version 1 database: %v", err)
	}
	final := putFile(t, lib, file, []byte("12345"))

	all, err := m.Installed()
	if err != nil {
		t.Fatalf("Installed: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("installed = %+v, want the old row", all)
	}
	in := all[0]
	if in.ID != "urn:uuid:old" || in.Name != "wikipedia_en_100" || in.Title != "Wikipedia" || in.Language != "eng" ||
		in.File != file || in.Size != 5 || in.Folder != "" || in.InstalledAt.IsZero() {
		t.Errorf("old row after the upgrade = %+v, want it as it was, as a download", in)
	}
	if p, err := m.FilePath(in); err != nil || p != final {
		t.Errorf("FilePath(old row) = %q, %v, want %s", p, err, final)
	}
	if !m.OnDisk(in) {
		t.Error("OnDisk(old row) = false with its file in the library")
	}

	dir := t.TempDir()
	putFile(t, dir, "x.zim", []byte("x"))
	if n, err := m.AddFolder(context.Background(), dir); err != nil || n != 1 {
		t.Errorf("AddFolder on the upgraded database = %d, %v, want 1", n, err)
	}
	// The download is still removed like one: file and row.
	if err := m.Remove("urn:uuid:old"); err != nil {
		t.Fatalf("Remove(old row): %v", err)
	}
	if _, err := os.Stat(final); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the old row's file after Remove: %v, want it deleted", err)
	}
}

// TestAddFolderRefusals: a folder that is not there, a file, kids-shelf's
// own library folder or a folder inside it, a folder already on the list
// and a folder inside or around one on the list are refused, and nothing
// is added.
func TestAddFolderRefusals(t *testing.T) {
	m, _, lib := newTestManager(t)
	base := t.TempDir()
	dir := filepath.Join(base, "kiwix")
	putFile(t, dir, "a.zim", []byte("a"))
	putFile(t, dir, "inner/b.zim", []byte("b"))
	file := putFile(t, base, "file.zim", []byte("f"))
	putFile(t, lib, ".partial/x.zim.part", []byte("x"))
	ctx := context.Background()

	if _, err := m.AddFolder(ctx, "relative/kiwix"); err == nil {
		t.Error("AddFolder of a relative path succeeded")
	}
	for _, tc := range []struct {
		name, path string
		want       error
	}{
		{"a folder that is not there", filepath.Join(base, "nope"), ErrFolderNotFound},
		{"a file", file, ErrNotAFolder},
		{"the library folder", lib, ErrLibraryFolder},
		{"a folder inside the library folder", filepath.Join(lib, ".partial"), ErrLibraryFolder},
	} {
		if _, err := m.AddFolder(ctx, tc.path); !errors.Is(err, tc.want) {
			t.Errorf("AddFolder(%s) = %v, want %v", tc.name, err, tc.want)
		}
	}
	if n, err := m.AddFolder(ctx, dir); err != nil || n != 2 {
		t.Fatalf("AddFolder(%s) = %d, %v, want 2", dir, n, err)
	}
	for _, tc := range []struct {
		name, path string
		want       error
	}{
		{"the same folder", dir, ErrFolderAdded},
		{"the same folder with a trailing separator", dir + string(filepath.Separator), ErrFolderAdded},
		{"a folder inside it", filepath.Join(dir, "inner"), ErrFolderNested},
		{"the folder around it", base, ErrFolderNested},
	} {
		if _, err := m.AddFolder(ctx, tc.path); !errors.Is(err, tc.want) {
			t.Errorf("AddFolder(%s) = %v, want %v", tc.name, err, tc.want)
		}
	}
	if runtime.GOOS != "windows" {
		toLib := filepath.Join(t.TempDir(), "link to the library")
		if err := os.Symlink(lib, toLib); err != nil {
			t.Fatal(err)
		}
		if _, err := m.AddFolder(ctx, toLib); !errors.Is(err, ErrLibraryFolder) {
			t.Errorf("AddFolder(a link to the library folder) = %v, want ErrLibraryFolder", err)
		}
		toDir := filepath.Join(t.TempDir(), "link to the folder")
		if err := os.Symlink(dir, toDir); err != nil {
			t.Fatal(err)
		}
		if _, err := m.AddFolder(ctx, toDir); !errors.Is(err, ErrFolderAdded) {
			t.Errorf("AddFolder(a link to a folder on the list) = %v, want ErrFolderAdded", err)
		}
	}
	folders, err := m.Folders()
	if err != nil || len(folders) != 1 || folders[0].Path != dir {
		t.Errorf("Folders = %+v, %v, want only %s", folders, err, dir)
	}
	if got := len(folderBooks(t, m, dir)); got != 2 {
		t.Errorf("books from the folder = %d, want 2", got)
	}
}

// TestRecordServedKeepsFolderBookDetails: what kiwix-serve reports about a
// folder book (the ZIM's own name, title and language) replaces the file
// name stand-ins and stays while the folder is out of reach; downloads
// keep the catalog's words, and an empty value never wipes a known one.
func TestRecordServedKeepsFolderBookDetails(t *testing.T) {
	m, q, lib := newTestManager(t)
	e, _ := publishTestZIM(t, m, q, lib)
	parent := t.TempDir()
	dir := filepath.Join(parent, "wikis")
	file := putFile(t, dir, "My_Wiki (2026).zim", []byte("w"))
	ctx := context.Background()
	if _, err := m.AddFolder(ctx, dir); err != nil {
		t.Fatalf("AddFolder: %v", err)
	}
	in := folderBooks(t, m, dir)[file]
	if in.Title != "My Wiki (2026)" || in.Name != "My_Wiki (2026)" || in.Language != "" {
		t.Errorf("folder book before kiwix-serve reported it = %+v, want its file name as title and name", in)
	}

	if err := m.RecordServed(map[string]ServedBook{
		in.ID: {Name: "vikidia_scn_all", Title: "Vikidia", Language: "scn"},
		e.ID:  {Name: "other_name", Title: "Other title", Language: "fra"},
	}); err != nil {
		t.Fatalf("RecordServed: %v", err)
	}
	if err := m.RecordServed(map[string]ServedBook{in.ID: {}}); err != nil {
		t.Fatalf("RecordServed with nothing known: %v", err)
	}
	if err := os.Rename(dir, filepath.Join(parent, "away")); err != nil {
		t.Fatal(err)
	}
	if err := m.ScanFolders(ctx); err != nil {
		t.Fatalf("ScanFolders: %v", err)
	}
	all, err := m.Installed()
	if err != nil {
		t.Fatalf("Installed: %v", err)
	}
	for _, got := range all {
		switch got.ID {
		case in.ID:
			if got.Name != "vikidia_scn_all" || got.Title != "Vikidia" || got.Language != "scn" {
				t.Errorf("folder book = %+v, want the ZIM's own name, title and language kept", got)
			}
		case e.ID:
			if got.Name != e.Name || got.Title != e.Title || got.Language != e.Language {
				t.Errorf("download = %+v, want the catalog's %q / %q / %q", got, e.Name, e.Title, e.Language)
			}
		}
	}
}

// TestFolderBookServedAs: a book is found in kiwix-serve's feed by the
// UUID in its file's header, whatever the file is called; a file the
// feed does not list, or that is not a ZIM, is not found.
func TestFolderBookServedAs(t *testing.T) {
	m, _, _ := newTestManager(t)
	dir := t.TempDir()
	const served = "975eb51a-1c3d-5383-8f3f-2db8f9cb8798"
	wiki := putFile(t, dir, "Whatever Name.zim", zimBytes(served, 100))
	other := putFile(t, dir, "other.zim", zimBytes("11111111-2222-3333-4444-555555555555", 100))
	notZIM := putFile(t, dir, "text.zim", bytes.Repeat([]byte("plain text "), 10))
	if _, err := m.AddFolder(context.Background(), dir); err != nil {
		t.Fatalf("AddFolder: %v", err)
	}
	feed := map[string]ServedBook{served: {UUID: served, Book: "my_wiki_(2026)"}}
	books := folderBooks(t, m, dir)
	if b, ok := m.ServedAs(books[wiki], feed); !ok || b.Book != "my_wiki_(2026)" {
		t.Errorf("ServedAs(%s) = %+v, %v, want the feed's my_wiki_(2026)", wiki, b, ok)
	}
	for _, f := range []string{other, notZIM} {
		if b, ok := m.ServedAs(books[f], feed); ok {
			t.Errorf("ServedAs(%s) = %+v, want not served", f, b)
		}
	}
}
