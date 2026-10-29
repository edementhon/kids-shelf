// Package library owns the media catalog: shows and their episodes, the bins
// shows live in (fun, learning, free), the media sources the scanner watches,
// and the playback history (resume positions, watch log, household recent
// list). It implements the core.Publisher, core.EpisodeLookup and
// core.PlaybackImporter interfaces ingest and migrate call into, and offers
// the catalog queries the web UI renders.
//
// The tables here are owned by this package alone (its migration carries
// Owner "library"): nothing outside this package touches them, and this
// package never touches tables another package owns.
package library

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/edementhon/kids-shelf/internal/core"
	"github.com/edementhon/kids-shelf/internal/store"
)

// Bins are the price tiers shows are filed into. Fun costs full price (rate
// 1.0), learning half (0.5) and free nothing; new shows land in fun.
const (
	BinFun      = "fun"
	BinLearning = "learning"
	BinFree     = "free"
)

var validBins = map[string]bool{BinFun: true, BinLearning: true, BinFree: true}

// BinRateOf returns the per-minute cost multiplier of a bin.
func BinRateOf(bin string) float64 {
	switch bin {
	case BinLearning:
		return 0.5
	case BinFree:
		return 0.0
	default:
		return 1.0
	}
}

// ErrNotFound is returned when no row matches the requested id or show.
var ErrNotFound = errors.New("library: not found")

