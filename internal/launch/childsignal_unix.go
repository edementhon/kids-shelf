//go:build !windows

package launch

import "syscall"

// childStopSignal is what the launcher forwards to the child when it is
// asked to stop: SIGTERM on unix.
var childStopSignal = syscall.SIGTERM
