// Package store opens the SQLite database and owns schema migrations and
// WAL-consistent snapshots.
//
// Table ownership: every migration is tagged with an Owner, and one owner is
// one package (jobs, library, budget, ...). A package only touches - creates,
// alters, reads or writes - the tables that its own migrations own, and only
// through its own SQL; it never reaches into tables another package owns.
// Each package registers its migrations (for example jobs.Migrations) with a
// single (*Store).Migrate call at startup, and migrations are forward-only:
// a (owner, version) pair, once recorded, must never change.
package store

import (
	"database/sql"
	"fmt"
	"sort"
	"sync"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// DriverName is the database/sql driver name registered by modernc.org/sqlite.
const DriverName = "sqlite"

// Migration is one numbered, forward-only schema change belonging to one
// owner package.
type Migration struct {
	Owner   string
	Version int
	SQL     string
}

// Store wraps the SQLite database. While a Store is open it holds the
// database's exclusive ownership lock (<path>.lock, see LockDB), so no other
// process can open the same database at the same time.
type Store struct {
	db         *sql.DB
	unlockOnce sync.Once
	unlock     func()
}

// Open opens (creating it if needed) the database at path with WAL journal
// mode, a 5 second busy timeout and foreign keys enabled. The parent
// directory of path must already exist, and path must not contain '?' or '&'
// (DSN query separators); use an absolute path.
//
// Open also takes the database's exclusive ownership lock (LockDB, zero
// wait) and holds it until Close: whoever opens the database owns it, so a
// restore or a migration can never run under a still-open database. The
// kernel drops the lock when the process exits or Close runs, so a crash
// never leaves a stale lock. Open fails with a clear error when another
// process holds the lock.
//
// The journal mode, busy timeout, foreign keys and transaction mode are set
// through DSN parameters the modernc.org/sqlite driver interprets per
// connection, so they cannot be lost to the connection pool; in particular
// every transaction starts as BEGIN IMMEDIATE, which prevents lost-update
// races between readers-then-writers (for example two concurrent job claims).
func Open(path string) (*Store, error) {
	unlock, err := LockDB(path, 0)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	dsn := path + "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=1&_txlock=immediate"
	db, err := sql.Open(DriverName, dsn)
	if err != nil {
		unlock()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		unlock()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	return &Store{db: db, unlock: unlock}, nil
}

// DB exposes the underlying *sql.DB. Packages holding tables they own may use
// it for queries and transactions; nothing else should.
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the underlying database and releases the ownership lock. It
// is safe to call more than once.
func (s *Store) Close() error {
	err := s.db.Close()
	s.unlockOnce.Do(s.unlock)
	return err
}

// Migrate applies the pending migrations among ms, grouped per owner and each
// owner's batch in ascending version order, each owner's batch inside its own
// transaction. Migrations whose (owner, version) pair is already recorded in
// schema_migrations are skipped, so Migrate is idempotent and the whole
// startup can pass every package's migrations every time. A failing migration
// rolls back its owner's whole batch and returns the error.
func (s *Store) Migrate(ms ...Migration) error {
	owners, byOwner, err := groupMigrations(ms)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(schemaMigrationsSQL); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}
	for _, owner := range owners {
		pending, err := s.pending(owner, byOwner[owner])
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			continue
		}
		if err := s.apply(owner, pending); err != nil {
			return err
		}
	}
	return nil
}

const schemaMigrationsSQL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
	owner      TEXT    NOT NULL,
	version    INTEGER NOT NULL,
	applied_at TEXT    NOT NULL,
	PRIMARY KEY (owner, version)
)`

// groupMigrations validates ms (no duplicate version within an owner) and
// returns the owners in first-seen order plus each owner's migrations sorted
// by ascending version.
func groupMigrations(ms []Migration) ([]string, map[string][]Migration, error) {
	byOwner := make(map[string][]Migration)
	var owners []string
	for _, m := range ms {
		if m.Owner == "" {
			return nil, nil, fmt.Errorf("store: migration %d has no owner", m.Version)
		}
		if _, ok := byOwner[m.Owner]; !ok {
			owners = append(owners, m.Owner)
		}
		byOwner[m.Owner] = append(byOwner[m.Owner], m)
	}
	for owner, list := range byOwner {
		sort.Slice(list, func(i, j int) bool { return list[i].Version < list[j].Version })
		for i := 1; i < len(list); i++ {
			if list[i].Version == list[i-1].Version {
				return nil, nil, fmt.Errorf("store: owner %q has duplicate version %d", owner, list[i].Version)
			}
		}
	}
	return owners, byOwner, nil
}

// pending returns the migrations of owner that are not yet recorded in
// schema_migrations, in ascending version order.
func (s *Store) pending(owner string, all []Migration) ([]Migration, error) {
	rows, err := s.db.Query(`SELECT version FROM schema_migrations WHERE owner = ?`, owner)
	if err != nil {
		return nil, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	defer rows.Close()
	applied := make(map[int]bool)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("store: read schema_migrations: %w", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	var pending []Migration
	for _, m := range all {
		if !applied[m.Version] {
			pending = append(pending, m)
		}
	}
	return pending, nil
}

// apply runs each migration in ms inside one transaction, recording it in
// schema_migrations as it goes; any error rolls the whole batch back.
func (s *Store) apply(owner string, ms []Migration) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: migrate %s: %w", owner, err)
	}
	for _, m := range ms {
		if _, err := tx.Exec(m.SQL); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migration %s/%d: %w", m.Owner, m.Version, err)
		}
		now := time.Now().UTC().Format(time.RFC3339)
		if _, err := tx.Exec(
			`INSERT INTO schema_migrations (owner, version, applied_at) VALUES (?, ?, ?)`,
			m.Owner, m.Version, now,
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: record migration %s/%d: %w", m.Owner, m.Version, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit migration %s: %w", owner, err)
	}
	return nil
}

// Snapshot writes a WAL-consistent copy of the database to dst with VACUUM
// INTO: the copy is a full, consistent image even while other connections are
// writing, so it is safe to snapshot a live database before an update that
// migrates the schema. dst must not already exist.
func (s *Store) Snapshot(dst string) error {
	if _, err := s.db.Exec("VACUUM INTO ?", dst); err != nil {
		return fmt.Errorf("store: snapshot to %s: %w", dst, err)
	}
	return nil
}
