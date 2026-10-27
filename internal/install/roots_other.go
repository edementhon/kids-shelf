//go:build !windows && !darwin

package install

import (
	"fmt"
	"os"
	"path/filepath"
)

func installRoot() (string, error) {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("finding the home directory: %w", err)
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "kids-shelf", "app"), nil
}

func entryBundleCandidates() []string { return nil }
