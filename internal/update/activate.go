package update

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/edementhon/kids-shelf/internal/disk"
	"github.com/edementhon/kids-shelf/internal/jobs"
	"github.com/edementhon/kids-shelf/internal/store"
)

// Migrations registers the tables this package owns with the store: a
// durable audit trail of confirmed versions. The journal in <data>/update/
// is the rolling record; the database keeps the trail, and a rollback
// restores the database to the snapshot, which is exactly the point.
var Migrations = []store.Migration{{
	Owner:   "update",
	Version: 1,
	SQL: `
CREATE TABLE IF NOT EXISTS update_applied (
	version    TEXT PRIMARY KEY,
	applied_at TEXT NOT NULL
)`,
}}

// Activate is the launcher-side start of an update. Call it with the
// instance lock held and no child running, before deciding which build to
// run. It returns the version to run ("" = the launcher build itself):
// a staged version is activated (snapshot reservation refreshed against
// the current free space, database snapshot taken, trying journaled), and a
// leftover "trying" from a previous start is treated as a failed trial and
// rolled back first. A disk too short for the snapshot refuses the
// activation: the current version keeps running and the version stays
// staged for the next launch.
func Activate(installDir, dataDir, dbPath string) (versionToRun string, err error) {
	st, err := ReadState(dataDir)
	if err != nil {
		return "", fmt.Errorf("update: reading the journal: %w", err)
	}
	// Reconcile the shared reservation book against the journal before
	// any activation logic: a crash after journaling "staged" but before
	// the manager's deferred release leaves the staging hold behind
	// forever, and the snapshot hold of a version the journal no longer
	// points at guards nothing. Only the staged or trying version's
	// snapshot hold survives.
	reconcileStaleHolds(dbPath, st)
	if st.Trying != nil {
		if rerr := rejectTrial(dataDir, dbPath, st); rerr != nil {
			return "", rerr
		}
		st, err = ReadState(dataDir)
		if err != nil {
			return "", fmt.Errorf("update: reading the journal: %w", err)
		}
	}
	if st.Staged == "" {
		return st.Current, nil
	}
	v := st.Staged
	if !validVersion(v) {
		return "", fmt.Errorf("update: staged version %q is not usable", v)
	}

	// Snapshot the database before anything points at the new version.
	snapshot := filepath.Join(filepath.Join(dataDir, "update"), "pre-"+v+".db")
	snapErr := (func() error {
		if err := os.MkdirAll(filepath.Dir(snapshot), 0o755); err != nil {
			return err
		}
		if err := os.Remove(snapshot); err != nil && !isNotExist(err) {
			return fmt.Errorf("removing stale snapshot %s: %w", snapshot, err)
		}
		s, err := store.Open(dbPath)
		if err != nil {
			return fmt.Errorf("opening the database for snapshot: %w", err)
		}
		// Refresh the snapshot reservation the app's stage held: the disk
		// may have filled up since it was taken, and snapshotting over a
		// short disk is exactly the failure the reservation exists to
		// prevent. The refresh re-holds the same bytes against the current
		// free space (also covering a stage that ran without the shared
		// book wired in).
		if fi, serr := os.Stat(dbPath); serr == nil && fi.Size() > 0 {
			if rerr := refreshSnapshotReservationOn(s, dataDir, v, uint64(fi.Size())); rerr != nil {
				s.Close()
				return rerr
			}
		}
		if err := s.Snapshot(snapshot); err != nil {
			s.Close()
			os.Remove(snapshot)
			return fmt.Errorf("snapshotting the database: %w", err)
		}
		// pre-<v>.db is on disk: the reservation the app's stage kept for
		// the snapshot bytes is released here, so it stops counting
		// against the floor of future updates.
		releaseSnapshotReservationOn(s, v)
		return s.Close()
	})()
	if snapErr != nil {
		var noSpace *disk.ErrNoSpace
		if errors.As(snapErr, &noSpace) {
			// The disk filled up since staging: the version itself is
			// fine, so do not reject it - stay on the current version,
			// keep it staged, and let the next launch re-attempt once
			// space frees up. The plain message goes to stderr, where the
			// launcher shows startup failures visibly.
			current := st.Current
			if current == "" {
				current = "the installed version"
			}
			fmt.Fprintf(os.Stderr, "kids-shelf: not enough free space to snapshot the database before activating %s (%v); staying on %s\n", v, snapErr, current)
			return st.Current, nil
		}
		// Without a snapshot a trial can never be rolled back, so the
		// staged version must not run: do not activate it. Mark it failed
		// (never retried), keep the journal free of any trial, and run the
		// current version; the plain message goes to stderr because the
		// launcher shows startup failures visibly.
		st.Rejected = addRejected(st.Rejected, v)
		st.Staged = ""
		// The version is rejected and will never be activated: its
		// snapshot reservation is done too (best effort - the database
		// itself may be what failed).
		releaseSnapshotReservation(dbPath, v)
		if werr := writeState(dataDir, st); werr != nil {
			return "", fmt.Errorf("update: journaling the failed activation of %s: %w", v, werr)
		}
		current := st.Current
		if current == "" {
			current = "the installed version"
		}
		fmt.Fprintf(os.Stderr, "kids-shelf: could not snapshot the database before activating %s (%v); staying on %s\n", v, snapErr, current)
		return st.Current, nil
	}

	st.Previous = st.Current
	st.Staged = ""
	st.Trying = &Trial{Version: v, Snapshot: snapshot}
	if err := writeState(dataDir, st); err != nil {
		return "", fmt.Errorf("update: journaling the trial: %w", err)
	}
	return v, nil
}

