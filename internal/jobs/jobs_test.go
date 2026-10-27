package jobs

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/edementhon/kids-shelf/internal/store"
)

func newTestQueue(t *testing.T) *Queue {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	q, err := New(s)
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	return q
}

func TestClaimIsExclusiveAcrossGoroutines(t *testing.T) {
	q := newTestQueue(t)
	const total = 40
	for i := range total {
		if _, err := q.Enqueue("kind", map[string]int{"i": i}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
	var (
		mu   sync.Mutex
		seen = make(map[string]bool)
		wg   sync.WaitGroup
	)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				job, err := q.Claim("kind")
				if err != nil {
					t.Errorf("Claim: %v", err)
					return
				}
				if job == nil {
					return // queue drained
				}
				if job.State != StateRunning {
					t.Errorf("claimed job %s state = %s, want %s", job.ID, job.State, StateRunning)
				}
				mu.Lock()
				if seen[job.ID] {
					t.Errorf("job %s claimed twice", job.ID)
				}
				seen[job.ID] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != total {
		t.Errorf("claimed %d distinct jobs, want %d", len(seen), total)
	}
	// Everything was claimed: no job is left queued.
	job, err := q.Claim("kind")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if job != nil {
		t.Errorf("Claim after drain returned job %s, want nil", job.ID)
	}
	all, err := q.List("kind")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != total {
		t.Errorf("List returned %d jobs, want %d", len(all), total)
	}
	for i := range all {
		if all[i].State != StateRunning {
			t.Errorf("job %s state = %s, want %s", all[i].ID, all[i].State, StateRunning)
		}
	}
}

func TestRecoverRequeuesRunningJobs(t *testing.T) {
	q := newTestQueue(t)
	id, err := q.Enqueue("kind", map[string]string{"a": "b"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	job, err := q.Claim("kind")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if job == nil || job.ID != id {
		t.Fatalf("Claim returned %+v, want job %s", job, id)
	}
	n, err := q.Recover()
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n != 1 {
		t.Errorf("Recover requeued %d jobs, want 1", n)
	}
	got, err := q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateQueued {
		t.Errorf("after Recover state = %s, want %s", got.State, StateQueued)
	}
	if got.Attempts != 1 {
		t.Errorf("after Recover attempts = %d, want 1", got.Attempts)
	}
	// A second Recover is a no-op: the first one requeued the only running
	// job, so nothing is left running to recover.
	n, err = q.Recover()
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n != 0 {
		t.Errorf("second Recover requeued %d jobs, want 0", n)
	}
	// The same job is claimable again, keeping its attempt count.
	job2, err := q.Claim("kind")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if job2 == nil || job2.ID != id {
		t.Fatalf("Claim after Recover returned %+v, want job %s", job2, id)
	}
	if job2.Attempts != 1 {
		t.Errorf("reclaimed job attempts = %d, want 1", job2.Attempts)
	}
}

func TestJobLifecycle(t *testing.T) {
	q := newTestQueue(t)
	type payload struct {
		URL      string `json:"url"`
		Playlist bool   `json:"playlist"`
	}
	id, err := q.Enqueue("youtube", payload{URL: "https://example.com/video", Playlist: true})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	job, err := q.Claim("youtube")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	var p payload
	if err := job.DecodePayload(&p); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if p.URL != "https://example.com/video" || !p.Playlist {
		t.Errorf("payload = %+v, want URL and Playlist set", p)
	}
	// Advance replaces stage and payload.
	if err := q.Advance(id, "downloading", payload{URL: p.URL}); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	job, err = q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.Stage != "downloading" {
		t.Errorf("stage = %s, want downloading", job.Stage)
	}
	// Advance with nil payload keeps the payload, only the stage moves.
	if err := q.Advance(id, "verifying", nil); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	job, _ = q.Get(id)
	if job.Stage != "verifying" {
		t.Errorf("stage = %s, want verifying", job.Stage)
	}
	if err := job.DecodePayload(&p); err != nil || p.Playlist {
		t.Errorf("nil-payload Advance changed payload: %+v, %v", p, err)
	}
	// Pause records the reason; a later Advance resumes.
	if err := q.Pause(id, "disk full"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	job, _ = q.Get(id)
	if job.State != StatePaused || job.Error != "disk full" {
		t.Errorf("after Pause: state=%s error=%q", job.State, job.Error)
	}
	if err := q.Advance(id, "resumed", nil); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	job, _ = q.Get(id)
	if job.State != StateRunning {
		t.Errorf("after resume Advance: state = %s, want %s", job.State, StateRunning)
	}
	if err := q.Done(id); err != nil {
		t.Fatalf("Done: %v", err)
	}
	job, _ = q.Get(id)
	if job.State != StateDone || job.Error != "" {
		t.Errorf("after Done: state=%s error=%q", job.State, job.Error)
	}

	// Fail stores the error.
	fid, err := q.Enqueue("youtube", payload{URL: "https://example.com/broken"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := q.Claim("youtube"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := q.Fail(fid, errors.New("interrupted")); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	fjob, err := q.Get(fid)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if fjob.State != StateFailed || fjob.Error != "interrupted" {
		t.Errorf("after Fail: state=%s error=%q", fjob.State, fjob.Error)
	}

	// Get of an unknown id.
	if _, err := q.Get("does-not-exist"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get unknown: got %v, want ErrNotFound", err)
	}

	// List filters by kind, newest first; creation timestamps are monotone.
	all, err := q.List("youtube")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("List(youtube) len = %d, want 2", len(all))
	}
	got := map[string]bool{}
	for i := range all {
		got[all[i].ID] = true
	}
	if !got[id] || !got[fid] {
		t.Errorf("List(youtube) = %v, want jobs %s and %s", got, id, fid)
	}
	if all[0].Created.Before(all[1].Created) {
		t.Errorf("List not newest first: %s before %s", all[0].Created, all[1].Created)
	}
	none, err := q.List("kiwix")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("List(kiwix) len = %d, want 0", len(none))
	}
}

func TestReservationAccounting(t *testing.T) {
	q := newTestQueue(t)
	id1, err := q.Enqueue("ingest", nil)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	id2, err := q.Enqueue("ingest", nil)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := q.Claim("ingest"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, err := q.Claim("ingest"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := q.SetReservedBytes(id1, 100); err != nil {
		t.Fatalf("SetReservedBytes: %v", err)
	}
	if err := q.SetReservedBytes(id2, 50); err != nil {
		t.Fatalf("SetReservedBytes: %v", err)
	}
	sum, err := q.SumActiveReservedBytes("")
	if err != nil {
		t.Fatalf("SumActiveReservedBytes: %v", err)
	}
	if sum != 150 {
		t.Errorf("sum = %d, want 150", sum)
	}
	sum, err = q.SumActiveReservedBytes(id1)
	if err != nil {
		t.Fatalf("SumActiveReservedBytes: %v", err)
	}
	if sum != 50 {
		t.Errorf("sum excluding %s = %d, want 50", id1, sum)
	}
	// Finished jobs stop counting.
	if err := q.Done(id1); err != nil {
		t.Fatalf("Done: %v", err)
	}
	sum, err = q.SumActiveReservedBytes("")
	if err != nil {
		t.Fatalf("SumActiveReservedBytes: %v", err)
	}
	if sum != 50 {
		t.Errorf("sum after Done = %d, want 50", sum)
	}
	if err := q.Fail(id2, fmt.Errorf("boom")); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	sum, err = q.SumActiveReservedBytes("")
	if err != nil {
		t.Fatalf("SumActiveReservedBytes: %v", err)
	}
	if sum != 0 {
		t.Errorf("sum after Fail = %d, want 0", sum)
	}
}
func TestReservationForUnknownJobFails(t *testing.T) {
	// TryReserve on an id with no job row must fail loudly: the old
	// behavior updated zero rows and reported success, so a reservation
	// whose job vanished silently never existed.
	q := newTestQueue(t)
	id, err := q.Enqueue("ingest", nil)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, _, err := q.TryReserve("no-such-job", 1, 100, 10, "keyA"); !errors.Is(err, ErrNotFound) {
		t.Errorf("TryReserve on an unknown id: got %v, want ErrNotFound", err)
	}
	if err := q.SetReservedBytes("no-such-job", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetReservedBytes(unknown id, nonzero) = %v, want ErrNotFound", err)
	}
	// A zero release of an id that never held anything stays harmless
	// (the launcher releases snapshot reservations of stages that never
	// made one).
	if err := q.SetReservedBytes("no-such-job", 0); err != nil {
		t.Errorf("SetReservedBytes(unknown id, 0) = %v, want nil", err)
	}
	// The real job still reserves normally.
	if _, ok, err := q.TryReserve(id, 10, 100, 10, "keyA"); err != nil || !ok {
		t.Errorf("TryReserve on a real job = (%d, %v, %v), want ok", 0, ok, err)
	}
}

func TestStandaloneReservationSurvivesReopen(t *testing.T) {
	// A standalone reservation (the updater's update:* ids have no job
	// row) is persisted in the reservations table: it survives a store
	// close and reopen and counts against later reservations on the same
	// filesystem.
	path := filepath.Join(t.TempDir(), "test.sqlite")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	q, err := New(s)
	if err != nil {
		s.Close()
		t.Fatalf("jobs.New: %v", err)
	}
	other, ok, err := q.TryHoldReservation("update:snapshot:v1", 500, 1000, 100, "keyA")
	if err != nil {
		t.Fatalf("TryHoldReservation: %v", err)
	}
	if ok != true || other != 0 {
		t.Fatalf("TryHoldReservation = (other %d, ok %v), want (0, true)", other, ok)
	}
	// Re-holding replaces the previous amount without double counting.
	if _, ok, err := q.TryHoldReservation("update:snapshot:v1", 400, 1000, 100, "keyA"); err != nil || !ok {
		t.Fatalf("re-hold = (ok %v, err %v), want ok", ok, err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	q2, err := New(s2)
	if err != nil {
		t.Fatalf("jobs.New after reopen: %v", err)
	}
	sum, err := q2.SumActiveReservedBytes("")
	if err != nil {
		t.Fatalf("SumActiveReservedBytes: %v", err)
	}
	if sum != 400 {
		t.Errorf("sum after reopen = %d, want 400 (the standalone reservation)", sum)
	}
	// It counts against later Reserve calls on the same filesystem: 900
	// usable above the floor, 400 already held, so 500 fits and 501 does
	// not.
	job, err := q2.Enqueue("ingest", nil)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, ok, err := q2.TryReserve(job, 501, 1000, 100, "keyA"); err != nil || ok {
		t.Errorf("TryReserve 501 over a 400-byte hold = (ok %v, err %v), want refused", ok, err)
	}
	if _, ok, err := q2.TryReserve(job, 500, 1000, 100, "keyA"); err != nil || !ok {
		t.Errorf("TryReserve 500 up to the hold = (ok %v, err %v), want accepted", ok, err)
	}
	// Release clears the hold; it stops counting.
	if err := q2.SetReservedBytes("update:snapshot:v1", 0); err != nil {
		t.Fatalf("SetReservedBytes(hold, 0): %v", err)
	}
	sum, err = q2.SumActiveReservedBytes("")
	if err != nil {
		t.Fatalf("SumActiveReservedBytes after release: %v", err)
	}
	if sum != 500 {
		t.Errorf("sum after releasing the hold = %d, want 500 (only the job's)", sum)
	}
}

func TestReservationsArePerFilesystem(t *testing.T) {
	// TryReserve sums only the reservations on the same filesystem key: a
	// reservation on keyB neither blocks nor is blocked by one on keyA.
	q := newTestQueue(t)
	idA, err := q.Enqueue("ingest", nil)
	if err != nil {
		t.Fatalf("Enqueue a: %v", err)
	}
	idB, err := q.Enqueue("ingest", nil)
	if err != nil {
		t.Fatalf("Enqueue b: %v", err)
	}
	if other, ok, err := q.TryReserve(idA, 40, 100, 10, "keyA"); err != nil || !ok || other != 0 {
		t.Fatalf("TryReserve a = (other %d, ok %v, err %v), want (0, true, nil)", other, ok, err)
	}
	// On keyB the full 90 are still free: a's 40 do not count against b.
	if other, ok, err := q.TryReserve(idB, 40, 100, 10, "keyB"); err != nil || !ok || other != 0 {
		t.Fatalf("TryReserve b on keyB = (other %d, ok %v, err %v), want (0, true, nil)", other, ok, err)
	}
	// On keyA only 50 are left behind a's 40: 51 must be refused.
	idC, err := q.Enqueue("ingest", nil)
	if err != nil {
		t.Fatalf("Enqueue c: %v", err)
	}
	if other, ok, err := q.TryReserve(idC, 51, 100, 10, "keyA"); err != nil || ok || other != 40 {
		t.Fatalf("TryReserve c on keyA = (other %d, ok %v, err %v), want (40, false, nil)", other, ok, err)
	}
	// Standalone holds follow the same per-filesystem sums: on keyB only
	// 50 are left behind b's 40, and a hold on keyB never touches keyA.
	if _, ok, err := q.TryHoldReservation("update:staged:v2", 51, 100, 10, "keyB"); err != nil || ok {
		t.Fatalf("TryHoldReservation 51 on keyB = (ok %v, err %v), want refused", ok, err)
	}
	if _, ok, err := q.TryHoldReservation("update:staged:v2", 50, 100, 10, "keyB"); err != nil || !ok {
		t.Fatalf("TryHoldReservation 50 on keyB = (ok %v, err %v), want ok", ok, err)
	}
	if other, ok, err := q.TryReserve(idC, 50, 100, 10, "keyA"); err != nil || !ok || other != 40 {
		t.Fatalf("TryReserve c after a keyB hold = (other %d, ok %v, err %v), want (40, true, nil)", other, ok, err)
	}
}

func TestClaimAndListFollowEnqueueOrder(t *testing.T) {
	// A tight loop enqueues everything in the same second; the random ids
	// would tie-break randomly, so Claim must follow seq, and List reverse
	// seq, to reconstruct the real enqueue order.
	q := newTestQueue(t)
	const total = 20
	order := make([]string, 0, total)
	for i := range total {
		id, err := q.Enqueue("kind", map[string]int{"i": i})
		if err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
		order = append(order, id)
	}
	for i := range total {
		job, err := q.Claim("kind")
		if err != nil {
			t.Fatalf("Claim %d: %v", i, err)
		}
		if job == nil {
			t.Fatalf("Claim %d returned nil, want %s", i, order[i])
		}
		if job.ID != order[i] {
			t.Fatalf("Claim %d = %s, want %s (enqueue order)", i, job.ID, order[i])
		}
	}
	all, err := q.List("kind")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != total {
		t.Fatalf("List len = %d, want %d", len(all), total)
	}
	for i := range all {
		if want := order[total-1-i]; all[i].ID != want {
			t.Fatalf("List[%d] = %s, want %s (reverse enqueue order)", i, all[i].ID, want)
		}
	}
}

// TestClaimOrdersByPriorityThenSeq pins the claim order the show-first
// decision runs on: any not-yet-shown job (priority 0) claims before an
// already-shown job's improvement (priority 1-3), no matter when either was
// enqueued, and among equal priorities the seq (enqueue) order holds.
func TestClaimOrdersByPriorityThenSeq(t *testing.T) {
	q := newTestQueue(t)
	enqueue := func() string {
		id, err := q.Enqueue("ingest", map[string]string{"kind": "folder"})
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		return id
	}
	// B is an old improvement (claimed, then requeued at a higher
	// priority); A and C are fresh show jobs enqueued after it.
	b := enqueue()
	job, err := q.Claim("ingest")
	if err != nil || job == nil || job.ID != b {
		t.Fatalf("Claim = %+v, %v, want %s", job, err, b)
	}
	if err := q.Requeue(b, "convert", 2); err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	a := enqueue()
	c := enqueue()
	for i, want := range []string{a, c, b} {
		job, err := q.Claim("ingest")
		if err != nil {
			t.Fatalf("Claim %d: %v", i, err)
		}
		if job == nil || job.ID != want {
			t.Fatalf("Claim %d = %+v, want %s (show jobs ahead of older improvements)", i, job, want)
		}
	}
	// Among improvements audio (1) precedes full (2) precedes remux (3).
	remux := enqueue()
	job, _ = q.Claim("ingest")
	if err := q.Requeue(remux, "convert", 3); err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	full := enqueue()
	job, _ = q.Claim("ingest")
	if err := q.Requeue(full, "convert", 2); err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	audio := enqueue()
	job, _ = q.Claim("ingest")
	if err := q.Requeue(audio, "convert", 1); err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	for i, want := range []string{audio, full, remux} {
		job, err := q.Claim("ingest")
		if err != nil {
			t.Fatalf("Claim %d: %v", i, err)
		}
		if job == nil || job.ID != want {
			t.Fatalf("improvement Claim %d = %+v, want %s", i, job, want)
		}
	}
}

// TestFailedFolderJobsRequeuedByMigration pins the v4 migration's second
// chance: ingest jobs failed under the old rules (their folder/NAS source
// is readable from the payload JSON) are requeued at the start, staged for
// the new show phase with no error and priority 0; failed YouTube jobs keep
// their failure (they have their own Retry button).
func TestFailedFolderJobsRequeuedByMigration(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	// A database from before the show-first change: jobs v1-v3 only.
	if err := s.Migrate(Migrations[:3]...); err != nil {
		t.Fatalf("migrate v1-v3: %v", err)
	}
	now := "2026-10-01T00:00:00Z"
	insert := func(id, payload string) {
		t.Helper()
		if _, err := s.DB().Exec(
			`INSERT INTO jobs (id, kind, stage, state, payload, error, created, updated, seq)
			 VALUES (?, 'ingest', 'convert', 'failed', ?, 'something broke', ?, ?, 1)`,
			id, payload, now, now,
		); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	insert("folder", `{"file":"/mnt/a.mkv","source":{"kind":"folder","path":"/mnt/a.mkv","title":"A","show":"S"}}`)
	insert("nas", `{"file":"/nas/b.mp4","source":{"kind":"nas","path":"/nas/b.mp4","title":"B","show":"S"}}`)
	insert("youtube", `{"file":"/dl/c.mp4","source":{"kind":"youtube","path":"https://www.youtube.com/watch?v=abc","title":"C","show":"S"}}`)

	// The new migration: priority column plus the one-time requeue.
	if err := s.Migrate(Migrations[3:]...); err != nil {
		t.Fatalf("migrate v4: %v", err)
	}
	// Applying it again is a no-op (recorded in schema_migrations).
	if err := s.Migrate(Migrations[3:]...); err != nil {
		t.Fatalf("re-migrate v4: %v", err)
	}
	q, err := New(s)
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	for _, tc := range []struct {
		id, wantState, wantStage, wantErr string
		wantPriority, wantAttempts        int
	}{
		{"folder", StateQueued, "", "", 0, 0},
		{"nas", StateQueued, "", "", 0, 0},
		{"youtube", StateFailed, "convert", "something broke", 0, 0},
	} {
		j, err := q.Get(tc.id)
		if err != nil {
			t.Fatalf("Get %s: %v", tc.id, err)
		}
		if j.State != tc.wantState || j.Stage != tc.wantStage || j.Error != tc.wantErr ||
			j.Priority != tc.wantPriority || j.Attempts != tc.wantAttempts {
			t.Errorf("%s: state=%s stage=%q error=%q priority=%d attempts=%d, want %s/%q/%q/%d/%d",
				tc.id, j.State, j.Stage, j.Error, j.Priority, j.Attempts,
				tc.wantState, tc.wantStage, tc.wantErr, tc.wantPriority, tc.wantAttempts)
		}
	}
	// The requeued jobs claim as fresh show work, ahead of anything else.
	job, err := q.Claim("ingest")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if job == nil || (job.ID != "folder" && job.ID != "nas") {
		t.Fatalf("first claim = %+v, want a requeued folder/NAS job", job)
	}
}
