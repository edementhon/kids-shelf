package practice

import (
	"errors"
	"testing"
)

// opMode maps a pool fact's operator symbol back to its storage mode.
func opMode(op string) string {
	switch op {
	case "-":
		return ModeSub
	case "x":
		return ModeMul
	case "/":
		return ModeDiv
	default:
		return ModeAdd
	}
}

func TestFactPoolCounts(t *testing.T) {
	e := newEnv(t, 1)
	kid := "k1"

	add, err := e.svc.FactPool(kid, ModeAdd)
	if err != nil {
		t.Fatalf("FactPool(add): %v", err)
	}
	sub, err := e.svc.FactPool(kid, ModeSub)
	if err != nil {
		t.Fatalf("FactPool(sub): %v", err)
	}
	mul, err := e.svc.FactPool(kid, ModeMul)
	if err != nil {
		t.Fatalf("FactPool(mul): %v", err)
	}
	div, err := e.svc.FactPool(kid, ModeDiv)
	if err != nil {
		t.Fatalf("FactPool(div): %v", err)
	}
	// Legacy pool sizes at the base 1-3 ladder: 120 add, 210 sub, 30 mul,
	// 33 div (div only exists once unlocked, but the pool is well-defined).
	if len(add) != 120 || len(sub) != 210 || len(mul) != 30 || len(div) != 33 {
		t.Errorf("base pool sizes = %d/%d/%d/%d, want 120/210/30/33", len(add), len(sub), len(mul), len(div))
	}

	// Widen the ladder to 1-5 and 1-10; each step grows mul and div pools.
	if err := saveTablesTop(e.svc.db, kid, 5); err != nil {
		t.Fatalf("saveTablesTop: %v", err)
	}
	mul5, _ := e.svc.FactPool(kid, ModeMul)
	div5, _ := e.svc.FactPool(kid, ModeDiv)
	if len(mul5) != 45 || len(div5) != 55 {
		t.Errorf("1-5 pool sizes = %d/%d, want 45/55", len(mul5), len(div5))
	}
	if err := saveTablesTop(e.svc.db, kid, 10); err != nil {
		t.Fatalf("saveTablesTop: %v", err)
	}
	mul10, _ := e.svc.FactPool(kid, ModeMul)
	div10, _ := e.svc.FactPool(kid, ModeDiv)
	if len(mul10) != 65 || len(div10) != 110 {
		t.Errorf("1-10 pool sizes = %d/%d, want 65/110", len(mul10), len(div10))
	}

	// Commutative keys: 9+2 and 2x9 collapse to min-first; sub/div ordered.
	if FactKeyFor(9, 2, "+") != "2+9" {
		t.Errorf("FactKeyFor(9,2,+) = %q", FactKeyFor(9, 2, "+"))
	}
	if FactKeyFor(2, 9, "x") != "2x9" {
		t.Errorf("FactKeyFor(2,9,x) = %q", FactKeyFor(2, 9, "x"))
	}
	if FactKeyFor(10, 3, "-") != "10-3" {
		t.Errorf("FactKeyFor(10,3,-) = %q", FactKeyFor(10, 3, "-"))
	}
	if FactKeyFor(45, 9, "/") != "45/9" {
		t.Errorf("FactKeyFor(45,9,/) = %q", FactKeyFor(45, 9, "/"))
	}
	f, ok := ParseKey("2+9")
	if !ok || f != (Fact{A: 2, B: 9, Op: "+"}) {
		t.Errorf("ParseKey(2+9) = %+v, %v", f, ok)
	}
}

