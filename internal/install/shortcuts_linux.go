//go:build linux

package install

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/edementhon/kids-shelf/internal/proc"
)

// External tools the Linux shortcuts use. They are variables so the tests
// can point them at nothing and exercise the fallbacks on a machine that
// has neither installed.
var (
	xdgUserDir = "xdg-user-dir"
	gioCmd     = "gio"
)

// linuxApplicationsDir returns the per-user applications folder:
// $XDG_DATA_HOME/applications, or ~/.local/share/applications when
// XDG_DATA_HOME is unset, empty or relative (the spec requires absolute).
func linuxApplicationsDir() (string, error) {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" && filepath.IsAbs(xdg) {
		return filepath.Join(xdg, "applications"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("finding the home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "applications"), nil
}

// linuxDesktopDir returns the Desktop folder for the desktop copy, or ""
// to skip it: `xdg-user-dir DESKTOP` wins when it answers with an existing
// directory, then ~/Desktop when that exists. A machine with neither has no
// desktop to put an icon on.
func linuxDesktopDir() string {
	if out, err := proc.Command(xdgUserDir, "DESKTOP").Output(); err == nil {
		if dir := strings.TrimSpace(string(out)); filepath.IsAbs(dir) {
			if st, err := os.Stat(dir); err == nil && st.IsDir() {
				return dir
			}
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, "Desktop")
	if st, err := os.Stat(dir); err == nil && st.IsDir() {
		return dir
	}
	return ""
}

// desktopEntry renders the freedesktop .desktop file. The Exec line quotes
// the launcher per the Desktop Entry spec so install paths with spaces or
// quotes survive, and appends the --data-dir argument when the
// installation used an explicit data directory, so the menu entry opens
// the same household the install created (review round 4 finding 9); the
// Icon line is the absolute path of the png dropped into InstallDir.
func desktopEntry(o Options) string {
	var b strings.Builder
	b.WriteString("[Desktop Entry]\n")
	b.WriteString("Type=Application\n")
	b.WriteString("Name=kids-shelf\n")
	b.WriteString("Comment=Kids' internet server\n")
	b.WriteString("Exec=" + desktopExecQuote(o.Launcher))
	if o.DataDir != "" {
		b.WriteString(" --data-dir " + desktopExecQuote(o.DataDir))
	}
	b.WriteString("\n")
	b.WriteString("Icon=" + filepath.Join(o.InstallDir, "kids-shelf.png") + "\n")
	b.WriteString("Terminal=false\n")
	b.WriteString("Categories=Education;\n")
	return b.String()
}

// desktopExecQuote quotes one Exec argument per the Desktop Entry spec: a
// value holding any reserved character is wrapped in double quotes with the
// backslash, quote, dollar and backtick escaped and percent doubled (that
// is the spec's way to write a literal %). Clean paths pass through
// unquoted.
func desktopExecQuote(s string) string {
	const reserved = ` "'\><~|&;$*?#()%`
	needs := false
	for _, r := range s {
		if r <= ' ' || strings.ContainsRune(reserved, r) {
			needs = true
			break
		}
	}
	if !needs {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\', '"', '$', '`':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '%':
			b.WriteString("%%")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// CreateShortcuts writes the menu .desktop entry, a copy on the Desktop
// when the machine has one, and the png icon into InstallDir. It is best
// effort: every part that can be created is, and the returned list plus
// joined error name what could not.
func CreateShortcuts(o Options) ([]string, error) {
	var created []string
	var errs []error

	pngPath := filepath.Join(o.InstallDir, "kids-shelf.png")
	if err := writeIcon(pngPath, "icons/kids-shelf-256.png"); err != nil {
		errs = append(errs, err)
	} else {
		created = append(created, pngPath)
	}

	appsDir, err := linuxApplicationsDir()
	if err != nil {
		return created, joinErrors(append(errs, err))
	}
	menuPath := filepath.Join(appsDir, "kids-shelf.desktop")
	if err := writeDesktopFile(menuPath, desktopEntry(o), false); err != nil {
		errs = append(errs, err)
	} else {
		created = append(created, menuPath)
	}

	if desktop := linuxDesktopDir(); desktop != "" {
		desktopPath := filepath.Join(desktop, "kids-shelf.desktop")
		if err := writeDesktopFile(desktopPath, desktopEntry(o), true); err != nil {
			errs = append(errs, err)
		} else {
			created = append(created, desktopPath)
		}
	}
	return created, joinErrors(errs)
}

// writeDesktopFile writes a .desktop file, creating the folder. The Desktop
// copy gets mode 0755 so the environment offers "Allow Launching"; gio's
// trusted metadata is a bonus that is skipped when gio is missing and
// ignored when it refuses, because some desktops need the flag but none
// break without it.
func writeDesktopFile(path, text string, onDesktop bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	mode := os.FileMode(0o644)
	if onDesktop {
		mode = 0o755
	}
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if onDesktop {
		// WriteFile keeps the mode of an existing file, so an older copy
		// from a previous install is chmodded explicitly.
		if err := os.Chmod(path, mode); err != nil {
			return fmt.Errorf("marking %s executable: %w", path, err)
		}
		if _, err := exec.LookPath(gioCmd); err != nil {
			return nil
		}
		_ = proc.Command(gioCmd, "set", path, "metadata::trusted", "true").Run()
	}
	return nil
}

// RemoveShortcuts deletes the menu entry and the Desktop copy, and nothing
// else: the icon lives inside InstallDir, which the uninstall flow removes
// wholesale.
func RemoveShortcuts(o Options) error {
	var errs []error
	appsDir, err := linuxApplicationsDir()
	if err != nil {
		errs = append(errs, err)
	} else if err := removeIfPresent(filepath.Join(appsDir, "kids-shelf.desktop")); err != nil {
		errs = append(errs, err)
	}
	if desktop := linuxDesktopDir(); desktop != "" {
		if err := removeIfPresent(filepath.Join(desktop, "kids-shelf.desktop")); err != nil {
			errs = append(errs, err)
		}
	}
	return joinErrors(errs)
}
