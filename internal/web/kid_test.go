package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edementhon/kids-shelf/internal/budget"
	"github.com/edementhon/kids-shelf/internal/core"
	"github.com/edementhon/kids-shelf/internal/library"
	"github.com/edementhon/kids-shelf/internal/store"
)

// kidEnv is one running server over real budget and library services on a
// temp store, with a cookie-jar client so unlock sessions work end to end.
type kidEnv struct {
	ts     *httptest.Server
	s      *Server
	lib    *library.Library
	bud    *budget.Budget
	client *http.Client
	ada    core.KidID // the "Ada" kid profile
	ctx    context.Context
	t      *testing.T
}

// fixedNow pins the server clock (and the budget's day boundary) to noon
// UTC on 2026-10-26, so imported accounts land on that day.
func fixedNow() time.Time {
	return time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
}

// newKidEnv builds a Server over real services on a temp store, with a
// cookie-jar client so unlock sessions work end to end. The shared PIN
// throttle is reset so no other test's wrong tries lock this one out.
func newKidEnv(t *testing.T) *kidEnv {
	t.Helper()
	now := fixedNow()
	return newKidEnvPin(t, &now, "9999")
}

// newKidEnvNoPin is newKidEnv for a fresh household that has not configured
// the grown-up watch-together PIN yet.
func newKidEnvNoPin(t *testing.T) *kidEnv {
	t.Helper()
	now := fixedNow()
	return newKidEnvPin(t, &now, "")
}

// newKidEnvAt is newKidEnv with a clock the test controls: the env's server
// clock reads *now, so advancing it exercises lockout expiry.
func newKidEnvAt(t *testing.T, now *time.Time) *kidEnv {
	t.Helper()
	return newKidEnvPin(t, now, "9999")
}

func newKidEnvPin(t *testing.T, now *time.Time, grownupPIN string) *kidEnv {
	t.Helper()
	pinThrottle.reset()
	st, err := store.Open(filepath.Join(t.TempDir(), "kid.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(budget.Migrations...); err != nil {
		t.Fatalf("budget migrations: %v", err)
	}
	if err := st.Migrate(library.Migrations...); err != nil {
		t.Fatalf("library migrations: %v", err)
	}
	bud, err := budget.New(st, budget.WithClock(func() time.Time { return *now }))
	if err != nil {
		t.Fatalf("budget.New: %v", err)
	}
	lib, err := library.New(st)
	if err != nil {
		t.Fatalf("library.New: %v", err)
	}
	ctx := context.Background()
	ada, err := bud.CreateKid(ctx, "Ada", "1234")
	if err != nil {
		t.Fatalf("CreateKid: %v", err)
	}
	if grownupPIN != "" {
		if err := bud.SetGrownupPIN(ctx, grownupPIN); err != nil {
			t.Fatalf("SetGrownupPIN: %v", err)
		}
	}

	env := &kidEnv{
		lib:    lib,
		bud:    bud,
		ada:    ada,
		ctx:    ctx,
		t:      t,
		client: newClient(),
	}
	s := NewApp("kid-test", Deps{Budget: bud, Library: lib, Now: func() time.Time { return *now }})
	env.s = s
	env.ts = httptest.NewServer(s.Mux())
	t.Cleanup(env.ts.Close)
	return env
}

// newKidEnvClock is newKidEnvAt with an advance function, for tests that
// must let a lockout expire.
func newKidEnvClock(t *testing.T) (*kidEnv, func(time.Duration)) {
	t.Helper()
	now := fixedNow()
	return newKidEnvAt(t, &now), func(d time.Duration) { now = now.Add(d) }
}

func newClient() *http.Client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		panic(err)
	}
	return &http.Client{Jar: jar}
}

// publishShow files a show of the given bin with one episode; the episode's
// File and Thumb point at real bytes in env's temp media dir.
func (e *kidEnv) publishShow(showID, showName, bin, epID, title string, dur int64) *library.Episode {
	e.t.Helper()
	if err := e.lib.SetShowBin(e.ctx, showName, bin); err != nil {
		e.t.Fatalf("SetShowBin(%s): %v", showName, err)
	}
	// The real path, as ingest records it: serveOriginal refuses a path
	// that resolves elsewhere, and macOS's temp dir (/var/folders) is a
	// symlink into /private/var (macos-smoke runners: 404).
	dir, err := filepath.EvalSymlinks(e.t.TempDir())
	if err != nil {
		e.t.Fatal(err)
	}
	file, thumb := filepath.Join(dir, epID+".mp4"), filepath.Join(dir, epID+".jpg")
	if err := os.WriteFile(file, []byte("MP4TESTDATA\n"), 0o644); err != nil {
		e.t.Fatalf("write media file: %v", err)
	}
	if err := os.WriteFile(thumb, []byte("THUMBJPEG"), 0o644); err != nil {
		e.t.Fatalf("write thumb: %v", err)
	}
	ep := core.Episode{
		ID:          core.EpisodeID(epID),
		ShowID:      showID,
		Title:       title,
		File:        file,
		DurationSec: dur,
		Thumb:       thumb,
		Source:      core.Source{Kind: core.SourceFolder, Path: showName + "/" + epID + ".mp4", Title: title, Show: showName},
	}
	if err := e.lib.Publish(e.ctx, ep); err != nil {
		e.t.Fatalf("Publish(%s): %v", epID, err)
	}
	got, err := e.lib.Episode(e.ctx, ep.ID)
	if err != nil {
		e.t.Fatalf("Episode(%s): %v", epID, err)
	}
	return &got
}

// publishWithFile files an episode whose File is the given path and Owned
// flag (a folder/NAS original when owned=false); the file may or may not
// exist, depending on what the test wants to serve.
func (e *kidEnv) publishWithFile(showID, showName, epID, title string, file string, owned bool) {
	e.t.Helper()
	if err := e.lib.SetShowBin(e.ctx, showName, library.BinFree); err != nil {
		e.t.Fatalf("SetShowBin(%s): %v", showName, err)
	}
	ep := core.Episode{
		ID:          core.EpisodeID(epID),
		ShowID:      showID,
		Title:       title,
		File:        file,
		DurationSec: 120,
		Source:      core.Source{Kind: core.SourceFolder, Path: showName + "/" + epID + ".mp4", Title: title, Show: showName},
		Owned:       owned,
	}
	if err := e.lib.Publish(e.ctx, ep); err != nil {
		e.t.Fatalf("Publish(%s): %v", epID, err)
	}
}

// publishWithStatus files an episode carrying the given fix status with a
// real, servable file, like the show phase does for a folder/NAS episode.
func (e *kidEnv) publishWithStatus(showID, showName, epID, title string, issue, fix string) {
	e.t.Helper()
	if err := e.lib.SetShowBin(e.ctx, showName, library.BinFree); err != nil {
		e.t.Fatalf("SetShowBin(%s): %v", showName, err)
	}
	dir := e.t.TempDir()
	file := filepath.Join(dir, epID+".mp4")
	if err := os.WriteFile(file, []byte("MP4TESTDATA\n"), 0o644); err != nil {
		e.t.Fatalf("write media file: %v", err)
	}
	ep := core.Episode{
		ID:          core.EpisodeID(epID),
		ShowID:      showID,
		Title:       title,
		File:        file,
		DurationSec: 120,
		Thumb:       file + ".jpg",
		Source:      core.Source{Kind: core.SourceFolder, Path: showName + "/" + epID + ".mp4", Title: title, Show: showName},
		Owned:       true,
		Issue:       issue,
		Fix:         fix,
	}
	if err := e.lib.Publish(e.ctx, ep); err != nil {
		e.t.Fatalf("Publish(%s): %v", epID, err)
	}
}

