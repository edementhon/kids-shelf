package web

// Handler tests for the /admin/content area. Every test drives the real
// services (store, jobs queue, library, youtube and kiwix managers) over a
// temp store like the app runs them; the only injected piece is the
// free-space probe (contentFreeSpace), which cannot be measured in a temp
// dir.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edementhon/kids-shelf/internal/core"
	"github.com/edementhon/kids-shelf/internal/disk"
	"github.com/edementhon/kids-shelf/internal/ingest"
	"github.com/edementhon/kids-shelf/internal/jobs"
	"github.com/edementhon/kids-shelf/internal/kiwix"
	"github.com/edementhon/kids-shelf/internal/library"
	"github.com/edementhon/kids-shelf/internal/store"
	"github.com/edementhon/kids-shelf/internal/youtube"
)

func contentTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// contentTestServer serves the app with the parent PIN already made
// (setUpParent), so the helpers below call /api/admin/content/* as the
// signed-in parent, like the page does.
func contentTestServer(t *testing.T, deps Deps) *httptest.Server {
	t.Helper()
	if deps.DataDir == "" {
		deps.DataDir = t.TempDir() // never the shared temp dir's PIN file
	}
	ts := httptest.NewServer(NewApp("content-test", deps).Mux())
	t.Cleanup(ts.Close)
	setUpParent(t, ts)
	return ts
}

func contentTestGet(t *testing.T, ts *httptest.Server, path string) (int, []byte) {
	t.Helper()
	resp, err := parentClient(ts.URL).Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	if _, err := b.ReadFrom(resp.Body); err != nil {
		t.Fatalf("GET %s: reading body: %v", path, err)
	}
	return resp.StatusCode, b.Bytes()
}

// contentTestPost posts a JSON body like the page does: application/json
// plus a same-origin Origin header.
func contentTestPost(t *testing.T, ts *httptest.Server, path string, body any) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("POST %s: encoding body: %v", path, err)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://"+req.Host)
	resp, err := parentClient(ts.URL).Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	if _, err := b.ReadFrom(resp.Body); err != nil {
		t.Fatalf("POST %s: reading body: %v", path, err)
	}
	return resp.StatusCode, b.Bytes()
}

func contentTestDecode(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decoding %q: %v", b, err)
	}
}

func contentTestError(t *testing.T, b []byte) string {
	t.Helper()
	var v struct {
		Error string `json:"error"`
	}
	contentTestDecode(t, b, &v)
	return v.Error
}

// TestContentYouTubeAddListRetry drives the real Manager and jobs queue
// through the API: a paste enqueues a job whose title, stage, progress and
// plain message are all listed; a failed job retries into a fresh queued
// job.
func TestContentYouTubeAddListRetry(t *testing.T) {
	q, err := jobs.New(contentTestStore(t))
	if err != nil {
		t.Fatal(err)
	}
	yt := youtube.NewManager(q, ingest.NewQueue(q), nil, nil, t.TempDir())
	ts := contentTestServer(t, Deps{YouTube: yt, DataDir: t.TempDir()})

	// The page shell loads without the PIN.
	code, body := contentTestGet(t, ts, "/admin/content")
	if code != http.StatusOK || !strings.Contains(string(body), "add videos") {
		t.Fatalf("GET /admin/content = %d, want the page shell", code)
	}

	// A real YouTube video URL enqueues as its canonical form.
	const url = "https://www.youtube.com/watch?v=dQw4w9WgXcQ"
	code, body = contentTestPost(t, ts, "/api/admin/content/youtube", map[string]any{"url": url})
	if code != http.StatusOK {
		t.Fatalf("add: status %d: %s", code, body)
	}
	var added struct {
		ID string `json:"id"`
	}
	contentTestDecode(t, body, &added)
	if added.ID == "" {
		t.Fatal("add: no job id returned")
	}

	// A non-YouTube address is refused with the plain link message.
	if code, body = contentTestPost(t, ts, "/api/admin/content/youtube", map[string]any{"url": "not-a-url"}); code != http.StatusBadRequest {
		t.Fatalf("bad link: status %d, want 400", code)
	}
	if !strings.Contains(contentTestError(t, body), "YouTube link") {
		t.Errorf("bad link error = %q, want the plain hint", body)
	}

	// The job lists with the pasted URL as its title while queued.
	code, body = contentTestGet(t, ts, "/api/admin/content/youtube")
	if code != http.StatusOK {
		t.Fatalf("list: status %d", code)
	}
	var list struct {
		Jobs []struct {
			ID       string  `json:"id"`
			URL      string  `json:"url"`
			Title    string  `json:"title"`
			State    string  `json:"state"`
			Stage    string  `json:"stage"`
			Progress float64 `json:"progress"`
			Message  string  `json:"message"`
		} `json:"jobs"`
	}
	contentTestDecode(t, body, &list)
	if len(list.Jobs) != 1 {
		t.Fatalf("list has %d jobs, want 1", len(list.Jobs))
	}
	j := list.Jobs[0]
	if j.ID != added.ID || j.URL != url || j.Title != url || j.State != "queued" {
		t.Errorf("queued job = %+v", j)
	}

	// A claimed and advanced job shows its resolve-time title and progress.
	if _, err := q.Claim(youtube.Kind); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := q.Advance(added.ID, "download", youtube.Payload{
		URL: url, Title: "Fun Video", Progress: 42,
	}); err != nil {
		t.Fatalf("advance: %v", err)
	}
	code, body = contentTestGet(t, ts, "/api/admin/content/youtube")
	contentTestDecode(t, body, &list)
	j = list.Jobs[0]
	if j.State != "running" || j.Stage != "download" || j.Title != "Fun Video" || j.Progress != 42 {
		t.Errorf("running job = %+v", j)
	}

	// A failure shows its plain message; retry enqueues the same URL again,
	// and the failed job it replaces leaves the list.
	if err := q.Fail(added.ID, errors.New("The download stopped.")); err != nil {
		t.Fatalf("fail: %v", err)
	}
	code, body = contentTestGet(t, ts, "/api/admin/content/youtube")
	contentTestDecode(t, body, &list)
	j = list.Jobs[0]
	if j.State != "failed" || j.Message != "The download stopped." {
		t.Errorf("failed job = %+v", j)
	}
	failedID := added.ID
	code, body = contentTestPost(t, ts, "/api/admin/content/youtube/retry", map[string]any{"id": added.ID})
	if code != http.StatusOK {
		t.Fatalf("retry: status %d: %s", code, body)
	}
	contentTestDecode(t, body, &added)
	code, body = contentTestGet(t, ts, "/api/admin/content/youtube")
	contentTestDecode(t, body, &list)
	if len(list.Jobs) != 1 || list.Jobs[0].ID == failedID {
		t.Fatalf("after retry: jobs %+v, want only the new job", list.Jobs)
	}
	if list.Jobs[0].State != "queued" || list.Jobs[0].URL != url || list.Jobs[0].Title != url {
		t.Errorf("retried job = %+v", list.Jobs[0])
	}

	// A queued job cannot be retried.
	code, _ = contentTestPost(t, ts, "/api/admin/content/youtube/retry", map[string]any{"id": list.Jobs[0].ID})
	if code != http.StatusBadRequest {
		t.Errorf("retry of a queued job: status %d, want 400", code)
	}
}

