package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/edementhon/kids-shelf/internal/budget"
	"github.com/edementhon/kids-shelf/internal/core"
	"github.com/edementhon/kids-shelf/internal/practice"
	"github.com/edementhon/kids-shelf/internal/store"
)

// practiceApp wires the real budget and practice services over one temp
// store, plus one registered kid, into an app server.
type practiceApp struct {
	app    *Server
	kid    core.KidID
	bud    *budget.Budget
	pr     *practice.Service
	clips  string
	cookie *http.Cookie // Alex's unlock session, sent by e.doJSON and e.doGet
}

func newPracticeApp(t *testing.T) *practiceApp {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "practice-web.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(budget.Migrations...); err != nil {
		t.Fatalf("budget migrate: %v", err)
	}
	if err := st.Migrate(practice.Migrations()...); err != nil {
		t.Fatalf("practice migrate: %v", err)
	}
	bud, err := budget.New(st)
	if err != nil {
		t.Fatalf("budget.New: %v", err)
	}
	clips := t.TempDir()
	// The practice service dedupes sessions by second-precision timestamp,
	// so the fast loops below advance the clock between rounds instead of
	// finishing several rounds inside one second. Each call moves it a
	// minute, and one test makes over 60 calls, so it starts at 08:00 today:
	// started from the real time after about 23:00, it crossed midnight
	// mid-test and the daily caps reset (2026-10-29).
	y, m, d := time.Now().Date()
	now := time.Date(y, m, d, 8, 0, 0, 0, time.Local)
	clock := func() time.Time {
		now = now.Add(time.Minute)
		return now
	}
	pr := practice.New(st.DB(), bud, practice.Options{ClipsDir: clips, Clock: clock})
	kid, err := bud.CreateKid(context.Background(), "Alex", "0000")
	if err != nil {
		t.Fatalf("CreateKid: %v", err)
	}
	app := NewApp("test", Deps{Budget: bud, Practice: pr})
	e := &practiceApp{app: app, kid: kid, bud: bud, pr: pr, clips: clips}
	e.cookie = unlockKid(t, app, kid, "0000")
	return e
}

// unlockKid signs a kid in with their PIN, the way the shelf and the
// practice home do, and returns the session cookie.
func unlockKid(t *testing.T, app *Server, kid core.KidID, pin string) *http.Cookie {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"kid": string(kid), "pin": pin})
	req := httptest.NewRequest(http.MethodPost, "/api/kid/unlock", bytes.NewReader(b))
	req.Host = "localhost"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost")
	rec := httptest.NewRecorder()
	app.Mux().ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return c
		}
	}
	t.Fatalf("unlock %s: status %d, no session cookie (%s)", kid, rec.Code, rec.Body.String())
	return nil
}

// doJSON is the package doJSON signed in as Alex.
func (e *practiceApp) doJSON(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	return doJSON(t, e.app, method, path, body, e.cookie)
}

// doGet is the package doGet signed in as Alex.
func (e *practiceApp) doGet(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	return doGet(t, e.app, path, e.cookie)
}

// doJSON performs a request and returns the status code and decoded body.
// Requests carry a localhost Host (the middleware's allowlist refuses the
// default example.com Host) and, for state changing calls, a same-origin
// Origin exactly like the app's pages send.
func doJSON(t *testing.T, app *Server, method, path string, body any, cookies ...*http.Cookie) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Host = "localhost"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("Origin", "http://localhost")
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	app.Mux().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// doGet performs a plain GET and returns the recorder.
func doGet(t *testing.T, app *Server, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = "localhost"
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	app.Mux().ServeHTTP(rec, req)
	return rec
}

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

