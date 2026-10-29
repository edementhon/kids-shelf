package kiwix

// Tests for the kiwix-serve supervisor (internal/kiwix/serve.go). They run
// a fake kiwix-serve built from this test binary itself: each test sets
// KS_SERVE_TEST_FAKE=1 with t.Setenv, and the child the Server spawns -
// the same binary, no shell wrapper, so it works on Windows - turns into a
// minimal kiwix-serve lookalike that records its argv to a file, serves 200
// on /kiwix/ and exits on GET /__shutdown to simulate a crash.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edementhon/kids-shelf/internal/jobs"
)

// init doubles as the fake kiwix-serve the tests spawn: with
// KS_SERVE_TEST_FAKE set, the child test binary (spawned by fakeServe) runs
// runFakeKiwixServe instead of the tests. The hook runs before the testing
// framework parses flags, so the kiwix-serve-style flags the Server passes
// never confuse it.
func init() {
	if os.Getenv("KS_SERVE_TEST_FAKE") == "1" {
		runFakeKiwixServe()
	}
}

// runFakeKiwixServe is the fake kiwix-serve: it records its full argv to
// the file KS_SERVE_TEST_ARGV points at, then serves 200 on every path
// except /__shutdown, which exits the process (a simulated crash).
func runFakeKiwixServe() {
	if argvFile := os.Getenv("KS_SERVE_TEST_ARGV"); argvFile != "" {
		os.WriteFile(argvFile, []byte(strings.Join(os.Args[1:], "\n")), 0o644)
	}
	addr := "127.0.0.1:0"
	for _, a := range os.Args[1:] {
		if v, ok := strings.CutPrefix(a, "--address="); ok {
			addr = v
		}
		if v, ok := strings.CutPrefix(a, "--port="); ok {
			if port, err := strconv.Atoi(v); err == nil && port > 0 {
				addr = net.JoinHostPort(addr, strconv.Itoa(port))
			}
		}
	}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__shutdown" {
			os.Exit(0)
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "fake kiwix-serve\n")
	})
	if err := http.ListenAndServe(addr, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
}

// fakeServe returns the "kiwix-serve" binary the Server can spawn: this
// test binary itself, resolved to an absolute path. The child inherits
// KS_SERVE_TEST_FAKE=1 - every caller sets it with t.Setenv - and init()
// turns it into runFakeKiwixServe. No shell wrapper, so the supervisor
// tests run on Windows, where a POSIX script cannot be executed.
func fakeServe(t *testing.T) string {
	t.Helper()
	if os.Getenv("KS_SERVE_TEST_FAKE") != "1" {
		// A child without the flag would run the whole test suite, spawning
		// its own fakes forever: refuse to build a fake that is not armed.
		t.Fatal(`fakeServe requires t.Setenv("KS_SERVE_TEST_FAKE", "1") before use`)
	}
	self, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatalf("absolute test binary path: %v", err)
	}
	return self
}

func serveName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

// fakeArgvFile points the fake at a file it records its argv into, and
// returns its path. The Server-spawned fake inherits the variable through
// the test environment.
func fakeArgvFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "argv")
	t.Setenv("KS_SERVE_TEST_ARGV", p)
	return p
}

// bookList is a test-owned installed set. The Server reads it through the
// books closure, so writes (a Sync update) never race the supervisor's
// snapshot.
type bookList struct {
	mu   sync.Mutex
	zims []Installed
}

func (b *bookList) snapshot() func() ([]Installed, error) {
	return func() ([]Installed, error) {
		b.mu.Lock()
		defer b.mu.Unlock()
		return append([]Installed(nil), b.zims...), nil
	}
}

func (b *bookList) add(zim string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.zims = append(b.zims, Installed{File: zim})
}

// startServer runs s in a goroutine and guarantees it is cancelled and
// joined again when the test ends.
func startServer(t *testing.T, s *Server) (ctx context.Context, cancel context.CancelFunc, done <-chan struct{}) {
	t.Helper()
	ctx, cancel = context.WithCancel(context.Background())
	finished := make(chan struct{})
	done = finished
	go func() {
		defer close(finished)
		s.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("Server.Run did not return after cancel")
		}
	})
	return ctx, cancel, done
}

