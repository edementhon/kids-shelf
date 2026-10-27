// Package app assembles the kids-shelf services into one runnable
// application: it opens the per-user database, applies every package's
// migrations, recovers interrupted jobs, wires the services together and
// starts the background workers. cmd/kids-shelf calls Open, builds the web
// server over Deps, sets the two fields it owns (Updates, Quit) and drives
// Start/Close.
package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/edementhon/kids-shelf/internal/budget"
	"github.com/edementhon/kids-shelf/internal/bundle"
	"github.com/edementhon/kids-shelf/internal/disk"
	"github.com/edementhon/kids-shelf/internal/ingest"
	"github.com/edementhon/kids-shelf/internal/jobs"
	"github.com/edementhon/kids-shelf/internal/kiwix"
	"github.com/edementhon/kids-shelf/internal/library"
	"github.com/edementhon/kids-shelf/internal/paths"
	"github.com/edementhon/kids-shelf/internal/practice"
	"github.com/edementhon/kids-shelf/internal/store"
	"github.com/edementhon/kids-shelf/internal/tools"
	"github.com/edementhon/kids-shelf/internal/update"
	"github.com/edementhon/kids-shelf/internal/web"
	"github.com/edementhon/kids-shelf/internal/youtube"
)

// dbName is the database file inside the data directory.
const dbName = "kids-shelf.db"

// Config selects the directories the app runs from. Empty directory fields
// resolve to the OS defaults (internal/paths); HelpersDir falls back to
// $KS_HELPERS_DIR, then to the versioned bundle extracted into the data dir.
type Config struct {
	DataDir    string // per-user data directory (database, jobs, clips)
	MediaDir   string // media root (converted episodes, Kiwix libraries)
	HelpersDir string // helper binaries (ffmpeg, ffprobe, qjs)
	Version    string // app version, for logs
	// ImportOnly starts only the ingest worker (see workersOf): for a
	// one-off data import run before the app serves.
	ImportOnly bool
}

// allMigrations are every package's schema migrations, applied together at
// Open. store.Migrate is idempotent per (owner, version), so a relaunch
// applies only the pending ones and Open is safe to run on a live database.
var allMigrations = func() []store.Migration {
	var ms []store.Migration
	ms = append(ms, jobs.Migrations...)
	ms = append(ms, budget.Migrations...)
	ms = append(ms, library.Migrations...)
	ms = append(ms, practice.Migrations()...)
	ms = append(ms, kiwix.Migrations...)
	ms = append(ms, update.Migrations...)
	return ms
}()

// App is the assembled application: the services plus their workers.
type App struct {
	cfg Config

	store        *store.Store
	queue        *jobs.Queue
	reservations *disk.Reservations
	budget       *budget.Budget
	library      *library.Library
	ingestQ      *ingest.Queue
	worker       *ingest.Worker
	helpers      *tools.Dir
	ytdlp        *youtube.Updater
	youtube      *youtube.Manager
	catalog      *kiwix.Catalog
	kiwix        *kiwix.Manager
	kiwixSrv     *kiwix.Server
	prac         *practice.Service

	dataDir  string
	mediaDir string
	clipsDir string

	ctx     context.Context
	mu      sync.Mutex
	started bool
	closed  bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	// helpersProblem is the plain-words message the parent page shows when
	// the helper programs are missing, broken or could not be set up; ""
	// means all is well. The background self-test (setupHelpers) replaces
	// it once its probes finish, so reads take helpersMu.
	helpersMu      sync.Mutex
	helpersProblem string
}

// helperHoldID is the disk reservation id held while the bundled helpers
// are extracted into the data dir; it lives in the reservations table, not
// among work jobs.
const helperHoldID = "helpers"

