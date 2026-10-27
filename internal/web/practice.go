// Package web, practice area. Serves the ported mathfacts pages on
// practice.Service: the "who's practicing" home, math and check-up rounds,
// spelling rounds with pre-generated clips and the 13-boss arena. Pages are
// server-rendered shells over plain JS/CSS embedded from static/practice;
// every answer is graded by the service, and earned minutes go to the
// budget counter through practice's Crediter. JSON lives under
// /api/practice with the legacy ?kid= query shape.
package web

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"html"
	"io/fs"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/edementhon/kids-shelf/internal/practice"
)

//go:embed static/practice
var practiceStatic embed.FS

func init() {
	register(func(s *Server) {
		s.mux.HandleFunc("GET /practice", s.handlePracticeHome)
		// One multi wildcard dispatches the four kid pages: separate
		// /practice/{kid}/... patterns would overlap "/practice/audio/{word}"
		// (literal and wildcard swapped at different segments), which is a
		// registration-time conflict in the Go 1.22 mux.
		s.mux.HandleFunc("GET /practice/{page...}", s.handlePracticePage)
		s.mux.HandleFunc("GET /practice/audio/{word}", s.handlePracticeAudio)
		s.mux.HandleFunc("GET /api/practice/status", s.handlePracticeStatus)
		s.mux.HandleFunc("GET /api/practice/round", s.handlePracticeRound)
		s.mux.HandleFunc("POST /api/practice/answer", s.handlePracticeAnswer)
		s.mux.HandleFunc("POST /api/practice/round_result", s.handlePracticeRoundResult)
		s.mux.HandleFunc("GET /api/practice/spell_status", s.handleSpellStatus)
		s.mux.HandleFunc("GET /api/practice/spell_round", s.handleSpellRound)
		s.mux.HandleFunc("POST /api/practice/spell_answer", s.handleSpellAnswer)
		s.mux.HandleFunc("GET /api/practice/boss_status", s.handleBossStatus)
		s.mux.HandleFunc("GET /api/practice/boss_round", s.handleBossRound)
		s.mux.HandleFunc("POST /api/practice/boss_answer", s.handleBossAnswer)
		sub, err := fs.Sub(practiceStatic, "static/practice")
		if err != nil {
			panic(err) // the embed always carries the static files
		}
		static := http.StripPrefix("/static/practice/", http.FileServerFS(sub))
		s.mux.HandleFunc("GET /static/practice/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "public, max-age=86400")
			static.ServeHTTP(w, r)
		})
	})
}

// ------------------------------------------------------------------ shell

// practicePage wraps a page body in the shared dark-theme shell.
func practicePage(title, body string) string {
	return `<!doctype html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>` + html.EscapeString(title) + `</title>
<link rel="stylesheet" href="/static/practice/practice.css">
</head><body><div class="page">` + body + `</div></body></html>`
}

// writePracticePage answers a page request with the shared shell.
func writePracticePage(w http.ResponseWriter, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, practicePage(title, body))
}

// practicePageError answers a page request with a plain-words HTML error.
func practicePageError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	fmt.Fprint(w, practicePage(msg, `<h1>`+html.EscapeString(msg)+`</h1>
<p class="muted" style="text-align:center">Back to <a href="/practice">who's practicing</a>.</p>`))
}

// practiceEmojis give each kid a stable animal on the practice cards, the
// way the legacy mathfacts app assigned one per hardcoded kid.
var practiceEmojis = []string{"🦊", "🐸", "🐨", "🐻", "🦁", "🐯", "🐵", "🐼", "🦄", "🐷"}

func kidEmoji(id string) string {
	h := fnv.New32a()
	h.Write([]byte(id))
	return practiceEmojis[h.Sum32()%uint32(len(practiceEmojis))]
}

func practiceKidMessage(code int) string {
	switch code {
	case http.StatusServiceUnavailable:
		return "Practice is not running right now."
	case http.StatusNotFound:
		return "That kid isn't here."
	default:
		return "Could not load the kids."
	}
}

// practiceKid resolves a kid id to its display name. The returned status is
// http.StatusOK, or the error status to report (the caller picks the
// response format: JSON for APIs, HTML for pages).
func (s *Server) practiceKid(r *http.Request, id string) (string, int) {
	if s.deps.Practice == nil || s.deps.Budget == nil {
		return "", http.StatusServiceUnavailable
	}
	kids, err := s.deps.Budget.ListKids(r.Context())
	if err != nil {
		return "", http.StatusInternalServerError
	}
	for _, k := range kids {
		if string(k.ID) == id {
			return k.Name, http.StatusOK
		}
	}
	return "", http.StatusNotFound
}