func TestFactWeight(t *testing.T) {
	e := newEnv(t, 2)
	cfg, err := e.svc.ConfigFor("k1")
	if err != nil {
		t.Fatalf("ConfigFor: %v", err)
	}
	if w := factWeight(nil, cfg); w != 3.0 {
		t.Errorf("unseen weight = %v, want 3.0", w)
	}
	// Fresh fact: mastered speed but boosted by the asked<3 rule.
	fresh := &FactStats{Asked: 1, Correct: 1, Times: []int{3000}, MedianMS: new(float64(3000)), BestMS: new(float64(3000))}
	if w := factWeight(fresh, cfg); w != 0.25*1.5 {
		t.Errorf("fresh weight = %v, want 0.375", w)
	}
	// Weak fact (acc 0.3, asked >= 3): 2.5, no boost.
	weak := &FactStats{Asked: 10, Correct: 3, Times: []int{5000, 5000, 5000}, MedianMS: new(float64(5000)), BestMS: new(float64(5000))}
	if w := factWeight(weak, cfg); w != 2.5 {
		t.Errorf("weak weight = %v, want 2.5", w)
	}
	// Mid accuracy fact: 1.0.
	mid := &FactStats{Asked: 10, Correct: 8, Times: []int{5000}}
	if w := factWeight(mid, cfg); w != 1.0 {
		t.Errorf("mid weight = %v, want 1.0", w)
	}
	// Rare weak fact: 2.5 * 1.5.
	rare := &FactStats{Asked: 2, Correct: 1, Times: []int{5000}}
	if w := factWeight(rare, cfg); w != 2.5*1.5 {
		t.Errorf("rare weight = %v, want 3.75", w)
	}
	// Mastered fact (asked >= 3): 0.25, no asked boost.
	mas := &FactStats{Asked: 10, Correct: 9, Times: []int{3000, 3000, 3000}, MedianMS: new(float64(3000)), BestMS: new(float64(3000))}
	if w := factWeight(mas, cfg); w != 0.25 {
		t.Errorf("mastered weight = %v, want 0.25", w)
	}
}

// TestSampleFavorsWeakFacts is the statistical gate for weighted sampling:
// with a seeded RNG, 20,000 draws must overwhelmingly favor unseen and
// low-accuracy facts over mastered ones. It draws through the same
// factWeight + weightedChoice path SampleQuestions uses, so the gate stays
// deterministic and cheap; SampleQuestions itself is smoke-tested below for
// round shape and distinctness.
func TestSampleFavorsWeakFacts(t *testing.T) {
	e := newEnv(t, 99)
	kid := "k1"
	pool, err := e.svc.FactPool(kid, ModeAdd)
	if err != nil {
		t.Fatalf("FactPool: %v", err)
	}
	class := make(map[string]string, len(pool))
	for _, f := range pool[:60] {
		class[f.Key()] = "mastered"
		times := make([]int, 10)
		for i := range times {
			times[i] = 3000
		}
		seedFact(t, e.svc, kid, ModeAdd, f.Key(), 10, 9, times, new(float64(3000)))
	}
	for _, f := range pool[60:90] {
		class[f.Key()] = "weak"
		seedFact(t, e.svc, kid, ModeAdd, f.Key(), 10, 3, []int{5000, 5000, 5000}, new(float64(5000)))
	}
	for _, f := range pool[90:] {
		class[f.Key()] = "unseen"
	}
	cfg, err := e.svc.ConfigFor(kid)
	if err != nil {
		t.Fatalf("ConfigFor: %v", err)
	}
	facts, err := e.svc.factsMap(kid, ModeAdd)
	if err != nil {
		t.Fatalf("factsMap: %v", err)
	}
	weights := make([]float64, len(pool))
	for i, f := range pool {
		if fs, ok := facts[f.Key()]; ok {
			weights[i] = factWeight(&fs, cfg)
		} else {
			weights[i] = factWeight(nil, cfg)
		}
	}

	counts := map[string]int{}
	e.svc.mu.Lock()
	for range 20000 {
		counts[class[pool[weightedChoice(e.svc.rng, weights)].Key()]]++
	}
	e.svc.mu.Unlock()
	total := counts["mastered"] + counts["weak"] + counts["unseen"]
	if total != 20000 {
		t.Fatalf("classified %d draws, want 20000", total)
	}
	// Expected shares: unseen 52.2%, weak 43.5%, mastered 4.3%. Assert with
	// wide margins so the gate only trips on a real regression.
	if counts["unseen"] < 4*counts["mastered"] {
		t.Errorf("unseen %d not favored over mastered %d", counts["unseen"], counts["mastered"])
	}
	if counts["weak"] < 3*counts["mastered"] {
		t.Errorf("weak %d not favored over mastered %d", counts["weak"], counts["mastered"])
	}
	if counts["mastered"]*10 >= total {
		t.Errorf("mastered facts drawn too often: %d/%d", counts["mastered"], total)
	}

	// Integration smoke: SampleQuestions returns 5 distinct questions.
	for range 10 {
		qs, err := e.svc.SampleQuestions(kid, ModeAdd, 5)
		if err != nil {
			t.Fatalf("SampleQuestions: %v", err)
		}
		if len(qs) != 5 {
			t.Fatalf("round returned %d questions, want 5", len(qs))
		}
		seen := map[string]bool{}
		for _, q := range qs {
			if seen[q.Key()] {
				t.Fatalf("round repeated %s", q.Key())
			}
			seen[q.Key()] = true
		}
	}
}

