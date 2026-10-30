// The libraries area of the kid side: the kid-facing /api/libraries list
// and the /libraries page on the app origin, plus the reverse proxy that
// exposes the running kiwix-serve under /kiwix/ - which lives only on the
// separate libraries origin (web.Server.LibrariesMux), because ZIM pages
// are third-party HTML and JavaScript and must never run where the app's
// APIs live. Libraries are free (never budgeted) and need no kid unlock,
// exactly like the picker tile that already links to /libraries. The API
// and page are registered from init like every other area; the proxy and
// the libraries origin's own kid pages (the way home, the "not in the
// library" page for outside links, the "not working" page) are wired by
// the libraries-origin mux.
package web

import (
	"bytes"
	"context"
	"html"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/edementhon/kids-shelf/internal/kiwix"
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
	// msgLibrariesNotHere is the 404 for a path the libraries origin does
	// not serve (API callers; a browser gets the kid page).
	msgLibrariesNotHere = "There is nothing here. Open Libraries from the home page."
)

// kiwixResponseTimeout bounds how long kiwix-serve may take to start
// answering one proxied request. A frozen backend then ends in the kid's
// "did not answer" page instead of a page that loads forever; the body of
// an answer that has started (a long video, say) is not cut off.
const kiwixResponseTimeout = 20 * time.Second

// kiwixProxyTransport carries the proxied /kiwix/ traffic to the loopback
// kiwix-serve, with the response-header bound above.
var kiwixProxyTransport http.RoundTripper = newKiwixTransport(kiwixResponseTimeout)

func newKiwixTransport(headerTimeout time.Duration) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = headerTimeout
	return t
}

// kiwixPageMax caps how much of a kiwix page the proxy reads to add the
// Kids Shelf bar or the outside-link guard; a larger page (none of the
// reader's pages are) passes through unchanged.
const kiwixPageMax = 8 << 20

// kiwixFeedTimeout bounds the read of the backend's own book feed (see
// kiwixFeed), so a slow or frozen backend costs at most a few seconds of
// one /api/libraries call and never blocks anything else.
const kiwixFeedTimeout = 3 * time.Second

// kiwixFeedClient talks to the loopback kiwix-serve, so it is a plain
// client: internal/kiwix's Catalog forbids private destinations and is
// wired for the download catalog, not for this back-and-forth.
var kiwixFeedClient = &http.Client{Timeout: kiwixFeedTimeout}

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

// appURL returns path on the app origin for the request's own host: the
// same host the browser used, on the app's public port. The port comes
// from the launcher's KS_PUBLIC_PORT (the libraries origin only exists
// under the launcher, which always sets it); without it the path stays
// relative.
func appURL(r *http.Request, path string) string {
	port, err := strconv.Atoi(os.Getenv("KS_PUBLIC_PORT"))
	if err != nil || port <= 0 || port > 65535 {
		return path
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort(hostnameOnly(r.Host), strconv.Itoa(port)) + path
}

// wantsPage reports whether the browser is opening a page (the tab or the
// reader's frame) rather than calling an API: such a request gets a kid
// page with a way home, never raw JSON. Fetch metadata says it outright
// where the browser sends it (secure contexts); on plain http over the
// LAN the Accept header of a navigation names text/html, which fetch()
// and image loads do not.
func wantsPage(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Dest") {
	case "document", "iframe", "frame":
		return true
	case "":
		return strings.Contains(r.Header.Get("Accept"), "text/html")
	}
	return false
}

// libraryPage is one kid message page on the libraries origin (see
// static/kid/libraries.page.html). BackHref always leads somewhere real;
// with BackIsHistory the button steps back in history first, when there
// is history to step back into.
type libraryPage struct {
	Heading       string
	Text          string
	BackLabel     string
	BackHref      string
	BackIsHistory bool
	Retry         bool
}

var libraryPageTmpl = template.Must(template.ParseFS(kidStatic, "static/kid/libraries.page.html"))

