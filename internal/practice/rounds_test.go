package practice

import (
	"errors"
	"strings"
	"testing"

	"github.com/edementhon/kids-shelf/internal/core"
)

// TestRoundAnswerGradesOnlyTheQuestionOnScreen: a double Enter repeats the
// answer for the question on screen (replayed, graded once) or sends it for
// the next question the kid has not seen yet (refused, nothing recorded).
func TestRoundAnswerGradesOnlyTheQuestionOnScreen(t *testing.T) {
	e := newEnv(t, 41)
	kid := "k1"
	qs := []Question{{A: 3, B: 4, Op: "+"}, {A: 2, B: 4, Op: "+"}, {A: 5, B: 1, Op: "+"}}
	round := e.svc.OpenMathRound(kid, ModeAdd, qs)

	first, err := e.svc.RecordRoundAnswer(ctx(), kid, ModeAdd, round, 0, 3, 4, "+", 7, 3000)
	if err != nil || !first.Correct {
		t.Fatalf("answer 0: %+v, %v", first, err)
	}
	// The same answer again (a second Enter before the page moved on).
	again, err := e.svc.RecordRoundAnswer(ctx(), kid, ModeAdd, round, 0, 3, 4, "+", 7, 3100)
	if err != nil || again != first {
		t.Fatalf("repeat of answer 0 = %+v, %v; want the first outcome %+v", again, err, first)
	}
	facts, _ := e.svc.FactsFor(kid, ModeAdd)
	if f := facts["3+4"]; f.Asked != 1 {
		t.Fatalf("3+4 asked %d times, want 1", f.Asked)
	}
	// The old answer sent for a question further on is refused.
	if _, err := e.svc.RecordRoundAnswer(ctx(), kid, ModeAdd, round, 2, 5, 1, "+", 7, 200); !errors.Is(err, ErrStaleAnswer) {
		t.Fatalf("answer for an unseen question err = %v, want ErrStaleAnswer", err)
	}
	// Position 1 holds 2+4, not 5+1.
	if _, err := e.svc.RecordRoundAnswer(ctx(), kid, ModeAdd, round, 1, 5, 1, "+", 6, 200); !errors.Is(err, ErrStaleAnswer) {
		t.Fatalf("answer naming the wrong question err = %v, want ErrStaleAnswer", err)
	}
	// Another mode or an unknown round is not this round.
	if _, err := e.svc.RecordRoundAnswer(ctx(), kid, ModeSub, round, 1, 2, 4, "+", 6, 200); !errors.Is(err, ErrStaleAnswer) {
		t.Fatalf("answer for another mode err = %v, want ErrStaleAnswer", err)
	}
	if _, err := e.svc.RecordRoundAnswer(ctx(), kid, ModeAdd, "nope", 1, 2, 4, "+", 6, 200); !errors.Is(err, ErrStaleAnswer) {
		t.Fatalf("answer for an unknown round err = %v, want ErrStaleAnswer", err)
	}
	if _, err := e.svc.RecordRoundAnswer(ctx(), "k2", ModeAdd, round, 1, 2, 4, "+", 6, 200); !errors.Is(err, ErrStaleAnswer) {
		t.Fatalf("another kid's answer err = %v, want ErrStaleAnswer", err)
	}
	facts, _ = e.svc.FactsFor(kid, ModeAdd)
	if _, ok := facts["1+5"]; ok {
		t.Fatal("a refused answer recorded 5+1")
	}
	if f := facts["2+4"]; f.Asked != 0 {
		t.Fatalf("2+4 asked %d times before it was on screen", f.Asked)
	}
	// The question on screen still grades normally.
	out, err := e.svc.RecordRoundAnswer(ctx(), kid, ModeAdd, round, 1, 2, 4, "+", 6, 3000)
	if err != nil || !out.Correct {
		t.Fatalf("answer 1: %+v, %v", out, err)
	}
}

