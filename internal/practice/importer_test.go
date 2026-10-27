package practice

import (
	"testing"
	"time"

	"github.com/edementhon/kids-shelf/internal/core"
)

func TestImportRoundTrip(t *testing.T) {
	e := newEnv(t, 40)
	kid := "k1"
	at := time.Date(2026, 10, 26, 10, 30, 0, 0, time.UTC)

	// Facts, exactly as migrate would feed them.
	if err := e.svc.ImportFactStats(ctx(), core.KidID(kid), ModeAdd, "2+9", core.FactStats{
		Asked: 40, Correct: 36, Times: []int{2400, 2500, 2600},
		MedianMS: new(float64(2500)), BestMS: new(float64(2100)),
	}); err != nil {
		t.Fatalf("ImportFactStats: %v", err)
	}
	if err := e.svc.ImportFactStats(ctx(), core.KidID(kid), ModeMul, "3x4", core.FactStats{
		Asked: 12, Correct: 11, Times: []int{2000}, MedianMS: new(float64(2000)), BestMS: new(float64(1900)),
	}); err != nil {
		t.Fatalf("ImportFactStats: %v", err)
	}
	facts, err := e.svc.FactsFor(kid, ModeAdd)
	if err != nil {
		t.Fatalf("FactsFor: %v", err)
	}
	f := facts["2+9"]
	if f.Asked != 40 || f.Correct != 36 || len(f.Times) != 3 || f.Times[1] != 2500 {
		t.Errorf("fact stats = %+v", f)
	}
	if f.MedianMS == nil || *f.MedianMS != 2500 || f.BestMS == nil || *f.BestMS != 2100 {
		t.Errorf("fact medians = %+v", f)
	}
	mulFacts, _ := e.svc.FactsFor(kid, ModeMul)
	if _, ok := mulFacts["3x4"]; !ok {
		t.Errorf("mul fact missing: %v", mulFacts)
	}

	// Sessions (one round, one check-up).
	if err := e.svc.ImportSessions(ctx(), core.KidID(kid), core.Session{
		At: at, Mode: ModeAdd, Checkup: false, N: 12, Correct: 10, MedianMS: new(float64(1800)),
	}); err != nil {
		t.Fatalf("ImportSessions: %v", err)
	}
	if err := e.svc.ImportSessions(ctx(), core.KidID(kid), core.Session{
		At: at.Add(time.Hour), Mode: ModeMul, Checkup: true, N: 8, Correct: 8,
	}); err != nil {
		t.Fatalf("ImportSessions: %v", err)
	}
	sessions, err := e.svc.Sessions(kid)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(sessions))
	}
	s0 := sessions[0]
	if !s0.At.Equal(at) || s0.Mode != ModeAdd || s0.Checkup || s0.N != 12 || s0.Correct != 10 {
		t.Errorf("session 0 = %+v", s0)
	}
	if s0.MedianMS == nil || *s0.MedianMS != 1800 {
		t.Errorf("session 0 median = %v", s0.MedianMS)
	}
	if !sessions[1].Checkup || sessions[1].MedianMS != nil {
		t.Errorf("session 1 = %+v", sessions[1])
	}

	// Math awards: the daily cap resumes where the legacy app left it.
	if err := e.svc.ImportAwards(ctx(), core.KidID(kid), core.Awards{Day: "2026-10-26", Earned: 2.5, Coins: 4}); err != nil {
		t.Fatalf("ImportAwards: %v", err)
	}
	aw, err := e.svc.Awards(kid)
	if err != nil {
		t.Fatalf("Awards: %v", err)
	}
	if aw.Day != "2026-10-26" || aw.Earned != 2.5 || aw.Coins != 4 {
		t.Errorf("awards = %+v", aw)
	}

	// Flags: legacy-named keys normalize to the stable KidID so the
	// once-ever promo and best-round bars still hold.
	if err := e.svc.ImportFlags(ctx(), core.KidID(kid), core.Flag{Key: "promo_bonus:alex", Value: true}); err != nil {
		t.Fatalf("ImportFlags: %v", err)
	}
	if err := e.svc.ImportFlags(ctx(), core.KidID(kid), core.Flag{Key: "best:alex:add", Value: float64(1234.5)}); err != nil {
		t.Fatalf("ImportFlags: %v", err)
	}
	if done, _ := e.svc.PromoBonusDone(kid); !done {
		t.Errorf("promo flag not normalized to the kid id")
	}
	best, err := e.svc.BestRound(kid, ModeAdd)
	if err != nil || best == nil || *best != 1234.5 {
		t.Errorf("best round = %v, %v", best, err)
	}

	// Award log keeps the legacy kid name and kind.
	if err := e.svc.ImportAwardLog(ctx(), core.AwardLogEntry{At: at, Kid: "alex", Kind: "jar", Amount: 1}); err != nil {
		t.Fatalf("ImportAwardLog: %v", err)
	}
	logs, err := e.svc.AwardLog()
	if err != nil {
		t.Fatalf("AwardLog: %v", err)
	}
	if len(logs) != 1 || logs[0].Kid != "alex" || logs[0].Kind != "jar" || logs[0].Amount != 1 || logs[0].Day != "2026-10-26" {
		t.Errorf("award log = %+v", logs)
	}

	// Spelling: records, sessions and the spelling jar.
	if err := e.svc.ImportSpelling(ctx(), core.KidID(kid), core.Spelling{
		Records: []core.SpellingRecord{
			{Word: "cat", Hits: 3, Misses: 1, BestMS: 5000},
			{Word: "dog", Hits: 0, Misses: 2},
		},
		Sessions: []core.SpellingSession{{At: at, Word: "cat", Correct: true, Ms: 5000}},
		Awards:   core.Awards{Day: "2026-10-26", Earned: 1.5, Coins: 2},
	}); err != nil {
		t.Fatalf("ImportSpelling: %v", err)
	}
	recs, _ := e.svc.SpellingRecords(kid)
	if len(recs) != 2 {
		t.Fatalf("spell records = %+v", recs)
	}
	if recs[0].Word != "cat" || recs[0].Hits != 3 || recs[0].Misses != 1 || recs[0].BestMS != 5000 {
		t.Errorf("spell record = %+v", recs[0])
	}
	spSess, _ := e.svc.SpellingSessions(kid)
	if len(spSess) != 1 || !spSess[0].Correct || spSess[0].Word != "cat" || spSess[0].Ms != 5000 {
		t.Errorf("spell sessions = %+v", spSess)
	}
	sa, _ := e.svc.SpellAwards(kid)
	if sa.Day != "2026-10-26" || sa.Earned != 1.5 || sa.Coins != 2 {
		t.Errorf("spell awards = %+v", sa)
	}

	// Boss progress: mid-arena state with a pending fight.
	pending := "golem"
	if err := e.svc.ImportBosses(ctx(), core.KidID(kid), core.BossState{
		Defeated:   []string{"adder", "minustaur"},
		HP:         map[string]int{"golem": 5},
		PHP:        map[string]int{"alienfish": 2},
		PendingBid: &pending,
	}); err != nil {
		t.Fatalf("ImportBosses: %v", err)
	}
	bs, _ := e.svc.BossState(kid)
	if len(bs.Defeated) != 2 || bs.Defeated[0] != "adder" || bs.Defeated[1] != "minustaur" {
		t.Errorf("boss defeated = %v", bs.Defeated)
	}
	if bs.HP["golem"] != 5 || bs.PHP["alienfish"] != 2 || bs.PendingBid == nil || *bs.PendingBid != "golem" {
		t.Errorf("boss fight state = %+v", bs)
	}
	bf, _ := e.svc.BossStatus(kid)
	if bf.Boss == nil || bf.Boss.ID != "times3" {
		t.Errorf("current boss = %+v, want times3", bf.Boss)
	}

	// Config: global defaults plus per-kid overrides merge on read.
	if err := e.svc.ImportConfig(ctx(), core.Config{
		Global: map[string]float64{"round_goal": 5, "award_daily_cap": 2},
		Kids:   map[string]map[string]float64{kid: {"coins_per_minute": 20}},
	}); err != nil {
		t.Fatalf("ImportConfig: %v", err)
	}
	cfg, _ := e.svc.ConfigFor(kid)
	if cfg.RoundGoal != 5 || cfg.AwardDailyCap != 2 || cfg.CoinsPerMinute != 20 {
		t.Errorf("config = %+v", cfg)
	}
	other, _ := e.svc.ConfigFor("stranger")
	if other.RoundGoal != 5 || other.AwardDailyCap != 2 || other.CoinsPerMinute != 10 {
		t.Errorf("stranger config = %+v", other)
	}
}

