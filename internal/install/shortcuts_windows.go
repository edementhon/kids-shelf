//go:build windows

package install

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/edementhon/kids-shelf/internal/proc"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// uninstallKeyPath is the per-user uninstall entry that Settings > Apps
// reads; the install flow removes it when kids-shelf is uninstalled.
const uninstallKeyPath = `Software\Microsoft\Windows\CurrentVersion\Uninstall\kids-shelf`

// CreateShortcuts writes the Desktop and Start Menu "kids-shelf" shortcuts
// and the per-user uninstall entry. It is best effort: every part that can
// be created is, and the returned list plus joined error name what could
// not be. Shortcut folders come from the Known Folder API, never from
// %USERPROFILE%\Desktop, so OneDrive-redirected desktops get their icon in
// the real place.
func CreateShortcuts(o Options) ([]string, error) {
	var created []string
	var errs []error

	icoPath := windowsIconPath(o)
	if err := writeIcon(icoPath, "icons/kids-shelf.ico"); err != nil {
		errs = append(errs, err)
	} else {
		created = append(created, icoPath)
	}

	for _, folder := range []*windows.KNOWNFOLDERID{windows.FOLDERID_Desktop, windows.FOLDERID_Programs} {
		dir, err := windows.KnownFolderPath(folder, windows.KF_FLAG_DEFAULT)
		if err != nil {
			errs = append(errs, fmt.Errorf("locating a shortcuts folder: %w", err))
			continue
		}
		lnk := filepath.Join(dir, "kids-shelf.lnk")
		if err := createWindowsShortcut(o, lnk); err != nil {
			errs = append(errs, err)
			continue
		}
		created = append(created, lnk)
	}

	if o.Version == "" || o.Uninstall == "" {
		errs = append(errs, errors.New("Windows shortcuts need Version and Uninstall for the uninstall entry"))
	} else if err := writeUninstallEntry(o); err != nil {
		errs = append(errs, err)
	}
	return created, joinErrors(errs)
}

// createWindowsShortcut writes one .lnk with a single PowerShell call; the
// paths travel in the environment, and the process is started hidden so no
// console window flashes on a windowsgui build.
func createWindowsShortcut(o Options, lnk string) error {
	cmd := proc.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", windowsShortcutScript)
	cmd.Env = append(os.Environ(), windowsLnkEnv(o, lnk)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		if len(out) > 0 {
			return fmt.Errorf("creating %s: %w: %s", lnk, err, out)
		}
		return fmt.Errorf("creating %s: %w", lnk, err)
	}
	return nil
}

// writeUninstallEntry writes the per-user uninstall registry entry, also
// best effort: a failing value is recorded without dropping the ones that
// succeeded. EstimatedSize is skipped when the install directory cannot be
// measured rather than reported as 0 KB.
func writeUninstallEntry(o Options) error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, uninstallKeyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("opening the uninstall registry entry: %w", err)
	}
	defer key.Close()

	var errs []error
	estKB, estErr := dirSizeKB(o.InstallDir)
	if estErr != nil {
		errs = append(errs, fmt.Errorf("measuring the install directory for EstimatedSize: %w", estErr))
	}
	for _, v := range windowsRegistryValues(o, estKB) {
		if estErr != nil && v.name == "EstimatedSize" {
			continue
		}
		var werr error
		if v.isDWord {
			werr = key.SetDWordValue(v.name, v.dword)
		} else {
			werr = key.SetStringValue(v.name, v.str)
		}
		if werr != nil {
			errs = append(errs, fmt.Errorf("writing %s: %w", v.name, werr))
		}
	}
	return joinErrors(errs)
}

// RemoveShortcuts deletes the Desktop and Start Menu shortcuts and the
// uninstall entry, ignoring ones that are already gone.
func RemoveShortcuts(o Options) error {
	var errs []error
	for _, folder := range []*windows.KNOWNFOLDERID{windows.FOLDERID_Desktop, windows.FOLDERID_Programs} {
		dir, err := windows.KnownFolderPath(folder, windows.KF_FLAG_DEFAULT)
		if err != nil {
			errs = append(errs, fmt.Errorf("locating a shortcuts folder: %w", err))
			continue
		}
		if err := removeIfPresent(filepath.Join(dir, "kids-shelf.lnk")); err != nil {
			errs = append(errs, err)
		}
	}
	if err := registry.DeleteKey(registry.CURRENT_USER, uninstallKeyPath); err != nil &&
		!errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) && !errors.Is(err, syscall.ERROR_PATH_NOT_FOUND) {
		errs = append(errs, fmt.Errorf("removing the uninstall registry entry: %w", err))
	}
	return joinErrors(errs)
}
