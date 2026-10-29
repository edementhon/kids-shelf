// Package web serves the kids-shelf HTTP interface: /healthz (the
// single-instance health check used by the launcher) plus the kid, practice,
// library and parent-dashboard routes. Each area lives in its own file and
// adds its routes through register, so areas never edit a shared file.
//
// Every request passes the security middleware (see Mux): a Host allowlist
// that refuses DNS-rebinding attacks, and a same-site check that refuses
// state changing requests without a matching Origin or Referer.
package web

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/edementhon/kids-shelf/internal/budget"
	"github.com/edementhon/kids-shelf/internal/ingest"
	"github.com/edementhon/kids-shelf/internal/kiwix"
	"github.com/edementhon/kids-shelf/internal/library"
	"github.com/edementhon/kids-shelf/internal/practice"
	"github.com/edementhon/kids-shelf/internal/update"
	"github.com/edementhon/kids-shelf/internal/youtube"
)

// Version is reported by /healthz. cmd/kids-shelf sets it from the ldflags
// value of main.version; it defaults to "dev".
var Version = "dev"

// UpdateControl is the updater surface the dashboard shows: current state
// in plain words and the "Restart to update" action.
type UpdateControl interface {
	Status() update.Status
	RestartToUpdate()
}

// KiwixBackend is the running kiwix-serve the /kiwix/ routes proxy to.
// Backend returns its loopback base URL ("http://127.0.0.1:<port>", no
// trailing slash), or "" while it is not running: no ZIM installed, no
// kiwix-serve binary, or a restart in progress.
type KiwixBackend interface {
	Backend() string
}

// FirewallControl reports and fixes whether other devices on the home
// network can reach this computer through its firewall. Only Windows has
// one (Windows Defender Firewall); elsewhere Deps.Firewall is nil.
type FirewallControl interface {
	// Status reports whether inbound connections to the program that
	// listens on the LAN are allowed on the active network profile.
	Status(ctx context.Context) FirewallStatus
	// Allow asks Windows for an administrator's approval (the UAC prompt)
	// and adds the allow rule, removing kids-shelf's own block rules. It
	// returns when the elevated command has finished or was declined.
	Allow(ctx context.Context) error
}

// FirewallStatus is the result of one firewall check. Blocked means other
// devices cannot connect; Unknown means the check itself could not run.
// Detail is plain words for the parent page, never command output.
type FirewallStatus struct {
	Blocked bool
	Unknown bool
	Detail  string
}

// Deps are the services the handlers use; internal/app builds them. A nil
// field means that service is not running, and its routes answer 503 with a
// plain message instead of panicking.
type Deps struct {
	Budget  *budget.Budget
	Library *library.Library
	Ingest  *ingest.Queue
	YouTube *youtube.Manager
	Catalog *kiwix.Catalog
	Kiwix   *kiwix.Manager
	// KiwixServer serves the installed ZIMs under /kiwix/ (nil: libraries
	// are unavailable in this copy).
	KiwixServer KiwixBackend
	Practice    *practice.Service
	Updates     UpdateControl
	MediaDir    string           // where converted episodes and thumbnails live
	ClipsDir    string           // pre-generated spelling clips
	DataDir     string           // per-user data directory
	Now         func() time.Time // defaults to time.Now
	Quit        func()           // stops the app cleanly (the dashboard's Quit button)
	// ExtraHosts are additional Host names the server accepts, besides
	// localhost, the loopback addresses, this machine's interface IPs and
	// kids-shelf.local (the DNS-rebinding allowlist).
	ExtraHosts []string
	// Firewall checks and fixes LAN access (nil: no firewall to manage).
	Firewall FirewallControl
	// HelpersProblem returns a plain-words problem with the bundled
	// helper programs (ffmpeg, ffprobe, qjs, kiwix-serve) for the parent
	// page, or "" when they are fine (nil: nothing to report).
	HelpersProblem func() string
	// HelpersWaiting reports whether that problem is a wait that ends by
	// itself (Windows still checking the video tools), so the page leaves
	// out its "quit and reopen, or install again" advice (nil: never).
	HelpersWaiting func() bool
	// VideoEncoder returns the parent page's "Video conversion uses:"
	// name from the ingest worker's cached hardware selection (nil: the
	// parent page says the conversion has not been checked yet).
	VideoEncoder func() string
	// ProxyToken is the secret the supervisor's proxy presents with the
	// real client address (X-KS-Token, X-KS-Client). Empty: requests are
	// never proxied, so client addresses come from the connection only.
	ProxyToken string
	// InTrial reports whether this child is running an unconfirmed update
	// trial (nil: not in a trial). It reflects the durable journal, not a
	// spawn-relative timer, so Quit and Remove stay refused however long
	// the trial has run before it settles.
	InTrial func() bool
	// Remove records a parent's request to remove kids-shelf from this
	// computer (deleteData: also the videos and settings) and stops the
	// app so the supervisor can hand over to the remover (nil: removal is
	// not available, e.g. in-process dev runs).
	Remove func(deleteData bool) error
	// LibrariesPort is the public port of the separate libraries origin
	// (the supervisor's public libraries listener, right after the app
	// port). Zero: this copy serves no separate origin, so /kiwix/ URLs
	// stay relative to the app origin.
	LibrariesPort int
}

