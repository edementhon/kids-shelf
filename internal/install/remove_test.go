//go:build linux

package install

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edementhon/kids-shelf/internal/launch"
	"github.com/edementhon/kids-shelf/internal/store"
	"github.com/edementhon/kids-shelf/internal/update"
)

// removeFixture lays out an installed kids-shelf: an install root with the
// launcher, a version build, the icon and install.lock; a data directory
// with the journal and the database next to instance.lock; and a media
// folder when asked. The install root is a sibling of the data dir, so
// neither sweep can accidentally cover the other.
func removeFixture(t *testing.T, withMedia bool) (root, data, media string) {
	t.Helper()
	root = t.TempDir()
	data = t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home) // controlled for the media resolution and the shortcuts

	for _, f := range []struct {
		path string
		data []byte
	}{
		{filepath.Join(root, "kids-shelf"), []byte("launcher")},
		{update.VersionBinary(root, "v1.0.0"), []byte("child")},
		{filepath.Join(root, "kids-shelf.png"), []byte("icon")},
		{filepath.Join(root, launch.InstallLockName), nil},
		{filepath.Join(data, "instance.lock"), nil},
		{update.StatePath(data), []byte(`{"current":"v1.0.0"}` + "\n")},
		{filepath.Join(data, "kids-shelf.db"), []byte("db")},
		{filepath.Join(data, "kids-shelf.db.lock"), nil},
	} {
		if f.data == nil {
			if err := os.WriteFile(f.path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.path, f.data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if withMedia {
		media = filepath.Join(home, "Videos", mediaDirName)
		if err := os.MkdirAll(filepath.Join(media, "episodes"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(media, "episodes", "one.mp4"), []byte("video"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root, data, media
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func wantEntries(t *testing.T, dir string, want ...string) {
	t.Helper()
	got := dirEntries(t, dir)
	if len(got) != len(want) {
		t.Fatalf("%s has %v, want %v", dir, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s has %v, want %v", dir, got, want)
			return
		}
	}
}

// TestRemoveKeepsLockFiles: without delete-data the install root is emptied
// except for install.lock and the data directory is untouched; both lock
// files survive and are never unlinked (review round 3 R5).
func TestRemoveKeepsLockFiles(t *testing.T) {
	root, data, _ := removeFixture(t, false)
	o := RemoveOptions{InstallDir: root, DataDir: data}

	if err := Remove(o); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	wantEntries(t, root, launch.InstallLockName)
	for _, keep := range []string{"instance.lock", "update", "kids-shelf.db"} {
		if _, err := os.Stat(filepath.Join(data, keep)); err != nil {
			t.Errorf("%s missing from the data dir after a delete-data=false removal: %v", keep, err)
		}
	}
}

// TestRemoveDeleteData: when the parent asked for it, the data directory is
// emptied except for the instance lock, and the app-owned default media
// folder goes too.
func TestRemoveDeleteData(t *testing.T) {
	root, data, media := removeFixture(t, true)
	o := RemoveOptions{InstallDir: root, DataDir: data, MediaDir: media, DeleteData: true}

	if err := Remove(o); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	wantEntries(t, root, launch.InstallLockName)
	wantEntries(t, data, "instance.lock", "kids-shelf.db.lock")
	if _, err := os.Stat(media); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("default media folder still exists after delete-data removal (stat err %v)", err)
	}
}

// TestRemoveDeleteDataLeavesInPlaceOriginals pins the in-place guarantee:
// "also delete videos and settings" deletes the app-owned default media
// folder (converted copies, thumbnails, YouTube downloads) and the data
// directory, but an original folder/NAS file an in-place episode records -
// which lives outside the media folder - is never touched, even though the
// episode row that named it is gone with the database.
func TestRemoveDeleteDataLeavesInPlaceOriginals(t *testing.T) {
	root, data, media := removeFixture(t, true)
	home := os.Getenv("HOME") // the fixture's controlled home
	originalDir := filepath.Join(home, "NAS", "Bluey")
	original := filepath.Join(originalDir, "S01E01.mp4")
	if err := os.MkdirAll(originalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(original, []byte("original bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	o := RemoveOptions{InstallDir: root, DataDir: data, MediaDir: media, DeleteData: true}
	if err := Remove(o); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(media); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("app-owned default media folder still exists after delete-data removal (stat err %v)", err)
	}
	b, err := os.ReadFile(original)
	if err != nil {
		t.Fatalf("in-place original was deleted by the removal: %v", err)
	}
	if string(b) != "original bytes" {
		t.Errorf("in-place original was modified: %q", b)
	}
}

// TestRemoveImportedFoldersUntouched: a custom media directory is never
// removed, even with delete-data ("never imported folders").
func TestRemoveImportedFoldersUntouched(t *testing.T) {
	root, data, _ := removeFixture(t, false)
	home := os.Getenv("HOME") // the fixture's controlled home
	custom := filepath.Join(home, "Videos", "MyImportedMovies")
	if err := os.MkdirAll(filepath.Join(custom, "episodes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(custom, "episodes", "two.mp4"), []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}

	o := RemoveOptions{InstallDir: root, DataDir: data, MediaDir: custom, DeleteData: true}
	if err := Remove(o); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(custom); err != nil {
		t.Errorf("custom media folder was removed: %v", err)
	}
}

// TestRemoveMediaFallbackIntoDataStays: when the default media folder would
// fall back to the data directory (no Videos folder), the fallback is never
// "deleted" - removal only ever empties the data dir's contents, and the
// instance lock keeps the directory itself.
func TestRemoveMediaFallbackIntoDataStays(t *testing.T) {
	root, data, _ := removeFixture(t, false)
	// The fixture's home has no Videos folder, so the default media
	// resolution falls back into the data dir, which removal never deletes
	// as a folder.

	o := RemoveOptions{InstallDir: root, DataDir: data, DeleteData: true}
	if err := Remove(o); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	wantEntries(t, data, "instance.lock", "kids-shelf.db.lock")
}

// TestRemoveNestedInstallRoot is review round 3 R5's race: the default
// Linux/macOS install root lives inside the data directory, so a recursive
// data sweep must leave the install root (and its install.lock) behind, not
// unlink it indirectly. Review round 4 finding 10 extends the guarantee to
// any depth: for <data>/nested/app the sweep must preserve every ancestor
// directory of the root and empty them selectively.
func TestRemoveNestedInstallRoot(t *testing.T) {
	for _, tc := range []struct {
		name string
		rel  string
	}{
		{"immediate child", "app"},
		{"nested root", filepath.Join("nested", "app")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := t.TempDir()
			root := filepath.Join(data, tc.rel)
			for _, f := range []string{
				filepath.Join(root, "kids-shelf"),
				update.VersionBinary(root, "v1.0.0"),
				filepath.Join(root, launch.InstallLockName),
				filepath.Join(data, "instance.lock"),
				filepath.Join(data, "kids-shelf.db"),
				filepath.Join(data, "kids-shelf.db.lock"),
			} {
				if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			o := RemoveOptions{InstallDir: root, DataDir: data, DeleteData: true}
			if err := Remove(o); err != nil {
				t.Fatalf("Remove: %v", err)
			}
			first := strings.Split(tc.rel, string(filepath.Separator))[0]
			want := []string{first, "instance.lock", "kids-shelf.db.lock"}
			slices.Sort(want)
			wantEntries(t, data, want...)
			if parent := filepath.Dir(root); parent != data {
				wantEntries(t, parent, "app")
			}
			wantEntries(t, root, launch.InstallLockName)
		})
	}
}

// TestRemoveReadsRemoveRequest: the durable remove.json written by the app
// before it exits decides delete-data, so a request survives whatever
// flags reached the remover (review round 3, section 5 item 7).
func TestRemoveReadsRemoveRequest(t *testing.T) {
	root, data, _ := removeFixture(t, false)
	if err := WriteRemoveRequest(data, true, ""); err != nil {
		t.Fatal(err)
	}

	// DeleteData is false in the options; the durable request wins.
	o := RemoveOptions{InstallDir: root, DataDir: data, DeleteData: false}
	if err := Remove(o); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	wantEntries(t, data, "instance.lock", "kids-shelf.db.lock")
	wantEntries(t, root, launch.InstallLockName)
}

// TestRemoveRequestCarriesMediaDir: the durable request also records the
// configured media directory, so the remover's "delete only the app-owned
// default" decision does not depend on flags or on opening the database it
// is about to delete.
func TestRemoveRequestCarriesMediaDir(t *testing.T) {
	root, data, _ := removeFixture(t, false)
	home := os.Getenv("HOME") // the fixture's controlled home
	custom := filepath.Join(home, "Videos", "CustomMedia")
	if err := os.MkdirAll(filepath.Join(custom, "episodes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(custom, "episodes", "three.mp4"), []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The options carry no MediaDir; the request does. DeleteData is false
	// in the options; the request's true wins.
	if err := WriteRemoveRequest(data, true, custom); err != nil {
		t.Fatal(err)
	}
	if err := Remove(RemoveOptions{InstallDir: root, DataDir: data}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	wantEntries(t, data, "instance.lock", "kids-shelf.db.lock")
	if _, err := os.Stat(custom); err != nil {
		t.Errorf("custom media folder from the request was removed: %v", err)
	}
}

// TestRemoveBusyLock: with a launcher still holding a lock the remover
// waits (bounded) and then answers the typed busy error instead of deleting
// anything.
func TestRemoveBusyLock(t *testing.T) {
	root, data, _ := removeFixture(t, false)
	lock, err := launch.LockInstall(root, 0)
	if err != nil {
		t.Fatalf("LockInstall: %v", err)
	}
	defer lock.Close()

	old := removeLockWait
	removeLockWait = 200 * time.Millisecond
	t.Cleanup(func() { removeLockWait = old })

	err = Remove(RemoveOptions{InstallDir: root, DataDir: data, DeleteData: true})
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("Remove with a held lock = %v, want ErrBusy", err)
	}
	if _, serr := os.Stat(filepath.Join(root, "kids-shelf")); serr != nil {
		t.Errorf("removal started despite the busy lock: %v", serr)
	}
}

// TestHandOffArgs: the supervisor's hand-off copies its binary to the temp
// dir and starts it with the --remove role and the directories spelled out;
// delete-data comes from the durable request when it is there.
func TestHandOffArgs(t *testing.T) {
	root, data, _ := removeFixture(t, false)
	t.Setenv("TMPDIR", t.TempDir()) // the remover copy must not outlive the test
	if err := WriteRemoveRequest(data, true, ""); err != nil {
		t.Fatal(err)
	}

	got := make(chan []string, 1)
	old := startRemover
	startRemover = func(cmd *exec.Cmd) error {
		got <- append([]string{cmd.Path}, cmd.Args[1:]...)
		return nil
	}
	t.Cleanup(func() { startRemover = old })

	if err := HandOff(RemoveOptions{InstallDir: root, DataDir: data}); err != nil {
		t.Fatalf("HandOff: %v", err)
	}
	args := <-got
	want := []string{
		"--remove", "--install", root, "--data", data, "--delete-data",
	}
	if len(args) != len(want)+1 {
		t.Fatalf("remover args = %v, want %v", args, want)
	}
	for i, a := range want {
		if args[i+1] != a {
			t.Errorf("remover arg %d = %q, want %q (all args %v)", i+1, args[i+1], a, args)
		}
	}

	// The temp copy is real, executable bytes of the running binary: the
	// remover is started by path, so a 0600 copy (os.CreateTemp's mode)
	// fails with "permission denied" on Linux and macOS.
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	sameBytes(t, exe, args[0])
	if runtime.GOOS != "windows" {
		st, err := os.Stat(args[0])
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm()&0o100 == 0 {
			t.Errorf("remover copy mode = %v, want executable", st.Mode().Perm())
		}
	}
}

// TestRemoveWaitsForDatabaseOwner pins review round 4 finding 5: the
// remover takes the database ownership lock after the instance lock, so an
// orphaned child that still holds the database cannot have it deleted
// underneath it; the sweep then keeps the lock file itself.
func TestRemoveWaitsForDatabaseOwner(t *testing.T) {
	root, data, _ := removeFixture(t, false)
	unlock, err := store.LockDB(update.DatabasePath(data), 0)
	if err != nil {
		t.Fatalf("LockDB: %v", err)
	}
	defer unlock()

	old := removeDBLockWait
	removeDBLockWait = 200 * time.Millisecond
	t.Cleanup(func() { removeDBLockWait = old })

	err = Remove(RemoveOptions{InstallDir: root, DataDir: data, DeleteData: true})
	if err == nil {
		t.Fatal("Remove succeeded while another owner holds the database lock")
	}
	if _, serr := os.Stat(filepath.Join(root, "kids-shelf")); serr != nil {
		t.Errorf("removal started despite the held database lock: %v", serr)
	}
}

// TestRemoveAllRetryBoundedWindow pins review round 4 finding 11's retry
// bound: a persistently mapped launcher is retried for only
// launcherReplaceWindow before its deletion error surfaces.
func TestRemoveAllRetryBoundedWindow(t *testing.T) {
	oldAll, oldRetry := removeAll, deleteRetryable
	oldWindow, oldPoll := launcherReplaceWindow, launcherReplacePoll
	launcherReplaceWindow = 250 * time.Millisecond
	launcherReplacePoll = 10 * time.Millisecond
	t.Cleanup(func() {
		removeAll, deleteRetryable = oldAll, oldRetry
		launcherReplaceWindow, launcherReplacePoll = oldWindow, oldPoll
	})
	deleteRetryable = func(error) bool { return true }
	removeAll = func(string) error { return errors.New("simulated sharing violation") }

	start := time.Now()
	err := removeAllRetry(t.TempDir())
	if err == nil {
		t.Fatal("removeAllRetry on a permanently mapped path succeeded, want an error")
	}
	if elapsed := time.Since(start); elapsed < launcherReplaceWindow/2 {
		t.Errorf("retry stopped after %v, want the bounded window to be honored", elapsed)
	}
}

// TestRemoveAllRetryRecovers: a transient mapping clears within the window
// and the deletion completes.
func TestRemoveAllRetryRecovers(t *testing.T) {
	oldAll, oldRetry := removeAll, deleteRetryable
	t.Cleanup(func() { removeAll, deleteRetryable = oldAll, oldRetry })
	deleteRetryable = func(error) bool { return true }

	dir := t.TempDir()
	attempts := 0
	removeAll = func(path string) error {
		attempts++
		if attempts <= 2 {
			return errors.New("still mapped")
		}
		return os.RemoveAll(path)
	}
	if err := removeAllRetry(dir); err != nil {
		t.Fatalf("removeAllRetry after the mapping cleared: %v", err)
	}
	if attempts != 3 {
		t.Errorf("removal attempts = %d, want 3 (two failures, one success)", attempts)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("path still exists after the recovered removal (stat err %v)", err)
	}
}

// TestRemoveKeepsEntryPointOnFailedRootRemoval pins review round 4 finding
// 11's order: the install root is emptied before the shortcuts and the
// uninstall entry are touched, so a removal that cannot delete a
// still-mapped launcher still leaves the normal removal entry point in
// place and the machine can be removed again.
func TestRemoveKeepsEntryPointOnFailedRootRemoval(t *testing.T) {
	root, data, _ := removeFixture(t, false)
	menu := filepath.Join(os.Getenv("HOME"), ".local", "share", "applications", "kids-shelf.desktop")
	if err := os.MkdirAll(filepath.Dir(menu), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(menu, []byte("[Desktop Entry]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldAll, oldRetry := removeAll, deleteRetryable
	t.Cleanup(func() { removeAll, deleteRetryable = oldAll, oldRetry })
	deleteRetryable = func(error) bool { return false } // permanent: no retry window
	removeAll = func(string) error { return errors.New("permanently mapped") }

	if err := Remove(RemoveOptions{InstallDir: root, DataDir: data, DeleteData: true}); err == nil {
		t.Fatal("Remove succeeded despite the undeletable root, want an error")
	}
	if _, err := os.Stat(menu); err != nil {
		t.Errorf("menu entry gone after a failed root removal (stat err %v); the entry point must survive", err)
	}
	if _, err := os.Stat(filepath.Join(root, "kids-shelf")); err != nil {
		t.Errorf("launcher removed despite the persistent failure (stat err %v)", err)
	}
}
