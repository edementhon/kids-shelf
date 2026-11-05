package jobs

import (
	"errors"
	"slices"
	"testing"
	"time"
)

// TestLineupOrderAndPlace pins the order the parent pages list jobs in:
// running first, then failed and paused (newest first), then queued in
// claim order, then done, most recently finished first. Each queued job's
// place in line is the order ClaimRange hands it out within its lane.
func TestLineupOrderAndPlace(t *testing.T) {
	q := newTestQueue(t)
	clock := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	q.now = func() time.Time {
		clock = clock.Add(time.Second)
		return clock
	}
	ids := map[string]string{}
	enqueue := func(name string, priority int) {
		t.Helper()
		id, err := q.Enqueue("kind", map[string]string{"name": name})
		if err != nil {
			t.Fatalf("Enqueue %s: %v", name, err)
		}
		if priority != 0 {
			if err := q.Requeue(id, "", priority); err != nil {
				t.Fatalf("Requeue %s: %v", name, err)
			}
		}
		ids[name] = id
	}
	claim := func(name string) {
		t.Helper()
		job, err := q.Claim("kind")
		if err != nil || job == nil || job.ID != ids[name] {
			t.Fatalf("Claim = %+v, %v; want %s", job, err, name)
		}
	}
	// Enqueue order: doneOld, doneNew, failed, paused, running, then the
	// waiting ones, with a fix-lane job (priority 5) enqueued before two
	// sort-lane ones (priority 0) and another fix-lane one (priority 3).
	for _, n := range []string{"doneOld", "doneNew", "failed", "paused", "running"} {
		enqueue(n, 0)
	}
	enqueue("fixA", 5)
	enqueue("waitA", 0)
	enqueue("fixB", 3)
	enqueue("waitB", 0)
	for _, n := range []string{"doneOld", "doneNew", "failed", "paused", "running"} {
		claim(n)
	}
	// doneNew finishes first and doneOld last, so doneOld is the most
	// recently finished although it was enqueued first.
	if err := q.Done(ids["doneNew"]); err != nil {
		t.Fatal(err)
	}
	if err := q.Done(ids["doneOld"]); err != nil {
		t.Fatal(err)
	}
	if err := q.Fail(ids["failed"], errors.New("broken")); err != nil {
		t.Fatal(err)
	}
	if err := q.Pause(ids["paused"], "disk full"); err != nil {
		t.Fatal(err)
	}

	lanes := []Lane{{0, 0}, {1, 1 << 30}}
	list, err := q.List("kind")
	if err != nil {
		t.Fatal(err)
	}
	place := Lineup(list, lanes...)
	name := map[string]string{}
	for n, id := range ids {
		name[id] = n
	}
	var got []string
	for _, j := range list {
		got = append(got, name[j.ID])
	}
	want := []string{"running", "paused", "failed", "waitA", "waitB", "fixB", "fixA", "doneOld", "doneNew"}
	if !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
	wantPlace := map[string]int{"waitA": 1, "waitB": 2, "fixB": 1, "fixA": 2}
	for n, id := range ids {
		if place[id] != wantPlace[n] {
			t.Errorf("place of %s = %d, want %d", n, place[id], wantPlace[n])
		}
	}

	// The place in line is what each lane really claims next.
	for l, order := range [][]string{{"waitA", "waitB"}, {"fixB", "fixA"}} {
		for i, n := range order {
			if place[ids[n]] != i+1 {
				t.Fatalf("%s has place %d, but is claim %d of its lane", n, place[ids[n]], i+1)
			}
			job, err := q.ClaimRange("kind", lanes[l].Min, lanes[l].Max)
			if err != nil || job == nil || job.ID != ids[n] {
				t.Fatalf("ClaimRange %v = %+v, %v; want %s", lanes[l], job, err, n)
			}
		}
	}

	// Without lanes every job is in one line, the way Claim hands them out.
	enqueue("x", 2)
	enqueue("y", 0)
	list, err = q.List("kind")
	if err != nil {
		t.Fatal(err)
	}
	place = Lineup(list)
	if place[ids["y"]] != 1 || place[ids["x"]] != 2 {
		t.Errorf("single-line places: y=%d x=%d, want 1 and 2", place[ids["y"]], place[ids["x"]])
	}
	claim("y")
	claim("x")
}
