package youtube

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/edementhon/kids-shelf/internal/jobs"
)

// TestJobsListOrderAndPlace: the YouTube list comes running first, then
// failed and paused, then waiting in download order (each with its place
// in line, matching what Run claims next), then done. A running download
// carries yt-dlp's time left.
func TestJobsListOrderAndPlace(t *testing.T) {
	m, _, q, _ := newFakeEnv(t, "single")
	ids := map[string]string{}
	add := func(name string) {
		t.Helper()
		id, err := q.Enqueue(Kind, Payload{URL: watchURL(name), VideoID: name, Title: name})
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = id
	}
	claimNext := func(name string) {
		t.Helper()
		job, err := q.Claim(Kind)
		if err != nil || job == nil || job.ID != ids[name] {
			t.Fatalf("Claim = %+v, %v; want %s", job, err, name)
		}
	}
	for _, n := range []string{"done", "failed", "running", "waitA", "waitB", "waitC"} {
		add(n)
	}
	claimNext("done")
	claimNext("failed")
	claimNext("running")
	if err := q.Done(ids["done"]); err != nil {
		t.Fatal(err)
	}
	if err := q.Fail(ids["failed"], errors.New("gone")); err != nil {
		t.Fatal(err)
	}
	if err := q.Advance(ids["running"], stageDownload, Payload{URL: watchURL("running"), Title: "running", Progress: 30, SecondsLeft: 95}); err != nil {
		t.Fatal(err)
	}
	list, err := m.Jobs()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, v := range list {
		got = append(got, v.Title)
		switch v.Title {
		case "running":
			if v.Progress != 30 || v.SecondsLeft != 95 {
				t.Errorf("running download = %v%% / %ds, want 30%% / 95s", v.Progress, v.SecondsLeft)
			}
		case "waitA", "waitB", "waitC":
			if want := int(v.Title[4]-'A') + 1; v.Place != want {
				t.Errorf("place of %s = %d, want %d", v.Title, v.Place, want)
			}
		default:
			if v.Place != 0 {
				t.Errorf("%s has place %d, want none (not waiting)", v.Title, v.Place)
			}
		}
	}
	if want := []string{"running", "failed", "waitA", "waitB", "waitC", "done"}; !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
	// The job in place 1 is the one the downloader takes next.
	claimNext("waitA")
}

// TestPlaylistKeepsItsVideos: a playlist job, once opened, lists as one
// row carrying the ids of its videos and the playlist's name, so the page
// can say how its videos are doing.
func TestPlaylistKeepsItsVideos(t *testing.T) {
	m, _, _, _ := newFakeEnv(t, "playlist3")
	if _, err := m.Enqueue(context.Background(), "https://www.youtube.com/playlist?list=PLfake"); err != nil {
		t.Fatal(err)
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	list, err := m.Jobs()
	if err != nil {
		t.Fatal(err)
	}
	var lists []JobView
	for _, v := range list {
		if v.List {
			lists = append(lists, v)
		} else if v.State != jobs.StateDone || v.VideoID == "" || v.IngestID == "" {
			t.Errorf("video row = %+v, want done with its video and ingest ids", v)
		}
	}
	if len(lists) != 1 {
		t.Fatalf("playlist rows = %d, want 1", len(lists))
	}
	pl := lists[0]
	if want := []string{"vid-aaa", "vid-bbb", "vid-ccc"}; !slices.Equal(pl.Videos, want) {
		t.Errorf("playlist videos = %v, want %v", pl.Videos, want)
	}
	if pl.Title != "Fake Playlist" {
		t.Errorf("playlist title = %q, want the playlist's name", pl.Title)
	}
}

// TestParseETA reads yt-dlp's time left from its progress lines.
func TestParseETA(t *testing.T) {
	for line, want := range map[string]int{
		"[download]  50.0% of 10.00MiB at 5.00MiB/s ETA 00:01":           1,
		"[download]  12.5% of ~ 300.00MiB at 2.00MiB/s ETA 02:05":        125,
		"[download]   3.0% of 2.00GiB at 1.00MiB/s ETA 1:02:03 (frag 1)": 3723,
		"[download]  10.0% of ~ 10.00MiB at Unknown B/s ETA Unknown":     0,
		"[download] 100% of 10.00MiB in 00:00:05 at 2.00MiB/s":           0,
	} {
		if got := parseETA(line); got != want {
			t.Errorf("parseETA(%q) = %d, want %d", line, got, want)
		}
	}
}
