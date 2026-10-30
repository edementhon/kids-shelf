package kiwix

// Tests for the served-books side (served.go): kiwix-serve's own feed, the
// names it gives books, and how installed rows find their book in it.

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The two books in testdata/served_entries.xml, the feed kiwix-serve 3.8.2
// answered for "My Wiki (2026).zim" and "Élève été+plus #1.zim" (a copy of
// the test ZIM with another UUID).
const (
	servedWikiUUID  = "975eb51a-1c3d-5383-8f3f-2db8f9cb8798"
	servedEleveUUID = "00010203-0405-0607-0809-0a0b0c0d0e0f"
)

// TestParseServedFeedFromKiwixServe reads the feed kiwix-serve wrote for
// two awkwardly named files: each book under its UUID, with the name
// kiwix-serve serves it under (lower case, no accents, "+" as "plus"),
// the ZIM's own title, language and name, and its illustration.
func TestParseServedFeedFromKiwixServe(t *testing.T) {
	feed, err := parseServedFeed(openTestdata(t, "served_entries.xml"))
	if err != nil {
		t.Fatalf("parseServedFeed: %v", err)
	}
	if len(feed) != 2 {
		t.Fatalf("books = %d, want 2: %+v", len(feed), feed)
	}
	for uuid, want := range map[string]ServedBook{
		servedWikiUUID: {
			UUID: servedWikiUUID, Book: "my_wiki_(2026)", Name: "vikidia_scn_all", Title: "Vikidia", Language: "scn",
			Icon: "/kiwix/catalog/v2/illustration/" + servedWikiUUID + "/?size=48",
		},
		servedEleveUUID: {
			UUID: servedEleveUUID, Book: "eleve_eteplusplus_#1", Name: "vikidia_scn_all", Title: "Vikidia", Language: "scn",
			Icon: "/kiwix/catalog/v2/illustration/" + servedEleveUUID + "/?size=48",
		},
	} {
		if got := feed[uuid]; got != want {
			t.Errorf("book %s = %+v, want %+v", uuid, got, want)
		}
	}
}

// TestURLNameEscapesLikeKiwixServe: a book name is escaped exactly as
// kiwix-serve 3.8 writes it in its own links (each pair below was seen in
// its feed for a file of that name).
func TestURLNameEscapesLikeKiwixServe(t *testing.T) {
	for book, want := range map[string]string{
		"my_wiki_(2026)":         "my_wiki_(2026)",
		"vikidia_en_all_2026-09": "vikidia_en_all_2026-09",
		"dot.name.v2":            "dot.name.v2",
		"hash#and?q":             "hash%23and%3Fq",
		"pct%20sign":             "pct%2520sign",
		`quote'dq"x`:             "quote'dq%22x",
		"semi;colon&amp=x":       "semi%3Bcolon%26amp%3Dx",
		"tab\tx":                 "tab%09x",
		"emoji_🦊":                "emoji_%F0%9F%A6%8A",
		"unicøde_日本":             "unic%C3%B8de_%E6%97%A5%E6%9C%AC",
		"eleve_eteplusplus_#1":   "eleve_eteplusplus_%231",
	} {
		if got := (ServedBook{Book: book}).URLName(); got != want {
			t.Errorf("URLName(%q) = %q, want %q", book, got, want)
		}
	}
}

