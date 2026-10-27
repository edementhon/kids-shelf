// Handler tests for the /kiwix/ proxy (on the separate libraries origin),
// the /api/libraries list and the /libraries page (both on the app origin).
// The proxy and the icon lookup run against httptest backends; the
// installed table is read through a real kiwix.Manager over a temp store,
// seeded exactly like the download pipeline would seed it.
package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edementhon/kids-shelf/internal/disk"
	"github.com/edementhon/kids-shelf/internal/jobs"
	"github.com/edementhon/kids-shelf/internal/kiwix"
	"github.com/edementhon/kids-shelf/internal/store"
)

// stubKiwixBackend is a KiwixBackend whose Backend() returns a fixed URL
// (or "" for "not running").
type stubKiwixBackend struct{ base string }

func (b stubKiwixBackend) Backend() string { return b.base }

func librariesTestServer(t *testing.T, deps Deps) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(NewApp("libraries-test", deps).Mux())
	t.Cleanup(ts.Close)
	return ts
}

// librariesOriginTestServer is the separate libraries origin: only /kiwix/
// is routed there, still behind the security middleware.
func librariesOriginTestServer(t *testing.T, deps Deps) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(NewApp("libraries-test", deps).LibrariesMux())
	t.Cleanup(ts.Close)
	return ts
}

// librariesKiwixManager builds a real kiwix.Manager on a temp store, ready
// for test rows to be inserted into the installed table.
func librariesKiwixManager(t *testing.T) (*kiwix.Manager, *store.Store) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "libraries.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	q, err := jobs.New(s)
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	km, err := kiwix.NewManager(s, q, disk.NewReservations(q), filepath.Join(t.TempDir(), "libraries"))
	if err != nil {
		t.Fatalf("kiwix.NewManager: %v", err)
	}
	return km, s
}

// seedInstalledRow inserts one installed row, the same shape install()
// writes (see internal/kiwix/library.go).
func seedInstalledRow(t *testing.T, s *store.Store, id, name, title, language, file string) {
	t.Helper()
	if _, err := s.DB().Exec(`INSERT INTO installed (id, name, title, language, file, size, installed_at)
		VALUES (?, ?, ?, ?, ?, 1, '2026-10-26T12:00:00Z')`, id, name, title, language, file); err != nil {
		t.Fatalf("seed installed row: %v", err)
	}
}

func librariesTestGet(t *testing.T, ts *httptest.Server, path string) (int, []byte, http.Header) {
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
	return resp.StatusCode, b.Bytes(), resp.Header
}

func librariesTestError(t *testing.T, b []byte) string {
	t.Helper()
	var v struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decoding error body %q: %v", b, err)
	}
	return v.Error
}

func TestKiwixProxyPassesPathAndQuery(t *testing.T) {
	var gotPath, gotQuery string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("backend-ok"))
	}))
	defer backend.Close()

	ts := librariesOriginTestServer(t, Deps{KiwixServer: stubKiwixBackend{backend.URL}})
	code, body, _ := librariesTestGet(t, ts, "/kiwix/content/some_book/Some_Page?pattern=din&x=1")
	if code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", code, body)
	}
	if string(body) != "backend-ok" {
		t.Errorf("body = %q, want the backend's reply", body)
	}
	if gotPath != "/kiwix/content/some_book/Some_Page" {
		t.Errorf("backend path = %q, want the incoming path unchanged", gotPath)
	}
	if gotQuery != "pattern=din&x=1" {
		t.Errorf("backend query = %q, want the incoming query unchanged", gotQuery)
	}
}

func TestKiwixProxyStripsForwardedHeaders(t *testing.T) {
	var fwdFor, fwdHost, fwdProto, seenHost string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fwdFor = r.Header.Get("X-Forwarded-For")
		fwdHost = r.Header.Get("X-Forwarded-Host")
		fwdProto = r.Header.Get("X-Forwarded-Proto")
		seenHost = r.Host
	}))
	defer backend.Close()

	ts := librariesOriginTestServer(t, Deps{KiwixServer: stubKiwixBackend{backend.URL}})
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/kiwix/content/book/A", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("X-Forwarded-Host", "evil.example")
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxied GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if fwdFor != "" || fwdHost != "" || fwdProto != "" {
		t.Errorf("backend saw forwarding headers: for=%q host=%q proto=%q, want all empty", fwdFor, fwdHost, fwdProto)
	}
	if got, want := seenHost, strings.TrimPrefix(backend.URL, "http://"); got != want {
		t.Errorf("backend Host = %q, want the backend's own %q (the client's Host was dropped)", got, want)
	}
}

