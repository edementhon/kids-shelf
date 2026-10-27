package practice

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Spelling words: the merged grade-level bank from the legacy app — easy
// (CVC, grade 1) through hard (grade 3+), one shared bank every kid works
// up through. Each entry is (word, hint sentence), copied verbatim.
type Word struct {
	Word string
	Hint string
}

// Tier is one of the five spelling tiers (15/15/15/15/18 words, easy ->
// hard). Tiers organize the bank and show progress; they never lock words.
type Tier struct {
	Name  string
	Words []string
}

// SpellTierNames mirrors the legacy tier labels.
var SpellTierNames = []string{"🥉 Bronze", "🥈 Silver", "🥇 Gold", "💎 Diamond", "👑 Crown"}

var spellAlex = []Word{
	{"cat", "The ___ sat on the mat."}, {"dog", "The ___ barked loudly."},
	{"sun", "The ___ is bright."}, {"big", "An elephant is very ___."},
	{"red", "A fire truck is ___."}, {"run", "I like to ___ outside."},
	{"jump", "Rabbits ___ high."}, {"help", "Can you ___ me?"},
	{"play", "We ___ at recess."}, {"read", "I ___ a book."},
	{"hand", "Wave your ___."}, {"stop", "___ at the curb."},
	{"blue", "The sky is ___."}, {"tree", "The ___ has leaves."},
	{"fish", "A ___ swims."}, {"bird", "A ___ flies."},
	{"cake", "Birthday ___ is sweet."}, {"milk", "Drink your ___."},
	{"book", "Open your ___."}, {"home", "I walk ___ after school."},
	{"rain", "___ makes puddles."}, {"snow", "___ is cold and white."},
	{"star", "Wish upon a ___."}, {"moon", "The ___ comes out at night."},
	{"food", "I eat ___ when hungry."}, {"game", "We played a ___."},
	{"time", "What ___ is it?"}, {"walk", "We ___ to the park."},
	{"sing", "Birds ___ in the morning."}, {"smile", "I ___ when happy."},
	{"water", "Drink ___ every day."}, {"under", "The cat is the bed."},
	{"night", "Stars shine at ___."}, {"small", "A mouse is ___."},
	{"school", "We learn at ___."}, {"garden", "Flowers grow in the ___."},
	{"light", "Turn on the ___."},
}

var spellSam = []Word{
	{"because", "___ I said so."}, {"friend", "My ___ is kind."},
	{"thought", "I ___ about it."}, {"caught", "He ___ the ball."},
	{"enough", "That is ___ for today."}, {"beautiful", "The sunset is ___."},
	{"favorite", "Blue is my ___ color."}, {"important", "Sleep is ___."},
	{"different", "We like ___ things."}, {"sometimes", "___ I read late."},
	{"special", "Today is ___."}, {"surprise", "It was a ___ party!"},
	{"island", "Hawaii is an ___."}, {"answer", "I know the ___."},
	{"question", "Ask a ___."},
	{"sentence", "Write a ___."}, {"measure", "___ the table."},
	{"national", "The ___ park is big."}, {"several", "I have ___ books."},
	{"usually", "I ___ walk to school."}, {"complete", "___ your work."},
	{"example", "For ___, a dog."}, {"height", "The ___ of a door."},
	{"probably", "It will ___ rain."}, {"hospital", "The ___ helps people."},
	{"government", "The ___ makes laws."}, {"impossible", "Nothing is ___."},
	{"knowledge", "Reading builds ___."}, {"language", "Spanish is a ___."},
	{"separate", "___ your socks."},
	{"believe", "I ___ you can do it."}, {"delicious", "The cake was ___."},
	{"restaurant", "We ate at a ___."}, {"chocolate", "___ tastes sweet."},
	{"vacuum", "___ the carpet."},
	{"umbrella", "Take an ___ if it rains."}, {"bracelet", "She wears a ___."},
	{"adventure", "Hiking is an ___."}, {"mysterious", "The cave was ___."},
	{"celebrate", "We ___ birthdays."}, {"environment", "Keep the ___ clean."},
}

// spellOrdered is the merged bank, roughly easy -> hard; spellTiers chunks
// it into five tiers and spellBank is the word -> hint lookup. Mirrors the
// legacy _SPELL_ORDERED/_SPELL_SHARED/SPELL_TIERS construction, including
// the duplicate check that made the legacy app refuse to start.
var (
	spellOrdered []Word
	spellBank    map[string]string
	spellTiers   []Tier
)

