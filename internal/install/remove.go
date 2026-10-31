// remove.go: removal (PLAN 1a "Install, reinstall, remove"). The parent
// page asks (PIN-gated, delete-data off by default); the app records the
// request durably in <data>/update/remove.json and exits with the remove
// code; the supervisor hands off to a temp copy of itself (HandOff), which
// waits for both locks, then empties the install root, deletes the
// shortcuts, the HKCU entry, the macOS entry bundle and its Dock item, and
// - when asked - the data directory and the app-owned default media folder.
// Lock files are never unlinked: install.lock, the instance lock and the
// database ownership lock stay behind in otherwise empty folders (review
// round 3 R5 and review round 4 finding 5).
package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/edementhon/kids-shelf/internal/launch"
	"github.com/edementhon/kids-shelf/internal/paths"
	"github.com/edementhon/kids-shelf/internal/proc"
	"github.com/edementhon/kids-shelf/internal/store"
	"github.com/edementhon/kids-shelf/internal/update"
)

// instanceLockFileName is the data dir's instance lock, which removal never
// unlinks: it and install.lock are the coordination files a concurrent
// launcher or installer may already have opened, so unlink-and-relock would
// let two processes own different inodes of the same path (R5). The name is
// the launch package's LockInstance contract.
const instanceLockFileName = "instance.lock"

// mediaDirName is the app-owned default media folder (internal/paths keeps
// the same name). Removal deletes it only when it is the configured media
// directory; imported or custom folders are never touched.
const mediaDirName = "kids-shelf media"

// removeLockWait bounds how long the remover waits for the supervisor and
// its child to release both locks; a variable so tests can shorten it.
var removeLockWait = time.Minute

// removeDBLockWait bounds the remover's wait for the database ownership
// lock, taken after the instance lock exactly as the installer takes it.
var removeDBLockWait = 30 * time.Second

// RemoveOptions describes one removal request.
type RemoveOptions struct {
	InstallDir string // install root to empty
	DataDir    string // data directory (contents removed when DeleteData)
	MediaDir   string // the configured media directory ("" = the app default)
	DeleteData bool   // fallback; remove.json is the durable request
}

// removeRequest is the durable removal request the app writes to
// <data>/update/remove.json before it exits (outside the database, so a
// crash between the request and the removal never loses the parent's
// choice; review round 3, section 5 item 7).
type removeRequest struct {
	DeleteData bool   `json:"delete_data"`
	MediaDir   string `json:"media_dir,omitempty"`
}

// RemoveRequestPath returns where the app records a pending removal.
func RemoveRequestPath(dataDir string) string {
	return filepath.Join(dataDir, "update", "remove.json")
}

// readRemoveRequest reads the durable removal request; the fallback options
// apply when the request file is missing or unreadable.
func readRemoveRequest(dataDir string, fallback RemoveOptions) (deleteData bool, mediaDir string) {
	deleteData, mediaDir = fallback.DeleteData, fallback.MediaDir
	if b, err := os.ReadFile(RemoveRequestPath(dataDir)); err == nil {
		var req removeRequest
		if json.Unmarshal(b, &req) == nil {
			deleteData = req.DeleteData
			if req.MediaDir != "" {
				mediaDir = req.MediaDir
			}
		}
	}
	return deleteData, mediaDir
}

// WriteRemoveRequest durably records a pending removal so the remover (a
// different process) reads the same choice even if the machine crashes
// between the request and the hand-off (review round 3, section 5 item 7).
// The app calls it before exiting on the remove path, outside the
// database. The file lands atomically using the journal's durability
// pattern.
func WriteRemoveRequest(dataDir string, deleteData bool, mediaDir string) error {
	req := removeRequest{DeleteData: deleteData, MediaDir: mediaDir}
	b, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(RemoveRequestPath(dataDir)), 0o755); err != nil {
		return err
	}
	return update.ReplaceFile(RemoveRequestPath(dataDir), append(b, '\n'), 0o600)
}

// HandOff is the supervisor's side of removal: the child has exited with
// ExitRemove, so the supervisor copies its own binary to the temp directory
// and starts it detached with the --remove role, then exits. The copy takes
// both locks itself, so the supervisor does not need to finish shutting
// down first.
func HandOff(o RemoveOptions) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating the executable: %w", err)
	}
	tmp, err := copyToTemp(exe)
	if err != nil {
		return err
	}
	args := []string{"--remove", "--install", o.InstallDir, "--data", o.DataDir}
	if deleteData, _ := readRemoveRequest(o.DataDir, o); deleteData {
		args = append(args, "--delete-data")
	}
	return startRemover(proc.Command(tmp, args...))
}

