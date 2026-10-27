package library

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/edementhon/kids-shelf/internal/core"
)

// Media sources and the periodic scan live here. A source is one root
// directory (a local folder or a mounted NAS share); each of its top-level
// directories is one show, and episodes are found recursively inside. The
// scanner only ever enqueues files into the ingest pipeline — originals are
// never touched, moved or rewritten.

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
// not know yet to the ingest queue. Callers run it periodically.
//
// The pass is cheap when nothing changed: every directory's mtime is recorded
// in the scan_dirs table, and a directory whose mtime is unchanged skips
// re-listing its own files (children are still descended into, each tracking
// its own mtime), so an untouched tree costs one stat and one readdir per
// directory. Files are matched by their Source{Kind, Path} — Path is the
// original file's canonical absolute path (core.CanonicalPath), the same
// provenance BySource resolves — and a file already enqueued (its ingest job
// may still be running) is tracked in scan_enqueued keyed on (kind, canonical
// path), so nothing is enqueued twice.
//
// Filesystem errors — a source root that vanished (an unmounted NAS), an
// unreadable directory — are tolerated so a transient mount problem does not
// fail the periodic pass; database errors are returned.
func (l *Library) Scan(ctx context.Context, q core.IngestQueue) error {
	srcs, err := l.Sources(ctx)
	if err != nil {
		return err
	}
	for _, src := range srcs {
		if err := l.scanSource(ctx, q, src); err != nil {
			return err
		}
	}
	return nil
}

func (l *Library) scanSource(ctx context.Context, q core.IngestQueue, src MediaSource) error {
	root := src.Path
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil // missing root (e.g. NAS not mounted): nothing to do
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	allow := make(map[string]bool, len(src.Allowlist))
	for _, n := range src.Allowlist {
		allow[n] = true
	}
	for _, e := range entries {
		name := e.Name()
		if skippedName(name) {
			continue
		}
		fi, err := e.Info()
		if err != nil || !fi.IsDir() {
			continue // one show is one top-level directory
		}
		if len(allow) > 0 && !allow[name] {
			continue
		}
		if err := l.scanShowDir(ctx, q, src, name, filepath.Join(root, name)); err != nil {
			return err
		}
	}
	return nil
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
func (l *Library) scanShowDir(ctx context.Context, q core.IngestQueue, src MediaSource, showDir, dir string) error {
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
		if e.IsDir() {
			if err := l.scanShowDir(ctx, q, src, showDir, filepath.Join(dir, name)); err != nil {
				return err
			}
			continue
		}
		if relist && isVideo(name) {
			if err := l.enqueueIfNew(ctx, q, src, showDir, filepath.Join(dir, name)); err != nil {
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
// the file was enqueued in an earlier pass. The enqueued marker is recorded
// only after a successful Enqueue.
func (l *Library) enqueueIfNew(ctx context.Context, q core.IngestQueue, src MediaSource, showDir, absPath string) error {
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
	seen, err := l.scanSeen(ctx, src.Kind, source.Path)
	if err != nil {
		return err
	}
	if seen {
		return nil // already enqueued; its ingest job may still be running
	}
	jobID, err := q.Enqueue(ctx, absPath, source)
	if err != nil {
		return err
	}
	if jobID == "" {
		return fmt.Errorf("library: ingest enqueued %q without a job id", absPath)
	}
	return l.markSeen(ctx, src.Kind, source.Path)
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

func (l *Library) scanSeen(ctx context.Context, kind core.SourceKind, path string) (bool, error) {
	var one int
	err := l.s.DB().QueryRowContext(ctx,
		`SELECT 1 FROM scan_enqueued WHERE kind = ? AND path = ?`, kind, path).Scan(&one)
	if err == nil {
		return true, nil
	}
	if err == sql.ErrNoRows {
		return false, nil
	}
	return false, err
}

func (l *Library) markSeen(ctx context.Context, kind core.SourceKind, path string) error {
	_, err := l.s.DB().ExecContext(ctx, `
INSERT INTO scan_enqueued (kind, path) VALUES (?, ?)
ON CONFLICT(kind, path) DO NOTHING`, kind, path)
	return err
}
