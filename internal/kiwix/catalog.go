// Package kiwix is the Kiwix side of the library: a client for the library
// catalog OPDS feeds, downloads of ZIM files through their .meta4 Metalink
// files over the shared job queue, and the table of installed ZIMs. Serving
// ZIM content is not here; the storage backend is still being decided (D4).
package kiwix

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultCatalogURL is the entry point of the Kiwix OPDS catalog. Entries
// 301 to https://opds.library.kiwix.org, which the client follows.
const DefaultCatalogURL = "https://library.kiwix.org"

// Entry is one ZIM listed in the catalog's OPDS entries feed. Size is the
// byte length the acquisition link declares; the authoritative size and
// checksum come from the .meta4 file when the download starts.
type Entry struct {
	ID              string    `json:"id"`               // urn:uuid id
	Name            string    `json:"name"`             // ZIM base name, e.g. "wikipedia_en_100"
	Title           string    `json:"title"`            // display title
	Summary         string    `json:"summary"`          // plain-text description
	Language        string    `json:"language"`         // comma-separated language codes, e.g. "eng,jpn"
	Category        string    `json:"category"`         // category slug, "" when the feed has none
	Flavour         string    `json:"flavour"`          // e.g. "maxi", "nopic"
	Tags            string    `json:"tags"`             // raw semicolon-separated tag list
	ArticleCount    int       `json:"article_count"`    // number of articles the ZIM index declares
	MediaCount      int       `json:"media_count"`      // number of media files the ZIM declares
	Updated         time.Time `json:"updated"`          // the feed's update time for this entry
	Size            int64     `json:"size"`             // byte length from the acquisition link
	Meta4URL        string    `json:"meta4_url"`        // absolute URL of the .meta4 Metalink file
	IllustrationURL string    `json:"illustration_url"` // absolute URL of the thumbnail illustration
}

// Result is one page of a catalog entries query, with the feed-wide paging
// numbers alongside the entries.
type Result struct {
	Total   int // total results the feed declares
	Start   int // index of the first returned entry
	Count   int // entries per page
	Entries []Entry
}

// Category is one entry of the /catalog/v2/categories feed.
type Category struct {
	Name string // category slug, usable as Query.Category
}

// Language is one entry of the /catalog/v2/languages feed.
type Language struct {
	Code  string // ISO 639-3 code, usable as Query.Lang (e.g. "eng")
	Name  string // display name, e.g. "English"
	Count int    // how many entries use this language
}

// Query selects entries from the catalog. Empty fields are omitted from the
// request; Start is the 0-based page offset and Count the page size.
type Query struct {
	Lang     string // ISO 639-3 code
	Category string // category slug
	Q        string // free-text search
	Name     string // exact ZIM base name filter (the k:name extension parameter)
	Start    int
	Count    int
}

// Catalog is a client for the Kiwix OPDS catalog. baseURL is configurable
// (DefaultCatalogURL when empty); all endpoints are resolved below it.
type Catalog struct {
	base string
	http *http.Client
}

// NewCatalog returns a Catalog on baseURL, or DefaultCatalogURL when baseURL
// is empty. A nil client gets a fresh one with a 30 second time-out, which is
// plenty for the small catalog feeds; download traffic uses its own client.
// The client - injected or fresh - is wrapped with the outbound destination
// policy and the maxRedirects redirect cap; tests serving the catalog from
// an httptest server pass AllowLoopback(hc) to keep 127.0.0.1 reachable.
func NewCatalog(baseURL string, hc *http.Client) *Catalog {
	base := DefaultCatalogURL
	if baseURL != "" {
		base = strings.TrimRight(baseURL, "/")
	}
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Catalog{base: base, http: restrictClient(hc, false)}
}

// Entries fetches one page of the catalog entries feed for q.
func (c *Catalog) Entries(ctx context.Context, q Query) (*Result, error) {
	u := c.base + "/catalog/v2/entries"
	if v := q.urlValues(); len(v) > 0 {
		u += "?" + v.Encode()
	}
	resp, err := c.get(ctx, u)
	if err != nil {
		return nil, fmt.Errorf("kiwix: catalog entries: %w", err)
	}
	defer resp.Body.Close()
	// abs is the URL the feed actually came from (after redirects); relative
	// link hrefs inside the feed resolve against it.
	feed, err := parseEntries(resp.Body, resp.Request.URL)
	if err != nil {
		return nil, fmt.Errorf("kiwix: catalog entries: %w", err)
	}
	return feed, nil
}

