package budget

import (
	"errors"
	"testing"
	"time"

	"github.com/edementhon/kids-shelf/internal/core"
)

// 2026-11-03 is a Tuesday.
func nov(d, h int) time.Time { return atZone(2026, 11, d, h, 0, 0, est) }

func gamesBudget(t *testing.T, start time.Time) (*Budget, *fakeClock, core.KidID) {
	t.Helper()
	c := &fakeClock{t: start}
	b, _ := newTestBudget(t, WithClock(c.now))
	return b, c, mustCreateKid(t, b, "Ada", "1111")
}

func setGames(t *testing.T, b *Budget, gs GamesSettings) {
	t.Helper()
	if err := b.SetGames(ctx, gs); err != nil {
		t.Fatalf("SetGames(%+v): %v", gs, err)
	}
}

func games(t *testing.T, b *Budget, kid core.KidID) float64 {
	t.Helper()
	bal, saved, err := b.Balance(ctx, kid, GamesCounter)
	if err != nil {
		t.Fatalf("Balance(games): %v", err)
	}
	if saved != 0 {
		t.Fatalf("games saved = %v, want 0 (game minutes have one balance)", saved)
	}
	return bal
}

func wantGames(t *testing.T, b *Budget, kid core.KidID, want float64, when string) {
	t.Helper()
	if got := games(t, b, kid); got != want {
		t.Errorf("%s: game minutes = %v, want %v", when, got, want)
	}
}

