package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edementhon/kids-shelf/internal/budget"
	"github.com/edementhon/kids-shelf/internal/core"
	"github.com/edementhon/kids-shelf/internal/firewall"
	"github.com/edementhon/kids-shelf/internal/library"
	"github.com/edementhon/kids-shelf/internal/practice"
	"github.com/edementhon/kids-shelf/internal/store"
	"github.com/edementhon/kids-shelf/internal/update"
)

// newTestAdmin builds a Server with real services on a temp store: budget and
// library with their migrations, practice for the earnings-counter setting,
// and a temp data dir for the parent PIN. The parent PIN is set up first
// (testParentPIN), like a parent does on the first visit, and doReq carries
// that session for every request without explicit cookies. mut lets a test
// add a Quit hook or a fake update control.
func newTestAdmin(t *testing.T, mut ...func(*Deps)) (*httptest.Server, *budget.Budget, *library.Library) {
	t.Helper()
	ts, b, l := newTestAdminNoPIN(t, mut...)
	setUpParent(t, ts)
	return ts, b, l
}

// newTestAdminNoPIN is newTestAdmin on a fresh install: no parent PIN yet.
func newTestAdminNoPIN(t *testing.T, mut ...func(*Deps)) (*httptest.Server, *budget.Budget, *library.Library) {
	t.Helper()
	pinThrottle.reset()
	adminSessions.clear()
	st, err := store.Open(filepath.Join(t.TempDir(), "admin.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := st.Migrate(budget.Migrations...); err != nil {
		t.Fatalf("budget migrations: %v", err)
	}
	if err := st.Migrate(library.Migrations...); err != nil {
		t.Fatalf("library migrations: %v", err)
	}
	if err := st.Migrate(practice.Migrations()...); err != nil {
		t.Fatalf("practice migrations: %v", err)
	}
	b, err := budget.New(st)
	if err != nil {
		t.Fatalf("budget.New: %v", err)
	}
	l, err := library.New(st)
	if err != nil {
		t.Fatalf("library.New: %v", err)
	}
	p := practice.New(st.DB(), b, practice.Options{})
	deps := Deps{
		Budget:   b,
		Library:  l,
		Practice: p,
		DataDir:  t.TempDir(),
	}
	for _, m := range mut {
		m(&deps)
	}
	ts := httptest.NewServer(NewApp("test", deps).Mux())
	t.Cleanup(ts.Close)
	return ts, b, l
}

// testParentPIN is the parent PIN every test server is set up with.
const testParentPIN = "1357"

// parentClients maps a test server's host:port to the client whose cookie
// jar holds that server's parent session; doReq uses it for requests that
// pass no explicit cookies, so the jar also follows a reissued cookie (a
// PIN change) or an expired one (logout, reset).
var parentClients sync.Map

// setUpParent makes the first parent PIN on ts through POST
// /api/admin/pin/setup and keeps the issued session in a cookie jar that
// doReq sends with every request to ts.
func setUpParent(t *testing.T, ts *httptest.Server) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	c := &http.Client{Jar: jar}
	host := ts.Listener.Addr().String()
	parentClients.Store(host, c)
	t.Cleanup(func() { parentClients.Delete(host) })
	resp, out := doReq(t, http.MethodPost, ts.URL+"/api/admin/pin/setup", map[string]any{"pin": testParentPIN})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("parent PIN setup: status %d body %v", resp.StatusCode, out)
	}
}

// doReq performs one request and decodes the JSON body (nil body allowed).
// State changing requests carry a same-origin Origin header, exactly like
// the app's own pages send them. With no explicit cookies the request goes
// with the server's parent session (setUpParent); explicit cookies replace
// it, so a test controls exactly what is sent.
func doReq(t *testing.T, method, url string, body any, cookies ...*http.Cookie) (*http.Response, map[string]any) {
	t.Helper()
	client := http.DefaultClient
	if len(cookies) == 0 {
		client = parentClient(url)
	}
	return sendReq(t, client, method, url, body, cookies...)
}

// parentClient returns the client carrying the parent session of the test
// server rawURL points at, or http.DefaultClient when it has none.
func parentClient(rawURL string) *http.Client {
	if u, err := neturl.Parse(rawURL); err == nil {
		if c, ok := parentClients.Load(u.Host); ok {
			return c.(*http.Client)
		}
	}
	return http.DefaultClient
}

// parentCookie returns the parent session cookie setUpParent keeps for ts,
// for tests that build their own requests.
func parentCookie(t *testing.T, ts *httptest.Server) *http.Cookie {
	t.Helper()
	if c, ok := parentClients.Load(ts.Listener.Addr().String()); ok {
		u, _ := neturl.Parse(ts.URL)
		for _, ck := range c.(*http.Client).Jar.Cookies(u) {
			if ck.Name == adminPinCookie {
				return ck
			}
		}
	}
	t.Fatalf("no parent session for %s", ts.URL)
	return nil
}

// visitorReq is doReq for someone without a parent session: only the given
// cookies are sent.
func visitorReq(t *testing.T, method, url string, body any, cookies ...*http.Cookie) (*http.Response, map[string]any) {
	t.Helper()
	return sendReq(t, http.DefaultClient, method, url, body, cookies...)
}

func sendReq(t *testing.T, client *http.Client, method, url string, body any, cookies ...*http.Cookie) (*http.Response, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("Origin", "http://"+req.Host)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	var out map[string]any
	if b, err := io.ReadAll(resp.Body); err == nil && len(b) > 0 {
		_ = json.Unmarshal(b, &out) // non-JSON bodies (e.g. the HTML page) leave out nil
	}
	return resp, out
}

func post(t *testing.T, url string, body any, cookies ...*http.Cookie) (*http.Response, map[string]any) {
	t.Helper()
	return doReq(t, http.MethodPost, url, body, cookies...)
}

func deleteReq(t *testing.T, url string, cookies ...*http.Cookie) (*http.Response, map[string]any) {
	t.Helper()
	return doReq(t, http.MethodDelete, url, nil, cookies...)
}

// kidID creates one kid through the API and returns its id.
func kidID(t *testing.T, ts *httptest.Server, name, pin string) string {
	t.Helper()
	resp, out := post(t, ts.URL+"/api/admin/kids", map[string]any{"name": name, "pin": pin})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create kid %q: status %d body %v", name, resp.StatusCode, out)
	}
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("create kid %q: no id in %v", name, out)
	}
	return id
}

// state returns the decoded /api/admin/state payload.
func state(t *testing.T, ts *httptest.Server, cookies ...*http.Cookie) map[string]any {
	t.Helper()
	resp, out := doReq(t, http.MethodGet, ts.URL+"/api/admin/state", nil, cookies...)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/admin/state: status %d body %v", resp.StatusCode, out)
	}
	return out
}

func kidByName(t *testing.T, st map[string]any, name string) map[string]any {
	t.Helper()
	kids, _ := st["kids"].([]any)
	for _, k := range kids {
		km, _ := k.(map[string]any)
		if km["name"] == name {
			return km
		}
	}
	t.Fatalf("kid %q not in state %v", name, st["kids"])
	return nil
}

// login returns the dashboard-PIN cookie POST /api/admin/pin issues.
func login(t *testing.T, ts *httptest.Server, pin string) *http.Cookie {
	t.Helper()
	resp, _ := post(t, ts.URL+"/api/admin/pin", map[string]any{"pin": pin})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login with pin %s: status %d, want 200", pin, resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == adminPinCookie {
			return c
		}
	}
	t.Fatalf("login with pin %s set no %s cookie", pin, adminPinCookie)
	return nil
}

func publishShow(t *testing.T, l *library.Library, showID, showName string) core.EpisodeID {
	t.Helper()
	ep := core.Episode{
		ID:          core.EpisodeID("ep-" + showName),
		ShowID:      showID,
		Title:       showName + " Episode 1",
		File:        "/media/" + showName + "/e1.mp4",
		DurationSec: 600,
		Source:      core.Source{Kind: core.SourceFolder, Path: "/videos/" + showName + "/e1.mp4", Show: showName},
	}
	if err := l.Publish(context.Background(), ep); err != nil {
		t.Fatalf("publish %s: %v", showName, err)
	}
	return ep.ID
}

