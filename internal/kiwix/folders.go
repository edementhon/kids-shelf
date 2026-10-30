package kiwix

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Library folders: folders on this computer (or on a drive or NAS share it
// can reach) that already hold ZIM files. kids-shelf serves every .zim in
// them where it is: nothing is copied, and kids-shelf never opens such a
// file for writing, renames or deletes it, and keeps no disk space aside
// for it. Each file is one installed row whose folder column is the
// folder's path and whose file column is the ZIM's absolute path.

// Errors AddFolder, RemoveFolder and ScanFolder return. The web layer puts
// them into plain words.
var (
	ErrFolderNotFound = errors.New("kiwix: folder not found")
	ErrNotAFolder     = errors.New("kiwix: not a folder")
	ErrLibraryFolder  = errors.New("kiwix: kids-shelf's own library folder")
	ErrFolderAdded    = errors.New("kiwix: folder already added")
	ErrFolderNested   = errors.New("kiwix: folder inside or around an added folder")
	ErrNoSuchFolder   = errors.New("kiwix: not a library folder")
)

// folderBookPrefix starts the id of every library folder book; catalog
// ids are urn:uuid: ids, so the two never collide.
const folderBookPrefix = "folder:"

// Folder is one library folder and what kids-shelf has from it.
type Folder struct {
	Path    string // absolute and cleaned, as the parent gave it
	AddedAt time.Time
	Books   int   // libraries on the shelf from it
	Size    int64 // their total size in bytes
}

// Reachable reports whether kids-shelf can read the folder right now (see
// readFolder). It opens the folder and reads at most one entry.
func (f Folder) Reachable() bool {
	_, ok := readFolder(f.Path, f.Books > 0, 1)
	return ok
}

// FolderScan is what one look through a library folder found.
type FolderScan struct {
	// Reachable is false when the folder could not be read; its libraries
	// were then kept exactly as they were.
	Reachable bool
	Added     int // libraries new on the shelf
	Dropped   int // libraries whose file is gone from the folder
	Books     int // libraries on the shelf from the folder now
	resized   int // libraries whose file changed size (a newer copy under the same name)
}

func (r FolderScan) changed() bool {
	return r.Added+r.Dropped+r.resized > 0
}

// Folders lists the library folders, ordered by path, with how many
// libraries each one has on the shelf and their total size.
func (m *Manager) Folders() ([]Folder, error) {
	rows, err := m.store.DB().Query(`
SELECT f.path, f.added_at, COUNT(i.id), COALESCE(SUM(i.size), 0)
FROM library_folders AS f LEFT JOIN installed AS i ON i.folder = f.path
GROUP BY f.path, f.added_at
ORDER BY f.path`)
	if err != nil {
		return nil, fmt.Errorf("kiwix: library folders: %w", err)
	}
	defer rows.Close()
	var out []Folder
	for rows.Next() {
		var f Folder
		var at string
		if err := rows.Scan(&f.Path, &at, &f.Books, &f.Size); err != nil {
			return nil, fmt.Errorf("kiwix: library folders: %w", err)
		}
		f.AddedAt, _ = time.Parse(time.RFC3339Nano, at)
		out = append(out, f)
	}
	return out, rows.Err()
}

// AddFolder adds a library folder and looks through it at once (see
// ScanFolder), returning how many libraries it holds. path must be the
// absolute path of a folder that exists now. kids-shelf's own library
// folder, or a folder inside it, is refused (its libraries are on the shelf
// already), and so is a folder already on the list or one inside or around
// a folder on it: its files would be on the shelf twice.
func (m *Manager) AddFolder(ctx context.Context, path string) (int, error) {
	if !filepath.IsAbs(path) {
		return 0, fmt.Errorf("kiwix: library folder %q is not an absolute path", path)
	}
	path = filepath.Clean(path)
	st, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrFolderNotFound, err)
	}
	if !st.IsDir() {
		return 0, ErrNotAFolder
	}
	if m.inLibraryDir(path) {
		return 0, ErrLibraryFolder
	}
	res, err := m.addFolder(ctx, path)
	if res.changed() {
		m.notifyChange()
	}
	return res.Books, err
}

