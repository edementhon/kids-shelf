//go:build !windows

package launch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// lock holds the exclusive instance lock for the lifetime of the process.
type lock struct {
	f *os.File
}

// acquireLock takes the instance lock, <dir>/instance.lock.
func acquireLock(dir string) (*lock, error) {
	return lockPath(filepath.Join(dir, lockFileName))
}

// lockPath opens path (creating it if needed) and takes a non-blocking
// exclusive flock. The kernel drops the lock when the process exits or the
// file is closed, so a crash never leaves a stale lock. It returns
// errLockBusy when another process already holds the lock.
func lockPath(path string) (*lock, error) {
	name := filepath.Base(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", name, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errLockBusy
		}
		return nil, fmt.Errorf("locking %s: %w", name, err)
	}
	return &lock{f: f}, nil
}

// Close releases the lock. The OS also releases it if the process dies,
// which is what makes the lock crash-safe.
func (l *lock) Close() error {
	return l.f.Close()
}

// addrInUse reports whether a failed net.Listen means the address is
// already in use; unix returns EADDRINUSE for that.
func addrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}
