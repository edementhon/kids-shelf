package budget

import (
	"errors"
	"testing"
)

// TestBudgetsOnDefaultTrue: the household budget switch starts on.
func TestBudgetsOnDefaultTrue(t *testing.T) {
	b, _ := newTestBudget(t, at(nowOct26))
	on, err := b.BudgetsOn(ctx)
	if err != nil {
		t.Fatalf("BudgetsOn: %v", err)
	}
	if !on {
		t.Error("BudgetsOn = false, want true")
	}
}

// TestSetBudgetsOnToggle: the switch flips both ways.
func TestSetBudgetsOnToggle(t *testing.T) {
	b, _ := newTestBudget(t, at(nowOct26))
	if err := b.SetBudgetsOn(ctx, false); err != nil {
		t.Fatalf("SetBudgetsOn(false): %v", err)
	}
	on, err := b.BudgetsOn(ctx)
	if err != nil || on {
		t.Errorf("BudgetsOn = %v, %v; want false, nil", on, err)
	}
	if err := b.SetBudgetsOn(ctx, true); err != nil {
		t.Fatalf("SetBudgetsOn(true): %v", err)
	}
	on, _ = b.BudgetsOn(ctx)
	if !on {
		t.Error("BudgetsOn = false after re-enabling, want true")
	}
}

// TestSafariScreensDefaultTrueAndToggle: the household "some screens use
// Safari" setting starts on and flips both ways.
func TestSafariScreensDefaultTrueAndToggle(t *testing.T) {
	b, _ := newTestBudget(t, at(nowOct26))
	on, err := b.SafariScreens(ctx)
	if err != nil {
		t.Fatalf("SafariScreens: %v", err)
	}
	if !on {
		t.Error("SafariScreens = false, want true")
	}
	if err := b.SetSafariScreens(ctx, false); err != nil {
		t.Fatalf("SetSafariScreens(false): %v", err)
	}
	on, err = b.SafariScreens(ctx)
	if err != nil || on {
		t.Errorf("SafariScreens = %v, %v; want false, nil", on, err)
	}
	if err := b.SetSafariScreens(ctx, true); err != nil {
		t.Fatalf("SetSafariScreens(true): %v", err)
	}
	on, _ = b.SafariScreens(ctx)
	if !on {
		t.Error("SafariScreens = false after re-enabling, want true")
	}
}

// TestBudgetsOffChargesNothingAndNeverCutsOff: with the switch off, Consume
// charges nothing and reports no cut-off, even at a zero balance with a Fun
// rate that would otherwise cut off.
func TestBudgetsOffChargesNothingAndNeverCutsOff(t *testing.T) {
	b, _ := newTestBudget(t, at(nowOct26))
	kid := mustCreateKid(t, b, "alex", "1111")
	importAccount(t, b, kid, 0, 0.5, "2026-10-26")
	if err := b.SetBudgetsOn(ctx, false); err != nil {
		t.Fatalf("SetBudgetsOn: %v", err)
	}
	today, saved, cut, err := b.Consume(ctx, kid, DefaultCounter, 5, RateFun)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if !almost(today, 0) || !almost(saved, 0.5) || cut {
		t.Errorf("Consume = (%v, %v, cut=%v), want (0, 0.5, false)", today, saved, cut)
	}
	importAccount(t, b, kid, 0, 0, "2026-10-26")
	today, saved, cut, err = b.Consume(ctx, kid, DefaultCounter, 5, RateFun)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if !almost(today, 0) || !almost(saved, 0) || cut {
		t.Errorf("Consume at zero = (%v, %v, cut=%v), want (0, 0, false)", today, saved, cut)
	}
}

// TestBudgetsReenabledMetersAgain: the switch only stops metering while it
// is off.
func TestBudgetsReenabledMetersAgain(t *testing.T) {
	b, _ := newTestBudget(t, at(nowOct26))
	kid := mustCreateKid(t, b, "alex", "1111")
	importAccount(t, b, kid, 0, 0.5, "2026-10-26")
	if err := b.SetBudgetsOn(ctx, false); err != nil {
		t.Fatalf("SetBudgetsOn: %v", err)
	}
	if _, _, cut, err := b.Consume(ctx, kid, DefaultCounter, 5, RateFun); err != nil || cut {
		t.Fatalf("Consume while off: cut=%v, err=%v; want false, nil", cut, err)
	}
	if err := b.SetBudgetsOn(ctx, true); err != nil {
		t.Fatalf("SetBudgetsOn: %v", err)
	}
	today, saved, cut, err := b.Consume(ctx, kid, DefaultCounter, 5, RateFun)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if !almost(today, 0) || !almost(saved, 0.5-5.0/60) || cut {
		t.Errorf("Consume after re-enable = (%v, %v, cut=%v), want (0, %v, false)",
			today, saved, cut, 0.5-5.0/60)
	}
}

