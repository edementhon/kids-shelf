// Package youtube downloads videos from YouTube with the standalone yt-dlp
// binary and hands the finished files to ingest.
//
// Everything long-running is a persisted job of kind "youtube" in the jobs
// queue (internal/jobs owns that table; this package owns none). A job runs
// through the resolve, download and handoff stages; a quit, a kill or a
// power cut at any stage resumes or cleanly restarts the work at the next
// launch (jobs.Recover), and the handoff to ingest runs exactly once even
// when the process dies between the ingest enqueue and the job's completion
// marker.
//
// The yt-dlp binary is kept current by the updater (updater.go), which
// follows the same download-verify-swap discipline as the app updater: it
// verifies against the release checksums, self-tests, never deletes the
// working version, and leaves the current pointer untouched on any failure.
// A failed download runs the updater once and retries; only when that also
// fails does the job fail, with a plain-words message for parents.
package youtube

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edementhon/kids-shelf/internal/core"
	"github.com/edementhon/kids-shelf/internal/disk"
	"github.com/edementhon/kids-shelf/internal/jobs"
	"github.com/edementhon/kids-shelf/internal/proc"
)

// Kind is the jobs.Queue kind every youtube job uses.
const Kind = "youtube"

// Stage names a youtube job moves through. Expand marks a playlist or
// channel job done after it has spawned one child job per video.
const (
	stageResolve  = "resolve"
	stageDownload = "download"
	stageHandoff  = "handoff"
	stageExpand   = "expand"
)

// tmpRoot is where per-job download temp dirs live, under the data dir.
const tmpRoot = "ytdlp-tmp"

// Payload is the persisted state of one youtube job. The resolve stage (or,
// for the children a playlist expands into, the parent) fills in the video's
// identity; the download and handoff stages append progress, the Downloaded
// marker that lets a resumed job skip re-downloading, and the ingest job id
// that makes the handoff exactly-once.
type Payload struct {
	URL           string  `json:"url"`
	VideoID       string  `json:"videoId,omitempty"`
	Title         string  `json:"title,omitempty"`
	Uploader      string  `json:"uploader,omitempty"`
	PlaylistTitle string  `json:"playlistTitle,omitempty"`
	Size          int64   `json:"size,omitempty"`
	Progress      float64 `json:"progress,omitempty"`
	Downloaded    bool    `json:"downloaded,omitempty"`
	IngestID      string  `json:"ingestId,omitempty"`
}

// show returns the show name an ingested episode belongs to: the uploader,
// falling back to the playlist title, falling back to "YouTube".
func (p *Payload) show() string {
	return firstNonEmpty(p.Uploader, p.PlaylistTitle, "YouTube")
}

// videoInfo is one video identified by the resolve stage.
type videoInfo struct {
	ID            string
	Title         string
	Uploader      string
	PlaylistTitle string
	// Size is the video's expected size from the resolve JSON (0 when
	// unknown, which is the normal case for flat playlist entries).
	Size int64
}

// ytResolve mirrors the json -J --flat-playlist output of yt-dlp. A single
// video has no "entries" key; a playlist or channel has one, so a nil
// Entries pointer distinguishes the two shapes. Filesize fields appear on
// direct video resolves and feed the disk reservation.
type ytResolve struct {
	ID             string            `json:"id"`
	Title          string            `json:"title"`
	Uploader       string            `json:"uploader"`
	Channel        string            `json:"channel"`
	Filesize       int64             `json:"filesize"`
	FilesizeApprox int64             `json:"filesize_approx"`
	Entries        *[]ytResolveEntry `json:"entries"`
}

// ytFormat is one format entry of the resolved selection ("requested_formats"
// of a -J run). It exists to sum the sizes of the video and audio streams a
// download will fetch before reserving disk space for it.
type ytFormat struct {
	Filesize       int64 `json:"filesize"`
	FilesizeApprox int64 `json:"filesize_approx"`
}

// ytSizeInfo mirrors the selection fields of the -J output the download's
// size probe asks for: the resolved (sorted, selected) formats and, for
// single-format selections, the top-level filesize fallbacks.
type ytSizeInfo struct {
	Filesize       int64      `json:"filesize"`
	FilesizeApprox int64      `json:"filesize_approx"`
	Requested      []ytFormat `json:"requested_formats"`
}

// selectedBytes returns the total size in bytes of the download's resolved
// selection: the sum of the requested formats' sizes, preferring each
// format's exact size over its approximation, with the selection's
// top-level filesize fields as the fallback for selections yt-dlp resolves
// to a single format.
func (s *ytSizeInfo) selectedBytes() int64 {
	var total int64
	for _, f := range s.Requested {
		if f.Filesize > 0 {
			total += f.Filesize
			continue
		}
		total += f.FilesizeApprox
	}
	if total > 0 || len(s.Requested) == 0 {
		return total
	}
	return resolveSize(s)
}

// output a video entry carries _type "url" (or "video") and its id; a
// channel's tab playlists (Videos/Shorts/Live) appear as _type "playlist"
// entries whose own videos live in Entries or, when the listing was flat,
// behind the tab's URL.
type ytResolveEntry struct {
	ID       string            `json:"id"`
	Title    string            `json:"title"`
	Channel  string            `json:"channel"`
	Uploader string            `json:"uploader"`
	Type     string            `json:"_type"`
	IeKey    string            `json:"ie_key"`
	URL      string            `json:"url"`
	Entries  *[]ytResolveEntry `json:"entries"`
}

