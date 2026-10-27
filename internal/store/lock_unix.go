//go:build !windows

package store

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// lockDB opens db+".lock" (creating it if needed) and takes an exclusive
// flock; wait bounds how long a concurrent holder may keep us waiting (0 =
// one non-blocking attempt). The kernel drops the lock when the process
// exits or the file is closed.
func lockDB(db string, wait time.Duration) (*os.File, error) {
	f, err := os.OpenFile(LockFile(db), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
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
