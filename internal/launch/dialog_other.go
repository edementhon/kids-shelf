//go:build !windows && !darwin && !linux

package launch

import "fmt"

// platformShowError has no native dialog on other operating systems; the
// caller (ShowError) has already printed the message to stderr.
func platformShowError(title, msg string) error {
	return fmt.Errorf("no native error dialog implemented on this operating system")
}
