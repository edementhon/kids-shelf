// Package jobs owns the persisted job queue shared by ingest, youtube, kiwix
// and update. Every long operation the server performs is a row in the jobs
// table with an explicit stage, so a quit, a kill or a power cut at any stage
// resumes or cleanly restarts the work at the next launch.
//
// The jobs table is owned by this package alone (its migrations carry Owner
// "jobs"): other packages read or write it only through these APIs. Disk
// reservations (internal/disk) read and write the reserved_bytes column
// through the TryReserve and SetReservedBytes methods here, never through
// SQL of their own. Standalone reservations that guard work without a job
// row (the updater's stage and activation-snapshot holds) live in the
// reservations table, owned here too, and count into the same sums.
package jobs

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/edementhon/kids-shelf/internal/store"
)

// Job states.
const (
	StateQueued  = "queued"
	StateRunning = "running"
	StatePaused  = "paused"
	StateDone    = "done"
	StateFailed  = "failed"
)

// Migrations registers the jobs table with the store. One owner is one
// package, so no other package owns any of these tables.
var Migrations = []store.Migration{{
	Owner:   "jobs",
	Version: 1,
	SQL: `
CREATE TABLE jobs (
	id             TEXT    PRIMARY KEY,
	kind           TEXT    NOT NULL,
	stage          TEXT    NOT NULL DEFAULT '',
	state          TEXT    NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'running', 'paused', 'done', 'failed')),
	payload        TEXT    NOT NULL DEFAULT '{}',
	reserved_bytes INTEGER NOT NULL DEFAULT 0,
	attempts       INTEGER NOT NULL DEFAULT 0,
	error          TEXT    NOT NULL DEFAULT '',
	created        TEXT    NOT NULL,
	updated        TEXT    NOT NULL
)`,
}, {
	Owner:   "jobs",
	Version: 2,
	SQL: `
ALTER TABLE jobs ADD COLUMN seq INTEGER NOT NULL DEFAULT 0;

UPDATE jobs SET seq = (
	SELECT COUNT(*) + 1 FROM jobs AS earlier
	WHERE earlier.created < jobs.created
	   OR (earlier.created = jobs.created AND earlier.id < jobs.id)
)`,
}, {
	// fs_key ties a reservation to the filesystem it was made on, so
	// per-filesystem floor checks (disk.FSKey) sum only the reservations on
	// the same filesystem; '' means "never attributed" and counts against
	// every filesystem (see sumReservationsTx).
	Owner:   "jobs",
	Version: 3,
	SQL: `
CREATE TABLE reservations (
	id      TEXT    PRIMARY KEY,
	bytes   INTEGER NOT NULL DEFAULT 0,
	fs_key  TEXT    NOT NULL DEFAULT '',
	created TEXT    NOT NULL
);

ALTER TABLE jobs ADD COLUMN fs_key TEXT NOT NULL DEFAULT '';`,
}}

// ErrNotFound is returned by Get when no job has the requested id.
var ErrNotFound = errors.New("jobs: not found")

// Job is one row of the queue.
type Job struct {
	ID            string
	Kind          string
	Stage         string
	State         string
	Payload       json.RawMessage
	ReservedBytes uint64
	Attempts      int
	Error         string
	Created       time.Time
	Updated       time.Time
}

// DecodePayload unmarshals the job's JSON payload into v.
func (j *Job) DecodePayload(v any) error {
	return json.Unmarshal(j.Payload, v)
}

// Queue is the persisted job queue, backed by the jobs table.
type Queue struct {
	store *store.Store
	now   func() time.Time
}

// New returns a Queue on s, applying the jobs migrations if they are not yet
// recorded. It is safe (and a no-op) to call more than once.
func New(s *store.Store) (*Queue, error) {
	if err := s.Migrate(Migrations...); err != nil {
		return nil, err
	}
	return &Queue{store: s, now: time.Now}, nil
}

