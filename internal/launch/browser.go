package launch

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// OpenURL opens url with the OS default handler.
func OpenURL(url string) error {
	return startOpener(url)
}

// openURL opens the kid home in the OS default browser. It is a variable
// so tests can count and observe opens without a browser; superviseOne
// calls it only on the first publish of a launcher's life.
var openURL = OpenURL

// OpenFile opens path with the OS default handler (used to show the
// last-error file to the user).
func OpenFile(path string) error {
	return startOpener(path)
}

// openerFor returns the command that opens target with the OS default
// handler: xdg-open on Linux, open on macOS, rundll32 url.dll,FileProtocolHandler on Windows.
func openerFor(goos, target string) (string, []string) {
	switch goos {
	case "darwin":
		return "open", []string{target}
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", target}
	default:
		return "xdg-open", []string{target}
	}
}

func opener(target string) (string, []string) {
	return openerFor(runtime.GOOS, target)
}

// startOpener runs the opener without waiting for it to finish; the target
// is passed as a plain argument, never through a shell.
func startOpener(target string) error {
	name, args := opener(target)
	if err := exec.Command(name, args...).Start(); err != nil {
		return fmt.Errorf("launching %s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}
