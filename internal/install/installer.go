// installer.go: the install/reinstall transaction (PLAN 1a "Install,
// reinstall, remove"). The installer is a launch from outside the install
// root: it takes both locks, publishes the running build under versions/V,
// replaces the root launcher, journals the outcome, refreshes the shortcuts
// and the macOS entry bundle, releases the locks and starts the root
// launcher detached. Every step is idempotent, so rerunning a download
// repairs a crash at any point.
package install

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/edementhon/kids-shelf/internal/launch"
	"github.com/edementhon/kids-shelf/internal/paths"
	"github.com/edementhon/kids-shelf/internal/proc"
	"github.com/edementhon/kids-shelf/internal/store"
	"github.com/edementhon/kids-shelf/internal/update"
)

// Sentinel errors the caller turns into native dialogs (cmd/kids-shelf
// maps each one to its plain message; the text below is that message).
var (
	// ErrBusy: the install or instance lock is held by a running
	// launcher or app.
	ErrBusy = errors.New("kids-shelf is running. Quit it from the parent page, then open this download again.")
	// ErrOlder: this copy is older than one this computer already ran.
	ErrOlder = errors.New("This copy is older than the one this computer already ran")
	// ErrLauncherLocked: the root launcher is still in use and could not
	// be replaced within the retry window (review round 3 R6).
	ErrLauncherLocked = errors.New("close other kids-shelf windows and try again")
	// ErrTrialPending: the launcher is still health-checking an update; a
	// version published now would be lost when that trial settles, so the
	// download waits instead (review round 4 finding 7).
	ErrTrialPending = errors.New("kids-shelf is finishing an update. Wait for it to settle, then open this download again.")
)

// ReleasesPage is where a parent gets the newest kids-shelf.
const ReleasesPage = "https://github.com/edementhon/kids-shelf/releases/latest"

// olderError is the Older refusal with its versions and next step spelled
// out; it matches ErrOlder. Without an installed copy to open (data kept
// after a removal, then an old download run again), the bare sentence left
// the parent with nothing to do (Windows VM run 2026-10-27).
type olderError struct{ msg string }

func (e olderError) Error() string        { return e.msg }
func (e olderError) Is(target error) bool { return target == ErrOlder }

// newOlderError builds the refusal for version v against the journal's
// highest version; valid says whether an installed copy will open instead.
func newOlderError(v, highWater string, valid bool) error {
	msg := "This copy of kids-shelf (" + v + ") is older than the one this computer already ran"
	if highWater != "" {
		msg += " (" + highWater + ")"
	}
	if valid {
		return olderError{msg + ". The kids-shelf already on this computer opens instead."}
	}
	return olderError{msg + ". Download the newest kids-shelf from " + ReleasesPage + " and open that instead."}
}

// InstallOptions selects what Install does. Empty directory fields resolve
// to the OS defaults; an empty Exe means the running binary; an empty
// Version falls back to update.BuildVersion (what the launcher compiled
// in).
type InstallOptions struct {
	InstallDir string
	DataDir    string
	Version    string
	Exe        string
	NoStart    bool // do not start the root launcher after a successful install (tests)
}

// Timing bounds of the transaction; variables so tests can shorten them.
var (
	// installLockWait bounds LockInstall and LockInstance before the
	// "kids-shelf is running" refusal.
	installLockWait = 15 * time.Second
	// installDBLockWait bounds the database ownership lock taken after an
	// abnormal supervisor death, proving the old child finished before
	// any journal change (review round 3, section 4).
	installDBLockWait = 30 * time.Second
	// launcherReplaceWindow bounds the retries of a failed rename over a
	// root launcher that is still mapped (Windows sharing violation).
	launcherReplaceWindow = 30 * time.Second
	// launcherReplacePoll is how often a blocked rename is retried.
	launcherReplacePoll = 100 * time.Millisecond
)

// renameOver replaces dst with src, atomically where the OS allows (per-OS
// default; a var so the retry test can inject failures).
var renameOver = func(src, dst string) error { return defaultRenameOver(src, dst) }

// renameRetryable reports whether a failed renameOver is worth retrying:
// a Windows sharing violation from a still-exiting launcher process; never
// on unix, where rename either succeeds or is a real error.
var renameRetryable = func(err error) bool { return defaultRenameRetryable(err) }