func writeLibraryPage(w http.ResponseWriter, code int, p libraryPage) {
	var b bytes.Buffer
	if err := libraryPageTmpl.Execute(&b, p); err != nil {
		log.Printf("libraries: kid page: %v", err)
		http.Error(w, p.Heading, code)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write(b.Bytes())
}

// writeLibrariesError answers a libraries-origin failure: the plain-words
// JSON error for API callers, and for a browser opening a page a kid page
// with a big "Back to Libraries" button and "Try again".
func writeLibrariesError(w http.ResponseWriter, r *http.Request, code int, msg string) {
	if !wantsPage(r) {
		writeError(w, code, msg)
		return
	}
	p := libraryPage{
		Heading:   "This library did not answer",
		Text:      "Try again, or go back to Libraries.",
		BackLabel: "Back to Libraries",
		BackHref:  appURL(r, "/libraries"),
		Retry:     true,
	}
	if code == http.StatusServiceUnavailable {
		p.Heading = "The libraries are not working right now"
		p.Text = "Try again in a little while. If it keeps happening, ask a grown-up."
	}
	writeLibraryPage(w, code, p)
}

// handleLibrariesHome sends the libraries origin's front door (/ and
// kiwix-serve's /kiwix/ welcome page, where kiwix's own home links lead)
// to the Libraries page on the app origin. It never asks kiwix-serve, so
// the way home works while the libraries are down.
func (s *Server) handleLibrariesHome(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, appURL(r, "/libraries"), http.StatusFound)
}

// handleLibrariesNotHere is the libraries origin's not-found: a kid page
// with the way back for a browser, the JSON error for anything else.
func (s *Server) handleLibrariesNotHere(w http.ResponseWriter, r *http.Request) {
	if !wantsPage(r) {
		writeError(w, http.StatusNotFound, msgLibrariesNotHere)
		return
	}
	writeLibraryPage(w, http.StatusNotFound, libraryPage{
		Heading:   "There is nothing here",
		Text:      "Let's go back to Libraries.",
		BackLabel: "Back to Libraries",
		BackHref:  appURL(r, "/libraries"),
	})
}

// handleKiwixExternal answers /kiwix/catch/external, where kiwix-serve's
// reader (--blockexternal) and the app's own link guard send every link
// that leaves the library. kiwix-serve's page would show the outside
// address as a live link; this one names nothing outside and offers one
// big way back to the book. The source query is deliberately never read.
func (s *Server) handleKiwixExternal(w http.ResponseWriter, r *http.Request) {
	writeLibraryPage(w, http.StatusOK, libraryPage{
		Heading:       "That page is not in the library",
		Text:          "It lives on the internet. Libraries only open their own pages.",
		BackLabel:     "Back to the book",
		BackHref:      appURL(r, "/libraries"),
		BackIsHistory: true,
	})
}

// libraryAssets are the files the proxy adds to kiwix-serve's pages,
// served from the libraries origin itself so the pages' own
// Content-Security-Policy ('self') lets them load.
var libraryAssets = map[string]string{
	"viewer.css": "static/kid/libraries.viewer.css",
	"content.js": "static/kid/libraries.content.js",
}

func (s *Server) handleLibraryAsset(w http.ResponseWriter, r *http.Request) {
	name, ok := libraryAssets[r.PathValue("file")]
	if !ok {
		s.handleLibrariesNotHere(w, r)
		return
	}
	http.ServeFileFS(w, r, kidStatic, name)
}