func TestImportUnlockDerivedFromRecords(t *testing.T) {
	e := newEnv(t, 41)
	kid := "k1"
	addPool, _ := e.svc.FactPool(kid, ModeAdd)
	subPool, _ := e.svc.FactPool(kid, ModeSub)
	all := make([]Fact, 0, len(addPool)+len(subPool))
	all = append(all, addPool...)
	all = append(all, subPool...)
	keys := make([]string, 99)
	for i, f := range all[:99] {
		keys[i] = f.Key()
	}
	seedFastBatch(t, e.svc, kid, ModeAdd, keys)
	open, _, err := e.svc.OpenModes(kid)
	if err != nil {
		t.Fatalf("OpenModes: %v", err)
	}
	if !open[ModeMul] {
		t.Errorf("mul should unlock from 99 imported fast records (99/330 = 0.3)")
	}
	if open[ModeDiv] {
		t.Errorf("div should stay locked (no mul records yet)")
	}
}

func TestImportResumesDailyCap(t *testing.T) {
	e := newEnv(t, 42)
	kid := "k1"
	if err := e.svc.ImportAwards(ctx(), core.KidID(kid), core.Awards{Day: "2026-10-26", Earned: 3.0, Coins: 0}); err != nil {
		t.Fatalf("ImportAwards: %v", err)
	}
	// A fast record after an at-cap import is recorded but pays nothing.
	out := answer(t, e.svc, kid, ModeAdd, 5, 1, "+", 6, 5000)
	if !out.Record || out.Coin == nil || out.Coin.Kind != "capped" {
		t.Fatalf("answer at imported cap = %+v", out)
	}
	if len(e.cred.Credits()) != 0 {
		t.Errorf("credits after imported cap = %d", len(e.cred.Credits()))
	}

	// An import from an old day resets on first use (the cap resumes only
	// on the same local day).
	if err := e.svc.ImportAwards(ctx(), core.KidID("k2"), core.Awards{Day: "2026-10-01", Earned: 3.0, Coins: 8}); err != nil {
		t.Fatalf("ImportAwards: %v", err)
	}
	aw, _ := e.svc.Awards("k2")
	if aw.Day != "2026-10-26" || aw.Earned != 0 || aw.Coins != 0 {
		t.Errorf("stale imported awards = %+v, want reset", aw)
	}
}

