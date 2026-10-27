package update

import (
	"fmt"
	"strings"
	"sync"

	"github.com/edementhon/kids-shelf/internal/disk"
)

// Reservation job-ids the updater reserves under, in the shared jobs tables
// so the app's other jobs see them and the launcher can release them again
// at activation (the snapshot bytes are not on disk until then).
//
// stageReservationID guards the download plus the extracted binary against
// the install filesystem; the manager releases it as soon as the version
// directory is published.
// snapshotReservationID guards the activation snapshot (pre-<v>.db, the
// current database size) against the data directory; it is held from Stage
// until Activate writes the snapshot or the version is rejected, because
// the launcher takes over the disk usage only then.
func stageReservationID(v string) string    { return "update:" + v }
func snapshotReservationID(v string) string { return "update:snapshot:" + v }

// reservationStore is a minimal disk.Store for the manager's own
// reservations when the app has not wired its job-queue-backed book in:
// one slot per id, with the id's filesystem key, so the stage and snapshot
// reservations coexist, count per filesystem, and the stage's release
// cannot clear the snapshot's.
type reservationStore struct {
	mu   sync.Mutex
	byID map[string]uint64
	key  map[string]string
}

func (r *reservationStore) init() {
	if r.byID == nil {
		r.byID = map[string]uint64{}
		r.key = map[string]string{}
	}
}

// sumOthers sums the reservations on fsKey's filesystem excluding id; a
// reservation without a key counts against every filesystem, mirroring
// jobs.sumReservationsTx.
func (r *reservationStore) sumOthers(id, fsKey string) uint64 {
	var other uint64
	for otherID, b := range r.byID {
		if otherID == id {
			continue
		}
		if k := r.key[otherID]; k == "" || k == fsKey {
			other += b
		}
	}
	return other
}

func (r *reservationStore) trySet(id string, bytes, free, floor uint64, fsKey string) (other uint64, ok bool) {
	other = r.sumOthers(id, fsKey)
	// free - floor, guarded against underflow, like jobs.Queue.TryReserve.
	var available uint64
	if free > floor {
		available = free - floor
	}
	if other > available || bytes > available-other {
		return other, false
	}
	r.byID[id] = bytes
	r.key[id] = fsKey
	return other, true
}

// TryReserve is the work-job path of disk.Store; the fallback book has no
// work jobs, so like jobs.Queue it fails loudly for an id that was never
// created: the updater's reservations go through TryHoldReservation.
func (r *reservationStore) TryReserve(jobID string, bytes, free, floor uint64, fsKey string) (otherReserved uint64, ok bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.init()
	if _, exists := r.byID[jobID]; !exists {
		return 0, false, fmt.Errorf("update: no reservation slot for work job %q (standalone reservations use Hold)", jobID)
	}
	other, ok := r.trySet(jobID, bytes, free, floor, fsKey)
	return other, ok, nil
}

func (r *reservationStore) TryHoldReservation(id string, bytes, free, floor uint64, fsKey string) (otherReserved uint64, ok bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.init()
	other, ok := r.trySet(id, bytes, free, floor, fsKey)
	return other, ok, nil
}

func (r *reservationStore) SetReservedBytes(id string, bytes uint64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if bytes == 0 {
		delete(r.byID, id)
		delete(r.key, id)
		return nil
	}
	r.init()
	r.byID[id] = bytes
	r.key[id] = ""
	return nil
}

func (r *reservationStore) HeldReservationIDs(prefix string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	for id := range r.byID {
		if strings.HasPrefix(id, prefix) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// updateHoldKeep returns the journal rule for which updater holds survive
// reconciliation: only the snapshot hold of the staged or trying version,
// because the launcher turns those reserved bytes into a real pre-<v>.db
// only then. Every other update:* hold - the staging hold of any stage,
// and the snapshot hold of a version that is neither staged nor in trial
// (never staged, rejected or rolled back) - is a leftover of a crash
// before the deferred release and must be released.
func updateHoldKeep(st State) func(id string) bool {
	return func(id string) bool {
		v, ok := strings.CutPrefix(id, "update:snapshot:")
		if !ok {
			return false
		}
		return v == st.Staged || (st.Trying != nil && st.Trying.Version == v)
	}
}

// reconcileHolds syncs the shared reservation book with the journal: only
// the snapshot hold of the staged or trying version survives, every other
// update:* hold is released. It runs at updater start and at launcher
// activation, so a crash after journaling "staged" but before the deferred
// release cannot leave the staging hold behind forever.
func (m *Manager) reconcileHolds(st State) error {
	return m.reservations().Reconcile("update:", updateHoldKeep(st))
}

// reservations returns the disk reservation book the manager reserves
// against, creating the single-process fallback that obeys Manager.Free and
// Floor when the app's shared book was not wired in.
func (m *Manager) reservations() *disk.Reservations {
	if m.Reservations != nil {
		return m.Reservations
	}
	m.Reservations = disk.NewReservations(&reservationStore{}).
		WithFree(func(path string) (uint64, error) { return m.free(path) }).
		WithFloor(m.floor())
	return m.Reservations
}