// await polls cond until it reports ok or the deadline passes. The detail
// string is included in the failure message.
func await(t *testing.T, what string, deadline time.Duration, cond func() (bool, string)) {
	t.Helper()
	dead := time.Now().Add(deadline)
	last := ""
	for time.Now().Before(dead) {
		if ok, detail := cond(); ok {
			return
		} else {
			last = detail
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s (last: %s)", what, last)
}

func makeZIM(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("not a real zim"), 0o644); err != nil {
		t.Fatalf("write zim %s: %v", p, err)
	}
	return p
}

// TestServeNoBooksStaysDown: with no installed ZIM the supervisor must not
// spawn anything and Backend must stay "".
func TestServeNoBooksStaysDown(t *testing.T) {
	t.Setenv("KS_SERVE_TEST_FAKE", "1")
	argvFile := fakeArgvFile(t)
	books := &bookList{}
	s := NewServer(fakeServe(t), books.snapshot())
	_, _, _ = startServer(t, s)

	await(t, "the no-books check to settle", time.Second, func() (bool, string) {
		return s.Backend() == "", s.Backend()
	})
	if _, err := os.Stat(argvFile); err == nil {
		t.Fatal("a kiwix-serve process ran despite no installed ZIMs")
	}
}

// TestServeOneBookReady: one ZIM on disk brings a process up with exactly
// the contracted argv; Backend answers and 200s on /kiwix/.
func TestServeOneBookReady(t *testing.T) {
	t.Setenv("KS_SERVE_TEST_FAKE", "1")
	zim := makeZIM(t, "one.zim")
	argvFile := fakeArgvFile(t)
	books := &bookList{}
	books.add(zim)
	s := NewServer(fakeServe(t), books.snapshot())
	_, _, _ = startServer(t, s)

	await(t, "backend up", 15*time.Second, func() (bool, string) { return s.Backend() != "", s.Backend() })

	resp, err := http.Get(s.Backend() + "/kiwix/")
	if err != nil {
		t.Fatalf("GET /kiwix/: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /kiwix/ = %d, want 200", resp.StatusCode)
	}

	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("the fake never recorded its argv: %v", err)
	}
	for _, want := range []string{
		"--address=127.0.0.1",
		"--urlRootLocation=/kiwix",
		"--blockexternal",
		"--skipInvalid",
		"--attachToProcess=" + strconv.Itoa(os.Getpid()),
		zim,
	} {
		if !strings.Contains(string(argv), want) {
			t.Errorf("argv missing %q:\n%s", want, argv)
		}
	}
}

// TestServeRecheckStartsWhenFileReturns: an installed book whose file is
// missing keeps the server down; once the file is back, the periodic
// recheck brings the server up on its own, with no Sync (nothing was
// installed or removed, a drive was just plugged in again).
func TestServeRecheckStartsWhenFileReturns(t *testing.T) {
	t.Setenv("KS_SERVE_TEST_FAKE", "1")
	zim := filepath.Join(t.TempDir(), "later.zim")
	_ = fakeArgvFile(t)
	books := &bookList{}
	books.add(zim)
	s := NewServer(fakeServe(t), books.snapshot())
	s.recheck = 100 * time.Millisecond
	_, _, _ = startServer(t, s)

	time.Sleep(300 * time.Millisecond)
	if b := s.Backend(); b != "" {
		t.Fatalf("Backend = %q with the only ZIM file missing, want \"\"", b)
	}
	if err := os.WriteFile(zim, []byte("not a real zim"), 0o644); err != nil {
		t.Fatalf("write zim: %v", err)
	}
	await(t, "backend up after the file came back", 15*time.Second, func() (bool, string) {
		return s.Backend() != "", s.Backend()
	})
}

