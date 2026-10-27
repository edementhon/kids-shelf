// The kiwix-serve supervisor. It keeps one kiwix-serve process running on
// the loopback interface for the installed ZIMs, restarts it when the
// installed set changes (Sync) or the process crashes (with a backoff),
// and exposes the process's base URL through Backend while it is up and
// ready, so the app can proxy the /kiwix/ routes to it.
package kiwix

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/edementhon/kids-shelf/internal/proc"
)

// Pacing for the kiwix-serve supervisor: how long a launch may take to
// answer, how quickly readiness is polled, how far crashes back off and
// when the crash backoff resets.
const (
	readyTimeout       = 15 * time.Second
	readyPoll          = 100 * time.Millisecond
	crashBackoffMin    = time.Second
	crashBackoffMax    = time.Minute
	healthyReset       = 5 * time.Minute // up this long without a crash: reset the backoff
	portAttempts       = 3               // fresh ports tried per launch before giving up
	lineLogBufferCap   = 64 << 10        // a line longer than this is flushed as-is
	readyClientTimeout = 2 * time.Second
)

// Server supervises one kiwix-serve process for the installed ZIMs. bin is
// the path to the kiwix-serve binary; books returns the installed set, and
// the File field of each book must be an absolute path to the ZIM on disk
// (the app's wiring resolves the relative stored names against the library
// directory).
type Server struct {
	bin   string
	books func() ([]Installed, error)

	mu      sync.Mutex
	started bool
	syncCh  chan struct{} // one pending "installed set changed" request (coalesced)
	stateCh chan struct{} // one pending pause/resume state-change ping (coalesced)
	backend string        // "http://127.0.0.1:<port>" while the process is up and ready, else ""

	// Pause bookkeeping: paused counts outstanding Pause calls; while it is
	// nonzero Run keeps the process stopped and ignores Sync. quiesced says
	// Run has stopped the process (or had nothing running) and is holding;
	// quiescedCh closes at that moment - which is what Pause waiters block
	// on - and is recreated when the last resume lifts the pause.
	paused     int
	quiesced   bool
	quiescedCh chan struct{}
}

// NewServer returns a Server that runs bin for the ZIMs books returns. The
// supervisor does not start until Run is called.
func NewServer(bin string, books func() ([]Installed, error)) *Server {
	return &Server{
		bin: bin, books: books,
		syncCh: make(chan struct{}, 1), stateCh: make(chan struct{}, 1),
		quiescedCh: make(chan struct{}),
	}
}

// Backend returns "http://127.0.0.1:<port>" (no trailing slash) while the
// kiwix-serve process is up and ready, "" otherwise: no ZIMs installed, no
// binary, a restart in progress, or the process has just exited. It is
// safe for concurrent use.
func (s *Server) Backend() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.backend
}

func (s *Server) setBackend(base string) {
	s.mu.Lock()
	s.backend = base
	s.mu.Unlock()
}

// Sync tells the server the installed set changed; it restarts the process
// with the new ZIM list. Bursts of calls coalesce into one restart, which
// reads the set fresh, so a caller may call it once per change or more. It
// never blocks. Sync is safe before Run starts: the pending request is
// consumed (harmlessly, the set is read again) by the first loop pass.
func (s *Server) Sync() {
	select {
	case s.syncCh <- struct{}{}:
	default:
	}
}

// Pause stops kiwix-serve and waits until the process has exited, so the
// caller can safely delete a ZIM the process has open - libkiwix caches
// opened archives, and on Windows the open file blocks deletion. While any
// Pause is outstanding Run keeps the process stopped and ignores Sync, so
// the set cannot be altered underneath the caller. It returns a resume func
// that lifts one pause level and restarts with the installed set current at
// that moment, through the same read-fresh path Sync takes. Pauses nest:
// each call needs its own resume, and the server stays down until the last
// one. Pause blocks until Run has quiesced; bound ctx to limit the wait. If
// ctx ends first, Pause drops its own pause level, returns an error and
// there is no resume to call. Safe for concurrent use.
func (s *Server) Pause(ctx context.Context) (resume func(), err error) {
	s.mu.Lock()
	s.paused++
	s.mu.Unlock()
	s.kick()
	for {
		s.mu.Lock()
		if s.quiesced {
			s.mu.Unlock()
			return s.resume, nil
		}
		ch := s.quiescedCh
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			// Give our pause level back so the server is not left down by a
			// waiter that will never resume it.
			s.resume()
			return nil, ctx.Err()
		case <-ch:
		}
	}
}

