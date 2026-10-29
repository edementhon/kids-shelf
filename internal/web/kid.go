// The kid area: the "Who's watching?" home, the video shelf, the player and
// the per-kid metering APIs. It owns the unlock sessions (a random cookie
// token per unlocked kid or grown-up watch-together session), the catalog
// APIs, live playback charging through budget.Consume and resume-position
// saves through library. Everything here is registered from init so the area
// never touches server.go; a nil service answers 503 with plain words.
package web

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edementhon/kids-shelf/internal/budget"
	"github.com/edementhon/kids-shelf/internal/core"
	"github.com/edementhon/kids-shelf/internal/library"
)

//go:embed static/kid
var kidStatic embed.FS

// Episode pages render a page of 24 episodes at a time, like the legacy
// shelf's episode band.
const episodesPerPage = 24

func init() {
	register(func(s *Server) { kidRoutes(s) })
}

func kidRoutes(s *Server) {
	s.mux.HandleFunc("GET /", s.handleKidIndex)
	s.mux.Handle("GET /static/kid/", http.FileServer(http.FS(kidStatic)))
	s.mux.HandleFunc("GET /watch/{episode}", s.handleWatch)
	s.mux.HandleFunc("GET /media/{episode}", s.handleMedia)
	s.mux.HandleFunc("GET /thumbs/{episode}", s.handleThumb)
	s.mux.HandleFunc("POST /api/kid/unlock", s.handleUnlock)
	s.mux.HandleFunc("POST /api/kid/watch-together", s.handleWatchTogether)
	s.mux.HandleFunc("GET /api/kid/grownup-pin", s.handleGrownupPin)
	s.mux.HandleFunc("POST /api/kid/started", s.handleStarted)
	s.mux.HandleFunc("POST /api/kid/lock", s.handleLock)
	s.mux.HandleFunc("GET /api/kid/me", s.handleMe)
	s.mux.HandleFunc("GET /api/kid/resume/{episode}", s.handleResume)
	s.mux.HandleFunc("POST /api/kid/consume", s.handleConsume)
	s.mux.HandleFunc("POST /api/kid/position", s.handlePosition)
	s.mux.HandleFunc("GET /api/kids", s.handleKids)
	s.mux.HandleFunc("GET /api/shows", s.handleShows)
	s.mux.HandleFunc("GET /api/shows/{id}/episodes", s.handleShowEpisodes)
	s.mux.HandleFunc("GET /api/search", s.handleSearch)
	s.mux.HandleFunc("GET /api/keep-watching", s.handleKeepWatching)
	s.mux.HandleFunc("GET /api/recent", s.handleRecent)
	s.mux.HandleFunc("GET /api/balance", s.handleBalance)
}

// ---------------------------------------------------------------------------
// Unlock sessions
// ---------------------------------------------------------------------------

// sessionCookie is the one cookie the kid area sets: an opaque token mapped
// to the unlocked session. It is HttpOnly and SameSite=Strict so page
// scripts cannot read it and other sites cannot send it.
const sessionCookie = "ks_session"

// kidSessionLifetime is how long a kid's unlock lasts (the assignment's
// 12 h); grownupSessionLifetime is the much shorter watch-together session,
// which plays everything free and unmetered, so a device a grown-up walked
// away from does not stay free for the rest of the day.
const (
	kidSessionLifetime     = 12 * time.Hour
	grownupSessionLifetime = 2 * time.Hour
)

// kidSessions is the in-memory unlock ledger. server.go owns the Server
// struct and cannot be edited, and the app runs exactly one Server per
// process, so a package-level ledger is the right home: the token lives in
// the cookie, the identity lives here, and a restart forgets every session
// (kids simply unlock again).
var kidSessions = newSessionLedger()

type sessionLedger struct {
	mu       sync.Mutex
	sessions map[string]kidSession
}

func newSessionLedger() *sessionLedger {
	return &sessionLedger{sessions: make(map[string]kidSession)}
}

// kidSession is one unlock: either a kid (kid set, grownup false) or the
// household grown-up watch-together (grownup true).
type kidSession struct {
	kid     core.KidID
	name    string
	grownup bool
	expires time.Time
}

func (l *sessionLedger) put(token string, s kidSession) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sessions[token] = s
}