func TestAdminKidsCRUD(t *testing.T) {
	ts, b, _ := newTestAdmin(t)

	// Bad inputs first.
	if resp, _ := post(t, ts.URL+"/api/admin/kids", map[string]any{"name": "", "pin": "1111"}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty name: status %d, want 400", resp.StatusCode)
	}
	if resp, _ := post(t, ts.URL+"/api/admin/kids", map[string]any{"name": "Alex", "pin": "12"}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("short pin: status %d, want 400", resp.StatusCode)
	}
	if resp, _ := post(t, ts.URL+"/api/admin/kids", map[string]any{"name": "Alex", "pin": "12ab"}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("non-numeric pin: status %d, want 400", resp.StatusCode)
	}

	// Adding a name that is already a kid's, in any letter case, is refused
	// in plain words and never touches that kid's PIN.
	id := kidID(t, ts, "Alex", "1111")
	for _, name := range []string{"Alex", "alex", " ALEX "} {
		again, out := post(t, ts.URL+"/api/admin/kids", map[string]any{"name": name, "pin": "9999"})
		want := "A kid called Alex already exists. Pick another name. To give Alex a new PIN, use ⋯ on Alex's card."
		if again.StatusCode != http.StatusConflict || out["error"] != want {
			t.Errorf("add %q again: status %d %v, want 409 %q", name, again.StatusCode, out, want)
		}
	}
	if !b.VerifyKidPIN(context.Background(), core.KidID(id), "1111") || b.VerifyKidPIN(context.Background(), core.KidID(id), "9999") {
		t.Error("a refused duplicate add changed the existing kid's PIN")
	}
	if kids := state(t, ts)["kids"].([]any); len(kids) != 1 {
		t.Errorf("kids after refused duplicates = %v, want only Alex", kids)
	}

	st := state(t, ts)
	k := kidByName(t, st, "Alex")
	if k["today"] != float64(10) || k["saved"] != float64(0) {
		t.Errorf("fresh account balances = today %v saved %v, want 10 and 0", k["today"], k["saved"])
	}
	if k["daily"] != float64(10) {
		t.Errorf("daily = %v, want the household default 10", k["daily"])
	}

	// Rename, and a duplicate name is refused.
	kidID(t, ts, "Sam", "2222")
	if resp, out := post(t, ts.URL+"/api/admin/kids/"+id+"/rename", map[string]any{"name": "Sam"}); resp.StatusCode != http.StatusConflict || out["error"] != "A kid called Sam already exists. Pick another name." {
		t.Errorf("rename to existing name: status %d %v, want 409 and the plain refusal", resp.StatusCode, out)
	}
	if resp, _ := post(t, ts.URL+"/api/admin/kids/"+id+"/rename", map[string]any{"name": "sAM"}); resp.StatusCode != http.StatusConflict {
		t.Errorf("rename to existing name in another case: status %d, want 409", resp.StatusCode)
	}
	if resp, _ := post(t, ts.URL+"/api/admin/kids/"+id+"/rename", map[string]any{"name": "Alex H."}); resp.StatusCode != http.StatusOK {
		t.Errorf("rename: status %d, want 200", resp.StatusCode)
	}
	kidByName(t, state(t, ts), "Alex H.")

	// Change the PIN.
	if resp, _ := post(t, ts.URL+"/api/admin/kids/"+id+"/pin", map[string]any{"pin": "7777"}); resp.StatusCode != http.StatusOK {
		t.Errorf("change pin: status %d, want 200", resp.StatusCode)
	}
	if !b.VerifyKidPIN(context.Background(), core.KidID(id), "7777") {
		t.Error("new kid PIN does not verify")
	}
	if b.VerifyKidPIN(context.Background(), core.KidID(id), "1111") {
		t.Error("old kid PIN still verifies")
	}

	// Per-kid daily minutes.
	if resp, _ := post(t, ts.URL+"/api/admin/kids/"+id+"/minutes", map[string]any{"minutes": 30}); resp.StatusCode != http.StatusOK {
		t.Errorf("set daily minutes: status %d, want 200", resp.StatusCode)
	}
	k = kidByName(t, state(t, ts), "Alex H.")
	if k["daily"] != float64(30) {
		t.Errorf("daily = %v, want 30", k["daily"])
	}
	if resp, _ := post(t, ts.URL+"/api/admin/kids/"+id+"/minutes", map[string]any{"minutes": -1}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("negative daily minutes: status %d, want 400", resp.StatusCode)
	}
	// An emptied minutes box (missing or null) is refused, never saved as 0.
	for _, body := range []map[string]any{{}, {"minutes": nil}} {
		if resp, out := post(t, ts.URL+"/api/admin/kids/"+id+"/minutes", body); resp.StatusCode != http.StatusBadRequest || out["error"] != "Type a number of minutes, like 30." {
			t.Errorf("minutes %v: status %d %v, want 400 and the plain message", body, resp.StatusCode, out)
		}
	}
	if k := kidByName(t, state(t, ts), "Alex H."); k["daily"] != float64(30) {
		t.Errorf("daily after an empty save = %v, want 30 kept", k["daily"])
	}

	// Remove, then the kid is gone and unknown ids 404.
	tid := kidByName(t, state(t, ts), "Sam")["id"].(string)
	if resp, _ := deleteReq(t, ts.URL+"/api/admin/kids/"+tid); resp.StatusCode != http.StatusOK {
		t.Errorf("remove kid: status %d, want 200", resp.StatusCode)
	}
	if resp, _ := deleteReq(t, ts.URL+"/api/admin/kids/"+tid); resp.StatusCode != http.StatusNotFound {
		t.Errorf("remove unknown kid: status %d, want 404", resp.StatusCode)
	}
	if kids := state(t, ts)["kids"].([]any); len(kids) != 1 {
		t.Errorf("kids after removal = %v, want only Alex H.", kids)
	}
}

func TestAdminAdjustMinutes(t *testing.T) {
	ts, _, _ := newTestAdmin(t)
	id := kidID(t, ts, "Alex", "1111")

	// Start of the day: the allowance is granted by the first balance read.
	resp, out := post(t, ts.URL+"/api/admin/adjust", map[string]any{"kid": id, "delta": 5})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("adjust +5: status %d body %v", resp.StatusCode, out)
	}
	if out["today"] != float64(15) || out["saved"] != float64(0) {
		t.Errorf("after +5: today %v saved %v, want 15 and 0", out["today"], out["saved"])
	}

	// Negative deltas are allowed but clamp at zero: the page's minus
	// buttons can never push a kid into debt.
	if _, out := post(t, ts.URL+"/api/admin/adjust", map[string]any{"kid": id, "delta": -2}); out["today"] != float64(13) {
		t.Errorf("after -2: today = %v, want 13", out["today"])
	}
	resp, out = post(t, ts.URL+"/api/admin/adjust", map[string]any{"kid": id, "delta": -100})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("adjust -100: status %d", resp.StatusCode)
	}
	if out["today"] != float64(0) || out["saved"] != float64(0) {
		t.Errorf("after -100: today %v saved %v, want both 0 (clamped)", out["today"], out["saved"])
	}

	// Unknown kid -> not found.
	if resp, _ := post(t, ts.URL+"/api/admin/adjust", map[string]any{"kid": "ghost", "delta": 1}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("adjust unknown kid: status %d, want 404", resp.StatusCode)
	}
}

func TestAdminSetBin(t *testing.T) {
	ts, _, l := newTestAdmin(t)
	publishShow(t, l, "show-bluey", "Bluey")

	// New shows land in fun.
	bins := state(t, ts)["bins"].(map[string]any)
	if len(bins["fun"].([]any)) != 1 {
		t.Errorf("new show not in fun: %v", bins)
	}
	showID := "show-bluey"

	// Move to learning, then free.
	if resp, _ := post(t, ts.URL+"/api/admin/bin", map[string]any{"show": showID, "bin": "learning"}); resp.StatusCode != http.StatusOK {
		t.Errorf("set learning: status %d, want 200", resp.StatusCode)
	}
	bins = state(t, ts)["bins"].(map[string]any)
	if len(bins["learning"].([]any)) != 1 || len(bins["fun"].([]any)) != 0 {
		t.Errorf("after learning: %v", bins)
	}

	// Unknown bin and unknown show.
	if resp, _ := post(t, ts.URL+"/api/admin/bin", map[string]any{"show": showID, "bin": "other"}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad bin: status %d, want 400", resp.StatusCode)
	}
	if resp, _ := post(t, ts.URL+"/api/admin/bin", map[string]any{"show": "ghost", "bin": "fun"}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown show: status %d, want 404", resp.StatusCode)
	}
}

func TestAdminDashboardPINGate(t *testing.T) {
	ts, _, _ := newTestAdmin(t)

	// The parent who made the PIN is in.
	if resp, _ := doReq(t, http.MethodGet, ts.URL+"/api/admin/state", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("state with the setup session: status %d, want 200", resp.StatusCode)
	}

	// Anyone else meets the lock (not the setup form: a PIN exists).
	resp, out := visitorReq(t, http.MethodGet, ts.URL+"/api/admin/state", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("state without cookie: status %d, want 401", resp.StatusCode)
	}
	if out["setup"] == true {
		t.Errorf("state without cookie said setup with a PIN set: %v", out)
	}
	wrong := &http.Cookie{Name: adminPinCookie, Value: "9999", Path: "/"}
	if resp, _ := doReq(t, http.MethodGet, ts.URL+"/api/admin/state", nil, wrong); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("state with wrong cookie: status %d, want 401", resp.StatusCode)
	}

	// Login with a wrong PIN, then the right one.
	if resp, _ := visitorReq(t, http.MethodPost, ts.URL+"/api/admin/pin", map[string]any{"pin": "1111"}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong manager pin: status %d, want 401", resp.StatusCode)
	}
	cookie := login(t, ts, testParentPIN)
	if resp, _ := doReq(t, http.MethodGet, ts.URL+"/api/admin/state", nil, cookie); resp.StatusCode != http.StatusOK {
		t.Errorf("state with login cookie: status %d, want 200", resp.StatusCode)
	}

	// Changing the PIN reissues the cookie; the old one stops working.
	resp, _ = post(t, ts.URL+"/api/admin/settings/dashboard", map[string]any{"pin": "2468"}, cookie)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("change manager pin: status %d, want 200", resp.StatusCode)
	}
	var newCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == adminPinCookie {
			newCookie = c
		}
	}
	if newCookie == nil {
		t.Fatal("pin change did not reissue the manager cookie")
	}
	if resp, _ := doReq(t, http.MethodGet, ts.URL+"/api/admin/state", nil, cookie); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("old cookie after pin change: status %d, want 401", resp.StatusCode)
	}
	if resp, _ := doReq(t, http.MethodGet, ts.URL+"/api/admin/state", nil, newCookie); resp.StatusCode != http.StatusOK {
		t.Errorf("reissued cookie after pin change: status %d, want 200", resp.StatusCode)
	}

	// Logout expires the cookie for browsers and ends the session.
	respLogout, _ := post(t, ts.URL+"/api/admin/logout", nil, newCookie)
	expired := false
	for _, c := range respLogout.Cookies() {
		if c.Name == adminPinCookie {
			expired = c.MaxAge < 0 || c.Expires.Before(time.Now())
		}
	}
	if !expired {
		t.Error("logout did not expire the manager cookie")
	}
	if resp, _ := doReq(t, http.MethodGet, ts.URL+"/api/admin/state", nil, newCookie); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("state after logout: status %d, want 401", resp.StatusCode)
	}
}

