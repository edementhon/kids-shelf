// Package practice ports the legacy mathfacts app to the kids-shelf
// database: fact pools with weighted sampling toward weak facts, unlock
// ladders and times-table promotions, check-ups with server-side milestone
// evaluation, a 13-boss arena and spelling rounds over pre-generated clips.
//
// It owns every practice_* table through Migrations (PLAN.md section 1a) and
// reaches the rest of the system only through core.Crediter: earned minutes
// go to the household counter (a setting, default "video") with a
// deterministic award id per credit, so a retried answer or round never
// double-credits. Daily caps (math 3, spelling 3 per local day), coin jars
// and unlock state all live in these tables and resume where the legacy app
// left them after migration.
package practice

import (
	"database/sql"
	"errors"
	"math"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/edementhon/kids-shelf/internal/core"
	"github.com/edementhon/kids-shelf/internal/store"
)

// Modes. add and sub are the base modes every kid starts with; mul unlocks
// after fast records on 30% of the add+sub pool, div after 30% of the
// current mul pool (see ModeUnlockStatus).
const (
	ModeAdd = "add"
	ModeSub = "sub"
	ModeMul = "mul"
	ModeDiv = "div"
)

// AllModes lists the practice modes in the fixed order the legacy app used
// them.
var AllModes = []string{ModeAdd, ModeSub, ModeMul, ModeDiv}

// ModeLabels maps each mode to its legacy UI label.
var ModeLabels = map[string]string{
	ModeAdd: "➕ Addition",
	ModeSub: "➖ Subtraction",
	ModeMul: "✖️ Multiplication",
	ModeDiv: "➗ Division",
}

// Legacy constants copied verbatim: the fastest-record bar shared by mastery
// and coin gates, the daily-caps tolerance and the slow-answer clamp.
const (
	fastRecordMS = 6000.0 // best_ms at or under this counts as a fast record
	capEpsilon   = 0.001  // float tolerance for daily-cap comparisons
	slowAnswerMS = 600000 // answers slower than this are clamped (legacy cap)
)

// Errors the service reports; the web layer maps them onto HTTP statuses.
var (
	ErrBadMode     = errors.New("practice: bad mode")
	ErrLocked      = errors.New("practice: mode locked")
	ErrUnknownWord = errors.New("practice: unknown spelling word")
	ErrNoBoss      = errors.New("practice: no boss left")
	ErrBossChanged = errors.New("practice: boss_changed")
)

// Options configures a Service. Every field is optional.
type Options struct {
	// ClipsDir is where pre-generated spelling clips live as
	// <clips>/<word>.wav. Clips are generated elsewhere; AudioPath reports
	// a missing clip instead of synthesizing speech.
	ClipsDir string
	// Clock replaces time.Now (injectable for tests). Defaults to time.Now.
	Clock func() time.Time
	// Location is the local day boundary for the daily caps. Defaults to
	// time.Local, the server machine's clock (DECISIONS.md).
	Location *time.Location
	// RNG backs every sampling call. Defaults to a new source seeded from
	// the clock. Safe for concurrent use (guarded internally).
	RNG *rand.Rand
}

// Service is the practice engine. It owns the practice_* tables through
// Migrations and serializes every state mutation with mu, exactly like the
// legacy app's STATE_LOCK.
type Service struct {
	db    *sql.DB
	cred  core.Crediter
	clips string
	clock func() time.Time
	loc   *time.Location
	rng   *rand.Rand

	mu sync.Mutex

	// roundsMu guards rounds, the live rounds the pages are playing
	// (rounds.go). Never held while taking mu.
	roundsMu sync.Mutex
	rounds   roundBook
}

// New builds a service over db. cred receives every minute credited; the
// Crediter contract (core.Crediter) makes each credit idempotent by award id.
func New(db *sql.DB, cred core.Crediter, opts Options) *Service {
	s := &Service{db: db, cred: cred, clips: opts.ClipsDir}
	s.clock = opts.Clock
	if s.clock == nil {
		s.clock = time.Now
	}
	s.loc = opts.Location
	if s.loc == nil {
		s.loc = time.Local
	}
	s.rng = opts.RNG
	if s.rng == nil {
		seed := uint64(time.Now().UnixNano())
		s.rng = rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	}
	return s
}

