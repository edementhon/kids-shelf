package ingest

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Disposition is the ffprobe disposition flags the pipeline decides on:
// attached_pic marks attached artwork (album cover art), which is a video
// stream but never the picture to play, and default marks the stream the
// source flags as the one to play.
type Disposition struct {
	Default     int `json:"default"`
	AttachedPic int `json:"attached_pic"`
}

// StreamInfo is one stream as ffprobe reports it, trimmed to the fields the
// pipeline decides on.
type StreamInfo struct {
	Index       int               `json:"index"`
	CodecType   string            `json:"codec_type"`
	CodecName   string            `json:"codec_name"`
	Profile     string            `json:"profile"`
	PixFmt      string            `json:"pix_fmt"`
	Width       int               `json:"width"`
	Height      int               `json:"height"`
	Channels    int               `json:"channels"`
	BitRate     bitrate           `json:"bit_rate"`
	Disposition Disposition       `json:"disposition"`
	SideData    []SideData        `json:"side_data_list,omitempty"`
	Tags        map[string]string `json:"tags,omitempty"`
}

// SideData is one entry of ffprobe's side_data_list; the pipeline reads the
// Display Matrix's rotation (how the coded frame must turn to play upright).
type SideData struct {
	Type     string `json:"side_data_type"`
	Rotation int    `json:"rotation"`
}

// bitrate is a bits-per-second value ffprobe prints as a JSON string
// ("2554268", or "N/A" when it is unknown); accept a bare number too. An
// unknown rate is 0.
type bitrate int64

func (b *bitrate) UnmarshalJSON(data []byte) error {
	var n int64
	if err := json.Unmarshal(data, &n); err == nil {
		*b = bitrate(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	n, _ = strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	*b = bitrate(n)
	return nil
}

// duration is a seconds value ffprobe prints as a JSON string ("52.208333");
// accept a bare number too.
type duration float64

func (d *duration) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		f, perr := strconv.ParseFloat(s, 64)
		if perr != nil {
			return fmt.Errorf("duration %q: %w", s, perr)
		}
		*d = duration(f)
		return nil
	}
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	*d = duration(f)
	return nil
}

// ProbeInfo is the shape of `ffprobe -v error -print_format json
// -show_format -show_streams` output, flattened: the format object's fields
// sit next to the stream list.
type ProbeInfo struct {
	FormatName string       `json:"-"`
	Duration   duration     `json:"-"`
	Streams    []StreamInfo `json:"streams"`
}

