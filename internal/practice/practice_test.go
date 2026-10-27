package practice

import (
	"context"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edementhon/kids-shelf/internal/core"
	"github.com/edementhon/kids-shelf/internal/store"
)

// testEnv wires a Service over a real SQLite store with a fake clock (a
// fixed local day, 2026-10-26, mutable through env.now), a seeded RNG and a
// recording FakeCrediter.
type testEnv struct {
	svc  *Service
	cred *core.FakeCrediter
	now  time.Time
}

func newEnv(t *testing.T, seed uint64) *testEnv {
	t.Helper()
	loc := time.FixedZone("test", 0)
	e := &testEnv{
		now:  time.Date(2026, 10, 26, 12, 0, 0, 0, loc),
		cred: &core.FakeCrediter{},
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "practice.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(Migrations()...); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	e.svc = New(st.DB(), e.cred, Options{
		ClipsDir: t.TempDir(),
		Clock:    func() time.Time { return e.now },
		Location: loc,
		RNG:      rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)),
	})
	return e
}

func ctx() context.Context { return context.Background() }

// seedFact imports one fact row with cumulative stats (as migrate would).
func seedFact(t *testing.T, svc *Service, kid, mode, key string, asked, correct int, times []int, best *float64) {
	t.Helper()
	err := svc.ImportFactStats(ctx(), core.KidID(kid), mode, key, core.FactStats{
		Asked:    asked,
		Correct:  correct,
		Times:    append([]int(nil), times...),
		MedianMS: median(times),
		BestMS:   best,
	})
	if err != nil {
		t.Fatalf("seedFact(%s): %v", key, err)
	}
}

// seedFast seeds one fast-record row on a fact (asked once, answered
// correctly in 3000 ms): counts for unlock gates and the coin ladder.
func seedFast(t *testing.T, svc *Service, kid, mode, key string) {
	t.Helper()
	seedFact(t, svc, kid, mode, key, 1, 1, []int{3000}, new(float64(3000)))
}

// seedMastered seeds one mastered row on a fact: accuracy 0.9 with a 5000 ms
// median, under both the mastery bar and the coin speed bar.
func seedMastered(t *testing.T, svc *Service, kid, mode, key string) {
	t.Helper()
	seedFact(t, svc, kid, mode, key, 20, 18, []int{5000, 5000}, new(float64(5000)))
}

