// The libraries area of the kid side: the kid-facing /api/libraries list
// and the /libraries page on the app origin, plus the reverse proxy that
// exposes the running kiwix-serve under /kiwix/ - which lives only on the
// separate libraries origin (web.Server.LibrariesMux), because ZIM pages
// are third-party HTML and JavaScript and must never run where the app's
// APIs live. Libraries are free (never budgeted) and need no kid unlock,
// exactly like the picker tile that already links to /libraries. The API
// and page are registered from init like every other area; the proxy is
// wired by the libraries-origin mux.
package web

import (
	"context"
	"encoding/xml"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Plain-words library messages; internal details go to the log, never to a
// page (the writeError contract).
const (
	// msgLibrariesDown is the 503 for "no kiwix-serve backend in this copy"
	// (nil or not yet up) and for a copy without the installed-libraries
	// service at all.
	msgLibrariesDown = "The libraries aren't available right now. Try again in a moment."
	// msgLibrariesUnreachable is the 502 when the backend is up but refused
	// a proxied request.
	msgLibrariesUnreachable = "A library didn't answer. Go back and try again."
	// msgLibrariesNotRead is the 500 when the installed table cannot be read;
	// the error text itself stays in the log.
	msgLibrariesNotRead = "The library list couldn't be read. Try again in a moment."
	// msgKiwixNotOnAppOrigin is the 404 for /kiwix/ on the app origin: ZIM
	// content runs only on the separate libraries origin (PLAN 1a), so
	// there is deliberately nothing to find here.
	msgKiwixNotOnAppOrigin = "Libraries aren't served on this address. Open Libraries from the home page."
)

// kiwixIconTimeout bounds the OPDS icon lookup. The icon is a nice-to-have
// on the library tiles, so a slow feed costs at most a few seconds of one
// /api/libraries call and never blocks anything else.
const kiwixIconTimeout = 3 * time.Second

// kiwixIconFeedMax caps the OPDS entries feed body (the feed and its
// truncation point; a longer body just fails the parse and costs the icons).
const kiwixIconFeedMax = 8 << 20

// kiwixIconClient talks to the loopback kiwix-serve, so it is a plain
// client: internal/kiwix's Catalog forbids private destinations and is
// wired for the download catalog, not for this back-and-forth.
var kiwixIconClient = &http.Client{Timeout: kiwixIconTimeout}

func init() {
	register(func(s *Server) { librariesRoutes(s) })
}

func librariesRoutes(s *Server) {
	// "GET" also matches HEAD; every other method is refused by the mux
	// with 405 before the handler runs. The exact /libraries pattern beats
	// kidRoutes' "GET /" catch-all, so the kid page no longer falls through
	// to the home page. /kiwix/ lives only on the libraries origin
	// (Server.LibrariesMux); on the app origin it is an explicit 404 so the
	// kid catch-all can never answer it with the home page.
	s.mux.HandleFunc("GET /api/libraries", s.handleAPILibraries)
	s.mux.HandleFunc("GET /libraries", s.handleLibrariesPage)
	s.mux.HandleFunc("GET /kiwix", s.handleKiwixNotFound)
	s.mux.HandleFunc("GET /kiwix/", s.handleKiwixNotFound)
}

// handleKiwixNotFound answers 404 on the app origin for /kiwix/: ZIM
// content is third-party HTML and JavaScript and must never run where the
// app's APIs live - only the separate libraries origin (Server.LibrariesMux)
// serves it. Without this explicit not-found, the kid catch-all "GET /"
// would answer /kiwix/ paths with the home page.
func (s *Server) handleKiwixNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, msgKiwixNotOnAppOrigin)
}

// handleKiwixProxy forwards /kiwix/* to the running kiwix-serve through the
// libraries-origin mux, so the Host allowlist and the same-site middleware
// wrap the proxied traffic there exactly like app traffic. kiwix-serve runs
// with --urlRootLocation=/kiwix, so the incoming path and query are passed
// unchanged and the backend answers /kiwix/viewer, /kiwix/content/<book>,
// /kiwix/search and /kiwix/catalog/... out of the box.
func (s *Server) handleKiwixProxy(w http.ResponseWriter, r *http.Request) {
	if s.deps.KiwixServer == nil {
		writeError(w, http.StatusServiceUnavailable, msgLibrariesDown)
		return
	}
	base := s.deps.KiwixServer.Backend()
	if base == "" {
		writeError(w, http.StatusServiceUnavailable, msgLibrariesDown)
		return
	}
	target, err := url.Parse(base)
	if err != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") {
		log.Printf("libraries: backend URL %q is not a usable http URL", base)
		writeError(w, http.StatusBadGateway, msgLibrariesUnreachable)
		return
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// The Rewrite path never touches X-Forwarded-* itself, so a
			// browser-sent X-Forwarded-For/Host/Proto would otherwise pass
			// through; kiwix-serve must never see a client-chosen forwarding
			// header, and we deliberately do not add fresh ones (a
			// X-Forwarded-Host would also break the Host-based links in
			// kiwix-serve's pages). SetURL has already replaced Out.Host
			// with the backend's own host.
			pr.Out.Header.Del("X-Forwarded-For")
			pr.Out.Header.Del("X-Forwarded-Host")
			pr.Out.Header.Del("X-Forwarded-Proto")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("libraries: kiwix proxy: %v", err)
			writeError(w, http.StatusBadGateway, msgLibrariesUnreachable)
		},
	}
	proxy.ServeHTTP(w, r)
}