// Migrations registers the library tables with the store. One owner is one
// package, so no other package owns any of these tables.
var Migrations = []store.Migration{{
	Owner:   "library",
	Version: 1,
	SQL: `
CREATE TABLE shows (
	id     TEXT PRIMARY KEY,
	name   TEXT NOT NULL UNIQUE,
	bin    TEXT NOT NULL DEFAULT 'fun' CHECK (bin IN ('fun', 'learning', 'free')),
	poster TEXT NOT NULL DEFAULT ''
);

CREATE TABLE episodes (
	id           TEXT    PRIMARY KEY,
	show_id      TEXT    NOT NULL REFERENCES shows(id),
	title        TEXT    NOT NULL,
	file         TEXT    NOT NULL,
	duration_sec INTEGER NOT NULL DEFAULT 0,
	thumb        TEXT    NOT NULL DEFAULT '',
	src_kind     TEXT    NOT NULL,
	src_path     TEXT    NOT NULL,
	added        TEXT    NOT NULL
);
CREATE INDEX episodes_show ON episodes(show_id);
CREATE INDEX episodes_src ON episodes(src_kind, src_path);

CREATE TABLE resume (
	kid      TEXT    NOT NULL,
	episode  TEXT    NOT NULL REFERENCES episodes(id),
	pos      REAL    NOT NULL DEFAULT 0,
	updated  TEXT    NOT NULL,
	PRIMARY KEY (kid, episode)
);

CREATE TABLE watch_log (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	kid     TEXT    NOT NULL,
	episode TEXT    NOT NULL REFERENCES episodes(id),
	at      TEXT    NOT NULL,
	title   TEXT    NOT NULL,
	cost    REAL    NOT NULL DEFAULT 0,
	free    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX watch_log_at ON watch_log(at DESC, id DESC);

CREATE TABLE recent (
	episode TEXT PRIMARY KEY REFERENCES episodes(id),
	at      TEXT NOT NULL,
	kid     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX recent_at ON recent(at DESC);

CREATE TABLE sources (
	id        INTEGER PRIMARY KEY AUTOINCREMENT,
	path      TEXT NOT NULL UNIQUE,
	kind      TEXT NOT NULL CHECK (kind IN ('folder', 'nas')),
	allowlist TEXT NOT NULL DEFAULT '[]'
);

CREATE TABLE scan_dirs (
	path  TEXT PRIMARY KEY,
	mtime INTEGER NOT NULL
);

CREATE TABLE scan_enqueued (
	kind TEXT NOT NULL,
	path TEXT NOT NULL,
	PRIMARY KEY (kind, path)
)`,
},

	{
		Owner:   "library",
		Version: 2,
		SQL: `
-- ImportWatch must be re-runnable: re-importing the same legacy watch-log
-- entry appends a duplicate row. Rebuild the table with a UNIQUE identity
-- (kid, episode, watched-at), keeping one row per existing duplicate, and
-- import with ON CONFLICT DO NOTHING from now on.
CREATE TABLE watch_log_new (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	kid     TEXT    NOT NULL,
	episode TEXT    NOT NULL REFERENCES episodes(id),
	at      TEXT    NOT NULL,
	title   TEXT    NOT NULL,
	cost    REAL    NOT NULL DEFAULT 0,
	free    INTEGER NOT NULL DEFAULT 0,
	UNIQUE (kid, episode, at)
);
INSERT INTO watch_log_new (id, kid, episode, at, title, cost, free)
SELECT MIN(id), kid, episode, at, title, cost, free
FROM watch_log
GROUP BY kid, episode, at;
DROP TABLE watch_log;
ALTER TABLE watch_log_new RENAME TO watch_log;
CREATE INDEX watch_log_at ON watch_log(at DESC, id DESC);
`,
	},
	{
		Owner:   "library",
		Version: 3,
		SQL: `
-- Fixed-width UTC timestamps. RFC3339Nano drops trailing zeros, so
-- '12:00:00Z' sorts after '12:00:00.1Z' in SQL; padding every fractional
-- part to 9 digits makes lexicographic order chronological order again.
-- Rows written before this migration had the canonical RFC3339Nano form
-- (UTC, optional 1-9 digit fraction, trailing Z), which the expression
-- below re-emits width-fixed.
UPDATE episodes SET added =
	substr(added, 1, 19) || '.' ||
	CASE WHEN substr(added, 20, 1) = '.'
		THEN substr(substr(added, 21, length(added) - 21) || '000000000', 1, 9)
		ELSE '000000000' END || 'Z';
UPDATE resume SET updated =
	substr(updated, 1, 19) || '.' ||
	CASE WHEN substr(updated, 20, 1) = '.'
		THEN substr(substr(updated, 21, length(updated) - 21) || '000000000', 1, 9)
		ELSE '000000000' END || 'Z';
UPDATE watch_log SET at =
	substr(at, 1, 19) || '.' ||
	CASE WHEN substr(at, 20, 1) = '.'
		THEN substr(substr(at, 21, length(at) - 21) || '000000000', 1, 9)
		ELSE '000000000' END || 'Z';
UPDATE recent SET at =
	substr(at, 1, 19) || '.' ||
	CASE WHEN substr(at, 20, 1) = '.'
		THEN substr(substr(at, 21, length(at) - 21) || '000000000', 1, 9)
		ELSE '000000000' END || 'Z';
`,
	},
	{
		Owner:   "library",
		Version: 4,
		SQL: `
-- SetShowBin stores a bin by show name so the choice survives until the
-- show is first published; Publish applies it when it creates the show.
CREATE TABLE show_bins (
	name TEXT PRIMARY KEY,
	bin  TEXT NOT NULL CHECK (bin IN ('fun', 'learning', 'free'))
);
`,
	},
	{
		Owner:   "library",
		Version: 5,
		SQL: `
-- Owned marks whether the episode's file is app-owned. Every row before
-- in-place serving existed was a converted file in the media folder (or a
-- YouTube download), so the default true is exactly right for old rows;
-- ingest sets it false only for folder/NAS originals that play in place
-- from their original path.
ALTER TABLE episodes ADD COLUMN app_owned INTEGER NOT NULL DEFAULT 1
	CHECK (app_owned IN (0, 1));
`,
	},
	{
		Owner:   "library",
		Version: 6,
		SQL: `
-- issue and fix tell the pages why an episode is not playing well: issue
-- is what is wrong with the current file ("" when it plays everywhere,
-- else 'sound', 'picture' or 'format') and fix is the state of the better
-- copy ("" when nothing is pending, 'pending' while one is queued or
-- being made, 'failed' when kids-shelf could not make it). Ingest sets
-- both at the show-phase publish, clears them when the improved copy is
-- published, and marks fix 'failed' when the improvement fails.
ALTER TABLE episodes ADD COLUMN issue TEXT NOT NULL DEFAULT ''
	CHECK (issue IN ('', 'sound', 'picture', 'format'));
ALTER TABLE episodes ADD COLUMN fix TEXT NOT NULL DEFAULT ''
	CHECK (fix IN ('', 'pending', 'failed'));
`,
	},
	{
		Owner:   "library",
		Version: 7,
		SQL: `
-- The scanner remembers, per enqueued file, the ingest job it started and
-- the file's size and modification time (Unix nanoseconds) when it last
-- looked. A file whose job failed is enqueued again once its size or
-- modification time changes, so a parent who replaces a broken file sees
-- it reach the shelf after the next scan. Rows from before this migration
-- have no job ('') and no fingerprint (-1): the scanner records the
-- fingerprint on its next pass and treats a later change the same way.
ALTER TABLE scan_enqueued ADD COLUMN job TEXT NOT NULL DEFAULT '';
ALTER TABLE scan_enqueued ADD COLUMN size INTEGER NOT NULL DEFAULT -1;
ALTER TABLE scan_enqueued ADD COLUMN mtime INTEGER NOT NULL DEFAULT -1;
`,
	},
}

