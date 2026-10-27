// Package core defines the shared identifiers and the package interfaces of
// PLAN.md section 1a, with no implementation: ingest hands a verified episode
// to the library through Publisher, youtube and migrate hand files to ingest
// through IngestQueue, and practice credits counters through Crediter. Each
// real package implements these interfaces; unit tests use the in-memory
// fakes in fakes.go.
package core

import (
	"context"
	"path/filepath"
	"time"
)

// EpisodeID is the stable id assigned to an episode at ingest. Resume
// positions, the watch log, bins and migration all key on the id, never on
// the converted file's path.
type EpisodeID string

// KidID is the stable id of a kid profile, assigned when the kid is created
// (migrate imports the legacy kids through Kids). Resume positions, the
// watch log and practice data key on the id, never on the display name,
// which is not stable: FindKid resolves a legacy name exactly once.
type KidID string

// ShowID is the stable id of the show an episode belongs to, assigned when
// the show is first seen at ingest. Bins are assigned per show; migrate
// resolves the show of every legacy episode through BySource.
type ShowID string

// SourceKind is where an episode's content came from.
type SourceKind string

// Source origins.
const (
	SourceYouTube SourceKind = "youtube" // downloaded from YouTube
	SourceFolder  SourceKind = "folder"  // file from a local media folder
	SourceNAS     SourceKind = "nas"     // file from a NAS/Plex-style share
)

// Source is the provenance metadata attached to an episode at ingest.
//
// Path is the episode's identity within its kind: for SourceFolder and
// SourceNAS it is the original file's absolute path, cleaned and
// slash-separated (see CanonicalPath); for SourceYouTube it is the watch URL.
// Every producer (the scanner, a parent's import, the legacy migration) must
// build the same Path for the same original file, so BySource finds it.
type Source struct {
	Kind  SourceKind // youtube, folder or nas
	Path  string     // canonical original file path, or the YouTube watch URL
	Title string     // title as reported by the source
	Show  string     // show name
}

// CanonicalPath returns the Source.Path form of an original file path:
// absolute, cleaned, with forward slashes.
func CanonicalPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(filepath.Clean(abs)), nil
}

// SourceRegistry registers a media root for scanning. kind is SourceFolder
// or SourceNAS; a non-empty allowlist limits the shows (top-level
// directories) taken from that root. migrate registers the legacy roots
// through it.
type SourceRegistry interface {
	AddSource(ctx context.Context, path string, kind SourceKind, allowlist []string) error
}

// Episode is a published, playable video.
type Episode struct {
	ID          EpisodeID
	ShowID      string
	Title       string
	File        string // the playable file: converted (library) path, or the original path when Owned is false
	DurationSec int64
	Thumb       string
	Source      Source

	// Owned reports whether File is an app-owned file (a converted copy in
	// the media folder, or a YouTube download): true. An in-place episode
	// (a folder/NAS original that already plays) sets Owned false, and its
	// File is the original's absolute path, which the app must never
	// modify, move or delete and which may be temporarily unavailable when
	// the NAS or drive is down.
	Owned bool
}

// IngestQueue hands a media file to the ingest pipeline and returns the
// ingest job id; the caller later learns the episode the job produced
// through IngestStatus. migrate uses this to map each legacy show/file key
// to the stable id of the episode that ingested its file.
type IngestQueue interface {
	// Enqueue is idempotent per source: when a job for the same
	// (src.Kind, src.Path) is queued, running, paused or done, it returns
	// that job's id and enqueues nothing, so a producer that crashes after
	// enqueuing but before recording the id can safely enqueue again. Only a
	// failed job for that source is replaced by a new one.
	Enqueue(ctx context.Context, file string, src Source) (jobID string, err error)

	// IngestStatus reports how far an ingest job has run. done is false
	// while the job is still running. Once it has finished, done is true
	// and id is the published episode id — or the zero EpisodeID if the
	// job failed or produced no episode, which callers must report rather
	// than drop silently. err is non-nil only when the status itself
	// could not be determined.
	IngestStatus(ctx context.Context, jobID string) (id EpisodeID, done bool, err error)
}

// Publisher receives a verified episode from ingest and files it in the
// library, indexed by its stable id.
type Publisher interface {
	Publish(ctx context.Context, ep Episode) error
}

// Crediter gives a kid credit on a counter (parents can point practice
// earnings at any counter; "video minutes" is the default). Credit is
// idempotent by awardID: crediting the same award twice counts once, which is
// what makes a retried practice session safe.
type Crediter interface {
	Credit(ctx context.Context, kid string, counter string, minutes float64, awardID string) error
}

