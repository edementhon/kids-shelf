// Package main is the kids-shelf launcher. The same binary plays three
// roles, chosen by the install layout and the environment:
//
//   - Supervisor (the desktop icon): takes the install and single-instance
//     locks (install first), binds and keeps the public app and libraries
//     ports, runs the app build as a supervised child with
//     KS_SUPERVISED=1 and --listen/--libraries-listen on private loopback
//     ports, and reverse-proxies the public ports to them
//     (PLAN.md 1a "Launcher (supervisor)"). It writes instance.json once
//     the child's /healthz answers, opens the browser, restarts the child
//     on exit code 3 - re-resolving which version to run, so a staged
//     update is activated there - reruns the same build on fresh ports on
//     exit code 5 (bind failure), hands over to the remover on exit code
//     4, and forwards SIGINT/SIGTERM. Another launch that finds the lock
//     held joins the running instance instead.
//   - Supervised child: serves only, on the private ports it was given;
//     the updater (and later the other background workers) run here, never
//     in a joining launcher.
//   - In-process (no <install>/versions/ layout, or KS_NO_SUPERVISE=1):
//     the launcher serves directly on both public ports, holding the
//     locks in-process.
//
// Startup failures open a plain error file and the OS viewer (or a native
// dialog), never a silent exit.
//
// Release builds are made with
//
//	go build -ldflags "-X main.version=<tag> -H=windowsgui"
//
// On Windows, -H=windowsgui selects the GUI subsystem so the launcher runs
// without a console window; the native dialog is what makes startup
// failures visible on such builds.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/edementhon/kids-shelf/internal/app"
	"github.com/edementhon/kids-shelf/internal/firewall"
	installer "github.com/edementhon/kids-shelf/internal/install"
	"github.com/edementhon/kids-shelf/internal/launch"
	"github.com/edementhon/kids-shelf/internal/paths"
	"github.com/edementhon/kids-shelf/internal/proc"
	"github.com/edementhon/kids-shelf/internal/update"
	"github.com/edementhon/kids-shelf/internal/web"
)

// version is injected at build time with -ldflags "-X main.version=...";
// it defaults to "dev".
var version = "dev"

func main() {
	fs := flag.NewFlagSet("kids-shelf", flag.ExitOnError)
	fs.SetOutput(os.Stderr)
	dataDir := fs.String("data-dir", "", "data directory (default: the per-user kids-shelf data dir, see internal/paths)")
	port := fs.Int("port", 0, "first public port to try for the web server (default 8070); the launcher picks the actual port and the libraries origin takes the next free port")
	listenAddr := fs.String("listen", "", "loopback address for the app server when supervised (127.0.0.1:PORT); in-process runs pick their own public port with --port")
	librariesListen := fs.String("libraries-listen", "", "loopback address for the libraries origin when supervised (127.0.0.1:PORT)")
	noBrowser := fs.Bool("no-browser", false, "do not open the browser or the error file")
	showVersion := fs.Bool("version", false, "print the version and exit")
	remove := fs.Bool("remove", false, "remove kids-shelf from this computer (used by the parent page and by Settings > Apps)")
	removeInstall := fs.String("install", "", "with --remove: the install root to remove")
	removeData := fs.String("data", "", "with --remove: the data directory")
	deleteData := fs.Bool("delete-data", false, "with --remove: also delete the videos and settings")
	noStart := fs.Bool("no-start", false, "install only, without starting kids-shelf afterwards (servers whose service manager starts the installed launcher)")
	fs.Parse(os.Args[1:])
	installNoStart = *noStart
	if *remove {
		os.Exit(runRemove(*removeInstall, *removeData, *deleteData, os.Stderr))
	}
	os.Exit(run(*showVersion, *dataDir, *port, *noBrowser, *listenAddr, *librariesListen, os.Stdout, os.Stderr))
}

