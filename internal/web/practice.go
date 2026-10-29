// Package web, practice area. Serves the ported mathfacts pages on
// practice.Service: the "who's practicing" home, math and check-up rounds,
// spelling rounds with pre-generated clips and the 13-boss arena. Pages are
// server-rendered shells over plain JS/CSS embedded from static/practice;
// every answer is graded by the service, and earned minutes go to the
// budget counter through practice's Crediter. JSON lives under
// /api/practice with the legacy ?kid= query shape. A kid practices only as
// the kid signed in with their PIN (the shelf's unlock session), so one
// sibling cannot earn minutes or spoil records on another's card.
package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io/fs"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

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
<link rel="stylesheet" href="` + practiceAsset("practice.css") + `">
</head><body><div class="page">` + body + `</div></body></html>`
}

var (
	practiceAssetsOnce sync.Once
	practiceAssetURLs  map[string]string
)

// practiceAsset returns the URL of a static/practice file with a content
// hash in the query, so a new build's scripts replace the day-long cached
// ones at once (an old cached script would not send what the APIs need).
func practiceAsset(name string) string {
	practiceAssetsOnce.Do(func() {
		practiceAssetURLs = make(map[string]string)
		entries, err := fs.ReadDir(practiceStatic, "static/practice")
		if err != nil {
			panic(err) // the embed always carries the static files
		}
		for _, e := range entries {
			b, err := practiceStatic.ReadFile("static/practice/" + e.Name())
			if err != nil {
				panic(err)
			}
			sum := sha256.Sum256(b)
			practiceAssetURLs[e.Name()] = "/static/practice/" + e.Name() + "?v=" + hex.EncodeToString(sum[:6])
		}
	})
	if u, ok := practiceAssetURLs[name]; ok {
		return u
	}
	return "/static/practice/" + name
}

// practiceScripts loads the shared helpers, then the page script.
func practiceScripts(page string) string {
	return `<script src="` + practiceAsset("common.js") + `"></script>
<script src="` + practiceAsset(page) + `"></script>`
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
<div class="col"><a class="btn gray" href="/practice">← Back</a></div>`))
}

// practiceKidInfo is a resolved kid: display name and shelf animal.
type practiceKidInfo struct {
	name  string
	emoji string
}

func practiceKidMessage(code int) string {
	switch code {
	case http.StatusServiceUnavailable:
		return "Practice is not running right now."
	case http.StatusNotFound:
		return "That kid isn't here."
	case http.StatusUnauthorized:
		return "Pick your name and type your PIN first."
	default:
		return "Could not load the kids."
	}
}

// practiceKid resolves a kid id to its name and animal and checks that this
// kid is the one signed in with their PIN. The returned status is
// http.StatusOK, or the error status to report (the caller picks the
// response format: JSON for APIs, HTML for pages).
func (s *Server) practiceKid(r *http.Request, id string) (practiceKidInfo, int) {
	if s.deps.Practice == nil || s.deps.Budget == nil {
		return practiceKidInfo{}, http.StatusServiceUnavailable
	}
	kids, err := s.deps.Budget.ListKids(r.Context())
	if err != nil {
		return practiceKidInfo{}, http.StatusInternalServerError
	}
	for _, k := range kids {
		if string(k.ID) != id {
			continue
		}
		if sess, ok := s.currentSession(r); !ok || sess.grownup || string(sess.kid) != id {
			return practiceKidInfo{}, http.StatusUnauthorized
		}
		return practiceKidInfo{name: k.Name, emoji: k.Animal}, http.StatusOK
	}
	return practiceKidInfo{}, http.StatusNotFound
}

// practiceKidOK is practiceKid for JSON APIs: failures answer JSON, and a
// missing sign-in carries err "signin" so the page can send the kid back
// to the PIN screen.
func (s *Server) practiceKidOK(w http.ResponseWriter, r *http.Request, id string) (practiceKidInfo, bool) {
	kid, code := s.practiceKid(r, id)
	switch code {
	case http.StatusOK:
		return kid, true
	case http.StatusUnauthorized:
		writeJSON(w, code, map[string]string{"error": practiceKidMessage(code), "err": "signin"})
	default:
		writeError(w, code, practiceKidMessage(code))
	}
	return practiceKidInfo{}, false
}

