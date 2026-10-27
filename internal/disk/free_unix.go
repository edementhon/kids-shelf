//go:build linux || darwin

package disk

import (
	"errors"
	"fmt"
	"strconv"

	"golang.org/x/sys/unix"
)

// Free returns the free bytes available to an unprivileged user on the
// filesystem containing path, which must exist and be a directory.
func Free(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("disk: statfs %s: %w", path, err)
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// FSKey returns a stable identifier of the filesystem containing path,
// suitable for matching reservations against each other: the device number
// (Stat_t.Dev) of the directory. Two paths share a filesystem exactly when
// their keys are equal.
func FSKey(path string) (string, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return "", fmt.Errorf("disk: fs key of %s: %w", path, err)
	}
	return strconv.FormatUint(uint64(st.Dev), 10), nil
}

func isDiskFull(err error) bool {
	return errors.Is(err, unix.ENOSPC)
}