// Confirm settles a trial whose version has served /healthz for the healthy
// window: current, previous and the high-water mark advance and the trial
// clears. It is idempotent: a trial that already settled is re-synced
// durably (write + fsync file + fsync dir) before returning success, so a
// crash can never lose a settled journal behind a Confirm the launcher
// counted as done.
func Confirm(dataDir, v string) error {
	st, err := ReadState(dataDir)
	if err != nil {
		return fmt.Errorf("update: reading the journal: %w", err)
	}
	if st.Trying == nil || st.Trying.Version != v {
		// Already settled (or never a trial of v): the settlement must
		// still be synced to disk before this call counts as success.
		if err := writeState(dataDir, st); err != nil {
			return fmt.Errorf("update: re-syncing the journal: %w", err)
		}
		return nil
	}
	recordApplied(DatabasePath(dataDir), v)
	st.Current = v
	st.HighWater = v
	st.Staged = ""
	st.Trying = nil
	if err := writeState(dataDir, st); err != nil {
		return fmt.Errorf("update: journaling the confirmation: %w", err)
	}
	return nil
}

// Reject ends a failed trial: the database snapshot is restored (the caller
// has killed and waited for the failing child), the version is marked
// rejected so it is never tried again, and the trial clears. Idempotent.
func Reject(dataDir, v string) error {
	st, err := ReadState(dataDir)
	if err != nil {
		return fmt.Errorf("update: reading the journal: %w", err)
	}
	if st.Trying == nil || st.Trying.Version != v {
		return nil
	}
	return rejectTrial(dataDir, DatabasePath(dataDir), st)
}

// rejectTrial restores the trial's snapshot over the database and journals
// the version as rejected with the trial cleared.
func rejectTrial(dataDir, dbPath string, st State) error {
	if st.Trying.Snapshot != "" {
		if err := restoreDB(st.Trying.Snapshot, dbPath); err != nil {
			return fmt.Errorf("update: restoring the database from %s: %w", st.Trying.Snapshot, err)
		}
	}
	// The rejected version will never be activated: release the snapshot
	// reservation its stage kept (idempotent - Activate may already have
	// released it after writing the snapshot).
	releaseSnapshotReservation(dbPath, st.Trying.Version)
	st.Rejected = addRejected(st.Rejected, st.Trying.Version)
	tried := st.Trying.Version
	st.Trying = nil
	if err := writeState(dataDir, st); err != nil {
		return fmt.Errorf("update: journaling %s as rejected: %w", tried, err)
	}
	return nil
}

