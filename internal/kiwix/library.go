package kiwix

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/edementhon/kids-shelf/internal/disk"
	"github.com/edementhon/kids-shelf/internal/jobs"
	"github.com/edementhon/kids-shelf/internal/store"
)

// Kind is the job queue kind every Kiwix download job is enqueued with.
const Kind = "kiwix"

// Job stages a kiwix download passes through. Each is persisted on the job
// row, so a quit, kill or power cut at any point resumes from where the job
// stopped (the partial file) on the next launch.
const (
	stageMeta4    = "meta4"
	stageReserve  = "reserve"
	stageDownload = "download"
	stageVerify   = "verify"
	stagePublish  = "publish"
)

// Errors returned by Manager.Enqueue and Manager.Remove.
var (
	ErrAlreadyInstalled = errors.New("kiwix: already installed")
	ErrNotInstalled     = errors.New("kiwix: not installed")
)

// Migrations registers the installed table with the store. Table ownership:
// this table belongs to the kiwix package alone.
var Migrations = []store.Migration{{
	Owner:   "kiwix",
	Version: 1,
	SQL: `
CREATE TABLE installed (
	id           TEXT    PRIMARY KEY,
	name         TEXT    NOT NULL,
	title        TEXT    NOT NULL DEFAULT '',
	language     TEXT    NOT NULL DEFAULT '',
	file         TEXT    NOT NULL,
	size         INTEGER NOT NULL,
	installed_at TEXT    NOT NULL
)`,
}}

// Installed is one row of the installed table: a ZIM that has been
// downloaded, verified and published into the library directory.
type Installed struct {
	ID          string
	Name        string
	Title       string
	Language    string
	File        string
	Size        int64
	InstalledAt time.Time
}

// jobPayload is the JSON payload of a kiwix job. The entry never changes;
// the rest records how far the pipeline got so a restarted job resumes from
// exactly that point. Meta4 is parsed from the entry's meta4 URL once and
// then persisted, so a restarted job does not need the catalog again.
type jobPayload struct {
	Entry      Entry  `json:"entry"`
	Meta4      *Meta4 `json:"meta4,omitempty"`
	Reserved   bool   `json:"reserved,omitempty"`
	Downloaded int64  `json:"downloaded,omitempty"`
}

// Manager runs Kiwix downloads through the shared job queue and owns the
// installed table. lib is the library directory: verified ZIMs are published
// there, partial downloads live in its .partial subdirectory.
type Manager struct {
	store *store.Store
	queue *jobs.Queue
	res   *disk.Reservations
	lib   string
	// HTTP timeouts for download attempts. They default to
	// defaultHeaderTimeout / defaultIdleTimeout; tests lower them through the
	// fields before the first request (the client is built lazily).
	headerTimeout time.Duration // how long a mirror may take to send headers
	idleTimeout   time.Duration // how long a mirror may send no bytes mid-body
	// allowLoopback lets tests point the download client at an httptest
	// mirror on 127.0.0.1; production always refuses loopback destinations
	// (see restrictedDialer).
	allowLoopback bool
	httpOnce      sync.Once
	httpClient    *http.Client

	// partWriter, when set, stands in for f on the partial file's data
	// writes; tests use it to fail a write with ENOSPC mid-download.
	partWriter func(*os.File) io.Writer

	// OnChange is called after the installed set changes - a ZIM is
	// published or removed - so the kiwix-serve supervisor (internal/kiwix
	// server) can restart with the new library. It is invoked with the
	// statement that changed the set fully done and no lock held, so a
	// callback may query Installed. Nil means nobody is listening.
	OnChange func()

	// Quiesce, when set, is called before Remove deletes a ZIM file, so the
	// kiwix-serve supervisor can stop the process that serves it first
	// (libkiwix caches opened archives, and on Windows the open file blocks
	// os.Remove). The returned resume runs when Remove finishes - success
	// or failure - and restarts the server with the current library. An
	// error aborts the removal before anything is deleted. Nil means no
	// quiescing.
	Quiesce func(ctx context.Context) (resume func(), err error)
}

// NewManager returns a Manager on s (for the installed table), q (the job
// queue, kind "kiwix") and res (disk reservations). lib is created if
// missing; the migrations for the installed table are applied if pending, so
// a Manager is self-sufficient for both tests and the app.
func NewManager(s *store.Store, q *jobs.Queue, res *disk.Reservations, lib string) (*Manager, error) {
	if err := s.Migrate(Migrations...); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(lib, 0o755); err != nil {
		return nil, fmt.Errorf("kiwix: create library dir: %w", err)
	}
	return &Manager{
		store: s, queue: q, res: res, lib: lib,
		headerTimeout: defaultHeaderTimeout,
		idleTimeout:   defaultIdleTimeout,
	}, nil
}

