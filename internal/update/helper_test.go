package update

// Test helpers shared by the update tests: the release signing key, the
// fake release server, the release zip fixtures, and database/install
// helpers.

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/edementhon/kids-shelf/internal/store"
)

// TestMain lets the restore-fence test re-exec this binary as the "another
// process" that holds the database open (KS_UPDATE_TEST_HOLD_DB=<path>): it
// opens the store, writes the readiness marker KS_UPDATE_TEST_HOLD_READY,
// and blocks until it is killed.
func TestMain(m *testing.M) {
	if db := os.Getenv("KS_UPDATE_TEST_HOLD_DB"); db != "" {
		s, err := store.Open(db)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		defer s.Close()
		if err := os.WriteFile(os.Getenv("KS_UPDATE_TEST_HOLD_READY"), []byte("held"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		select {}
	}
	os.Exit(m.Run())
}

// holdDB starts a subprocess that opens the store at db and holds it until
// the returned stop func is called (or the test ends).
func holdDB(t *testing.T, db string) (stop func()) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestMain")
	ready := filepath.Join(t.TempDir(), "held")
	cmd.Env = append(os.Environ(), "KS_UPDATE_TEST_HOLD_DB="+db, "KS_UPDATE_TEST_HOLD_READY="+ready)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the holder subprocess: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			cmd.Wait()
			t.Fatalf("the holder subprocess never opened the database")
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	return func() {
		cmd.Process.Kill()
		cmd.Wait()
	}
}

const (
	testTag    = "v2.0.0"
	testOldTag = "v1.0.0"
)

// newTestKey generates the release signing key and installs its public half
// as the compiled-in key for the duration of the test.
func newTestKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	old := PublicKeyHex
	PublicKeyHex = hex.EncodeToString(pub)
	t.Cleanup(func() { PublicKeyHex = old })
	return priv
}

// releaseBytes is the "new build" the release zip ships: it only needs to
// exist, since app-side staging never executes it.
func releaseBytes(t *testing.T) []byte {
	t.Helper()
	return []byte("#!/bin/sh\necho fake kids-shelf build v2.0.0\n")
}

// makeReleaseZip zips the release build under ZipEntryName and returns the
// zip bytes and the zip's sha256.
func makeReleaseZip(t *testing.T) (zipBytes []byte, zipSHA string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(ZipEntryName())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(releaseBytes(t)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zipBytes = buf.Bytes()
	sum, err := hashBytes(zipBytes)
	if err != nil {
		t.Fatal(err)
	}
	return zipBytes, sum
}

// makeSums builds the SHA256SUMS content for a release signed for tag: the
// first line declares the version the file was signed for, then one line
// per asset, "<64 hex>  <name>".
func makeSums(tag, zipSHA string) []byte {
	return []byte(fmt.Sprintf("version: %s\n%s  %s\n", tag, zipSHA, zipAssetName()))
}

// fakeReleaseServer serves the releases/latest metadata plus the signed
// manifest and the zip asset, and counts the requests per path.
type fakeReleaseServer struct {
	*httptest.Server
	mu         sync.Mutex
	priv       ed25519.PrivateKey
	tag        string
	zip        []byte
	sums       []byte
	sig        []byte
	size       int64  // announced asset size; 0 means len(zip)
	latestSlow func() // when set, the latest handler calls it first
	hits       map[string]int
}

// newFakeServer serves a release for tag signed with priv. The metadata
// announces the zip asset size; callers can override it afterwards with
// setSize for the oversized test.
func newFakeServer(t *testing.T, priv ed25519.PrivateKey, tag string, zipBytes, sums []byte) *fakeReleaseServer {
	t.Helper()
	sig := hex.EncodeToString(ed25519.Sign(priv, sums))
	fs := &fakeReleaseServer{
		priv: priv,
		tag:  tag,
		zip:  zipBytes,
		sums: sums,
		sig:  []byte(sig),
		hits: map[string]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fs.mu.Lock()
		if fs.latestSlow != nil {
			slow := fs.latestSlow
			fs.mu.Unlock()
			slow()
			fs.mu.Lock()
		}
		fs.hits["latest"]++
		size := fs.size
		if size == 0 {
			size = int64(len(fs.zip))
		}
		zipName := zipAssetName()
		meta := ghRelease{TagName: fs.tag, Assets: []ghAsset{
			{Name: zipName, URL: fs.URL + "/assets/" + zipName, Size: size},
			{Name: "SHA256SUMS", URL: fs.URL + "/assets/SHA256SUMS"},
			{Name: "SHA256SUMS.sig", URL: fs.URL + "/assets/SHA256SUMS.sig"},
		}}
		fs.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(meta)
	})
	mux.HandleFunc("/assets/", func(w http.ResponseWriter, r *http.Request) {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		fs.hits[r.URL.Path]++
		switch filepath.Base(r.URL.Path) {
		case "SHA256SUMS":
			w.Write(fs.sums)
		case "SHA256SUMS.sig":
			w.Write(fs.sig)
		default:
			w.Write(fs.zip)
		}
	})
	fs.Server = httptest.NewServer(mux)
	t.Cleanup(fs.Close)
	return fs
}

