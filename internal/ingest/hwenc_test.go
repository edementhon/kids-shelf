package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edementhon/kids-shelf/internal/jobs"
	"github.com/edementhon/kids-shelf/internal/tools"
)

// fakeScriptDir writes executable fake helper scripts: ffmpeg-hw answers
// -encoders from $KS_FAKE_ENCODERS and fails the encode of every encoder
// listed in $KS_FAKE_FAIL; ffmpeg always succeeds; ffprobe reports a good
// H.264 8-bit yuv420p 40-frame clip. Every invocation appends its name and
// arguments to $KS_FAKE_LOG, so tests can see exactly what ran.
func fakeScriptDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("KS_FAKE_LOG", filepath.Join(t.TempDir(), "runs.log"))
	writeExec(t, dir, "ffmpeg-hw", `#!/bin/sh
echo "$0 $@" >> "$KS_FAKE_LOG"
for a in "$@"; do
  if [ "$a" = "-encoders" ]; then
    for e in $KS_FAKE_ENCODERS; do echo " V....D $e              fake"; done
    exit 0
  fi
done
for a in "$@"; do
  for e in $KS_FAKE_FAIL; do
    [ "$a" = "$e" ] && exit 1
  done
done
exit 0
`)
	writeExec(t, dir, "ffmpeg", `#!/bin/sh
echo "$0 $@" >> "$KS_FAKE_LOG"
exit 0
`)
	writeExec(t, dir, "ffprobe", `#!/bin/sh
echo "$0 $@" >> "$KS_FAKE_LOG"
cat <<'EOF'
{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","profile":"High","pix_fmt":"yuv420p","width":1280,"height":720,"nb_frames":"40","disposition":{"default":1,"attached_pic":0}}],"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"1.000000"}}
EOF
exit 0
`)
	return dir
}

