// Package ingest implements core.IngestQueue over the persisted job queue.
// A media file is probed and planned, and folder/NAS originals then run the
// show-first pipeline: the episode is published playing its original file
// right away (probe -> plan -> show), and the conversion that normalizes it
// to an H.264 8-bit + AAC faststart MP4 runs afterwards at lower priority
// (convert -> verify -> thumbnail -> publish) and silently replaces the file
// the episode plays. YouTube downloads keep the old single pass. Each stage
// persists its outputs in the job's payload, so a quit, a kill or a power
// cut at any stage resumes exactly there after jobs.Recover. The original
// file is never modified or moved: convert writes a temp file next to the
// destination and renames it into place only when it succeeded, and a
// show-only job writes nothing at all.
package ingest

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edementhon/kids-shelf/internal/core"
	"github.com/edementhon/kids-shelf/internal/disk"
	"github.com/edementhon/kids-shelf/internal/jobs"
	"github.com/edementhon/kids-shelf/internal/proc"
	"github.com/edementhon/kids-shelf/internal/tools"
)

// JobKind is the jobs-queue kind ingest jobs run under.
const JobKind = "ingest"

// Pipeline stages, in order. The jobs table's stage column holds the stage
// that is running or next to run; processOne advances to the next stage's
// name only after the current one finished. The show stage ends the show
// phase for a folder/NAS job: it publishes the episode playing its original
// and, when a conversion is wanted, requeues the job at convert with an
// improvement priority; the remaining stages are the improvement.
const (
	StageProbe     = "probe"
	StagePlan      = "plan"
	StageShow      = "show"
	StageConvert   = "convert"
	StageVerify    = "verify"
	StageThumbnail = "thumbnail"
	StagePublish   = "publish"
)

// stages lists the pipeline in order; tests kill jobs at each one.
var stages = []string{StageProbe, StagePlan, StageShow, StageConvert, StageVerify, StageThumbnail, StagePublish}

// Claim ordering on the jobs table. Show-phase work (probe, plan, show)
// claims at prioShow, so a newly added file is published before any
// conversion of an already-visible episode. Improvements claim on the fix
// lane by fixPriority: the cheapest fix first (remux, a byte copy, before
// audio re-encodes before full re-encodes) and, within a class, the
// shortest video first.
const (
	prioShow = 0

	prioClassRemux = 1
	prioClassAudio = 2
	prioClassFull  = 3
	// prioClassBase scales a class into the priority itself:
	// class*1_000_000 + min(duration_sec, 999999), so every remux claims
	// before every audio before every full, and durations sort within a
	// class without ever reaching the next class.
	prioClassBase = 1_000_000
	// prioDurCap keeps a very long video from overflowing into the next
	// class: the longest possible improvement priority is 3,999,999.
	prioDurCap = 999_999
)

// fixPriority is the fix-lane claim order of an improvement: the cheaper
// the fix and the shorter the video, the sooner it claims. New jobs are
// requeued with this, and jobs migration v6 rewrites queued pre-change
// rows to the same values, so the whole fix lane runs in one order.
func fixPriority(action Action, durationSec int64) int {
	class := map[Action]int{
		ActionRemux: prioClassRemux,
		ActionAudio: prioClassAudio,
		ActionFull:  prioClassFull,
	}[action]
	if class == 0 {
		class = prioClassRemux // unknown plans fall back to the cheapest fix
	}
	d := durationSec
	if d < 0 {
		d = 0
	}
	if d > prioDurCap {
		d = prioDurCap
	}
	return class*prioClassBase + int(d)
}

// Words a parent sees in the dashboard when a job cannot finish. Every job
// failure ends in one of these; internal errors are never shown raw.
const (
	msgDamaged = "This file is damaged or incomplete and can't be played."
	msgNoVideo = "This file doesn't contain a video and can't be played."
	msgConvert = "This video couldn't be converted. Try another copy of the file."
	// msgKeepAsIs is what a failed improvement says: the episode is
	// already on the shelf playing its original bytes, so the failure only
	// means no better copy was made.
	msgKeepAsIs = "This video plays as it is, but kids-shelf couldn't make a better copy."
	msgDiskFull = "The disk is full. Delete some videos or move your media to a bigger drive, then try again."
	msgGeneric  = "Something went wrong while adding this video. Try again."
	// msgNoHelpers matches the problem app.Open shows on the parent page
	// when it could not set up the helper programs.
	msgNoHelpers = "Video conversion is unavailable: the helper programs could not be set up."
)

// payload is the per-job state persisted in the jobs table, grown stage by
// stage, so a resumed job picks up exactly where it stopped.
type payload struct {
	File        string         `json:"file"`
	Source      core.Source    `json:"source"`
	EpisodeID   core.EpisodeID `json:"episode_id"`
	DurationSec int64          `json:"duration_sec"`
	Probe       *ProbeInfo     `json:"probe,omitempty"`
	MoovFront   bool           `json:"moov_front"`
	Action      Action         `json:"action,omitempty"`
	VideoIndex  int            `json:"video_index"`
	AudioIndex  int            `json:"audio_index"` // -1 when the file has no audio
	Output      string         `json:"output,omitempty"`
	Thumb       string         `json:"thumb,omitempty"`
	// EncHw marks the successful conversion as hardware-encoded, so the
	// verify stage can tell a hardware output from a software one: a
	// hardware output that fails verification is redone once with
	// libx264 in the same job.
	EncHw bool `json:"enc_hw,omitempty"`
	// HwRetried marks the conversion already fell back to software after
	// a hardware failure; the retry never tries hardware again.
	HwRetried bool `json:"hw_retried,omitempty"`
	// Shown marks the show phase finished: the episode is published
	// playing its original (or was final in place), so any convert-stage
	// work left is an improvement. Jobs from before the show-first change
	// have it false and are routed through the show stage before their
	// conversion resumes (see runStage).
	Shown bool `json:"shown"`
	// Published is the show-phase publish marker: a kill between the
	// publish and the requeue resumes without publishing twice.
	Published bool `json:"published"`
	// Improved is the improvement-phase publish marker, the same
	// idempotency for the converted copy.
	Improved bool `json:"improved"`
}