// practicePageKidOK is practiceKid for pages: a kid who is not signed in is
// sent to the practice home to type their PIN; other failures answer HTML.
func (s *Server) practicePageKidOK(w http.ResponseWriter, r *http.Request, id string) (practiceKidInfo, bool) {
	kid, code := s.practiceKid(r, id)
	switch code {
	case http.StatusOK:
		return kid, true
	case http.StatusUnauthorized:
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, "/practice", http.StatusSeeOther)
	default:
		practicePageError(w, code, practiceKidMessage(code))
	}
	return practiceKidInfo{}, false
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
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "That boss is gone. Let's go to the next one.", "err": "boss_changed"})
	case errors.Is(err, practice.ErrStaleAnswer):
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "That round is over. Let's start a new one.", "err": "stale"})
	default:
		writeError(w, http.StatusInternalServerError, "Something went wrong. Try again.")
	}
}

// ------------------------------------------------------------------ pages

// handlePracticeHome is "who's practicing": the signed-in kid's math and
// spelling cards, or, when nobody is signed in, the kids to pick from with
// a PIN box (the same unlock as the shelf).
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
	back := `<a class="btn gray" href="/">← Back to the shelf</a>`
	if len(kids) == 0 {
		writePracticePage(w, "Practice", `<h1>🔢 Practice</h1>
<p class="lead">No kids are set up yet. A grown-up can add kids on the parent page.</p>
<div class="col"><a class="btn" href="/admin">Open the parent page</a>`+back+`</div>`)
		return
	}
	sess, signedIn := s.currentSession(r)
	me := -1
	if signedIn && !sess.grownup {
		for i, k := range kids {
			if k.ID == sess.kid {
				me = i
			}
		}
	}
	if me < 0 {
		var tiles strings.Builder
		for _, k := range kids {
			fmt.Fprintf(&tiles, `<button type="button" class="choice" data-kid="%s" data-name="%s">%s %s</button>
`, html.EscapeString(string(k.ID)), html.EscapeString(k.Name),
				html.EscapeString(k.Animal), html.EscapeString(k.Name))
		}
		writePracticePage(w, "Practice", `<h1>🔢 Practice</h1>
<div id="pick"><p class="lead">Who's practicing?</p>
<div class="col">`+tiles.String()+back+`</div></div>
<div id="pin" class="col" hidden>
<p class="lead" id="pinwho"></p>
<input class="ans" id="pinbox" type="password" inputmode="numeric" autocomplete="off" enterkeyhint="go" aria-label="Your PIN">
<div class="feedback no" id="pinmsg" role="alert"></div>
<button type="button" class="btn green" id="pingo">Go</button>
<button type="button" class="btn gray" id="pinback">← Back</button>
</div>
`+practiceScripts("home.js"))
		return
	}
	k := kids[me]
	id := string(k.ID)
	counter, err := pr.Counter()
	if err != nil {
		practicePageError(w, http.StatusInternalServerError, "Could not read the settings.")
		return
	}
	ma, err := pr.Awards(id)
	if err != nil {
		practicePageError(w, http.StatusInternalServerError, "Could not load your practice. Try again.")
		return
	}
	sa, err := pr.SpellAwards(id)
	if err != nil {
		practicePageError(w, http.StatusInternalServerError, "Could not load your practice. Try again.")
		return
	}
	today, saved, err := bud.Balance(r.Context(), k.ID, counter)
	if err != nil {
		practicePageError(w, http.StatusInternalServerError, "Could not read the budget.")
		return
	}
	body := fmt.Sprintf(`<h1>%s %s</h1>
<p class="lead">🎮 You have %s to watch.</p>
<div class="col">
<a class="choice col-card" href="/practice/%s/math"><span>🔢 Math</span><span class="sub">%s</span></a>
<a class="choice col-card" href="/practice/%s/spell"><span>🔤 Spelling</span><span class="sub">%s</span></a>
<button type="button" class="btn gray" id="notme">🔒 Not you?</button>
%s</div>
%s`, html.EscapeString(k.Animal), html.EscapeString(k.Name),
		minutesWord(today+saved),
		id, earnedLine(ma.Earned, ma.Cap),
		id, earnedLine(sa.Earned, sa.Cap),
		back, practiceScripts("home.js"))
	writePracticePage(w, "Practice", body)
}