// TestServeRecheckRestartsWhenFileReturns: the server runs with the one
// book whose file exists; when the other book's file appears, the recheck
// restarts it with both ZIMs, again with no Sync.
func TestServeRecheckRestartsWhenFileReturns(t *testing.T) {
	t.Setenv("KS_SERVE_TEST_FAKE", "1")
	zim1 := makeZIM(t, "one.zim")
	zim2 := filepath.Join(t.TempDir(), "two.zim")
	argvFile := fakeArgvFile(t)
	books := &bookList{}
	books.add(zim1)
	books.add(zim2)
	s := NewServer(fakeServe(t), books.snapshot())
	s.recheck = 100 * time.Millisecond
	_, _, _ = startServer(t, s)

	await(t, "backend up with the one file there", 15*time.Second, func() (bool, string) {
		return s.Backend() != "", s.Backend()
	})
	if argv, err := os.ReadFile(argvFile); err != nil || strings.Contains(string(argv), zim2) {
		t.Fatalf("first argv (err %v) must hold only the file on disk:\n%s", err, argv)
	}
	if err := os.WriteFile(zim2, []byte("not a real zim"), 0o644); err != nil {
		t.Fatalf("write zim: %v", err)
	}
	await(t, "argv with both ZIMs after the recheck", 15*time.Second, func() (bool, string) {
		argv, err := os.ReadFile(argvFile)
		if err != nil {
			return false, err.Error()
		}
		a := string(argv)
		return strings.Contains(a, zim1) && strings.Contains(a, zim2) && s.Backend() != "", a
	})
}

// TestManagerOnDisk: OnDisk follows the file in the library directory, and
// a stored name that escapes the directory is never reported as present.
func TestManagerOnDisk(t *testing.T) {
	m, q, lib := newTestManager(t)
	_, final := publishTestZIM(t, m, q, lib)
	if !m.OnDisk(Installed{File: "a.zim"}) {
		t.Fatal("OnDisk = false with the file in the library directory")
	}
	if err := os.Remove(final); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if m.OnDisk(Installed{File: "a.zim"}) {
		t.Fatal("OnDisk = true after the file was removed")
	}
	outside := filepath.Join(filepath.Dir(lib), "outside.zim")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if m.OnDisk(Installed{File: "../outside.zim"}) || m.OnDisk(Installed{}) {
		t.Fatal("OnDisk = true for a name outside the library directory or an empty name")
	}
}

// TestServeSyncRestartsWithNewList: adding a book and calling Sync restarts
// the process with both ZIMs on the command line.
func TestServeSyncRestartsWithNewList(t *testing.T) {
	t.Setenv("KS_SERVE_TEST_FAKE", "1")
	zim1 := makeZIM(t, "one.zim")
	zim2 := makeZIM(t, "two.zim")
	argvFile := fakeArgvFile(t)
	books := &bookList{}
	books.add(zim1)
	s := NewServer(fakeServe(t), books.snapshot())
	_, _, _ = startServer(t, s)

	await(t, "backend up", 15*time.Second, func() (bool, string) { return s.Backend() != "", s.Backend() })

	books.add(zim2)
	s.Sync()

	await(t, "argv with both ZIMs", 15*time.Second, func() (bool, string) {
		argv, err := os.ReadFile(argvFile)
		if err != nil {
			return false, err.Error()
		}
		a := string(argv)
		return strings.Contains(a, zim1) && strings.Contains(a, zim2), a
	})
}

// TestServeCrashRecovers: a process that dies on its own takes Backend to
// "" and comes back, with a fresh process, after the crash backoff.
func TestServeCrashRecovers(t *testing.T) {
	t.Setenv("KS_SERVE_TEST_FAKE", "1")
	zim := makeZIM(t, "one.zim")
	argvFile := fakeArgvFile(t)
	books := &bookList{}
	books.add(zim)
	s := NewServer(fakeServe(t), books.snapshot())
	_, _, _ = startServer(t, s)

	await(t, "backend up", 15*time.Second, func() (bool, string) { return s.Backend() != "", s.Backend() })

	// Tell the fake to die; the request itself never completes.
	_, _ = http.Get(s.Backend() + "/__shutdown")

	await(t, "backend down after the crash", 10*time.Second, func() (bool, string) {
		return s.Backend() == "", s.Backend()
	})
	await(t, "backend up again after the crash backoff", 20*time.Second, func() (bool, string) {
		return s.Backend() != "", s.Backend()
	})

	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("the recovered fake never recorded its argv: %v", err)
	}
	if !strings.Contains(string(argv), zim) {
		t.Errorf("recovered argv missing the ZIM path:\n%s", argv)
	}
}