// resume lifts one Pause level; the last one lets Run restart. Idempotent
// at zero: an extra resume must not unpause a later Pause.
func (s *Server) resume() {
	s.mu.Lock()
	if s.paused > 0 {
		s.paused--
	}
	s.mu.Unlock()
	s.kick()
}

// kick pings Run so it re-evaluates the pause state. Coalesced: one pending
// ping is enough, the loop head re-reads the whole world after every wake.
func (s *Server) kick() {
	select {
	case s.stateCh <- struct{}{}:
	default:
	}
}

// isPaused reports whether any Pause is outstanding.
func (s *Server) isPaused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paused > 0
}

// confirmQuiesced tells Pause waiters that Run has quiesced: no process is
// running and none will launch while paused. Idempotent.
func (s *Server) confirmQuiesced() {
	s.mu.Lock()
	if !s.quiesced {
		s.quiesced = true
		close(s.quiescedCh)
	}
	s.mu.Unlock()
}

// waitResume holds the loop while at least one Pause is outstanding. Sync
// requests during the hold are left in the channel: they must not relaunch
// (the installed set is re-read after the last resume anyway). It reports
// false when ctx ends, so Run exits rather than outliving its context.
func (s *Server) waitResume(ctx context.Context) bool {
	for {
		s.mu.Lock()
		if s.paused == 0 {
			s.quiesced = false
			s.quiescedCh = make(chan struct{})
			s.mu.Unlock()
			return true
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return false
		case <-s.stateCh:
		}
	}
}

// Run supervises the process until ctx ends: it starts kiwix-serve when
// there is something to serve, restarts it when Sync is called or the
// process crashes (with a backoff), holds it stopped while a Pause is
// outstanding, and on ctx end kills the process, waits for it to exit and
// returns. Call it once.
func (s *Server) Run(ctx context.Context) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()

	var backoff time.Duration
	var healthySince time.Time
	for {
		// A pause is outstanding: no process may run. The one that was
		// serving was stopped at the select below, so reaching here is the
		// "process gone" half of Pause's contract; confirm to the waiter
		// and hold - ignoring Sync, which must not relaunch during a pause
		// - until the last resume lifts the pause, then restart like a
		// Sync, with the installed set re-read at the loop head.
		if s.isPaused() {
			s.setBackend("")
			s.confirmQuiesced()
			if !s.waitResume(ctx) {
				return
			}
			backoff = 0
			healthySince = time.Time{}
			continue
		}
		// Every pass re-reads the world: a Sync, a pause, a crash or a
		// startup all land here, so changes that raced with an in-flight
		// restart are picked up with the current list.
		books, ok := s.list()
		if !ok || len(books) == 0 {
			// Nothing to serve: no books, the lookup failed, or the binary
			// is gone. Stay down until the set changes or ctx ends.
			s.setBackend("")
			backoff = 0
			healthySince = time.Time{}
			if !waitSignal(ctx, s.syncCh, s.stateCh) {
				return
			}
			continue
		}

		kp, err := s.launch(ctx, books)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("kiwix-serve: %v", err)
			backoff = nextBackoff(backoff, healthySince)
			healthySince = time.Time{}
			if !waitSignalOrSleep(ctx, s.syncCh, s.stateCh, backoff) {
				return
			}
			continue
		}

		s.setBackend(kp.base)
		healthySince = time.Now()
		select {
		case <-ctx.Done():
			// exec.CommandContext kills the process with ctx; wait for the
			// exit so Run only returns once the process is gone.
			s.setBackend("")
			<-kp.exited
			return
		case <-s.syncCh:
			// The installed set changed: restart with the new list. The
			// moment the restart begins, the backend goes away.
			s.setBackend("")
			stopProc(kp)
			backoff = 0 // a deliberate restart is not a crash
			healthySince = time.Time{}
			continue
		case <-s.stateCh:
			// A pause request landed mid-serve: stop the process; the loop
			// head decides whether to hold (paused, possibly resumed in the
			// meantime) or restart with the current list.
			s.setBackend("")
			stopProc(kp)
			backoff = 0 // a deliberate stop is not a crash
			healthySince = time.Time{}
			continue
		case <-kp.exited:
			// The process died on its own: clear the backend and restart
			// with a backoff, doubling until the process survives long
			// enough to prove it is healthy again.
			s.setBackend("")
			log.Printf("kiwix-serve: process exited unexpectedly; restarting in %v", nextBackoff(backoff, healthySince))
			backoff = nextBackoff(backoff, healthySince)
			healthySince = time.Time{}
			if !waitSignalOrSleep(ctx, s.syncCh, s.stateCh, backoff) {
				return
			}
			continue
		}
	}
}

