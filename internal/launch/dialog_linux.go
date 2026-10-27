//go:build linux

package launch

import (
	"fmt"
	"os/exec"
)

// platformShowError shows an error dialog on Linux, trying zenity, kdialog
// and xmessage in order and skipping the tools that are not installed.
func platformShowError(title, msg string) error {
	var lastErr error
	attempts := []struct {
		name string
		args []string
	}{
		{"zenity", []string{"--error", "--title", title, "--text", msg}},
		{"kdialog", []string{"--error", msg, "--title", title}},
		{"xmessage", []string{"-title", title, msg}},
	}
	for _, attempt := range attempts {
		if err := exec.Command(attempt.name, attempt.args...).Run(); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return fmt.Errorf("no dialog tool available (tried zenity, kdialog, xmessage): %v", lastErr)
}
