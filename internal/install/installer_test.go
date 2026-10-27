//go:build linux

package install

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/edementhon/kids-shelf/internal/launch"
	"github.com/edementhon/kids-shelf/internal/paths"
	"github.com/edementhon/kids-shelf/internal/update"
)

// testInstallEnv points the Linux shortcuts at a temp home so the menu
// entry lands where the test can see it.
func testInstallEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", "")
	disableLinuxTools()
}

// testExe is the running test binary: the installer copies its bytes into
// the layout but never executes them, so any regular file would do; the
// real executable keeps the test honest about byte fidelity.
func testExe(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return exe
}

// installOpts returns the options for a scripted install into temp dirs.
func installOpts(t *testing.T, version string) InstallOptions {
	t.Helper()
	return InstallOptions{
		InstallDir: t.TempDir(),
		DataDir:    t.TempDir(),
		Version:    version,
		Exe:        testExe(t),
		NoStart:    true,
	}
}

func sameBytes(t *testing.T, a, b string) {
	t.Helper()
	ab, err := os.ReadFile(a)
	if err != nil {
		t.Fatalf("reading %s: %v", a, err)
	}
	bb, err := os.ReadFile(b)
	if err != nil {
		t.Fatalf("reading %s: %v", b, err)
	}
	if string(ab) != string(bb) {
		t.Errorf("%s differs from %s", a, b)
	}
}

// TestInstallFreshCreatesLayout checks the fresh transaction end to end: the
// root launcher and versions/<v> hold the running binary, the lock file
// exists and stays, the journal lands Current and HighWater in one durable
// state, and the Linux menu entry points at the launcher.
func TestInstallFreshCreatesLayout(t *testing.T) {
	testInstallEnv(t)
	o := installOpts(t, "v1.0.0")

	if err := Install(o); err != nil {
		t.Fatalf("Install: %v", err)
	}

	sameBytes(t, o.Exe, filepath.Join(o.InstallDir, "kids-shelf"))
	sameBytes(t, o.Exe, update.VersionBinary(o.InstallDir, "v1.0.0"))

	if _, err := os.Stat(filepath.Join(o.InstallDir, launch.InstallLockName)); err != nil {
		t.Errorf("install.lock missing after install: %v", err)
	}

	st, err := update.ReadState(o.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Current != "v1.0.0" || st.HighWater != "v1.0.0" {
		t.Errorf("journal = current %q high_water %q, want both v1.0.0", st.Current, st.HighWater)
	}
	if st.Staged != "" || st.Trying != nil || st.Previous != "" {
		t.Errorf("fresh journal has leftover pointers: staged=%q trying=%+v previous=%q", st.Staged, st.Trying, st.Previous)
	}

	menu := filepath.Join(os.Getenv("HOME"), ".local", "share", "applications", kidsShelfDesktop)
	if b, err := os.ReadFile(menu); err != nil {
		t.Fatalf("menu entry not created: %v", err)
	} else if !strings.Contains(string(b), "Exec="+filepath.Join(o.InstallDir, "kids-shelf")+" --data-dir "+o.DataDir+"\n") {
		t.Errorf("menu entry does not point at the launcher with the data directory:\n%s", b)
	}
	if _, err := os.Stat(filepath.Join(o.InstallDir, "kids-shelf.png")); err != nil {
		t.Errorf("icon not written: %v", err)
	}
}

const kidsShelfDesktop = "kids-shelf.desktop"

// TestInstallRerunSameVersionIsANoop: reinstalling the same version over a
// valid installation changes nothing in the journal or the layout.
func TestInstallRerunSameVersionIsANoop(t *testing.T) {
	testInstallEnv(t)
	o := installOpts(t, "v1.0.0")
	if err := Install(o); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	before, err := os.ReadFile(update.StatePath(o.DataDir))
	if err != nil {
		t.Fatal(err)
	}

	if err := Install(o); err != nil {
		t.Fatalf("second Install: %v", err)
	}
	after, err := os.ReadFile(update.StatePath(o.DataDir))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("rerun changed the journal:\nbefore: %s\nafter:  %s", before, after)
	}
	sameBytes(t, o.Exe, update.VersionBinary(o.InstallDir, "v1.0.0"))
}