func TestMathRoundCreditsBudgetUpToCap(t *testing.T) {
	e := newPracticeApp(t)
	kid := string(e.kid)
	balance := func() float64 {
		t.Helper()
		today, _, err := e.bud.Balance(context.Background(), e.kid, "video")
		if err != nil {
			t.Fatalf("balance: %v", err)
		}
		return today
	}
	// A round of 10 fresh, fast, correct answers: each earns a jar coin and
	// the tenth banks one minute into the budget. Returns the coin kinds.
	playRound := func(firstFact int) []string {
		t.Helper()
		var results []map[string]any
		var kinds []string
		qs := make([]practice.Question, 10)
		for i := range qs {
			qs[i] = practice.Question{A: firstFact + i, B: 1, Op: "+"}
		}
		round := e.pr.OpenMathRound(kid, "add", qs)
		for i := range 10 {
			a, b := firstFact+i, 1
			code, d := e.doJSON(t, http.MethodPost, "/api/practice/answer", map[string]any{
				"kid": kid, "mode": "add", "round": round, "n": i,
				"a": a, "b": b, "op": "+", "given": a + b, "ms": 3000,
			})
			if code != http.StatusOK || d["correct"] != true {
				t.Fatalf("answer %d: code=%d correct=%v", i, code, d["correct"])
			}
			if c, ok := d["coin"].(map[string]any); ok {
				kinds = append(kinds, c["kind"].(string))
			}
			results = append(results, map[string]any{"a": a, "b": b, "op": "+", "given": a + b, "ms": 3000})
		}
		code, d := e.doJSON(t, http.MethodPost, "/api/practice/round_result", map[string]any{
			"kid": kid, "mode": "add", "checkup": false, "results": results,
		})
		if code != http.StatusOK {
			t.Fatalf("round_result code = %d", code)
		}
		if num(d["n"]) != 10 || num(d["correct"]) != 10 {
			t.Fatalf("round summary n/correct = %v/%v", d["n"], d["correct"])
		}
		return kinds
	}

	start := balance()
	kinds := playRound(1)
	for i, k := range kinds[:9] {
		if k != "coin" {
			t.Fatalf("answer %d coin kind = %q, want coin", i, k)
		}
	}
	if kinds[9] != "jar" {
		t.Fatalf("10th answer coin kind = %q, want jar", kinds[9])
	}
	if got := balance() - start; got != 1 {
		t.Fatalf("balance after round 1 grew %v, want 1", got)
	}
	playRound(11)
	if got := balance() - start; got != 2 {
		t.Fatalf("balance after round 2 grew %v, want 2", got)
	}
	playRound(21)
	if got := balance() - start; got != 3 {
		t.Fatalf("balance after round 3 grew %v, want cap 3", got)
	}
	playRound(31)
	if got := balance() - start; got != 3 {
		t.Fatalf("balance after the cap grew %v, want it stopped at 3", got)
	}
	// past the cap a fast record still records, but pays nothing
	round := e.pr.OpenMathRound(kid, "add", []practice.Question{{A: 50, B: 1, Op: "+"}})
	code, d := e.doJSON(t, http.MethodPost, "/api/practice/answer", map[string]any{
		"kid": kid, "mode": "add", "round": round, "n": 0,
		"a": 50, "b": 1, "op": "+", "given": 51, "ms": 3000,
	})
	if code != http.StatusOK || d["record"] != true {
		t.Fatalf("capped answer: code=%d record=%v", code, d["record"])
	}
	coin, _ := d["coin"].(map[string]any)
	if coin == nil || coin["kind"] != "capped" {
		t.Fatalf("capped answer coin = %v, want kind capped", d["coin"])
	}
	aw, err := e.pr.Awards(kid)
	if err != nil {
		t.Fatalf("awards: %v", err)
	}
	if aw.Earned != 3 {
		t.Errorf("earned = %v, want 3", aw.Earned)
	}
	// stats recorded through the API: the first fact of round one
	facts, err := e.pr.FactsFor(kid, "add")
	if err != nil {
		t.Fatalf("facts: %v", err)
	}
	f, ok := facts["1+1"]
	if !ok {
		t.Fatalf("fact 1+1 missing among %d facts", len(facts))
	}
	if f.Asked != 1 || f.Correct != 1 || f.BestMS == nil {
		t.Errorf("fact 1+1 = %+v, want asked 1 correct 1 with a best time", f)
	}
	sess, err := e.pr.Sessions(kid)
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}
	if len(sess) != 4 {
		t.Errorf("sessions = %d, want 4", len(sess))
	}
	best, err := e.pr.BestRound(kid, "add")
	if err != nil || best == nil || *best != 3000 {
		t.Fatalf("best round = %v, %v; want 3000ms", best, err)
	}
	// check-up rounds use the larger probe size
	code, d = e.doJSON(t, http.MethodGet, "/api/practice/round?kid="+kid+"&mode=add&checkup=1", nil)
	if code != http.StatusOK {
		t.Fatalf("checkup round code = %d", code)
	}
	if qs, _ := d["questions"].([]any); len(qs) != 12 {
		t.Fatalf("checkup questions = %d, want 12", len(qs))
	}
}

