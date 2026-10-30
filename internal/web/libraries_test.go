// Handler tests for the /kiwix/ proxy (on the separate libraries origin),
// the /api/libraries list and the /libraries page (both on the app origin).
// The proxy and the book feed run against httptest backends; the
// installed table is read through a real kiwix.Manager over a temp store,
// seeded exactly like the download pipeline would seed it.
package web

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

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
// for test rows to be inserted into the installed table, and returns its
// library directory too (where seedInstalledRow puts the ZIM files).
func librariesKiwixManager(t *testing.T) (*kiwix.Manager, *store.Store, string) {
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
	lib := filepath.Join(t.TempDir(), "libraries")
	km, err := kiwix.NewManager(s, q, disk.NewReservations(q), lib)
	if err != nil {
		t.Fatalf("kiwix.NewManager: %v", err)
	}
	return km, s, lib
}

// seedInstalledRow inserts one installed row, the same shape install()
// writes (see internal/kiwix/library.go), and puts its file in lib. An
// empty lib leaves the file missing (a drive that is not plugged in). The
// file starts like a real ZIM whose header carries the UUID of a
// "urn:uuid:<uuid>" id, which is how the backend's feed finds the book.
func seedInstalledRow(t *testing.T, s *store.Store, lib, id, name, title, language, file string) {
	t.Helper()
	if _, err := s.DB().Exec(`INSERT INTO installed (id, name, title, language, file, size, installed_at)
		VALUES (?, ?, ?, ?, ?, 1, '2026-10-26T12:00:00Z')`, id, name, title, language, file); err != nil {
		t.Fatalf("seed installed row: %v", err)
	}
	if lib != "" {
		if err := os.WriteFile(filepath.Join(lib, file), testZIM(strings.TrimPrefix(id, "urn:uuid:")), 0o644); err != nil {
			t.Fatalf("seed installed file: %v", err)
		}
	}
}

// testZIM returns the start of a ZIM file whose header carries uuid (in
// the 8-4-4-4-12 hex form the backend's feed writes book ids in); a uuid
// that does not parse leaves the header's UUID zero, a book no feed lists.
func testZIM(uuid string) []byte {
	b := make([]byte, 80)
	binary.LittleEndian.PutUint32(b, 0x044D495A)
	binary.LittleEndian.PutUint16(b[4:], 6)
	binary.LittleEndian.PutUint16(b[6:], 3)
	if raw, err := hex.DecodeString(strings.ReplaceAll(uuid, "-", "")); err == nil && len(raw) == 16 {
		copy(b[8:24], raw)
	}
	return b
}

func librariesTestGet(t *testing.T, ts *httptest.Server, path string) (int, []byte, http.Header) {
	t.Helper()
	return librariesTestGetAccept(t, ts, path, "")
}

// browserAccept is the Accept header Chromium sends when it opens a page
// (the tab or a frame); no Sec-Fetch-* on plain http over the LAN.
const browserAccept = "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"

// librariesTestGetAccept is librariesTestGet with an Accept header and
// without following redirects, so a redirect itself can be checked.
func librariesTestGetAccept(t *testing.T, ts *httptest.Server, path, accept string) (int, []byte, http.Header) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
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
	km, store, lib := librariesKiwixManager(t)
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
	// them with libraries_url. The book is one the backend's feed lists.
	seedInstalledRow(t, store, lib, "urn:uuid:600c47c6-a792-0f78-8fcc-9c05cf1c556b", "php.net_en_all", "Book", "eng", "php.net_en_all_2024-08.zim")
	_, urls := read()
	if len(urls) != 1 || urls[0] != "/kiwix/viewer#php.net_en_all_2024-08" {
		t.Errorf("books = %v, want one relative viewer URL", urls)
	}
}

