package budget

import (
	"errors"
	"testing"
)

// TestRollover is a table test ported from the legacy portal _rollover and
// the debt semantics its doc string describes: leftover non-negative today
// rolls into saved; a negative today (parent deduction) becomes debt that
// eats the next day's allowance; debt beyond a full day's allowance rides
// into saved, which may go negative. The clock is fixed on 2026-10-26, so
// `day` is the stored (stale) day and the gap length varies with the row.
func TestRollover(t *testing.T) {
	tests := []struct {
		name                 string
		today, saved         float64
		day                  string
		wantToday, wantSaved float64
	}{
		{"leftover rolls into saved", 6, 4, "2026-10-25", 10, 10},
		{"debt eats the next allowance", -3, 2, "2026-10-25", 7, 2},
		{"debt beyond a day rides into saved", -14, 1, "2026-10-25", -4, -3},
		{"multi-day gap, leftover", 6, 4, "2026-10-22", 10, 10},
		{"multi-day gap, debt", -3, 2, "2026-10-20", 7, 2},
		{"first touch", 0, 0, "", 10, 0},
		{"same day is a no-op", 4, 1, "2026-10-26", 4, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _ := newTestBudget(t, at(nowOct26))
			kid := mustCreateKid(t, b, "alex", "1111")
			importAccount(t, b, kid, tt.today, tt.saved, tt.day)
			today, saved, err := b.Balance(ctx, kid, DefaultCounter)
			if err != nil {
				t.Fatalf("Balance: %v", err)
			}
			if !almost(today, tt.wantToday) || !almost(saved, tt.wantSaved) {
				t.Errorf("Balance = (%v, %v), want (%v, %v)", today, saved, tt.wantToday, tt.wantSaved)
			}
		})
	}
}

// TestBalanceGrantsExactlyOneAllowancePerDay: the first read of a day grants
// the allowance; a second read the same day does not regrant it.
func TestBalanceGrantsExactlyOneAllowancePerDay(t *testing.T) {
	b, _ := newTestBudget(t, at(nowOct26))
	kid := mustCreateKid(t, b, "alex", "1111")
	today, saved, err := b.Balance(ctx, kid, DefaultCounter)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if !almost(today, 10) || !almost(saved, 0) {
		t.Errorf("first Balance = (%v, %v), want (10, 0)", today, saved)
	}
	today, saved, err = b.Balance(ctx, kid, DefaultCounter)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if !almost(today, 10) || !almost(saved, 0) {
		t.Errorf("second Balance = (%v, %v), want (10, 0)", today, saved)
	}
}

// TestAdjust is a table test ported from the legacy adjust_minutes: a
// positive delta adds to today (it rolls into saved at the next day
// boundary); a negative delta consumes today first, then saved; the
// remainder clamps at zero, or rides as a negative debt on today when
// allowNegative is set. The stored day equals the clock day, so no rollover
// interferes.
func TestAdjust(t *testing.T) {
	tests := []struct {
		name                 string
		today, saved         float64
		delta                float64
		allowNegative        bool
		wantToday, wantSaved float64
	}{
		{"adds to today", 4, 1, 5, false, 9, 1},
		{"adds on top of a debt", -3, 2, 5, false, 2, 2},
		{"takes today before saved", 4, 6, -6, false, 0, 4},
		{"clamps the remainder at zero", 2, 1, -5, false, 0, 0},
		{"rides the remainder as debt", 0, 1, -3, true, -2, 0},
		{"zero delta is a no-op", 4, 1, 0, false, 4, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _ := newTestBudget(t, at(nowOct26))
			kid := mustCreateKid(t, b, "alex", "1111")
			importAccount(t, b, kid, tt.today, tt.saved, "2026-10-26")
			today, saved, err := b.Adjust(ctx, kid, DefaultCounter, tt.delta, tt.allowNegative)
			if err != nil {
				t.Fatalf("Adjust: %v", err)
			}
			if !almost(today, tt.wantToday) || !almost(saved, tt.wantSaved) {
				t.Errorf("Adjust = (%v, %v), want (%v, %v)", today, saved, tt.wantToday, tt.wantSaved)
			}
		})
	}
}

