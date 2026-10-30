package launch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/edementhon/kids-shelf/internal/update"
)

// Timing windows of the supervised launch. They are variables so tests can
// shorten them; the defaults are the 1a contract numbers.
var (
	// StartupTimeout is how long the launcher waits for a child's first
	// /healthz answer before reporting a visible startup failure.
	StartupTimeout = 60 * time.Second
	// ConfirmHealthyFor is how long a trial version's /healthz must keep
	// answering before the launcher confirms the update.
	ConfirmHealthyFor = 30 * time.Second
	// TrialHealthTimeout is how long a trial version has to become
	// healthy-confirmed before the launcher kills it and rolls back.
	TrialHealthTimeout = 60 * time.Second
	// HealthPollInterval is how often the launcher asks /healthz.
	HealthPollInterval = 200 * time.Millisecond
	// shutdownGrace is how long the launcher waits for a stopped child
	// before killing it.
	shutdownGrace = 10 * time.Second
	// ConfirmRetryBackoffStart is how long the launcher waits after a
	// failed confirmation journal write before retrying it; the wait
	// doubles on every failure up to ConfirmRetryBackoffMax. The trial
	// stays open while the journal still says trying.
	ConfirmRetryBackoffStart = 200 * time.Millisecond
	ConfirmRetryBackoffMax   = 5 * time.Second
)

// ExitRestart is the child exit code that means "restart me": the app uses
// it after an update is staged, so the launcher re-resolves the version and
// activates the staged build.
const ExitRestart = 3

// ExitRemove is the child exit code for "remove kids-shelf from this
// computer" (the parent page's Remove): the supervisor hands over to the
// remover and exits (PLAN 1a "Install, reinstall, remove").
const ExitRemove = 4

// ExitBindFailed is the child exit code for "my private loopback port was
// taken": the supervisor picks new private ports and reruns the same image,
// without re-activating it or rejecting a trial.
const ExitBindFailed = 5

// maxBindAttempts bounds how many times the supervisor reruns the same
// resolved image after the child reports ExitBindFailed, before the
// failures become a visible startup error.
const maxBindAttempts = 5

// ErrBindFailed is what ServeChild reports when the child cannot bind the
// private listeners it was given. cmd/kids-shelf maps it to ExitBindFailed
// so the supervisor reruns the same build on fresh ports.
var ErrBindFailed = errors.New("the app could not bind the ports it was given")

// TrialPolicy is the launcher's health watch for an activated-but-
// unconfirmed update. All callbacks run on the goroutine that supervises
// the child.
type TrialPolicy struct {
	// InTrial reports whether the child that just started is an
	// unconfirmed trial.
	InTrial func() bool
	// Healthy is called once the child's /healthz has answered for
	// ConfirmHealthyFor. It must persist the confirmation durably; an
	// error keeps the trial open and the call is retried with backoff.
	// The version is treated as confirmed only when Healthy returns nil.
	Healthy func() error
	// Failed is called when a trial child must be rolled back; the child
	// is already dead (killed and waited for, or it exited). It must
	// restore the previous state; the supervisor then re-resolves and
	// starts the next version.
	Failed func(reason error) error
	// Blocked, when set, is called instead of Failed when the candidate
	// could not even start: the operating system refused to run it, as
	// Windows Smart App Control does with a new unsigned file for up to an
	// hour (VM, 2026-10-29). The version was hash-checked when it was
	// staged, so the build is not at fault and must not be rejected: the
	// hook puts it back for a later start, and Resolve must keep running
	// the current version for a while. The supervisor then re-resolves.
	Blocked func(reason error) error
}

