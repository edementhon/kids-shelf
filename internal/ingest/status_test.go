package ingest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/edementhon/kids-shelf/internal/core"
	"github.com/edementhon/kids-shelf/internal/jobs"
	"github.com/edementhon/kids-shelf/internal/store"
)

// TestJobsListOrderAndPlace: the conversions list comes running first,
// then failed and paused, then waiting in the order the two lanes will
// claim (sort lane first), then done. A waiting job's place is its place
// in its own lane's line and matches what that lane's ClaimRange picks
// next; a running conversion carries its progress.
func TestJobsListOrderAndPlace(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	q, err := jobs.New(s)
	if err != nil {
		t.Fatal(err)
	}
	iq := NewQueue(q)
	ids := map[string]string{}
	add := func(name string) string {
		t.Helper()
		id, err := iq.Enqueue(context.Background(), "/videos/"+name+".mkv", core.Source{
			Kind: core.SourceFolder, Path: "/videos/" + name + ".mkv", Title: name, Show: "Pretend Show",
		})
		if err != nil {
			t.Fatalf("Enqueue %s: %v", name, err)
		}
		ids[name] = id
		return id
	}
	claimNext := func(want string) {
		t.Helper()
		job, err := q.Claim(JobKind)
		if err != nil || job == nil || job.ID != ids[want] {
			t.Fatalf("Claim = %+v, %v; want %s", job, err, want)
		}
	}
	add("done")
	add("failed")
	add("converting")
	claimNext("done")
	claimNext("failed")
	claimNext("converting")
	if err := q.Done(ids["done"]); err != nil {
		t.Fatal(err)
	}
	if err := q.Fail(ids["failed"], os.ErrNotExist); err != nil {
		t.Fatal(err)
	}
	if err := q.Advance(ids["converting"], StageConvert, payload{File: "/videos/converting.mkv", Progress: 42, SecondsLeft: 360}); err != nil {
		t.Fatal(err)
	}
	// Two improvements wait on the fix lane (the full re-encode enqueued
	// first, but the cheaper remux claims first), two new files on the
	// sort lane.
	if err := q.Requeue(add("fixFull"), StageConvert, fixPriority(ActionFull, 30)); err != nil {
		t.Fatal(err)
	}
	add("sortA")
	if err := q.Requeue(add("fixRemux"), StageConvert, fixPriority(ActionRemux, 600)); err != nil {
		t.Fatal(err)
	}
	add("sortB")

	list, err := iq.Jobs()
	if err != nil {
		t.Fatal(err)
	}
	name := map[string]string{}
	for n, id := range ids {
		name[id] = n
	}
	var got []string
	for _, v := range list {
		got = append(got, name[v.ID])
	}
	want := []string{"converting", "failed", "sortA", "sortB", "fixRemux", "fixFull", "done"}
	if !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	places := map[string]int{}
	for _, v := range list {
		places[name[v.ID]] = v.Place
		if name[v.ID] == "converting" && (v.Progress != 42 || v.SecondsLeft != 360) {
			t.Errorf("running conversion = %v%% / %ds, want 42%% / 360s", v.Progress, v.SecondsLeft)
		}
	}
	wantPlace := map[string]int{"sortA": 1, "sortB": 2, "fixRemux": 1, "fixFull": 2}
	for n := range ids {
		if places[n] != wantPlace[n] {
			t.Errorf("place of %s = %d, want %d", n, places[n], wantPlace[n])
		}
	}
	// Place 1 of each lane is what that lane's worker claims next.
	for l, next := range []string{"sortA", "fixRemux"} {
		job, err := q.ClaimRange(JobKind, Lanes[l].Min, Lanes[l].Max)
		if err != nil || job == nil || job.ID != ids[next] {
			t.Errorf("lane %d claims %+v, %v; want %s (place 1)", l, job, err, next)
		}
	}
}

