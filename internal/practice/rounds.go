package practice

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Live rounds: every math round, spelling round and boss fight the pages
// play is registered here with the exact questions served, in order. An
// answer names its round and the position of the question on screen, so
// the server grades only the question the kid is looking at:
//
//   - a second Enter or tap that sends the same position again replays the
//     first outcome instead of grading (or healing the boss) twice;
//   - an answer for a position the page has not shown yet, or for another
//     question than the one served there, is refused with ErrStaleAnswer
//     and changes nothing;
//   - a failed grade (a credit error, say) frees the position again, so
//     the page can retry the same answer.
//
// Rounds live in memory only: after a restart an open page gets
// ErrStaleAnswer and starts a new round, the same as a stale tab.

// ErrStaleAnswer reports an answer for a round the server is not running
// (an old tab, a restart) or for a question that is not the one on screen.
var ErrStaleAnswer = errors.New("practice: stale answer")

const (
	roundMath  = "math"
	roundSpell = "spell"
	roundBoss  = "boss"

	// maxLiveRounds bounds the open rounds kept per kid: enough for a
	// few tabs or devices, small enough that abandoned rounds cost nothing.
	maxLiveRounds = 6
)

type liveRound struct {
	id       string
	kind     string // roundMath+":"+mode, roundSpell or roundBoss
	items    []string
	next     int   // position of the question on screen
	busy     bool  // the question at next is being graded right now
	ended    bool  // a boss fight was won or lost: only replays remain
	outcomes []any // graded outcomes by position, replayed for repeats
}

// roundBook holds every kid's open rounds, newest last.
type roundBook struct {
	byKid map[string][]*liveRound
}

func newRoundID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b[:])
}

// openRound registers items as a kid's new live round and returns its id.
// A boss fight replaces any earlier fight (one boss, one set of hearts);
// other kinds keep the newest maxLiveRounds rounds.
func (s *Service) openRound(kid, kind string, items []string) string {
	s.roundsMu.Lock()
	defer s.roundsMu.Unlock()
	if s.rounds.byKid == nil {
		s.rounds.byKid = make(map[string][]*liveRound)
	}
	list := s.rounds.byKid[kid]
	if kind == roundBoss {
		kept := list[:0]
		for _, r := range list {
			if r.kind != roundBoss {
				kept = append(kept, r)
			}
		}
		list = kept
	}
	r := &liveRound{id: newRoundID(), kind: kind, items: items, outcomes: make([]any, len(items))}
	list = append(list, r)
	if len(list) > maxLiveRounds {
		list = append([]*liveRound(nil), list[len(list)-maxLiveRounds:]...)
	}
	s.rounds.byKid[kid] = list
	return r.id
}

// claim reserves position n of a kid's round for grading item. It returns
// the earlier outcome when n was already graded with the same item, or a
// done func the caller must call with the outcome (nil on failure, which
// frees the position for a retry).
func (s *Service) claim(kid, kind, id string, n int, item string) (replay any, done func(outcome any), err error) {
	s.roundsMu.Lock()
	defer s.roundsMu.Unlock()
	var r *liveRound
	for _, c := range s.rounds.byKid[kid] {
		if c.id == id && c.kind == kind {
			r = c
			break
		}
	}
	if r == nil {
		return nil, nil, fmt.Errorf("%w: no live round %q", ErrStaleAnswer, id)
	}
	if n < 0 || n >= len(r.items) || r.items[n] != item {
		return nil, nil, fmt.Errorf("%w: question %d is not %s", ErrStaleAnswer, n, item)
	}
	if n < r.next {
		return r.outcomes[n], nil, nil
	}
	if n > r.next || r.busy || r.ended {
		return nil, nil, fmt.Errorf("%w: question %d is not on screen", ErrStaleAnswer, n)
	}
	r.busy = true
	return nil, func(outcome any) {
		s.roundsMu.Lock()
		defer s.roundsMu.Unlock()
		r.busy = false
		if outcome != nil {
			r.outcomes[n] = outcome
			r.next = n + 1
			if m, ok := outcome.(BossAnswerMsg); ok && (m.Victory || m.Killed) {
				r.ended = true
			}
		}
	}, nil
}

func mathItem(mode string, a, b int, op string) string {
	return mode + ":" + fmt.Sprintf("%d%s%d", a, normOp(op), b)
}

func spellItem(word string) string { return strings.ToLower(strings.TrimSpace(word)) }

// OpenMathRound registers qs as a kid's live math (or check-up) round in
// mode and returns the round id the answers must carry.
func (s *Service) OpenMathRound(kid, mode string, qs []Question) string {
	items := make([]string, len(qs))
	for i, q := range qs {
		items[i] = mathItem(mode, q.A, q.B, q.Op)
	}
	return s.openRound(kid, roundMath+":"+mode, items)
}

// OpenSpellRound registers words as a kid's live spelling round and
// returns the round id the answers must carry.
func (s *Service) OpenSpellRound(kid string, words []string) string {
	items := make([]string, len(words))
	for i, w := range words {
		items[i] = spellItem(w)
	}
	return s.openRound(kid, roundSpell, items)
}

// RecordRoundAnswer is RecordAnswer for question n of a live math round:
// it grades only the question on screen and replays the outcome of a
// repeated send (see the live rounds note above).
func (s *Service) RecordRoundAnswer(ctx context.Context, kid, mode, round string, n, a, b int, op string, given, ms int) (AnswerOutcome, error) {
	replay, done, err := s.claim(kid, roundMath+":"+mode, round, n, mathItem(mode, a, b, op))
	if err != nil {
		return AnswerOutcome{}, err
	}
	if done == nil {
		return replay.(AnswerOutcome), nil
	}
	out, err := s.RecordAnswer(ctx, kid, mode, a, b, op, given, ms)
	if err != nil {
		done(nil)
		return AnswerOutcome{}, err
	}
	done(out)
	return out, nil
}

// RecordSpellRoundAnswer is RecordSpellAnswer for word n of a live
// spelling round, with the same guard as RecordRoundAnswer.
func (s *Service) RecordSpellRoundAnswer(ctx context.Context, kid, round string, n int, word, given string, ms int) (SpellOutcome, error) {
	replay, done, err := s.claim(kid, roundSpell, round, n, spellItem(word))
	if err != nil {
		return SpellOutcome{}, err
	}
	if done == nil {
		return replay.(SpellOutcome), nil
	}
	out, err := s.RecordSpellAnswer(ctx, kid, word, given, ms)
	if err != nil {
		done(nil)
		return SpellOutcome{}, err
	}
	done(out)
	return out, nil
}

// BossRoundAnswer is BossAnswer for question n of the kid's live boss
// fight (BossFight.Round from BossRound), with the same guard as
// RecordRoundAnswer. A win or a knock-out ends the fight: a repeat of the
// last answer replays it, any later answer for that fight is stale.
func (s *Service) BossRoundAnswer(ctx context.Context, kid, round string, n, a, b int, op string, given int) (BossAnswerMsg, error) {
	replay, done, err := s.claim(kid, roundBoss, round, n, mathItem(roundBoss, a, b, op))
	if err != nil {
		return BossAnswerMsg{}, err
	}
	if done == nil {
		return replay.(BossAnswerMsg), nil
	}
	msg, err := s.BossAnswer(ctx, kid, a, b, op, given)
	if err != nil {
		done(nil)
		return BossAnswerMsg{}, err
	}
	done(msg)
	return msg, nil
}
