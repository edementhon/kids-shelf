package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edementhon/kids-shelf/internal/core"
	"github.com/edementhon/kids-shelf/internal/disk"
	"github.com/edementhon/kids-shelf/internal/jobs"
	"github.com/edementhon/kids-shelf/internal/store"
	"github.com/edementhon/kids-shelf/internal/tools"
)

const (
	fixtureFFmpegEnv = "KS_FIXTURE_FFMPEG"
	testShow         = "Test Show"
	testTitle        = "Test Episode"
)

// fixtureEnv returns the helper directory (KS_HELPERS_DIR, set by the
// orchestrator to our built ffmpeg/ffprobe) and the full ffmpeg used to
// generate fixtures, skipping the pipeline tests when either is absent.
func fixtureEnv(t *testing.T) (helpers, full string) {
	t.Helper()
	helpers = os.Getenv(tools.EnvHelpersDir)
	full = os.Getenv(fixtureFFmpegEnv)
	if helpers == "" || full == "" {
		t.Skip("pipeline tests need " + tools.EnvHelpersDir + " and " + fixtureFFmpegEnv + " (ffmpeg/ffprobe plus a full ffmpeg with encoders)")
	}
	return helpers, full
}

// fixtures holds generated sample files. A value of "" means the encoder
// needed to produce it is missing from the fixture ffmpeg, and the tests
// that need that file skip.
type fixtures struct {
	keep, remuxMKV, noFastStart, ac3MKV, dtsMKV, hevcMKV, truncated string
	rotatedMP4, coverFirstMP4                                       string
	truncatedFaststart, coverMP4, secondAudioMKV, noDefaultAudioMKV string
}

func genFixture(t *testing.T, full, out string, args ...string) {
	t.Helper()
	cmd := exec.Command(full, append([]string{
		"-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-t", "4",
	}, append(args, out)...)...)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture %s: %v\n%s", out, err, b)
	}
}

// encoderSet lists the encoders of the fixture ffmpeg.
func encoderSet(t *testing.T, full string) map[string]bool {
	t.Helper()
	out, err := exec.Command(full, "-hide_banner", "-encoders").Output()
	if err != nil {
		t.Fatalf("ffmpeg -encoders: %v", err)
	}
	set := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 {
			set[f[1]] = true
		}
	}
	return set
}

func makeFixtures(t *testing.T) fixtures {
	t.Helper()
	_, full := fixtureEnv(t)
	encoders := encoderSet(t, full)
	// Resolved like ingest resolves in-place originals (macOS temp dirs sit
	// under the /var -> /private/var symlink), so recorded paths compare equal.
	src, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := fixtures{}
	f.keep = filepath.Join(src, "keep.mp4")
	genFixture(t, full, f.keep,
		"-c:v", "libx264", "-profile:v", "high", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "128k", "-movflags", "+faststart")
	f.remuxMKV = filepath.Join(src, "remux.mkv")
	genFixture(t, full, f.remuxMKV,
		"-c:v", "libx264", "-profile:v", "high", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "128k", "-f", "matroska")
	f.noFastStart = filepath.Join(src, "nofaststart.mp4")
	genFixture(t, full, f.noFastStart,
		"-c:v", "libx264", "-profile:v", "high", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "128k")
	f.ac3MKV = filepath.Join(src, "ac3.mkv")
	genFixture(t, full, f.ac3MKV,
		"-c:v", "libx264", "-profile:v", "high", "-pix_fmt", "yuv420p",
		"-c:a", "ac3", "-b:a", "192k", "-f", "matroska")
	if encoders["dca"] || encoders["dts"] {
		f.dtsMKV = filepath.Join(src, "dts.mkv")
		codec := "dca"
		if encoders["dts"] {
			codec = "dts"
		}
		genFixture(t, full, f.dtsMKV,
			"-c:v", "libx264", "-profile:v", "high", "-pix_fmt", "yuv420p",
			"-c:a", codec, "-strict", "-2", "-ac", "2", "-b:a", "768k", "-f", "matroska")
	}
	if encoders["libx265"] {
		f.hevcMKV = filepath.Join(src, "hevc10.mkv")
		genFixture(t, full, f.hevcMKV,
			"-c:v", "libx265", "-x265-params", "log-level=error", "-pix_fmt", "yuv420p10le",
			"-c:a", "aac", "-b:a", "128k", "-f", "matroska")
	}
	// A faststart MP4 truncated to 60%: ffprobe reads the moov (the index)
	// at the front and still reports the whole duration, so only the
	// media-data checks (the box walk and the through-the-end pass) reject
	// it.
	b, err := os.ReadFile(f.keep)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	f.truncatedFaststart = filepath.Join(src, "truncated-faststart.mp4")
	if err := os.WriteFile(f.truncatedFaststart, b[:len(b)*3/5], 0o644); err != nil {
		t.Fatalf("write truncated faststart: %v", err)
	}
	// cover.mp4: an attached-picture cover (mjpeg, the first mapped video)
	// plus the real H.264 video and two AAC tracks; the mp4 muxer sorts
	// the attached picture last and flags the first audio default.
	coverJPG := filepath.Join(src, "cover.jpg")
	if err := exec.Command(full, "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=0x336699:size=240x180", "-frames:v", "1", "-q:v", "3",
		coverJPG).Run(); err != nil {
		t.Fatalf("fixture cover.jpg: %v", err)
	}
	f.coverMP4 = filepath.Join(src, "cover.mp4")
	if err := exec.Command(full, "-y", "-loglevel", "error",
		"-i", coverJPG,
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-f", "lavfi", "-i", "sine=frequency=880:sample_rate=32000",
		"-map", "0:v", "-map", "1:v", "-map", "2:a", "-map", "3:a",
		"-c:v:0", "mjpeg", "-q:v", "3",
		"-c:v:1", "libx264", "-profile:v:1", "high", "-pix_fmt:1", "yuv420p",
		"-c:a:0", "aac", "-b:a", "128k", "-ac:a:0", "2",
		"-c:a:1", "aac", "-b:a", "96k", "-ac:a:1", "1",
		"-disposition:0", "attached_pic",
		"-t", "4", f.coverMP4).Run(); err != nil {
		t.Fatalf("fixture cover.mp4: %v", err)
	}
	// cover-first.mp4 is cover.mp4 with its moov children reordered so
	// ffprobe lists the attached picture first: ffmpeg's mp4 muxers always
	// write the cover art last, but some phones and tools write it first,
	// and those files are still in-place eligible.
	cover, err := os.ReadFile(f.coverMP4)
	if err != nil {
		t.Fatalf("read cover.mp4: %v", err)
	}
	reordered, err := coverArtFirst(cover)
	if err != nil {
		t.Fatalf("reorder cover-art track: %v", err)
	}
	f.coverFirstMP4 = filepath.Join(src, "cover-first.mp4")
	if err := os.WriteFile(f.coverFirstMP4, reordered, 0o644); err != nil {
		t.Fatalf("write cover-first.mp4: %v", err)
	}
	// rotated.mp4: a phone-style file - a 1920x1080 MPEG-4 video carrying a
	// 90° display matrix, which therefore displays as 1080x1920 portrait.
	// -noautorotate keeps the matrix in the file, so the probe reports the
	// coded dimensions plus the rotation side data; the re-encode must
	// clamp the display to the portrait cap.
	f.rotatedMP4 = filepath.Join(src, "rotated.mp4")
	if err := exec.Command(full, "-y", "-loglevel", "error",
		"-noautorotate", "-display_rotation", "90",
		"-f", "lavfi", "-i", "testsrc2=size=1920x1080:rate=24",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-c:v", "mpeg4", "-q:v", "3", "-c:a", "aac", "-b:a", "128k",
		"-t", "4", "-movflags", "+faststart", f.rotatedMP4).Run(); err != nil {
		t.Fatalf("fixture rotated.mp4: %v", err)
	}
	// secondAudioMKV: two AAC tracks, the second flagged default, so the
	// pipeline must pick it (the first audio of a matroska carries no
	// default flag here). noDefaultAudioMKV is the same file without the
	// flag, where the first audio wins.
	audioTracks := func(out string, disposition bool) {
		args := []string{"-y", "-loglevel", "error",
			"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24",
			"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
			"-f", "lavfi", "-i", "sine=frequency=880:sample_rate=32000",
			"-map", "0:v", "-map", "1:a", "-map", "2:a",
			"-c:v", "libx264", "-profile:v", "high", "-pix_fmt", "yuv420p",
			"-c:a:0", "aac", "-b:a", "128k", "-ac:a:0", "2",
			"-c:a:1", "aac", "-b:a", "96k", "-ac:a:1", "1"}
		if disposition {
			args = append(args, "-disposition:a:1", "default")
		}
		args = append(args, "-t", "4", "-f", "matroska", out)
		if err := exec.Command(full, args...).Run(); err != nil {
			t.Fatalf("fixture %s: %v", out, err)
		}
	}
	f.secondAudioMKV = filepath.Join(src, "second-audio.mkv")
	audioTracks(f.secondAudioMKV, true)
	f.noDefaultAudioMKV = filepath.Join(src, "no-default-audio.mkv")
	audioTracks(f.noDefaultAudioMKV, false)
	// A truncated file: the no-faststart MP4 has its moov at the end, so
	// cutting off the tail makes ffprobe fail deterministically.
	b, err = os.ReadFile(f.noFastStart)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	f.truncated = filepath.Join(src, "truncated.mp4")
	if err := os.WriteFile(f.truncated, b[:len(b)*2/5], 0o644); err != nil {
		t.Fatalf("write truncated: %v", err)
	}
	return f
}

// isoBox is one ISO-BMFF box within a buffer.
type isoBox struct {
	start int
	size  int
	typ   string
}

// walkBoxes lists the boxes in data[start:end], honoring a 64-bit size
// header when a box declares one. A box whose declared size overruns the
// window (garbage bytes parsed as a header) ends the walk.
func walkBoxes(data []byte, start, end int) []isoBox {
	var out []isoBox
	for i := start; i+8 <= end; {
		size := int(binary.BigEndian.Uint32(data[i:]))
		if size == 1 {
			if i+16 > end {
				break
			}
			size = int(binary.BigEndian.Uint64(data[i+8:]))
		} else if size == 0 {
			size = end - i
		}
		if size < 8 || i+size > end {
			break
		}
		out = append(out, isoBox{start: i, size: size, typ: string(data[i+4 : i+8])})
		i += size
	}
	return out
}