// Library is the catalog over a store.
type Library struct {
	s   *store.Store
	now func() time.Time
}

// New returns a Library on s, applying the library migrations if they are not
// yet recorded. It is safe (and a no-op) to call more than once.
func New(s *store.Store) (*Library, error) {
	if err := s.Migrate(Migrations...); err != nil {
		return nil, err
	}
	return &Library{s: s, now: time.Now}, nil
}

// Show is one show of the shelf.
type Show struct {
	ID       core.ShowID
	Name     string
	Bin      string
	Poster   string // id of the first thumbed episode in natural title order; "" when none
	Episodes int    // published episode count; 0 when a show has none
}

// Episode is a published episode joined with its show.
type Episode struct {
	ID          core.EpisodeID
	Show        core.ShowID
	ShowName    string
	Title       string
	File        string // the playable file: converted (library) path, or the original path when Owned is false
	DurationSec int64
	Thumb       string
	Source      core.Source // Kind and Path are the recorded provenance
	Added       time.Time

	// Owned reports whether File is app-owned (a converted copy or a
	// YouTube download). An in-place episode (a folder/NAS original that
	// plays from its original path) has Owned false; its file lives
	// outside the media folder, is never deleted or moved by the app, and
	// may be unavailable while its NAS or drive is down.
	Owned bool

	// Issue is what is wrong with File for the players: "" when it plays
	// everywhere, else "sound", "picture" or "format". Fix is the state
	// of the better copy: "" (final), "pending" (a better copy is queued
	// or being made; the episode does not play until it lands) or
	// "failed" (kids-shelf could not make one, the current file keeps
	// playing). Ingest writes both; see core.Issue* and core.Fix*.
	Issue string
	Fix   string
}

// SearchResults is what Search returns: shows whose name matches and episodes
// whose title matches, independently.
type SearchResults struct {
	Shows    []Show
	Episodes []Episode
}

const selectEpisode = `
SELECT e.id, e.show_id, s.name, e.title, e.file, e.duration_sec, e.thumb,
       e.src_kind, e.src_path, e.app_owned, e.issue, e.fix, e.added
FROM episodes e
JOIN shows s ON s.id = e.show_id`

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

func scanEpisode(row scanner) (Episode, error) {
	var ep Episode
	var added string
	err := row.Scan(&ep.ID, &ep.Show, &ep.ShowName, &ep.Title, &ep.File,
		&ep.DurationSec, &ep.Thumb, &ep.Source.Kind, &ep.Source.Path,
		&ep.Owned, &ep.Issue, &ep.Fix, &added)
	if err != nil {
		return Episode{}, err
	}
	t, err := parseTime(added)
	if err != nil {
		return Episode{}, fmt.Errorf("library: bad added time %q: %w", added, err)
	}
	ep.Added = t
	return ep, nil
}

