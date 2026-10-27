package library

import (
	"context"
	"database/sql"
	"time"

	"github.com/edementhon/kids-shelf/internal/core"
)

// Playback history lives here: per-kid resume positions, the watch log and
// the household-wide recent list, all keyed by episode id (never by the
// converted file's path). The runtime player calls SavePosition,
// KeepWatching, RecordWatch and Recent; migrate replays the legacy state
// through the core.PlaybackImporter methods below.

// KeepWatchingItem is one saved position above the "barely started" cutoff,
// joined with its episode and show for the shelf row.
type KeepWatchingItem struct {
	Kid         core.KidID
	Episode     core.EpisodeID
	Title       string
	Thumb       string
	Pos         float64
	DurationSec int64
	Updated     time.Time
	Show        core.ShowID
	ShowName    string
}

// WatchEntry is one watch-log row, as the legacy portal recorded it: the
// metered cost in minutes and whether the watch was free travel with the
// entry.
type WatchEntry struct {
	ID      int64
	Kid     core.KidID
	Episode core.EpisodeID
	At      time.Time
	Title   string
	Cost    float64
	Free    bool
}

// RecentItem is one household recent-list row, joined with its episode and
// show.
type RecentItem struct {
	Episode  core.EpisodeID
	At       time.Time
	Kid      core.KidID // the kid the entry was matched to, empty when unknown
	Title    string
	Thumb    string
	Show     core.ShowID
	ShowName string
}

// keepWatchingMin is the reserved cut-off copied from the legacy portal:
// positions at or below it mean the episode was barely started and are not
// shown as "keep watching".
const keepWatchingMin = 5.0

// recentMax is the household recent list's length cap, as the legacy portal
// kept it.
const recentMax = 12

// SavePosition records where a kid stopped an episode, replacing any earlier
// position. A position of 0 (or less) clears the saved position.
func (l *Library) SavePosition(ctx context.Context, kid core.KidID, ep core.EpisodeID, pos float64) error {
	return l.savePosition(ctx, kid, ep, pos, l.now())
}

// ImportResume implements core.PlaybackImporter. A position of 0 clears the
// saved position; updated is the legacy updated time.
func (l *Library) ImportResume(ctx context.Context, kid core.KidID, ep core.EpisodeID, posSec float64, updated time.Time) error {
	return l.savePosition(ctx, kid, ep, posSec, updated)
}

func (l *Library) savePosition(ctx context.Context, kid core.KidID, ep core.EpisodeID, pos float64, at time.Time) error {
	if pos <= 0 {
		_, err := l.s.DB().ExecContext(ctx,
			`DELETE FROM resume WHERE kid = ? AND episode = ?`, kid, ep)
		return err
	}
	_, err := l.s.DB().ExecContext(ctx, `
INSERT INTO resume (kid, episode, pos, updated)
VALUES (?, ?, ?, ?)
ON CONFLICT(kid, episode) DO UPDATE SET
	pos     = excluded.pos,
	updated = excluded.updated`,
		kid, ep, pos, formatTime(at))
	return err
}

// Resume returns the saved position of a kid+episode, or 0 when none is
// saved.
func (l *Library) Resume(ctx context.Context, kid core.KidID, ep core.EpisodeID) (float64, error) {
	var pos float64
	err := l.s.DB().QueryRowContext(ctx,
		`SELECT pos FROM resume WHERE kid = ? AND episode = ?`, kid, ep).Scan(&pos)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return pos, err
}

// KeepWatching returns a kid's saved positions above the 5-second "barely
// started" cutoff, most recently updated first, as the legacy portal listed
// them.
func (l *Library) KeepWatching(ctx context.Context, kid core.KidID) ([]KeepWatchingItem, error) {
	rows, err := l.s.DB().QueryContext(ctx, `
SELECT r.kid, r.episode, r.pos, r.updated, e.title, e.thumb, e.duration_sec, s.id, s.name
FROM resume r
JOIN episodes e ON e.id = r.episode
JOIN shows s ON s.id = e.show_id
WHERE r.kid = ? AND r.pos > ?
ORDER BY r.updated DESC`, kid, keepWatchingMin)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []KeepWatchingItem
	for rows.Next() {
		var it KeepWatchingItem
		var updated string
		if err := rows.Scan(&it.Kid, &it.Episode, &it.Pos, &updated, &it.Title,
			&it.Thumb, &it.DurationSec, &it.Show, &it.ShowName); err != nil {
			return nil, err
		}
		t, err := parseTime(updated)
		if err != nil {
			return nil, err
		}
		it.Updated = t
		out = append(out, it)
	}
	return out, rows.Err()
}

// RecordWatch appends a watch-log entry and moves the episode to the front of
// the household recent list, as the legacy portal's log_watch did.
func (l *Library) RecordWatch(ctx context.Context, kid core.KidID, ep core.EpisodeID, title string, cost float64, free bool) error {
	return l.recordWatch(ctx, kid, ep, l.now(), title, cost, free)
}

// ImportWatch implements core.PlaybackImporter: it appends one legacy watch-
// log entry exactly as recorded, without touching the recent list (migrate
// replays that separately through ImportHouseholdRecent). The (kid, episode,
// watched-at) identity is UNIQUE, so re-importing the same entry on a rerun
// is a no-op instead of a duplicate row.
func (l *Library) ImportWatch(ctx context.Context, kid core.KidID, ep core.EpisodeID, watchedAt time.Time, title string, cost float64, free bool) error {
	_, err := l.s.DB().ExecContext(ctx, `
INSERT INTO watch_log (kid, episode, at, title, cost, free)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(kid, episode, at) DO NOTHING`,
		kid, ep, formatTime(watchedAt), title, cost, boolInt(free))
	return err
}

