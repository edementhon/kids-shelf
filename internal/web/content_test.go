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
	"strings"
	"testing"

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

func contentTestServer(t *testing.T, deps Deps) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(NewApp("content-test", deps).Mux())
	t.Cleanup(ts.Close)
	return ts
}

func contentTestGet(t *testing.T, ts *httptest.Server, path string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
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
	resp, err := http.DefaultClient.Do(req)
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

	// A failure shows its plain message; retry enqueues the same URL again.
	if err := q.Fail(added.ID, errors.New("The download stopped.")); err != nil {
		t.Fatalf("fail: %v", err)
	}
	code, body = contentTestGet(t, ts, "/api/admin/content/youtube")
	contentTestDecode(t, body, &list)
	j = list.Jobs[0]
	if j.State != "failed" || j.Message != "The download stopped." {
		t.Errorf("failed job = %+v", j)
	}
	code, body = contentTestPost(t, ts, "/api/admin/content/youtube/retry", map[string]any{"id": added.ID})
	if code != http.StatusOK {
		t.Fatalf("retry: status %d: %s", code, body)
	}
	contentTestDecode(t, body, &added)
	code, body = contentTestGet(t, ts, "/api/admin/content/youtube")
	contentTestDecode(t, body, &list)
	if len(list.Jobs) != 2 {
		t.Fatalf("after retry: %d jobs, want 2", len(list.Jobs))
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
	} {
		code, body := contentTestPost(t, ts, "/api/admin/content/youtube", map[string]any{"url": bad})
		if code != http.StatusBadRequest {
			t.Errorf("bad link %q: status %d, want 400 (%s)", bad, code, body)
		}
		if msg := contentTestError(t, body); msg != "That doesn't look like a YouTube link." {
			t.Errorf("bad link %q error = %q", bad, msg)
		}
	}
	_, body = contentTestGet(t, ts, "/api/admin/content/youtube")
	var list struct {
		Jobs []any `json:"jobs"`
	}
	contentTestDecode(t, body, &list)
	if len(list.Jobs) != 1 {
		t.Errorf("jobs after rejected links = %d, want only the youtu.be one", len(list.Jobs))
	}
}

// TestContentSourcesAddListRemove exercises source registration (folder and
// NAS kinds, an upsert replacing the kind), path validation, scanning and
// removal through the real library service.
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

	dir := t.TempDir()
	code, body := contentTestPost(t, ts, "/api/admin/content/sources", map[string]any{"path": dir, "kind": "folder"})
	if code != http.StatusOK {
		t.Fatalf("add folder: status %d: %s", code, body)
	}

	// Re-adding the same path as a NAS share replaces the registration.
	code, body = contentTestPost(t, ts, "/api/admin/content/sources", map[string]any{"path": dir, "kind": "nas"})
	if code != http.StatusOK {
		t.Fatalf("add nas: status %d: %s", code, body)
	}
	code, body = contentTestGet(t, ts, "/api/admin/content/sources")
	var list struct {
		Sources []struct {
			Path string `json:"path"`
			Kind string `json:"kind"`
		} `json:"sources"`
	}
	contentTestDecode(t, body, &list)
	if len(list.Sources) != 1 || list.Sources[0].Path != dir || list.Sources[0].Kind != "nas" {
		t.Fatalf("sources = %+v, want one NAS entry for %s", list.Sources, dir)
	}

	// Unknown kinds and paths that do not exist are refused with plain words.
	code, body = contentTestPost(t, ts, "/api/admin/content/sources", map[string]any{"path": dir, "kind": "usb"})
	if code != http.StatusBadRequest {
		t.Errorf("bad kind: status %d, want 400", code)
	}
	code, body = contentTestPost(t, ts, "/api/admin/content/sources", map[string]any{"path": filepath.Join(dir, "nope"), "kind": "folder"})
	if code != http.StatusBadRequest || !strings.Contains(contentTestError(t, body), "couldn't find") {
		t.Errorf("missing path: status %d error %q", code, contentTestError(t, body))
	}

	// Scanning an empty folder succeeds.
	code, body = contentTestPost(t, ts, "/api/admin/content/scan", nil)
	if code != http.StatusOK {
		t.Fatalf("scan: status %d: %s", code, body)
	}

	// Removal by path.
	code, body = contentTestPost(t, ts, "/api/admin/content/sources/remove", map[string]any{"path": dir})
	if code != http.StatusOK {
		t.Fatalf("remove: status %d: %s", code, body)
	}
	code, body = contentTestGet(t, ts, "/api/admin/content/sources")
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