// Publish files a verified episode: the show is created by name (from the
// episode's source) when missing, taking the bin configured through
// SetShowBin or defaulting to fun, and the episode is upserted by its id, so
// republishing is idempotent and never bumps the episode's added time.
func (l *Library) Publish(ctx context.Context, ep core.Episode) error {
	if ep.ID == "" {
		return errors.New("library: publish without episode id")
	}
	showName := strings.TrimSpace(ep.Source.Show)
	if showName == "" {
		return errors.New("library: publish without a show name in the episode source")
	}
	tx, err := l.s.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var showID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM shows WHERE name = ?`, showName).Scan(&showID)
	if err == sql.ErrNoRows {
		id := ep.ShowID
		if id == "" {
			id, err = newID()
			if err != nil {
				return err
			}
		}
		bin, err := l.binForNewShow(ctx, tx, showName)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO shows (id, name, bin) VALUES (?, ?, ?)`, id, showName, bin); err != nil {
			return err
		}
		showID = id
	} else if err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx, `
INSERT INTO episodes (id, show_id, title, file, duration_sec, thumb, src_kind, src_path, app_owned, issue, fix, added)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	show_id     = excluded.show_id,
	title       = excluded.title,
	file        = excluded.file,
	duration_sec = excluded.duration_sec,
	thumb       = excluded.thumb,
	src_kind    = excluded.src_kind,
	src_path    = excluded.src_path,
	app_owned   = excluded.app_owned,
	issue       = excluded.issue,
	fix         = excluded.fix`,
		ep.ID, showID, ep.Title, ep.File, ep.DurationSec, ep.Thumb,
		ep.Source.Kind, ep.Source.Path, boolInt(ep.Owned), ep.Issue, ep.Fix, formatTime(l.now()))
	if err != nil {
		return err
	}
	return tx.Commit()
}

// BySource resolves a source (kind + path) to the episode ingested from it
// and that episode's show.
func (l *Library) BySource(ctx context.Context, src core.Source) (core.EpisodeRef, bool, error) {
	var ref core.EpisodeRef
	err := l.s.DB().QueryRowContext(ctx,
		`SELECT id, show_id FROM episodes WHERE src_kind = ? AND src_path = ? LIMIT 1`,
		src.Kind, src.Path).Scan(&ref.Episode, &ref.Show)
	if err == sql.ErrNoRows {
		return ref, false, nil
	}
	if err != nil {
		return ref, false, err
	}
	return ref, true, nil
}

// SetBin files a show into a bin: "fun", "learning" or "free".
func (l *Library) SetBin(ctx context.Context, show core.ShowID, bin string) error {
	if !validBins[bin] {
		return fmt.Errorf("library: invalid bin %q", bin)
	}
	res, err := l.s.DB().ExecContext(ctx, `UPDATE shows SET bin = ? WHERE id = ?`, bin, show)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetShowBin files the show with this name into a bin, whether or not the
// show exists yet: the choice is stored in show_bins and applied when a show
// of that name is first published, and an existing show is re-filed at once.
func (l *Library) SetShowBin(ctx context.Context, showName string, bin string) error {
	if !validBins[bin] {
		return fmt.Errorf("library: invalid bin %q", bin)
	}
	showName = strings.TrimSpace(showName)
	if showName == "" {
		return errors.New("library: set show bin without a show name")
	}
	tx, err := l.s.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO show_bins (name, bin) VALUES (?, ?)
ON CONFLICT(name) DO UPDATE SET bin = excluded.bin`, showName, bin); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE shows SET bin = ? WHERE name = ?`, bin, showName); err != nil {
		return err
	}
	return tx.Commit()
}

// binForNewShow returns the bin a newly created show gets: the choice stored
// through SetShowBin when present, the fun default otherwise.
func (l *Library) binForNewShow(ctx context.Context, tx *sql.Tx, showName string) (string, error) {
	var bin string
	err := tx.QueryRowContext(ctx, `SELECT bin FROM show_bins WHERE name = ?`, showName).Scan(&bin)
	if err == sql.ErrNoRows {
		return BinFun, nil
	}
	if err != nil {
		return "", err
	}
	return bin, nil
}

