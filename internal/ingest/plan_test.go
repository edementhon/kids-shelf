package ingest

import (
	"errors"
	"testing"
)

func v(codec, pixFmt string) StreamInfo {
	return StreamInfo{CodecType: "video", CodecName: codec, PixFmt: pixFmt}
}

func a(codec, profile string, channels int) StreamInfo {
	return StreamInfo{CodecType: "audio", CodecName: codec, Profile: profile, Channels: channels}
}

// coverArt is an attached-picture video stream (album art): a video track
// players must never treat as the picture to play.
var coverArt = StreamInfo{CodecType: "video", CodecName: "mjpeg", Disposition: Disposition{AttachedPic: 1}}

func mp4(streams ...StreamInfo) *ProbeInfo {
	return &ProbeInfo{FormatName: "mov,mp4,m4a,3gp,3g2,mj2", Streams: streams}
}

func matroska(streams ...StreamInfo) *ProbeInfo {
	return &ProbeInfo{FormatName: "matroska,webm", Streams: streams}
}

func TestPlanTable(t *testing.T) {
	h264 := v("h264", "yuv420p")
	h264_10bit := v("h264", "yuv420p10le")
	aacLC := a("aac", "LC", 2)
	tests := []struct {
		name    string
		p       *ProbeInfo
		want    Action
		wantErr error
	}{
		{"inplace: mp4 h264+aac, faststart not required", mp4(h264, aacLC), ActionInplace, nil},
		{"remux: same codecs, mkv container", matroska(h264, aacLC), ActionRemux, nil},
		{"inplace: mov container h264+aac", &ProbeInfo{FormatName: "mov", Streams: []StreamInfo{h264, aacLC}}, ActionInplace, nil},
		{"audio: ac3 track", matroska(h264, a("ac3", "", 6)), ActionAudio, nil},
		{"audio: dts track", matroska(h264, a("dca", "", 2)), ActionAudio, nil},
		{"audio: aac but 8 channels", matroska(h264, a("aac", "LC", 8)), ActionAudio, nil},
		{"audio: he-aac profile", mp4(h264, a("aac", "HE-AAC", 2)), ActionAudio, nil},
		{"remux: attached-pic cover first, real video second", matroska(coverArt, h264, aacLC), ActionRemux, nil},
		{"inplace: mp4 with attached-pic cover, all audio fine", mp4(coverArt, h264, aacLC), ActionInplace, nil},
		{"remux: mp4 with a second ac3 track (nothing to drop in place)", mp4(h264, aacLC, a("ac3", "", 2)), ActionRemux, nil},
		{"no video stream: only attached artwork", matroska(coverArt, aacLC), "", errNoVideo},
		{"full: h264 10-bit", mp4(h264_10bit, aacLC), ActionFull, nil},
		{"full: h264 4:4:4", mp4(v("h264", "yuv444p"), aacLC), ActionFull, nil},
		{"full: vp9", matroska(v("vp9", "yuv420p"), aacLC), ActionFull, nil},
		{"full: av1", matroska(v("av1", "yuv420p"), aacLC), ActionFull, nil},
		{"full: mpeg4", matroska(v("mpeg4", "yuv420p"), aacLC), ActionFull, nil},
		{"inplace: no audio, mp4", mp4(h264), ActionInplace, nil},
		{"remux: no audio, mkv", matroska(h264), ActionRemux, nil},
		{"no video stream", mp4(aacLC), "", errNoVideo},
		{"no streams at all", mp4(), "", errNoVideo},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Plan(tt.p)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Plan err = %v, want %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("Plan = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAudioOK(t *testing.T) {
	if !audioOK(nil) {
		t.Error("no audio streams must pass (nothing to fix)")
	}
	if !audioOK([]StreamInfo{a("aac", "LC", 2), a("aac", "LC", 1)}) {
		t.Error("several aac-lc streams must pass")
	}
	if audioOK([]StreamInfo{a("aac", "LC", 2), a("ac3", "", 2)}) {
		t.Error("one non-aac stream must fail")
	}
	if audioOK([]StreamInfo{a("aac", "LC", 7)}) {
		t.Error("7-channel aac must fail")
	}
}

// TestSelectStreams pins the stream choice: the first video that is not
// attached artwork, and the default audio falling back to the first.
func TestSelectStreams(t *testing.T) {
	h264 := v("h264", "yuv420p")
	aac := a("aac", "LC", 2)
	def := func(s StreamInfo) StreamInfo {
		s.Disposition.Default = 1
		return s
	}
	tests := []struct {
		name         string
		p            *ProbeInfo
		wantV, wantA int
	}{
		{"plain video and audio", mp4(h264, aac), 0, 1},
		{"attached art skipped", mp4(coverArt, h264, aac), 1, 2},
		{"default flag beats first", mp4(h264, aac, def(a("aac", "LC", 1))), 0, 2},
		{"no default falls back to first", mp4(h264, aac, a("aac", "LC", 1)), 0, 1},
		{"no audio", mp4(h264), 0, -1},
		{"audio only, no video", mp4(aac), -1, 0},
		{"artwork only, no video", mp4(coverArt), -1, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, a := tt.p.selectStreams()
			if v != tt.wantV || a != tt.wantA {
				t.Errorf("selectStreams = (%d, %d), want (%d, %d)", v, a, tt.wantV, tt.wantA)
			}
		})
	}
}
