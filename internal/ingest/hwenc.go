// Opportunistic hardware H.264 encoding: full re-encodes (ActionFull) use
// a working hardware encoder when one exists, with libx264 as the fallback
// everywhere. The platform's candidates are listed once (hwCandidatesFor)
// and the first one that proves itself on a synthetic clip at selection
// time wins for the rest of the process; when none works, the processor
// does the encode. The household "use the graphics chip" setting is
// re-read on every selection, so turning it off stops hardware without
// re-probing, and a hardware run that fails - the encode itself, or the
// verify of its output - falls back to libx264 once for that job and
// counts toward a process-wide cutoff after three consecutive failures.
package ingest

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edementhon/kids-shelf/internal/tools"
)

// hwMaxConsecutiveFails is how many hardware failures in a row switch the
// process to software for the rest of it.
const hwMaxConsecutiveFails = 3

// hwSelTimeout bounds one candidate's self-test run; hwListTimeout bound
// the encoder listing of one helper binary.
const (
	hwSelTimeout  = 20 * time.Second
	hwListTimeout = 10 * time.Second
)

// hwCandidate is one opportunistic hardware H.264 encoder: which helper
// binary runs it, the global device args (always placed before -i), the
// video filter tail appended after the shared scale filter, and the
// encoder's own options. bitrate marks the resolution-based encoders
// (videotoolbox, Media Foundation, V4L2) that take -b:v instead of a
// quality knob.
type hwCandidate struct {
	label   string
	bin     string
	global  []string
	chain   string
	vcodec  string
	vopts   []string
	bitrate bool
}

// hwCandidates returns the running platform's hardware candidates (see
// hwCandidatesFor), probing the machine's real device nodes.
func hwCandidates(dir *tools.Dir) []hwCandidate {
	return hwCandidatesFor(runtime.GOOS, runtime.GOARCH, dir, nil)
}

