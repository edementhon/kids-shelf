// Following a playlist or a channel: Kids Shelf keeps checking it and
// brings in the videos that appear, so a family does not have to paste the
// same link again every week.
//
// The owner's rule (DECISIONS 2026-11-01) is that nothing reaches the kids
// until a parent lets it in. A pasted playlist or channel link therefore
// never downloads at once: the runner looks it up in the background and
// keeps a preview (name, number of videos, total length, the space it
// needs, how many are already in Kids Shelf) until the parent presses
// Start or Cancel. At Start the parent picks what to get now, an optional
// length limit and whether new videos go straight to the shelf (trusted)
// or wait for a yes (gated).
//
// Table ownership: this package owns youtube_follows and
// youtube_follow_videos (Migrations below). Downloads still go through the
// jobs queue (Kind "youtube"), deduplicated by video id against every
// youtube job, so a followed video never downloads twice.
package youtube

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edementhon/kids-shelf/internal/store"
)

// Migrations registers the tables this package owns. A followed source is
// one row of youtube_follows; every video Kids Shelf has seen in it is one
// row of youtube_follow_videos, so a video is offered at most once.
//
// youtube_follows.status: "lookup" (being looked up, or the lookup failed:
// check_error says why), "preview" (looked up, waiting for Start or Cancel),
// "following", or "once" (started without following: never checked, kept
// only so the page can say how its videos are doing; the web layer forgets
// it once none of them is on the way). checked is the time of the last
// listing (the lookup's for a fresh follow), the base of the 6-hour
// schedule; check_error is the last check's plain-words failure, "" after
// a good one.
//
// youtube_follow_videos.state: "listed" (in a preview, nothing decided),
// "added" (in Kids Shelf: downloaded, on its way, or there before),
// "waiting" (waits for the parent's yes), "skipped" (the parent said no),
// "too_long" (longer than the length limit) or "not_taken" (not chosen at
// Start). seconds is 0 when YouTube did not say; uploaded is YYYY-MM-DD or
// ""; pos is the video's place in the listing that first showed it.
var Migrations = []store.Migration{{
	Owner:   "youtube",
	Version: 1,
	SQL: `
CREATE TABLE youtube_follows (
	id          TEXT PRIMARY KEY,
	url         TEXT NOT NULL UNIQUE,
	kind        TEXT NOT NULL,
	name        TEXT NOT NULL DEFAULT '',
	status      TEXT NOT NULL,
	mode        TEXT NOT NULL DEFAULT 'ask',
	max_minutes INTEGER NOT NULL DEFAULT 0,
	created     TEXT NOT NULL,
	checked     TEXT NOT NULL DEFAULT '',
	check_error TEXT NOT NULL DEFAULT ''
);
CREATE TABLE youtube_follow_videos (
	follow_id  TEXT NOT NULL REFERENCES youtube_follows(id) ON DELETE CASCADE,
	video_id   TEXT NOT NULL,
	title      TEXT NOT NULL DEFAULT '',
	seconds    INTEGER NOT NULL DEFAULT 0,
	uploaded   TEXT NOT NULL DEFAULT '',
	state      TEXT NOT NULL,
	first_seen TEXT NOT NULL,
	pos        INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (follow_id, video_id)
);
CREATE INDEX youtube_follow_videos_state ON youtube_follow_videos (state);`,
}}

// Kinds of followed sources.
const (
	KindPlaylist = "playlist"
	KindChannel  = "channel"
)

// What happens to the new videos of a followed source.
const (
	ModeAuto = "auto" // straight to the shelf
	ModeAsk  = "ask"  // wait for the parent's yes
)

// What to get at Start.
const (
	TakeAll    = "all"
	TakeNewest = "newest" // channels only: the newest NewestCount videos
	TakeNone   = "none"
)

// NewestCount is how many videos "The newest" takes from a channel.
const NewestCount = 20

// LengthLimits are the length limits a parent can pick, in minutes; 0 is
// no limit.
var LengthLimits = []int{0, 10, 20, 30, 60}

const (
	statusLookup    = "lookup"
	statusPreview   = "preview"
	statusFollowing = "following"
	statusOnce      = "once"

	vidListed   = "listed"
	vidAdded    = "added"
	vidWaiting  = "waiting"
	vidSkipped  = "skipped"
	vidTooLong  = "too_long"
	vidNotTaken = "not_taken"
)

// Schedule: every followed source is checked every CheckEvery (the last
// check time is stored, so a restart does not check again early), and the
// first scheduled check after a start waits firstCheckDelay so the app
// comes up quietly. A channel's routine check lists only its newest
// channelCheckCap videos (channel listings are newest first; the full
// listing of a big channel takes half a minute or more).
const (
	CheckEvery      = 6 * time.Hour
	firstCheckDelay = time.Minute
	channelCheckCap = 100
)