// TestConvertReportsProgress runs a real full re-encode with the fixture
// ffmpeg and watches the conversions list while it runs: the convert stage
// reports its percent done (between 0 and 100) in the job, the job then
// reaches done, and the finished job carries no progress numbers.
func TestConvertReportsProgress(t *testing.T) {
	helpers, full := fixtureEnv(t)
	src := filepath.Join(t.TempDir(), "long.mp4")
	if out, err := exec.Command(full, "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=24",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-t", "40", "-c:v", "mpeg4", "-q:v", "5", "-c:a", "aac", "-b:a", "128k",
		"-movflags", "+faststart", src).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v\n%s", err, out)
	}
	h := newHarness(t, helpers)
	h.w.progressEvery = 20 * time.Millisecond
	id := h.enqueue(t, src)

	var (
		mu   sync.Mutex
		seen []float64
	)
	stop := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			list, err := h.iq.Jobs()
			if err != nil {
				continue
			}
			for _, v := range list {
				if v.ID == id && v.State == jobs.StateRunning && v.Stage == StageConvert && v.Progress != 0 {
					mu.Lock()
					seen = append(seen, v.Progress)
					mu.Unlock()
				}
			}
		}
	}()
	h.runToEnd(t, id)
	close(stop)
	<-watched

	if pl := h.payloadOf(t, id); pl.Action != ActionFull {
		t.Fatalf("plan = %s, want a full re-encode", pl.Action)
	}
	if len(seen) == 0 {
		t.Fatal("no progress was reported while the conversion ran")
	}
	for i, p := range seen {
		if p <= 0 || p >= 100 {
			t.Errorf("progress %v out of (0, 100)", p)
		}
		if i > 0 && p < seen[i-1] {
			t.Errorf("progress went back from %v to %v", seen[i-1], p)
		}
	}
	j, err := h.q.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if j.State != jobs.StateDone {
		t.Fatalf("state = %s, want done", j.State)
	}
	if pl := h.payloadOf(t, id); pl.Progress != 0 || pl.SecondsLeft != 0 {
		t.Errorf("finished job keeps progress %v / %ds, want none", pl.Progress, pl.SecondsLeft)
	}
	t.Logf("progress seen: %v", seen)
}

// TestConvertEstimate pins the numbers the list shows: whole percent of
// the duration, never 100 before the stage ends, and a time left only once
// the run is long enough to tell.
func TestConvertEstimate(t *testing.T) {
	for _, tc := range []struct {
		done, dur float64
		elapsed   time.Duration
		pct       float64
		left      int
	}{
		{0, 600, time.Minute, 0, 0},
		{60, 0, time.Minute, 0, 0},
		{60, 600, 5 * time.Second, 10, 0},      // too early to tell
		{6, 600, time.Minute, 1, 0},            // too little done to tell
		{60, 600, 30 * time.Second, 10, 270},   // 10% in 30 s: 270 s left
		{300, 600, 2 * time.Minute, 50, 120},   // half in 2 min: 2 min left
		{600, 600, 10 * time.Minute, 99, 6},    // done but not yet ended: 99%
		{599.99, 600, 10 * time.Minute, 99, 6}, // never rounds up to 100
		{900, 600, 10 * time.Minute, 99, 6},    // past the probed duration
		{420.5, 600, 7 * time.Minute, 70, 179}, // 70.08% in 7 min: about 3 min left
	} {
		pct, left := convertEstimate(tc.done, tc.dur, tc.elapsed)
		if pct != tc.pct || left != tc.left {
			t.Errorf("convertEstimate(%v, %v, %v) = %v, %d; want %v, %d", tc.done, tc.dur, tc.elapsed, pct, left, tc.pct, tc.left)
		}
	}
}

// TestLastOutTimeUSSkipsUnfinishedLine: while ffmpeg runs, the last line of
// its -progress file may be half written; the last complete one counts.
func TestLastOutTimeUSSkipsUnfinishedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "progress.log")
	if err := os.WriteFile(path, []byte("out_time_us=5000000\nprogress=continue\nout_time_us=12"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, ok := lastOutTimeUS(path); !ok || got != 5_000_000 {
		t.Errorf("lastOutTimeUS = %d, %v; want 5000000 (the half-written line skipped)", got, ok)
	}
}
