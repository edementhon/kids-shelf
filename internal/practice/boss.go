package practice

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/edementhon/kids-shelf/internal/core"
)

// Boss is one of the 13 sequential arena fights. The roster is copied from
// the legacy boss_page.py.
type Boss struct {
	ID           string
	Name         string
	Emoji        string
	Topic        string
	Desc         string
	Lethal       bool
	PlayerHearts int
}

// Bosses is the sequential boss roster, in fight order.
var Bosses = []Boss{
	{ID: "adder", Name: "Plus-Bot", Emoji: "🤖", Topic: "add", Desc: "Addition facts"},
	{ID: "minustaur", Name: "Minustaur", Emoji: "🐂", Topic: "sub", Desc: "Subtraction facts"},
	{ID: "times3", Name: "Trax", Emoji: "🐉", Topic: "mul3", Desc: "The ×3 dragon"},
	{ID: "golem", Name: "Rocky Golem", Emoji: "🗿", Topic: "mul4", Desc: "The ×4 boulder beast"},
	{ID: "diver", Name: "Splitzo", Emoji: "🐙", Topic: "div", Desc: "Division facts"},
	{ID: "buzzle", Name: "Buzzle", Emoji: "🐝", Topic: "mul6", Desc: "The ×6 bumblebee"},
	{ID: "times7", Name: "Lucky", Emoji: "🦄", Topic: "mul7", Desc: "The ×7 unicorn"},
	{ID: "alienfish", Name: "Alien Fish", Emoji: "🐟", Topic: "fish", Desc: "Its tail whacks you: 3 misses and you're out!", Lethal: true, PlayerHearts: 3},
	{ID: "ogre", Name: "Doom Ogre", Emoji: "👹", Topic: "ogre", Desc: "Subtraction + division gauntlet"},
	{ID: "tofu", Name: "Giant Tofu", Emoji: "🧊", Topic: "tofu", Desc: "Big 2-digit sums and differences"},
	{ID: "cavebeast", Name: "Cave Beast", Emoji: "🦇", Topic: "cave", Desc: "Lurks in the dark: everything, harder", Lethal: true, PlayerHearts: 4},
	{ID: "mixed", Name: "Chaos King", Emoji: "👾", Topic: "mixed", Desc: "A bit of everything"},
	{ID: "wyrm", Name: "Dread Wyrm", Emoji: "🐲", Topic: "wyrm", Desc: "Big subtraction, ×, ÷: the final serpent"},
}

// BossHearts is the life bar of every boss fight (legacy BOSS_HEARTS).
const BossHearts = 8

// BossFight is the arena status the UI renders: the current undefeated boss
// (if any), the in-flight hearts and the math jar totals.
type BossFight struct {
	Boss           *Boss
	HP             int
	PHP            int // player hearts; meaningful for lethal bosses
	Lethal         bool
	Hearts         int
	Defeated       []string
	RewardCoins    int
	EarnedToday    float64
	Coins          int
	Cap            float64
	CoinsPerMinute int
	PendingBid     *string
}

// BossAnswerMsg is the outcome of one boss answer.
type BossAnswerMsg struct {
	Correct  bool
	Answer   int
	Kind     string // "hit", "heal", "victory" or "killed"
	HP       int
	PHP      int
	Whack    bool
	Victory  bool
	Killed   bool
	Reward   *CoinMsg
	NextBoss *Boss
}

func bossByID(id string) *Boss {
	for i := range Bosses {
		if Bosses[i].ID == id {
			return &Bosses[i]
		}
	}
	return nil
}

