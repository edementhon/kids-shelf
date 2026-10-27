//go:build darwin

package install

import (
	"github.com/edementhon/kids-shelf/internal/paths"
	"github.com/edementhon/kids-shelf/internal/update"
)

// EntryMain is the macOS entry bundle's role (PLAN 1a "Launcher"). With a
// valid installation the bundle just starts or joins the root launcher and
// exits; without one it acts as the installer from its own bundle, so the
// first double-click of a downloaded kids-shelf.app installs the copy. It
// returns whatever the caller should surface as a startup error.
func EntryMain() error {
	install, err := InstallRoot()
	if err != nil {
		return err
	}
	dataDir, err := paths.DataDir()
	if err != nil {
		return err
	}
	valid, _, err := update.InstallState(install, dataDir)
	if err != nil {
		return err
	}
	if valid {
		// Start or join: the root launcher either takes the locks and
		// becomes the supervisor, or joins the running instance and
		// opens the browser. The entry bundle always uses the default
		// data directory, so no --data-dir argument is needed.
		return startRootLauncher(install, dataDir)
	}
	return Install(InstallOptions{})
}
