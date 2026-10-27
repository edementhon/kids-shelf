package budget

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/edementhon/kids-shelf/internal/core"
	"github.com/edementhon/kids-shelf/internal/store"
)

// ctx is the background context every test uses.
var ctx = context.Background()

// ghostID is a plausible kid id that no test ever registers.
var ghostID = core.KidID("c0ffee00000000000000000000000000")

// nowOct26 is the fixed "current" local time most tests use: noon on
// 2026-10-26, well past that day's midnight rollover.
var nowOct26 = atZone(2026, 10, 26, 12, 0, 0, est)

// newTestBudget opens a fresh store in a temp directory and returns a Budget
// over it. The store is closed when the test ends; to test persistence
// across restarts, call New again on the returned store.
func newTestBudget(t *testing.T, opts ...Option) (*Budget, *store.Store) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "budget.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	b, err := New(s, opts...)
	if err != nil {
		t.Fatalf("budget.New: %v", err)
	}
	return b, s
}

// mustCreateKid registers a kid and fails the test on error.
func mustCreateKid(t *testing.T, b *Budget, name, pin string) core.KidID {
	t.Helper()
	id, err := b.CreateKid(ctx, name, pin)
	if err != nil {
		t.Fatalf("CreateKid(%q): %v", name, err)
	}
	return id
}

// importAccount imports exact legacy state for kid on the default counter,
// failing the test on error.
func importAccount(t *testing.T, b *Budget, kid core.KidID, today, saved float64, day string) {
	t.Helper()
	if err := b.ImportAccount(ctx, kid, today, saved, day); err != nil {
		t.Fatalf("ImportAccount: %v", err)
	}
}

// at pins the clock to one instant.
func at(t time.Time) Option {
	return WithClock(func() time.Time { return t })
}

// fakeClock is a clock whose time a test advances by hand, for day-boundary
// transitions.
type fakeClock struct {
	t time.Time
}

func (c *fakeClock) now() time.Time { return c.t }

// est and edt simulate the US Eastern DST pair (UTC-5 / UTC-4) with fixed
// offsets, so the DST tests need no host tzdata.
var (
	est = time.FixedZone("EST", -5*3600)
	edt = time.FixedZone("EDT", -4*3600)
)

// atZone builds a local time in a fixed zone.
func atZone(y int, m time.Month, d, h, mi, s int, loc *time.Location) time.Time {
	return time.Date(y, m, d, h, mi, s, 0, loc)
}

// almost reports whether two minute balances are equal within the epsilon
// the account math itself uses.
func almost(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= 1e-9
}
