// Package install creates and removes the desktop and Start Menu shortcuts
// that point families at the kids-shelf launcher, plus the per-user Windows
// uninstall entry. The platform files do the OS work; this file holds the
// shared Options, the embedded icons and the small helpers they all use.
package install

import (
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Options describes the installed launcher that the shortcuts start. The
// install flow fills every field once it knows where it put the binary.
type Options struct {
	Launcher   string // absolute path of the root launcher the shortcuts start
	InstallDir string // install root; the shortcut icon files are written here
	Version    string // shown in Settings > Apps on Windows
	DataDir    string // --data-dir argument for shortcuts that carry one (Windows .lnk, Linux .desktop)
	Uninstall  string // Windows UninstallString, e.g. "\"<install>\\kids-shelf.exe\" --uninstall"
}

//go:embed icons/kids-shelf.ico icons/kids-shelf.icns icons/kids-shelf-256.png
var icons embed.FS

// writeIcon writes one of the embedded icon files to dst, creating the
// parent directory in case the install root is missing. The .icns is never
// written here: the macOS app bundle carries its own icon.
func writeIcon(dst, name string) error {
	data, err := icons.ReadFile(name)
	if err != nil {
		return fmt.Errorf("reading the embedded %s: %w", name, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(dst), err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", dst, err)
	}
	return nil
}

// removeIfPresent removes path, treating a missing file as success so the
// remove flows can be idempotent.
func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", path, err)
	}
	return nil
}

// joinErrors combines the per-part failures of a best-effort pass; nil when
// every part succeeded.
func joinErrors(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}
