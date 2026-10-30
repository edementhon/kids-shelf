//go:build !darwin

package install

// ensureEntryBundle is a no-op everywhere but macOS, where the entry bundle
// (/Applications/kids-shelf.app, else ~/Applications/kids-shelf.app) is
// the double-clickable copy of the downloaded .app.
func ensureEntryBundle(string) error { return nil }

// removeEntryBundle removes the macOS entry bundle; nothing to do elsewhere.
func removeEntryBundle() error { return nil }

// startInstalledApp starts the installed root launcher; only macOS starts
// it through an app bundle.
func startInstalledApp(installDir, dataDir string) error {
	return startRootLauncher(installDir, dataDir)
}