// seedToMastered imports mastered rows until `total` distinct pool keys of a
// mode are mastered (used for promotion and promo-bar boundaries). All rows
// are inserted in one transaction; the pool and existing stats are read once.
func seedToMastered(t *testing.T, svc *Service, kid, mode string, total int) {
	t.Helper()
	pool, err := svc.FactPool(kid, mode)
	if err != nil {
		t.Fatalf("FactPool: %v", err)
	}
	cfg, err := svc.ConfigFor(kid)
	if err != nil {
		t.Fatalf("ConfigFor: %v", err)
	}
	facts, err := svc.factsMap(kid, mode)
	if err != nil {
		t.Fatalf("factsMap: %v", err)
	}
	tx, err := svc.db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	mastered := 0
	for _, f := range pool {
		if fs, ok := facts[f.Key()]; ok && isMastered(&fs, cfg) {
			mastered++
		}
	}
	inserted := 0
	for _, f := range pool {
		if mastered+inserted >= total {
			break
		}
		if fs, ok := facts[f.Key()]; ok && isMastered(&fs, cfg) {
			continue
		}
		if err := saveFact(tx, kid, mode, f.Key(), &FactStats{
			Asked: 20, Correct: 18,
			Times:    []int{5000, 5000},
			MedianMS: new(float64(5000)),
			BestMS:   new(float64(5000)),
		}); err != nil {
			t.Fatalf("seedMastered: %v", err)
		}
		inserted++
	}
	if mastered+inserted < total {
		t.Fatalf("pool exhausted at %d/%d mastered", mastered+inserted, total)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// seedFastBatch imports one fast-record row (asked once, correct in 3000 ms)
// for every key of a mode inside a single transaction.
func seedFastBatch(t *testing.T, svc *Service, kid, mode string, keys []string) {
	t.Helper()
	tx, err := svc.db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	for _, key := range keys {
		if err := saveFact(tx, kid, mode, key, &FactStats{
			Asked: 1, Correct: 1,
			Times:    []int{3000},
			MedianMS: new(float64(3000)),
			BestMS:   new(float64(3000)),
		}); err != nil {
			t.Fatalf("seedFast %s: %v", key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// mkRound builds a round of correctCount correct answers (each answering
// i+1 = i+1 in ms correctMs) plus wrongN wrong answers.
func mkRound(correctCount, wrongN, correctMs int) []RoundAnswer {
	var out []RoundAnswer
	for i := 1; i <= correctCount; i++ {
		out = append(out, RoundAnswer{A: i, B: 1, Op: "+", Given: i + 1, Ms: correctMs})
	}
	for i := 0; i < wrongN; i++ {
		out = append(out, RoundAnswer{A: i + 1, B: 1, Op: "+", Given: 999, Ms: correctMs})
	}
	return out
}

// answer records one fact answer with a given answer time.
func answer(t *testing.T, svc *Service, kid, mode string, a, b int, op string, given, ms int) AnswerOutcome {
	t.Helper()
	out, err := svc.RecordAnswer(ctx(), kid, mode, a, b, op, given, ms)
	if err != nil {
		t.Fatalf("RecordAnswer(%dx%d %s): %v", a, b, op, err)
	}
	return out
}

func TestMigrationsOwnedAndIdempotent(t *testing.T) {
	ms := Migrations()
	if len(ms) != 2 {
		t.Fatalf("Migrations() = %d, want 2", len(ms))
	}
	if ms[0].Owner != "practice" || ms[0].Version != 1 {
		t.Errorf("migration 1 = %+v, want owner practice version 1", ms[0])
	}
	if ms[1].Owner != "practice" || ms[1].Version != 2 {
		t.Errorf("migration 2 = %+v, want owner practice version 2", ms[1])
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "m.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(Migrations()...); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if err := st.Migrate(Migrations()...); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM practice_facts`).Scan(&n); err != nil {
		t.Fatalf("practice_facts missing: %v", err)
	}
}

// TestMigrateV2DedupesHistory builds a v1-only database that already holds
// duplicated imported history (as a replayed legacy import would leave it)
// and verifies the v2 migration still succeeds, keeping one row per identity
// key and dropping the higher-rowid copies.
func TestMigrateV2DedupesHistory(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "dup.sqlite")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	// Apply only the v1 schema, then inject duplicate history rows.
	if err := st.Migrate(Migrations()[0]); err != nil {
		t.Fatalf("v1 Migrate: %v", err)
	}
	db := st.DB()
	for _, q := range []string{
		`INSERT INTO practice_sessions (kid, at, mode, checkup) VALUES ('k', '2026-10-26T10:00:00Z', 'add', 0)`,
		`INSERT INTO practice_sessions (kid, at, mode, checkup) VALUES ('k', '2026-10-26T10:00:00Z', 'add', 0)`,
		`INSERT INTO practice_award_log (at, kid, kind, day, amount, award_id) VALUES ('2026-10-26T10:00:00Z', 'k', 'minutes', '2026-10-26', 1, 'a1')`,
		`INSERT INTO practice_award_log (at, kid, kind, day, amount, award_id) VALUES ('2026-10-26T10:00:00Z', 'k', 'minutes', '2026-10-26', 1, 'a1')`,
		`INSERT INTO practice_spell_sessions (kid, at, word, correct, ms) VALUES ('k', '2026-10-26T10:00:00Z', 'cat', 1, 2400)`,
		`INSERT INTO practice_spell_sessions (kid, at, word, correct, ms) VALUES ('k', '2026-10-26T10:00:00Z', 'cat', 1, 2400)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed duplicate row: %v", err)
		}
	}
	tables := []string{"practice_sessions", "practice_award_log", "practice_spell_sessions"}
	lowest := make(map[string]int64, len(tables))
	for _, table := range tables {
		var m int64
		if err := db.QueryRow(`SELECT MIN(rowid) FROM ` + table).Scan(&m); err != nil {
			t.Fatalf("%s MIN(rowid): %v", table, err)
		}
		lowest[table] = m
	}
	// The current migrations must dedupe and then build the unique indexes.
	if err := st.Migrate(Migrations()...); err != nil {
		t.Fatalf("Migrate over duplicated history: %v", err)
	}
	for _, table := range tables {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatalf("%s COUNT(*): %v", table, err)
		}
		if n != 1 {
			t.Errorf("%s rows = %d after migration, want 1 (lowest rowid per key)", table, n)
		}
		var id int64
		if err := db.QueryRow(`SELECT MIN(rowid) FROM ` + table).Scan(&id); err != nil {
			t.Fatalf("%s rowid after migration: %v", table, err)
		}
		if id != lowest[table] {
			t.Errorf("%s kept rowid %d, want lowest %d", table, id, lowest[table])
		}
	}
}

// TestInstallClipsCoversWordBank checks every bank word gets a playable clip
// and that a rerun leaves existing files alone.
func TestInstallClipsCoversWordBank(t *testing.T) {
	dir := t.TempDir()
	if err := InstallClips(dir); err != nil {
		t.Fatalf("InstallClips: %v", err)
	}
	svc := New(nil, nil, Options{ClipsDir: dir})
	for _, w := range Words() {
		if _, ok := svc.AudioPath(w.Word); !ok {
			t.Errorf("no clip for %q", w.Word)
		}
	}
	keep := filepath.Join(dir, Words()[0].Word+".wav")
	if err := os.WriteFile(keep, []byte("parent-recorded"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := InstallClips(dir); err != nil {
		t.Fatalf("InstallClips rerun: %v", err)
	}
	if b, _ := os.ReadFile(keep); string(b) != "parent-recorded" {
		t.Error("rerun overwrote an existing clip")
	}
}