// TestAdjustAppliesRolloverFirst: adjust_minutes always rolls before applying
// the delta, exactly like the legacy portal.
func TestAdjustAppliesRolloverFirst(t *testing.T) {
	b, _ := newTestBudget(t, at(nowOct26))
	kid := mustCreateKid(t, b, "alex", "1111")
	importAccount(t, b, kid, 3, 1, "2026-10-25")
	today, saved, err := b.Adjust(ctx, kid, DefaultCounter, 4, false)
	if err != nil {
		t.Fatalf("Adjust: %v", err)
	}
	// rollover: saved = 1+3 = 4, today = 10; the +4 lands on today
	if !almost(today, 14) || !almost(saved, 4) {
		t.Errorf("Adjust = (%v, %v), want (14, 4)", today, saved)
	}
}

// TestAdjustDebtEatenByNextDay: the debt adjust_minutes leaves is exactly
// what the next day's rollover eats (portal: "it'll be eaten by tomorrow's
// refill").
func TestAdjustDebtEatenByNextDay(t *testing.T) {
	clock := &fakeClock{t: atZone(2026, 10, 26, 20, 0, 0, est)}
	b, _ := newTestBudget(t, WithClock(clock.now))
	kid := mustCreateKid(t, b, "alex", "1111")
	importAccount(t, b, kid, 0, 1, "2026-10-26")
	today, saved, err := b.Adjust(ctx, kid, DefaultCounter, -3, true)
	if err != nil {
		t.Fatalf("Adjust: %v", err)
	}
	if !almost(today, -2) || !almost(saved, 0) {
		t.Errorf("Adjust = (%v, %v), want (-2, 0)", today, saved)
	}
	clock.t = atZone(2026, 10, 27, 8, 0, 0, est)
	today, saved, err = b.Balance(ctx, kid, DefaultCounter)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if !almost(today, 8) || !almost(saved, 0) {
		t.Errorf("next day Balance = (%v, %v), want (8, 0)", today, saved)
	}
}

// TestConsume is a table test ported from the legacy consume_progress
// (clamp, saved-before-today, cut-off, never below zero) extended with the
// new bin rates (Fun 1.0, Learning 0.5, Free 0). The stored day equals the
// clock day, so no rollover interferes.
func TestConsume(t *testing.T) {
	tests := []struct {
		name                 string
		today, saved         float64
		seconds, rate        float64
		wantToday, wantSaved float64
		wantCut              bool
	}{
		{"fun rate charges full", 2, 8, 5, RateFun, 2, 8 - 5.0/60, false},
		{"learning rate charges half", 2, 8, 5, RateLearning, 2, 8 - 2.5/60, false},
		{"free rate charges nothing", 2, 8, 5, RateFree, 2, 8, false},
		{"saved before today", 1, 5.0 / 60, 5, RateFun, 1, 0, false},
		{"tick bigger than saved spills into today", 4, 0.02, 5, RateFun, 4 - (5.0/60 - 0.02), 0, false},
		{"clamps a huge tick to five seconds", 0, 2, 1000, RateFun, 0, 2 - 5.0/60, false},
		{"clamps negative seconds to zero", 1, 1, -5, RateFun, 1, 1, false},
		{"cuts off on exact exhaustion", 5.0 / 60, 0, 5, RateFun, 0, 0, true},
		{"cuts off when the tick exceeds the balance", 0.02, 0, 5, RateFun, 0, 0, true},
		{"cuts off at zero balance", 0, 0, 5, RateFun, 0, 0, true},
		{"cuts off at negative balance", -2, 0, 5, RateFun, -2, 0, true},
		{"free never cuts off at zero", 0, 0, 5, RateFree, 0, 0, false},
		{"free never cuts off at negative balance", -2, 0, 5, RateFree, -2, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _ := newTestBudget(t, at(nowOct26))
			kid := mustCreateKid(t, b, "alex", "1111")
			importAccount(t, b, kid, tt.today, tt.saved, "2026-10-26")
			today, saved, cut, err := b.Consume(ctx, kid, DefaultCounter, tt.seconds, tt.rate)
			if err != nil {
				t.Fatalf("Consume: %v", err)
			}
			if !almost(today, tt.wantToday) || !almost(saved, tt.wantSaved) || cut != tt.wantCut {
				t.Errorf("Consume = (%v, %v, cut=%v), want (%v, %v, cut=%v)",
					today, saved, cut, tt.wantToday, tt.wantSaved, tt.wantCut)
			}
		})
	}
}

