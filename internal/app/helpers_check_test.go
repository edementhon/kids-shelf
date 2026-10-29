package app

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/edementhon/kids-shelf/internal/ingest"
	"github.com/edementhon/kids-shelf/internal/jobs"
	"github.com/edementhon/kids-shelf/internal/tools"
)

// fakeHelpersDir writes trivial helper scripts that exit 0; the video
// helpers start out not executable, the way Windows Smart App Control
// refused a freshly extracted ffmpeg.exe and ffprobe.exe.
func fakeHelpersDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script helpers")
	}
	dir := t.TempDir()
	for name, mode := range map[string]os.FileMode{"ffmpeg": 0o644, "ffprobe": 0o644, "qjs": 0o755, "kiwix-serve": 0o755} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestHelpersCheckResumesBlockedJobs: while ffmpeg and ffprobe cannot
// start, the helpers check reports the problem and leaves the jobs that
// wait for them paused; once they start, a later pass clears the problem
// and puts those jobs back in the queue, without a restart. Jobs paused
// for another reason stay paused.
func TestHelpersCheckResumesBlockedJobs(t *testing.T) {
	helpers := fakeHelpersDir(t)
	cfg := testConfig(t, t.TempDir())
	cfg.HelpersDir = helpers
	a, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { a.Close() })
	paused := func(reason string) string {
		t.Helper()
		id, err := a.queue.Enqueue(ingest.JobKind, map[string]string{"file": "/videos/a.mkv"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.queue.Claim(ingest.JobKind); err != nil {
			t.Fatal(err)
		}
		if err := a.queue.Pause(id, reason); err != nil {
			t.Fatal(err)
		}
		return id
	}
	waiting := paused(ingest.MsgHelpersWaiting)
	diskFull := paused("The disk is full.")
	state := func(id string) string {
		t.Helper()
		j, err := a.queue.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		return j.State
	}
	ctx := context.Background()

	if err := a.checkHelpers(ctx); err != nil {
		t.Fatalf("checkHelpers: %v", err)
	}
	if p := a.helpersProblemFunc(); !strings.Contains(p, "ffmpeg and ffprobe did not start") {
		t.Errorf("problem while blocked = %q, want the video helpers named", p)
	}
	if got := state(waiting); got != jobs.StatePaused {
		t.Fatalf("waiting job is %s while the helpers are blocked, want paused", got)
	}

	for _, name := range []string{"ffmpeg", "ffprobe"} {
		if err := os.Chmod(filepath.Join(helpers, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.checkHelpers(ctx); err != nil {
		t.Fatalf("checkHelpers: %v", err)
	}
	if p := a.helpersProblemFunc(); p != "" {
		t.Errorf("problem after the helpers start = %q, want none", p)
	}
	if got := state(waiting); got != jobs.StateQueued {
		t.Errorf("waiting job is %s after the helpers started, want queued", got)
	}
	if got := state(diskFull); got != jobs.StatePaused {
		t.Errorf("disk-full job is %s, want still paused", got)
	}
}

// TestHelpersProblemWords: on Windows, video helpers the system refused to
// start get the plain Smart App Control words; elsewhere, or for another
// cause (a missing or crashing helper), the generic words stay. The
// libraries part is kept either way.
func TestHelpersProblemWords(t *testing.T) {
	blockedVideo := tools.SelfTestResult{FFmpeg: false, FFprobe: true, Qjs: true, KiwixServe: false}
	const windowsWords = "Windows is checking kids-shelf's video tools. Videos will start appearing when it is done; this can take a few minutes or a restart of the computer."
	if got := summarizeSelfTest("windows", blockedVideo, true); !strings.HasPrefix(got, windowsWords) ||
		!strings.Contains(got, "Libraries are unavailable: kiwix-serve did not start.") {
		t.Errorf("windows, refused = %q, want the Windows words plus the libraries part", got)
	}
	for _, tc := range []struct {
		goos    string
		refused bool
	}{{"windows", false}, {"linux", true}, {"darwin", true}} {
		got := summarizeSelfTest(tc.goos, blockedVideo, tc.refused)
		if !strings.HasPrefix(got, "Video conversion is unavailable: ffmpeg did not start.") || strings.Contains(got, "Windows") {
			t.Errorf("%s refused=%v = %q, want the generic words", tc.goos, tc.refused, got)
		}
	}
	if got := summarizeSelfTest("windows", tools.SelfTestResult{FFmpeg: true, FFprobe: true, Qjs: true, KiwixServe: true}, false); got != "" {
		t.Errorf("all working = %q, want none", got)
	}
}
