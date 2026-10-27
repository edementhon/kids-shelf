package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func writeExec(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// wantDir is the extraction directory Locate/Extract must produce.
func wantDir(dataDir, version string) string {
	return filepath.Join(dataDir, helpersSubdir, version)
}

func TestExtractCopiesVerifiesAndExecutes(t *testing.T) {
	fsys := fstest.MapFS{
		"ffmpeg":     {Data: []byte("ffmpeg-binary")},
		"ffprobe":    {Data: []byte("ffprobe-binary")},
		"nested/qjs": {Data: []byte("qjs-binary")},
	}
	m := Manifest{Version: "v1", Files: []ManifestFile{
		{Name: "ffmpeg", SHA256: sha("ffmpeg-binary")},
		{Name: "ffprobe", SHA256: sha("ffprobe-binary")},
		{Name: "nested/qjs", SHA256: sha("qjs-binary")},
	}}
	dataDir := t.TempDir()
	dir, err := Extract(fsys, m, dataDir)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if dir != wantDir(dataDir, "v1") {
		t.Errorf("Extract returned %q, want %q", dir, wantDir(dataDir, "v1"))
	}
	want := map[string]string{
		"ffmpeg":                       "ffmpeg-binary",
		"ffprobe":                      "ffprobe-binary",
		filepath.Join("nested", "qjs"): "qjs-binary",
	}
	for name, data := range want {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		if string(got) != data {
			t.Errorf("%s = %q, want %q", name, got, data)
		}
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("stat %s: %v", name, err)
			continue
		}
		if st.Mode()&0o111 == 0 {
			t.Errorf("%s mode %v: not executable", name, st.Mode())
		}
	}
	// No extraction temps left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".extract-") {
			t.Errorf("leftover extraction temp %s", e.Name())
		}
	}
}

func TestExtractIsAtomicOnBadDigest(t *testing.T) {
	fsys := fstest.MapFS{
		"ffmpeg":  {Data: []byte("data")},
		"ffprobe": {Data: []byte("data")},
	}
	m := Manifest{Version: "v1", Files: []ManifestFile{
		{Name: "ffmpeg", SHA256: sha("tampered")}, // wrong digest
		{Name: "ffprobe", SHA256: sha("data")},
	}}
	dataDir := t.TempDir()
	if _, err := Extract(fsys, m, dataDir); err == nil {
		t.Fatal("Extract succeeded with a bad digest, want error")
	}
	// The corrupt file must not be left in place.
	if _, err := os.Stat(filepath.Join(dataDir, "helpers", "v1", "ffmpeg")); !os.IsNotExist(err) {
		t.Errorf("corrupt helper left behind (err %v)", err)
	}
}

func TestExtractIsIdempotent(t *testing.T) {
	fsys := fstest.MapFS{"ffmpeg": {Data: []byte("data")}}
	m := Manifest{Version: "v1", Files: []ManifestFile{{Name: "ffmpeg", SHA256: sha("data")}}}
	dataDir := t.TempDir()
	if _, err := Extract(fsys, m, dataDir); err != nil {
		t.Fatalf("first Extract: %v", err)
	}
	ffmpeg := filepath.Join(dataDir, "helpers", "v1", "ffmpeg")
	st, err := os.Stat(ffmpeg)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// A second extraction leaves the in-use copy alone.
	time.Sleep(1100 * time.Millisecond) // mtime resolution
	if _, err := Extract(fsys, m, dataDir); err != nil {
		t.Fatalf("second Extract: %v", err)
	}
	st2, err := os.Stat(ffmpeg)
	if err != nil {
		t.Fatalf("stat after re-extract: %v", err)
	}
	if !st2.ModTime().Equal(st.ModTime()) {
		t.Error("already-correct helper was rewritten")
	}
}

func TestExtractValidation(t *testing.T) {
	fsys := fstest.MapFS{"ffmpeg": {Data: []byte("x")}}
	good := Manifest{Version: "v1", Files: []ManifestFile{{Name: "ffmpeg", SHA256: sha("x")}}}
	dataDir := t.TempDir()
	emptyVersion := good
	emptyVersion.Version = ""
	if _, err := Extract(fsys, emptyVersion, dataDir); err == nil {
		t.Error("empty version accepted")
	}
	noFiles := good
	noFiles.Files = nil
	if _, err := Extract(fsys, noFiles, dataDir); err == nil {
		t.Error("empty file list accepted")
	}
	missingFile := Manifest{Version: "v1", Files: []ManifestFile{{Name: "yt-dlp", SHA256: sha("x")}}}
	if _, err := Extract(fsys, missingFile, dataDir); err == nil {
		t.Error("file missing from fs accepted")
	}
	badHex := Manifest{Version: "v1", Files: []ManifestFile{{Name: "ffmpeg", SHA256: "xyz"}}}
	if _, err := Extract(fsys, badHex, dataDir); err == nil {
		t.Error("bad digest hex accepted")
	}
}

