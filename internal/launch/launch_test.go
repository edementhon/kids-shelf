package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/edementhon/kids-shelf/internal/apptest"
	"github.com/edementhon/kids-shelf/internal/web"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMain doubles as the entry point for the run-helper subprocess that
// the simultaneous-launch and join tests spawn (env KS_RUN_HELPER=1): the
// child blocks inside Run until it is killed, holding the instance lock, so
// the parent can exercise the join path and then verify the lock is
// released when the child dies (crash then relaunch). When KS_HELPER_BARRIER
// is set, the child first writes <data>/<name>.ready and then waits for the
// barrier file to appear before calling Run, so several helpers launch at
// the same instant.
func TestMain(m *testing.M) {
	if apptest.RunIfRequested() {
		return
	}
	if os.Getenv("KS_TEST_ROLE") == "supervisor" {
		// The kill -9 lifeline test's supervisor subprocess: it holds a
		// real supervised child and blocks until it is killed.
		fmt.Fprintf(os.Stderr, "fake supervisor: %v\n", runFakeSupervisor())
		os.Exit(2)
	}
	if os.Getenv("KS_TEST_ROLE") == "bindloop" {
		// The bind-exhaustion test's child: always exit with
		// ExitBindFailed, like a supervised app whose private loopback
		// port was taken, so the supervisor burns through the retry
		// budget.
		os.Exit(ExitBindFailed)
	}
	if os.Getenv("KS_RUN_HELPER") == "1" {
		data := os.Getenv("KS_HELPER_DATA")
		port, _ := strconv.Atoi(os.Getenv("KS_HELPER_PORT"))
		id := os.Getenv("KS_HELPER_ID")
		opts := Options{
			DataDir:   data,
			Port:      port,
			ID:        id,
			Handler:   web.New(id).Mux(),
			NoBrowser: true,
		}
		if child := os.Getenv("KS_HELPER_CHILD"); child != "" {
			// Supervised helper: the app runs as a child (a fake app
			// version); the loser joins the winner's child via /healthz.
			opts.Handler = nil
			opts.Child = child
			opts.Env = []string{"KS_TEST_ROLE=app", "KS_TEST_MODE=healthy"}
		}
		if barrier := os.Getenv("KS_HELPER_BARRIER"); barrier != "" {
			name := os.Getenv("KS_HELPER_NAME")
			ready := filepath.Join(data, name+".ready")
			if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
				fmt.Fprintf(os.Stderr, "helper: writing %s: %v\n", ready, err)
				os.Exit(2)
			}
			deadline := time.Now().Add(60 * time.Second)
			for {
				if _, err := os.Stat(barrier); err == nil {
					break
				}
				if time.Now().After(deadline) {
					fmt.Fprintf(os.Stderr, "helper: the barrier %s never appeared\n", barrier)
					os.Exit(2)
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
		if err := Run(context.Background(), opts); err != nil {
			fmt.Fprintf(os.Stderr, "helper: %v\n", err)
			os.Exit(2)
		}
		// Run returned, so this process joined the running instance: a
		// server blocks inside Run until it is killed.
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// freePort returns a port that was free a moment ago; the caller then owns
// the race of it being taken before use, which is acceptable in tests.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// waitForInstance polls for instance.json in dir until it parses and, when
// wantPID is non-zero, was written by that process (a killed holder leaves its
// old record behind until the next holder overwrites it).
func waitForInstance(t *testing.T, dir string, timeout time.Duration, wantPID ...int) instance {
	t.Helper()
	deadline := time.Now().Add(timeout)
	path := filepath.Join(dir, instanceFileName)
	for time.Now().Before(deadline) {
		if record, err := readInstanceFile(path); err == nil && (len(wantPID) == 0 || record.PID == wantPID[0]) {
			return record
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no matching %s at %s within %s", instanceFileName, path, timeout)
	return instance{}
}

// checkHealthNow GETs /healthz on the record's port and requires the id.
func checkHealthNow(t *testing.T, record instance) {
	t.Helper()
	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(record.Port) + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz on port %d: %v", record.Port, err)
	}
	defer resp.Body.Close()
	var answer struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		t.Fatalf("decoding /healthz: %v", err)
	}
	if answer.ID != record.ID {
		t.Fatalf("healthz id = %q, want %q", answer.ID, record.ID)
	}
}

func TestLockExclusive(t *testing.T) {
	dir := t.TempDir()
	l1, err := acquireLock(dir)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	l2, err := acquireLock(dir)
	if err == nil {
		l2.Close()
		l1.Close()
		t.Fatal("a second lock on the same instance file succeeded")
	}
	if !errors.Is(err, errLockBusy) {
		l1.Close()
		t.Fatalf("second lock error = %v, want errLockBusy", err)
	}
	// Releasing the first lock lets the next process acquire it: this is
	// what a crashed instance leaves behind, and what relaunch relies on.
	if err := l1.Close(); err != nil {
		t.Fatalf("closing the first lock: %v", err)
	}
	l3, err := acquireLock(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	l3.Close()
}

// TestRunJoinsRunningInstance covers the 1a launcher contract end to end:
// simultaneous launches (the second joins the first), crash then relaunch
// (killing the holder releases the OS lock and a fresh Run takes over).
func TestRunJoinsRunningInstance(t *testing.T) {
	dir := t.TempDir()
	port := freePort(t)
	id := "helper-session-" + strconv.Itoa(port)

	cmd := exec.Command(os.Args[0], "-test.run=^TestRunJoinsRunningInstance$")
	cmd.Env = append(os.Environ(),
		"KS_RUN_HELPER=1",
		"KS_HELPER_DATA="+dir,
		"KS_HELPER_PORT="+strconv.Itoa(port),
		"KS_HELPER_ID="+id,
	)
	var helperStderr bytes.Buffer
	cmd.Stderr = &helperStderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the helper process: %v", err)
	}
	helperKilled := false
	defer func() {
		if !helperKilled {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()

	helperRecord := waitForInstance(t, dir, 10*time.Second)
	if helperRecord.PID == os.Getpid() {
		t.Fatalf("instance.json was written by this test process, not the helper; helper stderr: %s", helperStderr.String())
	}
	if helperRecord.Port != port {
		t.Fatalf("helper runs on port %d, want %d", helperRecord.Port, port)
	}

	// Second launch while the helper holds the lock: Run joins (verifies
	// /healthz against instance.json) and returns without serving.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, Options{DataDir: dir, NoBrowser: true}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("joining the running instance: %v", err)
		}
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("join Run did not return")
	}
	cancel()

	// Crash the holder: the kernel drops the lock.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("killing the helper: %v; helper stderr: %s", err, helperStderr.String())
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("the helper exited without being killed")
	}
	helperKilled = true

	// Relaunch in this process: the lock is free, so Run serves.
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan error, 1)
	go func() {
		done2 <- Run(ctx2, Options{DataDir: dir, Port: port, ID: id, Handler: web.New(id).Mux(), NoBrowser: true})
	}()
	record := waitForInstance(t, dir, 10*time.Second, os.Getpid())
	if record.PID != os.Getpid() {
		t.Fatalf("instance.json pid = %d, want %d (this process)", record.PID, os.Getpid())
	}
	if record.Port != port {
		t.Fatalf("relaunch on port %d, want %d", record.Port, port)
	}
	if record.ID != id {
		t.Fatalf("instance id = %q, want %q", record.ID, id)
	}
	checkHealthNow(t, record)
	cancel2()
	select {
	case err := <-done2:
		if err != nil {
			t.Fatalf("Run after relaunch: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
}

func TestRunPortCollision(t *testing.T) {
	blocker, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("occupying a port: %v", err)
	}
	port := blocker.Addr().(*net.TCPAddr).Port
	defer blocker.Close()

	dir := t.TempDir()
	id := "port-collision-test"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{DataDir: dir, Port: port, ID: id, Handler: web.New(id).Mux(), NoBrowser: true})
	}()

	record := waitForInstance(t, dir, 10*time.Second)
	if record.Port != port+1 {
		t.Fatalf("Run picked port %d, want %d (the first is occupied)", record.Port, port+1)
	}
	checkHealthNow(t, record)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
}

func TestRunReportsStartingOrStuck(t *testing.T) {
	oldTimeout := joinTimeout
	joinTimeout = 300 * time.Millisecond
	t.Cleanup(func() { joinTimeout = oldTimeout })

	dir := t.TempDir()
	lock, err := acquireLock(dir)
	if err != nil {
		t.Fatalf("holding the lock: %v", err)
	}
	defer lock.Close()

	// The lock is held but no instance.json and no server: after the join
	// deadline Run must say the app seems to be starting or stuck and what
	// to do, never exit silently.
	err = Run(context.Background(), Options{DataDir: dir, NoBrowser: true})
	if err == nil {
		t.Fatal("Run succeeded with a held lock and no running instance")
	}
	msg := err.Error()
	if !strings.Contains(msg, "starting or stuck") {
		t.Fatalf("error %q does not mention that the app is starting or stuck", msg)
	}
	if !strings.Contains(msg, "start Kids Shelf again") {
		t.Fatalf("error %q gives no instruction on what to do", msg)
	}
}

func TestInstanceFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	record := instance{Port: 8070, ID: "abc123", PID: 4242, Started: "2026-10-26T00:00:00Z"}
	if err := writeInstanceFile(dir, record); err != nil {
		t.Fatalf("writeInstanceFile: %v", err)
	}
	got, err := readInstanceFile(filepath.Join(dir, instanceFileName))
	if err != nil {
		t.Fatalf("readInstanceFile: %v", err)
	}
	if got != record {
		t.Fatalf("round trip = %+v, want %+v", got, record)
	}
}

func TestNewID(t *testing.T) {
	a, err := NewID()
	if err != nil {
		t.Fatalf("NewID: %v", err)
	}
	b, err := NewID()
	if err != nil {
		t.Fatalf("NewID: %v", err)
	}
	if len(a) != 32 {
		t.Fatalf("id %q is %d hex chars, want 32", a, len(a))
	}
	if a == b {
		t.Fatal("two generated ids are equal")
	}
}

func TestOpenerFor(t *testing.T) {
	const target = "http://127.0.0.1:8070/"
	linux, args := openerFor("linux", target)
	if linux != "xdg-open" || len(args) != 1 || args[0] != target {
		t.Fatalf("linux opener = %q %v, want xdg-open with the target verbatim", linux, args)
	}
	other, args := openerFor("freebsd", target)
	if other != "xdg-open" || len(args) != 1 || args[0] != target {
		t.Fatalf("unknown OS opener = %q %v, want the xdg-open fallback", other, args)
	}
	darwin, args := openerFor("darwin", target)
	if darwin != "open" || len(args) != 1 || args[0] != target {
		t.Fatalf("darwin opener = %q %v, want open with the target verbatim", darwin, args)
	}
	windows, args := openerFor("windows", target)
	if windows != "rundll32" || len(args) != 2 || args[0] != "url.dll,FileProtocolHandler" || args[1] != target {
		t.Fatalf("windows opener = %q %v, want rundll32 url.dll,FileProtocolHandler with the target verbatim", windows, args)
	}
}

// TestRunSimultaneousFirstLaunch starts two Run helpers against a fresh
// data dir at the same instant (both wait for a barrier file the test
// drops only after both announced they are ready). Neither helper can find
// a published instance.json at first (the winner has not written it yet),
// so the loser must wait inside joinRunning for the winner to publish and
// pass /healthz instead of reporting the app as starting or stuck. Exactly
// one helper must become the server and the other must join it.
func TestRunSimultaneousFirstLaunch(t *testing.T) {
	dir := t.TempDir()
	port := freePort(t)
	barrier := filepath.Join(dir, "go")

	type runner struct {
		name   string
		cmd    *exec.Cmd
		stderr *bytes.Buffer
	}
	var alive []*runner
	start := func(name string) *runner {
		r := &runner{name: name, stderr: new(bytes.Buffer)}
		r.cmd = exec.Command(os.Args[0], "-test.run=^TestRunSimultaneousFirstLaunch$")
		r.cmd.Env = append(os.Environ(),
			"KS_RUN_HELPER=1",
			"KS_HELPER_DATA="+dir,
			"KS_HELPER_PORT="+strconv.Itoa(port),
			"KS_HELPER_ID=simultaneous-"+name,
			"KS_HELPER_NAME="+name,
			"KS_HELPER_BARRIER="+barrier,
		)
		r.cmd.Stderr = r.stderr
		if err := r.cmd.Start(); err != nil {
			t.Fatalf("starting helper %s: %v", name, err)
		}
		alive = append(alive, r)
		return r
	}
	a, b := start("a"), start("b")
	defer func() {
		for _, r := range alive {
			r.cmd.Process.Kill()
			r.cmd.Wait()
		}
	}()

	stderrOf := func(name string) string {
		for _, r := range alive {
			if r.name == name {
				return r.stderr.String()
			}
		}
		return ""
	}
	waitReady := func(name string) {
		path := filepath.Join(dir, name+".ready")
		deadline := time.Now().Add(30 * time.Second)
		for {
			if _, err := os.Stat(path); err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("helper %s never reached the barrier; stderr: %s", name, stderrOf(name))
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitReady("a")
	waitReady("b")
	if err := os.WriteFile(barrier, []byte("go"), 0o600); err != nil {
		t.Fatalf("dropping the barrier: %v", err)
	}

	// Exactly one helper holds the lock: instance.json names its pid.
	record := waitForInstance(t, dir, 15*time.Second)
	server, joiner := -1, -1
	for i, r := range alive {
		if r.cmd.Process.Pid == record.PID {
			server = i
		} else {
			joiner = i
		}
	}
	if server == -1 || joiner == -1 {
		t.Fatalf("instance.json pid %d matches neither helper (a=%d b=%d); a stderr: %s; b stderr: %s",
			record.PID, a.cmd.Process.Pid, b.cmd.Process.Pid, a.stderr.String(), b.stderr.String())
	}

	// The other helper joined: its Run returned nil, it exited 0 and wrote
	// nothing to stderr.
	waitDone := make(chan error, 1)
	go func() { waitDone <- alive[joiner].cmd.Wait() }()
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("join helper %s exited with %v; stderr: %s", alive[joiner].name, err, alive[joiner].stderr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("join helper %s never exited; stderr: %s", alive[joiner].name, alive[joiner].stderr.String())
	}
	if got := alive[joiner].stderr.String(); got != "" {
		t.Fatalf("join helper %s wrote to stderr: %s", alive[joiner].name, got)
	}
	checkHealthNow(t, record)

	// The server helper is still running; stop it.
	if err := alive[server].cmd.Process.Kill(); err != nil {
		t.Fatalf("killing the server helper %s: %v", alive[server].name, err)
	}
	if err := alive[server].cmd.Wait(); err == nil {
		t.Fatal("the server helper exited without being killed")
	}
	alive = nil
}

// TestRunTakeoverRespectsInstallLock: with an install layout configured,
// Run takes <install>/install.lock before the instance lock. While an
// installer holds the install lock a launch must not become the server -
// even though the instance lock is free - and once the installer finishes
// the next launch takes both locks and serves.
func TestRunTakeoverRespectsInstallLock(t *testing.T) {
	data := t.TempDir()
	install := t.TempDir()
	guard, err := LockInstall(install, 0)
	if err != nil {
		t.Fatalf("holding the install lock: %v", err)
	}

	oldJoin := joinTimeout
	joinTimeout = 300 * time.Millisecond
	t.Cleanup(func() { joinTimeout = oldJoin })

	// The installer owns the installation: the launch must not serve even
	// though nothing holds the instance lock.
	err = Run(context.Background(), Options{DataDir: data, InstallDir: install, NoBrowser: true})
	if err == nil {
		t.Fatal("Run served while an installer held the install lock")
	}
	if !strings.Contains(err.Error(), "starting or stuck") {
		t.Fatalf("Run = %v, want the starting-or-stuck explanation", err)
	}
	if _, rerr := os.Stat(filepath.Join(data, instanceFileName)); !os.IsNotExist(rerr) {
		t.Fatal("a launch blocked by the install lock published instance.json")
	}

	// The installer finishes: the next launch takes both locks and serves.
	if err := guard.Close(); err != nil {
		t.Fatalf("releasing the install lock: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{DataDir: data, InstallDir: install, Port: freePort(t), ID: "takeover", Handler: web.New("takeover").Mux(), NoBrowser: true})
	}()
	record := waitForInstance(t, data, 10*time.Second)
	checkHealthNow(t, record)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after the installer finished: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second Run did not stop after cancel")
	}
}

// TestOpenRunningWaitsForAStartingInstance: an uninstall from Settings can
// catch kids-shelf after it took the instance lock but before it published
// instance.json (the child took over 20 s in the Windows VM). OpenRunning
// must keep looking and open the parent page once the instance answers, and
// give up with an error when it never does.
func TestOpenRunningWaitsForAStartingInstance(t *testing.T) {
	const id = "starting-instance"
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "id": id})
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	port := ln.Addr().(*net.TCPAddr).Port

	var opened []string
	oldURL, oldWait := openRunningURL, openRunningWait
	openRunningURL = func(u string) error { opened = append(opened, u); return nil }
	openRunningWait = 5 * time.Second
	t.Cleanup(func() { openRunningURL, openRunningWait = oldURL, oldWait })

	dir := t.TempDir()
	go func() {
		time.Sleep(600 * time.Millisecond)
		writeInstanceFile(dir, instance{Port: port, ID: id})
	}()
	if err := OpenRunning(dir); err != nil {
		t.Fatalf("OpenRunning with an instance that publishes late: %v", err)
	}
	want := "http://127.0.0.1:" + strconv.Itoa(port) + "/admin"
	if len(opened) != 1 || opened[0] != want {
		t.Errorf("opened %v, want exactly %s", opened, want)
	}

	opened = nil
	openRunningWait = 400 * time.Millisecond
	if err := OpenRunning(t.TempDir()); err == nil {
		t.Error("OpenRunning with no instance ever published succeeded")
	}
	if len(opened) != 0 {
		t.Errorf("opened %v for an instance that never started", opened)
	}
}
