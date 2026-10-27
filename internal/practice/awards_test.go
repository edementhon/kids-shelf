package practice

import (
	"errors"
	"math/rand/v2"
	"path/filepath"
	"testing"
	"time"

	"github.com/edementhon/kids-shelf/internal/core"
	"github.com/edementhon/kids-shelf/internal/store"
)

// fastAnswer records one correct, fast fact answer (a distinct fact per
// index), granting a coin.
func fastAnswer(t *testing.T, svc *Service, kid string, i int) {
	t.Helper()
	answer(t, svc, kid, ModeAdd, i, 1, "+", i+1, 5000)
}

func TestJarBanksMinutes(t *testing.T) {
	e := newEnv(t, 11)
	kid := "k1"
	for i := range 10 {
		fastAnswer(t, e.svc, kid, i)
	}
	credits := e.cred.Credits()
	if len(credits) != 1 {
		t.Fatalf("credits = %d, want 1 for a banked jar", len(credits))
	}
	c := credits[0]
	if c.Kid != kid || c.Counter != "video" || c.Minutes != 1 {
		t.Errorf("credit = %+v, want kid=k1 counter=video minutes=1", c)
	}
	if c.AwardID != "k1:jar:2026-10-26:1" {
		t.Errorf("award id = %q, want k1:jar:2026-10-26:1", c.AwardID)
	}
	aw, err := e.svc.Awards(kid)
	if err != nil {
		t.Fatalf("Awards: %v", err)
	}
	if aw.Earned != 1.0 || aw.Coins != 0 {
		t.Errorf("after first jar: earned=%v coins=%d, want 1/0", aw.Earned, aw.Coins)
	}
	if aw.Cap != 3 {
		t.Errorf("cap = %v, want 3", aw.Cap)
	}

	// Ten more records bank a second minute with the next sequence number.
	for i := 10; i < 20; i++ {
		fastAnswer(t, e.svc, kid, i)
	}
	credits = e.cred.Credits()
	if len(credits) != 2 || credits[1].AwardID != "k1:jar:2026-10-26:2" {
		t.Fatalf("credits = %+v, want 2 with sequence 2", credits)
	}
	logs, err := e.svc.AwardLog()
	if err != nil {
		t.Fatalf("AwardLog: %v", err)
	}
	if len(logs) != 2 || logs[0].Kind != "jar" || logs[0].Amount != 1 || logs[0].AwardID != "k1:jar:2026-10-26:1" {
		t.Errorf("award log = %+v", logs)
	}
}

// TestDailyCapWithFakeClock earns to the 3-minute math cap, then proves a
// new local day resets the jar: the same work earns again on day two.
func TestDailyCapWithFakeClock(t *testing.T) {
	e := newEnv(t, 12)
	kid := "k1"
	for i := range 30 {
		fastAnswer(t, e.svc, kid, i)
	}
	if got := len(e.cred.Credits()); got != 3 {
		t.Fatalf("day-1 credits = %d, want 3", got)
	}
	// 31st record: records still improve, coins stop.
	out := answer(t, e.svc, kid, ModeAdd, 99, 1, "+", 100, 4000)
	if !out.Record {
		t.Errorf("31st record should still set a best")
	}
	if out.Coin == nil || out.Coin.Kind != "capped" {
		t.Fatalf("31st record coin = %+v, want capped", out.Coin)
	}
	aw, _ := e.svc.Awards(kid)
	if aw.Earned != 3.0 || aw.Day != "2026-10-26" {
		t.Errorf("end of day 1 awards = %+v", aw)
	}

	// Next local day: the cap resets and the jar starts over.
	e.now = e.now.Add(24 * time.Hour)
	out = answer(t, e.svc, kid, ModeAdd, 100, 1, "+", 101, 3000)
	if out.Coin == nil || out.Coin.Kind != "coin" || out.Coin.Coins != 1 {
		t.Fatalf("first day-2 answer coin = %+v, want 1 coin", out.Coin)
	}
	aw, _ = e.svc.Awards(kid)
	if aw.Day != "2026-10-27" || aw.Earned != 0 || aw.Coins != 1 {
		t.Errorf("day 2 awards = %+v, want new day, reset jar", aw)
	}
	for i := range 29 {
		fastAnswer(t, e.svc, kid, 200+i)
	}
	credits := e.cred.Credits()
	if len(credits) != 6 {
		t.Fatalf("total credits = %d, want 6 across two days", len(credits))
	}
	ids := map[string]bool{}
	for _, c := range credits {
		ids[c.AwardID] = true
	}
	for _, want := range []string{
		"k1:jar:2026-10-26:1", "k1:jar:2026-10-26:2", "k1:jar:2026-10-26:3",
		"k1:jar:2026-10-27:1", "k1:jar:2026-10-27:2", "k1:jar:2026-10-27:3",
	} {
		if !ids[want] {
			t.Errorf("missing award id %s in %v", want, ids)
		}
	}
}