// Enqueue appends a queued job of the given kind with a JSON-encoded payload
// and returns its id.
func (q *Queue) Enqueue(kind string, payload any) (string, error) {
	id, err := newID()
	if err != nil {
		return "", err
	}
	var raw []byte
	if payload == nil {
		raw = []byte("{}")
	} else if raw, err = json.Marshal(payload); err != nil {
		return "", fmt.Errorf("jobs: encode payload: %w", err)
	}
	// seq is assigned from the table's current maximum inside the insert, so
	// jobs ordered by seq are ordered by enqueue time even when several land
	// in the same second. created/updated keep RFC3339Nano for display;
	// ordering never relies on them.
	now := q.now().UTC().Format(time.RFC3339Nano)
	if _, err := q.store.DB().Exec(
		`INSERT INTO jobs (id, kind, stage, state, payload, reserved_bytes, attempts, error, created, updated, seq)
		 VALUES (?, ?, '', 'queued', ?, 0, 0, '', ?, ?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM jobs))`,
		id, kind, string(raw), now, now,
	); err != nil {
		return "", fmt.Errorf("jobs: enqueue %s: %w", kind, err)
	}
	return id, nil
}

// Claim atomically moves the oldest queued job of kind - lowest seq, i.e.
// enqueue order - to running and returns it, or (nil, nil) when none is
// queued. The transaction starts with BEGIN IMMEDIATE (set in
// internal/store), so two concurrent Claim calls can never hand out the same
// job: the loser sees it already running and picks the next one, if any.
func (q *Queue) Claim(kind string) (*Job, error) {
	for {
		tx, err := q.store.DB().BeginTx(context.Background(), nil)
		if err != nil {
			return nil, fmt.Errorf("jobs: claim: %w", err)
		}
		var id string
		err = tx.QueryRow(
			`SELECT id FROM jobs WHERE kind = ? AND state = 'queued' ORDER BY seq LIMIT 1`,
			kind,
		).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			tx.Rollback()
			return nil, nil
		}
		if err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("jobs: claim: %w", err)
		}
		now := q.now().UTC().Format(time.RFC3339Nano)
		res, err := tx.Exec(
			`UPDATE jobs SET state = 'running', updated = ? WHERE id = ? AND state = 'queued'`,
			now, id,
		)
		if err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("jobs: claim: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("jobs: claim: %w", err)
		}
		if n == 0 {
			// A concurrent claimer took the job between SELECT and UPDATE.
			if err := tx.Commit(); err != nil {
				return nil, fmt.Errorf("jobs: claim: %w", err)
			}
			continue
		}
		job, err := scanJob(tx.QueryRow(selectJob+` WHERE id = ?`, id))
		if err != nil {
			tx.Rollback()
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("jobs: claim: %w", err)
		}
		return job, nil
	}
}

// Advance records progress on a job: a new stage name and, unless payload is
// nil, a replacement payload (typically the same payload with stage-specific
// fields updated). Advancing a paused job returns it to running, which is how
// a paused job resumes.
func (q *Queue) Advance(id, stage string, payload any) error {
	now := q.now().UTC().Format(time.RFC3339Nano)
	if payload == nil {
		if _, err := q.store.DB().Exec(
			`UPDATE jobs SET stage = ?, state = CASE WHEN state = 'paused' THEN 'running' ELSE state END, updated = ? WHERE id = ?`,
			stage, now, id,
		); err != nil {
			return fmt.Errorf("jobs: advance %s: %w", id, err)
		}
		return nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("jobs: advance %s: encode payload: %w", id, err)
	}
	if _, err := q.store.DB().Exec(
		`UPDATE jobs SET stage = ?, payload = ?, state = CASE WHEN state = 'paused' THEN 'running' ELSE state END, updated = ? WHERE id = ?`,
		stage, string(raw), now, id,
	); err != nil {
		return fmt.Errorf("jobs: advance %s: %w", id, err)
	}
	return nil
}

