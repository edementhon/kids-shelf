// Package launch starts or joins the single kids-shelf server instance.
//
// One instance holds an exclusive OS lock on <data>/instance.lock; the
// kernel releases the lock when the owning process exits, for any reason,
// so a crash never leaves a stale lock. With an install layout the
// launcher additionally holds <install>/install.lock, always before the
// instance lock (PLAN 1a), so a joining launch can never start serving
// while an installer changes the installation. The holder records its
// public app port, public libraries port, instance id, pid and start time
// in <data>/instance.json. A second launch that fails to take the lock
// verifies the running instance's /healthz against instance.json and, when
// healthy, opens the browser instead of starting another server.
package launch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	lockFileName     = "instance.lock"
	instanceFileName = "instance.json"
	defaultPort      = 8070
	maxPortAttempts  = 10
	healthzTimeout   = 3 * time.Second
)

var (
	errLockBusy   = errors.New("the instance lock is held by another process")
	errNotStarted = errors.New("Kids Shelf seems to be starting or stuck")
	// joinTimeout bounds how long a second launch waits for the lock
	// holder to publish instance.json and pass /healthz before reporting
	// the app as starting or stuck. It is a variable so tests can shorten
	// it.
	joinTimeout = 10 * time.Second
)

// instance is what the running server records in instance.json. Port is
// the public app port (what joins, the browser and tablets use);
// LibrariesPort is the public libraries origin right after it.
type instance struct {
	Port          int    `json:"port"`
	LibrariesPort int    `json:"libraries_port"`
	ID            string `json:"id"`
	PID           int    `json:"pid"`
	Started       string `json:"started"`
}

// Options configures Run. DataDir is required; it holds instance.lock and
// instance.json. InstallDir, when set, is the install root whose
// install.lock Run takes before the instance lock and holds for the whole
// life of the launcher. Port is the first public port to try (0 means
// 8070); the next nine ports are tried in order when one is taken, and the
// public libraries listener takes the next free port after it. ID is the
// instance id reported by /healthz; when empty, a random one is generated.
// NoBrowser skips opening the browser.
//
// Run serves in-process when neither Child nor Resolve is set: the process
// listens on both public ports itself (no proxy, empty proxy token). With
// an install layout present it supervises the app instead: Child is the
// app build to run (the launcher build itself), Env and Args are added to
// the child's environment and command line, and Resolve is called with
// the locks held and no child running before every child start - it
// returns the binary to run and may activate a staged update (empty means
// Child). Trials carries the launcher's health watch; see TrialPolicy.
// OnRemove, when set, is called when a supervised child exits with
// ExitRemove (nil: that exit is a normal clean exit).
type Options struct {
	DataDir string
	Port    int
	ID      string
	Handler http.Handler
	// OpenHandler, when Handler is nil, builds the app and libraries
	// handlers in-process only after the locks are held (the app opens the
	// database there, so a joining launch never touches it); close runs on
	// shutdown.
	OpenHandler func(ctx context.Context) (h, libraries http.Handler, close func(), err error)
	NoBrowser   bool
	// Announce answers to the name kids-shelf.local on the home network
	// (internal/mdns) while a supervisor runs, and tells the app child
	// through KS_LAN_NAME. In-process runs (dev builds, tests) never
	// announce: they must not claim the name.
	Announce bool

	// InstallDir is the install root whose install.lock Run takes before
	// the instance lock and holds for the launcher's whole life (PLAN 1a);
	// the installer and remover hold the same lock while they change the
	// installation. Empty: no install lock (tests, in-process runs).
	InstallDir string

	Child   string
	Env     []string
	Args    []string
	Resolve func() (string, error)
	Trials  TrialPolicy
	// OnRemove is called when a supervised child exits with ExitRemove
	// (the parent page asked to remove kids-shelf); the launcher ends
	// after it. Nil: the exit is a normal clean exit.
	OnRemove func() error
}

