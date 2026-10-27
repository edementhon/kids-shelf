//go:build windows

package launch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// lock holds the exclusive instance lock for the lifetime of the process.
type lock struct {
	f  *os.File
	ol *windows.Overlapped
}

// acquireLock takes the instance lock, <dir>/instance.lock.
func acquireLock(dir string) (*lock, error) {
	return lockPath(filepath.Join(dir, lockFileName))
}

// lockPath opens path (creating it if needed) and takes a non-blocking
// exclusive LockFileEx. The OS drops the lock when the handle closes or the
// process dies, so a crash never leaves a stale lock. It returns
// errLockBusy when another process already holds the lock.
func lockPath(path string) (*lock, error) {
	name := filepath.Base(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", name, err)
	}
	// The Overlapped stays referenced by the lock for the whole lifetime,
	// even though LockFileEx returns synchronously here (fail immediately).
	ol := new(windows.Overlapped)
	err = windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		ol,
	)
	if err != nil {
		f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, errLockBusy
		}
		return nil, fmt.Errorf("locking %s: %w", name, err)
	}
	return &lock{f: f, ol: ol}, nil
}

// addrInUse reports whether a failed net.Listen means the address is
// already in use. Windows bind conflicts surface as WSAEADDRINUSE or, when
// another socket without SO_REUSEADDR holds the port, WSAEACCES.
func addrInUse(err error) bool {
	return errors.Is(err, windows.WSAEADDRINUSE) || errors.Is(err, windows.WSAEACCES)
}

// Close releases the lock. The OS also releases it if the process dies,
// which is what makes the lock crash-safe.
func (l *lock) Close() error {
	return l.f.Close()
}
