package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestGenerateGolden writes two helpers with known contents into a temp
// directory and checks the exact manifest.json they must produce, that the
// Version is stable across runs, and that an existing manifest.json is
// ignored (so re-running manifestgen leaves its output unchanged).
func TestGenerateGolden(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte("ffmpeg-binary\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "qjs"), []byte("qjs-binary\n"), 0o755)

	m, err := generate(dir)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if err := writeManifest(dir, m); err != nil {
		t.Fatalf("writeManifest: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	want := `{
  "Version": "e59cc8d14b5fafdd",
  "Files": [
    {
      "Name": "ffmpeg",
      "SHA256": "3f6fe93481a75295907cd70706ffee631097bd02e5ae56f76b869535feeaa3a7"
    },
    {
      "Name": "qjs",
      "SHA256": "0331676f3036c5019595da2eddeb933b72e1cd2d108a818e7605d94a32e6dfa6"
    }
  ]
}
`
	if string(got) != want {
		t.Fatalf("manifest.json mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}

	// The same helper set must always name the same Version, and a second
	// run - with the manifest already present - must reproduce the exact
	// file (the walk skips manifest.json).
	again, err := generate(dir)
	if err != nil {
		t.Fatalf("second generate: %v", err)
	}
	if again.Version != m.Version {
		t.Fatalf("Version changed across runs: %s != %s", again.Version, m.Version)
	}
	if len(again.Files) != 2 {
		t.Fatalf("second run listed %d files, want 2", len(again.Files))
	}
	if err := writeManifest(dir, again); err != nil {
		t.Fatalf("second writeManifest: %v", err)
	}
	got2, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		t.Fatalf("re-read manifest: %v", err)
	}
	if string(got2) != want {
		t.Fatalf("manifest.json changed across runs\n--- got ---\n%s", got2)
	}
}

// TestGenerateEmptyDir: a directory with nothing to bundle is refused.
func TestGenerateEmptyDir(t *testing.T) {
	if _, err := generate(t.TempDir()); err == nil {
		t.Fatal("generate of an empty directory succeeded, want an error")
	}
}
