package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edementhon/kids-shelf/internal/core"
	"github.com/edementhon/kids-shelf/internal/disk"
	"github.com/edementhon/kids-shelf/internal/jobs"
	"github.com/edementhon/kids-shelf/internal/store"
)

// The follow tests drive the real Follows over a real store and jobs
// queue, with this test binary as yt-dlp in "listing" mode: it prints the
// JSON a test wrote to $KS_FAKE_YTDLP_LISTING (cut to --playlist-end) and
// logs every call, so a test can change what YouTube "has" between checks
// and see exactly what was asked.

const (
	testPlaylist = "https://www.youtube.com/playlist?list=PLsunfun"
	testChannel  = "https://www.youtube.com/@pebblelab"
)

type fakeVid struct {
	id, title string
	secs      int
}

type followEnv struct {
	t       *testing.T
	f       *Follows
	m       *Manager
	q       *jobs.Queue
	listing string
	log     string
	clock   time.Time
}

func newFollowEnv(t *testing.T) *followEnv {
	t.Helper()
	m, _, q, dataDir, s := newFakeEnvStore(t, "listing")
	e := &followEnv{t: t, m: m, q: q,
		listing: filepath.Join(dataDir, "listing.json"),
		log:     filepath.Join(dataDir, "calls.log"),
		clock:   time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC),
	}
	t.Setenv("KS_FAKE_YTDLP_LISTING", e.listing)
	t.Setenv("KS_FAKE_YTDLP_LOG", e.log)
	f, err := NewFollows(s, m)
	if err != nil {
		t.Fatalf("NewFollows: %v", err)
	}
	// The env's clock; started an hour before it, so scheduled checks are
	// not held back by the first-check delay.
	f.now = func() time.Time { return e.clock }
	f.started = e.clock.Add(-time.Hour)
	e.f = f
	return e
}

