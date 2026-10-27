//go:build darwin

package launch

import (
	"fmt"
	"os/exec"
)

// platformShowError shows an error dialog on macOS with osascript's
// `display alert`. The title and message are passed as argv items to an
// `on run argv` handler, never interpolated into the script text, so quotes
// or AppleScript metacharacters in the message cannot break or alter the
// script.
func platformShowError(title, msg string) error {
	script := "on run argv\ndisplay alert (item 1 of argv) message (item 2 of argv)\nend run"
	if out, err := exec.Command("osascript", "-e", script, title, msg).CombinedOutput(); err != nil {
		if len(out) > 0 {
			return fmt.Errorf("showing the error dialog: %v: %s", err, out)
		}
		return fmt.Errorf("showing the error dialog: %v", err)
	}
	return nil
}
