package practice

import (
	"errors"
	"testing"

	"github.com/edementhon/kids-shelf/internal/core"
)

func TestBossRoster(t *testing.T) {
	if len(Bosses) != 13 {
		t.Fatalf("bosses = %d, want 13", len(Bosses))
	}
	seen := map[string]bool{}
	byID := map[string]Boss{}
	for i, b := range Bosses {
		if seen[b.ID] {
			t.Fatalf("duplicate boss id %q", b.ID)
		}
		seen[b.ID] = true
		byID[b.ID] = b
		if b.ID == "" || b.Name == "" || b.Topic == "" {
			t.Errorf("boss %d incomplete: %+v", i, b)
		}
	}
	if Bosses[0].ID != "adder" || Bosses[12].ID != "wyrm" {
		t.Errorf("roster order broken: first=%s last=%s", Bosses[0].ID, Bosses[12].ID)
	}
	if !byID["alienfish"].Lethal || byID["alienfish"].PlayerHearts != 3 {
		t.Errorf("alienfish = %+v, want lethal with 3 hearts", byID["alienfish"])
	}
	if !byID["cavebeast"].Lethal || byID["cavebeast"].PlayerHearts != 4 {
		t.Errorf("cavebeast = %+v, want lethal with 4 hearts", byID["cavebeast"])
	}
	for id, b := range byID {
		if id != "alienfish" && id != "cavebeast" && b.Lethal {
			t.Errorf("boss %s should not be lethal", id)
		}
	}
	if BossHearts != 8 {
		t.Errorf("BossHearts = %d, want 8", BossHearts)
	}
}

func TestBossQuestionRanges(t *testing.T) {
	e := newEnv(t, 30)
	draw := func(topic string) Question {
		e.svc.mu.Lock()
		defer e.svc.mu.Unlock()
		return e.svc.bossQuestion(topic)
	}
	in := func(v, lo, hi int) bool { return v >= lo && v <= hi }
	for range 300 {
		if q := draw("add"); q.Op != "+" || !in(q.A, 1, 12) || !in(q.B, 1, 12) {
			t.Errorf("add = %+v", q)
		}
		if q := draw("sub"); q.Op != "-" || q.A < q.B || !in(q.A, 2, 20) || !in(q.B, 1, 9) {
			t.Errorf("sub = %+v", q)
		}
		for _, m := range []string{"mul3", "mul4", "mul6", "mul7"} {
			q := draw(m)
			wantA := map[string]int{"mul3": 3, "mul4": 4, "mul6": 6, "mul7": 7}[m]
			if q.Op != "x" || q.A != wantA || !in(q.B, 1, 10) {
				t.Errorf("%s = %+v", m, q)
			}
		}
		if q := draw("div"); q.Op != "/" || !in(q.B, 1, 10) || q.A%q.B != 0 || !in(q.A/q.B, 1, 10) {
			t.Errorf("div = %+v", q)
		}
		for _, topic := range []string{"ogre", "wyrm", "fish", "cave", "mixed"} {
			q := draw(topic)
			switch topic {
			case "ogre":
				if q.Op == "-" {
					if !in(q.A, 10, 30) || !in(q.B, 2, 9) || q.A < q.B {
						t.Errorf("ogre sub = %+v", q)
					}
				} else if q.Op == "/" {
					if !in(q.B, 1, 10) || q.A%q.B != 0 {
						t.Errorf("ogre div = %+v", q)
					}
				} else {
					t.Errorf("ogre op = %q", q.Op)
				}
			case "wyrm":
				if q.Op == "-" {
					if !in(q.A, 20, 60) || !in(q.B, 2, 9) {
						t.Errorf("wyrm sub = %+v", q)
					}
				} else if q.Op == "x" {
					if !in(q.A, 2, 9) || !in(q.B, 1, 10) {
						t.Errorf("wyrm x = %+v", q)
					}
				} else if q.Op == "/" {
					if !in(q.B, 1, 10) || q.A%q.B != 0 {
						t.Errorf("wyrm div = %+v", q)
					}
				} else if !in(q.A, 10, 25) || !in(q.B, 5, 25) {
					t.Errorf("wyrm add = %+v", q)
				}
			case "fish":
				switch q.Op {
				case "-":
					if !in(q.A, 15, 40) || !in(q.B, 2, 9) || q.A < q.B {
						t.Errorf("fish sub = %+v", q)
					}
				case "x":
					if !in(q.A, 2, 9) || !in(q.B, 1, 10) {
						t.Errorf("fish x = %+v", q)
					}
				case "/":
					if !in(q.B, 1, 10) || q.A%q.B != 0 {
						t.Errorf("fish div = %+v", q)
					}
				default:
					if !in(q.A, 10, 25) || !in(q.B, 5, 25) {
						t.Errorf("fish add = %+v", q)
					}
				}
			case "cave":
				switch q.Op {
				case "x":
					if !in(q.A, 3, 10) || !in(q.B, 3, 10) {
						t.Errorf("cave x = %+v", q)
					}
				case "/":
					if !in(q.B, 1, 10) || q.A%q.B != 0 {
						t.Errorf("cave div = %+v", q)
					}
				case "-":
					if !in(q.A, 30, 99) || !in(q.B, 10, 99) || q.A < q.B {
						t.Errorf("cave sub = %+v", q)
					}
				default:
					if !in(q.A, 25, 99) || !in(q.B, 15, 99) {
						t.Errorf("cave add = %+v", q)
					}
				}
			case "mixed":
				if q.Op == "+" {
					if !in(q.A, 1, 12) || !in(q.B, 1, 12) {
						t.Errorf("mixed add = %+v", q)
					}
				} else if q.Op == "-" {
					if !in(q.A, 2, 20) || !in(q.B, 1, 9) || q.A < q.B {
						t.Errorf("mixed sub = %+v", q)
					}
				} else if !in(q.A, 1, 10) || !in(q.B, 1, 10) {
					t.Errorf("mixed x = %+v", q)
				}
			}
		}
	}
}