// practiceKidOK is practiceKid for JSON APIs: failures answer writeError.
func (s *Server) practiceKidOK(w http.ResponseWriter, r *http.Request, id string) (string, bool) {
	name, code := s.practiceKid(r, id)
	if code != http.StatusOK {
		writeError(w, code, practiceKidMessage(code))
		return "", false
	}
	return name, true
}

// practicePageKidOK is practiceKid for pages: failures answer HTML.
func (s *Server) practicePageKidOK(w http.ResponseWriter, r *http.Request, id string) (string, bool) {
	name, code := s.practiceKid(r, id)
	if code == http.StatusOK {
		return name, true
	}
	practicePageError(w, code, practiceKidMessage(code))
	return "", false
}

// ------------------------------------------------------------ JSON shapes

type practiceBossView struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Emoji        string `json:"emoji"`
	Desc         string `json:"desc"`
	Lethal       bool   `json:"lethal"`
	PlayerHearts int    `json:"player_hearts"`
}

func bossView(b *practice.Boss) *practiceBossView {
	if b == nil {
		return nil
	}
	return &practiceBossView{ID: b.ID, Name: b.Name, Emoji: b.Emoji,
		Desc: b.Desc, Lethal: b.Lethal, PlayerHearts: b.PlayerHearts}
}

type practiceCoinView struct {
	Kind   string `json:"kind"`
	Coins  int    `json:"coins"`
	Need   int    `json:"need"`
	Banked bool   `json:"banked"`
	Err    string `json:"err,omitempty"`
}

func coinView(c *practice.CoinMsg) *practiceCoinView {
	if c == nil {
		return nil
	}
	return &practiceCoinView{Kind: c.Kind, Coins: c.Coins,
		Need: c.Need, Banked: c.Banked, Err: c.Err}
}

type practiceProgressView struct {
	Mastered int     `json:"mastered"`
	Total    int     `json:"total"`
	Pct      float64 `json:"pct"`
}

func progressView(p map[string]practice.ModeProgress) map[string]practiceProgressView {
	out := make(map[string]practiceProgressView, len(p))
	for mode, v := range p {
		out[mode] = practiceProgressView{Mastered: v.Mastered, Total: v.Total, Pct: v.Pct}
	}
	return out
}

type practiceGateView struct {
	Locked bool    `json:"locked"`
	Have   float64 `json:"have"`
	Need   float64 `json:"need"`
	Label  string  `json:"label"`
}

func gateView(g map[string]practice.Gate) map[string]practiceGateView {
	out := make(map[string]practiceGateView, len(g))
	for mode, v := range g {
		out[mode] = practiceGateView{Locked: v.Locked, Have: v.Have,
			Need: v.Need, Label: v.Label}
	}
	return out
}

type practiceAwardView struct {
	Kind      string   `json:"kind"`
	OK        bool     `json:"ok"`
	Miss      bool     `json:"miss"`
	AccFail   *bool    `json:"acc_fail,omitempty"`
	WasBestMS *float64 `json:"was_best_ms,omitempty"`
	NewBestMS *float64 `json:"new_best_ms,omitempty"`
	ThisMS    *float64 `json:"this_ms,omitempty"`
	Text      string   `json:"text,omitempty"`
}

type practiceTierView struct {
	Tier  int    `json:"tier"`
	Name  string `json:"name"`
	Have  int    `json:"have"`
	Total int    `json:"total"`
}

type practiceQuestionView struct {
	A  int    `json:"a"`
	B  int    `json:"b"`
	Op string `json:"op"`
}

// ---------------------------------------------------------------- errors

// practiceFail maps a practice service error onto a plain-words response.
func practiceFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, practice.ErrBadMode):
		writeError(w, http.StatusBadRequest, "That kind of practice doesn't exist.")
	case errors.Is(err, practice.ErrLocked):
		writeError(w, http.StatusForbidden, "That kind is still locked. Keep practicing the open ones.")
	case errors.Is(err, practice.ErrUnknownWord):
		writeError(w, http.StatusNotFound, "That word isn't in the spelling bank.")
	case errors.Is(err, practice.ErrNoBoss):
		writeError(w, http.StatusNotFound, "Every boss is already defeated.")
	case errors.Is(err, practice.ErrBossChanged):
		writeError(w, http.StatusConflict, "That fight came from an old page. Start a new one.")
	default:
		writeError(w, http.StatusInternalServerError, "Something went wrong. Try again.")
	}
}

