//go:build windows

package install

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// defaultRenameOver replaces dst with src on Windows: os.Rename uses
// MoveFileEx with MOVEFILE_REPLACE_EXISTING, the atomic swap the updater
// relies on too.
func defaultRenameOver(src, dst string) error {
	return os.Rename(src, dst)
}

// defaultRenameRetryable reports whether a failed rename means the
// destination is still in use: replacing a running or mapped executable
// fails with a sharing violation or access denied, and a still-exiting
// launcher (which no longer holds the locks) is exactly that case. The
// installer retries such failures for launcherReplaceWindow (R6).
func defaultRenameRetryable(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

// fsyncDir is a no-op on Windows: os.Rename (MoveFileEx) already makes the
// rename durable, and FlushFileBuffers on a directory handle is not
// supported. The temp version directory is still "synced" for symmetry so
// the publication sequence reads the same on every OS.
func fsyncDir(string) error { return nil }

// defaultDeleteRetryable reports whether a failed deletion means a process
// still has the file mapped: removing a running or still-exiting executable
// fails with a sharing violation or access denied, and holding the locks
// does not prove every process that could launch it is gone (review round 4
// finding 11).
func defaultDeleteRetryable(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