// TestRoundAnswerRetryAfterFailure: a grade that fails (the budget is down)
// frees the question, so the page's Try again grades it once it recovers.
func TestRoundAnswerRetryAfterFailure(t *testing.T) {
	e := newEnv(t, 42)
	kid := "k1"
	qs := make([]Question, 10)
	for i := range qs {
		qs[i] = Question{A: i + 1, B: 1, Op: "+"}
	}
	round := e.svc.OpenMathRound(kid, ModeAdd, qs)
	for i := range 9 {
		if _, err := e.svc.RecordRoundAnswer(ctx(), kid, ModeAdd, round, i, i+1, 1, "+", i+2, 3000); err != nil {
			t.Fatalf("answer %d: %v", i, err)
		}
	}
	// The tenth fast record banks a minute; the credit fails.
	e.cred.Err = errors.New("budget down")
	if _, err := e.svc.RecordRoundAnswer(ctx(), kid, ModeAdd, round, 9, 10, 1, "+", 11, 3000); err == nil || errors.Is(err, ErrStaleAnswer) {
		t.Fatalf("answer during the outage err = %v, want the credit error", err)
	}
	e.cred.Err = nil
	out, err := e.svc.RecordRoundAnswer(ctx(), kid, ModeAdd, round, 9, 10, 1, "+", 11, 3000)
	if err != nil || out.Coin == nil || !out.Coin.Banked {
		t.Fatalf("retry: %+v, %v; want the banked minute", out, err)
	}
	if n := len(e.cred.Credits()); n != 1 {
		t.Fatalf("credits = %d, want 1", n)
	}
}

// TestSpellRoundAnswerGuard: the same guard on spelling rounds, where a
// double Enter used to grade the next hidden word with the last word typed.
func TestSpellRoundAnswerGuard(t *testing.T) {
	e := newEnv(t, 43)
	kid := "k1"
	round := e.svc.OpenSpellRound(kid, []string{"stop", "hand"})
	out, err := e.svc.RecordSpellRoundAnswer(ctx(), kid, round, 0, "stop", "stop", 2000)
	if err != nil || !out.Correct {
		t.Fatalf("stop: %+v, %v", out, err)
	}
	if again, err := e.svc.RecordSpellRoundAnswer(ctx(), kid, round, 0, "stop", "stop", 2100); err != nil || again != out {
		t.Fatalf("repeat of stop = %+v, %v", again, err)
	}
	if _, err := e.svc.RecordSpellRoundAnswer(ctx(), kid, round, 0, "hand", "stop", 100); !errors.Is(err, ErrStaleAnswer) {
		t.Fatalf("hand at position 0 err = %v, want ErrStaleAnswer", err)
	}
	recs, _ := e.svc.SpellingRecords(kid)
	for _, r := range recs {
		if r.Word == "stop" && r.Hits != 1 {
			t.Fatalf("stop hits = %d, want 1", r.Hits)
		}
		if r.Word == "hand" {
			t.Fatalf("hand was graded before it was played: %+v", r)
		}
	}
	if out, err := e.svc.RecordSpellRoundAnswer(ctx(), kid, round, 1, "Hand", "hand", 2000); err != nil || !out.Correct {
		t.Fatalf("hand: %+v, %v", out, err)
	}
}

// TestBossRoundAnswerGuard: one visible miss heals the boss once (and costs
// one of the kid's hearts on a lethal boss), however many times it is sent;
// an answer for the next question or from a replaced fight changes nothing.
func TestBossRoundAnswerGuard(t *testing.T) {
	e := newEnv(t, 44)
	kid := "k1"
	f, qs, err := e.svc.BossRound(kid)
	if err != nil || f.Round == "" {
		t.Fatalf("BossRound: %+v, %v", f, err)
	}
	q := qs[0]
	hit, err := e.svc.BossRoundAnswer(ctx(), kid, f.Round, 0, q.A, q.B, q.Op, AnswerOf(q.A, q.B, q.Op))
	if err != nil || hit.HP != BossHearts-1 {
		t.Fatalf("hit: %+v, %v", hit, err)
	}
	q = qs[1]
	miss, err := e.svc.BossRoundAnswer(ctx(), kid, f.Round, 1, q.A, q.B, q.Op, -1)
	if err != nil || miss.Kind != "heal" || miss.HP != BossHearts {
		t.Fatalf("miss: %+v, %v", miss, err)
	}
	if again, err := e.svc.BossRoundAnswer(ctx(), kid, f.Round, 1, q.A, q.B, q.Op, -1); err != nil || again != miss {
		t.Fatalf("repeat of the miss = %+v, %v; want the first outcome", again, err)
	}
	q = qs[3]
	if _, err := e.svc.BossRoundAnswer(ctx(), kid, f.Round, 3, q.A, q.B, q.Op, -1); !errors.Is(err, ErrStaleAnswer) {
		t.Fatalf("answer two questions ahead err = %v, want ErrStaleAnswer", err)
	}
	bs, _ := e.svc.BossState(kid)
	if bs.HP["adder"] != BossHearts {
		t.Fatalf("boss hp = %d, want %d (one hit, one heal)", bs.HP["adder"], BossHearts)
	}
	// A new fight (another device pressed Fight!) replaces this one.
	f2, qs2, err := e.svc.BossRound(kid)
	if err != nil {
		t.Fatalf("second BossRound: %v", err)
	}
	q = qs[2]
	if _, err := e.svc.BossRoundAnswer(ctx(), kid, f.Round, 2, q.A, q.B, q.Op, AnswerOf(q.A, q.B, q.Op)); !errors.Is(err, ErrStaleAnswer) {
		t.Fatalf("answer from the replaced fight err = %v, want ErrStaleAnswer", err)
	}
	q = qs2[0]
	if _, err := e.svc.BossRoundAnswer(ctx(), kid, f2.Round, 0, q.A, q.B, q.Op, AnswerOf(q.A, q.B, q.Op)); err != nil {
		t.Fatalf("answer in the new fight: %v", err)
	}
}