// ImportHouseholdRecent implements core.PlaybackImporter: it adds the episode
// to the household-wide recent list at the given time, de-duplicating by
// episode and keeping the newest 12.
func (l *Library) ImportHouseholdRecent(ctx context.Context, ep core.EpisodeID, at time.Time, kid core.KidID) error {
	return l.touchRecent(ctx, ep, at, kid)
}

func (l *Library) recordWatch(ctx context.Context, kid core.KidID, ep core.EpisodeID, at time.Time, title string, cost float64, free bool) error {
	tx, err := l.s.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO watch_log (kid, episode, at, title, cost, free)
VALUES (?, ?, ?, ?, ?, ?)`,
		kid, ep, formatTime(at), title, cost, boolInt(free)); err != nil {
		return err
	}
	if err := l.touchRecentTx(ctx, tx, ep, at, kid); err != nil {
		return err
	}
	return tx.Commit()
}

func (l *Library) touchRecent(ctx context.Context, ep core.EpisodeID, at time.Time, kid core.KidID) error {
	tx, err := l.s.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := l.touchRecentTx(ctx, tx, ep, at, kid); err != nil {
		return err
	}
	return tx.Commit()
}

// touchRecentTx upserts the episode at the front of the recent list and
// prunes every row outside the newest recentMax entries.
func (l *Library) touchRecentTx(ctx context.Context, tx *sql.Tx, ep core.EpisodeID, at time.Time, kid core.KidID) error {
	if _, err := tx.ExecContext(ctx, `
INSERT INTO recent (episode, at, kid)
VALUES (?, ?, ?)
ON CONFLICT(episode) DO UPDATE SET
	at  = excluded.at,
	kid = excluded.kid`, ep, formatTime(at), kid); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
DELETE FROM recent WHERE episode NOT IN (
	SELECT episode FROM recent ORDER BY at DESC, episode LIMIT ?)`, recentMax)
	return err
}

// WatchLog returns the watch-log entries, most recent first, capped at the
// legacy portal's 500-entry log.
func (l *Library) WatchLog(ctx context.Context) ([]WatchEntry, error) {
	rows, err := l.s.DB().QueryContext(ctx, `
SELECT id, kid, episode, at, title, cost, free
FROM watch_log
ORDER BY at DESC, id DESC
LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []WatchEntry
	for rows.Next() {
		var w WatchEntry
		var at string
		var free int
		if err := rows.Scan(&w.ID, &w.Kid, &w.Episode, &at, &w.Title, &w.Cost, &free); err != nil {
			return nil, err
		}
		t, err := parseTime(at)
		if err != nil {
			return nil, err
		}
		w.At = t
		w.Free = free != 0
		out = append(out, w)
	}
	return out, rows.Err()
}

// Recent returns the household recent list: at most recentMax entries, newest
// first, one per episode.
func (l *Library) Recent(ctx context.Context) ([]RecentItem, error) {
	rows, err := l.s.DB().QueryContext(ctx, `
SELECT r.episode, r.at, r.kid, e.title, e.thumb, s.id, s.name
FROM recent r
JOIN episodes e ON e.id = r.episode
JOIN shows s ON s.id = e.show_id
ORDER BY r.at DESC, r.episode
LIMIT ?`, recentMax)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RecentItem
	for rows.Next() {
		var it RecentItem
		var at string
		if err := rows.Scan(&it.Episode, &at, &it.Kid, &it.Title, &it.Thumb, &it.Show, &it.ShowName); err != nil {
			return nil, err
		}
		t, err := parseTime(at)
		if err != nil {
			return nil, err
		}
		it.At = t
		out = append(out, it)
	}
	return out, rows.Err()
}

// RecentForKid returns one kid's recently watched episodes: at most
// recentMax, newest first, one per episode. It reads the kid's own watch log
// plus household recent rows attributed to the kid (legacy imports), not the
// household list alone, which keeps one row per episode stamped with its
// last watcher and so loses an episode from a kid's list once a sibling
// watches it. Stored times are fixed-width UTC, so MAX orders them.
func (l *Library) RecentForKid(ctx context.Context, kid core.KidID) ([]RecentItem, error) {
	rows, err := l.s.DB().QueryContext(ctx, `
SELECT x.episode, MAX(x.at) AS last, e.title, e.thumb, s.id, s.name
FROM (
	SELECT episode, at FROM watch_log WHERE kid = ?
	UNION ALL
	SELECT episode, at FROM recent WHERE kid = ?
) x
JOIN episodes e ON e.id = x.episode
JOIN shows s ON s.id = e.show_id
GROUP BY x.episode
ORDER BY last DESC, x.episode
LIMIT ?`, kid, kid, recentMax)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RecentItem
	for rows.Next() {
		it := RecentItem{Kid: kid}
		var at string
		if err := rows.Scan(&it.Episode, &at, &it.Title, &it.Thumb, &it.Show, &it.ShowName); err != nil {
			return nil, err
		}
		t, err := parseTime(at)
		if err != nil {
			return nil, err
		}
		it.At = t
		out = append(out, it)
	}
	return out, rows.Err()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