// Kids owns the kid profiles, the per-kid PINs and the grown-up
// watch-together PIN. migrate imports the legacy portal's hardcoded kids
// and PIN through it once, before any playback data.
type Kids interface {
	// CreateKid registers a kid display name with its unlock PIN and
	// returns the kid's stable id. Creating a name that already exists is
	// idempotent: it returns the existing id without error, so a re-run of
	// migrate is safe.
	CreateKid(ctx context.Context, name string, pin string) (id KidID, err error)
	// FindKid reports whether a kid with this display name is registered
	// and, if so, its stable id.
	FindKid(ctx context.Context, name string) (id KidID, found bool, err error)
	// SetGrownupPIN sets the watch-together PIN: playback with it is free
	// and unmetered, and it stays separate from the dashboard PIN.
	SetGrownupPIN(ctx context.Context, pin string) error
}

// BudgetImporter imports a legacy portal account exactly as stored: the
// balance on today's ticket, the saved balance and the day they were last
// rolled over. The day is part of the state: the budget resumes its
// day-boundary rollover from it, so an account imported late in a day
// keeps the legacy session's remaining allowance instead of being reset.
type BudgetImporter interface {
	ImportAccount(ctx context.Context, kid KidID, today float64, saved float64, day string) error
}

// EpisodeRef pairs the episode a legacy source resolved to with the show it
// belongs to. migrate needs both: the episode id keys resume positions, the
// watch log and the recent list while the show id assigns the show to a bin.
type EpisodeRef struct {
	Episode EpisodeID
	Show    ShowID
}

// EpisodeLookup maps legacy show/file keys to the episodes that ingested
// their files, and assigns legacy shows to bins. migrate translates every
// portal key through BySource before importing resume positions, the watch
// log and the recent list; keys no episode was ingested from are reported,
// not dropped silently.
type EpisodeLookup interface {
	// BySource resolves a source to the episode ingested from it and the
	// show that episode belongs to, which migrate needs to assign bins. src
	// must carry the canonical Path (see Source); migrate turns each legacy
	// show/file key into the original file's canonical path under its root
	// before looking it up.
	BySource(ctx context.Context, src Source) (ref EpisodeRef, found bool, err error)
	// SetBin files a show into a bin: "fun", "learning" or "free".
	// migrate maps the legacy science discount to "learning", full-cost
	// shows to "fun" and free shows to "free".
	SetBin(ctx context.Context, show ShowID, bin string) error
	// SetShowBin files the show with this name into a bin, whether or not
	// the show exists yet: the choice is stored and applied when a show of
	// that name is first published. migrate uses it for every configured
	// science/full-cost show, watched or not.
	SetShowBin(ctx context.Context, showName string, bin string) error
}

// PlaybackImporter imports playback history from the legacy portal: resume
// positions, the watch log and the household-wide recent list, all keyed by
// episode id.
type PlaybackImporter interface {
	// ImportResume records where a kid stopped an episode; a position of 0
	// clears the saved position. updated is the legacy updated time.
	ImportResume(ctx context.Context, kid KidID, ep EpisodeID, posSec float64, updated time.Time) error
	// ImportWatch appends one watch-log entry as the legacy log recorded
	// it: the metered cost in minutes and whether the watch was free
	// (grown-up watch-together playback or a free show) travel with the
	// entry.
	ImportWatch(ctx context.Context, kid KidID, ep EpisodeID, watchedAt time.Time, title string, cost float64, free bool) error
	// ImportHouseholdRecent adds the episode to the household-wide recent
	// list, most recent first, as the legacy portal kept it (its recent
	// list is not per kid). kid is the kid the entry was matched to, empty
	// when unknown.
	ImportHouseholdRecent(ctx context.Context, ep EpisodeID, at time.Time, kid KidID) error
}