// coverArtFirst reorders an MP4 so ffprobe lists the attached picture as
// the first stream: the moov children are shuffled (the metadata box that
// holds the cover art is placed ahead of the trak boxes, mvhd staying
// first) and moov is moved in front of mdat, with the chunk offsets the
// faststart layout shifts patched up. ffmpeg's mp4 muxers always write the
// cover art last; the result emulates the cover-first files some phones
// and tools produce.
func coverArtFirst(b []byte) ([]byte, error) {
	top := walkBoxes(b, 0, len(b))
	var moov *isoBox
	oldMDAT := -1
	for i := range top {
		switch top[i].typ {
		case "moov":
			moov = &top[i]
		case "mdat":
			oldMDAT = top[i].start
		}
	}
	if moov == nil {
		return nil, errors.New("no moov box")
	}
	if oldMDAT < 0 {
		return nil, errors.New("no mdat box")
	}
	kids := walkBoxes(b, moov.start+8, moov.start+moov.size)
	var newMoov []byte
	for _, class := range []string{"mvhd", "udta"} {
		for _, k := range kids {
			if k.typ == class {
				newMoov = append(newMoov, b[k.start:k.start+k.size]...)
			}
		}
	}
	for _, k := range kids {
		if k.typ != "mvhd" && k.typ != "udta" {
			newMoov = append(newMoov, b[k.start:k.start+k.size]...)
		}
	}
	hdrLen := 8
	if binary.BigEndian.Uint32(b[moov.start:]) == 1 {
		hdrLen = 16
	}
	// Faststart layout: ftyp boxes, then moov, then everything else in its
	// original relative order, so the mdat lands after moov.
	out := make([]byte, 0, len(b))
	emit := func(t isoBox) {
		if t.typ == "moov" {
			out = append(out, b[moov.start:moov.start+hdrLen]...)
			out = append(out, newMoov...)
		} else {
			out = append(out, b[t.start:t.start+t.size]...)
		}
	}
	for _, t := range top {
		if t.typ == "ftyp" {
			emit(t)
		}
	}
	emit(*moov)
	for _, t := range top {
		if t.typ != "ftyp" && t.typ != "moov" {
			emit(t)
		}
	}
	// Moving moov in front of mdat shifts every chunk offset; bring the
	// stco (and co64) tables back in line.
	delta := 0
	for _, t := range walkBoxes(out, 0, len(out)) {
		if t.typ == "mdat" {
			delta = t.start - oldMDAT
			break
		}
	}
	if delta != 0 {
		moovStart := 0
		for i := range top {
			if top[i].typ == "ftyp" {
				moovStart = top[i].start + top[i].size
			}
		}
		patchChunkOffsets(out[moovStart:moovStart+hdrLen+len(newMoov)], delta)
	}
	return out, nil
}

// patchChunkOffsets adds delta to every chunk offset in the stco and co64
// tables of the given moov box.
func patchChunkOffsets(moov []byte, delta int) {
	containers := map[string]bool{
		"moov": true, "trak": true, "mdia": true, "minf": true,
		"stbl": true, "edts": true, "dinf": true, "udta": true, "meta": true,
	}
	var walk func(data []byte)
	walk = func(data []byte) {
		for _, bx := range walkBoxes(data, 0, len(data)) {
			switch bx.typ {
			case "stco", "co64":
				wide := bx.typ == "co64"
				n := int(binary.BigEndian.Uint32(data[bx.start+12:]))
				step := 4
				if wide {
					step = 8
				}
				i := bx.start + 16
				for ; n > 0; n-- {
					if wide {
						binary.BigEndian.PutUint64(data[i:], binary.BigEndian.Uint64(data[i:])+uint64(delta))
					} else {
						binary.BigEndian.PutUint32(data[i:], binary.BigEndian.Uint32(data[i:])+uint32(delta))
					}
					i += step
				}
			}
			if containers[bx.typ] {
				walk(data[bx.start+8 : bx.start+bx.size])
			}
		}
	}
	walk(moov)
}

// harness is a queue, a worker, its fake publisher and the media directory,
// all over one temp store.
type harness struct {
	iq       *Queue
	w        *Worker
	pub      *core.FakePublisher
	fix      *core.FakeFixRecorder
	q        *jobs.Queue
	mediaDir string
}

func newHarness(t *testing.T, helpers string) harness {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	q, err := jobs.New(s)
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	dir, err := tools.Locate("", nil, tools.Manifest{}, t.TempDir())
	if err != nil {
		t.Fatalf("tools.Locate: %v", err)
	}
	pub := &core.FakePublisher{}
	fix := &core.FakeFixRecorder{}
	mediaDir := t.TempDir()
	return harness{
		iq:       NewQueue(q),
		w:        NewWorker(q, dir, pub, mediaDir).WithFixRecorder(fix),
		pub:      pub,
		fix:      fix,
		q:        q,
		mediaDir: mediaDir,
	}
}