// Helpers resolves the locations of the binaries yt-dlp needs: yt-dlp
// itself, the QuickJS runtime it uses for YouTube's JavaScript challenges,
// and the directory holding ffmpeg/ffprobe. The indirection lets tests
// point the manager at fake binaries.
type Helpers interface {
	YtDlp() (string, error)
	QuickJS() (string, error)
	FFmpegDir() (string, error)
}

// PointerHelpers resolves the helper locations from the data directory. The
// yt-dlp binary comes from the updater's "current" pointer file and is
// re-read on every call, so an update between two download attempts is seen
// immediately. The QuickJS and ffmpeg paths are fixed by the process that
// provisions the helpers and passed in.
type PointerHelpers struct {
	DataDir        string
	QuickJSPath    string
	FFmpegLocation string
}

// YtDlp implements Helpers.
func (h *PointerHelpers) YtDlp() (string, error) {
	b, err := os.ReadFile(filepath.Join(h.DataDir, relHelperDir, pointerName))
	if err != nil {
		return "", fmt.Errorf("youtube: read yt-dlp pointer: %w", err)
	}
	path := strings.TrimSpace(string(b))
	if path == "" {
		return "", errors.New("youtube: yt-dlp pointer is empty")
	}
	return path, nil
}

// QuickJS implements Helpers.
func (h *PointerHelpers) QuickJS() (string, error) {
	if h.QuickJSPath == "" {
		return "", errors.New("youtube: no QuickJS path configured")
	}
	return h.QuickJSPath, nil
}

// FFmpegDir implements Helpers.
func (h *PointerHelpers) FFmpegDir() (string, error) {
	if h.FFmpegLocation == "" {
		return "", errors.New("youtube: no ffmpeg directory configured")
	}
	return h.FFmpegLocation, nil
}

// Manager runs the youtube job queue: it claims queued jobs, drives each
// through resolve, download and handoff, and keeps yt-dlp current.
type Manager struct {
	q       *jobs.Queue
	ingest  core.IngestQueue
	helpers Helpers
	updater *Updater
	dataDir string
	// resv accounts each job's worst-case download need against the free
	// space of the data directory before the download starts.
	resv *disk.Reservations

	// log, when set, receives non-fatal diagnostics (resolve and download
	// details that are not parent-facing).
	log func(format string, args ...any)
	// hook, when set, is called at stage boundaries; tests use it to
	// interrupt the pipeline exactly where a kill would land.
	hook func(phase string)
}

// NewManager returns a Manager over the queue, handing finished files to
// ingest and keeping yt-dlp current through updater. Per-job temp
// directories live under dataDir.
func NewManager(q *jobs.Queue, ingest core.IngestQueue, helpers Helpers, updater *Updater, dataDir string) *Manager {
	return &Manager{q: q, ingest: ingest, helpers: helpers, updater: updater, dataDir: dataDir, resv: disk.NewReservations(q)}
}

// WithReservations overrides the disk reservation manager; tests inject a
// fake free-space probe this way.
func (m *Manager) WithReservations(r *disk.Reservations) *Manager {
	if r != nil {
		m.resv = r
	}
	return m
}

func (m *Manager) logf(format string, args ...any) {
	if m.log != nil {
		m.log(format, args...)
	}
}

func (m *Manager) hookf(phase string) {
	if m.hook != nil {
		m.hook(phase)
	}
}

// Enqueue starts a youtube job for link: a single video downloads it, a
// playlist or channel URL expands into one download job per video. It
// returns the job's id in the queue. Only https URLs on YouTube's own
// hosts are accepted; anything else is refused up front with a plain
// error, so the dashboard can never point the downloader at an arbitrary
// address.
func (m *Manager) Enqueue(ctx context.Context, link string) (string, error) {
	u, err := url.Parse(link)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("youtube: not a usable web address: %q", link)
	}
	if !isYouTubeURL(u) {
		return "", fmt.Errorf("youtube: only YouTube links can be added: %q", link)
	}
	return m.q.Enqueue(Kind, Payload{URL: link})
}

// youtubeHosts is the set of hosts a YouTube download URL may name.
var youtubeHosts = map[string]bool{
	"youtube.com":       true,
	"www.youtube.com":   true,
	"m.youtube.com":     true,
	"music.youtube.com": true,
	"youtu.be":          true,
}

// isYouTubeURL reports whether u is an https URL naming one of YouTube's
// own hosts. The hostname match is exact (youtube.com.evil.example and
// user@evil.example tricks fall out), and only https is allowed.
func isYouTubeURL(u *url.URL) bool {
	if u.Scheme != "https" {
		return false
	}
	return youtubeHosts[strings.ToLower(u.Hostname())]
}

