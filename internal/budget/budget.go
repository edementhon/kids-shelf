// Package budget owns the kids' screen-time accounts and the kid profiles
// they belong to: per-kid daily allowances with a debt-aware rollover at the
// machine's local midnight, parent adjustments, live metering (playback
// seconds billed at a bin rate) and the kid PINs.
//
// It ports the account math of the legacy portal (legacy/portal/server.py:
// _rollover, adjust_minutes, consume_progress) with the deliberate changes
// from PLAN.md section 1 and DECISIONS.md: charging happens on the server at
// playback-seconds x bin rate (Fun 1.0, Learning 0.5, Free 0) instead of the
// display-only discount; the day boundary follows the machine's local clock
// instead of US Eastern; kids, PINs and settings live in the database
// instead of constants; and budgets can be switched off per household.
package budget

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/edementhon/kids-shelf/internal/core"
	"github.com/edementhon/kids-shelf/internal/store"
)

// Constants for the counters, the daily allowance and the bin rates.
const (
	// DefaultCounter is where playback is metered and practice earnings land
	// unless the parent points them at another counter (PLAN.md section 1a:
	// "practice → budget (credit N minutes to a counter)"). The legacy portal
	// had exactly one account per kid; it imports onto this counter.
	DefaultCounter = "video"

	// DefaultDailyMinutes is the per-kid daily allowance when the household
	// has not changed the default (the legacy portal's DAILY_MINUTES).
	DefaultDailyMinutes = 10.0

	// MaxConsumeSeconds clamps one live-metering tick: the legacy player
	// sends ~1s per tick, 5s covers the pause/ended flush, and a stuck or
	// tampered client can never zero a balance in one request (portal
	// consume_progress).
	MaxConsumeSeconds = 5.0

	// RateFun, RateLearning and RateFree are the per-minute billing rates of
	// the Fun, Learning and Free bins; the caller passes the rate of the bin
	// the episode is in. A rate <= 0 (RateFree) charges nothing and never
	// cuts off playback.
	RateFun      = 1.0
	RateLearning = 0.5
	RateFree     = 0.0

	// epsilon bounds the float comparisons the account math inherits from
	// the legacy JSON implementation (1e-9 in portal consume_progress).
	epsilon = 1e-9
)

// ErrNoSuchKid is returned by operations that address a kid id that is not
// registered.
var ErrNoSuchKid = errors.New("budget: no such kid")

// Migrations registers the budget tables with the store. One owner is one
// package (store package doc), so no other package owns any of these tables.
var Migrations = []store.Migration{{
	Owner:   "budget",
	Version: 1,
	SQL: `
CREATE TABLE kids (
	id      TEXT PRIMARY KEY,
	name    TEXT NOT NULL UNIQUE,
	pin     TEXT NOT NULL,
	created TEXT NOT NULL
);
CREATE TABLE household (
	id              INTEGER PRIMARY KEY CHECK (id = 1),
	budgets_on      INTEGER NOT NULL DEFAULT 1,
	default_minutes REAL    NOT NULL DEFAULT 10,
	grownup_pin     TEXT    NOT NULL DEFAULT ''
);
CREATE TABLE kid_settings (
	kid           TEXT PRIMARY KEY REFERENCES kids(id) ON DELETE CASCADE,
	daily_minutes REAL NOT NULL
);
CREATE TABLE accounts (
	kid     TEXT    NOT NULL REFERENCES kids(id) ON DELETE CASCADE,
	counter TEXT    NOT NULL,
	day     TEXT    NOT NULL DEFAULT '',
	today   REAL    NOT NULL DEFAULT 0,
	saved   REAL    NOT NULL DEFAULT 0,
	PRIMARY KEY (kid, counter)
);
CREATE TABLE credits (
	award_id TEXT PRIMARY KEY,
	kid      TEXT    NOT NULL REFERENCES kids(id) ON DELETE CASCADE,
	counter  TEXT    NOT NULL,
	minutes  REAL    NOT NULL,
	created  TEXT    NOT NULL
);
`,
}}

// Budget owns the kid profiles and their screen-time accounts, backed by the
// budget tables. It implements core.Kids, core.Crediter and
// core.BudgetImporter so migrate and practice reach it through the phase-1
// interfaces, and the web package can use it directly for the dashboard.
type Budget struct {
	store *store.Store
	now   func() time.Time
}

// Option configures a Budget.
type Option func(*Budget)

// WithClock substitutes the clock that fixes the day boundary: the local
// date (2006-01-02) of the time it reports. The default is time.Now, so the
// boundary is the machine's local midnight (DECISIONS.md: the day boundary
// follows the server machine's clock). Tests use a fixed or hand-advanced
// clock.
func WithClock(now func() time.Time) Option {
	return func(b *Budget) { b.now = now }
}