func init() {
	spellOrdered = make([]Word, 0, len(spellAlex)+len(spellSam))
	spellOrdered = append(spellOrdered, spellAlex...)
	spellOrdered = append(spellOrdered, spellSam...)
	spellBank = make(map[string]string, len(spellOrdered))
	for _, w := range spellOrdered {
		if _, dup := spellBank[w.Word]; dup {
			panic("practice: duplicate spelling word: " + w.Word)
		}
		spellBank[w.Word] = w.Hint
	}
	tierLen := len(spellOrdered) / 5
	for i := range 5 {
		lo := i * tierLen
		hi := lo + tierLen
		if i == 4 {
			hi = len(spellOrdered)
		}
		tier := Tier{Name: SpellTierNames[i]}
		for _, w := range spellOrdered[lo:hi] {
			tier.Words = append(tier.Words, w.Word)
		}
		spellTiers = append(spellTiers, tier)
	}
}

// Words returns the shared bank sorted by word, for hint lookup pages.
func Words() []Word {
	out := append([]Word(nil), spellOrdered...)
	sort.Slice(out, func(i, j int) bool { return out[i].Word < out[j].Word })
	return out
}

// Tiers returns the five spelling tiers.
func Tiers() []Tier {
	out := make([]Tier, len(spellTiers))
	for i, t := range spellTiers {
		out[i] = Tier{Name: t.Name, Words: append([]string(nil), t.Words...)}
	}
	return out
}

// WordHint returns the hint sentence for a word, if it is in the bank.
func WordHint(word string) (string, bool) {
	h, ok := spellBank[word]
	return h, ok
}

// SpellRecord is one word's per-kid practice state.
type SpellRecord struct {
	Word   string
	Hits   int
	Misses int
	BestMS int // fastest correct response, 0 when none
}

// SpellOutcome is the result of one recorded spelling answer.
type SpellOutcome struct {
	Correct bool
	Word    string
	Record  bool
	Coin    *CoinMsg
}

// SpellStatusView summarizes a kid's progress over the bank.
type SpellStatusView struct {
	Total    int
	Recorded int // words with a best time
	Missed   int // words missed more than hit
}

// SpellSessionView is one recorded spelling answer.
type SpellSessionView struct {
	At      time.Time
	Word    string
	Correct bool
	Ms      int
}

func (s *Service) loadSpellRecord(kid, word string) (SpellRecord, bool, error) {
	var r SpellRecord
	err := s.db.QueryRow(
		`SELECT hits, misses, best_ms FROM practice_spell_records WHERE kid=? AND word=?`,
		kid, word).Scan(&r.Hits, &r.Misses, &r.BestMS)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	r.Word = word
	return r, true, nil
}

func saveSpellRecord(ex execer, kid string, r SpellRecord) error {
	_, err := ex.Exec(`INSERT INTO practice_spell_records (kid, word, hits, misses, best_ms)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (kid, word) DO UPDATE SET
    hits=excluded.hits, misses=excluded.misses, best_ms=excluded.best_ms`,
		kid, r.Word, r.Hits, r.Misses, r.BestMS)
	return err
}

