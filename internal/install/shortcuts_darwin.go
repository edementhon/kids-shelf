//go:build darwin

package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/edementhon/kids-shelf/internal/proc"
)

var (
	defaultsCmd = "defaults"
	plutilCmd   = "plutil"
	killallCmd  = "killall"
)

// dockCommandTimeout bounds each defaults and killall call, so a wedged
// cfprefsd can never hang an install or removal: the Dock update is best
// effort, and "best effort" must include a bound on waiting.
var dockCommandTimeout = 10 * time.Second

// CreateShortcuts pins the entry bundle to the Dock on macOS. The Dock item
// is the macOS launch surface; there is no Desktop item, because any access
// to ~/Desktop raises a TCC permission prompt ("kids-shelf would like to
// access files in your Desktop folder"), observed on macOS 15.8.2. The
// update is best effort: the entry bundle is the launcher's entry point
// whether or not the Dock shows it, so a failure is logged and never stops
// the install. Nothing here may touch the Desktop.
func CreateShortcuts(Options) ([]string, error) {
	if err := dockAdd(); err != nil {
		fmt.Fprintf(os.Stderr, "kids-shelf: could not add kids-shelf to the Dock: %v\n", err)
	}
	return nil, nil
}

// RemoveShortcuts takes the entry bundle's Dock item out again. Best effort
// like CreateShortcuts: a failure is logged and never stops the removal.
func RemoveShortcuts(Options) error {
	if err := dockRemove(); err != nil {
		fmt.Fprintf(os.Stderr, "kids-shelf: could not remove kids-shelf from the Dock: %v\n", err)
	}
	return nil
}

// dockAdd makes sure the Dock's persistent-apps holds the entry bundle,
// once: export com.apple.dock through cfprefsd (`defaults export
// com.apple.dock -`), add the tile when no tile for either entry bundle
// location is there yet (dock.go's pure transform), import the result back
// (`defaults import com.apple.dock -`) and restart the Dock. The
// preferences file is never written directly. An already-pinned Dock is
// left alone, including its running instance: no import, no restart.
func dockAdd() error {
	known, err := entryBundleFileURLs()
	if err != nil {
		return err
	}
	dst := entryBundleDst()
	if dst == "" {
		return errors.New("the home directory is unknown; cannot pin the app to the Dock")
	}
	appURL, err := entryBundleFileURL(dst)
	if err != nil {
		return err
	}
	doc, err := dockExport()
	if err != nil {
		return err
	}
	out, changed, err := dockPlistAddEntry(doc, appURL, known, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	if err := dockImport(out); err != nil {
		return err
	}
	return dockRestart()
}

// dockRemove takes a kids-shelf tile out of the Dock, the same way dockAdd
// works: export, transform, import, restart. A Dock without the tile is
// left alone.
func dockRemove() error {
	known, err := entryBundleFileURLs()
	if err != nil {
		return err
	}
	doc, err := dockExport()
	if err != nil {
		return err
	}
	out, changed, err := dockPlistRemoveEntry(doc, known)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	if err := dockImport(out); err != nil {
		return err
	}
	return dockRestart()
}

// entryBundleFileURL returns the file URL of a bundle path in the form the
// Dock writes: percent-encoded, with the trailing slash
// (file:///Applications/kids-shelf.app/).
func entryBundleFileURL(path string) (string, error) {
	if path == "" {
		return "", errors.New("the entry bundle path is unknown")
	}
	u := url.URL{Scheme: "file", Path: path}
	return u.String() + "/", nil
}

// entryBundleFileURLs returns both entry bundle locations as file URLs.
// Both count as "the entry bundle is pinned": an install that moved from
// ~/Applications to /Applications must not duplicate the Dock item.
func entryBundleFileURLs() ([]string, error) {
	var urls []string
	for _, c := range entryBundleCandidates() {
		if c == "" {
			continue
		}
		u, err := entryBundleFileURL(c)
		if err != nil {
			return nil, err
		}
		urls = append(urls, u)
	}
	return urls, nil
}

// dockExport reads the com.apple.dock preferences through cfprefsd as XML:
// `defaults export domain -` prints the whole domain to stdout. Modern
// macOS exports a binary plist, so a non-XML export is converted with
// plutil (stdin to stdout; nothing is written to the preferences file or
// anywhere else) before the pure transform sees it. The plist file under
// ~/Library/Preferences is never read or written directly.
func dockExport() ([]byte, error) {
	out, err := dockCommand(defaultsCmd, "export", "com.apple.dock", "-")
	if err != nil {
		return nil, fmt.Errorf("reading the Dock preferences (defaults export com.apple.dock): %v: %s", err, out)
	}
	if len(out) > 0 && out[0] == '<' {
		return out, nil // already XML
	}
	ctx, cancel := context.WithTimeout(context.Background(), dockCommandTimeout)
	defer cancel()
	cmd := proc.CommandContext(ctx, plutilCmd, "-convert", "xml1", "-o", "-", "-")
	cmd.Stdin = bytes.NewReader(out)
	xmlOut, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("converting the Dock preferences to XML (plutil -convert xml1): %v: %s", err, xmlOut)
	}
	return xmlOut, nil
}

// dockCommand runs a Dock tool through proc with the dock timeout and
// returns its combined output.
func dockCommand(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dockCommandTimeout)
	defer cancel()
	return proc.CommandContext(ctx, name, args...).CombinedOutput()
}

// dockImport writes the modified com.apple.dock preferences through
// cfprefsd: `defaults import domain -` reads XML from stdin.
func dockImport(doc []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), dockCommandTimeout)
	defer cancel()
	cmd := proc.CommandContext(ctx, defaultsCmd, "import", "com.apple.dock", "-")
	cmd.Stdin = bytes.NewReader(doc)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("writing the Dock preferences (defaults import com.apple.dock): %v: %s", err, out)
	}
	return nil
}

// dockRestart restarts the Dock so it loads the updated preferences. A
// restart failure is best effort too: the change is already written, and
// the Dock picks it up on its next restart on its own.
func dockRestart() error {
	if out, err := dockCommand(killallCmd, "Dock"); err != nil {
		return fmt.Errorf("restarting the Dock (killall Dock): %v: %s", err, out)
	}
	return nil
}