func TestKiwixProxyBackendDown503(t *testing.T) {
	for _, name := range []string{"nil", "empty"} {
		t.Run(name, func(t *testing.T) {
			var deps Deps
			if name == "empty" {
				deps = Deps{KiwixServer: stubKiwixBackend{""}}
			}
			ts := librariesOriginTestServer(t, deps)
			code, body, _ := librariesTestGet(t, ts, "/kiwix/content/book/A")
			if code != http.StatusServiceUnavailable {
				t.Fatalf("status %d, want 503: %s", code, body)
			}
			if got := librariesTestError(t, body); got != msgLibrariesDown {
				t.Errorf("error = %q, want %q", got, msgLibrariesDown)
			}
		})
	}
}

func TestKiwixProxyBackendError502(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close() // connection refused from here on
	ts := librariesOriginTestServer(t, Deps{KiwixServer: stubKiwixBackend{dead.URL}})

	code, body, _ := librariesTestGet(t, ts, "/kiwix/content/book/A")
	if code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502: %s", code, body)
	}
	if got := librariesTestError(t, body); got != msgLibrariesUnreachable {
		t.Errorf("error = %q, want %q", got, msgLibrariesUnreachable)
	}
	for _, leak := range []string{"refused", "dial", "connection"} {
		if strings.Contains(strings.ToLower(string(body)), leak) {
			t.Errorf("body %q leaks Go error text (%q)", body, leak)
		}
	}
}

func TestKiwixProxyPost405(t *testing.T) {
	backend := httptest.NewServer(http.NotFoundHandler())
	defer backend.Close()
	ts := librariesOriginTestServer(t, Deps{KiwixServer: stubKiwixBackend{backend.URL}})

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/kiwix/content/book/A", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Origin", "http://"+req.Host) // same-site check needs it, like the app's own POSTs
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", resp.StatusCode)
	}
}

// TestKiwixOnlyOnTheLibrariesOrigin: the app mux must not serve /kiwix/
// (ZIM content never runs on the app origin) and the libraries mux must
// serve nothing but /kiwix/.
func TestKiwixOnlyOnTheLibrariesOrigin(t *testing.T) {
	backend := httptest.NewServer(http.NotFoundHandler())
	defer backend.Close()
	deps := Deps{KiwixServer: stubKiwixBackend{backend.URL}}

	app := librariesTestServer(t, deps)
	code, body, _ := librariesTestGet(t, app, "/kiwix/content/book/A")
	if code != http.StatusNotFound {
		t.Errorf("app mux GET /kiwix/ = %d, want 404 (ZIM content must not run on the app origin): %s", code, body)
	}

	origin := librariesOriginTestServer(t, deps)
	if code, _, _ := librariesTestGet(t, origin, "/api/libraries"); code != http.StatusNotFound {
		t.Errorf("libraries mux GET /api/libraries = %d, want 404 (only /kiwix/ lives there)", code)
	}
	if code, _, _ := librariesTestGet(t, origin, "/libraries"); code != http.StatusNotFound {
		t.Errorf("libraries mux GET /libraries = %d, want 404 (only /kiwix/ lives there)", code)
	}
}

