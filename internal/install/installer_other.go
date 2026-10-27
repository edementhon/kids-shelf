//go:build !windows

package install

import "os"

// defaultRenameOver is one atomic replace on unix: rename(2) replaces an
// existing destination in one step.
func defaultRenameOver(src, dst string) error {
	return os.Rename(src, dst)
}

// defaultRenameRetryable never retries on unix: a rename either succeeds or
// failed for a real, permanent reason.
func defaultRenameRetryable(error) bool { return false }

// fsyncDir fsyncs a directory so the entries it names persist together
// with the files themselves; on unix a directory handle can be opened and
// synced like a file. Windows needs no equivalent (installer_windows.go):
// its renames are already durable.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}

// defaultDeleteRetryable never retries on unix: a deletion either succeeds
// or failed for a real, permanent reason.
func defaultDeleteRetryable(error) bool { return false }
