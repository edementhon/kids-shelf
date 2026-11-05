package web

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/edementhon/kids-shelf/internal/core"
	"github.com/edementhon/kids-shelf/internal/ingest"
	"github.com/edementhon/kids-shelf/internal/jobs"
	"github.com/edementhon/kids-shelf/internal/store"
	"github.com/edementhon/kids-shelf/internal/youtube"
)

// contentFollows builds the YouTube manager and its follows over one store,
// the way internal/app wires them. No runner runs: lookups stay pending,
// which is what these handler tests look at (the lookups, checks and
// answers themselves are tested in internal/youtube with a fake yt-dlp).
func contentFollows(t *testing.T) (*store.Store, *jobs.Queue, *youtube.Manager, *youtube.Follows) {
	t.Helper()
	s := contentTestStore(t)
	q, err := jobs.New(s)
	if err != nil {
		t.Fatal(err)
	}
	yt := youtube.NewManager(q, ingest.NewQueue(q), nil, nil, t.TempDir())
	f, err := youtube.NewFollows(s, yt)
	if err != nil {
		t.Fatal(err)
	}
	return s, q, yt, f
}

// TestContentFollowRoutesNeedPIN: every Following route is behind the
// parent PIN, and a refused paste starts nothing.
func TestContentFollowRoutesNeedPIN(t *testing.T) {
	_, q, yt, f := contentFollows(t)
	ts := contentTestServer(t, Deps{YouTube: yt, Follows: f})
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/admin/content/youtube/follows"},
		{http.MethodPost, "/api/admin/content/youtube"},
		{http.MethodPost, "/api/admin/content/youtube/follows/start"},
		{http.MethodPost, "/api/admin/content/youtube/follows/cancel"},
		{http.MethodPost, "/api/admin/content/youtube/follows/settings"},
		{http.MethodPost, "/api/admin/content/youtube/follows/check"},
		{http.MethodPost, "/api/admin/content/youtube/follows/stop"},
		{http.MethodPost, "/api/admin/content/youtube/follows/add"},
		{http.MethodPost, "/api/admin/content/youtube/follows/skip"},
	} {
		var body any
		if c.method == http.MethodPost {
			body = map[string]any{"id": "x", "url": "https://www.youtube.com/playlist?list=PLsunfun"}
		}
		if resp, _ := visitorReq(t, c.method, ts.URL+c.path, body); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without the PIN = %d, want 401", c.method, c.path, resp.StatusCode)
		}
	}
	if list, err := f.List(); err != nil || len(list) != 0 {
		t.Errorf("follows after the refused calls = %+v, %v", list, err)
	}
	if list, err := q.List(youtube.Kind); err != nil || len(list) != 0 {
		t.Errorf("jobs after the refused calls = %d, %v", len(list), err)
	}
}