// Install implements the whole transaction for the running binary of
// version V. It returns the typed error the caller turns into a dialog,
// or a plain error for anything else; an ErrBusy answer means another
// kids-shelf is running and the caller should open the dashboard and exit.
func Install(o InstallOptions) error {
	installDir := o.InstallDir
	if installDir == "" {
		root, err := InstallRoot()
		if err != nil {
			return err
		}
		installDir = root
	}
	dataDir := o.DataDir
	if dataDir == "" {
		d, err := paths.DataDir()
		if err != nil {
			return err
		}
		dataDir = d
	}
	v := o.Version
	if v == "" {
		v = update.BuildVersion
	}
	if !update.ValidVersion(v) {
		return fmt.Errorf("this copy has no usable version (%q): installs are made from release builds only", v)
	}
	exe := o.Exe
	if exe == "" {
		e, err := os.Executable()
		if err != nil {
			return fmt.Errorf("locating the executable: %w", err)
		}
		exe = e
	}

	// The locks come first, install lock before instance lock (the launch
	// contract's order), and both create the folders the lock files live
	// in. A busy lock means a launcher or the app is running.
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", installDir, err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dataDir, err)
	}
	installLock, err := launch.LockInstall(installDir, installLockWait)
	if err != nil {
		return lockInstallErr(err)
	}
	defer installLock.Close()
	instanceLock, err := launch.LockInstance(dataDir, installLockWait)
	if err != nil {
		return lockInstallErr(err)
	}
	defer instanceLock.Close()

	// After an abnormal supervisor death the old child may still be
	// closing the database; its ownership lock proves it finished before
	// any journal change (the same fence restoreDB uses). Under a live
	// owner the instance lock would have been busy, so this is normally
	// instant.
	unlockDB, err := store.LockDB(update.DatabasePath(dataDir), installDBLockWait)
	if err != nil {
		return fmt.Errorf("waiting for kids-shelf to close its data: %w", err)
	}
	defer unlockDB()

	cls, err := update.InstallCheck(dataDir, v)
	if err != nil {
		return err
	}
	if cls == update.OlderVersion {
		// The dialog names the versions and the next step; the caller
		// opens the installed copy when the installation is valid.
		valid, _, _ := update.InstallState(installDir, dataDir)
		st, _ := update.ReadState(dataDir)
		return newOlderError(v, st.HighWater, valid)
	}
	valid, _, err := update.InstallState(installDir, dataDir)
	if err != nil {
		return err
	}

	// Nothing may be published while the journal has an unsettled trial
	// (Trying set): the next launch rejects that leftover trial, and the
	// freshly published build would never be staged, so the download would
	// silently do nothing (review round 4 finding 7). Only a startup on a
	// valid installation refuses; a fresh install over retained data clears
	// the stale pointer by design (InstallFresh, round 3 section 5 item 6).
	if valid {
		st, err := update.ReadState(dataDir)
		if err != nil {
			return err
		}
		if st.Trying != nil {
			return ErrTrialPending
		}
	}

	// Publish versions/V with the durable sequence - a fresh temp
	// directory holding the fsynced executable, renamed over, parent
	// fsynced - before any journal write.
	if err := publishVersion(installDir, v, exe); err != nil {
		return err
	}
	if err := replaceRootLauncher(installDir, exe); err != nil {
		return err
	}

	switch {
	case !valid:
		// Fresh layout over whatever the data dir retained: Current = V,
		// HighWater = max(HighWater, V), stale pointers cleared, one
		// durable write (review round 3 R3 and section 5 item 6).
		if err := update.InstallFresh(dataDir, v); err != nil {
			return err
		}
	case cls == update.NewerVersion:
		// The normal activation and trial run the staged version on the
		// next launch. The trial check above already refused a pending
		// trial, so the stage can no longer be lost to one.
		if err := update.InstallStage(dataDir, v); err != nil {
			return err
		}
	}
	// V == Current on a valid installation: nothing to journal.

	// macOS: the entry bundle is a fresh-file copy of the downloaded .app,
	// placed in /Applications (or ~/Applications when the account cannot
	// write it), so the launch entry point exists (macOS creates no Desktop
	// shortcut; the bundle is the entry point, pinned to the Dock).
	if err := ensureEntryBundle(exe); err != nil {
		return err
	}
	// Shortcuts are cosmetic on every OS: a prompt-blocked Desktop item, a
	// refused uninstall-entry write, a missing shortcut tool or a Dock
	// update cfprefsd refuses must not strand a completed install before
	// the launcher starts (observed on macOS 15.8.2: the Finder alias timed
	// out and the root launcher never ran). The errors are reported on
	// stderr and the install continues.
	if _, err := createShortcuts(shortcutOptions(installDir, dataDir, o.Version, v)); err != nil {
		fmt.Fprintf(os.Stderr, "kids-shelf: shortcuts were not created: %v\n", err)
	}

	if !o.NoStart {
		// The launcher takes both locks itself, so release ours first.
		unlockDB()
		if err := instanceLock.Close(); err != nil {
			return err
		}
		if err := installLock.Close(); err != nil {
			return err
		}
		return startInstalledApp(installDir, dataDir)
	}
	return nil
}