// ErrEntryNotFound is returned by Lookup when no catalog entry matches the
// requested identity.
var ErrEntryNotFound = errors.New("kiwix: catalog entry not found")

// Lookup returns the catalog entry identified by the requested identity: the
// first entry whose id matches id, or whose name and flavour both match when
// id is empty. The OPDS endpoint has no id lookup, so the search narrows the
// entries feed with the exact name filter the catalog exposes (the k:name
// parameter of its OpenSearch description) and matches what comes back, which
// keeps a name lookup at one feed page. When no name is given, the whole
// catalog is paged until the id matches or the lookup page bound is hit.
func (c *Catalog) Lookup(ctx context.Context, id, name, flavour string) (*Entry, error) {
	q := Query{Count: lookupPageSize, Name: name}
	var nameMatch *Entry
	for page := range lookupMaxPages {
		q.Start = page * lookupPageSize
		res, err := c.Entries(ctx, q)
		if err != nil {
			return nil, err
		}
		for i := range res.Entries {
			e := &res.Entries[i]
			if id != "" && e.ID == id {
				return e, nil
			}
			if id == "" && name != "" && e.Name == name && e.Flavour == flavour && nameMatch == nil {
				nameMatch = e
			}
		}
		if len(res.Entries) == 0 || q.Start+len(res.Entries) >= res.Total {
			break
		}
	}
	if nameMatch != nil {
		return nameMatch, nil
	}
	return nil, ErrEntryNotFound
}

// lookupPageSize and lookupMaxPages bound how far Lookup pages through the
// entries feed: 50 pages of 100 entries, over the whole catalog's size, so an
// unfiltered id scan always completes within the bound.
const (
	lookupPageSize = 100
	lookupMaxPages = 50
)

// Categories fetches the catalog's category list.
func (c *Catalog) Categories(ctx context.Context) ([]Category, error) {
	u := c.base + "/catalog/v2/categories"
	resp, err := c.get(ctx, u)
	if err != nil {
		return nil, fmt.Errorf("kiwix: catalog categories: %w", err)
	}
	defer resp.Body.Close()
	feed, err := parseCategories(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("kiwix: catalog categories: %w", err)
	}
	return feed, nil
}

// Languages fetches the catalog's language list.
func (c *Catalog) Languages(ctx context.Context) ([]Language, error) {
	u := c.base + "/catalog/v2/languages"
	resp, err := c.get(ctx, u)
	if err != nil {
		return nil, fmt.Errorf("kiwix: catalog languages: %w", err)
	}
	defer resp.Body.Close()
	feed, err := parseLanguages(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("kiwix: catalog languages: %w", err)
	}
	return feed, nil
}

// get issues a GET and requires a 200 response. Only absolute http(s) URLs
// are accepted; the transport's dial policy refuses private destinations.
func (c *Catalog) get(ctx context.Context, u string) (*http.Response, error) {
	if err := checkURLScheme(u); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: HTTP %s", u, resp.Status)
	}
	return resp, nil
}

// urlValues builds the entries query string.
func (q Query) urlValues() url.Values {
	v := url.Values{}
	if q.Lang != "" {
		v.Set("lang", q.Lang)
	}
	if q.Category != "" {
		v.Set("category", q.Category)
	}
	if q.Q != "" {
		v.Set("q", q.Q)
	}
	if q.Name != "" {
		v.Set("name", q.Name)
	}
	if q.Start > 0 {
		v.Set("start", strconv.Itoa(q.Start))
	}
	if q.Count > 0 {
		v.Set("count", strconv.Itoa(q.Count))
	}
	return v
}

// The catalog serves Atom feeds whose default namespace is Atom; the
// languages feed puts its code and count in dc: and thr: elements. Field
// tags stay unqualified: encoding/xml matches on the local name, so the
// namespace of an element (default or prefixed) does not matter, and the
// different endpoints get separate element structs so no two fields of one
// struct can claim the same local name.
type atomLink struct {
	Rel    string `xml:"rel,attr"`
	Type   string `xml:"type,attr"`
	Href   string `xml:"href,attr"`
	Length int64  `xml:"length,attr"`
}