// TestServeCancelKillsProcess: cancelling the context makes Run return and
// the process is gone - its port stops accepting connections.
func TestServeCancelKillsProcess(t *testing.T) {
	t.Setenv("KS_SERVE_TEST_FAKE", "1")
	zim := makeZIM(t, "one.zim")
	fakeArgvFile(t)
	books := &bookList{}
	books.add(zim)
	s := NewServer(fakeServe(t), books.snapshot())
	_, cancel, done := startServer(t, s)

	await(t, "backend up", 15*time.Second, func() (bool, string) { return s.Backend() != "", s.Backend() })
	port := strings.TrimPrefix(s.Backend(), "http://127.0.0.1:")

	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if b := s.Backend(); b != "" {
		t.Fatalf("Backend = %q after Run returned, want \"\"", b)
	}
	await(t, "the process's port to close", 5*time.Second, func() (bool, string) {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 200*time.Millisecond)
		if err != nil {
			return true, err.Error()
		}
		conn.Close()
		return false, "still accepting connections"
	})
}

// portClosed reports nil once nothing listens on loopback port any more, an
// error while it still accepts connections.
func portClosed(port string) error {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 200*time.Millisecond)
	if err != nil {
		return nil
	}
	conn.Close()
	return fmt.Errorf("port %s still accepts connections", port)
}

// TestServePauseStopsProcessAndHolds: Pause returns only once the fake
// process is gone (its port refuses connections), Backend reads "" while
// paused, a Sync during the pause does not relaunch, and the resume
// restarts with the installed set current at that moment.
func TestServePauseStopsProcessAndHolds(t *testing.T) {
	t.Setenv("KS_SERVE_TEST_FAKE", "1")
	zim1 := makeZIM(t, "one.zim")
	zim2 := makeZIM(t, "two.zim")
	argvFile := fakeArgvFile(t)
	books := &bookList{}
	books.add(zim1)
	s := NewServer(fakeServe(t), books.snapshot())
	_, _, _ = startServer(t, s)

	await(t, "backend up", 15*time.Second, func() (bool, string) { return s.Backend() != "", s.Backend() })
	port := strings.TrimPrefix(s.Backend(), "http://127.0.0.1:")

	resume, err := s.Pause(context.Background())
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	// Pause was synchronous: the process must be gone the moment it
	// returned, and the backend must read "".
	if err := portClosed(port); err != nil {
		t.Fatalf("Pause returned while the fake was still serving: %v", err)
	}
	if b := s.Backend(); b != "" {
		t.Fatalf("Backend = %q while paused, want \"\"", b)
	}

	// The installed set changes and Sync fires while paused: nothing may
	// relaunch until the resume.
	books.add(zim2)
	s.Sync()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if b := s.Backend(); b != "" {
			t.Fatalf("Sync relaunched the server during a pause (Backend %q)", b)
		}
		time.Sleep(100 * time.Millisecond)
	}

	resume()
	await(t, "backend up after the resume", 15*time.Second, func() (bool, string) { return s.Backend() != "", s.Backend() })
	// The resume restarted with the list current at that moment: both ZIMs.
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("the resumed fake never recorded its argv: %v", err)
	}
	for _, zim := range []string{zim1, zim2} {
		if !strings.Contains(string(argv), zim) {
			t.Errorf("resumed argv missing %q (the list current at resume):\n%s", zim, argv)
		}
	}
}