// createShortcuts is Install's shortcut step; a var so tests can inject a
// failed creation and pin that the install still starts the launcher.
var createShortcuts = func(o Options) ([]string, error) { return CreateShortcuts(o) }

// lockInstallErr maps a lock failure onto the typed errors the dialogs use:
// a busy lock is ErrBusy, anything else is a plain failure.
func lockInstallErr(err error) error {
	if errors.Is(err, launch.ErrBusy) {
		return ErrBusy
	}
	return fmt.Errorf("taking the kids-shelf lock: %w", err)
}

// shortcutOptions builds the shortcut Options for an installed launcher.
// The Windows uninstall entry starts the same --remove role HandOff uses,
// with the directories spelled out, so Settings > Apps can remove a copy
// whose data directory is not the default. The Windows and Linux shortcuts
// start the launcher with --data-dir only when the installation used a
// non-default one, by the same rule as the root launcher's own start
// (rootLauncherArgs); macOS's only shortcut is the entry bundle's Dock
// item, whose URL is the bundle path, not the root launcher.
func shortcutOptions(installDir, dataDir, oVersion, v string) Options {
	if oVersion == "" {
		oVersion = v
	}
	exe := filepath.Join(installDir, update.EntryName())
	o := Options{
		Launcher:   exe,
		InstallDir: installDir,
		Version:    oVersion,
		Uninstall:  `"` + exe + `" --remove --install "` + installDir + `" --data "` + dataDir + `"`,
	}
	if len(rootLauncherArgs(dataDir)) > 0 {
		o.DataDir = dataDir
	}
	return o
}

// publishVersion durably publishes the running binary as
// <install>/versions/<v>/kids-shelf: the executable is copied fresh into a
// temp directory and fsynced, the temp directory itself is fsynced (so the
// entry that names the executable survives a crash together with it, not
// just the file's bytes), renamed over any previous copy, and the parent is
// fsynced (the update package's durable sequence; review round 4 finding
// 12).
func publishVersion(installDir, v, exe string) error {
	vd := update.VersionDir(installDir, v)
	tmp := vd + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return fmt.Errorf("clearing the staging directory: %w", err)
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", tmp, err)
	}
	bin := filepath.Join(tmp, update.EntryName())
	if err := copySynced(exe, bin, 0o755); err != nil {
		return fmt.Errorf("publishing %s: %w", v, err)
	}
	if err := syncTempVersionDir(tmp); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("syncing the staging directory: %w", err)
	}
	if err := os.RemoveAll(vd); err != nil {
		return fmt.Errorf("replacing %s: %w", vd, err)
	}
	if err := os.Rename(tmp, vd); err != nil {
		return fmt.Errorf("publishing %s: %w", vd, err)
	}
	if err := update.SyncParent(vd); err != nil {
		return err
	}
	return nil
}

// syncTempVersionDir fsyncs the temporary version directory before its
// rename publishes it; a var so the test can pin the ordering (finding 12).
var syncTempVersionDir = func(dir string) error { return fsyncDir(dir) }

