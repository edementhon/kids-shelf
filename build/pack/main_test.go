package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// repoRoot walks up from this test file (build/pack/main_test.go) to the
// repository root. Pack resolves the plist template and the app icon
// relative to the working directory, so the tests chdir there.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
}

// runPack runs run with fresh output buffers.
func runPack(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// fakeBinary creates a small "built launcher" file and returns its path.
func fakeBinary(t *testing.T, dir, name string) string {
	t.Helper()
	bin := filepath.Join(dir, name)
	if err := os.WriteFile(bin, []byte("fake launcher binary\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// zipMembers returns the entry names of path with their unix modes.
func zipMembers(t *testing.T, path string) map[string]os.FileMode {
	t.Helper()
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer r.Close()
	members := make(map[string]os.FileMode, len(r.File))
	for _, f := range r.File {
		members[f.Name] = f.Mode().Perm()
	}
	return members
}

func TestPackLinuxAndWindowsLayouts(t *testing.T) {
	t.Chdir(repoRoot(t))
	for _, target := range []string{"linux/amd64", "linux/arm64", "windows/amd64"} {
		out := t.TempDir()
		bin := fakeBinary(t, out, "kids-shelf")
		code, _, errOut := runPack(t, "-target", target, "-bin", bin, "-version", "v1.2.3", "-out", out)
		if code != 0 {
			t.Fatalf("%s: pack = %d, want 0; stderr: %s", target, code, errOut)
		}

		wantName := "kids-shelf"
		if target == "windows/amd64" {
			wantName = "kids-shelf.exe"
		}
		zipPath := filepath.Join(out, "kids-shelf_"+strings.ReplaceAll(target, "/", "_")+".zip")
		members := zipMembers(t, zipPath)
		if len(members) != 1 {
			t.Fatalf("%s: zip has %d entries, want 1 (%v)", target, len(members), members)
		}
		if mode, ok := members[wantName]; !ok {
			t.Fatalf("%s: zip has no %s entry: %v", target, wantName, members)
		} else if mode != 0o755 {
			t.Fatalf("%s: %s mode = %o, want 0755", target, wantName, mode)
		}

		// The zip must carry the exact binary bytes.
		r, err := zip.OpenReader(zipPath)
		if err != nil {
			t.Fatal(err)
		}
		rc, err := r.File[0].Open()
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(bin)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: zip content = %q, want %q", target, got, want)
		}
	}
}

func TestPackDarwinApp(t *testing.T) {
	t.Chdir(repoRoot(t))
	out := t.TempDir()
	bin := fakeBinary(t, out, "kids-shelf")
	code, _, errOut := runPack(t, "-target", "darwin/arm64", "-bin", bin, "-version", "v1.2.3", "-out", out)
	if code != 0 {
		t.Fatalf("pack = %d, want 0; stderr: %s", code, errOut)
	}

	zipPath := filepath.Join(out, "kids-shelf_darwin_arm64.zip")
	members := zipMembers(t, zipPath)
	want := []struct {
		name string
		mode os.FileMode
	}{
		{"kids-shelf.app/Contents/Info.plist", 0o644},
		{"kids-shelf.app/Contents/MacOS/kids-shelf", 0o755},
		{"kids-shelf.app/Contents/Resources/kids-shelf.icns", 0o644},
	}
	if len(members) != len(want) {
		t.Fatalf("darwin zip has %d entries, want %d: %v", len(members), len(want), members)
	}
	for _, w := range want {
		got, ok := members[w.name]
		if !ok {
			t.Fatalf("darwin zip is missing %s (has %v)", w.name, members)
		}
		if got != w.mode {
			t.Fatalf("%s mode = %o, want %o", w.name, got, w.mode)
		}
	}

	// The Info.plist must carry the contract values, with the version
	// without the leading v.
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var plist []byte
	for _, f := range r.File {
		if f.Name == "kids-shelf.app/Contents/Info.plist" {
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			plist, err = io.ReadAll(rc)
			rc.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(plist) == 0 {
		t.Fatal("could not read Info.plist from the darwin zip")
	}
	assertPlist(t, plist, "1.2.3")
}

// assertPlist checks the contract values of a rendered Info.plist: the
// version appears without its leading v, and the fixed bundle keys are
// present with the right values.
func assertPlist(t *testing.T, plist []byte, version string) {
	t.Helper()
	for _, kv := range []struct{ key, want string }{
		{"CFBundleIdentifier", "io.github.edementhon.kids-shelf"},
		{"CFBundleExecutable", "kids-shelf"},
		{"CFBundleIconFile", "kids-shelf"},
		{"CFBundleShortVersionString", version},
		{"CFBundleVersion", version},
		{"LSMinimumSystemVersion", "11.0"},
	} {
		re := regexp.MustCompile(`<key>` + regexp.QuoteMeta(kv.key) + `</key>\s*<string>([^<]*)</string>`)
		m := re.FindSubmatch(plist)
		if m == nil {
			t.Fatalf("plist has no string value for %s:\n%s", kv.key, plist)
		}
		if got := string(m[1]); got != kv.want {
			t.Fatalf("plist %s = %q, want %q", kv.key, got, kv.want)
		}
	}
	for _, key := range []string{"LSUIElement", "NSHighResolutionCapable"} {
		re := regexp.MustCompile(`<key>` + regexp.QuoteMeta(key) + `</key>\s*<true/>`)
		if !re.Match(plist) {
			t.Fatalf("plist key %s is not <true/>:\n%s", key, plist)
		}
	}
	if bytes.Contains(plist, []byte("v"+version)) {
		t.Fatalf("plist still carries the v prefix (%s):\n%s", "v"+version, plist)
	}
}

func TestPlistTemplateVersions(t *testing.T) {
	t.Chdir(repoRoot(t))
	for _, version := range []string{"1.0.0", "10.2.34"} {
		plist, err := renderPlist(version)
		if err != nil {
			t.Fatalf("renderPlist(%s): %v", version, err)
		}
		assertPlist(t, plist, version)
	}
}

// TestStageZipSplit drives the darwin release flow pack exists to enable:
// stage the .app, let the signing step rewrite the binary, then zip.
func TestStageZipSplit(t *testing.T) {
	t.Chdir(repoRoot(t))
	out := t.TempDir()
	bin := fakeBinary(t, out, "kids-shelf")

	code, _, errOut := runPack(t, "-target", "darwin/amd64", "-bin", bin, "-version", "v2.0.1", "-out", out, "-stage")
	if code != 0 {
		t.Fatalf("pack -stage = %d, want 0; stderr: %s", code, errOut)
	}
	stagedBin := filepath.Join(out, "staged", "darwin_amd64", "kids-shelf.app", "Contents", "MacOS", "kids-shelf")
	if _, err := os.Stat(stagedBin); err != nil {
		t.Fatalf("staged binary missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "kids-shelf_darwin_amd64.zip")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("-stage must not zip; expecting no zip, got err %v", err)
	}

	// The signing step (rcodesign here) rewrites the staged binary.
	signed := []byte("signed binary\n")
	if err := os.WriteFile(stagedBin, signed, 0o755); err != nil {
		t.Fatal(err)
	}

	code, _, errOut = runPack(t, "-target", "darwin/amd64", "-out", out, "-zip")
	if code != 0 {
		t.Fatalf("pack -zip = %d, want 0; stderr: %s", code, errOut)
	}
	r, err := zip.OpenReader(filepath.Join(out, "kids-shelf_darwin_amd64.zip"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var f *zip.File
	for _, e := range r.File {
		if e.Name == "kids-shelf.app/Contents/MacOS/kids-shelf" {
			f = e
		}
	}
	if f == nil {
		t.Fatalf("zip has no %s entry: %v", "kids-shelf.app/Contents/MacOS/kids-shelf", r.File)
	}
	rc, err := f.Open()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, signed) {
		t.Fatalf("zip binary = %q, want the signed bytes %q", got, signed)
	}
}

// TestZipPhaseRechecksBinarySize covers the darwin flow where signing the
// staged .app grows the launcher past the extraction cap after stage: the
// stage-time check already passed, so the zip phase must refuse the grown
// binary instead of publishing a zip every updater rejects.
func TestZipPhaseRechecksBinarySize(t *testing.T) {
	t.Chdir(repoRoot(t))
	out := t.TempDir()
	bin := fakeBinary(t, out, "kids-shelf")

	code, _, errOut := runPack(t, "-target", "darwin/amd64", "-bin", bin, "-version", "v2.0.1", "-out", out, "-stage")
	if code != 0 {
		t.Fatalf("pack -stage = %d, want 0; stderr: %s", code, errOut)
	}
	stagedBin := filepath.Join(out, "staged", "darwin_amd64", "kids-shelf.app", "Contents", "MacOS", "kids-shelf")

	// Signing (here simulated) rewrites the staged Mach-O past the cap.
	f, err := os.Create(stagedBin)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxBinaryBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()

	code, _, errOut = runPack(t, "-target", "darwin/amd64", "-out", out, "-zip")
	if code != 1 {
		t.Fatalf("pack -zip = %d, want a run error (1); stderr: %s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(out, "kids-shelf_darwin_amd64.zip")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an oversized staged binary must not be zipped (stat err = %v)", err)
	}
}

// TestBinaryOverCapRefused drives the real stage path with a sparse binary
// just past the cap: the check runs on size alone, so no 300 MiB are ever
// read or written.
func TestBinaryOverCapRefused(t *testing.T) {
	out := t.TempDir()
	bin := filepath.Join(out, "huge")
	f, err := os.Create(bin)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxBinaryBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()

	code, _, errOut := runPack(t, "-target", "linux/amd64", "-bin", bin, "-version", "v1.2.3", "-out", out)
	if code == 0 {
		t.Fatal("pack accepted a binary over the 300 MiB cap")
	}
	if _, err := os.Stat(filepath.Join(out, "staged", "linux_amd64")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an oversized binary must not be staged (stat err = %v)", err)
	}
	if code != 1 {
		t.Fatalf("pack = %d, want a run error (1); stderr: %s", code, errOut)
	}
}

// TestZipOverCapRefused exercises the zip-side cap check with an injected
// small cap, since building a truly incompressible 300 MiB file in a test
// would dominate the suite.
func TestZipOverCapRefused(t *testing.T) {
	out := t.TempDir()
	staged := filepath.Join(out, "staged")
	if err := os.MkdirAll(staged, 0o755); err != nil {
		t.Fatal(err)
	}
	// Pseudorandom data that deflate cannot shrink below the tiny cap.
	// math/rand/v2's ChaCha8 is the seeded source with a Read method.
	data := make([]byte, 8<<10)
	var seed [32]byte
	seed[0] = 1
	if _, err := rand.NewChaCha8(seed).Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, "big"), data, 0o755); err != nil {
		t.Fatal(err)
	}

	zipPath := filepath.Join(out, "kids-shelf_linux_amd64.zip")
	if err := zipLayout(staged, zipPath, 1<<10); err == nil {
		t.Fatal("zipLayout accepted a zip over the cap")
	}
	if _, err := os.Stat(zipPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an oversized zip must not be left behind (stat err = %v)", err)
	}
	leftovers, err := filepath.Glob(filepath.Join(out, ".kids-shelf-*.zip"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("temp zips left behind: %v", leftovers)
	}

	// Under a generous cap the same staged layout packs and renames.
	if err := zipLayout(staged, zipPath, 1<<20); err != nil {
		t.Fatalf("zipLayout under the cap: %v", err)
	}
	if _, err := os.Stat(zipPath); err != nil {
		t.Fatalf("zip missing after a successful pack: %v", err)
	}
}

func TestPackRefusesBadInput(t *testing.T) {
	out := t.TempDir()
	bin := fakeBinary(t, out, "kids-shelf")
	for _, args := range [][]string{
		{"-target", "linux/amd64", "-bin", bin, "-out", out},                      // no version
		{"-target", "linux/amd64", "-bin", bin, "-version", "1.2.3", "-out", out}, // version without v
		{"-target", "linux/amd64", "-bin", bin, "-version", "v01.2.3", "-out", out},
		{"-target", "linux/amd64", "-bin", bin, "-version", "v1.2", "-out", out},
		{"-target", "plan9/amd64", "-bin", bin, "-version", "v1.2.3", "-out", out},
		{"-target", "darwin", "-bin", bin, "-version", "v1.2.3", "-out", out},
		{"-bin", bin, "-version", "v1.2.3", "-out", out},              // no target
		{"-target", "linux/amd64", "-version", "v1.2.3", "-out", out}, // no bin
		{"-target", "linux/amd64", "-bin", bin, "-version", "v1.2.3"}, // no out
		{"-target", "linux/amd64", "-bin", bin, "-version", "v1.2.3", "-out", out, "-stage", "-zip"},
	} {
		if code, _, _ := runPack(t, args...); code != 2 {
			t.Fatalf("pack %v = %d, want a usage error (2)", args, code)
		}
	}

	// -zip without a prior -stage fails cleanly with a run error.
	code, _, errOut := runPack(t, "-target", "linux/amd64", "-out", out, "-zip")
	if code != 1 {
		t.Fatalf("pack -zip without staging = %d, want 1; stderr: %s", code, errOut)
	}
}