// TestBossFightVictoryAndOnceOnlyCoins covers the once-only boss coins: the
// 8-heart fight, the 3-coin reward, the next boss and the stale-tab guard
// that keeps a replay from double-rewarding.
func TestBossFightVictoryAndOnceOnlyCoins(t *testing.T) {
	e := newEnv(t, 31)
	kid := "k1"

	f, qs, err := e.svc.BossRound(kid)
	if err != nil {
		t.Fatalf("BossRound: %v", err)
	}
	if f.Boss == nil || f.Boss.ID != "adder" || f.HP != BossHearts {
		t.Fatalf("round = %+v", f)
	}
	if len(qs) != BossHearts {
		t.Fatalf("questions = %d, want %d", len(qs), BossHearts)
	}
	seen := map[string]bool{}
	for _, q := range qs {
		if seen[q.Key()] {
			t.Fatalf("duplicate boss question %s", q.Key())
		}
		seen[q.Key()] = true
	}

	// Seven correct answers chip the boss down from 8 hearts.
	for i := range 7 {
		msg, err := e.svc.BossAnswer(ctx(), kid, 1, 1, "+", 2)
		if err != nil {
			t.Fatalf("BossAnswer %d: %v", i, err)
		}
		if msg.Kind != "hit" || msg.HP != BossHearts-1-i {
			t.Fatalf("answer %d = %+v", i, msg)
		}
	}
	// The eighth falls him: victory, 3 coins, next boss.
	msg, err := e.svc.BossAnswer(ctx(), kid, 1, 1, "+", 2)
	if err != nil {
		t.Fatalf("BossAnswer: %v", err)
	}
	if !msg.Victory || msg.Kind != "victory" {
		t.Fatalf("victory = %+v", msg)
	}
	if msg.Reward == nil || msg.Reward.Kind != "coin" || msg.Reward.Coins != 3 {
		t.Fatalf("reward = %+v, want 3 coins", msg.Reward)
	}
	if msg.NextBoss == nil || msg.NextBoss.ID != "minustaur" {
		t.Fatalf("next boss = %+v", msg.NextBoss)
	}
	bs, _ := e.svc.BossState(kid)
	if len(bs.Defeated) != 1 || bs.Defeated[0] != "adder" {
		t.Fatalf("defeated = %v", bs.Defeated)
	}
	aw, _ := e.svc.Awards(kid)
	if aw.Coins != 3 {
		t.Errorf("coins = %d, want 3", aw.Coins)
	}
	if len(e.cred.Credits()) != 0 {
		t.Errorf("3 coins should not bank a minute")
	}

	// A stale tab replaying the killing answer is rejected: the boss cannot
	// be defeated or paid twice.
	if _, err := e.svc.BossAnswer(ctx(), kid, 1, 1, "+", 2); !errors.Is(err, ErrBossChanged) {
		t.Fatalf("stale answer err = %v, want ErrBossChanged", err)
	}
	bs, _ = e.svc.BossState(kid)
	if len(bs.Defeated) != 1 {
		t.Fatalf("stale answer changed the defeated set")
	}
	aw, _ = e.svc.Awards(kid)
	if aw.Coins != 3 {
		t.Errorf("stale answer changed coins: %d", aw.Coins)
	}

	// A fresh round starts the next fight at full hearts.
	f, _, err = e.svc.BossRound(kid)
	if err != nil {
		t.Fatalf("BossRound: %v", err)
	}
	if f.Boss == nil || f.Boss.ID != "minustaur" || f.HP != BossHearts {
		t.Fatalf("next fight = %+v", f)
	}
}