// TestConsumeMaxTickBoundary: the clamp is inclusive — exactly 5 seconds is
// the largest single tick.
func TestConsumeMaxTickBoundary(t *testing.T) {
	b, _ := newTestBudget(t, at(nowOct26))
	kid := mustCreateKid(t, b, "alex", "1111")
	importAccount(t, b, kid, 0, 1, "2026-10-26")
	today, saved, cut, err := b.Consume(ctx, kid, DefaultCounter, 5, RateFun)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if !almost(today, 0) || !almost(saved, 1-5.0/60) || cut {
		t.Errorf("Consume = (%v, %v, cut=%v), want (0, %v, false)", today, saved, cut, 1-5.0/60)
	}
}

// TestConsumeAppliesRolloverFirst: metering always rolls before charging,
// exactly like the legacy portal (consume_progress rolls, then decrements).
func TestConsumeAppliesRolloverFirst(t *testing.T) {
	b, _ := newTestBudget(t, at(nowOct26))
	kid := mustCreateKid(t, b, "alex", "1111")
	importAccount(t, b, kid, 3, 1, "2026-10-25")
	today, saved, cut, err := b.Consume(ctx, kid, DefaultCounter, 5, RateFun)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	// rollover: saved = 1+3 = 4, today = 10; the 5s tick takes 5/60 from saved
	if !almost(today, 10) || !almost(saved, 4-5.0/60) || cut {
		t.Errorf("Consume = (%v, %v, cut=%v), want (10, %v, false)", today, saved, cut, 4-5.0/60)
	}
}

// TestCredit: practice earnings land on today after the day rollover,
// idempotently per award id, and counters stay separate accounts.
func TestCredit(t *testing.T) {
	b, _ := newTestBudget(t, at(nowOct26))
	kid := mustCreateKid(t, b, "alex", "1111")
	if err := b.Credit(ctx, string(kid), DefaultCounter, 4, "award-1"); err != nil {
		t.Fatalf("Credit: %v", err)
	}
	today, saved, err := b.Balance(ctx, kid, DefaultCounter)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if !almost(today, 14) || !almost(saved, 0) {
		t.Errorf("after first credit = (%v, %v), want (14, 0)", today, saved)
	}
	// retrying the same award counts once
	if err := b.Credit(ctx, string(kid), DefaultCounter, 4, "award-1"); err != nil {
		t.Fatalf("Credit retry: %v", err)
	}
	today, saved, _ = b.Balance(ctx, kid, DefaultCounter)
	if !almost(today, 14) || !almost(saved, 0) {
		t.Errorf("after retry = (%v, %v), want (14, 0)", today, saved)
	}
	// a different award credits again
	if err := b.Credit(ctx, string(kid), DefaultCounter, 2, "award-2"); err != nil {
		t.Fatalf("Credit: %v", err)
	}
	today, saved, _ = b.Balance(ctx, kid, DefaultCounter)
	if !almost(today, 16) || !almost(saved, 0) {
		t.Errorf("after second award = (%v, %v), want (16, 0)", today, saved)
	}
	// another counter is a separate account
	if err := b.Credit(ctx, string(kid), "games", 3, "award-3"); err != nil {
		t.Fatalf("Credit: %v", err)
	}
	today, saved, _ = b.Balance(ctx, kid, "games")
	if !almost(today, 13) || !almost(saved, 0) {
		t.Errorf("games counter = (%v, %v), want (13, 0)", today, saved)
	}
	today, saved, _ = b.Balance(ctx, kid, DefaultCounter)
	if !almost(today, 16) || !almost(saved, 0) {
		t.Errorf("video counter after games credit = (%v, %v), want (16, 0)", today, saved)
	}
}