// bytesPerSecond is the space one second of video is expected to take at
// the quality Kids Shelf downloads (selectionSort: H.264 at most 1080p
// with AAC audio, merged to mp4): 15 MB per minute. A real sample taken on
// 2026-11-05 with yt-dlp 2026.10.26 and exactly the download's arguments
// (-S vcodec:h264,res:1080,acodec:aac --merge-output-format mp4), three
// kids' science videos, all 1920x1080 H.264 with 128 kb/s AAC:
//
//	NASA Space Place hyf5JF_VxwM (animation)  126 s   9.1 MB   4.3 MB/min
//	Kids Academy sePqPIXMsAc (animation)      151 s  25.4 MB  10.1 MB/min
//	NASA Goddard NYnUjtWCqA0 (filmed footage) 208 s  51.2 MB  14.8 MB/min
//
// (10.6 MB a minute over all three). The figure is set at the live-action
// end on purpose: the preview should promise more space than a list
// needs, never less.
const bytesPerSecond = 250_000

// estimateBytes is the space a video of the given length is expected to
// take; 0 for an unknown length.
func estimateBytes(seconds int) int64 {
	return int64(seconds) * bytesPerSecond
}

// Errors the web layer turns into plain words.
var (
	ErrFollowNotFound = errors.New("youtube: no such playlist or channel")
	ErrFollowExists   = errors.New("youtube: this playlist or channel is followed already")
	ErrFollowChoice   = errors.New("youtube: that choice is not possible here")
	ErrNotAList       = errors.New("youtube: not a playlist or channel link")
)

// Follows keeps the followed playlists and channels: the previews of
// pasted links, the 6-hour checks and the parent's answers.
//
// One runner (Run) does every lookup and check, one at a time, so YouTube
// sees at most one listing from Kids Shelf at a time. mu serializes the
// writes: the runner's apply step after a listing and the parent's
// actions; the slow listing itself runs outside it.
type Follows struct {
	db  *sql.DB
	m   *Manager
	now func() time.Time
	// started is when this process began following; the first scheduled
	// check waits firstCheckDelay after it.
	started time.Time

	mu sync.Mutex

	wake      chan struct{}
	reqMu     sync.Mutex
	requested map[string]bool
	current   string
}

// NewFollows applies this package's migrations and returns the follows
// over s. Downloads go through m's queue.
func NewFollows(s *store.Store, m *Manager) (*Follows, error) {
	if err := s.Migrate(Migrations...); err != nil {
		return nil, err
	}
	f := &Follows{
		db: s.DB(), m: m, now: time.Now,
		wake: make(chan struct{}, 1), requested: map[string]bool{},
	}
	f.started = f.now()
	return f, nil
}