// writeExec writes an executable helper script into dir, like the tools
// package's own test helper.
func writeExec(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func fakeRuns(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(os.Getenv("KS_FAKE_LOG"))
	if err != nil {
		return ""
	}
	return string(b)
}

// fakeDir builds a tools.Dir over the fake scripts in dir.
func fakeDir(t *testing.T, dir string) *tools.Dir {
	return &tools.Dir{
		FFmpeg:     filepath.Join(dir, "ffmpeg"),
		FFprobe:    filepath.Join(dir, "ffprobe"),
		FFmpegHW:   filepath.Join(dir, "ffmpeg-hw"),
		Qjs:        "",
		YtDlp:      "",
		KiwixServe: "",
	}
}

// TestHWSelectionFakeRunner drives the selection with fake helper scripts:
// the first candidate whose self-test passes wins, none passing means
// software, and the household setting off returns software without running
// any self-test at all.
func TestHWSelectionFakeRunner(t *testing.T) {
	ctx := context.Background()
	t.Run("first passing candidate wins", func(t *testing.T) {
		dir := fakeScriptDir(t)
		t.Setenv("KS_FAKE_ENCODERS", "h264_fakeA h264_fakeB")
		t.Setenv("KS_FAKE_FAIL", "h264_fakeA")
		w := NewWorker(nil, fakeDir(t, dir), nil, t.TempDir())
		cands := []hwCandidate{
			{label: "Fake A", bin: filepath.Join(dir, "ffmpeg-hw"), vcodec: "h264_fakeA"},
			{label: "Fake B", bin: filepath.Join(dir, "ffmpeg-hw"), vcodec: "h264_fakeB"},
		}
		got := w.pickHardware(ctx, cands)
		if got == nil || got.label != "Fake B" {
			t.Fatalf("pickHardware = %+v, want Fake B", got)
		}
		// A's encode ran and failed, B's encode ran and passed.
		runs := fakeRuns(t)
		if !strings.Contains(runs, "-encoders") {
			t.Errorf("no -encoders listing ran:\n%s", runs)
		}
		if !strings.Contains(runs, "h264_fakeA") || !strings.Contains(runs, "h264_fakeB") {
			t.Errorf("both candidates must be self-tested:\n%s", runs)
		}
		// The self-test used the production encoder chain on the
		// synthetic clip with a 10-bit format step and a decode pass.
		for _, want := range []string{"testsrc=size=1280x720:rate=30", "format=yuv420p10le", "-f", "null"} {
			if !strings.Contains(runs, want) {
				t.Errorf("self-test run missing %q:\n%s", want, runs)
			}
		}
	})
	t.Run("none passing means software", func(t *testing.T) {
		dir := fakeScriptDir(t)
		t.Setenv("KS_FAKE_ENCODERS", "h264_fakeA h264_fakeB")
		t.Setenv("KS_FAKE_FAIL", "h264_fakeA h264_fakeB")
		w := NewWorker(nil, fakeDir(t, dir), nil, t.TempDir())
		cands := []hwCandidate{
			{label: "Fake A", bin: filepath.Join(dir, "ffmpeg-hw"), vcodec: "h264_fakeA"},
			{label: "Fake B", bin: filepath.Join(dir, "ffmpeg-hw"), vcodec: "h264_fakeB"},
		}
		if got := w.pickHardware(ctx, cands); got != nil {
			t.Fatalf("pickHardware = %+v, want nil (software)", got)
		}
	})
	t.Run("setting off skips self-tests", func(t *testing.T) {
		dir := fakeScriptDir(t)
		t.Setenv("KS_FAKE_ENCODERS", "h264_fakeA")
		w := NewWorker(nil, fakeDir(t, dir), nil, t.TempDir())
		w.WithHardwareSetting(func(context.Context) (bool, error) { return false, nil })
		if got := w.chooseEncoder(ctx); got != nil {
			t.Fatalf("chooseEncoder with the setting off = %+v, want nil (software)", got)
		}
		if runs := fakeRuns(t); runs != "" {
			t.Errorf("self-tests ran with the setting off:\n%s", runs)
		}
		if got := w.EncoderLabel(); got != "this computer's processor" {
			t.Errorf("EncoderLabel = %q, want the processor with the setting off", got)
		}
	})
	// The selection is cached: the second choice reuses the first, and an
	// encoder that stops working is not re-probed.
	t.Run("selection is cached", func(t *testing.T) {
		dir := fakeScriptDir(t)
		t.Setenv("KS_FAKE_ENCODERS", "h264_fakeA")
		w := NewWorker(nil, fakeDir(t, dir), nil, t.TempDir())
		cands := []hwCandidate{
			{label: "Fake A", bin: filepath.Join(dir, "ffmpeg-hw"), vcodec: "h264_fakeA"},
		}
		first := w.hw.choose(ctx, func(context.Context) *hwCandidate { return &cands[0] })
		if first == nil {
			t.Fatal("first choose returned nil")
		}
		probes := 0
		second := w.hw.choose(ctx, func(context.Context) *hwCandidate {
			probes++
			return &cands[0]
		})
		if probes != 0 || second != first {
			t.Errorf("second choose re-probed (%d) or changed the result", probes)
		}
	})
}

// TestHWSelectionLogsSoftwareE2E: with no hardware path (no ffmpeg-hw
// helper, which also leaves linux/amd64 without candidates) the selection
// ends in software and says so in the log, and a real full re-encode still
// converts and publishes. The helper set may carry ffmpeg-hw, so the test
// removes it from its own copy rather than depend on the machine.
func TestHWSelectionLogsSoftwareE2E(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	dir, err := tools.Locate("", nil, tools.Manifest{}, t.TempDir())
	if err != nil {
		t.Fatalf("tools.Locate: %v", err)
	}
	dir.FFmpegHW = ""
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	w := NewWorker(nil, dir, nil, t.TempDir())
	if got := w.chooseEncoder(context.Background()); got != nil {
		t.Fatalf("chooseEncoder = %+v, want nil (software) without ffmpeg-hw", got)
	}
	if !strings.Contains(buf.String(), "using software") {
		t.Errorf("selection log = %q, want it to say software", buf.String())
	}
	if got := w.EncoderLabel(); got != "this computer's processor" {
		t.Errorf("EncoderLabel = %q, want the processor", got)
	}

	// A real full re-encode (MPEG-4 input -> ActionFull) still goes
	// through software and publishes.
	f := makeFixtures(t)
	h := newHarness(t, helpers)
	before := sha256File(t, f.rotatedMP4)
	id := h.enqueue(t, f.rotatedMP4)
	h.runToEnd(t, id)
	ep, j := suitImprove(t, h, id, f.rotatedMP4)
	if !ep.Owned || ep.File != filepath.Join(h.mediaDir, "library", string(ep.ID)+".mp4") {
		t.Errorf("converted episode = %+v, want the app-owned converted copy", ep)
	}
	if j.State != jobs.StateDone {
		t.Fatalf("state = %s (error %q), want done", j.State, j.Error)
	}
	assertGoodMP4(t, helpers, ep.File)
	assertIntact(t, f.rotatedMP4, before)
	assertLibraryClean(t, h.mediaDir)
}

// TestHWCommandLines pins the production argv of every encoder: the device
// args sit before -i (a vaapi device created after the input does not
// attach on real hardware), the plan's stream mapping, the scale filter,
// the encoder's own args, and the audio and faststart args identical to
// the software path.
func TestHWCommandLines(t *testing.T) {
	const node = "/dev/dri/renderD128"
	pl := &payload{
		VideoIndex: 0,
		AudioIndex: 3,
		Probe: &ProbeInfo{Duration: 4, Streams: []StreamInfo{
			{CodecType: "video", CodecName: "hevc", PixFmt: "yuv420p10le", Width: 640, Height: 360},
			{CodecType: "audio", CodecName: "aac", Profile: "LC", Channels: 1, Disposition: Disposition{Default: 1}},
		}},
	}
	w := &Worker{}
	software := w.fullEncodeArgs(nil, pl, "out.mp4")
	if got := arg(software, "-c:v"); len(got) != 1 || got[0] != "libx264" {
		t.Errorf("software -c:v = %v, want libx264", got)
	}
	for key, want := range map[string][]string{
		"-crf": {"21"}, "-maxrate": {"8M"}, "-bufsize": {"16M"},
		"-pix_fmt": {"yuv420p"}, "-profile:v": {"high"},
		"-c:a": {"aac"}, "-b:a": {"160k"}, "-ac": {"2"},
	} {
		if got := arg(software, key); len(got) != 1 || got[0] != want[0] {
			t.Errorf("software %s = %v, want [%s]", key, got, want[0])
		}
	}
	if got := arg(software, "-movflags"); len(got) != 1 || got[0] != "+faststart" {
		t.Errorf("software -movflags = %v, want [+faststart]", got)
	}

	platforms := []struct {
		goos, goarch string
		devices      []string
	}{
		{"darwin", "amd64", nil},
		{"darwin", "arm64", nil},
		{"windows", "amd64", nil},
		{"linux", "amd64", []string{node}},
		{"linux", "arm64", []string{"/dev/video0"}},
	}
	for _, p := range platforms {
		seen := map[string]bool{}
		dir := &tools.Dir{FFmpeg: "/bin/ffmpeg", FFprobe: "/bin/ffprobe", FFmpegHW: "/bin/ffmpeg-hw"}
		cands := hwCandidatesFor(p.goos, p.goarch, dir, p.devices)
		if p.goos == "linux" && p.goarch == "arm64" && len(cands) != 1 {
			t.Fatalf("%s/%s candidates = %d, want 1", p.goos, p.goarch, len(cands))
		}
		for i := range cands {
			c := &cands[i]
			if seen[c.vcodec] {
				t.Errorf("%s listed twice", c.vcodec)
			}
			seen[c.vcodec] = true
			args := w.fullEncodeArgs(c, pl, "out.mp4")
			// The input must come after every global device arg.
			iIdx := indexOf(args, "-i")
			if iIdx < 0 {
				t.Fatalf("%s: no -i in %v", c.vcodec, args)
			}
			for _, g := range c.global {
				gi := indexOf(args, g)
				if gi < 0 || gi > iIdx {
					t.Errorf("%s: global arg %q at %d, want before -i at %d (%v)", c.vcodec, g, gi, iIdx, args)
				}
			}
			// Every -init_hw_device names a device type ffmpeg has
			// (libavutil/hwcontext.h, AV_HWDEVICE_TYPE_*): an unknown type
			// makes ffmpeg exit before encoding, so that encoder could
			// never pass its self-test (the first Pi candidate asked for
			// "v4l2m2m", which is an encoder, not a device type).
			for gi, g := range c.global {
				if g != "-init_hw_device" || gi+1 >= len(c.global) {
					continue
				}
				typ, _, _ := strings.Cut(strings.SplitN(c.global[gi+1], ":", 2)[0], "=")
				switch typ {
				case "vdpau", "cuda", "vaapi", "dxva2", "qsv", "videotoolbox", "d3d11va",
					"drm", "opencl", "mediacodec", "vulkan", "d3d12va", "amf":
				default:
					t.Errorf("%s: -init_hw_device %q names no ffmpeg device type", c.vcodec, c.global[gi+1])
				}
			}
			if args[len(args)-1] != "out.mp4" {
				t.Errorf("%s: output file must be the last arg", c.vcodec)
			}
			// The plan's streams, the scale filter and the chain.
			if got := arg(args, "-map"); len(got) != 2 || got[0] != "0:0" || got[1] != "0:3" {
				t.Errorf("%s maps %v, want [0:0 0:3]", c.vcodec, got)
			}
			wantFilter := scaleFilter(pl.Probe)
			if c.chain != "" {
				wantFilter += "," + c.chain
			}
			if got := arg(args, "-vf"); len(got) != 1 || got[0] != wantFilter {
				t.Errorf("%s -vf = %v, want [%s]", c.vcodec, got, wantFilter)
			}
			// Its own encoder and the shared audio/container tail.
			if got := arg(args, "-c:v"); len(got) != 1 || got[0] != c.vcodec {
				t.Errorf("%s -c:v = %v, want [%s]", c.vcodec, got, c.vcodec)
			}
			if got := arg(args, "-c:a"); len(got) != 1 || got[0] != "aac" {
				t.Errorf("%s -c:a = %v, want [aac]", c.vcodec, got)
			}
			if got := arg(args, "-movflags"); len(got) != 1 || got[0] != "+faststart" {
				t.Errorf("%s -movflags = %v, want [+faststart]", c.vcodec, got)
			}
			for _, opt := range c.vopts {
				if !contains(args, opt) {
					t.Errorf("%s missing its option %q (%v)", c.vcodec, opt, args)
				}
			}
			if c.bitrate {
				if got := arg(args, "-b:v"); len(got) != 1 || got[0] != bitrateTarget(pl.Probe) {
					t.Errorf("%s -b:v = %v, want [%s]", c.vcodec, got, bitrateTarget(pl.Probe))
				}
			}
		}
	}
	// Preference order per platform.
	windows := hwCandidatesFor("windows", "amd64", &tools.Dir{FFmpeg: "/bin/ffmpeg"}, nil)
	wantOrder := []string{"h264_nvenc", "h264_qsv", "h264_amf", "h264_mf"}
	for i, want := range wantOrder {
		if windows[i].vcodec != want {
			t.Errorf("windows candidate %d = %s, want %s", i, windows[i].vcodec, want)
		}
	}
	linux := hwCandidatesFor("linux", "amd64", &tools.Dir{FFmpegHW: "/bin/ffmpeg-hw"}, []string{node})
	if len(linux) < 3 || linux[0].vcodec != "h264_vaapi" || linux[1].vcodec != "h264_nvenc" || linux[2].vcodec != "h264_qsv" {
		t.Errorf("linux/amd64 candidates = %+v, want vaapi, nvenc, qsv", linux)
	}
	if !strings.Contains(linux[0].global[1], node) {
		t.Errorf("vaapi device arg = %v, want it to name %s", linux[0].global, node)
	}
	// A per-node candidate for each VA-API device.
	two := hwCandidatesFor("linux", "amd64", &tools.Dir{FFmpegHW: "/bin/ffmpeg-hw"}, []string{node, "/dev/dri/renderD129"})
	if len(two) != 4 {
		t.Errorf("linux/amd64 with two render nodes = %d candidates, want 4", len(two))
	}
	if two[1].global[1] != "vaapi=hw:/dev/dri/renderD129" {
		t.Errorf("second node candidate = %v, want its own device", two[1].global)
	}
}

// TestHWNoCandidatesWithoutFFmpegHW: linux/amd64 has no hardware candidates
// when the FFmpegHW helper is absent.
func TestHWNoCandidatesWithoutFFmpegHW(t *testing.T) {
	dir := &tools.Dir{FFmpeg: "/bin/ffmpeg"}
	if got := hwCandidatesFor("linux", "amd64", dir, []string{"/dev/dri/renderD128"}); len(got) != 0 {
		t.Errorf("linux/amd64 without ffmpeg-hw = %+v, want no candidates", got)
	}
	if got := hwCandidatesFor("linux", "amd64", nil, nil); len(got) != 0 {
		t.Errorf("nil dir = %+v, want no candidates", got)
	}
}

func TestBitrateTarget(t *testing.T) {
	vid := func(w, h int) *ProbeInfo {
		return &ProbeInfo{Streams: []StreamInfo{{CodecType: "video", Width: w, Height: h}}}
	}
	cases := []struct {
		p    *ProbeInfo
		want string
	}{
		{vid(1920, 1080), "5000000"},
		{vid(1280, 720), "3000000"},
		{vid(640, 360), "1500000"},
		{vid(3840, 2160), "5000000"}, // a 4K input is scaled to 1080p
		{vid(1080, 1920), "5000000"}, // portrait phone video
		{&ProbeInfo{}, "1500000"},    // unknown size: the floor
	}
	for _, c := range cases {
		if got := bitrateTarget(c.p); got != c.want {
			t.Errorf("bitrateTarget(%v) = %s, want %s", c.p, got, c.want)
		}
	}
}

// hwForce pins the worker's hardware selection to a fake candidate whose
// encoder name no real ffmpeg knows, so tests can recognize production
// hardware attempts by the arguments.
func hwForce(t *testing.T, h harness, bin string) {
	t.Helper()
	h.w.hw.mu.Lock()
	defer h.w.hw.mu.Unlock()
	h.w.hw.chosen = &hwCandidate{label: "Fake HW", bin: bin, vcodec: "h264_fake"}
	h.w.hw.tried = true
}

// hwFakeRun wraps the real runner and counts or breaks the hardware
// attempt, recognized by the -c:v h264_fake pair, so the software fallback
// really encodes with the helpers.
type hwFakeRun struct {
	hwAttempts  int
	sawSoftware bool
	failEncode  bool // the hardware encode exits nonzero
	dropFast    bool // the hardware "success" writes no faststart (verify rejects it)
}

func (f *hwFakeRun) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if pair(args, "-c:v", "h264_fake") {
		f.hwAttempts++
		if f.failEncode {
			return []byte("h264_fake: encoder failed\n"), errors.New("exit status 1")
		}
		args = rewritePair(args, "-c:v", "h264_fake", "-c:v", "libx264", "-preset", "veryfast", "-crf", "21")
		if f.dropFast {
			args = removePair(args, "-movflags", "+faststart")
		}
	}
	if pair(args, "-c:v", "libx264") {
		f.sawSoftware = true
	}
	return runCombined(ctx, name, args...)
}