func (h harness) enqueue(t *testing.T, file string) string {
	t.Helper()
	id, err := h.iq.Enqueue(context.Background(), file, core.Source{
		Kind:  core.SourceFolder,
		Path:  file,
		Title: testTitle,
		Show:  testShow,
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	return id
}

func (h harness) payloadOf(t *testing.T, jobID string) payload {
	t.Helper()
	j, err := h.q.Get(jobID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var pl payload
	if err := j.DecodePayload(&pl); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	return pl
}

// runToEnd drives a job from its queue state to a terminal state, claiming
// and re-claiming as a restarted worker would after jobs.Recover. A
// folder/NAS job's show stage requeues it for its improvement, so the
// target can be claimed twice before it is done, failed or paused; a claim
// that picks up another queued job (an earlier-priority improvement waits)
// is driven to its own terminal state first, like a real worker would.
func (h harness) runToEnd(t *testing.T, jobID string) {
	t.Helper()
	for {
		job, err := h.q.Claim(JobKind)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if job == nil {
			j, gerr := h.q.Get(jobID)
			if gerr != nil {
				t.Fatalf("Get: %v", gerr)
			}
			if j.State != jobs.StateQueued {
				return // done, failed or paused: the run is over
			}
			t.Fatalf("job %s was never claimed", jobID)
		}
		for !h.w.processOne(context.Background(), job.ID) {
		}
		if job.ID == jobID {
			if j, err := h.q.Get(jobID); err == nil && j.State != jobs.StateQueued {
				return // done, failed or paused
			}
			// The show stage requeued it for its improvement: claim again.
		}
	}
}

// runStages runs the next k pipeline stages of jobID, claiming the job for
// its first stage and re-claiming it when the show stage requeues it for
// its improvement, the way Worker.Run claims across the boundary. The job
// must not reach a terminal state before the k stages are up.
func (h harness) runStages(t *testing.T, jobID string, k int) {
	t.Helper()
	claim := func() {
		job, err := h.q.Claim(JobKind)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if job == nil || job.ID != jobID {
			t.Fatalf("claimed %+v, want job %s", job, jobID)
		}
	}
	claim()
	for ran := range k {
		done := h.w.processOne(context.Background(), jobID)
		if !done {
			continue
		}
		j, err := h.q.Get(jobID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		switch j.State {
		case jobs.StateQueued:
			// The show stage finished and requeued the job for its
			// improvement. When more stages remain, claim it again; after
			// the last one the requeue is the kill point, and the job
			// must be left queued for Recover and the next run.
			if ran+1 < k {
				claim()
			}
		default:
			t.Fatalf("job reached %s after %d stages; cannot kill at %s", j.State, ran, stages[k])
		}
	}
}

func sha256File(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// assertIntact verifies the original input was never modified or moved.
func assertIntact(t *testing.T, path, before string) {
	t.Helper()
	if got := sha256File(t, path); got != before {
		t.Errorf("%s changed during ingest", path)
	}
}

// probePath re-probes with the helpers' ffprobe, the same binary the worker
// uses.
func probePath(t *testing.T, helpers, path string) *ProbeInfo {
	t.Helper()
	info, err := ProbeFile(context.Background(), filepath.Join(helpers, "ffprobe"), path)
	if err != nil {
		t.Fatalf("re-probe %s: %v", path, err)
	}
	return info
}

// assertGoodMP4 is the gate check every output must pass: H.264 8-bit
// yuv420p, AAC audio, an MP4 container with moov before mdat.
func assertGoodMP4(t *testing.T, helpers, path string) {
	t.Helper()
	info := probePath(t, helpers, path)
	v := info.Video()
	if v == nil || v.CodecName != "h264" || !eightBitYUV420P(v.PixFmt) {
		t.Errorf("%s: video not h264 8-bit yuv420p: %+v", path, v)
	}
	if a := info.Audio(); len(a) == 0 || a[0].CodecName != "aac" {
		t.Errorf("%s: first audio not aac: %+v", path, a)
	}
	if !info.IsMP4() {
		t.Errorf("%s: not mp4 (format %q)", path, info.FormatName)
	}
	front, err := moovBeforeMdat(path)
	if err != nil || !front {
		t.Errorf("%s: moov not before mdat (%v)", path, err)
	}
}

// assertLibraryClean verifies no temp files are left in the library dir.
func assertLibraryClean(t *testing.T, mediaDir string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(mediaDir, "library"))
	if err != nil {
		t.Fatalf("read library dir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp.") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

// jpegWidth reads the width from a JPEG's SOF marker (the minimal ffprobe
// has no jpeg decoder, so the test parses the header itself).
func jpegWidth(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(b) < 2 || b[0] != 0xFF || b[1] != 0xD8 {
		t.Fatalf("%s is not a jpeg", path)
	}
	for i := 2; i+8 < len(b); {
		if b[i] != 0xFF {
			i++
			continue
		}
		marker := b[i+1]
		if marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			i += 2
			continue
		}
		length := int(b[i+2])<<8 | int(b[i+3])
		switch marker {
		case 0xC0, 0xC1, 0xC2: // SOF0/1/2
			return int(b[i+7])<<8 | int(b[i+8])
		}
		i += 2 + length
	}
	t.Fatalf("%s has no SOF marker", path)
	return 0
}

// suit checks a finished job published exactly one episode and returns it.
func suit(t *testing.T, h harness, jobID string) (core.Episode, *jobs.Job) {
	t.Helper()
	j, err := h.q.Get(jobID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.State != jobs.StateDone {
		t.Fatalf("state = %s, want %s (error %q)", j.State, jobs.StateDone, j.Error)
	}
	if h.pub.Called() != 1 {
		t.Fatalf("published %d times, want once", h.pub.Called())
	}
	return h.pub.Published()[0], j
}

// suitImprove checks a finished folder/NAS job that needed a conversion:
// the show phase published the episode playing its original (Owned false,
// File = the original path) and the improve phase published the converted
// copy under the same episode id (Owned true). It returns the final episode
// and the job.
func suitImprove(t *testing.T, h harness, jobID, original string) (core.Episode, *jobs.Job) {
	t.Helper()
	j, err := h.q.Get(jobID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.State != jobs.StateDone {
		t.Fatalf("state = %s, want %s (error %q)", j.State, jobs.StateDone, j.Error)
	}
	if n := h.pub.Called(); n != 2 {
		t.Fatalf("published %d times, want 2 (show + improve)", n)
	}
	pubs := h.pub.Published()
	show, final := pubs[0], pubs[1]
	if show.Owned {
		t.Errorf("show-phase episode Owned = true, want the original unowned")
	}
	if show.File != original {
		t.Errorf("show-phase File = %q, want the original %q", show.File, original)
	}
	if final.ID != show.ID {
		t.Errorf("improve publish changed the episode id: %q -> %q", show.ID, final.ID)
	}
	if !final.Owned {
		t.Errorf("converted episode Owned = false, want true")
	}
	return final, j
}

func TestPipelineInplace(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	h := newHarness(t, helpers)
	before := sha256File(t, f.keep)
	id := h.enqueue(t, f.keep)
	h.runToEnd(t, id)

	ep, j := suit(t, h, id)
	epID, done, err := h.iq.IngestStatus(context.Background(), id)
	if err != nil || !done || epID == "" {
		t.Fatalf("IngestStatus = (%q, %v, %v), want (id, true, nil)", epID, done, err)
	}
	if ep.ID != epID {
		t.Errorf("episode id %q, want %q", ep.ID, epID)
	}
	if ep.ShowID != showIDFor(testShow) {
		t.Errorf("ShowID = %q", ep.ShowID)
	}
	if ep.Title != testTitle || ep.Source.Show != testShow || ep.Source.Kind != core.SourceFolder {
		t.Errorf("episode metadata wrong: %+v", ep)
	}
	if ep.DurationSec != 4 {
		t.Errorf("DurationSec = %d, want 4", ep.DurationSec)
	}
	// INPLACE: the episode plays from the original's recorded path and no
	// copy exists in the media library directory.
	if ep.File != f.keep {
		t.Errorf("File = %q, want the original %q", ep.File, f.keep)
	}
	if ep.Owned {
		t.Errorf("Owned = true, want false for an in-place original")
	}
	if _, err := os.Stat(filepath.Join(h.mediaDir, "library", string(epID)+".mp4")); !os.IsNotExist(err) {
		t.Errorf("library copy exists for an in-place episode (err %v)", err)
	}
	// Thumbnail: a 480 px wide JPEG, still written next to the episode id.
	if ep.Thumb == "" {
		t.Fatalf("no thumbnail published")
	}
	if _, err := os.Stat(ep.Thumb); err != nil {
		t.Fatalf("thumbnail missing: %v", err)
	}
	if w := jpegWidth(t, ep.Thumb); w != 480 {
		t.Errorf("thumbnail width = %d, want 480", w)
	}
	assertIntact(t, f.keep, before)
	assertLibraryClean(t, h.mediaDir)
	if j.ReservedBytes != 0 {
		t.Errorf("reservation not released: %d", j.ReservedBytes)
	}
}

// TestPipelineInplaceSlowstart runs an MP4 whose moov box sits after mdat:
// faststart is not required for in-place episodes, so the file publishes
// from its original path instead of being remuxed.
func TestPipelineInplaceSlowstart(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	h := newHarness(t, helpers)
	id := h.enqueue(t, f.noFastStart)
	h.runToEnd(t, id)
	ep, _ := suit(t, h, id)
	if ep.File != f.noFastStart {
		t.Fatalf("File = %q, want the slowstart original %q", ep.File, f.noFastStart)
	}
	if ep.Owned {
		t.Errorf("Owned = true, want false for an in-place original")
	}
	if _, err := os.Stat(ep.Thumb); err != nil {
		t.Errorf("thumbnail missing: %v", err)
	}
	// The box walk really is slowstart - this file would be remuxed by the
	// old rules.
	front, err := moovBeforeMdat(f.noFastStart)
	if err != nil || front {
		t.Fatalf("fixture moov front = %v (err %v), want slowstart", front, err)
	}
	assertIntact(t, f.noFastStart, sha256File(t, f.noFastStart))
	assertLibraryClean(t, h.mediaDir)
}

// TestYouTubeDownloadNeverInPlace pins the decision's YouTube carve-out: a
// download that would qualify for in-place serving is still converted into
// the app-owned media folder - the yt-dlp download path is never recorded
// as the episode's file, and the episode is app-owned like every YouTube
// file before the in-place decision.
func TestYouTubeDownloadNeverInPlace(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	h := newHarness(t, helpers)
	id, err := h.iq.Enqueue(context.Background(), f.keep, core.Source{
		Kind:  core.SourceYouTube,
		Path:  "https://www.youtube.com/watch?v=abc123",
		Title: testTitle,
		Show:  testShow,
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	h.runToEnd(t, id)
	ep, _ := suit(t, h, id)
	if ep.File == f.keep {
		t.Fatalf("YouTube download played in place from the download path %q", f.keep)
	}
	if !ep.Owned {
		t.Errorf("YouTube episode Owned = false, want app-owned")
	}
	if _, err := os.Stat(ep.File); err != nil {
		t.Fatalf("converted copy missing: %v", err)
	}
	// Faststart MP4 downloads are byte-copied, exactly as before.
	if got := sha256File(t, ep.File); got != sha256File(t, f.keep) {
		t.Error("YouTube keep output does not match the download bytes")
	}
}

// TestPipelineInplaceWithLeadingCoverArt runs an in-place-eligible MP4 whose
// first listed stream is an attached picture - the layout some phones and
// tools write - end to end: verify must judge the selectStreams H.264
// video, not blindly the first video stream, so the episode passes and
// publishes from the original path.
func TestPipelineInplaceWithLeadingCoverArt(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	h := newHarness(t, helpers)
	id := h.enqueue(t, f.coverFirstMP4)
	h.runToEnd(t, id)
	ep, _ := suit(t, h, id)
	if ep.File != f.coverFirstMP4 {
		t.Fatalf("File = %q, want the original %q", ep.File, f.coverFirstMP4)
	}
	info := probePath(t, helpers, ep.File)
	// The original still leads with the picture; the selected streams are
	// the real H.264 video and the AAC audio.
	if len(info.Streams) < 2 || info.Streams[0].CodecName != "mjpeg" || info.Streams[0].Disposition.AttachedPic != 1 {
		t.Fatalf("in-place file does not lead with the attached picture: %+v", info.Streams)
	}
	vIdx, aIdx := info.selectStreams()
	if vIdx < 0 || info.Streams[vIdx].CodecName != "h264" || !eightBitYUV420P(info.Streams[vIdx].PixFmt) {
		t.Fatalf("selected video = stream %d (%+v), want h264 yuv420p", vIdx, info.Streams)
	}
	if aIdx < 0 || info.Streams[aIdx].CodecName != "aac" {
		t.Fatalf("selected audio = stream %d (%+v), want aac", aIdx, info.Streams)
	}
	if info.Streams[aIdx].Channels != 2 {
		t.Errorf("selected audio channels = %d, want the default stereo track", info.Streams[aIdx].Channels)
	}
	assertIntact(t, f.coverFirstMP4, sha256File(t, f.coverFirstMP4))
	assertLibraryClean(t, h.mediaDir)
}

func TestPipelineRemux(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	for name, in := range map[string]string{
		"mkv": f.remuxMKV,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, helpers)
			before := sha256File(t, in)
			id := h.enqueue(t, in)
			h.runToEnd(t, id)
			ep, _ := suitImprove(t, h, id, in)
			assertGoodMP4(t, helpers, ep.File)
			if _, err := os.Stat(ep.Thumb); err != nil {
				t.Errorf("thumbnail missing: %v", err)
			}
			assertIntact(t, in, before)
			assertLibraryClean(t, h.mediaDir)
		})
	}
}

func TestPipelineAudio(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	for name, in := range map[string]string{
		"ac3": f.ac3MKV,
		"dts": f.dtsMKV,
	} {
		t.Run(name, func(t *testing.T) {
			if in == "" {
				t.Skip("fixture encoder unavailable in " + fixtureFFmpegEnv)
			}
			h := newHarness(t, helpers)
			before := sha256File(t, in)
			id := h.enqueue(t, in)
			h.runToEnd(t, id)
			ep, _ := suitImprove(t, h, id, in)
			assertGoodMP4(t, helpers, ep.File)
			// The video is copied, the audio re-encoded to stereo AAC.
			info := probePath(t, helpers, ep.File)
			if a := info.Audio(); len(a) == 0 || a[0].Channels != 2 {
				t.Errorf("audio channels = %+v, want stereo", a)
			}
			assertIntact(t, in, before)
			assertLibraryClean(t, h.mediaDir)
		})
	}
}

func TestPipelineFull(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	if f.hevcMKV == "" {
		t.Skip("libx265 unavailable in " + fixtureFFmpegEnv)
	}
	h := newHarness(t, helpers)
	before := sha256File(t, f.hevcMKV)
	id := h.enqueue(t, f.hevcMKV)
	h.runToEnd(t, id)
	ep, _ := suitImprove(t, h, id, f.hevcMKV)
	assertGoodMP4(t, helpers, ep.File)
	info := probePath(t, helpers, ep.File)
	v := info.Video()
	if v == nil || v.Width > 1920 || v.Height > 1080 {
		t.Errorf("output size %dx%d exceeds 1920x1080", v.Width, v.Height)
	}
	// Finding 4: no upscaling - a 640x360 input keeps its dimensions.
	if v == nil || v.Width != 640 || v.Height != 360 {
		t.Errorf("output size %dx%d, want the input's 640x360 (no upscaling)", v.Width, v.Height)
	}
	if a := info.Audio(); len(a) == 0 || a[0].Channels != 2 {
		t.Errorf("audio not stereo: %+v", a)
	}
	assertIntact(t, f.hevcMKV, before)
	assertLibraryClean(t, h.mediaDir)
}

func TestPipelineTruncatedFileFailsPlainly(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	h := newHarness(t, helpers)
	before := sha256File(t, f.truncated)
	id := h.enqueue(t, f.truncated)
	h.runToEnd(t, id)
	j, err := h.q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.State != jobs.StateFailed {
		t.Fatalf("state = %s, want %s", j.State, jobs.StateFailed)
	}
	if j.Error != msgDamaged {
		t.Errorf("error = %q, want the plain damaged message", j.Error)
	}
	if h.pub.Called() != 0 {
		t.Errorf("published %d times for a rejected file", h.pub.Called())
	}
	epID, done, err := h.iq.IngestStatus(context.Background(), id)
	if err != nil || !done || epID != "" {
		t.Errorf("IngestStatus = (%q, %v, %v), want (\"\", true, nil)", epID, done, err)
	}
	assertIntact(t, f.truncated, before)
	assertLibraryClean(t, h.mediaDir)
}

// TestMissingHelpersFailJobPlainly: when the app could not set up the
// helper programs (no bundle embedded, or no disk space to extract them) it
// hands the worker a nil helper dir. app.Open promises the jobs then fail as
// they reach the helpers; the worker used to dereference the nil dir in
// stageProbe and panic instead (found installing on a home server,
// 2026-10-27).
func TestMissingHelpersFailJobPlainly(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	q, err := jobs.New(s)
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	pub := &core.FakePublisher{}
	mediaDir := t.TempDir()
	h := harness{iq: NewQueue(q), w: NewWorker(q, nil, pub, mediaDir), pub: pub, q: q, mediaDir: mediaDir}
	src := filepath.Join(t.TempDir(), "episode.mp4")
	if err := os.WriteFile(src, []byte("not probed"), 0o644); err != nil {
		t.Fatal(err)
	}
	id := h.enqueue(t, src)
	h.runToEnd(t, id)
	j, err := h.q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.State != jobs.StateFailed || j.Error != msgNoHelpers {
		t.Fatalf("job = (%s, %q), want (%s, %q)", j.State, j.Error, jobs.StateFailed, msgNoHelpers)
	}
	if h.pub.Called() != 0 {
		t.Errorf("published %d times without helpers", h.pub.Called())
	}
}

// TestKillAndResumeEveryStage enqueues one job per entry point of the
// pipeline - the show phase's probe, plan and show plus the improvement's
// convert, verify, thumbnail and publish - stops the worker after that
// stage, "crashes" (Recover), runs the worker again, and requires the
// result to be identical to an uninterrupted run, with each phase's publish
// exactly once. The remux fixture is a folder original that needs an
// improvement, so the run crosses the show/improve boundary (the show stage
// requeues the job itself); killing between show and improve means the job
// is already queued when Recover runs, and Recover leaves it alone.
func TestKillAndResumeEveryStage(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	for k := range stages {
		t.Run("after_"+stages[k], func(t *testing.T) {
			h := newHarness(t, helpers)
			// Reference: an uninterrupted run of the same file.
			ref := newHarness(t, helpers)
			refID := ref.enqueue(t, f.remuxMKV)
			ref.runToEnd(t, refID)
			if ref.pub.Called() != 2 {
				t.Fatalf("reference published %d times, want 2 (show + improve)", ref.pub.Called())
			}
			refPubs := ref.pub.Published()
			refShow, refFinal := refPubs[0], refPubs[1]
			refSHA := sha256File(t, refFinal.File)

			id := h.enqueue(t, f.remuxMKV)
			h.runStages(t, id, k)
			// "Restart": Recover requeues a job left running; a job the
			// show stage requeued is already queued and untouched.
			if _, err := h.q.Recover(); err != nil {
				t.Fatalf("Recover: %v", err)
			}
			h.runToEnd(t, id)

			j, err := h.q.Get(id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if j.State != jobs.StateDone {
				t.Fatalf("state = %s (error %q)", j.State, j.Error)
			}
			if h.pub.Called() != 2 {
				t.Errorf("published %d times, want exactly 2 (one per phase)", h.pub.Called())
			}
			pubs := h.pub.Published()
			show, final := pubs[0], pubs[1]
			if show.File != refShow.File || show.Owned {
				t.Errorf("show publish = %+v, want %+v (original, unowned)", show, refShow)
			}
			if final.ID != show.ID {
				t.Errorf("improve publish changed the episode id: %q -> %q", show.ID, final.ID)
			}
			if !final.Owned {
				t.Errorf("converted episode Owned = false, want true")
			}
			if got := sha256File(t, final.File); got != refSHA {
				t.Errorf("resumed output differs from the uninterrupted run")
			}
			assertGoodMP4(t, helpers, final.File)
			assertLibraryClean(t, h.mediaDir)
			if j.ReservedBytes != 0 {
				t.Errorf("reservation not released: %d", j.ReservedBytes)
			}
		})
	}
}

// TestDiskFullMidConvertPauses simulates a real ENOSPC: the helper fails
// with ffmpeg's "No space left on device" message, the job pauses with a
// parent-readable reason, the original and the shown episode stay intact
// and no temp file remains; the job then resumes and completes normally.
func TestDiskFullMidConvertPauses(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	h := newHarness(t, helpers)
	h.w.runCmd = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte("ffmpeg: error writing to output: No space left on device\n"), errors.New("exit status 1")
	}
	before := sha256File(t, f.remuxMKV)
	id := h.enqueue(t, f.remuxMKV)
	h.runToEnd(t, id)
	j, err := h.q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.State != jobs.StatePaused {
		t.Fatalf("state = %s, want %s (error %q)", j.State, jobs.StatePaused, j.Error)
	}
	if !strings.Contains(strings.ToLower(j.Error), "disk is full") {
		t.Errorf("pause reason = %q, want the plain disk-full words", j.Error)
	}
	if h.pub.Called() != 1 {
		t.Errorf("published %d times for a paused job, want 1 (the show phase)", h.pub.Called())
	}
	if ep := h.pub.Published()[0]; ep.File != f.remuxMKV || ep.Owned {
		t.Errorf("shown episode = %+v, want the original %q, unowned", ep, f.remuxMKV)
	}
	if j.ReservedBytes != 0 {
		t.Errorf("reservation not released: %d", j.ReservedBytes)
	}
	pl := h.payloadOf(t, id)
	if _, err := os.Stat(filepath.Join(h.mediaDir, "library", string(pl.EpisodeID)+".mp4")); !os.IsNotExist(err) {
		t.Errorf("partial output left for a paused job (err %v)", err)
	}
	assertIntact(t, f.remuxMKV, before)
	assertLibraryClean(t, h.mediaDir)

	// The disk is "fixed"; a parent retry (Advance + Recover) resumes the
	// job with the real runner and it completes.
	h.w.runCmd = runCombined
	if err := h.q.Advance(id, j.Stage, pl); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if _, err := h.q.Recover(); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	h.runToEnd(t, id)
	if j2, err := h.q.Get(id); err != nil || j2.State != jobs.StateDone {
		t.Fatalf("after resume: state = %s (err %v)", j2.State, err)
	}
	suitImprove(t, h, id, f.remuxMKV)
}

// TestNoSpaceBeforeConvertPauses injects a free-space probe reporting zero
// free bytes, so the improvement's pre-convert reservation is denied and
// the job pauses before any conversion. The show phase needs no disk and
// has already published the episode playing its original; only the
// better copy is missing.
func TestNoSpaceBeforeConvertPauses(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	h := newHarness(t, helpers)
	r := disk.NewReservations(h.q).WithFree(func(string) (uint64, error) { return 0, nil }).WithFloor(0)
	h.w.WithReservations(r)
	before := sha256File(t, f.remuxMKV)
	id := h.enqueue(t, f.remuxMKV)
	h.runToEnd(t, id)
	j, err := h.q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.State != jobs.StatePaused {
		t.Fatalf("state = %s, want %s", j.State, jobs.StatePaused)
	}
	if !strings.Contains(strings.ToLower(j.Error), "disk is full") {
		t.Errorf("pause reason = %q", j.Error)
	}
	if h.pub.Called() != 1 {
		t.Errorf("published %d times, want 1 (the show phase)", h.pub.Called())
	}
	// A pause is not a failure: the fix stays pending and nothing is
	// marked failed (the job resumes when space frees up).
	if h.fix.Called() != 0 {
		t.Errorf("fix failed marks = %v, want none for a paused job", h.fix.Failed())
	}
	assertIntact(t, f.remuxMKV, before)
	assertLibraryClean(t, h.mediaDir)
}

// TestRunLoop exercises Worker.Run end to end with two jobs and concurrency
// 1.
func TestRunLoop(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	h := newHarness(t, helpers)
	id1 := h.enqueue(t, f.keep)
	id2 := h.enqueue(t, f.remuxMKV)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		h.w.Run(ctx)
		close(done)
	}()
	deadline := time.After(60 * time.Second)
	for {
		j1, err1 := h.q.Get(id1)
		j2, err2 := h.q.Get(id2)
		if err1 == nil && err2 == nil && j1.State == jobs.StateDone && j2.State == jobs.StateDone {
			break
		}
		select {
		case <-deadline:
			t.Fatal("jobs did not finish in time")
		case <-time.After(50 * time.Millisecond):
		}
	}
	cancel()
	<-done
	if h.pub.Called() != 3 {
		t.Errorf("published %d episodes, want 3 (the in-place job plus the remux job's show and improve)", h.pub.Called())
	}
}

func TestIngestStatusLifecycle(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	h := newHarness(t, helpers)
	id := h.enqueue(t, f.keep)

	epID, done, err := h.iq.IngestStatus(context.Background(), id)
	if err != nil || done || epID != "" {
		t.Errorf("queued job: IngestStatus = (%q, %v, %v), want (\"\", false, nil)", epID, done, err)
	}
	h.runToEnd(t, id)
	epID, done, err = h.iq.IngestStatus(context.Background(), id)
	if err != nil || !done || epID == "" {
		t.Errorf("finished job: IngestStatus = (%q, %v, %v)", epID, done, err)
	}
	pl := h.payloadOf(t, id)
	if epID != pl.EpisodeID {
		t.Errorf("IngestStatus id %q, published payload id %q", epID, pl.EpisodeID)
	}
	// A failed job is finished with no episode.
	bad := h.enqueue(t, f.truncated)
	h.runToEnd(t, bad)
	epID, done, err = h.iq.IngestStatus(context.Background(), bad)
	if err != nil || !done || epID != "" {
		t.Errorf("failed job: IngestStatus = (%q, %v, %v)", epID, done, err)
	}
	// Unknown ids are errors.
	if _, _, err := h.iq.IngestStatus(context.Background(), "no-such-job"); err == nil {
		t.Error("unknown job id accepted")
	}
}

func TestEnqueueValidation(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()
	q, err := jobs.New(s)
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	iq := NewQueue(q)
	ctx := context.Background()
	if _, err := iq.Enqueue(ctx, "", core.Source{Kind: core.SourceFolder, Title: "t", Show: "s"}); err == nil {
		t.Error("empty file accepted")
	}
	if _, err := iq.Enqueue(ctx, "/tmp/x.mp4", core.Source{Kind: core.SourceFolder, Title: "", Show: "s"}); err == nil {
		t.Error("empty title accepted")
	}
	if _, err := iq.Enqueue(ctx, "/tmp/x.mp4", core.Source{Kind: core.SourceFolder, Title: "t", Show: ""}); err == nil {
		t.Error("empty show accepted")
	}
	id, err := iq.Enqueue(ctx, "/tmp/x.mp4", core.Source{Kind: core.SourceFolder, Path: "/tmp/x.mp4", Title: "t", Show: "s"})
	if err != nil {
		t.Fatalf("valid Enqueue: %v", err)
	}
	// The episode id is assigned at enqueue and stable in the payload.
	j, err := q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Kind != JobKind {
		t.Errorf("kind = %q, want %q", j.Kind, JobKind)
	}
	pl := payload{}
	if err := j.DecodePayload(&pl); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if pl.EpisodeID == "" {
		t.Error("episode id not assigned at enqueue")
	}
}

// TestProbeRealFixture runs the real ffprobe over a generated fixture and
// checks the parsed fields and box order.
func TestProbeRealFixture(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	info := probePath(t, helpers, f.keep)
	v := info.Video()
	if v == nil || v.CodecName != "h264" || v.PixFmt != "yuv420p" {
		t.Fatalf("probe = %+v", v)
	}
	if !info.IsMP4() {
		t.Errorf("format %q not mp4", info.FormatName)
	}
	if info.durationSeconds() != 4 {
		t.Errorf("duration = %v, want 4s", info.Duration)
	}
	front, err := moovBeforeMdat(f.keep)
	if err != nil || !front {
		t.Errorf("fixture not faststart: %v", err)
	}
	if err := ensureProbeFails(t, helpers, f.truncated); err != nil {
		t.Errorf("truncated fixture did not fail the probe: %v", err)
	}
}

func ensureProbeFails(t *testing.T, helpers, path string) error {
	t.Helper()
	_, err := ProbeFile(context.Background(), filepath.Join(helpers, "ffprobe"), path)
	if err == nil {
		return fmt.Errorf("probe succeeded on %s", path)
	}
	return nil
}

// TestTruncatedFaststartShownInPlace feeds the pipeline a faststart MP4
// truncated to 60% of its bytes. ffprobe reads only the index (moov, at the
// front) and still reports the full duration, so the file is healthy enough
// for the show phase: the folder source is probed with ffprobe alone (no
// box walk, no through-the-end decode of the original, exactly like the
// legacy app that served every file), so it publishes playing its original
// and the job is done - the deep media checks are reserved for converted
// outputs.
func TestTruncatedFaststartShownInPlace(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	h := newHarness(t, helpers)
	before := sha256File(t, f.truncatedFaststart)
	id := h.enqueue(t, f.truncatedFaststart)
	h.runToEnd(t, id)
	ep, _ := suit(t, h, id)
	if ep.File != f.truncatedFaststart {
		t.Fatalf("File = %q, want the truncated original %q", ep.File, f.truncatedFaststart)
	}
	if ep.Owned {
		t.Errorf("Owned = true, want false (the original is served in place)")
	}
	assertIntact(t, f.truncatedFaststart, before)
	assertLibraryClean(t, h.mediaDir)
}

// TestPipelineCorruptedMdatShownInPlace takes the in-place-eligible fixture
// and zeroes 4096 bytes mid-mdat: the moov index, the box tree and the
// packet headers survive, so ffprobe still reports a healthy file, but the
// frame payloads no longer decode fully. The show phase does not decode the
// original (the legacy app never did), so the episode still publishes
// playing its original bytes - the file plays until the corruption, exactly
// as it did before kids-shelf existed. The through-the-end decode check
// still guards every converted output (see the improvement tests).
func TestPipelineCorruptedMdatShownInPlace(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	b, err := os.ReadFile(f.keep)
	if err != nil {
		t.Fatalf("read KEEP fixture: %v", err)
	}
	boxes := walkBoxes(b, 0, len(b))
	var mdat *isoBox
	for i := range boxes {
		if boxes[i].typ == "mdat" {
			mdat = &boxes[i]
			break
		}
	}
	if mdat == nil {
		t.Fatal("KEEP fixture has no mdat box")
	}
	mid := mdat.start + mdat.size/2
	for i := mid; i < mid+4096; i++ {
		b[i] = 0
	}
	corrupt := filepath.Join(filepath.Dir(f.keep), "corrupt-mdat.mp4")
	if err := os.WriteFile(corrupt, b, 0o644); err != nil {
		t.Fatalf("write corrupt fixture: %v", err)
	}
	h := newHarness(t, helpers)
	before := sha256File(t, corrupt)
	id := h.enqueue(t, corrupt)
	h.runToEnd(t, id)
	ep, _ := suit(t, h, id)
	if ep.File != corrupt {
		t.Fatalf("File = %q, want the corrupted original %q", ep.File, corrupt)
	}
	if ep.Owned {
		t.Errorf("Owned = true, want false (the original is served in place)")
	}
	assertIntact(t, corrupt, before)
	assertLibraryClean(t, h.mediaDir)
}

// TestEnqueueIdempotentPerSource pins the core contract: a second Enqueue
// for the same (kind, path) returns the existing job - queued, running,
// paused or done; only a failed job is replaced by a new one.
func TestEnqueueIdempotentPerSource(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	h := newHarness(t, helpers)
	ctx := context.Background()
	src := core.Source{Kind: core.SourceFolder, Path: f.keep, Title: testTitle, Show: testShow}
	id1, err := h.iq.Enqueue(ctx, f.keep, src)
	if err != nil {
		t.Fatalf("first Enqueue: %v", err)
	}
	id2, err := h.iq.Enqueue(ctx, f.keep, src)
	if err != nil {
		t.Fatalf("second Enqueue: %v", err)
	}
	if id2 != id1 {
		t.Errorf("second Enqueue = %s, want %s (one job per source)", id2, id1)
	}
	all, err := h.q.List(JobKind)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("enqueued %d jobs for one source, want 1", len(all))
	}
	// A different file is a different job.
	other := h.enqueue(t, f.remuxMKV)
	if other == id1 {
		t.Error("different file reused the same job")
	}
	// The same file with a different kind is a different job too.
	nasSrc := src
	nasSrc.Kind = core.SourceNAS
	nasID, err := h.iq.Enqueue(ctx, f.keep, nasSrc)
	if err != nil {
		t.Fatalf("Enqueue NAS kind: %v", err)
	}
	if nasID == id1 {
		t.Error("different source kind reused the same job")
	}
	// A done job still returns its own id.
	h.runToEnd(t, id1)
	id1b, err := h.iq.Enqueue(ctx, f.keep, src)
	if err != nil {
		t.Fatalf("Enqueue after done: %v", err)
	}
	if id1b != id1 {
		t.Errorf("done job not reused: %s != %s", id1b, id1)
	}
	// Drain the remaining queued jobs (a worker claims in enqueue order) so
	// the failed-file run below is the only job left when it claims.
	h.runToEnd(t, other)
	h.runToEnd(t, nasID)
	// A failed job is replaced, not returned.
	badSrc := core.Source{Kind: core.SourceFolder, Path: f.truncated, Title: testTitle, Show: testShow}
	bad, err := h.iq.Enqueue(ctx, f.truncated, badSrc)
	if err != nil {
		t.Fatalf("Enqueue bad: %v", err)
	}
	h.runToEnd(t, bad)
	retry, err := h.iq.Enqueue(ctx, f.truncated, badSrc)
	if err != nil {
		t.Fatalf("Enqueue retry: %v", err)
	}
	if retry == bad {
		t.Errorf("failed job was returned; the contract replaces it with a new one")
	}
	j, err := h.q.Get(retry)
	if err != nil {
		t.Fatalf("Get retry: %v", err)
	}
	if j.State != jobs.StateQueued {
		t.Errorf("retry job state = %s, want queued", j.State)
	}
}

// TestEnqueueConcurrentSameSource races 20 producers enqueuing the same
// source and requires every one to get the same job id, with exactly one
// job row left: the same-source lookup and the insert are atomic, so
// concurrent producers cannot both miss and create two jobs.
func TestEnqueueConcurrentSameSource(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()
	q, err := jobs.New(s)
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	iq := NewQueue(q)
	path := filepath.Join(t.TempDir(), "same.mp4")
	src := core.Source{Kind: core.SourceFolder, Path: path, Title: testTitle, Show: testShow}
	ctx := context.Background()
	const n = 20
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			ids[i], errs[i] = iq.Enqueue(ctx, path, src)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}
	for i := 1; i < n; i++ {
		if ids[i] != ids[0] {
			t.Fatalf("Enqueue %d = %s, want %s (one job per source)", i, ids[i], ids[0])
		}
	}
	all, err := q.List(JobKind)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("enqueued %d jobs for one source, want 1", len(all))
	}
}

// TestPipelineStreamSelection drives the stream-selection fixture set end to
// end on converting plans (both fixtures are MKVs): attached artwork and
// extra tracks never reach the output, and the chosen audio is the
// default-disposition track, falling back to the first. In-place files keep
// every original stream, so selection there is pinned by
// TestPipelineInplaceWithLeadingCoverArt.
func TestPipelineStreamSelection(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	tests := []struct {
		name string
		in   string
		want int // channels of the chosen audio
	}{
		{"default-audio-track", f.secondAudioMKV, 1},     // flagged default -> mono
		{"first-audio-fallback", f.noDefaultAudioMKV, 2}, // no flag -> first, stereo
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.in == "" {
				t.Skip("fixture encoder unavailable in " + fixtureFFmpegEnv)
			}
			h := newHarness(t, helpers)
			before := sha256File(t, tt.in)
			id := h.enqueue(t, tt.in)
			h.runToEnd(t, id)
			ep, _ := suitImprove(t, h, id, tt.in)
			assertGoodMP4(t, helpers, ep.File)
			info := probePath(t, helpers, ep.File)
			vids, auds := 0, 0
			for _, s := range info.Streams {
				switch s.CodecType {
				case "video":
					vids++
				case "audio":
					auds++
				}
			}
			if vids != 1 {
				t.Errorf("video streams in output = %d, want 1 (artwork and extra tracks dropped)", vids)
			}
			if auds != 1 {
				t.Errorf("audio streams in output = %d, want 1", auds)
			}
			a := info.Audio()
			if len(a) != 1 {
				t.Fatalf("output audio = %+v, want exactly one track", a)
			}
			if a[0].Channels != tt.want {
				t.Errorf("chosen audio channels = %d, want %d", a[0].Channels, tt.want)
			}
			assertIntact(t, tt.in, before)
			assertLibraryClean(t, h.mediaDir)
		})
	}
}