// TestRecordsKeepImprovingAfterCap is the PLAN gate: after the cap, personal
// bests keep improving even though no minutes are earned.
func TestRecordsKeepImprovingAfterCap(t *testing.T) {
	e := newEnv(t, 13)
	kid := "k1"
	for i := range 30 {
		fastAnswer(t, e.svc, kid, i)
	}
	// Slower record first, then a faster one after the cap is reached.
	answer(t, e.svc, kid, ModeAdd, 0, 1, "+", 1, 5000)
	out := answer(t, e.svc, kid, ModeAdd, 0, 1, "+", 1, 3000)
	if !out.Record {
		t.Fatalf("faster answer after cap should record")
	}
	if out.Coin == nil || out.Coin.Kind != "capped" {
		t.Fatalf("coin after cap = %+v, want capped", out.Coin)
	}
	facts, _ := e.svc.FactsFor(kid, ModeAdd)
	// The 3000 ms answer was to 0+1 (key "0+1", min first).
	if f := facts["0+1"]; f.BestMS == nil || *f.BestMS != 3000 {
		t.Errorf("best = %v, want 3000", f.BestMS)
	}
	if len(e.cred.Credits()) != 3 {
		t.Errorf("credits changed after cap: %d", len(e.cred.Credits()))
	}
}

// TestCrediterIdempotentReplay is the PLAN gate for retries: two fresh
// services replaying the same banking sequence derive the same award id, so
// the shared FakeCrediter records the credit once.
func TestCrediterIdempotentReplay(t *testing.T) {
	cred := &core.FakeCrediter{}
	bank := func() {
		loc := time.FixedZone("test", 0)
		st, err := store.Open(filepath.Join(t.TempDir(), "p.sqlite"))
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		defer st.Close()
		if err := st.Migrate(Migrations()...); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
		svc := New(st.DB(), cred, Options{
			Clock:    func() time.Time { return time.Date(2026, 10, 26, 12, 0, 0, 0, loc) },
			Location: loc,
			RNG:      rand.New(rand.NewPCG(1, 2)),
		})
		for i := range 10 {
			if _, err := svc.RecordAnswer(ctx(), "k1", ModeAdd, i, 1, "+", i+1, 5000); err != nil {
				t.Fatalf("RecordAnswer: %v", err)
			}
		}
	}
	bank()
	bank()
	if got := len(cred.Credits()); got != 1 {
		t.Errorf("credits after replay = %d, want 1", got)
	}
	if got := cred.Attempts(); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
}

// TestCrediterErrorAppliesNothing: when the crediter fails, nothing is
// applied, so retrying the same answer replays the same state and the same
// award id.
func TestCrediterErrorAppliesNothing(t *testing.T) {
	e := newEnv(t, 14)
	kid := "k1"
	// Jar at 9 coins, so the next fast record banks a minute and the credit
	// path actually runs — its failure must apply nothing.
	if err := e.svc.ImportAwards(ctx(), core.KidID(kid), core.Awards{Day: "2026-10-26", Earned: 0, Coins: 9}); err != nil {
		t.Fatalf("ImportAwards: %v", err)
	}
	e.cred.Err = errors.New("budget down")
	if _, err := e.svc.RecordAnswer(ctx(), kid, ModeAdd, 1, 1, "+", 2, 5000); err == nil {
		t.Fatalf("answer succeeded despite credit failure")
	}
	facts, _ := e.svc.FactsFor(kid, ModeAdd)
	if len(facts) != 0 {
		t.Fatalf("fact recorded despite credit failure: %+v", facts)
	}
	aw, _ := e.svc.Awards(kid)
	if aw.Coins != 9 || aw.Earned != 0 {
		t.Errorf("awards mutated despite credit failure: %+v", aw)
	}

	e.cred.Err = nil
	out, err := e.svc.RecordAnswer(ctx(), kid, ModeAdd, 1, 1, "+", 2, 5000)
	if err != nil || !out.Record {
		t.Fatalf("retry after recovery: %+v, %v", out, err)
	}
	if out.Coin == nil || out.Coin.Kind != "jar" || !out.Coin.Banked {
		t.Fatalf("retry coin = %+v, want a banked minute", out.Coin)
	}
	facts, _ = e.svc.FactsFor(kid, ModeAdd)
	if f := facts["1+1"]; f.Asked != 1 {
		t.Errorf("retry double-applied: %+v", f)
	}
	if got := len(e.cred.Credits()); got != 1 {
		t.Errorf("credits = %d, want 1", got)
	}
}