// Pause moves a job to paused and stores reason (shown in the dashboard, for
// example a disk-full explanation). The job resumes via Advance.
func (q *Queue) Pause(id, reason string) error {
	now := q.now().UTC().Format(time.RFC3339Nano)
	if _, err := q.store.DB().Exec(
		`UPDATE jobs SET state = 'paused', error = ?, updated = ? WHERE id = ?`,
		reason, now, id,
	); err != nil {
		return fmt.Errorf("jobs: pause %s: %w", id, err)
	}
	return nil
}

// Done marks a job finished successfully.
func (q *Queue) Done(id string) error {
	now := q.now().UTC().Format(time.RFC3339Nano)
	if _, err := q.store.DB().Exec(
		`UPDATE jobs SET state = 'done', error = '', updated = ? WHERE id = ?`,
		now, id,
	); err != nil {
		return fmt.Errorf("jobs: done %s: %w", id, err)
	}
	return nil
}

// Fail marks a job failed and records the error.
func (q *Queue) Fail(id string, err error) error {
	if err == nil {
		err = errors.New("jobs: failed")
	}
	now := q.now().UTC().Format(time.RFC3339Nano)
	if _, e := q.store.DB().Exec(
		`UPDATE jobs SET state = 'failed', error = ?, updated = ? WHERE id = ?`,
		err.Error(), now, id,
	); e != nil {
		return fmt.Errorf("jobs: fail %s: %w", id, e)
	}
	return nil
}