// Enqueue adds a download job for e to the shared queue and returns its id.
// An entry that is already installed is refused, so a retried parent click
// never downloads the same ZIM twice.
func (m *Manager) Enqueue(e Entry) (string, error) {
	if e.ID == "" || e.Name == "" || e.Meta4URL == "" {
		return "", fmt.Errorf("kiwix: enqueue: entry needs id, name and meta4 URL")
	}
	done, err := m.isInstalled(e.ID)
	if err != nil {
		return "", err
	}
	if done {
		return "", ErrAlreadyInstalled
	}
	return m.queue.Enqueue(Kind, jobPayload{Entry: e})
}

// JobView is one kiwix download job as the parent dashboard lists it: the
// catalog entry being downloaded, how many bytes are already on disk, the
// job's state and stage, and the plain-words message for paused (disk full)
// and failed jobs (the jobs table's error column). JSON tags keep the
// dashboard API shape stable.
type JobView struct {
	ID         string    `json:"id"`
	State      string    `json:"state"`
	Stage      string    `json:"stage"`
	Message    string    `json:"message,omitempty"`
	Entry      Entry     `json:"entry"`
	Downloaded int64     `json:"downloaded"`
	Created    time.Time `json:"created"`
	Updated    time.Time `json:"updated"`
}

// Jobs lists every kiwix download job, most recently enqueued first. A job
// whose payload does not decode still lists with its id, state and error, so
// the dashboard never hides a stuck download.
func (m *Manager) Jobs() ([]JobView, error) {
	jobs, err := m.queue.List(Kind)
	if err != nil {
		return nil, err
	}
	out := make([]JobView, 0, len(jobs))
	for i := range jobs {
		j := &jobs[i]
		var p jobPayload
		_ = j.DecodePayload(&p)
		out = append(out, JobView{
			ID: j.ID, State: j.State, Stage: j.Stage, Message: j.Error,
			Entry: p.Entry, Downloaded: p.Downloaded,
			Created: j.Created, Updated: j.Updated,
		})
	}
	return out, nil
}

// Work claims queued kiwix jobs one by one and runs each to completion -
// download, verification, publish - until the queue is empty or ctx is done.
// A job whose run fails is marked failed and its reservation released; the
// error lands on the job row, which is what the dashboard shows.
func (m *Manager) Work(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			// Quit or kill: stop claiming. A job cut off mid-run stays
			// running with its reservation and partial file so Recover
			// requeues it at the next launch; it is never failed here.
			return ctx.Err()
		}
		job, err := m.queue.Claim(Kind)
		if err != nil {
			return err
		}
		if job == nil {
			return nil
		}
		if err := m.Run(ctx, job); err != nil {
			if ctx.Err() != nil {
				// Same quit/kill rule as above, for a job that died mid-run.
				return err
			}
			if errors.Is(err, errDiskFull) {
				// download() already paused the job with a parent-readable
				// reason and kept its reservation; carry on with the queue.
				continue
			}
			if failErr := m.queue.Fail(job.ID, err); failErr != nil {
				return failErr
			}
			if relErr := m.res.Release(job.ID); relErr != nil {
				return relErr
			}
		}
	}
}

// Run executes one kiwix job through its stages. On success the job is
// marked done and its reservation released. On error the error is returned
// and the job is left running with its reservation intact, so the caller can
// fail it (Work does) or a crash can let Recover requeue it: a restart
// resumes from the partial file instead of starting over.
func (m *Manager) Run(ctx context.Context, job *jobs.Job) error {
	var p jobPayload
	if err := job.DecodePayload(&p); err != nil {
		return fmt.Errorf("kiwix: job %s: decode payload: %w", job.ID, err)
	}
	if p.Meta4 == nil {
		meta, err := m.fetchMeta4(ctx, p.Entry.Meta4URL)
		if err != nil {
			return fmt.Errorf("kiwix: %s: %w", p.Entry.Name, err)
		}
		p.Meta4 = meta
		if err := m.queue.Advance(job.ID, stageMeta4, &p); err != nil {
			return err
		}
	}
	if !p.Reserved {
		if err := m.res.Reserve(job.ID, reserveNeed(p.Meta4.Size), m.lib); err != nil {
			return fmt.Errorf("kiwix: cannot download %s: %w", zimFileName(p.Entry), err)
		}
		p.Reserved = true
		if err := m.queue.Advance(job.ID, stageReserve, &p); err != nil {
			return err
		}
	}
	if err := m.install(ctx, job, &p); err != nil {
		return err
	}
	// The job is done regardless of a release hiccup: a done job is excluded
	// from the active reservation sum anyway.
	doneErr := m.queue.Done(job.ID)
	relErr := m.res.Release(job.ID)
	if doneErr != nil {
		return doneErr
	}
	if relErr != nil {
		return relErr
	}
	return nil
}

