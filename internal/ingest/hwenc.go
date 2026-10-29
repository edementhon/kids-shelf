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
// encoder's own options. Every candidate takes the resolution-based -b:v
// average and a rate-control mode that honors the shared -maxrate and
// -bufsize caps (see encArgs).
type hwCandidate struct {
	label  string
	bin    string
	global []string
	chain  string
	vcodec string
	vopts  []string
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
			label:  "Apple video engine (VideoToolbox)",
			bin:    dir.FFmpeg,
			vcodec: "h264_videotoolbox",
			// -pix_fmt yuv420p: 8-bit 4:2:0 output, the only variant
			// every target device decodes. -profile:v high: H.264
			// High profile. The bitrate and the -maxrate cap become
			// AverageBitRate and DataRateLimits (videotoolboxenc.c).
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
				vopts:  nvencOpts,
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
				// No options of its own: a bitrate below the
				// -maxrate cap and no -global_quality select VBR,
				// which carries MaxKbps and the buffer size
				// (qsvenc.c select_rc_mode); -global_quality
				// would make it ICQ or CQP, which have no cap.
			},
			{
				label:  "AMD graphics (AMF)",
				bin:    dir.FFmpeg,
				vcodec: "h264_amf",
				// -quality balanced: AMF's speed-versus-size
				// balance. -rc vbr_peak: peak-constrained VBR,
				// the -b:v average under the -maxrate peak and
				// the -bufsize VBV buffer (constant-QP mode would
				// ignore all three).
				vopts: []string{"-quality", "balanced", "-rc", "vbr_peak"},
			},
			{
				label:  "Windows video encoder (Media Foundation)",
				bin:    dir.FFmpeg,
				vcodec: "h264_mf",
				// -hw_encoding 1: only a hardware encoder will do
				// (the default lets Media Foundation fall back to
				// Microsoft's software one, which would pass the
				// self-test while being no faster than libx264).
				// -profile:v high: H.264 High profile where Media
				// Foundation exposes it; an encoder that does not
				// take the option only warns and keeps its own
				// default. -rate_control pc_vbr: peak-constrained
				// VBR, so the -maxrate cap (AVEncCommonMaxBitRate)
				// applies.
				vopts: []string{"-hw_encoding", "1", "-profile:v", "high", "-rate_control", "pc_vbr"},
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
					// No -rc_mode and no -global_quality: with a
					// bitrate below -maxrate and a -bufsize, ffmpeg
					// tries VBR (capped at -maxrate under the HRD
					// buffer), then CBR at -b:v, whichever the
					// driver supports (vaapi_encode.c "Rate control
					// mode selection"). A quality would try QVBR,
					// then ICQ and CQP, which have no bitrate cap.
				})
			}
			return append(cands,
				hwCandidate{
					label:  "NVIDIA graphics (NVENC)",
					bin:    dir.FFmpegHW,
					vcodec: "h264_nvenc",
					vopts:  nvencOpts,
				},
				hwCandidate{
					label:  "Intel graphics (Quick Sync)",
					bin:    dir.FFmpegHW,
					vcodec: "h264_qsv",
					global: []string{"-init_hw_device", "qsv=hw", "-filter_hw_device", "hw"},
					chain:  "format=nv12,hwupload",
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
				label:  "Raspberry Pi video encoder",
				bin:    dir.FFmpeg,
				vcodec: "h264_v4l2m2m",
				// -pix_fmt yuv420p: 8-bit 4:2:0 frames, what the Pi
				// encoder takes and every target device decodes.
				// ffmpeg's V4L2 encoder sets only the -b:v average
				// with frame-level rate control and never reads
				// -maxrate; the -fs output cap (fullEncodeArgs)
				// bounds its output.
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

// nvencOpts are NVENC's options on every platform: -preset p5, the
// slower, better-quality tier; -tune hq, high-quality tuning; -rc vbr,
// variable bitrate around the -b:v average under the -maxrate peak and the
// -bufsize VBV buffer. No -cq: a target quality makes NVENC drop the
// average and the VBV buffer (nvenc.c, "CQ mode shall discard avg
// bitrate/vbv buffer size").
var nvencOpts = []string{"-preset", "p5", "-tune", "hq", "-rc", "vbr"}

// encArgs returns the encoder's -c:v, the resolution-based -b:v average,
// its own options, and the shared -maxrate 8M -bufsize 16M caps of the
// software path. Every candidate's options select a rate-control mode that
// honors the caps (no constant-quality or constant-QP mode), so the disk
// reservation (reservedBytes, 8.16 Mbit/s worst case) stays honest;
// TestHWRateControlBounds pins each one against the FFmpeg source.
func (c *hwCandidate) encArgs(p *ProbeInfo) []string {
	args := []string{"-c:v", c.vcodec, "-b:v", bitrateTarget(p)}
	args = append(args, c.vopts...)
	return append(args, "-maxrate", "8M", "-bufsize", "16M")
}

// bitrateTarget picks the average bitrate of the hardware encoders: about
// 5 Mbit/s at 1080p, 3 at 720p, 1.5 below, never over the 8M cap. The
// class follows the display height - the smaller dimension after the
// rotation turn, which is 1080 for both a 1920x1080 landscape frame and a
// phone's 1080x1920 portrait - exactly the size the scale filter clamps
// to, so a 4K input lands in the 1080p class.
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
// for the libx264 software path. A hardware run also carries -fs at the
// job's disk reservation: ffmpeg stops writing once the file reaches it,
// so a driver that ignores the requested rate control (Media Foundation
// does not check its settings; ffmpeg's V4L2 encoder has no peak cap at
// all) can never write past the reservation. libx264's own -maxrate and
// -bufsize are enforced, so the software path needs no -fs.
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
		args = append(args, "-fs", strconv.FormatUint(reservedBytes(pl), 10))
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
// (or whose output failed verify earlier) goes straight to software. A
// hardware output that reached the -fs cap was cut short by ffmpeg (its
// encoder overshot the bitrate the reservation assumes), so it counts as
// a failed hardware run too.
func (w *Worker) fullConvert(ctx context.Context, pl *payload, tmp string) ([]byte, error) {
	enc := w.chooseEncoder(ctx)
	if enc != nil && !pl.HwRetried {
		out, err := w.run(ctx, enc.bin, w.fullEncodeArgs(enc, pl, tmp)...)
		switch {
		case err != nil && isDiskFullErr(err, out):
			return out, err
		case err == nil && !reachedCap(tmp, reservedBytes(pl)):
			pl.EncHw = true
			return out, nil
		case err == nil:
			log.Printf("ingest: hardware encode: %s output reached the %d-byte reservation, redoing with software", enc.label, reservedBytes(pl))
		}
		w.hw.noteFailure()
		pl.HwRetried = true
	}
	return w.run(ctx, w.dir.FFmpeg, w.fullEncodeArgs(nil, pl, tmp)...)
}

// reachedCap reports whether the file at path has reached limit bytes, the
// -fs cap at which ffmpeg stops writing packets (fftools/ffmpeg_mux.c
// write_packet). A file that cannot be read has not.
func reachedCap(path string, limit uint64) bool {
	st, err := os.Stat(path)
	return err == nil && uint64(st.Size()) >= limit
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
