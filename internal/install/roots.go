// roots.go: the install root per OS (PLAN 1a "Launcher"): the directory
// that holds the root launcher, install.lock and versions/. It is separate
// from the data directory on Windows, and lives inside it on macOS and
// Linux (which is why removal must leave both lock files and the install
// root directory behind; see remove.go).
package install

import "os"

// InstallRoot returns the install root:
//
//	Windows: %LOCALAPPDATA%\Programs\kids-shelf
//	macOS:   ~/Library/Application Support/kids-shelf/app
//	Linux:   $XDG_DATA_HOME/kids-shelf/app, else ~/.local/share/kids-shelf/app
//
// The Windows location comes from the Known Folder API first (so OneDrive
// redirects and renamed profile folders keep working) with %LOCALAPPDATA% as
// the fallback; the Linux fallback is the same one internal/paths uses for
// the data dir.
func InstallRoot() (string, error) {
	return installRoot()
}

// EntryBundle returns the macOS entry bundle path that exists today,
// preferring the system copy (/Applications/kids-shelf.app) over the user
// copy (~/Applications/kids-shelf.app), or "" where the OS has no entry
// bundle (Windows, Linux) or none is installed yet. The entry bundle is
// short-lived: it starts or joins the root launcher and exits, and it acts
// as the installer when no valid installation exists. Use EntryBundles when
// matching a running executable: the bundle may live in either place.
func EntryBundle() string {
	for _, c := range entryBundleCandidates() {
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c
		}
	}
	return ""
}

// EntryBundles returns both macOS entry bundle candidate paths, in
// preference order: /Applications/kids-shelf.app, then
// ~/Applications/kids-shelf.app. Both count as the entry bundle: the system
// copy is the default (accounts that can write /Applications - the admin
// accounts - can, with no prompt), and machines that installed before that
// choice existed keep a working ~/Applications copy. Windows and Linux
// have no entry bundle and get nil.
func EntryBundles() []string {
	return entryBundleCandidates()
}