// setupHelpers resolves the helper binaries the app runs: an explicit
// config directory, then $KS_HELPERS_DIR, then the versioned bundle
// extracted into the data dir (tools.Locate). Only the bundle path writes
// files, so only it first holds the helpers' needed size against the
// free-space floor the jobs reserve against, releasing the hold as soon as
// the extraction lands. The hold covers only the bytes extraction will
// actually write: a helper already on disk with the manifest's SHA-256 is
// reused, so a restart on a nearly full disk keeps working when the cached
// set is intact. It never fails: a failure leaves helpers nil and records
// a plain-words problem for the parent page, and the workers keep running
// - ingest jobs simply fail as they reach the helpers.
func (a *App) setupHelpers(ctx context.Context, dataDir string, reservations *disk.Reservations, bundled fs.FS, m tools.Manifest) *tools.Dir {
	extract := a.cfg.HelpersDir == "" && os.Getenv(tools.EnvHelpersDir) == ""
	if extract {
		need, err := helpersToWrite(bundled, m, dataDir)
		if err != nil {
			logf("helpers unavailable (bundled helpers incomplete): %v", err)
			a.setHelpersProblem("Video conversion is unavailable: the helper programs could not be set up.")
			return nil
		}
		if need > 0 {
			if err := reservations.Hold(helperHoldID, need, dataDir); err != nil {
				logf("helpers unavailable (could not reserve space for them): %v", err)
				var noSpace *disk.ErrNoSpace
				if errors.As(err, &noSpace) {
					a.setHelpersProblem("Video conversion is unavailable: the helper programs could not be set up (not enough disk space).")
				} else {
					a.setHelpersProblem("Video conversion is unavailable: the helper programs could not be set up.")
				}
				return nil
			}
			defer func() {
				if err := reservations.Release(helperHoldID); err != nil {
					logf("helpers: release extraction reservation: %v", err)
				}
			}()
		}
	}
	helpers, err := tools.Locate(a.cfg.HelpersDir, bundled, m, dataDir)
	if err != nil {
		// No helper directory configured and none bundled (a dev box):
		// keep the app usable, ingest simply cannot convert until the
		// parent points the dashboard at helpers.
		logf("helpers unavailable (video conversion needs them): %v", err)
		a.setHelpersProblem("Video conversion is unavailable: the helper programs could not be set up.")
		return nil
	}
	// The binaries are in place; probe each once in the background (every
	// probe is timeout-bounded) and record the plain-words result.
	go func() {
		a.setHelpersProblem(summarizeSelfTest(helpers.SelfTest(ctx)))
	}()
	return helpers
}

// helpersToWrite sums the bundle bytes extraction will actually write into
// <dataDir>/helpers/<version>: a file already on disk with the manifest's
// SHA-256 is left in place (tools.Extract skips it), so only the missing
// or mismatched helpers count against the free-space reservation.
func helpersToWrite(fsys fs.FS, m tools.Manifest, dataDir string) (uint64, error) {
	dir := filepath.Join(dataDir, "helpers", m.Version)
	var total uint64
	for _, f := range m.Files {
		valid, err := helperMatches(filepath.Join(dir, filepath.FromSlash(f.Name)), f.SHA256)
		if err != nil {
			return 0, err
		}
		if valid {
			continue
		}
		fi, err := fs.Stat(fsys, f.Name)
		if err != nil {
			return 0, err
		}
		total += uint64(fi.Size())
	}
	return total, nil
}

