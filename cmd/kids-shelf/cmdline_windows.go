package main

import (
	"os/exec"
	"syscall"
)

// setRawCmdLine hands Windows the exact command line. Go quotes each
// argument with backslash escapes, which cmd.exe does not understand: the
// remover's `del /f /q "<path>"` cleanup received a mangled path and left
// every temp copy behind (Windows VM run 2026-10-27). With CmdLine set,
// cmd.Args is ignored. proc's hidden-window flags stay in SysProcAttr.
func setRawCmdLine(cmd *exec.Cmd, line string) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CmdLine = line
}
