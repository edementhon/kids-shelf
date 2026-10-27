package launch

import (
	"fmt"
	"os"
)

// ShowError makes a startup error visible when the normal channels are
// unavailable (no data dir to write last-error.txt into, or a GUI-subsystem
// build with no console): it prints title and msg to stderr and then shows
// them in a native error dialog — MessageBox on Windows, osascript on
// macOS, and zenity/kdialog/xmessage on Linux. It is a package variable so
// tests can replace the dialog with a recorder instead of popping one up on
// the test machine.
var ShowError = func(title, msg string) error {
	fmt.Fprintf(os.Stderr, "%s: %s\n", title, msg)
	return platformShowError(title, msg)
}