// distinctInput makes a separate copy of a fixture, so several jobs can
// convert the same content: Enqueue is idempotent per source path, so a
// second job needs a second path.
func distinctInput(t *testing.T, src, name string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), name)
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
	return dst
}

// TestHWConvertFallsBackToSoftware: a full re-encode whose hardware run
// fails is redone with libx264 in the same job and publishes; the failure
// is counted.
func TestHWConvertFallsBackToSoftware(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	h := newHarness(t, helpers)
	hwForce(t, h, h.w.dir.FFmpeg)
	fake := &hwFakeRun{failEncode: true}
	h.w.runCmd = fake.run
	before := sha256File(t, f.rotatedMP4)
	id := h.enqueue(t, f.rotatedMP4)
	h.runToEnd(t, id)
	ep, j := suitImprove(t, h, id, f.rotatedMP4)
	if j.State != jobs.StateDone {
		t.Fatalf("state = %s (error %q), want done", j.State, j.Error)
	}
	if !ep.Owned {
		t.Error("converted episode Owned = false, want true")
	}
	assertGoodMP4(t, helpers, ep.File)
	assertIntact(t, f.rotatedMP4, before)
	if fake.hwAttempts != 1 {
		t.Errorf("hardware attempts = %d, want exactly 1", fake.hwAttempts)
	}
	if !fake.sawSoftware {
		t.Error("the job never ran the libx264 fallback")
	}
	if h.w.hw.fails != 1 {
		t.Errorf("consecutive failures = %d, want 1", h.w.hw.fails)
	}
	if h.w.hw.cutoff {
		t.Error("cutoff reached after a single failure")
	}
}