// Queue implements core.IngestQueue on top of a jobs.Queue: Enqueue hands a
// file to the pipeline and assigns its episode id up front (the id keys
// resume positions, the watch log and migration whether or not the job ever
// finishes), and IngestStatus reports progress and the published episode.
type Queue struct {
	q *jobs.Queue
	// enqueueMu makes Enqueue atomic per source, held across the same-source
	// lookup and the insert. The launcher guarantees one serving process per
	// data dir, so process-local mutual exclusion is sufficient: concurrent
	// producers (scanner and migrate goroutines) cannot both miss the lookup
	// and create two jobs for one source.
	enqueueMu sync.Mutex
}

// NewQueue returns an IngestQueue over q. The jobs table needs no migrations
// of its own here: the queue table belongs to internal/jobs and the stage
// outputs live in its payload column.
func NewQueue(q *jobs.Queue) *Queue {
	return &Queue{q: q}
}

// JobView is one ingest job as the parent dashboard lists it: the file it
// came from, the pipeline stage it is on, its state, and the plain-words
// message for paused (disk full) and failed jobs (the jobs table's error
// column). JSON tags keep the dashboard API shape stable.
type JobView struct {
	ID        string         `json:"id"`
	State     string         `json:"state"`
	Stage     string         `json:"stage"`
	Message   string         `json:"message,omitempty"`
	File      string         `json:"file"`
	Source    core.Source    `json:"source"`
	EpisodeID core.EpisodeID `json:"episode_id"`
	Created   time.Time      `json:"created"`
	Updated   time.Time      `json:"updated"`
}

// Jobs lists every ingest job, most recently enqueued first. A job whose
// payload does not decode still lists with its id, stage, state and error,
// so the dashboard never hides a stuck job.
func (s *Queue) Jobs() ([]JobView, error) {
	jobs, err := s.q.List(JobKind)
	if err != nil {
		return nil, err
	}
	out := make([]JobView, 0, len(jobs))
	for i := range jobs {
		j := &jobs[i]
		var p payload
		_ = j.DecodePayload(&p)
		out = append(out, JobView{
			ID: j.ID, State: j.State, Stage: j.Stage, Message: j.Error,
			File: p.File, Source: p.Source, EpisodeID: p.EpisodeID,
			Created: j.Created, Updated: j.Updated,
		})
	}
	return out, nil
}

// Enqueue implements core.IngestQueue. It is idempotent per source: a job
// for the same (src.Kind, src.Path) that is queued, running, paused or done
// is returned instead of enqueuing again, so a producer that crashes after
// enqueuing but before recording the id can safely retry; only a failed job
// is replaced by a new one.
func (s *Queue) Enqueue(ctx context.Context, file string, src core.Source) (string, error) {
	if file == "" {
		return "", errors.New("ingest: enqueue: empty file")
	}
	if src.Title == "" || src.Show == "" {
		return "", errors.New("ingest: enqueue: source needs a title and a show")
	}

	// The same-source lookup and the insert must be one atomic step, or two
	// producers (scanner and migrate goroutines) can both miss and create two
	// jobs; enqueueMu (see Queue) covers the pair.
	s.enqueueMu.Lock()
	defer s.enqueueMu.Unlock()

	if id, err := s.sameSourceJob(src); err != nil {
		return "", err
	} else if id != "" {
		return id, nil
	}
	id, err := newEpisodeID()
	if err != nil {
		return "", err
	}
	return s.q.Enqueue(JobKind, payload{
		File:      file,
		Source:    src,
		EpisodeID: id,
	})
}

// sameSourceJob returns the id of an existing ingest job for the same source
// (kind, path) that has not failed, or "" when none exists. The source lives
// in the job payload, so the lookup scans the ingest jobs by decoding their
// payloads; the queue table itself stays owned by internal/jobs. Callers
// must hold enqueueMu, which couples the lookup with the following insert.
func (s *Queue) sameSourceJob(src core.Source) (string, error) {
	all, err := s.q.List(JobKind)
	if err != nil {
		return "", fmt.Errorf("ingest: enqueue: %w", err)
	}
	for i := range all {
		j := &all[i]
		if j.State == jobs.StateFailed {
			continue
		}
		var pl payload
		if err := j.DecodePayload(&pl); err != nil {
			continue // not an ingest payload; never ours to reuse
		}
		if pl.Source.Kind == src.Kind && pl.Source.Path == src.Path {
			return j.ID, nil
		}
	}
	return "", nil
}

// IngestStatus implements core.IngestQueue: done is false while the job is
// queued, running or paused; once it finished, done is true with the
// published episode id - the zero id when the job failed.
func (s *Queue) IngestStatus(ctx context.Context, jobID string) (core.EpisodeID, bool, error) {
	j, err := s.q.Get(jobID)
	if err != nil {
		return "", false, err
	}
	var pl payload
	if err := j.DecodePayload(&pl); err != nil {
		return "", false, fmt.Errorf("ingest: status %s: %w", jobID, err)
	}
	switch j.State {
	case jobs.StateDone:
		return pl.EpisodeID, true, nil
	case jobs.StateFailed:
		return "", true, nil
	default:
		return "", false, nil
	}
}

// newEpisodeID returns a fresh random episode id (128-bit hex), assigned at
// enqueue so nothing else ever names the episode.
func newEpisodeID() (core.EpisodeID, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("ingest: episode id: %w", err)
	}
	return core.EpisodeID(hex.EncodeToString(b[:])), nil
}

