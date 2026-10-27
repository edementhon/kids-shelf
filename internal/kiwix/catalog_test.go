package kiwix

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Parser tests run against the real responses saved under testdata/ (one
// entries page, the categories and languages lists, and a real .meta4 from
// download.kiwix.org).

func openTestdata(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("open testdata/%s: %v", name, err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestParseEntries(t *testing.T) {
	// The feed was fetched with lang=eng&count=20; page metadata is asserted
	// against the file contents.
	base, err := url.Parse("https://opds.library.kiwix.org/catalog/v2/entries")
	if err != nil {
		t.Fatal(err)
	}
	res, err := parseEntries(openTestdata(t, "entries_eng_20.xml"), base)
	if err != nil {
		t.Fatalf("parseEntries: %v", err)
	}
	if res.Total != 1311 || res.Start != 0 || res.Count != 20 || len(res.Entries) != 20 {
		t.Fatalf("feed paging = total %d start %d count %d entries %d, want 1311/0/20/20",
			res.Total, res.Start, res.Count, len(res.Entries))
	}

	e := res.Entries[0]
	want := Entry{
		ID:           "urn:uuid:ab7c8918-9940-f83d-9a14-7f64b4f55636",
		Name:         "ja.stackoverflow.com_mul_all",
		Title:        "スタック・オーバーフロー",
		Summary:      "熱狂的でプロフェッショナルなプログラマーのためのQ&A",
		Language:     "eng,jpn",
		Category:     "stack_exchange",
		Tags:         "_details:no;stack_exchange;_category:stack_exchange;_videos:no;_ftindex:yes;_pictures:yes",
		ArticleCount: 46670,
		MediaCount:   6901,
		Size:         236055552,
		Meta4URL:     "https://lb.download.kiwix.org/zim/stack_exchange/ja.stackoverflow.com_mul_all_2026-10.zim.meta4",
		// Relative thumbnail href resolved against the feed URL.
		IllustrationURL: "https://opds.library.kiwix.org/catalog/v2/illustration/ab7c8918-9940-f83d-9a14-7f64b4f55636/?size=48",
		Updated:         time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC),
	}
	if e != want {
		t.Errorf("entry[0] = %+v, want %+v", e, want)
	}

	// The second entry has a multi-word title and an empty category keeps the
	// empty string rather than being dropped.
	e = res.Entries[1]
	if e.Name != "php.net_en_all" || e.Title != "PHP Manual" || e.Size != 32847872 {
		t.Errorf("entry[1] = %+v", e)
	}
	if res.Entries[2].Category != "" {
		t.Errorf("entry[2].Category = %q, want empty", res.Entries[2].Category)
	}
}

func TestParseCategories(t *testing.T) {
	cs, err := parseCategories(openTestdata(t, "categories.xml"))
	if err != nil {
		t.Fatalf("parseCategories: %v", err)
	}
	if len(cs) != 16 {
		t.Fatalf("got %d categories, want 16", len(cs))
	}
	if cs[0].Name != "gutenberg" {
		t.Errorf("categories[0] = %q, want gutenberg", cs[0].Name)
	}
	seen := map[string]bool{}
	for _, c := range cs {
		seen[c.Name] = true
	}
	for _, want := range []string{"stack_exchange", "vikidia", "phet", "other"} {
		if !seen[want] {
			t.Errorf("categories missing %q", want)
		}
	}
}

func TestParseLanguages(t *testing.T) {
	ls, err := parseLanguages(openTestdata(t, "languages.xml"))
	if err != nil {
		t.Fatalf("parseLanguages: %v", err)
	}
	if len(ls) != 337 {
		t.Fatalf("got %d languages, want 337", len(ls))
	}
	if ls[0] != (Language{Code: "abk", Name: "Abkhazian", Count: 2}) {
		t.Errorf("languages[0] = %+v", ls[0])
	}
	if last := ls[len(ls)-1]; last.Code != "zul" || last.Name != "isiZulu" {
		t.Errorf("last language = %+v", last)
	}
	found := false
	for _, l := range ls {
		if l.Code == "eng" {
			found = true
			if l.Name != "English" || l.Count != 1311 {
				t.Errorf("eng entry = %+v, want English/1311", l)
			}
		}
	}
	if !found {
		t.Error("languages feed has no eng entry")
	}
}

func TestParseMeta4(t *testing.T) {
	m, err := parseMeta4(openTestdata(t, "vikidia_en_all_maxi_2026-09.zim.meta4"))
	if err != nil {
		t.Fatalf("parseMeta4: %v", err)
	}
	if m.File != "vikidia_en_all_maxi_2026-09.zim" {
		t.Errorf("file = %q", m.File)
	}
	if m.Size != 77565163 {
		t.Errorf("size = %d, want 77565163", m.Size)
	}
	if m.SHA256 != "16728e6be34081f6c627b3cbf2c36ff31823defb0f30b68fab146b5ca68e591e" {
		t.Errorf("sha-256 = %q", m.SHA256)
	}
	if len(m.Mirrors) != 5 {
		t.Fatalf("got %d mirrors, want 5", len(m.Mirrors))
	}
	// Mirrors must come back in metalink priority order: 1, 2, 3, 4, 5.
	wantURLs := []string{
		"https://ny.mirror.driftle.ss/kiwix/zim/vikidia/vikidia_en_all_maxi_2026-09.zim",
		"https://wi.mirror.driftle.ss/kiwix/zim/vikidia/vikidia_en_all_maxi_2026-09.zim",
		"https://ftp.nluug.nl/pub/kiwix/zim/vikidia/vikidia_en_all_maxi_2026-09.zim",
		"https://mirror.download.kiwix.org/zim/vikidia/vikidia_en_all_maxi_2026-09.zim",
		"https://mirrors.dotsrc.org/kiwix/zim/vikidia/vikidia_en_all_maxi_2026-09.zim",
	}
	for i, w := range wantURLs {
		if m.Mirrors[i].URL != w {
			t.Errorf("mirrors[%d] = %s, want %s", i, m.Mirrors[i].URL, w)
		}
	}
	if m.Mirrors[0].Priority != 1 || m.Mirrors[4].Priority != 5 {
		t.Errorf("priority order wrong: %+v", m.Mirrors)
	}
}

// TestCatalog exercises the client end to end against an httptest server
// serving the saved responses, so URL building, status handling and link
// resolution are covered without the live API.
func TestCatalog(t *testing.T) {
	// gotQuery is handed from the handler goroutine to this one over a
	// channel; a plain variable would be a data race.
	gotQuery := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/catalog/v2/entries":
			http.ServeFile(w, r, filepath.Join("testdata", "entries_eng_20.xml"))
			gotQuery <- r.URL.RawQuery
		case "/catalog/v2/categories":
			http.ServeFile(w, r, filepath.Join("testdata", "categories.xml"))
		case "/catalog/v2/languages":
			http.ServeFile(w, r, filepath.Join("testdata", "languages.xml"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewCatalog(srv.URL, AllowLoopback(srv.Client()))
	res, err := c.Entries(context.Background(), Query{Lang: "eng", Count: 20})
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(res.Entries) != 20 {
		t.Fatalf("got %d entries, want 20", len(res.Entries))
	}
	if q := <-gotQuery; q != "count=20&lang=eng" {
		t.Errorf("entries query = %q, want count=20&lang=eng", q)
	}
	// The thumbnail href in the saved feed is relative; the client must hand
	// back an absolute URL.
	got := res.Entries[0].IllustrationURL
	want := srv.URL + "/catalog/v2/illustration/ab7c8918-9940-f83d-9a14-7f64b4f55636/?size=48"
	if got != want {
		t.Errorf("illustration URL = %q, want %q", got, want)
	}

	cs, err := c.Categories(context.Background())
	if err != nil {
		t.Fatalf("Categories: %v", err)
	}
	if len(cs) != 16 {
		t.Errorf("got %d categories, want 16", len(cs))
	}

	ls, err := c.Languages(context.Background())
	if err != nil {
		t.Fatalf("Languages: %v", err)
	}
	if len(ls) != 337 {
		t.Errorf("got %d languages, want 337", len(ls))
	}
}

func TestCatalogHTTPError(t *testing.T) {
	var statusCode = http.StatusInternalServerError
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(statusCode)
	}))
	defer srv.Close()

	c := NewCatalog(srv.URL, AllowLoopback(srv.Client()))
	if _, err := c.Entries(context.Background(), Query{}); err == nil {
		t.Fatal("Entries succeeded, want an HTTP error")
	}
	statusCode = http.StatusNotFound
	if _, err := c.Languages(context.Background()); err == nil {
		t.Fatal("Languages succeeded, want an HTTP error")
	}
}

// TestCatalogNameFilter locks the URL of a name-filtered entries query: the
// catalog's OpenSearch description documents name as an exact ZIM base name
// filter (the k:name extension parameter), distinct from the q free-text
// search.
func TestCatalogNameFilter(t *testing.T) {
	gotQuery := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/catalog/v2/entries" {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join("testdata", "entries_eng_20.xml"))
		gotQuery <- r.URL.RawQuery
	}))
	defer srv.Close()

	c := NewCatalog(srv.URL, AllowLoopback(srv.Client()))
	if _, err := c.Entries(context.Background(), Query{Name: "vikidia_en_all", Count: 100}); err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if q := <-gotQuery; q != "count=100&name=vikidia_en_all" {
		t.Errorf("entries query = %q, want count=100&name=vikidia_en_all", q)
	}
}