func (m *Manager) addFolder(ctx context.Context, path string) (FolderScan, error) {
	m.folderMu.Lock()
	defer m.folderMu.Unlock()
	folders, err := m.Folders()
	if err != nil {
		return FolderScan{}, err
	}
	resolved := resolvePath(path)
	for _, f := range folders {
		other := resolvePath(f.Path)
		if samePath(f.Path, path) || samePath(other, resolved) {
			return FolderScan{}, ErrFolderAdded
		}
		if within(f.Path, path) || within(path, f.Path) || within(other, resolved) || within(resolved, other) {
			return FolderScan{}, ErrFolderNested
		}
	}
	if _, err := m.store.DB().Exec(`INSERT INTO library_folders (path, added_at) VALUES (?, ?)`,
		path, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return FolderScan{}, fmt.Errorf("kiwix: add library folder %s: %w", path, err)
	}
	return m.scanFolder(ctx, path)
}

// RemoveFolder stops using a library folder: the folder leaves the list
// and the libraries from it leave the shelf. The files themselves stay
// where they are, untouched. A path that is not on the list returns
// ErrNoSuchFolder. It never waits for a look through the folders (one
// stuck on a NAS that stopped answering, say): a look that ends after it
// finds the folder gone and changes nothing.
func (m *Manager) RemoveFolder(path string) error {
	removed, err := m.removeFolder(path)
	if removed > 0 {
		m.notifyChange()
	}
	return err
}