// TestImportIdempotentReplay: replaying the same import must not duplicate
// history. Sessions, award-log entries and spelling sessions carry a durable
// identity derived from their content, so the second pass inserts nothing.
func TestImportIdempotentReplay(t *testing.T) {
	e := newEnv(t, 43)
	kid := "k1"
	at := time.Date(2026, 10, 26, 10, 30, 0, 0, time.UTC)
	sess := core.Session{At: at, Mode: ModeAdd, Checkup: false, N: 12, Correct: 10, MedianMS: new(float64(1800))}
	entry := core.AwardLogEntry{At: at, Kid: kid, Kind: "jar", Amount: 1}
	sp := core.Spelling{
		Records:  []core.SpellingRecord{{Word: "cat", Hits: 3, Misses: 1, BestMS: 5000}},
		Sessions: []core.SpellingSession{{At: at, Word: "cat", Correct: true, Ms: 5000}},
		Awards:   core.Awards{Day: "2026-10-26", Earned: 1.5, Coins: 2},
	}
	for range 2 {
		if err := e.svc.ImportSessions(ctx(), core.KidID(kid), sess); err != nil {
			t.Fatalf("ImportSessions: %v", err)
		}
		if err := e.svc.ImportAwardLog(ctx(), entry); err != nil {
			t.Fatalf("ImportAwardLog: %v", err)
		}
		if err := e.svc.ImportSpelling(ctx(), core.KidID(kid), sp); err != nil {
			t.Fatalf("ImportSpelling: %v", err)
		}
	}
	if sessions, _ := e.svc.Sessions(kid); len(sessions) != 1 {
		t.Errorf("sessions = %d, want 1 after replay", len(sessions))
	}
	if logs, _ := e.svc.AwardLog(); len(logs) != 1 {
		t.Errorf("award log = %d, want 1 after replay", len(logs))
	}
	if spSess, _ := e.svc.SpellingSessions(kid); len(spSess) != 1 {
		t.Errorf("spell sessions = %d, want 1 after replay", len(spSess))
	}
	recs, _ := e.svc.SpellingRecords(kid)
	if len(recs) != 1 || recs[0].Hits != 3 {
		t.Errorf("spell records = %+v, want the single imported record", recs)
	}
	if sa, _ := e.svc.SpellAwards(kid); sa.Earned != 1.5 || sa.Coins != 2 {
		t.Errorf("spell awards = %+v, want 1.5/2", sa)
	}
}

