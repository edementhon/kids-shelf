package disk

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/edementhon/kids-shelf/internal/jobs"
	"github.com/edementhon/kids-shelf/internal/store"
)

// keyA is the filesystem key the injected hook hands out, so the tests
// exercise per-filesystem accounting without touching real mounts.
var keyA = func(string) (string, error) { return "keyA", nil }

// fakeStore is a goroutine-safe in-memory Store for reservation tests.
// Reservations carry a filesystem key, like jobs.Queue's, so per-filesystem
// sums count only same-key reservations; a reservation without a key counts
// against every filesystem, mirroring the queue's sumReservationsTx.
type fakeStore struct {
	mu  sync.Mutex
	res map[string]uint64
	key map[string]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{res: map[string]uint64{}, key: map[string]string{}}
}

func (f *fakeStore) sumOthers(id, fsKey string) uint64 {
	var other uint64
	for otherID, b := range f.res {
		if otherID == id {
			continue
		}
		if k := f.key[otherID]; k == "" || k == fsKey {
			other += b
		}
	}
	return other
}

func (f *fakeStore) trySet(id string, bytes, free, floor uint64, fsKey string) (other uint64, ok bool) {
	other = f.sumOthers(id, fsKey)
	// free - floor, guarded against underflow.
	var available uint64
	if free > floor {
		available = free - floor
	}
	if other > available || bytes > available-other {
		return other, false
	}
	f.res[id] = bytes
	f.key[id] = fsKey
	return other, true
}

func (f *fakeStore) TryReserve(id string, bytes, free, floor uint64, fsKey string) (otherReserved uint64, ok bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	other, ok := f.trySet(id, bytes, free, floor, fsKey)
	return other, ok, nil
}

func (f *fakeStore) TryHoldReservation(id string, bytes, free, floor uint64, fsKey string) (otherReserved uint64, ok bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	other, ok := f.trySet(id, bytes, free, floor, fsKey)
	return other, ok, nil
}

func (f *fakeStore) SetReservedBytes(id string, bytes uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if bytes == 0 {
		delete(f.res, id)
		delete(f.key, id)
		return nil
	}
	f.res[id] = bytes
	f.key[id] = ""
	return nil
}

func (f *fakeStore) HeldReservationIDs(prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for id := range f.res {
		if strings.HasPrefix(id, prefix) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (f *fakeStore) total() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var sum uint64
	for _, b := range f.res {
		sum += b
	}
	return sum
}

func TestFree(t *testing.T) {
	n, err := Free(t.TempDir())
	if err != nil {
		t.Fatalf("Free: %v", err)
	}
	if n == 0 {
		t.Error("Free returned 0 bytes on a real filesystem")
	}
}

func TestFSKey(t *testing.T) {
	dir := t.TempDir()
	k1, err := FSKey(dir)
	if err != nil {
		t.Fatalf("FSKey: %v", err)
	}
	if k1 == "" {
		t.Fatal("FSKey returned an empty key")
	}
	// The key is stable for the same filesystem and an error for a
	// nonexistent path, like Free's.
	k2, err := FSKey(dir)
	if err != nil {
		t.Fatalf("FSKey again: %v", err)
	}
	if k2 != k1 {
		t.Errorf("FSKey(%s) = %q and %q; keys must be stable", dir, k1, k2)
	}
	if _, err := FSKey(filepath.Join(dir, "does-not-exist")); err == nil {
		t.Error("FSKey on a nonexistent path: got nil, want an error")
	}
}

func TestReserveSumsConcurrentReservations(t *testing.T) {
	st := newFakeStore()
	r := NewReservations(st).WithFree(func(string) (uint64, error) { return 100, nil }).WithFloor(10).WithFSKey(keyA)
	// 100 free - 10 floor = 90 usable.
	if err := r.Reserve("a", 40, "/media"); err != nil {
		t.Fatalf("Reserve a: %v", err)
	}
	// Two concurrent 50-byte reservations: 90 - 40 = 50 fits exactly once.
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = r.Reserve(fmt.Sprintf("b%d", i), 50, "/media")
		}(i)
	}
	wg.Wait()
	succeeded := 0
	var noSpace *ErrNoSpace
	for _, err := range errs {
		if err == nil {
			succeeded++
			continue
		}
		var e *ErrNoSpace
		if !errors.As(err, &e) {
			t.Errorf("concurrent Reserve error = %v, want ErrNoSpace", err)
			continue
		}
		noSpace = e
	}
	if succeeded != 1 {
		t.Fatalf("%d of 2 concurrent reservations succeeded, want exactly 1", succeeded)
	}
	if sum := st.total(); sum != 90 {
		t.Errorf("total reserved = %d, want 90", sum)
	}
	if noSpace == nil {
		t.Fatal("no ErrNoSpace recorded, want one")
	}
	if noSpace.Need != 50 || noSpace.Free != 100 || noSpace.Floor != 10 {
		t.Errorf("ErrNoSpace = %+v, want {Need:50 Free:100 Floor:10}", noSpace)
	}
	// Release frees a's 40 bytes again; b's 50 stay reserved, so 90 - 50 = 40 fit.
	if err := r.Release("a"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := r.Reserve("c", 41, "/media"); err == nil {
		t.Fatal("Reserve 41 after Release succeeded, want ErrNoSpace (only 40 left)")
	}
	if err := r.Reserve("c", 40, "/media"); err != nil {
		t.Fatalf("Reserve 40 after Release: %v", err)
	}
}

