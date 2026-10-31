package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/edementhon/kids-shelf/internal/disk"
)

// DefaultBaseURL is the release metadata endpoint the updater checks.
const DefaultBaseURL = "https://api.github.com/repos/edementhon/kids-shelf/releases/latest"

// Size limits for a staged release: the compressed asset download and the
// extracted binary are both capped at 300 MB.
const (
	MaxDownloadBytes  = 300 << 20
	MaxExtractedBytes = 300 << 20
)

const (
	manifestMaxBytes = 4 << 20
	sigMaxBytes      = 4 << 10
)

// PublicKeyHex is the hex ed25519 public key that verifies SHA256SUMS.sig.
// Releases are signed with its private half; the binary is built with
//
//	go build -ldflags "-X github.com/edementhon/kids-shelf/internal/update.PublicKeyHex=..."
//
// An empty value turns updates off (the dashboard shows that).
var PublicKeyHex string

// publicKeyMu guards PublicKeyHex: the app's background updater goroutine
// reads it while the end-to-end tests swap per-test keys.
var publicKeyMu sync.RWMutex

// PublicKey returns the compiled-in verification key hex ("" = updates off).
func PublicKey() string {
	publicKeyMu.RLock()
	defer publicKeyMu.RUnlock()
	return PublicKeyHex
}

// SetPublicKey replaces the verification key; tests use it to install the
// key matching their fake release server.
func SetPublicKey(hex string) {
	publicKeyMu.Lock()
	defer publicKeyMu.Unlock()
	PublicKeyHex = hex
}

// Sentinel errors that map to plain-words dashboard messages via
// statusMessage.
var (
	errBadSignature    = errors.New("update: the release signature does not verify")
	errBadChecksum     = errors.New("update: the download does not match its checksum")
	errTooLarge        = fmt.Errorf("update: the release asset is larger than the %d MB limit", MaxDownloadBytes>>20)
	errTooMuchData     = fmt.Errorf("update: the release contains more than %d MB of data", MaxExtractedBytes>>20)
	errNoSignedVersion = errors.New("update: SHA256SUMS does not declare the version it was signed for")
	errVersionMismatch = errors.New("update: the release's signed version does not match its tag")
)

// BuildVersion is the version of the running build, injected from the
// launcher (cmd/kids-shelf sets it from its ldflags-injected version). On
// a fresh install it seeds the high-water mark, so a signed release older
// than the running build is never accepted.
var BuildVersion string

// Status is what the dashboard shows about the updater.
type Status struct {
	State   string `json:"state"`   // off | idle | checking | staged | failed
	Message string `json:"message"` // plain words
	Staged  string `json:"staged"`  // staged version, when staged
	// trial marks a status recorded while the launcher was health-checking
	// an update: Status re-derives it from the journal, because the
	// launcher confirms or rolls back the trial without telling the app.
	trial bool
}

// Status states.
const (
	StateOff      = "off"
	StateIdle     = "idle"
	StateChecking = "checking"
	StateStaged   = "staged"
	StateFailed   = "failed"
)

// Manager runs the app side of the update contract: check, verify, stage.
// The launcher side (Activate, Confirm, Reject) is separate and runs only
// under the instance lock.
type Manager struct {
	DataDir string // data directory: journal
	Install string // install root holding versions/ (defaults to InstallDir)
	BaseURL string // release metadata endpoint (defaults to DefaultBaseURL)
	Client  *http.Client
	// Free measures free bytes on a directory; when either the install or
	// the data directory falls below Floor, nothing is downloaded.
	Free  func(path string) (uint64, error)
	Floor uint64
	// CheckEvery is the interval between checks; 0 means 24 h.
	CheckEvery time.Duration
	Logf       func(format string, args ...any)
	// OnRestart, when set, is called by RestartToUpdate instead of
	// os.Exit(3), so the caller can shut the app down cleanly and then
	// exit with launch.ExitRestart.
	OnRestart func()
	// Reservations is the app's shared disk reservation book (internal/disk
	// over the jobs queue), so the update's needs count against the same
	// floor as the app's jobs; when nil, a single-process fallback that
	// obeys the same Free and Floor is used.
	Reservations *disk.Reservations
	// Tidy, when set, runs after Prune whenever the running build is the
	// confirmed current version: the app removes the helper sets this
	// build does not use.
	Tidy func()
	// SettleEvery is how often Run looks for the running build's trial to
	// end before tidying; 0 means 15 s.
	SettleEvery time.Duration

	mu     sync.Mutex
	status Status
}