// TestInstallNewerStages: a newer version over a valid installation is
// published and staged; the launcher's activation and trial run it.
func TestInstallNewerStages(t *testing.T) {
	testInstallEnv(t)
	o := installOpts(t, "v1.0.0")
	if err := Install(o); err != nil {
		t.Fatalf("Install v1.0.0: %v", err)
	}
	o.Version = "v2.0.0"
	if err := Install(o); err != nil {
		t.Fatalf("Install v2.0.0: %v", err)
	}

	st, err := update.ReadState(o.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Current != "v1.0.0" {
		t.Errorf("current = %q, want v1.0.0 (staging never confirms)", st.Current)
	}
	if st.Staged != "v2.0.0" {
		t.Errorf("staged = %q, want v2.0.0", st.Staged)
	}
	if st.HighWater != "v1.0.0" {
		t.Errorf("high_water = %q, want v1.0.0 (a stage does not move the floor)", st.HighWater)
	}
	sameBytes(t, o.Exe, update.VersionBinary(o.InstallDir, "v2.0.0"))
}

// TestInstallOlderRefusesWithoutChanges: a copy below the journal's
// high-water mark is refused with the typed error and nothing changes.
func TestInstallOlderRefusesWithoutChanges(t *testing.T) {
	testInstallEnv(t)
	o := installOpts(t, "v1.0.0")
	if err := Install(o); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	// The machine has since run v2.0.0 (retained data): the floor moved.
	if err := update.InstallFresh(o.DataDir, "v2.0.0"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(update.StatePath(o.DataDir))
	if err != nil {
		t.Fatal(err)
	}
	beforeVersions, err := listDir(t, filepath.Join(o.InstallDir, "versions"))
	if err != nil {
		t.Fatal(err)
	}

	err = Install(o)
	if !errors.Is(err, ErrOlder) {
		t.Fatalf("Install of an older copy = %v, want ErrOlder", err)
	}
	// No installed v2.0.0 exists to open instead, so the refusal must tell
	// the parent where the newest copy is, and which versions clash.
	if msg := err.Error(); !strings.Contains(msg, ReleasesPage) || !strings.Contains(msg, "v1.0.0") || !strings.Contains(msg, "v2.0.0") {
		t.Errorf("older refusal without an installed copy = %q, want both versions and %s", msg, ReleasesPage)
	}
	after, err := os.ReadFile(update.StatePath(o.DataDir))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("older refusal changed the journal:\nbefore: %s\nafter:  %s", before, after)
	}
	afterVersions, err := listDir(t, filepath.Join(o.InstallDir, "versions"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(beforeVersions, ",") != strings.Join(afterVersions, ",") {
		t.Errorf("older refusal changed versions/: %v -> %v", beforeVersions, afterVersions)
	}
}

// TestInstallBusyLockTypedError: while another process holds either lock the
// install answers the typed ErrBusy that becomes the native dialog.
func TestInstallBusyLockTypedError(t *testing.T) {
	testInstallEnv(t)
	o := installOpts(t, "v1.0.0")
	// The installer creates the dir; make sure it exists before locking.
	if err := os.MkdirAll(o.InstallDir, 0o755); err != nil {
		t.Fatal(err)
	}
	installLock, err := launch.LockInstall(o.InstallDir, 0)
	if err != nil {
		t.Fatalf("LockInstall: %v", err)
	}

	old := installLockWait
	installLockWait = 200 * time.Millisecond
	t.Cleanup(func() { installLockWait = old })

	if err := Install(o); !errors.Is(err, ErrBusy) {
		t.Errorf("install with a held install lock = %v, want ErrBusy", err)
	}
	installLock.Close()

	// The instance lock alone is just as busy.
	instanceLock, err := launch.LockInstance(o.DataDir, 0)
	if err != nil {
		t.Fatalf("LockInstance: %v", err)
	}
	defer instanceLock.Close()
	if err := Install(o); !errors.Is(err, ErrBusy) {
		t.Errorf("install with a held instance lock = %v, want ErrBusy", err)
	}
}

// TestInstallLauncherReplaceBoundedRetry pins review round 3 R6 via the
// injectable rename: a launcher that stays in use (Windows sharing
// violation) is retried for only the bounded window, then the typed
// ErrLauncherLocked answers; a transient in-use window still succeeds.
func TestInstallLauncherReplaceBoundedRetry(t *testing.T) {
	testInstallEnv(t)
	o := installOpts(t, "v1.0.0")

	oldOver, oldRetry := renameOver, renameRetryable
	oldWindow, oldPoll := launcherReplaceWindow, launcherReplacePoll
	launcherReplaceWindow = 250 * time.Millisecond
	launcherReplacePoll = 10 * time.Millisecond
	t.Cleanup(func() {
		renameOver, renameRetryable = oldOver, oldRetry
		launcherReplaceWindow, launcherReplacePoll = oldWindow, oldPoll
	})
	renameRetryable = func(error) bool { return true }

	// A launcher that never lets go: bounded retries, then the typed error,
	// and no journal (the replace happens before any journal write).
	var attempts int
	renameOver = func(src, dst string) error {
		attempts++
		return errors.New("simulated sharing violation")
	}
	err := Install(o)
	if !errors.Is(err, ErrLauncherLocked) {
		t.Fatalf("unreplaceable launcher = %v, want ErrLauncherLocked", err)
	}
	if attempts < 3 {
		t.Errorf("rename attempts = %d, want the bounded window to retry several times", attempts)
	}
	if _, serr := os.Stat(update.StatePath(o.DataDir)); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("journal exists after a failed replace (stat err %v); the journal must come after the launcher", serr)
	}

	// A launcher that releases after two attempts: the retry succeeds and
	// the install completes.
	attempts = 0
	real := oldOver
	renameOver = func(src, dst string) error {
		attempts++
		if attempts <= 2 {
			return errors.New("still mapped")
		}
		return real(src, dst)
	}
	if err := Install(o); err != nil {
		t.Fatalf("transiently busy launcher: %v", err)
	}
	if attempts != 3 {
		t.Errorf("rename attempts = %d, want 3 (two failures, one success)", attempts)
	}
	sameBytes(t, o.Exe, update.EntryPath(o.InstallDir))
	st, err := update.ReadState(o.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Current != "v1.0.0" {
		t.Errorf("current = %q after the recovered replace, want v1.0.0", st.Current)
	}
}

// TestInstallCrashAfterRootPublication is review round 3 R3's advertised
// recovery path: the installer published the root and versions, then died
// before the journal write. The rerun must land Current and HighWater in
// the same durable write, so a later updater still refuses releases older
// than the build.
func TestInstallCrashAfterRootPublication(t *testing.T) {
	testInstallEnv(t)
	o := installOpts(t, "v1.0.0")
	if err := Install(o); err != nil {
		t.Fatalf("first Install: %v", err)
	}

	// The journal never landed (or landed empty): crash before the write.
	if err := os.RemoveAll(update.StatePath(o.DataDir)); err != nil {
		t.Fatal(err)
	}
	if err := Install(o); err != nil {
		t.Fatalf("rerun after the crash: %v", err)
	}
	st, err := update.ReadState(o.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Current != "v1.0.0" || st.HighWater != "v1.0.0" {
		t.Errorf("rerun journal = current %q high_water %q, want both v1.0.0", st.Current, st.HighWater)
	}

	// An existing but empty-Current journal (Current=\"\") is the same
	// fresh case, not a valid installation.
	if err := os.WriteFile(update.StatePath(o.DataDir), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Install(o); err != nil {
		t.Fatalf("rerun over an empty-Current journal: %v", err)
	}
	st, _ = update.ReadState(o.DataDir)
	if st.Current != "v1.0.0" || st.HighWater != "v1.0.0" {
		t.Errorf("empty-Current rerun journal = current %q high_water %q, want v1.0.0/v1.0.0", st.Current, st.HighWater)
	}
}

// TestInstallFreshOverRetainedDataClearsPointers is review round 3, section
// 5 item 6: data kept after a removal still carries stale Staged/Trying/
// Previous entries; a fresh install clears them while keeping the floor.
func TestInstallFreshOverRetainedDataClearsPointers(t *testing.T) {
	testInstallEnv(t)
	o := installOpts(t, "v1.5.0")

	// Retained data from a removed installation: the journal names the
	// version the machine last ran (with stale stage and trial pointers on
	// top) and an install root that is gone. Written in the journal's JSON
	// shape (the update package's own tests seed the same state through
	// writeState).
	journal := `{
  "current": "v1.5.0",
  "previous": "v1.4.0",
  "high_water": "v1.5.0",
  "staged": "v9.0.0",
  "trying": {"version": "v9.0.0", "snapshot": "pre.db", "started": "2026-10-27T00:00:00Z"}
}
`
	if err := os.MkdirAll(filepath.Dir(update.StatePath(o.DataDir)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(update.StatePath(o.DataDir), []byte(journal), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Install(o); err != nil {
		t.Fatalf("Install over retained data: %v", err)
	}
	st, err := update.ReadState(o.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Current != "v1.5.0" {
		t.Errorf("current = %q, want v1.5.0", st.Current)
	}
	if st.HighWater != "v1.5.0" {
		t.Errorf("high_water = %q, want v1.5.0 (the retained floor survives)", st.HighWater)
	}
	if st.Staged != "" || st.Trying != nil || st.Previous != "" {
		t.Errorf("stale pointers survived the fresh install: staged=%q trying=%+v previous=%q", st.Staged, st.Trying, st.Previous)
	}
}

// TestInstallRejectsUnusableVersion: only canonical release versions may be
// installed, and the refusal makes no changes.
func TestInstallRejectsUnusableVersion(t *testing.T) {
	testInstallEnv(t)
	o := installOpts(t, "1.0.0") // "dev"-class name, not vMAJOR.MINOR.PATCH
	if err := Install(o); err == nil {
		t.Fatal("Install with a non-canonical version succeeded, want a refusal")
	}
	if _, err := os.Stat(filepath.Join(o.InstallDir, "versions")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("versions/ exists after the refusal (stat err %v)", err)
	}
}

// listDir returns the sorted entry names of dir.
func listDir(t *testing.T, dir string) ([]string, error) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}

// TestInstallRefusesWhileTrialPending pins review round 4 finding 7: an
// unsettled trial in the journal (a crashed launch, or an update still
// being health-checked) refuses a newer install before anything is
// published or the launcher is replaced, with the visible, exported
// ErrTrialPending instead of a silently ignored stage.
func TestInstallRefusesWhileTrialPending(t *testing.T) {
	testInstallEnv(t)
	o := installOpts(t, "v1.0.0")
	if err := Install(o); err != nil {
		t.Fatalf("Install v1.0.0: %v", err)
	}
	// A trial of v2.0.0 is in flight (the machine crashed mid-trial or the
	// launcher is still health-checking it). Written in the journal's JSON
	// shape, like the update package's own tests seed it.
	journal := `{
  "current": "v1.0.0",
  "high_water": "v1.0.0",
  "trying": {"version": "v2.0.0", "snapshot": "pre.db", "started": "2026-10-27T00:00:00Z"}
}
`
	if err := os.WriteFile(update.StatePath(o.DataDir), []byte(journal), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(update.StatePath(o.DataDir))
	if err != nil {
		t.Fatal(err)
	}

	o.Version = "v2.0.0"
	if err := Install(o); !errors.Is(err, ErrTrialPending) {
		t.Fatalf("Install over an unsettled trial = %v, want ErrTrialPending", err)
	}
	after, err := os.ReadFile(update.StatePath(o.DataDir))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("trial refusal changed the journal:\nbefore: %s\nafter:  %s", before, after)
	}
	// Nothing of v2.0.0 was published: no versions/v2.0.0 (publishing
	// precedes the launcher replacement, so its absence means neither
	// happened).
	if _, err := os.Stat(update.VersionBinary(o.InstallDir, "v2.0.0")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("v2.0.0 published despite the pending trial (stat err %v)", err)
	}
	sameBytes(t, o.Exe, update.EntryPath(o.InstallDir))
}

// TestRootLauncherArgsCarryDataDir pins review round 4 finding 9: the root
// launcher is started with --data-dir exactly when the installation's data
// directory is not the OS default, so the first launch can never open
// another household.
func TestRootLauncherArgsCarryDataDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	def, err := paths.DataDir()
	if err != nil {
		t.Fatal(err)
	}
	if got := rootLauncherArgs(def); got != nil {
		t.Errorf("rootLauncherArgs(default %q) = %v, want no argument", def, got)
	}
	other := t.TempDir()
	if got := rootLauncherArgs(other); !reflect.DeepEqual(got, []string{"--data-dir", other}) {
		t.Errorf("rootLauncherArgs(%q) = %v, want [--data-dir %s]", other, got, other)
	}
}

// TestIsInstalledUsesDataDir pins the IsInstalled contract: the check
// consults the journal in the given data directory, not the OS default, so
// an installation made with an explicit --data-dir is recognized as
// installed and role classification never re-enters the installer against
// another household (review round 4 finding 9).
func TestIsInstalledUsesDataDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		// The Windows root comes from the Known Folder API, which a test
		// cannot redirect: installing there would touch the real
		// installation of whoever runs the tests.
		t.Skip("the Windows install root cannot be redirected to a temp dir")
	}
	testInstallEnv(t)
	root, err := InstallRoot()
	if err != nil {
		t.Fatal(err)
	}
	// Role classification is against the OS-default install root
	// (production installs never override InstallDir), so the installed
	// copy must be planted there for IsInstalled to see it.
	o := InstallOptions{DataDir: t.TempDir(), Version: "v1.0.0", Exe: testExe(t), NoStart: true}
	if err := Install(o); err != nil {
		t.Fatalf("Install: %v", err)
	}
	exe := update.EntryPath(root)
	if !IsInstalled(exe, o.DataDir) {
		t.Errorf("IsInstalled(%q, %q) = false, want true", exe, o.DataDir)
	}
	if IsInstalled(exe, t.TempDir()) {
		t.Error("IsInstalled with a data dir that has no journal = true, want false")
	}
	if IsInstalled(filepath.Join(root, "not-the-launcher"), o.DataDir) {
		t.Error("IsInstalled accepted a path that is not the root launcher")
	}
}

// TestPublishVersionSyncsTempDirBeforeRename pins review round 4 finding
// 12: the temporary version directory is fsynced before its rename
// publishes it, so a power failure cannot keep the journal while losing the
// binary's directory entry. A refused sync aborts the publish and leaves
// no staging directory behind.
func TestPublishVersionSyncsTempDirBeforeRename(t *testing.T) {
	installDir := t.TempDir()
	vd := update.VersionDir(installDir, "v1.0.0")
	tmp := vd + ".tmp"

	var synced []string
	old := syncTempVersionDir
	syncTempVersionDir = func(dir string) error {
		synced = append(synced, dir)
		return nil
	}
	t.Cleanup(func() { syncTempVersionDir = old })

	if err := publishVersion(installDir, "v1.0.0", testExe(t)); err != nil {
		t.Fatalf("publishVersion: %v", err)
	}
	if len(synced) != 1 || synced[0] != tmp {
		t.Errorf("synced dirs = %v, want the temp version dir %s", synced, tmp)
	}
	if _, err := os.Stat(vd); err != nil {
		t.Errorf("published version dir missing: %v", err)
	}

	called := false
	syncTempVersionDir = func(string) error {
		called = true
		return errors.New("simulated sync failure")
	}
	if err := publishVersion(installDir, "v1.0.0", testExe(t)); err == nil {
		t.Fatal("publishVersion with a failing dir sync succeeded, want an error")
	}
	if !called {
		t.Error("syncTempVersionDir was not called")
	}
	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("staging directory left behind after the refused publish (stat err %v)", err)
	}
}

// TestInstallStartsLauncherWhenShortcutsFail pins the observed macOS 15.8.2
// failure mode behind change A: shortcut creation is cosmetic, so a failed
// or prompt-blocked shortcut (the Finder alias timed out with -1712) must
// never fail the install or skip the root launcher start. The completed
// journal and versioned build stay, and starting the launcher is attempted
// regardless of the shortcut error.
func TestInstallStartsLauncherWhenShortcutsFail(t *testing.T) {
	testInstallEnv(t)
	oldCreate, oldStart := createShortcuts, startRootLauncher
	createShortcuts = func(Options) ([]string, error) {
		return nil, errors.New("simulated alias timeout")
	}
	started := false
	startRootLauncher = func(string, string) error {
		started = true
		return nil
	}
	t.Cleanup(func() { createShortcuts, startRootLauncher = oldCreate, oldStart })

	o := installOpts(t, "v1.0.0")
	o.NoStart = false // the launcher start must actually be reached
	if err := Install(o); err != nil {
		t.Fatalf("Install with a failing shortcut step: %v", err)
	}
	if !started {
		t.Error("the root launcher was not started after a shortcut failure")
	}
	st, err := update.ReadState(o.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Current != "v1.0.0" || st.HighWater != "v1.0.0" {
		t.Errorf("journal = current %q high_water %q, want both v1.0.0", st.Current, st.HighWater)
	}
	if _, err := os.Stat(update.VersionBinary(o.InstallDir, "v1.0.0")); err != nil {
		t.Errorf("versioned build missing after the install: %v", err)
	}
}
