package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/edementhon/kids-shelf/internal/budget"
	"github.com/edementhon/kids-shelf/internal/core"
	"github.com/edementhon/kids-shelf/internal/firewall"
	"github.com/edementhon/kids-shelf/internal/lanaddr"
	"github.com/edementhon/kids-shelf/internal/library"
	"github.com/edementhon/kids-shelf/internal/update"
)

// go:embed is handled in init-free form: the admin area's static files live
// under static/admin and are embedded from this file so the area owns them.
//
//go:embed static/admin
var adminFiles embed.FS

// adminPinCookie is the cookie POST /api/admin/pin (or the first-run POST
// /api/admin/pin/setup) issues once the parent PIN verifies; every
// /api/admin route of the parent page requires it. The parent page always
// has a PIN: the kids can see the Grown-ups link, so with no PIN set the
// gate answers "make one" instead of opening. The value is a random
// server-side session token (adminSessions), never the PIN itself, so the
// raw PIN cannot be replayed as a cookie.
const adminPinCookie = "ks_admin_pin"

// adminSessionLifetime is how long a verified dashboard session lasts.
const adminSessionLifetime = 12 * time.Hour

// adminSessions is the in-memory ledger of verified dashboard sessions: the
// cookie holds the opaque token, the ledger holds its expiry. A restart
// forgets every session (the parent simply enters the PIN again), and a PIN
// change or clear drops them all.
var adminSessions = newAdminSessionLedger()

type adminSession struct {
	expires time.Time
}

type adminSessionLedger struct {
	mu   sync.Mutex
	toks map[string]adminSession
}

func newAdminSessionLedger() *adminSessionLedger {
	return &adminSessionLedger{toks: make(map[string]adminSession)}
}

func (l *adminSessionLedger) put(token string, s adminSession) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.toks[token] = s
}

// valid reports whether token is a live dashboard session, dropping it when
// expired.
func (l *adminSessionLedger) valid(token string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.toks[token]
	if !ok {
		return false
	}
	if now.After(s.expires) {
		delete(l.toks, token)
		return false
	}
	return true
}

func (l *adminSessionLedger) drop(token string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.toks, token)
}

// clear invalidates every dashboard session (a PIN change or clear).
func (l *adminSessionLedger) clear() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.toks = make(map[string]adminSession)
}

// Plain-words messages the dashboard shows; internal details never reach a
// page (the writeError contract).
const (
	msgAdminServiceDown   = "This copy is not running the parent dashboard service."
	msgLibraryServiceDown = "This copy is not running the library service."
	msgEnterParentPIN     = "Enter the parent PIN to see this page."
	msgMakeParentPIN      = "Make a parent PIN to open this page."
	msgParentPINExists    = "There is already a parent PIN. Enter it to open this page."
	msgParentPINNeeded    = "The parent page always needs a PIN."
	msgPINFourDigits      = "The PIN must be exactly 4 digits."
	msgResetOnComputer    = "Reset the PIN on the computer kids-shelf runs on."
	msgWrongPIN           = "That PIN is not right. Try again."
	msgNoSuchKid          = "No such kid."
	msgNoSuchShow         = "That show was not found."
	msgKidNameExists      = "A kid with that name already exists."
	msgNegativeMinutes    = "Minutes cannot be negative."
	msgMinutesNumber      = "Type a number of minutes, like 30."
	msgSaveFailed         = "That could not be saved."
	msgUpdatesOff         = "This copy does not update itself."
	msgFirewallDenied     = "Windows did not get permission. Try again and choose Yes."
	msgRemovalUnavailable = "Removal is not available in this copy."
	// msgFinishingUpdate answers Quit and Remove during an unconfirmed
	// update trial, so a parent cannot turn a healthy update into a
	// rejected one (PLAN 1a "Install, reinstall, remove").
	msgFinishingUpdate = "One moment: kids-shelf is finishing an update."
)

func init() {
	register(func(s *Server) {
		m := s.mux
		m.HandleFunc("GET /admin", s.handleAdminPage)
		m.HandleFunc("GET /admin/static/", s.handleAdminStatic)
		m.HandleFunc("GET /api/admin/state", s.handleAdminState)
		m.HandleFunc("GET /api/admin/address", s.handleAdminAddress)
		m.HandleFunc("GET /api/admin/problems", s.handleAdminProblems)
		m.HandleFunc("POST /api/admin/firewall/allow", s.handleAdminFirewallAllow)
		m.HandleFunc("POST /api/admin/kids", s.handleAdminCreateKid)
		m.HandleFunc("POST /api/admin/kids/{id}/rename", s.handleAdminRenameKid)
		m.HandleFunc("POST /api/admin/kids/{id}/pin", s.handleAdminKidPIN)
		m.HandleFunc("POST /api/admin/kids/{id}/minutes", s.handleAdminKidMinutes)
		m.HandleFunc("DELETE /api/admin/kids/{id}", s.handleAdminRemoveKid)
		m.HandleFunc("POST /api/admin/adjust", s.handleAdminAdjust)
		m.HandleFunc("POST /api/admin/bin", s.handleAdminSetBin)
		m.HandleFunc("POST /api/admin/pin", s.handleAdminPIN)
		m.HandleFunc("GET /api/admin/pin/status", s.handleAdminPINStatus)
		m.HandleFunc("POST /api/admin/pin/setup", s.handleAdminPINSetup)
		m.HandleFunc("POST /api/admin/pin/reset", s.handleAdminPINReset)
		m.HandleFunc("POST /api/admin/logout", s.handleAdminLogout)
		m.HandleFunc("POST /api/admin/settings/grownup", s.handleAdminGrownupPIN)
		m.HandleFunc("POST /api/admin/settings/dashboard", s.handleAdminDashboardPIN)
		m.HandleFunc("POST /api/admin/settings/counter", s.handleAdminCounter)
		m.HandleFunc("POST /api/admin/settings/default", s.handleAdminDefaultMinutes)
		m.HandleFunc("POST /api/admin/settings/budgets", s.handleAdminBudgets)
		m.HandleFunc("POST /api/admin/settings/safari", s.handleAdminSafariScreens)
		m.HandleFunc("POST /api/admin/settings/hwencode", s.handleAdminHWEncode)
		m.HandleFunc("POST /api/admin/update/restart", s.handleAdminUpdateRestart)
		m.HandleFunc("GET /api/admin/remove", s.handleAdminRemoveInfo)
		m.HandleFunc("POST /api/admin/remove", s.handleAdminRemove)
		m.HandleFunc("POST /api/admin/quit", s.handleAdminQuit)
	})
}

// ─── dashboard PIN gate ────────────────────────────────────────────────────

// dashboardPinPath is the file holding the parent PIN. It is a
// single small JSON file owned by this area, not a database table: the web
// package has no store handle in Deps, and the budget package owns every
// settings table (its household row holds the grown-up watch-together PIN,
// which stays separate per core.Kids).
func (s *Server) dashboardPinPath() string {
	dir := s.deps.DataDir
	if dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "dashboard-pin.json")
}

// readDashboardPIN returns the stored parent PIN; "" means none is set yet
// (a fresh install, or a reset on the computer itself; a missing or
// unreadable file reads as ""), and the parent page then asks for one.
func (s *Server) readDashboardPIN() string {
	b, err := os.ReadFile(s.dashboardPinPath())
	if err != nil {
		return ""
	}
	var f struct {
		PIN string `json:"pin"`
	}
	if json.Unmarshal(b, &f) != nil {
		return ""
	}
	return f.PIN
}

