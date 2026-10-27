//go:build windows

package proc

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// Hide asks Windows to start cmd with no console window. It merges into
// cmd.SysProcAttr and keeps any fields already set there, so code that sets
// other attributes (a job object, inherited handles) can call it before or
// after; code that replaces SysProcAttr wholesale must call it again.
func Hide(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}