// supervise runs the app as a child of this process. It binds and keeps the
// public listeners - the app port and the libraries origin right after it -
// answers them through the proxies (the starting page while no child is
// up), private child ports chosen fresh per start, and forwards the child's
// /healthz answer into instance.json and the browser. Exit code 3 restarts
// the child (re-resolving which build to run), ExitBindFailed reruns the
// same image on fresh private ports, ExitRemove hands over to OnRemove, and
// any other exit ends the launcher. The trial policy watches an unconfirmed
// update: confirmed healthy for the settle window, or killed and rolled
// back on a non-zero exit or after the trial deadline without health.
// SIGINT/SIGTERM are forwarded to the child, which is then waited for.
func supervise(ctx context.Context, opts Options) error {
	id := opts.ID
	if id == "" {
		var err error
		id, err = NewID()
		if err != nil {
			return fmt.Errorf("generating an instance id: %w", err)
		}
	}

	// Bind and keep the public listeners: the supervisor is the only
	// program that listens on the LAN (and needs a firewall rule).
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

	// The name lasts the launcher's life, across child restarts.
	lanName, stopName := announceName(opts)
	defer stopName()

	// The public listeners answer through the proxies, which hold the
	// private child base URLs.
	token, err := proxyToken()
	if err != nil {
		return err
	}
	appProxy := newChildProxy(token, false)
	libsProxy := newChildProxy(token, true)
	appSrv := &http.Server{Handler: appProxy}
	libsSrv := &http.Server{Handler: libsProxy}
	serveCh := make(chan error, 2)
	go func() { serveCh <- appSrv.Serve(appL) }()
	go func() { serveCh <- libsSrv.Serve(libL) }()
	defer appSrv.Close()
	defer libsSrv.Close()

	// The browser opens once per launcher life: on the first publish (the
	// first child whose /healthz answers). Restarts after an update,
	// bind-failure reruns and trial rollbacks republish instance.json
	// while the same tab is already connected, so they must not open
	// another one.
	state := &superviseState{lanName: lanName}
	for {
		child, err := resolveChild(opts)
		if err != nil {
			return err
		}
		restart, err := superviseImage(ctx, opts, state, child, id, appPort, libPort, token, appProxy, libsProxy)
		if err != nil {
			return err
		}
		if !restart {
			return nil
		}
	}
}

// resolveChild picks the build to run: Options.Resolve decides (it may
// activate a staged update), falling back to Options.Child.
func resolveChild(opts Options) (string, error) {
	if opts.Resolve == nil {
		if opts.Child == "" {
			return "", errors.New("launch: supervised mode needs a child binary")
		}
		return opts.Child, nil
	}
	path, err := opts.Resolve()
	if err != nil {
		return "", err
	}
	if path == "" {
		path = opts.Child
	}
	if path == "" {
		return "", errors.New("launch: supervised mode needs a child binary")
	}
	return path, nil
}

// childOutcome tells the supervisor what one child run means for its loop.
type childOutcome int

// superviseState carries the launcher's per-life state across child runs,
// so a rerun of the child (an update restart, a bind retry, a trial
// rollback) can tell what the first run already did.
type superviseState struct {
	// browserOpened is set once the browser has been opened for this
	// launcher's life: on the first publish, and never again.
	browserOpened bool
	// lanName is the name tablets can use (envLANName), "" when the
	// launcher is not answering to one.
	lanName string
}

const (
	// outcomeEnd: the launcher ends (a clean child exit, an explicit quit,
	// or a remove that was handed over).
	outcomeEnd childOutcome = iota
	// outcomeRestart: resolve the next image and run it (exit code 3, or
	// a trial that was rolled back).
	outcomeRestart
	// outcomeRerunSame: the child could not bind its private ports; run
	// the same resolved image on fresh ports.
	outcomeRerunSame
)

