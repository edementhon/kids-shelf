// Package paths resolves the per-user data and media directories.
//
// The locations follow the phase 1a contract: the data directory is
// %AppData%\kids-shelf on Windows, ~/Library/Application Support/kids-shelf
// on macOS, and $XDG_DATA_HOME/kids-shelf (else ~/.local/share/kids-shelf)
// on Linux. Media defaults to a "kids-shelf media" folder in the user's
// Videos (Linux/Windows) or Movies (macOS) folder, falling back to the data
// directory when the well-known folder does not exist. Both are independent
// of where the binary sits, so a copied or replaced binary finds the same
// data.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const (
	dataDirName  = "kids-shelf"
	mediaDirName = "kids-shelf media"
	videosFolder = "Videos"
	moviesFolder = "Movies"
)

// DataDir returns the per-user data directory. An explicit override is
// returned unchanged before anything is resolved; otherwise the location
// follows the OS conventions above, preferring the environment variables
// (APPDATA, XDG_DATA_HOME) and consulting the home directory only when the
// location still needs one (macOS, or Linux without XDG_DATA_HOME).
func DataDir(override ...string) (string, error) {
	if o := first(override); o != "" {
		return o, nil
	}
	goos := runtime.GOOS
	appData := os.Getenv("APPDATA")
	xdg := os.Getenv("XDG_DATA_HOME")
	var home string
	if goos == "darwin" || (goos != "windows" && xdg == "") {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolving the home directory: %w", err)
		}
		home = h
	}
	return dataDirFor(goos, appData, xdg, home, "")
}

// dataDirFor resolves the data directory for an explicit OS name and
// environment, so every branch can be tested on one machine.
func dataDirFor(goos, appData, xdgDataHome, home, override string) (string, error) {
	if override != "" {
		return override, nil
	}
	switch goos {
	case "windows":
		if appData == "" {
			return "", fmt.Errorf("the %%APPDATA%% environment variable is not set")
		}
		return filepath.Join(appData, dataDirName), nil
	case "darwin":
		if home == "" {
			return "", fmt.Errorf("the home directory is unknown")
		}
		return filepath.Join(home, "Library", "Application Support", dataDirName), nil
	default:
		base := xdgDataHome
		if base == "" {
			if home == "" {
				return "", fmt.Errorf("the home directory is unknown and XDG_DATA_HOME is not set")
			}
			base = filepath.Join(home, ".local", "share")
		}
		return filepath.Join(base, dataDirName), nil
	}
}

// DefaultMediaDir returns the default media folder: a "kids-shelf media"
// folder in the user's Videos (Linux/Windows) or Movies (macOS) folder. A
// non-empty override is returned unchanged. When the well-known folder
// cannot be located or does not exist, dataDir (the data directory in use)
// is returned instead, as the media fallback location.
func DefaultMediaDir(dataDir, override string) string {
	if override != "" {
		return override
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return dataDir
	}
	return mediaDirFor(runtime.GOOS, home, dataDir, "")
}

// mediaDirFor resolves the default media directory for an explicit OS name
// and home directory, so every branch can be tested on one machine. It
// returns fallback when the user's well-known Videos/Movies folder does not
// exist or the home directory is unknown.
func mediaDirFor(goos, home, fallback, override string) string {
	if override != "" {
		return override
	}
	if home == "" {
		return fallback
	}
	folder := videosFolder
	if goos == "darwin" {
		folder = moviesFolder
	}
	wellKnown := filepath.Join(home, folder)
	if _, err := os.Stat(wellKnown); err != nil {
		return fallback
	}
	return filepath.Join(wellKnown, mediaDirName)
}

func first(override []string) string {
	if len(override) > 0 {
		return override[0]
	}
	return ""
}
