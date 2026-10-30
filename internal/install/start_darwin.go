//go:build darwin

package install

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
)

// startInstalledApp starts the installed kids-shelf. On macOS it opens the
// installed app bundle through LaunchServices (/usr/bin/open) when
// openThroughApp allows it: macOS then holds that signed app responsible
// for the folders kids-shelf reads, so it can ask the parent before
// kids-shelf reads Documents, Desktop, Downloads, drives or network shares.
// Started directly from the downloaded copy, which macOS runs from a
// temporary translocated mount that is gone by then, the first run was
// refused without a prompt (Mac VM, 2026-10-30). Anything else, or a
// failed open, starts the root launcher directly as before.
func startInstalledApp(installDir, dataDir string) error {
	bundle := installedEntryBundle()
	home, _ := os.UserHomeDir()
	accountHome := ""
	if u, err := user.Current(); err == nil {
		accountHome = u.HomeDir
	}
	if openThroughApp(bundle, rootLauncherArgs(dataDir), home, accountHome) {
		if err := exec.Command("/usr/bin/open", bundle).Run(); err == nil {
			return nil
		} else {
			fmt.Fprintf(os.Stderr, "kids-shelf: opening %s: %v; starting it directly\n", bundle, err)
		}
	}
	return startRootLauncher(installDir, dataDir)
}

// installedEntryBundle returns the first entry bundle that exists, or "".
func installedEntryBundle() string {
	for _, b := range entryBundleCandidates() {
		if st, err := os.Stat(b); err == nil && st.IsDir() {
			return b
		}
	}
	return ""
}