// handleKiwixProxy forwards /kiwix/* to the running kiwix-serve through the
// libraries-origin mux, so the Host allowlist and the same-site middleware
// wrap the proxied traffic there exactly like app traffic. kiwix-serve runs
// with --urlRootLocation=/kiwix, so the incoming path and query are passed
// unchanged and the backend answers /kiwix/viewer, /kiwix/content/<book>,
// /kiwix/search and /kiwix/catalog/... out of the box. The reader page gets
// the Kids Shelf bar and book pages get the outside-link guard (see
// decorateKiwixPage).
func (s *Server) handleKiwixProxy(w http.ResponseWriter, r *http.Request) {
	if s.deps.KiwixServer == nil {
		writeLibrariesError(w, r, http.StatusServiceUnavailable, msgLibrariesDown)
		return
	}
	base := s.deps.KiwixServer.Backend()
	if base == "" {
		writeLibrariesError(w, r, http.StatusServiceUnavailable, msgLibrariesDown)
		return
	}
	target, err := url.Parse(base)
	if err != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") {
		log.Printf("libraries: backend URL %q is not a usable http URL", base)
		writeLibrariesError(w, r, http.StatusBadGateway, msgLibrariesUnreachable)
		return
	}
	// A HEAD answer has no body to add to, so it passes through as is.
	decorate := r.Method == http.MethodGet && kiwixPageKind(r.URL.Path) != kiwixPagePlain
	proxy := &httputil.ReverseProxy{
		Transport: kiwixProxyTransport,
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
			if decorate {
				// A page the proxy adds to must arrive uncompressed.
				pr.Out.Header.Set("Accept-Encoding", "identity")
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			if !decorate {
				return nil
			}
			return decorateKiwixPage(resp, r)
		},
		ErrorHandler: func(w http.ResponseWriter, pr *http.Request, err error) {
			log.Printf("libraries: kiwix proxy: %v", err)
			writeLibrariesError(w, r, http.StatusBadGateway, msgLibrariesUnreachable)
		},
	}
	proxy.ServeHTTP(w, r)
}

// Which kiwix-serve pages the proxy adds to.
const (
	kiwixPagePlain  = iota // passed through unchanged
	kiwixPageReader        // /kiwix/viewer: gets the Kids Shelf bar
	kiwixPageBook          // /kiwix/content/...: gets the outside-link guard
)

func kiwixPageKind(path string) int {
	switch {
	case path == "/kiwix/viewer":
		return kiwixPageReader
	case strings.HasPrefix(path, "/kiwix/content/"):
		return kiwixPageBook
	}
	return kiwixPagePlain
}

// Inline pictures for the reader bar's buttons (a house and a book), so
// the bar needs no emoji font and no extra request.
const (
	readerBarHouse = `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3 2 11.5h3V21h5.5v-6h3v6H19v-9.5h3z" fill="#062"/></svg>`
	readerBarBooks = `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M3 5c3-1 6-1 9 1v14c-3-2-6-2-9-1zM21 5c-3-1-6-1-9 1v14c3-2 6-2 9-1z" fill="#2a7a4b"/></svg>`
)

// decorateKiwixPage adds the app's pieces to a kiwix-serve HTML page:
//   - the reader (/kiwix/viewer) gets the Kids Shelf bar, a big "Home"
//     and "Libraries" pair always on top of the book at every size, and
//     the stylesheet that lays it out and hides kiwix's small house
//     button and its language pop-up;
//   - every book page (/kiwix/content/...) gets the guard script that
//     points each outside link at the "not in the library" page and moves
//     a book page opened on its own into the reader.
//
// Anything else (an error status, a non-HTML file, a compressed or
// oversized body, a page without the expected tag) passes through as is.
// The changed page drops its validators: the browser must never pair a
// kiwix ETag with a body kiwix did not send.
func decorateKiwixPage(resp *http.Response, r *http.Request) error {
	kind := kiwixPageKind(r.URL.Path)
	if kind == kiwixPagePlain || resp.StatusCode != http.StatusOK ||
		!strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") ||
		resp.Header.Get("Content-Encoding") != "" {
		return nil
	}
	page, err := io.ReadAll(io.LimitReader(resp.Body, kiwixPageMax+1))
	if err != nil {
		return err
	}
	if len(page) > kiwixPageMax {
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(page), resp.Body), resp.Body}
		return nil
	}
	resp.Body.Close()
	// kiwix-serve and the ZIM scrapers write their tags in lower case.
	var out []byte
	if head := bytes.Index(page, []byte("</head>")); head >= 0 {
		if kind == kiwixPageReader {
			if body := openTagEnd(page[head:], "<body"); body >= 0 {
				bar := `<nav class="ks-bar" aria-label="Kids Shelf">` +
					`<a class="ks-btn" href="` + html.EscapeString(appURL(r, "/")) + `">` + readerBarHouse + `Home</a>` +
					`<a class="ks-btn ks-two" href="` + html.EscapeString(appURL(r, "/libraries")) + `">` + readerBarBooks + `Libraries</a>` +
					`</nav>`
				out = splice(page,
					insertion{head, `<link rel="stylesheet" href="/ks/viewer.css">`},
					insertion{head + body, bar})
			}
		} else {
			out = splice(page, insertion{head, `<script src="/ks/content.js"></script>`})
		}
	}
	if out == nil {
		out = page
	} else {
		resp.Header.Del("Etag")
		resp.Header.Del("Last-Modified")
	}
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
	return nil
}