// TestRotatedVideoPublishesPortrait runs a phone-style file - a 1920x1080
// MPEG-4 video carrying a 90° display matrix, which displays as 1080x1920 -
// end to end: the full re-encode must clamp the display to the portrait
// cap, baking the rotation into physically portrait pixels instead of
// keeping the matrix, and publish.
func TestRotatedVideoPublishesPortrait(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	h := newHarness(t, helpers)
	before := sha256File(t, f.rotatedMP4)
	id := h.enqueue(t, f.rotatedMP4)
	h.runToEnd(t, id)
	ep, _ := suitImprove(t, h, id, f.rotatedMP4)
	v := probePath(t, helpers, ep.File).Video()
	if v == nil {
		t.Fatal("output has no video stream")
	}
	if v.Width != 1080 || v.Height != 1920 {
		t.Errorf("output video = %dx%d, want the rotated display clamped to 1080x1920", v.Width, v.Height)
	}
	if r := v.rotation(); r != 0 {
		t.Errorf("output keeps rotation %d°, want it baked into the re-encode", r)
	}
	assertGoodMP4(t, helpers, ep.File)
	assertIntact(t, f.rotatedMP4, before)
	assertLibraryClean(t, h.mediaDir)
}

// TestConvertMapsChosenStreams records the ffmpeg command line per action
// and pins the explicit -map of the plan's streams, the encoder rate caps
// and the no-upscale rule for inputs at 1920x1080 or below.
func TestConvertMapsChosenStreams(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	h := newHarness(t, helpers)
	file := filepath.Join(t.TempDir(), "in.mp4")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write input: %v", err)
	}
	id := h.enqueue(t, file)
	pl := h.payloadOf(t, id)
	pl.File = file
	pl.Probe = &ProbeInfo{Duration: 4, Streams: []StreamInfo{
		{CodecType: "video", CodecName: "h264", PixFmt: "yuv420p", Width: 640, Height: 360},
		{CodecType: "video", CodecName: "mjpeg", Disposition: Disposition{AttachedPic: 1}},
		{CodecType: "audio", CodecName: "aac", Profile: "LC", Channels: 2},
		{CodecType: "audio", CodecName: "aac", Profile: "LC", Channels: 1, Disposition: Disposition{Default: 1}},
	}}
	// The plan's choices: the real video (not the artwork) and the flagged
	// default audio (the mono second track).
	pl.VideoIndex, pl.AudioIndex = 0, 3
	var recorded [][]string
	h.w.runCmd = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		recorded = append(recorded, args)
		return nil, nil
	}
	wantMap := func(t *testing.T, action Action, tail ...string) {
		t.Helper()
		pl.Action = action
		next, err := h.w.stageConvert(context.Background(), id, &pl)
		// The injected runner writes no file, so the rename inside
		// stageConvert fails - expected; only the recorded command line
		// matters here.
		if next != "" || err == nil {
			t.Fatalf("stageConvert(%s) = (%q, %v), want the injected-runner failure", action, next, err)
		}
		args := recorded[len(recorded)-1]
		if got := arg(args, "-map"); len(got) != 2 || got[0] != "0:0" || got[1] != "0:3" {
			t.Errorf("%s maps %v, want [0:0 0:3]", action, got)
		}
		for i := 0; i < len(tail); i += 2 {
			if got := arg(args, tail[i]); len(got) != 1 || got[0] != tail[i+1] {
				t.Errorf("%s: %s = %v, want [%s]", action, tail[i], got, tail[i+1])
			}
		}
	}
	wantMap(t, ActionRemux, "-c", "copy")
	wantMap(t, ActionAudio, "-c:v", "copy", "-c:a", "aac")
	wantMap(t, ActionFull,
		"-c:v", "libx264", "-maxrate", "8M", "-bufsize", "16M",
		"-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2")

	// A 4K input does downscale; the expression still rounds to even.
	pl.Probe.Streams[0].Width, pl.Probe.Streams[0].Height = 3840, 2160
	wantMap(t, ActionFull,
		"-vf", "scale=1920:1080:force_original_aspect_ratio=decrease,scale=trunc(iw/2)*2:trunc(ih/2)*2")

	// An audio-less file maps the video alone.
	pl.Probe.Streams[0].Width, pl.Probe.Streams[0].Height = 640, 360
	pl.AudioIndex = -1
	pl.Action = ActionRemux
	if next, err := h.w.stageConvert(context.Background(), id, &pl); next != "" || err == nil {
		t.Fatalf("stageConvert no-audio = (%q, %v), want the injected-runner failure", next, err)
	}
	args := recorded[len(recorded)-1]
	if got := arg(args, "-map"); len(got) != 1 || got[0] != "0:0" {
		t.Errorf("no-audio remux maps %v, want [0:0]", got)
	}
}

