//go:build !windows

package proc

import "os/exec"

// Hide does nothing outside Windows: only Windows gives a console program
// started by a GUI process a window of its own.
func Hide(*exec.Cmd) {}
