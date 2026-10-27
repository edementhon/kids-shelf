//go:build windows

package install

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func installRoot() (string, error) {
	dir, err := localAppData()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "Programs", "kids-shelf"), nil
}

// localAppData resolves the per-user local app data folder through the
// Known Folder API, falling back to %LOCALAPPDATA% when the API cannot
// answer (e.g. running as a service without a profile).
func localAppData() (string, error) {
	if dir, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, windows.KF_FLAG_DEFAULT); err == nil {
		return dir, nil
	}
	if dir := os.Getenv("LOCALAPPDATA"); dir != "" {
		return dir, nil
	}
	return "", fmt.Errorf("the Windows local app data folder is unknown")
}

func entryBundleCandidates() []string { return nil }