// Migrations returns the schema migrations this package owns. Register them
// with a single (*store.Store).Migrate call at startup.
func Migrations() []store.Migration {
	return []store.Migration{
		{Owner: "practice", Version: 1, SQL: schemaSQL},
		{Owner: "practice", Version: 2, SQL: schemaV2SQL},
	}
}

const schemaV2SQL = `
-- Durable identities for append-only history tables so a replayed import
-- (migrate run twice) cannot duplicate legacy rounds or awards. Live rows
-- keep their own timestamps (nanosecond precision) and award ids, so they
-- never collide; the identity only folds rows that are exact replay copies.
-- A database that already holds duplicated imported history would fail to
-- migrate, so drop the duplicates first, keeping the lowest rowid per key.
DELETE FROM practice_sessions WHERE rowid NOT IN
    (SELECT MIN(rowid) FROM practice_sessions GROUP BY kid, at, mode, checkup);
CREATE UNIQUE INDEX IF NOT EXISTS practice_sessions_identity
    ON practice_sessions (kid, at, mode, checkup);
DELETE FROM practice_award_log WHERE rowid NOT IN
    (SELECT MIN(rowid) FROM practice_award_log GROUP BY kid, at, kind, amount, award_id);
CREATE UNIQUE INDEX IF NOT EXISTS practice_award_log_identity
    ON practice_award_log (kid, at, kind, amount, award_id);
DELETE FROM practice_spell_sessions WHERE rowid NOT IN
    (SELECT MIN(rowid) FROM practice_spell_sessions GROUP BY kid, at, word, correct, ms);
CREATE UNIQUE INDEX IF NOT EXISTS practice_spell_sessions_identity
    ON practice_spell_sessions (kid, at, word, correct, ms);
`

const schemaSQL = `
CREATE TABLE practice_facts (
    kid TEXT NOT NULL,
    mode TEXT NOT NULL,
    key TEXT NOT NULL,
    asked INTEGER NOT NULL DEFAULT 0,
    correct INTEGER NOT NULL DEFAULT 0,
    times TEXT NOT NULL DEFAULT '[]',
    median_ms REAL,
    best_ms REAL,
    PRIMARY KEY (kid, mode, key)
);
CREATE TABLE practice_sessions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    kid TEXT NOT NULL,
    at TEXT NOT NULL,
    mode TEXT NOT NULL,
    checkup INTEGER NOT NULL DEFAULT 0,
    n INTEGER NOT NULL DEFAULT 0,
    correct INTEGER NOT NULL DEFAULT 0,
    median_ms REAL
);
CREATE TABLE practice_awards (
    kid TEXT NOT NULL,
    bucket TEXT NOT NULL,
    day TEXT NOT NULL DEFAULT '',
    earned REAL NOT NULL DEFAULT 0,
    coins INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (kid, bucket)
);
CREATE TABLE practice_flags (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE practice_award_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    at TEXT NOT NULL,
    kid TEXT NOT NULL,
    kind TEXT NOT NULL,
    day TEXT NOT NULL,
    amount REAL NOT NULL,
    award_id TEXT
);
CREATE TABLE practice_spell_records (
    kid TEXT NOT NULL,
    word TEXT NOT NULL,
    hits INTEGER NOT NULL DEFAULT 0,
    misses INTEGER NOT NULL DEFAULT 0,
    best_ms INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (kid, word)
);
CREATE TABLE practice_spell_sessions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    kid TEXT NOT NULL,
    at TEXT NOT NULL,
    word TEXT NOT NULL,
    correct INTEGER NOT NULL DEFAULT 0,
    ms INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE practice_bosses (
    kid TEXT PRIMARY KEY,
    defeated TEXT NOT NULL DEFAULT '[]',
    hp TEXT NOT NULL DEFAULT '{}',
    php TEXT NOT NULL DEFAULT '{}',
    pending_bid TEXT
);
CREATE TABLE practice_config (
    key TEXT PRIMARY KEY,
    value REAL NOT NULL
);
CREATE TABLE practice_settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE practice_tables (
    kid TEXT PRIMARY KEY,
    top INTEGER NOT NULL DEFAULT 3
);
`