func TestBossLethalKillAndRefill(t *testing.T) {
	e := newEnv(t, 32)
	kid := "k1"
	prior := []string{"adder", "minustaur", "times3", "golem", "diver", "buzzle", "times7"}
	if err := e.svc.ImportBosses(ctx(), core.KidID(kid), core.BossState{
		Defeated: prior, HP: map[string]int{}, PHP: map[string]int{},
	}); err != nil {
		t.Fatalf("ImportBosses: %v", err)
	}

	f, qs, err := e.svc.BossRound(kid)
	if err != nil {
		t.Fatalf("BossRound: %v", err)
	}
	if f.Boss == nil || f.Boss.ID != "alienfish" || !f.Lethal || f.PHP != 3 {
		t.Fatalf("alienfish fight = %+v", f)
	}
	if len(qs) != BossHearts {
		t.Fatalf("questions = %d", len(qs))
	}

	// Three misses whack the player out; the tail whack flags the hits.
	var killedMsg BossAnswerMsg
	for i := range 3 {
		msg, err := e.svc.BossAnswer(ctx(), kid, 1, 1, "+", 999)
		if err != nil {
			t.Fatalf("BossAnswer %d: %v", i, err)
		}
		if !msg.Whack {
			t.Errorf("miss %d should whack", i)
		}
		if i < 2 {
			if msg.Kind != "heal" || msg.PHP != 2-i {
				t.Fatalf("miss %d = %+v", i, msg)
			}
		} else {
			killedMsg = msg
		}
	}
	if !killedMsg.Killed || killedMsg.HP != BossHearts || killedMsg.PHP != 3 {
		t.Fatalf("killed = %+v", killedMsg)
	}

	// The boss refilled: the fight continues (the pending fight is intact)
	// and a correct answer chips a heart.
	msg, err := e.svc.BossAnswer(ctx(), kid, 1, 1, "+", 2)
	if err != nil {
		t.Fatalf("BossAnswer: %v", err)
	}
	if msg.Kind != "hit" || msg.HP != BossHearts-1 || msg.PHP != 3 {
		t.Fatalf("post-kill answer = %+v", msg)
	}
	bs, _ := e.svc.BossState(kid)
	if len(bs.Defeated) != 7 {
		t.Fatalf("killed boss should not be defeated: %v", bs.Defeated)
	}
}

func TestBossRewardCappedAtCap(t *testing.T) {
	e := newEnv(t, 33)
	kid := "k1"
	if err := e.svc.ImportAwards(ctx(), core.KidID(kid), core.Awards{Day: "2026-10-26", Earned: 3, Coins: 0}); err != nil {
		t.Fatalf("ImportAwards: %v", err)
	}
	if _, _, err := e.svc.BossRound(kid); err != nil {
		t.Fatalf("BossRound: %v", err)
	}
	var vic BossAnswerMsg
	for range BossHearts {
		msg, err := e.svc.BossAnswer(ctx(), kid, 1, 1, "+", 2)
		if err != nil {
			t.Fatalf("BossAnswer: %v", err)
		}
		vic = msg
	}
	if !vic.Victory {
		t.Fatalf("not a victory: %+v", vic)
	}
	if vic.Reward == nil || vic.Reward.Kind != "capped" {
		t.Fatalf("reward = %+v, want capped", vic.Reward)
	}
	aw, _ := e.svc.Awards(kid)
	if aw.Coins != 0 {
		t.Errorf("coins = %d, want 0 at cap", aw.Coins)
	}
}

// TestBossOrder walks the whole arena in order, then proves the arena closes.
func TestBossOrder(t *testing.T) {
	e := newEnv(t, 34)
	kid := "k1"
	for pos, want := range Bosses {
		f, _, err := e.svc.BossRound(kid)
		if err != nil {
			t.Fatalf("BossRound %d: %v", pos, err)
		}
		if f.Boss == nil || f.Boss.ID != want.ID {
			t.Fatalf("fight %d boss = %+v, want %s", pos, f.Boss, want.ID)
		}
		for range BossHearts {
			if _, err := e.svc.BossAnswer(ctx(), kid, 1, 1, "+", 2); err != nil {
				t.Fatalf("BossAnswer: %v", err)
			}
		}
	}
	if _, _, err := e.svc.BossRound(kid); !errors.Is(err, ErrNoBoss) {
		t.Fatalf("BossRound after clearing = %v, want ErrNoBoss", err)
	}
	st, err := e.svc.BossStatus(kid)
	if err != nil {
		t.Fatalf("BossStatus: %v", err)
	}
	if st.Boss != nil {
		t.Errorf("boss = %+v, want nil after clearing the arena", st.Boss)
	}
	if len(st.Defeated) != 13 {
		t.Errorf("defeated = %d, want 13", len(st.Defeated))
	}
}
