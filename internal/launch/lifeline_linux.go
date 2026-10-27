//go:build linux

package launch

import (
	"os/exec"
	"syscall"
)

// prepareChild arms the OS lifeline that must be in place before the child
// starts: when this supervisor dies, the kernel sends the child SIGKILL.
func prepareChild(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}

// attachChild arms OS lifelines that need the child's process handle and
// returns a cleanup that removes them when the supervision ends. Linux
// needs nothing beyond Pdeathsig; the pipe lifeline covers the rest.
func attachChild(cmd *exec.Cmd) (func(), error) {
	return func() {}, nil
}