// showIDFor derives a stable show id from the show's display name, so the
// same show always lands on the same id - across re-ingests, re-imports and
// migration - and never collides with another show's. The library package
// treats the id as an opaque key ingest assigns.
func showIDFor(show string) string {
	sum := sha256.Sum256([]byte(show))
	return "show_" + hex.EncodeToString(sum[:8])
}

// Worker claims ingest jobs and runs them through the pipeline, one at a
// time per lane: a sort lane for show-phase work (priority 0) and a fix
// lane for improvements, running concurrently so a long conversion never
// delays a newly added file from reaching the shelf (see Run).
type Worker struct {
	q        *jobs.Queue
	dir      *tools.Dir
	pub      core.Publisher
	fixer    core.FixRecorder
	resv     *disk.Reservations
	mediaDir string
	runCmd   func(ctx context.Context, name string, args ...string) ([]byte, error)
	// safari reports the household's "some screens use Safari" setting;
	// nil means on (the default). When it is off, remux-only plans are
	// final in place and never grow an improvement job.
	safari func(ctx context.Context) (bool, error)
	// hw is the process-wide hardware encoder selection: probed lazily
	// before the first full re-encode, cached, and cut off after three
	// consecutive failures (see hwenc.go).
	hw *hwState
}

// WithFixRecorder wires the library's fix-status setter: when an
// improvement fails (the original stays on the shelf), the worker marks
// the episode's fix "failed" so the pages can explain why it plays badly.
// internal/app passes the library; tests pass a fake or leave it nil.
func (w *Worker) WithFixRecorder(f core.FixRecorder) *Worker {
	w.fixer = f
	return w
}

// NewWorker returns a worker over q using the helper binaries in dir,
// publishing finished episodes through pub into mediaDir (the library
// subdirectory holds the converted files).
func NewWorker(q *jobs.Queue, dir *tools.Dir, pub core.Publisher, mediaDir string) *Worker {
	return &Worker{
		q:        q,
		dir:      dir,
		pub:      pub,
		resv:     disk.NewReservations(q),
		mediaDir: mediaDir,
		runCmd:   runCombined,
		hw:       newHWState(),
	}
}

// WithReservations overrides the reservation manager; tests inject free
// probes this way.
func (w *Worker) WithReservations(r *disk.Reservations) *Worker {
	w.resv = r
	return w
}

// WithSafariSetting wires the household's "some screens use Safari" setting
// into the worker (internal/app passes the budget setting). A nil setting
// means on, the default.
func (w *Worker) WithSafariSetting(safari func(ctx context.Context) (bool, error)) *Worker {
	w.safari = safari
	return w
}

// Run claims and processes jobs of kind "ingest" until ctx is done, on
// two lanes: a sort lane that claims only show-phase work (priority 0),
// so a newly added file is probed, planned and published within seconds
// even while a long conversion runs, and a fix lane that claims only
// improvements (priority 1 and up), cheapest fix first. Each lane handles
// one job at a time; the two run concurrently. A job being processed when
// ctx ends is left running with its stage persisted; jobs.Recover at the
// next launch requeues it from that stage.
func (w *Worker) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		w.runLane(ctx, prioShow, prioShow)
	}()
	go func() {
		defer wg.Done()
		w.runLane(ctx, 1, math.MaxInt)
	}()
	wg.Wait()
	return nil
}

