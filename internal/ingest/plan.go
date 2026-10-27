package ingest

import "errors"

// Action is how an input file is turned into a playable episode.
type Action string

const (
	// ActionInplace publishes the original file itself: it is already an
	// MP4-family file whose H.264 8-bit yuv420p video and AAC-LC audio (at
	// most 6 channels, or none) play in the browser, so nothing is copied
	// or rewritten. Faststart is NOT required: the media handler serves
	// Range requests and browsers fetch the moov at the end. The original
	// is never modified, moved or deleted; only its path is recorded.
	ActionInplace Action = "inplace"

	// ActionKeep publishes the file's bytes unchanged. Only YouTube
	// downloads take this path today: their files stay app-owned in the
	// media folder, so a faststart MP4 is copied verbatim instead of
	// playing from yt-dlp's download path (which the decision keeps
	// unchanged). Every folder/NAS file that used to be kept is now
	// classified ActionInplace.
	ActionKeep Action = "keep"

	// ActionRemux re-containers losslessly (-c copy -movflags +faststart):
	// the codecs are right but the container is not MP4, or extra tracks
	// the players would reject must be dropped.
	ActionRemux Action = "remux"

	// ActionAudio keeps the H.264 video bytes and re-encodes the audio to
	// AAC-LC stereo 160 kbit/s (the video plays but the audio is a format
	// some devices reject, or has more channels than they decode).
	ActionAudio Action = "audio"

	// ActionFull re-encodes everything: the video is not H.264 8-bit
	// yuv420p (HEVC, VP9, AV1, MPEG-4, 10-bit, ...), so it becomes libx264
	// `-preset veryfast -crf 21 -pix_fmt yuv420p -profile:v high`, scaled
	// down to at most 1920x1080 keeping the aspect ratio, with AAC-LC
	// stereo 160 kbit/s audio.
	ActionFull Action = "full"
)

// errNoVideo reports an input that has no video stream at all.
var errNoVideo = errors.New("ingest: no video stream")

// Plan chooses the single action for a probed file, by the streams
// selectStreams picks - the first video that is not attached artwork, and
// the default audio - because remux, audio and full rewrite the file and
// keep only those. An MP4-family container whose H.264 8-bit video and all
// AAC-LC (at most 6 channels) audio already play in the browser is played
// in place, whatever the moov position; a non-MP4 container with the same
// codecs is remuxed; bad audio re-encodes the audio; any other video
// re-encodes everything. It returns errNoVideo for files without a video
// stream, which cannot be played at all.
//
// Plan is exported for cmd/ks-survey, which classifies whole folders with
// the exact same rule ingest uses.
func Plan(p *ProbeInfo) (Action, error) {
	vIdx, aIdx := p.selectStreams()
	if vIdx < 0 {
		return "", errNoVideo
	}
	v := &p.Streams[vIdx]
	h264 := v.CodecName == "h264" && eightBitYUV420P(v.PixFmt)
	chosenOK := aIdx < 0 || (aacLC(p.Streams[aIdx]) && p.Streams[aIdx].Channels <= 6)
	switch {
	case h264 && chosenOK && p.IsMP4() && audioOK(p.Audio()):
		// Played in place from the original path: every audio stream must
		// be player-safe, because nothing is rewritten to drop the tracks
		// the players would reject. Faststart is irrelevant.
		return ActionInplace, nil
	case h264 && chosenOK:
		return ActionRemux, nil
	case h264:
		return ActionAudio, nil
	default:
		return ActionFull, nil
	}
}

// audioOK reports whether every audio stream is AAC-LC with at most 6
// channels, the condition for keeping or copying an audio track. A file with
// no audio stream passes: there is nothing to fix, and the audio-less video
// is still playable.
func audioOK(streams []StreamInfo) bool {
	for _, s := range streams {
		if !aacLC(s) || s.Channels > 6 {
			return false
		}
	}
	return true
}
