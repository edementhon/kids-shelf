// Package disk measures free space and tracks the disk reservations that
// long-running jobs hold against a free-space floor.
//
// Reservations are numbers kept in the reserved_bytes column of the jobs
// table (work jobs) or in the reservations table (standalone holds such as
// the updater's), both owned by internal/jobs; this package never touches
// those tables directly. It talks to them through the Store interface,
// implemented by jobs.Queue, so table ownership stays with the owning
// package.
package disk

import (
	"fmt"
)

// DefaultFloor is the free space kept aside for the database, logs and other
// non-job writes when no other floor is configured: 2 GiB.
const DefaultFloor uint64 = 2 << 30 // 2 GiB

// ErrNoSpace reports a reservation denied for lack of free space.
type ErrNoSpace struct {
	Need  uint64 // bytes the job wanted to reserve
	Free  uint64 // free space measured on the job's directory
	Floor uint64 // free space kept aside for non-job writes
}

func (e *ErrNoSpace) Error() string {
	return fmt.Sprintf("disk: no space: need %d bytes, %d free, %d kept as floor", e.Need, e.Free, e.Floor)
}

// IsDiskFull reports whether err is a disk-full error (ENOSPC on unix,
// ERROR_DISK_FULL / ERROR_HANDLE_DISK_FULL on Windows), through any wrapping.
func IsDiskFull(err error) bool {
	return isDiskFull(err)
}

// Store is the slice of the job queue reservations need. It is implemented by
// jobs.Queue.
type Store interface {
	// TryReserve atomically records bytes against jobID - a work job that
	// must exist - provided the bytes other active jobs and standalone
	// holds on jobID's filesystem (fsKey) have reserved leave room for them
	// above floor; ok reports whether the reservation was recorded.
	TryReserve(jobID string, bytes, free, floor uint64, fsKey string) (otherReserved uint64, ok bool, err error)
	// TryHoldReservation atomically records a standalone reservation (no
	// work job behind it) under id with the same per-filesystem free-space
	// check as TryReserve. Holds persist across process restarts.
	TryHoldReservation(id string, bytes, free, floor uint64, fsKey string) (otherReserved uint64, ok bool, err error)
	// SetReservedBytes records jobID's reservation in reserved_bytes; 0
	// releases it (a standalone hold included).
	SetReservedBytes(jobID string, bytes uint64) error
	// HeldReservationIDs returns the ids of every standalone reservation
	// whose id has the given prefix ("" matches all), for reconciling
	// holds against the state that justifies them.
	HeldReservationIDs(prefix string) ([]string, error)
}

// Reservations accounts each job's worst-case disk need (output + temp +
// original kept + index) in the jobs tables, summed across concurrent jobs
// per filesystem, and refuses a reservation that would push free space below
// the floor.
//
// The check-and-set pair runs inside one jobs-owned transaction (Queue's
// TryReserve / TryHoldReservation), so concurrent Reserve calls - even from
// separate managers over the same queue - can never oversubscribe.
type Reservations struct {
	store Store
	free  func(path string) (uint64, error)
	fsKey func(path string) (string, error)
	floor uint64
}

// NewReservations returns a Reservations that measures free space with Free,
// attributes reservations to filesystems with FSKey and keeps DefaultFloor
// aside.
func NewReservations(s Store) *Reservations {
	return &Reservations{store: s, free: Free, fsKey: FSKey, floor: DefaultFloor}
}

// WithFree overrides the free-space probe (used by tests); a nil probe keeps
// the current one.
func (r *Reservations) WithFree(fn func(path string) (uint64, error)) *Reservations {
	if fn != nil {
		r.free = fn
	}
	return r
}

// WithFSKey overrides the filesystem-key probe (used by tests); a nil probe
// keeps the current one.
func (r *Reservations) WithFSKey(fn func(path string) (string, error)) *Reservations {
	if fn != nil {
		r.fsKey = fn
	}
	return r
}

// WithFloor overrides the free-space floor kept aside for non-job writes.
func (r *Reservations) WithFloor(bytes uint64) *Reservations {
	r.floor = bytes
	return r
}

// Reserve records bytes against jobID on dir, provided free space on dir
// minus the bytes other jobs and holds on the same filesystem have already
// reserved minus the floor still covers them; otherwise it returns
// ErrNoSpace. The reservation is set to exactly bytes, so re-reserving
// replaces the previous amount. jobID must be a real work job.
func (r *Reservations) Reserve(jobID string, bytes uint64, dir string) error {
	return r.reserve(false, jobID, bytes, dir)
}

// Hold records a standalone reservation under id on dir - a reservation
// with no work job behind it, e.g. the updater's stage and activation
// snapshot. It uses the same per-filesystem check as Reserve and survives
// the process that made it, so an update staged while the app runs keeps
// guarding its bytes across the restart into the launcher.
func (r *Reservations) Hold(id string, bytes uint64, dir string) error {
	return r.reserve(true, id, bytes, dir)
}

func (r *Reservations) reserve(hold bool, id string, bytes uint64, dir string) error {
	free, err := r.free(dir)
	if err != nil {
		return fmt.Errorf("disk: measure %s: %w", dir, err)
	}
	key, err := r.fsKey(dir)
	if err != nil {
		return fmt.Errorf("disk: filesystem key of %s: %w", dir, err)
	}
	var ok bool
	if hold {
		_, ok, err = r.store.TryHoldReservation(id, bytes, free, r.floor, key)
	} else {
		_, ok, err = r.store.TryReserve(id, bytes, free, r.floor, key)
	}
	if err != nil {
		return err
	}
	if !ok {
		return &ErrNoSpace{Need: bytes, Free: free, Floor: r.floor}
	}
	return nil
}

// Release clears jobID's reservation (a standalone hold included).
func (r *Reservations) Release(jobID string) error {
	if err := r.store.SetReservedBytes(jobID, 0); err != nil {
		return fmt.Errorf("disk: release %s: %w", jobID, err)
	}
	return nil
}

// Reconcile releases every standalone hold whose id has the given prefix
// unless keep accepts it. It is the startup cleanup of updater holds the
// update journal no longer justifies: a crash after journaling "staged"
// but before the deferred release leaves the staging hold behind forever,
// and the snapshot hold of a version that is neither staged nor in trial
// reserves bytes nothing will use. Holds keep accepts are left untouched;
// a released hold frees its bytes like any other Release.
func (r *Reservations) Reconcile(prefix string, keep func(id string) bool) error {
	ids, err := r.store.HeldReservationIDs(prefix)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if keep(id) {
			continue
		}
		if err := r.store.SetReservedBytes(id, 0); err != nil {
			return err
		}
	}
	return nil
}