func TestAdminBadDashboardPIN(t *testing.T) {
	ts, _, _ := newTestAdmin(t)
	if resp, _ := post(t, ts.URL+"/api/admin/settings/dashboard", map[string]any{"pin": "12"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad manager pin: status %d, want 400", resp.StatusCode)
	}
	// The rejected pin changed nothing: the session still works and the
	// PIN is still the one made at setup.
	if resp, _ := doReq(t, http.MethodGet, ts.URL+"/api/admin/state", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("rejected bad pin ended the session: status %d", resp.StatusCode)
	}
	login(t, ts, testParentPIN)
}

// TestAdminNoPINAsksForSetup: on a fresh install (no parent PIN) every
// gated route answers 401 with setup true instead of opening, including the
// /admin/content data routes; the page shell still loads.
func TestAdminNoPINAsksForSetup(t *testing.T) {
	ts, _, _ := newTestAdminNoPIN(t)
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/admin/state"},
		{http.MethodGet, "/api/admin/problems"},
		{http.MethodGet, "/api/admin/remove"},
		{http.MethodPost, "/api/admin/kids"},
		{http.MethodPost, "/api/admin/settings/dashboard"},
		{http.MethodPost, "/api/admin/pin"},
		{http.MethodGet, "/api/admin/content/sources"},
	} {
		var body any
		if c.method == http.MethodPost {
			body = map[string]any{"name": "Ada", "pin": "1234"}
		}
		resp, out := visitorReq(t, c.method, ts.URL+c.path, body)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s with no PIN: status %d, want 401", c.method, c.path, resp.StatusCode)
		}
		if out["setup"] != true {
			t.Errorf("%s %s with no PIN: body %v, want setup true", c.method, c.path, out)
		}
		if msg, _ := out["error"].(string); msg == "" {
			t.Errorf("%s %s with no PIN: no plain error in %v", c.method, c.path, out)
		}
	}
	// The settings route above did not sneak a PIN in.
	if _, out := visitorReq(t, http.MethodGet, ts.URL+"/api/admin/pin/status", nil); out["set"] != false {
		t.Errorf("status after refused writes = %v, want set false", out)
	}
	if resp, _ := visitorReq(t, http.MethodGet, ts.URL+"/admin", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("GET /admin with no PIN: status %d, want 200 (the shell)", resp.StatusCode)
	}
}