// TestHWVerifyRedo: a hardware run that produces a copy the verify stage
// rejects is redone once with libx264 in the same job and publishes.
func TestHWVerifyRedo(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	h := newHarness(t, helpers)
	hwForce(t, h, h.w.dir.FFmpeg)
	fake := &hwFakeRun{dropFast: true}
	h.w.runCmd = fake.run
	id := h.enqueue(t, f.rotatedMP4)
	h.runToEnd(t, id)
	ep, j := suitImprove(t, h, id, f.rotatedMP4)
	if j.State != jobs.StateDone {
		t.Fatalf("state = %s (error %q), want done", j.State, j.Error)
	}
	if !ep.Owned {
		t.Error("converted episode Owned = false, want true")
	}
	assertGoodMP4(t, helpers, ep.File)
	if fake.hwAttempts != 1 {
		t.Errorf("hardware attempts = %d, want exactly 1", fake.hwAttempts)
	}
	if !fake.sawSoftware {
		t.Error("the job never ran the libx264 redo")
	}
	if h.w.hw.fails != 1 {
		t.Errorf("consecutive failures = %d, want 1 (the bad hardware copy)", h.w.hw.fails)
	}
	pl := h.payloadOf(t, id)
	if pl.EncHw {
		t.Error("payload still marks the conversion as hardware after the redo")
	}
}

