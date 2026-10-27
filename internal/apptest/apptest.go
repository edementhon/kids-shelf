// Package apptest plays the fake app versions that the launcher and
// end-to-end tests re-exec: the test binary, copied into an install layout
// as a versioned build, plays the role a supervised app build plays. The
// role comes from KS_TEST_ROLE=app and the mode from KS_TEST_MODE:
//
//	healthy                 serve /healthz until stopped (exit 0 on the stop signal)
//	crash-at-start          exit 1 at once
//	exit-3-restart          exit 3 at once
//	hang-no-health          run forever, never answer /healthz (exit 0 on the stop signal)
//	migrate-schema-then-crash  migrate the database, write a probe row, exit 1
//	                        (one-shot when KS_TEST_CRASH_FILE is set, so the
//	                        previous version serves cleanly after rollback)
//	serve-exit-once         serve healthy until KS_TEST_EXIT_FILE appears, then
//	                        exit once with KS_TEST_EXIT_CODE (default 3),
//	                        removing the file; later restarts serve healthy
//	exit-once               serve nothing; exit with KS_TEST_EXIT_CODE (default 3)
//	                        once KS_TEST_EXIT_FILE appears, removing it, then
//	                        serve healthy on later restarts
//
// The supervisor passes KS_SUPERVISED=1, KS_INSTANCE_ID and KS_INSTALL;
// the instance id is answered by /healthz like the real app does.
package apptest

import (
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"
)

// RunIfRequested plays a fake app version when the process was started as
// one and reports whether it did; call it from TestMain before m.Run so a
// plain `go test` never enters here.
func RunIfRequested() bool {
	if os.Getenv("KS_TEST_ROLE") != "app" {
		return false
	}
	// The supervisor passes a lifeline pipe as the child's stdin. When its
	// write end closes the supervisor died (for any reason, including
	// kill -9): the fake app must exit, like the real app shuts down
	// within launch.LifelineGrace. (The watcher lives here rather than in
	// internal/launch because the launch package's tests import this one.)
	go func() {
		<-lifelineEOF()
		markLeft()
		os.Exit(0)
	}()
	switch os.Getenv("KS_TEST_MODE") {
	case "crash-at-start":
		os.Exit(1)
	case "exit-3-restart":
		os.Exit(3)
	case "hang-no-health":
		block()
	case "migrate-schema-then-crash":
		// Migrate and crash once; with KS_TEST_CRASH_FILE the crash is
		// consumed, so the previous version that runs after the rollback
		// serves cleanly instead of crashing again.
		if f := os.Getenv("KS_TEST_CRASH_FILE"); f != "" {
			if _, err := os.Stat(f); err != nil {
				serveHealthy()
				return true
			}
			os.Remove(f)
		}
		writeRow(os.Getenv("KS_TEST_DB"))
		os.Exit(1)
	case "serve-exit-once":
		// Serve healthy until KS_TEST_EXIT_FILE appears, then exit once
		// with KS_TEST_EXIT_CODE (removing the file and marking
		// KS_TEST_EXIT_DONE); later restarts serve healthy. This is the
		// "app is running, stages an update, then the dashboard asks to
		// restart" shape.
		exitF := os.Getenv("KS_TEST_EXIT_FILE")
		if exitF != "" {
			if doneF := os.Getenv("KS_TEST_EXIT_DONE"); doneF != "" {
				if _, err := os.Stat(doneF); err == nil {
					serveHealthy()
					return true
				}
			}
			exitCode := 3
			if c := os.Getenv("KS_TEST_EXIT_CODE"); c != "" {
				exitCode, _ = strconv.Atoi(c)
			}
			serveUntilExit(exitF, exitCode)
			return true
		}
		serveHealthy()
		return true
	case "exit-once":
		// Exit once with KS_TEST_EXIT_CODE when KS_TEST_EXIT_FILE appears
		// (removing it and marking KS_TEST_EXIT_DONE), then serve healthy
		// on later restarts.
		exitF := os.Getenv("KS_TEST_EXIT_FILE")
		if exitF == "" {
			serveHealthy()
			return true
		}
		doneF := os.Getenv("KS_TEST_EXIT_DONE")
		if doneF != "" {
			if _, err := os.Stat(doneF); err == nil {
				serveHealthy()
				return true
			}
		}
		waitFor(exitF)
		os.Remove(exitF)
		if doneF != "" {
			os.WriteFile(doneF, []byte("done"), 0o600)
		}
		code := 3
		if c := os.Getenv("KS_TEST_EXIT_CODE"); c != "" {
			code, _ = strconv.Atoi(c)
		}
		os.Exit(code)
	default: // "healthy"
		serveHealthy()
	}
	return true
}