// TestAPILibrariesURLBracketsIPv6Host: a request served on an IPv6 literal
// host reports a libraries base URL with the host bracketed
// (http://[::1]:8071) and parseable, exactly what the kid page's frontend
// feeds to new URL().
func TestAPILibrariesURLBracketsIPv6Host(t *testing.T) {
	km, _, _ := librariesKiwixManager(t)
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

	km, s, lib := librariesKiwixManager(t)
	seedInstalledRow(t, s, lib, "urn:uuid:ab7c8918-9940-f83d-9a14-7f64b4f55636",
		"ja.stackoverflow.com_mul_all", "スタック・オーバーフロー", "eng,jpn",
		"ja.stackoverflow.com_mul_all_2026-10.zim")
	seedInstalledRow(t, s, lib, "urn:uuid:not-in-the-feed", "unknown_book_en", "Something Else", "eng",
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
	// A book the backend does not serve (its file's UUID is not in the
	// feed: kiwix-serve was not started with it yet, or could not open it)
	// gets neither: a link would open nothing.
	if second := out.Books[1]; second.Icon != "" || second.URL != "" {
		t.Errorf("book the backend does not serve = %+v, want no icon and no url", second)
	}
}

func TestAPILibrariesEmptyBookArray(t *testing.T) {
	km, _, _ := librariesKiwixManager(t)
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
			km, s, lib := librariesKiwixManager(t)
			seedInstalledRow(t, s, lib, "urn:uuid:book-1", "wikipedia_en_100", "Wikipedia", "eng",
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
	km, _, _ := librariesKiwixManager(t)
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

// hrefsIn returns every href and action value in an HTML body.
func hrefsIn(body []byte) []string {
	var out []string
	for _, m := range regexp.MustCompile(`(?i)(?:href|action)\s*=\s*"([^"]*)"`).FindAllSubmatch(body, -1) {
		out = append(out, string(m[1]))
	}
	return out
}

// TestKiwixExternalPageHasNoOutboundLink: the "outside link" page is the
// app's own, never kiwix-serve's (which shows the outside address as a
// live link): it links nowhere but the app, names no outside address, and
// offers one big "Back to the book" button.
func TestKiwixExternalPageHasNoOutboundLink(t *testing.T) {
	t.Setenv("KS_PUBLIC_PORT", "8070")
	var backendHits int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendHits++
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><body>External Link Detected <a href="https://example.com/x">https://example.com/x</a></body></html>`))
	}))
	defer backend.Close()
	for _, up := range []bool{true, false} {
		base := ""
		if up {
			base = backend.URL
		}
		ts := librariesOriginTestServer(t, Deps{KiwixServer: stubKiwixBackend{base}})
		code, body, header := librariesTestGetAccept(t, ts, "/kiwix/catch/external?source=https%3A%2F%2Fexample.com%2Fx", browserAccept)
		if code != http.StatusOK {
			t.Fatalf("backend up=%v: status %d, want 200: %s", up, code, body)
		}
		if ct := header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("backend up=%v: Content-Type = %q, want text/html", up, ct)
		}
		if strings.Contains(string(body), "example.com") {
			t.Errorf("backend up=%v: the page names the outside address:\n%s", up, body)
		}
		host := strings.Split(strings.TrimPrefix(ts.URL, "http://"), ":")[0]
		hrefs := hrefsIn(body)
		if len(hrefs) == 0 {
			t.Errorf("backend up=%v: the page has no way back", up)
		}
		for _, h := range hrefs {
			u, err := url.Parse(h)
			if err != nil || (u.Host != "" && u.Hostname() != host) || (u.Scheme != "" && u.Scheme != "http" && u.Scheme != "https") {
				t.Errorf("backend up=%v: link %q leaves the shelf", up, h)
			}
		}
		if !strings.Contains(string(body), "Back to the book") {
			t.Errorf("backend up=%v: no \"Back to the book\" button:\n%s", up, body)
		}
		if want := "http://" + host + ":8070/libraries"; !strings.Contains(string(body), `href="`+want+`"`) {
			t.Errorf("backend up=%v: the button's link must fall back to %s:\n%s", up, want, body)
		}
	}
	if backendHits != 0 {
		t.Errorf("kiwix-serve answered the outside-link page %d times, want never", backendHits)
	}
}

