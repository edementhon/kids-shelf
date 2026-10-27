package launch

import (
	"errors"
	"io"
	"path/filepath"
	"time"
)

// InstallLockName is the installation-scope lock in the install root
// (PLAN 1a "Launcher"): the supervisor holds it for its whole life, and the
// installer and remover hold it while they change the installation. It is
// always taken before the data dir's instance lock, and never unlinked.
const InstallLockName = "install.lock"

// ErrBusy reports that another process holds the lock.
var ErrBusy = errLockBusy

// lockPoll is how often LockInstall and LockInstance retry a busy lock.
const lockPoll = 100 * time.Millisecond

// LockInstall takes <installDir>/install.lock, retrying a busy lock for up
// to wait (0: one attempt). It returns ErrBusy when the lock is still held.
// Close releases it; so does the process dying.
func LockInstall(installDir string, wait time.Duration) (io.Closer, error) {
	return lockWithin(filepath.Join(installDir, InstallLockName), wait)
}

// LockInstance takes the data dir's instance lock the same way.
func LockInstance(dataDir string, wait time.Duration) (io.Closer, error) {
	return lockWithin(filepath.Join(dataDir, lockFileName), wait)
}

func lockWithin(path string, wait time.Duration) (io.Closer, error) {
	deadline := time.Now().Add(wait)
	for {
		l, err := lockPath(path)
		if err == nil {
			return l, nil
		}
		if !errors.Is(err, errLockBusy) || !time.Now().Before(deadline) {
			return nil, err
		}
		time.Sleep(lockPoll)
	}
}