func (m *Manager) removeFolder(path string) (int64, error) {
	tx, err := m.store.DB().Begin()
	if err != nil {
		return 0, fmt.Errorf("kiwix: remove library folder %s: %w", path, err)
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM library_folders WHERE path = ?`, path)
	if err != nil {
		return 0, fmt.Errorf("kiwix: remove library folder %s: %w", path, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNoSuchFolder
	}
	res, err = tx.Exec(`DELETE FROM installed WHERE folder = ?`, path)
	if err != nil {
		return 0, fmt.Errorf("kiwix: remove library folder %s: %w", path, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("kiwix: remove library folder %s: %w", path, err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ScanFolders looks through every library folder again (see ScanFolder).
// The app runs it at start and every few minutes. OnChange runs once at
// the end when anything changed, however many files did.
func (m *Manager) ScanFolders(ctx context.Context) error {
	changed, err := m.scanFolders(ctx)
	if changed {
		m.notifyChange()
	}
	return err
}

func (m *Manager) scanFolders(ctx context.Context) (changed bool, err error) {
	m.folderMu.Lock()
	defer m.folderMu.Unlock()
	folders, err := m.Folders()
	if err != nil {
		return false, err
	}
	for _, f := range folders {
		res, err := m.scanFolder(ctx, f.Path)
		changed = changed || res.changed()
		if err != nil {
			return changed, err
		}
	}
	return changed, nil
}

// ScanFolder looks through one library folder again (the parent's "Look
// again"). Every regular file ending in .zim, in any case, in the folder or
// a folder inside it is a library; a link to such a file counts too.
// Hidden files and folders (and the Synology @eaDir and #recycle kind) are
// skipped, and so is kids-shelf's own library folder when it lies inside.
// When the folder can be read, new files join the shelf and libraries whose
// file is gone leave it; a folder that cannot be read (a drive that is not
// plugged in, a NAS that is off) keeps its libraries, which then show as
// missing. OnChange runs once when anything changed. A path that is not on
// the list returns ErrNoSuchFolder.
func (m *Manager) ScanFolder(ctx context.Context, path string) (FolderScan, error) {
	res, err := m.scanOneFolder(ctx, path)
	if res.changed() {
		m.notifyChange()
	}
	return res, err
}

func (m *Manager) scanOneFolder(ctx context.Context, path string) (FolderScan, error) {
	m.folderMu.Lock()
	defer m.folderMu.Unlock()
	var one int
	if err := m.store.DB().QueryRow(`SELECT 1 FROM library_folders WHERE path = ?`, path).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return FolderScan{}, ErrNoSuchFolder
		}
		return FolderScan{}, fmt.Errorf("kiwix: library folder %s: %w", path, err)
	}
	return m.scanFolder(ctx, path)
}

// folderRow is one installed row from a library folder, as a scan compares
// it with the files it finds.
type folderRow struct {
	id   string
	size int64
}

// scanFolder looks through folder and brings its installed rows in line
// with the .zim files in it (see ScanFolder), in one transaction. The
// caller holds folderMu and reports a change to OnChange. A ctx that ends
// mid-look changes nothing.
func (m *Manager) scanFolder(ctx context.Context, folder string) (FolderScan, error) {
	rows, err := m.folderRows(folder)
	if err != nil {
		return FolderScan{}, err
	}
	res := FolderScan{Books: len(rows)}
	entries, ok := readFolder(folder, len(rows) > 0, -1)
	if !ok {
		return res, nil
	}
	w := folderWalk{found: make(map[string]int64)}
	if lib, err := os.Stat(m.lib); err == nil {
		w.lib = lib
	}
	if err := w.walk(ctx, folder, entries); err != nil {
		return res, err
	}

	// The store's transactions take the write lock as they begin, so the
	// folder cannot leave the list between this check and the commit; one
	// that left during the look (Stop using this folder) gets no rows back.
	tx, err := m.store.DB().Begin()
	if err != nil {
		return res, fmt.Errorf("kiwix: library folder %s: %w", folder, err)
	}
	defer tx.Rollback()
	var one int
	if err := tx.QueryRow(`SELECT 1 FROM library_folders WHERE path = ?`, folder).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return FolderScan{}, nil
		}
		return res, fmt.Errorf("kiwix: library folder %s: %w", folder, err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var next FolderScan
	for file, size := range w.found {
		if row, ok := rows[file]; ok {
			if row.size != size {
				if _, err := tx.Exec(`UPDATE installed SET size = ? WHERE id = ?`, size, row.id); err != nil {
					return res, fmt.Errorf("kiwix: library folder %s: %w", folder, err)
				}
				next.resized++
			}
			continue
		}
		// Until kiwix-serve reports the ZIM's own name, title and language
		// (RecordServed), the row reads its file name: no folder, no .zim,
		// and underscores as spaces in the title.
		base := Installed{File: file}.FileBase()
		r, err := tx.Exec(`
INSERT OR IGNORE INTO installed (id, name, title, language, file, size, installed_at, folder)
VALUES (?, ?, ?, '', ?, ?, ?, ?)`,
			folderBookID(file), base, strings.ReplaceAll(base, "_", " "), file, size, now, folder)
		if err != nil {
			return res, fmt.Errorf("kiwix: library folder %s: %w", folder, err)
		}
		if n, _ := r.RowsAffected(); n > 0 {
			next.Added++
		}
	}
	for file, row := range rows {
		if _, ok := w.found[file]; ok || w.unread(file) {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM installed WHERE id = ?`, row.id); err != nil {
			return res, fmt.Errorf("kiwix: library folder %s: %w", folder, err)
		}
		next.Dropped++
	}
	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("kiwix: library folder %s: %w", folder, err)
	}
	next.Reachable = true
	next.Books = len(rows) + next.Added - next.Dropped
	return next, nil
}

// folderRows returns the installed rows from folder, keyed by file path.
func (m *Manager) folderRows(folder string) (map[string]folderRow, error) {
	rows, err := m.store.DB().Query(`SELECT id, file, size FROM installed WHERE folder = ?`, folder)
	if err != nil {
		return nil, fmt.Errorf("kiwix: library folder %s: %w", folder, err)
	}
	defer rows.Close()
	out := make(map[string]folderRow)
	for rows.Next() {
		var r folderRow
		var file string
		if err := rows.Scan(&r.id, &file, &r.size); err != nil {
			return nil, fmt.Errorf("kiwix: library folder %s: %w", folder, err)
		}
		out[file] = r
	}
	return out, rows.Err()
}

// folderBookID is the installed id of the library folder ZIM at path: one
// path, one id, so a look through the folder recognizes the files it
// already knows.
func folderBookID(path string) string {
	sum := sha256.Sum256([]byte(path))
	return folderBookPrefix + hex.EncodeToString(sum[:])
}