// TestKiwixDownPagesAreHTMLForBrowsers: when the libraries are down or a
// library does not answer, a browser opening a page gets a kid page with
// a big "Back to Libraries" button on the app origin, never raw JSON; an
// API caller still gets the JSON error.
func TestKiwixDownPagesAreHTMLForBrowsers(t *testing.T) {
	t.Setenv("KS_PUBLIC_PORT", "8070")
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	cases := []struct {
		name string
		deps Deps
		code int
		msg  string
	}{
		{"nil", Deps{}, http.StatusServiceUnavailable, msgLibrariesDown},
		{"empty", Deps{KiwixServer: stubKiwixBackend{""}}, http.StatusServiceUnavailable, msgLibrariesDown},
		{"refused", Deps{KiwixServer: stubKiwixBackend{dead.URL}}, http.StatusBadGateway, msgLibrariesUnreachable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ts := librariesOriginTestServer(t, c.deps)
			host := strings.Split(strings.TrimPrefix(ts.URL, "http://"), ":")[0]
			code, body, header := librariesTestGetAccept(t, ts, "/kiwix/content/book/A", browserAccept)
			if code != c.code {
				t.Fatalf("status %d, want %d: %s", code, c.code, body)
			}
			if ct := header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
				t.Errorf("Content-Type = %q, want text/html for a page", ct)
			}
			if strings.Contains(string(body), `{"error"`) {
				t.Errorf("the page shows raw JSON:\n%s", body)
			}
			if want := `href="http://` + host + `:8070/libraries" target="_top"`; !strings.Contains(string(body), want) {
				t.Errorf("no %s home button (whole tab, even from the reader's frame):\n%s", want, body)
			}
			if !strings.Contains(string(body), "Back to Libraries") {
				t.Errorf("no \"Back to Libraries\" button:\n%s", body)
			}

			code, body, header = librariesTestGetAccept(t, ts, "/kiwix/content/book/A", "*/*")
			if code != c.code || !strings.HasPrefix(header.Get("Content-Type"), "application/json") {
				t.Fatalf("API caller: status %d, Content-Type %q, want %d JSON", code, header.Get("Content-Type"), c.code)
			}
			if got := librariesTestError(t, body); got != c.msg {
				t.Errorf("API caller error = %q, want %q", got, c.msg)
			}
		})
	}
}

// TestLibrariesOriginFrontDoorRedirectsHome: /, /kiwix and /kiwix/ (where
// kiwix-serve's own home buttons lead) on the libraries origin send the
// browser to /libraries on the app origin, answered by the app itself, so
// the way home works while kiwix-serve is down too.
func TestLibrariesOriginFrontDoorRedirectsHome(t *testing.T) {
	t.Setenv("KS_PUBLIC_PORT", "8070")
	ts := librariesOriginTestServer(t, Deps{KiwixServer: stubKiwixBackend{""}})
	host := strings.Split(strings.TrimPrefix(ts.URL, "http://"), ":")[0]
	for _, p := range []string{"/", "/kiwix", "/kiwix/"} {
		code, body, header := librariesTestGetAccept(t, ts, p, browserAccept)
		if code != http.StatusFound {
			t.Errorf("GET %s = %d, want 302: %s", p, code, body)
			continue
		}
		if got, want := header.Get("Location"), "http://"+host+":8070/libraries"; got != want {
			t.Errorf("GET %s Location = %q, want %q", p, got, want)
		}
	}
	// Anything else is a kid "nothing here" page with the way back.
	code, body, _ := librariesTestGetAccept(t, ts, "/nope", browserAccept)
	if code != http.StatusNotFound || !strings.Contains(string(body), "Back to Libraries") {
		t.Errorf("GET /nope = %d, want a 404 kid page with the way back:\n%s", code, body)
	}
}

