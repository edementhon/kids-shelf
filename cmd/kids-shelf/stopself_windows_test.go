//go:build windows

package main

import "golang.org/x/sys/windows"

// stopSelf sends this process the stop signal the OS would send the
// launcher: on Windows that is a Ctrl+C generated for the console, which
// the runtime delivers as os.Interrupt. It needs a console (the desktop
// launch has one); without one - a service or a console-less CI runner -
// it returns an error so the caller can skip the signal-path test.
func stopSelf() error {
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_C_EVENT, 0)
}
