package practice

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"time"
)

// FactStats is one fact's cumulative practice state, mirroring the legacy
// per-fact record: how often it was asked and answered correctly, the
// response-time window for correct answers (newest last; the last 12) and
// the median and best response times in milliseconds.
type FactStats struct {
	Asked    int
	Correct  int
	Times    []int // correct-answer response times in ms, newest last
	MedianMS *float64
	BestMS   *float64
}

// Fact identifies one fact in a pool. add and mul facts are commutative
// (min x max), sub and div are ordered, exactly as the legacy keys encoded
// them.
type Fact struct {
	A  int
	B  int
	Op string
}

// Key returns the storage key for the fact.
func (f Fact) Key() string { return FactKeyFor(f.A, f.B, f.Op) }

// Question is one sampled question; the answer is AnswerOf(A, B, Op).
type Question struct {
	A  int
	B  int
	Op string
}

// Key returns the unique storage key of the question, for deduplication
// within a round or boss fight.
func (q Question) Key() string { return FactKeyFor(q.A, q.B, q.Op) }

// FactKeyFor returns the storage key for a fact. add and mul keys are
// commutative (min first), sub and div ordered, matching the legacy
// fact_key_for.
func FactKeyFor(a, b int, op string) string {
	if op == "+" || op == "x" {
		return fmt.Sprintf("%d%s%d", min(a, b), op, max(a, b))
	}
	return fmt.Sprintf("%d%s%d", a, op, b)
}

// ParseKey splits a storage key back into a Fact.
func ParseKey(k string) (Fact, bool) {
	for _, sep := range []string{"+", "-", "/", "x"} {
		if i := strings.Index(k, sep); i > 0 {
			a, errA := strconv.Atoi(k[:i])
			b, errB := strconv.Atoi(k[i+1:])
			if errA == nil && errB == nil {
				return Fact{A: a, B: b, Op: sep}, true
			}
		}
	}
	return Fact{}, false
}

// AnswerOf computes the answer of a fact.
func AnswerOf(a, b int, op string) int {
	switch op {
	case "+":
		return a + b
	case "-":
		return a - b
	case "x":
		return a * b
	case "/":
		return a / b
	}
	return 0
}

func validMode(mode string) bool {
	return mode == ModeAdd || mode == ModeSub || mode == ModeMul || mode == ModeDiv
}

// median returns the median of times (averaging the middle two on even
// lengths, as the legacy median did), nil when there are none.
func median(times []int) *float64 {
	if len(times) == 0 {
		return nil
	}
	s := append([]int(nil), times...)
	sort.Ints(s)
	n := len(s)
	var m float64
	if n%2 == 1 {
		m = float64(s[n/2])
	} else {
		m = (float64(s[n/2-1]) + float64(s[n/2])) / 2
	}
	return &m
}

// ------------------------------------------------------------------ pools

// TablesTop returns the kid's current times-table ladder top (1-3, 1-5, 1-7
// or 1-10). New kids start at 3; promotion widens it (see FinishRound).
func (s *Service) TablesTop(kid string) (int, error) {
	var top int
	err := s.db.QueryRow(`SELECT top FROM practice_tables WHERE kid=?`, kid).Scan(&top)
	if errors.Is(err, sql.ErrNoRows) {
		return 3, nil
	}
	if err != nil {
		return 0, err
	}
	if top < 3 {
		return 3, nil
	}
	return top, nil
}

func saveTablesTop(ex execer, kid string, top int) error {
	_, err := ex.Exec(`INSERT INTO practice_tables (kid, top) VALUES (?, ?)
ON CONFLICT (kid) DO UPDATE SET top=excluded.top`, kid, top)
	return err
}