// hwCandidatesFor lists the hardware encoders of one target in preference
// order. The linux/amd64 set lives only in the FFmpegHW helper (the static
// software-only ffmpeg has none); every other set rides on the regular
// bundled ffmpeg. devices overrides the probed device nodes (the
// /dev/dri/renderD* nodes for VA-API, /dev/video* for V4L2) so tests can
// name their own. A missing helper binary means no candidates.
func hwCandidatesFor(goos, goarch string, dir *tools.Dir, devices []string) []hwCandidate {
	switch goos {
	case "darwin":
		if dir == nil || dir.FFmpeg == "" {
			return nil
		}
		return []hwCandidate{{
			label:   "Apple video engine (VideoToolbox)",
			bin:     dir.FFmpeg,
			vcodec:  "h264_videotoolbox",
			bitrate: true,
			// -pix_fmt yuv420p: 8-bit 4:2:0 output, the only variant
			// every target device decodes. -profile:v high: H.264
			// High profile.
			vopts: []string{"-pix_fmt", "yuv420p", "-profile:v", "high"},
		}}
	case "windows":
		if dir == nil || dir.FFmpeg == "" {
			return nil
		}
		return []hwCandidate{
			{
				label:  "NVIDIA graphics (NVENC)",
				bin:    dir.FFmpeg,
				vcodec: "h264_nvenc",
				// -preset p5: NVENC's slower, better-quality tier.
				// -tune hq: high-quality tuning. -rc vbr: variable
				// bitrate. -cq 23: constant-quality target, close
				// to x264 CRF 21. -b:v 0: let the quality target
				// set the bitrate, not a fixed average.
				vopts: []string{"-preset", "p5", "-tune", "hq", "-rc", "vbr", "-cq", "23", "-b:v", "0"},
			},
			{
				label:  "Intel graphics (Quick Sync)",
				bin:    dir.FFmpeg,
				vcodec: "h264_qsv",
				// -init_hw_device qsv=hw -filter_hw_device hw: one
				// QSV device for the filter graph, set up before
				// the input so the upload below lands on it.
				global: []string{"-init_hw_device", "qsv=hw", "-filter_hw_device", "hw"},
				// format=nv12,hwupload: 8-bit 4:2:0 frames into
				// the device's frame pool.
				chain: "format=nv12,hwupload",
				// -global_quality 23: QSV's constant-quality knob,
				// on the same 1-51 scale as x264 CRF.
				vopts: []string{"-global_quality", "23"},
			},
			{
				label:  "AMD graphics (AMF)",
				bin:    dir.FFmpeg,
				vcodec: "h264_amf",
				// -quality balanced: AMF's speed-versus-size
				// balance. -rc cqp: constant quantizer. -qp_i 22
				// -qp_p 24: I-frame and P-frame quantizers near
				// x264 CRF 21.
				vopts: []string{"-quality", "balanced", "-rc", "cqp", "-qp_i", "22", "-qp_p", "24"},
			},
			{
				label:   "Windows video encoder (Media Foundation)",
				bin:     dir.FFmpeg,
				vcodec:  "h264_mf",
				bitrate: true,
				// -hw_encoding 1: only a hardware encoder will do
				// (the default lets Media Foundation fall back to
				// Microsoft's software one, which would pass the
				// self-test while being no faster than libx264).
				// -profile:v high: H.264 High profile where Media
				// Foundation exposes it; an encoder that does not
				// take the option only warns and keeps its own
				// default.
				vopts: []string{"-hw_encoding", "1", "-profile:v", "high"},
			},
		}
	case "linux":
		if goarch == "amd64" {
			if dir == nil || dir.FFmpegHW == "" {
				return nil
			}
			if devices == nil {
				devices = driRenderNodes()
			}
			cands := make([]hwCandidate, 0, len(devices)+2)
			for _, node := range devices {
				cands = append(cands, hwCandidate{
					label:  "Intel or AMD graphics (VA-API)",
					bin:    dir.FFmpegHW,
					vcodec: "h264_vaapi",
					// -init_hw_device vaapi=hw:<node>: one VA-API
					// device on this render node, created before
					// -i (a device set up after the input does
					// not attach to the decoder). -filter_hw_device
					// hw: the filter graph uploads through it.
					global: []string{"-init_hw_device", "vaapi=hw:" + node, "-filter_hw_device", "hw"},
					// format=nv12,hwupload: 8-bit 4:2:0 frames
					// into the device's frame pool.
					chain: "format=nv12,hwupload",
					// No -rc_mode: with a bitrate and a quality both
					// set, ffmpeg tries QVBR, then ICQ, then CQP,
					// whichever the driver supports (vaapi_encode.c
					// "Rate control mode selection"). An explicit
					// QVBR needs -b:v or ffmpeg refuses to start, and
					// -qp only applies to CQP. -global_quality 23:
					// the quality target, close to x264 CRF 21;
					// bitrate: the resolution-based -b:v target.
					bitrate: true,
					vopts:   []string{"-global_quality", "23"},
				})
			}
			return append(cands,
				hwCandidate{
					label:  "NVIDIA graphics (NVENC)",
					bin:    dir.FFmpegHW,
					vcodec: "h264_nvenc",
					vopts:  []string{"-preset", "p5", "-tune", "hq", "-rc", "vbr", "-cq", "23", "-b:v", "0"},
				},
				hwCandidate{
					label:  "Intel graphics (Quick Sync)",
					bin:    dir.FFmpegHW,
					vcodec: "h264_qsv",
					global: []string{"-init_hw_device", "qsv=hw", "-filter_hw_device", "hw"},
					chain:  "format=nv12,hwupload",
					vopts:  []string{"-global_quality", "23"},
				},
			)
		}
		if goarch == "arm64" {
			if dir == nil || dir.FFmpeg == "" {
				return nil
			}
			if devices == nil {
				devices = videoNodes()
			}
			if len(devices) == 0 {
				return nil
			}
			// One candidate, no device argument: ffmpeg has no v4l2m2m
			// hardware-device type (libavutil/hwcontext.h), and
			// h264_v4l2m2m finds the memory-to-memory encoder node among
			// /dev/video* by itself. The self-test decides whether it works.
			return []hwCandidate{{
				label:   "Raspberry Pi video encoder",
				bin:     dir.FFmpeg,
				vcodec:  "h264_v4l2m2m",
				bitrate: true,
				// -pix_fmt yuv420p: 8-bit 4:2:0 frames, what the Pi
				// encoder takes and every target device decodes.
				vopts: []string{"-pix_fmt", "yuv420p"},
			}}
		}
	}
	return nil
}