// TestKiwixReaderGetsKidsShelfBar: the reader page comes back with the
// Kids Shelf bar (big Home and Libraries links to the app origin) and its
// stylesheet; the backend was asked for an uncompressed page and the
// changed page carries no kiwix validator.
func TestKiwixReaderGetsKidsShelfBar(t *testing.T) {
	t.Setenv("KS_PUBLIC_PORT", "8070")
	var acceptEnc string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acceptEnc = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Etag", `"kiwix-etag"`)
		_, _ = w.Write([]byte("<!DOCTYPE html>\n<html><head><title>ZIM Viewer</title></head>\n<body style=\"margin:0\" onload=\"setupViewer()\"><div id=\"kiwixtoolbarwrapper\"></div></body></html>"))
	}))
	defer backend.Close()
	ts := librariesOriginTestServer(t, Deps{KiwixServer: stubKiwixBackend{backend.URL}})
	host := strings.Split(strings.TrimPrefix(ts.URL, "http://"), ":")[0]

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/kiwix/viewer", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /kiwix/viewer: %v", err)
	}
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	resp.Body.Close()
	body := b.String()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if acceptEnc != "identity" {
		t.Errorf("backend saw Accept-Encoding %q, want identity for a page the proxy changes", acceptEnc)
	}
	if resp.Header.Get("Etag") != "" {
		t.Errorf("changed reader page kept kiwix's ETag %q", resp.Header.Get("Etag"))
	}
	for _, want := range []string{
		`<link rel="stylesheet" href="/ks/viewer.css"></head>`,
		`onload="setupViewer()"><nav class="ks-bar"`,
		`href="http://` + host + `:8070/">`,
		`href="http://` + host + `:8070/libraries">`,
		`Home</a>`,
		`Libraries</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("reader page missing %q:\n%s", want, body)
		}
	}
	if resp.ContentLength != int64(len(body)) {
		t.Errorf("Content-Length %d, body %d bytes", resp.ContentLength, len(body))
	}
}

// TestKiwixBookPagesGetLinkGuard: HTML book pages get the outside-link
// guard script; anything else under /kiwix/content/ (an image) and other
// kiwix pages pass through byte for byte.
func TestKiwixBookPagesGetLinkGuard(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/kiwix/content/book/A":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><head><title>A</title></head><body><a href="https://example.com">x</a></body></html>`))
		default:
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("png </head> bytes"))
		}
	}))
	defer backend.Close()
	ts := librariesOriginTestServer(t, Deps{KiwixServer: stubKiwixBackend{backend.URL}})

	_, body, _ := librariesTestGet(t, ts, "/kiwix/content/book/A")
	if !strings.Contains(string(body), `<script src="/ks/content.js"></script></head>`) {
		t.Errorf("book page without the link guard:\n%s", body)
	}
	for _, p := range []string{"/kiwix/content/book/I/a.png", "/kiwix/skin/x.png"} {
		if _, body, _ := librariesTestGet(t, ts, p); string(body) != "png </head> bytes" {
			t.Errorf("GET %s = %q, want the backend's bytes unchanged", p, body)
		}
	}
	for _, p := range []string{"/ks/viewer.css", "/ks/content.js"} {
		if code, _, _ := librariesTestGet(t, ts, p); code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", p, code)
		}
	}
	if code, _, _ := librariesTestGet(t, ts, "/ks/libraries.js"); code != http.StatusNotFound {
		t.Errorf("GET /ks/libraries.js = %d, want 404 (only the reader's own files)", code)
	}
}

// TestKiwixProxyFrozenBackendShowsPage: a backend that never answers ends
// in the kid's "did not answer" page once the response bound passes,
// instead of a page that loads forever.
func TestKiwixProxyFrozenBackendShowsPage(t *testing.T) {
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer backend.Close()
	defer close(release)
	old := kiwixProxyTransport
	kiwixProxyTransport = newKiwixTransport(200 * time.Millisecond)
	t.Cleanup(func() { kiwixProxyTransport = old })

	ts := librariesOriginTestServer(t, Deps{KiwixServer: stubKiwixBackend{backend.URL}})
	start := time.Now()
	code, body, _ := librariesTestGetAccept(t, ts, "/kiwix/content/book/A", browserAccept)
	if code != http.StatusBadGateway || !strings.Contains(string(body), "Back to Libraries") {
		t.Errorf("status %d, want 502 with the kid page:\n%s", code, body)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the frozen backend held the page for %v", d)
	}
}