// serve sets what the fake YouTube lists for the next lookup or check.
func (e *followEnv) serve(title, channel string, vids ...fakeVid) {
	e.t.Helper()
	entries := make([]map[string]any, 0, len(vids))
	for _, v := range vids {
		en := map[string]any{"_type": "url", "id": v.id, "title": v.title, "url": "https://www.youtube.com/watch?v=" + v.id}
		if v.secs > 0 {
			en["duration"] = v.secs
		} else {
			en["duration"] = nil
		}
		entries = append(entries, en)
	}
	b, err := json.Marshal(map[string]any{"id": "list", "title": title, "channel": channel, "uploader": channel, "_type": "playlist", "entries": entries})
	if err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(e.listing, b, 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *followEnv) serveError(line string) {
	e.t.Helper()
	if err := os.WriteFile(e.listing, []byte(line+"\n"), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// step runs the runner once (every lookup and check that is due).
func (e *followEnv) step() {
	e.t.Helper()
	if err := e.f.step(context.Background()); err != nil {
		e.t.Fatalf("step: %v", err)
	}
}

func (e *followEnv) view(id string) FollowView {
	e.t.Helper()
	list, err := e.f.List()
	if err != nil {
		e.t.Fatalf("List: %v", err)
	}
	for _, v := range list {
		if v.ID == id {
			return v
		}
	}
	e.t.Fatalf("follow %s not listed", id)
	return FollowView{}
}

// queued returns the video ids of every youtube job, in enqueue order.
func (e *followEnv) queued() []string {
	e.t.Helper()
	list, err := e.q.List(Kind)
	if err != nil {
		e.t.Fatal(err)
	}
	slices.SortFunc(list, func(a, b jobs.Job) int { return int(a.Seq - b.Seq) })
	var out []string
	for i := range list {
		var p Payload
		if err := list[i].DecodePayload(&p); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, p.VideoID)
	}
	return out
}

func (e *followEnv) states(id string) map[string]string {
	e.t.Helper()
	rows, err := e.f.db.Query(`SELECT video_id, state FROM youtube_follow_videos WHERE follow_id = ?`, id)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var v, s string
		if err := rows.Scan(&v, &s); err != nil {
			e.t.Fatal(err)
		}
		out[v] = s
	}
	return out
}

func (e *followEnv) calls() []string {
	e.t.Helper()
	b, err := os.ReadFile(e.log)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		e.t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// lookup pastes link and runs the lookup to its preview.
func (e *followEnv) lookup(link string) string {
	e.t.Helper()
	id, err := e.f.Lookup(link)
	if err != nil {
		e.t.Fatalf("Lookup: %v", err)
	}
	if v := e.view(id); v.Status != statusLookup {
		e.t.Fatalf("before the runner: status %q, want lookup", v.Status)
	}
	e.step()
	if v := e.view(id); v.Status != statusPreview {
		e.t.Fatalf("after the lookup: status %q (message %q), want preview", v.Status, v.Message)
	}
	return id
}

func channelVids(n int, prefix string) []fakeVid {
	out := make([]fakeVid, n)
	for i := range out {
		out[i] = fakeVid{id: prefix + string(rune('a'+i/26)) + string(rune('a'+i%26)), title: "Pebble video", secs: 300}
	}
	return out
}

// TestFollowPreview: a pasted playlist is looked up in the background and
// its preview counts the videos, their known total length (an unknown one
// is counted apart), the space at the downloaded quality, the videos Kids
// Shelf has already, and what each choice would download.
func TestFollowPreview(t *testing.T) {
	e := newFollowEnv(t)
	// One video is in Kids Shelf already (pasted on its own before).
	if _, err := e.q.Enqueue(Kind, Payload{URL: watchURL("sun-one"), VideoID: "sun-one"}); err != nil {
		t.Fatal(err)
	}
	e.serve("Sunny Science", "Starry Lab",
		fakeVid{"sun-one", "Why the Sun shines", 600},
		fakeVid{"sun-two", "Sunspots", 1200},
		fakeVid{"sun-three", "Mystery video", 0},
		fakeVid{"sun-four", "The long one", 3000},
	)
	id := e.lookup(testPlaylist)
	v := e.view(id)
	if v.Kind != KindPlaylist || v.Name != "Sunny Science" {
		t.Errorf("kind %q name %q, want playlist Sunny Science", v.Kind, v.Name)
	}
	p := v.Preview
	if p == nil {
		t.Fatal("no preview")
	}
	if p.Videos != 4 || p.Seconds != 4800 || p.Unknown != 1 || p.Already != 1 {
		t.Errorf("preview = %+v, want 4 videos, 4800 s, 1 unknown, 1 already", p)
	}
	if p.Bytes != 4800*bytesPerSecond {
		t.Errorf("bytes = %d, want %d", p.Bytes, 4800*bytesPerSecond)
	}
	if _, ok := p.Choices[TakeNewest]; ok {
		t.Error("a playlist offers no newest-20 choice")
	}
	all := p.Choices[TakeAll]
	if c := all[0]; c.Videos != 3 || c.Bytes != estimateBytes(1200+3000) || c.Unknown != 1 || c.TooLong != 0 {
		t.Errorf("all, no limit = %+v, want the 3 new videos", c)
	}
	if c := all[30]; c.Videos != 2 || c.TooLong != 1 || c.Bytes != estimateBytes(1200) {
		t.Errorf("all, 30 min = %+v, want 2 videos and the long one left out", c)
	}
	if c := p.Choices[TakeNone][0]; c.Videos != 0 || c.Bytes != 0 {
		t.Errorf("none = %+v, want nothing", c)
	}
	// Nothing downloads before Start.
	if got := e.queued(); len(got) != 1 {
		t.Errorf("jobs before Start = %v, want only the earlier one", got)
	}
	// The same link pasted again is the same preview.
	if again, err := e.f.Lookup(testPlaylist); err != nil || again != id {
		t.Errorf("paste again = %q, %v, want %q", again, err, id)
	}
}

// TestFollowStartPlaylistAll: Start with All downloads every video Kids
// Shelf does not have, in a show named after the playlist, and keeps
// following it; a later check finds only the new video, and in "straight
// to the shelf" mode downloads it.
func TestFollowStartPlaylistAll(t *testing.T) {
	e := newFollowEnv(t)
	if _, err := e.q.Enqueue(Kind, Payload{URL: watchURL("sun-one"), VideoID: "sun-one"}); err != nil {
		t.Fatal(err)
	}
	vids := []fakeVid{{"sun-one", "One", 60}, {"sun-two", "Two", 60}, {"sun-three", "Three", 60}}
	e.serve("Sunny Science", "Starry Lab", vids...)
	id := e.lookup(testPlaylist)
	if err := e.f.Start(id, StartOptions{Take: TakeAll, Follow: true, Mode: ModeAuto}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got, want := e.queued(), []string{"sun-one", "sun-two", "sun-three"}; !slices.Equal(got, want) {
		t.Fatalf("jobs = %v, want %v (nothing twice)", got, want)
	}
	list, _ := e.q.List(Kind)
	for i := range list {
		var p Payload
		_ = list[i].DecodePayload(&p)
		if p.VideoID != "sun-one" && p.show() != "Sunny Science" {
			t.Errorf("show of %s = %q, want the playlist's title", p.VideoID, p.show())
		}
	}
	v := e.view(id)
	if v.Status != statusFollowing || v.Mode != ModeAuto || v.Counts.Added != 3 || v.Counts.Waiting != 0 {
		t.Errorf("after Start: %+v", v)
	}

	// Six hours later a new video is in the playlist.
	e.clock = e.clock.Add(CheckEvery + time.Minute)
	e.serve("Sunny Science", "Starry Lab", append(vids, fakeVid{"sun-four", "Four", 60})...)
	e.step()
	if got, want := e.queued(), []string{"sun-one", "sun-two", "sun-three", "sun-four"}; !slices.Equal(got, want) {
		t.Fatalf("jobs after the check = %v, want %v", got, want)
	}
	if v := e.view(id); v.Counts.Added != 4 || v.Checked == nil || !v.Checked.Equal(e.clock) {
		t.Errorf("after the check: counts %+v checked %v", v.Counts, v.Checked)
	}
	// A playlist check lists the whole playlist.
	last := e.calls()[len(e.calls())-1]
	if strings.Contains(last, "--playlist-end") || !strings.HasSuffix(last, testPlaylist) {
		t.Errorf("playlist check = %q, want a full listing of the playlist", last)
	}
}

// TestFollowStartChannelNewest: a channel's Start with "The newest 20"
// downloads exactly the newest 20 and remembers the rest, which are never
// offered later. Checks list the Videos tab only, capped to the newest
// 100, and in "ask me first" a new video waits for the parent.
func TestFollowStartChannelNewest(t *testing.T) {
	e := newFollowEnv(t)
	vids := channelVids(130, "peb-")
	e.serve("Pebble Lab - Videos", "Pebble Lab", vids...)
	id := e.lookup(testChannel)
	v := e.view(id)
	if v.Kind != KindChannel || v.Name != "Pebble Lab" || v.Preview.Videos != 130 {
		t.Fatalf("preview: kind %q name %q videos %d, want the whole channel", v.Kind, v.Name, v.Preview.Videos)
	}
	if c := v.Preview.Choices[TakeNewest][0]; c.Videos != NewestCount || c.Bytes != estimateBytes(NewestCount*300) {
		t.Errorf("newest choice = %+v", c)
	}
	// The lookup listed the Videos tab in full.
	first := e.calls()[0]
	if !strings.HasSuffix(first, testChannel+"/videos") || strings.Contains(first, "--playlist-end") {
		t.Errorf("lookup = %q, want the full Videos tab", first)
	}
	if err := e.f.Start(id, StartOptions{Take: TakeNewest, Follow: true, Mode: ModeAsk}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := e.queued()
	want := make([]string, NewestCount)
	for i := range want {
		want[i] = vids[i].id
	}
	if !slices.Equal(got, want) {
		t.Fatalf("jobs = %v, want the newest %d", got, NewestCount)
	}
	st := e.states(id)
	for i, v := range vids {
		wantSt := vidAdded
		if i >= NewestCount {
			wantSt = vidNotTaken
		}
		if st[v.id] != wantSt {
			t.Errorf("%s: state %q, want %q", v.id, st[v.id], wantSt)
		}
	}
	list, _ := e.q.List(Kind)
	var p Payload
	_ = list[0].DecodePayload(&p)
	if p.show() != "Pebble Lab" {
		t.Errorf("show = %q, want the channel's name", p.show())
	}

	// A new upload tops the channel: the check finds only it, and it waits.
	e.clock = e.clock.Add(CheckEvery)
	e.serve("Pebble Lab - Videos", "Pebble Lab", append([]fakeVid{{"peb-new", "Brand new", 240}}, vids...)...)
	e.step()
	if got := e.queued(); len(got) != NewestCount {
		t.Errorf("jobs after the check = %d, want %d (ask first downloads nothing)", len(got), NewestCount)
	}
	v = e.view(id)
	if len(v.Waiting) != 1 || v.Waiting[0].ID != "peb-new" || v.Waiting[0].Seconds != 240 || v.Counts.Waiting != 1 {
		t.Errorf("waiting = %+v counts %+v, want only the new video", v.Waiting, v.Counts)
	}
	if n, err := e.f.Waiting(); err != nil || n != 1 {
		t.Errorf("Waiting() = %d, %v, want 1", n, err)
	}
	last := e.calls()[len(e.calls())-1]
	if !strings.Contains(last, "--playlist-end 100 "+testChannel+"/videos") {
		t.Errorf("channel check = %q, want the Videos tab capped to 100", last)
	}
	for _, c := range e.calls() {
		if strings.Contains(c, "/shorts") || strings.Contains(c, "/streams") {
			t.Errorf("call %q lists Shorts or Live", c)
		}
	}
}

// TestFollowNoneYetAddSkip: "None yet" downloads nothing and remembers
// every video; later new videos wait; Add downloads one, Skip remembers
// the no, and a skipped video is never offered again.
func TestFollowNoneYetAddSkip(t *testing.T) {
	e := newFollowEnv(t)
	old := []fakeVid{{"peb-old1", "Old one", 100}, {"peb-old2", "Old two", 100}}
	e.serve("Pebble Lab - Videos", "Pebble Lab", old...)
	id := e.lookup(testChannel)
	if err := e.f.Start(id, StartOptions{Take: TakeNone, Follow: true, Mode: ModeAsk}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := e.queued(); len(got) != 0 {
		t.Fatalf("jobs after None yet = %v, want none", got)
	}
	e.clock = e.clock.Add(CheckEvery)
	e.serve("Pebble Lab - Videos", "Pebble Lab", append([]fakeVid{{"peb-yes", "Say yes", 100}, {"peb-no", "Say no", 100}}, old...)...)
	if err := e.f.CheckNow(id); err != nil {
		t.Fatal(err)
	}
	e.step()
	v := e.view(id)
	if len(v.Waiting) != 2 {
		t.Fatalf("waiting = %+v, want the two new videos (not the old ones)", v.Waiting)
	}
	if n, err := e.f.Answer(id, "peb-yes", true); err != nil || n != 1 {
		t.Fatalf("Add = %d, %v", n, err)
	}
	if got := e.queued(); !slices.Equal(got, []string{"peb-yes"}) {
		t.Errorf("jobs after Add = %v, want peb-yes", got)
	}
	if n, err := e.f.Answer(id, "peb-no", false); err != nil || n != 1 {
		t.Fatalf("Skip = %d, %v", n, err)
	}
	v = e.view(id)
	if len(v.Waiting) != 0 || v.Counts.Added != 1 || v.Counts.Skipped != 1 {
		t.Errorf("after Add and Skip: waiting %+v counts %+v", v.Waiting, v.Counts)
	}
	// The skipped video is still on the channel: never offered again.
	e.clock = e.clock.Add(CheckEvery)
	e.step()
	if v := e.view(id); len(v.Waiting) != 0 || e.states(id)["peb-no"] != vidSkipped {
		t.Errorf("after another check: waiting %+v, peb-no %q", v.Waiting, e.states(id)["peb-no"])
	}
	if got := e.queued(); !slices.Equal(got, []string{"peb-yes"}) {
		t.Errorf("jobs after another check = %v", got)
	}
	// Add all and Skip all answer every waiting video of the source.
	e.clock = e.clock.Add(CheckEvery)
	e.serve("Pebble Lab - Videos", "Pebble Lab", fakeVid{"peb-a", "A", 10}, fakeVid{"peb-b", "B", 10}, fakeVid{"peb-yes", "Say yes", 100})
	e.step()
	if n, err := e.f.Answer(id, "", true); err != nil || n != 2 {
		t.Fatalf("Add all = %d, %v, want 2", n, err)
	}
	if got := e.queued(); !slices.Equal(got, []string{"peb-yes", "peb-a", "peb-b"}) {
		t.Errorf("jobs after Add all = %v", got)
	}
}

// TestFollowLengthLimit: the limit leaves out longer videos at Start and
// in checks; a video of unknown length is never left out; raising the
// limit brings the too long ones back, lowering it moves waiting ones out.
func TestFollowLengthLimit(t *testing.T) {
	e := newFollowEnv(t)
	vids := []fakeVid{{"len-short", "Short", 300}, {"len-long", "Long", 1500}, {"len-unknown", "Unknown", 0}}
	e.serve("Sunny Science", "Starry Lab", vids...)
	id := e.lookup(testPlaylist)
	if err := e.f.Start(id, StartOptions{Take: TakeAll, MaxMinutes: 10, Follow: true, Mode: ModeAsk}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := e.queued(); !slices.Equal(got, []string{"len-short", "len-unknown"}) {
		t.Fatalf("jobs = %v, want the short and the unknown-length ones", got)
	}
	if st := e.states(id)["len-long"]; st != vidTooLong {
		t.Errorf("long video state %q, want too_long", st)
	}
	e.clock = e.clock.Add(CheckEvery)
	e.serve("Sunny Science", "Starry Lab", append(vids, fakeVid{"len-new-long", "New long", 1200}, fakeVid{"len-new-short", "New short", 120})...)
	e.step()
	v := e.view(id)
	if len(v.Waiting) != 1 || v.Waiting[0].ID != "len-new-short" || v.Counts.TooLong != 2 {
		t.Errorf("after the check: waiting %+v counts %+v", v.Waiting, v.Counts)
	}
	// 20 minutes: the 20-minute video fits now and waits; the 25 does not.
	if err := e.f.Settings(id, ModeAsk, 20); err != nil {
		t.Fatal(err)
	}
	st := e.states(id)
	if st["len-new-long"] != vidWaiting || st["len-long"] != vidTooLong {
		t.Errorf("after raising the limit: %v", st)
	}
	// Back to 10 minutes: the waiting 20-minute video is too long again.
	if err := e.f.Settings(id, ModeAsk, 10); err != nil {
		t.Fatal(err)
	}
	if st := e.states(id)["len-new-long"]; st != vidTooLong {
		t.Errorf("after lowering the limit: %q, want too_long", st)
	}
	// No limit, straight to the shelf: both long videos download now.
	if err := e.f.Settings(id, ModeAuto, 0); err != nil {
		t.Fatal(err)
	}
	if got := e.queued(); !slices.Equal(got, []string{"len-short", "len-unknown", "len-long", "len-new-long"}) &&
		!slices.Equal(got, []string{"len-short", "len-unknown", "len-new-long", "len-long"}) {
		t.Errorf("jobs after removing the limit = %v", got)
	}
	if err := e.f.Settings(id, ModeAuto, 15); !errors.Is(err, ErrFollowChoice) {
		t.Errorf("a 15-minute limit: %v, want ErrFollowChoice", err)
	}
}

// TestFollowFailedCheckKeepsState: a check that cannot list (the playlist
// went private, or yt-dlp is gone) keeps every video and records a
// plain-words reason; the next good check clears it.
func TestFollowFailedCheckKeepsState(t *testing.T) {
	e := newFollowEnv(t)
	e.serve("Sunny Science", "Starry Lab", fakeVid{"sun-one", "One", 60})
	id := e.lookup(testPlaylist)
	if err := e.f.Start(id, StartOptions{Take: TakeAll, Follow: true, Mode: ModeAuto}); err != nil {
		t.Fatal(err)
	}
	e.clock = e.clock.Add(CheckEvery)
	e.serveError("ERROR: [youtube:tab] PLsunfun: The playlist does not exist.")
	e.step()
	v := e.view(id)
	if v.Message != "YouTube says this playlist isn't there. It may be private or removed." {
		t.Errorf("message = %q", v.Message)
	}
	if v.Status != statusFollowing || v.Counts.Added != 1 || v.Checked == nil || !v.Checked.Equal(e.clock) {
		t.Errorf("after the failed check: %+v", v)
	}
	// The failed check counts for the schedule: not checked again at once.
	e.step()
	if n := len(e.calls()); n != 2 {
		t.Errorf("yt-dlp calls = %d, want 2 (lookup, failed check)", n)
	}
	// yt-dlp missing: the reason says so.
	e.clock = e.clock.Add(CheckEvery)
	if err := os.WriteFile(filepath.Join(e.m.dataDir, "helpers", "yt-dlp", "current"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	e.step()
	if v := e.view(id); v.Message != msgListMissing || v.Counts.Added != 1 {
		t.Errorf("without yt-dlp: message %q counts %+v", v.Message, v.Counts)
	}
	installFakeYtDlp(t, e.m.dataDir, "2026.09.01")
	e.clock = e.clock.Add(CheckEvery)
	e.serve("Sunny Science", "Starry Lab", fakeVid{"sun-one", "One", 60})
	e.step()
	if v := e.view(id); v.Message != "" {
		t.Errorf("after a good check: message %q, want none", v.Message)
	}
}

// TestFollowLookupFailure: a lookup that fails says why and waits for the
// parent; Check now (Try again) looks it up again.
func TestFollowLookupFailure(t *testing.T) {
	e := newFollowEnv(t)
	e.serveError("ERROR: Unable to download API page: <urlopen error [Errno -3] Temporary failure in name resolution>")
	id, err := e.f.Lookup(testChannel)
	if err != nil {
		t.Fatal(err)
	}
	e.step()
	v := e.view(id)
	if v.Status != statusLookup || v.Message != msgListNetwork {
		t.Fatalf("after a failed lookup: status %q message %q", v.Status, v.Message)
	}
	e.step() // not tried again by itself
	if n := len(e.calls()); n != 1 {
		t.Errorf("yt-dlp calls = %d, want 1", n)
	}
	e.serve("Pebble Lab - Videos", "Pebble Lab", fakeVid{"peb-1", "One", 60})
	if err := e.f.CheckNow(id); err != nil {
		t.Fatal(err)
	}
	e.step()
	if v := e.view(id); v.Status != statusPreview || v.Message != "" {
		t.Errorf("after Try again: status %q message %q", v.Status, v.Message)
	}
	// Cancel forgets the preview.
	if err := e.f.Cancel(id); err != nil {
		t.Fatal(err)
	}
	if list, _ := e.f.List(); len(list) != 0 {
		t.Errorf("after Cancel: %+v", list)
	}
}

// TestFollowScheduleSurvivesRestart: the last check time is stored. After
// a restart a source checked an hour ago is not checked again; one whose
// last check is 6 hours old is checked about a minute after the start.
func TestFollowScheduleSurvivesRestart(t *testing.T) {
	t.Setenv("KS_FAKE_YTDLP", "listing")
	dataDir := t.TempDir()
	installFakeYtDlp(t, dataDir, "2026.09.01")
	markDailyCheck(t, dataDir)
	listing := filepath.Join(dataDir, "listing.json")
	t.Setenv("KS_FAKE_YTDLP_LISTING", listing)
	e := &followEnv{t: t, listing: listing, log: filepath.Join(dataDir, "calls.log"),
		clock: time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)}
	t.Setenv("KS_FAKE_YTDLP_LOG", e.log)
	open := func() (*store.Store, *Follows) {
		s, err := store.Open(filepath.Join(dataDir, "test.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		q, err := jobs.New(s)
		if err != nil {
			t.Fatal(err)
		}
		e.q = q
		e.m = NewManager(q, &core.FakeIngestQueue{}, &PointerHelpers{DataDir: dataDir, QuickJSPath: "qjs-fake"},
			NewUpdater(dataDir, "http://127.0.0.1:1/releases/latest"), dataDir)
		e.m.WithReservations(disk.NewReservations(q).WithFree(func(string) (uint64, error) { return 64 << 30, nil }))
		f, err := NewFollows(s, e.m)
		if err != nil {
			t.Fatal(err)
		}
		f.now = func() time.Time { return e.clock }
		f.started = e.clock
		e.f = f
		return s, f
	}
	s, _ := open()
	e.serve("Sunny Science", "Starry Lab", fakeVid{"sun-one", "One", 60})
	id := e.lookup(testPlaylist)
	if err := e.f.Start(id, StartOptions{Take: TakeAll, Follow: true, Mode: ModeAuto}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	calls := len(e.calls())

	// Restart an hour after the lookup: nothing is due.
	e.clock = e.clock.Add(time.Hour)
	s, f := open()
	e.clock = e.clock.Add(2 * time.Minute)
	e.step()
	if n := len(e.calls()); n != calls {
		t.Errorf("an hour after the last check, the restart checked (%d calls, want %d)", n, calls)
	}
	if w := f.nextWait(); w < 4*time.Hour || w > 5*time.Hour {
		t.Errorf("next check in %v, want the rest of the 6 hours", w)
	}
	s.Close()

	// Restart 7 hours after the lookup: due, but only a minute after start.
	e.clock = e.clock.Add(6 * time.Hour)
	s, f = open()
	defer s.Close()
	e.step()
	if n := len(e.calls()); n != calls {
		t.Errorf("right after the start: %d calls, want %d (the first check waits a minute)", n, calls)
	}
	if w := f.nextWait(); w != firstCheckDelay {
		t.Errorf("next check in %v, want %v", w, firstCheckDelay)
	}
	e.clock = e.clock.Add(firstCheckDelay)
	e.step()
	if n := len(e.calls()); n != calls+1 {
		t.Errorf("a minute after the start: %d calls, want %d", n, calls+1)
	}
	if v := e.view(id); v.Checked == nil || !v.Checked.Equal(e.clock) {
		t.Errorf("checked = %v, want %v", v.Checked, e.clock)
	}
}

// TestFollowStopLeavesVideos: Stop following forgets the source and its
// seen list, but no download job (and so no episode) is touched; a
// one-time Start (no following) also leaves its downloads alone.
func TestFollowStopLeavesVideos(t *testing.T) {
	e := newFollowEnv(t)
	e.serve("Sunny Science", "Starry Lab", fakeVid{"sun-one", "One", 60}, fakeVid{"sun-two", "Two", 60})
	id := e.lookup(testPlaylist)
	if err := e.f.Start(id, StartOptions{Take: TakeAll, Follow: true, Mode: ModeAuto}); err != nil {
		t.Fatal(err)
	}
	before := e.queued()
	if err := e.f.Cancel(id); !errors.Is(err, ErrFollowChoice) {
		t.Errorf("Cancel of a followed source: %v, want ErrFollowChoice", err)
	}
	if err := e.f.Stop(id); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := e.queued(); !slices.Equal(got, before) {
		t.Errorf("jobs after Stop = %v, want %v", got, before)
	}
	if list, _ := e.f.List(); len(list) != 0 {
		t.Errorf("follows after Stop = %+v", list)
	}
	var n int
	if err := e.f.db.QueryRow(`SELECT COUNT(*) FROM youtube_follow_videos`).Scan(&n); err != nil || n != 0 {
		t.Errorf("seen videos left after Stop: %d, %v", n, err)
	}

	// Once, without following: the videos download, and the source stays
	// listed as added once (never checked) until the web layer forgets it.
	e.serve("Moon Songs", "Starry Lab", fakeVid{"moon-one", "Moon", 60})
	id = e.lookup("https://www.youtube.com/playlist?list=PLmoon")
	if err := e.f.Start(id, StartOptions{Take: TakeAll, Follow: false}); err != nil {
		t.Fatal(err)
	}
	if got := e.queued(); !slices.Equal(got, append(before, "moon-one")) {
		t.Errorf("jobs after a one-time Start = %v", got)
	}
	v := e.view(id)
	if v.Status != statusOnce || v.Counts.Added != 1 || !slices.Equal(v.AddedIDs, []string{"moon-one"}) {
		t.Errorf("once-added source = %+v", v)
	}
	if err := e.f.CheckNow(id); !errors.Is(err, ErrFollowChoice) {
		t.Errorf("Check now of a once-added source: %v, want ErrFollowChoice", err)
	}
	if err := e.f.Settings(id, ModeAuto, 0); !errors.Is(err, ErrFollowChoice) {
		t.Errorf("Settings of a once-added source: %v, want ErrFollowChoice", err)
	}
	calls := len(e.calls())
	e.clock = e.clock.Add(2 * CheckEvery)
	e.step()
	if n := len(e.calls()); n != calls {
		t.Errorf("a once-added source was checked (%d calls, want %d)", n, calls)
	}
	if n, err := e.f.Waiting(); err != nil || n != 0 {
		t.Errorf("Waiting() = %d, %v", n, err)
	}
	// Pasted again, it is a fresh preview that knows the video is there.
	id = e.lookup("https://www.youtube.com/playlist?list=PLmoon")
	if p := e.view(id).Preview; p == nil || p.Already != 1 {
		t.Errorf("preview after a once-add = %+v, want 1 already there", p)
	}
	if err := e.f.Start(id, StartOptions{Take: TakeAll, Follow: false}); err != nil {
		t.Fatal(err)
	}
	if err := e.f.Forget(id); err != nil {
		t.Fatal(err)
	}
	if list, _ := e.f.List(); len(list) != 0 {
		t.Errorf("after Forget: %+v", list)
	}
	if got := e.queued(); !slices.Equal(got, append(before, "moon-one")) {
		t.Errorf("jobs after Forget = %v, want them untouched", got)
	}
}

// TestFollowedVideoShowNaming runs a followed video through download and
// handoff: the episode goes in the show named after the playlist, even
// though its uploader is another channel. A single video keeps its
// uploader.
func TestFollowedVideoShowNaming(t *testing.T) {
	m, fq, q, _ := newFakeEnv(t, "single")
	if _, err := q.Enqueue(Kind, Payload{URL: watchURL("sun-one"), VideoID: "sun-one", Title: "One", Uploader: "Starry Lab", Show: "Sunny Science"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Enqueue(context.Background(), watchURL("solo-video1")); err != nil {
		t.Fatal(err)
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	shows := map[string]string{}
	for _, c := range fq.Calls() {
		shows[c.Source.Path] = c.Source.Show
	}
	if got := shows[watchURL("sun-one")]; got != "Sunny Science" {
		t.Errorf("followed video show = %q, want Sunny Science", got)
	}
	if got := shows[watchURL("solo-video1")]; got != "Fake Channel" {
		t.Errorf("single video show = %q, want its uploader", got)
	}
}

// TestListKind tells playlists and channels from single videos.
func TestListKind(t *testing.T) {
	for link, want := range map[string]string{
		"https://www.youtube.com/playlist?list=PLx":  KindPlaylist,
		"https://www.youtube.com/@pebblelab":         KindChannel,
		"https://www.youtube.com/channel/UCabc":      KindChannel,
		"https://www.youtube.com/c/pebble":           KindChannel,
		"https://www.youtube.com/user/pebble":        KindChannel,
		"https://www.youtube.com/watch?v=abcdefghij": "",
		"https://youtu.be/abcdefghijk":               "",
		"https://www.youtube.com/shorts/abcdefghijk": "",
		"https://www.youtube.com/playlist":           "",
		"http://www.youtube.com/@pebblelab":          "",
		"https://evil.example/@pebblelab":            "",
	} {
		got, err := ListKind(link)
		if got != want || (want == "") != (err != nil) {
			t.Errorf("ListKind(%q) = %q, %v, want %q", link, got, err, want)
		}
	}
}