// driRenderNodes lists this machine's /dev/dri/renderD* nodes, the VA-API
// devices a desktop GPU exposes.
func driRenderNodes() []string {
	return namedDevices("/dev/dri", "renderD")
}

// videoNodes lists this machine's /dev/video* nodes, the V4L2 codec
// devices an ARM board exposes (the Raspberry Pi's h264 encoder among
// them); the self-test keeps only the one that accepts the clip.
func videoNodes() []string {
	return namedDevices("/dev", "video")
}

func namedDevices(dir, prefix string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

// encArgs returns the encoder's -c:v and its own options, capped by the
// shared -maxrate 8M -bufsize 16M like the software path, so the disk
// reservation (reservedBytes, 8.16 Mbit/s worst case) stays honest for
// every encoder. Backends whose rate control ignores the cap (CQP-style
// quality modes) accept the options without error.
func (c *hwCandidate) encArgs(p *ProbeInfo) []string {
	args := []string{"-c:v", c.vcodec}
	if c.bitrate {
		args = append(args, "-b:v", bitrateTarget(p))
	}
	args = append(args, c.vopts...)
	return append(args, "-maxrate", "8M", "-bufsize", "16M")
}

// bitrateTarget picks the average bitrate of the resolution-based hardware
// encoders (videotoolbox, Media Foundation, V4L2): about 5 Mbit/s at 1080p,
// 3 at 720p, 1.5 below, never over the 8M cap. The class follows the
// display height - the smaller dimension after the rotation turn, which is
// 1080 for both a 1920x1080 landscape frame and a phone's 1080x1920
// portrait - exactly the size the scale filter clamps to, so a 4K input
// lands in the 1080p class.
func bitrateTarget(p *ProbeInfo) string {
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
	small := wide
	if tall < small {
		small = tall
	}
	switch {
	case small >= 1080:
		return "5000000"
	case small >= 720:
		return "3000000"
	default:
		return "1500000"
	}
}

// selfTestProbe is the probe the synthetic selection clip would report:
// 1280x720, the size the self-test asks testsrc for, so the production
// scale filter and the resolution-based bitrate target apply exactly as
// they would to a real 720p input.
var selfTestProbe = &ProbeInfo{Streams: []StreamInfo{
	{CodecType: "video", CodecName: "rawvideo", Width: 1280, Height: 720},
}}

// selfTestArgs builds the candidate's probe invocation: the exact
// production encoder chain over a one-second synthetic clip whose decoded
// frames are 10-bit, the way HEVC 10-bit sources come out of the decoder.
// The production filter chain then has to bring them back to the encoder's
// format, which is the trickiest part of hardware encoding and the point
// of the probe.
func (c *hwCandidate) selfTestArgs(p *ProbeInfo, out string) []string {
	filter := "format=yuv420p10le," + scaleFilter(p)
	if c.chain != "" {
		filter += "," + c.chain
	}
	args := []string{"-y"}
	args = append(args, c.global...)
	// lavfi + testsrc: every hardware-enabled ffmpeg build includes exactly
	// these for this probe (build/helpers/build.sh); testsrc2 is not built.
	args = append(args,
		"-f", "lavfi", "-i", "testsrc=size=1280x720:rate=30", "-t", "1",
		"-map", "0:0", "-vf", filter)
	args = append(args, c.encArgs(p)...)
	return append(args, "-c:a", "aac", "-b:a", "160k", "-ac", "2", "-movflags", "+faststart", out)
}

// candidateSelfTest runs the candidate's exact production arguments on the
// one-second synthetic clip and requires the output to pass the same gates
// the verify stage applies: H.264 8-bit yuv420p, at least 25 frames, and a
// decode that holds through the end. Anything else - the binary missing,
// the encoder absent, a hang - fails the candidate under a per-candidate
// timeout.
func (w *Worker) candidateSelfTest(ctx context.Context, c *hwCandidate) bool {
	cctx, cancel := context.WithTimeout(ctx, hwSelTimeout)
	defer cancel()
	tmp, err := os.CreateTemp("", "kids-shelf-hwsel-*.mp4")
	if err != nil {
		return false
	}
	name := tmp.Name()
	tmp.Close()
	defer os.Remove(name)
	if _, err := w.run(cctx, c.bin, c.selfTestArgs(selfTestProbe, name)...); err != nil {
		return false
	}
	info, err := ProbeFile(cctx, w.dir.FFprobe, name)
	if err != nil {
		return false
	}
	v := info.Video()
	if v == nil || v.CodecName != "h264" || !eightBitYUV420P(v.PixFmt) || v.NbFrames < 25 {
		return false
	}
	if _, err := w.run(cctx, w.dir.FFmpeg, "-v", "error", "-xerror", "-nostdin", "-i", name, "-f", "null", "-"); err != nil {
		return false
	}
	return true
}

// binaryEncoders lists the encoders of one helper binary; a binary that
// does not start or does not answer has none.
func (w *Worker) binaryEncoders(ctx context.Context, bin string) map[string]bool {
	cctx, cancel := context.WithTimeout(ctx, hwListTimeout)
	defer cancel()
	out, err := w.run(cctx, bin, "-hide_banner", "-encoders")
	if err != nil {
		return nil
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

// selectHardware probes the platform's candidates and returns the first
// that passes its self-test; nil means software.
func (w *Worker) selectHardware(ctx context.Context) *hwCandidate {
	return w.pickHardware(ctx, hwCandidates(w.dir))
}

// pickHardware probes the given candidates in preference order and returns
// the first that passes its self-test; nil means software. The outcome is
// logged once per process.
func (w *Worker) pickHardware(ctx context.Context, cands []hwCandidate) *hwCandidate {
	if len(cands) == 0 {
		log.Printf("ingest: hardware encode: no hardware candidates on this machine, using software")
		return nil
	}
	have := map[string]map[string]bool{}
	for i := range cands {
		c := &cands[i]
		if have[c.bin] == nil {
			have[c.bin] = w.binaryEncoders(ctx, c.bin)
		}
		if !have[c.bin][c.vcodec] {
			continue
		}
		if w.candidateSelfTest(ctx, c) {
			log.Printf("ingest: hardware encode: using %s (%s)", c.label, c.bin)
			return c
		}
	}
	log.Printf("ingest: hardware encode: no working hardware encoder, using software")
	return nil
}

// fullEncodeArgs builds the ffmpeg command line of a full re-encode: the
// candidate's global device args before -i (a device created after the
// input would not attach to the decoder), the plan's stream maps, the
// scale filter plus the candidate's filter chain, the encoder's args, and
// the audio and faststart args shared with the software path. enc is nil
// for the libx264 software path.
func (w *Worker) fullEncodeArgs(enc *hwCandidate, pl *payload, out string) []string {
	args := []string{"-y"}
	if enc != nil {
		args = append(args, enc.global...)
	}
	args = append(args, "-i", pl.File, "-map", "0:"+strconv.Itoa(pl.VideoIndex))
	if pl.AudioIndex >= 0 {
		args = append(args, "-map", "0:"+strconv.Itoa(pl.AudioIndex))
	}
	filter := scaleFilter(pl.Probe)
	if enc != nil && enc.chain != "" {
		filter += "," + enc.chain
	}
	args = append(args, "-vf", filter)
	if enc != nil {
		args = append(args, enc.encArgs(pl.Probe)...)
	} else {
		args = append(args,
			"-c:v", "libx264", "-preset", "veryfast", "-crf", "21",
			"-maxrate", "8M", "-bufsize", "16M", "-pix_fmt", "yuv420p", "-profile:v", "high")
	}
	return append(args, "-c:a", "aac", "-b:a", "160k", "-ac", "2", "-movflags", "+faststart", out)
}

// fullConvert runs an ActionFull re-encode with the process's chosen
// encoder. A hardware run that fails - anything but a full disk, which
// pauses the job - falls back to libx264 in the same stage, once per job,
// and counts toward the process-wide cutoff; a job that already retried
// (or whose output failed verify earlier) goes straight to software.
func (w *Worker) fullConvert(ctx context.Context, pl *payload, tmp string) ([]byte, error) {
	enc := w.chooseEncoder(ctx)
	if enc != nil && !pl.HwRetried {
		out, err := w.run(ctx, enc.bin, w.fullEncodeArgs(enc, pl, tmp)...)
		if err != nil {
			if isDiskFullErr(err, out) {
				return out, err
			}
			w.hw.noteFailure()
			pl.HwRetried = true
		} else {
			pl.EncHw = true
			return out, nil
		}
	}
	return w.run(ctx, w.dir.FFmpeg, w.fullEncodeArgs(nil, pl, tmp)...)
}

// hwState is the process-wide hardware selection: probed once, lazily,
// before the first full re-encode, and cached for the process. The
// household setting is read on every selection, so turning the graphics
// chip off stops hardware immediately without re-probing; three
// consecutive hardware failures cut hardware off for the rest of the
// process.
type hwState struct {
	mu      sync.Mutex
	setting func(ctx context.Context) (bool, error) // nil means on (the default)
	tried   bool                                    // the self-test selection ran
	chosen  *hwCandidate                            // first passing candidate; nil = software
	fails   int                                     // consecutive hardware failures
	cutoff  bool                                    // fails reached 3: software from now on
}

func newHWState() *hwState {
	return &hwState{}
}

// WithHardwareSetting wires the household's "use the graphics chip to
// convert videos" setting into the worker (internal/app passes the budget
// setting). A nil setting means on, the default.
func (w *Worker) WithHardwareSetting(hw func(ctx context.Context) (bool, error)) *Worker {
	w.hw.setting = hw
	return w
}

// chooseEncoder returns the hardware encoder for the next full re-encode,
// or nil for software. The self-test selection runs once, lazily, on the
// first call. chooseEncoder is only ever called from the fix lane, right
// before a full re-encode, never at startup and never from the sort lane.
func (w *Worker) chooseEncoder(ctx context.Context) *hwCandidate {
	return w.hw.choose(ctx, w.selectHardware)
}

// EncoderLabel is the parent page's "Video conversion uses:" name. It only
// reads the cached selection and the household setting; it never probes or
// encodes anything.
func (w *Worker) EncoderLabel() string {
	return w.hw.label()
}

// choose returns the encoder for the next full re-encode: the cached
// selection, rechecked against the household setting and the failure
// cutoff. nil means software. probe runs the self-test selection on the
// first call and is never consulted again.
func (h *hwState) choose(ctx context.Context, probe func(ctx context.Context) *hwCandidate) *hwCandidate {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.setting != nil {
		// An unreadable setting falls back to on (hardware stays
		// available); the safari setting uses the same default.
		if on, err := h.setting(ctx); err == nil && !on {
			return nil
		}
	}
	if h.cutoff {
		return nil
	}
	if !h.tried {
		h.chosen = probe(ctx)
		h.tried = true
	}
	return h.chosen
}

// noteFailure records one hardware failure: the encode itself, or the
// verify of a hardware-encoded output. Three in a row cut hardware off for
// the rest of the process.
func (h *hwState) noteFailure() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fails++
	if h.fails >= hwMaxConsecutiveFails {
		h.cutoff = true
		log.Printf("ingest: hardware encode: %d failures in a row, using software for the rest of this run", h.fails)
	}
}

// noteSuccess clears the consecutive-failure count: a hardware output that
// passed verify proves the encoder works, so the count starts over.
func (h *hwState) noteSuccess() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fails = 0
}

// label is the parent page's "Video conversion uses:" name: the chosen
// hardware encoder, "this computer's processor" for software (the
// household setting off included), "not checked yet" before the first
// selection, plus the cutoff suffix after three failures.
func (h *hwState) label() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.setting != nil {
		if on, err := h.setting(context.Background()); err == nil && !on {
			return "this computer's processor"
		}
	}
	name := "this computer's processor"
	if h.chosen != nil {
		name = h.chosen.label
	}
	if h.cutoff {
		return name + " (the graphics chip stopped working, so the processor took over)"
	}
	if !h.tried {
		return "not checked yet"
	}
	return name
}