// posterCand is one thumbed episode that can stand in as its show's poster:
// the shelf is picture-first, so kids recognize a show by the first episode
// thumbnail they would see in its episode band.
type posterCand struct {
	id    core.EpisodeID
	title string
}

// ListShows returns every show with its bin, poster and published episode
// count, ordered by name. A show's poster is its first episode in natural
// title order that has a thumbnail, as that episode's id (the shows.poster
// column is never written); a show with no thumbed episode gets "".
func (l *Library) ListShows(ctx context.Context) ([]Show, error) {
	rows, err := l.s.DB().QueryContext(ctx, `
SELECT s.id, s.name, s.bin, s.poster, COUNT(e.id)
FROM shows s
LEFT JOIN episodes e ON e.show_id = s.id
GROUP BY s.id
ORDER BY lower(s.name), s.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Show
	for rows.Next() {
		var sh Show
		if err := rows.Scan(&sh.ID, &sh.Name, &sh.Bin, &sh.Poster, &sh.Episodes); err != nil {
			return nil, err
		}
		out = append(out, sh)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := l.derivePosters(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// derivePosters sets each show's Poster to its first episode in natural
// title order that has a thumbnail (that episode's id), "" when none has:
// the shows.poster column is never written. ListShows and Search share it,
// so a show looks the same on the shelf and in search results.
func (l *Library) derivePosters(ctx context.Context, shows []Show) error {
	if len(shows) == 0 {
		return nil
	}
	byID := make(map[core.ShowID]int, len(shows))
	for i, sh := range shows {
		byID[sh.ID] = i
		shows[i].Poster = ""
	}
	epRows, err := l.s.DB().QueryContext(ctx,
		`SELECT show_id, id, title FROM episodes WHERE thumb != ''`)
	if err != nil {
		return err
	}
	defer epRows.Close()

	byShow := make(map[core.ShowID][]posterCand)
	for epRows.Next() {
		var (
			showID core.ShowID
			cand   posterCand
		)
		if err := epRows.Scan(&showID, &cand.id, &cand.title); err != nil {
			return err
		}
		if _, ok := byID[showID]; ok {
			byShow[showID] = append(byShow[showID], cand)
		}
	}
	if err := epRows.Err(); err != nil {
		return err
	}
	for showID, cands := range byShow {
		sort.Slice(cands, func(i, j int) bool { return naturalLess(cands[i].title, cands[j].title) })
		shows[byID[showID]].Poster = string(cands[0].id)
	}
	return nil
}

// ShowEpisodes returns the episodes of one show, ordered naturally by title
// (embedded season/episode numbers sort numerically: "Episode 2" before
// "Episode 10").
func (l *Library) ShowEpisodes(ctx context.Context, show core.ShowID) ([]Episode, error) {
	rows, err := l.s.DB().QueryContext(ctx, selectEpisode+` WHERE e.show_id = ?`, show)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Episode
	for rows.Next() {
		ep, err := scanEpisode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ep)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return naturalLess(out[i].Title, out[j].Title) })
	return out, nil
}

// Search matches a case-insensitive substring against show names and episode
// titles. An empty query returns no results.
func (l *Library) Search(ctx context.Context, text string) (SearchResults, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return SearchResults{}, nil
	}
	pat := likePattern(text)

	showRows, err := l.s.DB().QueryContext(ctx, `
SELECT s.id, s.name, s.bin, s.poster, COUNT(e.id)
FROM shows s
LEFT JOIN episodes e ON e.show_id = s.id
WHERE lower(s.name) LIKE ? ESCAPE '\'
GROUP BY s.id
ORDER BY lower(s.name), s.name`, pat)
	if err != nil {
		return SearchResults{}, err
	}
	defer showRows.Close()

	var out SearchResults
	for showRows.Next() {
		var sh Show
		if err := showRows.Scan(&sh.ID, &sh.Name, &sh.Bin, &sh.Poster, &sh.Episodes); err != nil {
			return SearchResults{}, err
		}
		out.Shows = append(out.Shows, sh)
	}
	if err := showRows.Err(); err != nil {
		return SearchResults{}, err
	}
	if err := l.derivePosters(ctx, out.Shows); err != nil {
		return SearchResults{}, err
	}

	epRows, err := l.s.DB().QueryContext(ctx,
		selectEpisode+` WHERE lower(e.title) LIKE ? ESCAPE '\'`, pat)
	if err != nil {
		return SearchResults{}, err
	}
	defer epRows.Close()

	for epRows.Next() {
		ep, err := scanEpisode(epRows)
		if err != nil {
			return SearchResults{}, err
		}
		out.Episodes = append(out.Episodes, ep)
	}
	if err := epRows.Err(); err != nil {
		return SearchResults{}, err
	}
	sort.Slice(out.Episodes, func(i, j int) bool {
		return naturalLess(out.Episodes[i].Title, out.Episodes[j].Title)
	})
	return out, nil
}

// likePattern wraps a search text into a LIKE pattern whose % and _ are
// escaped so they match literally.
func likePattern(text string) string {
	text = strings.ToLower(text)
	text = strings.ReplaceAll(text, `\`, `\\`)
	text = strings.ReplaceAll(text, "%", `\%`)
	text = strings.ReplaceAll(text, "_", `\_`)
	return "%" + text + "%"
}

// Episode returns the episode with the given id, or ErrNotFound.
func (l *Library) Episode(ctx context.Context, id core.EpisodeID) (Episode, error) {
	ep, err := scanEpisode(l.s.DB().QueryRowContext(ctx, selectEpisode+` WHERE e.id = ?`, id))
	if err == sql.ErrNoRows {
		return Episode{}, ErrNotFound
	}
	return ep, err
}

// BinRate returns the per-minute cost multiplier of the episode's show bin:
// 1.0 for fun, 0.5 for learning and 0 for free.
func (l *Library) BinRate(ctx context.Context, ep core.EpisodeID) (float64, error) {
	var bin string
	err := l.s.DB().QueryRowContext(ctx, `
SELECT s.bin
FROM episodes e
JOIN shows s ON s.id = e.show_id
WHERE e.id = ?`, ep).Scan(&bin)
	if err == sql.ErrNoRows {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return BinRateOf(bin), nil
}

// SetFixFailed marks an episode's fix state "failed" without republishing
// it: ingest calls it when a queued better copy could not be made, and the
// current file keeps playing. The issue stays as the show phase published
// it; an episode with no issue is left alone (there is nothing to explain)
// and ErrNotFound is returned either way.
func (l *Library) SetFixFailed(ctx context.Context, id core.EpisodeID) error {
	res, err := l.s.DB().ExecContext(ctx,
		`UPDATE episodes SET fix = 'failed' WHERE id = ? AND issue != ''`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// FixingListCap is how many failed episodes the parent dashboard's
// "Fixing videos" card lists; the rest are summed up as "and N more".
const FixingListCap = 10

// FailedEpisode is one episode whose better copy failed, as the parent
// dashboard lists it: the show and title a parent recognises, and the
// issue code the plain-words explanation is rendered from.
type FailedEpisode struct {
	Show  string
	Title string
	Issue string
}

// FixingSummary is the state behind the parent dashboard's "Fixing
// videos" card: how many episodes wait for a better copy per issue (fix
// pending), how many improvements failed, and the newest failed episodes.
type FixingSummary struct {
	PendingSound   int
	PendingPicture int
	PendingFormat  int
	Failed         int
	FailedEpisodes []FailedEpisode // newest first, at most FixingListCap
	FailedMore     int             // failed episodes beyond the list cap
}

// FixingSummary counts every episode whose fix is pending (grouped by its
// issue) or failed, and lists the newest failed ones, ordered by when the
// episode was added (shown), newest first. Nothing here touches the jobs
// table: the pending and failed flags are the episodes' own, written by
// ingest as the improvement runs. The counts are zero on an empty library
// (SUM over no rows is NULL).
func (l *Library) FixingSummary(ctx context.Context) (FixingSummary, error) {
	var sum FixingSummary
	err := l.s.DB().QueryRowContext(ctx, `
SELECT
	COALESCE(SUM(CASE WHEN fix = 'pending' AND issue = 'sound' THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN fix = 'pending' AND issue = 'picture' THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN fix = 'pending' AND issue = 'format' THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN fix = 'failed' THEN 1 ELSE 0 END), 0)
FROM episodes`).Scan(
		&sum.PendingSound, &sum.PendingPicture, &sum.PendingFormat, &sum.Failed)
	if err != nil {
		return FixingSummary{}, err
	}
	rows, err := l.s.DB().QueryContext(ctx, `
SELECT s.name, e.title, e.issue
FROM episodes e
JOIN shows s ON s.id = e.show_id
WHERE e.fix = 'failed'
ORDER BY e.added DESC, e.id DESC
LIMIT ?`, FixingListCap)
	if err != nil {
		return FixingSummary{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var ep FailedEpisode
		if err := rows.Scan(&ep.Show, &ep.Title, &ep.Issue); err != nil {
			return FixingSummary{}, err
		}
		sum.FailedEpisodes = append(sum.FailedEpisodes, ep)
	}
	if err := rows.Err(); err != nil {
		return FixingSummary{}, err
	}
	if sum.Failed > len(sum.FailedEpisodes) {
		sum.FailedMore = sum.Failed - len(sum.FailedEpisodes)
	}
	return sum, nil
}

// newID returns a random 128-bit id, hex-encoded.
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// timeLayout is the canonical UTC form the tables store. Unlike RFC3339Nano,
// which drops trailing fractional zeros ("12:00:00Z"), the nine fractional
// digits keep lexicographic and chronological order identical in SQL, so
// ORDER BY on the text sorts correctly at sub-second resolution.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

func parseTime(s string) (time.Time, error) {
	if t, err := time.Parse(timeLayout, s); err == nil {
		return t, nil
	}
	// Rows written before the fixed-width migration may still carry the
	// variable-width RFC3339Nano form; parse those too.
	return time.Parse(time.RFC3339Nano, s)
}

// naturalLess orders strings with embedded numbers numerically, ignoring
// ASCII case: "Episode 2" sorts before "Episode 10" and "S1E2" before
// "S1E12". Equal runs compare by original length so "01" precedes "1".
func naturalLess(a, b string) bool {
	lower := func(c byte) byte {
		if c >= 'A' && c <= 'Z' {
			return c + ('a' - 'A')
		}
		return c
	}
	la, lb := len(a), len(b)
	i, j := 0, 0
	for i < la && j < lb {
		da := a[i] >= '0' && a[i] <= '9'
		db := b[j] >= '0' && b[j] <= '9'
		if da && db {
			sa, sb := i, j
			for i < la && a[i] >= '0' && a[i] <= '9' {
				i++
			}
			for j < lb && b[j] >= '0' && b[j] <= '9' {
				j++
			}
			na, nb := strings.TrimLeft(a[sa:i], "0"), strings.TrimLeft(b[sb:j], "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			if i-sa != j-sb {
				return i-sa < j-sb
			}
			continue
		}
		if da != db {
			return da // digit runs sort before letters
		}
		x, y := lower(a[i]), lower(b[j])
		if x != y {
			return x < y
		}
		i, j = i+1, j+1
	}
	return la-i < lb-j
}

// Compile-time checks that the Library implements the core interfaces ingest
// and migrate call into.
var (
	_ core.Publisher        = (*Library)(nil)
	_ core.EpisodeLookup    = (*Library)(nil)
	_ core.PlaybackImporter = (*Library)(nil)
	_ core.SourceRegistry   = (*Library)(nil)
	_ core.FixRecorder      = (*Library)(nil)
)
