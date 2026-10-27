package practice

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Award buckets and kinds, mirroring the legacy award log: "jar" is the math
// jar, "spell" the spelling jar, "promo" the once-ever bonus. Counters in
// practice_awards are keyed by bucket; award ids carry the kind.
const (
	bucketMath  = "math"
	bucketSpell = "spell"
	kindJar     = "jar"
	kindSpell   = "spell"
	kindPromo   = "promo"
)

// defaultCounter is the household counter that receives practice earnings
// unless the parent pointed them elsewhere (practice_settings "counter").
const defaultCounter = "video"

// Awards is one bucket's per-day totals: minutes earned toward the daily cap
// and the unbanked coin jar. The day is the local day (Options.Location).
type Awards struct {
	Day    string
	Earned float64
	Coins  int
}

// AwardsView is what the UI shows for one bucket.
type AwardsView struct {
	Day    string
	Earned float64
	Coins  int
	Cap    float64
}

// logRow is one credited minute, persisted in practice_award_log. award_id
// is the deterministic id under which the credit reached the Crediter.
type logRow struct {
	At      time.Time
	Kid     string
	Kind    string
	Day     string
	Amount  float64
	AwardID string
}

// AwardLogView is one recorded award for the parent view.
type AwardLogView struct {
	At      time.Time
	Kid     string
	Kind    string
	Day     string
	Amount  float64
	AwardID string
}

// flagRow is one practice_flags write (promo/bonus markers and best-round
// times).
type flagRow struct {
	key   string
	value string
}

// awardsFor loads a bucket's awards, advancing the local day: when the
// stored day is not today the jar resets — earned minutes and coins do not
func (s *Service) awardsFor(kid, bucket string) (*Awards, error) {
	var awDay string
	var awEarned float64
	var awCoins int
	err := s.db.QueryRow(
		`SELECT day, earned, coins FROM practice_awards WHERE kid=? AND bucket=?`,
		kid, bucket).Scan(&awDay, &awEarned, &awCoins)
	if errors.Is(err, sql.ErrNoRows) {
		return &Awards{Day: s.today()}, nil
	}
	if err != nil {
		return nil, err
	}
	aw := Awards{Day: awDay, Earned: awEarned, Coins: awCoins}
	if aw.Day != s.today() {
		aw = Awards{Day: s.today()}
	}
	return &aw, nil
}

func saveAwards(ex execer, kid, bucket string, aw *Awards) error {
	_, err := ex.Exec(`INSERT INTO practice_awards (kid, bucket, day, earned, coins)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (kid, bucket) DO UPDATE SET
    day=excluded.day, earned=excluded.earned, coins=excluded.coins`,
		kid, bucket, aw.Day, aw.Earned, aw.Coins)
	return err
}

// awardSeq returns how many awards of one kind were already credited to a
// kid on a day. The next award id is this plus one; because the count only
// changes when a credit was applied and persisted, a retried flow derives
// the same id and the Crediter counts it once.
func (s *Service) awardSeq(kid, kind, day string) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM practice_award_log WHERE kid=? AND kind=? AND day=?`,
		kid, kind, day).Scan(&n)
	return n, err
}

func insertLogs(ex execer, logs []logRow) error {
	for _, l := range logs {
		// INSERT OR IGNORE folds an exact replay of the same legacy award
		// (same id, timestamp, kind, amount), while live banks stay
		// distinct through their deterministically sequenced award id.
		if _, err := ex.Exec(`INSERT OR IGNORE INTO practice_award_log (at, kid, kind, day, amount, award_id)