// poke wakes the runner without blocking.
func (f *Follows) poke() {
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// ListKind says whether link (an https YouTube URL) names a playlist or a
// channel; ErrNotAList for anything else (a single video, a bad host).
func ListKind(link string) (string, error) {
	u, err := url.Parse(link)
	if err != nil || !isYouTubeURL(u) {
		return "", ErrNotAList
	}
	host := strings.ToLower(u.Hostname())
	path := strings.Trim(u.Path, "/")
	switch {
	case host == "youtu.be", path == "", path == "watch", strings.HasPrefix(path, "shorts/"):
		return "", ErrNotAList
	case path == "playlist":
		if u.Query().Get("list") == "" {
			return "", ErrNotAList
		}
		return KindPlaylist, nil
	}
	return KindChannel, nil
}

// Lookup starts looking up a playlist or channel link in the background and
// returns its id. The same link pasted again returns the same id; a link
// that is followed already returns its id with ErrFollowExists.
func (f *Follows) Lookup(link string) (string, error) {
	kind, err := ListKind(link)
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var id, status string
	err = f.db.QueryRow(`SELECT id, status FROM youtube_follows WHERE url = ?`, link).Scan(&id, &status)
	switch {
	case err == nil:
		if status == statusFollowing {
			return id, ErrFollowExists
		}
		if status == statusOnce {
			// Added once before: this paste is a fresh look at it. Its
			// videos already in Kids Shelf count as "already there".
			if _, err := f.db.Exec(`DELETE FROM youtube_follows WHERE id = ?`, id); err != nil {
				return "", err
			}
			break
		}
		if status == statusLookup {
			// A lookup that failed is tried again.
			if _, err := f.db.Exec(`UPDATE youtube_follows SET check_error = '' WHERE id = ?`, id); err != nil {
				return "", err
			}
			f.poke()
		}
		return id, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", err
	}
	id, err = newFollowID()
	if err != nil {
		return "", err
	}
	if _, err := f.db.Exec(
		`INSERT INTO youtube_follows (id, url, kind, status, created) VALUES (?, ?, ?, ?, ?)`,
		id, link, kind, statusLookup, stamp(f.now()),
	); err != nil {
		return "", err
	}
	f.poke()
	return id, nil
}

// StartOptions are the parent's choices at Start.
type StartOptions struct {
	Take       string `json:"take"`
	MaxMinutes int    `json:"maxMinutes"`
	Follow     bool   `json:"follow"`
	Mode       string `json:"mode"`
}

// Start acts on a preview: it downloads what the parent took now (videos
// already in Kids Shelf are not downloaded again; too long ones are left
// out), remembers every other video as seen, and either keeps following
// the source or, when Follow is off, forgets it.
func (f *Follows) Start(id string, o StartOptions) error {
	if !validLimit(o.MaxMinutes) || (o.Follow && o.Mode != ModeAuto && o.Mode != ModeAsk) {
		return ErrFollowChoice
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fr, err := f.get(id)
	if err != nil {
		return err
	}
	if fr.status != statusPreview {
		return ErrFollowChoice
	}
	switch o.Take {
	case TakeAll, TakeNone:
	case TakeNewest:
		if fr.kind != KindChannel {
			return ErrFollowChoice
		}
	default:
		return ErrFollowChoice
	}
	vids, err := f.videos(id, vidListed)
	if err != nil {
		return err
	}
	have, err := f.m.queuedVideoIDs()
	if err != nil {
		return err
	}
	fr.maxMinutes = o.MaxMinutes
	states := make(map[string]string, len(vids))
	for i, v := range vids {
		taken := o.Take == TakeAll || (o.Take == TakeNewest && i < NewestCount)
		switch {
		case have[v.ID]:
			states[v.ID] = vidAdded
		case !taken:
			states[v.ID] = vidNotTaken
		case fr.tooLong(v.Seconds):
			states[v.ID] = vidTooLong
		default:
			if err := f.enqueue(fr, v); err != nil {
				return err
			}
			have[v.ID] = true
			states[v.ID] = vidAdded
		}
	}
	status, mode := statusFollowing, o.Mode
	if !o.Follow {
		// Added once: kept, unchecked, while its videos are on the way.
		status, mode = statusOnce, ModeAsk
	}
	return f.tx(func(tx *sql.Tx) error {
		for vid, st := range states {
			if _, err := tx.Exec(`UPDATE youtube_follow_videos SET state = ? WHERE follow_id = ? AND video_id = ?`, st, id, vid); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`UPDATE youtube_follows SET status = ?, mode = ?, max_minutes = ?, check_error = '' WHERE id = ?`,
			status, mode, o.MaxMinutes, id)
		return err
	})
}

// Cancel forgets a link that is being looked up or waits in its preview.
func (f *Follows) Cancel(id string) error {
	return f.remove(id, false)
}

// Stop stops following a source. Its videos already in Kids Shelf (on the
// shelf or on their way) stay: no episode and no download job is touched.
func (f *Follows) Stop(id string) error {
	return f.remove(id, true)
}

// Forget drops a source added once (not followed). The web layer calls it
// when none of its videos is on the way any more; its videos stay.
func (f *Follows) Forget(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, err := f.db.Exec(`DELETE FROM youtube_follows WHERE id = ? AND status = ?`, id, statusOnce)
	return err
}

func (f *Follows) remove(id string, following bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	fr, err := f.get(id)
	if err != nil {
		return err
	}
	if fr.status == statusOnce || (fr.status == statusFollowing) != following {
		return ErrFollowChoice
	}
	_, err = f.db.Exec(`DELETE FROM youtube_follows WHERE id = ?`, id)
	return err
}

// Settings changes how a followed source treats its new videos and its
// length limit. The limit applies at once: waiting videos longer than it
// stop waiting (too long), and videos that were too long but now fit are
// treated as new (added or waiting, by the mode).
func (f *Follows) Settings(id, mode string, maxMinutes int) error {
	if (mode != ModeAuto && mode != ModeAsk) || !validLimit(maxMinutes) {
		return ErrFollowChoice
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fr, err := f.get(id)
	if err != nil {
		return err
	}
	if fr.status != statusFollowing {
		return ErrFollowChoice
	}
	fr.mode, fr.maxMinutes = mode, maxMinutes
	if _, err := f.db.Exec(`UPDATE youtube_follows SET mode = ?, max_minutes = ? WHERE id = ?`, mode, maxMinutes, id); err != nil {
		return err
	}
	return f.applyLimit(fr)
}

// applyLimit moves waiting videos over fr's limit to too long and treats
// too long videos that fit now as new. Callers hold mu.
func (f *Follows) applyLimit(fr follow) error {
	waiting, err := f.videos(fr.id, vidWaiting)
	if err != nil {
		return err
	}
	long, err := f.videos(fr.id, vidTooLong)
	if err != nil {
		return err
	}
	states := map[string]string{}
	for _, v := range waiting {
		if fr.tooLong(v.Seconds) {
			states[v.ID] = vidTooLong
		}
	}
	var have map[string]bool
	for _, v := range long {
		if fr.tooLong(v.Seconds) {
			continue
		}
		if have == nil {
			if have, err = f.m.queuedVideoIDs(); err != nil {
				return err
			}
		}
		st, err := f.arrive(fr, v, have)
		if err != nil {
			return err
		}
		states[v.ID] = st
	}
	return f.setStates(fr.id, states)
}

// arrive decides what a video new to fr becomes: added when Kids Shelf has
// it already, too long over the limit, otherwise added and downloaded
// (straight to the shelf) or waiting (ask first). Callers hold mu.
func (f *Follows) arrive(fr follow, v Video, have map[string]bool) (string, error) {
	switch {
	case have[v.ID]:
		return vidAdded, nil
	case fr.tooLong(v.Seconds):
		return vidTooLong, nil
	case fr.mode == ModeAuto:
		if err := f.enqueue(fr, v); err != nil {
			return "", err
		}
		have[v.ID] = true
		return vidAdded, nil
	}
	return vidWaiting, nil
}

// CheckNow asks the runner to check a followed source (or to look up again
// a link whose lookup failed) as soon as it is free.
func (f *Follows) CheckNow(id string) error {
	f.mu.Lock()
	fr, err := f.get(id)
	if err == nil && (fr.status == statusPreview || fr.status == statusOnce) {
		err = ErrFollowChoice
	}
	if err == nil && fr.status == statusLookup {
		_, err = f.db.Exec(`UPDATE youtube_follows SET check_error = '' WHERE id = ?`, id)
	}
	f.mu.Unlock()
	if err != nil {
		return err
	}
	if fr.status == statusFollowing {
		f.reqMu.Lock()
		f.requested[id] = true
		f.reqMu.Unlock()
	}
	f.poke()
	return nil
}

// Answer is the parent's yes (add) or no (skip) for the videos of a
// followed source that wait: one video, or all of them when video is "".
// A yes downloads the video through the usual queue; a no is remembered,
// so the video is never offered again. It returns how many videos it
// answered.
func (f *Follows) Answer(id, video string, add bool) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fr, err := f.get(id)
	if err != nil {
		return 0, err
	}
	if fr.status != statusFollowing {
		return 0, ErrFollowChoice
	}
	waiting, err := f.videos(id, vidWaiting)
	if err != nil {
		return 0, err
	}
	var have map[string]bool
	if add {
		if have, err = f.m.queuedVideoIDs(); err != nil {
			return 0, err
		}
	}
	states := map[string]string{}
	for _, v := range waiting {
		if video != "" && v.ID != video {
			continue
		}
		if !add {
			states[v.ID] = vidSkipped
			continue
		}
		if !have[v.ID] {
			if err := f.enqueue(fr, v); err != nil {
				return 0, err
			}
			have[v.ID] = true
		}
		states[v.ID] = vidAdded
	}
	return len(states), f.setStates(id, states)
}

// Waiting counts the videos that wait for the parent's yes, over every
// followed source.
func (f *Follows) Waiting() (int, error) {
	var n int
	err := f.db.QueryRow(`
SELECT COUNT(*) FROM youtube_follow_videos v JOIN youtube_follows f ON f.id = v.follow_id
WHERE f.status = ? AND v.state = ?`, statusFollowing, vidWaiting).Scan(&n)
	return n, err
}

// ─── views ─────────────────────────────────────────────────────────────────

// FollowView is one pasted list as the content page shows it: a lookup in
// progress (or failed: Message), a preview, or a followed source with its
// counts and the videos that wait for a yes.
type FollowView struct {
	ID         string        `json:"id"`
	URL        string        `json:"url"`
	Kind       string        `json:"kind"`
	Name       string        `json:"name"`
	Status     string        `json:"status"`
	Mode       string        `json:"mode"`
	MaxMinutes int           `json:"maxMinutes"`
	Created    time.Time     `json:"created"`
	Checked    *time.Time    `json:"checked,omitempty"`
	Message    string        `json:"message,omitempty"`
	Checking   bool          `json:"checking,omitempty"`
	Counts     FollowCounts  `json:"counts"`
	Waiting    []Video       `json:"waiting,omitempty"`
	Preview    *FollowDigest `json:"preview,omitempty"`
	// AddedIDs are the videos taken into Kids Shelf (followed or added
	// once); the web layer says how each one is doing from its jobs.
	AddedIDs []string `json:"-"`
}

// FollowCounts says how a source's videos stand with the parent: taken
// into Kids Shelf, waiting for a yes, skipped, too long.
type FollowCounts struct {
	Added   int `json:"added"`
	Waiting int `json:"waiting"`
	Skipped int `json:"skipped"`
	TooLong int `json:"tooLong"`
}

// Video is one video of a followed source.
type Video struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Seconds  int    `json:"seconds"`
	Uploaded string `json:"uploaded,omitempty"`
}

// FollowDigest is the preview of a looked-up list: how many videos, their
// known total length (Unknown have no length), the estimated space for all
// of them, how many Kids Shelf has already, and for every choice of what to
// get now and length limit what Start would download.
type FollowDigest struct {
	Videos  int   `json:"videos"`
	Seconds int64 `json:"seconds"`
	Unknown int   `json:"unknown"`
	Bytes   int64 `json:"bytes"`
	Already int   `json:"already"`
	// Choices: take -> length limit in minutes -> what Start downloads.
	Choices map[string]map[int]FollowChoice `json:"choices"`
}

// FollowChoice is what one Start choice would download: Videos new to
// Kids Shelf, their estimated Bytes, TooLong left out by the limit,
// Unknown of them without a length (not in Bytes).
type FollowChoice struct {
	Videos  int   `json:"videos"`
	Bytes   int64 `json:"bytes"`
	TooLong int   `json:"tooLong"`
	Unknown int   `json:"unknown"`
}

// List returns every pasted list: lookups and previews first (oldest
// first), then followed sources by name.
func (f *Follows) List() ([]FollowView, error) {
	rows, err := f.db.Query(`
SELECT id, url, kind, name, status, mode, max_minutes, created, checked, check_error
FROM youtube_follows
ORDER BY CASE WHEN status IN ('following', 'once') THEN 1 ELSE 0 END, CASE WHEN status IN ('following', 'once') THEN lower(name) ELSE created END, rowid`)
	if err != nil {
		return nil, err
	}
	var list []follow
	for rows.Next() {
		fr, err := scanFollow(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, fr)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	f.reqMu.Lock()
	current := f.current
	requested := make(map[string]bool, len(f.requested))
	for k := range f.requested {
		requested[k] = true
	}
	f.reqMu.Unlock()
	var have map[string]bool
	out := make([]FollowView, 0, len(list))
	for _, fr := range list {
		v := FollowView{
			ID: fr.id, URL: fr.url, Kind: fr.kind, Name: fr.name, Status: fr.status,
			Mode: fr.mode, MaxMinutes: fr.maxMinutes, Created: fr.created, Message: fr.checkError,
			Checking: fr.id == current || requested[fr.id],
		}
		if !fr.checked.IsZero() {
			c := fr.checked
			v.Checked = &c
		}
		switch fr.status {
		case statusPreview:
			if have == nil {
				if have, err = f.m.queuedVideoIDs(); err != nil {
					return nil, err
				}
			}
			vids, err := f.videos(fr.id, vidListed)
			if err != nil {
				return nil, err
			}
			v.Preview = digest(fr.kind, vids, have)
		case statusFollowing, statusOnce:
			if v.Counts, err = f.counts(fr.id); err != nil {
				return nil, err
			}
			if v.Waiting, err = f.videos(fr.id, vidWaiting); err != nil {
				return nil, err
			}
			added, err := f.videos(fr.id, vidAdded)
			if err != nil {
				return nil, err
			}
			for _, a := range added {
				v.AddedIDs = append(v.AddedIDs, a.ID)
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// digest builds a preview from the listed videos (in listing order).
func digest(kind string, vids []Video, have map[string]bool) *FollowDigest {
	d := &FollowDigest{Choices: map[string]map[int]FollowChoice{}}
	for _, v := range vids {
		d.Videos++
		if v.Seconds > 0 {
			d.Seconds += int64(v.Seconds)
		} else {
			d.Unknown++
		}
		if have[v.ID] {
			d.Already++
		}
	}
	d.Bytes = d.Seconds * bytesPerSecond
	takes := []string{TakeAll, TakeNone}
	if kind == KindChannel {
		takes = append(takes, TakeNewest)
	}
	for _, take := range takes {
		byLimit := map[int]FollowChoice{}
		for _, limit := range LengthLimits {
			fr := follow{maxMinutes: limit}
			var c FollowChoice
			for i, v := range vids {
				taken := take == TakeAll || (take == TakeNewest && i < NewestCount)
				if !taken || have[v.ID] {
					continue
				}
				if fr.tooLong(v.Seconds) {
					c.TooLong++
					continue
				}
				c.Videos++
				if v.Seconds == 0 {
					c.Unknown++
				}
				c.Bytes += estimateBytes(v.Seconds)
			}
			byLimit[limit] = c
		}
		d.Choices[take] = byLimit
	}
	return d
}

func (f *Follows) counts(id string) (FollowCounts, error) {
	rows, err := f.db.Query(`SELECT state, COUNT(*) FROM youtube_follow_videos WHERE follow_id = ? GROUP BY state`, id)
	if err != nil {
		return FollowCounts{}, err
	}
	defer rows.Close()
	var c FollowCounts
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return c, err
		}
		switch st {
		case vidAdded:
			c.Added = n
		case vidWaiting:
			c.Waiting = n
		case vidSkipped:
			c.Skipped = n
		case vidTooLong:
			c.TooLong = n
		}
	}
	return c, rows.Err()
}

// ─── the runner ────────────────────────────────────────────────────────────

// Run does the lookups and the checks until ctx ends: a pasted link is
// looked up at once, a Check now runs as soon as the runner is free, and
// every followed source is checked when its last check is CheckEvery old
// (but not before firstCheckDelay after the start). One at a time.
func (f *Follows) Run(ctx context.Context) error {
	for {
		if err := f.step(ctx); err != nil {
			return err
		}
		t := time.NewTimer(f.nextWait())
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-f.wake:
			t.Stop()
		case <-t.C:
		}
	}
}

// step does every lookup and check that is due now, one at a time.
func (f *Follows) step(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		id, lookup, err := f.nextWork()
		if err != nil || id == "" {
			return err
		}
		f.reqMu.Lock()
		f.current = id
		delete(f.requested, id)
		f.reqMu.Unlock()
		if lookup {
			err = f.lookup(ctx, id)
		} else {
			err = f.check(ctx, id)
		}
		f.reqMu.Lock()
		f.current = ""
		f.reqMu.Unlock()
		if err != nil {
			return err
		}
	}
}

// nextWork picks the next thing to do: a requested check, then a pending
// lookup, then the most overdue scheduled check.
func (f *Follows) nextWork() (id string, lookup bool, err error) {
	f.reqMu.Lock()
	var asked []string
	for k := range f.requested {
		asked = append(asked, k)
	}
	f.reqMu.Unlock()
	for _, k := range asked {
		fr, err := f.get(k)
		if errors.Is(err, ErrFollowNotFound) || (err == nil && fr.status != statusFollowing) {
			f.reqMu.Lock()
			delete(f.requested, k)
			f.reqMu.Unlock()
			continue
		}
		if err != nil {
			return "", false, err
		}
		return k, false, nil
	}
	err = f.db.QueryRow(`SELECT id FROM youtube_follows WHERE status = ? AND check_error = '' ORDER BY created, rowid LIMIT 1`, statusLookup).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	now := f.now()
	if now.Before(f.started.Add(firstCheckDelay)) {
		return "", false, nil
	}
	list, err := f.following()
	if err != nil {
		return "", false, err
	}
	var best *follow
	for i := range list {
		fr := &list[i]
		if fr.checked.Add(CheckEvery).After(now) {
			continue
		}
		if best == nil || fr.checked.Before(best.checked) {
			best = fr
		}
	}
	if best == nil {
		return "", false, nil
	}
	return best.id, false, nil
}

// nextWait is how long the runner sleeps before the next scheduled check
// is due (a wake cuts it short).
func (f *Follows) nextWait() time.Duration {
	list, err := f.following()
	if err != nil {
		return time.Minute
	}
	now := f.now()
	wait := CheckEvery
	for _, fr := range list {
		due := fr.checked.Add(CheckEvery)
		if first := f.started.Add(firstCheckDelay); due.Before(first) {
			due = first
		}
		wait = min(wait, due.Sub(now))
	}
	return max(wait, time.Second)
}

// lookup lists a pasted link in full and turns it into a preview.
func (f *Follows) lookup(ctx context.Context, id string) error {
	fr, err := f.get(id)
	if errors.Is(err, ErrFollowNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	name, vids, lerr := f.m.listFollow(ctx, fr.url, fr.kind, 0)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if fr, err = f.get(id); errors.Is(err, ErrFollowNotFound) || (err == nil && fr.status != statusLookup) {
		return nil // canceled while it was looked up
	} else if err != nil {
		return err
	}
	if lerr != nil {
		f.m.logf("youtube: look up %s: %v", fr.url, lerr)
		_, err := f.db.Exec(`UPDATE youtube_follows SET check_error = ? WHERE id = ?`, f.m.listFailureMessage(fr.kind, lerr), id)
		return err
	}
	now := stamp(f.now())
	return f.tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM youtube_follow_videos WHERE follow_id = ?`, id); err != nil {
			return err
		}
		for i, v := range vids {
			if err := insertVideo(tx, id, v, vidListed, now, i); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`UPDATE youtube_follows SET name = ?, status = ?, checked = ?, check_error = '' WHERE id = ?`,
			firstNonEmpty(name, fr.url), statusPreview, now, id)
		return err
	})
}

// check lists a followed source and deals with the videos new to it. A
// failed listing keeps everything and records the plain-words reason; the
// next check tries again. A video that left the list stays on the shelf.
func (f *Follows) check(ctx context.Context, id string) error {
	fr, err := f.get(id)
	if errors.Is(err, ErrFollowNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	limit := 0
	if fr.kind == KindChannel {
		limit = channelCheckCap
	}
	name, vids, lerr := f.m.listFollow(ctx, fr.url, fr.kind, limit)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if fr, err = f.get(id); errors.Is(err, ErrFollowNotFound) || (err == nil && fr.status != statusFollowing) {
		return nil // stopped while it was checked
	} else if err != nil {
		return err
	}
	now := stamp(f.now())
	if lerr != nil {
		f.m.logf("youtube: check %s: %v", fr.url, lerr)
		_, err := f.db.Exec(`UPDATE youtube_follows SET checked = ?, check_error = ? WHERE id = ?`,
			now, f.m.listFailureMessage(fr.kind, lerr), id)
		return err
	}
	if name != "" {
		fr.name = name
	}
	known := map[string]bool{}
	rows, err := f.db.Query(`SELECT video_id FROM youtube_follow_videos WHERE follow_id = ?`, id)
	if err != nil {
		return err
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		known[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	have, err := f.m.queuedVideoIDs()
	if err != nil {
		return err
	}
	type arrival struct {
		v     Video
		state string
		pos   int
	}
	var fresh []arrival
	for i, v := range vids {
		if known[v.ID] {
			continue
		}
		st, err := f.arrive(fr, v, have)
		if err != nil {
			return err
		}
		known[v.ID] = true
		fresh = append(fresh, arrival{v, st, i})
	}
	if err := f.tx(func(tx *sql.Tx) error {
		for _, a := range fresh {
			if err := insertVideo(tx, id, a.v, a.state, now, a.pos); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`UPDATE youtube_follows SET name = ?, checked = ?, check_error = '' WHERE id = ?`, fr.name, now, id)
		return err
	}); err != nil {
		return err
	}
	// Videos that were too long and fit a limit raised since come back.
	return f.applyLimit(fr)
}

// ─── storage helpers ───────────────────────────────────────────────────────

// follow is one youtube_follows row.
type follow struct {
	id, url, kind, name, status, mode string
	maxMinutes                        int
	created, checked                  time.Time
	checkError                        string
}

// tooLong reports whether a video of the given length is over the limit;
// a video of unknown length never is.
func (fr follow) tooLong(seconds int) bool {
	return fr.maxMinutes > 0 && seconds > fr.maxMinutes*60
}

type scanner interface{ Scan(...any) error }

func scanFollow(s scanner) (follow, error) {
	var fr follow
	var created, checked string
	if err := s.Scan(&fr.id, &fr.url, &fr.kind, &fr.name, &fr.status, &fr.mode, &fr.maxMinutes, &created, &checked, &fr.checkError); err != nil {
		return fr, err
	}
	fr.created = parseStamp(created)
	fr.checked = parseStamp(checked)
	return fr, nil
}

const followCols = `id, url, kind, name, status, mode, max_minutes, created, checked, check_error`

func (f *Follows) get(id string) (follow, error) {
	fr, err := scanFollow(f.db.QueryRow(`SELECT `+followCols+` FROM youtube_follows WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return fr, ErrFollowNotFound
	}
	return fr, err
}

func (f *Follows) following() ([]follow, error) {
	rows, err := f.db.Query(`SELECT `+followCols+` FROM youtube_follows WHERE status = ?`, statusFollowing)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []follow
	for rows.Next() {
		fr, err := scanFollow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, fr)
	}
	return out, rows.Err()
}

// videos returns a source's videos in one state: listed ones in listing
// order, the others newest first (the latest check's first, then by place
// in that listing).
func (f *Follows) videos(id, state string) ([]Video, error) {
	order := `first_seen DESC, pos`
	if state == vidListed {
		order = `pos`
	}
	rows, err := f.db.Query(`SELECT video_id, title, seconds, uploaded FROM youtube_follow_videos
WHERE follow_id = ? AND state = ? ORDER BY `+order, id, state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Video
	for rows.Next() {
		var v Video
		if err := rows.Scan(&v.ID, &v.Title, &v.Seconds, &v.Uploaded); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func insertVideo(tx *sql.Tx, followID string, v Video, state, now string, pos int) error {
	_, err := tx.Exec(`INSERT INTO youtube_follow_videos (follow_id, video_id, title, seconds, uploaded, state, first_seen, pos)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (follow_id, video_id) DO NOTHING`,
		followID, v.ID, v.Title, v.Seconds, v.Uploaded, state, now, pos)
	return err
}

func (f *Follows) setStates(followID string, states map[string]string) error {
	if len(states) == 0 {
		return nil
	}
	return f.tx(func(tx *sql.Tx) error {
		for vid, st := range states {
			if _, err := tx.Exec(`UPDATE youtube_follow_videos SET state = ? WHERE follow_id = ? AND video_id = ?`, st, followID, vid); err != nil {
				return err
			}
		}
		return nil
	})
}

func (f *Follows) tx(fn func(*sql.Tx) error) error {
	tx, err := f.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// enqueue starts the download of one video of fr, in the show named after
// the playlist or the channel.
func (f *Follows) enqueue(fr follow, v Video) error {
	_, err := f.m.q.Enqueue(Kind, Payload{
		URL:     watchURL(v.ID),
		VideoID: v.ID,
		Title:   v.Title,
		Show:    fr.name,
	})
	return err
}

func validLimit(m int) bool {
	for _, l := range LengthLimits {
		if m == l {
			return true
		}
	}
	return false
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseStamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func newFollowID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("youtube: random id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ─── listing ───────────────────────────────────────────────────────────────

// listFollow lists the videos of a followed playlist (in full) or channel
// (its Videos tab only: no Shorts, no Live; newest first, the first limit
// of them when limit > 0). It returns the list's name: the playlist's
// title or the channel's name. A yt-dlp that could not start is fetched
// again once, as for downloads.
func (m *Manager) listFollow(ctx context.Context, link, kind string, limit int) (string, []Video, error) {
	target := link
	if kind == KindChannel {
		target = strings.TrimRight(link, "/") + "/videos"
	}
	var extra []string
	if limit > 0 {
		extra = []string{"--playlist-end", strconv.Itoa(limit)}
	}
	r, err := m.listJSON(ctx, target, extra)
	if isStartError(err) && ctx.Err() == nil && m.updater != nil {
		m.logf("youtube: list %s: %v", link, err)
		if _, uerr := m.updater.EnsureLatest(ctx, true); uerr != nil && ctx.Err() == nil {
			m.logf("youtube: forced yt-dlp update failed: %v", uerr)
		}
		r, err = m.listJSON(ctx, target, extra)
	}
	if err != nil {
		return "", nil, err
	}
	if r.Entries == nil {
		return "", nil, errors.New("yt-dlp: unsupported url: not a list")
	}
	name := r.Title
	if kind == KindChannel {
		name = firstNonEmpty(r.Channel, r.Uploader, strings.TrimSuffix(r.Title, " - Videos"))
	}
	seen := map[string]bool{}
	var out []Video
	for _, e := range *r.Entries {
		switch e.Type {
		case "url", "video", "":
		default:
			continue // a nested list: the Videos tab and playlists have none
		}
		id := e.ID
		if id == "" {
			id = videoIDFromURL(e.URL)
		}
		if id == "" || seen[id] || e.Title == "[Private video]" || e.Title == "[Deleted video]" {
			continue
		}
		seen[id] = true
		out = append(out, Video{
			ID:       id,
			Title:    firstNonEmpty(e.Title, id),
			Seconds:  int(e.Duration + 0.5),
			Uploaded: uploadDay(e.UploadDate, e.Timestamp),
		})
	}
	return name, out, nil
}

func (m *Manager) listJSON(ctx context.Context, target string, extra []string) (*ytResolve, error) {
	bin, err := m.binary()
	if err != nil {
		return nil, err
	}
	qjs, err := m.helpers.QuickJS()
	if err != nil {
		return nil, err
	}
	return m.resolveJSON(ctx, bin, qjs, target, extra...)
}

// uploadDay is the upload date as YYYY-MM-DD from yt-dlp's upload_date
// (YYYYMMDD) or its unix timestamp; "" when neither is known.
func uploadDay(date string, ts float64) string {
	if len(date) == 8 {
		if t, err := time.Parse("20060102", date); err == nil {
			return t.Format("2006-01-02")
		}
	}
	if ts > 0 {
		return time.Unix(int64(ts), 0).UTC().Format("2006-01-02")
	}
	return ""
}

// Plain-words reasons a lookup or a check failed. Kids Shelf keeps what it
// knows and tries again at the next check.
const (
	msgListBot     = "YouTube is blocking Kids Shelf on this computer right now. It thinks a robot is asking. Kids Shelf tries again later."
	msgListNetwork = "This computer couldn't reach YouTube. Kids Shelf tries again later."
	msgListMissing = "Kids Shelf's YouTube downloader (yt-dlp) didn't start on this computer. Kids Shelf tries again later."
)

// listFailureMessage is the parent-facing reason a listing failed.
func (m *Manager) listFailureMessage(kind string, err error) string {
	what := "playlist"
	if kind == KindChannel {
		what = "channel"
	}
	if isStartError(err) {
		return msgListMissing
	}
	text := strings.ToLower(err.Error())
	switch classifyYtFail(text) {
	case ytFailBot:
		return msgListBot
	case ytFailNetwork:
		return msgListNetwork
	case ytFailUnavailable:
		return "YouTube says this " + what + " isn't there. It may be private or removed."
	}
	if strings.Contains(text, "does not exist") || strings.Contains(text, "private") || strings.Contains(text, "404") {
		return "YouTube says this " + what + " isn't there. It may be private or removed."
	}
	return "Kids Shelf couldn't read this " + what + " from YouTube. It tries again later."
}