// TestDefaultMinutes: the household default starts at 10 and the new value
// applies at the next rollover.
func TestDefaultMinutes(t *testing.T) {
	b, _ := newTestBudget(t, at(nowOct26))
	m, err := b.DefaultMinutes(ctx)
	if err != nil {
		t.Fatalf("DefaultMinutes: %v", err)
	}
	if !almost(m, DefaultDailyMinutes) {
		t.Errorf("DefaultMinutes = %v, want %v", m, DefaultDailyMinutes)
	}
	if err := b.SetDefaultMinutes(ctx, 15); err != nil {
		t.Fatalf("SetDefaultMinutes: %v", err)
	}
	m, err = b.DefaultMinutes(ctx)
	if err != nil || !almost(m, 15) {
		t.Errorf("DefaultMinutes = %v, %v; want 15, nil", m, err)
	}
	kid := mustCreateKid(t, b, "alex", "1111")
	importAccount(t, b, kid, 2, 0, "2026-10-25")
	today, saved, err := b.Balance(ctx, kid, DefaultCounter)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if !almost(today, 15) || !almost(saved, 2) {
		t.Errorf("Balance = (%v, %v), want (15, 2)", today, saved)
	}
}

// TestSetDefaultMinutesRejectsNegative: an allowance cannot be negative.
func TestSetDefaultMinutesRejectsNegative(t *testing.T) {
	b, _ := newTestBudget(t, at(nowOct26))
	if err := b.SetDefaultMinutes(ctx, -1); err == nil {
		t.Error("SetDefaultMinutes(-1): want error")
	}
}

// TestDailyMinutesOverride: a per-kid override wins over the household
// default; kids without one keep the default.
func TestDailyMinutesOverride(t *testing.T) {
	b, _ := newTestBudget(t, at(nowOct26))
	kid := mustCreateKid(t, b, "alex", "1111")
	m, err := b.DailyMinutes(ctx, kid)
	if err != nil || !almost(m, DefaultDailyMinutes) {
		t.Errorf("DailyMinutes = %v, %v; want %v, nil", m, err, DefaultDailyMinutes)
	}
	if err := b.SetDailyMinutes(ctx, kid, 25); err != nil {
		t.Fatalf("SetDailyMinutes: %v", err)
	}
	m, err = b.DailyMinutes(ctx, kid)
	if err != nil || !almost(m, 25) {
		t.Errorf("DailyMinutes = %v, %v; want 25, nil", m, err)
	}
	importAccount(t, b, kid, 2, 0, "2026-10-25")
	today, saved, err := b.Balance(ctx, kid, DefaultCounter)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if !almost(today, 25) || !almost(saved, 2) {
		t.Errorf("Balance = (%v, %v), want (25, 2)", today, saved)
	}
	other := mustCreateKid(t, b, "sam", "2222")
	m, err = b.DailyMinutes(ctx, other)
	if err != nil || !almost(m, DefaultDailyMinutes) {
		t.Errorf("DailyMinutes(other) = %v, %v; want %v, nil", m, err, DefaultDailyMinutes)
	}
	importAccount(t, b, other, 2, 0, "2026-10-25")
	today, saved, err = b.Balance(ctx, other, DefaultCounter)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if !almost(today, 10) || !almost(saved, 2) {
		t.Errorf("other Balance = (%v, %v), want (10, 2)", today, saved)
	}
}

// TestDailyMinutesZeroAllowance: an override of 0 grants no allowance; the
// leftover from yesterday still rolls into saved.
func TestDailyMinutesZeroAllowance(t *testing.T) {
	b, _ := newTestBudget(t, at(nowOct26))
	kid := mustCreateKid(t, b, "alex", "1111")
	if err := b.SetDailyMinutes(ctx, kid, 0); err != nil {
		t.Fatalf("SetDailyMinutes: %v", err)
	}
	importAccount(t, b, kid, 1, 0, "2026-10-25")
	today, saved, err := b.Balance(ctx, kid, DefaultCounter)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if !almost(today, 0) || !almost(saved, 1) {
		t.Errorf("Balance = (%v, %v), want (0, 1)", today, saved)
	}
}

