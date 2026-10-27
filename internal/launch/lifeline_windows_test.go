//go:build windows

package launch

import (
	"errors"
	"os/exec"
	"testing"

	"golang.org/x/sys/windows"
)

// TestAttachChildPidAlreadyGone: a child that has already exited cannot be
// opened (ERROR_INVALID_PARAMETER); attachChild reports the exited-child
// race - errChildExited - so the supervisor routes the exit through the
// normal handling (trial rollback when in trial) instead of surfacing an
// attach error.
func TestAttachChildPidAlreadyGone(t *testing.T) {
	cmd := exec.Command("cmd", "/C", "exit /b 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := attachChild(cmd); !errors.Is(err, errChildExited) {
		t.Fatalf("attachChild on a gone pid = %v, want errChildExited", err)
	}
}

// TestAssignFailureAfterExitRoutesToErrChildExited: when the child exits
// between OpenProcess and AssignProcessToJobObject, the assignment fails on
// a process that is no longer runnable; attachChild must probe the process
// handle (WaitForSingleObject with 0 timeout / GetExitCodeProcess) and, if
// the child has exited, report the same errChildExited race as the vanished
// pid, which superviseOne routes through the trial-rollback exit handling.
func TestAssignFailureAfterExitRoutesToErrChildExited(t *testing.T) {
	oldAssign := assignChildToJob
	t.Cleanup(func() { assignChildToJob = oldAssign })

	// ping lives long enough that OpenProcess succeeds; the injected
	// assignment kills the child and waits for the exit before failing,
	// exactly the window a fast-exiting child opens between the open and
	// the assign.
	cmd := exec.Command("ping", "-n", "5", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	assignChildToJob = func(job, proc windows.Handle) error {
		cmd.Process.Kill()
		<-waitDone
		return windows.ERROR_ACCESS_DENIED
	}
	if _, err := attachChild(cmd); !errors.Is(err, errChildExited) {
		t.Fatalf("attachChild after an assign failure on an exited child = %v, want errChildExited", err)
	}
}

// TestChildHasExitedAcceptsExitCode259: a process that exits with 259
// (STILL_ACTIVE) has still exited - WAIT_OBJECT_0 decides alone, the exit
// code is never consulted, so the probe must report it as gone.
func TestChildHasExitedAcceptsExitCode259(t *testing.T) {
	cmd := exec.Command("cmd", "/C", "exit /b 259")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	proc, err := windows.OpenProcess(windows.PROCESS_ALL_ACCESS, false, uint32(cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(proc)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	exited, err := childHasExited(proc)
	if err != nil {
		t.Fatalf("childHasExited: %v", err)
	}
	if !exited {
		t.Fatal("childHasExited on a process that exited with code 259 = false, want true")
	}
}

// TestAttachChildRoutesOnlyOnTheProbe: the exited-child routing reads the
// injected probe (childHasExited) and nothing else - a probe answering
// "exited" routes the failed attach through errChildExited with no exit
// code consulted anywhere.
func TestAttachChildRoutesOnlyOnTheProbe(t *testing.T) {
	oldProbe := childHasExited
	oldAssign := assignChildToJob
	t.Cleanup(func() {
		childHasExited = oldProbe
		assignChildToJob = oldAssign
	})

	cmd := exec.Command("ping", "-n", "5", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()
	childHasExited = func(windows.Handle) (bool, error) { return true, nil }
	assignChildToJob = func(job, proc windows.Handle) error {
		return windows.ERROR_ACCESS_DENIED
	}
	if _, err := attachChild(cmd); !errors.Is(err, errChildExited) {
		t.Fatalf("attachChild with an exited probe = %v, want errChildExited", err)
	}
}
