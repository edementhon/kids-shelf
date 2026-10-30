package store

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain lets the ownership-fence tests re-exec this binary as the
// "another process" that holds a database open (KS_STORE_TEST_HOLD=<path>):
// it opens the store, writes the readiness marker KS_STORE_TEST_HOLD_READY,
// and blocks until it is killed.
func TestMain(m *testing.M) {
	if db := os.Getenv("KS_STORE_TEST_HOLD"); db != "" {
		s, err := Open(db)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		defer s.Close()
		if err := os.WriteFile(os.Getenv("KS_STORE_TEST_HOLD_READY"), []byte("held"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		// Hold until the test kills us. Not `select {}`: with nothing
		// else able to wake, Go aborts the process ("all goroutines are
		// asleep"), dropping the lock; suspected on the macos-smoke
		// runners, where the lock tests failed only there.
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// holdDB starts a subprocess that opens the store at db and holds it until
// the returned stop func is called (or the test ends).
func holdDB(t *testing.T, db string) (stop func()) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestMain")
	ready := filepath.Join(t.TempDir(), "held")
	cmd.Env = append(os.Environ(), "KS_STORE_TEST_HOLD="+db, "KS_STORE_TEST_HOLD_READY="+ready)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the holder subprocess: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			cmd.Wait()
			t.Fatalf("the holder subprocess never opened the database")
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	return func() {
		cmd.Process.Kill()
		cmd.Wait()
	}
}

// TestOpenFailsWhileAnotherProcessHoldsIt: store.Open takes the database's
// ownership lock, so a second process opening the same database gets a
// clear error; once the holder exits, the kernel drops the lock and Open
// succeeds again.
func TestOpenFailsWhileAnotherProcessHoldsIt(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.sqlite")
	stop := holdDB(t, db)
	if _, err := Open(db); err == nil || !strings.Contains(err.Error(), "in use by another process") {
		t.Fatalf("Open while another process holds the database = %v, want a clear in-use error", err)
	}
	// The holder has exited: its lock is gone and Open succeeds.
	stop()
	s, err := Open(db)
	if err != nil {
		t.Fatalf("Open after the holder exited: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestOpenReacquiresAfterClose: Close releases the ownership lock, so the
// same database can be opened again right away (the app opens the store
// once per run).
func TestOpenReacquiresAfterClose(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.sqlite")
	s, err := Open(db)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := Open(db)
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	defer s2.Close()
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenSetsPragmas(t *testing.T) {
	s := newTestStore(t)
	db := s.DB()
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want %q", mode, "wal")
	}
	var fk int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}
	var busy int
	if err := db.QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil {
		t.Fatalf("busy_timeout: %v", err)
	}
	if busy != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", busy)
	}
}

func TestMigrateAppliesOncePerOwnerInOrder(t *testing.T) {
	s := newTestStore(t)
	// Deliberately interleaved between owners and out of order within an
	// owner; the alpha batch also proves execution order, because its v2
	// writes into the table its v1 creates.
	ms := []Migration{
		{Owner: "beta", Version: 2, SQL: "CREATE TABLE beta_two (v INTEGER)"},
		{Owner: "alpha", Version: 2, SQL: "INSERT INTO alpha_one (v) VALUES (7)"},
		{Owner: "beta", Version: 5, SQL: "CREATE TABLE beta_five (v INTEGER)"},
		{Owner: "alpha", Version: 1, SQL: "CREATE TABLE alpha_one (v INTEGER)"},
	}
	if err := s.Migrate(ms...); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	for _, table := range []string{"alpha_one", "beta_two", "beta_five"} {
		if !tableExists(t, s, table) {
			t.Errorf("table %s missing after Migrate", table)
		}
	}
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM alpha_one").Scan(&n); err != nil {
		t.Fatalf("count alpha_one: %v", err)
	}
	if n != 1 {
		t.Errorf("alpha_one rows = %d, want 1 (v2 must have run after v1)", n)
	}
	var recorded int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&recorded); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if recorded != 4 {
		t.Errorf("schema_migrations rows = %d, want 4", recorded)
	}
	// Applying again is a no-op: nothing is recorded twice, nothing breaks.
	if err := s.Migrate(ms...); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&recorded); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if recorded != 4 {
		t.Errorf("schema_migrations rows after second Migrate = %d, want 4", recorded)
	}
}

func TestMigrateRejectsDuplicateVersion(t *testing.T) {
	s := newTestStore(t)
	err := s.Migrate(
		Migration{Owner: "dup", Version: 1, SQL: "CREATE TABLE one (v INTEGER)"},
		Migration{Owner: "dup", Version: 1, SQL: "CREATE TABLE two (v INTEGER)"},
	)
	if err == nil {
		t.Fatal("Migrate with duplicate version: got nil, want error")
	}
}

func TestMigrateRollsBackFailedBatch(t *testing.T) {
	s := newTestStore(t)
	err := s.Migrate(
		Migration{Owner: "bad", Version: 1, SQL: "CREATE TABLE ok_before (v INTEGER)"},
		Migration{Owner: "bad", Version: 2, SQL: "THIS IS NOT SQL"},
	)
	if err == nil {
		t.Fatal("Migrate with bad SQL: got nil, want error")
	}
	// The whole owner batch rolled back: neither the table nor any record.
	if tableExists(t, s, "ok_before") {
		t.Error("table from rolled-back batch still exists")
	}
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM schema_migrations WHERE owner = 'bad'").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("schema_migrations rows for bad batch = %d, want 0", n)
	}
}

