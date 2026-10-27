package update

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edementhon/kids-shelf/internal/jobs"
	"github.com/edementhon/kids-shelf/internal/store"
)

// TestActivateStagesTrialAndSnapshots: a staged version turns into a trial
// with a database snapshot, and Activate returns that version to run.
func TestActivateStagesTrialAndSnapshots(t *testing.T) {
	install := setupInstall(t)
	dataDir := t.TempDir()
	seedDB(t, dataDir)
	db := DatabasePath(dataDir)

	if err := writeState(dataDir, State{Staged: testTag}); err != nil {
		t.Fatal(err)
	}
	v, err := Activate(install, dataDir, db)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if v != testTag {
		t.Errorf("version to run = %q, want %s", v, testTag)
	}
	st, err := ReadState(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Trying == nil || st.Trying.Version != testTag {
		t.Fatalf("trying = %+v, want a trial of %s", st.Trying, testTag)
	}
	if st.Staged != "" {
		t.Errorf("staged = %q after activation, must be cleared", st.Staged)
	}
	if _, err := os.Stat(st.Trying.Snapshot); err != nil {
		t.Fatalf("snapshot missing: %v", err)
	}
	// The snapshot is a consistent copy of the database with the seeded row.
	snapDir := t.TempDir()
	restore := filepath.Join(snapDir, "restored.db")
	if err := copyFile(st.Trying.Snapshot, restore, 0o600); err != nil {
		t.Fatal(err)
	}
	rows := probeRowsAt(t, restore)
	if len(rows) != 1 || rows[0] != "old" {
		t.Errorf("snapshot rows = %v, want [old]", rows)
	}
	if got := probeRows(t, dataDir); len(got) != 1 || got[0] != "old" {
		t.Errorf("live database changed by the snapshot: %v", got)
	}
}

// TestActivateReturnsCurrentWhenNothingStaged: with no staged version the
// launcher runs the journaled current version ("" = the entry build).
func TestActivateReturnsCurrentWhenNothingStaged(t *testing.T) {
	install := setupInstall(t)
	dataDir := t.TempDir()
	if err := writeState(dataDir, State{Current: testOldTag, HighWater: testOldTag}); err != nil {
		t.Fatal(err)
	}
	v, err := Activate(install, dataDir, DatabasePath(dataDir))
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if v != testOldTag {
		t.Errorf("version to run = %q, want %s", v, testOldTag)
	}

	if err := writeState(dataDir, State{}); err != nil {
		t.Fatal(err)
	}
	v, err = Activate(install, dataDir, DatabasePath(dataDir))
	if err != nil {
		t.Fatalf("Activate with an empty journal: %v", err)
	}
	if v != "" {
		t.Errorf("version to run on a fresh install = %q, want the entry build", v)
	}
}

// TestConfirmAdvancesJournal: a healthy trial advances current, previous
// and the high-water mark and records the audit row.
func TestConfirmAdvancesJournal(t *testing.T) {
	install := setupInstall(t)
	dataDir := t.TempDir()
	seedDB(t, dataDir)
	if err := writeState(dataDir, State{Current: testOldTag, HighWater: testOldTag, Staged: testTag}); err != nil {
		t.Fatal(err)
	}
	if _, err := Activate(install, dataDir, DatabasePath(dataDir)); err != nil {
		t.Fatal(err)
	}
	if err := Confirm(dataDir, testTag); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	st, _ := ReadState(dataDir)
	if st.Current != testTag || st.HighWater != testTag || st.Previous != testOldTag {
		t.Errorf("journal = %+v, want current=%s high_water=%s previous=%s", st, testTag, testTag, testOldTag)
	}
	if st.Trying != nil || st.Staged != "" {
		t.Errorf("trial/staged not cleared: %+v", st)
	}
	if n := appliedCount(t, dataDir, testTag); n != 1 {
		t.Errorf("update_applied rows for %s = %d, want 1", testTag, n)
	}
}

// TestRejectRestoresDatabaseAndRejects: a failed trial restores the
// snapshot (undoing the new version's writes) and journals the version as
// rejected.
func TestRejectRestoresDatabaseAndRejects(t *testing.T) {
	install := setupInstall(t)
	dataDir := t.TempDir()
	seedDB(t, dataDir)
	if err := writeState(dataDir, State{Current: testOldTag, HighWater: testOldTag, Staged: testTag}); err != nil {
		t.Fatal(err)
	}
	if _, err := Activate(install, dataDir, DatabasePath(dataDir)); err != nil {
		t.Fatal(err)
	}

	// The new version runs: it migrates the schema and writes a second row,
	// then dies.
	s, err := openTestStore(t, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`CREATE TABLE probe_v2 (v TEXT)`); err != nil {
		s.Close()
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO probe (v) VALUES ('new')`); err != nil {
		s.Close()
		t.Fatal(err)
	}
	s.Close()

	if err := Reject(dataDir, testTag); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	rows := probeRows(t, dataDir)
	if len(rows) != 1 || rows[0] != "old" {
		t.Errorf("rows after rollback = %v, want [old]", rows)
	}
	// The failed version's table is gone with the restored snapshot.
	if _, err := os.Stat(filepath.Join(dataDir, "kids-shelf.db-wal")); !os.IsNotExist(err) {
		t.Error("stale -wal file survived the rollback")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "kids-shelf.db-shm")); !os.IsNotExist(err) {
		t.Error("stale -shm file survived the rollback")
	}
	st, _ := ReadState(dataDir)
	if len(st.Rejected) != 1 || st.Rejected[0] != testTag {
		t.Errorf("rejected = %v, want [%s]", st.Rejected, testTag)
	}
	if st.Trying != nil {
		t.Errorf("trying = %+v, must be cleared", st.Trying)
	}
	if st.Current != testOldTag || st.HighWater != testOldTag {
		t.Errorf("journal = %+v; current and high-water must not move on failure", st)
	}
}

// TestAbandonTrialRestoresDatabaseAndKeepsCandidateStaged: an abandoned
// trial undoes the new version's database writes exactly like a rollback,
// but the version goes back to staged instead of rejected: the next launch
// tries it again.
func TestAbandonTrialRestoresDatabaseAndKeepsCandidateStaged(t *testing.T) {
	install := setupInstall(t)
	dataDir := t.TempDir()
	seedDB(t, dataDir)
	if err := writeState(dataDir, State{Current: testOldTag, HighWater: testOldTag, Staged: testTag}); err != nil {
		t.Fatal(err)
	}
	if _, err := Activate(install, dataDir, DatabasePath(dataDir)); err != nil {
		t.Fatal(err)
	}

	// The new version ran: it migrated the schema and wrote a second row,
	// then the supervisor gave up on it for an environmental reason (for
	// example its private ports were taken).
	s, err := openTestStore(t, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`CREATE TABLE probe_v2 (v TEXT)`); err != nil {
		s.Close()
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO probe (v) VALUES ('new')`); err != nil {
		s.Close()
		t.Fatal(err)
	}
	s.Close()

	if err := AbandonTrial(dataDir); err != nil {
		t.Fatalf("AbandonTrial: %v", err)
	}
	rows := probeRows(t, dataDir)
	if len(rows) != 1 || rows[0] != "old" {
		t.Errorf("rows after abandon = %v, want [old]", rows)
	}
	// The new version's table is gone with the restored snapshot.
	if _, err := os.Stat(filepath.Join(dataDir, "kids-shelf.db-wal")); !os.IsNotExist(err) {
		t.Error("stale -wal file survived the abandon")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "kids-shelf.db-shm")); !os.IsNotExist(err) {
		t.Error("stale -shm file survived the abandon")
	}
	st, _ := ReadState(dataDir)
	if st.Trying != nil {
		t.Errorf("trying = %+v, must be cleared", st.Trying)
	}
	if st.Staged != testTag {
		t.Errorf("staged = %q, want %s (the candidate is tried again)", st.Staged, testTag)
	}
	if len(st.Rejected) != 0 {
		t.Errorf("rejected = %v, want none; an abandoned trial must not reject the candidate", st.Rejected)
	}
	if st.Current != testOldTag || st.HighWater != testOldTag {
		t.Errorf("journal = %+v; current and high-water must not move on abandonment", st)
	}
}