// TestDailyMinutesRejectsNegativeAndUnknownKid.
func TestDailyMinutesRejectsNegativeAndUnknownKid(t *testing.T) {
	b, _ := newTestBudget(t, at(nowOct26))
	kid := mustCreateKid(t, b, "alex", "1111")
	if err := b.SetDailyMinutes(ctx, kid, -1); err == nil {
		t.Error("SetDailyMinutes(-1): want error")
	}
	if _, err := b.DailyMinutes(ctx, ghostID); !errors.Is(err, ErrNoSuchKid) {
		t.Errorf("DailyMinutes(unknown): err = %v, want ErrNoSuchKid", err)
	}
	if err := b.SetDailyMinutes(ctx, ghostID, 5); !errors.Is(err, ErrNoSuchKid) {
		t.Errorf("SetDailyMinutes(unknown): err = %v, want ErrNoSuchKid", err)
	}
}

// TestDayBoundarySpringForward: the day key is the clock's local date, so a
// 23-hour DST day rolls exactly once — at the local date change, never at an
// arbitrary elapsed-time horizon.
func TestDayBoundarySpringForward(t *testing.T) {
	clock := &fakeClock{t: atZone(2026, 3, 7, 23, 59, 0, est)}
	b, _ := newTestBudget(t, WithClock(clock.now))
	kid := mustCreateKid(t, b, "alex", "1111")
	importAccount(t, b, kid, 2, 1, "2026-03-07")

	// just before EST midnight: same day, no rollover
	today, saved, err := b.Balance(ctx, kid, DefaultCounter)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if !almost(today, 2) || !almost(saved, 1) {
		t.Errorf("Balance = (%v, %v), want (2, 1)", today, saved)
	}

	// across EST midnight: the day rolls
	clock.t = atZone(2026, 3, 8, 0, 1, 0, est)
	today, saved, err = b.Balance(ctx, kid, DefaultCounter)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if !almost(today, 10) || !almost(saved, 3) {
		t.Errorf("Balance = (%v, %v), want (10, 3)", today, saved)
	}

	// spring forward: 03:00 EDT is still March 8 — no second rollover
	clock.t = atZone(2026, 3, 8, 3, 0, 0, edt)
	today, saved, err = b.Balance(ctx, kid, DefaultCounter)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if !almost(today, 10) || !almost(saved, 3) {
		t.Errorf("Balance = (%v, %v), want (10, 3)", today, saved)
	}

	// a full 23-hour day later: the next local midnight rolls once
	clock.t = atZone(2026, 3, 9, 0, 1, 0, edt)
	today, saved, err = b.Balance(ctx, kid, DefaultCounter)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if !almost(today, 10) || !almost(saved, 13) {
		t.Errorf("Balance = (%v, %v), want (10, 13)", today, saved)
	}
}

// TestDayBoundaryFallBack: the repeated 01:00 hour of a 25-hour fall-back day
// must not roll twice; the roll happens at the next local midnight.
func TestDayBoundaryFallBack(t *testing.T) {
	clock := &fakeClock{t: atZone(2026, 10, 31, 23, 59, 0, edt)}
	b, _ := newTestBudget(t, WithClock(clock.now))
	kid := mustCreateKid(t, b, "alex", "1111")
	importAccount(t, b, kid, 1, 0, "2026-10-31")

	today, saved, err := b.Balance(ctx, kid, DefaultCounter)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if !almost(today, 1) || !almost(saved, 0) {
		t.Errorf("Balance = (%v, %v), want (1, 0)", today, saved)
	}

	// the midnight roll on Nov 1
	clock.t = atZone(2026, 11, 1, 0, 30, 0, edt)
	today, saved, err = b.Balance(ctx, kid, DefaultCounter)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if !almost(today, 10) || !almost(saved, 1) {
		t.Errorf("Balance = (%v, %v), want (10, 1)", today, saved)
	}

	// fall back: the 01:00 hour repeats in EST, same local date — no roll
	clock.t = atZone(2026, 11, 1, 1, 30, 0, est)
	today, saved, err = b.Balance(ctx, kid, DefaultCounter)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if !almost(today, 10) || !almost(saved, 1) {
		t.Errorf("Balance = (%v, %v), want (10, 1)", today, saved)
	}

	// the next local midnight rolls again
	clock.t = atZone(2026, 11, 2, 0, 1, 0, est)
	today, saved, err = b.Balance(ctx, kid, DefaultCounter)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if !almost(today, 10) || !almost(saved, 11) {
		t.Errorf("Balance = (%v, %v), want (10, 11)", today, saved)
	}
}