// bossQuestion draws one question for a boss topic, exactly as the legacy
// boss_question did: ×-bosses always draw the other factor from the full
// 1-10 tables (the boss IS the stretch), so every themed fight has at
// least 10 unique questions. Must be called with s.mu held (uses s.rng).
func (s *Service) bossQuestion(topic string) Question {
	switch topic {
	case "add":
		return Question{A: 1 + s.rng.IntN(12), B: 1 + s.rng.IntN(12), Op: "+"}
	case "sub":
		a := 2 + s.rng.IntN(19)
		b := 1 + s.rng.IntN(9)
		if b > a {
			a, b = b, a
		}
		return Question{A: a, B: b, Op: "-"}
	case "mul3", "mul4", "mul6", "mul7":
		t := map[string]int{"mul3": 3, "mul4": 4, "mul6": 6, "mul7": 7}[topic]
		return Question{A: t, B: 1 + s.rng.IntN(10), Op: "x"}
	case "div":
		b := 1 + s.rng.IntN(10)
		q := 1 + s.rng.IntN(10)
		return Question{A: b * q, B: b, Op: "/"}
	case "ogre":
		if s.rng.Float64() < 0.5 {
			a := 10 + s.rng.IntN(21)
			b := 2 + s.rng.IntN(8)
			if b > a {
				a, b = b, a
			}
			return Question{A: a, B: b, Op: "-"}
		}
		b := 1 + s.rng.IntN(10)
		q := 1 + s.rng.IntN(10)
		return Question{A: b * q, B: b, Op: "/"}
	case "wyrm":
		switch s.rng.IntN(4) {
		case 0: // sub 20..60 - 2..9
			return Question{A: 20 + s.rng.IntN(41), B: 2 + s.rng.IntN(8), Op: "-"}
		case 1: // x 2..9 × 1..10
			return Question{A: 2 + s.rng.IntN(8), B: 1 + s.rng.IntN(10), Op: "x"}
		case 2: // div
			b := 1 + s.rng.IntN(10)
			q := 1 + s.rng.IntN(10)
			return Question{A: b * q, B: b, Op: "/"}
		default: // add 10..25 + 5..25
			return Question{A: 10 + s.rng.IntN(16), B: 5 + s.rng.IntN(21), Op: "+"}
		}
	case "fish":
		switch s.rng.IntN(4) {
		case 0: // x
			return Question{A: 2 + s.rng.IntN(8), B: 1 + s.rng.IntN(10), Op: "x"}
		case 1: // div
			b := 1 + s.rng.IntN(10)
			q := 1 + s.rng.IntN(10)
			return Question{A: b * q, B: b, Op: "/"}
		case 2: // sub 15..40 - 2..9
			a := 15 + s.rng.IntN(26)
			b := 2 + s.rng.IntN(8)
			if b > a {
				a, b = b, a
			}
			return Question{A: a, B: b, Op: "-"}
		default: // add
			return Question{A: 10 + s.rng.IntN(16), B: 5 + s.rng.IntN(21), Op: "+"}
		}
	case "tofu":
		if s.rng.Float64() < 0.5 {
			return Question{A: 20 + s.rng.IntN(80), B: 10 + s.rng.IntN(90), Op: "+"}
		}
		a := 30 + s.rng.IntN(70)
		b := 10 + s.rng.IntN(90)
		if b > a {
			a, b = b, a
		}
		return Question{A: a, B: b, Op: "-"}
	case "cave":
		switch s.rng.IntN(4) {
		case 0: // x 3..10 × 3..10
			return Question{A: 3 + s.rng.IntN(8), B: 3 + s.rng.IntN(8), Op: "x"}
		case 1: // div
			b := 1 + s.rng.IntN(10)
			q := 1 + s.rng.IntN(10)
			return Question{A: b * q, B: b, Op: "/"}
		case 2: // sub 30..99 - 10..99
			a := 30 + s.rng.IntN(70)
			b := 10 + s.rng.IntN(90)
			if b > a {
				a, b = b, a
			}
			return Question{A: a, B: b, Op: "-"}
		default: // add 25..99 + 15..99
			return Question{A: 25 + s.rng.IntN(75), B: 15 + s.rng.IntN(85), Op: "+"}
		}
	default: // mixed: a bit of everything
		switch s.rng.IntN(3) {
		case 0:
			return Question{A: 1 + s.rng.IntN(12), B: 1 + s.rng.IntN(12), Op: "+"}
		case 1:
			a := 2 + s.rng.IntN(19)
			b := 1 + s.rng.IntN(9)
			if b > a {
				a, b = b, a
			}
			return Question{A: a, B: b, Op: "-"}
		default:
			return Question{A: 1 + s.rng.IntN(10), B: 1 + s.rng.IntN(10), Op: "x"}
		}
	}
}

// coreBossState is an alias kept local so boss state reads and writes carry
// the same shape core.PracticeImporter uses.
type coreBossState = core.BossState

func (s *Service) loadBossState(kid string) (*coreBossState, error) {
	var defeated, hp, php string
	var pending sql.NullString
	err := s.db.QueryRow(
		`SELECT defeated, hp, php, pending_bid FROM practice_bosses WHERE kid=?`,
		kid).Scan(&defeated, &hp, &php, &pending)
	if errors.Is(err, sql.ErrNoRows) {
		return &coreBossState{HP: map[string]int{}, PHP: map[string]int{}, Defeated: []string{}}, nil
	}
	if err != nil {
		return nil, err
	}
	bs := &coreBossState{HP: map[string]int{}, PHP: map[string]int{}, Defeated: []string{}}
	if err := json.Unmarshal([]byte(defeated), &bs.Defeated); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(hp), &bs.HP); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(php), &bs.PHP); err != nil {
		return nil, err
	}
	if pending.Valid {
		bs.PendingBid = &pending.String
	}
	return bs, nil
}

