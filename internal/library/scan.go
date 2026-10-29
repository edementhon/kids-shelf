package library

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/edementhon/kids-shelf/internal/core"
)

// Media sources and the periodic scan live here. A source is one root
// directory (a local folder or a mounted NAS share); each of its top-level
// directories is one show, and episodes are found recursively inside. Video
// files placed straight in the root form one more show named after the
// root folder itself. The scanner only ever enqueues files into the ingest
// pipeline. Originals are never touched, moved or rewritten.

// MediaSource is one scanned media root. Kind is core.SourceFolder or
// core.SourceNAS; Allowlist, when non-empty, limits the shows (top-level
// directories) the scanner considers; an empty allowlist means every show.
type MediaSource struct {
	ID        int64
	Path      string // absolute directory path
	Kind      core.SourceKind
	Allowlist []string
}

// AddSource registers a media root, replacing any earlier registration of the
// same path. kind must be core.SourceFolder or core.SourceNAS; allowlist
// entries are deduplicated and sorted.
func (l *Library) AddSource(ctx context.Context, path string, kind core.SourceKind, allowlist []string) error {
	if kind != core.SourceFolder && kind != core.SourceNAS {
		return fmt.Errorf("library: invalid source kind %q", kind)
	}
	allow := sortedDedup(allowlist)
	if allow == nil {
		allow = []string{} // store [] rather than null
	}
	b, err := json.Marshal(allow)
	if err != nil {
		return err
	}
	_, err = l.s.DB().ExecContext(ctx, `
INSERT INTO sources (path, kind, allowlist)
VALUES (?, ?, ?)
ON CONFLICT(path) DO UPDATE SET
	kind      = excluded.kind,
	allowlist = excluded.allowlist`, path, kind, string(b))
	return err
}

// RemoveSource unregisters a media root. Removing a path that is not
// registered is a no-op.
func (l *Library) RemoveSource(ctx context.Context, path string) error {
	_, err := l.s.DB().ExecContext(ctx, `DELETE FROM sources WHERE path = ?`, path)
	return err
}