// TestContentFollowPaste: a playlist or channel link no longer downloads
// at once: it is looked up first and listed as such, with no download job;
// the same link again is the same lookup; a single video still downloads
// at once; Cancel forgets the lookup; actions on things that are not
// there, or not possible, answer in plain words.
func TestContentFollowPaste(t *testing.T) {
	_, q, yt, f := contentFollows(t)
	ts := contentTestServer(t, Deps{YouTube: yt, Follows: f})

	var added struct {
		ID     string `json:"id"`
		Follow string `json:"follow"`
		Kind   string `json:"kind"`
	}
	code, body := contentTestPost(t, ts, "/api/admin/content/youtube", map[string]any{"url": "youtube.com/@pebblelab"})
	if code != http.StatusOK {
		t.Fatalf("paste a channel: %d %s", code, body)
	}
	contentTestDecode(t, body, &added)
	if added.Follow == "" || added.Kind != "channel" || added.ID != "" {
		t.Fatalf("paste a channel = %+v, want a lookup", added)
	}
	channelID := added.Follow
	if list, _ := q.List(youtube.Kind); len(list) != 0 {
		t.Errorf("a pasted channel made %d download jobs, want none", len(list))
	}

	code, body = contentTestPost(t, ts, "/api/admin/content/youtube", map[string]any{"url": "https://www.youtube.com/playlist?list=PLsunfun&si=abc"})
	if code != http.StatusOK {
		t.Fatalf("paste a playlist: %d %s", code, body)
	}
	added = struct {
		ID     string `json:"id"`
		Follow string `json:"follow"`
		Kind   string `json:"kind"`
	}{}
	contentTestDecode(t, body, &added)
	if added.Kind != "playlist" {
		t.Fatalf("paste a playlist = %+v", added)
	}
	playlistID := added.Follow
	code, body = contentTestPost(t, ts, "/api/admin/content/youtube", map[string]any{"url": "https://www.youtube.com/playlist?list=PLsunfun"})
	contentTestDecode(t, body, &added)
	if code != http.StatusOK || added.Follow != playlistID {
		t.Errorf("the same playlist again = %d %+v, want the same lookup %s", code, added, playlistID)
	}

	var list struct {
		Follows []struct {
			ID     string `json:"id"`
			URL    string `json:"url"`
			Kind   string `json:"kind"`
			Status string `json:"status"`
		} `json:"follows"`
		Waiting int   `json:"waiting"`
		Limits  []int `json:"limits"`
	}
	code, body = contentTestGet(t, ts, "/api/admin/content/youtube/follows")
	if code != http.StatusOK {
		t.Fatalf("list: %d %s", code, body)
	}
	contentTestDecode(t, body, &list)
	if len(list.Follows) != 2 || list.Waiting != 0 || len(list.Limits) != 5 {
		t.Fatalf("list = %+v", list)
	}
	for _, v := range list.Follows {
		if v.Status != "lookup" {
			t.Errorf("%s: status %q, want lookup", v.URL, v.Status)
		}
	}
	if list.Follows[0].URL != "https://www.youtube.com/@pebblelab" || list.Follows[1].URL != "https://www.youtube.com/playlist?list=PLsunfun" {
		t.Errorf("lookups = %+v, want the canonical links in paste order", list.Follows)
	}

	// A single video still downloads at once.
	code, body = contentTestPost(t, ts, "/api/admin/content/youtube", map[string]any{"url": "https://youtu.be/dQw4w9WgXcQ"})
	if code != http.StatusOK {
		t.Fatalf("paste a video: %d %s", code, body)
	}
	if jl, _ := q.List(youtube.Kind); len(jl) != 1 {
		t.Errorf("jobs after a video = %d, want 1", len(jl))
	}

	// Start before the lookup is done is not possible; unknown ids are gone.
	code, body = contentTestPost(t, ts, "/api/admin/content/youtube/follows/start", map[string]any{"id": channelID, "take": "all", "follow": true, "mode": "ask"})
	if code != http.StatusBadRequest || contentTestError(t, body) != msgFollowChoice {
		t.Errorf("start during the lookup = %d %s", code, body)
	}
	code, body = contentTestPost(t, ts, "/api/admin/content/youtube/follows/stop", map[string]any{"id": "nope"})
	if code != http.StatusNotFound || contentTestError(t, body) != msgFollowGone {
		t.Errorf("stop of nothing = %d %s", code, body)
	}
	code, body = contentTestPost(t, ts, "/api/admin/content/youtube/follows/settings", map[string]any{"id": channelID, "mode": "auto", "maxMinutes": 15})
	if code != http.StatusBadRequest {
		t.Errorf("a 15-minute limit = %d %s, want 400", code, body)
	}
	if code, body = contentTestPost(t, ts, "/api/admin/content/youtube/follows/cancel", map[string]any{"id": channelID}); code != http.StatusOK {
		t.Fatalf("cancel: %d %s", code, body)
	}
	_, body = contentTestGet(t, ts, "/api/admin/content/youtube/follows")
	contentTestDecode(t, body, &list)
	if len(list.Follows) != 1 || list.Follows[0].ID != playlistID {
		t.Errorf("after cancel: %+v, want only the playlist", list.Follows)
	}
}

