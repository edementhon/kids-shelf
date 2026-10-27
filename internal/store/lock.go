// Database ownership fence: whoever opens the database holds an exclusive
// OS lock on <db>.lock for as long as its Store is open, and the kernel
// drops the lock when the owning process exits, for any reason. Open takes
// the lock itself; the launcher takes it explicitly (LockDB) before
// restoring a database snapshot, so a restore can never run under a process
// that still has the database open.
package store

import (
	"errors"
	"fmt"
	"time"
)

// ErrDBBusy reports that another process holds the database's ownership
// lock and did not release it within the requested wait.
var ErrDBBusy = errors.New("database is in use by another process")

// errLockBusy is the platform layer's busy signal, mapped to ErrDBBusy by
// LockDB.
var errLockBusy = errors.New("lock is already held")

// LockFile returns the path of the database's ownership lock file.
func LockFile(db string) string { return db + ".lock" }

// LockDB takes the exclusive ownership lock that Open holds for the
// lifetime of a Store, waiting up to wait for a concurrent holder; pass 0
// for a single non-blocking attempt, which is what store.Open uses. It
// returns a release function that drops the lock; the kernel also drops it
// if the process exits, so a crash never leaves a stale lock. LockDB fails
// with ErrDBBusy when another process still holds the lock after wait.
func LockDB(db string, wait time.Duration) (func(), error) {
	f, err := lockDB(db, wait)
	if err != nil {
		if errors.Is(err, errLockBusy) {
			return nil, fmt.Errorf("%w: %s (%s)", ErrDBBusy, db, LockFile(db))
		}
		return nil, fmt.Errorf("locking %s: %w", LockFile(db), err)
	}
	return func() { f.Close() }, nil
}
