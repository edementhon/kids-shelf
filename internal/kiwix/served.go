package kiwix

import (
	"context"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// The books a running kiwix-serve serves, as its own OPDS feed reports
// them, and how an installed ZIM is found among them: by the UUID in the
// ZIM's header, which is the id kiwix-serve gives the book. That is what
// makes book links exact whatever the file is called. kiwix-serve 3.8
// names a book after its file: lower case, accents removed, spaces as
// underscores, "+" as "plus" and the .zim dropped ("My Wiki (2026).zim" is
// served as my_wiki_(2026), "Élève.zim" as eleve). Two files holding the
// same ZIM (one UUID) are served once, under the name of the last one on
// its command line, and both installed rows lead to that one book.

// ServedBook is one book a running kiwix-serve serves, as its own OPDS
// entries feed reports it.
type ServedBook struct {
	UUID     string // the ZIM's UUID; the feed's id is urn:uuid:<UUID>
	Book     string // kiwix-serve's name for it: /kiwix/content/<Book>/, unescaped
	Name     string // the ZIM's Name metadata
	Title    string // the ZIM's Title metadata
	Language string // the ZIM's Language metadata: ISO 639-3 codes, comma-separated
	// Icon is the book's 48 px illustration on the libraries origin, or ""
	// when the ZIM has none.
	Icon string
}

// URLName is Book escaped for a URL the way kiwix-serve escapes it in its
// own links (JavaScript's encodeURIComponent: ASCII letters, digits and
// -_.!~*'() stay, every other byte becomes %XX), so the reader at
// /kiwix/viewer#<URLName> and /kiwix/content/<URLName>/ open the book.
func (b ServedBook) URLName() string {
	const hexDigits = "0123456789ABCDEF"
	n := 0
	for i := range len(b.Book) {
		if !bookNameSafe(b.Book[i]) {
			n++
		}
	}
	if n == 0 {
		return b.Book
	}
	out := make([]byte, 0, len(b.Book)+2*n)
	for i := range len(b.Book) {
		c := b.Book[i]
		if bookNameSafe(c) {
			out = append(out, c)
			continue
		}
		out = append(out, '%', hexDigits[c>>4], hexDigits[c&15])
	}
	return string(out)
}

func bookNameSafe(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' ||
		strings.IndexByte("-_.!~*'()", c) >= 0
}

// servedFeedMax caps the body of kiwix-serve's entries feed; a longer one
// fails the parse.
const servedFeedMax = 8 << 20

// FetchServedFeed reads the books a running kiwix-serve serves from its
// own OPDS entries feed, GET <base>/kiwix/catalog/v2/entries?count=-1
// (every book in one feed: kiwix-serve 3.8 reads count=-1 as "all"). base
// is the backend's "http://127.0.0.1:<port>"; client talks to it as is
// (it is loopback, so not the catalog's restricted client). The result
// maps each book's UUID to what the feed says about it.
func FetchServedFeed(ctx context.Context, client *http.Client, base string) (map[string]ServedBook, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("kiwix: backend URL %q is not a usable http URL", base)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/kiwix/catalog/v2/entries?count=-1", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kiwix: served books feed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kiwix: served books feed: HTTP %s", resp.Status)
	}
	return parseServedFeed(io.LimitReader(resp.Body, servedFeedMax))
}

// servedEntry is the part of one kiwix-serve feed entry the shelf reads.
// Unqualified tags match through the Atom default namespace, exactly as
// the catalog parsing does.
type servedEntry struct {
	ID       string     `xml:"id"`
	Title    string     `xml:"title"`
	Language string     `xml:"language"`
	Name     string     `xml:"name"`
	Links    []atomLink `xml:"link"`
}

