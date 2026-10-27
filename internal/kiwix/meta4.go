package kiwix

import (
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Meta4 is a parsed .meta4 Metalink file (RFC 5854): the ZIM's size, its
// sha-256 and the mirror URLs in the order a download should try them.
type Meta4 struct {
	// File is the ZIM file name the metalink declares, e.g.
	// "wikipedia_en_100_2026-08.zim". It is only used in messages and the
	// resume bookkeeping: downloaded files are stored under the
	// catalog-generated name (zimFileName), so this field can never be a
	// path.
	File   string
	Size   int64
	SHA256 string
	// Mirrors lists the download URLs, sorted by metalink priority (ascending)
	// and, within a priority, in document order.
	Mirrors []Mirror
}

// Mirror is one download URL of a metalink file, with its declared priority.
type Mirror struct {
	URL      string
	Priority int
	Location string // country code from the metalink, informational only
}

// metalinkDoc is the XML element tree of a .meta4 file. Tags are
// unqualified; the metalink namespace (urn:ietf:params:xml:ns:metalink) is
// matched by local name, like the catalog feeds.
type metalinkDoc struct {
	XMLName xml.Name       `xml:"metalink"`
	Files   []metalinkFile `xml:"file"`
}

type metalinkFile struct {
	Name   string         `xml:"name,attr"`
	Size   int64          `xml:"size"`
	Hashes []metalinkHash `xml:"hash"`
	URLs   []metalinkURL  `xml:"url"`
}

type metalinkHash struct {
	Type  string `xml:"type,attr"`
	Value string `xml:",chardata"`
}

type metalinkURL struct {
	URL      string `xml:",chardata"`
	Priority int    `xml:"priority,attr"`
	Location string `xml:"location,attr"`
}

// parseMeta4 parses one .meta4 Metalink document.
func parseMeta4(r io.Reader) (*Meta4, error) {
	var doc metalinkDoc
	if err := xml.NewDecoder(r).Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse meta4: %w", err)
	}
	if len(doc.Files) == 0 {
		return nil, fmt.Errorf("parse meta4: no <file> element")
	}
	f := doc.Files[0]
	m := &Meta4{File: f.Name, Size: f.Size}
	for _, h := range f.Hashes {
		if h.Type == "sha-256" {
			m.SHA256 = strings.ToLower(strings.TrimSpace(h.Value))
		}
	}
	if m.SHA256 == "" {
		return nil, fmt.Errorf("parse meta4: no sha-256 hash for %s", f.Name)
	}
	if len(f.URLs) == 0 {
		return nil, fmt.Errorf("parse meta4: no mirror <url> for %s", f.Name)
	}
	for _, u := range f.URLs {
		m.Mirrors = append(m.Mirrors, Mirror{URL: u.URL, Priority: u.Priority, Location: u.Location})
	}
	sort.SliceStable(m.Mirrors, func(i, j int) bool {
		return m.Mirrors[i].Priority < m.Mirrors[j].Priority
	})
	return m, nil
}