// New returns a Budget on s, applying the budget migrations and seeding the
// single household settings row (budgets on, DefaultDailyMinutes, no
// grown-up PIN) if it is absent. It is safe (and a no-op) to call more than
// once.
func New(s *store.Store, opts ...Option) (*Budget, error) {
	if err := s.Migrate(Migrations...); err != nil {
		return nil, err
	}
	if _, err := s.DB().Exec(`INSERT OR IGNORE INTO household (id) VALUES (1)`); err != nil {
		return nil, fmt.Errorf("budget: seed settings: %w", err)
	}
	b := &Budget{store: s, now: time.Now}
	for _, o := range opts {
		o(b)
	}
	return b, nil
}

// day is the account day key: the clock's local date, so the day boundary is
// the machine's local midnight.
func (b *Budget) day() string {
	return b.now().Format("2006-01-02")
}

func (b *Budget) begin(ctx context.Context) (*sql.Tx, error) {
	tx, err := b.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("budget: begin: %w", err)
	}
	return tx, nil
}

// requireKid reports ErrNoSuchKid unless a kid with this id is registered.
// Every balance operation calls it first, so a typo'd kid id can never
// silently mint an account row (whose kid foreign key would reject it
// anyway).
func (b *Budget) requireKid(tx *sql.Tx, kid string) error {
	var one int
	err := tx.QueryRow(`SELECT 1 FROM kids WHERE id = ?`, kid).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNoSuchKid
	}
	if err != nil {
		return fmt.Errorf("budget: look up kid: %w", err)
	}
	return nil
}

// dailyAllowance is the kid's allowance for one day: the per-kid override
// when one is set, else the household default.
func (b *Budget) dailyAllowance(tx *sql.Tx, kid string) (float64, error) {
	var m float64
	err := tx.QueryRow(`SELECT daily_minutes FROM kid_settings WHERE kid = ?`, kid).Scan(&m)
	if err == nil {
		return m, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("budget: read kid settings: %w", err)
	}
	var def float64
	if err := tx.QueryRow(`SELECT default_minutes FROM household WHERE id = 1`).Scan(&def); err != nil {
		return 0, fmt.Errorf("budget: read household default: %w", err)
	}
	return def, nil
}

// rollover brings kid's account on counter up to day, exactly like the legacy
// portal _rollover: leftover non-negative today rolls into saved; a negative
// today (parent deduction or unpaid debt) is debt, added to the new day's
// allowance; debt bigger than a full day's allowance rides into saved, which
// may go negative. The account row is created on first touch, so the first
// balance read of a day grants that day's allowance. Returns the
// post-rollover balances.
func (b *Budget) rollover(tx *sql.Tx, kid, counter, day string) (today, saved float64, err error) {
	allow, err := b.dailyAllowance(tx, kid)
	if err != nil {
		return 0, 0, err
	}
	var dayRow string
	var todayRow, savedRow float64
	err = tx.QueryRow(`SELECT day, today, saved FROM accounts WHERE kid = ? AND counter = ?`, kid, counter).
		Scan(&dayRow, &todayRow, &savedRow)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		dayRow, todayRow, savedRow = "", 0, 0
	case err != nil:
		return 0, 0, fmt.Errorf("budget: read account: %w", err)
	case dayRow == day:
		return todayRow, savedRow, nil
	}
	debt := math.Min(todayRow, 0)
	carry := math.Max(todayRow, 0)
	today = allow + debt
	saved = savedRow + carry
	if today < 0 {
		// debt exceeded a full day's allowance; let the remainder ride
		saved += today
	}
	if _, err := tx.Exec(`INSERT INTO accounts (kid, counter, day, today, saved) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(kid, counter) DO UPDATE SET day = excluded.day, today = excluded.today, saved = excluded.saved`,
		kid, counter, day, today, saved); err != nil {
		return 0, 0, fmt.Errorf("budget: store account: %w", err)
	}
	return today, saved, nil
}

// Balance returns the kid's (today, saved) balance on counter, applying the
// day-boundary rollover first (a fresh account starts the day with the daily
// allowance).
func (b *Budget) Balance(ctx context.Context, kid core.KidID, counter string) (float64, float64, error) {
	if counter == "" {
		return 0, 0, errors.New("budget: empty counter")
	}
	tx, err := b.begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	if err := b.requireKid(tx, string(kid)); err != nil {
		return 0, 0, err
	}
	today, saved, err := b.rollover(tx, string(kid), counter, b.day())
	if err != nil {
		return 0, 0, err
	}
	return today, saved, tx.Commit()
}