// TestContentYouTubeLinkGating checks the URL gate: only real YouTube
// addresses enqueue, every accepted shape is canonicalized server-side, and
// anything else is refused with the plain link message.
func TestContentYouTubeLinkGating(t *testing.T) {
	q, err := jobs.New(contentTestStore(t))
	if err != nil {
		t.Fatal(err)
	}
	yt := youtube.NewManager(q, ingest.NewQueue(q), nil, nil, t.TempDir())
	ts := contentTestServer(t, Deps{YouTube: yt, DataDir: t.TempDir()})

	queuedURL := func() string {
		t.Helper()
		_, body := contentTestGet(t, ts, "/api/admin/content/youtube")
		var list struct {
			Jobs []struct {
				URL string `json:"url"`
			} `json:"jobs"`
		}
		contentTestDecode(t, body, &list)
		if len(list.Jobs) == 0 {
			t.Fatal("no youtube jobs queued")
		}
		return list.Jobs[0].URL
	}

	// youtu.be/ID is the canonical watch URL after enqueue.
	code, body := contentTestPost(t, ts, "/api/admin/content/youtube",
		map[string]any{"url": "https://youtu.be/dQw4w9WgXcQ"})
	if code != http.StatusOK {
		t.Fatalf("youtu.be add: status %d: %s", code, body)
	}
	if got := queuedURL(); got != "https://www.youtube.com/watch?v=dQw4w9WgXcQ" {
		t.Errorf("youtu.be canonical = %q, want the watch URL", got)
	}

	// Hosts and shapes outside the allowlist are refused with the plain
	// message, and nothing extra is enqueued.
	for _, bad := range []string{
		"https://evil.example/watch?v=dQw4w9WgXcQ",
		"https://www.notyoutube.com/watch?v=dQw4w9WgXcQ",
		"https://www.youtube.com/embed/dQw4w9WgXcQ",
		"https://www.youtube.com/watch?v=short",
		"https://youtu.be/",
		"evil.example/watch?v=dQw4w9WgXcQ",
		"javascript:alert(1)",
	} {
		code, body := contentTestPost(t, ts, "/api/admin/content/youtube", map[string]any{"url": bad})
		if code != http.StatusBadRequest {
			t.Errorf("bad link %q: status %d, want 400 (%s)", bad, code, body)
		}
		if msg := contentTestError(t, body); msg != msgContentBadLink || !strings.Contains(msg, "Copy link") {
			t.Errorf("bad link %q error = %q, want the message with its next step", bad, msg)
		}
	}

	// A link typed without https:// is still a YouTube link.
	code, body = contentTestPost(t, ts, "/api/admin/content/youtube",
		map[string]any{"url": "youtube.com/watch?v=jNQXAC9IVRw"})
	if code != http.StatusOK {
		t.Fatalf("scheme-less add: status %d: %s", code, body)
	}
	if got := queuedURL(); got != "https://www.youtube.com/watch?v=jNQXAC9IVRw" {
		t.Errorf("scheme-less canonical = %q, want the watch URL", got)
	}
	_, body = contentTestGet(t, ts, "/api/admin/content/youtube")
	var list struct {
		Jobs []any `json:"jobs"`
	}
	contentTestDecode(t, body, &list)
	if len(list.Jobs) != 2 {
		t.Errorf("jobs after rejected links = %d, want the youtu.be and scheme-less ones", len(list.Jobs))
	}
}

