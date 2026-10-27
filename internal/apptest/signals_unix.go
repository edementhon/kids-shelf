//go:build !windows

package apptest

import (
	"os"
	"syscall"
)

// stopSignals are the signals the fake app treats as a clean stop.
var stopSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}