// writeDashboardPIN stores the dashboard PIN atomically (temp file + rename)
// and removes the file when the PIN is cleared (only the local reset does).
func (s *Server) writeDashboardPIN(pin string) error {
	path := s.dashboardPinPath()
	if pin == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	b, err := json.Marshal(struct {
		PIN string `json:"pin"`
	}{PIN: pin})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// pinsEqual compares two secrets in constant time by comparing their sha-256
// digests, like the budget package does for the kid and grown-up PINs.
func pinsEqual(a, b string) bool {
	da := sha256.Sum256([]byte(a))
	db := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(da[:], db[:]) == 1
}

// dashboardPinMu serializes the check-then-write steps on the parent PIN
// file (first-run setup, a change in Settings, a local reset), so two
// browsers racing to make the first PIN cannot both win.
var dashboardPinMu sync.Mutex

// adminGateError is the 401 body when no parent PIN exists yet: Setup tells
// the page to show the "make a parent PIN" form instead of the PIN lock.
type adminGateError struct {
	Error string `json:"error"`
	Setup bool   `json:"setup"`
}

// writeSetupNeeded answers 401 with setup true: no parent PIN is set, so
// nothing behind the gate opens until one is made.
func writeSetupNeeded(w http.ResponseWriter) {
	writeJSON(w, http.StatusUnauthorized, adminGateError{Error: msgMakeParentPIN, Setup: true})
}

// requireAdminPIN gates one parent-page route: the request must carry the
// session cookie that POST /api/admin/pin (or /api/admin/pin/setup) issued.
// With no parent PIN set it answers 401 with setup true, so a fresh install
// leads with "make a parent PIN" and the page is never open. It writes the
// 401 plain message and returns false when the gate is closed; handlers
// (including the /admin/content area) call it first and return when it
// reports false.
func (s *Server) requireAdminPIN(w http.ResponseWriter, r *http.Request) bool {
	if s.readDashboardPIN() == "" {
		writeSetupNeeded(w)
		return false
	}
	ck, err := r.Cookie(adminPinCookie)
	if err != nil || !adminSessions.valid(ck.Value, s.now()) {
		writeError(w, http.StatusUnauthorized, msgEnterParentPIN)
		return false
	}
	return true
}

// clientIsLocal reports whether the real client is this computer itself: a
// loopback address after clientIP has resolved the launcher proxy's trusted
// X-KS-Client header. A tablet on the home network is never local, and a
// spoofed X-KS-Client from anything but the proxy is ignored by clientIP.
func (s *Server) clientIsLocal(r *http.Request) bool {
	ip := net.ParseIP(s.clientIP(r))
	return ip != nil && ip.IsLoopback()
}

// issueAdminCookie marks the request's client as a parent who verified the
// dashboard PIN: a fresh random session token goes in the cookie and into
// the in-memory ledger. The cookie is HttpOnly, SameSite=Strict and expires
// with the session, so a leaked or guessed value cannot be replayed beyond
// it and the PIN itself never sits in a cookie.
func (s *Server) issueAdminCookie(w http.ResponseWriter) {
	token, err := newSessionToken()
	if err != nil {
		writeError(w, 500, "Something went wrong. Try again.")
		return
	}
	expires := s.now().Add(adminSessionLifetime)
	adminSessions.put(token, adminSession{expires: expires})
	http.SetCookie(w, &http.Cookie{
		Name:     adminPinCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Expires:  expires,
		MaxAge:   int(adminSessionLifetime.Seconds()),
	})
}

func (s *Server) clearAdminCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminPinCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

// validPIN reports whether pin is exactly four digits; empty is accepted
// only when the caller allows clearing the PIN.
func validPIN(pin string, emptyOK bool) bool {
	if pin == "" {
		return emptyOK
	}
	if len(pin) != 4 {
		return false
	}
	for i := range 4 {
		if pin[i] < '0' || pin[i] > '9' {
			return false
		}
	}
	return true
}

// validName reports whether name is a usable kid/profile name: 1-40 trimmed
// letters, counted as characters (an accented letter or an emoji is one),
// not bytes.
func validName(name string) bool {
	name = strings.TrimSpace(name)
	return name != "" && utf8.RuneCountInString(name) <= 40
}

// takenKidName reports whether err refuses a name another kid already has
// (any letter case), and that kid's name as stored.
func takenKidName(err error) (string, bool) {
	var taken *budget.KidNameTakenError
	if !errors.As(err, &taken) {
		return "", false
	}
	return taken.Name, true
}

// msgKidCalled is the plain refusal for a name another kid already has.
func msgKidCalled(name string) string {
	return "A kid called " + name + " already exists. Pick another name."
}

// adminErrStatus picks the HTTP status for a service error surfaced to the
// dashboard.
func adminErrStatus(err error) int {
	switch {
	case errors.Is(err, budget.ErrNoSuchKid):
		return http.StatusNotFound
	case errors.As(err, new(*budget.KidNameTakenError)):
		return http.StatusConflict
	case errors.Is(err, library.ErrNotFound):
		return http.StatusNotFound
	case strings.Contains(err.Error(), "UNIQUE constraint failed: kids.name"):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

// plainAdminError turns a service error into the plain words a parent can
// read; unknown errors collapse to a generic sentence.
func plainAdminError(err error) string {
	if have, ok := takenKidName(err); ok {
		return msgKidCalled(have)
	}
	switch {
	case errors.Is(err, budget.ErrNoSuchKid):
		return msgNoSuchKid
	case errors.Is(err, library.ErrNotFound):
		return msgNoSuchShow
	case strings.Contains(err.Error(), "UNIQUE constraint failed: kids.name"):
		return msgKidNameExists
	case strings.Contains(err.Error(), "negative"):
		return msgNegativeMinutes
	default:
		return msgSaveFailed
	}
}

// serviceError writes err as a plain-words response with the status suited
// to it.
func serviceError(w http.ResponseWriter, err error) {
	writeError(w, adminErrStatus(err), plainAdminError(err))
}

// ─── page and static files ─────────────────────────────────────────────────

// handleAdminPage serves the parent page shell to everyone: the page shows
// the "make a parent PIN" form (no PIN yet) or the PIN lock until a session
// exists; all data is behind requireAdminPIN, never the shell.
func (s *Server) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	page, err := adminPage()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "The parent page is missing from this copy.")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, page)
}

// adminPage is the parent page shell with where to reopen kids-shelf
// filled in and its asset URLs versioned (see versionAssets).
var adminPage = sync.OnceValues(func() (string, error) {
	b, err := adminFiles.ReadFile("static/admin/index.html")
	if err != nil {
		return "", err
	}
	page := strings.ReplaceAll(string(b), "__KS_REOPEN__", reopenWhere())
	return versionAssets(page, adminFiles, "static/admin", "/admin/static/")
})

// versionAssets returns page with every quoted URL prefix+name of a file
// in dir of files given a content hash in its query, like practiceAsset:
// the page shells are never cached but their scripts and stylesheets are
// (handleAdminStatic, handleContentStatic), so without the hash a newly
// upgraded page could run with the previous build's cached script.
func versionAssets(page string, files embed.FS, dir, prefix string) (string, error) {
	entries, err := files.ReadDir(dir)
	if err != nil {
		return "", err
	}
	pairs := make([]string, 0, 2*len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := files.ReadFile(dir + "/" + e.Name())
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(b)
		u := prefix + e.Name()
		pairs = append(pairs, `"`+u+`"`, `"`+u+"?v="+hex.EncodeToString(sum[:6])+`"`)
	}
	return strings.NewReplacer(pairs...).Replace(page), nil
}