// TestAdminPINSetupOnce: setup validates 4 digits, works once, issues a
// working session, then answers 409 without touching the PIN; it is
// same-origin only like every state change.
func TestAdminPINSetupOnce(t *testing.T) {
	ts, _, _ := newTestAdminNoPIN(t)
	setup := ts.URL + "/api/admin/pin/setup"

	for _, bad := range []string{"", "12", "12a4", "12345"} {
		if resp, _ := visitorReq(t, http.MethodPost, setup, map[string]any{"pin": bad}); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("setup with %q: status %d, want 400", bad, resp.StatusCode)
		}
	}

	// A cross-origin setup never lands.
	req, err := http.NewRequest(http.MethodPost, setup, strings.NewReader(`{"pin":"9999"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	respX, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	respX.Body.Close()
	if respX.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin setup: status %d, want 403", respX.StatusCode)
	}

	resp, _ := visitorReq(t, http.MethodPost, setup, map[string]any{"pin": "2468"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first setup: status %d, want 200", resp.StatusCode)
	}
	var session *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == adminPinCookie && c.Value != "" {
			session = c
		}
	}
	if session == nil {
		t.Fatal("setup issued no session cookie")
	}
	if resp, _ := doReq(t, http.MethodGet, ts.URL+"/api/admin/state", nil, session); resp.StatusCode != http.StatusOK {
		t.Errorf("state with the setup session: status %d, want 200", resp.StatusCode)
	}

	// Once a PIN exists, setup can never replace it.
	resp, out := visitorReq(t, http.MethodPost, setup, map[string]any{"pin": "1111"})
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("second setup: status %d, want 409", resp.StatusCode)
	}
	if msg, _ := out["error"].(string); msg == "" {
		t.Errorf("409 has no plain message: %v", out)
	}
	if resp, _ := visitorReq(t, http.MethodPost, ts.URL+"/api/admin/pin", map[string]any{"pin": "1111"}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the refused setup PIN opens the page: status %d, want 401", resp.StatusCode)
	}
	login(t, ts, "2468")
	if resp, _ := doReq(t, http.MethodGet, ts.URL+"/api/admin/state", nil, session); resp.StatusCode != http.StatusOK {
		t.Errorf("refused setup ended the session: status %d, want 200", resp.StatusCode)
	}
}

// TestAdminPINSetupThrottled: a client locked out by wrong parent PINs is
// locked out of setup too (the same throttle as POST /api/admin/pin).
func TestAdminPINSetupThrottled(t *testing.T) {
	ts, _, _ := newTestAdminNoPIN(t)
	now := time.Now()
	for range pinThrottleAfter {
		pinThrottle.fail(realmDashboard, "127.0.0.1", "", now)
	}
	resp, out := visitorReq(t, http.MethodPost, ts.URL+"/api/admin/pin/setup", map[string]any{"pin": "2468"})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("setup while locked out: status %d, want 429 (body %v)", resp.StatusCode, out)
	}
	if _, st := visitorReq(t, http.MethodGet, ts.URL+"/api/admin/pin/status", nil); st["set"] != false {
		t.Errorf("a locked-out setup stored a PIN: %v", st)
	}
}

// TestAdminDashboardPINCannotBeCleared: Settings can change the parent PIN
// but never remove it.
func TestAdminDashboardPINCannotBeCleared(t *testing.T) {
	ts, _, _ := newTestAdmin(t)
	resp, out := post(t, ts.URL+"/api/admin/settings/dashboard", map[string]any{"pin": ""})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("clear parent pin: status %d, want 400", resp.StatusCode)
	}
	if msg, _ := out["error"].(string); msg != "The parent page always needs a PIN." {
		t.Errorf("clear parent pin: error %q", msg)
	}
	if resp, _ := visitorReq(t, http.MethodGet, ts.URL+"/api/admin/state", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("state without cookie after refused clear: status %d, want 401", resp.StatusCode)
	}
	login(t, ts, testParentPIN)
}

// pinApp is a parent-page app served in-process, so a test can choose the
// connection's peer address and the launcher proxy headers.
func pinApp(t *testing.T) http.Handler {
	t.Helper()
	pinThrottle.reset()
	adminSessions.clear()
	return NewApp("pin-test", Deps{DataDir: t.TempDir(), ProxyToken: "proxy-secret"}).Mux()
}

// serveFrom sends one request to h as if it came from peer (host:port),
// with the extra headers set, and decodes the JSON answer.
func serveFrom(t *testing.T, h http.Handler, method, path, peer string, body any, hdr map[string]string, cookies ...*http.Cookie) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = strings.NewReader(string(b))
	}
	r := httptest.NewRequest(method, path, rd)
	r.Host = "127.0.0.1:8080"
	r.RemoteAddr = peer
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet {
		r.Header.Set("Origin", "http://127.0.0.1:8080")
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

// viaProxy is the header pair the launcher's proxy adds for client.
func viaProxy(client string) map[string]string {
	return map[string]string{"X-KS-Token": "proxy-secret", "X-KS-Client": client}
}

// TestAdminPINStatusLocal: status needs no session, reveals only set and
// local, and local follows the real client: the connection's peer, or the
// trusted proxy's X-KS-Client, never a spoofed header.
func TestAdminPINStatusLocal(t *testing.T) {
	h := pinApp(t)
	cases := []struct {
		name  string
		peer  string
		hdr   map[string]string
		local bool
	}{
		{"direct loopback", "127.0.0.1:5000", nil, true},
		{"direct IPv6 loopback", "[::1]:5000", nil, true},
		{"direct tablet", "192.0.2.20:5000", nil, false},
		{"proxy, this computer", "127.0.0.1:5000", viaProxy("127.0.0.1"), true},
		{"proxy, IPv6 loopback", "127.0.0.1:5000", viaProxy("::1"), true},
		{"proxy, tablet", "127.0.0.1:5000", viaProxy("192.0.2.20"), false},
		{"tablet spoofing the proxy header", "192.0.2.20:5000", viaProxy("127.0.0.1"), false},
		{"wrong proxy token is ignored", "127.0.0.1:5000", map[string]string{"X-KS-Token": "nope", "X-KS-Client": "192.0.2.20"}, true},
	}
	for _, c := range cases {
		w, out := serveFrom(t, h, http.MethodGet, "/api/admin/pin/status", c.peer, nil, c.hdr)
		if w.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200", c.name, w.Code)
			continue
		}
		if out["local"] != c.local || out["set"] != false || len(out) != 2 {
			t.Errorf("%s: body %v, want {set:false local:%v} only", c.name, out, c.local)
		}
	}
	if w, _ := serveFrom(t, h, http.MethodPost, "/api/admin/pin/setup", "127.0.0.1:5000", map[string]any{"pin": "2468"}, nil); w.Code != http.StatusOK {
		t.Fatalf("setup: status %d", w.Code)
	}
	if _, out := serveFrom(t, h, http.MethodGet, "/api/admin/pin/status", "192.0.2.20:5000", nil, nil); out["set"] != true || out["local"] != false {
		t.Errorf("status after setup from a tablet = %v, want set true, local false", out)
	}
}

// TestAdminPINReset: only a browser on the kids-shelf computer itself can
// clear the parent PIN; afterwards every session is gone and the page asks
// for a new PIN.
func TestAdminPINReset(t *testing.T) {
	h := pinApp(t)
	w, _ := serveFrom(t, h, http.MethodPost, "/api/admin/pin/setup", "192.0.2.20:5000", map[string]any{"pin": "2468"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("setup: status %d", w.Code)
	}
	var session *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == adminPinCookie {
			session = c
		}
	}
	if session == nil {
		t.Fatal("setup issued no session")
	}

	for _, c := range []struct {
		name string
		peer string
		hdr  map[string]string
	}{
		{"direct tablet", "192.0.2.20:5000", nil},
		{"tablet through the proxy", "127.0.0.1:5000", viaProxy("192.0.2.20")},
		{"tablet spoofing the proxy header", "192.0.2.20:5000", viaProxy("127.0.0.1")},
	} {
		// Even a signed-in parent on a tablet cannot reset it.
		w, out := serveFrom(t, h, http.MethodPost, "/api/admin/pin/reset", c.peer, nil, c.hdr, session)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s: reset status %d, want 403", c.name, w.Code)
		}
		if msg, _ := out["error"].(string); msg != "Reset the PIN on the computer kids-shelf runs on." {
			t.Errorf("%s: reset error %q", c.name, msg)
		}
	}
	if _, out := serveFrom(t, h, http.MethodGet, "/api/admin/pin/status", "192.0.2.20:5000", nil, nil); out["set"] != true {
		t.Fatalf("a refused reset cleared the PIN: %v", out)
	}

	// A cross-origin reset never reaches the handler.
	r := httptest.NewRequest(http.MethodPost, "/api/admin/pin/reset", nil)
	r.Host = "127.0.0.1:8080"
	r.RemoteAddr = "127.0.0.1:5000"
	r.Header.Set("Origin", "https://evil.example")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, r)
	if rw.Code != http.StatusForbidden {
		t.Errorf("cross-origin reset: status %d, want 403", rw.Code)
	}
	if _, out := serveFrom(t, h, http.MethodGet, "/api/admin/pin/status", "127.0.0.1:5000", nil, nil); out["set"] != true {
		t.Fatalf("a cross-origin reset cleared the PIN: %v", out)
	}

	// The computer itself, through the launcher's proxy: the PIN is gone,
	// and the wrong guesses made there before the reset are forgotten.
	for range pinThrottleAfter {
		pinThrottle.fail(realmDashboard, "127.0.0.1", "", time.Now())
	}
	w, _ = serveFrom(t, h, http.MethodPost, "/api/admin/pin/reset", "127.0.0.1:5000", nil, viaProxy("127.0.0.1"))
	if w.Code != http.StatusOK {
		t.Fatalf("local reset: status %d, want 200", w.Code)
	}
	if _, out := serveFrom(t, h, http.MethodGet, "/api/admin/pin/status", "127.0.0.1:5000", nil, nil); out["set"] != false {
		t.Errorf("status after local reset = %v, want set false", out)
	}
	w, out := serveFrom(t, h, http.MethodGet, "/api/admin/state", "192.0.2.20:5000", nil, nil, session)
	if w.Code != http.StatusUnauthorized || out["setup"] != true {
		t.Errorf("old session after reset: status %d body %v, want 401 with setup true", w.Code, out)
	}
	w, _ = serveFrom(t, h, http.MethodPost, "/api/admin/pin/setup", "127.0.0.1:5000", map[string]any{"pin": "8642"}, viaProxy("127.0.0.1"))
	if w.Code != http.StatusOK {
		t.Errorf("new setup right after a local reset: status %d, want 200", w.Code)
	}
}

func TestAdminCSRFGate(t *testing.T) {
	ts, _, _ := newTestAdmin(t)

	// A cross-origin state change is refused before any handler runs: the
	// Origin belongs to another site and the body is not JSON, so both the
	// same-site check and the body gate would have to pass for it to land.
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/admin/settings/dashboard",
		strings.NewReader(`{"pin":"1234"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("cross-origin POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin POST = %d, want 403", resp.StatusCode)
	}
	// ...and nothing changed: the PIN is still the one from setup, so the
	// session it issued still works.
	if st, _ := doReq(t, http.MethodGet, ts.URL+"/api/admin/state", nil); st.StatusCode != http.StatusOK {
		t.Errorf("state after cross-origin POST = %d, want 200 (nothing should have changed)", st.StatusCode)
	}

	// /api/admin/quit without Origin or Referer is refused.
	req3, err := http.NewRequest(http.MethodPost, ts.URL+"/api/admin/quit", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatalf("quit without Origin: %v", err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusForbidden {
		t.Errorf("quit without Origin = %d, want 403", resp3.StatusCode)
	}

	// The Referer fallback passes the same-site check; without a Quit hook
	// the route itself answers 503, proving the middleware let it through.
	req4, err := http.NewRequest(http.MethodPost, ts.URL+"/api/admin/quit", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req4.Header.Set("Referer", ts.URL+"/admin")
	req4.AddCookie(parentCookie(t, ts))
	resp4, err := http.DefaultClient.Do(req4)
	if err != nil {
		t.Fatalf("quit with Referer: %v", err)
	}
	resp4.Body.Close()
	if resp4.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("quit with Referer = %d, want 503 (the route ran)", resp4.StatusCode)
	}

	// A same-origin POST with a JSON body works.
	if resp, _ := post(t, ts.URL+"/api/admin/settings/dashboard", map[string]any{"pin": "1234"}); resp.StatusCode != http.StatusOK {
		t.Errorf("same-origin POST = %d, want 200", resp.StatusCode)
	}

	// A same-origin POST with a non-JSON body is refused by readJSON.
	req2, err := http.NewRequest(http.MethodPost, ts.URL+"/api/kid/unlock",
		strings.NewReader(`kid=x&pin=1234`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req2.Header.Set("Content-Type", "text/plain")
	req2.Header.Set("Origin", "http://"+req2.Host)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("text/plain POST: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("text/plain same-origin POST = %d, want 400", resp2.StatusCode)
	}
}

// TestAdminPINRequiresTokenSession checks the session-token change: the raw
// PIN as the ks_admin_pin cookie value (which the old scheme accepted by
// comparing it to the stored PIN) no longer authorizes.
func TestAdminPINRequiresTokenSession(t *testing.T) {
	ts, _, _ := newTestAdmin(t)

	raw := &http.Cookie{Name: adminPinCookie, Value: testParentPIN, Path: "/"}
	if resp, _ := doReq(t, http.MethodGet, ts.URL+"/api/admin/state", nil, raw); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("raw-PIN cookie: status %d, want 401", resp.StatusCode)
	}
	cookie := login(t, ts, testParentPIN)
	if resp, _ := doReq(t, http.MethodGet, ts.URL+"/api/admin/state", nil, cookie); resp.StatusCode != http.StatusOK {
		t.Errorf("session token cookie: status %d, want 200", resp.StatusCode)
	}
}

// TestAdminPINThrottle locks the client out on the dashboard PIN check too:
// five wrong tries, then the sixth, even with the right PIN, is refused.
func TestAdminPINThrottle(t *testing.T) {
	ts, _, _ := newTestAdmin(t)
	for range 5 {
		if resp, _ := visitorReq(t, http.MethodPost, ts.URL+"/api/admin/pin", map[string]any{"pin": "0000"}); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong dashboard pin: status %d, want 401", resp.StatusCode)
		}
	}
	resp, out := visitorReq(t, http.MethodPost, ts.URL+"/api/admin/pin", map[string]any{"pin": testParentPIN})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("sixth dashboard try: status %d, want 429 (body %v)", resp.StatusCode, out)
	}
	if msg, _ := out["error"].(string); msg != "Too many tries. Wait a little bit, then try again." {
		t.Errorf("429 message = %q", msg)
	}
	// The parent page counts the lockout down from this field.
	if wait, _ := out["wait"].(float64); wait < 1 {
		t.Errorf("429 wait = %v, want the lockout seconds", out["wait"])
	}
}

func TestAdminQRAddress(t *testing.T) {
	ts, _, _ := newTestAdmin(t)
	resp, out := doReq(t, http.MethodGet, ts.URL+"/api/admin/address", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/admin/address: status %d body %v", resp.StatusCode, out)
	}
	addr, _ := out["address"].(string)
	port := ts.URL[strings.LastIndex(ts.URL, ":")+1:]
	if !strings.HasPrefix(addr, "http://") || !strings.HasSuffix(addr, "/") || !strings.Contains(addr, ":"+port) {
		t.Errorf("address = %q, want http://<lan-ip>:%s/", addr, port)
	}
	svg, _ := out["qr_svg"].(string)
	if svg == "" {
		t.Fatal("qr_svg missing from the address response")
	}
	assertValidQR(t, svg)
}

// assertValidQR checks that svg is well-formed XML for a QR code: an <svg>
// root with a square viewBox and dark modules drawn as a path.
func assertValidQR(t *testing.T, svg string) {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(svg))
	var root string
	var viewBox string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("qr svg is not well-formed XML: %v", err)
		}
		if se, ok := tok.(xml.StartElement); ok && root == "" {
			root = se.Name.Local
			for _, a := range se.Attr {
				if a.Name.Local == "viewBox" {
					viewBox = a.Value
				}
			}
		}
	}
	if root != "svg" {
		t.Fatalf("qr svg root = %q, want svg", root)
	}
	var a, b, c, d int
	if _, err := fmt.Sscanf(viewBox, "%d %d %d %d", &a, &b, &c, &d); err != nil || a != 0 || b != 0 || c != d || c < 33 {
		t.Errorf("qr viewBox = %q, want a square of at least 33 modules", viewBox)
	}
	if strings.Count(svg, "M") < 20 {
		t.Errorf("qr svg draws too few dark runs: %d", strings.Count(svg, "M"))
	}
}
func TestAdminQuit(t *testing.T) {
	var quits atomic.Int32
	ts, _, _ := newTestAdmin(t, func(d *Deps) { d.Quit = func() { quits.Add(1) } })
	if resp, _ := post(t, ts.URL+"/api/admin/quit", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("quit: status %d, want 200", resp.StatusCode)
	}
	deadline := time.Now().Add(2 * time.Second)
	for quits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if quits.Load() != 1 {
		t.Errorf("Quit hook called %d times, want 1", quits.Load())
	}

	// A copy without the hook answers 503 instead of pretending.
	tsNil, _, _ := newTestAdmin(t)
	if resp, _ := post(t, tsNil.URL+"/api/admin/quit", nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("quit without a hook: status %d, want 503", resp.StatusCode)
	}
}