// Sources returns every registered media root, ordered by path.
func (l *Library) Sources(ctx context.Context) ([]MediaSource, error) {
	rows, err := l.s.DB().QueryContext(ctx,
		`SELECT id, path, kind, allowlist FROM sources ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MediaSource
	for rows.Next() {
		var src MediaSource
		var allow string
		if err := rows.Scan(&src.ID, &src.Path, &src.Kind, &allow); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(allow), &src.Allowlist); err != nil {
			return nil, fmt.Errorf("library: bad allowlist for %q: %w", src.Path, err)
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

func sortedDedup(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	uniq := out[:1]
	for _, s := range out[1:] {
		if s != uniq[len(uniq)-1] {
			uniq = append(uniq, s)
		}
	}
	return uniq
}

// videoExts are the extensions the scanner recognizes, like the legacy
// portal's VIDEO_EXTS.
var videoExts = []string{".mp4", ".m4v", ".mkv", ".webm", ".avi", ".mov", ".ts"}

// skippedName reports whether a directory or file entry is ignored: names
// starting with '#' (legacy ignore marks), the Synology @eaDir metadata
// directories and hidden entries.
func skippedName(name string) bool {
	return strings.HasPrefix(name, "#") || name == "@eaDir" || strings.HasPrefix(name, ".")
}

func isVideo(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	for _, v := range videoExts {
		if ext == v {
			return true
		}
	}
	return false
}

// titleFor derives an episode title from the file name, like the legacy
// portal did: the name without its extension.
func titleFor(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// Scan walks every registered media source and hands files the catalog does
// not know yet to the ingest queue. Callers run it periodically. See
// ScanNew for the rules.
func (l *Library) Scan(ctx context.Context, q core.IngestQueue) error {
	_, err := l.ScanNew(ctx, q)
	return err
}

// ScanNew is Scan reporting how many files this pass handed to the ingest
// queue, so the parent page can say whether a scan found anything.
//
// The pass is cheap when nothing changed: every directory's mtime is recorded
// in the scan_dirs table, and a directory whose mtime is unchanged skips
// re-listing its own files (children are still descended into, each tracking
// its own mtime), so an untouched tree costs one stat and one readdir per
// directory. Files are matched by their Source{Kind, Path}, where Path is the
// original file's canonical absolute path (core.CanonicalPath), the same
// provenance BySource resolves; a file already enqueued (its ingest job
// may still be running) is tracked in scan_enqueued keyed on (kind, canonical
// path), so nothing is enqueued twice.
//
// A file that was enqueued but never reached the shelf is looked at on every
// pass, even in an unchanged directory (copying a file over an old one does
// not change the directory's mtime): when its size or modification time
// differs from what scan_enqueued recorded and its job failed, it is
// enqueued again. A broken file that stays the same is never retried.
//
// Filesystem errors, like a source root that vanished (an unmounted NAS) or an
// unreadable directory, are tolerated so a transient mount problem does not
// fail the periodic pass; database errors are returned.
func (l *Library) ScanNew(ctx context.Context, q core.IngestQueue) (int, error) {
	srcs, err := l.Sources(ctx)
	if err != nil {
		return 0, err
	}
	p := &scanPass{q: q}
	if p.waiting, err = l.unpublishedEnqueued(ctx); err != nil {
		return 0, err
	}
	for _, src := range srcs {
		if err := l.scanSource(ctx, p, src); err != nil {
			return p.n, err
		}
	}
	return p.n, nil
}

// scanPass is the state of one ScanNew pass.
type scanPass struct {
	q core.IngestQueue
	// waiting holds the files enqueued in an earlier pass that have no
	// episode yet: still converting, or failed.
	waiting map[scanKey]bool
	n       int // files handed to the queue in this pass
}

type scanKey struct {
	kind core.SourceKind
	path string // canonical path
}

// recheck reports whether absPath is a file enqueued earlier that has not
// reached the shelf, so it is looked at even when its directory is
// unchanged.
func (p *scanPass) recheck(kind core.SourceKind, absPath string) bool {
	if len(p.waiting) == 0 {
		return false
	}
	path, err := core.CanonicalPath(absPath)
	return err == nil && p.waiting[scanKey{kind, path}]
}

// looseShowName is the show that video files placed straight in a source
// root belong to: the root folder's own name, or "Videos" when the root has
// no usable name (a drive root such as / or D:\).
func looseShowName(root string) string {
	name := filepath.Base(filepath.Clean(root))
	if name == "." || name == "" || strings.ContainsAny(name, `/\`) || strings.HasSuffix(name, ":") {
		return "Videos"
	}
	return name
}

func (l *Library) scanSource(ctx context.Context, p *scanPass, src MediaSource) error {
	root := src.Path
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil // missing root (e.g. NAS not mounted): nothing to do
	}
	mtime := info.ModTime().UnixNano()
	prev, known, err := l.dirMtime(ctx, root)
	if err != nil {
		return err
	}
	relist := !known || prev != mtime
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	allow := make(map[string]bool, len(src.Allowlist))
	for _, n := range src.Allowlist {
		allow[n] = true
	}
	loose := looseShowName(root)
	for _, e := range entries {
		name := e.Name()
		if skippedName(name) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if !fi.IsDir() {
			// A video placed straight in the root joins the show named
			// after the root folder.
			if !isVideo(name) || (len(allow) > 0 && !allow[loose]) {
				continue
			}
			file := filepath.Join(root, name)
			if relist || p.recheck(src.Kind, file) {
				if err := l.enqueueIfNew(ctx, p, src, loose, file); err != nil {
					return err
				}
			}
			continue
		}
		if len(allow) > 0 && !allow[name] {
			continue
		}
		if err := l.scanShowDir(ctx, p, src, name, filepath.Join(root, name)); err != nil {
			return err
		}
	}
	return l.setDirMtime(ctx, root, mtime)
}

// scanShowDir walks one show directory recursively, enqueuing every video
// file whose source the catalog does not know yet. A directory's mtime is
// recorded only after its own walk succeeds, so a walk interrupted by a
// transient error is retried on the next pass.
//
// The per-directory mtime shortcut decides only whether this directory's own
// files are re-listed; child directories are always descended into, each
// keeping its own mtime. Adding "Show/Season 1/new.mp4" changes Season 1's
// mtime but not Show's, and the file is still found.
func (l *Library) scanShowDir(ctx context.Context, p *scanPass, src MediaSource, showDir, dir string) error {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil
	}
	mtime := info.ModTime().UnixNano()
	prev, known, err := l.dirMtime(ctx, dir)
	if err != nil {
		return err
	}
	// relist is true when this directory itself may have gained or lost
	// entries since the last walk; it never gates recursion into children.
	relist := !known || prev != mtime

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // unreadable now: retry next pass
	}
	for _, e := range entries {
		name := e.Name()
		if skippedName(name) {
			continue
		}
		file := filepath.Join(dir, name)
		if e.IsDir() {
			if err := l.scanShowDir(ctx, p, src, showDir, file); err != nil {
				return err
			}
			continue
		}
		if isVideo(name) && (relist || p.recheck(src.Kind, file)) {
			if err := l.enqueueIfNew(ctx, p, src, showDir, file); err != nil {
				return err
			}
		}
	}

	if err := l.setDirMtime(ctx, dir, mtime); err != nil {
		return err
	}
	return nil
}

// enqueueIfNew hands one video file to the ingest queue unless the catalog
// already has an episode ingested from its source (kind + canonical path) or
// the file was enqueued in an earlier pass and has not changed since. The
// enqueued marker is recorded only after a successful Enqueue.
//
// A file enqueued before is enqueued again only when its size or
// modification time changed and its job failed (or is unknown to the
// queue). While the job still runs the old stamp is kept, so a file that
// was still being copied when its job started is retried once that job
// fails. A job that finished with an episode the parent has since removed
// is never retried.
func (l *Library) enqueueIfNew(ctx context.Context, p *scanPass, src MediaSource, showDir, absPath string) error {
	path, err := core.CanonicalPath(absPath)
	if err != nil {
		return err
	}
	source := core.Source{
		Kind:  src.Kind,
		Path:  path,
		Title: titleFor(absPath),
		Show:  showDir,
	}
	if _, found, err := l.BySource(ctx, source); err != nil {
		return err
	} else if found {
		return nil // already published
	}
	fi, err := os.Stat(absPath)
	if err != nil {
		return nil // gone or unreadable now: look again next pass
	}
	size, mtime := fi.Size(), fi.ModTime().UnixNano()
	rec, seen, err := l.scanRecord(ctx, src.Kind, path)
	if err != nil {
		return err
	}
	if !seen {
		return l.enqueue(ctx, p, absPath, source, size, mtime)
	}
	if rec.size == -1 {
		// Recorded before stamps were kept: remember the file as it is
		// now, so a later change is noticed.
		return l.markSeen(ctx, src.Kind, path, rec.job, size, mtime)
	}
	if rec.size == size && rec.mtime == mtime {
		return nil // unchanged: its job runs, finished or failed on this very file
	}
	if rec.job != "" {
		id, done, err := p.q.IngestStatus(ctx, rec.job)
		if err == nil {
			if !done {
				return nil // still running: keep the old stamp
			}
			if id != "" {
				// It reached the shelf and was removed since: never re-add.
				return l.markSeen(ctx, src.Kind, path, rec.job, size, mtime)
			}
		}
		// Failed, or the queue no longer knows the job: the changed file
		// gets a new try. Enqueue never duplicates a live job.
	}
	return l.enqueue(ctx, p, absPath, source, size, mtime)
}

// enqueue hands absPath to the queue and records the job and the file's
// stamp in scan_enqueued.
func (l *Library) enqueue(ctx context.Context, p *scanPass, absPath string, source core.Source, size, mtime int64) error {
	jobID, err := p.q.Enqueue(ctx, absPath, source)
	if err != nil {
		return err
	}
	if jobID == "" {
		return fmt.Errorf("library: ingest enqueued %q without a job id", absPath)
	}
	p.n++
	return l.markSeen(ctx, source.Kind, source.Path, jobID, size, mtime)
}

// CountVideos counts the video files a scan of root would find: the ones
// straight in root and every one in its folders, with the scanner's skip
// rules. The parent page uses it to tell a parent what a folder holds when
// it is added. Unreadable folders below root are skipped like the scanner
// skips them; an unreadable root is an error. Like the scanner, it reads a
// root that is a symlink through the link (filepath.WalkDir would not
// follow it), and treats symlinks below the root as files.
func CountVideos(ctx context.Context, root string) (int, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, err
	}
	n := 0
	count := func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable: the scanner skips it too
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if skippedName(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() && isVideo(d.Name()) {
			n++
		}
		return nil
	}
	for _, e := range entries {
		if !e.IsDir() {
			if err := count("", e, nil); err != nil {
				return n, err
			}
			continue
		}
		if err := filepath.WalkDir(filepath.Join(root, e.Name()), count); err != nil {
			return n, err
		}
	}
	return n, nil
}

// InPlaceFile is one episode the shelf plays straight from its original
// file in a folder or on a NAS (not an app-owned copy): File is the path
// the player opens, SourcePath the original's canonical path.
type InPlaceFile struct {
	File       string
	SourcePath string
}

// InPlaceFiles lists every in-place episode's original file, so the parent
// page can tell a parent which folders hold videos that cannot be found
// (a drive or NAS that is not connected, files moved away). One query; the
// files are never opened or changed.
func (l *Library) InPlaceFiles(ctx context.Context) ([]InPlaceFile, error) {
	rows, err := l.s.DB().QueryContext(ctx,
		`SELECT file, src_path FROM episodes WHERE app_owned = 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InPlaceFile
	for rows.Next() {
		var f InPlaceFile
		if err := rows.Scan(&f.File, &f.SourcePath); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// unpublishedEnqueued returns every file enqueued in an earlier pass that
// has no episode on the shelf.
func (l *Library) unpublishedEnqueued(ctx context.Context) (map[scanKey]bool, error) {
	rows, err := l.s.DB().QueryContext(ctx, `
SELECT e.kind, e.path FROM scan_enqueued e
WHERE NOT EXISTS (SELECT 1 FROM episodes ep WHERE ep.src_kind = e.kind AND ep.src_path = e.path)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[scanKey]bool)
	for rows.Next() {
		var k scanKey
		if err := rows.Scan(&k.kind, &k.path); err != nil {
			return nil, err
		}
		out[k] = true
	}
	return out, rows.Err()
}

func (l *Library) dirMtime(ctx context.Context, dir string) (int64, bool, error) {
	var mtime int64
	err := l.s.DB().QueryRowContext(ctx,
		`SELECT mtime FROM scan_dirs WHERE path = ?`, dir).Scan(&mtime)
	if err == nil {
		return mtime, true, nil
	}
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	return 0, false, err
}

func (l *Library) setDirMtime(ctx context.Context, dir string, mtime int64) error {
	_, err := l.s.DB().ExecContext(ctx, `
INSERT INTO scan_dirs (path, mtime) VALUES (?, ?)
ON CONFLICT(path) DO UPDATE SET mtime = excluded.mtime`, dir, mtime)
	return err
}

// scanRecord is one scan_enqueued row: the ingest job the scanner started
// for a file ("" for rows from before jobs were recorded) and the file's
// size and mtime when it was last looked at (-1 when not yet recorded).
type scanRecord struct {
	job         string
	size, mtime int64
}

func (l *Library) scanRecord(ctx context.Context, kind core.SourceKind, path string) (scanRecord, bool, error) {
	var rec scanRecord
	err := l.s.DB().QueryRowContext(ctx,
		`SELECT job, size, mtime FROM scan_enqueued WHERE kind = ? AND path = ?`, kind, path).
		Scan(&rec.job, &rec.size, &rec.mtime)
	if err == nil {
		return rec, true, nil
	}
	if err == sql.ErrNoRows {
		return rec, false, nil
	}
	return rec, false, err
}

func (l *Library) markSeen(ctx context.Context, kind core.SourceKind, path, job string, size, mtime int64) error {
	_, err := l.s.DB().ExecContext(ctx, `
INSERT INTO scan_enqueued (kind, path, job, size, mtime) VALUES (?, ?, ?, ?, ?)
ON CONFLICT(kind, path) DO UPDATE SET job = excluded.job, size = excluded.size, mtime = excluded.mtime`,
		kind, path, job, size, mtime)
	return err
}