// TestHWThreeFailuresCutOff: three consecutive hardware failures switch
// the process to software for the rest of it; each failing job still
// publishes through the libx264 fallback.
func TestHWThreeFailuresCutOff(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	h := newHarness(t, helpers)
	hwForce(t, h, h.w.dir.FFmpeg)
	fake := &hwFakeRun{failEncode: true}
	h.w.runCmd = fake.run
	for i := range 3 {
		id := h.enqueue(t, distinctInput(t, f.rotatedMP4, fmt.Sprintf("job%d.mp4", i+1)))
		h.runToEnd(t, id)
	}
	if fake.hwAttempts != 3 {
		t.Fatalf("hardware attempts = %d, want 3 before the cutoff", fake.hwAttempts)
	}
	if !h.w.hw.cutoff {
		t.Fatal("cutoff not reached after 3 failures")
	}
	if got := h.w.EncoderLabel(); !strings.Contains(got, "the graphics chip stopped working, so the processor took over") {
		t.Errorf("EncoderLabel = %q, want the cutoff suffix", got)
	}
	// The fourth job goes straight to software: no hardware attempt.
	id := h.enqueue(t, distinctInput(t, f.rotatedMP4, "job4.mp4"))
	h.runToEnd(t, id)
	if fake.hwAttempts != 3 {
		t.Errorf("hardware attempts after the cutoff = %d, want still 3", fake.hwAttempts)
	}
	pl := h.payloadOf(t, id)
	if pl.EncHw {
		t.Error("post-cutoff conversion marked hardware")
	}
}