// UnmarshalJSON reads ffprobe's nested {"format": {...}, "streams": [...]}
// into the flat struct.
func (p *ProbeInfo) UnmarshalJSON(b []byte) error {
	var raw struct {
		Format struct {
			FormatName string   `json:"format_name"`
			Duration   duration `json:"duration"`
		} `json:"format"`
		Streams []StreamInfo `json:"streams"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	p.FormatName = raw.Format.FormatName
	p.Duration = raw.Format.Duration
	p.Streams = raw.Streams
	return nil
}

// MarshalJSON writes the same nested shape UnmarshalJSON reads, so a job
// payload carrying a probe survives its round trip through the jobs table.
func (p ProbeInfo) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Format struct {
			FormatName string   `json:"format_name"`
			Duration   duration `json:"duration"`
		} `json:"format"`
		Streams []StreamInfo `json:"streams"`
	}{
		Format: struct {
			FormatName string   `json:"format_name"`
			Duration   duration `json:"duration"`
		}{p.FormatName, p.Duration},
		Streams: p.Streams,
	})
}

// ProbeFile inspects path with ffprobe and returns the parsed report. Any
// probe failure - ffprobe missing, the file unreadable, the stream tables
// missing - surfaces as a non-nil error. It is exported for cmd/ks-survey,
// which probes whole folders with the exact same helper call ingest uses.
func ProbeFile(ctx context.Context, ffprobe, path string) (*ProbeInfo, error) {
	out, err := runCombined(ctx, ffprobe,
		"-v", "error", "-print_format", "json", "-show_format", "-show_streams", path)
	if err != nil {
		return nil, fmt.Errorf("ingest: probe %s: %w", path, err)
	}
	var p ProbeInfo
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, fmt.Errorf("ingest: probe %s: parse: %w", path, err)
	}
	return &p, nil
}

// Video returns the first video stream, or nil when the file has none.
func (p *ProbeInfo) Video() *StreamInfo {
	for i := range p.Streams {
		if p.Streams[i].CodecType == "video" {
			return &p.Streams[i]
		}
	}
	return nil
}

// Audio returns the audio streams in order.
func (p *ProbeInfo) Audio() []StreamInfo {
	var a []StreamInfo
	for _, s := range p.Streams {
		if s.CodecType == "audio" {
			a = append(a, s)
		}
	}
	return a
}

// selectStreams picks the streams the normalized output keeps: the first
// video stream that is not attached artwork (cover art), and the default
// audio stream, falling back to the first audio stream when none is flagged
// default. aIdx is -1 when the file has no audio. The returned values are
// positions in Streams, which equal the ffprobe stream indices (the 0-based
// stream numbers ffmpeg -map understands).
func (p *ProbeInfo) selectStreams() (vIdx, aIdx int) {
	vIdx, aIdx = -1, -1
	for i := range p.Streams {
		s := &p.Streams[i]
		switch {
		case s.CodecType == "video" && s.Disposition.AttachedPic == 0 && vIdx < 0:
			vIdx = i
		case s.CodecType == "audio" && aIdx < 0:
			aIdx = i
		}
	}
	for i := range p.Streams {
		s := &p.Streams[i]
		if s.CodecType == "audio" && s.Disposition.Default == 1 {
			aIdx = i
			break
		}
	}
	return vIdx, aIdx
}

// rotation returns how many degrees clockwise the stream's frame must turn
// to display upright (0, 90, 180, 270): the value of the Display Matrix side
// data, or the legacy rotate tag when there is no matrix (matroska writes
// it as ROTATE).
func (s *StreamInfo) rotation() int {
	r := 0
	for _, sd := range s.SideData {
		if sd.Type == "Display Matrix" {
			r = sd.Rotation
			break
		}
	}
	if r == 0 {
		for k, v := range s.Tags {
			if strings.EqualFold(k, "rotate") {
				if n, err := strconv.Atoi(v); err == nil {
					r = n
				}
				break
			}
		}
	}
	r %= 360
	if r < 0 {
		r += 360
	}
	return r
}

// IsMP4 reports whether the container is an MP4-family format, what the
// player requires (that and faststart make a file play from the start over
// the home network without buffering the whole file).
func (p *ProbeInfo) IsMP4() bool {

	for _, n := range strings.Split(p.FormatName, ",") {
		if n == "mov" {
			return true
		}
	}
	return false
}

// eightBitYUV420P reports whether the pixel format is 8-bit 4:2:0, the only
// variant every target device decodes in hardware-agnostic software. 10-bit
// and 4:4:4 variants (yuv420p10le, yuv444p, ...) are false.
func eightBitYUV420P(pixFmt string) bool {
	return pixFmt == "yuv420p"
}

// aacLC reports whether a stream is AAC-Low-Complexity, the audio profile
// the target players all support.
func aacLC(s StreamInfo) bool {
	return s.CodecName == "aac" && s.Profile == "LC"
}

// moovBeforeMdat walks the top-level boxes of an MP4-family file and reports
// whether the moov (index) box starts before the mdat (media data) box -
// "faststart". A file with no moov is false; a file with moov but no mdat
// reports true (there is nothing to reorder, and ffprobe would have rejected
// a moov-less or dataless file already).
//
// The walk rejects a box whose declared size reaches past the end of the
// file: a truncated faststart MP4 keeps its moov (the index, at the front)
// while its mdat's declared size no longer fits, and ffprobe - which only
// reads the index - would accept such a file. Trailing garbage after the
// last complete box is tolerated, so the scan stops on a partial header the
// same way it stops at a clean end of file.
func moovBeforeMdat(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false, err
	}
	fileSize := st.Size()
	var moov, mdat int64 = -1, -1
	pos := int64(0)
	var head [16]byte
	for {
		if _, err := io.ReadFull(f, head[:8]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break // trailing garbage or truncated tail; nothing more to scan
			}
			return false, err
		}
		size := int64(binary.BigEndian.Uint32(head[0:4]))
		typ := string(head[4:8])
		off := int64(8)
		if size == 1 { // 64-bit extended size follows the header
			if _, err := io.ReadFull(f, head[8:16]); err != nil {
				return false, err
			}
			size = int64(binary.BigEndian.Uint64(head[8:16]))
			off = 16
		} else if size == 0 { // box runs to end of file
			size = fileSize - pos
		}
		if size < off || pos+size > fileSize {
			return false, fmt.Errorf("ingest: box %s at %d runs past the end of the file (size %d, file %d)", typ, pos, size, fileSize)
		}
		switch typ {
		case "moov":
			moov = pos
		case "mdat":
			mdat = pos
		}
		pos += size
		if _, err := f.Seek(pos, io.SeekStart); err != nil {
			return false, err
		}
	}
	if moov == -1 {
		return false, nil
	}
	if mdat == -1 {
		return true, nil
	}
	return moov < mdat, nil
}

// durationSeconds returns the probed duration in seconds as an integer,
// rounded to the nearest second (what the watch log and resume positions
// use).
func (p *ProbeInfo) durationSeconds() int64 {
	return int64(p.Duration + 0.5)
}
