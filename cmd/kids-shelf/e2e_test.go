package main

// End-to-end tests: the real launcher wiring (runSupervisor) supervising
// the test binary re-exec'd as fake app versions, with a real staged
// release and the real update journal.

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/edementhon/kids-shelf/internal/launch"
	"github.com/edementhon/kids-shelf/internal/store"
	"github.com/edementhon/kids-shelf/internal/update"
)

// shortWindows shrinks the launcher's health windows for the e2e tests.
func shortWindows(t *testing.T) {
	t.Helper()
	oldS, oldC, oldT, oldP := launch.StartupTimeout, launch.ConfirmHealthyFor, launch.TrialHealthTimeout, launch.HealthPollInterval
	launch.StartupTimeout = 2 * time.Second
	launch.ConfirmHealthyFor = 400 * time.Millisecond
	launch.TrialHealthTimeout = 1 * time.Second
	launch.HealthPollInterval = 20 * time.Millisecond
	t.Cleanup(func() {
		launch.StartupTimeout, launch.ConfirmHealthyFor, launch.TrialHealthTimeout, launch.HealthPollInterval = oldS, oldC, oldT, oldP
	})
}

// freePort returns a port that was free a moment ago.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// testBinary returns this test binary's bytes, the stand-in for an app
// build (it plays fake app roles via KS_TEST_ROLE).
func testBinary(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(os.Args[0]) //nolint:gosec -- the test binary re-execs itself
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// setupInstall lays out an install: the launcher (entry) and versions/v1.0.0
// as copies of the test binary.
func setupInstall(t *testing.T) string {
	t.Helper()
	install := t.TempDir()
	bin := testBinary(t)
	if err := os.WriteFile(filepath.Join(install, update.EntryName()), bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(install, "versions", "v1.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The versioned build is the same binary with a marker appended (the
	// first bytes decide which version dir it came from is irrelevant; the
	// marker makes the copy distinct and still runnable).
	marked := append(append([]byte(nil), bin...), []byte("\nks-test-v1.0.0\n")...)
	if err := os.WriteFile(filepath.Join(install, "versions", "v1.0.0", update.EntryName()), marked, 0o755); err != nil {
		t.Fatal(err)
	}
	return install
}

// releaseServer serves a signed kids-shelf release for tag whose zip holds
// the test binary.
type releaseServer struct {
	*httptest.Server
	tag string
}

func newReleaseServer(t *testing.T, priv ed25519.PrivateKey, tag string, bits []byte) *releaseServer {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// The member a real release uses on this OS: inside kids-shelf.app on
	// darwin (the macos-smoke runners failed with the plain name).
	w, err := zw.Create(update.ZipEntryName())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(bits); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zipBytes := buf.Bytes()
	zipName := fmt.Sprintf("kids-shelf_%s_%s.zip", runtime.GOOS, runtime.GOARCH)
	sum := sha256.Sum256(zipBytes)
	// The manifest names the version it was signed for as its first line;
	// the signature covers the whole file, so a relabelled release is
	// refused.
	sums := fmt.Sprintf("version: %s\n%x  %s\n", tag, sum, zipName)
	sig := hex.EncodeToString(ed25519.Sign(priv, []byte(sums)))

	mux := http.NewServeMux()
	srv := &releaseServer{tag: tag}
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		meta := map[string]any{
			"tag_name": srv.tag,
			"assets": []map[string]any{
				{"name": zipName, "browser_download_url": srv.URL + "/assets/" + zipName, "size": len(zipBytes)},
				{"name": "SHA256SUMS", "browser_download_url": srv.URL + "/assets/SHA256SUMS"},
				{"name": "SHA256SUMS.sig", "browser_download_url": srv.URL + "/assets/SHA256SUMS.sig"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(meta)
	})
	mux.HandleFunc("/assets/", func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "SHA256SUMS":
			io.WriteString(w, sums)
		case "SHA256SUMS.sig":
			io.WriteString(w, sig)
		default:
			w.Write(zipBytes)
		}
	})
	srv.Server = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// seedDB creates the database with one probe row.
func seedDB(t *testing.T, dataDir string) {
	t.Helper()
	db := update.DatabasePath(dataDir)
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.DB().Exec(`CREATE TABLE IF NOT EXISTS probe (v TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO probe (v) VALUES ('old')`); err != nil {
		t.Fatal(err)
	}
}

// probeRows returns the probe rows, or []string{} when the table is gone.
func probeRows(t *testing.T, dataDir string) []string {
	t.Helper()
	s, err := store.Open(update.DatabasePath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.DB().Query(`SELECT v FROM probe ORDER BY v`)
	if err != nil {
		return []string{}
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

// writeJournal writes the given journal state directly (used to seed the
// journal as if an earlier confirmed run left it).
func writeJournal(t *testing.T, dataDir string, st update.State) {
	t.Helper()
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dataDir, "update", "state.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// setKey installs a test signing key as the compiled-in key.
func setKey(t *testing.T, priv ed25519.PrivateKey) {
	t.Helper()
	old := update.PublicKey()
	update.SetPublicKey(hex.EncodeToString(priv.Public().(ed25519.PublicKey)))
	t.Cleanup(func() { update.SetPublicKey(old) })
}

// newManager builds the app-side updater pointed at the release server.
func newManager(dataDir, install, baseURL string) *update.Manager {
	return &update.Manager{
		DataDir: dataDir,
		Install: install,
		BaseURL: baseURL,
		Free:    func(string) (uint64, error) { return 1 << 40, nil },
		Floor:   1 << 20,
	}
}

// waitJournal polls the journal until cond returns true.
func waitJournal(t *testing.T, dataDir string, what string, cond func(update.State) bool) update.State {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		st, err := update.ReadState(dataDir)
		if err != nil {
			t.Fatalf("reading the journal while waiting for %s: %v", what, err)
		}
		if cond(st) {
			return st
		}
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(filepath.Join(dataDir, "last-error.txt"))
			t.Fatalf("journal never reached %s: %+v; launcher error: %s", what, st, b)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitHealthz GETs the instance's /healthz and requires the recorded id.
func waitHealthz(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err := os.ReadFile(filepath.Join(dir, "instance.json"))
		if err == nil {
			var rec struct {
				Port int    `json:"port"`
				ID   string `json:"id"`
			}
			if json.Unmarshal(b, &rec) == nil && rec.Port != 0 {
				resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", rec.Port))
				if err == nil {
					body, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					if strings.Contains(string(body), rec.ID) {
						return
					}
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the instance never answered /healthz with its id")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestGoodReleaseFlowStagedRestartActivateConfirm is the flagship e2e
// scenario: the app stages a signed release while running, the child exits
// 3 ("restart to update"), the launcher activates the staged version, its
// /healthz answers for the confirm window, and the journal advances to the
// new current / high-water mark.
func TestGoodReleaseFlowStagedRestartActivateConfirm(t *testing.T) {
	shortWindows(t)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	setKey(t, priv)

	install := setupInstall(t)
	dataDir := t.TempDir()
	seedDB(t, dataDir)
	writeJournal(t, dataDir, update.State{Current: "v1.0.0", HighWater: "v1.0.0"})

	srv := newReleaseServer(t, priv, "v2.0.0", testBinary(t))
	exitFile := filepath.Join(dataDir, "exit-3")
	t.Setenv("KS_TEST_ROLE", "app")
	t.Setenv("KS_TEST_MODE", "serve-exit-once")
	t.Setenv("KS_TEST_EXIT_FILE", exitFile)
	t.Setenv("KS_TEST_EXIT_DONE", filepath.Join(dataDir, "exit-done"))
	t.Setenv("KS_TEST_EXIT_CODE", "3")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- launchSupervisor(ctx, dataDir, install, freePort(t)) }()

	// v1.0.0 (the fake app) is running and waiting on the exit file.
	waitHealthz(t, dataDir)

	// The published instance must carry the public app port and the
	// separate public libraries port; health flows through the proxy.
	b, _ := os.ReadFile(filepath.Join(dataDir, "instance.json"))
	var rec struct {
		Port          int `json:"port"`
		LibrariesPort int `json:"libraries_port"`
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("reading instance.json: %v", err)
	}
	if rec.Port == 0 || rec.LibrariesPort == 0 || rec.LibrariesPort == rec.Port {
		t.Errorf("instance.json = {port: %d, libraries_port: %d}, want two distinct public ports", rec.Port, rec.LibrariesPort)
	}

	// The app stages the signed release while running.
	m := newManager(dataDir, install, srv.URL+"/releases/latest")
	if err := m.Update(context.Background()); err != nil {
		t.Fatalf("staging: %v", err)
	}
	st, _ := update.ReadState(dataDir)
	if st.Staged != "v2.0.0" {
		t.Fatalf("staged = %q, want v2.0.0", st.Staged)
	}
	if _, err := os.Stat(update.VersionBinary(install, "v2.0.0")); err != nil {
		t.Fatalf("the staged build is missing: %v", err)
	}

	// The dashboard restart: the child exits 3, the launcher activates the
	// staged version and confirms it after the healthy window.
	if err := os.WriteFile(exitFile, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitJournal(t, dataDir, "current=v2.0.0 high_water=v2.0.0", func(st update.State) bool {
		return st.Current == "v2.0.0" && st.HighWater == "v2.0.0" && st.Trying == nil
	})
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("the launcher returned %d, want 0", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the launcher did not stop after cancel")
	}
	st, _ = update.ReadState(dataDir)
	if st.Previous != "v1.0.0" {
		t.Errorf("previous = %q, want v1.0.0", st.Previous)
	}
	if rows := probeRows(t, dataDir); len(rows) != 1 || rows[0] != "old" {
		t.Errorf("database rows = %v, want [old]", rows)
	}
}

// TestTrialCrashRollsBackWithDatabase: a trial version that migrates the
// schema, writes a row and crashes is rolled back - the snapshot is
// restored (only the first row survives, wal/shm leftovers removed), the
// version is rejected, and the previous version keeps serving /healthz.
func TestTrialCrashRollsBackWithDatabase(t *testing.T) {
	shortWindows(t)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	setKey(t, priv)

	install := setupInstall(t)
	dataDir := t.TempDir()
	seedDB(t, dataDir)
	writeJournal(t, dataDir, update.State{Current: "v1.0.0", HighWater: "v1.0.0"})

	srv := newReleaseServer(t, priv, "v2.0.0", testBinary(t))
	// Stage before the launcher starts, like an app that staged overnight.
	m := newManager(dataDir, install, srv.URL+"/releases/latest")
	if err := m.Update(context.Background()); err != nil {
		t.Fatalf("staging: %v", err)
	}

	// One-shot crash marker: the trial version consumes it when it crashes,
	// so the previous version that serves after the rollback stays up
	// instead of migrating and crashing again.
	crashFile := filepath.Join(dataDir, "crash-once")
	if err := os.WriteFile(crashFile, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KS_TEST_ROLE", "app")
	t.Setenv("KS_TEST_MODE", "migrate-schema-then-crash")
	t.Setenv("KS_TEST_CRASH_FILE", crashFile)
	t.Setenv("KS_TEST_DB", update.DatabasePath(dataDir))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- launchSupervisor(ctx, dataDir, install, freePort(t)) }()

	// The trial migrates the database, writes a row and exits 1; the journal
	// rejects v2.0.0 and the launcher restores the snapshot.
	waitJournal(t, dataDir, "rejected v2.0.0", func(st update.State) bool {
		return len(st.Rejected) == 1 && st.Rejected[0] == "v2.0.0" && st.Trying == nil
	})

	// The rollback restored the pre-update database. Its stale wal/shm
	// removal is pinned by internal/update's activate tests; here the
	// previous version restarts at once and reopens the database (creating
	// fresh wal/shm), so checking the files would race. A stale wal replayed
	// onto the snapshot would bring back the failed version's row, which
	// the row check below catches.
	db := update.DatabasePath(dataDir)

	// Per PLAN 1a the previous version keeps serving after the rollback.
	waitHealthz(t, dataDir)
	rows := probeRows(t, dataDir)
	if len(rows) != 1 || rows[0] != "old" {
		t.Errorf("rows after rollback = %v, want only [old] (the new version's write must be gone)", rows)
	}
	// The failed version's table is gone with the restored snapshot.
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.DB().Query(`SELECT v FROM probe_v2`); err == nil {
		t.Error("probe_v2 survived the rollback; the snapshot was not restored")
	}
	st, _ := update.ReadState(dataDir)
	if st.Current != "v1.0.0" || st.HighWater != "v1.0.0" {
		t.Errorf("journal = %+v, want current/high-water v1.0.0 unchanged", st)
	}

	// The previous version keeps serving; a clean stop ends the launcher 0.
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("the launcher returned %d after cancel, want 0 (the previous version serves after the rollback)", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the launcher did not stop after cancel")
	}
}

// TestTrialHangRollsBackWithDatabase: a trial that never answers /healthz
// is killed at the trial deadline and rolled back.
func TestTrialHangRollsBackWithDatabase(t *testing.T) {
	shortWindows(t)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	setKey(t, priv)

	install := setupInstall(t)
	dataDir := t.TempDir()
	seedDB(t, dataDir)
	writeJournal(t, dataDir, update.State{Current: "v1.0.0", HighWater: "v1.0.0"})

	srv := newReleaseServer(t, priv, "v2.0.0", testBinary(t))
	m := newManager(dataDir, install, srv.URL+"/releases/latest")
	if err := m.Update(context.Background()); err != nil {
		t.Fatalf("staging: %v", err)
	}

	t.Setenv("KS_TEST_ROLE", "app")
	t.Setenv("KS_TEST_MODE", "hang-no-health")

	start := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- launchSupervisor(ctx, dataDir, install, freePort(t)) }()

	waitJournal(t, dataDir, "rejected v2.0.0", func(st update.State) bool {
		return len(st.Rejected) == 1 && st.Rejected[0] == "v2.0.0" && st.Trying == nil
	})
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("the hang trial took %s to roll back; the window was not shortened", elapsed)
	}
	// The previous version runs next; it hangs too (its mode is inherited),
	// so the launcher ends with a visible never-healthy error.
	select {
	case code := <-done:
		if code == 0 {
			t.Fatal("the launcher ended cleanly after the hang; want a visible failure")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the launcher did not end")
	}
	rows := probeRows(t, dataDir)
	if len(rows) != 1 || rows[0] != "old" {
		t.Errorf("rows after rollback = %v, want only [old]", rows)
	}
}

// TestInProcessModeStartsNoUpdater: without an install layout the launcher
// serves in-process and never starts the updater - no install layout, no
// updates; only the supervised child stages. Update configuration is
// ignored, nothing is staged, and the server still stops cleanly.
func TestInProcessModeStartsNoUpdater(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("read-only directories do not block root")
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	setKey(t, priv)

	install := t.TempDir()
	if err := os.Chmod(install, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(install, 0o755) })

	// A release server that would happily stage, if the updater ran.
	srv := newReleaseServer(t, priv, "v2.0.0", []byte("some build"))
	t.Setenv("KS_INSTALL", install)
	t.Setenv("KS_UPDATE_BASEURL", srv.URL+"/releases/latest")

	dir := t.TempDir()
	port := freePort(t)
	done := make(chan int, 1)
	go func() { done <- launchRun(dir, port) }()

	waitHealthz(t, dir)
	// Give a wrongly started updater time to stage; nothing may be staged.
	time.Sleep(500 * time.Millisecond)
	st, _ := update.ReadState(dir)
	if st.Staged != "" {
		t.Errorf("in-process mode staged %q; only the supervised child may stage", st.Staged)
	}
	waitHealthz(t, dir)
	// Stop the in-process server the way the OS stops the launcher.
	if err := stopSelf(); err != nil {
		t.Skipf("cannot generate a stop signal in this environment: %v", err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("run returned %d, want 0", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not stop")
	}
}

// launchSupervisor runs the real launcher wiring in-process and returns its
// exit code.
func launchSupervisor(ctx context.Context, dataDir, install string, port int) int {
	var errOut bytes.Buffer
	return runSupervisor(ctx, dataDir, install, port, true, &errOut)
}

// launchRun runs the real CLI path in-process (the test binary has no
// install layout, so it serves in-process) and returns its exit code.
func launchRun(dataDir string, port int) int {
	var out, errOut bytes.Buffer
	return run(false, dataDir, port, true, "", "", &out, &errOut)
}