// registrar adds one area's routes to the server's mux.
type registrar func(s *Server)

var registrars []registrar

// register is called from each area file's init.
func register(r registrar) { registrars = append(registrars, r) }

// Server owns the HTTP routes and the request-security state (the cached
// list of this machine's interface IPs for the Host allowlist).
type Server struct {
	mux        *http.ServeMux
	deps       Deps
	healthJSON []byte

	hostMu  sync.Mutex
	hostIPs map[string]struct{} // this machine's interface IPs, string form
	hostsAt time.Time           // when hostIPs was last refreshed
}

// New returns a Server with only /healthz, for the launcher and its tests.
func New(instanceID string) *Server {
	s := &Server{mux: http.NewServeMux()}
	s.healthJSON, _ = json.Marshal(healthResponse{Status: "ok", ID: instanceID, Version: Version})
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	return s
}

// NewApp returns a Server for the given instance id (echoed by /healthz so a
// second launch can verify this instance) with every area's routes. Areas
// whose services are nil in deps answer 503.
func NewApp(instanceID string, deps Deps) *Server {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	s := New(instanceID)
	s.deps = deps
	for _, r := range registrars {
		r(s)
	}
	return s
}

// Mux returns the route table wrapped in the security middleware: the Host
// allowlist (DNS-rebinding protection) and the same-site check for state
// changing requests. New and NewApp both serve through it, so /healthz is
// covered too.
func (s *Server) Mux() http.Handler {
	return s.security(s.mux)
}

// LibrariesMux returns the route table of the separate libraries origin,
// wrapped in the same security middleware as Mux: ZIM pages are third-party
// HTML and JavaScript, so they run only on their own origin, where this mux
// routes nothing but /kiwix/ (plus the kid pages and files the libraries
// add to it) and the app's APIs are unreachable. The front door (/ and
// kiwix-serve's /kiwix/ welcome page) and the outside-link page are
// answered here, never by kiwix-serve, so the way home and the "not in the
// library" page work whether or not the libraries are running.
func (s *Server) LibrariesMux() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /{$}", s.handleLibrariesHome)
	m.HandleFunc("GET /kiwix", s.handleLibrariesHome)
	m.HandleFunc("GET /kiwix/{$}", s.handleLibrariesHome)
	m.HandleFunc("GET /kiwix/catch/external", s.handleKiwixExternal)
	m.HandleFunc("GET /kiwix/", s.handleKiwixProxy)
	m.HandleFunc("GET /ks/{file}", s.handleLibraryAsset)
	m.HandleFunc("GET /", s.handleLibrariesNotHere)
	return s.security(m)
}

// healthResponse is the /healthz payload.
type healthResponse struct {
	Status  string `json:"status"`
	ID      string `json:"id"`
	Version string `json:"version"`
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write(s.healthJSON)
}

// now returns the server clock, falling back to time.Now when Deps did not
// supply one (the /healthz-only New servers have no clock).
func (s *Server) now() time.Time {
	if s.deps.Now != nil {
		return s.deps.Now()
	}
	return time.Now()
}

