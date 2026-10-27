//go:build windows

package store

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// lockDB opens db+".lock" (creating it if needed) and takes an exclusive
// byte-range LockFileEx; wait bounds how long a concurrent holder may keep
// us waiting (0 = one non-blocking attempt). The OS drops the lock when the
// process exits or the handle closes.
func lockDB(db string, wait time.Duration) (*os.File, error) {
	f, err := os.OpenFile(LockFile(db), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	// The Overlapped must stay referenced for the whole wait loop, even
	// though every LockFileEx call here returns synchronously.
	ol := new(windows.Overlapped)
	deadline := time.Now().Add(wait)
	for {
		err := windows.LockFileEx(
			windows.Handle(f.Fd()),
			windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
			0,
			1,
			0,
			ol,
		)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			f.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, errLockBusy
		}
		time.Sleep(50 * time.Millisecond)
	}
}