// minutesWord is "1 minute", "3 minutes": whole minutes in kid words.
func minutesWord(m float64) string {
	n := formatMinutes(m)
	if n == "1" {
		return "1 minute"
	}
	return n + " minutes"
}

// earnedLine says how much of today's earning is done, in kid words.
func earnedLine(earned, capMin float64) string {
	if earned >= capMin-0.001 {
		return "All " + minutesWord(capMin) + " earned today"
	}
	return "Earned today: " + formatMinutes(earned) + " of " + minutesWord(capMin)
}

// formatMinutes shows whole minutes, never below 0, on kid screens: floats
// like 9.666 and negative balances are parent-page numbers, not kid words.
func formatMinutes(m float64) string {
	return strconv.Itoa(int(math.Max(0, math.Floor(m))))
}

// practiceRoundMarkup is the math and check-up play area. The answer box
// is a text box with the number keypad (no spinner arrows, and the arrow
// keys never change a typed answer).
const practiceRoundMarkup = `<div id="menu" class="col"><p class="lead">Loading…</p></div>
<div id="play" hidden>
<div class="progress"><div class="bar" id="bar" style="width:0%"></div></div>
<div id="banner" class="banner" role="status" hidden></div>
<div id="earned" class="muted center"></div>
<div id="jar" class="jar"></div>
<div id="bar-line" class="goal"></div>
<div class="eq"><span class="big" id="q"></span></div>
<div class="ansrow">
<input class="ans" id="ans" type="text" inputmode="numeric" pattern="[0-9]*" autocomplete="off" enterkeyhint="go" aria-label="Your answer">
<button type="button" class="btn" id="gobtn" style="font-size:1.6em" aria-label="Check">✓</button>
</div>
<div class="feedback" id="fb" role="status"></div>
<div class="col"><button type="button" class="btn gray" id="stopbtn">Stop this round</button></div>
</div>
<div id="done" class="col" hidden></div>`

func roundPageBody(id string, kid practiceKidInfo, checkup bool) string {
	h1 := `<h1>` + kid.emoji + " " + html.EscapeString(kid.name) + `</h1>`
	seed := "0"
	if checkup {
		h1 = `<h1>🩺 Check-up: ` + html.EscapeString(kid.name) + `</h1>`
		seed = "1"
	}
	return h1 + practiceRoundMarkup +
		`<script>window.KID=` + strconv.Quote(id) + `;window.CHECKUP=` + seed + `;</script>
` + practiceScripts("math.js")
}

// handlePracticePage dispatches /practice/{page...} to the four kid pages,
// parsing the kid id and page name from the remainder.
func (s *Server) handlePracticePage(w http.ResponseWriter, r *http.Request) {
	rest := r.PathValue("page")
	id, page, ok := strings.Cut(rest, "/")
	if !ok {
		practicePageError(w, http.StatusNotFound, "That isn't a practice page.")
		return
	}
	switch page {
	case "math", "checkup", "spell", "boss":
	default:
		practicePageError(w, http.StatusNotFound, "That isn't a practice page.")
		return
	}
	kid, ok := s.practicePageKidOK(w, r, id)
	if !ok {
		return
	}
	switch page {
	case "math":
		writePracticePage(w, kid.name+" - Math", roundPageBody(id, kid, false))
	case "checkup":
		writePracticePage(w, "Check-up - "+kid.name, roundPageBody(id, kid, true))
	case "spell":
		writePracticePage(w, kid.name+" - Spelling", spellPageBody(id, kid))
	case "boss":
		writePracticePage(w, "Boss Arena", bossPageBody(id))
	}
}