// TestMediaInPlaceRangeAndMissing pins the in-place serving contract: an
// episode whose file is a folder/NAS original serves Range/206 from the
// recorded path, a missing original answers a player-visible 503 with the
// drive message (never a 404, never another path), and a missing converted
// (owned) file keeps the plain 404.
func TestMediaInPlaceRangeAndMissing(t *testing.T) {
	e := newKidEnv(t)
	dir := realTempDir(t) // ingest records real paths (no symlinks)
	present := filepath.Join(dir, "present.mp4")
	if err := os.WriteFile(present, []byte("MP4TESTDATA\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.publishWithFile("show-nas", "NAS Show", "ep-nas", "NAS One", present, false)
	missing := filepath.Join(dir, "gone.mp4") // never created: NAS down
	e.publishWithFile("show-nas", "NAS Show", "ep-missing", "Missing", missing, false)

	// Range against the recorded original path works like any episode.
	req, _ := http.NewRequest("GET", e.ts.URL+"/media/ep-nas.mp4", nil)
	req.Header.Set("Range", "bytes=0-3")
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("Range GET in-place media: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || string(body) != "MP4T" {
		t.Errorf("in-place Range = %d %q, want 206 MP4T", resp.StatusCode, body)
	}

	resp, err = e.client.Get(e.ts.URL + "/media/ep-missing.mp4")
	if err != nil {
		t.Fatalf("GET missing in-place media: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("missing in-place original = %d, want 503", resp.StatusCode)
	}
	if !strings.Contains(string(body), "Is the video drive or NAS connected?") {
		t.Errorf("missing in-place body = %q, want the drive message", body)
	}

	// A missing converted file is still a plain 404.
	e.publishWithFile("show-nas", "NAS Show", "ep-owned", "Owned", present, true)
	if err := os.Remove(present); err != nil {
		t.Fatal(err)
	}
	resp, err = e.client.Get(e.ts.URL + "/media/ep-owned.mp4")
	if err != nil {
		t.Fatalf("GET missing owned media: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing converted file = %d, want 404", resp.StatusCode)
	}
}

// realTempDir is t.TempDir with symlinks resolved (macOS keeps temp dirs
// under /var, a symlink to /private/var); ingest records in-place
// originals the same way.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestMediaInPlaceRefusesSymlinkSwap: an in-place episode plays its original
// from the recorded path, so replacing that file, or a folder above it, with
// a symlink to another file must not make /media hand that file out (the
// server may run as a user who can read far more than videos).
func TestMediaInPlaceRefusesSymlinkSwap(t *testing.T) {
	e := newKidEnv(t)
	root := realTempDir(t)
	secret := filepath.Join(root, "secret.txt")
	if err := os.WriteFile(secret, []byte("PRIVATE"), 0o600); err != nil {
		t.Fatal(err)
	}
	show := filepath.Join(root, "Show")
	if err := os.Mkdir(show, 0o755); err != nil {
		t.Fatal(err)
	}
	ep := filepath.Join(show, "ep.mp4")
	if err := os.WriteFile(ep, []byte("MP4TESTDATA\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.publishWithFile("show-nas", "NAS Show", "ep-nas", "NAS One", ep, false)
	get := func() (int, string) {
		t.Helper()
		resp, err := e.client.Get(e.ts.URL + "/media/ep-nas.mp4")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get(); code != http.StatusOK || body != "MP4TESTDATA\n" {
		t.Fatalf("original = %d %q, want 200 and the video bytes", code, body)
	}

	// The file itself swapped for a symlink.
	if err := os.Remove(ep); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, ep); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if code, body := get(); code == http.StatusOK || strings.Contains(body, "PRIVATE") {
		t.Errorf("file symlink swap = %d %q, want a refusal without the target's bytes", code, body)
	}

	// A folder above it swapped for a symlink to a folder holding another
	// "ep.mp4".
	other := filepath.Join(root, "other")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "ep.mp4"), []byte("PRIVATE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(show); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, show); err != nil {
		t.Fatal(err)
	}
	if code, body := get(); code == http.StatusOK || strings.Contains(body, "PRIVATE") {
		t.Errorf("folder symlink swap = %d %q, want a refusal without the target's bytes", code, body)
	}
}

// seedCatalog publishes the standard test shows: a fun show, a learning
// show and a free show, returning the episode ids.
func (e *kidEnv) seedCatalog() (fun, learn, free core.EpisodeID) {
	e.publishShow("show-fun", "Fun Show", library.BinFun, "ep-fun-1", "Fun One", 180)
	e.publishShow("show-fun", "Fun Show", library.BinFun, "ep-fun-2", "Fun Two", 300)
	e.publishShow("show-words", "Word Show", library.BinLearning, "ep-learn", "Word One", 180)
	e.publishShow("show-free", "Free Zone", library.BinFree, "ep-free", "Free One", 300)
	return "ep-fun-1", "ep-learn", "ep-free"
}

// importToday sets the kid's balance for today exactly (no default
// allowance is granted on top).
func (e *kidEnv) importToday(kid core.KidID, today, saved float64) {
	e.t.Helper()
	if err := e.bud.ImportAccount(e.ctx, kid, today, saved, "2026-10-26"); err != nil {
		e.t.Fatalf("ImportAccount: %v", err)
	}
}

// do performs one request against the server. State changing requests carry
// a same-origin Origin header and JSON bodies, exactly like the app's pages.
func (e *kidEnv) do(method, path string, body any) (*http.Response, map[string]any) {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal body: %v", err)
		}
		r = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, e.ts.URL+path, r)
	if err != nil {
		e.t.Fatalf("NewRequest %s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("Origin", "http://"+req.Host)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp, m
}

// unlockAda enters Ada's PIN through the real unlock API.
func (e *kidEnv) unlockAda() {
	e.t.Helper()
	resp, m := e.do("POST", "/api/kid/unlock", map[string]string{"kid": string(e.ada), "pin": "1234"})
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("unlock: status %d body %v", resp.StatusCode, m)
	}
}

// unlockReq builds the unlock request exactly as the app sends it, from the
// given client IP so one test can simulate several distinct devices.
func (e *kidEnv) unlockReq(fromIP, kid, pin string) *http.Request {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/kid/unlock",
		strings.NewReader(`{"kid":"`+kid+`","pin":"`+pin+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = fromIP + ":1234"
	return req
}

// unlockCode runs the unlock handler for one request and returns the status
// code it wrote.
func (e *kidEnv) unlockCode(fromIP, kid, pin string) int {
	e.t.Helper()
	rec := httptest.NewRecorder()
	e.s.handleUnlock(rec, e.unlockReq(fromIP, kid, pin))
	return rec.Code
}

// consume posts one live-metering tick and returns the decoded response.
func (e *kidEnv) consume(ep string, sec float64) (int, map[string]any) {
	r, m := e.do("POST", "/api/kid/consume", map[string]any{"episode": ep, "sec": sec})
	return r.StatusCode, m
}

func TestUnlockWrongPINRejected(t *testing.T) {
	e := newKidEnv(t)

	for _, pin := range []string{"0000"} {
		resp, m := e.do("POST", "/api/kid/unlock", map[string]string{"kid": string(e.ada), "pin": pin})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("unlock with pin %q: status %d, want 401 (body %v)", pin, resp.StatusCode, m)
		}
		if sc := resp.Header.Get("Set-Cookie"); sc != "" {
			t.Errorf("failed unlock set a cookie: %q", sc)
		}
	}
	// An empty PIN is malformed, not a wrong PIN.
	if resp, _ := e.do("POST", "/api/kid/unlock", map[string]string{"kid": string(e.ada), "pin": ""}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unlock with empty pin: status %d, want 400", resp.StatusCode)
	}
	// An unknown kid id never verifies either.
	if resp, _ := e.do("POST", "/api/kid/unlock", map[string]string{"kid": "c0ffee00000000000000000000000000", "pin": "1234"}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unlock unknown kid: status %d, want 401", resp.StatusCode)
	}

	// No session was created: the kid API must still answer 401.
	if resp, _ := e.do("GET", "/api/kid/me", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("me after failed unlocks: status %d, want 401", resp.StatusCode)
	}
}

// TestKidPINBruteForceThrottle locks an IP after five wrong unlock tries:
// the sixth try, even with the correct PIN, is refused with the plain 429
// message and no session is created.
func TestKidPINBruteForceThrottle(t *testing.T) {
	e := newKidEnv(t)

	for range 5 {
		resp, _ := e.do("POST", "/api/kid/unlock", map[string]string{"kid": string(e.ada), "pin": "0000"})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong pin: status %d, want 401", resp.StatusCode)
		}
	}
	resp, m := e.do("POST", "/api/kid/unlock", map[string]string{"kid": string(e.ada), "pin": "1234"})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("sixth try: status %d, want 429 (body %v)", resp.StatusCode, m)
	}
	msg, _ := m["error"].(string)
	if msg != "Too many tries. Wait a little bit, then try again." {
		t.Errorf("429 message = %q, want the plain kid wording", msg)
	}
	// The keypad counts the lockout down from this field.
	if wait, _ := m["wait"].(float64); wait < 1 {
		t.Errorf("429 wait = %v, want the lockout seconds", m["wait"])
	}
	if resp, _ := e.do("GET", "/api/kid/me", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("me after locked-out unlock: status %d, want 401 (no session)", resp.StatusCode)
	}
}

// TestDeviceWrongGuessesDoNotBlockAnotherDevice lets device A guess wrong
// kid PINs ten times and checks device B's correct PIN still unlocks: there
// is no household-wide lockout and each client IP keeps its own state.
func TestDeviceWrongGuessesDoNotBlockAnotherDevice(t *testing.T) {
	e := newKidEnv(t)
	for range 10 {
		code := e.unlockCode("192.0.2.7", string(e.ada), "0000")
		if code != http.StatusUnauthorized && code != http.StatusTooManyRequests {
			t.Fatalf("device A wrong pin: status %d, want 401 or 429", code)
		}
	}
	if code := e.unlockCode("192.0.2.8", string(e.ada), "1234"); code != http.StatusOK {
		t.Fatalf("device B correct pin after device A's guesses: status %d, want 200", code)
	}
}

// TestRealmBudgetSlowsGuessingButNotCorrectPIN spends the kid realm's
// per-minute budget of wrong tries from twenty distinct devices and checks
// that further wrong attempts are refused with 429 while a fresh device's
// correct PIN still unlocks: the budget only slows guessing.
func TestRealmBudgetSlowsGuessingButNotCorrectPIN(t *testing.T) {
	e := newKidEnv(t)
	for i := range pinAggregateLimit {
		if code := e.unlockCode(fmt.Sprintf("192.0.2.%d", 100+i), string(e.ada), "0000"); code != http.StatusUnauthorized {
			t.Fatalf("wrong pin from device %d: status %d, want 401", i, code)
		}
	}
	if code := e.unlockCode("192.0.2.200", string(e.ada), "0000"); code != http.StatusTooManyRequests {
		t.Fatalf("wrong pin with the realm budget spent: status %d, want 429", code)
	}
	if code := e.unlockCode("192.0.2.201", string(e.ada), "1234"); code != http.StatusOK {
		t.Fatalf("correct pin with the realm budget spent: status %d, want 200", code)
	}
}

// TestKidLockoutExpires locks the client with five wrong tries and checks
// the correct PIN is accepted again once the 30-second lockout has passed.
func TestKidLockoutExpires(t *testing.T) {
	e, advance := newKidEnvClock(t)
	for range 5 {
		if resp, _ := e.do("POST", "/api/kid/unlock", map[string]string{"kid": string(e.ada), "pin": "0000"}); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong pin: status %d, want 401", resp.StatusCode)
		}
	}
	if resp, _ := e.do("POST", "/api/kid/unlock", map[string]string{"kid": string(e.ada), "pin": "1234"}); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("correct pin during the lockout: status %d, want 429", resp.StatusCode)
	}
	advance(31 * time.Second) // the first lockout is 30 s
	if resp, m := e.do("POST", "/api/kid/unlock", map[string]string{"kid": string(e.ada), "pin": "1234"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("correct pin after the lockout: status %d body %v, want 200", resp.StatusCode, m)
	}
}