// JobView is one youtube job as the parent dashboard lists it: the video
// title (the pasted URL until the resolve stage names the video), the job's
// state and stage, the download progress in percent and the plain-words
// message for paused (disk full) and failed jobs (the jobs table's error
// column). JSON tags keep the dashboard API shape stable.
type JobView struct {
	ID       string    `json:"id"`
	URL      string    `json:"url"`
	Title    string    `json:"title"`
	State    string    `json:"state"`
	Stage    string    `json:"stage"`
	Progress float64   `json:"progress"`
	Message  string    `json:"message,omitempty"`
	Size     int64     `json:"size,omitempty"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
}

// Jobs lists every youtube job, most recently enqueued first. A job whose
// payload does not decode still lists with its id, state and error, so the
// dashboard never hides a stuck job.
func (m *Manager) Jobs() ([]JobView, error) {
	jobs, err := m.q.List(Kind)
	if err != nil {
		return nil, err
	}
	out := make([]JobView, 0, len(jobs))
	for i := range jobs {
		j := &jobs[i]
		var p Payload
		_ = j.DecodePayload(&p)
		title := p.Title
		if title == "" {
			title = p.URL
		}
		out = append(out, JobView{
			ID: j.ID, URL: p.URL, Title: title, State: j.State, Stage: j.Stage,
			Progress: p.Progress, Message: j.Error, Size: p.Size,
			Created: j.Created, Updated: j.Updated,
		})
	}
	return out, nil
}

// Run processes everything queued for kind "youtube" until the queue is
// empty or ctx is canceled. It runs the daily yt-dlp update check once and
// sweeps finished temp dirs. A job interrupted mid-stage stays running in
// the queue for jobs.Recover at the next launch.
func (m *Manager) Run(ctx context.Context) error {
	if _, err := m.updater.EnsureLatest(ctx, false); err != nil && ctx.Err() == nil {
		m.logf("youtube: daily yt-dlp check failed: %v", err)
	}
	m.cleanupTempDirs(ctx)
	for {
		if err := ctx.Err(); err != nil {
			return ctx.Err()
		}
		job, err := m.q.Claim(Kind)
		if err != nil {
			return err
		}
		if job == nil {
			return nil
		}
		if err := m.process(ctx, job); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
	}
}

// process drives one claimed job to completion. Job-level failures are
// recorded with q.Fail and return nil; the returned error is reserved for
// cancellation and infrastructure failures, which leave the job running.
func (m *Manager) process(ctx context.Context, job *jobs.Job) error {
	var p Payload
	if err := job.DecodePayload(&p); err != nil {
		return fmt.Errorf("youtube: decode payload of job %s: %w", job.ID, err)
	}
	if p.VideoID == "" {
		if err := m.q.Advance(job.ID, stageResolve, &p); err != nil {
			return err
		}
		single, entries, err := m.resolve(ctx, p.URL)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			m.logf("youtube: resolve job %s: %v", job.ID, err)
			return m.fail(job.ID, resolveFailureMessage(err))
		}
		if entries != nil {
			return m.expand(ctx, job, entries)
		}
		p.VideoID, p.Title, p.Uploader, p.Size = single.ID, single.Title, single.Uploader, single.Size
		if err := m.q.Advance(job.ID, stageResolve, &p); err != nil {
			return err
		}
	}
	if !p.Downloaded {
		// Resolve the exact size of the format selection the download
		// will fetch, so the reservation below covers the real need (the
		// merge's second copy included) instead of a flat guess. Videos
		// whose size stays unknown fall back to the default and are
		// monitored during the download itself.
		if err := m.resolveDownloadSize(ctx, &p); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		// Reserve the download's worst-case space before any byte is
		// written. A reservation denied for lack of free space pauses the
		// job instead of failing it: the parent frees space, the job
		// resumes where it left off.
		if err := m.reserve(job.ID, p.Size); err != nil {
			var pe *pauseError
			if errors.As(err, &pe) {
				m.logf("youtube: pause job %s: %s", job.ID, pe.reason)
				return m.q.Pause(job.ID, pe.reason)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if err := m.q.Advance(job.ID, stageDownload, &p); err != nil {
			return err
		}
		if err := m.runDownload(ctx, job.ID, &p); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			m.logf("youtube: download of job %s failed once: %v", job.ID, err)
			// A full disk is not a yt-dlp problem: an update would not
			// help, a retry would fail the same way. Pause with a
			// parent-readable message and keep the partial download's
			// temp dir so the job can continue once space is freed.
			if pe, ok := pauseReason(err); ok {
				return m.q.Pause(job.ID, pe.reason)
			}
			if downloadDiskFull(err) {
				return m.q.Pause(job.ID, msgDiskFull)
			}
			if _, uerr := m.updater.EnsureLatest(ctx, true); uerr != nil && ctx.Err() == nil {
				m.logf("youtube: forced yt-dlp update failed: %v", uerr)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err := m.runDownload(ctx, job.ID, &p); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				m.logf("youtube: download of job %s failed twice: %v", job.ID, err)
				if pe, ok := pauseReason(err); ok {
					return m.q.Pause(job.ID, pe.reason)
				}
				if downloadDiskFull(err) {
					return m.q.Pause(job.ID, msgDiskFull)
				}
				return m.fail(job.ID, downloadMessage(p, err))
			}
		}
		p.Downloaded = true
		// The Downloaded marker is persisted before the handoff so a
		// resumed job skips re-downloading the finished file.
		if err := m.q.Advance(job.ID, stageHandoff, &p); err != nil {
			return err
		}
		m.hookf("downloaded")
		if err := ctx.Err(); err != nil {
			return ctx.Err()
		}
	}
	if p.IngestID == "" {
		file, err := m.downloadedFile(job.ID, p.VideoID)
		if err != nil {
			m.logf("youtube: job %s: %v", job.ID, err)
			return m.fail(job.ID, "We downloaded the video but couldn't find the finished file. Please try again.")
		}
		src := core.Source{
			Kind:  core.SourceYouTube,
			Path:  watchURL(p.VideoID),
			Title: p.Title,
			Show:  p.show(),
		}
		id, err := m.ingest.Enqueue(ctx, file, src)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			m.logf("youtube: enqueue ingest for job %s: %v", job.ID, err)
			return m.fail(job.ID, "We couldn't pass the downloaded video to the library. Please try again.")
		}
		p.IngestID = id
		// The ingest source (kind youtube, canonical watch URL) is the
		// durable key for the handoff: if the process dies after this
		// point but before the id is persisted, the resumed run enqueues
		// the same source and the queue returns the same job id, so the
		// handoff stays exactly-once without extra machinery. The hook
		// fires between the two so a kill can be simulated there.
		m.hookf("ingest-enqueued")
		if err := ctx.Err(); err != nil {
			return ctx.Err()
		}
		// The finished file now belongs to ingest; the reservation that
		// covered the download (and the merge's second copy) is released.
		if err := m.resv.Release(job.ID); err != nil {
			m.logf("youtube: release reservation of job %s: %v", job.ID, err)
		}
		// Persist the ingest job id before Done: a kill between the
		// enqueue and Done must not re-enqueue on resume.
		if err := m.q.Advance(job.ID, stageHandoff, &p); err != nil {
			return err
		}
		m.hookf("enqueued")
		if err := ctx.Err(); err != nil {
			return ctx.Err()
		}
	}
	return m.q.Done(job.ID)
}

// fail marks a job failed with a message written for parents. It also
// releases the job's disk reservation: a failed job holds no space. (A
// paused disk-full job keeps its reservation and its temp dir, because a
// resume needs both.)
func (m *Manager) fail(jobID, msg string) error {
	if err := m.resv.Release(jobID); err != nil {
		m.logf("youtube: release reservation of failed job %s: %v", jobID, err)
	}
	return m.q.Fail(jobID, errors.New(msg))
}

// The download reservation covers the merge's worst case: yt-dlp keeps the
// separate video and audio streams while writing the merged file, so a
// known size is reserved at mergeReserveFactor. Flat playlist entries carry
// no size, so their jobs fall back to defaultReserveBytes.
const (
	defaultReserveBytes = 2 << 30 // 2 GiB, for downloads of unknown size
	mergeReserveFactor  = 2.2
)

// reserveNeed estimates the worst-case disk need of one video download.
func reserveNeed(size int64) uint64 {
	if size <= 0 {
		return defaultReserveBytes
	}
	return uint64(float64(size) * mergeReserveFactor)
}

// reserve accounts jobID's worst-case download need on the data directory
// filesystem. A reservation denied for lack of free space returns a
// pauseError so the caller pauses the job instead of failing it.
func (m *Manager) reserve(jobID string, size int64) error {
	if err := m.resv.Reserve(jobID, reserveNeed(size), m.dataDir); err != nil {
		var ns *disk.ErrNoSpace
		if errors.As(err, &ns) {
			return &pauseError{reason: msgDiskFull}
		}
		return err
	}
	return nil
}

// msgDiskFull is the parent-facing pause reason for a full disk: what
// happened and what to do about it.
const msgDiskFull = "The disk is full. Delete some videos or move your media to a bigger drive, then try again."

// pauseError pauses a job instead of failing it: the work is blocked on
// something outside the job (a full disk), not broken.
type pauseError struct{ reason string }

func (e *pauseError) Error() string { return e.reason }

// pauseReason unwraps a pauseError (a denied reservation extension: the
// disk is full mid-download), reporting whether err is one.
func pauseReason(err error) (*pauseError, bool) {
	var pe *pauseError
	if errors.As(err, &pe) {
		return pe, true
	}
	return nil, false
}

// downloadDiskFull reports whether a failed download means the disk filled
// up: either the error carries ENOSPC (a Go-side write) or yt-dlp/ffmpeg
// printed their strerror text, which is the usual case because exec errors
// do not wrap the syscall.
func downloadDiskFull(err error) bool {
	if disk.IsDiskFull(err) {
		return true
	}
	return err != nil && strings.Contains(err.Error(), "No space left on device")
}

// expand resolves a playlist or channel job: every video becomes one
// download job, deduplicated by video id across all existing youtube jobs
// (queued, running, paused, done or failed), so the same video in two
// playlists downloads once. The parent job is then marked done.
func (m *Manager) expand(ctx context.Context, job *jobs.Job, entries []videoInfo) error {
	if len(entries) == 0 {
		return m.fail(job.ID, "This playlist or channel is empty, so there is nothing to download.")
	}
	seen, err := m.queuedVideoIDs()
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.ID == "" || seen[e.ID] {
			continue
		}
		if _, err := m.q.Enqueue(Kind, Payload{
			URL:           watchURL(e.ID),
			VideoID:       e.ID,
			Title:         e.Title,
			Uploader:      e.Uploader,
			PlaylistTitle: e.PlaylistTitle,
		}); err != nil {
			return err
		}
		seen[e.ID] = true
	}
	if err := m.q.Advance(job.ID, stageExpand, nil); err != nil {
		return err
	}
	return m.q.Done(job.ID)
}

// queuedVideoIDs returns the video ids of every existing youtube job.
func (m *Manager) queuedVideoIDs() (map[string]bool, error) {
	list, err := m.q.List(Kind)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	for i := range list {
		var p Payload
		if err := list[i].DecodePayload(&p); err != nil {
			continue
		}
		if p.VideoID != "" {
			seen[p.VideoID] = true
		}
	}
	return seen, nil
}

// maxFlatDepth bounds how many extra -J calls a nested flattening (channel
// tabs inside tabs) may issue before it stops following a listing.
const maxFlatDepth = 3

// resolve asks yt-dlp what url is: either one video (single set, entries
// nil) or a playlist or channel (entries set, possibly empty). The
// --flat-playlist flag keeps the answer cheap: yt-dlp lists the videos
// without extracting format metadata for each. A channel's listing nests:
// its Videos/Shorts/Live tabs are playlist entries of their own, flattened
// here into a plain list of videos.
func (m *Manager) resolve(ctx context.Context, url string) (*videoInfo, []videoInfo, error) {
	bin, err := m.helpers.YtDlp()
	if err != nil {
		return nil, nil, err
	}
	qjs, err := m.helpers.QuickJS()
	if err != nil {
		return nil, nil, err
	}
	r, err := m.resolveJSON(ctx, bin, qjs, url)
	if err != nil {
		return nil, nil, err
	}
	if r.Entries != nil {
		entries := make([]videoInfo, 0)
		if err := m.flattenEntries(ctx, bin, qjs, *r.Entries, *r, 0, &entries); err != nil {
			return nil, nil, err
		}
		return nil, entries, nil
	}
	if r.ID == "" {
		return nil, nil, errors.New("yt-dlp: no video id in resolve output")
	}
	return &videoInfo{
		ID:       r.ID,
		Title:    firstNonEmpty(r.Title, r.ID),
		Uploader: firstNonEmpty(r.Uploader, r.Channel),
		Size:     resolveSize(r),
	}, nil, nil
}

// resolveJSON runs one yt-dlp flat listing of url and decodes the JSON. The
// QuickJS runtime is passed through --js-runtimes: current yt-dlp refuses to
// extract YouTube at all without a JavaScript runtime (its error makes the
// video look unavailable, with only a warning naming the missing runtime),
// and the same runtime the download stage uses must drive the resolve.
func (m *Manager) resolveJSON(ctx context.Context, bin, qjs, url string) (*ytResolve, error) {
	stdout, stderr, err := m.runOutput(ctx, bin,
		"--no-config", "-J", "--flat-playlist",
		"--js-runtimes", "quickjs:"+qjs,
		url)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("yt-dlp: %s", strings.TrimSpace(stderr))
	}
	var r ytResolve
	if err := json.Unmarshal([]byte(stdout), &r); err != nil {
		return nil, fmt.Errorf("yt-dlp: unreadable resolve output: %v", err)
	}
	return &r, nil
}

// flattenEntries walks one level of a flat listing and appends a videoInfo
// per video entry. Video entries (_type "url" or "video", or the legacy
// shape with no _type) contribute their id; playlist entries (a channel's
// tabs) are recursed into, either through their own Entries when the listing
// carried them or through another -J call on the tab's URL, bounded to
// maxFlatDepth nested calls. Entries whose _type or URL does not resolve to a
// video id are skipped, so no job is ever built from a channel or tab id.
// Uploader and playlist title fall back to the top-level listing.
func (m *Manager) flattenEntries(ctx context.Context, bin, qjs string, entries []ytResolveEntry, top ytResolve, depth int, out *[]videoInfo) error {
	for _, e := range entries {
		switch e.Type {
		case "url", "video", "":
			id := e.ID
			if id == "" {
				id = videoIDFromURL(e.URL)
			}
			if id == "" {
				continue
			}
			*out = append(*out, videoInfo{
				ID:            id,
				Title:         firstNonEmpty(e.Title, id),
				Uploader:      firstNonEmpty(e.Uploader, e.Channel, top.Uploader, top.Channel),
				PlaylistTitle: top.Title,
			})
		case "playlist", "channel", "multi_video":
			var nested []ytResolveEntry
			if e.Entries != nil {
				nested = *e.Entries
			}
			if len(nested) == 0 && depth < maxFlatDepth && e.URL != "" {
				tab, err := m.resolveJSON(ctx, bin, qjs, e.URL)
				if err != nil {
					return err
				}
				if tab.Entries != nil {
					nested = *tab.Entries
				}
			}
			if err := m.flattenEntries(ctx, bin, qjs, nested, top, depth+1, out); err != nil {
				return err
			}
		default:
			// An entry type we do not recognize is neither a video nor a
			// list we can flatten; ignore it.
		}
	}
	return nil
}

// sized resolves the filesize fields shared by the resolve listings and the
// download's size probe.
type sized interface {
	filesize() int64
	filesizeApprox() int64
}

func (r *ytResolve) filesize() int64        { return r.Filesize }
func (r *ytResolve) filesizeApprox() int64  { return r.FilesizeApprox }
func (s *ytSizeInfo) filesize() int64       { return s.Filesize }
func (s *ytSizeInfo) filesizeApprox() int64 { return s.FilesizeApprox }

// resolveSize prefers the video's exact size, falling back to the
// approximation yt-dlp reports.
func resolveSize(r sized) int64 {
	if r.filesize() > 0 {
		return r.filesize()
	}
	return r.filesizeApprox()
}

// selectionSort is the format selection both the size probe and the download
// apply: H.264 video at most 1080p with AAC audio.
const selectionSort = "vcodec:h264,res:1080,acodec:aac"

// resolveDownloadSize asks yt-dlp what the download's format selection will
// actually cost: one -J run with the same --js-runtimes and -S as the
// download, summed over the resolved selection's formats. The result is
// stored in p.Size (0 when yt-dlp cannot say, which falls back to the
// default reservation and keeps the download monitored).
func (m *Manager) resolveDownloadSize(ctx context.Context, p *Payload) error {
	if p.Size > 0 {
		return nil
	}
	bin, err := m.helpers.YtDlp()
	if err != nil {
		return err
	}
	qjs, err := m.helpers.QuickJS()
	if err != nil {
		return err
	}
	var info ytSizeInfo
	stdout, stderr, err := m.runOutput(ctx, bin,
		"--no-config", "-J", "--js-runtimes", "quickjs:"+qjs,
		"-S", selectionSort, p.URL)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("yt-dlp: %s", strings.TrimSpace(stderr))
	}
	if err := json.Unmarshal([]byte(stdout), &info); err != nil {
		return fmt.Errorf("yt-dlp: unreadable size probe output: %v", err)
	}
	p.Size = info.selectedBytes()
	return nil
}

// downloadArgs builds the yt-dlp command line for one video. The sort
// prefers H.264 video, at most 1080p, and AAC audio; the merge to mp4
// guarantees the run leaves a single finished "<id>.mp4" in tmp. A resumed
// download (partial files already in tmp) drops --no-part so yt-dlp can
// continue the .part file it left behind instead of restarting from zero.
func (m *Manager) downloadArgs(qjs, ffdir, tmp, url string, resume bool) []string {
	args := []string{
		"--no-config",
		"--js-runtimes", "quickjs:" + qjs,
		"--ffmpeg-location", ffdir,
		"-S", selectionSort,
		"--merge-output-format", "mp4",
		"--no-playlist",
		"--continue",
		// One progress update per line; without it yt-dlp separates updates
		// with \r and the line reader sees only the last one.
		"--newline",
	}
	if !resume {
		args = append(args, "--no-part")
	}
	args = append(args,
		"-o", filepath.Join(tmp, "%(id)s.%(ext)s"),
		url,
	)
	return args
}

// tempDirBytes returns the total size in bytes of every file in dir.
func tempDirBytes(dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			return 0, err
		}
		total += fi.Size()
	}
	return total, nil
}

// runDownload downloads one video into the job's temp directory, reporting
// the parsed progress percentage into the job payload as it goes. A fresh
// run wipes the directory first: anything in it is a leftover of an
// interrupted run and must not be mistaken for a finished download. A
// resumed job (a paused download, or a re-run after a kill) keeps its
// partial files so yt-dlp continues them. Downloads of unknown size are
// watched: when the temp files outgrow the reservation, the reservation is
// extended; if there is no space to extend into, yt-dlp is stopped and the
// job pauses with the disk-full message, keeping the partial download.
func (m *Manager) runDownload(ctx context.Context, jobID string, p *Payload) error {
	bin, err := m.helpers.YtDlp()
	if err != nil {
		return err
	}
	qjs, err := m.helpers.QuickJS()
	if err != nil {
		return err
	}
	ffdir, err := m.helpers.FFmpegDir()
	if err != nil {
		return err
	}
	tmp := m.tmpDir(jobID)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return fmt.Errorf("youtube: create temp dir: %w", err)
	}
	empty, err := dirEmpty(tmp)
	if err != nil {
		return fmt.Errorf("youtube: inspect temp dir: %w", err)
	}
	resume := !empty
	if !resume {
		if err := cleanDir(tmp); err != nil {
			return fmt.Errorf("youtube: clean temp dir: %w", err)
		}
	}
	// The reservation just taken (or already held by a paused job) is the
	// line an unknown-size download may not cross.
	initialReserve := int64(reserveNeed(p.Size))
	downloadCtx, stop := context.WithCancel(ctx)
	defer stop()
	// growFailure carries the extension error out of the line callback;
	// ytdlp joins the scanner goroutines before it returns, so a read
	// here happens after any send.
	growFailure := make(chan error, 1)
	last := int(p.Progress)
	err = m.ytdlp(downloadCtx, bin, m.downloadArgs(qjs, ffdir, tmp, p.URL, resume), func(line string) {
		// Unknown-size downloads are monitored against their
		// reservation: once the temp files exceed it, the reservation
		// is extended to the merged worst case. A failed extension
		// means the disk is full: stop yt-dlp and let the caller
		// pause the job (the partial files stay for the resume).
		if initialReserve > 0 {
			if size, err := tempDirBytes(tmp); err == nil && size > initialReserve {
				if err := m.growTempReservation(jobID, tmp); err != nil {
					select {
					case growFailure <- err:
					default:
					}
					stop()
					return
				}
			}
		}

		pct, ok := parseProgress(line)
		if !ok || int(pct) == last {
			return
		}
		last = int(pct)
		p.Progress = pct
		if err := m.q.Advance(jobID, stageDownload, p); err != nil {
			m.logf("youtube: record progress of job %s: %v", jobID, err)
		}
	})
	if err != nil {
		select {
		case growErr := <-growFailure:
			return growErr
		default:
		}
		return err
	}
	return nil
}

// growTempReservation raises jobID's reservation to cover the bytes already
// in tmp (times the merge factor), so an unknown-size download that grew
// past its default reservation keeps an accurate worst-case claim. When the
// extension is denied for lack of free space it returns a pauseError: the
// disk is full, the download must stop with its partial files intact.
func (m *Manager) growTempReservation(jobID, tmp string) error {
	size, err := tempDirBytes(tmp)
	if err != nil {
		return err
	}
	return m.reserve(jobID, size)
}

// ytdlp runs the yt-dlp binary with args, passing every output line to
// onLine. When the process exits non-zero, the returned error carries the
// last "ERROR:" line yt-dlp printed.
func (m *Manager) ytdlp(ctx context.Context, bin string, args []string, onLine func(string)) error {
	cmd := proc.CommandContext(ctx, bin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	lines := make(chan string, 64)
	var wg sync.WaitGroup
	wg.Add(2)
	scan := func(r io.Reader) {
		defer wg.Done()
		s := bufio.NewScanner(r)
		for s.Scan() {
			lines <- s.Text()
		}
		// s.Err() after a kill is expected; the process is gone.
	}
	go scan(stdout)
	go scan(stderr)
	go func() {
		wg.Wait()
		close(lines)
	}()
	lastErr := ""
	for line := range lines {
		if onLine != nil {
			onLine(line)
		}
		if strings.HasPrefix(line, "ERROR: ") {
			lastErr = line
		}
	}
	if err := cmd.Wait(); err != nil {
		if lastErr != "" {
			return fmt.Errorf("yt-dlp: %s (%v)", lastErr, err)
		}
		return err
	}
	return nil
}

// runOutput runs a short yt-dlp command and returns its stdout and stderr
// separately: resolve JSON comes on stdout, warnings on stderr.
func (m *Manager) runOutput(ctx context.Context, bin string, args ...string) (string, string, error) {
	cmd := proc.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// downloadedFile returns the finished video file in the job's temp dir.
// yt-dlp merges every selection to an mp4 named "<id>.mp4", so that is the
// expected artifact; a single remaining non-part file matching "<id>.*" (a
// selection that needed no merge) is the fallback. ".part" files are
// unfinished downloads and never candidates.
func (m *Manager) downloadedFile(jobID, videoID string) (string, error) {
	tmp := m.tmpDir(jobID)
	mp4 := filepath.Join(tmp, videoID+".mp4")
	if fi, err := os.Stat(mp4); err == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
		return mp4, nil
	}
	matches, err := filepath.Glob(filepath.Join(tmp, videoID+".*"))
	if err != nil {
		return "", err
	}
	finished := matches[:0]
	for _, cand := range matches {
		if !strings.HasSuffix(cand, ".part") {
			finished = append(finished, cand)
		}
	}
	if len(finished) == 1 {
		return finished[0], nil
	}
	return "", fmt.Errorf("no finished file for %s in %s (%d candidates)", videoID, tmp, len(matches))
}

// tmpDir is the per-job temp directory under the data dir.
func (m *Manager) tmpDir(jobID string) string {
	return filepath.Join(m.dataDir, tmpRoot, jobID)
}

// cleanupTempDirs removes temp dirs whose work is done: the job failed, the
// job finished without handing anything to ingest, or ingest has finished
// with the handed-over file. Jobs still queued, running or paused keep
// their dirs.
func (m *Manager) cleanupTempDirs(ctx context.Context) {
	root := filepath.Join(m.dataDir, tmpRoot)
	entries, err := os.ReadDir(root)
	if err != nil {
		return // no temp dirs yet
	}
	for _, e := range entries {
		jobID := e.Name()
		job, err := m.q.Get(jobID)
		if err != nil {
			if errors.Is(err, jobs.ErrNotFound) {
				_ = os.RemoveAll(filepath.Join(root, jobID))
			}
			continue
		}
		switch job.State {
		case jobs.StateDone, jobs.StateFailed:
			m.removeFinishedTemp(ctx, root, job)
		default:
			// queued, running or paused: keep
		}
	}
}

func (m *Manager) removeFinishedTemp(ctx context.Context, root string, job *jobs.Job) {
	dir := filepath.Join(root, job.ID)
	var p Payload
	if err := job.DecodePayload(&p); err != nil {
		return // unknown shape: keep the dir
	}
	if p.IngestID == "" {
		// Expand parents and skipped jobs have nothing ingest needs.
		_ = os.RemoveAll(dir)
		return
	}
	_, done, err := m.ingest.IngestStatus(ctx, p.IngestID)
	if err == nil && done {
		_ = os.RemoveAll(dir)
	}
}

var downloadProgress = regexp.MustCompile(`\[download\]\s*([0-9]+(?:\.[0-9]+)?)%`)

// parseProgress extracts the percentage from a yt-dlp "[download]" line.
func parseProgress(line string) (float64, bool) {
	m := downloadProgress.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	pct, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	return pct, true
}

// dirEmpty reports whether dir contains no entries ("." and ".." never
// appear in ReadDir output).
func dirEmpty(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

// cleanDir removes everything in dir; it is called before each fresh
// download so an interrupted run's partial files never survive into a new
// one. A resumed download keeps its partial files instead.
func cleanDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// watchURL is the canonical per-video URL used as the ingest source path.
func watchURL(videoID string) string {
	return "https://www.youtube.com/watch?v=" + videoID
}

// videoIDFromURL extracts the video id a watch URL or youtu.be link names,
// "" when the URL is not a recognizable video address.
func videoIDFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if v := u.Query().Get("v"); v != "" {
		return v
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) > 0 && segs[len(segs)-1] != "" {
		return segs[len(segs)-1]
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ytFailKind classifies what a yt-dlp failure actually was, so parent-facing
// messages can name the real cause instead of blaming the link.
type ytFailKind int

const (
	ytFailUnknown     ytFailKind = iota
	ytFailBot                    // YouTube refused this computer as a bot
	ytFailNetwork                // this computer could not reach YouTube
	ytFailUnavailable            // YouTube says the video itself is unavailable
	ytFailLink                   // the address cannot be read as a video
)

// ytFailPatterns match the lowercased yt-dlp error text, one list per kind.
// The lists are checked in kind order so a bot block (which may mention
// "not available") wins over the generic unavailable wording.
var ytFailPatterns = []struct {
	kind ytFailKind
	re   []string
}{
	{ytFailBot, []string{
		"not a bot",         // "Sign in to confirm you're not a bot"
		"detected as a bot", // "unusual traffic" / robot checks
		"unusual traffic",
		"link between youtube and this computer is blocked",
	}},
	{ytFailNetwork, []string{
		"unable to connect",
		"connection refused",
		"connection reset",
		"connection timed out",
		"timed out",
		"timeout",
		"name or service not known",
		"no such host",
		"could not resolve host",
		"temporary failure in name resolution",
		"getaddrinfo",
		"network is unreachable",
		"no route to host",
		"errno",
		"tls",
		"ssl",
		"read operation timed out",
		"response ended prematurely",
	}},
	{ytFailUnavailable, []string{
		"video unavailable",
		"video is unavailable",
		"is unavailable",
		"this video is not available",
		"video is not available",
		"not available in your country",
		"video is private",
		"private video",
		"private or removed",
		"removed by the uploader",
		"has been removed",
		"was removed",
		"sign in to confirm your age",
		"age-restricted",
		"live stream is not available",
	}},
	{ytFailLink, []string{
		"unsupported url",
		"no video id in resolve output",
		"could not extract url",
	}},
}

// classifyYtFail returns the kind of failure errText describes, falling back
// to ytFailUnknown. The input is the yt-dlp stderr or a wrapped form of it,
// never user input.
func classifyYtFail(errText string) ytFailKind {
	s := strings.ToLower(errText)
	for _, group := range ytFailPatterns {
		for _, pat := range group.re {
			if strings.Contains(s, pat) {
				return group.kind
			}
		}
	}
	return ytFailUnknown
}

// Parent-facing failure texts: no yt-dlp jargon, no exit codes, just what
// happened and what to do. Each names its own cause; only ytFailLink blames
// the address, and only the unresolvable fallback stays vague.
const (
	msgResolveBot         = "YouTube is blocking the downloader on this computer right now. It thinks a robot is downloading, so it stopped us. Please try again in a few hours."
	msgResolveNetwork     = "This computer couldn't reach YouTube. Please check the internet connection and try again."
	msgResolveUnavailable = "YouTube says this video isn't available. It may be private or removed, or it may not play in your country."
	msgResolveLink        = "That doesn't look like a YouTube video link we can read. Check the address and try again."
	msgResolveFallback    = "We couldn't read that link from YouTube. Please try again later."
)

// resolveFailureMessage is the parent-facing text for a failed resolve: it
// follows the cause yt-dlp reported, never guessing at the link.
func resolveFailureMessage(err error) string {
	switch classifyYtFail(err.Error()) {
	case ytFailBot:
		return msgResolveBot
	case ytFailNetwork:
		return msgResolveNetwork
	case ytFailUnavailable:
		return msgResolveUnavailable
	case ytFailLink:
		return msgResolveLink
	default:
		return msgResolveFallback
	}
}

// downloadMessage is the parent-facing failure text for a download that
// failed twice: the plain fallback for causes we cannot see, and the same
// named causes as the resolve message when yt-dlp said what went wrong.
func downloadMessage(p Payload, err error) string {
	name := firstNonEmpty(p.Title, p.URL)
	switch classifyYtFail(err.Error()) {
	case ytFailBot:
		return fmt.Sprintf("We couldn't download \"%s\" from YouTube. YouTube is blocking the downloader on this computer right now. It thinks a robot is downloading, so it stopped us. Please try again in a few hours.", name)
	case ytFailNetwork:
		return fmt.Sprintf("We couldn't download \"%s\" from YouTube. This computer couldn't reach YouTube. Please check the internet connection and try again.", name)
	case ytFailUnavailable:
		return fmt.Sprintf("We couldn't download \"%s\" from YouTube. YouTube says this video isn't available. It may be private or removed, or it may not play in your country.", name)
	default:
		return fmt.Sprintf("We couldn't download \"%s\" from YouTube. We tried twice and it didn't work. Please try again later.", name)
	}
}