// readFolder opens a library folder and reads up to limit of its entries
// (all of them when limit <= 0). It reports the folder out of reach when it
// cannot be read (a drive that is not plugged in, a NAS that is off, a path
// that is now a file), and also when it is empty while kids-shelf has
// libraries from it (hadBooks): a drive or NAS share that is not mounted
// often leaves its empty mount folder behind, and that must not read as
// "every file is gone".
func readFolder(path string, hadBooks bool, limit int) ([]os.DirEntry, bool) {
	d, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer d.Close()
	entries, err := d.ReadDir(limit)
	if err != nil && (limit <= 0 || !errors.Is(err, io.EOF)) {
		return nil, false
	}
	if len(entries) == 0 && hadBooks {
		return nil, false
	}
	return entries, true
}

// folderWalk is one look through a library folder's tree.
type folderWalk struct {
	lib        os.FileInfo      // kids-shelf's own library folder, skipped; nil when unknown
	found      map[string]int64 // every ZIM found, by path, with its size
	unreadable []string         // folders inside that could not be read
}

// walk adds the ZIMs among a folder's entries to w.found and descends into
// its folders. A folder inside that cannot be read is noted in
// w.unreadable, so its libraries are kept rather than dropped.
func (w *folderWalk) walk(ctx context.Context, dir string, entries []os.DirEntry) error {
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := e.Name()
		if skippedFolderEntry(name) {
			continue
		}
		p := filepath.Join(dir, name)
		switch t := e.Type(); {
		case t.IsDir():
			if w.isLibrary(p) {
				continue
			}
			sub, err := os.ReadDir(p)
			if err != nil {
				w.unreadable = append(w.unreadable, p)
				continue
			}
			if err := w.walk(ctx, p, sub); err != nil {
				return err
			}
		case !strings.EqualFold(filepath.Ext(name), ".zim"):
		case t.IsRegular():
			if info, err := e.Info(); err == nil {
				w.found[p] = info.Size()
			}
		case t&fs.ModeSymlink != 0:
			// A link to a ZIM is followed; a link to a folder is not, so a
			// loop of links can never trap the walk.
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
				w.found[p] = st.Size()
			}
		}
	}
	return nil
}

// isLibrary reports whether dir is kids-shelf's own library folder (with
// its .partial downloads), whatever path leads to it.
func (w *folderWalk) isLibrary(dir string) bool {
	if w.lib == nil {
		return false
	}
	st, err := os.Stat(dir)
	return err == nil && os.SameFile(st, w.lib)
}

// unread reports whether file lies in a folder this walk could not read.
func (w *folderWalk) unread(file string) bool {
	for _, d := range w.unreadable {
		if within(d, file) {
			return true
		}
	}
	return false
}

// skippedFolderEntry reports whether a look through a library folder
// passes over an entry, by the video scanner's rules: hidden files and
// folders (".partial", the "._x.zim" files a Mac leaves on a drive), the
// "#recycle" kind of folder and Synology's @eaDir.
func skippedFolderEntry(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "#") || name == "@eaDir"
}

// inLibraryDir reports whether path is kids-shelf's own library folder or
// a folder inside it, whatever path leads there (a link, another spelling
// on a disk that ignores case).
func (m *Manager) inLibraryDir(path string) bool {
	if within(m.lib, path) || within(resolvePath(m.lib), resolvePath(path)) {
		return true
	}
	lib, err := os.Stat(m.lib)
	if err != nil {
		return false
	}
	for p := path; ; {
		if st, err := os.Stat(p); err == nil && os.SameFile(st, lib) {
			return true
		}
		parent := filepath.Dir(p)
		if parent == p {
			return false
		}
		p = parent
	}
}

// resolvePath is path with its links resolved, or just cleaned when that
// fails (a folder that is out of reach right now).
func resolvePath(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return filepath.Clean(path)
}

// foldPath cleans a path for comparing, ignoring case where the file
// system usually does (Windows, macOS).
func foldPath(p string) string {
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		p = strings.ToLower(p)
	}
	return p
}

// samePath reports whether a and b name the same path.
func samePath(a, b string) bool {
	return foldPath(a) == foldPath(b)
}

// within reports whether p is root or lies inside it.
func within(root, p string) bool {
	root, p = foldPath(root), foldPath(p)
	if root == p {
		return true
	}
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