// helperMatches reports whether the extracted helper at path already has
// the manifest's SHA-256; a missing file counts as needing extraction.
func helperMatches(path, wantHex string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()
	want, err := hex.DecodeString(wantHex)
	if err != nil || len(want) != sha256.Size {
		return false, fmt.Errorf("helpers: %s: manifest sha256 must be %d hex digits", path, 2*sha256.Size)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return bytes.Equal(h.Sum(nil), want), nil
}

// setHelpersProblem records the plain-words message the parent page shows
// about the helper programs; "" means all is well.
func (a *App) setHelpersProblem(msg string) {
	a.helpersMu.Lock()
	a.helpersProblem = msg
	a.helpersMu.Unlock()
}

// helpersProblemFunc returns the current plain-words message for the
// parent page; "" means all is well.
func (a *App) helpersProblemFunc() string {
	a.helpersMu.Lock()
	defer a.helpersMu.Unlock()
	return a.helpersProblem
}

// summarizeSelfTest turns a self-test result into that message: nothing
// when every helper works, otherwise what is unavailable and why. A
// failing kiwix-serve only costs the libraries, so it is reported
// separately from the video helpers (ffmpeg, ffprobe, qjs).
func summarizeSelfTest(res tools.SelfTestResult) string {
	var video []string
	if !res.FFmpeg {
		video = append(video, "ffmpeg")
	}
	if !res.FFprobe {
		video = append(video, "ffprobe")
	}
	if !res.Qjs {
		video = append(video, "qjs")
	}
	var msg string
	if len(video) > 0 {
		msg = "Video conversion is unavailable: " + joinNames(video) + " did not start."
	}
	if !res.KiwixServe {
		if msg != "" {
			msg += " "
		}
		msg += "Libraries are unavailable: kiwix-serve did not start."
	}
	return msg
}

// joinNames lists names in plain words for parents: "ffmpeg", "ffmpeg and
// ffprobe", "ffmpeg, ffprobe and qjs".
func joinNames(names []string) string {
	switch len(names) {
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}

// Open opens the store at <data>/kids-shelf.db (creating the data dir),
// applies every package's migrations, requeues jobs a crash left running,
// and wires the services: disk reservations, budget, library, the ingest
// queue and worker, the helper directory, the yt-dlp updater and YouTube
// manager, the Kiwix catalog, manager and kiwix-serve supervisor, and the
// practice service. The helpers may be missing on a dev box (no bundle
// embedded) or fail their reservation on a full disk; the app still opens
// and the workers run, and ingest jobs fail as they reach the helpers.
func Open(ctx context.Context, cfg Config) (*App, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	a := &App{cfg: cfg}

	dataDir, err := paths.DataDir(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("app: data dir: %w", err)
	}
	mediaDir := paths.DefaultMediaDir(dataDir, cfg.MediaDir)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("app: create data dir: %w", err)
	}

	st, err := store.Open(filepath.Join(dataDir, dbName))
	if err != nil {
		return nil, fmt.Errorf("app: open store: %w", err)
	}
	// Every later failure closes the store before returning.
	fail := func(step string, err error) (*App, error) {
		st.Close()
		return nil, fmt.Errorf("app: %s: %w", step, err)
	}
	if err := st.Migrate(allMigrations...); err != nil {
		return fail("migrate", err)
	}

	queue, err := jobs.New(st)
	if err != nil {
		return fail("jobs", err)
	}
	if _, err := queue.Recover(); err != nil {
		return fail("recover jobs", err)
	}

	b, err := budget.New(st)
	if err != nil {
		return fail("budget", err)
	}
	lib, err := library.New(st)
	if err != nil {
		return fail("library", err)
	}
	iq := ingest.NewQueue(queue)
	reservations := disk.NewReservations(queue)
	helpers := a.setupHelpers(ctx, dataDir, reservations, bundle.FS(), bundle.Manifest())
	ingestWorker := ingest.NewWorker(queue, helpers, lib, mediaDir)

	updater := youtube.NewUpdater(dataDir, "")
	var qjsPath, ffmpegDir string
	if helpers != nil {
		qjsPath = helpers.Qjs
		ffmpegDir = filepath.Dir(helpers.FFmpeg)
	}
	yt := youtube.NewManager(queue, iq, &youtube.PointerHelpers{
		DataDir:        dataDir,
		QuickJSPath:    qjsPath,
		FFmpegLocation: ffmpegDir,
	}, updater, dataDir)

	catalog := kiwix.NewCatalog("", nil)
	kiwixMgr, err := kiwix.NewManager(st, queue, disk.NewReservations(queue), filepath.Join(mediaDir, "libraries"))
	if err != nil {
		return fail("kiwix", err)
	}

	// The kiwix-serve supervisor serves the installed ZIMs under /kiwix/.
	// It is wired only when the helper binary is present; otherwise (or
	// with no ZIMs yet) Backend stays empty and the /kiwix/ routes answer
	// 503 until a library change wakes it. Installed.File is the name
	// inside the library directory; kiwix-serve needs on-disk paths, so
	// the books closure resolves them.
	var kiwixSrv *kiwix.Server
	if helpers != nil {
		if _, err := os.Stat(helpers.KiwixServe); err == nil {
			libDir := filepath.Join(mediaDir, "libraries")
			kiwixSrv = kiwix.NewServer(helpers.KiwixServe, func() ([]kiwix.Installed, error) {
				books, err := kiwixMgr.Installed()
				if err != nil {
					return nil, err
				}
				for i := range books {
					books[i].File = filepath.Join(libDir, books[i].File)
				}
				return books, nil
			})
			// Every publish or removal restarts the server with the new
			// library (Sync is non-blocking and coalesces bursts).
			kiwixMgr.OnChange = kiwixSrv.Sync
			// Removing a ZIM stops the server first (Pause) so the file is
			// not held open while it is deleted; Remove resumes it after.
			kiwixMgr.Quiesce = kiwixSrv.Pause
		}
	}

	clipsDir := filepath.Join(dataDir, "spelling-clips")
	if err := practice.InstallClips(clipsDir); err != nil {
		// Missing clips only mean the spelling page falls back to the
		// browser's speech, so this never stops the app.
		log.Printf("app: spelling clips: %v", err)
	}
	pr := practice.New(st.DB(), b, practice.Options{ClipsDir: clipsDir})

	a.store = st
	a.queue = queue
	a.reservations = reservations
	a.budget = b
	a.library = lib
	a.ingestQ = iq
	a.worker = ingestWorker
	a.helpers = helpers
	a.ytdlp = updater
	a.youtube = yt
	a.catalog = catalog
	a.kiwix = kiwixMgr
	a.kiwixSrv = kiwixSrv
	a.prac = pr
	a.dataDir = dataDir
	a.mediaDir = mediaDir
	a.clipsDir = clipsDir

	logf("kids-shelf %s ready (data %s, media %s)", cfg.Version, dataDir, mediaDir)
	return a, nil
}

// Reservations returns the app's persistent disk reservation book: the
// jobs-queue-backed accounting every worker reserves against. cmd/kids-shelf
// hands it to the update manager too, so the update's stage and snapshot
// holds count against the same floor as the app's jobs and survive the
// restart into the launcher.
func (a *App) Reservations() *disk.Reservations {
	return a.reservations
}

// Deps returns the web dependencies with every service filled. Updates and
// Quit stay nil: cmd/kids-shelf owns the update manager and the dashboard's
// Quit action.
func (a *App) Deps() web.Deps {
	d := web.Deps{
		Budget:   a.budget,
		Library:  a.library,
		Ingest:   a.ingestQ,
		YouTube:  a.youtube,
		Catalog:  a.catalog,
		Kiwix:    a.kiwix,
		Practice: a.prac,
		MediaDir: a.mediaDir,
		ClipsDir: a.clipsDir,
		DataDir:  a.dataDir,
		Now:      time.Now,
	}
	// A nil *kiwix.Server assigned to the interface would make a non-nil
	// interface and the /kiwix/ routes would treat libraries as available
	// while the server field stays nil inside; only hand it over when it
	// exists.
	if a.kiwixSrv != nil {
		d.KiwixServer = a.kiwixSrv
	}
	// HelpersProblem is the parent page's plain-words view of the helper
	// programs, recorded by setupHelpers from the background self-test.
	d.HelpersProblem = a.helpersProblemFunc
	// InTrial reads the durable update journal: a Trying entry means the
	// supervisor is still health-checking an update, and the web server
	// must refuse Quit and Remove until it settles (the expiration timer
	// the supervisor used to pass along was too early for slow startups).
	// A journal that cannot be read counts as no trial; the supervisor
	// still guards the trial itself, this only keeps the parent page
	// honest.
	d.InTrial = func() bool {
		st, err := update.ReadState(a.dataDir)
		return err == nil && st.Trying != nil
	}
	return d
}

// Start launches the background workers (internal/app/workers.go) and the
// kiwix-serve supervisor under a context derived from ctx, so canceling ctx
// or calling Close stops them. It returns an error if called twice.
func (a *App) Start(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.started {
		return errors.New("app: already started")
	}
	a.started = true
	a.ctx, a.cancel = context.WithCancel(ctx)
	a.wg.Add(len(workersOf(a)))
	if a.kiwixSrv != nil {
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			a.kiwixSrv.Run(a.ctx)
		}()
	}
	for _, w := range workersOf(a) {
		go a.runWorker(w)
	}
	return nil
}

// Close cancels the app context, waits for every worker and the kiwix-serve
// supervisor to exit and then closes the store. It is idempotent (the
// dashboard Quit path may call it through main's cleanup); call it only from
// outside the workers, since it blocks until they all finish.
func (a *App) Close() error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	cancel := a.cancel
	a.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	a.wg.Wait()
	if a.store != nil {
		return a.store.Close()
	}
	return nil
}

// logf writes one line to the standard logger with the app prefix.
func logf(format string, args ...any) {
	log.Printf("app: "+format, args...)
}