// Run starts a new kids-shelf instance or joins the running one, both
// rooted at opts.DataDir.
//
// When this process acquires both locks, Run picks the public ports
// starting at opts.Port (the libraries origin right after the app port),
// serves, writes instance.json, opens the browser (unless opts.NoBrowser)
// and returns nil when ctx is done.
//
// When another process holds the lock, Run waits for the holder to publish
// instance.json and pass /healthz with the recorded id for up to
// joinTimeout, retrying the locks in case the holder just died. A healthy
// join opens the browser (unless opts.NoBrowser) and returns nil; only
// after the deadline does Run return an error explaining that the app
// seems to be starting or stuck, and what to do.
func Run(ctx context.Context, opts Options) error {
	if opts.DataDir == "" {
		return errors.New("launch: Options.DataDir is required")
	}
	if err := os.MkdirAll(opts.DataDir, 0o755); err != nil {
		return fmt.Errorf("creating the data directory: %w", err)
	}
	locks, err := opts.acquireLocks()
	if err == nil {
		defer locks.Close()
		return serve(ctx, opts)
	}
	if !errors.Is(err, errLockBusy) {
		return fmt.Errorf("acquiring the instance lock: %w", err)
	}
	return joinRunning(ctx, opts)
}

// serve runs the instance this process locked: supervised when a child
// build is configured, in-process otherwise.
func serve(ctx context.Context, opts Options) error {
	if opts.Child != "" || opts.Resolve != nil {
		return supervise(ctx, opts)
	}
	return serveInProcess(ctx, opts)
}

// serveInProcess serves the app directly in this process on the public app
// listener and, when the app build provides one, the libraries mux on the
// public libraries listener right after it: no proxy and an empty proxy
// token, exactly the dev/tests shape of PLAN 1a.
func serveInProcess(ctx context.Context, opts Options) error {
	appL, appPort, err := listen(opts.Port)
	if err != nil {
		return err
	}
	defer appL.Close()
	libL, libPort, err := listen(appPort + 1)
	if err != nil {
		return err
	}
	defer libL.Close()
	// The in-process server is the public listener, so the app half learns
	// its own public ports through the same variables the supervisor sets
	// on a child: the /libraries API needs the libraries port, and the
	// kiwix-serve supervisor writes its "back to the app" home link with
	// the app port.
	os.Setenv("KS_PUBLIC_PORT", strconv.Itoa(appPort))
	os.Setenv("KS_PUBLIC_LIBRARIES_PORT", strconv.Itoa(libPort))

	var handler, librariesHandler http.Handler
	if opts.Handler != nil {
		handler = opts.Handler
	} else if opts.OpenHandler != nil {
		h, lh, closeFn, err := opts.OpenHandler(ctx)
		if err != nil {
			return err
		}
		defer closeFn()
		handler, librariesHandler = h, lh
	}
	if handler == nil {
		return errors.New("launch: Options.Handler is required to start an instance")
	}
	id := opts.ID
	if id == "" {
		id, err = NewID()
		if err != nil {
			return fmt.Errorf("generating an instance id: %w", err)
		}
	}
	record := instance{
		Port:          appPort,
		LibrariesPort: libPort,
		ID:            id,
		PID:           os.Getpid(),
		Started:       time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeInstanceFile(opts.DataDir, record); err != nil {
		return fmt.Errorf("writing %s: %w", instanceFileName, err)
	}
	srv := &http.Server{Handler: handler}
	var libsSrv *http.Server
	if librariesHandler != nil {
		libsSrv = &http.Server{Handler: librariesHandler}
	}
	serveErr := make(chan error, 2)
	go func() { serveErr <- srv.Serve(appL) }()
	if libsSrv != nil {
		go func() { serveErr <- libsSrv.Serve(libL) }()
	}
	if !opts.NoBrowser {
		url := "http://127.0.0.1:" + strconv.Itoa(appPort) + "/"
		if err := OpenURL(url); err != nil {
			fmt.Fprintf(os.Stderr, "kids-shelf: opening the browser: %v\n", err)
		}
	}
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
		if libsSrv != nil {
			libsSrv.Shutdown(shutdownCtx)
		}
		return nil
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serving on port %d: %w", appPort, err)
	}
}