func TestSpellRoundWithAndWithoutClip(t *testing.T) {
	e := newPracticeApp(t)
	kid := string(e.kid)
	// a clip exists only for "cat"
	clip := []byte("RIFF\x1e\x00\x00\x00fake-wav")
	if err := os.WriteFile(filepath.Join(e.clips, "cat.wav"), clip, 0o644); err != nil {
		t.Fatalf("write clip: %v", err)
	}
	rec := e.doGet(t, "/practice/audio/cat.wav")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET clip = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/wav" {
		t.Errorf("clip Content-Type = %q, want audio/wav", ct)
	}
	if !bytes.Equal(rec.Body.Bytes(), clip) {
		t.Error("clip body differs from the file")
	}
	// a known word without a clip answers 404 so the page falls back to speech
	rec = e.doGet(t, "/practice/audio/dog.wav")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET missing clip = %d, want 404", rec.Code)
	}
	// a non-word and a non-clip URL answer 404 too
	rec = e.doGet(t, "/practice/audio/nonsense.wav")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET unknown word = %d, want 404", rec.Code)
	}
	rec = e.doGet(t, "/practice/audio/cat")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET non-wav = %d, want 404", rec.Code)
	}

	// a spelling round through the API
	code, d := e.doJSON(t, http.MethodGet, "/api/practice/spell_round?kid="+kid, nil)
	if code != http.StatusOK || d["ok"] != true {
		t.Fatalf("spell round: code=%d ok=%v", code, d["ok"])
	}
	words, _ := d["words"].([]any)
	if len(words) != 10 {
		t.Fatalf("round words = %d, want 10", len(words))
	}
	round, _ := d["round"].(string)
	used := make(map[string]bool, len(words))
	for i, wv := range words {
		w := wv.(string)
		used[w] = true
		code, d := e.doJSON(t, http.MethodPost, "/api/practice/spell_answer", map[string]any{
			"kid": kid, "round": round, "n": i, "word": w, "given": w, "ms": 2500,
		})
		if code != http.StatusOK || d["correct"] != true || d["record"] != true {
			t.Fatalf("spell answer %q: code=%d correct=%v record=%v", w, code, d["correct"], d["record"])
		}
	}
	// 10 fast records = 10 spelling coins = one banked minute
	sa, err := e.pr.SpellAwards(kid)
	if err != nil {
		t.Fatalf("spell awards: %v", err)
	}
	if sa.Earned != 1 || sa.Coins != 0 {
		t.Fatalf("after round: earned=%v coins=%d, want 1/0", sa.Earned, sa.Coins)
	}
	// 20 more fresh words fill the spelling cap (3 per day)
	var fresh []string
	for _, w := range practice.Words() {
		if used[w.Word] {
			continue
		}
		fresh = append(fresh, w.Word)
		if len(fresh) == 20 {
			break
		}
	}
	round = e.pr.OpenSpellRound(kid, fresh)
	for i, w := range fresh {
		code, d := e.doJSON(t, http.MethodPost, "/api/practice/spell_answer", map[string]any{
			"kid": kid, "round": round, "n": i, "word": w, "given": w, "ms": 2500,
		})
		if code != http.StatusOK || d["correct"] != true {
			t.Fatalf("spell answer %q: code=%d", w, code)
		}
	}
	sa, err = e.pr.SpellAwards(kid)
	if err != nil {
		t.Fatalf("spell awards: %v", err)
	}
	if sa.Earned != 3 {
		t.Fatalf("spell earned = %v, want cap 3", sa.Earned)
	}
	// past the cap a fresh word still records but pays nothing
	attempted := make(map[string]bool, len(used)+len(fresh))
	for w := range used {
		attempted[w] = true
	}
	for _, w := range fresh {
		attempted[w] = true
	}
	var freshWord string
	for _, w := range practice.Words() {
		if !attempted[w.Word] {
			freshWord = w.Word
			break
		}
	}
	if freshWord == "" {
		t.Fatal("spelling bank exhausted")
	}
	round = e.pr.OpenSpellRound(kid, []string{freshWord})
	code, d = e.doJSON(t, http.MethodPost, "/api/practice/spell_answer", map[string]any{
		"kid": kid, "round": round, "n": 0, "word": freshWord, "given": freshWord, "ms": 2500,
	})
	if code != http.StatusOK || d["record"] != true {
		t.Fatalf("capped spell answer: code=%d record=%v", code, d["record"])
	}
	coin, _ := d["coin"].(map[string]any)
	if coin == nil || coin["kind"] != "capped" {
		t.Fatalf("capped spell coin = %v, want kind capped", d["coin"])
	}
	if sa, err := e.pr.SpellAwards(kid); err != nil || sa.Earned != 3 {
		t.Fatalf("spell earned moved past cap: %v, %v", sa.Earned, err)
	}
	st, err := e.pr.SpellStatus(kid)
	if err != nil {
		t.Fatalf("spell status: %v", err)
	}
	if st.Recorded != 31 {
		t.Errorf("recorded words = %d, want 31", st.Recorded)
	}
	code, d = e.doJSON(t, http.MethodGet, "/api/practice/spell_status?kid="+kid, nil)
	if code != http.StatusOK || num(d["recorded"]) != 31 || num(d["cap"]) != 3 {
		t.Fatalf("spell status = %v", d)
	}
	if tiers, _ := d["tiers"].([]any); len(tiers) != 5 {
		t.Fatalf("spell tiers = %d, want 5", len(tiers))
	}
}

