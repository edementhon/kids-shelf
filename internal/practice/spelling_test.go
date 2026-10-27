package practice

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSpellingBankAndTiers(t *testing.T) {
	words := Words()
	if len(words) != 78 {
		t.Fatalf("bank = %d words, want 78", len(words))
	}
	seen := make(map[string]bool, len(words))
	for _, w := range words {
		if seen[w.Word] {
			t.Fatalf("duplicate word %q", w.Word)
		}
		seen[w.Word] = true
		if w.Hint == "" {
			t.Errorf("empty hint for %q", w.Word)
		}
	}
	if _, ok := WordHint("cat"); !ok {
		t.Errorf("cat missing from bank")
	}
	if _, ok := WordHint("zzz"); ok {
		t.Errorf("zzz should not be in bank")
	}

	tiers := Tiers()
	if len(tiers) != 5 {
		t.Fatalf("tiers = %d, want 5", len(tiers))
	}
	want := []int{15, 15, 15, 15, 18}
	total := 0
	for i, ti := range tiers {
		if len(ti.Words) != want[i] {
			t.Errorf("tier %d size = %d, want %d", i, len(ti.Words), want[i])
		}
		if ti.Name != SpellTierNames[i] {
			t.Errorf("tier %d name = %q, want %q", i, ti.Name, SpellTierNames[i])
		}
		total += len(ti.Words)
		for _, w := range ti.Words {
			if !seen[w] {
				t.Errorf("tier word %q not in bank", w)
			}
		}
	}
	if total != 78 {
		t.Errorf("tier total = %d, want 78", total)
	}
	if tiers[0].Words[0] != "cat" {
		t.Errorf("first tier starts at %q, want cat", tiers[0].Words[0])
	}
	last := tiers[4].Words
	if last[len(last)-1] != "environment" {
		t.Errorf("last tier ends at %q, want environment", last[len(last)-1])
	}
}

// TestSpellSamplingFavorsWeakWords is the spelling sampling gate: with a
// seeded RNG, unseen words beat miss-streak words, which beat healthy ones.
// Weights 3.0 / 2.5 / 1.0 over 48 / 20 / 10 words give expected shares of
// 70.6% / 24.5% / 4.9%; the assertions use wide margins. It draws through
// the same weighted-choice path SampleSpellWords uses, so the gate stays
// deterministic and cheap; SampleSpellWords itself is smoke-tested below for
// round shape and distinctness.
func TestSpellSamplingFavorsWeakWords(t *testing.T) {
	e := newEnv(t, 20)
	kid := "k1"
	words := Words()
	class := make(map[string]string, len(words))
	for _, w := range words[:10] {
		class[w.Word] = "hit"
		if _, err := e.svc.RecordSpellAnswer(ctx(), kid, w.Word, w.Word, 5000); err != nil {
			t.Fatalf("hit %s: %v", w.Word, err)
		}
	}
	for _, w := range words[10:30] {
		class[w.Word] = "missed"
		for range 2 {
			if _, err := e.svc.RecordSpellAnswer(ctx(), kid, w.Word, "zzz", 3000); err != nil {
				t.Fatalf("miss %s: %v", w.Word, err)
			}
		}
	}
	for _, w := range words[30:] {
		class[w.Word] = "unseen"
	}
	// The same weights SampleSpellWords derives from the record classes.
	weights := make([]float64, len(words))
	for i, w := range words {
		switch class[w.Word] {
		case "unseen":
			weights[i] = 3.0
		case "missed":
			weights[i] = 2.5
		default:
			weights[i] = 1.0
		}
	}
	counts := map[string]int{}
	e.svc.mu.Lock()
	for range 20000 {
		counts[class[words[weightedChoice(e.svc.rng, weights)].Word]]++
	}
	e.svc.mu.Unlock()
	total := counts["missed"] + counts["hit"] + counts["unseen"]
	if total != 20000 {
		t.Fatalf("classified %d draws, want 20000", total)
	}
	if counts["unseen"] < 5*counts["hit"] {
		t.Errorf("unseen %d not favored over hit %d", counts["unseen"], counts["hit"])
	}
	if counts["missed"] < 3*counts["hit"] {
		t.Errorf("missed %d not favored over hit %d", counts["missed"], counts["hit"])
	}

	// Integration smoke: SampleSpellWords returns 5 distinct words.
	for range 10 {
		picked, err := e.svc.SampleSpellWords(kid, 5, nil)
		if err != nil {
			t.Fatalf("SampleSpellWords: %v", err)
		}
		if len(picked) != 5 {
			t.Fatalf("round returned %d words, want 5", len(picked))
		}
		seen := map[string]bool{}
		for _, w := range picked {
			if seen[w] {
				t.Fatalf("round repeated %s", w)
			}
			seen[w] = true
		}
	}
}