func TestAdminUpdates(t *testing.T) {
	// No updater in this copy: plain words, no restart button.
	ts, _, _ := newTestAdmin(t)
	if msg := state(t, ts)["updates"].(map[string]any)["message"]; msg != "This copy does not update itself." {
		t.Errorf("updates message = %v, want the plain no-self-update words", msg)
	}
	if resp, _ := post(t, ts.URL+"/api/admin/update/restart", nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("restart without updater: status %d, want 503", resp.StatusCode)
	}

	// An updater switched off (no signing key) says the same plain words,
	// not the key's jargon.
	off := &fakeUpdateControl{status: update.Status{State: update.StateOff, Message: "updates are off: no signing key was built into this copy"}}
	tsOff, _, _ := newTestAdmin(t, func(d *Deps) { d.Updates = off })
	if msg := state(t, tsOff)["updates"].(map[string]any)["message"]; msg != "This copy does not update itself." {
		t.Errorf("off updater message = %v, want the plain no-self-update words", msg)
	}

	// With an updater, its staged state is reported and the restart kicks in.
	ctrl := &fakeUpdateControl{status: update.Status{State: update.StateStaged, Staged: "v1.2.3", Message: "version v1.2.3 is staged and ready; restart to apply it"}}
	ts2, _, _ := newTestAdmin(t, func(d *Deps) { d.Updates = ctrl })
	up := state(t, ts2)["updates"].(map[string]any)
	if up["state"] != "staged" || up["staged"] != "v1.2.3" {
		t.Errorf("updates state = %v, want staged v1.2.3", up)
	}
	if resp, _ := post(t, ts2.URL+"/api/admin/update/restart", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("restart to update: status %d, want 200", resp.StatusCode)
	}
	if ctrl.restarts != 1 {
		t.Errorf("RestartToUpdate called %d times, want 1", ctrl.restarts)
	}
}

type fakeUpdateControl struct {
	status   update.Status
	restarts int
}

func (f *fakeUpdateControl) Status() update.Status { return f.status }
func (f *fakeUpdateControl) RestartToUpdate()      { f.restarts++ }

func TestAdminWatchLog(t *testing.T) {
	ts, b, l := newTestAdmin(t)
	id := kidID(t, ts, "Alex", "1111")
	publishShow(t, l, "show-bluey", "Bluey")
	ep := core.EpisodeID("ep-Bluey")
	ctx := context.Background()
	if err := l.RecordWatch(ctx, core.KidID(id), ep, "Bluey Episode 1", 5, false); err != nil {
		t.Fatalf("RecordWatch: %v", err)
	}
	if err := l.RecordWatch(ctx, core.KidID(id), ep, "Bluey Episode 1", 0, true); err != nil {
		t.Fatalf("RecordWatch free: %v", err)
	}
	if _, _, err := b.Adjust(ctx, core.KidID(id), budget.DefaultCounter, -3, true); err != nil {
		t.Fatalf("adjust: %v", err)
	}

	log := state(t, ts)["watch_log"].([]any)
	if len(log) != 2 {
		t.Fatalf("watch log has %d entries, want 2", len(log))
	}
	first := log[0].(map[string]any)
	if at, _ := first["at"].(string); !strings.Contains(at, "T") {
		t.Errorf("watch entry time = %v, want a local timestamp", first["at"])
	}
	if first["kid_name"] != "Alex" || first["show"] != "Bluey" || first["title"] != "Bluey Episode 1" {
		t.Errorf("watch entry joins = %v, want Alex / Bluey / title", first)
	}
	if first["free"] != true {
		t.Errorf("newest entry is the free one: %v", first)
	}
	second := log[1].(map[string]any)
	if second["free"] != false || second["cost"] != float64(5) {
		t.Errorf("older entry cost = %v free %v, want 5 and false", second["cost"], second["free"])
	}
}

func TestAdminCounter(t *testing.T) {
	ts, _, _ := newTestAdmin(t)
	if c := state(t, ts)["settings"].(map[string]any)["counter"]; c != "video" {
		t.Errorf("default practice counter = %v, want video", c)
	}
	if resp, _ := post(t, ts.URL+"/api/admin/settings/counter", map[string]any{"counter": "math"}); resp.StatusCode != http.StatusOK {
		t.Errorf("set counter: status %d, want 200", resp.StatusCode)
	}
	if c := state(t, ts)["settings"].(map[string]any)["counter"]; c != "math" {
		t.Errorf("practice counter after set = %v, want math", c)
	}
	if resp, _ := post(t, ts.URL+"/api/admin/settings/counter", map[string]any{"counter": "a/b"}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("counter with a slash: status %d, want 400", resp.StatusCode)
	}
	if resp, _ := post(t, ts.URL+"/api/admin/settings/counter", map[string]any{"counter": ""}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty counter: status %d, want 400", resp.StatusCode)
	}
}

func TestAdminBudgets(t *testing.T) {
	ts, _, _ := newTestAdmin(t)
	if on := state(t, ts)["budgets_on"]; on != true {
		t.Error("budgets should default to on")
	}
	if resp, _ := post(t, ts.URL+"/api/admin/settings/budgets", map[string]any{"on": false}); resp.StatusCode != http.StatusOK {
		t.Errorf("budgets off: status %d, want 200", resp.StatusCode)
	}
	if on := state(t, ts)["budgets_on"]; on != false {
		t.Error("budgets_on should be false after the toggle")
	}
	if resp, _ := post(t, ts.URL+"/api/admin/settings/budgets", map[string]any{"on": true}); resp.StatusCode != http.StatusOK {
		t.Errorf("budgets on: status %d, want 200", resp.StatusCode)
	}
	if on := state(t, ts)["budgets_on"]; on != true {
		t.Error("budgets_on should be true after the toggle back")
	}
}