func TestReserveSumsPerFilesystem(t *testing.T) {
	// Two different filesystem keys injected through the hook: a
	// reservation on one filesystem must not count against the other, and
	// one on the same key must.
	st := newFakeStore()
	keys := map[string]string{"/media": "keyA", "/data": "keyB"}
	r := NewReservations(st).
		WithFree(func(string) (uint64, error) { return 100, nil }).
		WithFloor(10).
		WithFSKey(func(path string) (string, error) { return keys[path], nil })
	if err := r.Reserve("a", 40, "/media"); err != nil {
		t.Fatalf("Reserve a on keyA: %v", err)
	}
	// 90 usable on keyB and nothing reserved there: 40 fit alongside a's.
	if err := r.Reserve("b", 40, "/data"); err != nil {
		t.Fatalf("Reserve b on keyB: %v", err)
	}
	if sum := st.total(); sum != 80 {
		t.Errorf("total reserved = %d, want 80 (40 on keyA + 40 on keyB)", sum)
	}
	// Back on keyA only 50 of the 90 are left (40 are a's).
	if err := r.Reserve("c", 51, "/media"); err == nil {
		t.Error("Reserve 51 on keyA succeeded, want ErrNoSpace (only 50 left)")
	}
	if err := r.Reserve("c", 50, "/media"); err != nil {
		t.Fatalf("Reserve 50 on keyA: %v", err)
	}
	// Holding standalone reservations follows the same per-filesystem
	if err := r.Hold("update:snapshot:v1", 51, "/data"); err == nil {
		t.Error("Hold 51 on keyB succeeded, want ErrNoSpace (only 50 left)")
	}
	if err := r.Hold("update:snapshot:v1", 50, "/data"); err != nil {
		t.Fatalf("Hold 50 on keyB: %v", err)
	}
	if err := r.Reserve("c", 50, "/media"); err != nil {
		t.Fatalf("Reserve after a keyB hold: %v", err)
	}
}

func TestReserveNoSpaceUnderFloor(t *testing.T) {
	st := newFakeStore()
	r := NewReservations(st).WithFree(func(string) (uint64, error) { return 30, nil }).WithFloor(20).WithFSKey(keyA)
	// 30 free - 20 floor = 10 usable: exactly 10 fits, one byte more does not.
	if err := r.Reserve("a", 10, "/media"); err != nil {
		t.Fatalf("Reserve 10: %v", err)
	}
	err := r.Reserve("b", 1, "/media")
	var e *ErrNoSpace
	if !errors.As(err, &e) {
		t.Fatalf("Reserve 1 over the floor: got %v, want ErrNoSpace", err)
	}
	if e.Need != 1 || e.Free != 30 || e.Floor != 20 {
		t.Errorf("ErrNoSpace = %+v, want {Need:1 Free:30 Floor:20}", e)
	}
	// Free below the floor: nothing can be reserved.
	st2 := newFakeStore()
	r2 := NewReservations(st2).WithFree(func(string) (uint64, error) { return 5, nil }).WithFloor(10).WithFSKey(keyA)
	if err := r2.Reserve("x", 1, "/media"); err == nil {
		t.Error("Reserve with free < floor: got nil, want ErrNoSpace")
	}
}