// TestCoinsKeptWhenMinuteDoesNotFit: when a banked minute would overshoot
// the cap, the coins stay in the jar instead of vanishing (the legacy app
// deducted them and lost them; the cap rule itself is unchanged).
func TestCoinsKeptWhenMinuteDoesNotFit(t *testing.T) {
	e := newEnv(t, 15)
	kid := "k1"
	if err := e.svc.ImportAwards(ctx(), core.KidID(kid), core.Awards{Day: "2026-10-26", Earned: 2.5, Coins: 9}); err != nil {
		t.Fatalf("ImportAwards: %v", err)
	}
	out := answer(t, e.svc, kid, ModeAdd, 1, 1, "+", 2, 5000)
	if out.Coin == nil || out.Coin.Kind != "coin" || out.Coin.Coins != 10 {
		t.Fatalf("coin = %+v, want 10 coins held", out.Coin)
	}
	if got := len(e.cred.Credits()); got != 0 {
		t.Errorf("credits = %d, want 0 (cap does not fit a full minute)", got)
	}
	aw, _ := e.svc.Awards(kid)
	if aw.Coins != 10 || aw.Earned != 2.5 {
		t.Errorf("awards = %+v, want coins kept at 10", aw)
	}
}

// TestPromoOnceEver masters every pool past the promo bar and pays the
// once-ever +2 bonus exactly once.
func TestPromoOnceEver(t *testing.T) {
	e := newEnv(t, 16)
	kid := "k1"
	seedToMastered(t, e.svc, kid, ModeAdd, 108) // 108/120 = 0.9
	seedToMastered(t, e.svc, kid, ModeSub, 189) // 189/210 = 0.9
	seedToMastered(t, e.svc, kid, ModeMul, 27)  // 27/30 = 0.9
	seedToMastered(t, e.svc, kid, ModeDiv, 30)  // 30/33 = 0.909
	r, err := e.svc.FinishRound(ctx(), kid, ModeAdd, false, mkRound(10, 0, 1000))
	if err != nil {
		t.Fatalf("FinishRound: %v", err)
	}
	found := false
	for _, a := range r.Awards {
		if a.Kind == "promo" && a.OK {
			found = true
		}
	}
	if !found {
		t.Fatalf("no promo award in %+v", r.Awards)
	}
	if !r.PromoDone {
		t.Errorf("PromoDone should be true")
	}
	credits := e.cred.Credits()
	if len(credits) != 1 || credits[0].AwardID != "k1:promo:2026-10-26:1" || credits[0].Minutes != 2 {
		t.Fatalf("promo credits = %+v", credits)
	}

	// A second round does not pay the promo again.
	r, err = e.svc.FinishRound(ctx(), kid, ModeAdd, false, mkRound(10, 0, 1000))
	if err != nil {
		t.Fatalf("FinishRound: %v", err)
	}
	for _, a := range r.Awards {
		if a.Kind == "promo" && a.OK {
			t.Fatalf("promo paid twice: %+v", r.Awards)
		}
	}
	if got := len(e.cred.Credits()); got != 1 {
		t.Errorf("credits = %d, want still 1", got)
	}
}

// TestPromoBlockedByCap: the once-ever promo cannot pay when a full bonus no
// longer fits under the daily cap (2 + 2.5 > 3).
func TestPromoBlockedByCap(t *testing.T) {
	e := newEnv(t, 17)
	kid := "k1"
	if err := e.svc.ImportAwards(ctx(), core.KidID(kid), core.Awards{Day: "2026-10-26", Earned: 2.5, Coins: 0}); err != nil {
		t.Fatalf("ImportAwards: %v", err)
	}
	seedToMastered(t, e.svc, kid, ModeAdd, 108)
	seedToMastered(t, e.svc, kid, ModeSub, 189)
	seedToMastered(t, e.svc, kid, ModeMul, 27)
	seedToMastered(t, e.svc, kid, ModeDiv, 30)
	r, err := e.svc.FinishRound(ctx(), kid, ModeAdd, false, mkRound(10, 0, 1000))
	if err != nil {
		t.Fatalf("FinishRound: %v", err)
	}
	blocked := false
	for _, a := range r.Awards {
		if a.Kind == "promo" && !a.OK && a.Text == "daily cap reached" {
			blocked = true
		}
	}
	if !blocked {
		t.Fatalf("promo should be blocked by the daily cap: %+v", r.Awards)
	}
	if got := len(e.cred.Credits()); got != 0 {
		t.Errorf("credits = %d, want 0 (cap blocked the promo)", got)
	}
	if done, _ := e.svc.PromoBonusDone(kid); done {
		t.Errorf("promo flag set despite the cap blocking the credit")
	}
}

func TestCounterSetting(t *testing.T) {
	e := newEnv(t, 18)
	if c, err := e.svc.Counter(); err != nil || c != "video" {
		t.Fatalf("default counter = %q, %v", c, err)
	}
	if err := e.svc.SetCounter(ctx(), "game"); err != nil {
		t.Fatalf("SetCounter: %v", err)
	}
	for i := range 10 {
		fastAnswer(t, e.svc, "k1", i)
	}
	credits := e.cred.Credits()
	if len(credits) != 1 {
		t.Fatalf("credits = %d, want 1", len(credits))
	}
	if credits[0].Counter != "game" {
		t.Fatalf("credit counter = %q, want game", credits[0].Counter)
	}
}
