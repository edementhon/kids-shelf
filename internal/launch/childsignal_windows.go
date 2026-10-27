//go:build windows

package launch

import "os"

// childStopSignal is what the launcher forwards to the child when it is
// asked to stop: os.Interrupt on Windows (there is no SIGTERM).
var childStopSignal = os.Interrupt