// TestContentSourcesAddListRemove exercises source registration through the
// real library service: a folder with videos is added and reports how many
// it holds; a file, an empty folder, a folder already on the list (also
// written with a trailing slash or as another kind), a relative path, an
// unknown kind and a missing path are refused with plain words; Explorer's
// quoted "Copy as path" is accepted; Scan says how many files it handed
// over; a folder that went missing is flagged; removal works by path.
func TestContentSourcesAddListRemove(t *testing.T) {
	s := contentTestStore(t)
	q, err := jobs.New(s)
	if err != nil {
		t.Fatal(err)
	}
	lib, err := library.New(s)
	if err != nil {
		t.Fatal(err)
	}
	ingQ := ingest.NewQueue(q)
	ts := contentTestServer(t, Deps{Library: lib, Ingest: ingQ, DataDir: t.TempDir()})

	add := func(path, kind string) (int, string, int) {
		t.Helper()
		code, body := contentTestPost(t, ts, "/api/admin/content/sources", map[string]any{"path": path, "kind": kind})
		if code != http.StatusOK {
			return code, contentTestError(t, body), 0
		}
		var ok struct {
			Videos int `json:"videos"`
		}
		contentTestDecode(t, body, &ok)
		return code, "", ok.Videos
	}

	dir := t.TempDir()
	for _, f := range []string{"Loose.mp4", filepath.Join("Bluey", "ep1.mkv"), filepath.Join("Bluey", "notes.txt")} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), []byte("v"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if code, msg, n := add(dir, "folder"); code != http.StatusOK || n != 2 {
		t.Fatalf("add folder: status %d %q, %d videos; want 200 with 2 videos", code, msg, n)
	}

	refusals := []struct {
		name, path, kind, want string
	}{
		{"same folder again", dir, "folder", "already on the list"},
		{"same folder, trailing slash", dir + string(filepath.Separator), "folder", "already on the list"},
		{"same folder as a NAS", dir, "nas", "already on the list"},
		{"a file", filepath.Join(dir, "Loose.mp4"), "folder", "file, not a folder"},
		{"an empty folder", t.TempDir(), "folder", "no videos in that folder"},
		{"a relative path", "Videos", "folder", "whole path"},
		{"an unknown kind", dir, "usb", "Choose"},
		{"a missing path", filepath.Join(dir, "nope"), "folder", "couldn't find"},
	}
	for _, r := range refusals {
		code, msg, _ := add(r.path, r.kind)
		if code != http.StatusBadRequest || !strings.Contains(msg, r.want) {
			t.Errorf("%s: status %d error %q, want 400 saying %q", r.name, code, msg, r.want)
		}
	}
	textOnly := t.TempDir()
	if err := os.WriteFile(filepath.Join(textOnly, "readme.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, msg, _ := add(textOnly, "folder"); code != http.StatusBadRequest || !strings.Contains(msg, "no videos") {
		t.Errorf("folder with no videos: status %d error %q", code, msg)
	}

	// Windows Explorer's "Copy as path" wraps the path in quotes.
	nas := t.TempDir()
	if err := os.MkdirAll(filepath.Join(nas, "Show"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nas, "Show", "a.mp4"), []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, msg, n := add(` "`+nas+`" `, "nas"); code != http.StatusOK || n != 1 {
		t.Fatalf("quoted path: status %d %q, %d videos; want 200 with 1 video", code, msg, n)
	}

	var list struct {
		Sources []struct {
			Path    string `json:"path"`
			Kind    string `json:"kind"`
			Problem string `json:"problem"`
		} `json:"sources"`
	}
	_, body := contentTestGet(t, ts, "/api/admin/content/sources")
	contentTestDecode(t, body, &list)
	if len(list.Sources) != 2 {
		t.Fatalf("sources = %+v, want the folder and the NAS share", list.Sources)
	}
	for _, src := range list.Sources {
		if src.Path != dir && src.Path != nas {
			t.Errorf("source path %q, want the cleaned, unquoted path", src.Path)
		}
		if src.Problem != "" {
			t.Errorf("source %s problem %q, want none", src.Path, src.Problem)
		}
	}

	// Scan reports what it handed over: the loose video, the show's video
	// and the NAS video, then nothing on a second pass.
	for i, want := range []int{3, 0} {
		code, body := contentTestPost(t, ts, "/api/admin/content/scan", nil)
		if code != http.StatusOK {
			t.Fatalf("scan %d: status %d: %s", i, code, body)
		}
		var res struct {
			Added int `json:"added"`
		}
		contentTestDecode(t, body, &res)
		if res.Added != want {
			t.Errorf("scan %d added %d, want %d", i, res.Added, want)
		}
	}

	// A share that is no longer there is flagged in plain words.
	if err := os.RemoveAll(nas); err != nil {
		t.Fatal(err)
	}
	_, body = contentTestGet(t, ts, "/api/admin/content/sources")
	contentTestDecode(t, body, &list)
	for _, src := range list.Sources {
		if src.Path == nas && !strings.Contains(src.Problem, "NAS") {
			t.Errorf("missing NAS share problem = %q, want the NAS hint", src.Problem)
		}
	}

	// Removal by path.
	for _, p := range []string{dir, nas} {
		code, body := contentTestPost(t, ts, "/api/admin/content/sources/remove", map[string]any{"path": p})
		if code != http.StatusOK {
			t.Fatalf("remove: status %d: %s", code, body)
		}
	}
	_, body = contentTestGet(t, ts, "/api/admin/content/sources")
	contentTestDecode(t, body, &list)
	if len(list.Sources) != 0 {
		t.Errorf("after remove: %d sources, want 0", len(list.Sources))
	}
}

// TestContentIngestListsStates checks the conversions list maps the queue's
// states, stages and plain messages faithfully.
func TestContentIngestListsStates(t *testing.T) {
	q, err := jobs.New(contentTestStore(t))
	if err != nil {
		t.Fatal(err)
	}
	ingQ := ingest.NewQueue(q)
	ts := contentTestServer(t, Deps{Ingest: ingQ, DataDir: t.TempDir()})

	file := filepath.Join(t.TempDir(), "clip.mp4")
	id, err := ingQ.Enqueue(context.Background(), file, core.Source{
		Kind: core.SourceFolder, Path: file, Title: "clip.mp4", Show: "My Shows",
	})
	if err != nil {
		t.Fatal(err)
	}
	const diskMsg = "The disk is full. Delete some videos or move your media to a bigger drive, then try again."
	if err := q.Pause(id, diskMsg); err != nil {
		t.Fatal(err)
	}

	file2 := filepath.Join(t.TempDir(), "other.mp4")
	id2, err := ingQ.Enqueue(context.Background(), file2, core.Source{
		Kind: core.SourceFolder, Path: file2, Title: "other.mp4", Show: "My Shows",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Advance(id2, ingest.StageConvert, nil); err != nil {
		t.Fatal(err)
	}
	if err := q.Fail(id2, errors.New("This file is damaged or incomplete and can't be played.")); err != nil {
		t.Fatal(err)
	}

	code, body := contentTestGet(t, ts, "/api/admin/content/ingest")
	if code != http.StatusOK {
		t.Fatalf("list: status %d", code)
	}
	var list struct {
		Jobs []struct {
			ID      string `json:"id"`
			State   string `json:"state"`
			Stage   string `json:"stage"`
			Message string `json:"message"`
			File    string `json:"file"`
			Title   string `json:"title"`
		} `json:"jobs"`
	}
	contentTestDecode(t, body, &list)
	if len(list.Jobs) != 2 {
		t.Fatalf("list has %d jobs, want 2", len(list.Jobs))
	}
	newest, oldest := list.Jobs[0], list.Jobs[1] // most recent first
	if newest.ID != id2 || newest.State != "failed" || !strings.Contains(newest.Message, "damaged") {
		t.Errorf("failed job = %+v", newest)
	}
	if oldest.ID != id || oldest.State != "paused" || oldest.Message != diskMsg || oldest.File != file {
		t.Errorf("paused job = %+v", oldest)
	}
	// Both jobs were enqueued with a title (the source's episode name), so
	// the list leads with it instead of the full path.
	if newest.Title != "other.mp4" || oldest.Title != "clip.mp4" {
		t.Errorf("titles = %q, %q; want the source episode titles", newest.Title, oldest.Title)
	}
}

// TestContentIngestTitle checks the name the conversions list leads with:
// the episode title the producer recorded, then the show name, and only the
// full source path as a last resort.
func TestContentIngestTitle(t *testing.T) {
	cases := []struct {
		name string
		job  ingest.JobView
		want string
	}{
		{
			name: "episode title",
			job: ingest.JobView{File: "/media/Shows/clip.mp4", Source: core.Source{
				Path: "/media/Shows/clip.mp4", Title: "Magic Xylophone", Show: "Bluey",
			}},
			want: "Magic Xylophone",
		},
		{
			name: "show name when no episode title",
			job: ingest.JobView{File: "/media/Shows/clip.mp4", Source: core.Source{
				Path: "/media/Shows/clip.mp4", Show: "Bluey",
			}},
			want: "Bluey",
		},
		{
			name: "path fallback",
			job:  ingest.JobView{File: "/media/Shows/clip.mp4"},
			want: "/media/Shows/clip.mp4",
		},
		{
			name: "blank title falls through",
			job: ingest.JobView{File: "/media/Shows/clip.mp4", Source: core.Source{
				Title: "   ", Show: "Bluey",
			}},
			want: "Bluey",
		},
	}
	for _, tc := range cases {
		if got := contentIngestTitle(tc.job); got != tc.want {
			t.Errorf("%s: contentIngestTitle = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestContentSortFlavours checks the catalog page puts the with-pictures
// edition of a library before its text-only and short versions while keeping
// the feed's order between different libraries.
func TestContentSortFlavours(t *testing.T) {
	entries := []contentZIM{
		{Name: "wikipedia_en_100_nopic", Title: "Wikipedia top 100", Flavour: "nopic"},
		{Name: "vikidia_en_all_maxi", Title: "Vikidia", Flavour: "maxi"},
		{Name: "wikipedia_en_100_maxi", Title: "Wikipedia top 100", Flavour: "maxi"},
		{Name: "other_en_all", Title: "Something else", Flavour: ""},
		{Name: "wikipedia_en_100_mini", Title: "Wikipedia top 100", Flavour: "mini"},
	}
	contentSortFlavours(entries)
	got := make([]string, 0, len(entries))
	for _, e := range entries {
		got = append(got, e.Name)
	}
	want := []string{
		"wikipedia_en_100_maxi", "wikipedia_en_100_nopic", "wikipedia_en_100_mini",
		"vikidia_en_all_maxi", "other_en_all",
	}
	if !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// TestContentKiwixCatalogAndState serves the saved Kiwix feeds and checks
// the catalog endpoint passes language/category/search through, the filter
// lists decode, and the state endpoint reports free space.
func TestContentKiwixCatalogAndState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/catalog/v2/entries":
			http.ServeFile(w, r, filepath.Join("..", "kiwix", "testdata", "entries_eng_20.xml"))
		case "/catalog/v2/categories":
			http.ServeFile(w, r, filepath.Join("..", "kiwix", "testdata", "categories.xml"))
		case "/catalog/v2/languages":
			http.ServeFile(w, r, filepath.Join("..", "kiwix", "testdata", "languages.xml"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	s := contentTestStore(t)
	q, err := jobs.New(s)
	if err != nil {
		t.Fatal(err)
	}
	km, err := kiwix.NewManager(s, q, disk.NewReservations(q), filepath.Join(t.TempDir(), "libraries"))
	if err != nil {
		t.Fatal(err)
	}
	catalog := kiwix.NewCatalog(srv.URL, kiwix.AllowLoopback(srv.Client()))

	oldFree := contentFreeSpace
	contentFreeSpace = func(string) (uint64, error) { return 5 << 30, nil }
	t.Cleanup(func() { contentFreeSpace = oldFree })

	ts := contentTestServer(t, Deps{Catalog: catalog, Kiwix: km, MediaDir: t.TempDir(), DataDir: t.TempDir()})

	code, body := contentTestGet(t, ts, "/api/admin/content/kiwix/catalog?lang=eng&count=20")
	if code != http.StatusOK {
		t.Fatalf("catalog: status %d: %s", code, body)
	}
	var res struct {
		Total   int `json:"total"`
		Start   int `json:"start"`
		Count   int `json:"count"`
		Entries []struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			Title        string `json:"title"`
			Summary      string `json:"summary"`
			Language     string `json:"language"`
			Category     string `json:"category"`
			ArticleCount int    `json:"article_count"`
			Size         int64  `json:"size"`
			Meta4URL     string `json:"meta4_url"`
		} `json:"entries"`
	}
	contentTestDecode(t, body, &res)
	if res.Total != 1311 || res.Count != 20 || len(res.Entries) != 20 {
		t.Fatalf("catalog = total %d count %d entries %d, want 1311/20/20", res.Total, res.Count, len(res.Entries))
	}
	e := res.Entries[0]
	if e.Title != "スタック・オーバーフロー" || e.ArticleCount != 46670 || e.Size != 236055552 {
		t.Errorf("entry[0] = %+v", e)
	}
	if !strings.HasPrefix(e.Meta4URL, "https://lb.download.kiwix.org/") || e.Category != "stack_exchange" || e.Language != "eng,jpn" {
		t.Errorf("entry[0] link = %q category %q language %q", e.Meta4URL, e.Category, e.Language)
	}

	code, body = contentTestGet(t, ts, "/api/admin/content/kiwix/categories")
	var cats struct {
		Categories []string `json:"categories"`
	}
	contentTestDecode(t, body, &cats)
	if code != http.StatusOK || len(cats.Categories) != 16 {
		t.Fatalf("categories: status %d, %d entries", code, len(cats.Categories))
	}

	code, body = contentTestGet(t, ts, "/api/admin/content/kiwix/languages")
	var langs struct {
		Languages []struct {
			Code  string `json:"code"`
			Name  string `json:"name"`
			Count int    `json:"count"`
		} `json:"languages"`
	}
	contentTestDecode(t, body, &langs)
	if code != http.StatusOK || len(langs.Languages) != 337 {
		t.Fatalf("languages: status %d, %d entries", code, len(langs.Languages))
	}
	found := false
	for _, l := range langs.Languages {
		if l.Code == "eng" {
			found = true
			if l.Name != "English" || l.Count != 1311 {
				t.Errorf("eng = %+v", l)
			}
		}
	}
	if !found {
		t.Error("languages feed has no eng entry")
	}

	code, body = contentTestGet(t, ts, "/api/admin/content/kiwix")
	var state struct {
		Installed []any `json:"installed"`
		Downloads []any `json:"downloads"`
		FreeBytes int64 `json:"free_bytes"`
	}
	contentTestDecode(t, body, &state)
	if code != http.StatusOK || len(state.Installed) != 0 || len(state.Downloads) != 0 {
		t.Fatalf("state = %+v", state)
	}
	if state.FreeBytes != 5<<30 {
		t.Errorf("free_bytes = %d, want %d", state.FreeBytes, 5<<30)
	}
}

// TestContentKiwixDownloadFreeSpace covers the two sides of the download
// gate: a library that does not fit is refused with a plain message and no
// job is created; with space, the same entry enqueues into the real queue.
func TestContentKiwixDownloadFreeSpace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/catalog/v2/entries" {
			http.ServeFile(w, r, filepath.Join("..", "kiwix", "testdata", "entries_eng_20.xml"))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	s := contentTestStore(t)
	q, err := jobs.New(s)
	if err != nil {
		t.Fatal(err)
	}
	km, err := kiwix.NewManager(s, q, disk.NewReservations(q), filepath.Join(t.TempDir(), "libraries"))
	if err != nil {
		t.Fatal(err)
	}
	catalog := kiwix.NewCatalog(srv.URL, kiwix.AllowLoopback(srv.Client()))

	free := uint64(100 << 20) // 100 MiB: smaller than the first catalog entry (225 MiB)
	oldFree := contentFreeSpace
	contentFreeSpace = func(string) (uint64, error) { return free, nil }
	t.Cleanup(func() { contentFreeSpace = oldFree })

	ts := contentTestServer(t, Deps{Catalog: catalog, Kiwix: km, MediaDir: t.TempDir(), DataDir: t.TempDir()})

	// Take the first catalog entry exactly as the page would send its
	// identifier back.
	_, body := contentTestGet(t, ts, "/api/admin/content/kiwix/catalog?lang=eng&count=1")
	var res struct {
		Entries []map[string]any `json:"entries"`
	}
	contentTestDecode(t, body, &res)
	if len(res.Entries) == 0 {
		t.Fatal("catalog returned no entries")
	}
	entry := res.Entries[0]
	if size := int64(entry["size"].(float64)); size <= int64(free) {
		t.Fatalf("test entry (%d B) must exceed the stubbed free space (%d B)", size, free)
	}
	ident := map[string]any{
		"id": entry["id"], "name": entry["name"], "flavour": entry["flavour"],
	}

	// A client-supplied meta4 URL or size never reaches the downloader: the
	// id here is not in the catalog, so the request fails even though the
	// meta4 URL and size look plausible.
	code, body := contentTestPost(t, ts, "/api/admin/content/kiwix/download", map[string]any{
		"id": "urn:uuid:not-in-the-catalog-0000", "name": "no_such_zim_en_all",
		"meta4_url": "https://evil.example/x.zim.meta4", "size": 1,
	})
	if code != http.StatusBadRequest {
		t.Fatalf("unknown entry download: status %d, want 400: %s", code, body)
	}

	// Short on space: refused, plain message, nothing enqueued.
	code, body = contentTestPost(t, ts, "/api/admin/content/kiwix/download", ident)
	if code != http.StatusBadRequest {
		t.Fatalf("short download: status %d, want 400: %s", code, body)
	}
	jlist, err := q.List(kiwix.Kind)
	if err != nil {
		t.Fatalf("list after refused download: %v", err)
	}
	if len(jlist) != 0 {
		t.Fatalf("jobs after refused download = %d, want 0", len(jlist))
	}

	// Plenty of space: the identifier enqueues the catalog's own entry,
	// with the catalog's meta4 URL and size even when the request tries to
	// supply its own.
	free = 100 << 30 // 100 GiB
	sneaky := map[string]any{
		"id": entry["id"], "name": entry["name"], "flavour": entry["flavour"],
		"meta4_url": "https://evil.example/x.zim.meta4", "size": 1,
	}
	code, body = contentTestPost(t, ts, "/api/admin/content/kiwix/download", sneaky)
	if code != http.StatusOK {
		t.Fatalf("roomy download: status %d: %s", code, body)
	}
	var added struct {
		ID string `json:"id"`
	}
	contentTestDecode(t, body, &added)
	if added.ID == "" {
		t.Fatal("download returned no job id")
	}
	jobs, err := q.List(kiwix.Kind)
	if err != nil || len(jobs) != 1 || jobs[0].ID != added.ID {
		t.Fatalf("jobs after download = %+v (err %v), want one job %s", jobs, err, added.ID)
	}

	// The list endpoint shows the enqueued download with the catalog's
	// entry: title, size and meta4 URL from the feed, never the page's.
	code, body = contentTestGet(t, ts, "/api/admin/content/kiwix")
	var state struct {
		Downloads []struct {
			ID         string         `json:"id"`
			State      string         `json:"state"`
			Downloaded int64          `json:"downloaded"`
			Entry      map[string]any `json:"entry"`
		} `json:"downloads"`
	}
	contentTestDecode(t, body, &state)
	if len(state.Downloads) != 1 || state.Downloads[0].State != "queued" || state.Downloads[0].Downloaded != 0 {
		t.Errorf("downloads = %+v", state.Downloads)
	}
	if title := state.Downloads[0].Entry["title"]; title != "スタック・オーバーフロー" {
		t.Errorf("download entry title = %v", title)
	}
	meta := state.Downloads[0].Entry["meta4_url"].(string)
	if !strings.HasPrefix(meta, "https://lb.download.kiwix.org/") || strings.Contains(meta, "evil.example") {
		t.Errorf("download entry meta4_url = %q, want the catalog's download URL", meta)
	}
	if size := state.Downloads[0].Entry["size"].(float64); size != 236055552 {
		t.Errorf("download entry size = %v, want the catalog's 236055552", size)
	}
}

// nameFilteredFeed is the one-entry feed the live catalog's exact name
// filter (the k:name parameter) returns for php.net_en_all.
const nameFilteredFeed = `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <id>name-filtered</id>
  <totalResults>1</totalResults>
  <startIndex>1</startIndex>
  <itemsPerPage>20</itemsPerPage>
  <entry>
    <id>urn:uuid:600c47c6-a792-0f78-8fcc-9c05cf1c556b</id>
    <title>PHP Manual</title>
    <updated>2024-08-13T00:00:00Z</updated>
    <summary>PHP Manual</summary>
    <language>eng</language>
    <name>php.net_en_all</name>
    <flavour></flavour>
    <category>other</category>
    <tags>_category:other;_ftindex:yes;_pictures:yes;_videos:yes;_details:yes</tags>
    <articleCount>11604</articleCount>
    <mediaCount>76</mediaCount>
    <link type="text/html" href="https://browse.library.kiwix.org/content/php.net_en_all_2024-08" />
    <author><name>-</name></author>
    <publisher><name>openZIM</name></publisher>
    <dc:issued>2024-08-13T00:00:00Z</dc:issued>
    <link rel="http://opds-spec.org/acquisition/open-access" type="application/x-zim" href="https://lb.download.kiwix.org/zim/zimit/php.net_en_all_2024-08.zim.meta4" length="32847872" />
  </entry>
</feed>`

const emptyFeed = `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <id>empty</id>
  <totalResults>0</totalResults>
  <startIndex>1</startIndex>
  <itemsPerPage>20</itemsPerPage>
</feed>`

// TestContentKiwixDownloadUsesNameLookup is the regression test for the
// fresh-install "That library can't be downloaded" failure: the old lookup
// searched the feed with q=<zim name>, but the live catalog's q matches
// titles only and never the internal ZIM name, so every download 400'd. The
// download path must use the catalog's exact name filter (the k:name
// parameter), which the fake here honors: a name-only request enqueues the
// catalog's own entry instead of failing, while a q= search for that same
// name (what the old code sent) finds nothing.
func TestContentKiwixDownloadUsesNameLookup(t *testing.T) {
	feed, err := os.ReadFile(filepath.Join("..", "kiwix", "testdata", "entries_eng_20.xml"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/catalog/v2/entries" {
			http.NotFound(w, r)
			return
		}
		switch {
		case r.URL.Query().Get("name") == "php.net_en_all":
			io.WriteString(w, nameFilteredFeed)
		case r.URL.Query().Get("name") != "":
			io.WriteString(w, emptyFeed)
		case r.URL.Query().Get("q") != "":
			io.WriteString(w, emptyFeed) // the live q= search misses ZIM names
		default:
			w.Write(feed)
		}
	}))
	defer srv.Close()

	s := contentTestStore(t)
	q, err := jobs.New(s)
	if err != nil {
		t.Fatal(err)
	}
	km, err := kiwix.NewManager(s, q, disk.NewReservations(q), filepath.Join(t.TempDir(), "libraries"))
	if err != nil {
		t.Fatal(err)
	}
	catalog := kiwix.NewCatalog(srv.URL, kiwix.AllowLoopback(srv.Client()))
	oldFree := contentFreeSpace
	contentFreeSpace = func(string) (uint64, error) { return 3 << 30, nil } // 3 GiB: 32 MB entry plus the 2 GiB reservation floor
	t.Cleanup(func() { contentFreeSpace = oldFree })

	ts := contentTestServer(t, Deps{Catalog: catalog, Kiwix: km, MediaDir: t.TempDir(), DataDir: t.TempDir()})

	// The page sends the name+flavour pair without an id; the lookup must
	// use the exact name filter or this request fails.
	code, body := contentTestPost(t, ts, "/api/admin/content/kiwix/download", map[string]any{
		"name": "php.net_en_all", "flavour": "",
	})
	if code != http.StatusOK {
		t.Fatalf("download by name: status %d: %s", code, body)
	}
	code, body = contentTestGet(t, ts, "/api/admin/content/kiwix")
	if code != http.StatusOK {
		t.Fatalf("state: status %d: %s", code, body)
	}
	var st struct {
		Downloads []struct {
			ID    string         `json:"id"`
			State string         `json:"state"`
			Entry map[string]any `json:"entry"`
		} `json:"downloads"`
	}
	contentTestDecode(t, body, &st)
	if len(st.Downloads) != 1 || st.Downloads[0].State != "queued" {
		t.Fatalf("downloads = %+v, want one queued job", st.Downloads)
	}
	if title := st.Downloads[0].Entry["title"]; title != "PHP Manual" {
		t.Errorf("download entry title = %v, want PHP Manual (the catalog's entry)", title)
	}
	if size := st.Downloads[0].Entry["size"].(float64); size != 32847872 {
		t.Errorf("download entry size = %v, want the catalog's 32847872", size)
	}
}

// TestContentPageOSPathExample checks the sources form's placeholder example
// matches the OS this copy runs on: the shell is served with the runtime.GOOS
// path (a Mac parent must never see a Linux example), and the placeholder
// token is always replaced.
func TestContentPageOSPathExample(t *testing.T) {
	ts := contentTestServer(t, Deps{DataDir: t.TempDir()})
	code, body := contentTestGet(t, ts, "/admin/content")
	if code != http.StatusOK {
		t.Fatalf("GET /admin/content = %d, want 200", code)
	}
	page := string(body)
	if strings.Contains(page, "__KS_OS_PATH__") {
		t.Error("the page still carries the un-replaced placeholder token")
	}
	if !strings.Contains(page, "placeholder=\"Path to the folder, e.g. "+exampleMediaPath()+"\"") {
		t.Errorf("page placeholder = %q area, want the %s example", page[strings.Index(page, "srcpath"):strings.Index(page, "srcpath")+140], runtime.GOOS)
	}
}

// TestContentServiceDown503: a missing service answers 503 with plain words
// instead of panicking.
func TestContentServiceDown503(t *testing.T) {
	ts := contentTestServer(t, Deps{DataDir: t.TempDir()})
	for _, path := range []string{
		"/api/admin/content/youtube",
		"/api/admin/content/sources",
		"/api/admin/content/ingest",
		"/api/admin/content/kiwix",
		"/api/admin/content/kiwix/catalog",
		"/api/admin/content/kiwix/folders",
	} {
		code, body := contentTestGet(t, ts, path)
		if code != http.StatusServiceUnavailable {
			t.Errorf("GET %s = %d, want 503", path, code)
			continue
		}
		if msg := contentTestError(t, body); !strings.Contains(msg, "not running") {
			t.Errorf("GET %s error = %q, want 'not running'", path, msg)
		}
	}
}

// TestContentFixes drives the "Fixing videos" card endpoint: pending
// counts per issue, the failed count, the newest failed episodes up to
// the list cap and the "and N more" remainder, all from real library
// rows.
func TestContentFixes(t *testing.T) {
	s := contentTestStore(t)
	lib, err := library.New(s)
	if err != nil {
		t.Fatalf("library.New: %v", err)
	}
	ts := contentTestServer(t, Deps{Library: lib, DataDir: t.TempDir()})
	ctx := context.Background()
	pub := func(id, show, title string, issue, fix string) {
		t.Helper()
		ep := core.Episode{
			ID: core.EpisodeID(id), ShowID: "s-" + id, Title: title,
			File: "/out/" + id + ".mp4", DurationSec: 100,
			Source: core.Source{Kind: core.SourceFolder, Path: show + "/" + id + ".mp4", Title: title, Show: show},
			Issue:  issue, Fix: fix,
		}
		if err := lib.Publish(ctx, ep); err != nil {
			t.Fatalf("Publish %s: %v", id, err)
		}
	}
	pub("ps", "Sound Show", "Silent One", core.IssueSound, core.FixPending)
	pub("pp", "Picture Show", "Dark One", core.IssuePicture, core.FixPending)
	pub("pf", "Format Show", "MKV One", core.IssueFormat, core.FixPending)
	for i := range library.FixingListCap + 2 {
		pub(fmt.Sprintf("f%02d", i), "Failed Show", fmt.Sprintf("Episode %02d", i), core.IssueSound, core.FixFailed)
	}

	for _, path := range []string{"/api/admin/content/fixes"} {
		code, body := contentTestGet(t, ts, path)
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d, body %s", path, code, body)
		}
		var out struct {
			Pending        map[string]int `json:"pending"`
			Failed         int            `json:"failed"`
			FailedEpisodes []struct {
				Show  string `json:"show"`
				Title string `json:"title"`
				Issue string `json:"issue"`
			} `json:"failedEpisodes"`
			FailedMore int `json:"failedMore"`
		}
		contentTestDecode(t, body, &out)
		if out.Pending["sound"] != 1 || out.Pending["picture"] != 1 || out.Pending["format"] != 1 {
			t.Errorf("pending = %v, want sound 1, picture 1, format 1", out.Pending)
		}
		if out.Failed != library.FixingListCap+2 {
			t.Errorf("failed = %d, want %d", out.Failed, library.FixingListCap+2)
		}
		if len(out.FailedEpisodes) != library.FixingListCap {
			t.Errorf("failedEpisodes = %d, want the cap %d", len(out.FailedEpisodes), library.FixingListCap)
		}
		if out.FailedMore != 2 {
			t.Errorf("failedMore = %d, want 2", out.FailedMore)
		}
		newest := out.FailedEpisodes[0]
		if newest.Show != "Failed Show" || newest.Title != "Episode 11" || newest.Issue != core.IssueSound {
			t.Errorf("newest failed = %+v, want Failed Show / Episode 11 / sound", newest)
		}
	}
}

// TestContentIngestRetry: a failed conversion had no way back. The list now
// flags failed conversions that can be retried, explains the ones that
// cannot, hides failures a newer job replaced and finished jobs older than a
// day; the retry endpoint requeues the same file and source as a new job,
// refuses a job that is not the newest failure for its file, and sits
// behind the parent PIN.
func TestContentIngestRetry(t *testing.T) {
	s := contentTestStore(t)
	q, err := jobs.New(s)
	if err != nil {
		t.Fatal(err)
	}
	lib, err := library.New(s)
	if err != nil {
		t.Fatal(err)
	}
	ingQ := ingest.NewQueue(q)
	ts := contentTestServer(t, Deps{Library: lib, Ingest: ingQ, DataDir: t.TempDir()})
	ctx := context.Background()
	dir := t.TempDir()

	enqueue := func(name string, kind core.SourceKind, create bool) string {
		t.Helper()
		file := filepath.Join(dir, name)
		if create {
			if err := os.WriteFile(file, []byte("v"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		path := filepath.ToSlash(file)
		if kind == core.SourceYouTube {
			path = "https://www.youtube.com/watch?v=" + strings.Repeat(name[:1], 11)
		}
		id, err := ingQ.Enqueue(ctx, file, core.Source{Kind: kind, Path: path, Title: name, Show: "Show"})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	// A conversion that finished two days ago drops off the list.
	old := enqueue("old.mp4", core.SourceFolder, true)
	if _, err := q.Claim(ingest.JobKind); err != nil {
		t.Fatal(err)
	}
	if err := q.Done(old); err != nil {
		t.Fatal(err)
	}
	broken := enqueue("broken.mp4", core.SourceFolder, true)
	gone := enqueue("gone.mp4", core.SourceFolder, false)
	ytGone := enqueue("y.mp4", core.SourceYouTube, false)
	shelved := enqueue("shelved.mp4", core.SourceFolder, true)
	for _, id := range []string{broken, gone, ytGone, shelved} {
		if err := q.Fail(id, errors.New("This file is damaged or incomplete and can't be played.")); err != nil {
			t.Fatal(err)
		}
	}
	// shelved's first copy is on the shelf (a failed improvement).
	if err := lib.Publish(ctx, core.Episode{
		ID: "ep-shelved", ShowID: "s-1", Title: "shelved", File: "/out/x.mp4", DurationSec: 10,
		Source: core.Source{Kind: core.SourceFolder, Path: filepath.ToSlash(filepath.Join(dir, "shelved.mp4")), Title: "shelved", Show: "Show"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE jobs SET updated = ? WHERE id = ?`,
		time.Now().Add(-48*time.Hour).UTC().Format(time.RFC3339Nano), old); err != nil {
		t.Fatal(err)
	}

	type row struct {
		ID    string `json:"id"`
		State string `json:"state"`
		Retry bool   `json:"retry"`
		Hint  string `json:"hint"`
	}
	listed := func() map[string]row {
		t.Helper()
		code, body := contentTestGet(t, ts, "/api/admin/content/ingest")
		if code != http.StatusOK {
			t.Fatalf("list: status %d: %s", code, body)
		}
		var l struct {
			Jobs []row `json:"jobs"`
		}
		contentTestDecode(t, body, &l)
		out := map[string]row{}
		for _, j := range l.Jobs {
			out[j.ID] = j
		}
		return out
	}
	got := listed()
	if _, ok := got[old]; ok {
		t.Errorf("a conversion done two days ago is still listed")
	}
	if r := got[broken]; !r.Retry || r.Hint != "" {
		t.Errorf("failed job with its file present = %+v, want retry", r)
	}
	if r := got[gone]; r.Retry || !strings.Contains(r.Hint, "not in its folder") {
		t.Errorf("failed job with its file gone = %+v, want the put-it-back hint", r)
	}
	if r := got[ytGone]; r.Retry || !strings.Contains(r.Hint, "YouTube link") {
		t.Errorf("failed YouTube job with its file gone = %+v, want the add-again hint", r)
	}
	if r := got[shelved]; r.Retry || !strings.Contains(r.Hint, "on the shelf") {
		t.Errorf("failed job of a video on the shelf = %+v, want no retry", r)
	}

	// The retry gate is the parent PIN.
	if resp, _ := visitorReq(t, http.MethodPost, ts.URL+"/api/admin/content/ingest/retry", map[string]any{"id": broken}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("retry without the PIN: status %d, want 401", resp.StatusCode)
	}

	code, body := contentTestPost(t, ts, "/api/admin/content/ingest/retry", map[string]any{"id": broken})
	if code != http.StatusOK {
		t.Fatalf("retry: status %d: %s", code, body)
	}
	var res struct {
		ID string `json:"id"`
	}
	contentTestDecode(t, body, &res)
	if res.ID == "" || res.ID == broken {
		t.Fatalf("retry id = %q, want a new job", res.ID)
	}
	got = listed()
	if _, ok := got[broken]; ok {
		t.Error("the replaced failure is still listed")
	}
	if r := got[res.ID]; r.State != "queued" {
		t.Errorf("new job = %+v, want queued", r)
	}
	j, err := q.Get(res.ID)
	if err != nil {
		t.Fatal(err)
	}
	var pl struct {
		File   string      `json:"file"`
		Source core.Source `json:"source"`
	}
	if err := j.DecodePayload(&pl); err != nil {
		t.Fatal(err)
	}
	if pl.File != filepath.Join(dir, "broken.mp4") || pl.Source.Path != filepath.ToSlash(filepath.Join(dir, "broken.mp4")) || pl.Source.Kind != core.SourceFolder {
		t.Errorf("new job payload = %+v, want the same file and source", pl)
	}

	for _, c := range []struct{ name, id, want string }{
		{"the replaced failure", broken, ""},
		{"a queued job", res.ID, ""},
		{"a job with its file gone", gone, "not in its folder"},
		{"a video on the shelf", shelved, "on the shelf"},
		{"an unknown job", "nope", ""},
	} {
		code, body := contentTestPost(t, ts, "/api/admin/content/ingest/retry", map[string]any{"id": c.id})
		if code != http.StatusBadRequest || !strings.Contains(contentTestError(t, body), c.want) {
			t.Errorf("retry %s: status %d %s, want 400 %q", c.name, code, body, c.want)
		}
	}
}

// TestContentKiwixEditionsAndMissing: two editions of one library looked
// alike, the catalog offered Download for an edition already installed,
// finished downloads stayed forever, and a library whose file went missing
// looked fine. The state now names each edition and flags a missing file,
// lists a finished download only while its library is installed, and the
// catalog marks installed editions.
func TestContentKiwixEditionsAndMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/catalog/v2/entries" {
			http.ServeFile(w, r, filepath.Join("..", "kiwix", "testdata", "entries_eng_20.xml"))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	s := contentTestStore(t)
	q, err := jobs.New(s)
	if err != nil {
		t.Fatal(err)
	}
	libDir := filepath.Join(t.TempDir(), "libraries")
	km, err := kiwix.NewManager(s, q, disk.NewReservations(q), libDir)
	if err != nil {
		t.Fatal(err)
	}
	catalog := kiwix.NewCatalog(srv.URL, kiwix.AllowLoopback(srv.Client()))
	ts := contentTestServer(t, Deps{Catalog: catalog, Kiwix: km, DataDir: t.TempDir()})

	code, body := contentTestGet(t, ts, "/api/admin/content/kiwix/catalog?lang=eng&count=20")
	if code != http.StatusOK {
		t.Fatalf("catalog: status %d", code)
	}
	var cat struct {
		Entries []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Flavour   string `json:"flavour"`
			Installed bool   `json:"installed"`
		} `json:"entries"`
	}
	contentTestDecode(t, body, &cat)
	first := cat.Entries[0]
	for _, e := range cat.Entries {
		if e.Installed {
			t.Fatalf("entry %s marked installed before anything is installed", e.Name)
		}
	}

	// Two editions of one library: the first catalog entry's own edition
	// (file present) and a text-only one whose file is gone.
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	present := first.Name + "_" + first.Flavour + "_2026-09.zim"
	if err := os.WriteFile(filepath.Join(libDir, present), []byte("zim"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, row := range [][]any{
		{"other-id", first.Name, "Same Library", "eng", present, 3},
		{"gone-id", first.Name, "Same Library", "eng", first.Name + "_nopic_2026-09.zim", 2},
	} {
		if _, err := s.DB().Exec(`INSERT INTO installed (id, name, title, language, file, size, installed_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			append(row, time.Now().UTC().Format(time.RFC3339Nano))...); err != nil {
			t.Fatal(err)
		}
	}
	// A finished download of an edition that is no longer installed.
	stale, err := q.Enqueue(kiwix.Kind, map[string]any{"entry": kiwix.Entry{ID: "removed-id", Name: "removed_lib", Flavour: "maxi", Title: "Removed"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Claim(kiwix.Kind); err != nil {
		t.Fatal(err)
	}
	if err := q.Done(stale); err != nil {
		t.Fatal(err)
	}

	code, body = contentTestGet(t, ts, "/api/admin/content/kiwix")
	if code != http.StatusOK {
		t.Fatalf("state: status %d: %s", code, body)
	}
	var state struct {
		Installed []struct {
			ID      string `json:"id"`
			Flavour string `json:"flavour"`
			Missing bool   `json:"missing"`
		} `json:"installed"`
		Downloads []struct {
			ID string `json:"id"`
		} `json:"downloads"`
	}
	contentTestDecode(t, body, &state)
	byID := map[string]struct {
		flavour string
		missing bool
	}{}
	for _, in := range state.Installed {
		byID[in.ID] = struct {
			flavour string
			missing bool
		}{in.Flavour, in.Missing}
	}
	if got := byID["other-id"]; got.flavour != first.Flavour || got.missing {
		t.Errorf("present edition = %+v, want flavour %q and not missing", got, first.Flavour)
	}
	if got := byID["gone-id"]; got.flavour != "nopic" || !got.missing {
		t.Errorf("edition whose file is gone = %+v, want nopic and missing", got)
	}
	if len(state.Downloads) != 0 {
		t.Errorf("downloads = %+v, want the finished download of a removed library left out", state.Downloads)
	}

	// The catalog now marks the installed edition (same name and edition,
	// another id) and only that one.
	_, body = contentTestGet(t, ts, "/api/admin/content/kiwix/catalog?lang=eng&count=20")
	contentTestDecode(t, body, &cat)
	for _, e := range cat.Entries {
		want := e.Name == first.Name && (e.Flavour == first.Flavour || e.Flavour == "nopic")
		if e.Installed != want {
			t.Errorf("entry %s/%s installed = %v, want %v", e.Name, e.Flavour, e.Installed, want)
		}
	}
}

// TestContentSourcesMissingOriginals: kids now see only "This one can't
// play right now" when an in-place video's original is gone, so the folder
// list tells the parent, per folder, how many videos from it can't be
// found: all of them when the folder itself is gone (a NAS that is off),
// the ones whose files moved when it is there. Converted copies never
// count.
func TestContentSourcesMissingOriginals(t *testing.T) {
	s := contentTestStore(t)
	lib, err := library.New(s)
	if err != nil {
		t.Fatal(err)
	}
	ts := contentTestServer(t, Deps{Library: lib, DataDir: t.TempDir()})
	ctx := context.Background()
	here, gone := t.TempDir(), filepath.Join(t.TempDir(), "nas")
	n := 0
	pub := func(root, name string, owned, create bool) {
		t.Helper()
		file := filepath.Join(root, "Show", name)
		if create {
			if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte("v"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		n++
		kind := core.SourceFolder
		if root == gone {
			kind = core.SourceNAS
		}
		play := file
		if owned {
			play = "/media/out/" + name
		}
		if err := lib.Publish(ctx, core.Episode{
			ID: core.EpisodeID(fmt.Sprintf("ep-%d", n)), ShowID: "show", Title: name, File: play, DurationSec: 10, Owned: owned,
			Source: core.Source{Kind: kind, Path: filepath.ToSlash(file), Title: name, Show: "Show"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	pub(here, "a.mp4", false, true)
	pub(here, "b.mp4", false, false) // moved away
	pub(here, "c.mp4", true, false)  // converted copy: its original does not matter
	pub(gone, "d.mp4", false, false)
	pub(gone, "e.mp4", false, false)
	if err := lib.AddSource(ctx, here, core.SourceFolder, nil); err != nil {
		t.Fatal(err)
	}
	if err := lib.AddSource(ctx, gone, core.SourceNAS, nil); err != nil {
		t.Fatal(err)
	}

	code, body := contentTestGet(t, ts, "/api/admin/content/sources")
	if code != http.StatusOK {
		t.Fatalf("sources: status %d: %s", code, body)
	}
	var list struct {
		Sources []struct {
			Path    string `json:"path"`
			Missing int    `json:"missing"`
			Problem string `json:"problem"`
		} `json:"sources"`
	}
	contentTestDecode(t, body, &list)
	got := map[string][2]string{}
	for _, src := range list.Sources {
		got[src.Path] = [2]string{strconv.Itoa(src.Missing), src.Problem}
	}
	if g := got[here]; g[0] != "1" || !strings.HasPrefix(g[1], "1 video from this folder can't be found") {
		t.Errorf("folder with a moved file = %v, want 1 missing with the can't-be-found warning", g)
	}
	if g := got[gone]; g[0] != "2" || !strings.Contains(g[1], "2 videos from it can't play") || !strings.Contains(g[1], "NAS") {
		t.Errorf("NAS share that is gone = %v, want 2 missing with the NAS warning", g)
	}
}

// contentKiwixManager is a real kiwix.Manager over the test store, with its
// library folder under a temp dir; it returns the library folder too.
func contentKiwixManager(t *testing.T) (*kiwix.Manager, string) {
	t.Helper()
	s := contentTestStore(t)
	q, err := jobs.New(s)
	if err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(t.TempDir(), "libraries")
	km, err := kiwix.NewManager(s, q, disk.NewReservations(q), lib)
	if err != nil {
		t.Fatal(err)
	}
	return km, lib
}

// contentPutZIM writes a small ZIM-like file at path, making its folders.
func contentPutZIM(t *testing.T, path, uuid string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, testZIM(uuid), 0o644); err != nil {
		t.Fatal(err)
	}
}

type contentTestFolder struct {
	Path      string `json:"path"`
	Libraries int    `json:"libraries"`
	Size      int64  `json:"size"`
	Reachable bool   `json:"reachable"`
}

func contentTestFolders(t *testing.T, ts *httptest.Server) []contentTestFolder {
	t.Helper()
	code, body := contentTestGet(t, ts, "/api/admin/content/kiwix/folders")
	if code != http.StatusOK {
		t.Fatalf("GET folders = %d: %s", code, body)
	}
	var out struct {
		Folders []contentTestFolder `json:"folders"`
	}
	contentTestDecode(t, body, &out)
	return out.Folders
}

type contentTestInstalled struct {
	ID      string `json:"id"`
	File    string `json:"file"`
	Folder  string `json:"folder"`
	Missing bool   `json:"missing"`
}

func contentTestInstalledList(t *testing.T, ts *httptest.Server) []contentTestInstalled {
	t.Helper()
	code, body := contentTestGet(t, ts, "/api/admin/content/kiwix")
	if code != http.StatusOK {
		t.Fatalf("GET kiwix state = %d: %s", code, body)
	}
	var out struct {
		Installed []contentTestInstalled `json:"installed"`
	}
	contentTestDecode(t, body, &out)
	return out.Installed
}

// TestContentKiwixFolders drives the library folder endpoints the way the
// page does: a folder of .zim files joins the shelf where it is, the list
// counts its libraries and their size, a library from it cannot be removed
// on its own, "Look again" follows the folder, a folder out of reach keeps
// its libraries as missing, and stopping the folder leaves every file.
func TestContentKiwixFolders(t *testing.T) {
	km, _ := contentKiwixManager(t)
	ts := contentTestServer(t, Deps{Kiwix: km, DataDir: t.TempDir()})
	parent := t.TempDir()
	dir := filepath.Join(parent, "Kiwix files")
	wiki := filepath.Join(dir, "wikipedia_en_100_maxi_2026-09.zim")
	mine := filepath.Join(dir, "more", "My Wiki (2026).zim")
	contentPutZIM(t, wiki, "")
	contentPutZIM(t, mine, "")
	size := int64(len(testZIM("")))

	// The path is tidied like a pasted video folder's.
	code, body := contentTestPost(t, ts, "/api/admin/content/kiwix/folders", map[string]any{"path": ` "` + dir + `" `})
	if code != http.StatusOK {
		t.Fatalf("add folder = %d: %s", code, body)
	}
	var added struct {
		OK        bool   `json:"ok"`
		Path      string `json:"path"`
		Libraries int    `json:"libraries"`
	}
	contentTestDecode(t, body, &added)
	if !added.OK || added.Path != dir || added.Libraries != 2 {
		t.Errorf("add folder = %+v, want ok, %s and 2 libraries", added, dir)
	}
	if got := contentTestFolders(t, ts); len(got) != 1 || got[0] != (contentTestFolder{Path: dir, Libraries: 2, Size: 2 * size, Reachable: true}) {
		t.Errorf("folders = %+v, want %s with 2 libraries of %d bytes, reachable", got, dir, 2*size)
	}
	installed := contentTestInstalledList(t, ts)
	byFile := map[string]contentTestInstalled{}
	for _, in := range installed {
		byFile[in.File] = in
	}
	for _, f := range []string{wiki, mine} {
		if in, ok := byFile[f]; !ok || in.Folder != dir || in.Missing {
			t.Errorf("installed %s = %+v (listed %v), want it from %s, not missing", f, in, ok, dir)
		}
	}

	// A library from the folder is never removed (and deleted) on its own.
	code, body = contentTestPost(t, ts, "/api/admin/content/kiwix/remove", map[string]any{"id": byFile[wiki].ID})
	if code != http.StatusBadRequest || contentTestError(t, body) == "" {
		t.Errorf("remove a folder library = %d %s, want 400 with plain words", code, body)
	}
	if _, err := os.Stat(wiki); err != nil {
		t.Errorf("the folder's file after the refused remove: %v", err)
	}

	// Look again: the file deleted by hand leaves the shelf.
	if err := os.Remove(wiki); err != nil {
		t.Fatal(err)
	}
	type rescan struct {
		Reachable bool `json:"reachable"`
		Added     int  `json:"added"`
		Dropped   int  `json:"dropped"`
		Libraries int  `json:"libraries"`
	}
	code, body = contentTestPost(t, ts, "/api/admin/content/kiwix/folders/rescan", map[string]any{"path": dir})
	var res rescan
	contentTestDecode(t, body, &res)
	if code != http.StatusOK || res != (rescan{Reachable: true, Dropped: 1, Libraries: 1}) {
		t.Errorf("look again = %d %+v, want 200, reachable, 1 dropped, 1 library", code, res)
	}
	if code, _ := contentTestPost(t, ts, "/api/admin/content/kiwix/folders/rescan", map[string]any{"path": parent}); code != http.StatusNotFound {
		t.Errorf("look again in a folder not on the list = %d, want 404", code)
	}

	// The drive is unplugged: the folder and its library stay, as missing.
	away := filepath.Join(parent, "away")
	if err := os.Rename(dir, away); err != nil {
		t.Fatal(err)
	}
	if got := contentTestFolders(t, ts); len(got) != 1 || got[0].Reachable || got[0].Libraries != 1 {
		t.Errorf("folders with the drive away = %+v, want it listed, not reachable, 1 library", got)
	}
	code, body = contentTestPost(t, ts, "/api/admin/content/kiwix/folders/rescan", map[string]any{"path": dir})
	res = rescan{}
	contentTestDecode(t, body, &res)
	if code != http.StatusOK || res != (rescan{Libraries: 1}) {
		t.Errorf("look again with the drive away = %d %+v, want 200, not reachable, the library kept", code, res)
	}
	if got := contentTestInstalledList(t, ts); len(got) != 1 || got[0].File != mine || !got[0].Missing || got[0].Folder != dir {
		t.Errorf("installed with the drive away = %+v, want %s from %s, missing", got, mine, dir)
	}
	if err := os.Rename(away, dir); err != nil {
		t.Fatal(err)
	}

	// Stop using the folder: its library leaves the shelf, its file stays.
	if code, body := contentTestPost(t, ts, "/api/admin/content/kiwix/folders/remove", map[string]any{"path": dir}); code != http.StatusOK {
		t.Fatalf("stop using the folder = %d: %s", code, body)
	}
	if got := contentTestFolders(t, ts); len(got) != 0 {
		t.Errorf("folders after stopping = %+v, want none", got)
	}
	if got := contentTestInstalledList(t, ts); len(got) != 0 {
		t.Errorf("installed after stopping the folder = %+v, want none", got)
	}
	if _, err := os.Stat(mine); err != nil {
		t.Errorf("the folder's file after stopping the folder: %v", err)
	}
	if code, _ := contentTestPost(t, ts, "/api/admin/content/kiwix/folders/remove", map[string]any{"path": dir}); code != http.StatusNotFound {
		t.Errorf("stop using a folder not on the list = %d, want 404", code)
	}
}

// TestContentKiwixFolderAddRefusals: a folder that cannot be used is
// refused with a plain message and nothing is added: no path, a relative
// path, a folder that is not there, a file, kids-shelf's own library
// folder, a folder already on the list, and one inside or around it.
func TestContentKiwixFolderAddRefusals(t *testing.T) {
	km, lib := contentKiwixManager(t)
	ts := contentTestServer(t, Deps{Kiwix: km, DataDir: t.TempDir()})
	base := t.TempDir()
	dir := filepath.Join(base, "kiwix")
	contentPutZIM(t, filepath.Join(dir, "inner", "a.zim"), "")
	file := filepath.Join(base, "file.zim")
	contentPutZIM(t, file, "")

	refuse := func(name, path string) {
		t.Helper()
		code, body := contentTestPost(t, ts, "/api/admin/content/kiwix/folders", map[string]any{"path": path})
		if code != http.StatusBadRequest || contentTestError(t, body) == "" {
			t.Errorf("add %s (%q) = %d %s, want 400 with plain words", name, path, code, body)
		}
	}
	refuse("no path", "   ")
	refuse("a relative path", filepath.Join("relative", "kiwix"))
	refuse("a folder that is not there", filepath.Join(base, "nope"))
	refuse("a file", file)
	refuse("the library folder", lib)
	if code, body := contentTestPost(t, ts, "/api/admin/content/kiwix/folders", map[string]any{"path": dir}); code != http.StatusOK {
		t.Fatalf("add %s = %d: %s", dir, code, body)
	}
	refuse("the same folder again", dir)
	refuse("a folder inside it", filepath.Join(dir, "inner"))
	refuse("the folder around it", base)
	if got := contentTestFolders(t, ts); len(got) != 1 || got[0].Path != dir {
		t.Errorf("folders = %+v, want only %s", got, dir)
	}
}

// TestContentKiwixFoldersNeedPIN: every library folder route is behind the
// parent PIN, and a refused call changes nothing.
func TestContentKiwixFoldersNeedPIN(t *testing.T) {
	km, _ := contentKiwixManager(t)
	ts := contentTestServer(t, Deps{Kiwix: km, DataDir: t.TempDir()})
	dir := t.TempDir()
	contentPutZIM(t, filepath.Join(dir, "a.zim"), "")
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/admin/content/kiwix/folders"},
		{http.MethodPost, "/api/admin/content/kiwix/folders"},
		{http.MethodPost, "/api/admin/content/kiwix/folders/rescan"},
		{http.MethodPost, "/api/admin/content/kiwix/folders/remove"},
	} {
		var body any
		if c.method == http.MethodPost {
			body = map[string]any{"path": dir}
		}
		if resp, _ := visitorReq(t, c.method, ts.URL+c.path, body); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without the PIN = %d, want 401", c.method, c.path, resp.StatusCode)
		}
	}
	if folders, err := km.Folders(); err != nil || len(folders) != 0 {
		t.Errorf("folders after the refused calls = %+v, %v, want none", folders, err)
	}
}