// PracticeImporter imports the legacy mathfacts state. Every method takes
// small typed structs mirroring the legacy state.json shapes (PLAN.md
// section 1a "Migration keys"), so migrate never moves raw JSON maps
// around. The structs live beside the interface so the practice package can
// implement it without inventing its own shapes.
type PracticeImporter interface {
	// ImportFactStats imports one fact's cumulative stats for a kid and
	// mode. key is the legacy storage key as written by the mathfacts app
	// (e.g. "3x4", "2+9").
	ImportFactStats(ctx context.Context, kid KidID, mode string, key string, s FactStats) error
	// ImportSessions appends one finished practice round or check-up.
	ImportSessions(ctx context.Context, kid KidID, s Session) error
	// ImportAwards imports a kid's math daily awards: minutes earned
	// toward the daily cap and the coin jar, with the day they were last
	// reset on.
	ImportAwards(ctx context.Context, kid KidID, a Awards) error
	// ImportFlags imports one legacy flag: promo/bonus markers and
	// best-record times.
	ImportFlags(ctx context.Context, kid KidID, f Flag) error
	// ImportAwardLog appends one legacy game-minute award.
	ImportAwardLog(ctx context.Context, e AwardLogEntry) error
	// ImportSpelling imports a kid's spelling state: per-word records,
	// answer sessions and the spelling coin jar (the legacy spell_awards).
	ImportSpelling(ctx context.Context, kid KidID, s Spelling) error
	// ImportBosses imports a kid's boss progress.
	ImportBosses(ctx context.Context, kid KidID, b BossState) error
	// ImportConfig imports the legacy config: global defaults plus per-kid
	// overrides.
	ImportConfig(ctx context.Context, c Config) error
}

// FactStats is the legacy per-fact state: how often the fact was asked and
// answered correctly, the response-time window for correct answers (newest
// last; the legacy app kept 12) and the median and best response times in
// milliseconds.
type FactStats struct {
	Asked    int
	Correct  int
	Times    []int    // correct-answer response times in ms, newest last
	MedianMS *float64 // median of Times in ms (the legacy median averages the middle two); nil when there are no correct answers yet
	BestMS   *float64 // fastest correct response in ms; nil when there is none
}

// Session is one finished legacy practice round or check-up.
type Session struct {
	At       time.Time
	Mode     string   // add, sub, mul or div
	Checkup  bool     // true for check-up probes
	N        int      // questions asked
	Correct  int      // questions answered correctly
	MedianMS *float64 // median response time of correct answers in ms; nil when none
}

// Awards is a kid's legacy per-day award totals and coin jar: one for the
// math bucket (PracticeImporter.ImportAwards) and one for spelling
// (Spelling.Awards). Day is the ET date (YYYY-MM-DD) the totals were last
// reset on, so practice resumes the daily cap exactly where the legacy app
// left it.
type Awards struct {
	Day    string
	Earned float64 // minutes earned toward the daily cap
	Coins  int     // unbanked coins in the jar
}

// Flag is one legacy mathfacts state flag. Keys embed the legacy kid name:
// "promo_bonus:<kid>" (bool: whether the once-ever promo bonus was paid)
// and "best:<kid>:<mode>" (float: best round median response time in ms, the
// round's median — fractional when the round had an even number of correct
// answers).
type Flag struct {
	Key   string
	Value any // bool for promo flags, float64 (ms) for best-record flags
}

// AwardLogEntry is one legacy game-minute award, as the mathfacts app
// logged it: kind is "spell", "jar" or "promo" and amount the minutes paid
// into the portal's game bucket. Kid keeps the legacy name the log was
// written under.
type AwardLogEntry struct {
	At     time.Time
	Kid    string
	Kind   string
	Amount float64
}

// SpellingRecord is one legacy spelling word's state.
type SpellingRecord struct {
	Word   string
	Hits   int
	Misses int
	BestMS int // fastest correct response in ms; 0 when there is none
}

// SpellingSession is one legacy spelling answer, as graded by the server.
type SpellingSession struct {
	At      time.Time
	Word    string
	Correct bool
	Ms      int // response time in ms
}

// Spelling is one kid's legacy spelling state: per-word records, answer
// sessions and the spelling coin jar (the legacy "spell" and "spell_awards"
// keys).
type Spelling struct {
	Records  []SpellingRecord
	Sessions []SpellingSession
	Awards   Awards // spell_awards: per-day totals and the spelling jar
}

// BossState is one kid's legacy boss progress. Defeated lists the boss ids
// beaten (once each); HP and PHP track the current fight's remaining boss
// hearts and, for lethal fights, the player's hearts. PendingBid is the
// fight a round was opened for, nil when no fight is in progress.
type BossState struct {
	Defeated   []string
	HP         map[string]int // boss id -> hearts remaining
	PHP        map[string]int // boss id -> player hearts remaining (lethal fights only)
	PendingBid *string        // id of the fight the last round was opened for
}

// Config is the legacy mathfacts configuration: global defaults plus
// per-kid overrides, as the parent admin wrote them. Values are numeric
// settings (minute caps, response-time bars in ms, fractions).
type Config struct {
	Global map[string]float64
	Kids   map[string]map[string]float64
}