// reopenWhere names where a parent opens kids-shelf again on this computer,
// for the Quit confirmation, the "stopped" screen and the problems banner:
// a Mac has the Dock and Applications, not a desktop icon.
func reopenWhere() string {
	switch runtime.GOOS {
	case "darwin":
		return "the Dock or the Applications folder"
	case "windows":
		return "the desktop icon or the Start menu"
	default:
		return "the desktop icon or the applications menu"
	}
}

// handleAdminStatic serves the area's embedded files (admin.js, admin.css,
// test-av.mp4) with no directory listing. The page asks for them with a
// content hash in the query (see versionAssets), which is ignored here.
func (s *Server) handleAdminStatic(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/admin/static/")
	if name == "" || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	b, err := adminFiles.ReadFile("static/admin/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ctype := "application/octet-stream"
	switch {
	case strings.HasSuffix(name, ".js"):
		ctype = "text/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		ctype = "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".html"):
		ctype = "text/html; charset=utf-8"
	case strings.HasSuffix(name, ".mp4"):
		ctype = "video/mp4"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write(b)
}

// ─── state ─────────────────────────────────────────────────────────────────

type adminKid struct {
	ID    string  `json:"id"`
	Name  string  `json:"name"`
	Today float64 `json:"today"`
	Saved float64 `json:"saved"`
	Daily float64 `json:"daily"`
}

type adminWatch struct {
	ID      int64   `json:"id"`
	Kid     string  `json:"kid"`
	KidName string  `json:"kid_name"`
	At      string  `json:"at"` // server-local "2006-01-02T15:04", no zone: the JS renders it as local time
	Show    string  `json:"show"`
	Title   string  `json:"title"`
	Cost    float64 `json:"cost"`
	Free    bool    `json:"free"`
}

type adminShow struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Episodes int    `json:"episodes"`
}

type adminBins struct {
	Fun      []adminShow `json:"fun"`
	Learning []adminShow `json:"learning"`
	Free     []adminShow `json:"free"`
}

type adminSettings struct {
	Counter string `json:"counter,omitempty"` // absent when practice is not running
	Address string `json:"address"`
	// NameAddress is the same page by the name the computer announces on
	// the home network ("http://kids-shelf.local:8070/"), or "" when it
	// announces none.
	NameAddress string `json:"name_address"`
	QRSVG       string `json:"qr_svg"` // the IP address with connectQuery
	// SafariScreens is the "some screens are iPads, iPhones or Macs using
	// Safari" setting: while on, remux-only files are converted to MP4 in
	// the background after they appear on the shelf.
	SafariScreens bool `json:"safari_screens"`
	// HWEncode is the "use the graphics chip to convert videos" setting.
	HWEncode bool `json:"hw_encode"`
	// VideoEncoder is the parent page's "Video conversion uses:" name:
	// the chosen hardware encoder, "this computer's processor" for
	// software, or "not checked yet" before the first selection.
	VideoEncoder string `json:"video_encoder"`
	// GrownupPINSet says whether a watch-together PIN exists, so the
	// Settings row can show "A PIN is set" or "No PIN yet" (never the PIN).
	GrownupPINSet bool `json:"grownup_pin_set"`
}

type adminUpdate struct {
	State   string `json:"state"`
	Message string `json:"message"`
	Staged  string `json:"staged"`
	// Applying is true while a staged update is already being set up (the
	// launcher restarts on its own), so the page hides Restart to update.
	Applying bool `json:"applying"`
}

type adminState struct {
	Kids           []adminKid    `json:"kids"`
	DefaultMinutes float64       `json:"default_minutes"`
	BudgetsOn      bool          `json:"budgets_on"`
	WatchLog       []adminWatch  `json:"watch_log"`
	Bins           adminBins     `json:"bins"`
	Settings       adminSettings `json:"settings"`
	Updates        adminUpdate   `json:"updates"`
}

// adminStateData assembles everything the dashboard renders in one fetch, the
// way the legacy /api/admstate did.
func (s *Server) adminStateData(ctx context.Context, r *http.Request) (adminState, error) {
	var st adminState
	b, l := s.deps.Budget, s.deps.Library

	kids, err := b.ListKids(ctx)
	if err != nil {
		return st, err
	}
	st.DefaultMinutes, err = b.DefaultMinutes(ctx)
	if err != nil {
		return st, err
	}
	st.BudgetsOn, err = b.BudgetsOn(ctx)
	if err != nil {
		return st, err
	}
	// Lists encode as [] (not null) so a fresh install's page can iterate them.
	st.Kids, st.WatchLog = []adminKid{}, []adminWatch{}
	kidNames := make(map[string]string, len(kids))
	for _, k := range kids {
		kidNames[string(k.ID)] = k.Name
		today, saved, err := b.Balance(ctx, k.ID, budget.DefaultCounter)
		if err != nil {
			return st, err
		}
		daily, err := b.DailyMinutes(ctx, k.ID)
		if err != nil {
			return st, err
		}
		st.Kids = append(st.Kids, adminKid{
			ID: string(k.ID), Name: k.Name,
			Today: today, Saved: saved, Daily: daily,
		})
	}

	// Watch log (Library.WatchLog): joined to kid names and show names.
	entries, err := l.WatchLog(ctx)
	if err != nil {
		return st, err
	}
	for _, e := range entries {
		we := adminWatch{
			ID: e.ID, Kid: string(e.Kid), KidName: kidNames[string(e.Kid)],
			At:    e.At.Local().Format("2006-01-02T15:04"),
			Title: e.Title, Cost: e.Cost, Free: e.Free,
		}
		if ep, err := l.Episode(ctx, e.Episode); err == nil {
			we.Show = ep.ShowName
		}
		st.WatchLog = append(st.WatchLog, we)
	}

	// Bins (Library.ListShows) grouped into the three columns.
	shows, err := l.ListShows(ctx)
	if err != nil {
		return st, err
	}
	// Empty columns must encode as [] (not null) so the page can iterate them.
	st.Bins.Fun, st.Bins.Learning, st.Bins.Free = []adminShow{}, []adminShow{}, []adminShow{}
	for _, sh := range shows {
		card := adminShow{ID: string(sh.ID), Name: sh.Name, Episodes: sh.Episodes}
		switch sh.Bin {
		case library.BinLearning:
			st.Bins.Learning = append(st.Bins.Learning, card)
		case library.BinFree:
			st.Bins.Free = append(st.Bins.Free, card)
		default:
			st.Bins.Fun = append(st.Bins.Fun, card)
		}
	}

	// Settings.
	if p := s.deps.Practice; p != nil {
		st.Settings.Counter, _ = p.Counter()
	}
	st.Settings.Address = s.adminAddress(r)
	st.Settings.NameAddress = s.nameAddress(r)
	if svg, err := addressQR(st.Settings.Address); err == nil {
		st.Settings.QRSVG = svg
	}
	if on, err := b.SafariScreens(ctx); err == nil {
		st.Settings.SafariScreens = on
	}
	if on, err := b.HWEncode(ctx); err == nil {
		st.Settings.HWEncode = on
	}
	if s.deps.VideoEncoder != nil {
		st.Settings.VideoEncoder = s.deps.VideoEncoder()
	}
	st.Settings.GrownupPINSet = b.GrownupPINSet(ctx)
	// Updates (may be nil: plain words tell the parent this copy has none).
	st.Updates.Message = msgUpdatesOff
	if s.deps.Updates != nil {
		u := s.deps.Updates.Status()
		st.Updates.State = u.State
		st.Updates.Staged = u.Staged
		st.Updates.Message = plainUpdateMessage(u)
		st.Updates.Applying = updateApplying(u)
	}
	return st, nil
}