// TestUnlockBoundaries covers the mul and div gates at the 30% boundary:
// exactly 99/330 fast add+sub records unlock mul (98 lock it), and exactly
// 9/30 fast mul records unlock div (8 lock it).
func TestUnlockBoundaries(t *testing.T) {
	e := newEnv(t, 3)
	kid := "k1"
	addPool, _ := e.svc.FactPool(kid, ModeAdd)
	subPool, _ := e.svc.FactPool(kid, ModeSub)
	all := append(append([]Fact{}, addPool...), subPool...)
	if len(all) != 330 {
		t.Fatalf("add+sub pool = %d, want 330", len(all))
	}

	open, gates, err := e.svc.OpenModes(kid)
	if err != nil {
		t.Fatalf("OpenModes: %v", err)
	}
	if !open[ModeAdd] || !open[ModeSub] {
		t.Errorf("base modes should be open")
	}

	// The first 98 pool facts are all add keys; seed them fast in one tx.
	keys := make([]string, 98)
	for i, f := range all[:98] {
		keys[i] = f.Key()
	}
	seedFastBatch(t, e.svc, kid, ModeAdd, keys)
	_, gates, _ = e.svc.OpenModes(kid)
	if !gates[ModeMul].Locked {
		t.Fatalf("mul unlocked at 98/330 (%.3f < 0.3)", gates[ModeMul].Have)
	}
	if gates[ModeMul].Have != 0.297 || gates[ModeMul].Need != 0.3 {
		t.Errorf("gate have/need = %v/%v, want 0.297/0.3", gates[ModeMul].Have, gates[ModeMul].Need)
	}

	seedFast(t, e.svc, kid, opMode(all[98].Op), all[98].Key())
	open, gates, _ = e.svc.OpenModes(kid)
	if gates[ModeMul].Locked || !open[ModeMul] {
		t.Fatalf("mul locked at 99/330 = 0.3: %+v", gates[ModeMul])
	}
	if gates[ModeMul].Have != 0.3 {
		t.Errorf("have = %v, want 0.3", gates[ModeMul].Have)
	}

	// div gate reads the CURRENT mul pool (30 facts at the 1-3 ladder).
	mulPool, _ := e.svc.FactPool(kid, ModeMul)
	if len(mulPool) != 30 {
		t.Fatalf("mul pool = %d, want 30", len(mulPool))
	}
	keys8 := make([]string, 8)
	for i, f := range mulPool[:8] {
		keys8[i] = f.Key()
	}
	seedFastBatch(t, e.svc, kid, ModeMul, keys8)
	_, gates, _ = e.svc.OpenModes(kid)
	if !gates[ModeDiv].Locked {
		t.Fatalf("div unlocked at 8/30 (%.3f < 0.3)", gates[ModeDiv].Have)
	}
	seedFast(t, e.svc, kid, opMode(mulPool[8].Op), mulPool[8].Key())
	open, gates, _ = e.svc.OpenModes(kid)
	if gates[ModeDiv].Locked || !open[ModeDiv] {
		t.Fatalf("div locked at 9/30: %+v", gates[ModeDiv])
	}
}