// arg returns every value of the given option in an ffmpeg command line.
func arg(args []string, key string) []string {
	var got []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == key {
			got = append(got, args[i+1])
		}
	}
	return got
}

// TestScaleFilterRotation pins the scale box per display size: the coded
// dimensions turned by the stream's rotation metadata (Display Matrix side
// data or the legacy rotate tag), with portrait displays clamped to
// 1080x1920 and landscape to 1920x1080, never upscaling.
func TestScaleFilterRotation(t *testing.T) {
	even := "scale=trunc(iw/2)*2:trunc(ih/2)*2"
	landscape := "scale=1920:1080:force_original_aspect_ratio=decrease," + even
	portrait := "scale=1080:1920:force_original_aspect_ratio=decrease," + even
	vid := func(w, h int) StreamInfo {
		return StreamInfo{CodecType: "video", Width: w, Height: h}
	}
	matrix := func(s StreamInfo, r int) StreamInfo {
		s.SideData = []SideData{{Type: "Display Matrix", Rotation: r}}
		return s
	}
	rotateTag := func(s StreamInfo, r string) StreamInfo {
		s.Tags = map[string]string{"rotate": r}
		return s
	}
	tests := []struct {
		name string
		p    *ProbeInfo
		want string
	}{
		{"no-video", &ProbeInfo{}, even},
		{"landscape-1080p", &ProbeInfo{Streams: []StreamInfo{vid(1920, 1080)}}, even},
		{"landscape-4k", &ProbeInfo{Streams: []StreamInfo{vid(3840, 2160)}}, landscape},
		{"portrait-coded", &ProbeInfo{Streams: []StreamInfo{vid(1080, 1920)}}, even},
		{"rotated-1080p-displays-portrait", &ProbeInfo{Streams: []StreamInfo{matrix(vid(1920, 1080), 90)}}, even},
		{"rotated-4k", &ProbeInfo{Streams: []StreamInfo{matrix(vid(3840, 2160), 90)}}, portrait},
		{"rotated-270", &ProbeInfo{Streams: []StreamInfo{matrix(vid(3840, 2160), 270)}}, portrait},
		{"counter-clockwise-matrix", &ProbeInfo{Streams: []StreamInfo{matrix(vid(3840, 2160), -90)}}, portrait},
		{"rotate-tag", &ProbeInfo{Streams: []StreamInfo{rotateTag(vid(3840, 2160), "90")}}, portrait},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scaleFilter(tt.p); got != tt.want {
				t.Errorf("scaleFilter = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestReserveBound pins the worst-case reservation: keep and remux carry the
// input size, audio the source video bytes plus the audio cap, full the
// encoder-capped duration bound.
func TestReserveBound(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	h := newHarness(t, helpers)
	file := filepath.Join(t.TempDir(), "in.mp4")
	if err := os.WriteFile(file, make([]byte, 1000), 0o644); err != nil {
		t.Fatalf("write input: %v", err)
	}
	id := h.enqueue(t, file)
	defer h.w.resv.Release(id)
	pl := h.payloadOf(t, id)
	pl.File = file
	reserved := func(t *testing.T) uint64 {
		t.Helper()
		if err := h.w.reserve(id, &pl); err != nil {
			t.Fatalf("reserve: %v", err)
		}
		j, err := h.q.Get(id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		return j.ReservedBytes
	}
	pl.Action = ActionKeep
	if got := reserved(t); got != 1000 {
		t.Errorf("keep reserves %d, want the input size 1000", got)
	}
	pl.Action = ActionRemux
	if got := reserved(t); got != 1000 {
		t.Errorf("remux reserves %d, want the input size 1000", got)
	}
	pl.Action = ActionAudio
	pl.DurationSec = 500
	// The video is copied uncapped, so without a probe the bound falls back
	// to the input size (1000) plus the 160 kbit/s audio re-encode.
	if got := reserved(t); got != 63_429_900 {
		t.Errorf("audio reserves %d, want input + 160 kbit/s bound 63429900", got)
	}
	// With a probe, the video share is the stream bit_rate over the duration.
	pl.Probe = &ProbeInfo{Duration: 10, Streams: []StreamInfo{{CodecType: "video", BitRate: 20_000_000}}}
	pl.DurationSec = 10
	if got := reserved(t); got != 80_148_800 {
		t.Errorf("audio reserves %d, want the probe bit-rate bound 80148800", got)
	}
	pl.Probe = nil
	pl.DurationSec = 500
	pl.Action = ActionFull
	if got := reserved(t); got != 613_428_800 {
		t.Errorf("full reserves %d, want the capped-duration bound 613428800", got)
	}
}

// TestReservedBytesFormula checks the duration math directly, including the
// probe duration (with its fraction) winning over the rounded seconds.
func TestReservedBytesFormula(t *testing.T) {
	pl := &payload{Action: ActionFull, DurationSec: 500}
	if got := reservedBytes(pl); got != 613_428_800 {
		t.Errorf("500 s: reservedBytes = %d, want 613428800", got)
	}
	pl.Probe = &ProbeInfo{Duration: 4.032}
	if got := reservedBytes(pl); got != 56_952_704 {
		t.Errorf("4.032 s: reservedBytes = %d, want 56952704", got)
	}
	// An AUDIO job copies the video uncapped: without a probe the video
	// share is the input size, with one it is the stream bit_rate over the
	// duration; the audio share is always 160 kbit/s over the duration.
	pl.Probe = nil
	if got := audioReservedBytes(pl, 1000); got != 63_429_900 {
		t.Errorf("audio 500 s, input fallback: audioReservedBytes = %d, want 63429900", got)
	}
	pl.Probe = &ProbeInfo{Duration: 10, Streams: []StreamInfo{{CodecType: "video", BitRate: 20_000_000}}}
	if got := audioReservedBytes(pl, 1000); got != 80_148_800 {
		t.Errorf("audio 10 s at 20 Mbit/s: audioReservedBytes = %d, want 80148800", got)
	}
}

// TestCopyFileHonorsContext checks that a cancelled context stops the byte
// copy and removes the partial destination - both when the cancellation is
// known up front and when it lands mid-copy.
func TestCopyFileHonorsContext(t *testing.T) {
	t.Run("pre-cancelled", func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "src")
		dst := filepath.Join(dir, "dst")
		if err := os.WriteFile(src, []byte("some bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := copyFile(ctx, src, dst); err == nil {
			t.Fatal("copy succeeded despite the cancelled context")
		}
		if _, err := os.Stat(dst); !os.IsNotExist(err) {
			t.Errorf("destination left behind: %v", err)
		}
	})
	t.Run("mid-copy", func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "src")
		dst := filepath.Join(dir, "dst")
		// A 512 MiB sparse source: instant to create, slow enough that the
		// cancellation lands while the copy is still running.
		if err := os.WriteFile(src, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(src, 512<<20); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- copyFile(ctx, src, dst) }()
		deadline := time.After(30 * time.Second)
		for {
			if _, err := os.Stat(dst); err == nil {
				break
			}
			select {
			case <-deadline:
				t.Fatal("copy never started")
			case <-time.After(time.Millisecond):
			}
		}
		cancel()
		if err := <-done; err == nil {
			t.Fatal("copy finished despite the mid-copy cancellation")
		}
		if _, err := os.Stat(dst); !os.IsNotExist(err) {
			t.Errorf("partial destination left behind: %v", err)
		}
	})
}

// TestCancelDuringConvertLeavesJobRunning stops the worker's context at the
// improvement's convert stage of a remux job: the show phase has already
// published the episode playing its original, and the job must stay running
// at convert (nothing failed, no temp) for jobs.Recover; after the restart
// it completes and publishes the converted copy. An in-place job has no
// copy to cancel, so the stop behavior is pinned on real convert work.
func TestCancelDuringConvertLeavesJobRunning(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	h := newHarness(t, helpers)
	id := h.enqueue(t, f.remuxMKV)
	job, err := h.q.Claim(JobKind)
	if err != nil || job == nil || job.ID != id {
		t.Fatalf("Claim = %+v, %v", job, err)
	}
	for i := range 2 { // probe, plan
		if done := h.w.processOne(context.Background(), id); done {
			t.Fatalf("job finished after %d stages", i)
		}
	}
	// The show stage publishes the original and requeues the job for its
	// improvement; claim it again, now sitting at convert.
	if done := h.w.processOne(context.Background(), id); done {
		j, gerr := h.q.Get(id)
		if gerr != nil {
			t.Fatalf("Get: %v", gerr)
		}
		if j.State != jobs.StateQueued || j.Stage != StageConvert {
			t.Fatalf("after show: state=%s stage=%s, want queued at convert", j.State, j.Stage)
		}
	}
	job, err = h.q.Claim(JobKind)
	if err != nil || job == nil || job.ID != id {
		t.Fatalf("re-claim = %+v, %v", job, err)
	}
	if job.Stage != StageConvert || !h.payloadOf(t, id).Shown {
		t.Fatalf("improvement job = %+v, want convert after the show phase", job)
	}
	// A quit, a kill or a power cut: the context dies and the worker stops
	// mid-convert. The run must stop (leave the running job for Recover),
	// not fail the file.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.w.processOne(ctx, id)
	j, err := h.q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.State != jobs.StateRunning {
		t.Fatalf("state = %s, want running (left for Recover)", j.State)
	}
	if j.Stage != StageConvert {
		t.Errorf("stage = %s, want convert", j.Stage)
	}
	assertLibraryClean(t, h.mediaDir)
	if h.pub.Called() != 1 {
		t.Errorf("published %d times before the improve phase finished, want 1 (the show publish)", h.pub.Called())
	}
	// The next launch recovers the running job and it completes normally.
	n, err := h.q.Recover()
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n != 1 {
		t.Fatalf("Recover requeued %d jobs, want 1", n)
	}
	h.runToEnd(t, id)
	suitImprove(t, h, id, f.remuxMKV)
}

func TestLastOutTimeUS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "progress.log")
	if err := os.WriteFile(path,
		[]byte("frame=100\nout_time_us=1000\ntotal_size=5\nout_time_us=8000000\nprogress=end\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := lastOutTimeUS(path)
	if !ok {
		t.Fatal("no out_time_us found")
	}
	if got != 8_000_000 {
		t.Errorf("lastOutTimeUS = %d, want 8000000", got)
	}
}

// TestShowThenImprove pins the show-first contract end to end on a folder
// video that needs a full re-encode: after the show phase the episode is on
// the shelf playing its original (Owned false, File = the real path, no
// conversion run yet), and after the improve phase the same episode id
// points at the converted, app-owned copy.
func TestShowThenImprove(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	h := newHarness(t, helpers)
	before := sha256File(t, f.rotatedMP4)
	id := h.enqueue(t, f.rotatedMP4)
	// Show phase only: probe, plan, show.
	h.runStages(t, id, 3)
	if h.pub.Called() != 1 {
		t.Fatalf("after the show phase, published %d times, want 1", h.pub.Called())
	}
	ep := h.pub.Published()[0]
	if ep.Owned {
		t.Errorf("show-phase episode Owned = true, want false")
	}
	if ep.File != f.rotatedMP4 {
		t.Errorf("show-phase File = %q, want the original %q", ep.File, f.rotatedMP4)
	}
	if ep.DurationSec != 4 {
		t.Errorf("show-phase DurationSec = %d, want 4 (from the probe)", ep.DurationSec)
	}
	pl := h.payloadOf(t, id)
	if _, err := os.Stat(filepath.Join(h.mediaDir, "library", string(pl.EpisodeID)+".mp4")); !os.IsNotExist(err) {
		t.Errorf("conversion ran during the show phase (err %v)", err)
	}
	// The improve phase swaps the same episode id to the converted copy.
	h.runToEnd(t, id)
	final, j := suitImprove(t, h, id, f.rotatedMP4)
	if final.ID != ep.ID {
		t.Fatalf("improve publish used episode %q, want %q", final.ID, ep.ID)
	}
	if !final.Owned {
		t.Error("converted episode Owned = false, want true")
	}
	if want := filepath.Join(h.mediaDir, "library", string(ep.ID)+".mp4"); final.File != want {
		t.Errorf("converted File = %q, want %q", final.File, want)
	}
	assertGoodMP4(t, helpers, final.File)
	assertIntact(t, f.rotatedMP4, before)
	assertLibraryClean(t, h.mediaDir)
	if j.ReservedBytes != 0 {
		t.Errorf("reservation not released: %d", j.ReservedBytes)
	}
}

// TestFailedImprovementKeepsEpisode: when the improvement fails - convert or
// verify - the episode stays published playing its original, is never
// unpublished, and the job fails with the keep-as-is words.
func TestFailedImprovementKeepsEpisode(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	for _, tc := range []struct {
		name     string
		run      func(ctx context.Context, name string, args ...string) ([]byte, error)
		noOutput bool // the conversion itself failed, so no final copy exists
	}{
		{"convert", func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return []byte("ffmpeg: invalid argument\n"), errors.New("exit status 1")
		}, true},
		{"verify", func(ctx context.Context, name string, args ...string) ([]byte, error) {
			// Fail only the through-the-end decode pass, which carries
			// -xerror; the show thumbnail and the remux itself succeed.
			for _, a := range args {
				if a == "-xerror" {
					return nil, errors.New("exit status 1")
				}
			}
			return runCombined(ctx, name, args...)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, helpers)
			h.w.runCmd = tc.run
			before := sha256File(t, f.remuxMKV)
			id := h.enqueue(t, f.remuxMKV)
			h.runToEnd(t, id)
			j, err := h.q.Get(id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if j.State != jobs.StateFailed {
				t.Fatalf("state = %s, want %s", j.State, jobs.StateFailed)
			}
			if j.Error != msgKeepAsIs {
				t.Errorf("error = %q, want the keep-as-is words %q", j.Error, msgKeepAsIs)
			}
			// The episode stays published with the original, unowned.
			if h.pub.Called() != 1 {
				t.Fatalf("published %d times, want exactly the one show publish", h.pub.Called())
			}
			if ep := h.pub.Published()[0]; ep.File != f.remuxMKV || ep.Owned {
				t.Errorf("episode after the failed improvement = %+v, want the original %q, unowned", ep, f.remuxMKV)
			}
			// The show publish carried the pending fix status, and the
			// failure marked it failed with the issue kept.
			if ep := h.pub.Published()[0]; ep.Issue != core.IssueFormat || ep.Fix != core.FixPending {
				t.Errorf("show publish fix status = %q/%q, want format/pending", ep.Issue, ep.Fix)
			}
			if got := h.fix.Failed(); len(got) != 1 || got[0] != h.payloadOf(t, id).EpisodeID {
				t.Errorf("fix failed marks = %v, want exactly [%s]", got, h.payloadOf(t, id).EpisodeID)
			}
			if tc.noOutput {
				if _, err := os.Stat(filepath.Join(h.mediaDir, "library", string(h.payloadOf(t, id).EpisodeID)+".mp4")); !os.IsNotExist(err) {
					t.Errorf("converted output left for a failed conversion (err %v)", err)
				}
			}
			assertIntact(t, f.remuxMKV, before)
			assertLibraryClean(t, h.mediaDir)
			// A retry replaces the failed job, exactly like today.
			if _, err := h.q.Recover(); err != nil {
				t.Fatalf("Recover: %v", err)
			}
		})
	}
}

// TestClaimShowsBeforeImprovements pins the claim order a folder added next
// week must see: show jobs (priority 0) claim before an older
// already-shown job's improvement, no matter when either was enqueued.
func TestClaimShowsBeforeImprovements(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	h := newHarness(t, helpers)
	// B is an older job whose show phase already ran: it sits queued at
	// convert, waiting for its (remux) improvement.
	b := h.enqueue(t, f.remuxMKV)
	h.runStages(t, b, 3) // probe, plan, show: requeued for its remux improvement
	if j, _ := h.q.Get(b); j.Priority != fixPriority(ActionRemux, h.payloadOf(t, b).DurationSec) {
		t.Fatalf("B priority = %d, want the fix-lane remux priority %d", j.Priority, fixPriority(ActionRemux, h.payloadOf(t, b).DurationSec))
	}
	// A and C are fresh show jobs enqueued after B. They must be distinct
	// files: Enqueue is idempotent per source, so three jobs enqueued for
	// one path would be one job.
	aFile := filepath.Join(t.TempDir(), "a.mkv")
	cFile := filepath.Join(t.TempDir(), "c.mkv")
	for _, dst := range []string{aFile, cFile} {
		if b, err := os.ReadFile(f.remuxMKV); err != nil {
			t.Fatalf("read remux fixture: %v", err)
		} else if err := os.WriteFile(dst, b, 0o644); err != nil {
			t.Fatalf("write remux copy: %v", err)
		}
	}
	a := h.enqueue(t, aFile)
	c := h.enqueue(t, cFile)
	for i, want := range []string{a, c, b} {
		job, err := h.q.Claim(JobKind)
		if err != nil {
			t.Fatalf("Claim %d: %v", i, err)
		}
		if job == nil || job.ID != want {
			t.Fatalf("Claim %d = %+v, want %s (show jobs claim before older improvements)", i, job, want)
		}
	}
}

// TestImprovementPriorityOrder pins the ordering among improvements: audio
// re-encodes first, then full re-encodes, then remuxes: each requeued
// improvement carries its fix-lane priority (class*1_000_000 + duration).
func TestImprovementPriorityOrder(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	for _, tc := range []struct {
		name       string
		in         string
		wantAction Action
	}{
		{"audio", f.ac3MKV, ActionAudio},
		{"full", f.rotatedMP4, ActionFull},
		{"remux", f.remuxMKV, ActionRemux},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, helpers)
			id := h.enqueue(t, tc.in)
			h.runStages(t, id, 3) // probe, plan, show: requeued for its improvement
			j, err := h.q.Get(id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if j.State != jobs.StateQueued || j.Stage != StageConvert {
				t.Fatalf("after show: state=%s stage=%s, want queued at convert", j.State, j.Stage)
			}
			want := fixPriority(tc.wantAction, h.payloadOf(t, id).DurationSec)
			if j.Priority != want {
				t.Errorf("priority = %d, want %d for %s", j.Priority, want, tc.wantAction)
			}
		})
	}
}