func TestSnapshotConsistentWhileWriterActive(t *testing.T) {
	s := newTestStore(t)
	if err := s.Migrate(Migration{Owner: "snap", Version: 1, SQL: "CREATE TABLE evens (v INTEGER NOT NULL)"}); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	for i := range 100 {
		if _, err := s.db.Exec("INSERT INTO evens (v) VALUES (?)", i); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// A writer keeps committing rows while the snapshot runs.
	stop := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for i := 100; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := s.db.Exec("INSERT INTO evens (v) VALUES (?)", i); err != nil {
				t.Errorf("writer: %v", err)
				return
			}
			if i%50 == 0 {
				time.Sleep(time.Millisecond) // give the snapshot a turn
			}
		}
	}()
	// Wait until the writer has visibly committed past the seed.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM evens").Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n >= 250 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("writer never reached 250 rows")
		}
		time.Sleep(time.Millisecond)
	}
	dst := filepath.Join(t.TempDir(), "snap.sqlite")
	if err := s.Snapshot(dst); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	close(stop)
	<-writerDone

	cp, err := Open(dst)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer cp.Close()
	// The snapshot is a consistent prefix of the writer's sequence: the rows
	// 0..max are all present (no partial transaction), count == max+1, and it
	// is no older than the seed.
	var cnt, max int
	if err := cp.db.QueryRow("SELECT COUNT(*), COALESCE(MAX(v), -1) FROM evens").Scan(&cnt, &max); err != nil {
		t.Fatalf("snapshot query: %v", err)
	}
	if cnt < 100 {
		t.Errorf("snapshot has %d rows, want at least the 100 seeded", cnt)
	}
	if cnt != max+1 {
		t.Errorf("snapshot inconsistent: %d rows but max v = %d", cnt, max)
	}
	// The same rows exist in the live database: the snapshot matches a real
	// point in time of the writer.
	var live int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM evens WHERE v <= ?", max).Scan(&live); err != nil {
		t.Fatalf("live query: %v", err)
	}
	if live != cnt {
		t.Errorf("snapshot rows %d do not match live rows up to max %d (%d)", cnt, max, live)
	}
}

func tableExists(t *testing.T, s *Store, name string) bool {
	t.Helper()
	var n int
	err := s.db.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", name,
	).Scan(&n)
	if err != nil {
		t.Fatalf("check table %s: %v", name, err)
	}
	return n == 1
}