// AbandonTrial ends an unconfirmed trial without rejecting the candidate:
// the pre-trial snapshot is restored over the database (migrations may have
// run while the child was up), the version goes back to Staged, Trying
// clears and no Rejected entry is written, so the next launch tries the
// candidate again. The supervisor calls this when it gives up on a run of
// ExitBindFailed results: the private ports being taken is the
// environment's fault, not the candidate's, so rejecting it would be wrong.
// Idempotent: no trial, nothing happens.
func AbandonTrial(dataDir string) error {
	st, err := ReadState(dataDir)
	if err != nil {
		return fmt.Errorf("update: reading the journal: %w", err)
	}
	if st.Trying == nil {
		return nil
	}
	v := st.Trying.Version
	dbPath := DatabasePath(dataDir)
	if st.Trying.Snapshot != "" {
		if err := restoreDB(st.Trying.Snapshot, dbPath); err != nil {
			return fmt.Errorf("update: restoring the database from %s: %w", st.Trying.Snapshot, err)
		}
	}
	// The snapshot reservation no longer guards an in-flight trial; the
	// next Activate re-holds it before re-snapshotting. Best effort, like
	// the trial-reject path.
	releaseSnapshotReservation(dbPath, v)
	st.Staged = v
	st.Trying = nil
	if err := writeState(dataDir, st); err != nil {
		return fmt.Errorf("update: journaling %s back to staged: %w", v, err)
	}
	return nil
}

// addRejected appends v to rejected when it is not already there.
func addRejected(rejected []string, v string) []string {
	out := append([]string(nil), rejected...)
	dup := false
	for _, r := range out {
		if r == v {
			dup = true
			break
		}
	}
	if !dup {
		out = append(out, v)
	}
	return out
}