// run executes the CLI and returns the process exit code. It is separate
// from main so tests can drive it with injected output streams.
func run(showVersion bool, dataDir string, port int, noBrowser bool, listenAddr, librariesListen string, out, errOut io.Writer) int {
	if showVersion {
		fmt.Fprintf(out, "kids-shelf %s\n", version)
		return 0
	}
	dir, err := paths.DataDir(dataDir)
	if err != nil {
		return fail(err, "", noBrowser, errOut)
	}
	web.Version = version
	update.BuildVersion = version
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if os.Getenv("KS_SUPERVISED") == "1" {
		// Supervised child: serve only, workers run here.
		return runServing(ctx, dir, port, listenAddr, librariesListen, noBrowser, errOut)
	}

	exe, err := os.Executable()
	if err != nil {
		return fail(fmt.Errorf("locating the executable: %w", err), dir, noBrowser, errOut)
	}
	if code, handled := installerRole(exe, dir, noBrowser, errOut); handled {
		return code
	}
	install := filepath.Dir(exe)
	if _, verr := os.Stat(filepath.Join(install, "versions")); os.Getenv("KS_NO_SUPERVISE") == "1" || errors.Is(verr, os.ErrNotExist) {
		// No install layout: the launcher serves in-process on both public
		// ports itself.
		return runServing(ctx, dir, port, "", "", noBrowser, errOut)
	} else if verr != nil {
		return fail(fmt.Errorf("checking the install layout: %w", verr), dir, noBrowser, errOut)
	}
	return runSupervisor(ctx, dir, install, port, noBrowser, errOut)
}