// FactPool returns the sorted keys of a kid's mode pool. add pools cover
// sums 1..20, sub covers ordered positive differences 1-0..20-19, mul is the
// kid's times-table ladder times 0..10 (commutative keys) and div is the
// inverse of the same ladder, asked as dividend/divisor. Counts match the
// legacy fact_pool exactly.
func (s *Service) FactPool(kid, mode string) ([]Fact, error) {
	top, err := s.TablesTop(kid)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var pool []Fact
	add := func(a, b int, op string) {
		k := FactKeyFor(a, b, op)
		if !seen[k] {
			seen[k] = true
			pool = append(pool, Fact{A: a, B: b, Op: op})
		}
	}
	switch mode {
	case ModeAdd:
		for a := range 21 {
			for b := range 21 {
				if sum := a + b; sum >= 1 && sum <= 20 {
					add(a, b, "+")
				}
			}
		}
	case ModeSub:
		for a := 1; a <= 20; a++ {
			for b := range a {
				add(a, b, "-")
			}
		}
	case ModeMul:
		for a := 1; a <= top; a++ {
			for b := range 11 {
				add(a, b, "x")
			}
		}
	case ModeDiv:
		for d := 1; d <= top; d++ {
			for q := range 11 {
				add(d*q, d, "/")
			}
		}
	default:
		return nil, fmt.Errorf("%w: %s", ErrBadMode, mode)
	}
	sort.Slice(pool, func(i, j int) bool { return pool[i].Key() < pool[j].Key() })
	return pool, nil
}

// ------------------------------------------------------------ fact storage

func (s *Service) loadFact(kid, mode, key string) (FactStats, bool, error) {
	var f FactStats
	var times string
	var med, best sql.NullFloat64
	err := s.db.QueryRow(
		`SELECT asked, correct, times, median_ms, best_ms FROM practice_facts WHERE kid=? AND mode=? AND key=?`,
		kid, mode, key).Scan(&f.Asked, &f.Correct, &times, &med, &best)
	if errors.Is(err, sql.ErrNoRows) {
		return f, false, nil
	}
	if err != nil {
		return f, false, err
	}
	if err := json.Unmarshal([]byte(times), &f.Times); err != nil {
		return f, false, err
	}
	if med.Valid {
		f.MedianMS = new(med.Float64)
	}
	if best.Valid {
		f.BestMS = new(best.Float64)
	}
	return f, true, nil
}

// factsMap loads all of a kid's facts for one mode as a keyed map.
func (s *Service) factsMap(kid, mode string) (map[string]FactStats, error) {
	rows, err := s.db.Query(
		`SELECT key, asked, correct, times, median_ms, best_ms FROM practice_facts WHERE kid=? AND mode=?`,
		kid, mode)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]FactStats)
	for rows.Next() {
		var key, times string
		var f FactStats
		var med, best sql.NullFloat64
		if err := rows.Scan(&key, &f.Asked, &f.Correct, &times, &med, &best); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(times), &f.Times); err != nil {
			return nil, err
		}
		if med.Valid {
			f.MedianMS = new(med.Float64)
		}
		if best.Valid {
			f.BestMS = new(best.Float64)
		}
		out[key] = f
	}
	return out, rows.Err()
}