func TestMasteryAndFastRecordBoundaries(t *testing.T) {
	e := newEnv(t, 4)
	cfg, err := e.svc.ConfigFor("k1")
	if err != nil {
		t.Fatalf("ConfigFor: %v", err)
	}
	// Accuracy boundary: 8/10 is not mastered, 9/10 (exactly 0.9) is.
	f := &FactStats{Asked: 10, Correct: 8, Times: []int{3000, 3000}, MedianMS: new(float64(3000)), BestMS: new(float64(3000))}
	if isMastered(f, cfg) {
		t.Errorf("acc 0.8 should not be mastered")
	}
	f.Correct = 9
	if !isMastered(f, cfg) {
		t.Errorf("acc 0.9 exactly should be mastered")
	}
	// Speed boundary: median 6000 is fast enough, 6001 is not.
	g := &FactStats{Asked: 3, Correct: 3, Times: []int{6000, 6000, 6000}, MedianMS: new(float64(6000)), BestMS: new(float64(6000))}
	if !isMastered(g, cfg) {
		t.Errorf("median 6000 should be mastered")
	}
	g.MedianMS = new(float64(6001))
	if isMastered(g, cfg) {
		t.Errorf("median 6001 should not be mastered")
	}
	// Fast-record boundary: best 6000 counts, 6001 does not.
	if !hasFastRecord(g) {
		t.Errorf("best 6000 should count as fast")
	}
	g.BestMS = new(float64(6001))
	if hasFastRecord(g) {
		t.Errorf("best 6001 should not count as fast")
	}
	// A fact with no best time at all never counts.
	if hasFastRecord(&FactStats{Asked: 5, Correct: 5}) {
		t.Errorf("fact without best_ms should not be fast")
	}
}

// TestPromoteTablesLadder widens the times tables at the 80% boundary of
// each ladder step: 1-3 -> 1-5 -> 1-7 -> 1-10, then stops.
func TestPromoteTablesLadder(t *testing.T) {
	e := newEnv(t, 5)
	kid := "k1"
	round := func() (RoundSummary, error) {
		return e.svc.FinishRound(ctx(), kid, ModeMul, false, nil)
	}
	top := func() int {
		v, err := e.svc.TablesTop(kid)
		if err != nil {
			t.Fatalf("TablesTop: %v", err)
		}
		return v
	}
	hasTablesAward := func(r RoundSummary) bool {
		for _, a := range r.Awards {
			if a.Kind == "tables" && a.OK {
				return true
			}
		}
		return false
	}

	// 23/30 = 0.7667: no promotion.
	seedToMastered(t, e.svc, kid, ModeMul, 23)
	if r, err := round(); err != nil || hasTablesAward(r) || top() != 3 {
		t.Fatalf("23/30 should not promote (top=%d, err=%v)", top(), err)
	}
	// 24/30 = 0.8 exactly: promote to 1-5.
	seedToMastered(t, e.svc, kid, ModeMul, 24)
	r, err := round()
	if err != nil || !hasTablesAward(r) || top() != 5 {
		t.Fatalf("24/30 should promote to 1-5 (top=%d, err=%v)", top(), err)
	}
	// 35/45 = 0.7778: no; 36/45 = 0.8: promote to 1-7.
	seedToMastered(t, e.svc, kid, ModeMul, 35)
	if r, err := round(); err != nil || hasTablesAward(r) || top() != 5 {
		t.Fatalf("35/45 should not promote (top=%d, err=%v)", top(), err)
	}
	seedToMastered(t, e.svc, kid, ModeMul, 36)
	if r, err := round(); err != nil || !hasTablesAward(r) || top() != 7 {
		t.Fatalf("36/45 should promote to 1-7 (top=%d, err=%v)", top(), err)
	}
	// 44/56 = 0.7857: no; 45/56 = 0.8036: promote to 1-10.
	seedToMastered(t, e.svc, kid, ModeMul, 44)
	if r, err := round(); err != nil || hasTablesAward(r) || top() != 7 {
		t.Fatalf("44/56 should not promote (top=%d, err=%v)", top(), err)
	}
	seedToMastered(t, e.svc, kid, ModeMul, 45)
	if r, err := round(); err != nil || !hasTablesAward(r) || top() != 10 {
		t.Fatalf("45/56 should promote to 1-10 (top=%d, err=%v)", top(), err)
	}
	// 10 is the top of the ladder: mastering everything changes nothing.
	seedToMastered(t, e.svc, kid, ModeMul, 65)
	if r, err := round(); err != nil || hasTablesAward(r) || top() != 10 {
		t.Fatalf("full mastery at 1-10 should not promote further (top=%d, err=%v)", top(), err)
	}
}

