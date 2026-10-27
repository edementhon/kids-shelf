//go:build windows

package launch

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// platformShowError shows an error dialog on Windows with the native
// MessageBox (MB_ICONERROR), so a GUI-subsystem build fails visibly. It is
// topmost and asks for the foreground: a process the parent did not start
// directly (the remover's temp copy, an installer behind the browser) has
// no foreground rights, and its dialog opened behind Edge in the Windows VM.
func platformShowError(title, msg string) error {
	titlePtr, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return fmt.Errorf("encoding the dialog title: %w", err)
	}
	msgPtr, err := windows.UTF16PtrFromString(msg)
	if err != nil {
		return fmt.Errorf("encoding the dialog message: %w", err)
	}
	ret, err := windows.MessageBox(0, msgPtr, titlePtr, windows.MB_OK|windows.MB_ICONERROR|windows.MB_SETFOREGROUND|windows.MB_TOPMOST)
	if err != nil || ret == 0 {
		if err == nil {
			err = fmt.Errorf("MessageBox returned %d", ret)
		}
		return fmt.Errorf("showing the error dialog: %w", err)
	}
	return nil
}