func TestLocateOrder(t *testing.T) {
	t.Setenv(EnvHelpersDir, "")
	explicitDir := t.TempDir()
	envDir := t.TempDir()

	// Explicit wins over the environment.
	t.Setenv(EnvHelpersDir, envDir)
	d, err := Locate(explicitDir, nil, Manifest{}, "data")
	if err != nil {
		t.Fatalf("Locate explicit: %v", err)
	}
	if d.FFmpeg != filepath.Join(explicitDir, helperName("ffmpeg")) {
		t.Errorf("FFmpeg = %q, want explicit dir", d.FFmpeg)
	}

	// Environment when no explicit dir is configured.
	d, err = Locate("", nil, Manifest{}, "data")
	if err != nil {
		t.Fatalf("Locate env: %v", err)
	}
	if d.FFmpeg != filepath.Join(envDir, helperName("ffmpeg")) {
		t.Errorf("FFmpeg = %q, want env dir", d.FFmpeg)
	}
	if d.YtDlp != filepath.Join(envDir, helperName("yt-dlp")) {
		t.Errorf("YtDlp = %q, want env dir", d.YtDlp)
	}
	if d.KiwixServe != filepath.Join(envDir, helperName("kiwix-serve")) {
		t.Errorf("KiwixServe = %q, want env dir", d.KiwixServe)
	}

	// Extraction when neither is set.
	t.Setenv(EnvHelpersDir, "")
	fsys := fstest.MapFS{"ffmpeg": {Data: []byte("b")}, "ffprobe": {Data: []byte("b")}}
	m := Manifest{Version: "v2", Files: []ManifestFile{
		{Name: "ffmpeg", SHA256: sha("b")},
		{Name: "ffprobe", SHA256: sha("b")},
	}}
	dataDir := t.TempDir()
	d, err = Locate("", fsys, m, dataDir)
	if err != nil {
		t.Fatalf("Locate extract: %v", err)
	}
	extracted := wantDir(dataDir, "v2")
	if d.FFmpeg != filepath.Join(extracted, helperName("ffmpeg")) {
		t.Errorf("FFmpeg = %q, want extracted dir", d.FFmpeg)
	}
	if _, err := os.Stat(filepath.Join(extracted, "ffprobe")); err != nil {
		t.Errorf("extracted ffprobe missing: %v", err)
	}

	// Nothing configured and nothing bundled is an error.
	if _, err := Locate("", nil, Manifest{}, "data"); err == nil {
		t.Error("Locate with no source succeeded, want error")
	}

	// A configured directory must exist.
	if _, err := Locate(filepath.Join(t.TempDir(), "missing"), nil, Manifest{}, t.TempDir()); err == nil {
		t.Error("Locate with a missing explicit dir succeeded, want error")
	}
}

func TestSelfTest(t *testing.T) {
	dir := t.TempDir()
	writeExec(t, dir, "ffmpeg", "#!/bin/sh\nexit 0\n")
	writeExec(t, dir, "ffprobe", "#!/bin/sh\nexit 0\n")
	writeExec(t, dir, "qjs", "#!/bin/sh\nexit 1\n") // broken qjs
	writeExec(t, dir, "kiwix-serve", "#!/bin/sh\nexit 0\n")
	d := dirAt(dir)
	r := d.SelfTest(context.Background())
	if !r.FFmpeg || !r.FFprobe || !r.KiwixServe {
		t.Errorf("working helpers reported broken: %+v", r)
	}
	if r.Qjs {
		t.Error("broken qjs reported working")
	}
}

func TestSelfTestHonorsTimeout(t *testing.T) {
	dir := t.TempDir()
	writeExec(t, dir, "ffmpeg", "#!/bin/sh\nsleep 30\n")
	d := dirAt(dir)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	r := d.SelfTest(ctx)
	if r.FFmpeg {
		t.Error("hanging helper reported working")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("SelfTest took %v, did not honor the timeout", elapsed)
	}
}

func TestSelfTestMissingBinaries(t *testing.T) {
	d := dirAt(t.TempDir())
	r := d.SelfTest(context.Background())
	if r.FFmpeg || r.FFprobe || r.Qjs || r.KiwixServe {
		t.Errorf("missing binaries reported working: %+v", r)
	}
}