// lookupTestEntries is a slice of the live catalog's Vikidia entries, ids
// and meta4 URLs included, served by the Lookup test's fake.
func lookupTestEntries() []Entry {
	return []Entry{
		{
			ID: "urn:uuid:6ea683ce-15bf-8036-74c6-3b5c1eeabde2", Name: "vikidia_en_all",
			Title: "Vikidia", Summary: "The encyclopedia for Children", Language: "eng",
			Category: "vikidia", Flavour: "maxi", ArticleCount: 9411, MediaCount: 6791,
			Updated: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Size: 77565952,
			Meta4URL: "https://lb.download.kiwix.org/zim/vikidia/vikidia_en_all_maxi_2026-09.zim.meta4",
		},
		{
			ID: "urn:uuid:16dc00df-6d5c-a923-de75-2ab3761c13ea", Name: "vikidia_en_all",
			Title: "Vikidia", Summary: "The encyclopedia for Children", Language: "eng",
			Category: "vikidia", Flavour: "nopic", ArticleCount: 9441, MediaCount: 25,
			Updated: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Size: 10556416,
			Meta4URL: "https://lb.download.kiwix.org/zim/vikidia/vikidia_en_all_nopic_2026-09a.zim.meta4",
		},
		{
			ID: "urn:uuid:975eb51a-1c3d-5383-8f3f-2db8f9cb8798", Name: "vikidia_scn_all",
			Title: "Vikidia", Summary: "Di Vikidia", Language: "scn",
			Category: "vikidia", Flavour: "maxi", ArticleCount: 520, MediaCount: 426,
			Updated: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Size: 1792000,
			Meta4URL: "https://lb.download.kiwix.org/zim/vikidia/vikidia_scn_all_maxi_2026-09.zim.meta4",
		},
	}
}