// TestBossRoundEndsOnKnockOut: after the lethal boss knocks the kid out,
// the repeat of the last miss replays the knock-out and the fight takes no
// more answers.
func TestBossRoundEndsOnKnockOut(t *testing.T) {
	e := newEnv(t, 45)
	kid := "k1"
	var prior []string
	for _, b := range Bosses {
		if b.ID == "alienfish" {
			break
		}
		prior = append(prior, b.ID)
	}
	if err := e.svc.ImportBosses(ctx(), core.KidID(kid), core.BossState{
		Defeated: prior, HP: map[string]int{}, PHP: map[string]int{},
	}); err != nil {
		t.Fatalf("ImportBosses: %v", err)
	}
	f, qs, err := e.svc.BossRound(kid)
	if err != nil || f.Boss.ID != "alienfish" {
		t.Fatalf("BossRound: %+v, %v", f, err)
	}
	var last BossAnswerMsg
	for i := range 3 {
		q := qs[i]
		last, err = e.svc.BossRoundAnswer(ctx(), kid, f.Round, i, q.A, q.B, q.Op, -1)
		if err != nil {
			t.Fatalf("miss %d: %v", i, err)
		}
		if want := 2 - i; i < 2 && last.PHP != want {
			t.Fatalf("miss %d left %d hearts, want %d", i, last.PHP, want)
		}
	}
	if !last.Killed {
		t.Fatalf("third miss = %+v, want the knock-out", last)
	}
	q := qs[2]
	if again, err := e.svc.BossRoundAnswer(ctx(), kid, f.Round, 2, q.A, q.B, q.Op, -1); err != nil || again != last {
		t.Fatalf("repeat of the knock-out = %+v, %v", again, err)
	}
	q = qs[3]
	if _, err := e.svc.BossRoundAnswer(ctx(), kid, f.Round, 3, q.A, q.B, q.Op, AnswerOf(q.A, q.B, q.Op)); !errors.Is(err, ErrStaleAnswer) {
		t.Fatalf("answer after the knock-out err = %v, want ErrStaleAnswer", err)
	}
}

// TestBossStatusShowsFullHeartsAfterStop: a stopped fight leaves damage in
// the stored state, but the next fight starts at full hearts, so the arena
// must show full hearts too (it used to show the old damage).
func TestBossStatusShowsFullHeartsAfterStop(t *testing.T) {
	e := newEnv(t, 46)
	kid := "k1"
	f, qs, err := e.svc.BossRound(kid)
	if err != nil {
		t.Fatalf("BossRound: %v", err)
	}
	for i := range 3 {
		q := qs[i]
		if _, err := e.svc.BossRoundAnswer(ctx(), kid, f.Round, i, q.A, q.B, q.Op, AnswerOf(q.A, q.B, q.Op)); err != nil {
			t.Fatalf("hit %d: %v", i, err)
		}
	}
	st, err := e.svc.BossStatus(kid)
	if err != nil || st.HP != BossHearts {
		t.Fatalf("status after a stopped fight: hp %d, %v; want %d", st.HP, err, BossHearts)
	}
}

// TestSpellHintsHaveABlank: every hint shows where the word goes.
func TestSpellHintsHaveABlank(t *testing.T) {
	for _, w := range Words() {
		if !strings.Contains(w.Hint, "___") {
			t.Errorf("hint for %q has no blank: %q", w.Word, w.Hint)
		}
	}
}