// TestCreditIdempotencySurvivesRestart: the award log is stored, so a retry
// after a restart still counts once.
func TestCreditIdempotencySurvivesRestart(t *testing.T) {
	b, s := newTestBudget(t, at(nowOct26))
	kid := mustCreateKid(t, b, "alex", "1111")
	if err := b.Credit(ctx, string(kid), DefaultCounter, 4, "award-1"); err != nil {
		t.Fatalf("Credit: %v", err)
	}
	b2, err := New(s, at(nowOct26))
	if err != nil {
		t.Fatalf("budget.New: %v", err)
	}
	if err := b2.Credit(ctx, string(kid), DefaultCounter, 4, "award-1"); err != nil {
		t.Fatalf("Credit retry: %v", err)
	}
	today, saved, err := b.Balance(ctx, kid, DefaultCounter)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if !almost(today, 14) || !almost(saved, 0) {
		t.Errorf("after restart retry = (%v, %v), want (14, 0)", today, saved)
	}
}

// TestCreditAppliesRolloverFirst: credits land on today after the day
// rollover, so a stale account first rolls.
func TestCreditAppliesRolloverFirst(t *testing.T) {
	b, _ := newTestBudget(t, at(nowOct26))
	kid := mustCreateKid(t, b, "alex", "1111")
	importAccount(t, b, kid, 3, 1, "2026-10-25")
	if err := b.Credit(ctx, string(kid), DefaultCounter, 2, "award-1"); err != nil {
		t.Fatalf("Credit: %v", err)
	}
	today, saved, err := b.Balance(ctx, kid, DefaultCounter)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	// rollover: saved = 1+3 = 4, today = 10; the +2 lands on today
	if !almost(today, 12) || !almost(saved, 4) {
		t.Errorf("Balance = (%v, %v), want (12, 4)", today, saved)
	}
}

// TestCreditEmptyAwardIDAlwaysCredits: with no award id there is no key to
// deduplicate on, so every call credits.
func TestCreditEmptyAwardIDAlwaysCredits(t *testing.T) {
	b, _ := newTestBudget(t, at(nowOct26))
	kid := mustCreateKid(t, b, "alex", "1111")
	if err := b.Credit(ctx, string(kid), DefaultCounter, 1, ""); err != nil {
		t.Fatalf("Credit: %v", err)
	}
	if err := b.Credit(ctx, string(kid), DefaultCounter, 1, ""); err != nil {
		t.Fatalf("Credit: %v", err)
	}
	today, saved, err := b.Balance(ctx, kid, DefaultCounter)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if !almost(today, 12) || !almost(saved, 0) {
		t.Errorf("Balance = (%v, %v), want (12, 0)", today, saved)
	}
}

// TestImportAccountSetsExactLegacyState: the stored day, today and saved come
// across verbatim, so an account imported late in a day keeps the legacy
// session's remaining allowance instead of being reset.
func TestImportAccountSetsExactLegacyState(t *testing.T) {
	b, _ := newTestBudget(t, at(nowOct26))
	kid := mustCreateKid(t, b, "alex", "1111")
	importAccount(t, b, kid, 7.5, 3.25, "2026-10-26")
	today, saved, err := b.Balance(ctx, kid, DefaultCounter)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if !almost(today, 7.5) || !almost(saved, 3.25) {
		t.Errorf("Balance = (%v, %v), want (7.5, 3.25)", today, saved)
	}
}