// TestAbandonTrialWithoutTrialIsNoop: no trial in the journal, nothing to
// undo; the call succeeds and the journal is untouched.
func TestAbandonTrialWithoutTrialIsNoop(t *testing.T) {
	dataDir := t.TempDir()
	if err := writeState(dataDir, State{Current: testOldTag, HighWater: testOldTag, Staged: testTag}); err != nil {
		t.Fatal(err)
	}
	if err := AbandonTrial(dataDir); err != nil {
		t.Fatalf("AbandonTrial without a trial: %v", err)
	}
	st, _ := ReadState(dataDir)
	if st.Staged != testTag || st.Trying != nil || len(st.Rejected) != 0 {
		t.Errorf("journal changed by a no-trial AbandonTrial: %+v", st)
	}
}

// TestLeftoverTryingRollsBackAtNextStart is the power-loss case: the
// journal says a trial was in flight; the next Activate treats it as failed
// and runs the previous version on a restored database.
func TestLeftoverTryingRollsBackAtNextStart(t *testing.T) {
	install := setupInstall(t)
	dataDir := t.TempDir()
	seedDB(t, dataDir)
	db := DatabasePath(dataDir)

	// Stage and activate like the launcher would, so the snapshot exists.
	if err := writeState(dataDir, State{Current: testOldTag, HighWater: testOldTag, Staged: testTag}); err != nil {
		t.Fatal(err)
	}
	if _, err := Activate(install, dataDir, db); err != nil {
		t.Fatal(err)
	}

	// The machine lost power mid-trial: the new version's writes are in the
	// live database, the journal still says trying.
	s, err := openTestStore(t, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO probe (v) VALUES ('new')`); err != nil {
		s.Close()
		t.Fatal(err)
	}
	s.Close()

	v, err := Activate(install, dataDir, db)
	if err != nil {
		t.Fatalf("Activate after power loss: %v", err)
	}
	if v != testOldTag {
		t.Errorf("version to run = %q, want the previous %s", v, testOldTag)
	}
	rows := probeRows(t, dataDir)
	if len(rows) != 1 || rows[0] != "old" {
		t.Errorf("rows after the leftover-trial rollback = %v, want [old]", rows)
	}
	st, _ := ReadState(dataDir)
	if st.Trying != nil {
		t.Errorf("trying = %+v, must be cleared", st.Trying)
	}
	if len(st.Rejected) != 1 || st.Rejected[0] != testTag {
		t.Errorf("rejected = %v, want [%s]", st.Rejected, testTag)
	}
}

// TestConfirmAndRejectAreIdempotent: settling an already-settled trial is a
// no-op.
func TestConfirmAndRejectAreIdempotent(t *testing.T) {
	install := setupInstall(t)
	dataDir := t.TempDir()
	if err := writeState(dataDir, State{Current: testOldTag, HighWater: testOldTag, Staged: testTag}); err != nil {
		t.Fatal(err)
	}
	if _, err := Activate(install, dataDir, DatabasePath(dataDir)); err != nil {
		t.Fatal(err)
	}
	if err := Confirm(dataDir, testTag); err != nil {
		t.Fatal(err)
	}
	if err := Confirm(dataDir, testTag); err != nil {
		t.Fatalf("second Confirm: %v", err)
	}
	if err := Reject(dataDir, testTag); err != nil {
		t.Fatalf("Reject after Confirm: %v", err)
	}
	st, _ := ReadState(dataDir)
	if st.Current != testTag || len(st.Rejected) != 0 {
		t.Errorf("journal = %+v, want confirmed %s and nothing rejected", st, testTag)
	}

	// And the other way: reject first, then reject again.
	if err := writeState(dataDir, State{Current: testOldTag, Staged: "v2.0.1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Activate(install, dataDir, DatabasePath(dataDir)); err != nil {
		t.Fatal(err)
	}
	if err := Reject(dataDir, "v2.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := Reject(dataDir, "v2.0.1"); err != nil {
		t.Fatalf("second Reject: %v", err)
	}
	st, _ = ReadState(dataDir)
	if got := len(st.Rejected); got != 1 {
		t.Errorf("rejected has %d entries, want 1 (no duplicates)", got)
	}
}

// TestActivateRejectsLeftoverTryingThenStages: with a leftover trial AND a
// fresh staged version, the leftover is rolled back first and the staged
// one activated.
func TestActivateRejectsLeftoverTryingThenStages(t *testing.T) {
	install := setupInstall(t)
	dataDir := t.TempDir()
	db := DatabasePath(dataDir)
	seedDB(t, dataDir)

	// The snapshot must exist for the rollback and predate the failed
	// version's writes; take it now, while the database has only 'old'.
	snap := filepath.Join(dataDir, "update", "pre-"+testTag+".db")
	if err := os.MkdirAll(filepath.Dir(snap), 0o755); err != nil {
		t.Fatal(err)
	}
	sts, err := openTestStore(t, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := sts.Snapshot(snap); err != nil {
		sts.Close()
		t.Fatal(err)
	}
	sts.Close()

	// The failed version's writes land in the live database after the
	// snapshot was taken.
	s2, err := openTestStore(t, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.DB().Exec(`INSERT INTO probe (v) VALUES ('new')`); err != nil {
		s2.Close()
		t.Fatal(err)
	}
	s2.Close()

	if err := writeState(dataDir, State{
		Current: testOldTag, HighWater: testOldTag,
		Trying: &Trial{Version: testTag, Snapshot: snap},
		Staged: "v2.0.1",
	}); err != nil {
		t.Fatal(err)
	}
	v, err := Activate(install, dataDir, db)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if v != "v2.0.1" {
		t.Errorf("version to run = %q, want the staged v2.0.1", v)
	}
	rows := probeRows(t, dataDir)
	if len(rows) != 1 || rows[0] != "old" {
		t.Errorf("rows = %v; the leftover trial must be rolled back before the new one starts", rows)
	}
	st, _ := ReadState(dataDir)
	if len(st.Rejected) != 1 || st.Rejected[0] != testTag {
		t.Errorf("rejected = %v, want [%s]", st.Rejected, testTag)
	}
	if st.Trying == nil || st.Trying.Version != "v2.0.1" {
		t.Errorf("trying = %+v, want a trial of v2.0.1", st.Trying)
	}
}

// TestSnapshotFailureRunsCurrentAndRejectsStaged: a staged version whose
// database snapshot fails must never be activated - the journal marks it
// rejected (never retried), no trial starts, and Activate returns the
// current version so the launcher keeps serving it.
func TestSnapshotFailureRunsCurrentAndRejectsStaged(t *testing.T) {
	install := setupInstall(t)
	dataDir := t.TempDir()
	seedDB(t, dataDir)
	if err := writeState(dataDir, State{Current: testOldTag, HighWater: testOldTag, Staged: testTag}); err != nil {
		t.Fatal(err)
	}
	// A non-empty directory at the snapshot path cannot be removed, so
	// the snapshot step fails deterministically (even as root).
	snap := filepath.Join(dataDir, "update", "pre-"+testTag+".db")
	if err := os.MkdirAll(snap, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snap, "junk"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	v, err := Activate(install, dataDir, DatabasePath(dataDir))
	if err != nil {
		t.Fatalf("Activate with a failing snapshot: %v", err)
	}
	if v != testOldTag {
		t.Errorf("version to run = %q, want the current %s", v, testOldTag)
	}
	st, _ := ReadState(dataDir)
	if st.Staged != "" {
		t.Errorf("staged = %q after a failed activation, must be cleared", st.Staged)
	}
	if st.Trying != nil {
		t.Errorf("trying = %+v; no trial may start without a snapshot", st.Trying)
	}
	if len(st.Rejected) != 1 || st.Rejected[0] != testTag {
		t.Errorf("rejected = %v, want [%s] (the staged version marked failed)", st.Rejected, testTag)
	}
}

// TestActivateShortDiskRefusesActivation: the activation refreshes the
// staged version's snapshot reservation against the current free space
// before snapshotting; when the disk cannot cover it, Activate must not
// activate (no trial, nothing rejected, still staged for the next launch)
// and the current version keeps running.
func TestActivateShortDiskRefusesActivation(t *testing.T) {
	install := setupInstall(t)
	dataDir := t.TempDir()
	seedDB(t, dataDir)
	db := DatabasePath(dataDir)
	if err := writeState(dataDir, State{Current: testOldTag, HighWater: testOldTag, Staged: testTag}); err != nil {
		t.Fatal(err)
	}
	oldFree := snapshotFree
	snapshotFree = func(string) (uint64, error) { return 1, nil } // below the floor
	t.Cleanup(func() { snapshotFree = oldFree })

	v, err := Activate(install, dataDir, db)
	if err != nil {
		t.Fatalf("Activate on a short disk: %v", err)
	}
	if v != testOldTag {
		t.Errorf("version to run = %q, want the current %s", v, testOldTag)
	}
	st, err := ReadState(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Staged != testTag {
		t.Errorf("staged = %q, want %s (kept staged for the next launch)", st.Staged, testTag)
	}
	if st.Trying != nil {
		t.Errorf("trying = %+v; no trial may start on a short disk", st.Trying)
	}
	if len(st.Rejected) != 0 {
		t.Errorf("rejected = %v; a short disk must not reject the staged version", st.Rejected)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "update", "pre-"+testTag+".db")); !os.IsNotExist(err) {
		t.Error("a snapshot was taken although the disk was too short")
	}
}

// TestRestoreWalRemovalFailureKeepsTrying: an -wal file that cannot be
// deleted (a non-empty directory here) fails the restore, the journal
// still says trying, and once the blocker is gone the retry completes.
func TestRestoreWalRemovalFailureKeepsTrying(t *testing.T) {
	install := setupInstall(t)
	dataDir := t.TempDir()
	seedDB(t, dataDir)
	db := DatabasePath(dataDir)
	if err := writeState(dataDir, State{Current: testOldTag, HighWater: testOldTag, Staged: testTag}); err != nil {
		t.Fatal(err)
	}
	if _, err := Activate(install, dataDir, db); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(db+"-wal", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(db+"-wal", "junk"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := Reject(dataDir, testTag)
	if err == nil || !strings.Contains(err.Error(), "-wal") {
		t.Fatalf("Reject = %v, want a -wal deletion error", err)
	}
	st, _ := ReadState(dataDir)
	if st.Trying == nil || st.Trying.Version != testTag {
		t.Errorf("trying = %+v; a failed restore must keep the trial open", st.Trying)
	}
	if err := os.RemoveAll(db + "-wal"); err != nil {
		t.Fatal(err)
	}
	if err := Reject(dataDir, testTag); err != nil {
		t.Fatalf("Reject after the blocker cleared: %v", err)
	}
	st, _ = ReadState(dataDir)
	if st.Trying != nil {
		t.Errorf("trying = %+v, must clear after the completed rollback", st.Trying)
	}
	if len(st.Rejected) != 1 || st.Rejected[0] != testTag {
		t.Errorf("rejected = %v, want [%s]", st.Rejected, testTag)
	}
	if rows := probeRows(t, dataDir); len(rows) != 1 || rows[0] != "old" {
		t.Errorf("rows after the completed rollback = %v, want [old]", rows)
	}
}

// TestRestoreRefusesWhileAnotherProcessHoldsDB: the launcher's restore
// takes the database's ownership lock (bounded wait); while a subprocess
// holds the database open the restore is refused with a clear error and
// the trial stays open, and once the holder exits the restore proceeds.
func TestRestoreRefusesWhileAnotherProcessHoldsDB(t *testing.T) {
	dataDir := t.TempDir()
	db := DatabasePath(dataDir)
	seedDB(t, dataDir)

	// Snapshot the 'old'-only database, like a previous Activate did.
	snap := filepath.Join(dataDir, "update", "pre-"+testTag+".db")
	if err := os.MkdirAll(filepath.Dir(snap), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := openTestStore(t, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Snapshot(snap); err != nil {
		s.Close()
		t.Fatal(err)
	}
	s.Close()
	// The failed version's write lands in the live database after the
	// snapshot, and the journal still says trying (power loss mid-trial).
	s2, err := openTestStore(t, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.DB().Exec(`INSERT INTO probe (v) VALUES ('new')`); err != nil {
		s2.Close()
		t.Fatal(err)
	}
	s2.Close()
	if err := writeState(dataDir, State{Current: testOldTag, HighWater: testOldTag, Trying: &Trial{Version: testTag, Snapshot: snap}}); err != nil {
		t.Fatal(err)
	}

	old := restoreLockWait
	restoreLockWait = 300 * time.Millisecond
	t.Cleanup(func() { restoreLockWait = old })

	stop := holdDB(t, db)
	err = Reject(dataDir, testTag)
	if err == nil || !strings.Contains(err.Error(), "in use by another process") {
		t.Fatalf("Reject while another process holds the DB = %v, want a clear in-use error", err)
	}
	st, _ := ReadState(dataDir)
	if st.Trying == nil || st.Trying.Version != testTag {
		t.Fatalf("trying = %+v; a refused restore must keep the trial open", st.Trying)
	}
	// The holder exits: the kernel drops the lock and the restore proceeds.
	stop()
	if err := Reject(dataDir, testTag); err != nil {
		t.Fatalf("Reject after the holder exited: %v", err)
	}
	st, _ = ReadState(dataDir)
	if st.Trying != nil {
		t.Errorf("trying = %+v, must clear after the completed rollback", st.Trying)
	}
	if rows := probeRows(t, dataDir); len(rows) != 1 || rows[0] != "old" {
		t.Errorf("rows after the completed rollback = %v, want [old]", rows)
	}
}

// TestActivateReleasesSnapshotReservation: the app's stage keeps the
// snapshot reservation in the shared jobs table until the launcher writes
// pre-<v>.db; Activate releases it right after the snapshot.
func TestActivateReleasesSnapshotReservation(t *testing.T) {
	install := setupInstall(t)
	dataDir := t.TempDir()
	seedDB(t, dataDir)
	db := DatabasePath(dataDir)
	if err := writeState(dataDir, State{Staged: testTag}); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(db)
	if err != nil {
		t.Fatal(err)
	}
	s, err := openTestStore(t, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.New(s); err != nil {
		s.Close()
		t.Fatalf("jobs migrations: %v", err)
	}
	if _, err := s.DB().Exec(
		`INSERT INTO jobs (id, kind, stage, state, payload, reserved_bytes, attempts, created, updated)
		 VALUES (?, 'update', 'staged', 'running', '{}', ?, 0, 'now', 'now')`,
		snapshotReservationID(testTag), fi.Size(),
	); err != nil {
		s.Close()
		t.Fatalf("seeding the snapshot reservation: %v", err)
	}
	var before int64
	if err := s.DB().QueryRow(`SELECT reserved_bytes FROM jobs WHERE id = ?`, snapshotReservationID(testTag)).Scan(&before); err != nil {
		s.Close()
		t.Fatal(err)
	}
	if before == 0 {
		s.Close()
		t.Fatal("the seeded snapshot reservation is zero")
	}
	s.Close() // release the ownership lock; Activate opens the database itself

	if _, err := Activate(install, dataDir, db); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	s2, err := openTestStore(t, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	var after int64
	if err := s2.DB().QueryRow(`SELECT reserved_bytes FROM jobs WHERE id = ?`, snapshotReservationID(testTag)).Scan(&after); err != nil {
		t.Fatalf("the snapshot reservation row vanished: %v", err)
	}
	if after != 0 {
		t.Errorf("snapshot reservation after Activate = %d, want 0 (released once pre-<v>.db is on disk)", after)
	}
}

// TestRejectReleasesSnapshotReservation: rejecting a leftover trial also
// releases its stage-time snapshot reservation.
func TestRejectReleasesSnapshotReservation(t *testing.T) {
	dataDir := t.TempDir()
	seedDB(t, dataDir)
	snap := filepath.Join(dataDir, "update", "pre-"+testTag+".db")
	if err := os.MkdirAll(filepath.Dir(snap), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := openTestStore(t, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	// The stage left the snapshot reservation in the shared jobs table
	// before the launcher took the snapshot, so the restored database
	// still carries it and the rejection releases it.
	if _, err := jobs.New(s); err != nil {
		s.Close()
		t.Fatalf("jobs migrations: %v", err)
	}
	if _, err := s.DB().Exec(
		`INSERT INTO jobs (id, kind, stage, state, payload, reserved_bytes, attempts, created, updated)
		 VALUES (?, 'update', 'staged', 'running', '{}', 1234, 0, 'now', 'now')`,
		snapshotReservationID(testTag),
	); err != nil {
		s.Close()
		t.Fatal(err)
	}
	if err := s.Snapshot(snap); err != nil {
		s.Close()
		t.Fatal(err)
	}
	// The failed version's write lands in the live database.
	if _, err := s.DB().Exec(`INSERT INTO probe (v) VALUES ('new')`); err != nil {
		s.Close()
		t.Fatal(err)
	}
	s.Close()
	if err := writeState(dataDir, State{Current: testOldTag, HighWater: testOldTag, Trying: &Trial{Version: testTag, Snapshot: snap}}); err != nil {
		t.Fatal(err)
	}

	if err := Reject(dataDir, testTag); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	s2, err := openTestStore(t, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	var after int64
	if err := s2.DB().QueryRow(`SELECT reserved_bytes FROM jobs WHERE id = ?`, snapshotReservationID(testTag)).Scan(&after); err != nil {
		t.Fatalf("the snapshot reservation row vanished: %v", err)
	}
	if after != 0 {
		t.Errorf("snapshot reservation after Reject = %d, want 0 (released on reject)", after)
	}
}

// TestConfirmReSyncsWhenAlreadySettled: Confirm on an already-settled trial
// must still re-sync the journal durably (write + fsync file + fsync dir)
// before returning success - a journal write that cannot be synced is an
// error, never a silent success.
func TestConfirmReSyncsWhenAlreadySettled(t *testing.T) {
	install := setupInstall(t)
	dataDir := t.TempDir()
	seedDB(t, dataDir)
	if err := writeState(dataDir, State{Current: testOldTag, HighWater: testOldTag, Staged: testTag}); err != nil {
		t.Fatal(err)
	}
	if _, err := Activate(install, dataDir, DatabasePath(dataDir)); err != nil {
		t.Fatal(err)
	}
	if err := Confirm(dataDir, testTag); err != nil {
		t.Fatal(err)
	}

	// The journal is settled; a second Confirm still re-syncs it. Break
	// the write: a non-empty directory at the temp path cannot be
	// truncated/renamed over.
	tmp := StatePath(dataDir) + ".tmp"
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "junk"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Confirm(dataDir, testTag)
	if err == nil {
		t.Fatal("Confirm on an already-settled trial returned nil although the journal re-sync failed; the settlement must be re-synced before success")
	}
	if !strings.Contains(err.Error(), "re-sync") {
		t.Errorf("Confirm error = %v, want the re-sync failure", err)
	}
	if err := os.RemoveAll(tmp); err != nil {
		t.Fatal(err)
	}
	if err := Confirm(dataDir, testTag); err != nil {
		t.Fatalf("Confirm after the blocker cleared: %v", err)
	}
}

// TestRestoreSyncsDirAfterWalShmRemoval: the restore fsyncs the database
// directory after deleting the stale -wal/-shm files; if that final sync
// fails the restore is not durable, so the trial stays open and the error
// surfaces instead of the rollback completing.
func TestRestoreSyncsDirAfterWalShmRemoval(t *testing.T) {
	install := setupInstall(t)
	dataDir := t.TempDir()
	seedDB(t, dataDir)
	db := DatabasePath(dataDir)
	if err := writeState(dataDir, State{Current: testOldTag, HighWater: testOldTag, Staged: testTag}); err != nil {
		t.Fatal(err)
	}
	if _, err := Activate(install, dataDir, db); err != nil {
		t.Fatal(err)
	}
	// Stale WAL/SHM files a killed trial version leaves behind.
	if err := os.WriteFile(db+"-wal", []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db+"-shm", []byte("shm"), 0o600); err != nil {
		t.Fatal(err)
	}

	old := syncRestoredDir
	syncRestoredDir = func(string) error { return errors.New("dir sync failed (injected)") }
	t.Cleanup(func() { syncRestoredDir = old })

	err := Reject(dataDir, testTag)
	if err == nil || !strings.Contains(err.Error(), "dir sync failed") {
		t.Fatalf("Reject = %v, want the post-removal fsync failure", err)
	}
	// The failure happened after the WAL/SHM removal and the trial must
	// still be open: the restore is not durable until the removals are
	// synced.
	st, _ := ReadState(dataDir)
	if st.Trying == nil || st.Trying.Version != testTag {
		t.Fatalf("trying = %+v; a restore whose post-removal fsync failed must keep the trial open", st.Trying)
	}
	if _, err := os.Stat(db + "-wal"); !os.IsNotExist(err) {
		t.Error("-wal survived the failed restore; it must be gone before the post-removal fsync")
	}
	if _, err := os.Stat(db + "-shm"); !os.IsNotExist(err) {
		t.Error("-shm survived the failed restore; it must be gone before the post-removal fsync")
	}
	// With the sync working again, the retry completes and clears the
	// trial.
	syncRestoredDir = old
	if err := Reject(dataDir, testTag); err != nil {
		t.Fatalf("Reject after the sync recovered: %v", err)
	}
	st, _ = ReadState(dataDir)
	if st.Trying != nil {
		t.Errorf("trying = %+v, must clear after the completed rollback", st.Trying)
	}
	if rows := probeRows(t, dataDir); len(rows) != 1 || rows[0] != "old" {
		t.Errorf("rows after the completed rollback = %v, want [old]", rows)
	}
}

// openTestStore opens the data directory's database.
func openTestStore(t *testing.T, dataDir string) (*store.Store, error) {
	return store.Open(DatabasePath(dataDir))
}