func (s *Service) saveBossState(kid string, bs *coreBossState) error {
	defeated, err := json.Marshal(bs.Defeated)
	if err != nil {
		return err
	}
	hp, err := json.Marshal(bs.HP)
	if err != nil {
		return err
	}
	php, err := json.Marshal(bs.PHP)
	if err != nil {
		return err
	}
	var pending any
	if bs.PendingBid != nil {
		pending = *bs.PendingBid
	}
	_, err = s.db.Exec(`INSERT INTO practice_bosses (kid, defeated, hp, php, pending_bid)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (kid) DO UPDATE SET
    defeated=excluded.defeated, hp=excluded.hp, php=excluded.php,
    pending_bid=excluded.pending_bid`,
		kid, string(defeated), string(hp), string(php), pending)
	return err
}

// currentBoss returns the first undefeated boss, nil when the arena is
// cleared.
func currentBoss(bs *coreBossState) *Boss {
	for i := range Bosses {
		if !contains(bs.Defeated, Bosses[i].ID) {
			return &Bosses[i]
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// BossState returns a kid's stored boss progress (defeated set, in-flight
// hearts and the pending fight).
func (s *Service) BossState(kid string) (core.BossState, error) {
	bs, err := s.loadBossState(kid)
	if err != nil {
		return core.BossState{}, err
	}
	return core.BossState{
		Defeated:   append([]string(nil), bs.Defeated...),
		HP:         copyIntMap(bs.HP),
		PHP:        copyIntMap(bs.PHP),
		PendingBid: bs.PendingBid,
	}, nil
}

// BossRound starts a fresh fight with the current undefeated boss: marks the
// fight in pending_bid (stale-tab guard), resets the boss to full hearts and
// draws 8 questions, unique within the fight, never back-to-back repeats
// when the themed pool runs dry. Any in-flight fight is replaced.
func (s *Service) BossRound(kid string) (BossFight, []Question, error) {
	cfg, err := s.ConfigFor(kid)
	if err != nil {
		return BossFight{}, nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	bs, err := s.loadBossState(kid)
	if err != nil {
		return BossFight{}, nil, err
	}
	cur := currentBoss(bs)
	if cur == nil {
		return BossFight{}, nil, ErrNoBoss
	}
	bid := cur.ID

	bs.PendingBid = &bid
	qs, seen, tries := []Question{}, map[string]bool{}, 0
	for len(qs) < BossHearts && tries < BossHearts*60 {
		tries++
		q := s.bossQuestion(cur.Topic)
		k := q.Key()
		if seen[k] {
			continue
		}
		seen[k] = true
		qs = append(qs, q)
	}
	for len(qs) < BossHearts {
		q := s.bossQuestion(cur.Topic)
		if len(qs) > 0 && q.Key() == qs[len(qs)-1].Key() {
			continue
		}
		qs = append(qs, q)
	}
	// Reset the fight: the boss refills, the player's current hearts are kept.
	bs.HP[bid] = BossHearts
	if err := s.saveBossState(kid, bs); err != nil {
		return BossFight{}, nil, err
	}
	aw, err := s.awardsFor(kid, bucketMath)
	if err != nil {
		return BossFight{}, nil, err
	}
	f := BossFight{
		Boss:           bossByID(bid),
		HP:             BossHearts,
		Lethal:         cur.Lethal,
		Hearts:         BossHearts,
		Defeated:       append([]string(nil), bs.Defeated...),
		RewardCoins:    cfg.BossCoins,
		EarnedToday:    aw.Earned,
		Coins:          aw.Coins,
		Cap:            cfg.AwardDailyCap,
		CoinsPerMinute: cfg.CoinsPerMinute,
		PendingBid:     bs.PendingBid,
	}
	f.PHP = cur.PlayerHearts
	if cur.Lethal {
		if php, ok := bs.PHP[bid]; ok {
			f.PHP = php
		}
	}
	return f, qs, nil
}

// BossStatus renders the arena: the current undefeated boss, the fight's
// hearts and the math jar totals.
func (s *Service) BossStatus(kid string) (BossFight, error) {
	cfg, err := s.ConfigFor(kid)
	if err != nil {
		return BossFight{}, err
	}
	bs, err := s.loadBossState(kid)
	if err != nil {
		return BossFight{}, err
	}
	aw, err := s.awardsFor(kid, bucketMath)
	if err != nil {
		return BossFight{}, err
	}
	f := BossFight{
		Hearts:         BossHearts,
		Defeated:       append([]string(nil), bs.Defeated...),
		RewardCoins:    cfg.BossCoins,
		EarnedToday:    aw.Earned,
		Coins:          aw.Coins,
		Cap:            cfg.AwardDailyCap,
		CoinsPerMinute: cfg.CoinsPerMinute,
		PendingBid:     bs.PendingBid,
	}
	if cur := currentBoss(bs); cur != nil {
		f.Boss = bossByID(cur.ID)
		f.Lethal = cur.Lethal
		f.HP = bs.HP[cur.ID]
		if f.HP == 0 {
			f.HP = BossHearts
		}
		f.PHP = cur.PlayerHearts
		if cur.Lethal {
			if php, ok := bs.PHP[cur.ID]; ok {
				f.PHP = php
			}
		}
	}
	return f, nil
}

// BossAnswer grades one boss answer and moves the fight: a correct answer
// takes a heart (wrong answers give one back), lethal bosses whack the
// player per miss. Clearing the boss's hearts defeats it once, the boss
// coins (3) go to the math jar, and a lethal boss whose player hearts hit
// zero refills instead. Answers only count while the round's pending fight
// matches the current boss; a stale tab gets ErrBossChanged.
func (s *Service) BossAnswer(ctx context.Context, kid string, a, b int, op string, given int) (BossAnswerMsg, error) {
	op = normOp(op)
	correct := given == AnswerOf(a, b, op)
	cfg, err := s.ConfigFor(kid)
	if err != nil {
		return BossAnswerMsg{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	bs, err := s.loadBossState(kid)
	if err != nil {
		return BossAnswerMsg{}, err
	}
	cur := currentBoss(bs)
	if cur == nil {
		return BossAnswerMsg{}, ErrNoBoss
	}
	bid := cur.ID
	if bs.PendingBid != nil && *bs.PendingBid != bid {
		return BossAnswerMsg{}, fmt.Errorf("%w: pending %s, current %s", ErrBossChanged, *bs.PendingBid, bid)
	}
	lethal := cur.Lethal
	phearts := cur.PlayerHearts
	hp := bs.HP[bid]
	if hp == 0 {
		hp = BossHearts
	}
	php := phearts
	if lethal {
		if v, ok := bs.PHP[bid]; ok {
			php = v
		}
	}

	if correct {
		hp = max(0, hp-1)
	} else {
		hp = min(BossHearts, hp+1)
		if lethal {
			php = max(0, php-1)
		}
	}
	bs.HP[bid] = hp
	if lethal {
		bs.PHP[bid] = php
	}

	msg := BossAnswerMsg{Correct: correct, Answer: AnswerOf(a, b, op), HP: hp, PHP: php}
	if correct {
		msg.Kind = "hit"
	} else {
		msg.Kind = "heal"
		if lethal {
			msg.Whack = true
		}
	}

	if hp <= 0 { // VICTORY: the boss falls, once
		bs.Defeated = append(bs.Defeated, bid)
		delete(bs.HP, bid)
		delete(bs.PHP, bid)
		aw, err := s.awardsFor(kid, bucketMath)
		if err != nil {
			return BossAnswerMsg{}, err
		}
		var logs []logRow
		if aw.Earned >= cfg.AwardDailyCap-capEpsilon {
			msg.Reward = &CoinMsg{Kind: "capped"}
		} else {
			reward, err := s.bankCoins(ctx, kid, aw, kindJar, cfg.BossCoins, cfg, &logs)
			if err != nil {
				return BossAnswerMsg{}, err
			}
			msg.Reward = reward
		}
		if err := s.saveBossState(kid, bs); err != nil {
			return BossAnswerMsg{}, err
		}
		if err := saveAwards(s.db, kid, bucketMath, aw); err != nil {
			return BossAnswerMsg{}, err
		}
		if err := insertLogs(s.db, logs); err != nil {
			return BossAnswerMsg{}, err
		}
		msg.Kind = "victory"
		msg.Victory = true
		msg.NextBoss = currentBoss(bs)
		return msg, nil
	}
	if lethal && php <= 0 { // KILLED: boss refills, retry later
		bs.HP[bid] = BossHearts
		bs.PHP[bid] = phearts
		msg.Kind = "killed"
		msg.Killed = true
		msg.HP = BossHearts
		msg.PHP = phearts
	}
	if err := s.saveBossState(kid, bs); err != nil {
		return BossAnswerMsg{}, err
	}
	return msg, nil
}

func copyIntMap(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
