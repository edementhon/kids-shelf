package budget

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"
)

// Game minutes are a second counter per kid for screen time Kids Shelf does
// not run itself, such as video games. They have no daily allowance: one
// balance carries from week to week, may go below zero when a parent takes
// away more than is left, and grows by a weekly top-up that lands at local
// midnight on the household's chosen weekday. This is the legacy portal's
// game counter (30 minutes every Wednesday); Eric's household moved the
// top-up to Tuesday (2026-11-03). Off by default: a household that does not
// turn game minutes on never sees them.

const (
	// GamesCounter is the account counter that holds game minutes.
	GamesCounter = "games"

	// DefaultGamesWeekly and DefaultGamesDay are the top-up a household
	// gets until it chooses another (migration 5 sets the same values).
	DefaultGamesWeekly = 30.0
	DefaultGamesDay    = time.Tuesday

	// MaxGamesWeekly bounds the weekly top-up at a whole week of minutes.
	MaxGamesWeekly = 7 * 24 * 60.0
)

// ErrGamesSettings is returned by SetGames for a weekly top-up outside 0 to
// MaxGamesWeekly minutes or a weekday outside Sunday to Saturday.
var ErrGamesSettings = errors.New("budget: game minutes need 0 to 10080 minutes a week and a weekday")

// GamesSettings is the household's game-minutes setup.
type GamesSettings struct {
	On     bool
	Weekly float64      // minutes added to every kid each week
	Day    time.Weekday // the weekday the top-up lands on, at local midnight
}

// Games returns the household's game-minutes setup.
func (b *Budget) Games(ctx context.Context) (GamesSettings, error) {
	tx, err := b.begin(ctx)
	if err != nil {
		return GamesSettings{}, err
	}
	defer tx.Rollback()
	return b.games(tx)
}

func (b *Budget) games(tx *sql.Tx) (GamesSettings, error) {
	var on, day int
	var weekly float64
	err := tx.QueryRow(`SELECT games_on, games_weekly, games_day FROM household WHERE id = 1`).Scan(&on, &weekly, &day)
	if errors.Is(err, sql.ErrNoRows) {
		return GamesSettings{Weekly: DefaultGamesWeekly, Day: DefaultGamesDay}, nil
	}
	if err != nil {
		return GamesSettings{}, fmt.Errorf("budget: read game minutes settings: %w", err)
	}
	return GamesSettings{On: on != 0, Weekly: weekly, Day: time.Weekday(day)}, nil
}

// SetGames changes the household's game-minutes setup. Every kid's game
// minutes are first brought up to date under the old setup, so no top-up
// that was due is lost and none lands twice: after turning game minutes on
// again or moving the weekday, the next top-up comes on the next chosen
// weekday. A kid with no game minutes yet gets one top-up the first time
// they are read while game minutes are on.
func (b *Budget) SetGames(ctx context.Context, gs GamesSettings) error {
	if gs.Weekly < 0 || gs.Weekly > MaxGamesWeekly || math.IsNaN(gs.Weekly) || gs.Day < time.Sunday || gs.Day > time.Saturday {
		return ErrGamesSettings
	}
	tx, err := b.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	old, err := b.games(tx)
	if err != nil {
		return err
	}
	var kids []string
	rows, err := tx.Query(`SELECT kid FROM accounts WHERE counter = ?`, GamesCounter)
	if err != nil {
		return fmt.Errorf("budget: list game minutes: %w", err)
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return fmt.Errorf("budget: list game minutes: %w", err)
		}
		kids = append(kids, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("budget: list game minutes: %w", err)
	}
	for _, k := range kids {
		if _, err := b.gamesRollover(tx, k, old); err != nil {
			return err
		}
	}
	if gs.On != old.On || gs.Day != old.Day {
		if _, err := tx.Exec(`UPDATE accounts SET day = ? WHERE counter = ?`,
			gamesKey(b.now(), gs.Day), GamesCounter); err != nil {
			return fmt.Errorf("budget: restart game minutes week: %w", err)
		}
	}
	on := 0
	if gs.On {
		on = 1
	}
	if _, err := tx.Exec(`UPDATE household SET games_on = ?, games_weekly = ?, games_day = ? WHERE id = 1`,
		on, gs.Weekly, int(gs.Day)); err != nil {
		return fmt.Errorf("budget: set game minutes: %w", err)
	}
	return tx.Commit()
}

// NextGamesTopUp is the local date of the next weekly top-up after now
// (never today: today's top-up, when today is the day, has landed already).
func (b *Budget) NextGamesTopUp(day time.Weekday) time.Time {
	now := b.now()
	ahead := (int(day) - int(now.Weekday()) + 7) % 7
	if ahead == 0 {
		ahead = 7
	}
	return time.Date(now.Year(), now.Month(), now.Day()+ahead, 0, 0, 0, 0, now.Location())
}

// gamesKey is the local date (2006-01-02) of the most recent top-up day on
// or before now's date: the week a games account has been brought up to.
func gamesKey(now time.Time, day time.Weekday) string {
	back := (int(now.Weekday()) - int(day) + 7) % 7
	return time.Date(now.Year(), now.Month(), now.Day()-back, 0, 0, 0, 0, now.Location()).Format("2006-01-02")
}

// weeksBetween counts the top-up days after from up to and including to,
// both dates of top-up days (or from a day of an earlier schedule): 0 when
// to is not after from.
func weeksBetween(from, to string) (int, error) {
	f, err := time.Parse("2006-01-02", from)
	if err != nil {
		return 0, fmt.Errorf("budget: game minutes week %q: %w", from, err)
	}
	t, err := time.Parse("2006-01-02", to)
	if err != nil {
		return 0, fmt.Errorf("budget: game minutes week %q: %w", to, err)
	}
	days := int(t.Sub(f).Hours() / 24)
	if days <= 0 {
		return 0, nil
	}
	return (days + 6) / 7, nil
}

// gamesRollover brings kid's game minutes up to this week under gs and
// returns the balance. While game minutes are on, every top-up day since
// the account was last brought up to date adds gs.Weekly, so the balance is
// the same whether or not anyone looked in between; a kid's first read adds
// one top-up. While they are off nothing is added, but the account row is
// made so that a parent's change or a credit has a row to land on.
func (b *Budget) gamesRollover(tx *sql.Tx, kid string, gs GamesSettings) (float64, error) {
	key := gamesKey(b.now(), gs.Day)
	var dayRow string
	var bal float64
	err := tx.QueryRow(`SELECT day, today FROM accounts WHERE kid = ? AND counter = ?`, kid, GamesCounter).Scan(&dayRow, &bal)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("budget: read game minutes: %w", err)
	}
	if exists && (!gs.On || dayRow == key) {
		return bal, nil
	}
	add := 0.0
	if gs.On {
		weeks := 1
		if exists && dayRow != "" {
			if weeks, err = weeksBetween(dayRow, key); err != nil {
				return 0, err
			}
		}
		add = float64(weeks) * gs.Weekly
	}
	bal += add
	if _, err := tx.Exec(`INSERT INTO accounts (kid, counter, day, today, saved) VALUES (?, ?, ?, ?, 0)
		ON CONFLICT(kid, counter) DO UPDATE SET day = excluded.day, today = excluded.today, saved = 0`,
		kid, GamesCounter, key, bal); err != nil {
		return 0, fmt.Errorf("budget: store game minutes: %w", err)
	}
	return bal, nil
}