// startRemover launches the remover copy detached; a variable so tests can
// observe HandOff's arguments without spawning a process.
var startRemover = func(cmd *exec.Cmd) error { return cmd.Start() }

// copyToTemp copies the running binary to a fresh file in the system temp
// directory, so the supervisor's image can be replaced while the remover
// runs from somewhere stable. The copy must be executable: os.CreateTemp
// makes the file 0600 and copySynced keeps an existing file's mode, so it
// is set explicitly (without it the remover failed to start with
// "permission denied" on Linux and macOS).
func copyToTemp(exe string) (string, error) {
	pattern := "kids-shelf-remove-*"
	if runtime.GOOS == "windows" {
		pattern += ".exe"
	}
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", fmt.Errorf("creating the remover copy: %w", err)
	}
	tmp := f.Name()
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := copySynced(exe, tmp, 0o755); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("copying the remover: %w", err)
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("making the remover runnable: %w", err)
	}
	return tmp, nil
}

// Remove is the --remove role: it takes all three locks - waiting while the
// supervisor and its child still hold them - then empties the install root,
// deletes the shortcuts, the Windows uninstall entry, the macOS entry
// bundle and its Dock item, and, when the parent asked for it, the data
// directory's contents and the app-owned default media folder. Every part
// is idempotent, and lock files are never unlinked.
func Remove(o RemoveOptions) error {
	// The lock files must be creatable, and an install root or data
	// directory that is already gone (a partial removal, or a request
	// after a manual cleanup) is not an error: the locks and the minimal
	// coordination directory they need are recreated, and the rest of the
	// removal is idempotent anyway.
	if err := os.MkdirAll(o.InstallDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", o.InstallDir, err)
	}
	if err := os.MkdirAll(o.DataDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", o.DataDir, err)
	}
	installLock, err := launch.LockInstall(o.InstallDir, removeLockWait)
	if err != nil {
		return lockInstallErr(err)
	}
	defer installLock.Close()
	instanceLock, err := launch.LockInstance(o.DataDir, removeLockWait)
	if err != nil {
		return lockInstallErr(err)
	}
	defer instanceLock.Close()

	// After an abnormal supervisor death the old child may still be
	// closing its database; its ownership lock proves it finished before
	// the sweep deletes the database underneath it (the same fence the
	// installer and restoreDB use; review round 4 finding 5).
	unlockDB, err := store.LockDB(update.DatabasePath(o.DataDir), removeDBLockWait)
	if err != nil {
		return fmt.Errorf("waiting for Kids Shelf to close its data: %w", err)
	}
	defer unlockDB()

	deleteData, mediaDir := readRemoveRequest(o.DataDir, o)

	// The install root goes first, and a root that cannot be emptied stops
	// the removal right there: the shortcuts and the Windows uninstall
	// entry are the removal entry points and are deleted only after the
	// install root's files are gone. A launcher that stays mapped (Windows
	// sharing violation, retried for its window) therefore leaves the
	// machine in a state that can be removed again (review round 4 finding
	// 11).
	if err := removeInstallRoot(o.InstallDir); err != nil {
		return err
	}

	var errs []error
	if err := RemoveShortcuts(Options{
		Launcher:   update.EntryPath(o.InstallDir),
		InstallDir: o.InstallDir,
	}); err != nil {
		errs = append(errs, err)
	}
	if err := removeEntryBundle(); err != nil {
		errs = append(errs, err)
	}
	if deleteData {
		if err := removeDataContents(o); err != nil {
			errs = append(errs, err)
		}
		if err := removeDefaultMedia(o, mediaDir); err != nil {
			errs = append(errs, err)
		}
	}
	return joinErrors(errs)
}

// removeAll deletes a path like os.RemoveAll; a var so the retry test can
// inject persistent failures.
var removeAll = os.RemoveAll

// deleteRetryable reports whether a failed deletion is worth retrying: a
// Windows sharing violation or access denied from an executable another
// process still has mapped; never on unix, where deletion either succeeds
// or failed for a real reason.
var deleteRetryable = func(err error) bool { return defaultDeleteRetryable(err) }