// Recover requeues every job left running by a quit, a kill or a power cut,
// bumping its attempt counter, and returns how many were requeued. Call it at
// startup, before workers start claiming.
func (q *Queue) Recover() (int, error) {
	now := q.now().UTC().Format(time.RFC3339Nano)
	res, err := q.store.DB().Exec(
		`UPDATE jobs SET state = 'queued', attempts = attempts + 1, updated = ? WHERE state = 'running'`,
		now,
	)
	if err != nil {
		return 0, fmt.Errorf("jobs: recover: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("jobs: recover: %w", err)
	}
	return int(n), nil
}

// Get returns the job with the given id, or ErrNotFound.
func (q *Queue) Get(id string) (*Job, error) {
	job, err := scanJob(q.store.DB().QueryRow(selectJob+` WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	return job, nil
}

// List returns the jobs of the given kind, most recently enqueued first
// (highest seq); an empty kind lists all jobs.
func (q *Queue) List(kind string) ([]Job, error) {
	query := selectJob
	args := []any{}
	if kind != "" {
		query += " WHERE kind = ?"
		args = append(args, kind)
	}
	query += " ORDER BY seq DESC"
	rows, err := q.store.DB().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("jobs: list %q: %w", kind, err)
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("jobs: list %q: %w", kind, err)
	}
	return out, nil
}

// SumActiveReservedBytes returns the sum of reserved_bytes across all active
// work jobs (state not in done, failed) plus every standalone reservation,
// excluding jobID. It is the "already reserved" figure disk reservations
// compare against.
func (q *Queue) SumActiveReservedBytes(excludeJobID string) (uint64, error) {
	var jobsSum, holds int64
	if err := q.store.DB().QueryRow(
		`SELECT COALESCE(SUM(reserved_bytes), 0) FROM jobs WHERE state NOT IN ('done', 'failed') AND id != ?`,
		excludeJobID,
	).Scan(&jobsSum); err != nil {
		return 0, fmt.Errorf("jobs: sum reservations: %w", err)
	}
	if err := q.store.DB().QueryRow(
		`SELECT COALESCE(SUM(bytes), 0) FROM reservations WHERE id != ?`,
		excludeJobID,
	).Scan(&holds); err != nil {
		return 0, fmt.Errorf("jobs: sum reservations: %w", err)
	}
	return uint64(jobsSum) + uint64(holds), nil
}

// TryReserve records jobID's reservation as bytes only if the reservations of
// every other active job or standalone hold on the same filesystem (fsKey)
// leave room for them above the floor: it requires free >= floor and
// free-floor-other >= bytes. jobID must be an existing work job - a
// reservation that updates no row silently never existed, so an unknown id
// is an ErrNotFound error. Standalone reservations without a work job (the
// updater's stage and snapshot holds) use TryHoldReservation. The check and
// the write run in one BEGIN IMMEDIATE transaction, so two concurrent
// reservations - even from managers with separate Reservations objects over
// the same queue - can never both approve. otherReserved is the sum they
// were checked against; ok reports whether the reservation was recorded.
func (q *Queue) TryReserve(jobID string, bytes, free, floor uint64, fsKey string) (otherReserved uint64, ok bool, err error) {
	tx, err := q.store.DB().BeginTx(context.Background(), nil)
	if err != nil {
		return 0, false, fmt.Errorf("jobs: try reserve: %w", err)
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRow(`SELECT 1 FROM jobs WHERE id = ?`, jobID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, fmt.Errorf("jobs: try reserve %s: %w", jobID, ErrNotFound)
		}
		return 0, false, fmt.Errorf("jobs: try reserve %s: %w", jobID, err)
	}
	otherReserved, err = sumReservationsTx(tx, fsKey, jobID)
	if err != nil {
		return 0, false, fmt.Errorf("jobs: try reserve %s: %w", jobID, err)
	}
	// free - floor, guarded against underflow.
	var available uint64
	if free > floor {
		available = free - floor
	}
	if otherReserved > available || bytes > available-otherReserved {
		return otherReserved, false, nil
	}
	now := q.now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(
		`UPDATE jobs SET reserved_bytes = ?, fs_key = ?, updated = ? WHERE id = ?`,
		int64(bytes), fsKey, now, jobID,
	); err != nil {
		return 0, false, fmt.Errorf("jobs: try reserve %s: %w", jobID, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("jobs: try reserve %s: %w", jobID, err)
	}
	return otherReserved, true, nil
}

// TryHoldReservation records a standalone reservation - one with no work job
// behind it, e.g. the updater's stage and snapshot holds - under id in the
// reservations table, with the same per-filesystem free-space check as
// TryReserve. A hold survives the process that made it and a reopening of
// the store, so an update staged while the app runs keeps guarding its
// activation-snapshot bytes across the restart into the launcher; only
// SetReservedBytes with 0 (Release) removes it. Re-holding an id replaces
// its previous reservation. otherReserved is the sum the hold was checked
// against; ok reports whether it was recorded.
func (q *Queue) TryHoldReservation(id string, bytes, free, floor uint64, fsKey string) (otherReserved uint64, ok bool, err error) {
	tx, err := q.store.DB().BeginTx(context.Background(), nil)
	if err != nil {
		return 0, false, fmt.Errorf("jobs: hold reservation: %w", err)
	}
	defer tx.Rollback()
	otherReserved, err = sumReservationsTx(tx, fsKey, id)
	if err != nil {
		return 0, false, fmt.Errorf("jobs: hold reservation %s: %w", id, err)
	}
	// free - floor, guarded against underflow.
	var available uint64
	if free > floor {
		available = free - floor
	}
	if otherReserved > available || bytes > available-otherReserved {
		return otherReserved, false, nil
	}
	now := q.now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(
		`INSERT INTO reservations (id, bytes, fs_key, created) VALUES (?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET bytes = excluded.bytes, fs_key = excluded.fs_key`,
		id, int64(bytes), fsKey, now,
	); err != nil {
		return 0, false, fmt.Errorf("jobs: hold reservation %s: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("jobs: hold reservation %s: %w", id, err)
	}
	return otherReserved, true, nil
}

// sumReservationsTx sums what a new reservation on fsKey's filesystem must
// leave room for, excluding excludeID: the reserved_bytes of every active
// work job on the same filesystem plus every standalone reservation with it.
// A row whose fs_key is empty never went through this API (or predates the
// fs_key column): it cannot be attributed to one filesystem, so it counts
// against every one, keeping a legacy reservation in the floor check.
func sumReservationsTx(tx *sql.Tx, fsKey, excludeID string) (uint64, error) {
	var jobsSum, holds int64
	if err := tx.QueryRow(
		`SELECT COALESCE(SUM(reserved_bytes), 0) FROM jobs
		 WHERE state NOT IN ('done', 'failed') AND id != ? AND (fs_key = ? OR fs_key = '')`,
		excludeID, fsKey,
	).Scan(&jobsSum); err != nil {
		return 0, fmt.Errorf("summing job reservations: %w", err)
	}
	if err := tx.QueryRow(
		`SELECT COALESCE(SUM(bytes), 0) FROM reservations WHERE id != ? AND (fs_key = ? OR fs_key = '')`,
		excludeID, fsKey,
	).Scan(&holds); err != nil {
		return 0, fmt.Errorf("summing held reservations: %w", err)
	}
	return uint64(jobsSum) + uint64(holds), nil
}

// SetReservedBytes records jobID's reservation in reserved_bytes; passing 0
// releases it. The id may name a work job (the jobs table) or a standalone
// reservation (the reservations table, released by deleting its row); a zero
// release of an id that exists in neither is a no-op, so launcher-side
// releases of a stage that never held anything stay harmless. A nonzero
// amount requires a real job row: standalone holds are only ever made
// through TryHoldReservation, which checks free space.
func (q *Queue) SetReservedBytes(jobID string, bytes uint64) error {
	now := q.now().UTC().Format(time.RFC3339Nano)
	if _, err := q.store.DB().Exec(
		`UPDATE jobs SET reserved_bytes = ?, updated = ? WHERE id = ?`,
		int64(bytes), now, jobID,
	); err != nil {
		return fmt.Errorf("jobs: set reservation for %s: %w", jobID, err)
	}
	if bytes == 0 {
		if _, err := q.store.DB().Exec(`DELETE FROM reservations WHERE id = ?`, jobID); err != nil {
			return fmt.Errorf("jobs: release hold for %s: %w", jobID, err)
		}
		return nil
	}
	var exists int
	err := q.store.DB().QueryRow(`SELECT 1 FROM jobs WHERE id = ?`, jobID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("jobs: set reservation for %s: %w", jobID, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("jobs: set reservation for %s: %w", jobID, err)
	}
	return nil
}

// HeldReservationIDs returns the ids of every standalone reservation in the
// reservations table whose id begins with prefix ("" matches all). It backs
// disk.Reconcile's startup cleanup: the updater releases the holds the
// update journal no longer justifies (a crash between journaling "staged"
// and the deferred release would leave the staging hold behind forever).
func (q *Queue) HeldReservationIDs(prefix string) ([]string, error) {
	rows, err := q.store.DB().Query(
		`SELECT id FROM reservations WHERE substr(id, 1, ?) = ?`,
		len(prefix), prefix,
	)
	if err != nil {
		return nil, fmt.Errorf("jobs: listing held reservations %q: %w", prefix, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("jobs: listing held reservations %q: %w", prefix, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("jobs: listing held reservations %q: %w", prefix, err)
	}
	return ids, nil
}

const selectJob = `SELECT id, kind, stage, state, payload, reserved_bytes, attempts, error, created, updated FROM jobs`

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

func scanJob(s scanner) (*Job, error) {
	var j Job
	var payload, created, updated string
	var reserved int64
	err := s.Scan(&j.ID, &j.Kind, &j.Stage, &j.State, &payload, &reserved, &j.Attempts, &j.Error, &created, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("jobs: scan: %w", err)
	}
	j.Payload = json.RawMessage(payload)
	j.ReservedBytes = uint64(reserved)
	if j.Created, err = time.Parse(time.RFC3339, created); err != nil {
		return nil, fmt.Errorf("jobs: scan created: %w", err)
	}
	if j.Updated, err = time.Parse(time.RFC3339, updated); err != nil {
		return nil, fmt.Errorf("jobs: scan updated: %w", err)
	}
	return &j, nil
}

// newID returns a random 128-bit id, hex-encoded.
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("jobs: random id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