// Adjust applies a parent delta to kid's balance on counter, after the day
// rollover, exactly like the legacy adjust_minutes: a positive delta adds to
// today (it rolls into saved at the next day boundary); a negative delta
// takes today first, then saved; when the deduction exceeds the balance and
// allowNegative is false the balance is clamped at zero, and when it is true
// the remainder rides as a negative debt on today that the next day's
// allowance must eat. Returns the resulting balances.
func (b *Budget) Adjust(ctx context.Context, kid core.KidID, counter string, delta float64, allowNegative bool) (today, saved float64, err error) {
	if counter == "" {
		return 0, 0, errors.New("budget: empty counter")
	}
	tx, err := b.begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	if err := b.requireKid(tx, string(kid)); err != nil {
		return 0, 0, err
	}
	today, saved, err = b.rollover(tx, string(kid), counter, b.day())
	if err != nil {
		return 0, 0, err
	}
	if delta >= 0 {
		today += delta
	} else {
		take := -delta
		fromToday := math.Min(today, take)
		today -= fromToday
		saved -= take - fromToday
		if saved < 0 {
			if allowNegative {
				today += saved
			}
			saved = 0
		}
	}
	if _, err := tx.Exec(`UPDATE accounts SET today = ?, saved = ? WHERE kid = ? AND counter = ?`,
		today, saved, string(kid), counter); err != nil {
		return 0, 0, fmt.Errorf("budget: adjust: %w", err)
	}
	return today, saved, tx.Commit()
}

// Consume meters `seconds` of watched playback on counter at the bin's rate
// (RateFun, RateLearning or RateFree; the caller passes the rate of the bin
// the episode is in) and returns the resulting (today, saved) plus cutOff —
// whether the balance hit zero, in which case the player should cut playback.
// The seconds are clamped to MaxConsumeSeconds per call (the legacy player's
// 1s ticks and 5s flush) and charged as seconds/60*rate minutes, saved
// first, then today, never below zero. A Free rate (<= 0) and budgets
// switched off charge nothing and never report cutOff, even when the balance
// is zero or negative.
func (b *Budget) Consume(ctx context.Context, kid core.KidID, counter string, seconds, rate float64) (today, saved float64, cutOff bool, err error) {
	if counter == "" {
		return 0, 0, false, errors.New("budget: empty counter")
	}
	tx, err := b.begin(ctx)
	if err != nil {
		return 0, 0, false, err
	}
	defer tx.Rollback()
	if err := b.requireKid(tx, string(kid)); err != nil {
		return 0, 0, false, err
	}
	today, saved, err = b.rollover(tx, string(kid), counter, b.day())
	if err != nil {
		return 0, 0, false, err
	}
	on, err := b.budgetsOn(tx)
	if err != nil {
		return 0, 0, false, err
	}
	if !on || rate <= 0 {
		return today, saved, false, tx.Commit()
	}
	seconds = math.Min(math.Max(seconds, 0), MaxConsumeSeconds)
	minutes := seconds / 60.0 * rate
	avail := saved + today
	if avail <= epsilon {
		// out of minutes before this tick (the charge is zero but they are
		// cut off), matching the legacy portal
		return today, saved, true, tx.Commit()
	}
	fromSaved := math.Min(saved, minutes)
	saved -= fromSaved
	today -= minutes - fromSaved
	if today < 0 {
		today = 0
	}
	cutOff = saved+today <= epsilon
	if _, err := tx.Exec(`UPDATE accounts SET today = ?, saved = ? WHERE kid = ? AND counter = ?`,
		today, saved, string(kid), counter); err != nil {
		return 0, 0, false, fmt.Errorf("budget: consume: %w", err)
	}
	return today, saved, cutOff, tx.Commit()
}

// Credit implements core.Crediter: it credits `minutes` to kid's balance on
// counter (practice earnings land here, on the parent-chosen counter whose
// default is DefaultCounter). The credit is idempotent by awardID: a second
// Credit with an awardID already recorded in the credits table counts once,
// so a retried practice session is safe, including across restarts. The
// minutes land on today after the day rollover, like a parent adding
// minutes. An empty awardID has no key to remember, so it credits without
// deduplication.
func (b *Budget) Credit(ctx context.Context, kid string, counter string, minutes float64, awardID string) error {
	if counter == "" {
		return errors.New("budget: empty counter")
	}
	if minutes < 0 {
		return fmt.Errorf("budget: negative credit %v", minutes)
	}
	tx, err := b.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := b.requireKid(tx, kid); err != nil {
		return err
	}
	today, saved, err := b.rollover(tx, kid, counter, b.day())
	if err != nil {
		return err
	}
	if awardID != "" {
		var one int
		err := tx.QueryRow(`SELECT 1 FROM credits WHERE award_id = ?`, awardID).Scan(&one)
		if err == nil {
			return tx.Commit() // already credited
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("budget: read credit: %w", err)
		}
		if _, err := tx.Exec(`INSERT INTO credits (award_id, kid, counter, minutes, created) VALUES (?, ?, ?, ?, ?)`,
			awardID, kid, counter, minutes, b.now().UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("budget: record credit: %w", err)
		}
	}
	today += minutes
	if _, err := tx.Exec(`UPDATE accounts SET today = ?, saved = ? WHERE kid = ? AND counter = ?`,
		today, saved, kid, counter); err != nil {
		return fmt.Errorf("budget: apply credit: %w", err)
	}
	return tx.Commit()
}