// ------------------------------------------------------------------ pages

func (s *Server) handlePracticeHome(w http.ResponseWriter, r *http.Request) {
	if s.deps.Practice == nil || s.deps.Budget == nil {
		practicePageError(w, http.StatusServiceUnavailable, "Practice is not running right now.")
		return
	}
	pr, bud := s.deps.Practice, s.deps.Budget
	kids, err := bud.ListKids(r.Context())
	if err != nil {
		practicePageError(w, http.StatusInternalServerError, "Could not load the kids.")
		return
	}
	counter, err := pr.Counter()
	if err != nil {
		practicePageError(w, http.StatusInternalServerError, "Could not read the settings.")
		return
	}
	var cards strings.Builder
	for _, k := range kids {
		id := string(k.ID)
		ma, err := pr.Awards(id)
		if err != nil {
			practiceFail(w, err)
			return
		}
		sa, err := pr.SpellAwards(id)
		if err != nil {
			practiceFail(w, err)
			return
		}
		cfg, err := pr.ConfigFor(id)
		if err != nil {
			practiceFail(w, err)
			return
		}
		today, saved, err := bud.Balance(r.Context(), k.ID, counter)
		if err != nil {
			practicePageError(w, http.StatusInternalServerError, "Could not read the budget.")
			return
		}
		banked := today + saved
		fmt.Fprintf(&cards, `<button class="choice" onclick="location='/practice/%s/math'" style="flex-direction:column">
<span>%s %s</span>
<span class="muted" style="font-size:.8em;margin-top:2px">🎮 %s video minutes · today %s of %s minutes you can earn</span>
<span class="muted" style="font-size:.8em">🪙 %d of %d coins. %d coins earn you 1 more minute of watching</span>
</button>`, id, kidEmoji(id), html.EscapeString(k.Name),
			formatMinutes(banked), formatMinutes(ma.Earned), formatMinutes(ma.Cap),
			ma.Coins, cfg.CoinsPerMinute, cfg.CoinsPerMinute)
		fmt.Fprintf(&cards, `<button class="choice" onclick="location='/practice/%s/spell'" style="flex-direction:column">
<span>🔤 %s Spelling</span>
<span class="muted" style="font-size:.8em;margin-top:2px">Spelling today %s of %s minutes you can earn · 🪙 %d of %d coins. %d coins earn you 1 more minute</span>
</button>`, id, html.EscapeString(k.Name),
			formatMinutes(sa.Earned), formatMinutes(sa.Cap),
			sa.Coins, cfg.CoinsPerMinute, cfg.CoinsPerMinute)
	}
	body := `<h1>🔢 Math Practice</h1>
<div class="muted" style="text-align:center;margin-bottom:16px">Who's practicing?</div>
<div style="display:flex;flex-direction:column;align-items:center">` + cards.String() + `
<a class="btn gray" href="/" style="margin-top:16px;min-height:44px;display:inline-flex;align-items:center;padding:0 20px">← Back to the shelf</a></div>`
	writePracticePage(w, "Math Practice", body)
}

// formatMinutes shows whole minutes, never below 0, on kid screens: floats
// like 9.666 and negative balances are parent-page numbers, not kid words.
func formatMinutes(m float64) string {
	return strconv.Itoa(int(math.Max(0, math.Floor(m))))
}

const practiceRoundMarkup = `<div id="menu"></div>
<div id="play" style="display:none">
<div class="progress"><div class="bar" id="bar" style="width:0%"></div></div>
<div id="earned" class="muted" style="text-align:center;margin-bottom:6px"></div>
<div id="jar" style="text-align:center;margin-bottom:4px;font-size:1.1em;font-weight:700;color:#ffd76e"></div>
<div id="bar-line" style="text-align:center;margin-bottom:10px;font-size:1.05em;color:#ffd76e;font-weight:600"></div>
<div class="eq"><span class="big" id="q"></span></div>
<div class="ansrow">
<input class="ans" id="ans" type="number" inputmode="numeric" autocomplete="off" enterkeyhint="go">
<button class="btn" id="gobtn" style="font-size:1.6em">✓</button>
</div>
<div class="feedback" id="fb"></div>
<div style="text-align:center;margin-top:14px"><button type="button" class="btn gray" id="stopbtn">Stop this round</button></div>
</div>
<div id="done" style="display:none"></div>`