// TestAdminStateCountsWaitingVideos: the parent page learns how many new
// videos of followed sources wait for a yes.
func TestAdminStateCountsWaitingVideos(t *testing.T) {
	s, _, _, f := contentFollows(t)
	db := s.DB()
	if _, err := db.Exec(`INSERT INTO youtube_follows (id, url, kind, name, status, mode, created) VALUES ('f1', 'https://www.youtube.com/@pebblelab', 'channel', 'Pebble Lab', 'following', 'ask', '2026-11-05T08:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	for i, st := range []string{"waiting", "waiting", "waiting", "skipped", "added"} {
		if _, err := db.Exec(`INSERT INTO youtube_follow_videos (follow_id, video_id, title, state, first_seen) VALUES ('f1', ?, 'A video', ?, '2026-11-05T08:00:00Z')`,
			string(rune('a'+i)), st); err != nil {
			t.Fatal(err)
		}
	}
	ts, _, _ := newTestAdmin(t, func(d *Deps) { d.Follows = f })
	resp, st := doReq(t, http.MethodGet, ts.URL+"/api/admin/state", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("state: %d", resp.StatusCode)
	}
	if got := st["youtube_waiting"]; got != float64(3) {
		t.Errorf("youtube_waiting = %v, want 3", got)
	}
}

// followTestSource inserts a source and its videos the way Follows stores
// them (the lookups that produce them are tested in internal/youtube).
func followTestSource(t *testing.T, s *store.Store, id, status string, states map[string]string) {
	t.Helper()
	db := s.DB()
	if _, err := db.Exec(`INSERT INTO youtube_follows (id, url, kind, name, status, mode, created) VALUES (?, ?, 'playlist', 'Sunny Science', ?, 'ask', '2026-11-05T08:00:00Z')`,
		id, "https://www.youtube.com/playlist?list=PL"+id, status); err != nil {
		t.Fatal(err)
	}
	for v, st := range states {
		if _, err := db.Exec(`INSERT INTO youtube_follow_videos (follow_id, video_id, title, state, first_seen) VALUES (?, ?, 'A video', ?, '2026-11-05T08:00:00Z')`, id, v, st); err != nil {
			t.Fatal(err)
		}
	}
}

type followTestList struct {
	Follows []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Counts struct {
			Added   int `json:"added"`
			Waiting int `json:"waiting"`
		} `json:"counts"`
		Shelf contentFollowShelf `json:"shelf"`
	} `json:"follows"`
}

// followTestJobs adds youtube jobs for videos and moves them: "shelf" is
// downloaded and converted, "converting" downloaded and waiting for the
// converter, "queued" waits to download, "failed" could not download.
func followTestJobs(t *testing.T, q *jobs.Queue, iq *ingest.Queue, videos map[string]string) {
	t.Helper()
	ctx := context.Background()
	ids := map[string]string{}
	order := []string{}
	for v := range videos {
		order = append(order, v)
	}
	slices.Sort(order)
	for _, v := range order {
		id, err := q.Enqueue(youtube.Kind, youtube.Payload{URL: "https://www.youtube.com/watch?v=" + v, VideoID: v, Title: v})
		if err != nil {
			t.Fatal(err)
		}
		ids[v] = id
	}
	for _, v := range order {
		if videos[v] == "queued" {
			continue
		}
		// Claim takes the oldest queued job; queued ones sort last by name.
		job, err := q.Claim(youtube.Kind)
		if err != nil || job == nil {
			t.Fatalf("claim %s: %+v %v", v, job, err)
		}
		switch videos[v] {
		case "failed":
			if err := q.Fail(job.ID, errors.New("gone")); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := q.Done(job.ID); err != nil {
			t.Fatal(err)
		}
		src := core.Source{Kind: core.SourceYouTube, Path: "https://www.youtube.com/watch?v=" + v, Title: v, Show: "Sunny Science"}
		ingID, err := iq.Enqueue(ctx, t.TempDir()+"/x.mp4", src)
		if err != nil {
			t.Fatal(err)
		}
		if videos[v] == "shelf" {
			j, err := q.ClaimRange(ingest.JobKind, 0, 0)
			if err != nil || j == nil || j.ID != ingID {
				t.Fatalf("claim ingest %s: %+v %v", v, j, err)
			}
			if err := q.Done(ingID); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// TestContentFollowShelfCounts: a source's counts come from its videos'
// real jobs: on the shelf only once converted, on the way while waiting,
// downloading or converting, and could not be added when a job failed.
func TestContentFollowShelfCounts(t *testing.T) {
	s, q, yt, f := contentFollows(t)
	iq := ingest.NewQueue(q)
	ts := contentTestServer(t, Deps{YouTube: yt, Ingest: iq, Follows: f})
	followTestSource(t, s, "f1", "following", map[string]string{
		"a-shelf": "added", "b-conv": "added", "c-fail": "added", "z-queued": "added", "w-wait": "waiting",
	})
	followTestJobs(t, q, iq, map[string]string{"a-shelf": "shelf", "b-conv": "converting", "c-fail": "failed", "z-queued": "queued"})
	var list followTestList
	_, body := contentTestGet(t, ts, "/api/admin/content/youtube/follows")
	contentTestDecode(t, body, &list)
	if len(list.Follows) != 1 {
		t.Fatalf("follows = %+v", list.Follows)
	}
	got := list.Follows[0]
	if want := (contentFollowShelf{OnShelf: 1, OnTheWay: 2, Failed: 1}); got.Shelf != want {
		t.Errorf("shelf = %+v, want %+v", got.Shelf, want)
	}
	if got.Counts.Added != 4 || got.Counts.Waiting != 1 {
		t.Errorf("counts = %+v", got.Counts)
	}
}

// TestContentFollowOnceLeavesWhenDone: a source added once (not followed)
// lists with its counts while a video is on the way, and leaves the list
// by itself once none is; its jobs stay.
func TestContentFollowOnceLeavesWhenDone(t *testing.T) {
	s, q, yt, f := contentFollows(t)
	iq := ingest.NewQueue(q)
	ts := contentTestServer(t, Deps{YouTube: yt, Ingest: iq, Follows: f})
	followTestSource(t, s, "o1", "once", map[string]string{"a-shelf": "added", "z-queued": "added"})
	followTestJobs(t, q, iq, map[string]string{"a-shelf": "shelf", "z-queued": "queued"})
	var list followTestList
	_, body := contentTestGet(t, ts, "/api/admin/content/youtube/follows")
	contentTestDecode(t, body, &list)
	if len(list.Follows) != 1 || list.Follows[0].Status != "once" ||
		list.Follows[0].Shelf != (contentFollowShelf{OnShelf: 1, OnTheWay: 1}) {
		t.Fatalf("while a video is queued: %+v", list.Follows)
	}
	// The last video downloads (and has no conversion): nothing is on the
	// way any more, so the source leaves the list.
	job, err := q.Claim(youtube.Kind)
	if err != nil || job == nil {
		t.Fatalf("claim: %+v %v", job, err)
	}
	if err := q.Done(job.ID); err != nil {
		t.Fatal(err)
	}
	list = followTestList{}
	_, body = contentTestGet(t, ts, "/api/admin/content/youtube/follows")
	contentTestDecode(t, body, &list)
	if len(list.Follows) != 0 {
		t.Errorf("after the last video: %+v, want none", list.Follows)
	}
	if views, err := f.List(); err != nil || len(views) != 0 {
		t.Errorf("stored follows = %+v, %v, want none", views, err)
	}
	if jl, _ := q.List(youtube.Kind); len(jl) != 2 {
		t.Errorf("jobs = %d, want both kept", len(jl))
	}
}