// Config is the numeric practice configuration, mirroring the legacy
// CONFIG_DEFAULTS plus per-kid overrides stored in practice_config (the
// legacy config.json). The household counter setting lives in
// practice_settings instead ("counter", default "video").
type Config struct {
	AwardPerMilestone float64
	CoinsPerMinute    int
	AwardDailyCap     float64
	SpellDailyCap     float64
	SpellSpeedMS      float64
	CheckupBonus      float64
	RoundMinQuestions int
	MasteryAccuracy   float64
	MasterySpeedMS    float64
	RoundGoal         int
	PromoBarPercent   float64
	CheckupSize       int
	UnlockFrac        float64
	TablesPromoteFrac float64
	BossCoins         int
}

func defaultConfig() Config {
	return Config{
		AwardPerMilestone: 1,
		CoinsPerMinute:    10,
		AwardDailyCap:     3,
		SpellDailyCap:     3,
		SpellSpeedMS:      120000,
		CheckupBonus:      2,
		RoundMinQuestions: 10,
		MasteryAccuracy:   0.9,
		MasterySpeedMS:    6000,
		RoundGoal:         10,
		PromoBarPercent:   0.9,
		CheckupSize:       12,
		UnlockFrac:        0.3,
		TablesPromoteFrac: 0.8,
		BossCoins:         3,
	}
}

// apply merges one legacy numeric config key into c. Unknown keys are
// ignored, as the legacy app silently ignored config it did not use.
func (c *Config) apply(key string, v float64) {
	switch key {
	case "award_per_milestone":
		c.AwardPerMilestone = v
	case "coins_per_minute":
		c.CoinsPerMinute = int(v)
	case "award_daily_cap":
		c.AwardDailyCap = v
	case "spell_daily_cap":
		c.SpellDailyCap = v
	case "spell_speed_ms":
		c.SpellSpeedMS = v
	case "checkup_bonus":
		c.CheckupBonus = v
	case "round_min_questions":
		c.RoundMinQuestions = int(v)
	case "mastery_accuracy":
		c.MasteryAccuracy = v
	case "mastery_speed_ms":
		c.MasterySpeedMS = v
	case "round_goal":
		c.RoundGoal = int(v)
	case "promo_bar_percent":
		c.PromoBarPercent = v
	case "checkup_size":
		c.CheckupSize = int(v)
	case "unlock_frac":
		c.UnlockFrac = v
	case "tables_promote_frac":
		c.TablesPromoteFrac = v
	case "boss_coins":
		c.BossCoins = int(v)
	}
}

// ConfigFor returns the effective configuration for a kid: defaults, then
// global overrides, then per-kid overrides, exactly as the legacy
// kid_config merged config.json. Per-kid overrides are stored under
// "kids.<kid>.<name>" with whatever kid identifier configured them.
func (s *Service) ConfigFor(kid string) (Config, error) {
	c := defaultConfig()
	globals, err := s.configValues("global.")
	if err != nil {
		return c, err
	}
	for k, v := range globals {
		c.apply(k, v)
	}
	perKid, err := s.configValues("kids." + kid + ".")
	if err != nil {
		return c, err
	}
	for k, v := range perKid {
		c.apply(k, v)
	}
	return c, nil
}

// configValues returns the config entries whose key starts with prefix,
// with the prefix stripped.
func (s *Service) configValues(prefix string) (map[string]float64, error) {
	rows, err := s.db.Query(`SELECT key, value FROM practice_config WHERE key LIKE ?`, prefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]float64)
	for rows.Next() {
		var key string
		var v float64
		if err := rows.Scan(&key, &v); err != nil {
			return nil, err
		}
		out[strings.TrimPrefix(key, prefix)] = v
	}
	return out, rows.Err()
}

func (s *Service) now() time.Time { return s.clock() }
func (s *Service) today() string  { return s.now().In(s.loc).Format("2006-01-02") }

// round2 matches the legacy round(…, 2) on award totals.
func round2(x float64) float64 { return math.Round(x*100) / 100 }

// round3 matches the legacy round(…, 3) on unlock/promotion fractions.
func round3(x float64) float64 { return math.Round(x*1000) / 1000 }

// clampMS clamps an answer time to [0, 600000] as the legacy app did.
func clampMS(ms int) int { return max(0, min(ms, slowAnswerMS)) }

// normOp maps an operation to one of + - x /, defaulting to + exactly like
// the legacy handlers.
func normOp(op string) string {
	switch op {
	case "+", "-", "x", "/":
		return op
	}
	return "+"
}