func roundPageBody(id, name string, checkup bool) string {
	h1 := `<h1>` + kidEmoji(id) + " " + html.EscapeString(name) + `</h1>`
	seed := "0"
	if checkup {
		h1 = `<h1>🩺 Check-up: ` + html.EscapeString(name) + `</h1>`
		seed = "1"
	}
	return h1 + `<div id="menustatus" class="muted" style="text-align:center;margin-bottom:10px"></div>` +
		practiceRoundMarkup +
		`<script>window.KID=` + strconv.Quote(id) + `;window.CHECKUP=` + seed + `;</script>
<script src="/static/practice/math.js"></script>`
}

// handlePracticePage dispatches /practice/{page...} to the four kid pages,
// parsing the kid id and page name from the remainder.
func (s *Server) handlePracticePage(w http.ResponseWriter, r *http.Request) {
	rest := r.PathValue("page")
	kid, page, ok := strings.Cut(rest, "/")
	if !ok {
		practicePageError(w, http.StatusNotFound, "That isn't a practice page.")
		return
	}
	switch page {
	case "math":
		name, ok := s.practicePageKidOK(w, r, kid)
		if !ok {
			return
		}
		writePracticePage(w, name+" - Math", roundPageBody(kid, name, false))
	case "checkup":
		name, ok := s.practicePageKidOK(w, r, kid)
		if !ok {
			return
		}
		writePracticePage(w, "Check-up - "+name, roundPageBody(kid, name, true))
	case "spell":
		s.handleSpellPage(w, r, kid)
	case "boss":
		s.handleBossPage(w, r, kid)
	default:
		practicePageError(w, http.StatusNotFound, "That isn't a practice page.")
	}
}

func (s *Server) handleSpellPage(w http.ResponseWriter, r *http.Request, id string) {
	name, ok := s.practicePageKidOK(w, r, id)
	if !ok {
		return
	}
	hints := make(map[string]string)
	for _, w := range practice.Words() {
		hints[w.Word] = w.Hint
	}
	hintJSON, _ := json.Marshal(hints)
	body := `<h1>` + kidEmoji(id) + " " + html.EscapeString(name) + ` - 🔤 Spelling</h1>
<div id="menu"><div class="muted" style="text-align:center">Loading…</div></div>
<div id="play" style="display:none">
<div class="progress"><div class="bar" id="bar" style="width:0%"></div></div>
<div id="earned" class="muted" style="text-align:center;margin-bottom:6px"></div>
<div id="jar" style="text-align:center;margin-bottom:4px;font-size:1.1em;font-weight:700;color:#ffd76e"></div>
<div style="text-align:center;margin:18px 0">
<button class="btn orange" style="font-size:1.6em" onclick="playWord()">🔊 Play the word</button>
</div>
<div class="muted" style="text-align:center;margin-bottom:6px" id="hint"></div>
<div class="ansrow">
<input class="ans" id="ans" style="width:280px" autocomplete="off" autocapitalize="off" spellcheck="false" placeholder="type the word" enterkeyhint="go">
<button class="btn green" id="gobtn" style="font-size:1.5em">✓</button>
</div>
<div id="fb" class="feedback" style="min-height:1.5em;margin-top:10px"></div>
<div style="text-align:center;margin-top:14px"><button type="button" class="btn gray" id="stopbtn">Stop this round</button></div>
</div>
<div id="done" style="display:none"></div>
<script>window.KID=` + strconv.Quote(id) + `;window.HINTS=` + string(hintJSON) + `;</script>
<script src="/static/practice/spell.js"></script>`
	writePracticePage(w, name+" - Spelling", body)
}

