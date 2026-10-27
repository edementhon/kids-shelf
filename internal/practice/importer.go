package practice

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/edementhon/kids-shelf/internal/core"
)

// Compile-time check: the practice service implements the migration
// interface itself, so migrate drives the same tables through the same code
// paths as live practice.
var _ core.PracticeImporter = (*Service)(nil)

// ImportFactStats imports one fact's cumulative stats. key is the legacy
// storage key ("3x4", "2+9", …) as the mathfacts app wrote it.
func (s *Service) ImportFactStats(ctx context.Context, kid core.KidID, mode, key string, st core.FactStats) error {
	f := FactStats{
		Asked:    st.Asked,
		Correct:  st.Correct,
		Times:    append([]int(nil), st.Times...),
		MedianMS: st.MedianMS,
		BestMS:   st.BestMS,
	}
	return saveFact(s.db, string(kid), mode, key, &f)
}

// ImportSessions appends one finished legacy practice round or check-up.
func (s *Service) ImportSessions(ctx context.Context, kid core.KidID, sess core.Session) error {
	return insertSession(s.db, sessionRow{
		At:       sess.At,
		Kid:      string(kid),
		Mode:     sess.Mode,
		Checkup:  sess.Checkup,
		N:        sess.N,
		Correct:  sess.Correct,
		MedianMS: sess.MedianMS,
	})
}

// ImportAwards imports a kid's math daily awards: minutes earned toward the
// daily cap and the coin jar, with the day they were last reset on. The
// daily cap resumes exactly where the legacy app left it.
func (s *Service) ImportAwards(ctx context.Context, kid core.KidID, a core.Awards) error {
	aw := Awards{Day: a.Day, Earned: a.Earned, Coins: a.Coins}
	if aw.Day == "" {
		aw.Day = s.today()
	}
	return saveAwards(s.db, string(kid), bucketMath, &aw)
}

// ImportFlags imports one legacy flag. Keys embed the legacy kid name
// ("promo_bonus:<kid>", "best:<kid>:<mode>"); the same flag is also stored
// under the stable KidID so the once-ever promo bonus and best-round bars
// still hold after migration. The legacy-named key is kept for the record.
func (s *Service) ImportFlags(ctx context.Context, kid core.KidID, f core.Flag) error {
	val := flagString(f.Value)
	keys := []string{f.Key}
	switch {
	case strings.HasPrefix(f.Key, "promo_bonus:"):
		keys = append(keys, "promo_bonus:"+string(kid))
	case strings.HasPrefix(f.Key, "best:"):
		if parts := strings.SplitN(f.Key, ":", 3); len(parts) == 3 {
			keys = append(keys, "best:"+string(kid)+":"+parts[2])
		}
	}
	seen := map[string]bool{}
	for _, k := range keys {
		if seen[k] {
			continue
		}
		seen[k] = true
		if err := saveFlags(s.db, []flagRow{{key: k, value: val}}); err != nil {
			return err
		}
	}
	return nil
}

// ImportAwardLog appends one legacy game-minute award. Imported entries
// predate the crediter and carry no award id; the award log keeps the legacy
// kid name.
func (s *Service) ImportAwardLog(ctx context.Context, e core.AwardLogEntry) error {
	l := logRow{
		At:      e.At,
		Kid:     e.Kid,
		Kind:    e.Kind,
		Day:     e.At.In(s.loc).Format("2006-01-02"),
		Amount:  e.Amount,
		AwardID: "",
	}
	return insertLogs(s.db, []logRow{l})
}

// ImportSpelling imports a kid's spelling state: per-word records, answer
// sessions and the spelling coin jar.
func (s *Service) ImportSpelling(ctx context.Context, kid core.KidID, sp core.Spelling) error {
	kidStr := string(kid)
	return s.withTx(func(tx *sql.Tx) error {
		for _, r := range sp.Records {
			if err := saveSpellRecord(tx, kidStr, SpellRecord{
				Word: r.Word, Hits: r.Hits, Misses: r.Misses, BestMS: r.BestMS,
			}); err != nil {
				return err
			}
		}
		for _, sess := range sp.Sessions {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO practice_spell_sessions (kid, at, word, correct, ms) VALUES (?, ?, ?, ?, ?)`,
				kidStr, sess.At.UTC().Format(time.RFC3339), sess.Word, boolInt(sess.Correct), sess.Ms); err != nil {
				return err
			}
		}
		aw := Awards{Day: sp.Awards.Day, Earned: sp.Awards.Earned, Coins: sp.Awards.Coins}
		if aw.Day == "" {
			aw.Day = s.today()
		}
		return saveAwards(tx, kidStr, bucketSpell, &aw)
	})
}

// ImportBosses imports a kid's boss progress.
func (s *Service) ImportBosses(ctx context.Context, kid core.KidID, b core.BossState) error {
	bs := &core.BossState{
		Defeated:   append([]string(nil), b.Defeated...),
		HP:         copyIntMap(b.HP),
		PHP:        copyIntMap(b.PHP),
		PendingBid: b.PendingBid,
	}
	return s.saveBossState(string(kid), bs)
}

// ImportConfig imports the legacy config: global defaults plus per-kid
// overrides, stored under "global.<key>" and "kids.<kid>.<key>". Kid keys
// are stored as given; migrate resolves legacy names to KidIDs when it
// wants overrides to apply to a specific profile.
//
// Two per-kid keys are profile settings rather than numeric config and are
// routed to their own tables, mirroring the legacy KIDS entries: "tables_top"
// seeds the times-table ladder (practice_tables, e.g. Sam starts at 1-10
// instead of 1-3) and "base_mul" (non-zero) opens multiplication as a base
// mode (practice_flags "base_mode:<kid>:mul"), so an imported kid keeps its
// tier and access instead of being re-gated by records.
func (s *Service) ImportConfig(ctx context.Context, c core.Config) error {
	return s.withTx(func(tx *sql.Tx) error {
		for k, v := range c.Global {
			if _, err := tx.Exec(`INSERT INTO practice_config (key, value) VALUES (?, ?)
ON CONFLICT (key) DO UPDATE SET value=excluded.value`, "global."+k, v); err != nil {
				return err
			}
		}
		for kid, m := range c.Kids {
			if v, ok := m["tables_top"]; ok {
				if err := saveTablesTop(tx, kid, int(v)); err != nil {
					return err
				}
			}
			if v, ok := m["base_mul"]; ok && v != 0 {
				if err := saveFlags(tx, []flagRow{{key: "base_mode:" + kid + ":mul", value: "true"}}); err != nil {
					return err
				}
			}
			for k, v := range m {
				if k == "tables_top" || k == "base_mul" {
					continue
				}
				if _, err := tx.Exec(`INSERT INTO practice_config (key, value) VALUES (?, ?)
ON CONFLICT (key) DO UPDATE SET value=excluded.value`, "kids."+kid+"."+k, v); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// flagString renders a legacy flag value: booleans and floats are stored as
// the strings the runtime flag readers parse.
func flagString(v any) string {
	switch t := v.(type) {
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case string:
		return t
	default:
		return fmt.Sprint(v)
	}
}