func TestBossWinAwardsOnce(t *testing.T) {
	e := newPracticeApp(t)
	kid := string(e.kid)
	code, d := e.doJSON(t, http.MethodGet, "/api/practice/boss_status?kid="+kid, nil)
	if code != http.StatusOK {
		t.Fatalf("boss status code = %d", code)
	}
	boss, _ := d["boss"].(map[string]any)
	if boss == nil || boss["id"] != "adder" {
		t.Fatalf("current boss = %v, want adder", boss)
	}
	if num(d["hp"]) != 8 || num(d["hearts"]) != 8 {
		t.Fatalf("fresh fight hp/hearts = %v/%v, want 8/8", d["hp"], d["hearts"])
	}
	code, d = e.doJSON(t, http.MethodGet, "/api/practice/boss_round?kid="+kid, nil)
	if code != http.StatusOK || d["ok"] != true {
		t.Fatalf("boss round: code=%d ok=%v", code, d["ok"])
	}
	questions, _ := d["questions"].([]any)
	if len(questions) != 8 {
		t.Fatalf("boss questions = %d, want 8", len(questions))
	}
	round, _ := d["round"].(string)
	for i, qv := range questions {
		q := qv.(map[string]any)
		a, b := int(num(q["a"])), int(num(q["b"]))
		op := q["op"].(string)
		code, d := e.doJSON(t, http.MethodPost, "/api/practice/boss_answer", map[string]any{
			"kid": kid, "round": round, "n": i, "a": a, "b": b, "op": op, "given": practice.AnswerOf(a, b, op),
		})
		if code != http.StatusOK || d["correct"] != true {
			t.Fatalf("boss answer %d: code=%d correct=%v", i, code, d["correct"])
		}
		msg, _ := d["msg"].(map[string]any)
		if msg == nil {
			t.Fatalf("no msg: %v", d)
		}
		if i == 7 {
			if msg["victory"] != true {
				t.Fatalf("expected victory, got %v", msg)
			}
			reward, _ := msg["reward"].(map[string]any)
			if reward == nil || reward["kind"] != "coin" || num(reward["coins"]) != 3 {
				t.Fatalf("reward = %v, want 3 coins in the jar", msg["reward"])
			}
		}
	}
	aw, err := e.pr.Awards(kid)
	if err != nil {
		t.Fatalf("awards: %v", err)
	}
	if aw.Coins != 3 {
		t.Fatalf("jar coins = %d, want the 3-boss reward", aw.Coins)
	}
	bs, err := e.pr.BossState(kid)
	if err != nil {
		t.Fatalf("boss state: %v", err)
	}
	if len(bs.Defeated) != 1 || bs.Defeated[0] != "adder" {
		t.Fatalf("defeated = %v, want [adder]", bs.Defeated)
	}
	code, d = e.doJSON(t, http.MethodGet, "/api/practice/boss_status?kid="+kid, nil)
	next, _ := d["boss"].(map[string]any)
	if next == nil || next["id"] != "minustaur" {
		t.Fatalf("next boss = %v, want minustaur", next)
	}
	if defeated, _ := d["defeated"].([]any); len(defeated) != 1 {
		t.Fatalf("defeated list = %d, want 1", len(defeated))
	}
	// a repeat of the winning answer (a double tap) replays the win and
	// pays nothing more; an answer from an old page is refused
	q7 := questions[7].(map[string]any)
	a7, b7, op7 := int(num(q7["a"])), int(num(q7["b"])), q7["op"].(string)
	code, d = e.doJSON(t, http.MethodPost, "/api/practice/boss_answer", map[string]any{
		"kid": kid, "round": round, "n": 7, "a": a7, "b": b7, "op": op7, "given": practice.AnswerOf(a7, b7, op7),
	})
	if msg, _ := d["msg"].(map[string]any); code != http.StatusOK || msg == nil || msg["victory"] != true {
		t.Fatalf("repeat of the winning answer: code=%d body=%v", code, d)
	}
	code, d = e.doJSON(t, http.MethodPost, "/api/practice/boss_answer", map[string]any{
		"kid": kid, "round": "", "n": 0, "a": a7, "b": b7, "op": op7, "given": practice.AnswerOf(a7, b7, op7),
	})
	if code != http.StatusConflict || d["err"] != "stale" {
		t.Fatalf("answer from an old page: code=%d body=%v, want 409 stale", code, d)
	}
	aw, err = e.pr.Awards(kid)
	if err != nil {
		t.Fatalf("awards: %v", err)
	}
	if aw.Coins != 3 {
		t.Fatalf("coins after the repeat and the stale answer = %d, want still 3", aw.Coins)
	}
}

