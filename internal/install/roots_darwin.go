//go:build darwin

package install

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func installRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("finding the home directory: %w", err)
	}
	return filepath.Join(home, "Library", "Application Support", "kids-shelf", "app"), nil
}

// entryBundleCandidates lists where the macOS entry bundle may live, in
// preference order: the system Applications folder, then the user's. Both
// count as the entry bundle: /Applications is the default for accounts that
// can write it (the admin accounts can, with no prompt), and the
// ~/Applications copy keeps working on machines that installed before that
// choice existed.
func entryBundleCandidates() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return []string{filepath.Join("/Applications", "kids-shelf.app")}
	}
	return []string{
		filepath.Join("/Applications", "kids-shelf.app"),
		filepath.Join(home, "Applications", "kids-shelf.app"),
	}
}

// entryBundleDst chooses where a fresh entry bundle goes: the system
// Applications folder when the user can write it (admin accounts can, with
// no prompt), else the user's ~/Applications.
func entryBundleDst() string {
	if canWriteSystemApplications() {
		return filepath.Join("/Applications", "kids-shelf.app")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Applications", "kids-shelf.app")
}

// canWriteSystemApplications reports whether the account may create files
// in /Applications: the group-writable system folder that admin accounts
// can write without any prompt (a real write test would leave probe files
// behind if the process died in between; access() is the same check the
// Finder makes).
func canWriteSystemApplications() bool {
	return unix.Access("/Applications", unix.W_OK) == nil
}