// get returns the session for token, dropping it when expired.
func (l *sessionLedger) get(token string, now time.Time) (kidSession, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.sessions[token]
	if !ok {
		return kidSession{}, false
	}
	if now.After(s.expires) {
		delete(l.sessions, token)
		return kidSession{}, false
	}
	return s, true
}

func (l *sessionLedger) drop(token string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.sessions, token)
}

// newSessionToken returns a fresh 128-bit random token, hex-encoded.
func newSessionToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// startSession writes a new session and its cookie, lasting the kid or the
// grown-up session lifetime.
func (s *Server) startSession(w http.ResponseWriter, sess kidSession) {
	token, err := newSessionToken()
	if err != nil {
		writeError(w, 500, "Something went wrong. Try again.")
		return
	}
	lifetime := kidSessionLifetime
	if sess.grownup {
		lifetime = grownupSessionLifetime
	}
	sess.expires = s.deps.Now().Add(lifetime)
	kidSessions.put(token, sess)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Expires:  sess.expires,
		MaxAge:   int(lifetime.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// clearSessionCookie expires the cookie on the client.
func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Expires:  time.Unix(1, 0),
		MaxAge:   -1,
	})
}

// currentSession resolves the request's cookie to a live session.
func (s *Server) currentSession(r *http.Request) (kidSession, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return kidSession{}, false
	}
	return kidSessions.get(c.Value, s.deps.Now())
}

// requireSession writes 401 and reports false when the request has no live
// unlock session. The kid-only APIs (balance, metering, positions, lock)
// sit behind it; catalog APIs stay open like the legacy portal's were.
func (s *Server) requireSession(w http.ResponseWriter, r *http.Request) (kidSession, bool) {
	sess, ok := s.currentSession(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "You're not unlocked yet. Go back and pick who's watching.")
		return kidSession{}, false
	}
	return sess, true
}

// requireLibrary writes 503 when the library service is not running.
func (s *Server) requireLibrary(w http.ResponseWriter) bool {
	if s.deps.Library == nil {
		writeError(w, http.StatusServiceUnavailable, "The shelf isn't ready yet. Try again in a moment.")
		return false
	}
	return true
}

// requireBudget writes 503 when the budget service is not running yet (the
// app is still starting).
func (s *Server) requireBudget(w http.ResponseWriter) bool {
	if s.deps.Budget == nil {
		writeError(w, http.StatusServiceUnavailable, "The shelf is still starting. Try again in a moment.")
		return false
	}
	return true
}