// updateApplying reports whether a staged update is already being applied
// (the updater's trial state), when there is nothing left to restart for.
func updateApplying(u update.Status) bool {
	return u.State == update.StateStaged && strings.Contains(u.Message, "being applied")
}

// plainUpdateMessage turns the updater's status into words a parent can act
// on: what happened and what (if anything) to do next. The updater's own
// words are terse and technical ("checksum", "signed", "staged"); the
// failure reasons are matched to the few plain cases a family can act on.
func plainUpdateMessage(u update.Status) string {
	switch u.State {
	case update.StateOff:
		// No signing key was built into this copy: updates genuinely
		// cannot run.
		return msgUpdatesOff
	case update.StateChecking:
		return "Looking for a new version of kids-shelf."
	case update.StateIdle:
		return "kids-shelf is up to date."
	case update.StateStaged:
		if updateApplying(u) {
			return "A new version is being set up. kids-shelf restarts on its own, and this page comes back when it is ready."
		}
		return "A new version (" + u.Staged + ") is ready. Press Restart to update to start using it. Videos stop for a moment while kids-shelf restarts."
	case update.StateFailed:
		m := u.Message
		switch {
		case strings.Contains(m, "went back to"):
			return "The new version did not start properly, so kids-shelf went back to the one it had before. Nothing else changed."
		case strings.Contains(m, "disk space"):
			return "The update needs more free space on this computer. Remove some videos; kids-shelf tries again tomorrow."
		case strings.Contains(m, "cannot be written"):
			return "kids-shelf could not save the new version on this computer. It tries again tomorrow."
		case strings.Contains(m, "not newer"):
			return "kids-shelf is up to date."
		case strings.Contains(m, "could not check"):
			return "kids-shelf could not reach the update server. Check that this computer is on the internet; it tries again tomorrow."
		default:
			// Bad signature, bad checksum, a mismatched or unusable release:
			// the download was not used, nothing changed.
			return "The new version that was downloaded was damaged, so it was not used. Nothing changed; kids-shelf tries again tomorrow."
		}
	}
	return "kids-shelf is up to date."
}

func (s *Server) handleAdminState(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	if s.deps.Budget == nil {
		writeError(w, http.StatusServiceUnavailable, msgAdminServiceDown)
		return
	}
	if s.deps.Library == nil {
		writeError(w, http.StatusServiceUnavailable, msgLibraryServiceDown)
		return
	}
	st, err := s.adminStateData(r.Context(), r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "The parent page could not be loaded.")
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handleAdminAddress returns the LAN address, the announced-name address
// and the QR code on their own, used by the Settings panel and by tests.
func (s *Server) handleAdminAddress(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	addr := s.adminAddress(r)
	svg, err := addressQR(addr)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "The QR code could not be drawn.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"address": addr, "name_address": s.nameAddress(r), "qr_svg": svg})
}

// ─── helpers and firewall problems ────────────────────────────────────────

// handleAdminProblems reports the things that keep the shelf from working
// for the family, so the dashboard can show one banner at the top: a
// problem with the bundled helper programs (missing or broken binaries)
// and the firewall state. Firewall is omitted when this copy has no
// firewall to manage; helpers is "" when there is no helper check.
func (s *Server) handleAdminProblems(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	problems := map[string]any{"helpers": "", "helpers_waiting": false}
	if s.deps.HelpersProblem != nil {
		problems["helpers"] = s.deps.HelpersProblem()
	}
	if s.deps.HelpersWaiting != nil {
		problems["helpers_waiting"] = s.deps.HelpersWaiting()
	}
	if s.deps.Firewall != nil {
		st := s.deps.Firewall.Status(r.Context())
		problems["firewall"] = map[string]any{
			"blocked": st.Blocked,
			"unknown": st.Unknown,
			"detail":  st.Detail,
		}
	}
	writeJSON(w, http.StatusOK, problems)
}

// handleAdminFirewallAllow asks Windows for permission to let other devices
// in (the "Let tablets in" button) and answers with the fresh status, so
// the banner can clear in the same round trip. A declined prompt is its own
// plain message; anything else is the generic save failure.
func (s *Server) handleAdminFirewallAllow(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	fw := s.deps.Firewall
	if fw == nil {
		writeError(w, http.StatusServiceUnavailable, msgAdminServiceDown)
		return
	}
	if err := fw.Allow(r.Context()); err != nil {
		if errors.Is(err, firewall.ErrDeclined) {
			writeError(w, http.StatusBadRequest, msgFirewallDenied)
			return
		}
		serviceError(w, err)
		return
	}
	st := fw.Status(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"firewall": map[string]any{
			"blocked": st.Blocked,
			"unknown": st.Unknown,
			"detail":  st.Detail,
		},
	})
}

// ─── kids ──────────────────────────────────────────────────────────────────

func (s *Server) handleAdminCreateKid(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	b := s.deps.Budget
	if b == nil {
		writeError(w, http.StatusServiceUnavailable, msgAdminServiceDown)
		return
	}
	var req struct {
		Name string `json:"name"`
		PIN  string `json:"pin"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "That didn't look right. Try again.")
		return
	}
	name := strings.TrimSpace(req.Name)
	if !validName(name) {
		writeError(w, http.StatusBadRequest, "Give the kid a name with up to 40 letters.")
		return
	}
	if !validPIN(req.PIN, false) {
		writeError(w, http.StatusBadRequest, "The PIN must be exactly 4 digits.")
		return
	}
	// AddKid, not migrate's idempotent CreateKid: a name that is already a
	// kid's (any letter case) is refused, so the parent is never told "added"
	// while the new PIN is quietly dropped.
	id, err := b.AddKid(r.Context(), name, req.PIN)
	if err != nil {
		if have, ok := takenKidName(err); ok {
			writeError(w, http.StatusConflict, msgKidCalled(have)+" To give "+have+" a new PIN, use ⋯ on "+have+"'s card.")
			return
		}
		serviceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": string(id)})
}

func (s *Server) handleAdminRenameKid(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	b := s.deps.Budget
	if b == nil {
		writeError(w, http.StatusServiceUnavailable, msgAdminServiceDown)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "That didn't look right. Try again.")
		return
	}
	name := strings.TrimSpace(req.Name)
	if !validName(name) {
		writeError(w, http.StatusBadRequest, "Give the kid a name with up to 40 letters.")
		return
	}
	if err := b.RenameKid(r.Context(), core.KidID(r.PathValue("id")), name); err != nil {
		serviceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAdminKidPIN(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	b := s.deps.Budget
	if b == nil {
		writeError(w, http.StatusServiceUnavailable, msgAdminServiceDown)
		return
	}
	var req struct {
		PIN string `json:"pin"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "That didn't look right. Try again.")
		return
	}
	if !validPIN(req.PIN, false) {
		writeError(w, http.StatusBadRequest, "The PIN must be exactly 4 digits.")
		return
	}
	if err := b.SetKidPIN(r.Context(), core.KidID(r.PathValue("id")), req.PIN); err != nil {
		serviceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAdminKidMinutes(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	b := s.deps.Budget
	if b == nil {
		writeError(w, http.StatusServiceUnavailable, msgAdminServiceDown)
		return
	}
	// Minutes is a pointer so a missing or null value (an emptied box) is
	// refused instead of silently saving 0 minutes a day.
	var req struct {
		Minutes *float64 `json:"minutes"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, msgMinutesNumber)
		return
	}
	if req.Minutes == nil {
		writeError(w, http.StatusBadRequest, msgMinutesNumber)
		return
	}
	if *req.Minutes < 0 {
		writeError(w, http.StatusBadRequest, msgNegativeMinutes)
		return
	}
	if err := b.SetDailyMinutes(r.Context(), core.KidID(r.PathValue("id")), *req.Minutes); err != nil {
		serviceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAdminRemoveKid(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	b := s.deps.Budget
	if b == nil {
		writeError(w, http.StatusServiceUnavailable, msgAdminServiceDown)
		return
	}
	if err := b.RemoveKid(r.Context(), core.KidID(r.PathValue("id"))); err != nil {
		serviceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ─── minutes ───────────────────────────────────────────────────────────────

func (s *Server) handleAdminAdjust(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	b := s.deps.Budget
	if b == nil {
		writeError(w, http.StatusServiceUnavailable, msgAdminServiceDown)
		return
	}
	var req struct {
		Kid   string  `json:"kid"`
		Delta float64 `json:"delta"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "That didn't look right. Try again.")
		return
	}
	if req.Kid == "" {
		writeError(w, http.StatusBadRequest, msgNoSuchKid)
		return
	}
	// Adjust clamps at zero (allowNegative=false): the minus buttons on the
	// page stop at 0, so a parent can never push a kid into debt by
	// overshooting.
	today, saved, err := b.Adjust(r.Context(), core.KidID(req.Kid), budget.DefaultCounter, req.Delta, false)
	if err != nil {
		serviceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]float64{"today": today, "saved": saved})
}