func TestRecordAnswerUpdatesStats(t *testing.T) {
	e := newEnv(t, 6)
	kid, mode := "k1", ModeAdd

	out := answer(t, e.svc, kid, mode, 1, 1, "+", 2, 1500)
	if !out.Correct || !out.Record || out.Answer != 2 {
		t.Fatalf("first answer = %+v, want correct+record", out)
	}
	if out.Coin == nil || out.Coin.Kind != "coin" || out.Coin.Coins != 1 {
		t.Fatalf("first fast record coin = %+v, want 1 coin", out.Coin)
	}
	facts, err := e.svc.FactsFor(kid, mode)
	if err != nil {
		t.Fatalf("FactsFor: %v", err)
	}
	f := facts["1+1"]
	if f.Asked != 1 || f.Correct != 1 || f.BestMS == nil || *f.BestMS != 1500 {
		t.Errorf("facts after first answer = %+v", f)
	}
	if f.MedianMS == nil || *f.MedianMS != 1500 {
		t.Errorf("median = %v, want 1500", f.MedianMS)
	}

	// Wrong answers never record times or coins.
	out = answer(t, e.svc, kid, mode, 1, 1, "+", 3, 900)
	if out.Correct || out.Record || out.Coin != nil {
		t.Fatalf("wrong answer = %+v, want no record/coin", out)
	}
	facts, _ = e.svc.FactsFor(kid, mode)
	f = facts["1+1"]
	if f.Asked != 2 || f.Correct != 1 {
		t.Errorf("facts after wrong answer = %+v", f)
	}

	// Even-length median averages the middle two: add a 2000 ms correct
	// answer to [1500] and expect 1750. 2000 ms is slower than the 1500 ms
	// best, so it does not record.
	answer(t, e.svc, kid, mode, 1, 1, "+", 2, 2000)
	facts, _ = e.svc.FactsFor(kid, mode)
	if m := *facts["1+1"].MedianMS; m != 1750 {
		t.Errorf("median = %v, want 1750", m)
	}

	// The response window keeps the last 12 correct times (newest last), so
	// 13 answers keep 1100..2200.
	for i := range 13 {
		answer(t, e.svc, kid, mode, 2, 1, "+", 3, 1000+i*100)
	}
	facts, _ = e.svc.FactsFor(kid, mode)
	// Answers to 2+1 store under the commutative key 1+2, as legacy did.
	if len(facts["1+2"].Times) != 12 {
		t.Errorf("times window = %d, want 12", len(facts["1+2"].Times))
	}
	if facts["1+2"].Times[0] != 1100 || facts["1+2"].Times[11] != 2200 {
		t.Errorf("window trims oldest: %v", facts["1+2"].Times)
	}
}