// insertion is one snippet to add to a page at a byte offset.
type insertion struct {
	at   int
	text string
}

// splice returns page with each insertion added at its offset, in one
// allocation; offsets must be ascending.
func splice(page []byte, ins ...insertion) []byte {
	n := len(page)
	for _, in := range ins {
		n += len(in.text)
	}
	out := make([]byte, 0, n)
	prev := 0
	for _, in := range ins {
		out = append(out, page[prev:in.at]...)
		out = append(out, in.text...)
		prev = in.at
	}
	return append(out, page[prev:]...)
}

// openTagEnd returns the offset right after the first opening tag that
// starts with open (attributes and all), or -1 when there is none.
func openTagEnd(page []byte, open string) int {
	i := bytes.Index(page, []byte(open))
	if i < 0 {
		return -1
	}
	end := bytes.IndexByte(page[i:], '>')
	if end < 0 {
		return -1
	}
	return i + end + 1
}

// libraryBook is one installed library as the kid page reads it, a
// download and a library folder book alike. Name is the file name without
// .zim, a label of last resort. URL and Icon are filled only while the
// backend is up and serves the book's file; otherwise the tile still
// renders (title and language), just without anything to open. Missing
// says the file itself is gone (a drive that is not plugged in, a NAS that
// is off), which no waiting fixes: the page asks for a grown-up.
type libraryBook struct {
	Name     string `json:"name"`
	Title    string `json:"title"`
	Language string `json:"language"`
	URL      string `json:"url"`
	Icon     string `json:"icon"`
	Missing  bool   `json:"missing"`
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
	var feed map[string]kiwix.ServedBook
	if out.Available {
		// A backend that does not answer its own feed (frozen, say) is
		// not available, whatever the supervisor last saw.
		feed, out.Available = s.kiwixFeed(r.Context())
	}
	for _, in := range installed {
		b := libraryBook{
			Name:     in.FileBase(),
			Title:    in.Title,
			Language: in.Language,
			Missing:  !s.deps.Kiwix.OnDisk(in),
		}
		if out.Available && !b.Missing {
			// The book kiwix-serve serves for this file, found by the
			// UUID in the file's header: its name is exactly the one
			// kiwix-serve gave the file (lower case, no accents, spaces
			// as underscores), escaped like kiwix-serve's own links.
			if sb, ok := s.deps.Kiwix.ServedAs(in, feed); ok {
				b.URL = "/kiwix/viewer#" + sb.URLName()
				b.Icon = sb.Icon
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

// kiwixFeed reads the books the backend serves from its own OPDS feed (see
// kiwix.FetchServedFeed). ok is false when it gave no usable answer
// (frozen, gone, or a feed that does not read): with no feed no book can
// be linked, so the caller reports the libraries as not available.
func (s *Server) kiwixFeed(ctx context.Context) (feed map[string]kiwix.ServedBook, ok bool) {
	feed, err := kiwix.FetchServedFeed(ctx, kiwixFeedClient, s.deps.KiwixServer.Backend())
	if err != nil {
		log.Printf("libraries: %v", err)
		return nil, false
	}
	return feed, true
}

// handleLibrariesPage serves the kid page that lists the installed
// libraries; all data arrives from /api/libraries, nothing here is dynamic.
func (s *Server) handleLibrariesPage(w http.ResponseWriter, r *http.Request) {
	http.ServeFileFS(w, r, kidStatic, "static/kid/libraries.html")
}