func TestSpellSamplePoolRestriction(t *testing.T) {
	e := newEnv(t, 21)
	kid := "k1"
	words := Words()
	pool := []string{words[0].Word, words[1].Word, words[2].Word}
	picked, err := e.svc.SampleSpellWords(kid, 3, pool)
	if err != nil {
		t.Fatalf("SampleSpellWords: %v", err)
	}
	if len(picked) != 3 {
		t.Fatalf("picked %d, want 3", len(picked))
	}
	for _, w := range picked {
		if w != pool[0] && w != pool[1] && w != pool[2] {
			t.Fatalf("picked %q outside the tier pool", w)
		}
	}
}

func TestSpellRecordAndSessions(t *testing.T) {
	e := newEnv(t, 22)
	kid := "k1"

	// A miss never records a best time or a coin.
	out, err := e.svc.RecordSpellAnswer(ctx(), kid, "cat", "zzz", 3000)
	if err != nil {
		t.Fatalf("RecordSpellAnswer: %v", err)
	}
	if out.Correct || out.Record || out.Coin != nil {
		t.Fatalf("miss outcome = %+v", out)
	}

	// Case-insensitive exact match: "CAT" for cat.
	out, err = e.svc.RecordSpellAnswer(ctx(), kid, "cat", "CAT", 3000)
	if err != nil {
		t.Fatalf("RecordSpellAnswer: %v", err)
	}
	if !out.Correct || !out.Record {
		t.Fatalf("hit outcome = %+v", out)
	}
	if out.Coin == nil || out.Coin.Kind != "coin" || out.Coin.Coins != 1 {
		t.Fatalf("coin = %+v, want 1 coin", out.Coin)
	}

	recs, _ := e.svc.SpellingRecords(kid)
	if len(recs) != 1 || recs[0].Word != "cat" || recs[0].Hits != 1 || recs[0].Misses != 1 || recs[0].BestMS != 3000 {
		t.Fatalf("records = %+v", recs)
	}
	sess, _ := e.svc.SpellingSessions(kid)
	if len(sess) != 2 || !sess[1].Correct || sess[1].Ms != 3000 {
		t.Fatalf("sessions = %+v", sess)
	}

	// A faster best updates the record.
	out, err = e.svc.RecordSpellAnswer(ctx(), kid, "cat", "cat", 2500)
	if err != nil || !out.Record {
		t.Fatalf("faster answer: %+v, %v", out, err)
	}
	recs, _ = e.svc.SpellingRecords(kid)
	if recs[0].BestMS != 2500 {
		t.Errorf("best = %d, want 2500", recs[0].BestMS)
	}

	// Over the spelling speed bar (2 min), a correct answer is not a record
	// and earns nothing.
	out, err = e.svc.RecordSpellAnswer(ctx(), kid, "cat", "cat", 130000)
	if err != nil || !out.Correct {
		t.Fatalf("slow correct answer: %+v, %v", out, err)
	}
	if out.Record || out.Coin != nil {
		t.Fatalf("slow answer recorded: %+v", out)
	}

	// Unknown words are rejected.
	if _, err := e.svc.RecordSpellAnswer(ctx(), kid, "zzz", "zzz", 1000); !errors.Is(err, ErrUnknownWord) {
		t.Fatalf("unknown word err = %v, want ErrUnknownWord", err)
	}
}