// markLeft records that this fake app exited because its lifeline EOFed
// (its supervisor died), so tests can observe the exit without polling
// process ids.
func markLeft() {
	if f := os.Getenv("KS_TEST_LEFT_FILE"); f != "" {
		os.WriteFile(f, []byte("left"), 0o600)
	}
}

// lifelineEOF watches the supervisor's lifeline pipe (our stdin) and
// returns a channel closed when the pipe reaches EOF: the write end closed
// because the supervisor died. It mirrors launch.LifelineEOF (which this
// package cannot import without a test-only import cycle).
func lifelineEOF() <-chan struct{} {
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 1)
		for {
			if _, err := os.Stdin.Read(buf); err != nil {
				close(done)
				return
			}
		}
	}()
	return done
}

// block runs forever without answering anything, exiting 0 on the stop
// signal so the launcher's shutdown path stays clean.
func block() {
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, stopSignals...)
	<-sig
	os.Exit(0)
}

// serveHealthy answers /healthz with the instance id until the stop signal
// arrives, then exits 0.
func serveHealthy() {
	srv, errCh := startHealthServer()
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, stopSignals...)
	select {
	case <-sig:
		srv.Close()
		os.Exit(0)
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "fake app: serving: %v\n", err)
			os.Exit(2)
		}
		os.Exit(0)
	}
}

// serveUntilExit runs the healthz server and exits with code once the file
// appears (removing it and marking the done sentinel), or 0 on the stop
// signal.
func serveUntilExit(exitFile string, code int) {
	srv, errCh := startHealthServer()
	done := make(chan struct{})
	go func() {
		waitFor(exitFile)
		os.Remove(exitFile)
		if doneF := os.Getenv("KS_TEST_EXIT_DONE"); doneF != "" {
			os.WriteFile(doneF, []byte("done"), 0o600)
		}
		srv.Close()
		close(done)
	}()
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, stopSignals...)
	select {
	case <-sig:
		os.Exit(0)
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "fake app: serving: %v\n", err)
			os.Exit(2)
		}
		// The server was closed because the exit file appeared: the
		// watcher goroutine is still finishing the handoff (it may be in
		// srv.Close), so wait for its done marker instead of guessing the
		// exit code.
		<-done
		os.Exit(code)
	case <-done:
		os.Exit(code)
	}
}

// startHealthServer binds --listen (the supervisor's private app port; the
// phase-4 launcher contract) and serves the healthz mux; serve errors
// arrive on the returned channel (ErrServerClosed after Close).
func startHealthServer() (*http.Server, <-chan error) {
	// The supervisor tests that assert the child environment (KS_PROXY_TOKEN,
	// KS_PUBLIC_*) point KS_TEST_ENV_FILE at a file the fake
	// app dumps its environment into before serving.
	if envFile := os.Getenv("KS_TEST_ENV_FILE"); envFile != "" {
		os.WriteFile(envFile, []byte(strings.Join(os.Environ(), "\n")), 0o600)
	}
	id := os.Getenv("KS_INSTANCE_ID")
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","id":%q}`, id)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<html><body>fake kids-shelf</body></html>")
	})
	addr := flagValue("--listen")
	if addr == "" {
		// Pre-phase-4 callers (the kill -9 lifeline test) still pass
		// --port and bind every interface.
		port := flagValue("--port")
		if port == "" {
			fmt.Fprintln(os.Stderr, "fake app: no --listen or --port argument")
			os.Exit(2)
		}
		addr = ":" + port
	}
	srv := &http.Server{Addr: addr, Handler: mux}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	return srv, errCh
}

// waitFor blocks until the file exists (the parent drops it to let the
// child exit, simulating a staged update plus a dashboard restart).
func waitFor(path string) {
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "fake app: %s never appeared\n", path)
			os.Exit(2)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// flagValue returns the value of the --flag argument, "" when absent.
func flagValue(flag string) string {
	args := os.Args[1:]
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}