// TestAPILibrariesAddsLibrariesURL: with a separate libraries origin
// configured, /api/libraries reports its full base URL built from the
// request's own host (which the kid page prefixes book URLs with).
func TestAPILibrariesAddsLibrariesURL(t *testing.T) {
	km, store := librariesKiwixManager(t)
	// Viewer URLs are filled only while the backend is up (the backend-down
	// contract is pinned by TestAPILibrariesBackendDownStillListsBooks), so
	// this test runs a live one; the icon feed is best-effort.
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join("..", "kiwix", "testdata", "entries_eng_20.xml"))
	}))
	defer feed.Close()
	ts := librariesTestServer(t, Deps{Kiwix: km, LibrariesPort: 8071, KiwixServer: stubKiwixBackend{feed.URL}})

	read := func() (string, []string) {
		t.Helper()
		resp, err := http.Get(ts.URL + "/api/libraries")
		if err != nil {
			t.Fatalf("GET /api/libraries: %v", err)
		}
		defer resp.Body.Close()
		var out struct {
			LibrariesURL string `json:"libraries_url"`
			Books        []struct {
				URL string `json:"url"`
			} `json:"books"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decoding /api/libraries: %v", err)
		}
		urls := make([]string, 0, len(out.Books))
		for _, b := range out.Books {
			urls = append(urls, b.URL)
		}
		return out.LibrariesURL, urls
	}

	baseURL, _ := read()
	host := strings.TrimPrefix(ts.URL, "http://")
	if got, want := baseURL, "http://"+strings.Split(host, ":")[0]+":8071"; got != want {
		t.Errorf("libraries_url = %q, want %q (the request host on the libraries port)", got, want)
	}

	// Book URLs stay relative to the libraries origin; the page prefixes
	// them with libraries_url.
	seedInstalledRow(t, store, "urn:uuid:book", "book_en", "Book", "eng", "book_en.zim")
	_, urls := read()
	if len(urls) != 1 || urls[0] != "/kiwix/viewer#book_en" {
		t.Errorf("books = %v, want one relative viewer URL", urls)
	}
}

// TestAPILibrariesURLBracketsIPv6Host: a request served on an IPv6 literal
// host reports a libraries base URL with the host bracketed
// (http://[::1]:8071) and parseable, exactly what the kid page's frontend
// feeds to new URL().
func TestAPILibrariesURLBracketsIPv6Host(t *testing.T) {
	km, _ := librariesKiwixManager(t)
	ts := librariesTestServer(t, Deps{Kiwix: km, LibrariesPort: 8071})

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/libraries", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = "[::1]:8070"
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /api/libraries: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		LibrariesURL string `json:"libraries_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding /api/libraries: %v", err)
	}
	want := "http://[::1]:8071"
	if out.LibrariesURL != want {
		t.Errorf("libraries_url = %q, want %q (IPv6 hosts must be bracketed)", out.LibrariesURL, want)
	}
	parsed, err := url.Parse(out.LibrariesURL)
	if err != nil || parsed.Host != "[::1]:8071" {
		t.Errorf("libraries_url %q does not parse to host [::1]:8071 (parsed %q, err %v)", out.LibrariesURL, parsed, err)
	}
}

func TestAPILibrariesBooksWithIcons(t *testing.T) {
	var feedCount string
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/kiwix/catalog/v2/entries" {
			t.Errorf("feed path = %q, want /kiwix/catalog/v2/entries", r.URL.Path)
		}
		feedCount = r.URL.Query().Get("count")
		http.ServeFile(w, r, filepath.Join("..", "kiwix", "testdata", "entries_eng_20.xml"))
	}))
	defer feed.Close()

	km, s := librariesKiwixManager(t)
	seedInstalledRow(t, s, "urn:uuid:ab7c8918-9940-f83d-9a14-7f64b4f55636",
		"ja.stackoverflow.com_mul_all", "スタック・オーバーフロー", "eng,jpn",
		"ja.stackoverflow.com_mul_all_2026-10.zim")
	seedInstalledRow(t, s, "urn:uuid:not-in-the-feed", "unknown_book_en", "Something Else", "eng",
		"unknown_book_en_2026-01.zim")

	ts := librariesTestServer(t, Deps{Kiwix: km, KiwixServer: stubKiwixBackend{feed.URL}})
	code, body, _ := librariesTestGet(t, ts, "/api/libraries")
	if code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", code, body)
	}
	var out struct {
		Available bool `json:"available"`
		Books     []struct {
			Name     string `json:"name"`
			Title    string `json:"title"`
			Language string `json:"language"`
			URL      string `json:"url"`
			Icon     string `json:"icon"`
		} `json:"books"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decoding %q: %v", body, err)
	}
	if !out.Available {
		t.Error("available = false, want true with the backend up")
	}
	if feedCount != "-1" {
		t.Errorf("feed count = %q, want -1 (all books in one feed)", feedCount)
	}
	if len(out.Books) != 2 {
		t.Fatalf("books = %d, want 2: %s", len(out.Books), body)
	}
	first := out.Books[0]
	if first.Name != "ja.stackoverflow.com_mul_all_2026-10" {
		t.Errorf("books[0].name = %q, want the file base name without .zim", first.Name)
	}
	if first.Title != "スタック・オーバーフロー" || first.Language != "eng,jpn" {
		t.Errorf("books[0] title/language = %q / %q, want the installed row's", first.Title, first.Language)
	}
	if first.URL != "/kiwix/viewer#ja.stackoverflow.com_mul_all_2026-10" {
		t.Errorf("books[0].url = %q, want the viewer URL", first.URL)
	}
	wantIcon := "/kiwix/catalog/v2/illustration/ab7c8918-9940-f83d-9a14-7f64b4f55636/?size=48"
	if first.Icon != wantIcon {
		t.Errorf("books[0].icon = %q, want %q (uuid from the OPDS feed)", first.Icon, wantIcon)
	}
	if second := out.Books[1]; second.Icon != "" || second.URL == "" {
		t.Errorf("unmatched book = %+v, want icon omitted and url filled", second)
	}
}

func TestAPILibrariesEmptyBookArray(t *testing.T) {
	km, _ := librariesKiwixManager(t)
	ts := librariesTestServer(t, Deps{Kiwix: km, KiwixServer: stubKiwixBackend{""}})
	code, body, _ := librariesTestGet(t, ts, "/api/libraries")
	if code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", code, body)
	}
	if !strings.Contains(string(body), `"books":[]`) {
		t.Errorf("body %q does not carry an empty JSON array for books", body)
	}
	var out struct {
		Available bool          `json:"available"`
		Books     []libraryBook `json:"books"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decoding %q: %v", body, err)
	}
	if out.Available {
		t.Error("available = true, want false with no backend")
	}
	if out.Books == nil || len(out.Books) != 0 {
		t.Errorf("books = %#v, want an empty (non-nil) array", out.Books)
	}
}