// Link rel values identifying the acquisition and illustration links.
const (
	relAcquisition = "http://opds-spec.org/acquisition/open-access"
	relThumbnail   = "http://opds-spec.org/image/thumbnail"
)

// entriesFeed is the element tree of /catalog/v2/entries.
type entriesFeed struct {
	XMLName      xml.Name       `xml:"feed"`
	Total        int            `xml:"totalResults"`
	StartIndex   int            `xml:"startIndex"`
	ItemsPerPage int            `xml:"itemsPerPage"`
	Entries      []entriesEntry `xml:"entry"`
}

type entriesEntry struct {
	ID           string     `xml:"id"`
	Title        string     `xml:"title"`
	Updated      time.Time  `xml:"updated"`
	Summary      string     `xml:"summary"`
	Language     string     `xml:"language"`
	Name         string     `xml:"name"`
	Flavour      string     `xml:"flavour"`
	Category     string     `xml:"category"`
	Tags         string     `xml:"tags"`
	ArticleCount int        `xml:"articleCount"`
	MediaCount   int        `xml:"mediaCount"`
	Links        []atomLink `xml:"link"`
}

// parseEntries parses a /catalog/v2/entries feed. abs is the URL the feed
// was served from; relative link hrefs (the illustration, for one) resolve
// against it so callers get absolute URLs.
func parseEntries(r io.Reader, abs *url.URL) (*Result, error) {
	var feed entriesFeed
	if err := xml.NewDecoder(r).Decode(&feed); err != nil {
		return nil, fmt.Errorf("parse entries feed: %w", err)
	}
	res := &Result{Total: feed.Total, Start: feed.StartIndex, Count: feed.ItemsPerPage}
	for _, e := range feed.Entries {
		entry := Entry{
			ID:           e.ID,
			Name:         e.Name,
			Title:        e.Title,
			Summary:      e.Summary,
			Language:     e.Language,
			Category:     e.Category,
			Flavour:      e.Flavour,
			Tags:         e.Tags,
			ArticleCount: e.ArticleCount,
			MediaCount:   e.MediaCount,
			Updated:      e.Updated,
		}
		for _, l := range e.Links {
			switch l.Rel {
			case relAcquisition:
				entry.Meta4URL = l.Href
				entry.Size = l.Length
			case relThumbnail:
				entry.IllustrationURL = resolveURL(abs, l.Href)
			}
		}
		res.Entries = append(res.Entries, entry)
	}
	return res, nil
}

// resolveURL resolves ref against base, keeping absolute refs unchanged.
func resolveURL(base *url.URL, ref string) string {
	if base == nil {
		return ref
	}
	u, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return base.ResolveReference(u).String()
}

// categoriesEntry is one element tree node of /catalog/v2/categories.
type categoriesEntry struct {
	Title string     `xml:"title"`
	Links []atomLink `xml:"link"`
}

// parseCategories parses a /catalog/v2/categories feed into its category
// slugs (each entry's title).
func parseCategories(r io.Reader) ([]Category, error) {
	var feed struct {
		XMLName xml.Name          `xml:"feed"`
		Entries []categoriesEntry `xml:"entry"`
	}
	if err := xml.NewDecoder(r).Decode(&feed); err != nil {
		return nil, fmt.Errorf("parse categories feed: %w", err)
	}
	cs := make([]Category, 0, len(feed.Entries))
	for _, e := range feed.Entries {
		cs = append(cs, Category{Name: e.Title})
	}
	return cs, nil
}

// languagesEntry is one element tree node of /catalog/v2/languages: the
// language code lives in dc:language, the per-language entry count in
// thr:count, both matched here by local name.
type languagesEntry struct {
	Title string `xml:"title"`
	Code  string `xml:"language"`
	Count int    `xml:"count"`
}

// parseLanguages parses a /catalog/v2/languages feed.
func parseLanguages(r io.Reader) ([]Language, error) {
	var feed struct {
		XMLName xml.Name         `xml:"feed"`
		Entries []languagesEntry `xml:"entry"`
	}
	if err := xml.NewDecoder(r).Decode(&feed); err != nil {
		return nil, fmt.Errorf("parse languages feed: %w", err)
	}
	ls := make([]Language, 0, len(feed.Entries))
	for _, e := range feed.Entries {
		ls = append(ls, Language{Code: e.Code, Name: e.Title, Count: e.Count})
	}
	return ls, nil
}