// TestServePauseNestsAndIsConcurrent: overlapping Pause calls count - the
// server stays down until every one of them is resumed, and a resume that
// arrives early must not relaunch it.
func TestServePauseNestsAndIsConcurrent(t *testing.T) {
	t.Setenv("KS_SERVE_TEST_FAKE", "1")
	zim := makeZIM(t, "one.zim")
	fakeArgvFile(t)
	books := &bookList{}
	books.add(zim)
	s := NewServer(fakeServe(t), books.snapshot())
	_, _, _ = startServer(t, s)

	await(t, "backend up", 15*time.Second, func() (bool, string) { return s.Backend() != "", s.Backend() })
	port := strings.TrimPrefix(s.Backend(), "http://127.0.0.1:")

	const n = 3
	type pauseResult struct {
		resume func()
		err    error
	}
	results := make([]pauseResult, n)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := s.Pause(context.Background())
			results[i] = pauseResult{r, err}
		}(i)
	}
	wg.Wait()
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("Pause %d: %v", i, r.err)
		}
		if r.resume == nil {
			t.Fatalf("Pause %d returned a nil resume", i)
		}
	}
	if err := portClosed(port); err != nil {
		t.Fatalf("a process is serving with %d pauses outstanding: %v", n, err)
	}
	if b := s.Backend(); b != "" {
		t.Fatalf("Backend = %q while paused, want \"\"", b)
	}

	// Two of the three resumes are not enough: the server stays down.
	for i := range n - 1 {
		results[i].resume()
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if b := s.Backend(); b != "" {
			t.Fatalf("an early resume relaunched the server (Backend %q)", b)
		}
		time.Sleep(100 * time.Millisecond)
	}

	results[n-1].resume()
	await(t, "backend up after the last resume", 15*time.Second, func() (bool, string) { return s.Backend() != "", s.Backend() })
}

// TestServeRealKiwixServe runs the real kiwix-serve binary against a real
// ZIM. It needs the built helper (KS_HELPERS_DIR) and a ZIM to serve
// (KS_TEST_ZIM) and skips otherwise.
func TestServeRealKiwixServe(t *testing.T) {
	helperDir := os.Getenv("KS_HELPERS_DIR")
	zim := os.Getenv("KS_TEST_ZIM")
	if helperDir == "" || zim == "" {
		t.Skip("KS_HELPERS_DIR or KS_TEST_ZIM unset (real kiwix-serve test)")
	}
	bin := filepath.Join(helperDir, serveName("kiwix-serve"))
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("%s not built: %v", bin, err)
	}
	if _, err := os.Stat(zim); err != nil {
		t.Fatalf("KS_TEST_ZIM %s does not exist", zim)
	}
	books := &bookList{}
	books.add(zim)
	s := NewServer(bin, books.snapshot())
	_, _, _ = startServer(t, s)

	await(t, "real kiwix-serve ready", 20*time.Second, func() (bool, string) {
		return s.Backend() != "", s.Backend()
	})
	book := strings.TrimSuffix(filepath.Base(zim), ".zim")
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(s.Backend() + "/kiwix/content/" + book + "/")
	if err != nil {
		t.Fatalf("GET /kiwix/content/%s/: %v", book, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /kiwix/content/%s/ = %d after redirects, want 200", book, resp.StatusCode)
	}
}

