//go:build !linux && !windows

package launch

import "os/exec"

// prepareChild arms the OS lifeline that must be in place before the child
// starts. On this platform the pipe lifeline is the only mechanism.
func prepareChild(cmd *exec.Cmd) {}

// attachChild arms OS lifelines that need the child's process handle and
// returns a cleanup that removes them when the supervision ends.
func attachChild(cmd *exec.Cmd) (func(), error) {
	return func() {}, nil
}