func TestAPILibrariesBackendDownStillListsBooks(t *testing.T) {
	for _, name := range []string{"nil", "empty"} {
		t.Run(name, func(t *testing.T) {
			km, s := librariesKiwixManager(t)
			seedInstalledRow(t, s, "urn:uuid:book-1", "wikipedia_en_100", "Wikipedia", "eng",
				"wikipedia_en_100_maxi_2026-08.zim")
			var deps Deps
			if name == "empty" {
				deps.KiwixServer = stubKiwixBackend{""}
			}
			deps.Kiwix = km
			ts := librariesTestServer(t, deps)

			code, body, _ := librariesTestGet(t, ts, "/api/libraries")
			if code != http.StatusOK {
				t.Fatalf("status %d, want 200: %s", code, body)
			}
			var out struct {
				Available bool `json:"available"`
				Books     []struct {
					Name string `json:"name"`
					URL  string `json:"url"`
					Icon string `json:"icon"`
				} `json:"books"`
			}
			if err := json.Unmarshal(body, &out); err != nil {
				t.Fatalf("decoding %q: %v", body, err)
			}
			if out.Available {
				t.Error("available = true, want false with the backend down")
			}
			if len(out.Books) != 1 {
				t.Fatalf("books = %d, want the installed book still listed: %s", len(out.Books), body)
			}
			if out.Books[0].Name != "wikipedia_en_100_maxi_2026-08" {
				t.Errorf("book name = %q, want the file base name without .zim", out.Books[0].Name)
			}
			if out.Books[0].URL != "" || out.Books[0].Icon != "" {
				t.Errorf("book url/icon = %q/%q, want empty while the backend is down", out.Books[0].URL, out.Books[0].Icon)
			}
		})
	}
}

func TestAPILibrariesServiceDown503(t *testing.T) {
	ts := librariesTestServer(t, Deps{Kiwix: nil})
	code, body, _ := librariesTestGet(t, ts, "/api/libraries")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503: %s", code, body)
	}
	if got := librariesTestError(t, body); got != msgLibrariesDown {
		t.Errorf("error = %q, want %q", got, msgLibrariesDown)
	}
}

func TestLibrariesPageServes(t *testing.T) {
	km, _ := librariesKiwixManager(t)
	ts := librariesTestServer(t, Deps{Kiwix: km})

	code, body, header := librariesTestGet(t, ts, "/libraries")
	if code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", code, body)
	}
	if ct := header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(string(body), "Libraries") {
		t.Errorf("page body does not mention Libraries")
	}

	// The page's static files ride the already-embedded kid static dir.
	for _, p := range []string{"/static/kid/libraries.js", "/static/kid/libraries.css"} {
		code, _, _ := librariesTestGet(t, ts, p)
		if code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", p, code)
		}
	}

	// The kid home still serves and is not displaced by the new page route.
	code, _, _ = librariesTestGet(t, ts, "/")
	if code != http.StatusOK {
		t.Errorf("GET / = %d, want 200", code)
	}
}