// setSize overrides the announced zip asset size.
func (fs *fakeReleaseServer) setSize(n int64) {
	fs.mu.Lock()
	fs.size = n
	fs.mu.Unlock()
}

// setTag changes the announced release for tests that need a second one.
func (fs *fakeReleaseServer) setTag(tag string) {
	fs.mu.Lock()
	fs.tag = tag
	fs.mu.Unlock()
}

// setSums replaces the served manifest and re-signs it, for tests that
// serve a second release under the same server.
func (fs *fakeReleaseServer) setSums(sumBytes []byte) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.sums = sumBytes
	fs.sig = []byte(hex.EncodeToString(ed25519.Sign(fs.priv, sumBytes)))
}

// hitsFor returns how many requests a path received.
func (fs *fakeReleaseServer) hitsFor(path string) int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.hits[path]
}

func (fs *fakeReleaseServer) metaURL() string {
	return fs.URL + "/releases/latest"
}

// newTestManager builds a Manager for dataDir/install pointed at baseURL,
// with a free-space probe that never blocks and a small floor.
func newTestManager(t *testing.T, dataDir, install, baseURL string) *Manager {
	t.Helper()
	m := &Manager{
		DataDir: dataDir,
		Install: install,
		BaseURL: baseURL,
		Free:    func(string) (uint64, error) { return 1 << 40, nil },
		Floor:   1 << 20,
	}
	return m
}

// seedDB creates the data directory's database the way the running app
// leaves it: a probe table with one row.
func seedDB(t *testing.T, dataDir string) {
	t.Helper()
	s, err := store.Open(DatabasePath(dataDir))
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

// probeRows returns the rows of the probe table, or nil when the table does
// not exist (the restored snapshot has none).
func probeRows(t *testing.T, dataDir string) []string {
	t.Helper()
	s, err := store.Open(DatabasePath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.DB().Query(`SELECT v FROM probe ORDER BY v`)
	if err != nil {
		return nil
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

// appliedCount returns how many update_applied rows exist for version.
func appliedCount(t *testing.T, dataDir, version string) int {
	t.Helper()
	s, err := store.Open(DatabasePath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM update_applied WHERE version = ?`, version).Scan(&n); err != nil {
		return 0
	}
	return n
}

// probeRowsAt returns the rows of the probe table in the database at path
// (used to inspect a snapshot copy).
func probeRowsAt(t *testing.T, dbPath string) []string {
	t.Helper()
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.DB().Query(`SELECT v FROM probe ORDER BY v`)
	if err != nil {
		t.Fatal(err)
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

// setupInstall lays out an installed app: the launcher at the install root
// and the current version's build under versions/.
func setupInstall(t *testing.T) string {
	t.Helper()
	install := t.TempDir()
	if err := os.MkdirAll(filepath.Join(install, "versions", testOldTag), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(install, "versions", testOldTag, EntryName())
	if err := os.WriteFile(bin, []byte("old build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(EntryPath(install), []byte("launcher"), 0o755); err != nil {
		t.Fatal(err)
	}
	return install
}

func hashBytes(b []byte) (string, error) {
	f, err := os.CreateTemp("", "hash-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err := f.Write(b); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return hashFile(name)
}

// stageGood runs the full app-side stage of a signed good release against
// the fake server and returns the manager.
func stageGood(t *testing.T, dataDir, install string) (*Manager, ed25519.PrivateKey, *fakeReleaseServer) {
	t.Helper()
	priv := newTestKey(t)
	zipB, zipSHA := makeReleaseZip(t)
	sums := makeSums(testTag, zipSHA)
	srv := newFakeServer(t, priv, testTag, zipB, sums)
	m := newTestManager(t, dataDir, install, srv.metaURL())
	if err := m.Update(context.Background()); err != nil {
		t.Fatalf("Update: %v", err)
	}
	return m, priv, srv
}