// FactsFor returns all recorded stats for a kid's mode, keyed by fact key.
// The parent dashboard and migration tooling read this.
func (s *Service) FactsFor(kid, mode string) (map[string]FactStats, error) {
	return s.factsMap(kid, mode)
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func saveFact(ex execer, kid, mode, key string, f *FactStats) error {
	times := f.Times
	if times == nil {
		times = []int{}
	}
	b, err := json.Marshal(times)
	if err != nil {
		return err
	}
	var med, best any
	if f.MedianMS != nil {
		med = *f.MedianMS
	}
	if f.BestMS != nil {
		best = *f.BestMS
	}
	_, err = ex.Exec(`INSERT INTO practice_facts (kid, mode, key, asked, correct, times, median_ms, best_ms)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (kid, mode, key) DO UPDATE SET
    asked=excluded.asked, correct=excluded.correct, times=excluded.times,
    median_ms=excluded.median_ms, best_ms=excluded.best_ms`,
		kid, mode, key, f.Asked, f.Correct, string(b), med, best)
	return err
}

// --------------------------------------------------------------- sampling

// weightedChoice draws one index from weights (legacy random.choices).
func weightedChoice(rng *rand.Rand, weights []float64) int {
	total := 0.0
	for _, w := range weights {
		total += w
	}
	r := rng.Float64() * total
	for i, w := range weights {
		r -= w
		if r <= 0 {
			return i
		}
	}
	return len(weights) - 1
}

// factWeight mirrors the legacy sample_questions weight: a fact the kid has
// never seen outweighs everything, mastery shrinks the weight, low accuracy
// grows it and a fact asked fewer than three times gets a small boost.
func factWeight(f *FactStats, cfg Config) float64 {
	if f == nil {
		return 3.0
	}
	acc := float64(f.Correct) / float64(max(1, f.Asked))
	med := math.MaxFloat64
	if f.MedianMS != nil {
		med = *f.MedianMS
	}
	w := 1.0
	if acc >= cfg.MasteryAccuracy && med <= cfg.MasterySpeedMS {
		w *= 0.25
	} else if acc < 0.7 {
		w *= 2.5
	}
	if f.Asked < 3 {
		w *= 1.5
	}
	return w
}

// SampleQuestions draws n distinct questions weighted toward unmastered and
// rarely-seen facts, in random order, the legacy sample_questions verbatim.
// The mode must be open for the kid.
func (s *Service) SampleQuestions(kid, mode string, n int) ([]Question, error) {
	if !validMode(mode) {
		return nil, fmt.Errorf("%w: %s", ErrBadMode, mode)
	}
	open, _, err := s.OpenModes(kid)
	if err != nil {
		return nil, err
	}
	if !open[mode] {
		return nil, fmt.Errorf("%w: %s", ErrLocked, mode)
	}
	cfg, err := s.ConfigFor(kid)
	if err != nil {
		return nil, err
	}
	pool, err := s.FactPool(kid, mode)
	if err != nil {
		return nil, err
	}
	facts, err := s.factsMap(kid, mode)
	if err != nil {
		return nil, err
	}
	weights := make([]float64, len(pool))
	for i, f := range pool {
		if fs, ok := facts[f.Key()]; ok {
			weights[i] = factWeight(&fs, cfg)
		} else {
			weights[i] = factWeight(nil, cfg)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	picked := make([]Question, 0, n)
	seen := make(map[string]bool)
	for tries := 0; len(picked) < n && tries < n*30; tries++ {
		f := pool[weightedChoice(s.rng, weights)]
		if seen[f.Key()] {
			continue
		}
		seen[f.Key()] = true
		picked = append(picked, Question{A: f.A, B: f.B, Op: f.Op})
	}
	s.rng.Shuffle(len(picked), func(i, j int) {
		picked[i], picked[j] = picked[j], picked[i]
	})
	return picked, nil
}

// --------------------------------------------------------------- mastery

// isMastered reports whether a fact clears the mastery bar: accuracy at or
// above the bar and a median response at or under the speed bar.
func isMastered(f *FactStats, cfg Config) bool {
	if f == nil || f.Asked == 0 {
		return false
	}
	acc := float64(f.Correct) / float64(max(1, f.Asked))
	med := math.MaxFloat64
	if f.MedianMS != nil {
		med = *f.MedianMS
	}
	return acc >= cfg.MasteryAccuracy && med <= cfg.MasterySpeedMS
}

// hasFastRecord reports whether the fact has any personal-best time at or
// under the speed bar, softer than mastery: no accuracy requirement.
func hasFastRecord(f *FactStats) bool {
	return f != nil && f.BestMS != nil && *f.BestMS <= fastRecordMS
}

// Gate describes one locked mode and what unlocks it.
type Gate struct {
	Locked bool
	Have   float64
	Need   float64
	Label  string
}

// ModeUnlockStatus reports the unlock gates: mul needs fast records on 30%
// of the add+sub pool, div on 30% of the current mul pool, the legacy
// mode_unlock_status ladder.
func (s *Service) ModeUnlockStatus(kid string) (map[string]Gate, error) {
	cfg, err := s.ConfigFor(kid)
	if err != nil {
		return nil, err
	}
	return s.modeUnlockStatusLocked(kid, cfg)
}

func (s *Service) modeUnlockStatusLocked(kid string, cfg Config) (map[string]Gate, error) {
	addPool, err := s.FactPool(kid, ModeAdd)
	if err != nil {
		return nil, err
	}
	subPool, err := s.FactPool(kid, ModeSub)
	if err != nil {
		return nil, err
	}
	mulPool, err := s.FactPool(kid, ModeMul)
	if err != nil {
		return nil, err
	}
	ab, err := s.factsMap(kid, ModeAdd)
	if err != nil {
		return nil, err
	}
	subFacts, err := s.factsMap(kid, ModeSub)
	if err != nil {
		return nil, err
	}
	for k, f := range subFacts {
		ab[k] = f
	}
	mulFacts, err := s.factsMap(kid, ModeMul)
	if err != nil {
		return nil, err
	}

	abWithRecord := 0
	for _, p := range addPool {
		if f, ok := ab[p.Key()]; ok && hasFastRecord(&f) {
			abWithRecord++
		}
	}
	for _, p := range subPool {
		if f, ok := ab[p.Key()]; ok && hasFastRecord(&f) {
			abWithRecord++
		}
	}
	mWithRecord := 0
	for _, p := range mulPool {
		if f, ok := mulFacts[p.Key()]; ok && hasFastRecord(&f) {
			mWithRecord++
		}
	}

	// A legacy profile that started with multiplication as a base mode
	// ("base_mul" imported via ImportConfig) keeps mul open regardless of
	// records, exactly as legacy KIDS base_modes did.
	baseMul, err := s.flagExists("base_mode:" + kid + ":mul")
	if err != nil {
		return nil, err
	}

	abFrac := float64(abWithRecord) / float64(max(1, len(addPool)+len(subPool)))
	mFrac := float64(mWithRecord) / float64(max(1, len(mulPool)))
	return map[string]Gate{
		ModeMul: {
			Locked: !baseMul && abFrac < cfg.UnlockFrac,
			Have:   round3(abFrac),
			Need:   cfg.UnlockFrac,
			Label:  "Set fast records on 30% of ➕/➖ facts",
		},
		ModeDiv: {
			Locked: mFrac < cfg.UnlockFrac,
			Have:   round3(mFrac),
			Need:   cfg.UnlockFrac,
			Label:  "Set fast records on 30% of ✖️ facts",
		},
	}, nil
}

// OpenModes returns which modes a kid can play (add and sub always; mul and
// div once their gate clears) plus the gate details.
func (s *Service) OpenModes(kid string) (map[string]bool, map[string]Gate, error) {
	cfg, err := s.ConfigFor(kid)
	if err != nil {
		return nil, nil, err
	}
	gates, err := s.modeUnlockStatusLocked(kid, cfg)
	if err != nil {
		return nil, nil, err
	}
	open := map[string]bool{ModeAdd: true, ModeSub: true}
	for m, g := range gates {
		if !g.Locked {
			open[m] = true
		}
	}
	return open, gates, nil
}

// ModeProgress is one mode's mastered fraction of its pool.
type ModeProgress struct {
	Mode     string
	Mastered int
	Total    int
	Pct      float64
}

// PromotionStatus reports how much of each mode's pool is mastered. The
// once-ever promo bonus pays when every mode clears the promo bar.
func (s *Service) PromotionStatus(kid string) (map[string]ModeProgress, error) {
	cfg, err := s.ConfigFor(kid)
	if err != nil {
		return nil, err
	}
	return s.promotionStatusLocked(kid, cfg)
}

func (s *Service) promotionStatusLocked(kid string, cfg Config) (map[string]ModeProgress, error) {
	out := make(map[string]ModeProgress)
	for _, mode := range AllModes {
		pool, err := s.FactPool(kid, mode)
		if err != nil {
			return nil, err
		}
		facts, err := s.factsMap(kid, mode)
		if err != nil {
			return nil, err
		}
		mastered := 0
		for _, f := range pool {
			if fs, ok := facts[f.Key()]; ok && isMastered(&fs, cfg) {
				mastered++
			}
		}
		out[mode] = ModeProgress{
			Mode:     mode,
			Mastered: mastered,
			Total:    len(pool),
			Pct:      round3(float64(mastered) / float64(max(1, len(pool)))),
		}
	}
	return out, nil
}

// ---------------------------------------------------------- answer recording

// CoinMsg reports what a fast answer earned: nothing because the daily cap
// is reached ("capped"), a coin added to the jar ("coin") or a full jar
// banked into a credited minute ("jar").
type CoinMsg struct {
	Kind   string
	Coins  int
	Need   int
	Banked bool
	Err    string
}

// AnswerOutcome is the result of one recorded fact answer.
type AnswerOutcome struct {
	Correct bool
	Answer  int
	Record  bool // a new per-fact personal best was set
	Coin    *CoinMsg
}

// RecordAnswer grades and records one answer: per-fact stats (asked, correct,
// the 12-entry response window, median and best), a coin for a new fast
// record and a credited minute for every banked jar. The daily cap gates the
// coin payout only; personal-best records keep improving after the cap. A
// credit failure applies nothing, so the caller can retry; the award id is
// derived from the state that a retry would replay, so it never
// double-credits (core.Crediter idempotency).
func (s *Service) RecordAnswer(ctx context.Context, kid, mode string, a, b int, op string, given, ms int) (AnswerOutcome, error) {
	if !validMode(mode) {
		return AnswerOutcome{}, fmt.Errorf("%w: %s", ErrBadMode, mode)
	}
	open, _, err := s.OpenModes(kid)
	if err != nil {
		return AnswerOutcome{}, err
	}
	if !open[mode] {
		return AnswerOutcome{}, fmt.Errorf("%w: %s", ErrLocked, mode)
	}
	op = normOp(op)
	ms = clampMS(ms)
	cfg, err := s.ConfigFor(kid)
	if err != nil {
		return AnswerOutcome{}, err
	}
	correct := given == AnswerOf(a, b, op)
	key := FactKeyFor(a, b, op)

	s.mu.Lock()
	defer s.mu.Unlock()

	f, _, err := s.loadFact(kid, mode, key)
	if err != nil {
		return AnswerOutcome{}, err
	}
	f.Asked++
	record := false
	if correct {
		f.Correct++
		f.Times = append(f.Times, ms)
		if len(f.Times) > 12 {
			f.Times = f.Times[len(f.Times)-12:]
		}
		f.MedianMS = median(f.Times)
		if f.BestMS == nil || float64(ms) < *f.BestMS {
			f.BestMS = new(float64(ms))
			record = true
		}
	}

	aw, err := s.awardsFor(kid, bucketMath)
	if err != nil {
		return AnswerOutcome{}, err
	}
	var logs []logRow
	var coin *CoinMsg
	if aw.Earned >= cfg.AwardDailyCap-capEpsilon {
		coin = &CoinMsg{Kind: "capped"}
	} else if record && ms <= int(cfg.MasterySpeedMS) {
		coin, err = s.bankCoins(ctx, kid, aw, kindJar, 1, cfg, &logs)
		if err != nil {
			return AnswerOutcome{}, err
		}
	}

	err = s.withTx(func(tx *sql.Tx) error {
		if err := saveFact(tx, kid, mode, key, &f); err != nil {
			return err
		}
		if err := saveAwards(tx, kid, bucketMath, aw); err != nil {
			return err
		}
		return insertLogs(tx, logs)
	})
	if err != nil {
		return AnswerOutcome{}, err
	}
	return AnswerOutcome{Correct: correct, Answer: AnswerOf(a, b, op), Record: record, Coin: coin}, nil
}

// ---------------------------------------------------------------- rounds

// RoundAnswer is one answered question of a finished round, as submitted by
// the client; the server grades it.
type RoundAnswer struct {
	A     int
	B     int
	Op    string
	Given int
	Ms    int
}

type gradedAnswer struct {
	a, b  int
	op    string
	given int
	ms    int
}

// RoundAward is one server-side award emitted for a round.
type RoundAward struct {
	Kind      string // "record", "promo" or "tables"
	OK        bool
	Miss      bool // a record milestone was missed
	AccFail   *bool
	WasBestMS *float64
	NewBestMS *float64
	ThisMS    *float64
	Text      string // tables promotion label, or promo failure info
}

// RoundSummary is the evaluated round result, mirroring what the legacy
// /api/round_result returned.
type RoundSummary struct {
	Mode        string
	Checkup     bool
	N           int
	Correct     int
	MedianMS    *float64
	Promo       map[string]ModeProgress
	Awards      []RoundAward
	EarnedToday float64
	Cap         float64
	PromoDone   bool
	TablesTop   int
}

// FinishRound evaluates a finished practice round or check-up and records
// the session. Practice rounds (not check-ups) with at least
// round_min_questions are checked for a milestone (accuracy at or above the
// mastery bar with a median that beats the stored personal best by at least
// 2%); any round may trigger the once-ever promo bonus when every pool
// clears the promo bar; mul rounds widen the times-table ladder when 80% of
// the current pool is mastered. As with RecordAnswer, a credit failure
// applies nothing so a retried round replays identically.
func (s *Service) FinishRound(ctx context.Context, kid, mode string, checkup bool, answers []RoundAnswer) (RoundSummary, error) {
	if !validMode(mode) {
		return RoundSummary{}, fmt.Errorf("%w: %s", ErrBadMode, mode)
	}
	cfg, err := s.ConfigFor(kid)
	if err != nil {
		return RoundSummary{}, err
	}
	results := make([]gradedAnswer, 0, len(answers))
	for _, r := range answers {
		results = append(results, gradedAnswer{
			a: r.A, b: r.B, op: normOp(r.Op), given: r.Given, ms: clampMS(r.Ms),
		})
	}
	n := len(results)
	ncorrect := 0
	var times []int
	for _, r := range results {
		if r.given == AnswerOf(r.a, r.b, r.op) {
			ncorrect++
			times = append(times, r.ms)
		}
	}
	med := median(times)

	s.mu.Lock()
	defer s.mu.Unlock()

	var flagSets []flagRow
	var logs []logRow
	aw, err := s.awardsFor(kid, bucketMath)
	if err != nil {
		return RoundSummary{}, err
	}
	awardsOut := []RoundAward{}

	// Milestone (practice rounds only): >= min questions, >= mastery
	// accuracy, and the median beats the stored best by >= 2%.
	if !checkup && n >= cfg.RoundMinQuestions {
		best, err := s.flagFloat("best:" + kid + ":" + mode)
		if err != nil {
			return RoundSummary{}, err
		}
		accOK := float64(ncorrect)/float64(max(1, n)) >= cfg.MasteryAccuracy
		switch {
		case accOK && med != nil && (best == nil || *med < *best*0.98):
			flagSets = append(flagSets, flagRow{key: "best:" + kid + ":" + mode, value: formatFloat(*med)})
			awardsOut = append(awardsOut, RoundAward{Kind: "record", OK: true, WasBestMS: best, NewBestMS: med})
		case accOK && med != nil:
			awardsOut = append(awardsOut, RoundAward{Kind: "record", Miss: true, WasBestMS: best, ThisMS: med})
		default:
			fail := !accOK
			awardsOut = append(awardsOut, RoundAward{Kind: "record", Miss: true, AccFail: &fail, WasBestMS: best, ThisMS: med})
		}
	}

	// Once-ever promo bonus: every mode's pool past the promo bar.
	promo, err := s.promotionStatusLocked(kid, cfg)
	if err != nil {
		return RoundSummary{}, err
	}
	all := len(promo) > 0
	for _, p := range promo {
		if p.Pct < cfg.PromoBarPercent {
			all = false
			break
		}
	}
	if all {
		done, err := s.flagExists("promo_bonus:" + kid)
		if err != nil {
			return RoundSummary{}, err
		}
		if !done {
			ok, info, err := s.tryAward(ctx, kid, aw, kindPromo, cfg.CheckupBonus, cfg, &flagSets, &logs)
			if err != nil {
				return RoundSummary{}, err
			}
			awardsOut = append(awardsOut, RoundAward{Kind: "promo", OK: ok, Text: info})
		}
	}

	// Times-table ladder: widen when 80% of the current mul pool is mastered.
	// Promotion needs the mul pool to grow (div follows implicitly), and only
	// from practice rounds; check-ups never promote.
	newTop := 0
	if mode == ModeMul && !checkup {
		label, top, err := s.promoteTablesLocked(kid, cfg)
		if err != nil {
			return RoundSummary{}, err
		}
		if top != 0 {
			newTop = top
			awardsOut = append(awardsOut, RoundAward{Kind: "tables", OK: true, Text: label})
		}
	}

	sess := sessionRow{
		At: s.now(), Kid: kid, Mode: mode, Checkup: checkup,
		N: n, Correct: ncorrect, MedianMS: med,
	}
	err = s.withTx(func(tx *sql.Tx) error {
		if err := insertSession(tx, sess); err != nil {
			return err
		}
		if err := saveAwards(tx, kid, bucketMath, aw); err != nil {
			return err
		}
		if err := saveFlags(tx, flagSets); err != nil {
			return err
		}
		if err := insertLogs(tx, logs); err != nil {
			return err
		}
		if newTop != 0 {
			return saveTablesTop(tx, kid, newTop)
		}
		return nil
	})
	if err != nil {
		return RoundSummary{}, err
	}

	tablesTop, err := s.TablesTop(kid)
	if err != nil {
		return RoundSummary{}, err
	}
	promoDone, err := s.flagExists("promo_bonus:" + kid)
	if err != nil {
		return RoundSummary{}, err
	}
	return RoundSummary{
		Mode: mode, Checkup: checkup, N: n, Correct: ncorrect, MedianMS: med,
		Promo: promo, Awards: awardsOut, EarnedToday: aw.Earned,
		Cap: cfg.AwardDailyCap, PromoDone: promoDone, TablesTop: tablesTop,
	}, nil
}

// promoteTablesLocked evaluates the tables ladder (1-3 -> 1-5 -> 1-7 ->
// 1-10) and returns the promotion label and new top (0 = no promotion); the
// caller persists the new top atomically with the round.
func (s *Service) promoteTablesLocked(kid string, cfg Config) (string, int, error) {
	ladder := []int{3, 5, 7, 10}
	cur, err := s.TablesTop(kid)
	if err != nil {
		return "", 0, err
	}
	pool, err := s.FactPool(kid, ModeMul)
	if err != nil {
		return "", 0, err
	}
	facts, err := s.factsMap(kid, ModeMul)
	if err != nil {
		return "", 0, err
	}
	mastered := 0
	for _, f := range pool {
		if fs, ok := facts[f.Key()]; ok && isMastered(&fs, cfg) {
			mastered++
		}
	}
	pct := float64(mastered) / float64(max(1, len(pool)))
	nxt := cur
	for _, step := range ladder {
		if step > cur && pct >= cfg.TablesPromoteFrac {
			nxt = step
			break
		}
	}
	if nxt != cur {
		return fmt.Sprintf("×tables widened to 1 to %d!", nxt), nxt, nil
	}
	return "", 0, nil
}

// -------------------------------------------------------------- sessions

// SessionView is one recorded practice round or check-up.
type SessionView struct {
	At       time.Time
	Mode     string
	Checkup  bool
	N        int
	Correct  int
	MedianMS *float64
}

type sessionRow struct {
	At       time.Time
	Kid      string
	Mode     string
	Checkup  bool
	N        int
	Correct  int
	MedianMS *float64
}

func insertSession(ex execer, sess sessionRow) error {
	var med any
	if sess.MedianMS != nil {
		med = *sess.MedianMS
	}
	// INSERT OR IGNORE keeps a replayed import (same kid, timestamp, mode,
	// checkup) from duplicating a round; live rounds never share a
	// timestamp, so nothing is dropped in production.
	_, err := ex.Exec(`INSERT OR IGNORE INTO practice_sessions (kid, at, mode, checkup, n, correct, median_ms)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sess.Kid, sess.At.UTC().Format(time.RFC3339), sess.Mode,
		boolInt(sess.Checkup), sess.N, sess.Correct, med)
	return err
}

// Sessions returns a kid's recorded rounds, oldest first.
func (s *Service) Sessions(kid string) ([]SessionView, error) {
	rows, err := s.db.Query(
		`SELECT at, mode, checkup, n, correct, median_ms FROM practice_sessions WHERE kid=? ORDER BY id`,
		kid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionView
	for rows.Next() {
		var at, mode string
		var checkup, n, correct int
		var med sql.NullFloat64
		if err := rows.Scan(&at, &mode, &checkup, &n, &correct, &med); err != nil {
			return nil, err
		}
		t, err := time.Parse(time.RFC3339, at)
		if err != nil {
			return nil, err
		}
		v := SessionView{At: t, Mode: mode, Checkup: checkup == 1, N: n, Correct: correct}
		if med.Valid {
			v.MedianMS = new(med.Float64)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func formatFloat(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
