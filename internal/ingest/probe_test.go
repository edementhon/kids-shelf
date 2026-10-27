package ingest

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// box writes one top-level MP4 box: 4-byte big-endian size, 4-byte type,
// payload.
func box(typ string, payload []byte) []byte {
	b := make([]byte, 4+4+len(payload))
	binary.BigEndian.PutUint32(b[0:4], uint32(len(b)))
	copy(b[4:8], typ)
	copy(b[8:], payload)
	return b
}

// writeBoxes writes a file made of the given boxes, then returns its path.
func writeBoxes(t *testing.T, boxes ...[]byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "video.mp4")
	if err := os.WriteFile(path, bytes.Join(boxes, nil), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func sizedBox(typ string, size int) []byte {
	b := make([]byte, size)
	copy(b[4:8], typ)
	binary.BigEndian.PutUint32(b[0:4], uint32(size))
	return b
}

func TestMoovBeforeMdat(t *testing.T) {
	ftyp := box("ftyp", bytes.Repeat([]byte{0}, 8))
	moov := sizedBox("moov", 24)
	mdat := sizedBox("mdat", 100)
	tests := []struct {
		name string
		file func(t *testing.T) string
		want bool
	}{
		{"moov then mdat (faststart)", func(t *testing.T) string { return writeBoxes(t, ftyp, moov, mdat) }, true},
		{"mdat then moov", func(t *testing.T) string { return writeBoxes(t, ftyp, mdat, moov) }, false},
		{"no moov", func(t *testing.T) string { return writeBoxes(t, ftyp, mdat) }, false},
		{"moov only", func(t *testing.T) string { return writeBoxes(t, ftyp, moov) }, true},
		{"trailing garbage after moov", func(t *testing.T) string {
			return writeBoxes(t, ftyp, moov, append(mdat, 1, 2, 3))
		}, true},
		{"moov inside later box chain", func(t *testing.T) string {
			return writeBoxes(t, ftyp, box("free", bytes.Repeat([]byte{0}, 32)), moov, mdat)
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.file(t)
			got, err := moovBeforeMdat(path)
			if err != nil {
				t.Fatalf("moovBeforeMdat: %v", err)
			}
			if got != tt.want {
				t.Errorf("moovBeforeMdat = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMoovBeforeMdatMissingFile(t *testing.T) {
	if _, err := moovBeforeMdat(filepath.Join(t.TempDir(), "nope.mp4")); err == nil {
		t.Error("missing file accepted")
	}
}

func TestMoovBeforeMdat64BitSizes(t *testing.T) {
	// A box with the 64-bit extended size form must still be walked.
	ftyp := box("ftyp", bytes.Repeat([]byte{0}, 8))
	var wide []byte
	wide = append(wide, 0, 0, 0, 1)               // size == 1: extended form follows
	wide = append(wide, 'm', 'd', 'a', 't')       // type
	wide = append(wide, 0, 0, 0, 0, 0, 0, 0, 8+8) // 64-bit size incl. header
	path := writeBoxes(t, ftyp, wide, sizedBox("moov", 24))
	got, err := moovBeforeMdat(path)
	if err != nil {
		t.Fatalf("moovBeforeMdat: %v", err)
	}
	if got {
		t.Error("moovBeforeMdat = true, want false (mdat came first)")
	}
}

// TestMoovBeforeMdatRejectsBoxesPastEOF pins the damaged-file guard: a box
// whose declared size runs past the end of the file is an error, because a
// truncated faststart MP4 keeps its moov (the index) at the front while the
// mdat no longer fits, and ffprobe would accept it.
func TestMoovBeforeMdatRejectsBoxesPastEOF(t *testing.T) {
	ftyp := box("ftyp", bytes.Repeat([]byte{0}, 8))
	moov := sizedBox("moov", 24)
	tests := []struct {
		name string
		file string
	}{
		// Each box declares more bytes than the file actually holds,
		// exactly what a truncated faststart mdat does.
		{"mdat declares more than the file holds", func() string {
			mdat := sizedBox("mdat", 1000)
			return writeBoxes(t, ftyp, moov, mdat[:500])
		}()},
		{"moov declares more than the file holds", func() string {
			moovBig := sizedBox("moov", 1000)
			return writeBoxes(t, ftyp, moovBig[:300])
		}()},
		{"64-bit box runs past eof", func() string {
			wide := append([]byte{0, 0, 0, 1, 'm', 'd', 'a', 't'}, make([]byte, 8)...)
			binary.BigEndian.PutUint64(wide[8:16], 1000)
			return writeBoxes(t, ftyp, moov, wide)
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := moovBeforeMdat(tt.file); err == nil {
				t.Error("box past end of file accepted")
			}
		})
	}
}

func TestProbeInfoAccessors(t *testing.T) {
	h264 := StreamInfo{CodecType: "video", CodecName: "h264", PixFmt: "yuv420p"}
	aac := StreamInfo{CodecType: "audio", CodecName: "aac", Profile: "LC", Channels: 2}
	mp4 := &ProbeInfo{
		FormatName: "mov,mp4,m4a,3gp,3g2,mj2",
		Duration:   52.6,
		Streams:    []StreamInfo{h264, aac},
	}
	if v := mp4.Video(); v == nil || v.CodecName != "h264" {
		t.Errorf("Video() = %+v", v)
	}
	if a := mp4.Audio(); len(a) != 1 || a[0].CodecName != "aac" {
		t.Errorf("Audio() = %+v", a)
	}
	if !mp4.IsMP4() {
		t.Error("IsMP4() = false for mov family")
	}
	if got := mp4.durationSeconds(); got != 53 {
		t.Errorf("durationSeconds() = %d, want 53", got)
	}
	webm := &ProbeInfo{FormatName: "matroska,webm"}
	if webm.IsMP4() {
		t.Error("IsMP4() = true for matroska")
	}
	if !eightBitYUV420P("yuv420p") || eightBitYUV420P("yuv420p10le") || eightBitYUV420P("") {
		t.Error("eightBitYUV420P misclassifies pixel formats")
	}
	if !aacLC(aac) || aacLC(StreamInfo{CodecName: "aac", Profile: "HE-AAC"}) || aacLC(StreamInfo{CodecName: "ac3"}) {
		t.Error("aacLC misclassifies audio profiles")
	}
	if noVideo := (&ProbeInfo{Streams: []StreamInfo{aac}}); noVideo.Video() != nil {
		t.Error("Video() returned a stream for an audio-only file")
	}
}

func TestDurationUnmarshal(t *testing.T) {
	var p ProbeInfo
	if err := json.Unmarshal([]byte(`{"format": {"format_name": "mov", "duration": "5.250000"}}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.FormatName != "mov" {
		t.Errorf("format_name = %q", p.FormatName)
	}
	if p.Duration != 5.25 {
		t.Errorf("string duration = %v, want 5.25", p.Duration)
	}
	p = ProbeInfo{}
	if err := json.Unmarshal([]byte(`{"format": {"duration": 5.25}}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.Duration != 5.25 {
		t.Errorf("number duration = %v", p.Duration)
	}
	p = ProbeInfo{}
	if err := json.Unmarshal([]byte(`{"format": {"duration": null}}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.Duration != 0 {
		t.Errorf("null duration = %v", p.Duration)
	}
}

// TestProbeInfoPayloadRoundTrip checks that a probe survives its trip
// through a job payload (json.Marshal in jobs, json.Unmarshal on resume).
func TestProbeInfoPayloadRoundTrip(t *testing.T) {
	p := &ProbeInfo{
		FormatName: "mov,mp4,m4a,3gp,3g2,mj2",
		Duration:   52.6,
		Streams: []StreamInfo{
			{CodecType: "video", CodecName: "h264", PixFmt: "yuv420p"},
			{CodecType: "audio", CodecName: "aac", Profile: "LC", Channels: 2},
		},
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got ProbeInfo
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.FormatName != p.FormatName || got.Duration != p.Duration {
		t.Errorf("round trip lost format info: %+v", got)
	}
	if len(got.Streams) != 2 || got.Streams[1].Channels != 2 {
		t.Errorf("round trip lost streams: %+v", got.Streams)
	}
}
