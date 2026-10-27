//go:build !windows

package main

import "os/exec"

// setRawCmdLine only matters on Windows (see cmdline_windows.go).
func setRawCmdLine(cmd *exec.Cmd, line string) {}