// list returns the installed ZIMs whose files exist on disk, or ok=false
// when nothing can run: no kiwix-serve binary, the installed-set lookup
// failed, or no installed ZIM file exists. Every pass re-checks all of it,
// so a Sync after the binary or the books appear brings the server up.
func (s *Server) list() (books []Installed, ok bool) {
	if st, err := os.Stat(s.bin); err != nil || st.IsDir() {
		return nil, false
	}
	set, err := s.books()
	if err != nil {
		log.Printf("kiwix-serve: installed books: %v", err)
		return nil, false
	}
	for _, b := range set {
		if b.File == "" {
			continue
		}
		if st, err := os.Stat(b.File); err == nil && !st.IsDir() {
			books = append(books, b)
		}
	}
	if len(books) == 0 {
		return nil, false
	}
	return books, true
}

// kiwixProc is one running kiwix-serve process and its exit signal.
type kiwixProc struct {
	cmd    *exec.Cmd
	base   string // "http://127.0.0.1:<port>"
	exited <-chan struct{}
}

// customIndex returns the path of the "back to the app" home page for
// kiwix-serve's --customIndex, or "" when the app's public port is unknown
// (a kiwix-serve run outside the launcher) or no library directory exists
// to write it into. The page is written next to the ZIMs, atomically, and
// sends the kid's browser back to /libraries on the app origin: the host is
// the one the browser is already using, and the app's public port comes
// from the launcher's KS_PUBLIC_PORT.
func (s *Server) customIndex(books []Installed) string {
	appPort := os.Getenv("KS_PUBLIC_PORT")
	if appPort == "" {
		return ""
	}
	for _, b := range books {
		if b.File == "" {
			continue
		}
		path := filepath.Join(filepath.Dir(b.File), "kiwix-index.html")
		if err := writeCustomIndex(path, appPort); err != nil {
			// The home link is a nicety: a page that cannot be written must
			// not stop kiwix-serve from serving.
			log.Printf("kiwix-serve: writing the libraries home page %s: %v", path, err)
			return ""
		}
		return path
	}
	return ""
}

// writeCustomIndex writes one kiwix-index.html atomically (temp file +
// rename): a tiny page whose script sends the browser to /libraries on the
// app origin (location.hostname is the host the kid is already on).
func writeCustomIndex(path, appPort string) error {
	page := []byte(fmt.Sprintf("<!doctype html>\n<html lang=\"en\">\n<head><meta charset=\"utf-8\"><title>Back to Kids Shelf</title></head>\n<body>\n<script>\n// kiwix-serve serves this page on the libraries origin; the kid's\n// libraries live at /libraries on the app origin: the same host the\n// browser is already on, on the app's public port %s.\nlocation.replace(location.protocol + '//' + location.hostname + ':' + %s + '/libraries');\n</script>\n</body>\n</html>\n", appPort, appPort))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, page, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// launch starts kiwix-serve for books on a free loopback port, polls until
// it answers /kiwix/ with 200, and returns the running process. A launch
// that fails (no free port, it never becomes ready, it dies immediately)
// is taken down and retried with a fresh port up to portAttempts times.
// The returned process's exited channel closes when the process exits.
func (s *Server) launch(ctx context.Context, books []Installed) (*kiwixProc, error) {
	var lastErr error
	customIndex := s.customIndex(books)
	for range portAttempts {
		port, err := freePort()
		if err != nil {
			lastErr = err
			continue
		}
		args := []string{
			"--address=127.0.0.1",
			fmt.Sprintf("--port=%d", port),
			"--urlRootLocation=/kiwix",
			"--blockexternal",
			"--skipInvalid",
			fmt.Sprintf("--attachToProcess=%d", os.Getpid()),
		}
		if customIndex != "" {
			args = append(args, "--customIndex="+customIndex)
		}
		for _, b := range books {
			args = append(args, b.File)
		}
		out := &lineLogger{prefix: "kiwix-serve: "}
		cmd := proc.CommandContext(ctx, s.bin, args...)
		cmd.Stdout = out
		cmd.Stderr = out
		if err := cmd.Start(); err != nil {
			lastErr = fmt.Errorf("start: %w", err)
			continue
		}
		exited := make(chan struct{})
		go func() {
			cmd.Wait()
			out.Close() // flush a trailing unterminated line, if any
			close(exited)
		}()
		base := fmt.Sprintf("http://127.0.0.1:%d", port)
		if s.waitReady(ctx, base, exited) {
			return &kiwixProc{cmd: cmd, base: base, exited: exited}, nil
		}
		stopProc(&kiwixProc{cmd: cmd, exited: exited})
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		lastErr = fmt.Errorf("port %d: never answered %s/kiwix/ with 200", port, base)
	}
	return nil, fmt.Errorf("start failed after %d ports: %w", portAttempts, lastErr)
}

// stopProc kills the process and waits for it to be gone.
func stopProc(p *kiwixProc) {
	if p.cmd.Process != nil {
		p.cmd.Process.Kill()
	}
	<-p.exited
}

// waitReady polls GET <base>/kiwix/ until it answers 200, the process
// exits, or ctx ends, up to readyTimeout.
func (s *Server) waitReady(ctx context.Context, base string, exited <-chan struct{}) bool {
	url := base + "/kiwix/"
	deadline := time.Now().Add(readyTimeout)
	client := &http.Client{Timeout: readyClientTimeout}
	for time.Now().Before(deadline) {
		if resp, err := client.Get(url); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-exited:
			return false
		case <-time.After(readyPoll):
		}
	}
	return false
}