// listen binds the first free port starting at base, trying up to
// maxPortAttempts consecutive ports. The listener is bound to all
// interfaces so tablets and other devices on the LAN can reach the server.
func listen(base int) (net.Listener, int, error) {
	if base == 0 {
		base = defaultPort
	}
	for i := range maxPortAttempts {
		port := base + i
		if port < 1 || port > 65535 {
			break
		}
		addr := ":" + strconv.Itoa(port)
		l, err := net.Listen("tcp", addr)
		if err == nil {
			return l, port, nil
		}
		if !addrInUse(err) {
			return nil, 0, fmt.Errorf("listening on %s: %w", addr, err)
		}
	}
	return nil, 0, fmt.Errorf("ports %d to %d are all in use; close something on those ports or pass --port to pick another range", base, base+maxPortAttempts-1)
}

// joinRunning handles a launch that found the locks already held.
//
// The holder may still be starting: on a simultaneous first launch the
// winner has not published instance.json yet, and a crashed holder leaves
// a stale instance.json behind until the next holder overwrites it. Within
// a single joinTimeout deadline this launch therefore loops, re-trying
// both locks (acquiring them makes this process the server, which covers a
// holder that died just now) and otherwise re-reading instance.json and
// verifying the recorded /healthz id. Only after the deadline does it
// report the app as starting or stuck. The takeover reacquires the install
// lock first, so a joining launch never starts serving while an installer
// owns the installation.
func joinRunning(ctx context.Context, opts Options) error {
	path := filepath.Join(opts.DataDir, instanceFileName)
	deadline := time.Now().Add(joinTimeout)
	var lastErr error
	for {
		locks, err := opts.acquireLocks()
		if err == nil {
			// The holder died between attempts: this process is now the
			// instance and must serve.
			defer locks.Close()
			return serve(ctx, opts)
		}
		if !errors.Is(err, errLockBusy) {
			return fmt.Errorf("acquiring the instance lock: %w", err)
		}
		record, rerr := readInstanceFile(path)
		if rerr == nil {
			// Never spend more of the join budget on one health attempt
			// than the loop has left, or than a single healthzTimeout.
			budget := time.Until(deadline)
			if budget > healthzTimeout {
				budget = healthzTimeout
			}
			if herr := checkHealth(record, budget); herr == nil {
				// The running instance answered /healthz with its recorded
				// id: join it.
				if !opts.NoBrowser {
					url := "http://127.0.0.1:" + strconv.Itoa(record.Port) + "/"
					if err := OpenURL(url); err != nil {
						return fmt.Errorf("opening the browser: %w", err)
					}
				}
				return nil
			} else {
				lastErr = herr
			}
		} else {
			lastErr = rerr
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: the instance lock is held but the running instance did not confirm it is healthy within %s (last problem: %v). Wait a few seconds and start Kids Shelf again; if it stays like this, close the running Kids Shelf process and start it again", errNotStarted, joinTimeout, lastErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// OpenRunning opens the browser on the running instance's parent page
// (/admin), recorded in dataDir's instance.json, once its /healthz answers
// with the recorded id. Its callers (the installer's busy answer and an
// uninstall from Settings while kids-shelf runs, PLAN 1a) tell the parent to
// quit it from the parent page, so that page is what opens; the kid home
// opened in the Windows VM and left the parent to find the way.
//
// The instance may still be starting: it holds the lock before it publishes
// instance.json, and the child took over 20 s to answer in the Windows VM.
// So the record is re-read and re-checked until openRunningWait runs out.
func OpenRunning(dataDir string) error {
	path := filepath.Join(dataDir, instanceFileName)
	deadline := time.Now().Add(openRunningWait)
	for {
		record, err := readInstanceFile(path)
		if err == nil {
			budget := time.Until(deadline)
			if budget > healthzTimeout {
				budget = healthzTimeout
			}
			if err = checkHealth(record, budget); err == nil {
				return openRunningURL("http://127.0.0.1:" + strconv.Itoa(record.Port) + "/admin")
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Kids Shelf did not finish starting within %s: %w", openRunningWait, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// openRunningWait bounds OpenRunning's wait for a starting instance, and
// openRunningURL opens the page; both are variables so tests can shorten the
// wait and observe the URL without a browser.
var (
	openRunningWait = time.Minute
	openRunningURL  = OpenURL
)

// checkHealth asks the recorded instance's /healthz for its id, retrying
// transient failures until the budget deadline or the first definite
// answer. The caller bounds the budget so the join deadline is respected.
func checkHealth(record instance, budget time.Duration) error {
	url := "http://127.0.0.1:" + strconv.Itoa(record.Port) + "/healthz"
	if budget <= 0 {
		return fmt.Errorf("no time left to check /healthz")
	}
	deadline := time.Now().Add(budget)
	var lastErr error
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		lastErr = checkHealthOnce(url, record.ID, remaining)
		if lastErr == nil {
			return nil
		}
		var netErr net.Error
		if !errors.As(lastErr, &netErr) {
			// A definite answer (wrong id, foreign response): do not retry.
			return lastErr
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("no healthy answer within %s; last error: %v", budget, lastErr)
}

func checkHealthOnce(url, wantID string, timeout time.Duration) error {
	reqCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var answer struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		return fmt.Errorf("reading the health answer: %w", err)
	}
	if answer.ID != wantID {
		return fmt.Errorf("the health answer carried id %q instead of %q", answer.ID, wantID)
	}
	return nil
}

func readInstanceFile(path string) (instance, error) {
	var record instance
	b, err := os.ReadFile(path)
	if err != nil {
		return record, err
	}
	if err := json.Unmarshal(b, &record); err != nil {
		return record, err
	}
	return record, nil
}

// writeInstanceFile atomically (temp file + rename) records the running
// instance, so a reader never sees a torn file.
func writeInstanceFile(dir string, record instance) error {
	b, err := json.Marshal(record)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, instanceFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// acquireLocks takes the install lock (when an install layout is
// configured) and then the instance lock, in the PLAN 1a order - always
// install first, so a launcher can never serve under an installer that
// holds the installation. It makes one attempt at each; a busy lock
// reports errLockBusy and nothing is held.
func (opts Options) acquireLocks() (io.Closer, error) {
	var installLock io.Closer
	if opts.InstallDir != "" {
		if err := os.MkdirAll(opts.InstallDir, 0o755); err != nil {
			return nil, fmt.Errorf("creating the install directory: %w", err)
		}
		l, err := LockInstall(opts.InstallDir, 0)
		if err != nil {
			return nil, err
		}
		installLock = l
	}
	instanceLock, err := acquireLock(opts.DataDir)
	if err != nil {
		if installLock != nil {
			installLock.Close()
		}
		return nil, err
	}
	return &lockPair{install: installLock, instance: instanceLock}, nil
}

// lockPair releases the two launcher locks, instance first then install -
// the reverse of acquisition, so the install lock is never held (even for a
// moment) without the instance lock.
type lockPair struct {
	install  io.Closer
	instance io.Closer
}

func (p *lockPair) Close() error {
	var firstErr error
	if p.instance != nil {
		firstErr = p.instance.Close()
	}
	if p.install != nil {
		if err := p.install.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// NewID returns a random 128-bit instance id, hex-encoded. The id ties a
// running instance's /healthz answer to its instance.json record so a
// second launch can tell the real server from something else holding the
// port.
func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