// TestCatalogLookup drives Catalog.Lookup against a fake OPDS server with the
// live catalog's filter semantics. The regression it guards: the catalog's q
// search matches titles only, never the internal ZIM names the parent page
// sends back, so looking an entry up by name must use the exact name filter
// (name=), not the free-text q search - q=vikidia_en_all returns nothing.
func TestCatalogLookup(t *testing.T) {
	srv, got := newLookupCatalog(t, false)
	defer srv.Close()
	c := NewCatalog(srv.URL, AllowLoopback(srv.Client()))

	const maxiID = "urn:uuid:6ea683ce-15bf-8036-74c6-3b5c1eeabde2"

	// The id the parent page sends for "Vikidia with pictures" resolves to
	// the catalog's own entry (size, title and meta4 URL from the feed) via
	// one page narrowed by the exact name filter.
	e, err := c.Lookup(context.Background(), maxiID, "vikidia_en_all", "maxi")
	if err != nil {
		t.Fatalf("Lookup by id: %v", err)
	}
	if e.ID != maxiID || e.Name != "vikidia_en_all" || e.Flavour != "maxi" ||
		e.Size != 77565952 || e.Meta4URL == "" {
		t.Errorf("Lookup = %+v, want the catalog's maxi edition", e)
	}
	if q := <-got; !strings.Contains(q, "name=vikidia_en_all") || strings.Contains(q, "q=") {
		t.Errorf("lookup query = %q, want a name= filter and no q search", q)
	}

	// Without an id, the name+flavour pair alone resolves the same way.
	e, err = c.Lookup(context.Background(), "", "vikidia_en_all", "nopic")
	if err != nil {
		t.Fatalf("Lookup by name+flavour: %v", err)
	}
	if e.ID != "urn:uuid:16dc00df-6d5c-a923-de75-2ab3761c13ea" || e.Flavour != "nopic" {
		t.Errorf("Lookup = %+v, want the nopic edition", e)
	}

	// An identity that is not in the catalog is a plain not-found error, the
	// signal the parent page turns into its refresh message.
	if _, err := c.Lookup(context.Background(), "urn:uuid:not-in-the-catalog-0000", "no_such_zim_en_all", "maxi"); !errors.Is(err, ErrEntryNotFound) {
		t.Errorf("unknown identity: err = %v, want ErrEntryNotFound", err)
	}
}