// SampleSpellWords draws n distinct words weighted toward unrecorded and
// miss-streak words — the legacy sample_spell_words. pool, when non-empty,
// limits the draw to those words (tier rounds).
func (s *Service) SampleSpellWords(kid string, n int, pool []string) ([]string, error) {
	inPool := make(map[string]bool, len(pool))
	for _, w := range pool {
		inPool[w] = true
	}
	var items []string
	for _, w := range spellOrdered {
		if len(pool) == 0 || inPool[w.Word] {
			items = append(items, w.Word)
		}
	}
	weights := make([]float64, len(items))
	records, err := s.spellRecordsMap(kid)
	if err != nil {
		return nil, err
	}
	for i, w := range items {
		r, ok := records[w]
		switch {
		case !ok:
			weights[i] = 3.0
		case r.Misses > r.Hits:
			weights[i] = 2.5
		default:
			weights[i] = 1.0
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	picked := make([]string, 0, n)
	seen := make(map[string]bool)
	for tries := 0; len(picked) < n && tries < n*30; tries++ {
		w := items[weightedChoice(s.rng, weights)]
		if seen[w] {
			continue
		}
		seen[w] = true
		picked = append(picked, w)
	}
	s.rng.Shuffle(len(picked), func(i, j int) {
		picked[i], picked[j] = picked[j], picked[i]
	})
	return picked, nil
}

func (s *Service) spellRecordsMap(kid string) (map[string]SpellRecord, error) {
	rows, err := s.db.Query(`SELECT word, hits, misses, best_ms FROM practice_spell_records WHERE kid=?`, kid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]SpellRecord)
	for rows.Next() {
		var r SpellRecord
		if err := rows.Scan(&r.Word, &r.Hits, &r.Misses, &r.BestMS); err != nil {
			return nil, err
		}
		out[r.Word] = r
	}
	return out, rows.Err()
}

// spellCheck grades a spelling answer: exact match wins, case-insensitive
// fallback — the legacy spell_check.
func spellCheck(given, word string) bool {
	return given == word || strings.ToLower(given) == strings.ToLower(word)
}

// RecordSpellAnswer grades and records one spelling answer: per-word hits,
// misses and best times, a coin for a new fast record on the word and a
// credited minute for every banked spelling jar (its own daily cap, separate
// from math). Best times keep improving after the cap; only the coin payout
// stops. A credit failure applies nothing, as with RecordAnswer.
func (s *Service) RecordSpellAnswer(ctx context.Context, kid, word, given string, ms int) (SpellOutcome, error) {
	word = strings.ToLower(strings.TrimSpace(word))
	if _, ok := spellBank[word]; !ok {
		return SpellOutcome{}, ErrUnknownWord
	}
	correct := spellCheck(strings.TrimSpace(given), word)
	ms = clampMS(ms)
	cfg, err := s.ConfigFor(kid)
	if err != nil {
		return SpellOutcome{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	r, _, err := s.loadSpellRecord(kid, word)
	if err != nil {
		return SpellOutcome{}, err
	}
	r.Word = word
	record := false
	if correct {
		r.Hits++
		if ms <= int(cfg.SpellSpeedMS) && (r.BestMS == 0 || ms < r.BestMS) {
			r.BestMS = ms
			record = true
		}
	} else {
		r.Misses++
	}

	aw, err := s.awardsFor(kid, bucketSpell)
	if err != nil {
		return SpellOutcome{}, err
	}
	var logs []logRow
	var coin *CoinMsg
	if aw.Earned >= cfg.SpellDailyCap-capEpsilon {
		coin = &CoinMsg{Kind: "capped"}
	} else if record {
		coin, err = s.bankCoins(ctx, kid, aw, kindSpell, 1, cfg, &logs)
		if err != nil {
			return SpellOutcome{}, err
		}
	}

	err = s.withTx(func(tx *sql.Tx) error {
		if err := saveSpellRecord(tx, kid, r); err != nil {
			return err
		}
		if err := saveAwards(tx, kid, bucketSpell, aw); err != nil {
			return err
		}
		if err := insertLogs(tx, logs); err != nil {
			return err
		}
		// INSERT OR IGNORE: a replayed import must not duplicate a word's
		// answer session, while genuine repeat answers differ in time
		// (or correctness/ms) and pass through.
		_, err := tx.Exec(`INSERT OR IGNORE INTO practice_spell_sessions (kid, at, word, correct, ms) VALUES (?, ?, ?, ?, ?)`,
			kid, s.now().UTC().Format(time.RFC3339), word, boolInt(correct), ms)
		return err
	})
	if err != nil {
		return SpellOutcome{}, err
	}
	return SpellOutcome{Correct: correct, Word: word, Record: record, Coin: coin}, nil
}

// SpellStatus reports a kid's progress over the bank.
func (s *Service) SpellStatus(kid string) (SpellStatusView, error) {
	records, err := s.spellRecordsMap(kid)
	if err != nil {
		return SpellStatusView{}, err
	}
	st := SpellStatusView{Total: len(spellOrdered)}
	for _, w := range spellOrdered {
		r, ok := records[w.Word]
		if !ok {
			continue
		}
		if r.BestMS != 0 {
			st.Recorded++
		}
		if r.Misses > r.Hits {
			st.Missed++
		}
	}
	return st, nil
}

// SpellingRecords returns a kid's per-word records, sorted by word.
func (s *Service) SpellingRecords(kid string) ([]SpellRecord, error) {
	records, err := s.spellRecordsMap(kid)
	if err != nil {
		return nil, err
	}
	out := make([]SpellRecord, 0, len(records))
	for _, r := range records {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Word < out[j].Word })
	return out, nil
}

// SpellingSessions returns a kid's recorded answers, oldest first.
func (s *Service) SpellingSessions(kid string) ([]SpellSessionView, error) {
	rows, err := s.db.Query(
		`SELECT at, word, correct, ms FROM practice_spell_sessions WHERE kid=? ORDER BY id`, kid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SpellSessionView
	for rows.Next() {
		var at, word string
		var correct, ms int
		if err := rows.Scan(&at, &word, &correct, &ms); err != nil {
			return nil, err
		}
		t, err := time.Parse(time.RFC3339, at)
		if err != nil {
			return nil, err
		}
		out = append(out, SpellSessionView{At: t, Word: word, Correct: correct == 1, Ms: ms})
	}
	return out, rows.Err()
}

// AudioPath returns the path of the pre-generated clip for a word —
// <ClipsDir>/<word>.wav — and whether the file exists. The bundled clips
// are written there by InstallClips; a missing clip reports false instead
// of synthesizing speech.
func (s *Service) AudioPath(word string) (string, bool) {
	p := filepath.Join(s.clips, word+".wav")
	_, err := os.Stat(p)
	return p, err == nil
}