// writeJSON writes v as JSON with the given status code.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// writeError writes {"error": msg} where msg is plain words a parent or kid
// can read; internal details go to the log, never to the page.
func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// readJSON decodes a request body of at most 64 KiB into v. Every state
// changing API speaks JSON, so a request body without Content-Type
// application/json is refused before any field is read.
func readJSON(r *http.Request, v any) error {
	ct := r.Header.Get("Content-Type")
	if !isJSONContentType(ct) {
		return errors.New("web: request body must be application/json")
	}
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 64<<10)).Decode(v)
}

func isJSONContentType(ct string) bool {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.EqualFold(strings.TrimSpace(ct), "application/json")
}

// security wraps next with the two request gates every route passes through:
// the Host allowlist (a DNS-rebinding attack answers through a foreign Host
// header, so anything but a known name or this machine's own addresses is
// refused) and the same-site check (state changing requests need an Origin
// or Referer matching the Host they were sent to).
func (s *Server) security(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r.Host) {
			writeError(w, http.StatusMisdirectedRequest, "This address is not served here.")
			return
		}
		if !s.sameSite(r) {
			writeError(w, http.StatusForbidden, "That request was not allowed.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostAllowed reports whether the request's Host is one of the addresses the
// shelf answers on: localhost, any loopback address, an IP literal of one of
// this machine's network interfaces (refreshed every minute), the
// kids-shelf.local name, or a name from Deps.ExtraHosts. Any port is fine.
func (s *Server) hostAllowed(host string) bool {
	h := hostnameOnly(host)
	if h == "" {
		return false
	}
	hl := strings.ToLower(h)
	if hl == "localhost" || hl == "kids-shelf.local" {
		return true
	}
	for _, extra := range s.deps.ExtraHosts {
		if strings.EqualFold(hostnameOnly(strings.TrimSpace(extra)), hl) {
			return true
		}
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	_, ok := s.interfaceIPs()[ip.String()]
	return ok
}

// interfaceIPs returns this machine's interface IP literals, refreshed at
// most once a minute (the interface list is stable and the lookup is not
// free; a minute of staleness is harmless either way).
func (s *Server) interfaceIPs() map[string]struct{} {
	s.hostMu.Lock()
	defer s.hostMu.Unlock()
	if s.hostIPs == nil || s.now().Sub(s.hostsAt) >= time.Minute {
		addrs, err := net.InterfaceAddrs()
		ips := make(map[string]struct{})
		if err == nil {
			for _, a := range addrs {
				var ip net.IP
				switch v := a.(type) {
				case *net.IPNet:
					ip = v.IP
				case *net.IPAddr:
					ip = v.IP
				}
				if ip != nil {
					ips[ip.String()] = struct{}{}
				}
			}
			s.hostIPs = ips
		}
		s.hostsAt = s.now()
	}
	return s.hostIPs
}

// sameSite reports whether a state changing request came from its own
// origin: the Origin header's host:port must equal the request Host. GET and
// HEAD (reads, and everything behind /static and /media) need no check.
// When Origin is absent the Referer header may stand in; when both are
// absent the request is refused, which is the /api/admin/quit class:
// bodyless POSTs still carry an Origin from the app's own pages.
func (s *Server) sameSite(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = r.Header.Get("Referer")
	}
	return origin != "" && originMatchesHost(origin, r.Host)
}

// originMatchesHost compares the host:port of an Origin or Referer URL to a
// request Host, case-insensitively, treating an explicit default port
// (http 80 / https 443) as the same as a bare host.
func originMatchesHost(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return sameHostPort(u.Host, host)
}

// sameHostPort compares two host[:port] values. Hosts compare
// case-insensitively; ports must be equal, and a missing port equals the
// other side's explicit 80 or 443.
func sameHostPort(a, b string) bool {
	ah, ap := splitHostPort(a)
	bh, bp := splitHostPort(b)
	if !strings.EqualFold(ah, bh) {
		return false
	}
	if ap == bp {
		return true
	}
	if ap == "" && (bp == "80" || bp == "443") {
		return true
	}
	return bp == "" && (ap == "80" || ap == "443")
}

// splitHostPort splits "host:port", returning just the host when there is
// no port.
func splitHostPort(hp string) (host, port string) {
	if h, p, err := net.SplitHostPort(hp); err == nil {
		return h, p
	}
	return hp, ""
}

// hostnameOnly strips the port from a Host header value.
func hostnameOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}