// TestImportKidProfileCarriesTierAndAccess: a legacy kid that started at the
// 1-10 tables with multiplication open (sam/jo in the legacy KIDS
// table) keeps the tier and the base-mode access after import instead of
// being reset to the 1-3 ladder and re-gated by records.
func TestImportKidProfileCarriesTierAndAccess(t *testing.T) {
	e := newEnv(t, 44)
	kid := "k1"
	if err := e.svc.ImportConfig(ctx(), core.Config{
		Kids: map[string]map[string]float64{kid: {"tables_top": 10, "base_mul": 1}},
	}); err != nil {
		t.Fatalf("ImportConfig: %v", err)
	}
	if top, err := e.svc.TablesTop(kid); err != nil || top != 10 {
		t.Fatalf("tables top = %d, %v; want 10", top, err)
	}
	if mul, err := e.svc.FactPool(kid, ModeMul); err != nil || len(mul) != 65 {
		t.Fatalf("mul pool = %d, %v; want the 1-10 ladder of 65", len(mul), err)
	}
	open, gates, err := e.svc.OpenModes(kid)
	if err != nil {
		t.Fatalf("OpenModes: %v", err)
	}
	if !open[ModeMul] || gates[ModeMul].Locked {
		t.Errorf("mul should be open as a base mode: open=%v gate=%+v", open[ModeMul], gates[ModeMul])
	}
	if open[ModeDiv] {
		t.Errorf("div should stay gated by mul records")
	}
	if _, err := e.svc.SampleQuestions(kid, ModeMul, 5); err != nil {
		t.Fatalf("SampleQuestions(mul) for a base-mode kid: %v", err)
	}
	// The special profile keys never leak into numeric config.
	if cfg, _ := e.svc.ConfigFor(kid); cfg.UnlockFrac != 0.3 {
		t.Errorf("config polluted by profile keys: %+v", cfg)
	}

	// An untouched kid still starts at the 1-3 ladder with mul locked.
	if mul3, _ := e.svc.FactPool("k2", ModeMul); len(mul3) != 30 {
		t.Errorf("plain kid mul pool = %d, want 30", len(mul3))
	}
	open2, _, _ := e.svc.OpenModes("k2")
	if open2[ModeMul] {
		t.Errorf("plain kid mul should stay locked")
	}
}