func TestReservePropagatesProbeError(t *testing.T) {
	st := newFakeStore()
	boom := errors.New("probe broke")
	r := NewReservations(st).WithFree(func(string) (uint64, error) { return 0, boom }).WithFSKey(keyA)
	if err := r.Reserve("a", 1, "/media"); !errors.Is(err, boom) {
		t.Errorf("Reserve with failing probe: got %v, want the probe error", err)
	}
}

func TestReservePropagatesKeyError(t *testing.T) {
	st := newFakeStore()
	boom := errors.New("key probe broke")
	r := NewReservations(st).
		WithFree(func(string) (uint64, error) { return 100, nil }).
		WithFSKey(func(string) (string, error) { return "", boom })
	if err := r.Reserve("a", 1, "/media"); !errors.Is(err, boom) {
		t.Errorf("Reserve with failing fs-key probe: got %v, want the probe error", err)
	}
}

func TestReservationsWithQueueStore(t *testing.T) {
	// jobs.Queue implements disk.Store: the reservation lands in the jobs
	// table and is visible through the queue.
	s, err := store.Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	q, err := jobs.New(s)
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	id, err := q.Enqueue("ingest", nil)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := q.Claim("ingest"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	r := NewReservations(q).WithFree(func(string) (uint64, error) { return 1 << 40, nil }).WithFSKey(keyA)
	if err := r.Reserve(id, 5<<30, "/media"); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	job, err := q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.ReservedBytes != 5<<30 {
		t.Errorf("reserved_bytes = %d, want %d", job.ReservedBytes, 5<<30)
	}
	if err := r.Release(id); err != nil {
		t.Fatalf("Release: %v", err)
	}
	job, _ = q.Get(id)
	if job.ReservedBytes != 0 {
		t.Errorf("reserved_bytes after Release = %d, want 0", job.ReservedBytes)
	}
}

func TestReserveIsAtomicAcrossManagers(t *testing.T) {
	// Two Reservations over ONE queue: the check-and-set lives in the jobs
	// table's transaction, not behind a per-manager mutex, so exactly one of
	// two concurrent 60-byte requests wins against 100 free / 10 floor.
	s, err := store.Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	q, err := jobs.New(s)
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	idA, err := q.Enqueue("ingest", nil)
	if err != nil {
		t.Fatalf("Enqueue a: %v", err)
	}
	idB, err := q.Enqueue("ingest", nil)
	if err != nil {
		t.Fatalf("Enqueue b: %v", err)
	}
	newManager := func(st *jobs.Queue) *Reservations {
		return NewReservations(st).WithFree(func(string) (uint64, error) { return 100, nil }).WithFloor(10).WithFSKey(keyA)
	}
	r1, r2 := newManager(q), newManager(q)
	managers := []struct {
		r  *Reservations
		id string
	}{
		{r1, idA},
		{r2, idB},
	}
	errs := make([]error, len(managers))
	var wg sync.WaitGroup
	for i, m := range managers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = m.r.Reserve(m.id, 60, "/media")
		}()
	}
	wg.Wait()
	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
			continue
		}
		var e *ErrNoSpace
		if !errors.As(err, &e) {
			t.Fatalf("Reserve error = %v, want ErrNoSpace", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d of 2 concurrent reservations succeeded, want exactly 1", succeeded)
	}
	sum, err := q.SumActiveReservedBytes("")
	if err != nil {
		t.Fatalf("SumActiveReservedBytes: %v", err)
	}
	if sum != 60 {
		t.Errorf("sum of reservations = %d, want 60", sum)
	}
}

func TestIsDiskFull(t *testing.T) {
	if !IsDiskFull(fmt.Errorf("write /media/video.mp4: %w", syscall.ENOSPC)) {
		t.Error("wrapped ENOSPC not recognized")
	}
	if !IsDiskFull(syscall.ENOSPC) {
		t.Error("bare ENOSPC not recognized")
	}
	if IsDiskFull(nil) {
		t.Error("nil is not a disk-full error")
	}
	if IsDiskFull(errors.New("permission denied")) {
		t.Error("unrelated error recognized as disk full")
	}
}