// TestImportAccountResumesRolloverFromStoredDay: an account imported with a
// stale day rolls at the next local midnight exactly like a live account.
func TestImportAccountResumesRolloverFromStoredDay(t *testing.T) {
	b, _ := newTestBudget(t, at(nowOct26))
	kid := mustCreateKid(t, b, "alex", "1111")
	importAccount(t, b, kid, 2, 8, "2026-10-25")
	today, saved, err := b.Balance(ctx, kid, DefaultCounter)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if !almost(today, 10) || !almost(saved, 10) {
		t.Errorf("Balance = (%v, %v), want (10, 10)", today, saved)
	}
}

// TestImportAccountOverwrites: re-importing replaces the account wholesale.
func TestImportAccountOverwrites(t *testing.T) {
	b, _ := newTestBudget(t, at(nowOct26))
	kid := mustCreateKid(t, b, "alex", "1111")
	importAccount(t, b, kid, 5, 5, "2026-10-26")
	importAccount(t, b, kid, 1, 2, "2026-10-26")
	today, saved, err := b.Balance(ctx, kid, DefaultCounter)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if !almost(today, 1) || !almost(saved, 2) {
		t.Errorf("Balance = (%v, %v), want (1, 2)", today, saved)
	}
}

// TestEmptyCounterRejected: no operation accepts an empty counter name.
func TestEmptyCounterRejected(t *testing.T) {
	b, _ := newTestBudget(t, at(nowOct26))
	kid := mustCreateKid(t, b, "alex", "1111")
	if _, _, err := b.Balance(ctx, kid, ""); err == nil {
		t.Error("Balance with empty counter: want error")
	}
	if _, _, err := b.Adjust(ctx, kid, "", 1, false); err == nil {
		t.Error("Adjust with empty counter: want error")
	}
	if _, _, _, err := b.Consume(ctx, kid, "", 1, RateFun); err == nil {
		t.Error("Consume with empty counter: want error")
	}
	if err := b.Credit(ctx, string(kid), "", 1, "a1"); err == nil {
		t.Error("Credit with empty counter: want error")
	}
}

// TestUnknownKidErrors: every kid-addressed operation reports ErrNoSuchKid
// and verification never succeeds.
func TestUnknownKidErrors(t *testing.T) {
	b, _ := newTestBudget(t, at(nowOct26))
	if _, _, err := b.Balance(ctx, ghostID, DefaultCounter); !errors.Is(err, ErrNoSuchKid) {
		t.Errorf("Balance: err = %v, want ErrNoSuchKid", err)
	}
	if _, _, err := b.Adjust(ctx, ghostID, DefaultCounter, 1, false); !errors.Is(err, ErrNoSuchKid) {
		t.Errorf("Adjust: err = %v, want ErrNoSuchKid", err)
	}
	if _, _, _, err := b.Consume(ctx, ghostID, DefaultCounter, 1, RateFun); !errors.Is(err, ErrNoSuchKid) {
		t.Errorf("Consume: err = %v, want ErrNoSuchKid", err)
	}
	if err := b.Credit(ctx, string(ghostID), DefaultCounter, 1, "a1"); !errors.Is(err, ErrNoSuchKid) {
		t.Errorf("Credit: err = %v, want ErrNoSuchKid", err)
	}
	if err := b.ImportAccount(ctx, ghostID, 1, 1, "2026-10-26"); !errors.Is(err, ErrNoSuchKid) {
		t.Errorf("ImportAccount: err = %v, want ErrNoSuchKid", err)
	}
	if b.VerifyKidPIN(ctx, ghostID, "1111") {
		t.Error("VerifyKidPIN on an unknown kid: want false")
	}
}