func (m *Manager) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}

func (m *Manager) install() string {
	if m.Install != "" {
		return m.Install
	}
	dir, err := InstallDir()
	if err != nil {
		return ""
	}
	return dir
}

func (m *Manager) baseURL() string {
	if m.BaseURL != "" {
		return m.BaseURL
	}
	return DefaultBaseURL
}

func (m *Manager) client() *http.Client {
	if m.Client != nil {
		return m.Client
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

func (m *Manager) floor() uint64 {
	if m.Floor != 0 {
		return m.Floor
	}
	return disk.DefaultFloor
}

func (m *Manager) free(path string) (uint64, error) {
	if m.Free != nil {
		return m.Free(path)
	}
	return disk.Free(path)
}

// Run checks at once and then every CheckEvery (24 h by default) until ctx
// is done. A failed check is logged and retried on the next tick; the
// server keeps running either way. Once the running build is the confirmed
// version (its trial, if any, has ended) it tidies up after earlier
// updates, and again before every later check.
func (m *Manager) Run(ctx context.Context) {
	every := m.CheckEvery
	if every == 0 {
		every = 24 * time.Hour
	}
	go m.tidyWhenSettled(ctx)
	m.Update(ctx)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.tidy()
			m.Update(ctx)
		}
	}
}

// tidyWhenSettled tidies as soon as the running build is confirmed: a
// fresh update is confirmed about half a minute after it starts. It gives
// up after 60 looks (15 minutes by default); the daily check tries again.
func (m *Manager) tidyWhenSettled(ctx context.Context) {
	wait := m.SettleEvery
	if wait == 0 {
		wait = 15 * time.Second
	}
	for range 60 {
		if m.tidy() {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// tidy removes what earlier updates left behind (Prune), then runs Tidy.
// It acts only when no trial is open and the running build is the
// confirmed current version, so nothing a rollback could need is touched;
// it reports whether it acted.
func (m *Manager) tidy() bool {
	st, err := ReadState(m.DataDir)
	if err != nil || st.Trying != nil || !validVersion(st.Current) || st.Current != BuildVersion {
		return false
	}
	p, err := Prune(m.install(), m.DataDir)
	if p.Versions+p.Snapshots > 0 {
		m.logf("update: removed %d old version(s) and %d database snapshot(s) left by earlier updates (%.0f MB)",
			p.Versions, p.Snapshots, float64(p.Bytes)/1e6)
	}
	if err != nil {
		m.logf("update: some leftovers of earlier updates could not be removed yet: %v", err)
	}
	if m.Tidy != nil {
		m.Tidy()
	}
	return true
}

// Update runs one check-and-stage cycle. It never returns an error that
// should stop serving: failures are recorded in Status and logged.
func (m *Manager) Update(ctx context.Context) error {
	m.mu.Lock()
	m.status = Status{State: StateChecking, Message: "checking for updates"}
	m.mu.Unlock()

	st, err := ReadState(m.DataDir)
	if err != nil {
		m.setFailed(err)
		return err
	}
	// Reconcile the shared reservation book against the journal before
	// anything else: a crash after journaling "staged" but before the
	// deferred release leaves the staging hold behind forever, and the
	// snapshot hold of a version the journal no longer points at (never
	// staged, rejected or rolled back) keeps stealing floor from the
	// app's jobs. Only the staged or trying version's snapshot hold
	// survives.
	if err := m.reconcileHolds(st); err != nil {
		m.setFailed(err)
		return err
	}

	// No key compiled in: updates are off. This is not an error state.
	if PublicKey() == "" {
		m.setStatus(Status{State: StateOff, Message: "updates are off: no signing key was built into this copy"})
		m.logf("update: updates are off (no signing key compiled in)")
		return nil
	}

	if _, serr := os.Stat(StatePath(m.DataDir)); errors.Is(serr, os.ErrNotExist) {
		// First run: no journal has ever been written here. The running
		// build itself is the highest version that has run, so seed the
		// high-water mark from it - a fresh install must never accept a
		// signed release older than the build it is running. A dev build
		// has no usable version and stays unseeded.
		if validVersion(BuildVersion) {
			st.HighWater = BuildVersion
			if werr := writeState(m.DataDir, st); werr != nil {
				m.setFailed(werr)
				return werr
			}
			m.logf("update: first run: high-water mark seeded from this build (%s)", BuildVersion)
		}
	}
	if st.Trying != nil {
		// An update is being health-checked by the launcher; staging
		// another on top would lose the rollback records.
		m.setStatus(Status{State: StateStaged, Staged: st.Trying.Version, Message: "an update is being applied; the app will restart on its own", trial: true})
		return nil
	}
	if st.Staged != "" {
		m.setStatus(Status{State: StateStaged, Staged: st.Staged, Message: "version " + st.Staged + " is staged and ready; restart to apply it"})
		return nil
	}

	rel, err := m.Check(ctx)
	if err != nil {
		m.setFailed(err)
		return err
	}
	if rel == nil {
		m.setStatus(Status{State: StateIdle, Message: "no update available"})
		return nil
	}
	if err := m.Stage(ctx, rel); err != nil {
		m.setFailed(err)
		return err
	}
	m.setStatus(Status{State: StateStaged, Staged: rel.Tag, Message: "version " + rel.Tag + " is staged and ready; restart to apply it"})
	return nil
}

// Release is one downloadable kids-shelf release as announced by the
// releases/latest endpoint.
type Release struct {
	Tag     string // version; also the versions/<v> directory name
	ZipURL  string // ZipAssetName(): kids-shelf_<goos>_<goarch>.zip, or the universal Mac zip
	ZipSize int64  // asset size in bytes, as announced by the endpoint
	SumURL  string // SHA256SUMS
	SigURL  string // SHA256SUMS.sig
}

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

// ZipAssetName is the release zip this computer installs and updates
// from: kids-shelf_<goos>_<goarch>.zip, except on a Mac, where one
// universal app (Apple chip and Intel) is the only Mac download
// (DECISIONS "One universal Mac download").
func ZipAssetName() string { return zipAssetNameFor(runtime.GOOS, runtime.GOARCH) }

func zipAssetNameFor(goos, goarch string) string {
	if goos == "darwin" {
		goarch = "universal"
	}
	return fmt.Sprintf("kids-shelf_%s_%s.zip", goos, goarch)
}

// Check fetches the release metadata and returns the newest candidate
// release, or nil when there is nothing to stage (already on the newest
// confirmed version, or the only newer release was rejected).
func (m *Manager) Check(ctx context.Context) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.baseURL(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "kids-shelf-updater")
	resp, err := m.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("update: fetching %s: %w", m.baseURL(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("update: %s answered %s", m.baseURL(), resp.Status)
	}
	var gh ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&gh); err != nil {
		return nil, fmt.Errorf("update: decoding %s: %w", m.baseURL(), err)
	}
	if !validVersion(gh.TagName) {
		return nil, fmt.Errorf("update: release %q is not a usable version", gh.TagName)
	}
	rel := &Release{Tag: gh.TagName}
	zipName := ZipAssetName()
	for _, a := range gh.Assets {
		switch a.Name {
		case zipName:
			rel.ZipURL, rel.ZipSize = a.URL, a.Size
		case "SHA256SUMS":
			rel.SumURL = a.URL
		case "SHA256SUMS.sig":
			rel.SigURL = a.URL
		}
	}
	if rel.ZipURL == "" || rel.SumURL == "" || rel.SigURL == "" {
		return nil, fmt.Errorf("update: release %s lacks %s, SHA256SUMS and SHA256SUMS.sig assets", gh.TagName, zipName)
	}
	if rel.ZipSize <= 0 {
		return nil, fmt.Errorf("update: the %s asset of %s has no announced size", zipName, gh.TagName)
	}
	if rel.ZipSize > MaxDownloadBytes {
		return nil, errTooLarge
	}
	st, err := ReadState(m.DataDir)
	if err != nil {
		return nil, err
	}
	for _, r := range st.Rejected {
		if r == gh.TagName {
			m.logf("update: %s was tried before and failed; not retrying it", gh.TagName)
			return nil, nil
		}
	}
	if !versionAbove(gh.TagName, st.HighWater) {
		m.logf("update: already on %s or newer", st.HighWater)
		return nil, nil
	}
	return rel, nil
}

// Stage downloads, verifies and lays down rel under <install>/versions/<v>
// without touching the journal until the publish rename succeeded; on
// success it journals staged = v.
func (m *Manager) Stage(ctx context.Context, rel *Release) error {
	install := filepath.Clean(m.install())
	if install == "" {
		return errors.New("update: install directory is unknown")
	}
	if !validVersion(rel.Tag) {
		return fmt.Errorf("update: release %q is not a usable version", rel.Tag)
	}

	// Never download when disk space is short: the download, the extracted
	// binary and the database snapshot land on these two filesystems.
	for _, dir := range []string{install, m.DataDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("update: creating %s: %w", dir, err)
		}
		free, err := m.free(dir)
		if err != nil {
			return fmt.Errorf("update: measuring free space on %s: %w", dir, err)
		}
		if free < m.floor() {
			return fmt.Errorf("update: not downloading, only %d bytes free on %s (floor %d)", free, dir, m.floor())
		}
	}
	res := m.reservations()
	// Reserve the worst case before anything is downloaded: the zip asset
	// and the extracted binary (bounded by the cap) against the install
	// filesystem, and the current database size - the activation snapshot
	// the launcher takes on restart - against the data directory
	// filesystem: two reservations when the two live on different
	// filesystems, so both get an independent floor check. Both are
	// standalone holds (no work job), so they survive this manager and the
	// restart into the launcher.
	need := uint64(MaxExtractedBytes)
	if rel.ZipSize > 0 {
		need += uint64(rel.ZipSize)
	}
	if err := res.Hold(stageReservationID(rel.Tag), need, install); err != nil {
		return fmt.Errorf("update: reserving download space: %w", err)
	}
	defer res.Release(stageReservationID(rel.Tag))
	// The snapshot reservation is released again on every failure below;
	// only a fully staged (journaled) version keeps it, because the
	// launcher turns the reserved bytes into a real pre-<v>.db only then.
	snapshotHeld := false
	defer func() {
		if snapshotHeld {
			res.Release(snapshotReservationID(rel.Tag))
		}
	}()
	if fi, err := os.Stat(DatabasePath(m.DataDir)); err == nil && fi.Size() > 0 {
		if err := res.Hold(snapshotReservationID(rel.Tag), uint64(fi.Size()), m.DataDir); err != nil {
			return fmt.Errorf("update: reserving snapshot space: %w", err)
		}
		snapshotHeld = true
		// Deliberately not released on success: the snapshot bytes are
		// not on disk until the launcher activates the version (or
		// rejects it), so the hold keeps guarding them across the
		// restart.
	}

	tmp, err := os.MkdirTemp(install, ".update-")
	if err != nil {
		return fmt.Errorf("update: creating the staging directory: %w", err)
	}
	// The staging directory holds the downloaded zip and the extracted
	// binary; it must go whether the stage succeeds or fails.
	defer os.RemoveAll(tmp)

	// 1. Manifest: SHA256SUMS must verify against the compiled-in key, and
	// its first line declares the version it was signed for: `version:
	// vX.Y.Z`. The signature covers the whole file, so a release can never
	// be relabelled after signing.
	sums, err := downloadBytes(ctx, m.client(), rel.SumURL, manifestMaxBytes)
	if err != nil {
		return err
	}
	sig, err := downloadBytes(ctx, m.client(), rel.SigURL, sigMaxBytes)
	if err != nil {
		return err
	}
	if err := verifySumsSignature(sums, string(sig)); err != nil {
		return err
	}
	signedV, err := signedVersion(sums)
	if err != nil {
		return err
	}
	if signedV != rel.Tag {
		return fmt.Errorf("%w: the manifest is signed for %s but the release is tagged %s", errVersionMismatch, signedV, rel.Tag)
	}
	st, err := ReadState(m.DataDir)
	if err != nil {
		return err
	}
	if !versionAbove(signedV, st.HighWater) {
		return fmt.Errorf("update: %s is not newer than the highest version ever run (%s); refusing to downgrade", signedV, st.HighWater)
	}
	zipSHA, err := findSum(sums, ZipAssetName())
	if err != nil {
		return err
	}

	// 2. The zip asset must match its sha256 and stay under the cap.
	zipPath := filepath.Join(tmp, "release.zip")
	if err := downloadFile(ctx, m.client(), rel.ZipURL, zipPath, MaxDownloadBytes); err != nil {
		return err
	}
	got, err := hashFile(zipPath)
	if err != nil {
		return err
	}
	if got != zipSHA {
		return fmt.Errorf("%w: got %s, want %s for %s", errBadChecksum, got, zipSHA, rel.ZipURL)
	}

	// 3. Extract only the expected binary, no paths, no symlinks, capped.
	// extractBinary fsyncs the writable handle it wrote; nothing is
	// reopened read-only to Sync (which Windows cannot flush).
	vd := VersionDir(install, rel.Tag)
	os.RemoveAll(vd + ".tmp")
	os.RemoveAll(vd)
	if err := os.MkdirAll(filepath.Dir(vd), 0o755); err != nil {
		return err
	}
	bin := filepath.Join(vd+".tmp", EntryName())
	if err := extractBinary(zipPath, bin, MaxExtractedBytes); err != nil {
		return err
	}
	if err := os.Rename(vd+".tmp", vd); err != nil {
		return fmt.Errorf("update: installing %s: %w", vd, err)
	}
	if err := syncParent(vd); err != nil {
		return err
	}

	// 4. Journal staged = v, atomically.
	st, err = ReadState(m.DataDir)
	if err != nil {
		return err
	}
	st.Staged = rel.Tag
	if err := writeState(m.DataDir, st); err != nil {
		return fmt.Errorf("update: journaling the staged version: %w", err)
	}
	// The stage is journaled: the snapshot hold stays for the launcher, so
	// the deferred failure-release must not fire.
	snapshotHeld = false
	m.logf("update: staged %s", rel.Tag)
	return nil
}

// RestartToUpdate makes the process exit with code 3 (the launcher's
// "restart me" code) so the launcher activates the staged version. It is a
// no-op when nothing is staged.
func (m *Manager) RestartToUpdate() {
	st, err := ReadState(m.DataDir)
	if err != nil || st.Staged == "" {
		m.logf("update: nothing staged; not restarting")
		return
	}
	if m.OnRestart != nil {
		m.OnRestart()
		return
	}
	os.Exit(3)
}

func (m *Manager) setStatus(st Status) {
	m.mu.Lock()
	m.status = st
	m.mu.Unlock()
}

func (m *Manager) setFailed(err error) {
	m.mu.Lock()
	m.status = Status{State: StateFailed, Message: statusMessage(err)}
	m.mu.Unlock()
	m.logf("update: %v", err)
}

// Status returns the current updater state for the dashboard. A status
// recorded during a trial is re-read from the journal: the launcher settles
// the trial on its own (confirmed after 30 healthy seconds, or rolled back),
// and the parent page showed "an update is being applied" until the next
// daily check otherwise (Windows VM run 2026-10-27).
func (m *Manager) Status() Status {
	m.mu.Lock()
	s := m.status
	m.mu.Unlock()
	if !s.trial {
		return s
	}
	st, err := ReadState(m.DataDir)
	if err != nil || st.Trying != nil {
		return s
	}
	if st.Current == s.Staged {
		return Status{State: StateIdle, Message: "Kids Shelf is up to date (" + st.Current + ")"}
	}
	return Status{State: StateFailed, Message: "the update to " + s.Staged + " did not start properly, so Kids Shelf went back to " + st.Current}
}

// statusMessage turns a failure into the plain words a dashboard shows.
func statusMessage(err error) string {
	switch {
	case errors.Is(err, errBadSignature):
		return "the release is not signed with the expected key"
	case errors.Is(err, errBadChecksum):
		return "the download did not match its checksum"
	case errors.Is(err, errVersionMismatch):
		return "the release's signed version does not match its tag"
	case errors.Is(err, errTooLarge):
		return "the release asset is larger than the 300 MB limit"
	case errors.Is(err, errTooMuchData):
		return "the release contains more than 300 MB of data"
	case strings.Contains(err.Error(), "not newer than"):
		return "the release is not newer than the version already installed"
	case disk.IsDiskFull(err):
		return "there is not enough free disk space"
	default:
		var noSpace *disk.ErrNoSpace
		if errors.As(err, &noSpace) {
			return "there is not enough free disk space"
		}
		if strings.Contains(err.Error(), "creating the staging directory") {
			return "the install folder cannot be written to"
		}
		if strings.Contains(err.Error(), "not a usable version") ||
			strings.Contains(err.Error(), "signed version") ||
			strings.Contains(err.Error(), "lacks ") ||
			strings.Contains(err.Error(), "no announced size") ||
			strings.Contains(err.Error(), "contains no ") ||
			strings.Contains(err.Error(), "is a directory") ||
			strings.Contains(err.Error(), "is a symlink") {
			return "the release is not a usable Kids Shelf release"
		}
		// The release server / manifest errors are the rest: say plainly
		// that checking failed. The detail is logged.
		return "could not check for or install the update"
	}
}