func TestPracticeUnknownKid404(t *testing.T) {
	e := newPracticeApp(t)
	for _, p := range []string{
		"/practice/zzz/math", "/practice/zzz/checkup", "/practice/zzz/spell", "/practice/zzz/boss",
		"/api/practice/status?kid=zzz", "/api/practice/round?kid=zzz&mode=add",
		"/api/practice/spell_status?kid=zzz", "/api/practice/spell_round?kid=zzz",
		"/api/practice/boss_status?kid=zzz", "/api/practice/boss_round?kid=zzz",
	} {
		if rec := e.doGet(t, p); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
	}
	for _, tc := range []struct {
		path string
		body any
	}{
		{"/api/practice/answer", map[string]any{"kid": "zzz", "mode": "add", "a": 1, "b": 1, "op": "+", "given": 2, "ms": 100}},
		{"/api/practice/round_result", map[string]any{"kid": "zzz", "mode": "add", "checkup": false, "results": []any{}}},
		{"/api/practice/spell_answer", map[string]any{"kid": "zzz", "word": "cat", "given": "cat", "ms": 100}},
		{"/api/practice/boss_answer", map[string]any{"kid": "zzz", "a": 1, "b": 1, "op": "+", "given": 2}},
	} {
		if code, _ := e.doJSON(t, http.MethodPost, tc.path, tc.body); code != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404", tc.path, code)
		}
	}
}

func TestPracticePagesRender(t *testing.T) {
	e := newPracticeApp(t)
	id := string(e.kid)
	rec := e.doGet(t, "/practice")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /practice = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Alex", "to watch", "🔢 Math", "🔤 Spelling", "Not you?"} {
		if !strings.Contains(body, want) {
			t.Errorf("home page missing %q", want)
		}
	}
	if !strings.Contains(body, `<meta name="viewport"`) {
		t.Error("home page missing the viewport meta tag")
	}
	for _, p := range []string{"/math", "/checkup", "/spell", "/boss"} {
		rec := e.doGet(t, "/practice/"+id+p)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /practice/%s%s = %d, want 200", id, p, rec.Code)
		}
		b := rec.Body.String()
		if !strings.Contains(b, `<meta name="viewport"`) {
			t.Errorf("page %s missing the viewport meta tag", p)
		}
		// scripts carry a content hash, so a new build is never run
		// with yesterday's cached script
		if !strings.Contains(b, `/static/practice/common.js?v=`) {
			t.Errorf("page %s loads common.js without a version", p)
		}
	}
	// static assets are embedded and served
	for _, p := range []string{"/static/practice/practice.css", "/static/practice/common.js",
		"/static/practice/home.js", "/static/practice/math.js", "/static/practice/spell.js", "/static/practice/boss.js"} {
		if rec := e.doGet(t, p); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", p, rec.Code)
		}
	}
}