func spellPageBody(id string, kid practiceKidInfo) string {
	hints := make(map[string]string)
	for _, w := range practice.Words() {
		hints[w.Word] = w.Hint
	}
	hintJSON, _ := json.Marshal(hints)
	return `<h1>` + kid.emoji + " " + html.EscapeString(kid.name) + `: 🔤 Spelling</h1>
<div id="menu" class="col"><p class="lead">Loading…</p></div>
<div id="play" hidden>
<div class="progress"><div class="bar" id="bar" style="width:0%"></div></div>
<div id="banner" class="banner" role="status" hidden></div>
<div id="earned" class="muted center"></div>
<div id="jar" class="jar"></div>
<div class="col">
<button type="button" class="btn orange" style="font-size:1.6em" id="playbtn">🔊 Play the word</button>
</div>
<div class="hint" id="hint"></div>
<div class="ansrow">
<input class="ans" id="ans" style="width:280px" autocomplete="off" autocapitalize="off" autocorrect="off" spellcheck="false" placeholder="type the word" enterkeyhint="go" aria-label="Type the word">
<button type="button" class="btn green" id="gobtn" style="font-size:1.5em" aria-label="Check">✓</button>
</div>
<div id="fb" class="feedback" role="status"></div>
<div class="col"><button type="button" class="btn gray" id="stopbtn">Stop this round</button></div>
</div>
<div id="done" class="col" hidden></div>
<script>window.KID=` + strconv.Quote(id) + `;window.HINTS=` + string(hintJSON) + `;</script>
` + practiceScripts("spell.js")
}

func bossPageBody(id string) string {
	return `<h1>⚔️ Boss Arena</h1>
<div id="menu" class="col"><p class="lead">Loading…</p></div>
<div id="play" hidden>
<div id="art" class="center"></div>
<div id="hearts" class="center" style="font-size:1.6em"></div>
<div id="jar" class="jar"></div>
<div id="q" class="center" style="font-size:2em;font-weight:800;margin:14px 0"></div>
<div class="timer"><div id="tbar"></div></div>
<div id="ttxt" class="muted center"></div>
<div class="ansrow">
<input class="ans" id="ans" style="width:200px" type="text" autocomplete="off" inputmode="numeric" pattern="[0-9]*" placeholder="?" enterkeyhint="go" aria-label="Your answer">
<button type="button" class="btn red" id="gobtn" style="font-size:1.5em" aria-label="Check">✓</button>
</div>
<div id="fb" class="feedback" role="status"></div>
<div class="col"><button type="button" class="btn gray" id="stopbtn">Stop the fight</button></div>
</div>
<div id="done" class="col" hidden></div>
<script>window.KID=` + strconv.Quote(id) + `;</script>
` + practiceScripts("boss.js")
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
		"round": pr.OpenMathRound(id, mode, qs),
	})
}

// practiceAnswerBody is one math answer. Round and N name the question on
// screen (the round id from /api/practice/round and its position), so a
// repeated or early send can never grade a question the kid has not seen.
type practiceAnswerBody struct {
	Kid   string `json:"kid"`
	Mode  string `json:"mode"`
	Round string `json:"round"`
	N     int    `json:"n"`
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
	out, err := s.deps.Practice.RecordRoundAnswer(r.Context(), body.Kid, body.Mode,
		body.Round, body.N, body.A, body.B, body.Op, body.Given, body.Ms)
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
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "words": words, "round": pr.OpenSpellRound(id, words),
	})
}

// practiceSpellAnswerBody is one spelling answer; Round and N name the
// word being played, as in practiceAnswerBody.
type practiceSpellAnswerBody struct {
	Kid   string `json:"kid"`
	Round string `json:"round"`
	N     int    `json:"n"`
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
	out, err := s.deps.Practice.RecordSpellRoundAnswer(r.Context(), body.Kid,
		body.Round, body.N, body.Word, body.Given, body.Ms)
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
		"ok": true, "boss": bossView(f.Boss), "hearts": f.Hearts, "php": f.PHP,
		"questions": questions, "round": f.Round,
	})
}

// practiceBossAnswerBody is one boss answer; Round and N name the question
// on screen of the fight from /api/practice/boss_round.
type practiceBossAnswerBody struct {
	Kid   string `json:"kid"`
	Round string `json:"round"`
	N     int    `json:"n"`
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
	m, err := s.deps.Practice.BossRoundAnswer(r.Context(), body.Kid,
		body.Round, body.N, body.A, body.B, body.Op, body.Given)
	if err != nil {
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