// TestHWSuccessResetsCount: a hardware output that passes verify clears
// the consecutive-failure count, so the cutoff never fires.
func TestHWSuccessResetsCount(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	h := newHarness(t, helpers)
	hwForce(t, h, h.w.dir.FFmpeg)
	fake := &hwFakeRun{failEncode: true}
	h.w.runCmd = fake.run
	id := h.enqueue(t, distinctInput(t, f.rotatedMP4, "job1.mp4"))
	h.runToEnd(t, id)
	if h.w.hw.fails != 1 {
		t.Fatalf("after the failing job, failures = %d, want 1", h.w.hw.fails)
	}
	// The encoder works now: its rewritten output passes verify and the
	// count starts over.
	fake.failEncode = false
	id = h.enqueue(t, distinctInput(t, f.rotatedMP4, "job2.mp4"))
	h.runToEnd(t, id)
	if h.w.hw.fails != 0 {
		t.Errorf("after a hardware success, failures = %d, want 0", h.w.hw.fails)
	}
	if h.w.hw.cutoff {
		t.Error("cutoff reached despite a later success")
	}
}

// TestHWRedoNeverLoops: a hardware output that keeps failing verify is
// redone exactly once; the software redo's own verify failure fails the
// job with the keep-as-is words and never tries hardware a second time.
func TestHWRedoNeverLoops(t *testing.T) {
	helpers, _ := fixtureEnv(t)
	f := makeFixtures(t)
	h := newHarness(t, helpers)
	hwForce(t, h, h.w.dir.FFmpeg)
	fake := &hwFakeRun{dropFast: true}
	h.w.runCmd = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		// Make the decode pass fail for every output (the hardware one
		// and the software redo), the way the existing verify-failure
		// test does.
		for _, a := range args {
			if a == "-progress" {
				return nil, errors.New("exit status 1")
			}
		}
		return fake.run(ctx, name, args...)
	}
	id := h.enqueue(t, f.rotatedMP4)
	h.runToEnd(t, id)
	j, err := h.q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.State != jobs.StateFailed {
		t.Fatalf("state = %s, want failed (the software redo failed verify)", j.State)
	}
	if j.Error != msgKeepAsIs {
		t.Errorf("error = %q, want the keep-as-is words", j.Error)
	}
	if fake.hwAttempts != 1 {
		t.Errorf("hardware attempts = %d, want exactly 1 (never a second hardware try)", fake.hwAttempts)
	}
}

// pair reports whether args contains the key/value pair.
func pair(args []string, key, val string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == key && args[i+1] == val {
			return true
		}
	}
	return false
}

// rewritePair replaces the first key/value pair with the given replacement.
func rewritePair(args []string, key, val string, repl ...string) []string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == key && args[i+1] == val {
			out := make([]string, 0, len(args)-2+len(repl))
			out = append(out, args[:i]...)
			out = append(out, repl...)
			return append(out, args[i+2:]...)
		}
	}
	return args
}

// removePair drops the first key/value pair.
func removePair(args []string, key, val string) []string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == key && args[i+1] == val {
			out := make([]string, 0, len(args)-2)
			out = append(out, args[:i]...)
			return append(out, args[i+2:]...)
		}
	}
	return args
}

func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

func contains(args []string, want string) bool {
	return indexOf(args, want) >= 0
}