VALUES (?, ?, ?, ?, ?, ?)`,
			l.At.UTC().Format(time.RFC3339), l.Kid, l.Kind, l.Day, l.Amount, l.AwardID); err != nil {
			return err
		}
	}
	return nil
}

// bankCoins adds n coins to the jar and banks full jars into credited
// minutes: 10 coins per minute (coins_per_minute), one minute per bank
// (award_per_milestone), gated by the bucket's daily cap. It mutates aw and
// appends log rows; the caller persists everything atomically after the
// credits succeed. kind is kindJar (math) or kindSpell (spelling).
func (s *Service) bankCoins(ctx context.Context, kid string, aw *Awards, kind string, n int, cfg Config, logs *[]logRow) (*CoinMsg, error) {
	aw.Coins += n
	msg := &CoinMsg{Kind: "coin", Coins: aw.Coins, Need: cfg.CoinsPerMinute}
	capVal := cfg.AwardDailyCap
	if kind == kindSpell {
		capVal = cfg.SpellDailyCap
	}
	for aw.Coins >= cfg.CoinsPerMinute && aw.Earned < capVal-capEpsilon {
		if aw.Earned+cfg.AwardPerMilestone > capVal+capEpsilon {
			break // only a full minute fits under the cap; keep the coins in the jar
		}
		seq, err := s.awardSeq(kid, kind, aw.Day)
		if err != nil {
			return nil, err
		}
		awardID := fmt.Sprintf("%s:%s:%s:%d", kid, kind, aw.Day, seq+1)
		counter, err := s.Counter()
		if err != nil {
			return nil, err
		}
		if err := s.cred.Credit(ctx, kid, counter, cfg.AwardPerMilestone, awardID); err != nil {
			// Nothing is persisted by the caller on error, so a retry
			// replays the same state and derives the same award id; the
			// Crediter keeps a crash-window double-credit from counting.
			return nil, err
		}
		aw.Coins -= cfg.CoinsPerMinute
		aw.Earned = round2(aw.Earned + cfg.AwardPerMilestone)
		*logs = append(*logs, logRow{
			At: s.now(), Kid: kid, Kind: kind, Day: aw.Day,
			Amount: cfg.AwardPerMilestone, AwardID: awardID,
		})
		msg = &CoinMsg{Kind: "jar", Coins: aw.Coins, Need: cfg.CoinsPerMinute, Banked: true}
	}
	return msg, nil
}

// tryAward applies one single award (kindPromo today; the coin jars use
// bankCoins): daily-cap check, the once-ever promo flag, an idempotent
// credit and the earned/log/flag bookkeeping. Mutates aw; the caller
// persists everything afterward.
func (s *Service) tryAward(ctx context.Context, kid string, aw *Awards, kind string, amount float64, cfg Config, flagSets *[]flagRow, logs *[]logRow) (bool, string, error) {
	capVal := cfg.AwardDailyCap
	if kind == kindSpell {
		capVal = cfg.SpellDailyCap
	}
	if aw.Earned+amount > capVal+capEpsilon {
		return false, "daily cap reached", nil
	}
	if kind == kindPromo {
		done, err := s.flagExists("promo_bonus:" + kid)
		if err != nil {
			return false, "", err
		}
		if done {
			return false, "already awarded", nil
		}
	}
	seq, err := s.awardSeq(kid, kind, aw.Day)
	if err != nil {
		return false, "", err
	}
	awardID := fmt.Sprintf("%s:%s:%s:%d", kid, kind, aw.Day, seq+1)
	counter, err := s.Counter()
	if err != nil {
		return false, "", err
	}
	if err := s.cred.Credit(ctx, kid, counter, amount, awardID); err != nil {
		return false, "", err
	}
	aw.Earned = round2(aw.Earned + amount)
	if kind == kindPromo {
		*flagSets = append(*flagSets, flagRow{key: "promo_bonus:" + kid, value: "true"})
	}
	*logs = append(*logs, logRow{
		At: s.now(), Kid: kid, Kind: kind, Day: aw.Day,
		Amount: amount, AwardID: awardID,
	})
	return true, "", nil
}

// withTx runs fn inside a transaction, rolling back on error.
func (s *Service) withTx(fn func(tx *sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ------------------------------------------------------------ flags

func saveFlags(ex execer, rows []flagRow) error {
	for _, r := range rows {
		if _, err := ex.Exec(`INSERT INTO practice_flags (key, value) VALUES (?, ?)
ON CONFLICT (key) DO UPDATE SET value=excluded.value`, r.key, r.value); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) flagExists(key string) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM practice_flags WHERE key=?`, key).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Service) flagFloat(key string) (*float64, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM practice_flags WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return nil, err
	}
	return new(f), nil
}

// BestRound returns the kid's stored best round median for a mode (the
// milestone "best:<kid>:<mode>" flag), nil when none.
func (s *Service) BestRound(kid, mode string) (*float64, error) {
	return s.flagFloat("best:" + kid + ":" + mode)
}

// PromoBonusDone reports whether the once-ever promo bonus was already
// credited to the kid.
func (s *Service) PromoBonusDone(kid string) (bool, error) {
	return s.flagExists("promo_bonus:" + kid)
}

// ------------------------------------------------------------ views

// Awards returns the math bucket's current day, totals and cap.
func (s *Service) Awards(kid string) (AwardsView, error) {
	cfg, err := s.ConfigFor(kid)
	if err != nil {
		return AwardsView{}, err
	}
	aw, err := s.awardsFor(kid, bucketMath)
	if err != nil {
		return AwardsView{}, err
	}
	return AwardsView{Day: aw.Day, Earned: aw.Earned, Coins: aw.Coins, Cap: cfg.AwardDailyCap}, nil
}

// SpellAwards returns the spelling bucket's current day, totals and cap.
func (s *Service) SpellAwards(kid string) (AwardsView, error) {
	cfg, err := s.ConfigFor(kid)
	if err != nil {
		return AwardsView{}, err
	}
	aw, err := s.awardsFor(kid, bucketSpell)
	if err != nil {
		return AwardsView{}, err
	}
	return AwardsView{Day: aw.Day, Earned: aw.Earned, Coins: aw.Coins, Cap: cfg.SpellDailyCap}, nil
}

// AwardLog returns every recorded award, oldest first.
func (s *Service) AwardLog() ([]AwardLogView, error) {
	rows, err := s.db.Query(`SELECT at, kid, kind, day, amount, award_id FROM practice_award_log ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AwardLogView
	for rows.Next() {
		var at, kid, kind, day, awardID string
		var amount float64
		if err := rows.Scan(&at, &kid, &kind, &day, &amount, &awardID); err != nil {
			return nil, err
		}
		t, err := time.Parse(time.RFC3339, at)
		if err != nil {
			return nil, err
		}
		out = append(out, AwardLogView{At: t, Kid: kid, Kind: kind, Day: day, Amount: amount, AwardID: awardID})
	}
	return out, rows.Err()
}

// ------------------------------------------------------------ counter

// Counter returns the household counter that receives practice earnings
// (default "video"); the parent dashboard points earnings elsewhere through
// SetCounter.
func (s *Service) Counter() (string, error) {
	var c string
	err := s.db.QueryRow(`SELECT value FROM practice_settings WHERE key='counter'`).Scan(&c)
	if errors.Is(err, sql.ErrNoRows) {
		return defaultCounter, nil
	}
	if err != nil {
		return "", err
	}
	return c, nil
}

// SetCounter points practice earnings at another counter for the household.
func (s *Service) SetCounter(_ context.Context, counter string) error {
	_, err := s.db.Exec(`INSERT INTO practice_settings (key, value) VALUES ('counter', ?)
ON CONFLICT (key) DO UPDATE SET value=excluded.value`, counter)
	return err
}