// TestAPILibrariesMarksMissingFile: a book whose file is not on disk is
// listed as missing with nothing to open, next to a book that opens.
func TestAPILibrariesMarksMissingFile(t *testing.T) {
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join("..", "kiwix", "testdata", "entries_eng_20.xml"))
	}))
	defer feed.Close()
	km, s, lib := librariesKiwixManager(t)
	seedInstalledRow(t, s, lib, "urn:uuid:600c47c6-a792-0f78-8fcc-9c05cf1c556b", "a_here", "Here", "eng", "a_here.zim")
	seedInstalledRow(t, s, "", "urn:uuid:gone", "b_gone", "Gone", "eng", "b_gone.zim")
	ts := librariesTestServer(t, Deps{Kiwix: km, KiwixServer: stubKiwixBackend{feed.URL}})

	_, body, _ := librariesTestGet(t, ts, "/api/libraries")
	var out struct {
		Available bool          `json:"available"`
		Books     []libraryBook `json:"books"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decoding %q: %v", body, err)
	}
	if !out.Available || len(out.Books) != 2 {
		t.Fatalf("available %v, %d books, want true and 2: %s", out.Available, len(out.Books), body)
	}
	if b := out.Books[0]; b.Missing || b.URL == "" {
		t.Errorf("book on disk = %+v, want an openable tile", b)
	}
	if b := out.Books[1]; !b.Missing || b.URL != "" {
		t.Errorf("book without its file = %+v, want missing and no URL", b)
	}
}

// TestAPILibrariesUnansweringBackendUnavailable: a backend the supervisor
// still lists but that answers nothing (frozen, gone) is reported as not
// available, so the page does not offer tiles that cannot open.
func TestAPILibrariesUnansweringBackendUnavailable(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	km, s, lib := librariesKiwixManager(t)
	seedInstalledRow(t, s, lib, "urn:uuid:here", "a_here", "Here", "eng", "a_here.zim")
	ts := librariesTestServer(t, Deps{Kiwix: km, KiwixServer: stubKiwixBackend{dead.URL}})
	_, body, _ := librariesTestGet(t, ts, "/api/libraries")
	var out struct {
		Available bool          `json:"available"`
		Books     []libraryBook `json:"books"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decoding %q: %v", body, err)
	}
	if out.Available || len(out.Books) != 1 || out.Books[0].URL != "" {
		t.Errorf("got %s, want available false and the book listed without a URL", body)
	}
}

// The two books of internal/kiwix/testdata/served_entries.xml, the feed
// kiwix-serve 3.8.2 answered for "My Wiki (2026).zim" and "Élève
// été+plus #1.zim" (the second a copy of the test ZIM with another UUID).
const (
	servedWikiUUID  = "975eb51a-1c3d-5383-8f3f-2db8f9cb8798"
	servedEleveUUID = "00010203-0405-0607-0809-0a0b0c0d0e0f"
)