// ─── bins ──────────────────────────────────────────────────────────────────

func (s *Server) handleAdminSetBin(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	l := s.deps.Library
	if l == nil {
		writeError(w, http.StatusServiceUnavailable, msgLibraryServiceDown)
		return
	}
	var req struct {
		Show string `json:"show"`
		Bin  string `json:"bin"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "That didn't look right. Try again.")
		return
	}
	switch req.Bin {
	case library.BinFun, library.BinLearning, library.BinFree:
	default:
		writeError(w, http.StatusBadRequest, "Pick Fun, Learning or Free.")
		return
	}
	if err := l.SetBin(r.Context(), core.ShowID(req.Show), req.Bin); err != nil {
		serviceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ─── PIN entry and logout ──────────────────────────────────────────────────

// handleAdminPIN verifies the parent PIN and issues the session cookie.
// It is one of the few /api/admin routes the gate leaves open, so a parent
// can always get in (or back in after a new PIN was set elsewhere). Wrong
// tries feed the dashboard throttle (per client IP). With no PIN set yet
// it answers like the gate: 401 with setup true.
func (s *Server) handleAdminPIN(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PIN string `json:"pin"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Type your PIN.")
		return
	}
	stored := s.readDashboardPIN()
	if stored == "" {
		writeSetupNeeded(w)
		return
	}
	if s.pinLocked(w, r, realmDashboard, "") {
		return
	}
	if !pinsEqual(req.PIN, stored) {
		if s.pinWrong(w, realmDashboard, s.clientIP(r), "") {
			return
		}
		writeError(w, http.StatusUnauthorized, msgWrongPIN)
		return
	}
	pinThrottle.ok(realmDashboard, s.clientIP(r), "")
	s.issueAdminCookie(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// adminPINStatus is the GET /api/admin/pin/status body: whether a parent
// PIN exists and whether this browser runs on the kids-shelf computer
// itself (where the PIN can be reset). Nothing else is revealed.
type adminPINStatus struct {
	Set   bool `json:"set"`
	Local bool `json:"local"`
}

// handleAdminPINStatus answers without a session: the lock and setup forms
// need it before anyone is signed in.
func (s *Server) handleAdminPINStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, adminPINStatus{
		Set:   s.readDashboardPIN() != "",
		Local: s.clientIsLocal(r),
	})
}