// replaceRootLauncher puts a fresh copy of the running binary at
// <install>/kids-shelf: the temp file is written and fsynced, then renamed
// over the installed launcher and the directory fsynced. A failed rename
// over a launcher that is still mapped by a shutting-down process (Windows
// sharing violation) is retried for launcherReplaceWindow, because holding
// the locks does not prove the executable is unmapped (review round 3 R6);
// after the window the typed ErrLauncherLocked answers.
func replaceRootLauncher(installDir, exe string) error {
	dst := update.EntryPath(installDir)
	tmp := filepath.Join(installDir, ".kids-shelf.new")
	if err := copySynced(exe, tmp, 0o755); err != nil {
		return fmt.Errorf("preparing the new launcher: %w", err)
	}
	deadline := time.Now().Add(launcherReplaceWindow)
	for {
		err := renameOver(tmp, dst)
		if err == nil {
			return update.SyncParent(dst)
		}
		if !renameRetryable(err) {
			os.Remove(tmp)
			return fmt.Errorf("replacing the launcher: %w", err)
		}
		if time.Now().After(deadline) {
			os.Remove(tmp)
			return ErrLauncherLocked
		}
		time.Sleep(launcherReplacePoll)
	}
}

// copySynced copies src to dst with mode, fsyncing the copy before it
// closes, so the bytes survive a crash before the rename that publishes
// them.
func copySynced(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	return nil
}

// startRootLauncher starts the installed launcher detached (no console
// window on Windows). It either becomes the supervisor or joins the running
// instance and opens the browser, so "start or join" is one command. The
// data directory is spelled out when it is not the OS default, so a first
// installation made with an explicit --data-dir never runs against another
// one (review round 4 finding 9). It is a var so tests can observe that
// Install still starts the launcher when shortcut creation failed, without
// spawning a real process.
var startRootLauncher = func(installDir, dataDir string) error {
	cmd := proc.Command(update.EntryPath(installDir), rootLauncherArgs(dataDir)...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting kids-shelf: %w", err)
	}
	return nil
}

// rootLauncherArgs is the --data-dir argument for a root launcher launch:
// none when dataDir is the OS default, the explicit directory otherwise.
// It is separate from startRootLauncher so tests can pin the decision
// without starting a process.
func rootLauncherArgs(dataDir string) []string {
	if def, err := paths.DataDir(); err != nil || !sameDir(dataDir, def) {
		return []string{"--data-dir", dataDir}
	}
	return nil
}

// StartInstalled starts or joins the installed root launcher for dataDir:
// the Older refusal's "open the installed copy" path (PLAN 1a). It is the
// same start-or-join command the installer finishes with, so a copy that is
// already running is joined instead of duplicated.
func StartInstalled(dataDir string) error {
	install, err := InstallRoot()
	if err != nil {
		return err
	}
	return startInstalledApp(install, dataDir)
}

// openThroughApp decides whether macOS starts the installed copy through
// its app bundle (see startInstalledApp): only when the bundle exists, the
// data directory is the default one (LaunchServices passes no
// --data-dir), and this process runs with the account's own home folder.
// LaunchServices always starts the app with the account's real
// environment, so a run with another HOME (the smoke test's throwaway
// home) must start the launcher directly or it would install into the
// real profile instead.
func openThroughApp(bundle string, launcherArgs []string, home, accountHome string) bool {
	if bundle == "" || len(launcherArgs) != 0 || home == "" || accountHome == "" {
		return false
	}
	return filepath.Clean(home) == filepath.Clean(accountHome)
}

// IsInstalled reports whether exe is the root launcher of a valid
// installation for the journal in dataDir: it must be <install>/kids-shelf
// and the journaled current version must exist. cmd/kids-shelf uses it to
// choose between the supervisor role and the installer role; the data
// directory is an argument because an explicit --data-dir may differ from
// the OS default (review round 4 finding 9).
func IsInstalled(exe, dataDir string) bool {
	install, err := InstallRoot()
	if err != nil {
		return false
	}
	if filepath.Clean(exe) != filepath.Clean(update.EntryPath(install)) {
		return false
	}
	valid, _, err := update.InstallState(install, dataDir)
	return err == nil && valid
}