// ImportAccount implements core.BudgetImporter: it sets the legacy portal
// account exactly as stored — today's balance, the saved bank and the day
// they were last rolled over — so the budget resumes its day-boundary
// rollover from the legacy state. The legacy portal had one account per kid
// (its only counter), so the import lands on DefaultCounter. No rollover is
// applied: the stored day is authoritative until the next local midnight.
func (b *Budget) ImportAccount(ctx context.Context, kid core.KidID, today, saved float64, day string) error {
	tx, err := b.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := b.requireKid(tx, string(kid)); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO accounts (kid, counter, day, today, saved) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(kid, counter) DO UPDATE SET day = excluded.day, today = excluded.today, saved = excluded.saved`,
		string(kid), DefaultCounter, day, today, saved); err != nil {
		return fmt.Errorf("budget: import account: %w", err)
	}
	return tx.Commit()
}

// SetBudgetsOn switches metering for the whole household. Off, Consume
// charges nothing and never reports cutOff. Default is on.
func (b *Budget) SetBudgetsOn(ctx context.Context, on bool) error {
	tx, err := b.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	onInt := 0
	if on {
		onInt = 1
	}
	if _, err := tx.Exec(`UPDATE household SET budgets_on = ? WHERE id = 1`, onInt); err != nil {
		return fmt.Errorf("budget: set budgets switch: %w", err)
	}
	return tx.Commit()
}

// BudgetsOn reports whether metering is switched on for the household.
func (b *Budget) BudgetsOn(ctx context.Context) (bool, error) {
	tx, err := b.begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	return b.budgetsOn(tx)
}

func (b *Budget) budgetsOn(tx *sql.Tx) (bool, error) {
	var v int
	err := tx.QueryRow(`SELECT budgets_on FROM household WHERE id = 1`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil // default on
	}
	if err != nil {
		return false, fmt.Errorf("budget: read budgets switch: %w", err)
	}
	return v != 0, nil
}

// SetDefaultMinutes changes the household daily allowance used for kids
// without a per-kid override. The change takes effect at each kid's next day
// rollover.
func (b *Budget) SetDefaultMinutes(ctx context.Context, minutes float64) error {
	if minutes < 0 {
		return fmt.Errorf("budget: negative daily minutes %v", minutes)
	}
	tx, err := b.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE household SET default_minutes = ? WHERE id = 1`, minutes); err != nil {
		return fmt.Errorf("budget: set default minutes: %w", err)
	}
	return tx.Commit()
}

// DefaultMinutes returns the household daily allowance.
func (b *Budget) DefaultMinutes(ctx context.Context) (float64, error) {
	tx, err := b.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var m float64
	err = tx.QueryRow(`SELECT default_minutes FROM household WHERE id = 1`).Scan(&m)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultDailyMinutes, nil
	}
	if err != nil {
		return 0, fmt.Errorf("budget: read default minutes: %w", err)
	}
	return m, nil
}

// SetDailyMinutes overrides the daily allowance for one kid; 0 is allowed (a
// kid can be given no allowance). The change takes effect at the kid's next
// day rollover; an already-granted balance is not retracted.
func (b *Budget) SetDailyMinutes(ctx context.Context, kid core.KidID, minutes float64) error {
	if minutes < 0 {
		return fmt.Errorf("budget: negative daily minutes %v", minutes)
	}
	tx, err := b.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := b.requireKid(tx, string(kid)); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO kid_settings (kid, daily_minutes) VALUES (?, ?)
		ON CONFLICT(kid) DO UPDATE SET daily_minutes = excluded.daily_minutes`,
		string(kid), minutes); err != nil {
		return fmt.Errorf("budget: set daily minutes: %w", err)
	}
	return tx.Commit()
}

// DailyMinutes returns the kid's daily allowance: the per-kid override when
// set, else the household default.
func (b *Budget) DailyMinutes(ctx context.Context, kid core.KidID) (float64, error) {
	tx, err := b.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := b.requireKid(tx, string(kid)); err != nil {
		return 0, err
	}
	return b.dailyAllowance(tx, string(kid))
}