// libraryBook is one installed library as the kid page reads it. URL and
// Icon are filled only while the backend is up; with the backend down the
// tile still renders (title and language), just without anything to open.
type libraryBook struct {
	Name     string `json:"name"`
	Title    string `json:"title"`
	Language string `json:"language"`
	URL      string `json:"url"`
	Icon     string `json:"icon"`
}

func (s *Server) handleAPILibraries(w http.ResponseWriter, r *http.Request) {
	libOrigin := s.librariesOrigin(r)
	out := struct {
		Available    bool          `json:"available"`
		LibrariesURL string        `json:"libraries_url"`
		Books        []libraryBook `json:"books"`
	}{Available: s.kiwixUp(), LibrariesURL: libOrigin, Books: []libraryBook{}}
	if s.deps.Kiwix == nil {
		writeError(w, http.StatusServiceUnavailable, msgLibrariesDown)
		return
	}
	installed, err := s.deps.Kiwix.Installed()
	if err != nil {
		log.Printf("libraries: installed list: %v", err)
		writeError(w, http.StatusInternalServerError, msgLibrariesNotRead)
		return
	}
	icons := map[string]string{}
	if out.Available {
		icons = s.kiwixIcons(r.Context())
	}
	for _, in := range installed {
		b := libraryBook{
			Name:     kiwixBookName(in.File),
			Title:    in.Title,
			Language: in.Language,
		}
		if out.Available {
			b.URL = "/kiwix/viewer#" + b.Name
			// The feed names books by their ZIM metadata name, which is the
			// catalog name we recorded at download time; some ZIMs carry
			// the dated file base instead, so that name matches too.
			if icon, ok := icons[in.Name]; ok {
				b.Icon = icon
			} else {
				b.Icon = icons[b.Name]
			}
		}
		out.Books = append(out.Books, b)
	}
	writeJSON(w, http.StatusOK, out)
}

// librariesOrigin returns the full base URL of the libraries origin for the
// request's own host: the same host the browser used, on the public
// libraries port, so book links opened from the app origin land on the
// libraries mux. Empty when this copy serves no separate origin.
func (s *Server) librariesOrigin(r *http.Request) string {
	if s.deps.LibrariesPort == 0 {
		return ""
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort(hostnameOnly(r.Host), strconv.Itoa(s.deps.LibrariesPort))
}

// kiwixUp reports whether the kiwix-serve backend is present and ready.
func (s *Server) kiwixUp() bool {
	return s.deps.KiwixServer != nil && s.deps.KiwixServer.Backend() != ""
}

// kiwixBookName is the book identifier the backend addresses a ZIM by: the
// stored file name without the .zim extension (the "book name" of the
// kiwix-serve start command contract). Stored file names are sanitized to
// [A-Za-z0-9._-], so the name is safe to splice into the /viewer# fragment
// and the /content/ path unescaped.
func kiwixBookName(file string) string {
	return strings.TrimSuffix(filepath.Base(file), ".zim")
}

// kiwixIcons maps each installed book's catalog name to its illustration URL
// by reading the backend's own OPDS entries feed (GET /kiwix/catalog/v2/
// entries?count=-1 returns every book in one feed; count=-1 is honored by
// kiwix-serve 3.8, which uses it as "all"). The lookup is best-effort: a
// slow, broken or oversized feed costs the icons, never the library list.
func (s *Server) kiwixIcons(ctx context.Context) map[string]string {
	backend, err := url.Parse(s.deps.KiwixServer.Backend())
	if err != nil || backend.Host == "" {
		return nil
	}
	feedURL := backend.String() + "/kiwix/catalog/v2/entries?count=-1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL, nil)
	if err != nil {
		return nil
	}
	resp, err := kiwixIconClient.Do(req)
	if err != nil {
		log.Printf("libraries: icon feed: %v", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("libraries: icon feed: HTTP %s", resp.Status)
		return nil
	}
	var feed kiwixOPDSFeed
	if err := xml.NewDecoder(io.LimitReader(resp.Body, kiwixIconFeedMax)).Decode(&feed); err != nil {
		log.Printf("libraries: icon feed: %v", err)
		return nil
	}
	icons := make(map[string]string, len(feed.Entries))
	for _, e := range feed.Entries {
		uuid := strings.TrimPrefix(e.ID, "urn:uuid:")
		if uuid == "" || uuid == e.ID {
			continue
		}
		icons[e.Name] = "/kiwix/catalog/v2/illustration/" + uuid + "/?size=48"
	}
	return icons
}

// kiwixOPDSFeed is the minimal shape of the backend's /catalog/v2/entries
// feed: only the id (urn:uuid:<book-id>, the same id the illustration
// endpoint takes) and the name (the ZIM metadata name, which is what the
// app recorded at download time) are needed for the lookup. Unqualified
// tags match through the Atom default namespace, exactly as
// internal/kiwix's own OPDS parsing relies on.
type kiwixOPDSFeed struct {
	Entries []kiwixOPDSEntry `xml:"entry"`
}

type kiwixOPDSEntry struct {
	ID   string `xml:"id"`
	Name string `xml:"name"`
}

// handleLibrariesPage serves the kid page that lists the installed
// libraries; all data arrives from /api/libraries, nothing here is dynamic.
func (s *Server) handleLibrariesPage(w http.ResponseWriter, r *http.Request) {
	http.ServeFileFS(w, r, kidStatic, "static/kid/libraries.html")
}