func (s *Server) handleBossPage(w http.ResponseWriter, r *http.Request, id string) {
	_, ok := s.practicePageKidOK(w, r, id)
	if !ok {
		return
	}
	body := `<h1 style="text-align:center">⚔️ Boss Arena</h1>
<div id="menu"><div class="muted" style="text-align:center">Loading…</div></div>
<div id="play" style="display:none">
<div id="art" style="text-align:center"></div>
<div id="hearts" style="text-align:center;font-size:1.6em;color:#e04b4b"></div>
<div id="jar" style="text-align:center;margin:6px 0;font-weight:700;color:#ffd76e"></div>
<div id="q" style="text-align:center;font-size:2em;font-weight:800;margin:14px 0"></div>
<div style="max-width:260px;margin:0 auto 4px;height:10px;border-radius:6px;background:#2a3547;overflow:hidden">
<div id="tbar" style="height:100%;width:100%;background:#4ba3e0;border-radius:6px"></div>
</div>
<div id="ttxt" class="muted" style="text-align:center;font-size:.85em"></div>
<div class="ansrow">
<input class="ans" id="ans" style="width:200px" autocomplete="off" inputmode="numeric" placeholder="?" enterkeyhint="go">
<button class="btn red" id="gobtn" style="font-size:1.5em">✓</button>
</div>
<div id="fb" class="feedback" style="min-height:1.5em;margin-top:10px"></div>
<div style="text-align:center;margin-top:14px"><button type="button" class="btn gray" id="stopbtn">Stop the fight</button></div>
</div>
<div id="done" style="display:none"></div>
<script>window.KID=` + strconv.Quote(id) + `;</script>
<script src="/static/practice/boss.js"></script>`
	writePracticePage(w, "Boss Arena", body)
}

// ------------------------------------------------------------- spell audio

func (s *Server) handlePracticeAudio(w http.ResponseWriter, r *http.Request) {
	if s.deps.Practice == nil {
		writeError(w, http.StatusServiceUnavailable, "Practice is not running right now.")
		return
	}
	raw := r.PathValue("word")
	if !strings.HasSuffix(raw, ".wav") {
		writeError(w, http.StatusNotFound, "No clip for that word.")
		return
	}
	word := strings.TrimSuffix(raw, ".wav")
	if _, ok := practice.WordHint(word); !ok {
		writeError(w, http.StatusNotFound, "That word isn't in the spelling bank.")
		return
	}
	path, ok := s.deps.Practice.AudioPath(word)
	if !ok {
		writeError(w, http.StatusNotFound, "No clip for that word yet.")
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		writeError(w, http.StatusNotFound, "No clip for that word yet.")
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(data)
}

// -------------------------------------------------------------- math APIs

func (s *Server) handlePracticeStatus(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("kid")
	if _, ok := s.practiceKidOK(w, r, id); !ok {
		return
	}
	pr := s.deps.Practice
	promo, err := pr.PromotionStatus(id)
	if err != nil {
		practiceFail(w, err)
		return
	}
	open, gates, err := pr.OpenModes(id)
	if err != nil {
		practiceFail(w, err)
		return
	}
	aw, err := pr.Awards(id)
	if err != nil {
		practiceFail(w, err)
		return
	}
	cfg, err := pr.ConfigFor(id)
	if err != nil {
		practiceFail(w, err)
		return
	}
	best := make(map[string]*float64, len(practice.AllModes))
	for _, m := range practice.AllModes {
		b, err := pr.BestRound(id, m)
		if err != nil {
			practiceFail(w, err)
			return
		}
		best[m] = b
	}
	fight, err := pr.BossStatus(id)
	if err != nil {
		practiceFail(w, err)
		return
	}
	top, err := pr.TablesTop(id)
	if err != nil {
		practiceFail(w, err)
		return
	}
	openModes := make([]string, 0, len(practice.AllModes))
	for _, m := range practice.AllModes {
		if open[m] {
			openModes = append(openModes, m)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "kid": id, "promo": progressView(promo),
		"earned_today": aw.Earned, "coins": aw.Coins,
		"coins_per_minute": cfg.CoinsPerMinute, "cap": aw.Cap,
		"best_ms": best, "open_modes": openModes, "gates": gateView(gates),
		"tables_top": top, "boss": bossView(fight.Boss), "boss_coins": cfg.BossCoins,
	})
}

func (s *Server) handlePracticeRound(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("kid")
	if _, ok := s.practiceKidOK(w, r, id); !ok {
		return
	}
	pr := s.deps.Practice
	cfg, err := pr.ConfigFor(id)
	if err != nil {
		practiceFail(w, err)
		return
	}
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = practice.ModeAdd
	}
	checkup := r.URL.Query().Get("checkup") == "1"
	n := cfg.RoundGoal
	if checkup {
		n = cfg.CheckupSize
	}
	qs, err := pr.SampleQuestions(id, mode, n)
	if err != nil {
		if errors.Is(err, practice.ErrLocked) {
			gates, gerr := pr.ModeUnlockStatus(id)
			if gerr != nil {
				practiceFail(w, gerr)
				return
			}
			g := gates[mode]
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error": "That kind is still locked. Keep practicing to unlock it.",
				"err":   "locked",
				"have":  g.Have,
				"need":  g.Need,
				"label": g.Label,
			})
			return
		}
		practiceFail(w, err)
		return
	}
	questions := make([]practiceQuestionView, 0, len(qs))
	for _, q := range qs {
		questions = append(questions, practiceQuestionView{A: q.A, B: q.B, Op: q.Op})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "mode": mode, "checkup": checkup, "questions": questions,
	})
}

