package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/edementhon/kids-shelf/internal/core"
	"github.com/edementhon/kids-shelf/internal/ingest"
	"github.com/edementhon/kids-shelf/internal/jobs"
	"github.com/edementhon/kids-shelf/internal/youtube"
)

// contentStatusYouTubeRow is the part of a YouTube-list row these tests read.
type contentStatusYouTubeRow struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	State       string  `json:"state"`
	Place       int     `json:"place"`
	Progress    float64 `json:"progress"`
	SecondsLeft int     `json:"secondsLeft"`
	List        bool    `json:"list"`
	Show        string  `json:"show"`
	Next        *struct {
		ID          string  `json:"id"`
		State       string  `json:"state"`
		Stage       string  `json:"stage"`
		Place       int     `json:"place"`
		Progress    float64 `json:"progress"`
		SecondsLeft int     `json:"secondsLeft"`
		Message     string  `json:"message"`
		Retry       bool    `json:"retry"`
		Hint        string  `json:"hint"`
	} `json:"next"`
	Playlist *contentVideoCounts `json:"playlist"`
}

// TestContentJobListsTellTheStory sets up a playlist in every state a
// parent can meet and checks both lists: the YouTube list joins each
// downloaded video with its conversion (state, place in the converter's
// line, percent and time left, failure with its hint), sorts working,
// then needs-you, then waiting in the order things will run, then done,
// and counts the videos for the summary line and the playlist row; the
// conversions list sorts the same way with places per lane and its own
// summary.
func TestContentJobListsTellTheStory(t *testing.T) {
	q, err := jobs.New(contentTestStore(t))
	if err != nil {
		t.Fatal(err)
	}
	iq := ingest.NewQueue(q)
	yt := youtube.NewManager(q, iq, nil, nil, t.TempDir())
	ts := contentTestServer(t, Deps{YouTube: yt, Ingest: iq, DataDir: t.TempDir()})
	ctx := context.Background()

	ytIDs := map[string]string{}
	ytAdd := func(name string, p youtube.Payload) {
		t.Helper()
		id, err := q.Enqueue(youtube.Kind, p)
		if err != nil {
			t.Fatal(err)
		}
		ytIDs[name] = id
	}
	ytClaim := func(name string) {
		t.Helper()
		job, err := q.Claim(youtube.Kind)
		if err != nil || job == nil || job.ID != ytIDs[name] {
			t.Fatalf("Claim youtube = %+v, %v; want %s", job, err, name)
		}
	}
	ingIDs := map[string]string{}
	ingAdd := func(name, file string, src core.Source) {
		t.Helper()
		id, err := iq.Enqueue(ctx, file, src)
		if err != nil {
			t.Fatal(err)
		}
		ingIDs[name] = id
	}
	ingClaim := func(name string) {
		t.Helper()
		job, err := q.ClaimRange(ingest.JobKind, 0, 0)
		if err != nil || job == nil || job.ID != ingIDs[name] {
			t.Fatalf("Claim ingest = %+v, %v; want %s", job, err, name)
		}
	}
	video := func(n string) youtube.Payload {
		return youtube.Payload{URL: "https://www.youtube.com/watch?v=" + n, VideoID: n, Title: "Clip " + n, Uploader: "Pretend Channel"}
	}
	ytSource := func(n string) core.Source {
		return core.Source{Kind: core.SourceYouTube, Path: "https://www.youtube.com/watch?v=" + n, Title: "Clip " + n, Show: "Pretend Channel"}
	}
	gone := t.TempDir() + "/gone.mp4"

	// The conversions: v1 is on the shelf, v2 converting, a folder video
	// waits first in line, v3 waits behind it, v6 failed.
	ingAdd("v1", gone, ytSource("v1"))
	ingAdd("v2", gone, ytSource("v2"))
	ingAdd("folder", "/videos/Pretend Show/one.mkv", core.Source{Kind: core.SourceFolder, Path: "/videos/Pretend Show/one.mkv", Title: "one", Show: "Pretend Show"})
	ingAdd("v3", gone, ytSource("v3"))
	ingAdd("v6", gone, ytSource("v6"))
	ingClaim("v1")
	ingClaim("v2")
	if err := q.Done(ingIDs["v1"]); err != nil {
		t.Fatal(err)
	}
	j, err := q.Get(ingIDs["v2"])
	if err != nil {
		t.Fatal(err)
	}
	var pl map[string]any
	if err := json.Unmarshal(j.Payload, &pl); err != nil {
		t.Fatal(err)
	}
	pl["progress"], pl["seconds_left"] = 42, 360
	if err := q.Advance(ingIDs["v2"], ingest.StageConvert, pl); err != nil {
		t.Fatal(err)
	}
	if err := q.Fail(ingIDs["v6"], errors.New("This file is damaged or incomplete and can't be played.")); err != nil {
		t.Fatal(err)
	}

	// The downloads: the playlist opened into v1 to v5; v1 to v3 are
	// downloaded and handed over, v4 downloads, v5 waits. v6, pasted on
	// its own, was downloaded but could not be converted.
	ytAdd("playlist", youtube.Payload{URL: "https://www.youtube.com/playlist?list=PLpretend", Title: "Pretend Playlist"})
	for _, n := range []string{"v1", "v2", "v3", "v6", "v4", "v5"} {
		ytAdd(n, video(n))
	}
	// A link pasted on its own and not looked up yet waits behind v5.
	ytAdd("pasted", youtube.Payload{URL: "https://www.youtube.com/watch?v=v7"})
	ytClaim("playlist")
	if err := q.Advance(ytIDs["playlist"], "expand", youtube.Payload{
		URL: "https://www.youtube.com/playlist?list=PLpretend", Title: "Pretend Playlist",
		Videos: []string{"v1", "v2", "v3", "v4", "v5"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := q.Done(ytIDs["playlist"]); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"v1", "v2", "v3", "v6"} {
		ytClaim(n)
		p := video(n)
		p.Downloaded, p.IngestID = true, ingIDs[n]
		if err := q.Advance(ytIDs[n], "handoff", p); err != nil {
			t.Fatal(err)
		}
		if err := q.Done(ytIDs[n]); err != nil {
			t.Fatal(err)
		}
	}
	ytClaim("v4")
	p4 := video("v4")
	p4.Progress, p4.SecondsLeft = 10, 125
	if err := q.Advance(ytIDs["v4"], "download", p4); err != nil {
		t.Fatal(err)
	}

	code, body := contentTestGet(t, ts, "/api/admin/content/youtube")
	if code != http.StatusOK {
		t.Fatalf("youtube list: status %d: %s", code, body)
	}
	var ytList struct {
		Jobs    []contentStatusYouTubeRow `json:"jobs"`
		Summary contentVideoCounts        `json:"summary"`
	}
	contentTestDecode(t, body, &ytList)
	name := map[string]string{}
	for n, id := range ytIDs {
		name[id] = n
	}
	var order []string
	rows := map[string]contentStatusYouTubeRow{}
	for _, r := range ytList.Jobs {
		order = append(order, name[r.ID])
		rows[name[r.ID]] = r
	}
	// Working: the playlist (still filling up), the download, the
	// conversion. Then the failed conversion. Then the waiting download
	// and the downloaded video waiting to convert. Then the finished one.
	if want := []string{"playlist", "v4", "v2", "v6", "v5", "pasted", "v3", "v1"}; !slices.Equal(order, want) {
		t.Errorf("youtube order = %v, want %v", order, want)
	}
	if r := rows["v4"]; r.State != jobs.StateRunning || r.Progress != 10 || r.SecondsLeft != 125 || r.Next != nil {
		t.Errorf("downloading row = %+v, want 10%% with 125 s left and no conversion yet", r)
	}
	if r := rows["v5"]; r.State != jobs.StateQueued || r.Place != 1 {
		t.Errorf("waiting download = %+v, want next in line", r)
	}
	if r := rows["v2"]; r.Next == nil || r.Next.State != jobs.StateRunning || r.Next.Stage != ingest.StageConvert || r.Next.Progress != 42 || r.Next.SecondsLeft != 360 {
		t.Errorf("converting row next = %+v, want converting at 42%% with 360 s left", r.Next)
	}
	if r := rows["v3"]; r.Next == nil || r.Next.State != jobs.StateQueued || r.Next.Place != 2 {
		t.Errorf("waiting-to-convert row next = %+v, want queued 2nd in line (behind the folder video)", r.Next)
	}
	if r := rows["v1"]; r.Next == nil || r.Next.State != jobs.StateDone || r.Show != "Pretend Channel" {
		t.Errorf("finished row = %+v, want on the shelf in Pretend Channel", r)
	}
	if r := rows["v6"]; r.Next == nil || r.Next.State != jobs.StateFailed || r.Next.ID != ingIDs["v6"] ||
		r.Next.Message == "" || r.Next.Retry || r.Next.Hint == "" {
		t.Errorf("failed conversion row next = %+v, want the failure, no retry (file gone) and a hint", r.Next)
	}
	wantPlaylist := contentVideoCounts{Videos: 5, Downloading: 1, Converting: 1, Waiting: 2, OnShelf: 1}
	if r := rows["playlist"]; !r.List || r.Playlist == nil || *r.Playlist != wantPlaylist {
		t.Errorf("playlist row = %+v / %+v, want counts %+v", r, r.Playlist, wantPlaylist)
	}
	if r := rows["pasted"]; r.Place != 2 {
		t.Errorf("pasted link place = %d, want 2nd in line", r.Place)
	}
	wantSummary := contentVideoCounts{Videos: 7, Downloading: 1, Converting: 1, Waiting: 3, OnShelf: 1, NeedsYou: 1}
	if ytList.Summary != wantSummary {
		t.Errorf("youtube summary = %+v, want %+v", ytList.Summary, wantSummary)
	}

	code, body = contentTestGet(t, ts, "/api/admin/content/ingest")
	if code != http.StatusOK {
		t.Fatalf("ingest list: status %d: %s", code, body)
	}
	var ingList struct {
		Jobs []struct {
			ID          string  `json:"id"`
			State       string  `json:"state"`
			Place       int     `json:"place"`
			Progress    float64 `json:"progress"`
			SecondsLeft int     `json:"seconds_left"`
		} `json:"jobs"`
		Summary contentIngestCounts `json:"summary"`
	}
	contentTestDecode(t, body, &ingList)
	ingName := map[string]string{}
	for n, id := range ingIDs {
		ingName[id] = n
	}
	order = nil
	for _, r := range ingList.Jobs {
		n := ingName[r.ID]
		order = append(order, n)
		switch n {
		case "v2":
			if r.Progress != 42 || r.SecondsLeft != 360 {
				t.Errorf("converting job = %+v, want 42%% with 360 s left", r)
			}
		case "folder", "v3":
			if want := map[string]int{"folder": 1, "v3": 2}[n]; r.Place != want {
				t.Errorf("place of %s = %d, want %d", n, r.Place, want)
			}
		}
	}
	if want := []string{"v2", "v6", "folder", "v3", "v1"}; !slices.Equal(order, want) {
		t.Errorf("ingest order = %v, want %v", order, want)
	}
	if want := (contentIngestCounts{Converting: 1, Waiting: 2, NeedsYou: 1}); ingList.Summary != want {
		t.Errorf("ingest summary = %+v, want %+v", ingList.Summary, want)
	}
	// The job in place 1 is the one the sort lane takes next.
	ingClaim("folder")
}