func TestAdminSafariScreens(t *testing.T) {
	ts, _, _ := newTestAdmin(t)
	settings := state(t, ts)["settings"].(map[string]any)
	if on := settings["safari_screens"]; on != true {
		t.Errorf("safari_screens = %v, want the default true", on)
	}
	if resp, _ := post(t, ts.URL+"/api/admin/settings/safari", map[string]any{"on": false}); resp.StatusCode != http.StatusOK {
		t.Errorf("safari off: status %d, want 200", resp.StatusCode)
	}
	if on := state(t, ts)["settings"].(map[string]any)["safari_screens"]; on != false {
		t.Error("safari_screens should be false after the toggle")
	}
	if resp, _ := post(t, ts.URL+"/api/admin/settings/safari", map[string]any{"on": true}); resp.StatusCode != http.StatusOK {
		t.Errorf("safari on: status %d, want 200", resp.StatusCode)
	}
	if on := state(t, ts)["settings"].(map[string]any)["safari_screens"]; on != true {
		t.Error("safari_screens should be true after the toggle back")
	}
}

// TestAdminHWEncode: the admin state carries the household "use the
// graphics chip" setting and the encoder name, and the settings checkbox
// endpoint toggles the setting.
func TestAdminHWEncode(t *testing.T) {
	ts, _, _ := newTestAdmin(t, func(d *Deps) {
		d.VideoEncoder = func() string { return "Intel or AMD graphics (VA-API)" }
	})
	settings := state(t, ts)["settings"].(map[string]any)
	if on := settings["hw_encode"]; on != true {
		t.Errorf("hw_encode = %v, want the default true", on)
	}
	if name := settings["video_encoder"]; name != "Intel or AMD graphics (VA-API)" {
		t.Errorf("video_encoder = %v, want the wired encoder name", name)
	}
	if resp, _ := post(t, ts.URL+"/api/admin/settings/hwencode", map[string]any{"on": false}); resp.StatusCode != http.StatusOK {
		t.Errorf("hwencode off: status %d, want 200", resp.StatusCode)
	}
	if on := state(t, ts)["settings"].(map[string]any)["hw_encode"]; on != false {
		t.Error("hw_encode should be false after the toggle")
	}
	if resp, _ := post(t, ts.URL+"/api/admin/settings/hwencode", map[string]any{"on": true}); resp.StatusCode != http.StatusOK {
		t.Errorf("hwencode on: status %d, want 200", resp.StatusCode)
	}
	if on := state(t, ts)["settings"].(map[string]any)["hw_encode"]; on != true {
		t.Error("hw_encode should be true after the toggle back")
	}
}

// TestAdminHWEncodeWithoutVideoEncoder: without a wired encoder name the
// state still carries the setting, and the name stays absent (the parent
// page's JS then shows "not checked yet").
func TestAdminHWEncodeWithoutVideoEncoder(t *testing.T) {
	ts, _, _ := newTestAdmin(t)
	settings := state(t, ts)["settings"].(map[string]any)
	if on := settings["hw_encode"]; on != true {
		t.Errorf("hw_encode = %v, want the default true", on)
	}
	if name := settings["video_encoder"]; name != "" {
		t.Errorf("video_encoder = %v, want empty without a VideoEncoder func", name)
	}
}

func TestAdminDefaultMinutes(t *testing.T) {
	ts, _, _ := newTestAdmin(t)
	if m := state(t, ts)["default_minutes"]; m != float64(10) {
		t.Errorf("default minutes = %v, want 10", m)
	}
	if resp, _ := post(t, ts.URL+"/api/admin/settings/default", map[string]any{"minutes": 15}); resp.StatusCode != http.StatusOK {
		t.Errorf("set default minutes: status %d, want 200", resp.StatusCode)
	}
	if m := state(t, ts)["default_minutes"]; m != float64(15) {
		t.Errorf("default minutes after set = %v, want 15", m)
	}
	if resp, _ := post(t, ts.URL+"/api/admin/settings/default", map[string]any{"minutes": -5}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("negative default: status %d, want 400", resp.StatusCode)
	}
	// An emptied box (missing or null minutes) is refused, never saved as 0.
	for _, body := range []map[string]any{{}, {"minutes": nil}} {
		if resp, out := post(t, ts.URL+"/api/admin/settings/default", body); resp.StatusCode != http.StatusBadRequest || out["error"] != "Type a number of minutes, like 30." {
			t.Errorf("default %v: status %d %v, want 400 and the plain message", body, resp.StatusCode, out)
		}
	}
	if m := state(t, ts)["default_minutes"]; m != float64(15) {
		t.Errorf("default minutes after an empty save = %v, want 15 kept", m)
	}
}

func TestAdminGrownupPIN(t *testing.T) {
	ts, b, _ := newTestAdmin(t)
	ctx := context.Background()
	pinSet := func() any { return state(t, ts)["settings"].(map[string]any)["grownup_pin_set"] }
	if v := pinSet(); v != false {
		t.Errorf("grownup_pin_set before any PIN = %v, want false", v)
	}
	if b.VerifyGrownupPIN(ctx, "7364") {
		t.Fatal("grown-up PIN verifies before being set")
	}
	if resp, _ := post(t, ts.URL+"/api/admin/settings/grownup", map[string]any{"pin": "7364"}); resp.StatusCode != http.StatusOK {
		t.Errorf("set grownup pin: status %d, want 200", resp.StatusCode)
	}
	if !b.VerifyGrownupPIN(ctx, "7364") {
		t.Error("grown-up PIN does not verify after being set")
	}
	if v := pinSet(); v != true {
		t.Errorf("grownup_pin_set after setting = %v, want true", v)
	}
	if resp, _ := post(t, ts.URL+"/api/admin/settings/grownup", map[string]any{"pin": "12"}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad grownup pin: status %d, want 400", resp.StatusCode)
	}
	if resp, _ := post(t, ts.URL+"/api/admin/settings/grownup", map[string]any{"pin": ""}); resp.StatusCode != http.StatusOK {
		t.Errorf("clear grownup pin: status %d, want 200", resp.StatusCode)
	}
	if b.VerifyGrownupPIN(ctx, "7364") {
		t.Error("grown-up PIN still verifies after clearing")
	}
	if v := pinSet(); v != false {
		t.Errorf("grownup_pin_set after clearing = %v, want false", v)
	}
}

// TestAdminKidNameCountsLetters: the 40-letter name limit counts letters, so
// accented letters and emoji (several bytes each) fit, and 41 letters do not.
func TestAdminKidNameCountsLetters(t *testing.T) {
	ts, _, _ := newTestAdmin(t)
	name := "Ébène-Zoë " + strings.Repeat("🌟", 30) // 40 letters, far more than 40 bytes
	if resp, out := post(t, ts.URL+"/api/admin/kids", map[string]any{"name": name, "pin": "1111"}); resp.StatusCode != http.StatusOK {
		t.Errorf("40-letter name with accents and emoji: status %d %v, want 200", resp.StatusCode, out)
	}
	if resp, _ := post(t, ts.URL+"/api/admin/kids", map[string]any{"name": strings.Repeat("é", 41), "pin": "2222"}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("41-letter name: status %d, want 400", resp.StatusCode)
	}
}

// TestAdminUpdateMessagesPlain: the Updates card never shows the updater's
// technical words; each state reads as plain words with a next step.
func TestAdminUpdateMessagesPlain(t *testing.T) {
	cases := []struct {
		st   update.Status
		want string
	}{
		{update.Status{State: update.StateChecking, Message: "checking for updates"}, "Looking for a new version of kids-shelf."},
		{update.Status{State: update.StateIdle, Message: "no update available"}, "kids-shelf is up to date."},
		{update.Status{State: update.StateStaged, Staged: "v1.2.3", Message: "version v1.2.3 is staged and ready; restart to apply it"},
			"A new version (v1.2.3) is ready. Press Restart to update to start using it. Videos stop for a moment while kids-shelf restarts."},
		{update.Status{State: update.StateStaged, Staged: "v1.2.3", Message: "an update is being applied; the app will restart on its own"},
			"A new version is being set up. kids-shelf restarts on its own, and this page comes back when it is ready."},
		{update.Status{State: update.StateFailed, Message: "the download did not match its checksum"},
			"The new version that was downloaded was damaged, so it was not used. Nothing changed; kids-shelf tries again tomorrow."},
		{update.Status{State: update.StateFailed, Message: "the release is not signed with the expected key"},
			"The new version that was downloaded was damaged, so it was not used. Nothing changed; kids-shelf tries again tomorrow."},
		{update.Status{State: update.StateFailed, Message: "there is not enough free disk space"},
			"The update needs more free space on this computer. Remove some videos; kids-shelf tries again tomorrow."},
		{update.Status{State: update.StateFailed, Message: "could not check for or install the update"},
			"kids-shelf could not reach the update server. Check that this computer is on the internet; it tries again tomorrow."},
		{update.Status{State: update.StateFailed, Message: "the update to v1.2.3 did not start properly, so kids-shelf went back to v1.2.2"},
			"The new version did not start properly, so kids-shelf went back to the one it had before. Nothing else changed."},
	}
	for _, c := range cases {
		ctrl := &fakeUpdateControl{status: c.st}
		ts, _, _ := newTestAdmin(t, func(d *Deps) { d.Updates = ctrl })
		if got := state(t, ts)["updates"].(map[string]any)["message"]; got != c.want {
			t.Errorf("%s %q: message %q, want %q", c.st.State, c.st.Message, got, c.want)
		}
	}
}

