//go:build windows

package launch

import (
	"errors"
	"fmt"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// prepareChild arms the OS lifeline that must be in place before the child
// starts; Windows has no equivalent of Pdeathsig, so nothing is set here.
func prepareChild(cmd *exec.Cmd) {}

// assignChildToJob assigns proc to job; a var so the windows attach tests
// can make the assignment fail after the child has exited.
var assignChildToJob = windows.AssignProcessToJobObject

// waitTimedOut is the zero-timeout wait's "still running" answer;
// x/sys/windows does not export it as a plain uint32 constant (its
// WAIT_TIMEOUT is the Win32 error of the same value).
const waitTimedOut = 0x102

// childHasExited reports whether the process behind handle has exited: the
// zero-timeout wait's WAIT_OBJECT_0 decides alone because a signaled
// process handle means the process is gone whatever its exit code - a
// process may legitimately exit with 259 (STILL_ACTIVE) as its status, so
// the exit code would be a second opinion that lies. A var so the windows
// tests can pin the exited-child routing of a failed attach.
var childHasExited = func(proc windows.Handle) (bool, error) {
	wait, err := windows.WaitForSingleObject(proc, 0)
	if err != nil {
		return false, err
	}
	switch wait {
	case windows.WAIT_OBJECT_0:
		return true, nil
	case waitTimedOut:
		return false, nil
	default:
		return false, fmt.Errorf("WaitForSingleObject: %#x", wait)
	}
}

// attachChild puts the child in a job object with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE: when this supervisor exits or is
// killed, the job handle closes and the OS kills the child. The returned
// cleanup closes the job handle when the supervision ends, which is also
// what kills the child if this process dies abruptly.
func attachChild(cmd *exec.Cmd) (func(), error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("creating the child job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("configuring the child job object: %w", err)
	}
	// assignment holds for the process's lifetime, so this handle can be
	// closed right after.
	proc, err := windows.OpenProcess(windows.PROCESS_ALL_ACCESS, false, uint32(cmd.Process.Pid))
	if err != nil {
		windows.CloseHandle(job)
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			// The pid is already gone: the child exited before the job
			// attach. os/exec cannot create the child suspended, so a
			// fast-exiting child races this call; the supervisor routes
			// this through the normal exit handling.
			return nil, errChildExited
		}
		// Any other failure (the process is alive but cannot be opened)
		// is real and must surface.
		return nil, fmt.Errorf("opening the child process: %w", err)
	}
	if err := assignChildToJob(job, proc); err != nil {
		// The assignment can fail because the child exited between
		// OpenProcess and AssignProcessToJobObject (os/exec cannot create
		// the child suspended, so a fast-exiting child races the attach):
		// a process that is already gone cannot be assigned to a job.
		// Probe the process handle and, when it has exited, route through
		// the same exited-child handling as the vanished pid above; any
		// other failure is real and must surface.
		exited, perr := childHasExited(proc)
		windows.CloseHandle(proc)
		windows.CloseHandle(job)
		if perr == nil && exited {
			return nil, errChildExited
		}
		return nil, fmt.Errorf("assigning the child to the job object: %w", err)
	}
	windows.CloseHandle(proc)
	return func() { windows.CloseHandle(job) }, nil
}