// TestSafariSettingRemux decides the remux improvement by the household
// setting: off, an MKV with H.264+AAC is published in place and the job is
// done with no conversion; on, it is published in place first and remuxed
// later, with the remux improvement priority.
func TestSafariSettingRemux(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	t.Run("off", func(t *testing.T) {
		h := newHarness(t, helpers)
		h.w.WithSafariSetting(func(context.Context) (bool, error) { return false, nil })
		before := sha256File(t, f.remuxMKV)
		id := h.enqueue(t, f.remuxMKV)
		h.runToEnd(t, id)
		ep, j := suit(t, h, id) // exactly one publish: final in place
		if ep.File != f.remuxMKV {
			t.Errorf("File = %q, want the original %q (published in place)", ep.File, f.remuxMKV)
		}
		if ep.Owned {
			t.Error("Owned = true, want false")
		}
		pl := h.payloadOf(t, id)
		if _, err := os.Stat(filepath.Join(h.mediaDir, "library", string(pl.EpisodeID)+".mp4")); !os.IsNotExist(err) {
			t.Errorf("conversion ran with the Safari setting off (err %v)", err)
		}
		if j.Priority != 0 {
			t.Errorf("priority = %d, want 0 (no improvement job)", j.Priority)
		}
		if j.Attempts != 0 {
			t.Errorf("attempts = %d, want 0 (never requeued)", j.Attempts)
		}
		assertIntact(t, f.remuxMKV, before)
		assertLibraryClean(t, h.mediaDir)
	})
	t.Run("on", func(t *testing.T) {
		h := newHarness(t, helpers)
		id := h.enqueue(t, f.remuxMKV)
		h.runStages(t, id, 3)
		if j, _ := h.q.Get(id); j.Priority != fixPriority(ActionRemux, h.payloadOf(t, id).DurationSec) {
			t.Fatalf("improvement priority = %d, want the fix-lane remux priority", j.Priority)
		}
		h.runToEnd(t, id)
		suitImprove(t, h, id, f.remuxMKV)
	})
}

