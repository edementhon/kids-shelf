// The "Connect a tablet" confirmation. The parent page's QR code carries
// connectQuery, and the kid page opened from it posts /api/connected once.
// The server keeps only the latest such ping (its time and the client
// address) in memory, and the parent page's Connect a tablet step polls
// /api/admin/connect/status to say "a tablet just connected".
//
// The same kid page then moves the tablet to the name the computer
// announces (kids-shelf.local) when the name reaches this same kids-shelf
// from that tablet: it reads /api/name, then loads /api/name-check?id=
// through the name as an image. A home-screen icon saved after the move
// keeps working when the router gives the computer a new address.
package web

import (
	"net/http"
	"sync"
	"time"
)

// connectPingEvery is the shortest gap between two recorded pings. The
// ledger holds a single entry, so a flood cannot grow memory; this only
// keeps a busy device from rewriting it many times a second.
const connectPingEvery = time.Second

func init() {
	register(func(s *Server) {
		s.mux.HandleFunc("POST /api/connected", s.handleConnected)
		s.mux.HandleFunc("GET /api/admin/connect/status", s.handleAdminConnectStatus)
		s.mux.HandleFunc("GET /api/name", s.handleName)
		s.mux.HandleFunc("GET /api/name-check", s.handleNameCheck)
	})
}

// handleName tells the kid page the name tablets can use ("" when the
// computer announces none) and this instance's id, which /api/name-check
// compares. Both are public: /healthz reports the id too.
func (s *Server) handleName(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"name": s.deps.LANName, "id": s.id})
}

// pixelGIF is a 1x1 transparent GIF.
var pixelGIF = []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\x00\x00\x00!\xf9\x04\x01\x00\x00\x00\x00,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;")

// handleNameCheck answers an image only when id is this instance's, so a
// tablet moves to the name only when the name reaches this same kids-shelf
// (not another one in the house). An image, not JSON, because the page
// loads it from the other origin without CORS.
func (s *Server) handleNameCheck(w http.ResponseWriter, r *http.Request) {
	if s.id == "" || r.URL.Query().Get("id") != s.id {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/gif")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(pixelGIF)
}

// connectPing is one tablet arriving from the QR code.
type connectPing struct {
	at     time.Time
	client string
}

// connectLedger holds the latest connect ping. Like the session ledgers it
// is package-level: the app runs one Server per process, and a restart
// forgets it (the parent page only cares about the last few seconds).
type connectLedger struct {
	mu   sync.Mutex
	last connectPing
}

var connectPings = &connectLedger{}

// record stores a ping from client at now, unless the previous one is less
// than connectPingEvery old; it reports whether it stored it.
func (l *connectLedger) record(client string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.last.at.IsZero() {
		if d := now.Sub(l.last.at); d >= 0 && d < connectPingEvery {
			return false
		}
	}
	l.last = connectPing{at: now, client: client}
	return true
}

func (l *connectLedger) latest() connectPing {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last
}

func (l *connectLedger) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.last = connectPing{}
}

// handleConnected records that a device opened the kid page from the QR
// code. It needs no PIN: the kid page sends it before anyone signs in, and
// all it can do is make the parent page say a tablet connected. The
// computer itself (loopback) is not a tablet and is not recorded.
func (s *Server) handleConnected(w http.ResponseWriter, r *http.Request) {
	if s.clientIsLocal(r) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !connectPings.record(s.clientIP(r), s.now()) {
		writeError(w, http.StatusTooManyRequests, "Too many tries. Wait a little bit, then try again.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAdminConnectStatus answers the parent page's Connect a tablet step:
// last is the RFC 3339 time of the latest ping ("" when there was none
// since kids-shelf started) and client the address it came from.
func (s *Server) handleAdminConnectStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	p := connectPings.latest()
	last := ""
	if !p.at.IsZero() {
		last = p.at.UTC().Format(time.RFC3339Nano)
	}
	writeJSON(w, http.StatusOK, map[string]string{"last": last, "client": p.client})
}
