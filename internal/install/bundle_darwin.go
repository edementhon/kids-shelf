//go:build darwin

package install

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/edementhon/kids-shelf/internal/update"
)

// ensureEntryBundle places a fresh copy of the bundle this process runs
// from at the entry bundle path: /Applications/kids-shelf.app when the user
// can write /Applications (the admin accounts can, with no prompt), else
// ~/Applications/kids-shelf.app (PLAN 1a "Launcher"). A machine that
// already has a ~/Applications copy keeps working; reinstalling moves it by
// placing the fresh copy in /Applications and dropping the old one. The
// copy is a fresh-file copy of the whole tree into a temp directory renamed
// over the destination, so a crash never leaves a torn bundle and rerunning
// the installer repairs one.
func ensureEntryBundle(exe string) error {
	src := appBundleDir(exe)
	if src == "" {
		return errors.New("this copy is not inside a kids-shelf.app bundle")
	}
	dst := entryBundleDst()
	if dst == "" {
		return errors.New("cannot place the app bundle: /Applications is not writable and the home directory is unknown")
	}
	if samePath(src, dst) {
		return nil // already installed in place
	}
	if err := replaceBundle(src, dst); err != nil {
		return err
	}
	// The bundle may have moved (an install that ran from the old
	// ~/Applications copy): drop the stale copy at the other entry
	// location. Best effort: the bundle is only an extra launch path, and
	// the running process holds its own image open, so unlinking the
	// directory is safe.
	dropStaleEntryBundle(dst)
	return nil
}

// dropStaleEntryBundle removes the entry bundle candidate this install did
// not place its fresh copy at, logging (never failing) when it cannot.
func dropStaleEntryBundle(dst string) {
	for _, c := range entryBundleCandidates() {
		if samePath(c, dst) {
			continue
		}
		if err := os.RemoveAll(c); err != nil {
			fmt.Fprintf(os.Stderr, "kids-shelf: could not remove the old entry bundle %s: %v\n", c, err)
		}
	}
}

// removeEntryBundle deletes the installed entry bundles. Both entry
// locations are removed (a machine may hold either, or both after a move).
// It is idempotent: a missing bundle is a successful removal.
func removeEntryBundle() error {
	var errs []error
	for _, c := range entryBundleCandidates() {
		if c == "" {
			continue
		}
		if err := os.RemoveAll(c); err != nil {
			errs = append(errs, fmt.Errorf("removing %s: %w", c, err))
		}
	}
	return joinErrors(errs)
}

// appBundleDir walks up from an executable inside a macOS bundle
// (.../kids-shelf.app/Contents/MacOS/kids-shelf) to the bundle directory.
func appBundleDir(exe string) string {
	dir := filepath.Dir(exe)
	for {
		if strings.HasSuffix(filepath.Base(dir), ".app") {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// replaceBundle fresh-copies the src tree into dst: the copy lands in a
// temp directory that is renamed over the destination and the parent
// directory is fsynced, so the swap is durable.
func replaceBundle(src, dst string) error {
	parent := filepath.Dir(dst)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", parent, err)
	}
	if err := clearBundleTemps(parent); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".kids-shelf.app.")
	if err != nil {
		return fmt.Errorf("creating the bundle staging directory: %w", err)
	}
	if err := copyTree(src, tmp); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("copying the app bundle: %w", err)
	}
	if err := os.RemoveAll(dst); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("replacing %s: %w", dst, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("placing %s: %w", dst, err)
	}
	return update.SyncParent(dst)
}

// clearBundleTemps drops the staging directories a crashed previous copy
// left in the bundle's parent, so they never accumulate.
func clearBundleTemps(parent string) error {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return fmt.Errorf("reading %s: %w", parent, err)
	}
	var errs []error
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".kids-shelf.app.") {
			if err := os.RemoveAll(filepath.Join(parent, e.Name())); err != nil {
				errs = append(errs, fmt.Errorf("clearing %s: %w", e.Name(), err))
			}
		}
	}
	return joinErrors(errs)
}

// copyTree copies one directory tree into dst: directories become
// directories, files are copied byte-for-byte with their mode and fsynced,
// and symlinks are recreated as symlinks (macOS bundles carry them inside
// Frameworks).
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return copySynced(path, target, info.Mode().Perm())
	})
}

// samePath reports whether two paths name the same directory.
func samePath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}