// superviseImage runs one resolved image: fresh private loopback ports per
// start, rerunning the exact same build - never calling Resolve again and
// never rejecting a trial - when the child reports ExitBindFailed. It
// returns whether the launcher should resolve the next image (true) or end
// (false). After maxBindAttempts the bind failures are a visible failure;
// an unfinished trial is abandoned first (not rejected), so the next launch
// tries the candidate again.
func superviseImage(ctx context.Context, opts Options, state *superviseState, child, id string, appPort, libPort int, token string, appProxy, libsProxy *childProxy) (restart bool, err error) {
	for attempt := 0; ; attempt++ {
		appAddr, err := privatePort()
		if err != nil {
			return false, err
		}
		libsAddr, err := privatePort()
		if err != nil {
			return false, err
		}
		outcome, err := superviseOne(ctx, opts, state, child, id, appPort, libPort, appAddr, libsAddr, token, appProxy, libsProxy)
		if err != nil {
			return false, err
		}
		switch outcome {
		case outcomeRestart:
			return true, nil
		case outcomeEnd:
			return false, nil
		case outcomeRerunSame:
			if attempt+1 >= maxBindAttempts {
				// The private ports were taken every time: a visible
				// startup failure. Collisions are environmental, not a
				// broken candidate, so an unconfirmed trial must not be
				// left in the journal - the next launch would read it as
				// a crashed trial and reject the candidate. Abandon the
				// trial instead (restore the snapshot, back to staged),
				// so the next launch tries the candidate again.
				if aerr := update.AbandonTrial(opts.DataDir); aerr != nil {
					return false, fmt.Errorf("the app could not bind the ports it was given %d times in a row, and the unfinished update trial could not be abandoned: %w", maxBindAttempts, aerr)
				}
				return false, fmt.Errorf("the app could not bind the ports it was given %d times in a row; close some programs and start kids-shelf again", maxBindAttempts)
			}
		}
	}
}