// install brings the entry's ZIM into the library: download into the partial
// file (resuming it if it exists), verify the sha-256, rename into place and
// record the installed row. It is re-entrant: an already-installed row, or a
// final file already in place with the right size, short-circuits to the
// publish hand-off, which is what makes a kill during verification or during
// the rename safe - nothing is published twice.
func (m *Manager) install(ctx context.Context, job *jobs.Job, p *jobPayload) error {
	entry := &p.Entry
	done, err := m.isInstalled(entry.ID)
	if err != nil {
		return err
	}
	if done {
		return nil
	}

	// The stored name comes from the catalog entry only - never from the
	// .meta4's declared file name, which is untrusted input - and every
	// path is resolved inside the library directory.
	file := zimFileName(*entry)
	final, err := m.libPath(file)
	if err != nil {
		return err
	}

	// A previous run may have renamed the ZIM into place and then been cut
	// off before recording the row; trust a final file of the declared size
	// after verifying it. Anything else already in place is left alone: a
	// fresh download only ever lands in .partial/, and replaces the final
	// file only after the sha-256 check, so an installed (or stale) file is
	// never deleted or overwritten before the new download has verified.
	if st, err := os.Stat(final); err == nil && st.Size() == p.Meta4.Size {
		return m.verifyAndRecord(ctx, job, final, p, file)
	}

	if p.Meta4.Size <= 0 {
		return fmt.Errorf("kiwix: %s: meta4 declares no size", file)
	}
	partial, err := m.libPath(filepath.Join(".partial", file+".part"))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(partial), 0o755); err != nil {
		return fmt.Errorf("kiwix: create %s: %w", filepath.Dir(partial), err)
	}

	if err := m.download(ctx, job, partial, p); err != nil {
		return err
	}
	if err := m.queue.Advance(job.ID, stageVerify, p); err != nil {
		return err
	}
	if err := m.verify(ctx, partial, p.Meta4.SHA256); err != nil {
		// Only a genuine checksum mismatch drops the partial; a quit or kill
		// during verification keeps the completed download so the next run
		// just verifies and publishes it.
		if ctx.Err() == nil {
			os.Remove(partial)
		}
		return fmt.Errorf("kiwix: %s failed sha-256 verification: %w", file, err)
	}
	return m.publish(ctx, job, partial, final, p, file)
}

// verifyAndRecord publishes a final file that is already in place: it passed
// the size check; verify the hash and record it.
func (m *Manager) verifyAndRecord(ctx context.Context, job *jobs.Job, final string, p *jobPayload, file string) error {
	if err := m.queue.Advance(job.ID, stageVerify, p); err != nil {
		return err
	}
	if err := m.verify(ctx, final, p.Meta4.SHA256); err != nil {
		return fmt.Errorf("kiwix: %s failed sha-256 verification: %w", file, err)
	}
	return m.publish(ctx, job, final /* already in place, no rename needed */, "", p, file)
}

// publish records the installed row for a verified ZIM. When partial differs
// from final it is renamed into place first, then the row is inserted, then
// the job advances to stagePublish. A kill anywhere in between leaves either
// the row (the next run is a no-op) or a final file of the right size (the
// next run verifies and records), so publishing is idempotent.
func (m *Manager) publish(ctx context.Context, job *jobs.Job, partial, final string, p *jobPayload, file string) error {
	if final != "" && partial != final {
		if err := os.Rename(partial, final); err != nil {
			return fmt.Errorf("kiwix: publish %s: %w", final, err)
		}
	}
	inserted, err := m.insertInstalled(job, *p, file)
	if err != nil {
		return err
	}
	if inserted {
		m.notifyChange()
	}
	return m.queue.Advance(job.ID, stagePublish, p)
}

// insertInstalled records the row for a published ZIM. INSERT OR IGNORE
// makes concurrent publishes of the same entry race-safe: a second publish
// of an entry that is already recorded inserts nothing and reports it.
func (m *Manager) insertInstalled(job *jobs.Job, p jobPayload, file string) (bool, error) {
	res, err := m.store.DB().Exec(`
INSERT OR IGNORE INTO installed (id, name, title, language, file, size, installed_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.Entry.ID, p.Entry.Name, p.Entry.Title, p.Entry.Language, file, p.Meta4.Size,
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return false, fmt.Errorf("kiwix: record %s: %w", file, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// isInstalled reports whether the entry's id has a row in the installed
// table.
func (m *Manager) isInstalled(id string) (bool, error) {
	var one int
	err := m.store.DB().QueryRow(`SELECT 1 FROM installed WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("kiwix: installed lookup: %w", err)
	}
	return true, nil
}