// TestManagerChangeHook: OnChange fires once when a ZIM is published and
// once when it is removed - and not again for a duplicate publish, which
// the kiwix-serve supervisor treats as "nothing changed". It drives
// publish, the production path, not the insert helper beneath it.
func TestManagerChangeHook(t *testing.T) {
	m, q, _ := newTestManager(t)
	calls := 0
	m.OnChange = func() { calls++ }

	e := testEntry("http://mirror.invalid/test.zim.meta4")
	id, err := m.Enqueue(e)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	job, err := q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	p := jobPayload{Entry: e, Meta4: &Meta4{Size: 1}}
	if err := m.publish(context.Background(), job, "", "", &p, "a.zim"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if calls != 1 {
		t.Fatalf("OnChange after publish = %d calls, want 1", calls)
	}

	// A second publish of the same entry inserts nothing (INSERT OR
	// IGNORE); the installed set is unchanged, so no second call.
	if err := m.publish(context.Background(), job, "", "", &p, "a.zim"); err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if calls != 1 {
		t.Fatalf("OnChange after duplicate publish = %d calls, want 1", calls)
	}

	if err := m.Remove(e.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if calls != 2 {
		t.Fatalf("OnChange after Remove = %d calls, want 2", calls)
	}
}

// TestManagerRemoveQuiesces: Remove calls Quiesce before the file is
// deleted - the callback can still stat it - and runs its resume once the
// removal is done, so the server restarts with the new library.
func TestManagerRemoveQuiesces(t *testing.T) {
	m, q, lib := newTestManager(t)
	e, final := publishTestZIM(t, m, q, lib)

	var order []string
	m.Quiesce = func(ctx context.Context) (func(), error) {
		order = append(order, "quiesce")
		if _, err := os.Stat(final); err != nil {
			t.Errorf("the ZIM was already deleted when Quiesce ran: %v", err)
		}
		return func() { order = append(order, "resume") }, nil
	}

	if err := m.Remove(e.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(order) != 2 || order[0] != "quiesce" || order[1] != "resume" {
		t.Fatalf("Quiesce/resume order = %v, want [quiesce resume]", order)
	}
	if _, err := os.Stat(final); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the ZIM file survived Remove: %v", err)
	}
}

// TestManagerRemoveQuiesceResumesOnFailure: when deleting the file fails,
// Remove still runs the Quiesce resume, so the server is not left stopped.
func TestManagerRemoveQuiesceResumesOnFailure(t *testing.T) {
	m, q, lib := newTestManager(t)
	e, final := publishTestZIM(t, m, q, lib)

	// Replace the ZIM with a non-empty directory, which os.Remove cannot
	// remove, so the deletion deterministically fails on every platform.
	if err := os.Remove(final); err != nil {
		t.Fatalf("replace the ZIM with a directory: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(final, "sub"), 0o755); err != nil {
		t.Fatalf("make the ZIM path undeletable: %v", err)
	}
	quiesced := false
	resumed := false
	m.Quiesce = func(ctx context.Context) (func(), error) {
		quiesced = true
		if _, err := os.Stat(final); err != nil {
			t.Errorf("the ZIM was already gone when Quiesce ran: %v", err)
		}
		return func() { resumed = true }, nil
	}

	if err := m.Remove(e.ID); err == nil {
		t.Fatal("Remove succeeded on a ZIM whose file cannot be deleted")
	}
	if !quiesced || !resumed {
		t.Fatalf("Quiesce ran = %v, resume ran = %v, want both", quiesced, resumed)
	}
	if ok, err := m.isInstalled(e.ID); err != nil {
		t.Fatalf("isInstalled: %v", err)
	} else if !ok {
		t.Fatal("Remove deleted the installed row despite failing to delete the file")
	}
}

// TestManagerRemoveQuiesceErrorAborts: a Quiesce failure fails Remove with
// the plain error and deletes nothing.
func TestManagerRemoveQuiesceErrorAborts(t *testing.T) {
	m, q, lib := newTestManager(t)
	e, final := publishTestZIM(t, m, q, lib)

	want := errors.New("quiesce failed")
	m.Quiesce = func(ctx context.Context) (func(), error) {
		return nil, want
	}

	if got := m.Remove(e.ID); got != want {
		t.Fatalf("Remove = %v, want the plain Quiesce error %v", got, want)
	}
	if _, err := os.Stat(final); err != nil {
		t.Fatalf("the ZIM was deleted despite the Quiesce failure: %v", err)
	}
	if ok, err := m.isInstalled(e.ID); err != nil {
		t.Fatalf("isInstalled: %v", err)
	} else if !ok {
		t.Fatal("Remove deleted the installed row despite the Quiesce failure")
	}
}

// publishTestZIM publishes a "a.zim" row through the production path, puts
// a real file behind it in the library directory, and returns the entry and
// the file's absolute path.
func publishTestZIM(t *testing.T, m *Manager, q *jobs.Queue, lib string) (Entry, string) {
	t.Helper()
	e := testEntry("http://mirror.invalid/test.zim.meta4")
	id, err := m.Enqueue(e)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	job, err := q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	p := jobPayload{Entry: e, Meta4: &Meta4{Size: 1}}
	if err := m.publish(context.Background(), job, "", "", &p, "a.zim"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	final := filepath.Join(lib, "a.zim")
	if err := os.WriteFile(final, []byte("a zim"), 0o644); err != nil {
		t.Fatalf("write %s: %v", final, err)
	}
	return e, final
}