type practiceAnswerBody struct {
	Kid   string `json:"kid"`
	Mode  string `json:"mode"`
	A     int    `json:"a"`
	B     int    `json:"b"`
	Op    string `json:"op"`
	Given int    `json:"given"`
	Ms    int    `json:"ms"`
}

func (s *Server) handlePracticeAnswer(w http.ResponseWriter, r *http.Request) {
	var body practiceAnswerBody
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "That answer didn't come through properly.")
		return
	}
	if _, ok := s.practiceKidOK(w, r, body.Kid); !ok {
		return
	}
	out, err := s.deps.Practice.RecordAnswer(r.Context(), body.Kid, body.Mode,
		body.A, body.B, body.Op, body.Given, body.Ms)
	if err != nil {
		practiceFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "correct": out.Correct, "answer": out.Answer,
		"record": out.Record, "coin": coinView(out.Coin),
	})
}

type practiceRoundAnswerBody struct {
	A     int    `json:"a"`
	B     int    `json:"b"`
	Op    string `json:"op"`
	Given int    `json:"given"`
	Ms    int    `json:"ms"`
}

type practiceRoundResultBody struct {
	Kid     string                    `json:"kid"`
	Mode    string                    `json:"mode"`
	Checkup bool                      `json:"checkup"`
	Results []practiceRoundAnswerBody `json:"results"`
}

func (s *Server) handlePracticeRoundResult(w http.ResponseWriter, r *http.Request) {
	var body practiceRoundResultBody
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "That round didn't come through properly.")
		return
	}
	if _, ok := s.practiceKidOK(w, r, body.Kid); !ok {
		return
	}
	answers := make([]practice.RoundAnswer, 0, len(body.Results))
	for _, res := range body.Results {
		answers = append(answers, practice.RoundAnswer{
			A: res.A, B: res.B, Op: res.Op, Given: res.Given, Ms: res.Ms,
		})
	}
	sum, err := s.deps.Practice.FinishRound(r.Context(), body.Kid, body.Mode,
		body.Checkup, answers)
	if err != nil {
		practiceFail(w, err)
		return
	}
	awards := make([]practiceAwardView, 0, len(sum.Awards))
	for _, a := range sum.Awards {
		awards = append(awards, practiceAwardView{
			Kind: a.Kind, OK: a.OK, Miss: a.Miss, AccFail: a.AccFail,
			WasBestMS: a.WasBestMS, NewBestMS: a.NewBestMS, ThisMS: a.ThisMS,
			Text: a.Text,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "n": sum.N, "correct": sum.Correct, "median_ms": sum.MedianMS,
		"promo": progressView(sum.Promo), "awards": awards,
		"earned_today": sum.EarnedToday, "cap": sum.Cap,
	})
}

// ----------------------------------------------------------- spelling APIs

func (s *Server) handleSpellStatus(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("kid")
	if _, ok := s.practiceKidOK(w, r, id); !ok {
		return
	}
	pr := s.deps.Practice
	st, err := pr.SpellStatus(id)
	if err != nil {
		practiceFail(w, err)
		return
	}
	records, err := pr.SpellingRecords(id)
	if err != nil {
		practiceFail(w, err)
		return
	}
	aw, err := pr.SpellAwards(id)
	if err != nil {
		practiceFail(w, err)
		return
	}
	cfg, err := pr.ConfigFor(id)
	if err != nil {
		practiceFail(w, err)
		return
	}
	have := make(map[string]bool, len(records))
	for _, rec := range records {
		if rec.BestMS != 0 {
			have[rec.Word] = true
		}
	}
	tiers := practice.Tiers()
	tierViews := make([]practiceTierView, 0, len(tiers))
	for i, t := range tiers {
		n := 0
		for _, w := range t.Words {
			if have[w] {
				n++
			}
		}
		tierViews = append(tierViews, practiceTierView{Tier: i, Name: t.Name, Have: n, Total: len(t.Words)})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "total": st.Total, "recorded": st.Recorded, "missed": st.Missed,
		"tiers": tierViews, "earned_today": aw.Earned, "coins": aw.Coins,
		"coins_per_minute": cfg.CoinsPerMinute, "cap": aw.Cap,
	})
}