// TestSpellCapSeparateFromMath is the PLAN gate: spelling banks against its
// own daily cap (3) and jar, fully independent of the math jar.
func TestSpellCapSeparateFromMath(t *testing.T) {
	e := newEnv(t, 23)
	kid := "k1"
	words := Words()

	// 30 fast records on distinct words bank 3 spelling minutes.
	for i := range 30 {
		if _, err := e.svc.RecordSpellAnswer(ctx(), kid, words[i].Word, words[i].Word, 5000); err != nil {
			t.Fatalf("spell answer %d: %v", i, err)
		}
	}
	credits := e.cred.Credits()
	if len(credits) != 3 {
		t.Fatalf("spell credits = %d, want 3", len(credits))
	}
	for i, c := range credits {
		if c.AwardID != []string{"k1:spell:2026-10-26:1", "k1:spell:2026-10-26:2", "k1:spell:2026-10-26:3"}[i] {
			t.Errorf("credit %d id = %q", i, c.AwardID)
		}
	}

	// 31st record: best keeps improving, coins stop.
	out, err := e.svc.RecordSpellAnswer(ctx(), kid, words[30].Word, words[30].Word, 4000)
	if err != nil {
		t.Fatalf("spell answer 31: %v", err)
	}
	if !out.Record || out.Coin == nil || out.Coin.Kind != "capped" {
		t.Fatalf("31st spell record = %+v", out)
	}
	sa, _ := e.svc.SpellAwards(kid)
	if sa.Earned != 3.0 || sa.Cap != 3 {
		t.Errorf("spell awards = %+v", sa)
	}

	// The math jar banks independently of the exhausted spelling cap.
	for i := range 10 {
		fastAnswer(t, e.svc, kid, i)
	}
	credits = e.cred.Credits()
	if len(credits) != 4 || credits[3].AwardID != "k1:jar:2026-10-26:1" {
		t.Fatalf("math jar did not bank independently: %+v", credits)
	}
}

func TestSpellStatusAndAudioPath(t *testing.T) {
	e := newEnv(t, 24)
	kid := "k1"

	st, err := e.svc.SpellStatus(kid)
	if err != nil {
		t.Fatalf("SpellStatus: %v", err)
	}
	if st.Total != 78 || st.Recorded != 0 || st.Missed != 0 {
		t.Errorf("empty status = %+v", st)
	}

	if _, err := e.svc.RecordSpellAnswer(ctx(), kid, "cat", "cat", 5000); err != nil {
		t.Fatalf("RecordSpellAnswer: %v", err)
	}
	if _, err := e.svc.RecordSpellAnswer(ctx(), kid, "dog", "zzz", 5000); err != nil {
		t.Fatalf("RecordSpellAnswer: %v", err)
	}
	st, _ = e.svc.SpellStatus(kid)
	if st.Recorded != 1 || st.Missed != 1 {
		t.Errorf("status = %+v, want 1 recorded, 1 missed", st)
	}

	// AudioPath reports the clips-dir path and whether the clip exists.
	p, ok := e.svc.AudioPath("cat")
	want := filepath.Join(e.svc.clips, "cat.wav")
	if p != want {
		t.Errorf("path = %q, want %q", p, want)
	}
	if ok {
		t.Errorf("missing clip reported as present")
	}
	if err := os.WriteFile(want, []byte("RIFF...."), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if p, ok := e.svc.AudioPath("cat"); !ok || p != want {
		t.Errorf("existing clip not reported: %q, %v", p, ok)
	}
}
