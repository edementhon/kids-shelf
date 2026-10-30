package launch

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edementhon/kids-shelf/internal/store"
	"github.com/edementhon/kids-shelf/internal/update"
)

// shortenWindows shrinks the supervision timing so the trial tests finish
// fast; the values are restored when the test ends.
func shortenWindows(t *testing.T) {
	t.Helper()
	oldStartup, oldConfirm, oldTrial, oldPoll := StartupTimeout, ConfirmHealthyFor, TrialHealthTimeout, HealthPollInterval
	StartupTimeout = 2 * time.Second
	ConfirmHealthyFor = 300 * time.Millisecond
	TrialHealthTimeout = 1 * time.Second
	HealthPollInterval = 20 * time.Millisecond
	t.Cleanup(func() {
		StartupTimeout, ConfirmHealthyFor, TrialHealthTimeout, HealthPollInterval = oldStartup, oldConfirm, oldTrial, oldPoll
	})
}

// fakeApp is the path of the test binary, which plays the fake app roles
// via apptest when exec'd with KS_TEST_ROLE=app.
func fakeApp(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// appEnv builds the environment additions that make the child play a fake
// app version.
func appEnv(extra ...string) []string {
	return append([]string{"KS_TEST_ROLE=app", "KS_TEST_MODE=healthy"}, extra...)
}

// startSupervisor runs Run in a goroutine and returns its completion
// channel plus a cancel func.
func startSupervisor(t *testing.T, dir string, opts Options) (done chan error, cancel context.CancelFunc) {
	t.Helper()
	opts.DataDir = dir
	opts.NoBrowser = true
	ctx, cancel := context.WithCancel(context.Background())
	done = make(chan error, 1)
	go func() { done <- Run(ctx, opts) }()
	t.Cleanup(cancel)
	return done, cancel
}

// exitFile is a reused marker that makes an "exit-once" child exit with the
// given code exactly once (it removes the file first), so the restarted
// child serves healthy.
func exitFile(t *testing.T, dir, code string) string {
	t.Helper()
	path := filepath.Join(dir, "exit-"+code)
	if err := os.WriteFile(path, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSuperviseHealthyChild: the launcher starts the app child, publishes
// instance.json only after /healthz answers, and stops the child when the
// launch ends.
func TestSuperviseHealthyChild(t *testing.T) {
	shortenWindows(t)
	dir := t.TempDir()
	done, cancel := startSupervisor(t, dir, Options{
		Child: fakeApp(t),
		Env:   appEnv(),
	})

	record := waitForInstance(t, dir, 10*time.Second)
	if record.PID == os.Getpid() {
		t.Fatalf("instance.json names this process; the child must serve")
	}
	checkHealthNow(t, record)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor did not stop after cancel")
	}
}

// TestSuperviseRestartsOnExitThree: a child that exits 3 is restarted and
// Resolve is asked again for the next build (where a staged update gets
// activated).
func TestSuperviseRestartsOnExitThree(t *testing.T) {
	shortenWindows(t)
	dir := t.TempDir()
	app := fakeApp(t)
	var resolves atomic.Int32
	done, cancel := startSupervisor(t, dir, Options{
		Child: app,
		Resolve: func() (string, error) {
			resolves.Add(1)
			return app, nil
		},
		Env: appEnv("KS_TEST_MODE=exit-once", "KS_TEST_EXIT_FILE="+exitFile(t, dir, "3"), "KS_TEST_EXIT_DONE="+filepath.Join(dir, "exit-done")),
	})

	// The first child exits 3 at once; the second is healthy and gets
	// instance.json published.
	record := waitForInstance(t, dir, 10*time.Second)
	if record.PID == os.Getpid() {
		t.Fatal("instance.json must name the child")
	}
	checkHealthNow(t, record)
	if n := resolves.Load(); n != 2 {
		t.Fatalf("Resolve called %d times, want 2 (restart after exit code 3)", n)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor did not stop after cancel")
	}
}

// TestSuperviseReportsNonZeroExit: a child that crashes outside a trial
// ends the launcher with a visible error.
func TestSuperviseReportsNonZeroExit(t *testing.T) {
	shortenWindows(t)
	dir := t.TempDir()
	done, _ := startSupervisor(t, dir, Options{
		Child: fakeApp(t),
		Env:   appEnv("KS_TEST_MODE=crash-at-start"),
	})
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "exit") {
			t.Fatalf("Run = %v, want an exit-code error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor did not end after the child crashed")
	}
}

// TestSuperviseReportsNeverHealthy: a non-trial child that never answers
// /healthz is a visible startup failure after StartupTimeout.
func TestSuperviseReportsNeverHealthy(t *testing.T) {
	shortenWindows(t)
	dir := t.TempDir()
	done, _ := startSupervisor(t, dir, Options{
		Child: fakeApp(t),
		Env:   appEnv("KS_TEST_MODE=hang-no-health"),
	})
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "did not become healthy") {
			t.Fatalf("Run = %v, want a never-healthy error", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the supervisor did not give up on the hung child")
	}
}

// TestSuperviseTrialConfirmed: a trial whose /healthz answers for the
// confirm window gets Healthy called and keeps running.
func TestSuperviseTrialConfirmed(t *testing.T) {
	shortenWindows(t)
	dir := t.TempDir()
	var confirmed atomic.Bool
	done, cancel := startSupervisor(t, dir, Options{
		Child: fakeApp(t),
		Env:   appEnv(),
		Trials: TrialPolicy{
			InTrial: func() bool { return true },
			Healthy: func() error { confirmed.Store(true); return nil },
		},
	})

	deadline := time.Now().Add(10 * time.Second)
	for !confirmed.Load() {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("the trial was never confirmed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor did not stop after cancel")
	}
}

// TestSuperviseTrialCrashRollsBack: a trial that exits non-zero calls
// Failed (the rollback hook) and then runs the next version.
func TestSuperviseTrialCrashRollsBack(t *testing.T) {
	shortenWindows(t)
	dir := t.TempDir()
	app := fakeApp(t)
	var resolves atomic.Int32
	var failedReason atomic.Value
	done, cancel := startSupervisor(t, dir, Options{
		Child: app,
		Resolve: func() (string, error) {
			resolves.Add(1)
			return app, nil
		},
		Env: appEnv("KS_TEST_MODE=exit-once", "KS_TEST_EXIT_FILE="+exitFile(t, dir, "1"), "KS_TEST_EXIT_DONE="+filepath.Join(dir, "exit-done"), "KS_TEST_EXIT_CODE=1"),
		Trials: TrialPolicy{
			InTrial: func() bool { return true },
			Failed: func(reason error) error {
				failedReason.Store(reason.Error())
				return nil
			},
		},
	})

	deadline := time.Now().Add(10 * time.Second)
	for failedReason.Load() == nil {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("the trial failure was never reported")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if reason := failedReason.Load().(string); !strings.Contains(reason, "exit") {
		t.Errorf("Failed reason = %q, want an exit mention", reason)
	}
	// The launcher re-resolves and runs the previous version.
	record := waitForInstance(t, dir, 10*time.Second)
	checkHealthNow(t, record)
	if n := resolves.Load(); n != 2 {
		t.Errorf("Resolve called %d times, want 2 (rollback then rerun)", n)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor did not stop after cancel")
	}
}

// TestSuperviseTrialHangTimesOut: a trial that never answers /healthz is
// killed at the trial deadline and Failed is called.
func TestSuperviseTrialHangTimesOut(t *testing.T) {
	shortenWindows(t)
	dir := t.TempDir()
	app := fakeApp(t)
	var failedReason atomic.Value
	done, cancel := startSupervisor(t, dir, Options{
		Child: app,
		Env:   appEnv("KS_TEST_MODE=hang-no-health"),
		Trials: TrialPolicy{
			InTrial: func() bool { return true },
			Failed: func(reason error) error {
				failedReason.Store(reason.Error())
				return nil
			},
		},
	})

	deadline := time.Now().Add(15 * time.Second)
	for failedReason.Load() == nil {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("the hung trial was never failed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if reason := failedReason.Load().(string); !strings.Contains(reason, "did not become healthy") {
		t.Errorf("Failed reason = %q, want a never-healthy mention", reason)
	}
	// The supervisor restarted the child (a hang again, its mode is
	// inherited); stopping the launch forwards the stop signal and the
	// hang child exits 0.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the supervisor did not stop after cancel")
	}
}

// TestSuperviseWriteInstanceFailure: a failed instance.json publish ends
// the launcher visibly instead of serving without a record.
func TestSuperviseWriteInstanceFailure(t *testing.T) {
	shortenWindows(t)
	dir := t.TempDir()
	// instance.json as a directory blocks the atomic rename.
	if err := os.MkdirAll(filepath.Join(dir, instanceFileName), 0o755); err != nil {
		t.Fatal(err)
	}
	done, _ := startSupervisor(t, dir, Options{
		Child: fakeApp(t),
		Env:   appEnv(),
	})
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "instance.json") {
			t.Fatalf("Run = %v, want an instance.json error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor did not end after the publish failure")
	}
}

// TestSuperviseTrialStartFailureRollsBack: a candidate build that cannot
// even start (during a trial) triggers the rollback path immediately -
// Failed is called and the supervisor re-resolves, which runs the previous
// version instead of ending with a startup error.
func TestSuperviseTrialStartFailureRollsBack(t *testing.T) {
	shortenWindows(t)
	dir := t.TempDir()
	app := fakeApp(t)
	var resolves atomic.Int32
	var failedReason atomic.Value
	done, cancel := startSupervisor(t, dir, Options{
		Child: app,
		Resolve: func() (string, error) {
			resolves.Add(1)
			if resolves.Load() == 1 {
				// A staged version whose binary cannot be started.
				return filepath.Join(dir, "missing-build"), nil
			}
			return app, nil
		},
		Env: appEnv(),
		Trials: TrialPolicy{
			InTrial: func() bool { return true },
			Failed: func(reason error) error {
				failedReason.Store(reason.Error())
				return nil
			},
		},
	})
	deadline := time.Now().Add(10 * time.Second)
	for failedReason.Load() == nil {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("the start failure was never rolled back")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if reason := failedReason.Load().(string); !strings.Contains(reason, "starting") {
		t.Errorf("Failed reason = %q, want a start failure", reason)
	}
	// The rollback re-resolves and the previous version serves.
	record := waitForInstance(t, dir, 10*time.Second)
	checkHealthNow(t, record)
	if n := resolves.Load(); n != 2 {
		t.Errorf("Resolve called %d times, want 2 (rollback then rerun)", n)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor did not stop after cancel")
	}
}

// TestSuperviseConfirmRetriesUntilJournalWriteSucceeds: a Healthy callback
// that fails (the journal write failed) keeps the trial open and is
// retried with backoff; the version is only treated as confirmed after a
// nil return.
func TestSuperviseConfirmRetriesUntilJournalWriteSucceeds(t *testing.T) {
	oldS, oldC, oldT, oldP := StartupTimeout, ConfirmHealthyFor, TrialHealthTimeout, HealthPollInterval
	oldB, oldM := ConfirmRetryBackoffStart, ConfirmRetryBackoffMax
	StartupTimeout = 2 * time.Second
	ConfirmHealthyFor = 150 * time.Millisecond
	TrialHealthTimeout = 8 * time.Second
	HealthPollInterval = 10 * time.Millisecond
	ConfirmRetryBackoffStart = 50 * time.Millisecond
	ConfirmRetryBackoffMax = 400 * time.Millisecond
	defer func() {
		StartupTimeout, ConfirmHealthyFor, TrialHealthTimeout, HealthPollInterval = oldS, oldC, oldT, oldP
		ConfirmRetryBackoffStart, ConfirmRetryBackoffMax = oldB, oldM
	}()

	dir := t.TempDir()
	var attempts atomic.Int32
	var confirmed atomic.Bool
	done, cancel := startSupervisor(t, dir, Options{
		Child: fakeApp(t),
		Env:   appEnv(),
		Trials: TrialPolicy{
			InTrial: func() bool { return true },
			Healthy: func() error {
				if attempts.Add(1) <= 2 {
					return errors.New("journal write failed")
				}
				confirmed.Store(true)
				return nil
			},
		},
	})
	deadline := time.Now().Add(10 * time.Second)
	for !confirmed.Load() {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("the trial was never confirmed; attempts = %d", attempts.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := attempts.Load(); n < 3 {
		t.Errorf("Healthy called %d times, want >= 3 (retry with backoff)", n)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor did not stop after cancel")
	}
}

// TestSuperviseChildExitsWhenLifelineCloses: the child's stdin is the
// lifeline pipe; closing the write end (the supervisor died) makes the
// fake app exit and record its departure within LifelineGrace.
func TestSuperviseChildExitsWhenLifelineCloses(t *testing.T) {
	dir := t.TempDir()
	left := filepath.Join(dir, "left")
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(fakeApp(t), "--port", strconv.Itoa(freePort(t)))
	cmd.Env = append(os.Environ(), "KS_TEST_ROLE=app", "KS_TEST_MODE=healthy", "KS_TEST_LEFT_FILE="+left)
	cmd.Stdin = pr
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pr.Close()
	pw.Close() // the supervisor "died"

	deadline := time.Now().Add(LifelineGrace + 4*time.Second)
	for {
		if _, err := os.Stat(left); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the child did not exit within %s of the lifeline closing", LifelineGrace)
		}
		time.Sleep(50 * time.Millisecond)
	}
	cmd.Wait()
}

// TestSuperviseAttachFailureAfterExitRollsBackTrial: Windows cannot create
// the child suspended, so a child that exits before the job-object attach
// races it; the attach reports errChildExited and the supervisor routes the
// exit through the normal trial handling (rollback) instead of surfacing
// the attach error.
func TestSuperviseAttachFailureAfterExitRollsBackTrial(t *testing.T) {
	shortenWindows(t)
	dir := t.TempDir()
	app := fakeApp(t)
	oldAttach := attachChildFunc
	attachGate := make(chan struct{})
	var attaches atomic.Int32
	attachChildFunc = func(cmd *exec.Cmd) (func(), error) {
		if attaches.Add(1) == 1 {
			// First child only: fail after it has exited, exactly like the
			// Windows attach sees an already-exited pid.
			<-attachGate
			return nil, errChildExited
		}
		return attachChild(cmd)
	}
	t.Cleanup(func() { attachChildFunc = oldAttach })

	// The exit file is present from the start, so the first child exits 3
	// at once; the marker tells the test when it has provably exited.
	exitFile := filepath.Join(dir, "exit-3")
	doneFile := filepath.Join(dir, "exit-done")
	if err := os.WriteFile(exitFile, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	var resolves atomic.Int32
	var failedReason atomic.Value
	done, cancel := startSupervisor(t, dir, Options{
		Child: app,
		Resolve: func() (string, error) {
			resolves.Add(1)
			return app, nil
		},
		Env: appEnv("KS_TEST_MODE=serve-exit-once",
			"KS_TEST_EXIT_FILE="+exitFile,
			"KS_TEST_EXIT_DONE="+doneFile,
			"KS_TEST_EXIT_CODE=3"),
		Trials: TrialPolicy{
			InTrial: func() bool { return true },
			Failed: func(reason error) error {
				failedReason.Store(reason.Error())
				return nil
			},
		},
	})

	// Wait until the first child has provably exited, then let the attach
	// fail.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(doneFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("the first child never exited")
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(attachGate)

	// The exit is routed through the normal trial handling: Failed gets the
	// child's exit code, never an attach error, and the next version
	// serves.
	deadline = time.Now().Add(10 * time.Second)
	for failedReason.Load() == nil {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("the trial failure was never reported")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if reason := failedReason.Load().(string); !strings.Contains(reason, "exited with code 3") {
		t.Errorf("Failed reason = %q, want the child's exit code 3 (not an attach error)", reason)
	}
	record := waitForInstance(t, dir, 10*time.Second)
	checkHealthNow(t, record)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor did not stop after cancel")
	}
}

// TestSuperviseAttachRealFailureEndsWithError: an attach failure that is
// not the exited-child race is still a real failure: the supervisor kills
// the child and ends with the attach error.
func TestSuperviseAttachRealFailureEndsWithError(t *testing.T) {
	shortenWindows(t)
	dir := t.TempDir()
	oldAttach := attachChildFunc
	attachChildFunc = func(*exec.Cmd) (func(), error) {
		return nil, errors.New("job object failed")
	}
	t.Cleanup(func() { attachChildFunc = oldAttach })

	done, _ := startSupervisor(t, dir, Options{
		Child: fakeApp(t),
		Env:   appEnv(),
	})
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "job object failed") {
			t.Fatalf("Run = %v, want the attach error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor did not end after the attach failure")
	}
}

// TestSuperviseRerunsSameImageOnBindFailure: a child that exits with
// ExitBindFailed (its private loopback port was taken) is rerun on fresh
// ports, the same resolved image is never re-resolved, and the rerun child
// publishes instance.json with the public ports behind the proxy.
func TestSuperviseRerunsSameImageOnBindFailure(t *testing.T) {
	shortenWindows(t)
	dir := t.TempDir()
	exit := exitFile(t, dir, "5")
	doneFile := filepath.Join(dir, "exit-5.done")
	var resolves atomic.Int32
	done, cancel := startSupervisor(t, dir, Options{
		Child: fakeApp(t),
		Resolve: func() (string, error) {
			resolves.Add(1)
			return "", nil // fall back to Options.Child
		},
		Env: appEnv("KS_TEST_MODE=exit-once", "KS_TEST_EXIT_CODE=5",
			"KS_TEST_EXIT_FILE="+exit, "KS_TEST_EXIT_DONE="+doneFile),
	})

	// The first child exits 5 at once; the rerun (same image, fresh ports)
	// serves healthy and publishes instance.json.
	record := waitForInstance(t, dir, 10*time.Second)
	if n := resolves.Load(); n != 1 {
		t.Errorf("Resolve called %d times; the bind-failure rerun must not re-resolve", n)
	}
	checkHealthNow(t, record)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor did not stop after cancel")
	}
}

// TestSuperviseOpensBrowserOnceAcrossRestart: the browser opens exactly
// once per launcher life, on the first published instance. A child that
// published instance.json, then exits 3 for a staged update, is restarted
// and publishes again: the restarted child must not get a second tab.
func TestSuperviseOpensBrowserOnceAcrossRestart(t *testing.T) {
	shortenWindows(t)
	dir := t.TempDir()
	exit := filepath.Join(dir, "exit-3")
	doneFile := filepath.Join(dir, "exit-3.done")
	var opens atomic.Int32
	origOpen := openURL
	openURL = func(url string) error { opens.Add(1); return nil }
	t.Cleanup(func() { openURL = origOpen })

	opts := Options{
		Child: fakeApp(t),
		Env: appEnv("KS_TEST_MODE=serve-exit-once",
			"KS_TEST_EXIT_FILE="+exit,
			"KS_TEST_EXIT_DONE="+doneFile,
			"KS_TEST_EXIT_CODE=3"),
	}
	// startSupervisor forces NoBrowser; start by hand so the browser path
	// stays live.
	opts.DataDir = dir
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, opts) }()
	t.Cleanup(cancel)

	// The first child publishes and gets the launcher's one browser open.
	first := waitForInstance(t, dir, 10*time.Second)
	checkHealthNow(t, first)
	deadline := time.Now().Add(5 * time.Second)
	for opens.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := opens.Load(); n != 1 {
		t.Fatalf("browser opened %d times on the first publish, want 1", n)
	}

	// Stage an update: the child exits 3, the supervisor re-resolves and
	// the restarted child publishes a new instance.
	if err := os.WriteFile(exit, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(10 * time.Second)
	var restarted instance
	for time.Now().Before(deadline) {
		if rec, err := readInstanceFile(filepath.Join(dir, instanceFileName)); err == nil && rec.PID != first.PID {
			restarted = rec
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if restarted.PID == 0 {
		t.Fatal("the restarted child never published instance.json")
	}
	checkHealthNow(t, restarted)
	// Give a buggy double-open a moment to happen before asserting.
	time.Sleep(200 * time.Millisecond)
	if n := opens.Load(); n != 1 {
		t.Fatalf("browser opened %d times across the restart, want exactly 1", n)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor did not stop after cancel")
	}
}

// TestSuperviseOpensBrowserOnceOnBindRerun: a bind-failure rerun (the
// child exits ExitBindFailed and the same image runs on fresh ports) must
// not open a second tab either once the rerun publishes.
func TestSuperviseOpensBrowserOnceOnBindRerun(t *testing.T) {
	shortenWindows(t)
	dir := t.TempDir()
	exit := filepath.Join(dir, "exit-5")
	doneFile := filepath.Join(dir, "exit-5.done")
	var opens atomic.Int32
	origOpen := openURL
	openURL = func(url string) error { opens.Add(1); return nil }
	t.Cleanup(func() { openURL = origOpen })

	opts := Options{
		Child: fakeApp(t),
		Env: appEnv("KS_TEST_MODE=serve-exit-once",
			"KS_TEST_EXIT_FILE="+exit,
			"KS_TEST_EXIT_DONE="+doneFile,
			"KS_TEST_EXIT_CODE=5"),
	}
	opts.DataDir = dir
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, opts) }()
	t.Cleanup(cancel)

	first := waitForInstance(t, dir, 10*time.Second)
	checkHealthNow(t, first)
	deadline := time.Now().Add(5 * time.Second)
	for opens.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := opens.Load(); n != 1 {
		t.Fatalf("browser opened %d times on the first publish, want 1", n)
	}

	// The child exits 5; the supervisor reruns the same image on fresh
	// ports and the rerun publishes a new instance.
	if err := os.WriteFile(exit, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(10 * time.Second)
	var rerun instance
	for time.Now().Before(deadline) {
		if rec, err := readInstanceFile(filepath.Join(dir, instanceFileName)); err == nil && rec.PID != first.PID {
			rerun = rec
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if rerun.PID == 0 {
		t.Fatal("the rerun child never published instance.json")
	}
	checkHealthNow(t, rerun)
	time.Sleep(200 * time.Millisecond)
	if n := opens.Load(); n != 1 {
		t.Fatalf("browser opened %d times across the bind rerun, want exactly 1", n)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor did not stop after cancel")
	}
}

// TestSuperviseBindFailureDoesNotRejectTrial: an unconfirmed trial that
// exits with ExitBindFailed must be rerun as the same image - never
// rolled back through Trials.Failed, never rejected - and the rerun's
// health confirm goes through the normal trial policy.
func TestSuperviseBindFailureDoesNotRejectTrial(t *testing.T) {
	shortenWindows(t)
	dir := t.TempDir()
	exit := exitFile(t, dir, "5")
	doneFile := filepath.Join(dir, "exit-5-trial.done")
	var failed, healthy atomic.Int32
	done, cancel := startSupervisor(t, dir, Options{
		Child: fakeApp(t),
		Env: appEnv("KS_TEST_MODE=exit-once", "KS_TEST_EXIT_CODE=5",
			"KS_TEST_EXIT_FILE="+exit, "KS_TEST_EXIT_DONE="+doneFile),
		Trials: TrialPolicy{
			InTrial: func() bool { return true },
			Healthy: func() error { healthy.Add(1); return nil },
			Failed:  func(reason error) error { failed.Add(1); return nil },
		},
	})

	record := waitForInstance(t, dir, 10*time.Second)
	checkHealthNow(t, record)
	deadline := time.Now().Add(5 * time.Second)
	for healthy.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if healthy.Load() == 0 {
		t.Fatal("the rerun trial was never confirmed")
	}
	if n := failed.Load(); n != 0 {
		t.Fatalf("Trials.Failed called %d times; a bind failure must not reject the trial", n)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor did not stop after cancel")
	}
}

// trialVersion is the fake version the launch trial tests stage and
// activate; it must satisfy update.ValidVersion.
const trialVersion = "v9.9.9"

// startTrial lays out an activated unconfirmed trial of trialVersion in
// dir, exactly as the launcher sets one up: a seeded database, a staged
// version activated through update.Activate, and the journal saying trying.
// It returns the install root (update.Activate currently only reads it, but
// the real launcher passes the real install root).
func startTrial(t *testing.T, dir string) string {
	t.Helper()
	install := t.TempDir()
	s, err := store.Open(update.DatabasePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`CREATE TABLE IF NOT EXISTS probe (v TEXT)`); err != nil {
		s.Close()
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO probe (v) VALUES ('old')`); err != nil {
		s.Close()
		t.Fatal(err)
	}
	s.Close()
	if err := update.InstallStage(dir, trialVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := update.Activate(install, dir, update.DatabasePath(dir)); err != nil {
		t.Fatal(err)
	}
	return install
}

// TestSuperviseBindExhaustionAbandonsTrialAndRelaunchTriesItAgain: after
// maxBindAttempts consecutive ExitBindFailed results the supervisor gives
// up with a visible error, but it first abandons the unconfirmed trial
// without rejecting it (the ports being taken is environmental, not the
// candidate's fault), so the journal goes back to staged and a relaunch
// activates and confirms the candidate instead of rejecting it.
func TestSuperviseBindExhaustionAbandonsTrialAndRelaunchTriesItAgain(t *testing.T) {
	shortenWindows(t)
	dir := t.TempDir()
	install := startTrial(t, dir)

	// The child must exit ExitBindFailed on every start: the test binary
	// plays the bindloop role, which exits 5 unconditionally, driving the
	// supervisor through the whole retry budget to exhaustion.
	var resolves atomic.Int32
	var failed atomic.Int32
	done, _ := startSupervisor(t, dir, Options{
		Child: fakeApp(t),
		Resolve: func() (string, error) {
			resolves.Add(1)
			return fakeApp(t), nil
		},
		Env: []string{"KS_TEST_ROLE=bindloop"},
		Trials: TrialPolicy{
			InTrial: func() bool { return true },
			Healthy: func() error { return nil },
			Failed: func(reason error) error {
				failed.Add(1)
				return nil
			},
		},
	})
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "could not bind the ports") {
			t.Fatalf("Run = %v, want the bind-exhaustion error", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the supervisor did not give up after the bind failures")
	}
	if n := resolves.Load(); n != 1 {
		t.Errorf("Resolve called %d times; bind retries must never re-resolve", n)
	}
	if n := failed.Load(); n != 0 {
		t.Fatalf("Trials.Failed called %d times; a bind failure must not reject the trial", n)
	}
	st, err := update.ReadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Trying != nil {
		t.Errorf("trying = %+v after exhaustion; the trial must be abandoned", st.Trying)
	}
	if st.Staged != trialVersion {
		t.Errorf("staged = %q after exhaustion; the candidate must be back to staged", st.Staged)
	}
	if len(st.Rejected) != 0 {
		t.Errorf("rejected = %v after exhaustion; the candidate must not be rejected", st.Rejected)
	}

	// The relaunch: the journaled staged candidate is activated again
	// (update.Activate, like runSupervisor's Resolve), serves healthy and
	// confirms - the exact flow a stale trying entry would have derailed
	// into a rejection.
	curVersion := ""
	var confirmed atomic.Bool
	done2, cancel2 := startSupervisor(t, dir, Options{
		Child: fakeApp(t),
		Resolve: func() (string, error) {
			v, err := update.Activate(install, dir, update.DatabasePath(dir))
			if err != nil {
				return "", err
			}
			curVersion = v
			return fakeApp(t), nil
		},
		Env: appEnv(),
		Trials: TrialPolicy{
			InTrial: func() bool {
				st, rerr := update.ReadState(dir)
				return rerr == nil && st.Trying != nil && st.Trying.Version == curVersion
			},
			Healthy: func() error {
				confirmed.Store(true)
				return update.Confirm(dir, curVersion)
			},
			Failed: func(reason error) error {
				t.Errorf("the relaunch trial failed: %v", reason)
				return update.Reject(dir, curVersion)
			},
		},
	})
	record := waitForInstance(t, dir, 10*time.Second)
	checkHealthNow(t, record)
	deadline := time.Now().Add(10 * time.Second)
	for !confirmed.Load() {
		if time.Now().After(deadline) {
			cancel2()
			<-done2
			t.Fatal("the relaunched candidate was never confirmed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel2()
	select {
	case err := <-done2:
		if err != nil {
			t.Fatalf("relaunch Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the relaunched supervisor did not stop after cancel")
	}
	st, err = update.ReadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Current != trialVersion {
		t.Errorf("current = %q after the relaunch, want %s confirmed", st.Current, trialVersion)
	}
	if len(st.Rejected) != 0 {
		t.Errorf("rejected = %v; the candidate must never have been rejected", st.Rejected)
	}
}

// TestSuperviseRefusesRemoveDuringUnconfirmedTrial: PLAN 1a refuses Remove
// during an unconfirmed trial ("One moment: kids-shelf is finishing an
// update"), so a parent cannot turn a healthy update into a rejected one.
// If a child still exits with ExitRemove mid-trial (a stale request or an
// older app build), the supervisor honors the refusal instead of running
// OnRemove, abandons the trial without rejecting (candidate back to
// staged), and ends cleanly.
func TestSuperviseRefusesRemoveDuringUnconfirmedTrial(t *testing.T) {
	shortenWindows(t)
	dir := t.TempDir()
	startTrial(t, dir)

	var removes atomic.Int32
	var failed atomic.Int32
	exit := exitFile(t, dir, "4")
	done, _ := startSupervisor(t, dir, Options{
		Child: fakeApp(t),
		Env: appEnv("KS_TEST_MODE=exit-once", "KS_TEST_EXIT_CODE=4",
			"KS_TEST_EXIT_FILE="+exit),
		OnRemove: func() error { removes.Add(1); return nil },
		Trials: TrialPolicy{
			InTrial: func() bool { return true },
			Failed: func(reason error) error {
				failed.Add(1)
				return nil
			},
		},
	})
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after ExitRemove mid-trial = %v, want a clean end (removal refused)", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor did not end after the refused removal")
	}
	if n := removes.Load(); n != 0 {
		t.Fatalf("OnRemove called %d times; PLAN 1a refuses Remove during an unconfirmed trial", n)
	}
	if n := failed.Load(); n != 0 {
		t.Fatalf("Trials.Failed called %d times; the refused removal must not reject the trial", n)
	}
	st, err := update.ReadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Trying != nil {
		t.Errorf("trying = %+v; the trial must be abandoned when the removal is refused", st.Trying)
	}
	if st.Staged != trialVersion {
		t.Errorf("staged = %q; the candidate must be back to staged", st.Staged)
	}
	if len(st.Rejected) != 0 {
		t.Errorf("rejected = %v; the candidate must not be rejected", st.Rejected)
	}
}

// TestSuperviseOnRemoveDispatch: a child that exits with ExitRemove hands
// the launcher over to Options.OnRemove exactly once; without OnRemove the
// exit is a normal clean end.
func TestSuperviseOnRemoveDispatch(t *testing.T) {
	shortenWindows(t)
	exit := exitFile(t, t.TempDir(), "4")
	var removes atomic.Int32
	dir := t.TempDir()
	done, _ := startSupervisor(t, dir, Options{
		Child: fakeApp(t),
		Env: appEnv("KS_TEST_MODE=exit-once", "KS_TEST_EXIT_CODE=4",
			"KS_TEST_EXIT_FILE="+exit),
		OnRemove: func() error { removes.Add(1); return nil },
	})
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after ExitRemove = %v, want a clean end", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor did not end after ExitRemove")
	}
	if removes.Load() != 1 {
		t.Fatalf("OnRemove called %d times, want exactly once", removes.Load())
	}

	// Without OnRemove the same exit is treated as a normal clean exit.
	dir2 := t.TempDir()
	exit2 := exitFile(t, dir2, "4")
	done2, _ := startSupervisor(t, dir2, Options{
		Child: fakeApp(t),
		Env: appEnv("KS_TEST_MODE=exit-once", "KS_TEST_EXIT_CODE=4",
			"KS_TEST_EXIT_FILE="+exit2),
	})
	select {
	case err := <-done2:
		if err != nil {
			t.Fatalf("Run with nil OnRemove after ExitRemove = %v, want a clean end", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor did not end after ExitRemove with nil OnRemove")
	}
}

// TestSuperviseOnRemoveErrorSurfaces: a failing remover is a visible
// failure, never a silent end.
func TestSuperviseOnRemoveErrorSurfaces(t *testing.T) {
	shortenWindows(t)
	dir := t.TempDir()
	exit := exitFile(t, dir, "4")
	done, _ := startSupervisor(t, dir, Options{
		Child: fakeApp(t),
		Env: appEnv("KS_TEST_MODE=exit-once", "KS_TEST_EXIT_CODE=4",
			"KS_TEST_EXIT_FILE="+exit),
		OnRemove: func() error { return errors.New("remove failed") },
	})
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "remove failed") {
			t.Fatalf("Run = %v, want the OnRemove error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor did not end after the OnRemove error")
	}
}

// TestSuperviseInstanceJsonCarriesPublicPorts: instance.json must record
// the public app port and the public libraries port (the next free port
// after it), the health probe goes through the app proxy, and the
// libraries port answers through its own proxy.
func TestSuperviseInstanceJsonCarriesPublicPorts(t *testing.T) {
	shortenWindows(t)
	dir := t.TempDir()
	base := freePort(t)
	done, cancel := startSupervisor(t, dir, Options{
		Port:  base,
		Child: fakeApp(t),
		Env:   appEnv(),
	})

	record := waitForInstance(t, dir, 10*time.Second)
	if record.Port < base || record.Port > base+maxPortAttempts-1 {
		t.Fatalf("public app port = %d, want one of %d..%d", record.Port, base, base+maxPortAttempts-1)
	}
	if record.LibrariesPort <= record.Port {
		t.Errorf("libraries port = %d, want the next free port after the app port %d", record.LibrariesPort, record.Port)
	}
	// The health probe flows through the public proxy to the child.
	checkHealthNow(t, record)
	// The libraries port lives too (the fake child binds no libraries
	// listener, so its proxy answers the 503 the starting state would).
	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(record.LibrariesPort) + "/kiwix/")
	if err != nil {
		t.Fatalf("libraries port: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("libraries port status = %d, want 503", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor did not stop after cancel")
	}
}

// TestSuperviseChildEnvironment: the child is spawned with the phase-4
// identity envelope - --listen/--libraries-listen private ports,
// KS_PROXY_TOKEN (non-empty), and KS_PUBLIC_PORT and
// KS_PUBLIC_LIBRARIES_PORT matching instance.json. Trial refusal is the
// app's own durable journal state, so the supervisor passes no
// KS_TRIAL_UNTIL expiry.
func TestSuperviseChildEnvironment(t *testing.T) {
	shortenWindows(t)
	dir := t.TempDir()
	envFile := filepath.Join(dir, "child.env")
	done, cancel := startSupervisor(t, dir, Options{
		Child: fakeApp(t),
		Env:   appEnv("KS_TEST_ENV_FILE=" + envFile),
		Trials: TrialPolicy{
			InTrial: func() bool { return true },
			Healthy: func() error { return nil },
			Failed:  func(reason error) error { return nil },
		},
	})

	record := waitForInstance(t, dir, 10*time.Second)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(envFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the child never dumped its environment")
		}
		time.Sleep(10 * time.Millisecond)
	}
	b, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("reading the child environment: %v", err)
	}
	env := string(b)
	line := func(key string) string {
		prefix := key + "="
		for _, l := range strings.Split(env, "\n") {
			if v, ok := strings.CutPrefix(l, prefix); ok {
				return v
			}
		}
		return ""
	}
	if !strings.Contains(env, "KS_SUPERVISED=1") {
		t.Error("the child is not marked KS_SUPERVISED=1")
	}
	if got := line("KS_INSTANCE_ID"); got != record.ID {
		t.Errorf("KS_INSTANCE_ID = %q, want the published instance id %q", got, record.ID)
	}
	if got := line("KS_PROXY_TOKEN"); got == "" {
		t.Error("KS_PROXY_TOKEN is empty; the child needs the proxy secret")
	}
	if got := line("KS_PUBLIC_PORT"); got != strconv.Itoa(record.Port) {
		t.Errorf("KS_PUBLIC_PORT = %q, want the public app port %d", got, record.Port)
	}
	if got := line("KS_PUBLIC_LIBRARIES_PORT"); got != strconv.Itoa(record.LibrariesPort) {
		t.Errorf("KS_PUBLIC_LIBRARIES_PORT = %q, want the public libraries port %d", got, record.LibrariesPort)
	}
	if got := line("KS_TRIAL_UNTIL"); got != "" {
		t.Errorf("KS_TRIAL_UNTIL = %q; the supervisor no longer hands the trial to the child (trial refusal is the app's durable journal state)", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor did not stop after cancel")
	}
}

// runFakeSupervisor is the test-binary subprocess role that runs a real
// supervised child (lifeline pipe plus OS lifelines), records the child's
// pid in KS_SUP_PIDFILE, and then blocks until it is killed - the kill -9
// test's supervisor.
func runFakeSupervisor() error {
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	defer pr.Close()
	defer pw.Close()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	cmd := exec.Command(os.Args[0], "--port", strconv.Itoa(port))
	cmd.Stdin = pr
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	prepareChild(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	cleanup, err := attachChild(cmd)
	if err != nil {
		cmd.Process.Kill()
		return err
	}
	defer cleanup()
	if pidfile := os.Getenv("KS_SUP_PIDFILE"); pidfile != "" {
		if err := os.WriteFile(pidfile, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
			return err
		}
	}
	// Hold until killed. Blocking on the child's exit would end the
	// supervisor as soon as the child stops, defeating the kill test.
	select {}
}

// TestSuperviseTrialThatCannotStartIsBlockedNotFailed: a candidate the
// operating system refuses to start (Smart App Control holding back a new
// file) goes to Blocked, not Failed, so the update is kept for a later try
// instead of being rejected for good; the supervisor then re-resolves and
// serves the current version.
func TestSuperviseTrialThatCannotStartIsBlockedNotFailed(t *testing.T) {
	shortenWindows(t)
	dir := t.TempDir()
	app := fakeApp(t)
	// A file that exists but cannot be executed stands in for a binary
	// the operating system refuses to start.
	refused := filepath.Join(dir, "refused-candidate")
	if err := os.WriteFile(refused, []byte("not a program"), 0o644); err != nil {
		t.Fatal(err)
	}
	var resolves, failed atomic.Int32
	var blockedReason atomic.Value
	done, cancel := startSupervisor(t, dir, Options{
		Child: app,
		Resolve: func() (string, error) {
			if resolves.Add(1) == 1 {
				return refused, nil
			}
			return app, nil
		},
		Env: appEnv(),
		Trials: TrialPolicy{
			InTrial: func() bool { return resolves.Load() == 1 },
			Failed: func(error) error {
				failed.Add(1)
				return nil
			},
			Blocked: func(reason error) error {
				blockedReason.Store(reason.Error())
				return nil
			},
		},
	})

	record := waitForInstance(t, dir, 10*time.Second)
	checkHealthNow(t, record)
	if failed.Load() != 0 {
		t.Error("Failed was called for a candidate that could not start; the update would be rejected for good")
	}
	if r, _ := blockedReason.Load().(string); !strings.Contains(r, "starting the new version") {
		t.Errorf("Blocked reason = %q, want the start failure", r)
	}
	if n := resolves.Load(); n != 2 {
		t.Errorf("Resolve called %d times, want 2 (blocked candidate, then the current version)", n)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor did not stop after cancel")
	}
}