// privatePort picks a free loopback port without holding it: the child
// binds it a moment later, so the choice is only advisory. The child
// reports ExitBindFailed when the port was taken in between and the
// supervisor reruns on fresh ports.
func privatePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// superviseOne runs one child on the given private loopback ports and
// returns what the supervisor should do next. It waits for /healthz on the
// child's own private app port before pointing the public proxies at it,
// writing instance.json (with the public ports) and opening the browser on
// the first publish of the launcher's life only; only that direct health
// answer may publish or confirm anything.
func superviseOne(ctx context.Context, opts Options, state *superviseState, child, id string, appPort, libPort, appAddr, libsAddr int, token string, appProxy, libsProxy *childProxy) (outcome childOutcome, err error) {
	args := append([]string{
		"--listen", "127.0.0.1:" + strconv.Itoa(appAddr),
		"--libraries-listen", "127.0.0.1:" + strconv.Itoa(libsAddr),
	}, opts.Args...)
	cmd := exec.Command(child, args...)
	cmd.Env = append(os.Environ(),
		"KS_SUPERVISED=1",
		"KS_INSTANCE_ID="+id,
		"KS_PROXY_TOKEN="+token,
		"KS_PUBLIC_PORT="+strconv.Itoa(appPort),
		"KS_PUBLIC_LIBRARIES_PORT="+strconv.Itoa(libPort),
		envLANName+"="+state.lanName,
	)
	// The parent page refuses Quit and Remove while the journal says an
	// unconfirmed trial is in flight (its own durable trial state), so the
	// child needs no expiry env var: the supervisor watches the trial
	// health itself through TrialPolicy.
	trial := opts.Trials.InTrial != nil && opts.Trials.InTrial()
	cmd.Env = append(cmd.Env, opts.Env...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Lifeline: the child's stdin is the read end of a pipe whose write
	// end this supervisor holds. If the supervisor dies - for any reason,
	// including kill -9 - the OS closes the write end, the child sees EOF
	// and must shut down within LifelineGrace and exit. Without it, a dead
	// supervisor could leave its child serving unlocked while a new
	// supervisor restored the database under it.
	lifelineR, lifelineW, perr := os.Pipe()
	if perr != nil {
		return outcomeEnd, fmt.Errorf("creating the child lifeline: %w", perr)
	}
	defer lifelineR.Close()
	defer lifelineW.Close()
	cmd.Stdin = lifelineR
	prepareChild(cmd)

	if serr := cmd.Start(); serr != nil {
		if trial {
			// The candidate build cannot even start: run the previous
			// version now, putting the candidate back for later (Blocked)
			// or rolling it back for good (Failed).
			reason := fmt.Errorf("starting the new version: %w", serr)
			hook := opts.Trials.Failed
			if opts.Trials.Blocked != nil {
				hook = opts.Trials.Blocked
			}
			if ferr := hook(reason); ferr != nil {
				return outcomeEnd, ferr
			}
			return outcomeRestart, nil
		}
		return outcomeEnd, fmt.Errorf("starting the app: %w", serr)
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	cleanup, aerr := attachChildFunc(cmd)
	if aerr != nil {
		if errors.Is(aerr, errChildExited) {
			// The child exited before the OS lifeline could be attached
			// (os/exec cannot create the child suspended, so on Windows a
			// fast exit races the job-object attach): route through the
			// normal exit handling - trial rollback when in trial -
			// instead of surfacing the attach error.
			werr := <-waitCh
			return opts.childExited(werr, trial, !trial)
		}
		cmd.Process.Kill()
		<-waitCh
		return outcomeEnd, aerr
	}
	defer cleanup()

	confirmed := !trial
	deadline := time.Now().Add(StartupTimeout)
	startupWindow := StartupTimeout
	if trial {
		deadline = time.Now().Add(TrialHealthTimeout)
		startupWindow = TrialHealthTimeout
	}
	url := "http://127.0.0.1:" + strconv.Itoa(appAddr) + "/healthz"

	instanceWritten := false
	healthySince := time.Time{}
	confirmBackoff := ConfirmRetryBackoffStart
	var lastConfirmAttempt time.Time
	ticker := time.NewTicker(HealthPollInterval)
	defer ticker.Stop()
	// While this child is gone - exited, killed, or still booting - the
	// public proxies answer the starting page.
	defer func() {
		appProxy.setDown()
		libsProxy.setDown()
	}()
	for {
		select {
		case <-ctx.Done():
			// Forward the stop signal and wait for the child.
			cmd.Process.Signal(childStopSignal)
			select {
			case <-waitCh:
				return outcomeEnd, nil
			case <-time.After(shutdownGrace):
				cmd.Process.Kill()
				<-waitCh
				return outcomeEnd, nil
			}
		case werr := <-waitCh:
			return opts.childExited(werr, trial, confirmed)
		case <-ticker.C:
			ok := healthOK(url, id)
			if ok {
				if !instanceWritten {
					// The child's own /healthz answered on its private
					// port: only now do the public proxies forward, the
					// public ports get published, and the browser opens.
					if uerr := appProxy.setUp("http://127.0.0.1:" + strconv.Itoa(appAddr)); uerr != nil {
						cmd.Process.Kill()
						<-waitCh
						return outcomeEnd, uerr
					}
					if uerr := libsProxy.setUp("http://127.0.0.1:" + strconv.Itoa(libsAddr)); uerr != nil {
						cmd.Process.Kill()
						<-waitCh
						return outcomeEnd, uerr
					}
					record := instance{Port: appPort, LibrariesPort: libPort, ID: id, PID: cmd.Process.Pid, Started: time.Now().UTC().Format(time.RFC3339)}
					if werr := writeInstanceFile(opts.DataDir, record); werr != nil {
						// The health check worked; a publish failure still
						// ends the run visibly rather than silently.
						cmd.Process.Kill()
						<-waitCh
						return outcomeEnd, fmt.Errorf("writing %s: %w", instanceFileName, werr)
					}
					instanceWritten = true
					if !state.browserOpened {
						// The browser opens on the first publish of this
						// launcher's life only: the same tab stays on the
						// public ports, which the supervisor keeps across
						// child restarts. Reruns of the child (an update
						// restart, a bind retry, a trial rollback) must
						// not stack another tab.
						state.browserOpened = true
						if !opts.NoBrowser {
							if oerr := openURL("http://127.0.0.1:" + strconv.Itoa(appPort) + "/"); oerr != nil {
								fmt.Fprintf(os.Stderr, "kids-shelf: opening the browser: %v\n", oerr)
							}
						}
					}
				}
				if healthySince.IsZero() {
					healthySince = time.Now()
				}
				if trial && !confirmed && time.Since(healthySince) >= ConfirmHealthyFor {
					if opts.Trials.Healthy == nil {
						confirmed = true
					} else if time.Since(lastConfirmAttempt) >= confirmBackoff {
						lastConfirmAttempt = time.Now()
						if cerr := opts.Trials.Healthy(); cerr != nil {
							// The journal still says trying: the version is
							// not confirmed. Retry with backoff and keep
							// the trial open.
							if confirmBackoff < ConfirmRetryBackoffMax {
								confirmBackoff *= 2
							}
						} else {
							confirmed = true
						}
					}
				}
			} else {
				healthySince = time.Time{}
			}
			if !deadline.IsZero() && time.Now().After(deadline) {
				if trial && !confirmed {
					// The trial never became healthy-confirmed in time:
					// kill it, roll back, re-resolve.
					killAndWait(cmd, waitCh)
					reason := fmt.Errorf("the new version did not become healthy within %s", TrialHealthTimeout)
					if ferr := opts.Trials.Failed(reason); ferr != nil {
						return outcomeEnd, ferr
					}
					return outcomeRestart, nil
				}
				if !instanceWritten {
					// A non-trial child that never answered /healthz is a
					// visible startup failure, not a silent retry loop.
					killAndWait(cmd, waitCh)
					return outcomeEnd, fmt.Errorf("the app did not become healthy within %s", startupWindow)
				}
			}
		}
	}
}

// attachChildFunc is attachChild (the OS-specific lifeline attach), split
// out so tests can inject an attach that fails after the child exited.
var attachChildFunc = attachChild

// errChildExited reports that the child could not be attached to its OS
// lifeline because it had already exited: os/exec cannot create the child
// suspended, so on Windows a fast-exiting child races the job-object
// attach. The supervisor routes this through the normal exit handling
// (trial rollback when in trial) instead of returning it as a failure.
var errChildExited = errors.New("the child exited before the OS lifeline was attached")

// childExited dispatches one child exit. ExitRemove hands over to
// OnRemove - except during an unconfirmed trial, where PLAN 1a refuses the
// removal and the trial is abandoned without rejecting - ExitBindFailed
// reruns the same image, an unconfirmed trial that exits for any other
// reason rolls back through Trials.Failed and re-resolves, exit code 3
// restarts (re-resolving, which activates a staged update), a zero exit
// ends cleanly, and any other code is a visible failure.
func (opts Options) childExited(werr error, trial, confirmed bool) (outcome childOutcome, err error) {
	code := exitCode(werr)
	// A bind failure must rerun the same image without rejecting or
	// rolling back the trial. Remove is refused during an unconfirmed
	// trial (PLAN 1a: "During an unconfirmed trial (its first 30 s) Quit
	// and Remove are refused", so a parent cannot turn a healthy update
	// into a rejected one): the app enforces that on its side, and if the
	// child still exits with ExitRemove mid-trial - a stale request or an
	// older app build - the supervisor honors the refusal here instead of
	// handing over to the remover. The trial itself did not fail (the
	// candidate never got its chance), so it is abandoned without
	// rejecting: the snapshot is restored and the candidate goes back to
	// staged for the next launch.
	if code == ExitRemove {
		if trial && !confirmed {
			if aerr := update.AbandonTrial(opts.DataDir); aerr != nil {
				return outcomeEnd, fmt.Errorf("refusing the removal during the update trial: %w", aerr)
			}
			return outcomeEnd, nil
		}
		if opts.OnRemove == nil {
			return outcomeEnd, nil
		}
		if rerr := opts.OnRemove(); rerr != nil {
			return outcomeEnd, fmt.Errorf("removing kids-shelf: %w", rerr)
		}
		return outcomeEnd, nil
	}
	if code == ExitBindFailed {
		return outcomeRerunSame, nil
	}
	if trial && !confirmed {
		// A trial that exits before confirming failed, whatever its exit
		// code: roll back and re-resolve.
		reason := errors.New("the new version exited before confirming")
		if code != 0 {
			reason = fmt.Errorf("the new version exited with code %d", code)
		}
		if ferr := opts.Trials.Failed(reason); ferr != nil {
			return outcomeEnd, ferr
		}
		return outcomeRestart, nil
	}
	if code == ExitRestart {
		// The child staged an update and asked for a restart: re-resolve
		// (which activates the staged version).
		return outcomeRestart, nil
	}
	if code == 0 {
		return outcomeEnd, nil
	}
	return outcomeEnd, fmt.Errorf("the app exited with code %d", code)
}

// ServeChild is the app half of a supervised launch: it binds the two
// private loopback listeners it was given (the app before the libraries
// origin) and serves the app and libraries muxes until ctx is done. No
// lock, no instance.json, no browser; the supervisor owns those and
// health-checks this process. When either listener cannot be bound it
// returns ErrBindFailed so the child exits with ExitBindFailed and the
// supervisor reruns it on fresh ports.
func ServeChild(ctx context.Context, appAddr, librariesAddr string, appHandler, librariesHandler http.Handler) error {
	if appAddr == "" {
		return errors.New("launch: an app listen address is required to serve")
	}
	if appHandler == nil {
		return errors.New("launch: an app handler is required to serve")
	}
	appL, err := net.Listen("tcp", appAddr)
	if err != nil {
		return fmt.Errorf("%w: listening on %s: %v", ErrBindFailed, appAddr, err)
	}
	defer appL.Close()
	appSrv := &http.Server{Handler: appHandler}

	var libsL net.Listener
	var libsSrv *http.Server
	if librariesAddr != "" && librariesHandler != nil {
		libsL, err = net.Listen("tcp", librariesAddr)
		if err != nil {
			return fmt.Errorf("%w: listening on %s: %v", ErrBindFailed, librariesAddr, err)
		}
		defer libsL.Close()
		libsSrv = &http.Server{Handler: librariesHandler}
	}

	serveCh := make(chan error, 2)
	go func() { serveCh <- appSrv.Serve(appL) }()
	if libsSrv != nil {
		go func() { serveCh <- libsSrv.Serve(libsL) }()
	}
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		appSrv.Shutdown(shutdownCtx)
		if libsSrv != nil {
			libsSrv.Shutdown(shutdownCtx)
		}
		return nil
	case err := <-serveCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// healthOK asks the child's /healthz and requires the recorded instance id.
func healthOK(url, wantID string) bool {
	return checkHealthOnce(url, wantID, healthzTimeout) == nil
}

// exitCode extracts the child exit code from the Wait error; a signal kill
// counts as a non-zero exit.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return 1
}

// killAndWait terminates a failed trial child and does not return before
// the child has provably exited: Kill first, then Wait, with no timeout
// that would let a rollback restore the database under a still-running
// child.
func killAndWait(cmd *exec.Cmd, waitCh <-chan error) {
	cmd.Process.Kill()
	<-waitCh
}

// LifelineGrace is how long a supervised child may take to shut down after
// its lifeline reaches EOF (its supervisor died): 5 seconds.
const LifelineGrace = 5 * time.Second

// LifelineEOF watches the supervisor's lifeline pipe (passed as the
// child's stdin) and returns a channel that is closed when the pipe hits
// EOF: the write end closed because the supervisor exited, for any reason,
// including kill -9. The child must shut down within LifelineGrace and
// exit after it fires. The supervisor never writes to the pipe.
func LifelineEOF(r io.Reader) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 1)
		for {
			if _, err := r.Read(buf); err != nil {
				close(done)
				return
			}
		}
	}()
	return done
}