// runServing starts the app half: the app (database, services, background
// workers) and its web routes. It runs only in the process that serves -
// the supervised child, whose supervisor already holds the locks, or the
// in-process launcher once it holds them - never in a joining launcher.
// Only the supervised child stages updates: in-process mode has no install
// layout and therefore no updates.
func runServing(ctx context.Context, dir string, port int, listenAddr, librariesListen string, noBrowser bool, errOut io.Writer) int {
	id, err := launch.NewID()
	if err != nil {
		return fail(fmt.Errorf("generating an instance id: %w", err), dir, noBrowser, errOut)
	}
	instID := id
	if v := os.Getenv("KS_INSTANCE_ID"); v != "" {
		instID = v
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// openApp opens the database and services, starts the workers, wires
	// the updater (when any) to the app's reservation book, and returns
	// the app and libraries muxes plus a close that stops the workers and
	// the store. onRemove, when set, backs the parent page's Remove
	// (supervised children only: in-process runs have no installation).
	openApp := func(ctx context.Context, updater *update.Manager, onRemove func(deleteData bool, mediaDir string) error) (http.Handler, http.Handler, func(), error) {
		a, err := app.Open(ctx, app.Config{DataDir: dir, Version: version})
		if err != nil {
			return nil, nil, nil, fmt.Errorf("opening the kids-shelf data: %w", err)
		}
		if err := a.Start(ctx); err != nil {
			a.Close()
			return nil, nil, nil, fmt.Errorf("starting kids-shelf: %w", err)
		}
		if updater != nil {
			// The updater reserves against the same persisted book as the
			// app's jobs: its stage and snapshot holds count against the
			// app's jobs and survive the restart into the launcher, which
			// re-checks and releases them at activation.
			updater.Reservations = a.Reservations()
		}
		deps := a.Deps()
		deps.Quit = cancel
		if updater != nil {
			deps.Updates = updater
		}
		// The proxy identity arrives through the supervisor's environment;
		// an in-process launcher leaves it empty (no proxy). The trial
		// state comes from the durable journal (app.Deps sets InTrial).
		// KS_PUBLIC_LIBRARIES_PORT exists in both shapes: the supervisor
		// sets it on the child, and the in-process launcher sets it before
		// opening the app.
		deps.ProxyToken = os.Getenv("KS_PROXY_TOKEN")
		deps.LibrariesPort = envInt("KS_PUBLIC_LIBRARIES_PORT")
		// Windows Defender Firewall keys its rule to the program that
		// listens on the LAN: the root launcher when supervised (KS_INSTALL
		// names its install root), this executable when in-process. No
		// other OS has a firewall the app manages, and Deps.Firewall stays
		// nil there.
		if runtime.GOOS == "windows" {
			program := ""
			if inst := os.Getenv("KS_INSTALL"); inst != "" {
				program = update.EntryPath(inst)
			} else if exe, eerr := os.Executable(); eerr == nil {
				program = exe
			}
			if program != "" {
				deps.Firewall = firewallAdapter{firewall.New(program)}
			}
		}
		if onRemove != nil {
			mediaDir := deps.MediaDir
			deps.Remove = func(deleteData bool) error { return onRemove(deleteData, mediaDir) }
		}
		srv := web.NewApp(instID, deps)
		return srv.Mux(), srv.LibrariesMux(), func() { a.Close() }, nil
	}

	if os.Getenv("KS_SUPERVISED") == "1" {
		// Lifeline: the supervisor's pipe is our stdin; EOF means the
		// supervisor died (for any reason, including kill -9), so shut
		// down within launch.LifelineGrace and exit.
		go func() {
			<-launch.LifelineEOF(os.Stdin)
			cancel()
		}()

		// "Restart to update" shuts the app down cleanly, then exits with
		// launch.ExitRestart so the supervisor activates the staged build.
		var restart, removing atomic.Bool
		updater := newUpdater(dir)
		updater.OnRestart = func() {
			restart.Store(true)
			cancel()
		}
		// Remove records the request outside the database (a rollback
		// restore cannot lose it), then shuts down and exits with
		// launch.ExitRemove so the supervisor hands over to the remover.
		onRemove := func(deleteData bool, mediaDir string) error {
			if err := installer.WriteRemoveRequest(dir, deleteData, mediaDir); err != nil {
				return err
			}
			removing.Store(true)
			cancel()
			return nil
		}
		mux, libsMux, closeApp, err := openApp(ctx, updater, onRemove)
		if err != nil {
			fmt.Fprintf(errOut, "kids-shelf: %v\n", err)
			return 1
		}
		updaterDone := make(chan struct{})
		go func() {
			updater.Run(ctx)
			close(updaterDone)
		}()
		// The supervisor holds the locks, owns instance.json and opens the
		// browser; this process only binds the private listeners it was
		// given. A bind failure exits with launch.ExitBindFailed so the
		// supervisor reruns this same build on fresh ports.
		serveErr := launch.ServeChild(ctx, listenAddr, librariesListen, mux, libsMux)
		// Shutdown: cancel and wait for the updater goroutine (so a
		// staging run never dies mid-write), then close the app.
		cancel()
		<-updaterDone
		closeApp()
		if serveErr != nil {
			if errors.Is(serveErr, launch.ErrBindFailed) {
				return launch.ExitBindFailed
			}
			fmt.Fprintf(errOut, "kids-shelf: %v\n", serveErr)
			return 1
		}
		if removing.Load() {
			return launch.ExitRemove
		}
		if restart.Load() {
			return launch.ExitRestart
		}
		return 0
	}
	if err := launch.Run(ctx, launch.Options{
		DataDir: dir,
		Port:    port,
		ID:      id,
		OpenHandler: func(ctx context.Context) (http.Handler, http.Handler, func(), error) {
			return openApp(ctx, nil, nil)
		},
		NoBrowser: noBrowser,
	}); err != nil {
		return fail(err, dir, noBrowser, errOut)
	}
	return 0
}

// runSupervisor takes the install and instance locks and supervises the
// app child on the public ports: it resolves which version to run
// (activating a staged update), starts the child, and watches the trial
// health policy through internal/launch.
func runSupervisor(ctx context.Context, dir, install string, port int, noBrowser bool, errOut io.Writer) int {
	entryPath := update.EntryPath(install)
	dbPath := update.DatabasePath(dir)
	var curVersion string

	resolve := func() (string, error) {
		// Activate snapshots or restores the database. A restore refuses
		// with a plain error while any process still has the database open
		// (the DB ownership lock), so a rollback can never run under a
		// child of a previous supervisor that is still serving.
		v, err := update.Activate(install, dir, dbPath)
		if err != nil {
			return "", err
		}
		curVersion = v
		if v == "" {
			return entryPath, nil
		}
		return update.VersionBinary(install, v), nil
	}
	err := launch.Run(ctx, launch.Options{
		DataDir:    dir,
		Port:       port,
		InstallDir: install,
		NoBrowser:  noBrowser,
		Child:      entryPath,
		Resolve:    resolve,
		Env:        []string{"KS_INSTALL=" + install, "KS_UPDATE_BASEURL=" + os.Getenv("KS_UPDATE_BASEURL")},
		Args:       []string{"--data-dir", dir},
		// The parent page's Remove: the child recorded the request and
		// exited; hand over to a temp copy of this binary, which waits for
		// both locks and empties the installation (PLAN 1a).
		OnRemove: func() error {
			return installer.HandOff(installer.RemoveOptions{InstallDir: install, DataDir: dir})
		},
		Trials: launch.TrialPolicy{
			InTrial: func() bool {
				st, rerr := update.ReadState(dir)
				return rerr == nil && st.Trying != nil && st.Trying.Version == curVersion
			},
			Healthy: func() error {
				// The journal write must succeed before the update counts
				// as confirmed; the supervisor retries with backoff and
				// keeps the trial open until it does.
				return update.Confirm(dir, curVersion)
			},
			Failed: func(reason error) error {
				log.Printf("kids-shelf: the update to %s failed (%v); switching back", curVersion, reason)
				return update.Reject(dir, curVersion)
			},
		},
	})
	if err != nil {
		return fail(err, dir, noBrowser, errOut)
	}
	return 0
}

// envInt parses v as a decimal port; unset or unparsable values read as 0.
func envInt(name string) int {
	v := os.Getenv(name)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}

// newUpdater builds the app-side update manager rooted at the data
// directory. KS_UPDATE_BASEURL overrides the release endpoint (tests and
// self-hosted mirrors).
func newUpdater(dataDir string) *update.Manager {
	m := &update.Manager{DataDir: dataDir, Logf: log.Printf}
	if u := os.Getenv("KS_UPDATE_BASEURL"); u != "" {
		m.BaseURL = u
	}
	return m
}

// fail reports a startup error the way a non-technical user can see it:
// printed to stderr, written to <data>/last-error.txt, and (unless
// --no-browser) opened in the OS default viewer. When the data directory
// could not be resolved, last-error.txt cannot be written, or the viewer
// cannot open the file, the error is also shown in a native dialog so it
// stays visible without a console (Windows GUI subsystem builds).
func fail(err error, dir string, noBrowser bool, errOut io.Writer) int {
	fmt.Fprintf(errOut, "kids-shelf: %v\n", err)
	msg := fmt.Sprintf("kids-shelf could not start.\n\n%v\n\nStart kids-shelf again. If the problem keeps happening, ask for help and include the text above.\n", err)
	if dir == "" {
		showErrorDialog(noBrowser, msg, errOut)
		return 1
	}
	path := filepath.Join(dir, "last-error.txt")
	if werr := os.WriteFile(path, []byte(msg), 0o600); werr != nil {
		fmt.Fprintf(errOut, "kids-shelf: could not write %s: %v\n", path, werr)
		showErrorDialog(noBrowser, msg, errOut)
		return 1
	}
	if !noBrowser {
		if oerr := launch.OpenFile(path); oerr != nil {
			fmt.Fprintf(errOut, "kids-shelf: could not open %s: %v\n", path, oerr)
			showErrorDialog(noBrowser, msg, errOut)
		}
	}
	return 1
}

// showErrorDialog shows msg in a native error dialog unless --no-browser
// suppresses every UI, logging a dialog failure to stderr. The error text
// already reached stderr through fail, so a missing dialog tool never
// swallows the message.
func showErrorDialog(noBrowser bool, msg string, errOut io.Writer) {
	if noBrowser {
		return
	}
	if derr := launch.ShowError("kids-shelf", msg); derr != nil {
		fmt.Fprintf(errOut, "kids-shelf: could not show the error dialog: %v\n", derr)
	}
}

// installNoStart is the --no-start flag: the installer role installs and
// exits without starting the root launcher, for servers where a service
// manager (systemd) runs <install>/kids-shelf itself.
var installNoStart bool

// installerRole runs the installer, or the macOS entry bundle's start-or-
// join, when this launch is not the root launcher of a valid installation
// (PLAN 1a "Launcher (supervisor)"). It reports handled=false for the root
// launcher, supervised children and dev builds: a "dev" version cannot
// enter the versioned layout, and running from a checkout keeps serving
// in-process.
func installerRole(exe, dir string, noBrowser bool, errOut io.Writer) (code int, handled bool) {
	if version == "dev" || os.Getenv("KS_NO_SUPERVISE") == "1" || installer.IsInstalled(exe, dir) {
		return 0, false
	}
	if runtime.GOOS == "darwin" && isEntryBundle(exe) {
		if err := installer.EntryMain(); err != nil {
			return installerFailure(err, dir, noBrowser, errOut), true
		}
		return 0, true
	}
	if err := installer.Install(installer.InstallOptions{DataDir: dir, Version: version, Exe: exe, NoStart: installNoStart}); err != nil {
		return installerFailure(err, dir, noBrowser, errOut), true
	}
	return 0, true
}

// installerFailure shows an installer refusal in plain words. The expected
// refusals (already running, older copy, launcher still in use, update
// still settling) carry their message as the error text; anything else is
// a startup error.
func installerFailure(err error, dir string, noBrowser bool, errOut io.Writer) int {
	switch {
	case errors.Is(err, installer.ErrBusy), errors.Is(err, installer.ErrOlder),
		errors.Is(err, installer.ErrLauncherLocked), errors.Is(err, installer.ErrTrialPending):
		fmt.Fprintf(errOut, "kids-shelf: %v\n", err)
		// The dialog comes first and the page opens once the parent has
		// read it: opened the other way round, the browser came up over the
		// dialog in the Windows VM. PLAN 1a: on Busy the running
		// kids-shelf's parent page opens, where the dialog says to quit
		// it; on Older the newer installed copy starts (or is joined).
		showErrorDialog(noBrowser, err.Error(), errOut)
		if !noBrowser {
			var oerr error
			switch {
			case errors.Is(err, installer.ErrBusy):
				oerr = launch.OpenRunning(dir)
			case errors.Is(err, installer.ErrOlder) && !installNoStart:
				// --no-start: a service manager owns the installed copy.
				oerr = installer.StartInstalled(dir)
			}
			if oerr != nil {
				fmt.Fprintf(errOut, "kids-shelf: could not open the installed kids-shelf: %v\n", oerr)
				if errors.Is(err, installer.ErrBusy) {
					showErrorDialog(noBrowser, msgNotStarted, errOut)
				}
			}
		}
		return 1
	}
	return fail(err, dir, noBrowser, errOut)
}

// msgNotStarted follows a "quit it from the parent page" dialog when that
// page could not be opened: the running kids-shelf had not finished
// starting within launch.OpenRunning's wait.
const msgNotStarted = "kids-shelf has not finished starting yet. In a minute, open it from the kids-shelf icon and use Quit on the parent page."

// isEntryBundle reports whether exe is the executable inside an installed
// macOS entry bundle: /Applications/kids-shelf.app or
// ~/Applications/kids-shelf.app. A machine that installed before
// /Applications became the default keeps its ~/Applications copy, so both
// locations count.
func isEntryBundle(exe string) bool {
	for _, bundle := range installer.EntryBundles() {
		if bundle != "" && filepath.Clean(exe) == filepath.Join(bundle, "Contents", "MacOS", "kids-shelf") {
			return true
		}
	}
	return false
}

// runRemove is the --remove role: the parent page's Remove (the supervisor
// starts it from a temp copy) or Settings > Apps > Uninstall (Windows starts
// it from the install root, where the running file cannot be deleted, so it
// hands over to a temp copy first). The remover waits for both locks, so a
// running kids-shelf is told to quit instead of being removed under it.
func runRemove(installDir, dataDir string, deleteData bool, errOut io.Writer) int {
	if installDir == "" || dataDir == "" {
		fmt.Fprintln(errOut, "kids-shelf: --remove needs --install and --data")
		return 2
	}
	o := installer.RemoveOptions{InstallDir: installDir, DataDir: dataDir, DeleteData: deleteData}
	// The Windows Firewall probe and its UAC prompt run with a bounded
	// context, so a hung PowerShell can never tie up the removal.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if exe, err := os.Executable(); err == nil && within(exe, installDir) {
		// Started by Settings > Apps > Uninstall. A running kids-shelf
		// holds the instance lock: say so now and open its parent page,
		// instead of a temp copy waiting a silent minute for locks that
		// will not be released (Windows VM run 2026-10-27). The parent
		// page's Remove never comes through here: its supervisor hands
		// off directly, and that remover's wait covers the shutdown.
		lock, lerr := launch.LockInstance(dataDir, 0)
		if errors.Is(lerr, launch.ErrBusy) {
			// Dialog first, then the page (the browser covered the
			// dialog when it opened first).
			showErrorDialog(false, "kids-shelf is still running. Quit it from the parent page, then remove it again.", errOut)
			if oerr := launch.OpenRunning(dataDir); oerr != nil {
				fmt.Fprintf(errOut, "kids-shelf: could not open the running kids-shelf: %v\n", oerr)
				showErrorDialog(false, msgNotStarted, errOut)
			}
			return 1
		}
		if lerr == nil {
			lock.Close()
		}
		if err := installer.HandOff(o); err != nil {
			return fail(err, "", false, errOut)
		}
		return 0
	}
	// This process runs from HandOff's temp copy. Unix lets a running file
	// be unlinked, so the copy removes itself; Windows cannot, so a
	// detached hidden cmd.exe waits a few seconds and deletes the copy
	// after this process has exited. The defer runs on every exit path of
	// the temp-copy flow, just before the process ends.
	if exe, err := os.Executable(); err == nil && strings.HasPrefix(filepath.Base(exe), "kids-shelf-remove-") {
		if runtime.GOOS == "windows" {
			defer removeTempCopy(exe, errOut)
		} else {
			defer os.Remove(exe)
		}
	}
	if err := installer.Remove(o); err != nil {
		if errors.Is(err, installer.ErrBusy) {
			fmt.Fprintf(errOut, "kids-shelf: %v\n", err)
			showErrorDialog(false, "kids-shelf is still running. Quit it from the parent page, then remove it again.", errOut)
			return 1
		}
		return fail(err, "", false, errOut)
	}
	// The installation is gone, and so should the Windows Firewall rules
	// that pointed at its launcher be (Eric chose a clean machine over one
	// fewer admin prompt). The probe avoids a pointless prompt on a computer
	// that never allowed kids-shelf, and a declined or failed removal is
	// only logged: the files are already gone. firewall.New returns a
	// controller on every OS, so the Windows check is explicit.
	if runtime.GOOS == "windows" {
		if c := firewall.New(update.EntryPath(installDir)); c.RulesExist(ctx) {
			if err := c.Remove(ctx); err != nil {
				fmt.Fprintf(errOut, "kids-shelf: could not remove the Windows Firewall rule for kids-shelf: %v\n", err)
			}
		}
	}
	return 0
}

// removeTempCopy starts a detached, hidden cmd.exe that waits a few
// seconds (a ping) and then deletes the running remover's temp copy,
// which Windows cannot delete while this process still runs. The wait
// covers this process's exit; a copy that survives (the start failed, an
// exceptional machine) is left for the system's temp cleanup and never
// fails the removal.
func removeTempCopy(path string, errOut io.Writer) {
	cmd := proc.Command("cmd.exe")
	setRawCmdLine(cmd, `cmd.exe /c ping -n 4 127.0.0.1 >nul & del /f /q "`+path+`"`)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(errOut, "kids-shelf: could not start the remover-copy cleanup: %v\n", err)
	}
}

// within reports whether path lies inside dir.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// firewallAdapter presents a firewall.Control as web.FirewallControl: the
// firewall package keeps its own Status type because it cannot import web
// (web imports it for the declined-permission error).
type firewallAdapter struct{ c firewall.Control }

func (f firewallAdapter) Status(ctx context.Context) web.FirewallStatus {
	s := f.c.Status(ctx)
	return web.FirewallStatus{Blocked: s.Blocked, Unknown: s.Unknown, Detail: s.Detail}
}

func (f firewallAdapter) Allow(ctx context.Context) error { return f.c.Allow(ctx) }