func TestGamesOffUntilTurnedOn(t *testing.T) {
	b, c, ada := gamesBudget(t, nov(3, 12))
	gs, err := b.Games(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if gs != (GamesSettings{Weekly: 30, Day: time.Tuesday}) {
		t.Errorf("fresh settings = %+v, want off, 30 a week, Tuesday", gs)
	}
	wantGames(t, b, ada, 0, "off, first read")
	c.t = nov(17, 12)
	wantGames(t, b, ada, 0, "off, two Tuesdays later")
}

func TestGamesWeeklyTopUp(t *testing.T) {
	b, c, ada := gamesBudget(t, nov(4, 9)) // a Wednesday
	setGames(t, b, GamesSettings{On: true, Weekly: 30, Day: time.Tuesday})
	wantGames(t, b, ada, 30, "first read starts with one top-up")
	c.t = nov(9, 23)
	wantGames(t, b, ada, 30, "Monday night, same week")
	c.t = nov(10, 0)
	wantGames(t, b, ada, 60, "Tuesday midnight")
	c.t = nov(10, 20)
	wantGames(t, b, ada, 60, "later on Tuesday")
	c.t = atZone(2026, 12, 1, 8, 0, 0, est) // three Tuesdays later, nobody looked
	wantGames(t, b, ada, 150, "three missed weeks all land")
}

func TestGamesDebtCarriesIntoNextWeek(t *testing.T) {
	b, c, ada := gamesBudget(t, nov(3, 12))
	setGames(t, b, GamesSettings{On: true, Weekly: 30, Day: time.Tuesday})
	today, _, err := b.Adjust(ctx, ada, GamesCounter, -45, true)
	if err != nil {
		t.Fatal(err)
	}
	if today != -15 {
		t.Errorf("30 - 45 = %v, want -15", today)
	}
	c.t = nov(10, 12)
	wantGames(t, b, ada, 15, "next Tuesday pays the debt first")
	if _, _, err := b.Adjust(ctx, ada, GamesCounter, 5, true); err != nil {
		t.Fatal(err)
	}
	wantGames(t, b, ada, 20, "after +5")
}

func TestGamesNewWeekdayNeverTopsUpTwiceInAWeek(t *testing.T) {
	b, c, ada := gamesBudget(t, nov(3, 12))
	setGames(t, b, GamesSettings{On: true, Weekly: 30, Day: time.Tuesday})
	wantGames(t, b, ada, 30, "Tuesday")
	c.t = nov(4, 12)
	setGames(t, b, GamesSettings{On: true, Weekly: 30, Day: time.Wednesday})
	wantGames(t, b, ada, 30, "moved to Wednesday, on that Wednesday")
	c.t = nov(10, 12)
	wantGames(t, b, ada, 30, "the old Tuesday")
	c.t = nov(11, 12)
	wantGames(t, b, ada, 60, "the next Wednesday")
}

func TestGamesChangeKeepsTopUpsAlreadyDue(t *testing.T) {
	b, c, ada := gamesBudget(t, nov(3, 12))
	setGames(t, b, GamesSettings{On: true, Weekly: 30, Day: time.Tuesday})
	wantGames(t, b, ada, 30, "Tuesday")
	c.t = nov(18, 12) // two top-ups due, nobody looked
	setGames(t, b, GamesSettings{On: true, Weekly: 10, Day: time.Tuesday})
	wantGames(t, b, ada, 90, "the two due weeks keep the old 30")
	c.t = nov(24, 12)
	wantGames(t, b, ada, 100, "the next week adds the new 10")
}

func TestGamesOffAndOnAgain(t *testing.T) {
	b, c, ada := gamesBudget(t, nov(3, 12))
	setGames(t, b, GamesSettings{On: true, Weekly: 30, Day: time.Tuesday})
	wantGames(t, b, ada, 30, "on")
	c.t = nov(4, 12)
	setGames(t, b, GamesSettings{On: false, Weekly: 30, Day: time.Tuesday})
	c.t = nov(19, 12) // two Tuesdays while off
	wantGames(t, b, ada, 30, "off: no top-ups")
	setGames(t, b, GamesSettings{On: true, Weekly: 30, Day: time.Tuesday})
	wantGames(t, b, ada, 30, "on again: nothing for the weeks it was off")
	c.t = nov(24, 12)
	wantGames(t, b, ada, 60, "the next Tuesday")
}

func TestGamesAndVideoMinutesStayApart(t *testing.T) {
	b, c, ada := gamesBudget(t, nov(3, 12))
	setGames(t, b, GamesSettings{On: true, Weekly: 30, Day: time.Tuesday})
	if _, _, err := b.Adjust(ctx, ada, GamesCounter, -20, true); err != nil {
		t.Fatal(err)
	}
	today, saved, err := b.Balance(ctx, ada, DefaultCounter)
	if err != nil {
		t.Fatal(err)
	}
	if today != DefaultDailyMinutes || saved != 0 {
		t.Errorf("video = %v today, %v saved, want the plain daily %v", today, saved, DefaultDailyMinutes)
	}
	c.t = nov(4, 12) // a new day, not a top-up day
	wantGames(t, b, ada, 10, "a new day adds no daily minutes to games")
	if today, saved, _ := b.Balance(ctx, ada, DefaultCounter); today != DefaultDailyMinutes || saved != DefaultDailyMinutes {
		t.Errorf("video next day = %v today, %v saved, want %v and %v", today, saved, DefaultDailyMinutes, DefaultDailyMinutes)
	}
}

func TestSetGamesRejectsBadSettings(t *testing.T) {
	b, _, _ := gamesBudget(t, nov(3, 12))
	for _, gs := range []GamesSettings{
		{On: true, Weekly: -1, Day: time.Tuesday},
		{On: true, Weekly: MaxGamesWeekly + 1, Day: time.Tuesday},
		{On: true, Weekly: 30, Day: 7},
		{On: true, Weekly: 30, Day: -1},
	} {
		if err := b.SetGames(ctx, gs); !errors.Is(err, ErrGamesSettings) {
			t.Errorf("SetGames(%+v) = %v, want ErrGamesSettings", gs, err)
		}
	}
	if gs, _ := b.Games(ctx); gs.On {
		t.Errorf("a rejected change turned game minutes on: %+v", gs)
	}
}

func TestNextGamesTopUp(t *testing.T) {
	b, c, _ := gamesBudget(t, nov(3, 12))
	for _, tc := range []struct {
		now  time.Time
		want int // day of November
	}{
		{nov(3, 12), 10}, // on a Tuesday: the next one
		{nov(4, 0), 10},
		{nov(9, 23), 10},
	} {
		c.t = tc.now
		if got := b.NextGamesTopUp(time.Tuesday); got.Day() != tc.want || got.Month() != time.November {
			t.Errorf("at %v: next top-up %v, want November %d", tc.now, got, tc.want)
		}
	}
}