// handleAdminPINSetup makes the first parent PIN. It works only while no PIN
// is set (409 otherwise, so it can never replace a PIN), goes through the
// same same-origin middleware and dashboard throttle as POST /api/admin/pin,
// and issues the session cookie so the parent continues straight in.
func (s *Server) handleAdminPINSetup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PIN string `json:"pin"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, msgPINFourDigits)
		return
	}
	if s.pinLocked(w, r, realmDashboard, "") {
		return
	}
	dashboardPinMu.Lock()
	defer dashboardPinMu.Unlock()
	if s.readDashboardPIN() != "" {
		writeError(w, http.StatusConflict, msgParentPINExists)
		return
	}
	if !validPIN(req.PIN, false) {
		writeError(w, http.StatusBadRequest, msgPINFourDigits)
		return
	}
	if err := s.writeDashboardPIN(req.PIN); err != nil {
		writeError(w, http.StatusInternalServerError, msgSaveFailed)
		return
	}
	pinThrottle.ok(realmDashboard, s.clientIP(r), "")
	s.issueAdminCookie(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleAdminPINReset clears a forgotten parent PIN, but only for a browser
// on the kids-shelf computer itself (clientIsLocal); anyone else gets 403.
// Every session ends, and the next visit leads with the setup form. The
// computer's own wrong tries are forgotten too, so a parent who guessed a
// few times before resetting can make the new PIN right away.
func (s *Server) handleAdminPINReset(w http.ResponseWriter, r *http.Request) {
	if !s.clientIsLocal(r) {
		writeError(w, http.StatusForbidden, msgResetOnComputer)
		return
	}
	dashboardPinMu.Lock()
	defer dashboardPinMu.Unlock()
	if err := s.writeDashboardPIN(""); err != nil {
		writeError(w, http.StatusInternalServerError, msgSaveFailed)
		return
	}
	adminSessions.clear()
	pinThrottle.ok(realmDashboard, s.clientIP(r), "")
	s.clearAdminCookie(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(adminPinCookie); err == nil {
		adminSessions.drop(c.Value)
	}
	s.clearAdminCookie(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ─── settings ──────────────────────────────────────────────────────────────

// handleAdminGrownupPIN sets or clears the watch-together PIN (playback with
// it is free and unmetered). Empty clears it.
func (s *Server) handleAdminGrownupPIN(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	b := s.deps.Budget
	if b == nil {
		writeError(w, http.StatusServiceUnavailable, msgAdminServiceDown)
		return
	}
	var req struct {
		PIN string `json:"pin"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "That didn't look right. Try again.")
		return
	}
	if !validPIN(req.PIN, true) {
		writeError(w, http.StatusBadRequest, msgPINFourDigits)
		return
	}
	if err := b.SetGrownupPIN(r.Context(), req.PIN); err != nil {
		serviceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleAdminDashboardPIN changes the parent PIN; it can never clear it (an
// empty PIN answers 400: the parent page always needs one). Changing it
// revokes every outstanding session (the PIN gate means "verified since
// this PIN was set") and reissues the cookie so the parent's current
// browser stays in.
func (s *Server) handleAdminDashboardPIN(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	var req struct {
		PIN string `json:"pin"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "That didn't look right. Try again.")
		return
	}
	if req.PIN == "" {
		writeError(w, http.StatusBadRequest, msgParentPINNeeded)
		return
	}
	if !validPIN(req.PIN, false) {
		writeError(w, http.StatusBadRequest, msgPINFourDigits)
		return
	}
	dashboardPinMu.Lock()
	defer dashboardPinMu.Unlock()
	if err := s.writeDashboardPIN(req.PIN); err != nil {
		writeError(w, http.StatusInternalServerError, msgSaveFailed)
		return
	}
	adminSessions.clear()
	s.issueAdminCookie(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleAdminCounter points practice earnings at a household counter; the
// default is "video" (budget.DefaultCounter).
func (s *Server) handleAdminCounter(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	p := s.deps.Practice
	if p == nil {
		writeError(w, http.StatusServiceUnavailable, "This copy is not running the practice service.")
		return
	}
	var req struct {
		Counter string `json:"counter"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "That didn't look right. Try again.")
		return
	}
	c := strings.TrimSpace(req.Counter)
	if c == "" || len(c) > 32 || strings.ContainsAny(c, "/\\") {
		writeError(w, http.StatusBadRequest, "Name the counter with up to 32 letters and numbers.")
		return
	}
	if err := p.SetCounter(r.Context(), c); err != nil {
		writeError(w, http.StatusInternalServerError, msgSaveFailed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAdminDefaultMinutes(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	b := s.deps.Budget
	if b == nil {
		writeError(w, http.StatusServiceUnavailable, msgAdminServiceDown)
		return
	}
	// A pointer, like the per-kid minutes: an emptied box never saves 0.
	var req struct {
		Minutes *float64 `json:"minutes"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, msgMinutesNumber)
		return
	}
	if req.Minutes == nil {
		writeError(w, http.StatusBadRequest, msgMinutesNumber)
		return
	}
	if *req.Minutes < 0 {
		writeError(w, http.StatusBadRequest, msgNegativeMinutes)
		return
	}
	if err := b.SetDefaultMinutes(r.Context(), *req.Minutes); err != nil {
		serviceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAdminBudgets(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	b := s.deps.Budget
	if b == nil {
		writeError(w, http.StatusServiceUnavailable, msgAdminServiceDown)
		return
	}
	var req struct {
		On bool `json:"on"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "That didn't look right. Try again.")
		return
	}
	if err := b.SetBudgetsOn(r.Context(), req.On); err != nil {
		writeError(w, http.StatusInternalServerError, msgSaveFailed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleAdminSafariScreens sets the household "some screens use Safari"
// switch. It applies to videos processed after the change, never
// retroactively, which the settings card's help text says.
func (s *Server) handleAdminSafariScreens(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	b := s.deps.Budget
	if b == nil {
		writeError(w, http.StatusServiceUnavailable, msgAdminServiceDown)
		return
	}
	var req struct {
		On bool `json:"on"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "That didn't look right. Try again.")
		return
	}
	if err := b.SetSafariScreens(r.Context(), req.On); err != nil {
		writeError(w, http.StatusInternalServerError, msgSaveFailed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleAdminHWEncode sets the household "use the graphics chip to convert
// videos" switch. Off forces the processor for every conversion; the
// processor also takes over whenever the graphics chip fails.
func (s *Server) handleAdminHWEncode(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	b := s.deps.Budget
	if b == nil {
		writeError(w, http.StatusServiceUnavailable, msgAdminServiceDown)
		return
	}
	var req struct {
		On bool `json:"on"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "That didn't look right. Try again.")
		return
	}
	if err := b.SetHWEncode(r.Context(), req.On); err != nil {
		writeError(w, http.StatusInternalServerError, msgSaveFailed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ─── updates and quit ──────────────────────────────────────────────────────
func (s *Server) handleAdminUpdateRestart(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	if s.deps.Updates == nil {
		writeError(w, http.StatusServiceUnavailable, msgUpdatesOff)
		return
	}
	s.deps.Updates.RestartToUpdate()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleAdminQuit stops the app cleanly. The response is written before the
// hook runs (from a goroutine a moment later) so the browser reliably gets
// the ok and the page can show that kids-shelf has stopped; the hook is
// nil-safe because a dev copy may have nothing to quit. During an
// unconfirmed update trial it is refused, like Remove: quitting mid-trial
// would turn a healthy update into a rejected one.
func (s *Server) handleAdminQuit(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	if s.trialInProgress() {
		writeError(w, http.StatusConflict, msgFinishingUpdate)
		return
	}
	if s.deps.Quit == nil {
		writeError(w, http.StatusServiceUnavailable, "This copy cannot stop kids-shelf.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	go func() {
		time.Sleep(200 * time.Millisecond)
		s.deps.Quit()
	}()
}

// trialInProgress reports whether this child is an unconfirmed update
// trial, the window in which Quit and Remove must be refused. The answer
// comes from Deps.InTrial (the durable journal), so a trial that has not
// settled yet is refused however long it has run.
func (s *Server) trialInProgress() bool {
	return s.deps.InTrial != nil && s.deps.InTrial()
}

// handleAdminRemoveInfo returns the two folders the removal confirmation
// names - the data directory and the media directory - so the parent sees
// exactly what stays and what goes before confirming.
func (s *Server) handleAdminRemoveInfo(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"data_dir":  s.deps.DataDir,
		"media_dir": s.deps.MediaDir,
	})
}

// handleAdminRemove records a parent's request to remove kids-shelf from
// this computer and stops the app, so the supervisor can hand over to the
// remover (PLAN 1a "Install, reinstall, remove"). Deleting the videos and
// settings is off by default and only happens when the page sends
// deleteData = true; the confirmation names both folders.
func (s *Server) handleAdminRemove(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	if s.trialInProgress() {
		writeError(w, http.StatusConflict, msgFinishingUpdate)
		return
	}
	if s.deps.Remove == nil {
		writeError(w, http.StatusServiceUnavailable, msgRemovalUnavailable)
		return
	}
	var body struct {
		DeleteData bool `json:"deleteData"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, plainAdminError(err))
		return
	}
	if err := s.deps.Remove(body.DeleteData); err != nil {
		serviceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ─── LAN address ───────────────────────────────────────────────────────────

// adminAddress returns "http://<lan-ip>:<port>/", the address other devices
// on the home network use to reach the shelf. The port is the one the
// browser itself used to open this page (the launcher picks 8070 or the next
// free port); the IP is the home-network address lanaddr.Best picks.
func (s *Server) adminAddress(r *http.Request) string {
	ip := lanaddr.Best()
	if ip == "" {
		host := r.Host
		if i := strings.LastIndex(host, ":"); i >= 0 {
			host = host[:i]
		}
		host = strings.Trim(host, "[]")
		if host != "" {
			ip = host
		} else {
			ip = "localhost"
		}
	}
	return addressFor(ip, requestPort(r))
}

// nameAddress returns "http://<name>:<port>/" for the name the supervisor
// announces on the home network (Deps.LANName, kids-shelf.local), on the
// same port as adminAddress, or "" when no name is announced.
func (s *Server) nameAddress(r *http.Request) string {
	if s.deps.LANName == "" {
		return ""
	}
	return addressFor(s.deps.LANName, requestPort(r))
}

// requestPort returns the port the browser used to reach this page, or ""
// when the Host header has none.
func requestPort(r *http.Request) string {
	if _, p, err := net.SplitHostPort(r.Host); err == nil {
		return p
	}
	if i := strings.LastIndex(r.Host, ":"); i >= 0 {
		return r.Host[i+1:]
	}
	return ""
}

func addressFor(host, port string) string {
	u := "http://" + host
	if port != "" {
		u += ":" + port
	}
	return u + "/"
}

// connectQuery ends the address the QR code carries: the kid page opened
// from it tells the parent page that a tablet connected (connect.go), then
// drops it from the address bar.
const connectQuery = "?connect=1"

// addressQR draws the QR code for addr, the IP address (it works without
// the announced name), with connectQuery added.
func addressQR(addr string) (string, error) {
	return qrSVG(addr + connectQuery)
}

// ─── QR codes ──────────────────────────────────────────────────────────────
// qrSVG renders text as a QR code (ISO/IEC 18004: byte mode, error
// correction level M, versions 1-10) and returns an SVG document with the
// standard 4-module quiet zone. It is a small dependency-free encoder: the
// tables below are the published spec parameters, and the placement was
// checked bit-for-bit against an independent encoder for several inputs and
// every mask pattern (see admin_test.go TestQRMatchesReference). Version 10
// holds 213 bytes, far beyond any LAN address.

type qrVer struct {
	version int
	size    int
	blocks  []int // data codewords per block
	ec      int   // error-correction codewords per block
	align   []int // alignment-pattern centers
	cap     int   // byte-mode capacity at level M
}

var qrVersions = []qrVer{
	{1, 21, []int{16}, 10, nil, 14},
	{2, 25, []int{28}, 16, []int{6, 18}, 26},
	{3, 29, []int{44}, 26, []int{6, 22}, 42},
	{4, 33, []int{32, 32}, 18, []int{6, 26}, 62},
	{5, 37, []int{43, 43}, 24, []int{6, 30}, 84},
	{6, 41, []int{27, 27, 27, 27}, 16, []int{6, 34}, 106},
	{7, 45, []int{31, 31, 31, 31}, 18, []int{6, 22, 38}, 122},
	{8, 49, []int{38, 38, 39, 39}, 22, []int{6, 24, 42}, 152},
	{9, 53, []int{36, 36, 36, 37, 37}, 22, []int{6, 26, 46}, 180},
	{10, 57, []int{43, 43, 43, 43, 44}, 26, []int{6, 28, 50}, 213},
}

var (
	gfLog = [256]byte{}
	gfExp = [512]byte{}
)

func init() {
	// GF(2^8) tables with the QR primitive polynomial x^8+x^4+x^3+x^2+1.
	x := byte(1)
	for i := range 255 {
		gfExp[i] = x
		gfLog[x] = byte(i)
		shifted := x << 1
		if x&0x80 != 0 {
			// x·2 exceeds the field: reduce with the primitive polynomial
			// x^8+x^4+x^3+x^2+1 (the 0x100 bit fell off the 8-bit shift).
			x = shifted ^ 0x1d
		} else {
			x = shifted
		}
	}
	for i := 255; i < len(gfExp); i++ {
		gfExp[i] = gfExp[i-255]
	}
}

func gfMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return gfExp[int(gfLog[a])+int(gfLog[b])]
}

// rsEC returns the ec error-correction codewords of data: the remainder of
// data(x)·x^ec divided by the generator polynomial ∏(x − α^i) over GF(2^8).
func rsEC(data []byte, ec int) []byte {
	gen := make([]byte, ec+1)
	gen[0] = 1
	for i := 1; i <= ec; i++ {
		a := gfExp[i-1]
		for j := i; j >= 1; j-- {
			gen[j] ^= gfMul(gen[j-1], a)
		}
	}
	rem := make([]byte, ec)
	for _, b := range data {
		factor := b ^ rem[0]
		copy(rem, rem[1:])
		rem[ec-1] = 0
		if factor != 0 {
			for i := 1; i <= ec; i++ {
				rem[i-1] ^= gfMul(gen[i], factor)
			}
		}
	}
	return rem
}

// qrCodewords packs text into the version's interleaved codeword stream:
// mode 0100 + byte count + bytes, terminated and padded, split into blocks,
// each block's Reed-Solomon codewords appended, then the round-robin
// interleave the spec mandates for multi-block versions.
func qrCodewords(text string, v qrVer) ([]byte, error) {
	if len(text) > v.cap {
		return nil, fmt.Errorf("QR code: %d bytes is too long (fits %d)", len(text), v.cap)
	}
	totalData := 0
	for _, n := range v.blocks {
		totalData += n
	}
	countBits := 8
	if v.version >= 10 {
		countBits = 16
	}
	var bits []byte
	put := func(n, width int) {
		for i := width - 1; i >= 0; i-- {
			if n>>uint(i)&1 == 1 {
				bits = append(bits, 1)
			} else {
				bits = append(bits, 0)
			}
		}
	}
	put(0b0100, 4)
	put(len(text), countBits)
	for i := range len(text) {
		put(int(text[i]), 8)
	}
	for i := 0; i < 4 && len(bits) < totalData*8; i++ {
		bits = append(bits, 0)
	}
	for len(bits)%8 != 0 {
		bits = append(bits, 0)
	}
	data := make([]byte, 0, totalData)
	for i := 0; i < len(bits); i += 8 {
		var b byte
		for j := range 8 {
			b = b<<1 | bits[i+j]
		}
		data = append(data, b)
	}
	for i := 0; len(data) < totalData; i++ {
		if i%2 == 0 {
			data = append(data, 0xEC)
		} else {
			data = append(data, 0x11)
		}
	}
	blocks := make([][]byte, len(v.blocks))
	off := 0
	for i, n := range v.blocks {
		blocks[i] = data[off : off+n]
		off += n
	}
	ecs := make([][]byte, len(blocks))
	for i, b := range blocks {
		ecs[i] = rsEC(b, v.ec)
	}
	out := make([]byte, 0, totalData+v.ec*len(blocks))
	maxD := 0
	for _, b := range blocks {
		if len(b) > maxD {
			maxD = len(b)
		}
	}
	for i := range maxD {
		for _, b := range blocks {
			if i < len(b) {
				out = append(out, b[i])
			}
		}
	}
	for i := range v.ec {
		for _, e := range ecs {
			out = append(out, e[i])
		}
	}
	return out, nil
}

// qrMatrix places the codewords and the function patterns (finder, timing,
// alignment, format and version information) into the version's square grid,
// dark = true, applying mask pattern mask.
func qrMatrix(codewords []byte, v qrVer, mask int) [][]bool {
	n := v.size
	m := make([][]bool, n)
	fixed := make([][]bool, n)
	for y := range n {
		m[y] = make([]bool, n)
		fixed[y] = make([]bool, n)
	}

	// Finder patterns and their light separators.
	finder := func(x, y int) {
		for r := -1; r <= 7; r++ {
			for c := -1; c <= 7; c++ {
				if x+c < 0 || y+r < 0 || x+c >= n || y+r >= n {
					continue
				}
				dark := (0 <= r && r <= 6 && (c == 0 || c == 6)) ||
					(0 <= c && c <= 6 && (r == 0 || r == 6)) ||
					(2 <= r && r <= 4 && 2 <= c && c <= 4)
				m[y+r][x+c] = dark
				fixed[y+r][x+c] = true
			}
		}
	}
	finder(0, 0)
	finder(n-7, 0)
	finder(0, n-7)

	// Alignment patterns, skipping the positions inside a finder.
	for _, cy := range v.align {
		for _, cx := range v.align {
			if fixed[cy][cx] {
				continue
			}
			for r := -2; r <= 2; r++ {
				for c := -2; c <= 2; c++ {
					dark := r == -2 || r == 2 || c == -2 || c == 2 || (r == 0 && c == 0)
					m[cy+r][cx+c] = dark
					fixed[cy+r][cx+c] = true
				}
			}
		}
	}

	// Timing patterns along row and column 6, keeping any alignment-module
	// cells already placed.
	for i := 8; i < n-8; i++ {
		if !fixed[i][6] {
			m[i][6] = i%2 == 0
			fixed[i][6] = true
		}
		if !fixed[6][i] {
			m[6][i] = i%2 == 0
			fixed[6][i] = true
		}
	}

	// Format information: EC level M (00) + mask, BCH(15,5) against 0x537,
	// XORed with 0x5412; two copies around the finders plus the dark module.
	fv := formatBits(0, mask)
	for i := range 15 {
		var y, x int
		switch {
		case i < 6:
			y, x = i, 8
		case i < 8:
			y, x = i+1, 8
		default:
			y, x = n-15+i, 8
		}
		m[y][x] = fv>>uint(i)&1 == 1
		fixed[y][x] = true
	}
	for i := range 15 {
		var y, x int
		switch {
		case i < 8:
			y, x = 8, n-i-1
		case i < 9:
			y, x = 8, 7
		default:
			y, x = 8, 15-i-1
		}
		m[y][x] = fv>>uint(i)&1 == 1
		fixed[y][x] = true
	}
	m[n-8][8] = true
	fixed[n-8][8] = true

	// Version information for version 7 and up.
	if v.version >= 7 {
		vv := versionBits(v.version)
		for i := range 18 {
			bit := vv>>uint(i)&1 == 1
			m[i/3][i%3+n-11] = bit
			fixed[i/3][i%3+n-11] = true
			m[i%3+n-11][i/3] = bit
			fixed[i%3+n-11][i/3] = true
		}
	}

	// Data modules, two columns at a time from the bottom-right corner,
	// skipping the timing column, zigzagging up and down. The next-column
	// value is kept separate from the pair so the sequence (…, 6→5, 4→3,
	// 2→1) is exactly the spec's, unaffected by the body's adjustment.
	inc := -1
	row := n - 1
	bitIdx := 7
	byteIdx := 0
	for next := n - 1; next > 0; next -= 2 {
		col := next
		if col <= 6 {
			col--
		}
		for {
			for _, c := range [2]int{col, col - 1} {
				if !fixed[row][c] {
					dark := false
					if byteIdx < len(codewords) {
						dark = codewords[byteIdx]>>uint(bitIdx)&1 == 1
					}
					if qrMask(mask, row, c) {
						dark = !dark
					}
					m[row][c] = dark
					bitIdx--
					if bitIdx < 0 {
						byteIdx++
						bitIdx = 7
					}
				}
			}
			row += inc
			if row < 0 || row >= n {
				row -= inc
				inc = -inc
				break
			}
		}
	}
	return m
}

// qrMask reports whether the mask pattern flips the module at (row, col).
func qrMask(pattern, y, x int) bool {
	switch pattern {
	case 0:
		return (y+x)%2 == 0
	case 1:
		return y%2 == 0
	case 2:
		return x%3 == 0
	case 3:
		return (y+x)%3 == 0
	case 4:
		return (y/2+x/3)%2 == 0
	case 5:
		return (y*x)%2+(y*x)%3 == 0
	case 6:
		return ((y*x)%2+(y*x)%3)%2 == 0
	case 7:
		return ((y*x)%3+(y+x)%2)%2 == 0
	}
	return false
}

func bitLen(x int) int {
	n := 0
	for x != 0 {
		n++
		x >>= 1
	}
	return n
}

// formatBits is the 15-bit format string: five data bits (EC level and mask)
// with BCH(15,5) error correction against generator 0x537, XORed with the
// fixed 0x5412 mask.
func formatBits(ec, mask int) int {
	data := ec<<3 | mask
	d := data << 10
	g := 0x537
	for {
		t := bitLen(d) - bitLen(g)
		if t < 0 {
			break
		}
		d ^= g << t
	}
	return (data<<10 | d) ^ 0x5412
}

// versionBits is the 18-bit version string: six data bits with BCH(18,6)
// error correction against generator 0x1F25.
func versionBits(version int) int {
	d := version << 12
	g := 0x1F25
	for {
		t := bitLen(d) - bitLen(g)
		if t < 0 {
			break
		}
		d ^= g << t
	}
	return version<<12 | d
}

// qrBestMask picks the mask pattern with the lowest ISO/IEC 18004 penalty
// score (N1-N4), like production encoders do; every scanner decodes any of
// the eight, but the lowest penalty scans most reliably. The penalty is
// scored on the final, format-info-included matrix (some encoders score a
// blank-format test matrix; the choice between the two is cosmetic, the
// modules themselves were verified bit-for-bit against a reference for all
// eight masks).
func qrBestMask(codewords []byte, v qrVer) int {
	best, bestScore := 0, math.MaxInt
	for mask := 0; mask < 8; mask++ {
		if score := qrPenalty(qrMatrix(codewords, v, mask)); score < bestScore {
			best, bestScore = mask, score
		}
	}
	return best
}

// qrPenalty scores a finished matrix per the spec's N1-N4 rules: runs of
// five or more, 2x2 blocks, 1:1:3:1:1 patterns and the dark-module ratio.
func qrPenalty(m [][]bool) int {
	n := len(m)
	score := 0
	run := func(cells []bool) {
		runLen := 1
		for i := 1; i <= len(cells); i++ {
			if i < len(cells) && cells[i] == cells[i-1] {
				runLen++
				continue
			}
			if runLen >= 5 {
				score += runLen - 2
			}
			runLen = 1
		}
	}
	for y := range n {
		run(m[y])
	}
	for x := range n {
		col := make([]bool, n)
		for y := range n {
			col[y] = m[y][x]
		}
		run(col)
	}
	for y := range n - 1 {
		for x := range n - 1 {
			if m[y][x] == m[y][x+1] && m[y][x] == m[y+1][x] && m[y][x] == m[y+1][x+1] {
				score += 3
			}
		}
	}
	for y := range n {
		for x := 0; x+10 < n; x++ {
			if patternRun(m[y], x) {
				score += 40
			}
		}
	}
	for x := range n {
		for y := 0; y+10 < n; y++ {
			col := make([]bool, 11)
			for i := range 11 {
				col[i] = m[y+i][x]
			}
			if patternRun(col, 0) {
				score += 40
			}
		}
	}
	dark := 0
	for y := range n {
		for x := range n {
			if m[y][x] {
				dark++
			}
		}
	}
	score += int(math.Abs(float64(dark*100)/float64(n*n)-50)/5) * 10
	return score
}

// patternRun reports the 1:1:3:1:1 (dark:light) run with four light or four
// dark modules on one side, the spec's N3 pattern, at cells[x..x+10].
func patternRun(cells []bool, x int) bool {
	if cells[x+1] || !cells[x+4] || cells[x+5] || !cells[x+6] || cells[x+9] {
		return false
	}
	return (cells[x] && cells[x+2] && cells[x+3] && !cells[x+7] && !cells[x+8] && !cells[x+10]) ||
		(!cells[x] && !cells[x+2] && !cells[x+3] && cells[x+7] && cells[x+8] && cells[x+10])
}

// qrSVG renders text as a QR code and returns an SVG document: a white
// background, then the dark modules as one rectangle per horizontal run, in
// a viewBox that includes the required 4-module quiet zone.
func qrSVG(text string) (string, error) {
	var v qrVer
	ok := false
	for _, cand := range qrVersions {
		if len(text) <= cand.cap {
			v = cand
			ok = true
			break
		}
	}
	if !ok {
		return "", errors.New("text is too long for a QR code")
	}
	cw, err := qrCodewords(text, v)
	if err != nil {
		return "", err
	}
	m := qrMatrix(cw, v, qrBestMask(cw, v))
	const quiet = 4
	side := v.size + 2*quiet
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges">`, side, side)
	b.WriteString(`<rect width="100%" height="100%" fill="#fff"/>`)
	b.WriteString(`<path fill="#000" d="`)
	for y := range v.size {
		x := 0
		for x < v.size {
			if !m[y][x] {
				x++
				continue
			}
			start := x
			for x < v.size && m[y][x] {
				x++
			}
			fmt.Fprintf(&b, "M%d %dh%dv1h-%dz", start+quiet, y+quiet, x-start, x-start)
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String(), nil
}