// removeAllRetry deletes path, retrying for launcherReplaceWindow while a
// still-running process keeps it mapped: holding the locks does not prove
// the root launcher is unmapped (a handing-off supervisor or another icon
// launch can still be executing it), exactly the situation the root
// replacement's rename retry handles (review round 4 finding 11).
func removeAllRetry(path string) error {
	deadline := time.Now().Add(launcherReplaceWindow)
	for {
		err := removeAll(path)
		if err == nil {
			return nil
		}
		if !deleteRetryable(err) {
			return err
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(launcherReplacePoll)
	}
}

// removeInstallRoot empties the install root but never unlinks
// install.lock: the lock file and the directory stay, because another
// process may open the same name at any moment (R5).
func removeInstallRoot(installDir string) error {
	entries, err := os.ReadDir(installDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", installDir, err)
	}
	var errs []error
	for _, e := range entries {
		if e.Name() == launch.InstallLockName {
			continue
		}
		if err := removeAllRetry(filepath.Join(installDir, e.Name())); err != nil {
			errs = append(errs, fmt.Errorf("removing %s: %w", e.Name(), err))
		}
	}
	return joinErrors(errs)
}

// removeDataContents empties the data directory but never unlinks the
// instance lock or the database ownership lock (kids-shelf.db.lock): both
// are coordination files another process may be holding, and unlinking one
// lets a second process lock a different inode of the same path (R5 and
// review round 4 finding 5). A nested install root survives the sweep too:
// every ancestor directory in its chain is emptied selectively, and at the
// root itself install.lock is kept (review round 4 finding 10).
func removeDataContents(o RemoveOptions) error {
	entries, err := os.ReadDir(o.DataDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", o.DataDir, err)
	}
	databaseLock := filepath.Base(store.LockFile(update.DatabasePath(o.DataDir)))
	var errs []error
	for _, e := range entries {
		if e.Name() == instanceLockFileName || e.Name() == databaseLock {
			continue
		}
		path := filepath.Join(o.DataDir, e.Name())
		if sameDir(path, o.InstallDir) {
			continue // the install root and its install.lock
		}
		removed, err := removeNestedRoot(path, o.InstallDir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if removed {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, fmt.Errorf("removing %s: %w", e.Name(), err))
		}
	}
	return joinErrors(errs)
}

// removeNestedRoot handles an entry of the data sweep that contains the
// install root (for example <data>/nested holding <data>/nested/app):
// deleting the entry wholesale would unlink install.lock underneath it, so
// the chain from the entry down to the root is emptied selectively instead
// (review round 4 finding 10). removed is false when the install root is
// not inside path.
func removeNestedRoot(path, installDir string) (removed bool, err error) {
	rel, err := filepath.Rel(path, installDir)
	if err != nil || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false, nil
	}
	return true, removeSelective(path, installDir)
}

// removeSelective empties dir while keeping every child that leads to the
// install root; at the root itself only install.lock survives
// (removeInstallRoot does that part).
func removeSelective(dir, installDir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", dir, err)
	}
	var errs []error
	for _, e := range entries {
		child := filepath.Join(dir, e.Name())
		if sameDir(child, installDir) {
			if err := removeInstallRoot(child); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		rel, rerr := filepath.Rel(child, installDir)
		if rerr == nil && rel != "." && rel != ".." &&
			!strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			if err := removeSelective(child, installDir); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if err := os.RemoveAll(child); err != nil {
			errs = append(errs, fmt.Errorf("removing %s: %w", e.Name(), err))
		}
	}
	return joinErrors(errs)
}

// removeDefaultMedia deletes the app-owned default media folder, and only
// it: the folder must be the "kids-shelf media" folder paths.DefaultMediaDir
// resolves and it must be the media directory the app is actually
// configured to use. Imported and custom folders are never touched.
func removeDefaultMedia(o RemoveOptions, mediaDir string) error {
	resolved := paths.DefaultMediaDir(o.DataDir, mediaDir)
	if resolved == "" {
		return nil
	}
	if resolved != paths.DefaultMediaDir(o.DataDir, "") {
		return nil // a custom media directory: never removed
	}
	if filepath.Base(resolved) != mediaDirName {
		return nil
	}
	if sameDir(resolved, o.DataDir) || sameDir(resolved, o.InstallDir) {
		return nil // the default fell back into a directory this removal keeps
	}
	if err := os.RemoveAll(resolved); err != nil {
		return fmt.Errorf("removing the media folder %s: %w", resolved, err)
	}
	return nil
}

// sameDir reports whether path and dir name the same directory.
func sameDir(path, dir string) bool {
	if dir == "" {
		return false
	}
	a, err1 := filepath.Abs(path)
	b, err2 := filepath.Abs(dir)
	return err1 == nil && err2 == nil && filepath.Clean(a) == filepath.Clean(b)
}