// restoreDB replaces db with the snapshot: the copy streams to a temp file
// and swaps in with one atomic replace, then the restored image and its
// directory are fsynced and stale WAL/SHM files are dropped so a
// crash-recovered journal from the failed version can never replay over
// the restored image.
//
// The launcher takes the database's ownership lock first (the same lock
// store.Open holds for a live connection), waiting restoreLockWait for a
// still-open database: restoring under a process that has the database
// open would corrupt it, so after the wait the restore is refused with a
// plain error instead.
func restoreDB(snapshot, db string) error {
	unlock, err := store.LockDB(db, restoreLockWait)
	if err != nil {
		return fmt.Errorf("the database cannot be locked for the restore: %w", err)
	}
	defer unlock()
	tmp := db + ".restore"
	in, err := os.Open(snapshot)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := replace(tmp, db); err != nil {
		os.Remove(tmp)
		return err
	}
	// The restore must survive a crash before the journal is rewritten:
	// fsync the restored image itself (the rename made it a new inode)
	// and the directory that holds it.
	rf, err := os.OpenFile(db, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := rf.Sync(); err != nil {
		rf.Close()
		return err
	}
	if err := rf.Close(); err != nil {
		return err
	}
	if err := syncParent(db); err != nil {
		return err
	}
	// Stale WAL/SHM files must go; any deletion error other than
	// not-exist fails the whole restore, so the journal still says trying
	// and the launcher shows the error rather than clearing the trial
	// over a database that could replay the failed version's journal.
	if err := os.Remove(db + "-wal"); err != nil && !isNotExist(err) {
		return fmt.Errorf("removing %s-wal: %w", db, err)
	}
	if err := os.Remove(db + "-shm"); err != nil && !isNotExist(err) {
		return fmt.Errorf("removing %s-shm: %w", db, err)
	}
	// The removals themselves must survive a crash before the journal
	// clears Trying: fsync the directory again, now that the stale WAL/SHM
	// files are gone.
	return syncRestoredDir(db)
}

// recordApplied appends v to the update_applied audit table. It is
// best-effort: the journal is the source of truth, so a failure here is
// silently dropped.
func recordApplied(dbPath, v string) {
	s, err := store.Open(dbPath)
	if err != nil {
		return
	}
	defer s.Close()
	if err := s.Migrate(Migrations...); err != nil {
		return
	}
	s.DB().Exec(
		`INSERT OR IGNORE INTO update_applied (version, applied_at) VALUES (?, ?)`,
		v, time.Now().UTC().Format(time.RFC3339),
	)
}

// restoreLockWait bounds how long a restore waits for another process to
// release the database's ownership lock before it refuses; a var so tests
// can shorten it.
var restoreLockWait = 15 * time.Second

// syncRestoredDir fsyncs the database's directory after the stale
// -wal/-shm files are removed, so their removal survives a crash before
// the journal clears the trial. A var so the restore test can fail it at
// exactly that point and pin the durability ordering.
var syncRestoredDir = syncParent

// snapshotFree measures free space on the data directory for the
// activation-time refresh of the snapshot reservation; a var so tests can
// make the refresh see a short disk.
var snapshotFree = disk.Free

// refreshSnapshotReservationOn re-checks the staged version's snapshot
// reservation against the filesystem's current free space before the
// launcher snapshots the database: the app's stage held it once, but the
// disk may have filled up since. The refresh re-holds the same bytes (the
// hold is idempotent), and a refusal surfaces as a disk.ErrNoSpace, which
// Activate answers by staying on the current version without rejecting the
// stage.
func refreshSnapshotReservationOn(s *store.Store, dataDir, v string, dbSize uint64) error {
	q, err := jobs.New(s)
	if err != nil {
		return err
	}
	free, err := snapshotFree(dataDir)
	if err != nil {
		return fmt.Errorf("update: measuring free space before the snapshot: %w", err)
	}
	key, err := disk.FSKey(dataDir)
	if err != nil {
		return fmt.Errorf("update: filesystem key of %s: %w", dataDir, err)
	}
	if _, ok, err := q.TryHoldReservation(snapshotReservationID(v), dbSize, free, disk.DefaultFloor, key); err != nil {
		return err
	} else if !ok {
		return &disk.ErrNoSpace{Need: dbSize, Free: free, Floor: disk.DefaultFloor}
	}
	return nil
}

// releaseSnapshotReservation clears the snapshot reservation the app's
// stage made for v: the snapshot bytes are now on disk (pre-<v>.db was
// written by Activate) or v is rejected and will never be activated, so
// they stop counting against the update floor. Best effort: the
// reservation is advisory accounting, and a database that cannot be opened
// here is already failing the restore or the snapshot.
func releaseSnapshotReservation(dbPath, v string) {
	s, err := store.Open(dbPath)
	if err != nil {
		return
	}
	defer s.Close()
	releaseSnapshotReservationOn(s, v)
}

// releaseSnapshotReservationOn is releaseSnapshotReservation with the
// store already open (the launcher holds it for the snapshot step).
func releaseSnapshotReservationOn(s *store.Store, v string) {
	q, err := jobs.New(s)
	if err != nil {
		return
	}
	_ = q.SetReservedBytes(snapshotReservationID(v), 0)
}

// reconcileStaleHolds releases every updater hold in the launcher-side
// shared book (the jobs queue over the app database) that the journal no
// longer justifies: the staging hold of a stage that crashed after
// journaling "staged" but before its deferred release, and the snapshot
// hold of a version that is neither staged nor in trial. Only the staged
// or trying version's snapshot hold survives. Best effort, like
// releaseSnapshotReservation: a database that cannot be opened here
// (including a fresh install that has not created one yet) has no holds to
// reconcile, and the holds are advisory accounting anyway.
func reconcileStaleHolds(dbPath string, st State) {
	if _, err := os.Stat(dbPath); err != nil {
		return
	}
	s, err := store.Open(dbPath)
	if err != nil {
		return
	}
	defer s.Close()
	q, err := jobs.New(s)
	if err != nil {
		return
	}
	_ = disk.NewReservations(q).Reconcile("update:", updateHoldKeep(st))
}