func (s *Server) handleSpellRound(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("kid")
	if _, ok := s.practiceKidOK(w, r, id); !ok {
		return
	}
	pr := s.deps.Practice
	cfg, err := pr.ConfigFor(id)
	if err != nil {
		practiceFail(w, err)
		return
	}
	var pool []string
	if tierQ := r.URL.Query().Get("tier"); tierQ != "" {
		n, err := strconv.Atoi(tierQ)
		if err != nil || n < 0 || n >= len(practice.Tiers()) {
			writeError(w, http.StatusBadRequest, "That spelling group doesn't exist.")
			return
		}
		pool = practice.Tiers()[n].Words
	}
	words, err := pr.SampleSpellWords(id, cfg.RoundGoal, pool)
	if err != nil {
		practiceFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "words": words})
}

type practiceSpellAnswerBody struct {
	Kid   string `json:"kid"`
	Word  string `json:"word"`
	Given string `json:"given"`
	Ms    int    `json:"ms"`
}

func (s *Server) handleSpellAnswer(w http.ResponseWriter, r *http.Request) {
	var body practiceSpellAnswerBody
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "That answer didn't come through properly.")
		return
	}
	if _, ok := s.practiceKidOK(w, r, body.Kid); !ok {
		return
	}
	out, err := s.deps.Practice.RecordSpellAnswer(r.Context(), body.Kid,
		body.Word, body.Given, body.Ms)
	if err != nil {
		practiceFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "correct": out.Correct, "word": out.Word,
		"record": out.Record, "coin": coinView(out.Coin),
	})
}

// -------------------------------------------------------------- boss APIs

func (s *Server) handleBossStatus(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("kid")
	if _, ok := s.practiceKidOK(w, r, id); !ok {
		return
	}
	f, err := s.deps.Practice.BossStatus(id)
	if err != nil {
		practiceFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "boss": bossView(f.Boss), "hp": f.HP, "hearts": f.Hearts,
		"php": f.PHP, "defeated": f.Defeated, "reward_coins": f.RewardCoins,
		"earned_today": f.EarnedToday, "coins": f.Coins, "cap": f.Cap,
		"coins_per_minute": f.CoinsPerMinute,
	})
}

func (s *Server) handleBossRound(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("kid")
	if _, ok := s.practiceKidOK(w, r, id); !ok {
		return
	}
	f, qs, err := s.deps.Practice.BossRound(id)
	if err != nil {
		practiceFail(w, err)
		return
	}
	questions := make([]practiceQuestionView, 0, len(qs))
	for _, q := range qs {
		questions = append(questions, practiceQuestionView{A: q.A, B: q.B, Op: q.Op})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "boss": bossView(f.Boss), "hearts": f.Hearts, "questions": questions,
	})
}

type practiceBossAnswerBody struct {
	Kid   string `json:"kid"`
	A     int    `json:"a"`
	B     int    `json:"b"`
	Op    string `json:"op"`
	Given int    `json:"given"`
}

func (s *Server) handleBossAnswer(w http.ResponseWriter, r *http.Request) {
	var body practiceBossAnswerBody
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "That answer didn't come through properly.")
		return
	}
	if _, ok := s.practiceKidOK(w, r, body.Kid); !ok {
		return
	}
	m, err := s.deps.Practice.BossAnswer(r.Context(), body.Kid,
		body.A, body.B, body.Op, body.Given)
	if err != nil {
		if errors.Is(err, practice.ErrBossChanged) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":  "That fight came from an old page. Start a new one.",
				"err":    "boss_changed",
				"answer": practice.AnswerOf(body.A, body.B, body.Op),
			})
			return
		}
		practiceFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "correct": m.Correct, "answer": m.Answer,
		"msg": map[string]any{
			"kind": m.Kind, "hp": m.HP, "php": m.PHP, "whack": m.Whack,
			"victory": m.Victory, "killed": m.Killed,
			"reward": coinView(m.Reward), "next_boss": bossView(m.NextBoss),
		},
	})
}