// Installed returns every installed ZIM, ordered by name.
func (m *Manager) Installed() ([]Installed, error) {
	rows, err := m.store.DB().Query(
		`SELECT id, name, title, language, file, size, installed_at FROM installed ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("kiwix: installed list: %w", err)
	}
	defer rows.Close()
	var out []Installed
	for rows.Next() {
		var in Installed
		var at string
		if err := rows.Scan(&in.ID, &in.Name, &in.Title, &in.Language, &in.File, &in.Size, &at); err != nil {
			return nil, err
		}
		in.InstalledAt, _ = time.Parse(time.RFC3339Nano, at)
		out = append(out, in)
	}
	return out, rows.Err()
}

// Remove deletes an installed ZIM: the file in the library directory and the
// partial download, then the installed row. Removing an entry that is not
// installed returns ErrNotInstalled. The stored file name is resolved
// inside the library directory; a name that would escape it is refused and
// nothing is deleted. When Quiesce is set it runs before the file is
// deleted - it stops the kiwix-serve process that holds the file open (see
// Quiesce) - and its resume runs when Remove is done, whether the removal
// succeeded or not.
func (m *Manager) Remove(id string) error {
	var file string
	err := m.store.DB().QueryRow(`SELECT file FROM installed WHERE id = ?`, id).Scan(&file)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotInstalled
	}
	if err != nil {
		return fmt.Errorf("kiwix: remove %s: %w", id, err)
	}
	if file != "" {
		final, err := m.libPath(file)
		if err != nil {
			return err
		}
		// A serving kiwix-serve holds the file open - libkiwix caches opened
		// archives, and on Windows that blocks os.Remove - so quiesce the
		// server before deleting; resume runs at the end either way and
		// restarts it with the current library.
		if m.Quiesce != nil {
			resume, err := m.Quiesce(context.Background())
			if err != nil {
				// Nothing was deleted; report the quiesce failure plainly,
				// without file/path wrapping.
				return err
			}
			defer resume()
		}
		if err := os.Remove(final); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("kiwix: remove %s: %w", file, err)
		}
		// Drop any leftover partial download of the same ZIM.
		if partial, perr := m.libPath(filepath.Join(".partial", file+".part")); perr != nil {
			return perr
		} else {
			os.Remove(partial)
		}
	}
	if _, err := m.store.DB().Exec(`DELETE FROM installed WHERE id = ?`, id); err != nil {
		return fmt.Errorf("kiwix: remove %s: %w", id, err)
	}
	m.notifyChange()
	return nil
}

// notifyChange reports the installed set changed to the OnChange hook, if
// any, once the statement that changed the set has fully returned.
func (m *Manager) notifyChange() {
	if m.OnChange != nil {
		m.OnChange()
	}
}

// reserveNeed is the disk reservation for a ZIM of size bytes: the file plus
// 5% headroom.
func reserveNeed(size int64) uint64 {
	if size <= 0 {
		return 0
	}
	return uint64(size) + uint64(size)/20
}

// zimFileName is the name a ZIM is stored under: generated from the catalog
// entry (name, flavour and updated date), never from the .meta4's declared
// file name, and sanitized to the safe file-name alphabet [A-Za-z0-9._-] so
// it is always a plain relative name that cannot escape the library
// directory.
func zimFileName(e Entry) string {
	base := sanitizeFileName(e.Name + "_" + e.Flavour + "_" + e.Updated.UTC().Format("2006-01"))
	return base + ".zim"
}

// sanitizeFileName keeps only [A-Za-z0-9._-] from s, collapsing every other
// run of characters into one underscore, so the result contains no path
// separators and cannot be "." or "..".
func sanitizeFileName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	under := false
	for i := range s {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == '_' || c == '-' {
			under = false
			b.WriteByte(c)
			continue
		}
		if !under && b.Len() > 0 {
			b.WriteByte('_')
		}
		under = true
	}
	return b.String()
}

// libPath resolves name - a stored file name or relative path inside the
// library directory - against lib, rejecting anything that would escape it.
// Every file operation on downloaded ZIMs goes through here: the .meta4
// file name, catalog fields and stored rows are all untrusted input to a
// path.
func (m *Manager) libPath(name string) (string, error) {
	p := filepath.Join(m.lib, name)
	rel, err := filepath.Rel(m.lib, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("kiwix: %q escapes the library directory", name)
	}
	return p, nil
}