// freePort picks a free TCP port on the loopback interface: bind, read the
// port, close. The child binds it a moment later, so the choice is
// advisory, which is why a failed launch retries with a fresh port.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// nextBackoff returns the crash backoff for the next restart: the previous
// one doubled (starting at crashBackoffMin), capped at crashBackoffMax,
// reset to the minimum when the last run survived healthyReset.
func nextBackoff(cur time.Duration, healthySince time.Time) time.Duration {
	if !healthySince.IsZero() && time.Since(healthySince) >= healthyReset {
		cur = 0
	}
	if cur == 0 {
		return crashBackoffMin
	}
	cur *= 2
	if cur > crashBackoffMax {
		return crashBackoffMax
	}
	return cur
}

// waitSignal waits for a Sync request, a pause/resume state change, or ctx.
// It reports false when ctx ended.
func waitSignal(ctx context.Context, syncCh, stateCh <-chan struct{}) bool {
	select {
	case <-ctx.Done():
		return false
	case <-syncCh:
		return true
	case <-stateCh:
		return true
	}
}

// waitSignalOrSleep waits for a Sync request, a pause/resume state change,
// the backoff timer, or ctx. It reports false when ctx ended. A Sync cuts
// the wait short; the caller re-reads the set, so the latest state still
// wins.
func waitSignalOrSleep(ctx context.Context, syncCh, stateCh <-chan struct{}, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-syncCh:
		return true
	case <-stateCh:
		return true
	case <-timer.C:
		return true
	}
}

// lineLogger forwards the child's stdout and stderr to the standard logger
// one line at a time, prefixed, like a service log. Any trailing bytes
// without a newline are flushed by Close.
type lineLogger struct {
	prefix string
	buf    []byte
}

// Write must report every byte as written: a short count makes the
// exec copy goroutine stop and close the pipe, and the child's next write
// then dies of SIGPIPE (kiwix-serve did, right after its first log line).
func (l *lineLogger) Write(p []byte) (int, error) {
	n := len(p)
	for {
		if i := bytes.IndexByte(p, '\n'); i >= 0 {
			l.buf = append(l.buf, p[:i]...)
			l.log()
			p = p[i+1:]
			continue
		}
		l.buf = append(l.buf, p...)
		if len(l.buf) >= lineLogBufferCap {
			l.log() // a pathological never-ending line must not grow forever
		}
		return n, nil
	}
}

func (l *lineLogger) log() {
	log.Printf("%s%s", l.prefix, l.buf)
	l.buf = l.buf[:0]
}

// Close flushes any line the child left unterminated.
func (l *lineLogger) Close() {
	if len(l.buf) > 0 {
		l.log()
	}
}