// parseServedFeed parses a /catalog/v2/entries feed from kiwix-serve into
// its books by UUID. The book name comes from each entry's text/html link,
// /kiwix/content/<name>; an entry without a urn:uuid: id or without that
// link names no book a reader can open and is left out.
func parseServedFeed(r io.Reader) (map[string]ServedBook, error) {
	var feed struct {
		XMLName xml.Name      `xml:"feed"`
		Entries []servedEntry `xml:"entry"`
	}
	if err := xml.NewDecoder(r).Decode(&feed); err != nil {
		return nil, fmt.Errorf("kiwix: parse served books feed: %w", err)
	}
	out := make(map[string]ServedBook, len(feed.Entries))
	for _, e := range feed.Entries {
		uuid, ok := strings.CutPrefix(strings.TrimSpace(e.ID), "urn:uuid:")
		if !ok || uuid == "" {
			continue
		}
		uuid = strings.ToLower(uuid)
		b := ServedBook{UUID: uuid, Name: e.Name, Title: e.Title, Language: e.Language}
		for _, l := range e.Links {
			switch {
			case l.Type == "text/html" && l.Rel == "":
				b.Book = servedBookName(l.Href)
			case l.Rel == relThumbnail:
				b.Icon = "/kiwix/catalog/v2/illustration/" + url.PathEscape(uuid) + "/?size=48"
			}
		}
		if b.Book != "" {
			out[uuid] = b
		}
	}
	return out, nil
}

// servedBookName returns the book name in a feed entry's content link
// (/kiwix/content/<name>, possibly absolute), unescaped, or "" when the
// link has no such name.
func servedBookName(href string) string {
	u, err := url.Parse(href)
	if err != nil {
		return ""
	}
	i := strings.LastIndex(u.Path, "/content/")
	if i < 0 {
		return ""
	}
	name := strings.TrimSuffix(u.Path[i+len("/content/"):], "/")
	if name == "" || strings.Contains(name, "/") {
		return ""
	}
	return name
}

// zimMagic is the magic number at the start of every ZIM file.
const zimMagic = 0x044D495A

// zimUUID reads the UUID in the header of the ZIM file at path (16 bytes
// at offset 8, after the magic number and the version), written the way
// kiwix-serve's feed writes book ids: lower-case hex in 8-4-4-4-12 groups.
// It reads only those first 24 bytes, through a read-only handle.
func zimUUID(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var h [24]byte
	if _, err := io.ReadFull(f, h[:]); err != nil {
		return "", fmt.Errorf("kiwix: %s: read ZIM header: %w", path, err)
	}
	if binary.LittleEndian.Uint32(h[:4]) != zimMagic {
		return "", fmt.Errorf("kiwix: %s is not a ZIM file", path)
	}
	u := h[8:24]
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16]), nil
}

// ServedAs finds the book in feed (FetchServedFeed) that serves in's file,
// by the UUID in the file's header: Book is then exactly the name
// kiwix-serve gave that file. ok is false when the file cannot be read or
// kiwix-serve does not serve it (not started yet with it, or not a ZIM it
// could open).
func (m *Manager) ServedAs(in Installed, feed map[string]ServedBook) (ServedBook, bool) {
	if len(feed) == 0 {
		return ServedBook{}, false
	}
	p, err := m.FilePath(in)
	if err != nil {
		return ServedBook{}, false
	}
	uuid, err := zimUUID(p)
	if err != nil {
		return ServedBook{}, false
	}
	b, ok := feed[uuid]
	return b, ok
}

// RecordServed keeps what kiwix-serve reports about the books from library
// folders - each ZIM's own name, title and language (Server.OnServed) - in
// their rows, so the lists stay readable while a folder is out of reach.
// books maps installed row ids to the book kiwix-serve serves for them.
// Downloads keep the catalog's words, and an empty value never replaces a
// known one.
func (m *Manager) RecordServed(books map[string]ServedBook) error {
	tx, err := m.store.DB().Begin()
	if err != nil {
		return fmt.Errorf("kiwix: record served books: %w", err)
	}
	defer tx.Rollback()
	for id, b := range books {
		if _, err := tx.Exec(`
UPDATE installed SET
	name     = CASE WHEN ? = '' THEN name ELSE ? END,
	title    = CASE WHEN ? = '' THEN title ELSE ? END,
	language = CASE WHEN ? = '' THEN language ELSE ? END
WHERE id = ? AND folder != ''`,
			b.Name, b.Name, b.Title, b.Title, b.Language, b.Language, id); err != nil {
			return fmt.Errorf("kiwix: record served books: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("kiwix: record served books: %w", err)
	}
	return nil
}