// TestCatalogLookupNameIgnoringServer covers a catalog that does not know the
// name extension parameter and answers every query with the whole feed:
// Lookup still finds the id by paging and scanning.
func TestCatalogLookupNameIgnoringServer(t *testing.T) {
	srv, _ := newLookupCatalog(t, true)
	defer srv.Close()
	c := NewCatalog(srv.URL, AllowLoopback(srv.Client()))

	const maxiID = "urn:uuid:6ea683ce-15bf-8036-74c6-3b5c1eeabde2"
	e, err := c.Lookup(context.Background(), maxiID, "vikidia_en_all", "maxi")
	if err != nil {
		t.Fatalf("Lookup on a name-ignoring catalog: %v", err)
	}
	if e.ID != maxiID {
		t.Errorf("Lookup = %+v, want id %s", e, maxiID)
	}
}

// newLookupCatalog serves lookupTestEntries with the real OPDS filter
// semantics: the name parameter filters by exact ZIM base name, while q is a
// free-text search over titles that never matches an internal name. With
// ignoreFilters it answers every query with the whole feed, the way a server
// without the name extension behaves. Every entries request's raw query is
// sent to got for assertion.
func newLookupCatalog(t *testing.T, ignoreFilters bool) (*httptest.Server, chan string) {
	t.Helper()
	got := make(chan string, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/catalog/v2/entries" {
			http.NotFound(w, r)
			return
		}
		got <- r.URL.RawQuery
		entries := lookupTestEntries()
		if !ignoreFilters {
			qs := r.URL.Query()
			if n := qs.Get("name"); n != "" {
				entries = selectEntries(entries, func(e Entry) bool { return e.Name == n })
			} else if s := qs.Get("q"); s != "" {
				entries = selectEntries(entries, func(e Entry) bool {
					return strings.Contains(strings.ToLower(e.Title), strings.ToLower(s))
				})
			}
		}
		writeEntriesFeed(w, entries)
	}))
	t.Cleanup(func() { close(got) })
	return srv, got
}

func selectEntries(entries []Entry, keep func(Entry) bool) []Entry {
	var out []Entry
	for _, e := range entries {
		if keep(e) {
			out = append(out, e)
		}
	}
	return out
}

// writeEntriesFeed emits a minimal OPDS entries feed for the given entries.
func writeEntriesFeed(w http.ResponseWriter, entries []Entry) {
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom" xmlns:dc="http://purl.org/dc/terms/">
  <id>lookup-test</id>
  <title>Filtered Entries</title>
  <updated>2026-09-01T00:00:00Z</updated>
  <totalResults>%d</totalResults>
  <startIndex>0</startIndex>
  <itemsPerPage>%d</itemsPerPage>
`, len(entries), len(entries))
	for _, e := range entries {
		fmt.Fprintf(w, `  <entry>
    <id>%s</id>
    <title>%s</title>
    <updated>%s</updated>
    <summary>%s</summary>
    <language>%s</language>
    <name>%s</name>
    <flavour>%s</flavour>
    <category>%s</category>
    <tags>_pictures:yes</tags>
    <articleCount>%d</articleCount>
    <mediaCount>%d</mediaCount>
    <link rel="http://opds-spec.org/image/thumbnail" href="https://example.invalid/ill.png" type="image/png"/>
    <link rel="http://opds-spec.org/acquisition/open-access" type="application/x-zim" href="%s" length="%d" />
  </entry>
`, e.ID, e.Title, e.Updated.UTC().Format(time.RFC3339), e.Summary, e.Language,
			e.Name, e.Flavour, e.Category, e.ArticleCount, e.MediaCount, e.Meta4URL, e.Size)
	}
	fmt.Fprint(w, "</feed>\n")
}
