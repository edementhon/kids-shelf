//go:build windows

package disk

import (
	"errors"
	"fmt"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
)

// Free returns the free bytes available to the caller on the volume
// containing path, which must exist and be a directory.
func Free(path string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, fmt.Errorf("disk: path %s: %w", path, err)
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
		return 0, fmt.Errorf("disk: GetDiskFreeSpaceEx %s: %w", path, err)
	}
	return free, nil
}

// volumeRootOf resolves the root of the volume that actually backs path
// ("C:\" for a plain drive path). GetVolumePathName walks junctions and
// mounted folders to the volume holding the path, so a path reached through
// a junction onto another volume is attributed to that volume rather than
// the drive letter it came in through. A var so the windows tests can pin
// the resolution without a real junction.
var volumeRootOf = func(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	p, err := windows.UTF16PtrFromString(abs)
	if err != nil {
		return "", err
	}
	// A zeroed buffer: GetVolumePathName null-terminates what it writes,
	// so UTF16ToString reads exactly the volume root.
	buf := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumePathName(p, &buf[0], uint32(len(buf))); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf), nil
}

// volumeSerialOf returns the serial number of the volume rooted at root,
// queried the way GetVolumeInformation requires (on the volume root). A var
// so the windows tests can pin the queried volume without a second one.
var volumeSerialOf = func(root string) (uint32, error) {
	p, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return 0, err
	}
	var serial uint32
	if err := windows.GetVolumeInformation(p, nil, 0, &serial, nil, nil, nil, 0); err != nil {
		return 0, err
	}
	return serial, nil
}

// FSKey returns a stable identifier of the filesystem containing path,
// suitable for matching reservations against each other: the volume serial
// number of the volume holding path, resolved through the actual volume
// GetVolumePathName reports (so a junction or mounted folder onto another
// volume gets that volume's serial), queried on the volume root. Two paths
// share a filesystem exactly when their keys are equal.
func FSKey(path string) (string, error) {
	root, err := volumeRootOf(path)
	if err != nil {
		return "", fmt.Errorf("disk: fs key of %s: %w", path, err)
	}
	if root == "" {
		return "", fmt.Errorf("disk: fs key of %s: cannot determine its volume", path)
	}
	serial, err := volumeSerialOf(root)
	if err != nil {
		return "", fmt.Errorf("disk: fs key of %s (volume %s): %w", path, root, err)
	}
	return fmt.Sprintf("%x", serial), nil
}

func isDiskFull(err error) bool {
	return errors.Is(err, windows.ERROR_DISK_FULL) ||
		errors.Is(err, windows.ERROR_HANDLE_DISK_FULL) ||
		errors.Is(err, syscall.ENOSPC)
}