// TestFormatMinutesKidWords: kid screens show whole minutes, never below 0,
// never raw floats (the "-1.3500000000000671 video minutes" bug).
func TestFormatMinutesKidWords(t *testing.T) {
	if got := formatMinutes(9.66666666666665); got != "9" {
		t.Errorf("formatMinutes(9.666) = %q, want 9", got)
	}
	if got := formatMinutes(-1.3500000000000671); got != "0" {
		t.Errorf("formatMinutes(-1.35) = %q, want 0", got)
	}
	if got := formatMinutes(3); got != "3" {
		t.Errorf("formatMinutes(3) = %q, want 3", got)
	}
	if got := formatMinutes(0.4); got != "0" {
		t.Errorf("formatMinutes(0.4) = %q, want 0", got)
	}
}

// TestPracticeHomeKidWords: the practice home says what there is to watch
// and what is earned today in kid words, and never prints a float.
func TestPracticeHomeKidWords(t *testing.T) {
	e := newPracticeApp(t)
	body := e.doGet(t, "/practice").Body.String()
	if !regexp.MustCompile(`You have \d+ minutes? to watch\.`).MatchString(body) {
		t.Error("home page missing kid-words \"You have N minutes to watch.\"")
	}
	if !strings.Contains(body, "Earned today: 0 of 3 minutes") {
		t.Error("home page missing kid-words \"Earned today: 0 of 3 minutes\"")
	}
	if i := strings.Index(body, "minutes to watch"); i > 0 {
		seg := body[max(0, i-20):i]
		if strings.Contains(seg, ".") {
			t.Errorf("home page shows a decimal minutes value before %q", body[i-20:i])
		}
	}
	if got := minutesWord(1); got != "1 minute" {
		t.Errorf("minutesWord(1) = %q, want 1 minute", got)
	}
	for _, banned := range []string{"median", " toward the next minute", "\u2014", "\u2013"} {
		if strings.Contains(body, banned) {
			t.Errorf("home page still says %q", banned)
		}
	}
}

// TestPracticeRoundHasStopButton: a round must offer a visible way out, and
// the round pages carry it in their markup.
func TestPracticeRoundHasStopButton(t *testing.T) {
	e := newPracticeApp(t)
	id := string(e.kid)
	for _, p := range []string{"/practice/" + id + "/math", "/practice/" + id + "/spell", "/practice/" + id + "/boss"} {
		body := e.doGet(t, p).Body.String()
		if !strings.Contains(body, "Stop this round") && !strings.Contains(body, "Stop the fight") {
			t.Errorf("page %s missing a stop control", p)
		}
	}
}

func TestPracticeNotRunning503(t *testing.T) {
	app := NewApp("test", Deps{})
	for _, p := range []string{
		"/practice", "/practice/any/math", "/practice/audio/cat.wav",
		"/api/practice/status?kid=any", "/api/practice/round?kid=any&mode=add",
	} {
		if rec := doGet(t, app, p); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s = %d, want 503", p, rec.Code)
		}
	}
}

// TestPracticeAnswerNamesTheQuestionOnScreen: a double Enter used to send
// the old answer for the next question (never shown) and grade it. Now an
// answer names its round and position: a repeat replays the first outcome,
// an answer for a question not yet on screen is refused, and nothing is
// recorded for it.
func TestPracticeAnswerNamesTheQuestionOnScreen(t *testing.T) {
	e := newPracticeApp(t)
	kid := string(e.kid)
	code, d := e.doJSON(t, http.MethodGet, "/api/practice/round?kid="+kid+"&mode=add", nil)
	round, _ := d["round"].(string)
	qs, _ := d["questions"].([]any)
	if code != http.StatusOK || round == "" || len(qs) < 3 {
		t.Fatalf("round: code=%d round=%q questions=%d", code, round, len(qs))
	}
	q := func(i int) (int, int, string) {
		m := qs[i].(map[string]any)
		return int(num(m["a"])), int(num(m["b"])), m["op"].(string)
	}
	answer := func(n, a, b int, op string, given int, round string) (int, map[string]any) {
		return e.doJSON(t, http.MethodPost, "/api/practice/answer", map[string]any{
			"kid": kid, "mode": "add", "round": round, "n": n,
			"a": a, "b": b, "op": op, "given": given, "ms": 3000,
		})
	}
	a0, b0, op0 := q(0)
	given := practice.AnswerOf(a0, b0, op0)
	if code, d := answer(0, a0, b0, op0, given, round); code != http.StatusOK || d["correct"] != true {
		t.Fatalf("answer 0: code=%d body=%v", code, d)
	}
	if code, d := answer(0, a0, b0, op0, given, round); code != http.StatusOK || d["correct"] != true {
		t.Fatalf("repeat of answer 0: code=%d body=%v", code, d)
	}
	a2, b2, op2 := q(2)
	for _, tc := range []struct {
		name  string
		n     int
		round string
	}{
		{"a question two ahead", 2, round},
		{"no round (an old page)", 2, ""},
	} {
		code, d := answer(tc.n, a2, b2, op2, given, tc.round)
		if code != http.StatusConflict || d["err"] != "stale" {
			t.Errorf("%s: code=%d body=%v, want 409 stale", tc.name, code, d)
		}
	}
	facts, err := e.pr.FactsFor(kid, "add")
	if err != nil {
		t.Fatalf("facts: %v", err)
	}
	asked := 0
	for _, f := range facts {
		asked += f.Asked
	}
	if asked != 1 {
		t.Fatalf("facts asked %d times in total, want 1 (the repeat and the refused answers count nothing)", asked)
	}
}