func TestUnlockSetsSessionCookie(t *testing.T) {
	e := newKidEnv(t)

	resp, m := e.do("POST", "/api/kid/unlock", map[string]string{"kid": string(e.ada), "pin": "1234"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unlock: status %d body %v", resp.StatusCode, m)
	}
	sc := resp.Header.Get("Set-Cookie")
	for _, want := range []string{"HttpOnly", "SameSite=Strict", "Path=/"} {
		if !strings.Contains(sc, want) {
			t.Errorf("Set-Cookie %q missing %q", sc, want)
		}
	}

	resp, m = e.do("GET", "/api/kid/me", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("me: status %d", resp.StatusCode)
	}
	if m["kid"] != string(e.ada) || m["name"] != "Ada" {
		t.Errorf("me = %v, want kid %s name Ada", m, e.ada)
	}
	if m["grownup"] != false {
		t.Errorf("me grownup = %v, want false", m["grownup"])
	}

	resp, m = e.do("GET", "/api/kids", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("kids: status %d", resp.StatusCode)
	}
	kids, _ := m["kids"].([]any)
	if len(kids) != 1 || kids[0].(map[string]any)["name"] != "Ada" {
		t.Errorf("kids = %v, want one entry Ada", m["kids"])
	}
}

func TestWatchTogetherUnlock(t *testing.T) {
	e := newKidEnv(t)

	resp, m := e.do("POST", "/api/kid/watch-together", map[string]string{"pin": "0000"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong grown-up pin: status %d, want 401", resp.StatusCode)
	}

	resp, m = e.do("POST", "/api/kid/watch-together", map[string]string{"pin": "9999"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("watch-together: status %d body %v", resp.StatusCode, m)
	}
	resp, m = e.do("GET", "/api/kid/me", nil)
	if resp.StatusCode != http.StatusOK || m["grownup"] != true {
		t.Errorf("me = status %d body %v, want grownup session", resp.StatusCode, m)
	}
}

func TestConsumeLearningChargesHalf(t *testing.T) {
	e := newKidEnv(t)
	_, learn, _ := e.seedCatalog()
	e.unlockAda()

	// The player sends one 1-second tick per real second of playback. Sixty
	// ticks of a learning show must charge exactly 60/60 × 0.5 = 0.5 min.
	// (Any single request is clamped to 5 s by the server, so this also
	// proves the per-tick path, not the flush path.)
	var last map[string]any
	for range 60 {
		status, m := e.consume(string(learn), 1)
		if status != http.StatusOK {
			t.Fatalf("consume tick: status %d body %v", status, m)
		}
		if m["cutOff"] != false {
			t.Fatalf("consume tick cut off early: %v", m)
		}
		last = m
	}
	today, _ := last["today"].(float64)
	total, _ := last["total"].(float64)
	if today < 9.5-1e-6 || today > 9.5+1e-6 {
		t.Errorf("consume 60 s learning = today %v, want 9.5", today)
	}
	if total < 9.5-1e-6 || total > 9.5+1e-6 {
		t.Errorf("consume 60 s learning = total %v, want 9.5", total)
	}
	if last["free"] != false {
		t.Errorf("consume learning free = %v, want false", last["free"])
	}
}

func TestConsumeFreeNeverChargesOrCutsOff(t *testing.T) {
	e := newKidEnv(t)
	_, _, free := e.seedCatalog()
	e.unlockAda()
	e.importToday(e.ada, 0, 0) // zero balance

	status, m := e.consume(string(free), 60)
	if status != http.StatusOK {
		t.Fatalf("consume free: status %d body %v", status, m)
	}
	if m["cutOff"] != false {
		t.Errorf("free consume reported cutOff = %v, want false at zero balance", m["cutOff"])
	}
	if m["free"] != true {
		t.Errorf("free consume free = %v, want true", m["free"])
	}
	if m["today"] != 0.0 {
		t.Errorf("free consume charged today = %v, want 0", m["today"])
	}
}

func TestConsumeGrownupFreeAtZeroAndNegativeBalance(t *testing.T) {
	e := newKidEnv(t)
	fun, _, _ := e.seedCatalog()

	resp, _ := e.do("POST", "/api/kid/watch-together", map[string]string{"pin": "9999"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("watch-together: status %d", resp.StatusCode)
	}
	// A deliberately negative account must not matter for the grown-up.
	e.importToday(e.ada, -1, 0)

	status, m := e.consume(string(fun), 60)
	if status != http.StatusOK {
		t.Fatalf("grown-up consume: status %d body %v", status, m)
	}
	if m["cutOff"] != false || m["grownup"] != true || m["free"] != true {
		t.Errorf("grown-up consume = %v, want no cutOff, grownup free", m)
	}

	// And again at zero, to double-cover the boundary.
	e.importToday(e.ada, 0, 0)
	status, m = e.consume(string(fun), 60)
	if status != http.StatusOK || m["cutOff"] != false {
		t.Errorf("grown-up consume at zero: status %d body %v, want no cutOff", status, m)
	}
}

func TestConsumeCutsOffAtZeroForFun(t *testing.T) {
	e := newKidEnv(t)
	fun, _, _ := e.seedCatalog()
	e.unlockAda()
	e.importToday(e.ada, 0.03, 0) // 1.8 s of fun left

	status, m := e.consume(string(fun), 5)
	if status != http.StatusOK {
		t.Fatalf("consume: status %d body %v", status, m)
	}
	if m["cutOff"] != true {
		t.Errorf("consume fun past the balance: cutOff = %v, want true", m["cutOff"])
	}
	if m["total"] != 0.0 {
		t.Errorf("consume fun past the balance: total = %v, want 0", m["total"])
	}

	// A second tick at an already-zero balance also cuts off.
	status, m = e.consume(string(fun), 5)
	if status != http.StatusOK || m["cutOff"] != true {
		t.Errorf("consume at zero: status %d cutOff %v, want 200/true", status, m["cutOff"])
	}
}

func TestConsumeClampsSeconds(t *testing.T) {
	e := newKidEnv(t)
	fun, _, _ := e.seedCatalog()
	e.unlockAda()
	e.importToday(e.ada, 5, 0)

	// 999 seconds must be clamped to the 5 s per-request maximum: a charge
	// of exactly 5/60 minutes.
	status, m := e.consume(string(fun), 999)
	if status != http.StatusOK {
		t.Fatalf("consume: status %d body %v", status, m)
	}
	// The wire balance is rounded to two decimals: 5 - 5/60 = 4.9166… → 4.92.
	want := 4.92
	today, _ := m["today"].(float64)
	if today < want-1e-9 || today > want+1e-9 {
		t.Errorf("today after clamped consume = %v, want %.2f", today, want)
	}
	if m["cutOff"] != false {
		t.Errorf("cutOff = %v, want false", m["cutOff"])
	}
}

func TestPositionSaveResumeClear(t *testing.T) {
	e := newKidEnv(t)
	fun, _, _ := e.seedCatalog()
	e.unlockAda()

	resp, m := e.do("POST", "/api/kid/position", map[string]any{"episode": string(fun), "pos": 42})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("position: status %d body %v", resp.StatusCode, m)
	}
	resp, m = e.do("GET", "/api/kid/resume/"+string(fun), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resume: status %d", resp.StatusCode)
	}
	if m["pos"] != 42.0 {
		t.Errorf("resume pos = %v, want 42", m["pos"])
	}

	// Zero clears the saved spot, as the "finished watching" signal does.
	resp, _ = e.do("POST", "/api/kid/position", map[string]any{"episode": string(fun), "pos": 0})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clear position: status %d", resp.StatusCode)
	}
	if resp, m = e.do("GET", "/api/kid/resume/"+string(fun), nil); m["pos"] != 0.0 {
		t.Errorf("resume after clear = %v, want 0", m["pos"])
	}

	// A grown-up session saves nothing but is not an error.
	resp, _ = e.do("POST", "/api/kid/watch-together", map[string]string{"pin": "9999"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("watch-together: status %d", resp.StatusCode)
	}
	if resp, m = e.do("POST", "/api/kid/position", map[string]any{"episode": string(fun), "pos": 77}); resp.StatusCode != http.StatusOK {
		t.Errorf("grown-up position: status %d, want 200 no-op", resp.StatusCode)
	}
}

func TestKeepWatchingListsSavedPositions(t *testing.T) {
	e := newKidEnv(t)
	fun, _, _ := e.seedCatalog()
	e.unlockAda()

	e.do("POST", "/api/kid/position", map[string]any{"episode": string(fun), "pos": 42})
	resp, m := e.do("GET", "/api/keep-watching", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("keep-watching: status %d body %v", resp.StatusCode, m)
	}
	items, _ := m["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("keep-watching items = %v, want 1", m["items"])
	}
	it := items[0].(map[string]any)
	if it["episode"] != string(fun) || it["pos"] != 42.0 || it["title"] != "Fun One" {
		t.Errorf("keep-watching item = %v, want ep-fun-1 at 42s", it)
	}
}

func TestMediaRangeAndThumb(t *testing.T) {
	e := newKidEnv(t)
	fun, _, _ := e.seedCatalog()

	// Plain GET answers 200 with the whole file.
	resp, err := http.Get(e.ts.URL + "/media/" + string(fun) + ".mp4")
	if err != nil {
		t.Fatalf("GET media: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "MP4TESTDATA\n" {
		t.Errorf("media GET = %d %q, want 200 MP4TESTDATA", resp.StatusCode, body)
	}

	// A Range request gets a 206 with exactly the asked bytes.
	req, _ := http.NewRequest("GET", e.ts.URL+"/media/"+string(fun)+".mp4", nil)
	req.Header.Set("Range", "bytes=0-3")
	resp, err = e.client.Do(req)
	if err != nil {
		t.Fatalf("Range GET media: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Errorf("Range GET status = %d, want 206", resp.StatusCode)
	}
	if string(body) != "MP4T" {
		t.Errorf("Range GET body = %q, want MP4T", body)
	}
	if cr := resp.Header.Get("Content-Range"); !strings.HasPrefix(cr, "bytes 0-3/") {
		t.Errorf("Content-Range = %q, want bytes 0-3/12", cr)
	}

	// Thumbs serve from the stored path too.
	resp, err = http.Get(e.ts.URL + "/thumbs/" + string(fun) + ".jpg")
	if err != nil {
		t.Fatalf("GET thumb: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "THUMBJPEG" {
		t.Errorf("thumb GET = %d %q, want 200 THUMBJPEG", resp.StatusCode, body)
	}
}

func TestMediaUnknownAndBadSuffixNotFound(t *testing.T) {
	e := newKidEnv(t)
	e.seedCatalog()

	for _, path := range []string{
		"/media/no-such-episode.mp4",
		"/thumbs/no-such-episode.jpg",
		"/media/no-such-episode", // missing suffix
		"/thumbs/no-such-episode",
	} {
		resp, err := http.Get(e.ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestKidAPIsRequireSession(t *testing.T) {
	e := newKidEnv(t)
	fun, _, _ := e.seedCatalog()

	// A fresh client without any cookie must get 401 from every kid API.
	anon := newClient()
	e.client = anon
	checks := []struct{ method, path string }{
		{"GET", "/api/kid/me"},
		{"GET", "/api/balance"},
		{"GET", "/api/keep-watching"},
		{"GET", "/api/recent"},
		{"GET", "/api/kid/resume/" + string(fun)},
		{"POST", "/api/kid/consume"},
		{"POST", "/api/kid/position"},
		{"POST", "/api/kid/started"},
	}
	for _, c := range checks {
		var body any
		if c.method == "POST" {
			body = map[string]any{"episode": string(fun), "sec": 1}
		}
		resp, m := e.do(c.method, c.path, body)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s status = %d, want 401 (body %v)", c.method, c.path, resp.StatusCode, m)
		}
	}

	// Lock without a session is not an error: it just clears any cookie.
	if resp, _ := e.do("POST", "/api/kid/lock", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("lock without session: status %d, want 200", resp.StatusCode)
	}
}

func TestWatchRecordsWatchStart(t *testing.T) {
	e := newKidEnv(t)
	fun, learn, free := e.seedCatalog()

	// No session: the player redirects back to the picker (no redirect
	// following, so the 303 itself is what we see).
	raw := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := raw.Get(e.ts.URL + "/watch/" + string(fun))
	if err != nil {
		t.Fatalf("GET watch: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("watch without session status = %d, want 303", resp.StatusCode)
	}

	// Serving the player page writes nothing: only a real playback start
	// (POST /api/kid/started, which the player sends on its first 'playing'
	// event) records the watch.
	e.unlockAda()
	resp, m := e.do("GET", "/watch/"+string(fun), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("watch status = %d body %v", resp.StatusCode, m)
	}
	if log, err := e.lib.WatchLog(e.ctx); err != nil || len(log) != 0 {
		t.Fatalf("watch log after page load = %d entries (err %v), want 0 until playback starts", len(log), err)
	}

	// The kid's playback actually starts: cost at the full bin rate, and the
	// episode lands in the recent list.
	resp, m = e.do("POST", "/api/kid/started", map[string]any{"episode": string(fun)})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("started: status %d body %v", resp.StatusCode, m)
	}
	log1, err := e.lib.WatchLog(e.ctx)
	if err != nil {
		t.Fatalf("WatchLog: %v", err)
	}
	if len(log1) != 1 {
		t.Fatalf("watch log = %d entries, want 1", len(log1))
	}
	if log1[0].Episode != "ep-fun-1" || log1[0].Cost != 3.0 || log1[0].Free {
		t.Errorf("watch entry = %+v, want ep-fun-1 cost 3 free false (3 min at rate 1.0)", log1[0])
	}
	rec1, err := e.lib.Recent(e.ctx)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(rec1) != 1 || rec1[0].Episode != "ep-fun-1" {
		t.Errorf("recent = %+v, want ep-fun-1", rec1)
	}

	// A free-bin episode watched by a kid records free and cost 0.
	resp, _ = e.do("POST", "/api/kid/started", map[string]any{"episode": string(free)})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("started free: status %d", resp.StatusCode)
	}

	// A grown-up session records free and cost 0 (grown-up policy).
	resp, _ = e.do("POST", "/api/kid/watch-together", map[string]string{"pin": "9999"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("watch-together: status %d", resp.StatusCode)
	}
	resp, _ = e.do("POST", "/api/kid/started", map[string]any{"episode": string(learn)})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("started grown-up: status %d", resp.StatusCode)
	}

	log3, err := e.lib.WatchLog(e.ctx)
	if err != nil {
		t.Fatalf("WatchLog: %v", err)
	}
	if len(log3) != 3 {
		t.Fatalf("watch log = %d entries, want 3", len(log3))
	}
	for i, w := range log3 {
		if i == 2 {
			// The first watch was Ada on the fun show: full price.
			if w.Free || w.Cost != 3.0 || w.Episode != "ep-fun-1" {
				t.Errorf("fun watch entry = %+v, want cost 3 free false", w)
			}
			continue
		}
		if w.Free != true || w.Cost != 0 {
			t.Errorf("free/grown-up watch entry = %+v, want free cost 0", w)
		}
	}
	if log3[0].Kid != "" {
		t.Errorf("newest watch entry kid = %q, want empty (grown-up)", log3[0].Kid)
	}
	if log3[1].Episode != "ep-free" {
		t.Errorf("second watch entry = %+v, want the free episode", log3[1])
	}
}

// TestWatchBlockedNoLog is the parent-tester bug: a play blocked by
// "Time's up" must not leave a watch-log row (the second row that never
// played). The started handler checks the balance and refuses.
func TestWatchBlockedNoLog(t *testing.T) {
	e := newKidEnv(t)
	fun, _, _ := e.seedCatalog()
	e.importToday(e.ada, 0, 0)
	e.unlockAda()

	resp, m := e.do("POST", "/api/kid/started", map[string]any{"episode": string(fun)})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("started at zero balance: status %d body %v, want 403", resp.StatusCode, m)
	}
	if log, err := e.lib.WatchLog(e.ctx); err != nil || len(log) != 0 {
		t.Errorf("watch log after blocked play = %d entries (err %v), want 0", len(log), err)
	}
	if rec, err := e.lib.Recent(e.ctx); err != nil || len(rec) != 0 {
		t.Errorf("recent after blocked play = %d entries (err %v), want 0", len(rec), err)
	}

	// A negative balance (parent overshoot) behaves the same.
	e.importToday(e.ada, -1.3, 0)
	resp, _ = e.do("POST", "/api/kid/started", map[string]any{"episode": string(fun)})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("started at negative balance: status %d, want 403", resp.StatusCode)
	}
	if log, _ := e.lib.WatchLog(e.ctx); len(log) != 0 {
		t.Errorf("watch log after negative-balance block = %d entries, want 0", len(log))
	}
}

// TestWatchTogetherNoPINExplained: with no grown-up PIN set, the keypad is
// told in plain words and watch-together never unlocks.
func TestWatchTogetherNoPINExplained(t *testing.T) {
	e := newKidEnvNoPin(t)

	resp, m := e.do("GET", "/api/kid/grownup-pin", nil)
	if resp.StatusCode != http.StatusOK || m["set"] != false {
		t.Fatalf("grownup-pin = status %d body %v, want set false", resp.StatusCode, m)
	}
	resp, m = e.do("POST", "/api/kid/watch-together", map[string]string{"pin": "1234"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("watch-together without a PIN: status %d, want 409", resp.StatusCode)
	}
	errMsg, _ := m["error"].(string)
	if !strings.Contains(errMsg, "No grown-up PIN yet") {
		t.Errorf("watch-together error = %q, want the no-PIN explanation", errMsg)
	}
	if resp, _ := e.do("GET", "/api/kid/me", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("me after no-PIN watch-together: status %d, want 401 (never unlocked)", resp.StatusCode)
	}

	// Once the parent sets a PIN, the same endpoints agree it exists.
	if err := e.bud.SetGrownupPIN(e.ctx, "9999"); err != nil {
		t.Fatalf("SetGrownupPIN: %v", err)
	}
	resp, m = e.do("GET", "/api/kid/grownup-pin", nil)
	if resp.StatusCode != http.StatusOK || m["set"] != true {
		t.Fatalf("grownup-pin after set = status %d body %v, want set true", resp.StatusCode, m)
	}
}

// TestRecentPerKid: "Recently watched" is each kid's own list; a grown-up
// session still sees the whole household shelf.
func TestRecentPerKid(t *testing.T) {
	e := newKidEnv(t)
	fun, learn, _ := e.seedCatalog()
	mia, err := e.bud.CreateKid(e.ctx, "Mia", "5678")
	if err != nil {
		t.Fatalf("CreateKid Mia: %v", err)
	}

	e.unlockAda()
	if resp, _ := e.do("POST", "/api/kid/started", map[string]any{"episode": string(fun)}); resp.StatusCode != http.StatusOK {
		t.Fatalf("ada started: status %d", resp.StatusCode)
	}
	if resp, _ := e.do("POST", "/api/kid/unlock", map[string]string{"kid": string(mia), "pin": "5678"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("mia unlock: status %d", resp.StatusCode)
	}
	if resp, _ := e.do("POST", "/api/kid/started", map[string]any{"episode": string(learn)}); resp.StatusCode != http.StatusOK {
		t.Fatalf("mia started: status %d", resp.StatusCode)
	}

	resp, m := e.do("GET", "/api/recent", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mia recent: status %d", resp.StatusCode)
	}
	items, _ := m["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["episode"] != string(learn) {
		t.Errorf("mia recent = %v, want only the learning episode she watched", m["items"])
	}

	e.unlockAda()
	resp, m = e.do("GET", "/api/recent", nil)
	items, _ = m["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["episode"] != string(fun) {
		t.Errorf("ada recent = %v, want only the fun episode she watched", m["items"])
	}

	if resp, _ := e.do("POST", "/api/kid/watch-together", map[string]string{"pin": "9999"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("watch-together: status %d", resp.StatusCode)
	}
	resp, m = e.do("GET", "/api/recent", nil)
	items, _ = m["items"].([]any)
	if len(items) != 2 {
		t.Errorf("grown-up recent = %v, want both episodes", m["items"])
	}
}

// TestShortEpisodeCostNoFloor: a 20 s fun episode must show the real charge
// (20/60 min), not a 0.5-minute floor that disagrees with the metering.
func TestShortEpisodeCostNoFloor(t *testing.T) {
	e := newKidEnv(t)
	e.publishShow("show-fun", "Fun Show", library.BinFun, "ep-short", "Short One", 20)

	resp, m := e.do("GET", "/api/shows/show-fun/episodes", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("episodes: status %d", resp.StatusCode)
	}
	eps, _ := m["episodes"].([]any)
	if len(eps) != 1 {
		t.Fatalf("episodes = %d entries, want 1", len(eps))
	}
	ep := eps[0].(map[string]any)
	if ep["cost"] != 0.33 || ep["free"] != false {
		t.Errorf("short episode = %v, want cost 0.33 free false (20 s at rate 1.0)", ep)
	}
	if ep["bin"] != "fun" {
		t.Errorf("short episode bin = %v, want fun", ep["bin"])
	}
}

func TestEpisodesPageAndCosts(t *testing.T) {
	e := newKidEnv(t)
	e.seedCatalog()

	resp, m := e.do("GET", "/api/shows/show-words/episodes?offset=0&limit=1", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("episodes: status %d body %v", resp.StatusCode, m)
	}
	if m["total"] != 1.0 {
		t.Errorf("total = %v, want 1", m["total"])
	}
	eps, _ := m["episodes"].([]any)
	if len(eps) != 1 {
		t.Fatalf("page = %d episodes, want 1", len(eps))
	}
	ep := eps[0].(map[string]any)
	// 180 s = 3 min at the learning rate 0.5 → "Costs: 1.5 min".
	if ep["cost"] != 1.5 || ep["free"] != false {
		t.Errorf("learning episode = %v, want cost 1.5 free false", ep)
	}
	if ep["thumb"] != "/thumbs/ep-learn.jpg" {
		t.Errorf("episode thumb = %v, want /thumbs/ep-learn.jpg", ep["thumb"])
	}

	// The free-bin show's episodes are free.
	resp, m = e.do("GET", "/api/shows/show-free/episodes", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("free episodes: status %d", resp.StatusCode)
	}
	eps, _ = m["episodes"].([]any)
	if len(eps) != 1 || eps[0].(map[string]any)["free"] != true {
		t.Errorf("free episodes = %v, want one free entry", m["episodes"])
	}
}

func TestSearchMatchesShowsAndEpisodes(t *testing.T) {
	e := newKidEnv(t)
	e.seedCatalog()

	resp, m := e.do("GET", "/api/search?q=word", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search: status %d body %v", resp.StatusCode, m)
	}
	shows, _ := m["shows"].([]any)
	eps, _ := m["episodes"].([]any)
	if len(shows) != 1 || shows[0].(map[string]any)["name"] != "Word Show" {
		t.Errorf("search shows = %v, want Word Show", m["shows"])
	}
	if len(eps) != 1 || eps[0].(map[string]any)["title"] != "Word One" {
		t.Errorf("search episodes = %v, want Word One", m["episodes"])
	}
}

func TestShowsListed(t *testing.T) {
	e := newKidEnv(t)
	e.seedCatalog()

	resp, m := e.do("GET", "/api/shows", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("shows: status %d body %v", resp.StatusCode, m)
	}
	shows, _ := m["shows"].([]any)
	if len(shows) != 3 {
		t.Fatalf("shows = %d, want 3", len(shows))
	}
	byName := map[string]map[string]any{}
	for _, s := range shows {
		sm := s.(map[string]any)
		byName[sm["name"].(string)] = sm
	}
	if byName["Fun Show"]["bin"] != "fun" || byName["Word Show"]["bin"] != "learning" || byName["Free Zone"]["bin"] != "free" {
		t.Errorf("show bins = %v, want fun/learning/free", byName)
	}
	if byName["Fun Show"]["episodes"] != 2.0 {
		t.Errorf("Fun Show episodes = %v, want 2", byName["Fun Show"]["episodes"])
	}
	// Every show's poster is the /thumbs/ URL of its first thumbed episode
	// in natural title order (Fun Show's episodes are Fun One, Fun Two).
	for name, want := range map[string]string{
		"Fun Show":  "/thumbs/ep-fun-1.jpg",
		"Word Show": "/thumbs/ep-learn.jpg",
		"Free Zone": "/thumbs/ep-free.jpg",
	} {
		if got := byName[name]["poster"]; got != want {
			t.Errorf("%s poster = %v, want %s", name, got, want)
		}
	}
	thumbResp, err := http.Get(e.ts.URL + "/thumbs/ep-fun-1.jpg")
	if err != nil {
		t.Fatalf("GET poster URL: %v", err)
	}
	thumbResp.Body.Close()
	if thumbResp.StatusCode != http.StatusOK {
		t.Errorf("poster URL status = %d, want 200", thumbResp.StatusCode)
	}
}

// TestKidAPIAndWatchDataCarryIssueFix checks the two places the Contract
// puts the fix status: the episode objects the catalog API returns and
// the embedded player data the watch page loads.
func TestKidAPIAndWatchDataCarryIssueFix(t *testing.T) {
	e := newKidEnv(t)
	e.publishWithStatus("s1", "Fix Show", "ep1", "No Sound", core.IssueSound, core.FixPending)
	e.publishWithStatus("s1", "Fix Show", "ep2", "Fine", core.IssueNone, core.FixNone)

	resp, m := e.do("GET", "/api/shows/s1/episodes", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("episodes: status %d body %v", resp.StatusCode, m)
	}
	eps, _ := m["episodes"].([]any)
	if len(eps) != 2 {
		t.Fatalf("episodes = %d, want 2", len(eps))
	}
	byID := map[string]map[string]any{}
	for _, ep := range eps {
		em := ep.(map[string]any)
		byID[em["id"].(string)] = em
	}
	if got := byID["ep1"]; got["issue"] != "sound" || got["fix"] != "pending" {
		t.Errorf("ep1 issue/fix = %v/%v, want sound/pending", got["issue"], got["fix"])
	}
	if got := byID["ep2"]; got["issue"] != "" || got["fix"] != "" {
		t.Errorf("ep2 issue/fix = %v/%v, want empty", got["issue"], got["fix"])
	}

	// The player page embeds window.KS = {...}; parse it back out. The
	// page is HTML, so it is fetched through the cookie-jar client
	// directly (do() would consume the body as JSON).
	e.unlockAda()
	req, _ := http.NewRequest("GET", e.ts.URL+"/watch/ep1", nil)
	pageResp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("GET watch: %v", err)
	}
	body, _ := io.ReadAll(pageResp.Body)
	pageResp.Body.Close()
	if pageResp.StatusCode != http.StatusOK {
		t.Fatalf("watch page: status %d", pageResp.StatusCode)
	}
	idx := strings.Index(string(body), "window.KS = ")
	if idx < 0 {
		t.Fatalf("watch page has no embedded KS data: %s", body)
	}
	start := idx + len("window.KS = ")
	end := strings.Index(string(body)[start:], ";")
	if end < 0 {
		t.Fatalf("watch page KS data not terminated")
	}
	var wd watchData
	if err := json.Unmarshal([]byte(string(body)[start:start+end]), &wd); err != nil {
		t.Fatalf("decode watch data: %v", err)
	}
	if wd.Issue != core.IssueSound || wd.Fix != core.FixPending {
		t.Errorf("watch data issue/fix = %q/%q, want sound/pending", wd.Issue, wd.Fix)
	}
}

// TestPendingRefusesPlayback pins the "getting ready" contract: a pending
// episode answers 409 with the plain message from /media and from
// /api/kid/started (no minutes charged, no watch-log row). Failed and
// clean episodes play exactly as before.
func TestPendingRefusesPlayback(t *testing.T) {
	e := newKidEnv(t)
	e.publishWithStatus("s1", "Fix Show", "ep-pending", "Waiting", core.IssueSound, core.FixPending)
	e.publishWithStatus("s1", "Fix Show", "ep-failed", "Broken Fix", core.IssuePicture, core.FixFailed)
	e.publishWithStatus("s1", "Fix Show", "ep-clean", "Fine", core.IssueNone, core.FixNone)
	e.importToday(e.ada, 10, 0)
	e.unlockAda()

	resp, m := e.do("GET", "/media/ep-pending.mp4", nil)
	if resp.StatusCode != http.StatusConflict || m["error"] != msgGettingReady {
		t.Errorf("pending media = %d %v, want 409 %q", resp.StatusCode, m, msgGettingReady)
	}
	resp, m = e.do("POST", "/api/kid/started", map[string]any{"episode": "ep-pending"})
	if resp.StatusCode != http.StatusConflict || m["error"] != msgGettingReady {
		t.Errorf("pending started = %d %v, want 409 %q", resp.StatusCode, m, msgGettingReady)
	}
	if log, err := e.lib.WatchLog(e.ctx); err != nil || len(log) != 0 {
		t.Errorf("watch log after refused pending start = %d entries (err %v), want 0", len(log), err)
	}
	if rec, err := e.lib.Recent(e.ctx); err != nil || len(rec) != 0 {
		t.Errorf("recent after refused pending start = %d entries (err %v), want 0", len(rec), err)
	}

	// Failed and clean episodes keep playing: /media serves them, and
	// /api/kid/started records the watch.
	for _, id := range []string{"ep-failed", "ep-clean"} {
		resp, _ := e.do("GET", "/media/"+id+".mp4", nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s media status = %d, want 200", id, resp.StatusCode)
		}
	}
	if resp, m := e.do("POST", "/api/kid/started", map[string]any{"episode": "ep-failed"}); resp.StatusCode != http.StatusOK {
		t.Errorf("failed-fix started = %d %v, want 200", resp.StatusCode, m)
	}
	if log, err := e.lib.WatchLog(e.ctx); err != nil || len(log) != 1 {
		t.Errorf("watch log after a real start = %d entries (err %v), want 1", len(log), err)
	}
}

// TestWatchNextSkipsPending: the watch page's Next/NextTitle must resolve
// past pending episodes, so the "Next episode" button never leads into a
// "getting ready" panel.
func TestWatchNextSkipsPending(t *testing.T) {
	e := newKidEnv(t)
	e.publishWithStatus("s1", "Fix Show", "ep-1", "1", core.IssueNone, core.FixNone)
	e.publishWithStatus("s1", "Fix Show", "ep-2", "2", core.IssueSound, core.FixPending)
	e.publishWithStatus("s1", "Fix Show", "ep-3", "3", core.IssueNone, core.FixNone)
	e.unlockAda()

	nextOf := func(id string) (string, string) {
		t.Helper()
		req, _ := http.NewRequest("GET", e.ts.URL+"/watch/"+id, nil)
		resp, err := e.client.Do(req)
		if err != nil {
			t.Fatalf("GET watch %s: %v", id, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("watch %s: status %d", id, resp.StatusCode)
		}
		idx := strings.Index(string(body), "window.KS = ")
		if idx < 0 {
			t.Fatalf("watch %s has no embedded KS data", id)
		}
		start := idx + len("window.KS = ")
		end := strings.Index(string(body)[start:], ";")
		var wd watchData
		if err := json.Unmarshal([]byte(string(body)[start:start+end]), &wd); err != nil {
			t.Fatalf("decode watch data: %v", err)
		}
		return wd.Next, wd.NextTitle
	}
	// ep-1's next is ep-3: the pending ep-2 is skipped.
	if next, title := nextOf("ep-1"); next != "ep-3" || title != "3" {
		t.Errorf("ep-1 next = %q/%q, want ep-3/3 (pending ep-2 skipped)", next, title)
	}
	// The last episode has no next.
	if next, title := nextOf("ep-3"); next != "" || title != "" {
		t.Errorf("ep-3 next = %q/%q, want empty", next, title)
	}
}

// TestConsumeRefusesPending: a metering tick for an episode whose better
// copy is still being made ("Getting ready", fix pending) is refused before
// any charge, like /media and /api/kid/started, whatever the client sends.
func TestConsumeRefusesPending(t *testing.T) {
	e := newKidEnv(t)
	e.publishShow("show-fun", "Fun Show", library.BinFun, "ep-ready", "Ready", 300)
	if err := e.lib.Publish(e.ctx, core.Episode{
		ID: "ep-pending", ShowID: "show-fun", Title: "Pending", File: "/media/ep-pending.mkv", DurationSec: 300,
		Source: core.Source{Kind: core.SourceFolder, Path: "Fun Show/ep-pending.mkv", Title: "Pending", Show: "Fun Show"},
		Issue:  core.IssuePicture, Fix: core.FixPending,
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	e.importToday(e.ada, 10, 0)
	e.unlockAda()
	status, m := e.consume("ep-pending", 5)
	if status != http.StatusConflict {
		t.Fatalf("consume pending: status %d body %v, want 409", status, m)
	}
	if msg, _ := m["error"].(string); msg != msgGettingReady {
		t.Errorf("consume pending error = %q, want %q", msg, msgGettingReady)
	}
	resp, bal := e.do("GET", "/api/balance", nil)
	if resp.StatusCode != http.StatusOK || bal["total"] != 10.0 {
		t.Errorf("balance after a refused tick = %v, want 10 minutes untouched", bal)
	}
	// A playable episode of the same show still charges (5 s at fun rate).
	if status, m := e.consume("ep-ready", 5); status != http.StatusOK || m["total"] != 9.92 {
		t.Errorf("consume ready = %d %v, want 9.92 left", status, m)
	}
}

// TestPositionNearEndClears: a position reported within 5 seconds of the
// episode's end clears the resume spot, so a finished episode leaves Keep
// watching even when a client sends the end position instead of 0; one
// just outside the last 5 seconds is still saved.
func TestPositionNearEndClears(t *testing.T) {
	e := newKidEnv(t)
	fun, _, _ := e.seedCatalog() // 180 s
	e.unlockAda()
	resume := func() float64 {
		t.Helper()
		resp, m := e.do("GET", "/api/kid/resume/"+string(fun), nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("resume: status %d body %v", resp.StatusCode, m)
		}
		pos, _ := m["pos"].(float64)
		return pos
	}
	e.do("POST", "/api/kid/position", map[string]any{"episode": string(fun), "pos": 174})
	if got := resume(); got != 174 {
		t.Fatalf("resume after 174 s = %v, want 174 (6 s before the end is kept)", got)
	}
	e.do("POST", "/api/kid/position", map[string]any{"episode": string(fun), "pos": 176})
	if got := resume(); got != 0 {
		t.Errorf("resume after 176 s of 180 = %v, want 0 (finished)", got)
	}
	_, m := e.do("GET", "/api/keep-watching", nil)
	if items, _ := m["items"].([]any); len(items) != 0 {
		t.Errorf("keep watching after the end = %v, want empty", items)
	}
}

// TestKeepWatchingAndRecentCarryIssueFix: both recommendation rows send
// each episode's issue and fix, so the kid page labels them and sends a
// pending one to its "Getting ready" panel instead of the player.
func TestKeepWatchingAndRecentCarryIssueFix(t *testing.T) {
	e := newKidEnv(t)
	e.publishWithStatus("s1", "Fix Show", "ep-failed", "Quiet", core.IssueSound, core.FixFailed)
	e.unlockAda()
	e.do("POST", "/api/kid/position", map[string]any{"episode": "ep-failed", "pos": 42})
	if resp, m := e.do("POST", "/api/kid/started", map[string]any{"episode": "ep-failed"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("started: status %d body %v", resp.StatusCode, m)
	}
	// An episode watched earlier that backfill or a retry marked pending.
	e.publishWithStatus("s1", "Fix Show", "ep-later", "Later", core.IssuePicture, core.FixPending)
	if err := e.lib.ImportResume(e.ctx, e.ada, "ep-later", 30, fixedNow()); err != nil {
		t.Fatalf("ImportResume: %v", err)
	}
	if err := e.lib.ImportHouseholdRecent(e.ctx, "ep-later", fixedNow().Add(-time.Hour), e.ada); err != nil {
		t.Fatalf("ImportHouseholdRecent: %v", err)
	}
	want := map[string][2]string{
		"ep-failed": {core.IssueSound, core.FixFailed},
		"ep-later":  {core.IssuePicture, core.FixPending},
	}
	check := func(path string) {
		t.Helper()
		resp, m := e.do("GET", path, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d body %v", path, resp.StatusCode, m)
		}
		items, _ := m["items"].([]any)
		if len(items) != len(want) {
			t.Fatalf("%s items = %v, want %d", path, items, len(want))
		}
		for _, raw := range items {
			it := raw.(map[string]any)
			w := want[it["episode"].(string)]
			if it["issue"] != w[0] || it["fix"] != w[1] {
				t.Errorf("%s %s: issue %v fix %v, want %s/%s", path, it["episode"], it["issue"], it["fix"], w[0], w[1])
			}
		}
	}
	check("/api/keep-watching")
	check("/api/recent")
	if resp, _ := e.do("POST", "/api/kid/watch-together", map[string]string{"pin": "9999"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("watch-together: status %d", resp.StatusCode)
	}
	check("/api/recent") // the household list
}

// TestWatchTogetherSessionIsShort: a grown-up watch-together session (free,
// unmetered) ends after 2 hours, so a kid cannot keep watching for free on
// a device a grown-up walked away from; a kid's own unlock keeps the long
// kid session lifetime.
func TestWatchTogetherSessionIsShort(t *testing.T) {
	e, advance := newKidEnvClock(t)
	resp, _ := e.do("POST", "/api/kid/watch-together", map[string]string{"pin": "9999"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("watch-together: status %d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie && c.MaxAge != int((2*time.Hour).Seconds()) {
			t.Errorf("grown-up cookie MaxAge = %d, want %d", c.MaxAge, int((2 * time.Hour).Seconds()))
		}
	}
	advance(2*time.Hour - time.Minute)
	if resp, _ := e.do("GET", "/api/kid/me", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("grown-up session before 2 h: status %d, want 200", resp.StatusCode)
	}
	advance(2 * time.Minute)
	if resp, _ := e.do("GET", "/api/kid/me", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("grown-up session after 2 h: status %d, want 401", resp.StatusCode)
	}

	e.unlockAda()
	advance(3 * time.Hour)
	if resp, m := e.do("GET", "/api/kid/me", nil); resp.StatusCode != http.StatusOK || m["grownup"] != false {
		t.Errorf("kid session after 3 h: status %d body %v, want still unlocked", resp.StatusCode, m)
	}
}

// TestEntryMessagesSpeakToTheirReader: a wrong grown-up PIN tells the
// grown-up to try again (not to ask a grown-up), and an unlock before the
// minutes service is up says plainly that the shelf is still starting.
func TestEntryMessagesSpeakToTheirReader(t *testing.T) {
	e := newKidEnv(t)
	resp, m := e.do("POST", "/api/kid/watch-together", map[string]string{"pin": "0000"})
	msg, _ := m["error"].(string)
	if resp.StatusCode != http.StatusUnauthorized || strings.Contains(strings.ToLower(msg), "ask a grown-up") || !strings.Contains(msg, "Try again") {
		t.Errorf("wrong grown-up PIN = %d %q, want 401 asking the grown-up to try again", resp.StatusCode, msg)
	}

	starting := httptest.NewServer(NewApp("kid-test", Deps{Library: e.lib}).Mux())
	t.Cleanup(starting.Close)
	req, err := http.NewRequest(http.MethodPost, starting.URL+"/api/kid/unlock", strings.NewReader(`{"kid":"x","pin":"1234"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", starting.URL)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	msg, _ = body["error"].(string)
	if r.StatusCode != http.StatusServiceUnavailable || !strings.Contains(msg, "still starting") || strings.Contains(msg, "budget") {
		t.Errorf("unlock while starting = %d %q, want 503 saying the shelf is still starting", r.StatusCode, msg)
	}
}