// TestRecoveredImprovementNeverShownShowsFirst simulates a job from before
// the show-first change: it was running at its (old) convert stage with a
// payload that carries no shown marker. After Recover it must do the show
// phase first (publish the original) and only then convert.
func TestRecoveredImprovementNeverShownShowsFirst(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	h := newHarness(t, helpers)
	id := h.enqueue(t, f.remuxMKV)
	job, err := h.q.Claim(JobKind)
	if err != nil || job == nil || job.ID != id {
		t.Fatalf("Claim = %+v, %v", job, err)
	}
	for i := range 2 { // probe, plan
		if done := h.w.processOne(context.Background(), id); done {
			t.Fatalf("job finished after %d stages", i)
		}
	}
	// Rewind to the old convert stage, as a job killed before the
	// show-first change would look: Action planned, nothing shown.
	if err := h.q.Advance(id, StageConvert, h.payloadOf(t, id)); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if _, err := h.q.Recover(); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	h.runToEnd(t, id)
	suitImprove(t, h, id, f.remuxMKV)
}

// TestYouTubeRecoveredAfterPublishPublishesOnce pins the legacy skip in
// stagePublish: a YouTube job recovered at the publish stage with the old
// Published marker must finish without publishing a second time - the
// YouTube path stays download -> convert -> publish once.
func TestYouTubeRecoveredAfterPublishPublishesOnce(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	h := newHarness(t, helpers)
	id, err := h.iq.Enqueue(context.Background(), f.keep, core.Source{
		Kind:  core.SourceYouTube,
		Path:  "https://www.youtube.com/watch?v=abc123",
		Title: testTitle,
		Show:  testShow,
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	job, err := h.q.Claim(JobKind)
	if err != nil || job == nil || job.ID != id {
		t.Fatalf("Claim = %+v, %v", job, err)
	}
	// Run probe, plan, convert, verify, thumbnail: the job now sits at the
	// publish stage with a real converted file, exactly where a kill could
	// leave it.
	for i := range 5 {
		if done := h.w.processOne(context.Background(), id); done {
			t.Fatalf("job finished after %d stages", i)
		}
	}
	if j, _ := h.q.Get(id); j.Stage != StagePublish {
		t.Fatalf("stage = %s, want publish", j.Stage)
	}
	pl := h.payloadOf(t, id)
	// The pre-show-first stagePublish persisted Published=true after its
	// publish; a recovered call must not publish a second time.
	pl.Published = true
	if next, err := h.w.stagePublish(context.Background(), id, &pl); next != "" || err != nil {
		t.Fatalf("stagePublish with the legacy Published marker = (%q, %v), want a no-op", next, err)
	}
	if h.pub.Called() != 0 {
		t.Fatalf("legacy Published marker republished the episode")
	}
	// With no marker the stage does publish the converted, app-owned file.
	pl.Published = false
	if next, err := h.w.stagePublish(context.Background(), id, &pl); next != "" || err != nil {
		t.Fatalf("stagePublish = (%q, %v)", next, err)
	}
	pubs := h.pub.Published()
	if len(pubs) != 1 || !pubs[0].Owned {
		t.Fatalf("published %+v, want the app-owned converted episode", pubs)
	}
	if err := h.q.Done(id); err != nil {
		t.Fatalf("Done: %v", err)
	}
	ep, _ := suit(t, h, id)
	if !ep.Owned {
		t.Error("YouTube episode Owned = false, want true")
	}
}

// TestShowPhaseIssueFix pins the fix status the show-phase publish writes:
// audio plans publish format/pending, full picture/pending, remux
// format/pending (the Safari setting is on by default), in-place plans
// publish ""/"" and a remux with the Safari setting off is final in place.
// The improvement publish clears both fields.
func TestShowPhaseIssueFix(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	t.Run("audio", func(t *testing.T) {
		h := newHarness(t, helpers)
		id := h.enqueue(t, f.ac3MKV)
		h.runToEnd(t, id)
		ep, _ := suitImprove(t, h, id, f.ac3MKV)
		show := h.pub.Published()[0]
		if show.Issue != core.IssueSound || show.Fix != core.FixPending {
			t.Errorf("show publish = %q/%q, want sound/pending", show.Issue, show.Fix)
		}
		if ep.Issue != core.IssueNone || ep.Fix != core.FixNone {
			t.Errorf("improved publish = %q/%q, want cleared", ep.Issue, ep.Fix)
		}
	})
	t.Run("full", func(t *testing.T) {
		h := newHarness(t, helpers)
		id := h.enqueue(t, f.rotatedMP4)
		h.runToEnd(t, id)
		ep, _ := suitImprove(t, h, id, f.rotatedMP4)
		show := h.pub.Published()[0]
		if show.Issue != core.IssuePicture || show.Fix != core.FixPending {
			t.Errorf("show publish = %q/%q, want picture/pending", show.Issue, show.Fix)
		}
		if ep.Issue != core.IssueNone || ep.Fix != core.FixNone {
			t.Errorf("improved publish = %q/%q, want cleared", ep.Issue, ep.Fix)
		}
	})
	t.Run("remux", func(t *testing.T) {
		h := newHarness(t, helpers)
		id := h.enqueue(t, f.remuxMKV)
		h.runStages(t, id, 3)
		show := h.pub.Published()[0]
		if show.Issue != core.IssueFormat || show.Fix != core.FixPending {
			t.Errorf("show publish = %q/%q, want format/pending", show.Issue, show.Fix)
		}
		h.runToEnd(t, id)
		if ep, _ := suitImprove(t, h, id, f.remuxMKV); ep.Issue != core.IssueNone || ep.Fix != core.FixNone {
			t.Errorf("improved publish = %q/%q, want cleared", ep.Issue, ep.Fix)
		}
	})
	t.Run("inplace", func(t *testing.T) {
		h := newHarness(t, helpers)
		id := h.enqueue(t, f.keep)
		h.runToEnd(t, id)
		ep, _ := suit(t, h, id)
		if ep.Issue != core.IssueNone || ep.Fix != core.FixNone {
			t.Errorf("in-place publish = %q/%q, want cleared", ep.Issue, ep.Fix)
		}
	})
	t.Run("remux safari off", func(t *testing.T) {
		h := newHarness(t, helpers)
		h.w.WithSafariSetting(func(context.Context) (bool, error) { return false, nil })
		id := h.enqueue(t, f.remuxMKV)
		h.runToEnd(t, id)
		ep, _ := suit(t, h, id)
		if ep.Issue != core.IssueNone || ep.Fix != core.FixNone {
			t.Errorf("safari-off publish = %q/%q, want cleared", ep.Issue, ep.Fix)
		}
	})
}

// TestFixLaneClaimOrder pins the fix lane's claim order: class remux
// before audio before full, and within a class the shorter video first.
// The priorities come from the real show-stage requeue, so the order is
// exactly what a live queue asks the lane for.
func TestFixLaneClaimOrder(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	h := newHarness(t, helpers)
	// Three fixtures whose plans are remux (MKV, H.264+AAC), audio (AC3)
	// and full (MPEG-4), plus a second remux file the test can give a
	// shorter duration than the first.
	remuxLong, remuxShort := f.remuxMKV, filepath.Join(t.TempDir(), "short.mkv")
	if b, err := os.ReadFile(f.remuxMKV); err != nil {
		t.Fatalf("read remux fixture: %v", err)
	} else if err := os.WriteFile(remuxShort, b, 0o644); err != nil {
		t.Fatalf("write short remux: %v", err)
	}
	// Run probe and plan for the two remux jobs, then hand each show stage
	// a payload with the duration this test wants, so the real stageShow
	// requeue computes the priority from it (it reads pl.DurationSec).
	showWithDuration := func(id string, dur int64) {
		t.Helper()
		h.runStages(t, id, 2) // probe, plan; the job is left running at show
		pl := h.payloadOf(t, id)
		pl.DurationSec = dur
		if err := h.q.Advance(id, StageShow, pl); err != nil {
			t.Fatalf("Advance %s: %v", id, err)
		}
		// The job is still claimed from runStages; run its show stage,
		// which publishes and requeues with the patched duration.
		h.w.processOne(context.Background(), id)
	}
	remuxLongID := h.enqueue(t, remuxLong)
	shortID := h.enqueue(t, remuxShort)
	showWithDuration(remuxLongID, 60)
	showWithDuration(shortID, 12)
	audioID := h.enqueue(t, f.ac3MKV)
	fullID := h.enqueue(t, f.rotatedMP4)
	for _, id := range []string{audioID, fullID} {
		h.runStages(t, id, 3) // probe, plan, show: requeued at its fix priority
	}
	want := []string{shortID, remuxLongID, audioID, fullID}
	for i, wantID := range want {
		job, err := h.q.ClaimRange(JobKind, 1, math.MaxInt)
		if err != nil {
			t.Fatalf("ClaimRange %d: %v", i, err)
		}
		if job == nil || job.ID != wantID {
			t.Fatalf("fix lane claim %d = %+v, want %s", i, job, wantID)
		}
	}
	// Nothing else waits in the fix lane.
	if job, err := h.q.ClaimRange(JobKind, 1, math.MaxInt); err != nil || job != nil {
		t.Fatalf("fix lane has a fifth claim %+v (err %v), want none", job, err)
	}
}

// TestSortLaneCompletesWhileFixLaneConverts runs the two lanes at once:
// an improvement's conversion is blocked in the fix lane, and a brand-new
// in-place file is still published by the sort lane while the conversion
// sits stuck, exactly the "appears within seconds" contract.
func TestSortLaneCompletesWhileFixLaneConverts(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	h := newHarness(t, helpers)
	longID := h.enqueue(t, f.remuxMKV)
	h.runStages(t, longID, 3) // probe, plan, show: queued at its fix priority

	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	h.w.runCmd = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		// The improvement's convert is the only helper call whose output
		// path ends in .mp4 (decodeCheck ends "-f null -", thumbnails end
		// .tmp.jpg); the helper name is the full path to ffmpeg.
		if filepath.Base(name) == "ffmpeg" && len(args) > 0 && strings.HasSuffix(args[len(args)-1], ".mp4") {
			enteredOnce.Do(func() { close(entered) })
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return runCombined(ctx, name, args...)
	}

	fixDone := make(chan struct{})
	go func() {
		defer close(fixDone)
		job, err := h.q.ClaimRange(JobKind, 1, math.MaxInt)
		if err != nil {
			t.Errorf("fix lane ClaimRange: %v", err)
			return
		}
		if job == nil {
			t.Error("fix lane claimed nothing")
			return
		}
		h.w.process(context.Background(), job)
	}()
	<-entered // the conversion is running and blocked

	// A new in-place file: the sort lane must take it to the shelf while
	// the conversion above is still stuck.
	shortID := h.enqueue(t, f.keep)
	job, err := h.q.ClaimRange(JobKind, 0, 0)
	if err != nil || job == nil {
		t.Fatalf("sort lane claim: %v %+v", err, job)
	}
	h.w.process(context.Background(), job)
	j2, err := h.q.Get(shortID)
	if err != nil || j2.State != jobs.StateDone {
		t.Fatalf("sort-lane job after its run: %+v (err %v), want done", j2, err)
	}
	if ep := h.pub.Published(); len(ep) != 2 || ep[1].ID != core.EpisodeID(h.payloadOf(t, shortID).EpisodeID) {
		t.Fatalf("published %+v, want the sort-lane episode on the shelf while the fix lane converts", ep)
	}
	j1, err := h.q.Get(longID)
	if err != nil || j1.State != jobs.StateRunning || j1.Stage != StageConvert {
		t.Fatalf("fix-lane job while converting: %+v (err %v), want running at convert", j1, err)
	}

	close(release)
	<-fixDone
	if j1, err := h.q.Get(longID); err != nil || j1.State != jobs.StateDone {
		t.Fatalf("fix-lane job after release: %+v (err %v), want done", j1, err)
	}
	if ep := h.pub.Published(); len(ep) != 3 || !ep[2].Owned {
		t.Fatalf("final publishes = %d, want the improvement's app-owned copy", len(ep))
	}
}

// TestReprioritizeQueuedImprovements drives jobs migration v6 over rows a
// pre-change database would hold: improvement jobs queued under the old
// priorities (audio 1, full 2, remux 3) whose payloads were written by
// the real ingest payload marshalling. The migration rewrites their
// priority to the fix-lane formula, leaves done and failed jobs alone,
// and re-running changes nothing.
func TestReprioritizeQueuedImprovements(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "prechange.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	// The database as it was before this change: only jobs migrations v1
	// through v5 recorded.
	if err := s.Migrate(jobs.Migrations[:5]...); err != nil {
		t.Fatalf("pre-change migrations: %v", err)
	}
	// Build the rows the way the pre-change pipeline persisted them: the
	// payload is json.Marshal of the real payload struct (the exact
	// encoding jobs.Queue.Advance writes), and the old priorities come
	// from the pre-change requeue (audio 1, full 2, remux 3). The rows
	// are inserted by hand because jobs.New would apply version 6 on top
	// of a database that predates this change.
	oldPrio := map[Action]int{ActionAudio: 1, ActionFull: 2, ActionRemux: 3}
	insert := func(action Action, dur int64, state string) string {
		t.Helper()
		pl := payload{
			File: "/media/folder/a.mkv", Source: core.Source{
				Kind: core.SourceFolder, Path: "/media/folder/a.mkv",
				Title: "A", Show: "Show",
			},
			EpisodeID:   core.EpisodeID("ep-" + string(action) + "-" + strconv.FormatInt(dur, 10)),
			DurationSec: dur, Action: action, Shown: true, Published: true,
		}
		raw, err := json.Marshal(pl)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		id := strings.TrimPrefix(string(pl.EpisodeID), "ep-")
		now := "2026-10-26T12:00:00Z"
		if _, err := s.DB().Exec(`
INSERT INTO jobs (id, kind, stage, state, payload, priority, reserved_bytes, attempts, error, created, updated, seq)
VALUES (?, 'ingest', 'convert', ?, ?, ?, 0, 1, '', ?, ?, ?)`,
			"job-"+id, state, string(raw), oldPrio[action], now, now, 1); err != nil {
			t.Fatalf("insert %s: %v", action, err)
		}
		return "job-" + id
	}
	remuxID := insert(ActionRemux, 61, jobs.StateQueued)
	audioID := insert(ActionAudio, 2400, jobs.StateQueued)
	fullID := insert(ActionFull, 7, jobs.StateQueued)
	queuedRemuxID := insert(ActionRemux, 3, jobs.StateQueued)
	// A done and a failed improvement stay untouched: nothing claims them.
	doneID := insert(ActionAudio, 5, jobs.StateDone)
	failedID := insert(ActionFull, 9, jobs.StateFailed)

	if err := s.Migrate(jobs.Migrations...); err != nil {
		t.Fatalf("v6 migration: %v", err)
	}
	q, err := jobs.New(s) // records nothing new; only used to read rows back
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	want := map[string]int{
		remuxID:       fixPriority(ActionRemux, 61),
		audioID:       fixPriority(ActionAudio, 2400),
		fullID:        fixPriority(ActionFull, 7),
		queuedRemuxID: fixPriority(ActionRemux, 3),
	}
	for id, wantPrio := range want {
		j, err := q.Get(id)
		if err != nil {
			t.Fatalf("Get %s: %v", id, err)
		}
		if j.Priority != wantPrio {
			t.Errorf("job %s priority = %d, want %d", id, j.Priority, wantPrio)
		}
	}
	for _, id := range []string{doneID, failedID} {
		if j, _ := q.Get(id); j.Priority != 1 && j.Priority != 2 && j.Priority != 3 {
			t.Errorf("terminal job %s priority = %d, want its old priority kept (1-3)", id, j.Priority)
		}
	}
	// A second launch applies nothing: Migrate records version 6.
	before := map[string]int{}
	for id := range want {
		j, _ := q.Get(id)
		before[id] = j.Priority
	}
	if err := s.Migrate(jobs.Migrations...); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	for id, prio := range before {
		if j, _ := q.Get(id); j.Priority != prio {
			t.Errorf("second run changed job %s priority to %d", id, j.Priority)
		}
	}
}