// TestBossDoubleSendHealsOnce: a double tap on a miss used to heal the boss
// twice (and, on a lethal boss, cost two hearts).
func TestBossDoubleSendHealsOnce(t *testing.T) {
	e := newPracticeApp(t)
	kid := string(e.kid)
	_, d := e.doJSON(t, http.MethodGet, "/api/practice/boss_round?kid="+kid, nil)
	round, _ := d["round"].(string)
	qs, _ := d["questions"].([]any)
	if round == "" || len(qs) != 8 {
		t.Fatalf("boss round = %v", d)
	}
	send := func(n int, right bool) (int, map[string]any) {
		m := qs[n].(map[string]any)
		a, b, op := int(num(m["a"])), int(num(m["b"])), m["op"].(string)
		given := practice.AnswerOf(a, b, op)
		if !right {
			given = -1
		}
		return e.doJSON(t, http.MethodPost, "/api/practice/boss_answer", map[string]any{
			"kid": kid, "round": round, "n": n, "a": a, "b": b, "op": op, "given": given,
		})
	}
	send(0, true)
	send(1, true) // boss at 6
	for range 2 {
		code, d := send(2, false)
		msg, _ := d["msg"].(map[string]any)
		if code != http.StatusOK || msg == nil || num(msg["hp"]) != 7 {
			t.Fatalf("miss: code=%d body=%v, want hp 7", code, d)
		}
	}
	bs, err := e.pr.BossState(kid)
	if err != nil || bs.HP["adder"] != 7 {
		t.Fatalf("boss hp = %d (%v), want 7: one miss heals once", bs.HP["adder"], err)
	}
}

// TestPracticeRequiresSignedInKid: practice opens only for the kid signed in
// with their PIN; a sibling's card, pages and APIs stay shut.
func TestPracticeRequiresSignedInKid(t *testing.T) {
	e := newPracticeApp(t)
	bea, err := e.bud.CreateKid(context.Background(), "Bea", "1111")
	if err != nil {
		t.Fatalf("CreateKid: %v", err)
	}
	other := string(bea)
	// Alex's session sees only Alex.
	body := e.doGet(t, "/practice").Body.String()
	if !strings.Contains(body, "Alex") || strings.Contains(body, "Bea") {
		t.Errorf("Alex's practice home should show only Alex")
	}
	// Bea's things are shut for Alex.
	if code, d := e.doJSON(t, http.MethodGet, "/api/practice/status?kid="+other, nil); code != http.StatusUnauthorized || d["err"] != "signin" {
		t.Errorf("Bea's status as Alex: code=%d body=%v, want 401 signin", code, d)
	}
	if code, _ := e.doJSON(t, http.MethodPost, "/api/practice/answer", map[string]any{
		"kid": other, "mode": "add", "round": "", "n": 0, "a": 1, "b": 1, "op": "+", "given": 2, "ms": 100,
	}); code != http.StatusUnauthorized {
		t.Errorf("answer as Bea from Alex's session = %d, want 401", code)
	}
	for _, p := range []string{"/math", "/checkup", "/spell", "/boss"} {
		rec := e.doGet(t, "/practice/"+other+p)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/practice" {
			t.Errorf("Bea's %s page as Alex = %d to %q, want 303 to /practice", p, rec.Code, rec.Header().Get("Location"))
		}
	}
	// Nobody signed in: the kids to pick from and a PIN box, no cards.
	rec := doGet(t, e.app, "/practice")
	body = rec.Body.String()
	for _, want := range []string{"Who's practicing?", `data-kid="` + string(e.kid) + `"`, `data-kid="` + other + `"`, `id="pinbox"`} {
		if !strings.Contains(body, want) {
			t.Errorf("signed-out home missing %q", want)
		}
	}
	if strings.Contains(body, "/practice/"+other+"/math") {
		t.Error("signed-out home links straight into a kid's practice")
	}
	if code, _ := doJSON(t, e.app, http.MethodGet, "/api/practice/status?kid="+string(e.kid), nil); code != http.StatusUnauthorized {
		t.Errorf("status with no session = %d, want 401", code)
	}
	// Bea's own PIN opens Bea's practice.
	beaCookie := unlockKid(t, e.app, bea, "1111")
	if code, _ := doJSON(t, e.app, http.MethodGet, "/api/practice/status?kid="+other, nil, beaCookie); code != http.StatusOK {
		t.Errorf("Bea's status as Bea = %d, want 200", code)
	}
}