func TestAdminStaticFiles(t *testing.T) {
	ts, _, _ := newTestAdmin(t)
	resp, err := http.Get(ts.URL + "/admin")
	if err != nil {
		t.Fatalf("GET /admin: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin: status %d, want 200", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	html := string(b)
	if !strings.Contains(html, "kids-shelf · parent page") || !strings.Contains(html, "/admin/static/admin.js") {
		t.Errorf("GET /admin did not serve the dashboard shell")
	}
	// Where to open kids-shelf again is this OS's own place (the Dock on a
	// Mac), never a placeholder or a desktop icon a Mac does not have.
	if strings.Contains(html, "__KS_REOPEN__") || !strings.Contains(html, `data-reopen="`+reopenWhere()+`"`) ||
		!strings.Contains(html, "open it again from "+reopenWhere()+".") {
		t.Errorf("GET /admin did not fill in where to reopen kids-shelf (%q)", reopenWhere())
	}
	if runtime.GOOS == "darwin" && strings.Contains(reopenWhere(), "desktop") {
		t.Errorf("reopenWhere on a Mac = %q, want the Dock or Applications", reopenWhere())
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET /admin Content-Type = %q, want text/html", ct)
	}

	resp, err = http.Get(ts.URL + "/admin/static/admin.js")
	if err != nil {
		t.Fatalf("GET admin.js: %v", err)
	}
	resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("admin.js Content-Type = %q, want text/javascript", ct)
	}

	resp, err = http.Get(ts.URL + "/admin/static/test-av.mp4")
	if err != nil {
		t.Fatalf("GET test-av.mp4: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET test-av.mp4: status %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "video/mp4" {
		t.Errorf("test-av.mp4 Content-Type = %q, want video/mp4", ct)
	}
	clip, _ := io.ReadAll(resp.Body)
	if len(clip) < 100_000 {
		t.Errorf("test-av.mp4 is only %d bytes, want a real clip", len(clip))
	}

	if resp, _ := doReq(t, http.MethodGet, ts.URL+"/admin/static/nope.txt", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing static file: status %d, want 404", resp.StatusCode)
	}
}

// TestParentPagesVersionAssets: the parent page and the content page load
// their script and stylesheet by URLs carrying the embedded file's content
// hash, so a page served after an upgrade never runs with the previous
// build's script still cached for an hour under the same URL; the
// versioned URLs serve the files.
func TestParentPagesVersionAssets(t *testing.T) {
	check := func(t *testing.T, ts *httptest.Server, page string, files embed.FS, dir, prefix string, names ...string) {
		t.Helper()
		resp, err := http.Get(ts.URL + page)
		if err != nil {
			t.Fatalf("GET %s: %v", page, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		html := string(b)
		for _, name := range names {
			data, err := files.ReadFile(dir + "/" + name)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(data)
			u := prefix + name + "?v=" + hex.EncodeToString(sum[:6])
			if !strings.Contains(html, `"`+u+`"`) || strings.Contains(html, `"`+prefix+name+`"`) {
				t.Errorf("GET %s does not load %s as %s", page, name, u)
			}
			resp, err := http.Get(ts.URL + u)
			if err != nil {
				t.Fatalf("GET %s: %v", u, err)
			}
			got, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK || !bytes.Equal(got, data) {
				t.Errorf("GET %s = %d, %d bytes; want the embedded file", u, resp.StatusCode, len(got))
			}
		}
	}
	ts, _, _ := newTestAdmin(t)
	check(t, ts, "/admin", adminFiles, "static/admin", "/admin/static/", "admin.js", "admin.css")
	cts := contentTestServer(t, Deps{DataDir: t.TempDir()})
	check(t, cts, "/admin/content", contentFiles, "static/content", "/admin/content/static/", "content.js", "content.css")
}

func TestAdminServicesDown(t *testing.T) {
	// With no library the state endpoint answers 503 in plain words.
	ts, _, _ := newTestAdmin(t, func(d *Deps) { d.Library = nil })
	resp, out := doReq(t, http.MethodGet, ts.URL+"/api/admin/state", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("state without library: status %d, want 503", resp.StatusCode)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "library") {
		t.Errorf("503 error = %q, want plain words mentioning the library", msg)
	}
}

func TestQRCodeSizes(t *testing.T) {
	// A LAN address lands in version 2; the quiet zone is included.
	svg, err := qrSVG("http://192.0.2.18:8070/")
	if err != nil {
		t.Fatalf("qrSVG: %v", err)
	}
	if !strings.Contains(svg, `viewBox="0 0 33 33"`) {
		t.Errorf("address QR viewBox = %q, want version 2 (33 with quiet zone)", svg[:120])
	}
	// A long string needs version 8 (multi-block interleave).
	long := strings.Repeat("0123456789ABCDEF", 8)[:125]
	svg, err = qrSVG(long)
	if err != nil {
		t.Fatalf("qrSVG long: %v", err)
	}
	if !strings.Contains(svg, `viewBox="0 0 57 57"`) {
		t.Errorf("long QR viewBox = %q, want version 10 (57 with quiet zone)", svg[:120])
	}
	// Beyond the supported table: a plain error, not a panic.
	if _, err := qrSVG(strings.Repeat("x", 300)); err == nil {
		t.Error("300 bytes should be too long for a QR code")
	}
}

// fakeFirewall is a scriptable FirewallControl: scripts must be atomic so
// statuses are set before any request runs.
type fakeFirewall struct {
	status  FirewallStatus
	err     error
	allows  int
	allowed atomic.Bool // flipped by Allow so handlers can observe it
}

func (f *fakeFirewall) Status(ctx context.Context) FirewallStatus {
	return f.status
}

func (f *fakeFirewall) Allow(ctx context.Context) error {
	f.allows++
	f.allowed.Store(true)
	return f.err
}

// TestAdminProblemsShape checks both shapes of /api/admin/problems: with a
// firewall and helper problem reported, and with neither (firewall is
// omitted entirely).
func TestAdminProblemsShape(t *testing.T) {
	ts, _, _ := newTestAdmin(t, func(d *Deps) {
		d.Firewall = &fakeFirewall{status: FirewallStatus{Blocked: true, Detail: "Windows Firewall is blocking kids-shelf."}}
		d.HelpersProblem = func() string { return "The video tools are missing." }
	})
	resp, out := doReq(t, http.MethodGet, ts.URL+"/api/admin/problems", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/admin/problems: status %d body %v", resp.StatusCode, out)
	}
	if helpers, _ := out["helpers"].(string); helpers != "The video tools are missing." {
		t.Errorf("helpers = %q, want the problem text", helpers)
	}
	fw, ok := out["firewall"].(map[string]any)
	if !ok {
		t.Fatalf("firewall missing from %v", out)
	}
	if b, _ := fw["blocked"].(bool); !b {
		t.Errorf("firewall.blocked = %v, want true", fw["blocked"])
	}
	if u, _ := fw["unknown"].(bool); u {
		t.Errorf("firewall.unknown = %v, want false", fw["unknown"])
	}
	if dt, _ := fw["detail"].(string); dt == "" {
		t.Error("firewall.detail missing")
	}

	// Without a firewall there is no firewall field at all, and without a
	// helper check the helpers field is the empty string.
	ts2, _, _ := newTestAdmin(t)
	resp2, out2 := doReq(t, http.MethodGet, ts2.URL+"/api/admin/problems", nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("problems without firewall: status %d body %v", resp2.StatusCode, out2)
	}
	if _, has := out2["firewall"]; has {
		t.Errorf("firewall present without a control: %v", out2)
	}
	if helpers, ok := out2["helpers"].(string); !ok || helpers != "" {
		t.Errorf("helpers without a check = %v, want \"\"", out2["helpers"])
	}
}

// TestAdminFirewallAllowSuccess runs the happy path: Allow is called, the
// fresh status comes back, and the not-blocked result is what the page
// needs to clear the banner.
func TestAdminFirewallAllowSuccess(t *testing.T) {
	fake := &fakeFirewall{status: FirewallStatus{Blocked: true, Detail: "blocked"}}
	ts, _, _ := newTestAdmin(t, func(d *Deps) { d.Firewall = fake })
	// The allow flips the state to open, as the real controller does
	// after a successful elevated run.
	fake.status = FirewallStatus{Blocked: false, Detail: "Windows Firewall allows kids-shelf on this network."}

	resp, out := post(t, ts.URL+"/api/admin/firewall/allow", map[string]any{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/firewall/allow: status %d body %v", resp.StatusCode, out)
	}
	if fake.allows != 1 {
		t.Errorf("Allow called %d times, want 1", fake.allows)
	}
	if !fake.allowed.Load() {
		t.Error("Allow was not reached")
	}
	fw, ok := out["firewall"].(map[string]any)
	if !ok {
		t.Fatalf("fresh firewall status missing from %v", out)
	}
	if b, _ := fw["blocked"].(bool); b {
		t.Errorf("firewall.blocked after allow = %v, want false", fw["blocked"])
	}
}

// TestAdminFirewallAllowDenied maps a declined UAC prompt to the exact
// plain message the page shows.
func TestAdminFirewallAllowDenied(t *testing.T) {
	fake := &fakeFirewall{err: firewall.ErrDeclined}
	ts, _, _ := newTestAdmin(t, func(d *Deps) { d.Firewall = fake })
	resp, out := post(t, ts.URL+"/api/admin/firewall/allow", map[string]any{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("declined allow: status %d body %v", resp.StatusCode, out)
	}
	if msg, _ := out["error"].(string); msg != msgFirewallDenied {
		t.Errorf("declined error = %q, want %q", msg, msgFirewallDenied)
	}
}

// TestAdminFirewallAllowFailure falls back to the generic message when the
// failure is not a declined prompt.
func TestAdminFirewallAllowFailure(t *testing.T) {
	fake := &fakeFirewall{err: errors.New("boom")}
	ts, _, _ := newTestAdmin(t, func(d *Deps) { d.Firewall = fake })
	resp, out := post(t, ts.URL+"/api/admin/firewall/allow", map[string]any{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("failed allow: status %d body %v", resp.StatusCode, out)
	}
	if msg, _ := out["error"].(string); strings.Contains(msg, "boom") {
		t.Errorf("error leaks internal text: %q", msg)
	}
}

// TestAdminProblemsPINGate closes both endpoints when the request carries
// no session cookie, the same gate as every other /api/admin route.
func TestAdminProblemsPINGate(t *testing.T) {
	ts, _, _ := newTestAdmin(t, func(d *Deps) {
		d.Firewall = &fakeFirewall{status: FirewallStatus{Blocked: true}}
		d.HelpersProblem = func() string { return "problem" }
	})
	// No cookie means 401, the session cookie 200.
	if resp, _ := visitorReq(t, http.MethodGet, ts.URL+"/api/admin/problems", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("problems without cookie: status %d, want 401", resp.StatusCode)
	}
	if resp, _ := visitorReq(t, http.MethodPost, ts.URL+"/api/admin/firewall/allow", map[string]any{}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("allow without cookie: status %d, want 401", resp.StatusCode)
	}
	c := login(t, ts, testParentPIN)
	if resp, _ := doReq(t, http.MethodGet, ts.URL+"/api/admin/problems", nil, c); resp.StatusCode != http.StatusOK {
		t.Errorf("problems with cookie: status %d, want 200", resp.StatusCode)
	}
}

// TestAdminFirewallSameOrigin checks the middleware gate: a bodyless POST
// without an Origin or Referer never reaches the handler.
func TestAdminFirewallSameOrigin(t *testing.T) {
	ts, _, _ := newTestAdmin(t, func(d *Deps) {
		d.Firewall = &fakeFirewall{status: FirewallStatus{Blocked: true}}
	})
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/admin/firewall/allow", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("cross-site allow: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("allow without Origin = %d, want 403", resp.StatusCode)
	}
}

// TestAdminFirewallNilControl answers 503 in plain words, like every other
// absent service in the dashboard.
func TestAdminFirewallNilControl(t *testing.T) {
	ts, _, _ := newTestAdmin(t)
	resp, out := post(t, ts.URL+"/api/admin/firewall/allow", map[string]any{})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("allow without a firewall control: status %d, want 503", resp.StatusCode)
	}
	if msg, _ := out["error"].(string); msg == "" {
		t.Error("503 error is empty; want plain words")
	}
}

// TestAdminRemove wires the remove route end to end: the info fetch names
// the two folders, the hook receives the deleteData flag, and a copy
// without the hook answers 503.
func TestAdminRemove(t *testing.T) {
	var got atomic.Value                                     // bool
	dataDir := filepath.Join(t.TempDir(), "the-data-folder") // holds the parent PIN
	if err := os.Mkdir(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ts, _, _ := newTestAdmin(t, func(d *Deps) {
		d.DataDir = dataDir
		d.MediaDir = "the-media-folder"
		d.Remove = func(deleteData bool) error {
			got.Store(deleteData)
			return nil
		}
	})

	if resp, out := doReq(t, http.MethodGet, ts.URL+"/api/admin/remove", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("remove info: status %d, want 200", resp.StatusCode)
	} else if out["data_dir"] != dataDir || out["media_dir"] != "the-media-folder" {
		t.Errorf("remove info = %v, want the two folder paths", out)
	}

	if resp, _ := post(t, ts.URL+"/api/admin/remove", map[string]any{"deleteData": true}); resp.StatusCode != http.StatusOK {
		t.Fatalf("remove with deleteData: status %d, want 200", resp.StatusCode)
	}
	if v, ok := got.Load().(bool); !ok || !v {
		t.Errorf("Remove called with %v, want true", got.Load())
	}
	if resp, _ := post(t, ts.URL+"/api/admin/remove", map[string]any{"deleteData": false}); resp.StatusCode != http.StatusOK {
		t.Fatalf("remove without deleteData: status %d, want 200", resp.StatusCode)
	}
	if v, ok := got.Load().(bool); !ok || v {
		t.Errorf("Remove called with %v, want false", got.Load())
	}

	tsNil, _, _ := newTestAdmin(t)
	if resp, _ := post(t, tsNil.URL+"/api/admin/remove", map[string]any{}); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("remove without a hook: status %d, want 503", resp.StatusCode)
	}
}

// TestAdminRemoveSameOrigin checks the middleware gate: a state changing
// POST without an Origin or Referer never reaches the remove handler.
func TestAdminRemoveSameOrigin(t *testing.T) {
	ts, _, _ := newTestAdmin(t, func(d *Deps) {
		d.Remove = func(bool) error { t.Error("Remove ran without a same-origin request"); return nil }
	})
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/admin/remove", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("cross-site remove: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("remove without Origin = %d, want 403", resp.StatusCode)
	}
}

// TestAdminRemovePINGate: removal, like every parent-page mutation, needs
// the session cookie.
func TestAdminRemovePINGate(t *testing.T) {
	var removes atomic.Int32
	ts, _, _ := newTestAdmin(t, func(d *Deps) {
		d.Remove = func(bool) error { removes.Add(1); return nil }
	})
	if resp, _ := visitorReq(t, http.MethodPost, ts.URL+"/api/admin/remove", map[string]any{"deleteData": true}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("remove without cookie: status %d, want 401", resp.StatusCode)
	}
	if n := removes.Load(); n != 0 {
		t.Errorf("Remove ran %d time(s) without the dashboard PIN gate", n)
	}
	c := login(t, ts, testParentPIN)
	if resp, _ := post(t, ts.URL+"/api/admin/remove", map[string]any{"deleteData": true}, c); resp.StatusCode != http.StatusOK {
		t.Errorf("remove with cookie: status %d, want 200", resp.StatusCode)
	}
	if n := removes.Load(); n != 1 {
		t.Errorf("Remove ran %d time(s) with the cookie, want 1", n)
	}
}

// TestAdminQuitAndRemoveDuringTrial: while Deps.InTrial reports an
// unconfirmed update trial, both Quit and Remove answer 409 with the plain
// "finishing an update" message and the hooks never run, so a parent cannot
// turn a healthy update into a rejected one. When the trial settles they
// work normally.
func TestAdminQuitAndRemoveDuringTrial(t *testing.T) {
	var inTrial atomic.Bool
	inTrial.Store(true)
	var quits atomic.Int32
	var removes atomic.Int32
	ts, _, _ := newTestAdmin(t, func(d *Deps) {
		d.InTrial = inTrial.Load
		d.Quit = func() { quits.Add(1) }
		d.Remove = func(deleteData bool) error { removes.Add(1); return nil }
	})

	msg := "One moment: kids-shelf is finishing an update."
	if resp, out := post(t, ts.URL+"/api/admin/quit", nil); resp.StatusCode != http.StatusConflict {
		t.Errorf("quit during trial: status %d, want 409", resp.StatusCode)
	} else if err, _ := out["error"].(string); err != msg {
		t.Errorf("quit during trial: error %q, want %q", err, msg)
	}
	if resp, out := post(t, ts.URL+"/api/admin/remove", map[string]any{"deleteData": true}); resp.StatusCode != http.StatusConflict {
		t.Errorf("remove during trial: status %d, want 409", resp.StatusCode)
	} else if err, _ := out["error"].(string); err != msg {
		t.Errorf("remove during trial: error %q, want %q", err, msg)
	}
	time.Sleep(50 * time.Millisecond) // the quit hook would have fired by now
	if quits.Load() != 0 {
		t.Errorf("Quit ran %d times during a trial, want 0", quits.Load())
	}
	if removes.Load() != 0 {
		t.Errorf("Remove ran %d times during a trial, want 0", removes.Load())
	}

	// The trial settled: both work again.
	inTrial.Store(false)
	if resp, _ := post(t, ts.URL+"/api/admin/remove", map[string]any{"deleteData": true}); resp.StatusCode != http.StatusOK {
		t.Errorf("remove after trial: status %d, want 200", resp.StatusCode)
	}
	if resp, _ := post(t, ts.URL+"/api/admin/quit", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("quit after trial: status %d, want 200", resp.StatusCode)
	}
	deadline := time.Now().Add(2 * time.Second)
	for quits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if quits.Load() != 1 || removes.Load() != 1 {
		t.Errorf("after the trial: quits=%d removes=%d, want 1 and 1", quits.Load(), removes.Load())
	}
}
