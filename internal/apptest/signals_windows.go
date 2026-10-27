//go:build windows

package apptest

import "os"

// stopSignals are the signals the fake app treats as a clean stop; on
// Windows there is no SIGTERM.
var stopSignals = []os.Signal{os.Interrupt}