// runLane claims jobs whose priority lies in [minP, maxP] and runs each
// to a terminal state, one at a time, until ctx ends. A lane with nothing
// to claim waits a second before asking again; a claim error (the
// database is transiently busy) backs off the same way instead of killing
// the other lane.
func (w *Worker) runLane(ctx context.Context, minP, maxP int) {
	for {
		if ctx.Err() != nil {
			return
		}
		job, err := w.q.ClaimRange(JobKind, minP, maxP)
		if err != nil {
			log.Printf("ingest: claim: %v", err)
		} else if job != nil {
			w.process(ctx, job)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// process runs a claimed job to a terminal state (done, failed or paused)
// or until ctx ends.
func (w *Worker) process(ctx context.Context, job *jobs.Job) {
	for {
		if ctx.Err() != nil {
			return
		}
		if w.processOne(ctx, job.ID) {
			return
		}
	}
}

// processOne runs exactly one remaining stage of jobID, which must already
// be claimed (state running), and reports whether the job reached a terminal
// state. The stage name is persisted before the stage runs and the next
// stage's name only after it finished, so a kill at any point resumes at the
// stage it interrupted. process calls it in a loop; the kill-and-resume test
// calls it once per simulated run.
func (w *Worker) processOne(ctx context.Context, jobID string) (done bool) {
	job, err := w.q.Get(jobID)
	if err != nil {
		return true // database trouble; nothing more to do this round
	}
	if job.State != jobs.StateRunning {
		return true
	}
	pl, err := decodePayload(job)
	if err != nil {
		w.q.Fail(job.ID, errors.New(msgGeneric))
		return true
	}
	if w.dir == nil {
		// app.Open could not set up the helper programs; every stage needs
		// them, so the job fails with the words the parent page shows.
		w.q.Fail(job.ID, errors.New(msgNoHelpers))
		return true
	}
	if err := os.MkdirAll(w.libraryDir(), 0o755); err != nil {
		w.q.Fail(job.ID, errors.New(msgGeneric))
		return true
	}
	stage := job.Stage
	if stage == "" {
		stage = StageProbe
	}
	if ctx.Err() != nil {
		return false // killed before the stage ran; leave running for Recover
	}
	if err := w.q.Advance(job.ID, stage, pl); err != nil {
		w.q.Fail(job.ID, errors.New(msgGeneric))
		return true
	}
	next, err := w.runStage(ctx, job.ID, stage, &pl)
	if err != nil {
		if ctx.Err() != nil {
			// The server is quitting or shutting down mid-stage: leave the
			// job running where it is; jobs.Recover resumes it next launch
			// instead of failing the file.
			return false
		}
		// Persist the stage's final payload (conversions released their
		// disk reservation, etc.) before pausing or failing.
		if aerr := w.q.Advance(job.ID, stage, pl); aerr == nil {
			w.stageErr(ctx, job.ID, &pl, err)
		} else {
			w.q.Fail(job.ID, errors.New(msgGeneric))
		}
		return true
	}
	if next == requeuedStage {
		// The show stage requeued the job for its improvement: it is
		// queued again with the convert stage and its improvement
		// priority, and this claim is over.
		return true
	}
	if next == "" {
		if err := w.q.Done(job.ID); err != nil {
			w.q.Fail(job.ID, errors.New(msgGeneric))
		}
		return true
	}
	if err := w.q.Advance(job.ID, next, pl); err != nil {
		w.q.Fail(job.ID, errors.New(msgGeneric))
		return true
	}
	return false
}

func (w *Worker) runStage(ctx context.Context, jobID, stage string, pl *payload) (string, error) {
	// A folder/NAS job recovered mid-improvement that was never shown (it
	// predates the show-first change, or a kill landed between the plan
	// and the old publish) must do the show phase first and only then
	// requeue for its conversion. YouTube jobs never show in place.
	if pl.Source.Kind != core.SourceYouTube && !pl.Shown {
		switch stage {
		case StageConvert, StageVerify, StageThumbnail, StagePublish:
			return w.stageShow(ctx, jobID, pl)
		}
	}
	switch stage {
	case StageProbe:
		return w.stageProbe(ctx, pl)
	case StagePlan:
		return w.stagePlan(pl)
	case StageShow:
		return w.stageShow(ctx, jobID, pl)
	case StageConvert:
		return w.stageConvert(ctx, jobID, pl)
	case StageVerify:
		return w.stageVerify(ctx, jobID, pl)
	case StageThumbnail:
		return w.stageThumb(ctx, pl)
	case StagePublish:
		return w.stagePublish(ctx, jobID, pl)
	default:
		return "", errors.New(msgGeneric)
	}
}

// requeuedStage is the runStage result a stage returns after requeuing the
// job itself (the show stage publishes and then requeues for its
// improvement); processOne treats it like a terminal claim, without touching
// the job's stage.
const requeuedStage = "requeued"

// stageErr records how a stage failed: a pauseError pauses the job with its
// parent-readable reason (a full disk is something the parent can fix), any
// other error fails the job with the error's message, and stages keep their
// messages in plain words. A failed improvement keeps the episode playing
// its original file, so the fix is marked "failed" (the issue stays) and
// the pages can explain why the video plays badly.
func (w *Worker) stageErr(ctx context.Context, jobID string, pl *payload, err error) {
	var pe *pauseError
	if errors.As(err, &pe) {
		w.q.Pause(jobID, pe.reason)
		return
	}
	w.q.Fail(jobID, err)
	// Only improvements mark anything: a paused job (a full disk) resumes
	// and keeps its pending fix, and a job that never reached the show
	// phase published nothing to mark.
	if pl.Shown && w.fixer != nil {
		if ferr := w.fixer.SetFixFailed(ctx, pl.EpisodeID); ferr != nil {
			log.Printf("ingest: mark fix failed for %s: %v", pl.EpisodeID, ferr)
		}
	}
}

// pauseError pauses a job instead of failing it: the work is blocked on
// something outside the file (a full disk), not broken.
type pauseError struct{ reason string }

func (e *pauseError) Error() string { return e.reason }

// stageProbe inspects the original with ffprobe and keeps the report in the
// payload. A folder/NAS original is read by ffprobe alone - no
// through-the-end decode and no box-tree walk - because the show phase
// publishes it immediately: the legacy app served every original without
// checking media data, and the owner prefers everything visible sooner; the
// deep checks run on the converted output in stageVerify instead. YouTube
// downloads keep the old probe: a box tree that runs past the end of the
// file (a truncated faststart MP4 keeps its moov index while the mdat no
// longer fits) fails it with the plain damaged-file words, and so does
// media data that does not decode through the end - which matters for a
// file the plan would publish as-is (inplace or keep), because its bytes
// are served verbatim and nothing downstream ever reads the media data
// again.
func (w *Worker) stageProbe(ctx context.Context, pl *payload) (string, error) {
	info, err := ProbeFile(ctx, w.dir.FFprobe, pl.File)
	if err != nil {
		return "", errors.New(msgDamaged)
	}
	pl.Probe = info
	pl.DurationSec = info.durationSeconds()
	if pl.Source.Kind != core.SourceYouTube {
		// Folder/NAS: the show phase publishes whatever ffprobe can read.
		return StagePlan, nil
	}
	moov, mErr := moovBeforeMdat(pl.File)
	if mErr != nil {
		if info.IsMP4() {
			return "", errors.New(msgDamaged)
		}
		moov = false // not an MP4 box tree; nothing to reorder
	}
	pl.MoovFront = moov
	if a, aerr := Plan(info); aerr == nil && (a == ActionKeep || a == ActionInplace) {
		vIdx, aIdx := info.selectStreams()
		if err := w.decodeCheck(ctx, pl.File, vIdx, aIdx, float64(info.Duration), true); err != nil {
			return "", errors.New(msgDamaged)
		}
	}
	return StagePlan, nil
}

// stagePlan picks the normalization action and stores it together with the
// stream indices the action's ffmpeg run maps, so convert and verify use
// exactly the streams the plan decided on. A YouTube download that would
// qualify for in-place serving is downgraded to keep/remux: its file stays
// an app-owned converted copy in the media folder, exactly as before the
// in-place decision. Folder/NAS jobs go to the show stage next - the plan
// only decides what the improvement will fix later, never what plays now.
func (w *Worker) stagePlan(pl *payload) (string, error) {
	if pl.Probe == nil {
		return "", errors.New(msgGeneric) // unreachable: probe runs first
	}
	a, err := Plan(pl.Probe)
	if err != nil {
		return "", errors.New(msgNoVideo)
	}
	if a == ActionInplace && pl.Source.Kind == core.SourceYouTube {
		if pl.MoovFront {
			a = ActionKeep
		} else {
			a = ActionRemux
		}
	}
	pl.Action = a
	pl.VideoIndex, pl.AudioIndex = pl.Probe.selectStreams()
	if pl.Source.Kind != core.SourceYouTube {
		return StageShow, nil
	}
	return StageConvert, nil
}

// stageShow is the show phase: it publishes the episode playing its
// original file (real path, Owned false, thumbnail from the original) and
// then either finishes the job - the plan needs nothing more, or the
// household turned the Safari remux off - or requeues it at convert with
// the fix-lane priority for its plan's action. The publish carries the
// fix status: the issue the queued improvement will fix and fix
// "pending", or "" / "" when the plan is final in place. The publish is
// persisted before the requeue, so a kill anywhere in the stage resumes
// without publishing twice, and library.Publish upserts by episode id, so
// resume positions and watch-log rows stay attached when the improvement
// later replaces the file.
func (w *Worker) stageShow(ctx context.Context, jobID string, pl *payload) (string, error) {
	improve, action := w.improvement(ctx, pl)
	if !pl.Published {
		real, err := filepath.EvalSymlinks(pl.File)
		if err != nil {
			return "", errors.New(msgGeneric)
		}
		pl.Output = real
		// The thumbnail is cosmetic: stageThumb never fails the job.
		w.stageThumb(ctx, pl)
		issue, fix := issueFixFor(action)
		ep := core.Episode{
			ID:          pl.EpisodeID,
			ShowID:      showIDFor(pl.Source.Show),
			Title:       pl.Source.Title,
			File:        real,
			DurationSec: pl.DurationSec,
			Thumb:       pl.Thumb,
			Source:      pl.Source,
			Owned:       false,
			Issue:       issue,
			Fix:         fix,
		}
		if err := w.pub.Publish(ctx, ep); err != nil {
			return "", errors.New(msgGeneric)
		}
		pl.Published = true
		pl.Shown = true
		if err := w.q.Advance(jobID, StageShow, pl); err != nil {
			return "", errors.New(msgGeneric)
		}
	}
	if !improve {
		// Shown and final in place: the job is done.
		return "", nil
	} else if err := w.q.Requeue(jobID, StageConvert, fixPriority(action, pl.DurationSec)); err != nil {
		return "", errors.New(msgGeneric)
	}
	return requeuedStage, nil
}

// improvement decides whether an already-shown folder/NAS job still needs
// its conversion, and which action the fix lane will apply. In-place
// plans are final; a remux-only plan is final too when the "some screens
// use Safari" setting is off (the setting applies to videos processed
// after the change); audio re-encodes run before full re-encodes before
// remuxes.
func (w *Worker) improvement(ctx context.Context, pl *payload) (bool, Action) {
	switch pl.Action {
	case ActionInplace, ActionKeep:
		return false, ""
	case ActionRemux:
		if w.safari != nil {
			// An unreadable setting falls back to on (convert): a failed
			// read must not leave an episode that Safari cannot play.
			if on, err := w.safari(ctx); err == nil && !on {
				return false, ""
			}
		}
		return true, ActionRemux
	case ActionAudio:
		return true, ActionAudio
	case ActionFull:
		return true, ActionFull
	default:
		return true, ActionRemux
	}
}

// issueFixFor is the fix-status pair a show-phase publish records for its
// plan: "" / "" when nothing is queued (the plan is final in place),
// otherwise the issue the queued improvement will fix plus fix "pending".
// Pending episodes stay on the shelf but do not play until the better
// copy is published; the web layer answers their playback 409.
func issueFixFor(action Action) (issue, fix string) {
	switch action {
	case ActionAudio:
		return core.IssueSound, core.FixPending
	case ActionFull:
		return core.IssuePicture, core.FixPending
	case ActionRemux:
		return core.IssueFormat, core.FixPending
	default:
		return core.IssueNone, core.FixNone
	}
}

// stageConvert produces the improvement's file, an app-owned MP4 in the
// media library: it first reserves the worst-case bytes (input size for
// keep and remux, the encoder-capped duration bound for audio and full
// re-encodes) against the disk floor; a reservation denied for lack of
// space pauses the job. The helper writes a temp file which is renamed into
// place only on success, so a failure - a full disk included - never
// damages the original or the shown episode and never leaves a partial
// copy. The reservation is released on every exit; a kill mid-convert
// leaves the reservation held, so the resumed run recognizes it and
// re-reserves only when it is gone. A conversion that fails for any other
// reason fails the job with the keep-as-is words: the episode stays
// published playing its original, it is never unpublished.
func (w *Worker) stageConvert(ctx context.Context, jobID string, pl *payload) (string, error) {
	tmp := filepath.Join(w.libraryDir(), "."+string(pl.EpisodeID)+".tmp.mp4")
	final := filepath.Join(w.libraryDir(), string(pl.EpisodeID)+".mp4")
	fail := func(err error) (string, error) {
		os.Remove(tmp)
		w.resv.Release(jobID)
		return "", err
	}
	if !w.reserved(jobID) {
		if err := w.reserve(jobID, pl); err != nil {
			return fail(err)
		}
	}
	var out []byte
	var err error
	switch pl.Action {
	case ActionKeep:
		err = copyFile(ctx, pl.File, tmp)
	case ActionRemux:
		out, err = w.run(ctx, w.dir.FFmpeg, w.streamArgs(pl, "-c", "copy", "-movflags", "+faststart", tmp)...)
	case ActionAudio:
		out, err = w.run(ctx, w.dir.FFmpeg, w.streamArgs(pl,
			"-c:v", "copy", "-c:a", "aac", "-b:a", "160k", "-ac", "2",
			"-movflags", "+faststart", tmp)...)
	case ActionFull:
		out, err = w.fullConvert(ctx, pl, tmp)
	default:
		return fail(errors.New(msgGeneric))
	}
	if err != nil {
		return fail(w.convertErr(err, out, pl.Shown))
	}
	if err := os.Rename(tmp, final); err != nil {
		return fail(errors.New(convertMsg(pl.Shown)))
	}
	w.resv.Release(jobID)
	pl.Output = final
	return StageVerify, nil
}

// streamArgs builds the ffmpeg prefix that opens the input and maps exactly
// the plan's chosen streams (0-based stream numbers), so attached artwork,
// extra audio tracks, subtitles and data streams never reach the output.
func (w *Worker) streamArgs(pl *payload, tail ...string) []string {
	args := []string{"-y", "-i", pl.File, "-map", "0:" + strconv.Itoa(pl.VideoIndex)}
	if pl.AudioIndex >= 0 {
		args = append(args, "-map", "0:"+strconv.Itoa(pl.AudioIndex))
	}
	return append(args, tail...)
}

// scaleFilter returns the -vf expression for a full re-encode: the display
// size - the coded dimensions turned by the stream's rotation metadata - is
// clamped to 1920x1080 for landscape video and 1080x1920 for portrait (a
// rotated 1920x1080 phone video displays as 1080x1920), without upscaling
// and always rounding to even (what the H.264 encoder requires).
func scaleFilter(p *ProbeInfo) string {
	wide, tall := 0, 0
	if p != nil {
		if vIdx, _ := p.selectStreams(); vIdx >= 0 {
			s := &p.Streams[vIdx]
			wide, tall = s.Width, s.Height
			if r := s.rotation(); r == 90 || r == 270 {
				wide, tall = tall, wide
			}
		}
	}
	even := "scale=trunc(iw/2)*2:trunc(ih/2)*2"
	if wide == 0 || tall == 0 {
		return even
	}
	limW, limH := 1920, 1080
	if tall > wide {
		limW, limH = 1080, 1920
	}
	if wide > limW || tall > limH {
		return fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,%s", limW, limH, even)
	}
	return even
}

// reserved reports whether jobID currently holds a disk reservation; the
// jobs table is the source of truth, so a resumed job whose reservation
// survived its kill skips re-reserving.
func (w *Worker) reserved(jobID string) bool {
	j, err := w.q.Get(jobID)
	if err != nil {
		return false
	}
	return j.ReservedBytes > 0
}

// reserve accounts jobID's worst-case disk need on the media directory. Keep
// and remux only write a copy, so the input size suffices. A full re-encode
// is bounded by its encoder caps (video -maxrate 8M, audio 160k): the probed
// duration at 8.16 Mbit/s. An AUDIO job copies the video uncapped, so its
// bound is the source video's own bytes plus the 160 kbit/s audio. Every
// bound carries a 10% margin plus a fixed 50 MB of container and index
// slack. A full disk pauses the job instead of failing it.
func (w *Worker) reserve(jobID string, pl *payload) error {
	st, err := os.Stat(pl.File)
	if err != nil {
		return errors.New(msgGeneric)
	}
	input := uint64(st.Size())
	worst := input
	if pl.Action == ActionAudio {
		worst = audioReservedBytes(pl, input)
	} else if pl.Action == ActionFull {
		worst = reservedBytes(pl)
	}
	if err := w.resv.Reserve(jobID, worst, w.mediaDir); err != nil {
		var ns *disk.ErrNoSpace
		if errors.As(err, &ns) {
			return &pauseError{reason: msgDiskFull}
		}
		return errors.New(msgGeneric)
	}
	return nil
}

// probeDuration returns the payload's duration in seconds: the probed
// duration (fraction included) when there is one, else the rounded seconds,
// never negative.
func probeDuration(pl *payload) float64 {
	dur := float64(pl.DurationSec)
	if pl.Probe != nil && pl.Probe.Duration > 0 {
		dur = float64(pl.Probe.Duration)
	}
	if dur < 0 {
		dur = 0
	}
	return dur
}

// reservedBytes is the worst-case media a full re-encode can write: with
// -maxrate 8M on the video and 160 kbit/s on the audio, the output cannot
// exceed the probed duration at 8.16 Mbit/s, times 1.1 for slack, plus a
// fixed 50 MiB for the container, the index and muxing jitter.
func reservedBytes(pl *payload) uint64 {
	return uint64(probeDuration(pl)*8_160_000/8*1.1) + 50<<20
}

// audioReservedBytes is the worst-case media an AUDIO job can write: the
// video is copied uncapped, so its share is the source video stream's own
// bytes - the probe's bit_rate over the duration, or the whole input file
// when the probe reports none - plus the audio re-encode at 160 kbit/s,
// times 1.1 for slack, plus the fixed 50 MiB muxing headroom.
func audioReservedBytes(pl *payload, input uint64) uint64 {
	video := float64(input)
	if pl.Probe != nil {
		if vIdx, _ := pl.Probe.selectStreams(); vIdx >= 0 && pl.Probe.Streams[vIdx].BitRate > 0 {
			video = float64(pl.Probe.Streams[vIdx].BitRate) * probeDuration(pl) / 8
		}
	}
	audio := probeDuration(pl) * 160_000 / 8
	return uint64((video+audio)*1.1) + 50<<20
}

// convertErr classifies a failed conversion: a full disk pauses the job (the
// original and everything already published stay intact, the parent frees
// space, the job resumes), anything else means the file cannot be
// normalized - and when the episode is already shown, the job fails with
// the keep-as-is words instead of the conversion ones.
func (w *Worker) convertErr(err error, out []byte, shown bool) error {
	if isDiskFullErr(err, out) {
		return &pauseError{reason: msgDiskFull}
	}
	return errors.New(convertMsg(shown))
}

// convertMsg is the failure wording of a convert/verify error: the
// keep-as-is words when the episode is already published playing its
// original (the failure only costs the better copy), the plain conversion
// words otherwise (YouTube downloads, which publish once).
func convertMsg(shown bool) string {
	if shown {
		return msgKeepAsIs
	}
	return msgConvert
}

// isDiskFullErr reports whether a failed helper run means the disk filled
// up: either the error carries ENOSPC (a Go-side write) or ffmpeg printed
// its strerror text, which is the usual case because exec errors do not wrap
// the syscall.
func isDiskFullErr(err error, out []byte) bool {
	if disk.IsDiskFull(err) {
		return true
	}
	return bytes.Contains(out, []byte("No space left on device"))
}

// stageVerify re-probes the converted output and requires what the target
// players need: H.264 8-bit yuv420p video, AAC audio (when there is audio)
// and media data that decodes through the end - a metadata check alone would
// accept an output whose mdat was truncated after the index was written. The
// moov-before-mdat (faststart) requirement applies to every converted file.
// The codec checks run on the streams selectStreams picks, not blindly the
// first video stream: a kept file keeps the input's own numbering, cover
// art included, so its output must still have its real H.264 video
// accepted.
//
// A hardware-encoded output that fails verification is redone once with
// libx264 in the same job (the graphics chip made a copy the players
// reject), and the failure counts toward the process-wide hardware cutoff;
// the redo is verified again and only then, if it also fails, does the job
// fail with the keep-as-is words when the episode is already shown. A
// software output is never retried.
func (w *Worker) stageVerify(ctx context.Context, jobID string, pl *payload) (string, error) {
	if err := w.verifyOutput(ctx, pl); err != nil {
		if pl.EncHw && !pl.HwRetried {
			w.hw.noteFailure()
			pl.HwRetried = true
			pl.EncHw = false
			next, cErr := w.stageConvert(ctx, jobID, pl)
			if cErr != nil {
				return "", cErr
			}
			if next != StageVerify {
				return "", errors.New(convertMsg(pl.Shown))
			}
			if vErr := w.verifyOutput(ctx, pl); vErr != nil {
				return "", vErr
			}
			return StageThumbnail, nil
		}
		return "", err
	}
	if pl.EncHw {
		// A hardware output that passed verification proves the encoder
		// works: the consecutive-failure count starts over.
		w.hw.noteSuccess()
	}
	return StageThumbnail, nil
}

// verifyOutput runs the conversion checks on pl.Output.
func (w *Worker) verifyOutput(ctx context.Context, pl *payload) error {
	info, err := ProbeFile(ctx, w.dir.FFprobe, pl.Output)
	if err != nil {
		return errors.New(convertMsg(pl.Shown))
	}
	vIdx, aIdx := info.selectStreams()
	if vIdx < 0 {
		return errors.New(convertMsg(pl.Shown))
	}
	v := &info.Streams[vIdx]
	if v.CodecName != "h264" || !eightBitYUV420P(v.PixFmt) {
		return errors.New(convertMsg(pl.Shown))
	}
	if aIdx >= 0 && info.Streams[aIdx].CodecName != "aac" {
		return errors.New(convertMsg(pl.Shown))
	}
	front, err := moovBeforeMdat(pl.Output)
	if err != nil || !front {
		return errors.New(convertMsg(pl.Shown))
	}
	// Decode through the end on the same chosen streams: remux and
	// re-encodes renumber them (video 0, audio 1), while a kept file keeps
	// the input's own numbering, cover art included. Only a full re-encode
	// is held to a glitch-free decode: every other plan copies the source's
	// video bitstream as it is, so a small glitch the original already had
	// (browsers play through it) is in the copy too, and rejecting the copy
	// for it would leave the episode on the worse original.
	strict := pl.Action == ActionFull
	if err := w.decodeCheck(ctx, pl.Output, vIdx, aIdx, float64(info.Duration), strict); err != nil {
		return errors.New(convertMsg(pl.Shown))
	}
	return nil
}

// decodeCheck makes ffmpeg decode every byte of the chosen streams' media
// data and requires it to hold through the probed duration. The decoded
// frames are thrown away by the null muxer (-f null -), but the decode is
// real. With strict, -xerror turns any decode error (a broken bitstream,
// zeroed frames) into a nonzero exit. Without it, isolated bad frames pass,
// while a stream that mostly fails to decode still ends ffmpeg with an
// error (its default -max_error_rate). Either way the last output
// timestamp, read from the -progress file, must reach within a second of
// the probed duration, so a truncated file is rejected even when it ends
// cleanly on a packet boundary. wantDur <= 1 s skips the timestamp check.
func (w *Worker) decodeCheck(ctx context.Context, path string, vIdx, aIdx int, wantDur float64, strict bool) error {
	prog, err := os.CreateTemp("", "kids-shelf-ingest-*.log")
	if err != nil {
		return err
	}
	progPath := prog.Name()
	prog.Close()
	defer os.Remove(progPath)
	args := []string{"-v", "error"}
	if strict {
		args = append(args, "-xerror")
	}
	args = append(args, "-nostdin", "-progress", progPath,
		"-i", path, "-map", "0:"+strconv.Itoa(vIdx))
	if aIdx >= 0 {
		args = append(args, "-map", "0:"+strconv.Itoa(aIdx))
	}
	args = append(args, "-f", "null", "-")
	if _, err := w.run(ctx, w.dir.FFmpeg, args...); err != nil {
		return err
	}
	lastUS, ok := lastOutTimeUS(progPath)
	if !ok {
		return errors.New("ingest: decode check: no output timestamps reported")
	}
	if wantDur > 1 && float64(lastUS)/1e6 < wantDur-1 {
		return errors.New("ingest: decode check: media ends before the probed duration")
	}
	return nil
}

// lastOutTimeUS returns the last output time in microseconds ffmpeg reported
// in a -progress file, and whether any was reported.
func lastOutTimeUS(path string) (int64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	key := []byte("out_time_us=")
	idx := -1
	for i := 0; i+len(key) <= len(b); {
		j := bytes.Index(b[i:], key)
		if j < 0 {
			break
		}
		idx = i + j
		i = idx + len(key)
	}
	if idx < 0 {
		return 0, false
	}
	val := b[idx+len(key):]
	if nl := bytes.IndexByte(val, '\n'); nl >= 0 {
		val = val[:nl]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(val)), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// stageThumbnail writes a JPEG preview at 10% of the video - at most 60
// seconds in - scaled to 480 px wide, next to the episode file. A thumbnail
// is cosmetic, so a failure leaves none rather than rejecting the episode.
func (w *Worker) stageThumb(ctx context.Context, pl *payload) (string, error) {
	t := math.Min(0.1*float64(pl.DurationSec), 60)
	if t < 0 {
		t = 0
	}
	tmp := filepath.Join(w.libraryDir(), "."+string(pl.EpisodeID)+".tmp.jpg")
	final := filepath.Join(w.libraryDir(), string(pl.EpisodeID)+".jpg")
	_, err := w.run(ctx, w.dir.FFmpeg,
		"-y", "-ss", strconv.FormatFloat(t, 'f', 3, 64), "-i", pl.Output,
		"-frames:v", "1", "-vf", "scale=480:-2", "-q:v", "3", tmp)
	if err == nil {
		if rerr := os.Rename(tmp, final); rerr == nil {
			pl.Thumb = final
		}
	}
	os.Remove(tmp) // renamed away, or the timestamp frame never decoded
	return StagePublish, nil
}

// stagePublish files the improvement's converted episode with the library.
// The publish is persisted before the job finishes, so a kill between
// publish and the queue's Done marker resumes without publishing twice.
// library.Publish upserts by episode id, so the episode keeps its resume
// positions, watch-log rows and recent entry; only the recorded file (and
// the Owned flag) changes, to the app-owned converted copy. The show phase
// publishes inside stageShow; this stage is the improvement's publish.
func (w *Worker) stagePublish(ctx context.Context, jobID string, pl *payload) (string, error) {
	if pl.Improved {
		return "", nil
	}
	// A job from before the show-first change (YouTube, or a folder/NAS
	// job the old single-publish flow already finished) marked Published;
	// nothing more is wanted from it.
	if !pl.Shown && pl.Published {
		return "", nil
	}
	ep := core.Episode{
		ID:          pl.EpisodeID,
		ShowID:      showIDFor(pl.Source.Show),
		Title:       pl.Source.Title,
		File:        pl.Output,
		DurationSec: pl.DurationSec,
		Thumb:       pl.Thumb,
		Source:      pl.Source,
		Owned:       true,
		// The improved copy plays everywhere: Issue and Fix stay empty,
		// which clears the pending status the show-phase publish set.
		Issue: core.IssueNone,
		Fix:   core.FixNone,
	}
	if err := w.pub.Publish(ctx, ep); err != nil {
		return "", errors.New(msgGeneric)
	}
	pl.Improved = true
	if err := w.q.Advance(jobID, StagePublish, pl); err != nil {
		return "", errors.New(msgGeneric)
	}
	return "", nil
}

// run executes a helper, returning its combined output.
func (w *Worker) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return w.runCmd(ctx, name, args...)
}

// runCombined runs a helper and captures stdout and stderr together; errors
// are the process's, and the output is what classify failures read.
func runCombined(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := proc.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return out, err
}

// runStdout runs a helper and returns its standard output alone; standard
// error only goes into the error when the helper fails. ffprobe prints
// warnings on stderr even when it succeeds, and mixed into its JSON they
// made perfectly readable files look damaged.
func runStdout(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := proc.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%w: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	return out, nil
}

// libraryDir is where converted episodes and thumbnails land.
func (w *Worker) libraryDir() string {
	return filepath.Join(w.mediaDir, "library")
}

func decodePayload(job *jobs.Job) (payload, error) {
	var pl payload
	if err := job.DecodePayload(&pl); err != nil {
		return payload{}, fmt.Errorf("ingest: decode %s: %w", job.ID, err)
	}
	return pl, nil
}

// copyFile copies src to dst (created 0644) in chunks, checking ctx between
// them, and removes dst on any failure - a cancelled ctx included. A keep
// job's convert stage is then left running with no temp behind it, exactly
// where jobs.Recover resumes it, instead of copying the whole file after the
// server was told to stop.
func copyFile(ctx context.Context, src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, cErr := copyChunks(ctx, out, in)
	closeErr := out.Close()
	if cErr != nil || closeErr != nil {
		os.Remove(dst)
		if cErr != nil {
			return cErr
		}
		return closeErr
	}
	return nil
}

// copyChunks copies in 1 MiB reads, checking ctx before each one, so a
// cancel stops a byte copy within one chunk of when it happened.
func copyChunks(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 1<<20)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			wn, werr := dst.Write(buf[:n])
			written += int64(wn)
			if werr != nil {
				return written, werr
			}
		}
		if rerr == io.EOF {
			return written, nil
		}
		if rerr != nil {
			return written, rerr
		}
	}
}