// TestKidAnimalSameOnShelfAndPractice: a kid is the same animal in the
// shelf's /api/kids list and on the practice pages (Ava was a fox on the
// shelf and a tiger in practice), for every kid in the household.
func TestKidAnimalSameOnShelfAndPractice(t *testing.T) {
	e := newPracticeApp(t)
	for _, name := range []string{"Bea", "Cy"} {
		if _, err := e.bud.AddKid(context.Background(), name, "1111"); err != nil {
			t.Fatalf("AddKid %s: %v", name, err)
		}
	}
	code, out := doJSON(t, e.app, http.MethodGet, "/api/kids", nil)
	if code != http.StatusOK {
		t.Fatalf("/api/kids = %d", code)
	}
	list, _ := out["kids"].([]any)
	if len(list) != 3 {
		t.Fatalf("/api/kids kids = %v, want 3", out["kids"])
	}
	animals := map[string]string{} // name to animal
	seen := map[string]bool{}
	for _, v := range list {
		k, _ := v.(map[string]any)
		name, _ := k["name"].(string)
		animal, _ := k["animal"].(string)
		if animal == "" || seen[animal] {
			t.Fatalf("/api/kids gives %s animal %q; want one of their own", name, animal)
		}
		seen[animal] = true
		animals[name] = animal
	}
	// The signed-out practice home lists every kid with their animal.
	home := doGet(t, e.app, "/practice").Body.String()
	for name, animal := range animals {
		if !strings.Contains(home, ">"+animal+" "+name+"</button>") {
			t.Errorf("practice home shows %s without the shelf's animal %s", name, animal)
		}
	}
	// Alex's own pages show Alex's shelf animal.
	want := animals["Alex"] + " Alex"
	for _, path := range []string{"/practice", "/practice/" + string(e.kid) + "/math", "/practice/" + string(e.kid) + "/spell"} {
		if body := e.doGet(t, path).Body.String(); !strings.Contains(body, "<h1>"+want) {
			t.Errorf("%s should show %q, the shelf's animal", path, want)
		}
	}
}

// TestPracticeHomeNoKids: with nobody set up, the home says what to do.
func TestPracticeHomeNoKids(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "empty.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(budget.Migrations...); err != nil {
		t.Fatalf("budget migrate: %v", err)
	}
	if err := st.Migrate(practice.Migrations()...); err != nil {
		t.Fatalf("practice migrate: %v", err)
	}
	bud, err := budget.New(st)
	if err != nil {
		t.Fatalf("budget.New: %v", err)
	}
	app := NewApp("test", Deps{Budget: bud, Practice: practice.New(st.DB(), bud, practice.Options{})})
	rec := doGet(t, app, "/practice")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "No kids are set up yet") {
		t.Fatalf("empty home: code=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestPracticeNoDashes: kid screens and their code use no em or en dashes.
func TestPracticeNoDashes(t *testing.T) {
	entries, err := fs.ReadDir(practiceStatic, "static/practice")
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range entries {
		b, _ := practiceStatic.ReadFile("static/practice/" + en.Name())
		if strings.ContainsAny(string(b), "\u2013\u2014") {
			t.Errorf("%s contains an em or en dash", en.Name())
		}
	}
}