// kidName resolves a kid id to its display name, for the session greeting.
func (s *Server) kidName(ctx context.Context, id core.KidID) string {
	kids, err := s.deps.Budget.ListKids(ctx)
	if err != nil {
		return ""
	}
	for _, k := range kids {
		if k.ID == id {
			return k.Name
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Pages
// ---------------------------------------------------------------------------

func (s *Server) handleKidIndex(w http.ResponseWriter, r *http.Request) {
	http.ServeFileFS(w, r, kidStatic, "static/kid/index.html")
}

// watchData is the per-episode state the player page embeds. Next/NextTitle
// hold the following playable episode of the same show in shelf order, for
// the "Next episode" button on the end screen ("" when there is none).
// Pending episodes are skipped: the button must never lead into a
// "getting ready" panel. Issue/Fix tell the player why the episode does
// not play yet (see the 409 player contract).
type watchData struct {
	Episode   string `json:"Episode"`
	Title     string `json:"Title"`
	Free      bool   `json:"Free"`
	Next      string `json:"Next"`
	NextTitle string `json:"NextTitle"`
	Issue     string `json:"Issue"`
	Fix       string `json:"Fix"`
}

// playerPage is the player template; each request substitutes __KS_DATA__.
//
//go:embed static/kid/player.html
var playerPage string

// handleWatch serves the player for one episode. It no longer writes the
// watch log: a page load is not a watch, and a play blocked by "Time's up"
// must not leave a row. The player calls POST /api/kid/started on its first
// real 'playing' event, and only that call records the watch.
func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request) {
	if !s.requireLibrary(w) {
		return
	}
	ep := core.EpisodeID(r.PathValue("episode"))
	rec, err := s.deps.Library.Episode(r.Context(), ep)
	if err != nil {
		writeError(w, http.StatusNotFound, "We couldn't find that video.")
		return
	}
	sess, ok := s.currentSession(r)
	if !ok {
		// Nobody is unlocked: send them back to the picker.
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	free := sess.grownup
	if !free {
		if rate, err := s.deps.Library.BinRate(r.Context(), rec.ID); err == nil {
			free = rate <= 0
		}
	}
	nextID, nextTitle := "", ""
	if eps, err := s.deps.Library.ShowEpisodes(r.Context(), rec.Show); err == nil {
		for i, e := range eps {
			if e.ID != rec.ID {
				continue
			}
			// The next episode must be playable: skip the pending ones,
			// whose better copy is not made yet, so the "Next episode"
			// button never leads into a "getting ready" panel.
			for _, n := range eps[i+1:] {
				if n.Fix != core.FixPending {
					nextID, nextTitle = string(n.ID), n.Title
					break
				}
			}
			break
		}
	}
	data, err := json.Marshal(watchData{
		Episode: string(rec.ID), Title: rec.Title, Free: free,
		Next: nextID, NextTitle: nextTitle,
		Issue: rec.Issue, Fix: rec.Fix,
	})
	if err != nil {
		writeError(w, 500, "Something went wrong. Try again.")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, strings.Replace(playerPage, "__KS_DATA__", string(data), 1))
}

// handleStarted records a watch that really started. The player calls it on
// the first 'playing' event, so a page load that is blocked before any
// playback never reaches here and never lands in the watch log. A metered
// kid whose minutes are already gone is cut off instead of logged.
func (s *Server) handleStarted(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	var req struct {
		Episode string `json:"episode"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "That didn't look right. Try again.")
		return
	}
	if req.Episode == "" {
		writeError(w, http.StatusBadRequest, "That didn't look right. Try again.")
		return
	}
	if !s.requireLibrary(w) {
		return
	}
	rec, err := s.deps.Library.Episode(r.Context(), core.EpisodeID(req.Episode))
	if err != nil {
		writeError(w, http.StatusNotFound, "We couldn't find that video.")
		return
	}
	if rec.Fix == core.FixPending {
		// The better copy is not made yet: no minutes, no watch-log row.
		writeError(w, http.StatusConflict, msgGettingReady)
		return
	}
	free := sess.grownup
	cost := 0.0
	if !free {
		rate, err := s.deps.Library.BinRate(r.Context(), rec.ID)
		if err != nil {
			writeError(w, http.StatusNotFound, "We couldn't find that video.")
			return
		}
		free = rate <= 0
		if !free {
			if !s.requireBudget(w) {
				return
			}
			today, saved, err := s.deps.Budget.Balance(r.Context(), sess.kid, budget.DefaultCounter)
			if err != nil {
				log.Printf("kid: balance: %v", err)
				writeError(w, 500, "Something went wrong. Try again.")
				return
			}
			if today+saved <= 1e-9 {
				writeError(w, http.StatusForbidden, "Time's up! You used all your minutes for today.")
				return
			}
			// The logged cost is the whole-episode price at the show's
			// bin rate (per-second charging prorates what is actually
			// watched); it is what the episode card shows.
			cost = round2(float64(rec.DurationSec) / 60 * rate)
		}
	}
	if err := s.deps.Library.RecordWatch(r.Context(), sess.kid, rec.ID, rec.Title, cost, free); err != nil {
		log.Printf("kid: record watch %s: %v", rec.ID, err)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// mediaUnavailableMsg is what the player shows when an in-place original is
// not reachable (NAS unmounted, drive unplugged).
const mediaUnavailableMsg = "This video's file is not available right now. Is the video drive or NAS connected?"

// msgGettingReady is what a pending episode answers when its playback is
// asked for: the better copy is not made yet, so the better copy's video
// does not play. The player shows a "getting ready" panel; nothing is
// charged and no watch is recorded.
const msgGettingReady = "This one is still getting ready. Pick another one for now."

// handleMedia serves the converted MP4 of a known episode with Range
// support. The file path comes from the library row, never from the
// request, so "/media/<id>.mp4" can never reach the filesystem directly.
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	if !s.requireLibrary(w) {
		return
	}
	name := r.PathValue("episode")
	if !strings.HasSuffix(name, ".mp4") {
		http.NotFound(w, r)
		return
	}
	s.serveEpisodeFile(w, r, core.EpisodeID(strings.TrimSuffix(name, ".mp4")))
}

// handleThumb serves a known episode's JPEG thumbnail, resolved the same way
// as handleMedia.
func (s *Server) handleThumb(w http.ResponseWriter, r *http.Request) {
	if !s.requireLibrary(w) {
		return
	}
	name := r.PathValue("episode")
	if !strings.HasSuffix(name, ".jpg") {
		http.NotFound(w, r)
		return
	}
	s.serveThumbFile(w, r, core.EpisodeID(strings.TrimSuffix(name, ".jpg")))
}

func (s *Server) serveEpisodeFile(w http.ResponseWriter, r *http.Request, id core.EpisodeID) {
	rec, err := s.deps.Library.Episode(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if rec.Fix == core.FixPending {
		// The better copy is not made yet: the episode is on the shelf
		// but does not play, whatever file it currently points at.
		writeError(w, http.StatusConflict, msgGettingReady)
		return
	}
	if !rec.Owned {
		s.serveOriginal(w, r, rec.File)
		return
	}
	s.serveFile(w, r, rec.File)
}

// serveOriginal streams an in-place episode's folder/NAS original. Ingest
// recorded the file's real path (no symlinks); a path that now resolves
// elsewhere, or an opened file that is not the one at that path, is
// refused, so replacing the original (or a folder above it) with a symlink
// can never make playback hand out another file the server can read. A file
// that cannot be opened answers 503 with the message the player shows: the
// drive or NAS is probably just disconnected.
func (s *Server) serveOriginal(w http.ResponseWriter, r *http.Request, path string) {
	f, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, mediaUnavailableMsg)
		return
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() {
		writeError(w, http.StatusServiceUnavailable, mediaUnavailableMsg)
		return
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil || real != path {
		http.NotFound(w, r)
		return
	}
	atPath, err := os.Lstat(path)
	if err != nil || !os.SameFile(opened, atPath) {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, filepath.Base(path), opened.ModTime(), f)
}

func (s *Server) serveThumbFile(w http.ResponseWriter, r *http.Request, id core.EpisodeID) {
	rec, err := s.deps.Library.Episode(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.serveFile(w, r, rec.Thumb)
}

// serveFile streams the stored file at path with http.ServeContent, which
// handles Range requests, content types and conditional GETs.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, path string) {
	if path == "" {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, filepath.Base(path), st.ModTime(), f)
}

// ---------------------------------------------------------------------------
// Unlock APIs
// ---------------------------------------------------------------------------

// handleUnlock checks a kid's PIN and, when it matches, starts that kid's
// session. The PIN never leaves the server's constant-time comparison, and
// wrong tries feed the kid-unlock throttle (per kid account and IP).
func (s *Server) handleUnlock(w http.ResponseWriter, r *http.Request) {
	if !s.requireBudget(w) {
		return
	}
	var req struct {
		Kid string `json:"kid"`
		Pin string `json:"pin"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "That didn't look right. Try again.")
		return
	}
	req.Pin = strings.TrimSpace(req.Pin)
	if req.Kid == "" || req.Pin == "" {
		writeError(w, http.StatusBadRequest, "Pick a name and enter your PIN.")
		return
	}
	if s.pinLocked(w, r, realmKidUnlock, req.Kid) {
		return
	}
	if !s.deps.Budget.VerifyKidPIN(r.Context(), core.KidID(req.Kid), req.Pin) {
		if s.pinWrong(w, realmKidUnlock, s.clientIP(r), req.Kid) {
			return
		}
		writeError(w, http.StatusUnauthorized, "That PIN didn't work. Try again.")
		return
	}
	pinThrottle.ok(realmKidUnlock, s.clientIP(r), req.Kid)
	s.startSession(w, kidSession{kid: core.KidID(req.Kid), name: s.kidName(r.Context(), core.KidID(req.Kid))})
}

// handleWatchTogether starts a grown-up watch-together session: playback
// with it is free and unmetered. Wrong tries feed the watch-together
// throttle (per client IP). When no grown-up PIN has been set at all, the
// keypad is told in plain words instead of being told its PIN was wrong.
func (s *Server) handleWatchTogether(w http.ResponseWriter, r *http.Request) {
	if !s.requireBudget(w) {
		return
	}
	var req struct {
		Pin string `json:"pin"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "That didn't look right. Try again.")
		return
	}
	req.Pin = strings.TrimSpace(req.Pin)
	if req.Pin == "" {
		writeError(w, http.StatusBadRequest, "Enter the grown-up PIN.")
		return
	}
	if !s.deps.Budget.GrownupPINSet(r.Context()) {
		// Nobody has set a PIN yet: say what to do instead of pretending
		// the entered PIN was wrong. Watch-together stays locked until a
		// PIN exists, so a missing PIN can never become a free kid path.
		writeError(w, http.StatusConflict, "No grown-up PIN yet. Set one on the parent page, under Settings.")
		return
	}
	if s.pinLocked(w, r, realmWatchTogether, "") {
		return
	}
	if !s.deps.Budget.VerifyGrownupPIN(r.Context(), req.Pin) {
		if s.pinWrong(w, realmWatchTogether, s.clientIP(r), "") {
			return
		}
		writeError(w, http.StatusUnauthorized, "That PIN didn't work. Try again.")
		return
	}
	pinThrottle.ok(realmWatchTogether, s.clientIP(r), "")
	s.startSession(w, kidSession{grownup: true})
}

// handleGrownupPin reports whether the household watch-together PIN is set,
// so the keypad can explain itself before anyone types.
func (s *Server) handleGrownupPin(w http.ResponseWriter, r *http.Request) {
	if !s.requireBudget(w) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"set": s.deps.Budget.GrownupPINSet(r.Context())})
}

// handleLock ends the session and clears the cookie.
func (s *Server) handleLock(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		kidSessions.drop(c.Value)
	}
	clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleMe reports who is unlocked, or 401 when nobody is.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kid":     sess.kid,
		"name":    sess.name,
		"grownup": sess.grownup,
	})
}

// ---------------------------------------------------------------------------
// Catalog APIs
// ---------------------------------------------------------------------------

// kidJSON and showJSON are the wire shapes of the catalog entries (the
// library's own structs marshal with capital field names).
type kidJSON struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Animal string `json:"animal"`
}

type showJSON struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Bin      string `json:"bin"`
	Poster   string `json:"poster"`
	Episodes int    `json:"episodes"`
}

type episodeJSON struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	DurationSec int64   `json:"durationSec"`
	Thumb       string  `json:"thumb"`
	Show        string  `json:"show"`
	ShowName    string  `json:"showName"`
	Bin         string  `json:"bin"`  // "fun", "learning" or "free"
	Cost        float64 `json:"cost"` // whole-episode cost in minutes; 0 when free
	Free        bool    `json:"free"`
	Issue       string  `json:"issue"` // "" when the file plays everywhere
	Fix         string  `json:"fix"`   // "" when the fix is final; "pending" or "failed"
}

// handleKids lists the kid profiles for the picker. Names are not secrets
// on a family LAN, exactly as the legacy portal's ticket list was open.
func (s *Server) handleKids(w http.ResponseWriter, r *http.Request) {
	if !s.requireBudget(w) {
		return
	}
	kids, err := s.deps.Budget.ListKids(r.Context())
	if err != nil {
		log.Printf("kid: list kids: %v", err)
		writeError(w, 500, "Something went wrong. Try again.")
		return
	}
	out := make([]kidJSON, 0, len(kids))
	for _, k := range kids {
		out = append(out, kidJSON{ID: string(k.ID), Name: k.Name, Animal: k.Animal})
	}
	writeJSON(w, http.StatusOK, map[string]any{"kids": out})
}

// posterURL renders a show's poster episode id as the /thumbs/ URL the shelf
// cards load. "" stays "" so the page can fall back to the generic icon.
func posterURL(poster string) string {
	if poster == "" {
		return ""
	}
	return "/thumbs/" + poster + ".jpg"
}

// handleShows lists every show for the shelf grid; each poster is a /thumbs/
// URL of the show's first thumbed episode, so the cards are picture-first.
func (s *Server) handleShows(w http.ResponseWriter, r *http.Request) {
	if !s.requireLibrary(w) {
		return
	}
	shows, err := s.deps.Library.ListShows(r.Context())
	if err != nil {
		log.Printf("kid: list shows: %v", err)
		writeError(w, 500, "Something went wrong. Try again.")
		return
	}
	out := make([]showJSON, 0, len(shows))
	for _, sh := range shows {
		out = append(out, showJSON{ID: string(sh.ID), Name: sh.Name, Bin: sh.Bin, Poster: posterURL(sh.Poster), Episodes: sh.Episodes})
	}
	writeJSON(w, http.StatusOK, map[string]any{"shows": out})
}

// showBins maps every show id to its bin, for per-episode pricing.
func (s *Server) showBins(ctx context.Context) (map[string]string, error) {
	shows, err := s.deps.Library.ListShows(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[string]string, len(shows))
	for _, sh := range shows {
		m[string(sh.ID)] = sh.Bin
	}
	return m, nil
}

// round2 rounds minutes to two decimals so the JSON never leaks float noise.
func round2(x float64) float64 {
	return math.Round(x*100) / 100
}

// episodeView converts a library episode into its wire shape with the cost
// the server actually charges: durationSec/60 * the show's bin rate, the
// same per-second rule budget.Consume uses (no 0.5-minute floor, so a 20 s
// fun episode shows about 0.3 min, not 0.5).
func episodeView(ep library.Episode, binByShow map[string]string) episodeJSON {
	bin := binByShow[string(ep.Show)]
	rate := library.BinRateOf(bin)
	free := rate <= 0
	cost := 0.0
	if !free {
		cost = round2(float64(ep.DurationSec) / 60 * rate)
	}
	thumb := ""
	if ep.Thumb != "" {
		thumb = "/thumbs/" + string(ep.ID) + ".jpg"
	}
	return episodeJSON{
		ID:          string(ep.ID),
		Title:       ep.Title,
		DurationSec: ep.DurationSec,
		Thumb:       thumb,
		Show:        string(ep.Show),
		ShowName:    ep.ShowName,
		Bin:         bin,
		Cost:        cost,
		Free:        free,
		Issue:       ep.Issue,
		Fix:         ep.Fix,
	}
}

// pageParams parses offset/limit for the paged episode band, clamped so a
// page request can never be absurd.
func pageParams(r *http.Request) (offset, limit int) {
	offset, _ = strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	limit, _ = strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = episodesPerPage
	}
	if limit > 200 {
		limit = 200
	}
	return offset, limit
}

// handleShowEpisodes returns one page of a show's episodes plus the total,
// so the UI can draw the "Show more (N left)" button.
func (s *Server) handleShowEpisodes(w http.ResponseWriter, r *http.Request) {
	if !s.requireLibrary(w) {
		return
	}
	eps, err := s.deps.Library.ShowEpisodes(r.Context(), core.ShowID(r.PathValue("id")))
	if err != nil {
		log.Printf("kid: show episodes: %v", err)
		writeError(w, 500, "Something went wrong. Try again.")
		return
	}
	bins, err := s.showBins(r.Context())
	if err != nil {
		log.Printf("kid: show bins: %v", err)
		writeError(w, 500, "Something went wrong. Try again.")
		return
	}
	offset, limit := pageParams(r)
	lo := min(offset, len(eps))
	hi := min(offset+limit, len(eps))
	page := make([]episodeJSON, 0, hi-lo)
	for _, ep := range eps[lo:hi] {
		page = append(page, episodeView(ep, bins))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"episodes": page,
		"total":    len(eps),
		"offset":   offset,
		"limit":    limit,
	})
}

// handleSearch matches a query against show names and episode titles,
// independently, as the library's Search does.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if !s.requireLibrary(w) {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusOK, map[string]any{"shows": []showJSON{}, "episodes": []episodeJSON{}})
		return
	}
	res, err := s.deps.Library.Search(r.Context(), q)
	if err != nil {
		log.Printf("kid: search: %v", err)
		writeError(w, 500, "Something went wrong. Try again.")
		return
	}
	bins, err := s.showBins(r.Context())
	if err != nil {
		log.Printf("kid: show bins: %v", err)
		writeError(w, 500, "Something went wrong. Try again.")
		return
	}
	shows := make([]showJSON, 0, len(res.Shows))
	for _, sh := range res.Shows {
		shows = append(shows, showJSON{ID: string(sh.ID), Name: sh.Name, Bin: sh.Bin, Poster: posterURL(sh.Poster), Episodes: sh.Episodes})
	}
	eps := make([]episodeJSON, 0, len(res.Episodes))
	for _, ep := range res.Episodes {
		eps = append(eps, episodeView(ep, bins))
	}
	writeJSON(w, http.StatusOK, map[string]any{"shows": shows, "episodes": eps})
}

// ---------------------------------------------------------------------------
// Per-kid APIs (session required)
// ---------------------------------------------------------------------------

// handleKeepWatching lists one kid's saved positions above the barely-
// started cutoff for the shelf row.
func (s *Server) handleKeepWatching(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	if sess.grownup {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}})
		return
	}
	if !s.requireLibrary(w) {
		return
	}
	items, err := s.deps.Library.KeepWatching(r.Context(), sess.kid)
	if err != nil {
		log.Printf("kid: keep watching: %v", err)
		writeError(w, 500, "Something went wrong. Try again.")
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		thumb := ""
		if it.Thumb != "" {
			thumb = "/thumbs/" + string(it.Episode) + ".jpg"
		}
		out = append(out, map[string]any{
			"episode":  string(it.Episode),
			"title":    it.Title,
			"thumb":    thumb,
			"pos":      it.Pos,
			"duration": it.DurationSec,
			"show":     string(it.Show),
			"showName": it.ShowName,
			"issue":    it.Issue,
			"fix":      it.Fix,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// handleRecent lists the recently-watched row, newest first: for a kid, that
// kid's own watches (library.RecentForKid; the household table keeps only
// each episode's last watcher, so filtering it dropped an episode once a
// sibling watched it too); for a grown-up session, the whole household list,
// which the page labels accordingly.
func (s *Server) handleRecent(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	if !s.requireLibrary(w) {
		return
	}
	var items []library.RecentItem
	var err error
	if sess.grownup {
		items, err = s.deps.Library.Recent(r.Context())
	} else {
		items, err = s.deps.Library.RecentForKid(r.Context(), core.KidID(sess.kid))
	}
	if err != nil {
		log.Printf("kid: recent: %v", err)
		writeError(w, 500, "Something went wrong. Try again.")
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		thumb := ""
		if it.Thumb != "" {
			thumb = "/thumbs/" + string(it.Episode) + ".jpg"
		}
		out = append(out, map[string]any{
			"episode":  string(it.Episode),
			"title":    it.Title,
			"thumb":    thumb,
			"show":     string(it.Show),
			"showName": it.ShowName,
			"issue":    it.Issue,
			"fix":      it.Fix,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// balanceView is the wire shape of one balance read.
type balanceView struct {
	Today   float64 `json:"today"`
	Saved   float64 `json:"saved"`
	Total   float64 `json:"total"`
	Grownup bool    `json:"grownup"`
}

// handleBalance returns the unlocked kid's minutes (today + saved). A
// grown-up session has no account and is simply free.
func (s *Server) handleBalance(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	if sess.grownup {
		writeJSON(w, http.StatusOK, balanceView{Grownup: true})
		return
	}
	if !s.requireBudget(w) {
		return
	}
	today, saved, err := s.deps.Budget.Balance(r.Context(), sess.kid, budget.DefaultCounter)
	if err != nil {
		log.Printf("kid: balance: %v", err)
		writeError(w, 500, "Something went wrong. Try again.")
		return
	}
	writeJSON(w, http.StatusOK, balanceFor(today, saved))
}

func balanceFor(today, saved float64) balanceView {
	today, saved = round2(today), round2(saved)
	return balanceView{Today: today, Saved: saved, Total: round2(today + saved)}
}

// consumeResponse is what the player's live-metering tick gets back.
type consumeResponse struct {
	Today   float64 `json:"today"`
	Saved   float64 `json:"saved"`
	Total   float64 `json:"total"`
	CutOff  bool    `json:"cutOff"`
	Free    bool    `json:"free"`
	Grownup bool    `json:"grownup"`
}

// handleConsume meters real playback seconds: the server prices the episode
// at its show's bin rate (Library.BinRate) and charges budget.Consume,
// clamped to budget.MaxConsumeSeconds per call. A grown-up session and a
// free-bin episode never charge and never cut off, even at a zero or
// negative balance. An episode still "Getting ready" (fix pending) does not
// play, so a tick for it is refused before anything is charged.
func (s *Server) handleConsume(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	var req struct {
		Episode string  `json:"episode"`
		Sec     float64 `json:"sec"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "That didn't look right. Try again.")
		return
	}
	if req.Episode == "" {
		writeError(w, http.StatusBadRequest, "That didn't look right. Try again.")
		return
	}
	if sess.grownup {
		writeJSON(w, http.StatusOK, consumeResponse{Grownup: true, Free: true})
		return
	}
	if !s.requireLibrary(w) {
		return
	}
	rec, err := s.deps.Library.Episode(r.Context(), core.EpisodeID(req.Episode))
	if err != nil {
		writeError(w, http.StatusNotFound, "We couldn't find that video.")
		return
	}
	if rec.Fix == core.FixPending {
		writeError(w, http.StatusConflict, msgGettingReady)
		return
	}
	rate, err := s.deps.Library.BinRate(r.Context(), rec.ID)
	if err != nil {
		writeError(w, http.StatusNotFound, "We couldn't find that video.")
		return
	}
	if !s.requireBudget(w) {
		return
	}
	ctx := r.Context()
	if rate <= 0 {
		// Free bin: no charge, never a cut-off, whatever the balance.
		today, saved, err := s.deps.Budget.Balance(ctx, sess.kid, budget.DefaultCounter)
		if err != nil {
			log.Printf("kid: balance: %v", err)
			writeError(w, 500, "Something went wrong. Try again.")
			return
		}
		b := balanceFor(today, saved)
		writeJSON(w, http.StatusOK, consumeResponse{Today: b.Today, Saved: b.Saved, Total: b.Total, Free: true})
		return
	}
	sec := math.Round(math.Min(math.Max(req.Sec, 0), budget.MaxConsumeSeconds)*100) / 100
	if sec <= 0 {
		// Nothing played since the last tick: report the balance untouched.
		today, saved, err := s.deps.Budget.Balance(ctx, sess.kid, budget.DefaultCounter)
		if err != nil {
			log.Printf("kid: balance: %v", err)
			writeError(w, 500, "Something went wrong. Try again.")
			return
		}
		b := balanceFor(today, saved)
		writeJSON(w, http.StatusOK, consumeResponse{Today: b.Today, Saved: b.Saved, Total: b.Total})
		return
	}
	var cutOff bool
	today, saved, cutOff, err := s.deps.Budget.Consume(ctx, sess.kid, budget.DefaultCounter, sec, rate)
	if err != nil {
		log.Printf("kid: consume: %v", err)
		writeError(w, 500, "Something went wrong. Try again.")
		return
	}
	b := balanceFor(today, saved)
	writeJSON(w, http.StatusOK, consumeResponse{Today: b.Today, Saved: b.Saved, Total: b.Total, CutOff: cutOff})
}

// handlePosition saves a kid's resume position (the player sends it on
// pause, every 10 s and on pagehide). A grown-up session has no account to
// save, so it is a no-op. A position of 0 clears the saved spot, and so
// does one within finishedWithin of the episode's end: the player sends 0
// when an episode ends, and this keeps a finished episode out of Keep
// watching even when a client sends the end position instead.
func (s *Server) handlePosition(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	if sess.grownup {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	if !s.requireLibrary(w) {
		return
	}
	var req struct {
		Episode string  `json:"episode"`
		Pos     float64 `json:"pos"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "That didn't look right. Try again.")
		return
	}
	if req.Episode == "" {
		writeError(w, http.StatusBadRequest, "That didn't look right. Try again.")
		return
	}
	pos := req.Pos
	if rec, err := s.deps.Library.Episode(r.Context(), core.EpisodeID(req.Episode)); err == nil &&
		rec.DurationSec > 0 && pos >= float64(rec.DurationSec)-finishedWithin {
		pos = 0
	}
	if err := s.deps.Library.SavePosition(r.Context(), sess.kid, core.EpisodeID(req.Episode), pos); err != nil {
		log.Printf("kid: save position: %v", err)
		writeError(w, 500, "Something went wrong. Try again.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// finishedWithin is how close to the end (in seconds) a saved position
// counts as having finished the episode.
const finishedWithin = 5.0

// handleResume returns the unlocked kid's saved position for one episode
// (0 when none). The player restarts only positions above 5 seconds.
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	if sess.grownup {
		writeJSON(w, http.StatusOK, map[string]float64{"pos": 0})
		return
	}
	if !s.requireLibrary(w) {
		return
	}
	pos, err := s.deps.Library.Resume(r.Context(), sess.kid, core.EpisodeID(r.PathValue("episode")))
	if err != nil {
		log.Printf("kid: resume: %v", err)
		writeError(w, 500, "Something went wrong. Try again.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]float64{"pos": pos})
}