// TestMilestoneBoundary evaluates the round milestone at its edges: 90%
// accuracy, >= 10 questions and a median beating the stored best by >= 2%.
func TestMilestoneBoundary(t *testing.T) {
	e := newEnv(t, 7)
	kid := "k1"
	best := func() *float64 {
		b, err := e.svc.BestRound(kid, ModeAdd)
		if err != nil {
			t.Fatalf("BestRound: %v", err)
		}
		return b
	}

	// First good round (9/10 at 1000 ms): no prior best, record set.
	r, err := e.svc.FinishRound(ctx(), kid, ModeAdd, false, mkRound(9, 1, 1000))
	if err != nil {
		t.Fatalf("FinishRound: %v", err)
	}
	if len(r.Awards) != 1 || r.Awards[0].Kind != "record" || !r.Awards[0].OK {
		t.Fatalf("awards = %+v, want a record milestone", r.Awards)
	}
	if b := best(); b == nil || *b != 1000 {
		t.Fatalf("best = %v, want 1000", b)
	}
	if r.MedianMS == nil || *r.MedianMS != 1000 || r.N != 10 || r.Correct != 9 {
		t.Errorf("summary = %+v", r)
	}

	// Just under 2% faster (981 ms vs best 1000) is NOT a new record.
	if _, err := e.svc.FinishRound(ctx(), kid, ModeAdd, false, mkRound(9, 1, 981)); err != nil {
		t.Fatalf("FinishRound: %v", err)
	}
	if b := best(); b == nil || *b != 1000 {
		t.Fatalf("best changed on a 981 ms round: %v", b)
	}

	// 2.1% faster records; the flag moves down to 979.
	r, err = e.svc.FinishRound(ctx(), kid, ModeAdd, false, mkRound(9, 1, 979))
	if err != nil {
		t.Fatalf("FinishRound: %v", err)
	}
	if b := best(); b == nil || *b != 979 {
		t.Fatalf("best = %v, want 979", b)
	}

	// Accuracy failure branch (8/10) is a miss, not a record.
	if _, err := e.svc.FinishRound(ctx(), kid, ModeAdd, false, mkRound(8, 2, 500)); err != nil {
		t.Fatalf("FinishRound: %v", err)
	}
	if b := best(); b == nil || *b != 979 {
		t.Fatalf("best changed on an 8/10 round: %v", b)
	}

	// Check-ups never set the milestone, even at 10 questions.
	r, err = e.svc.FinishRound(ctx(), kid, ModeAdd, true, mkRound(10, 0, 1000))
	if err != nil {
		t.Fatalf("FinishRound: %v", err)
	}
	for _, a := range r.Awards {
		if a.Kind == "record" {
			t.Fatalf("check-up produced a record award: %+v", r.Awards)
		}
	}
	if !r.Checkup {
		t.Errorf("checkup flag lost")
	}

	// Rounds shorter than round_min_questions never evaluate the milestone.
	r, err = e.svc.FinishRound(ctx(), kid, ModeAdd, false, mkRound(9, 0, 900))
	if err != nil {
		t.Fatalf("FinishRound: %v", err)
	}
	for _, a := range r.Awards {
		if a.Kind == "record" {
			t.Fatalf("9-question round produced a record award: %+v", r.Awards)
		}
	}

	// Locked modes reject answers and rounds.
	if _, err := e.svc.RecordAnswer(ctx(), kid, ModeMul, 1, 1, "x", 1, 5000); !errors.Is(err, ErrLocked) {
		t.Fatalf("locked mul answer err = %v, want ErrLocked", err)
	}
	if _, err := e.svc.SampleQuestions(kid, ModeMul, 5); !errors.Is(err, ErrLocked) {
		t.Fatalf("locked mul round err = %v, want ErrLocked", err)
	}
}