// TestServeReportsServedBooks: once kiwix-serve is up, the supervisor
// reads its feed and reports, per installed row, the book that serves the
// row's file (found by the UUID in the file's header) to OnServed; a file
// the feed does not list is left out.
func TestServeReportsServedBooks(t *testing.T) {
	t.Setenv("KS_SERVE_TEST_FAKE", "1")
	feedFile, err := filepath.Abs(filepath.Join("testdata", "served_entries.xml"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("KS_SERVE_TEST_FEED", feedFile)
	_ = fakeArgvFile(t)
	dir := t.TempDir()
	books := &bookList{zims: []Installed{
		{ID: "row-wiki", File: putFile(t, dir, "My Wiki (2026).zim", zimBytes(servedWikiUUID, 64))},
		{ID: "row-eleve", File: putFile(t, dir, "Élève été+plus #1.zim", zimBytes(servedEleveUUID, 64))},
		{ID: "row-other", File: putFile(t, dir, "other.zim", zimBytes("11111111-2222-3333-4444-555555555555", 64))},
	}}
	s := NewServer(fakeServe(t), books.snapshot())
	reports := make(chan map[string]ServedBook, 4)
	s.OnServed = func(b map[string]ServedBook) { reports <- b }
	_, _, _ = startServer(t, s)

	var got map[string]ServedBook
	select {
	case got = <-reports:
	case <-time.After(20 * time.Second):
		t.Fatal("OnServed never ran after kiwix-serve came up")
	}
	if len(got) != 2 {
		t.Fatalf("reported %d books, want the 2 the feed lists: %+v", len(got), got)
	}
	if b := got["row-wiki"]; b.Book != "my_wiki_(2026)" || b.Title != "Vikidia" || b.Language != "scn" || b.Name != "vikidia_scn_all" {
		t.Errorf("row-wiki = %+v, want my_wiki_(2026) with the ZIM's own title, language and name", b)
	}
	if b := got["row-eleve"]; b.Book != "eleve_eteplusplus_#1" {
		t.Errorf("row-eleve = %+v, want eleve_eteplusplus_#1", b)
	}
}

// TestFolderBooksOnRealKiwixServe runs the real kiwix-serve on a library
// folder whose files have awkward names (spaces, brackets, capitals,
// accents, "+" and "#"): every book is found in its feed, the name the
// shelf links to opens the book and its metadata, and what kiwix-serve
// reports ends up in the rows. It needs the built helper (KS_HELPERS_DIR)
// and a ZIM to copy (KS_TEST_ZIM) and skips otherwise.
func TestFolderBooksOnRealKiwixServe(t *testing.T) {
	helperDir := os.Getenv("KS_HELPERS_DIR")
	zim := os.Getenv("KS_TEST_ZIM")
	if helperDir == "" || zim == "" {
		t.Skip("KS_HELPERS_DIR or KS_TEST_ZIM unset (real kiwix-serve test)")
	}
	bin := filepath.Join(helperDir, serveName("kiwix-serve"))
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("%s not built: %v", bin, err)
	}
	data, err := os.ReadFile(zim)
	if err != nil {
		t.Fatalf("KS_TEST_ZIM: %v", err)
	}
	m, _, _ := newTestManager(t)
	dir := t.TempDir()
	wiki := putFile(t, dir, "My Wiki (2026).zim", data)
	// Two files of one ZIM are one book to kiwix-serve, so the second copy
	// gets a UUID of its own.
	other := append([]byte(nil), data...)
	copy(other[8:24], zimBytes(servedEleveUUID, 24)[8:24])
	eleve := putFile(t, dir, "sub/Élève été+plus #1.zim", other)
	if n, err := m.AddFolder(context.Background(), dir); err != nil || n != 2 {
		t.Fatalf("AddFolder = %d, %v, want 2", n, err)
	}

	s := NewServer(bin, func() ([]Installed, error) {
		rows, err := m.Installed()
		if err != nil {
			return nil, err
		}
		var books []Installed
		for _, in := range rows {
			if p, err := m.FilePath(in); err == nil {
				in.File = p
				books = append(books, in)
			}
		}
		return books, nil
	})
	recorded := make(chan struct{}, 4)
	s.OnServed = func(b map[string]ServedBook) {
		if err := m.RecordServed(b); err != nil {
			t.Errorf("RecordServed: %v", err)
		}
		recorded <- struct{}{}
	}
	m.OnChange = s.Sync
	_, _, _ = startServer(t, s)
	await(t, "real kiwix-serve ready", 20*time.Second, func() (bool, string) {
		return s.Backend() != "", s.Backend()
	})

	client := &http.Client{Timeout: 60 * time.Second}
	feed, err := FetchServedFeed(context.Background(), client, s.Backend())
	if err != nil {
		t.Fatalf("FetchServedFeed: %v", err)
	}
	books := folderBooks(t, m, dir)
	for file, want := range map[string]string{wiki: "my_wiki_(2026)", eleve: "eleve_eteplusplus_#1"} {
		sb, ok := m.ServedAs(books[file], feed)
		if !ok || sb.Book != want {
			t.Errorf("%s: served as %+v (found %v), want kiwix-serve's name %q", file, sb, ok, want)
			continue
		}
		// The reader opens /kiwix/content/<name>/ and asks for the title
		// at /kiwix/raw/<name>/meta/Title, with <name> as the link has it.
		for _, path := range []string{"/kiwix/content/" + sb.URLName() + "/", "/kiwix/raw/" + sb.URLName() + "/meta/Title"} {
			resp, err := client.Get(s.Backend() + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("GET %s = %d, want 200", path, resp.StatusCode)
			}
			if filepath.Base(path) == "Title" && string(body) != sb.Title {
				t.Errorf("GET %s = %q, want the feed's title %q", path, body, sb.Title)
			}
		}
	}

	select {
	case <-recorded:
	case <-time.After(20 * time.Second):
		t.Fatal("OnServed never ran with the real kiwix-serve")
	}
	for file, in := range folderBooks(t, m, dir) {
		sb, _ := m.ServedAs(in, feed)
		if sb.Title == "" || in.Title != sb.Title || in.Language != sb.Language || in.Name != sb.Name {
			t.Errorf("%s: row %+v, want the ZIM's own title, language and name from %+v", file, in, sb)
		}
	}
}