// TestAPILibrariesFolderBooks: books from a library folder are listed
// like downloads, with links under the names kiwix-serve gave their
// awkwardly named files (found by the UUID in each file's header, escaped
// like kiwix-serve's own links) and their icons; when the folder is out of
// reach they stay listed, as missing, with nothing to open.
func TestAPILibrariesFolderBooks(t *testing.T) {
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join("..", "kiwix", "testdata", "served_entries.xml"))
	}))
	defer feed.Close()
	km, _, _ := librariesKiwixManager(t)
	parent := t.TempDir()
	dir := filepath.Join(parent, "Kiwix")
	for name, uuid := range map[string]string{
		"My Wiki (2026).zim":             servedWikiUUID,
		"Élève été+plus #1.zim":          servedEleveUUID,
		"sub/not-served-yet_2026-10.zim": "11111111-2222-3333-4444-555555555555",
	} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, testZIM(uuid), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := km.AddFolder(t.Context(), dir); err != nil || n != 3 {
		t.Fatalf("AddFolder = %d, %v, want 3", n, err)
	}
	ts := librariesTestServer(t, Deps{Kiwix: km, KiwixServer: stubKiwixBackend{feed.URL}})
	read := func() (bool, map[string]libraryBook) {
		t.Helper()
		code, body, _ := librariesTestGet(t, ts, "/api/libraries")
		if code != http.StatusOK {
			t.Fatalf("GET /api/libraries = %d: %s", code, body)
		}
		var out struct {
			Available bool          `json:"available"`
			Books     []libraryBook `json:"books"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decoding %q: %v", body, err)
		}
		byTitle := map[string]libraryBook{}
		for _, b := range out.Books {
			byTitle[b.Title] = b
		}
		return out.Available, byTitle
	}

	available, books := read()
	want := map[string]libraryBook{
		"My Wiki (2026)": {
			Name: "My Wiki (2026)", Title: "My Wiki (2026)",
			URL:  "/kiwix/viewer#my_wiki_(2026)",
			Icon: "/kiwix/catalog/v2/illustration/" + servedWikiUUID + "/?size=48",
		},
		"Élève été+plus #1": {
			Name: "Élève été+plus #1", Title: "Élève été+plus #1",
			URL:  "/kiwix/viewer#eleve_eteplusplus_%231",
			Icon: "/kiwix/catalog/v2/illustration/" + servedEleveUUID + "/?size=48",
		},
		// kiwix-serve has not been restarted with this one yet: listed,
		// nothing to open.
		"not-served-yet 2026-10": {Name: "not-served-yet_2026-10", Title: "not-served-yet 2026-10"},
	}
	if !available || len(books) != len(want) {
		t.Fatalf("available %v, books %+v, want available and %d books", available, books, len(want))
	}
	for title, w := range want {
		if got := books[title]; got != w {
			t.Errorf("book %q = %+v, want %+v", title, got, w)
		}
	}

	if err := os.Rename(dir, filepath.Join(parent, "away")); err != nil {
		t.Fatal(err)
	}
	available, books = read()
	if !available || len(books) != len(want) {
		t.Fatalf("with the folder away: available %v, books %+v, want available and %d books", available, books, len(want))
	}
	for title := range want {
		if b := books[title]; !b.Missing || b.URL != "" || b.Icon != "" {
			t.Errorf("book %q with its folder away = %+v, want missing with nothing to open", title, b)
		}
	}
}

// TestAPILibrariesFolderBookRealKiwixServe: with the real kiwix-serve
// serving a library folder whose file has an awkward name, the kid API's
// link opens the book through the libraries origin, and the title the
// ZIM gives itself replaces the file name. It needs the built helper
// (KS_HELPERS_DIR) and a ZIM to copy (KS_TEST_ZIM) and skips otherwise.
func TestAPILibrariesFolderBookRealKiwixServe(t *testing.T) {
	helperDir := os.Getenv("KS_HELPERS_DIR")
	zim := os.Getenv("KS_TEST_ZIM")
	if helperDir == "" || zim == "" {
		t.Skip("KS_HELPERS_DIR or KS_TEST_ZIM unset (real kiwix-serve test)")
	}
	bin := filepath.Join(helperDir, "kiwix-serve")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("%s not built: %v", bin, err)
	}
	data, err := os.ReadFile(zim)
	if err != nil {
		t.Fatalf("KS_TEST_ZIM: %v", err)
	}
	km, _, _ := librariesKiwixManager(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "My Wiki (2026).zim"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if n, err := km.AddFolder(t.Context(), dir); err != nil || n != 1 {
		t.Fatalf("AddFolder = %d, %v, want 1", n, err)
	}
	// Wired like the app: rows resolved through FilePath, what kiwix-serve
	// reports kept in the rows.
	srv := kiwix.NewServer(bin, func() ([]kiwix.Installed, error) {
		rows, err := km.Installed()
		if err != nil {
			return nil, err
		}
		var books []kiwix.Installed
		for _, in := range rows {
			if p, err := km.FilePath(in); err == nil {
				in.File = p
				books = append(books, in)
			}
		}
		return books, nil
	})
	srv.OnServed = func(b map[string]kiwix.ServedBook) {
		if err := km.RecordServed(b); err != nil {
			t.Errorf("RecordServed: %v", err)
		}
	}
	km.OnChange = srv.Sync
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	deps := Deps{Kiwix: km, KiwixServer: srv}
	ts := librariesTestServer(t, deps)
	origin := librariesOriginTestServer(t, deps)
	var book libraryBook
	for deadline := time.Now().Add(30 * time.Second); ; {
		_, body, _ := librariesTestGet(t, ts, "/api/libraries")
		var out struct {
			Available bool          `json:"available"`
			Books     []libraryBook `json:"books"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decoding %q: %v", body, err)
		}
		if out.Available && len(out.Books) == 1 && out.Books[0].URL != "" && out.Books[0].Title != "My Wiki (2026)" {
			book = out.Books[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no openable book with the ZIM's own title from the real kiwix-serve: %s", body)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if book.URL != "/kiwix/viewer#my_wiki_(2026)" || book.Missing {
		t.Errorf("book = %+v, want the viewer link kiwix-serve's name my_wiki_(2026)", book)
	}
	// The reader opens /kiwix/content/<the part after #>/ (which leads on
	// to the book's main page) and asks for /kiwix/raw/<it>/meta/Title:
	// both answer through the libraries origin.
	name := strings.TrimPrefix(book.URL, "/kiwix/viewer#")
	client := &http.Client{Timeout: 60 * time.Second}
	for _, path := range []string{"/kiwix/viewer", "/kiwix/content/" + name + "/", "/kiwix/raw/" + name + "/meta/Title"} {
		resp, err := client.Get(origin.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s on the libraries origin = %d: %.200s", path, resp.StatusCode, body)
		}
		if strings.HasSuffix(path, "/Title") && string(body) != book.Title {
			t.Errorf("GET %s = %q, want the listed title %q", path, body, book.Title)
		}
	}
}
