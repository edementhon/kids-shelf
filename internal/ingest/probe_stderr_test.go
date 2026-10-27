//go:build !windows

package ingest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProbeFileIgnoresWarningsOnStderr: ffprobe prints warnings on stderr
// even when it succeeds (a Matroska subtitle track compressed in a way the
// build does not read says "Unsupported encoding type"). Mixing them into
// the JSON on stdout made 25 Blu-ray MKVs of a real library look damaged
// and kept them off the shelf (2026-10-27).
func TestProbeFileIgnoresWarningsOnStderr(t *testing.T) {
	fake := func(body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "ffprobe")
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ok := fake(`echo '[matroska,webm @ 0x1] Unsupported encoding type' >&2
echo '{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","pix_fmt":"yuv420p","width":1920,"height":1080}],"format":{"format_name":"matroska,webm"}}'
`)
	info, err := ProbeFile(context.Background(), ok, "/videos/show.mkv")
	if err != nil {
		t.Fatalf("ProbeFile with a stderr warning: %v (the warning must not spoil the JSON)", err)
	}
	if v := info.Video(); v == nil || v.CodecName != "h264" {
		t.Errorf("video stream = %+v, want the h264 stream", v)
	}

	// A real failure still fails, and says what ffprobe said.
	bad := fake(`echo '/videos/gone.mkv: No such file or directory' >&2
exit 1
`)
	if _, err := ProbeFile(context.Background(), bad, "/videos/gone.mkv"); err == nil || !strings.Contains(err.Error(), "No such file") {
		t.Errorf("ProbeFile on a failing ffprobe = %v, want an error carrying ffprobe's message", err)
	}
}
