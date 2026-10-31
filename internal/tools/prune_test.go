package tools

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPruneOtherSetsKeepsTheRunningSetAndUnknownNames: the sets earlier
// builds extracted go; the running build's set, the yt-dlp folder, a name
// that only looks similar (upper case; not a variant of a real set's name,
// which a Mac's case-insensitive disk would treat as the same folder) and a
// loose file all stay; and with a keep name
// that is not a helper set (helpers from a configured folder) nothing is
// removed at all.
func TestPruneOtherSetsKeepsTheRunningSetAndUnknownNames(t *testing.T) {
	data := t.TempDir()
	root := filepath.Join(data, "helpers")
	for _, d := range []string{"2cbb0d59c6bdc6f0", "8bf972a3ae3184f7", "e3786dbf310600be", "yt-dlp/2026.10.26", "ABCDEF0123456789", "8bf972a3ae3184f7-old"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, d, "ffmpeg"), make([]byte, 100), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "0123456789abcdef"), []byte("a file"), 0o644); err != nil {
		t.Fatal(err)
	}

	if n, _, err := PruneOtherSets(data, "/some/configured/dir"); err != nil || n != 0 {
		t.Fatalf("keep not a set name: removed %d, err %v; want nothing removed", n, err)
	}

	n, freed, err := PruneOtherSets(data, "e3786dbf310600be")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || freed != 200 {
		t.Errorf("removed %d sets, %d bytes; want 2 sets, 200 bytes", n, freed)
	}
	for _, gone := range []string{"2cbb0d59c6bdc6f0", "8bf972a3ae3184f7"} {
		if _, err := os.Stat(filepath.Join(root, gone)); err == nil {
			t.Errorf("%s kept, want removed", gone)
		}
	}
	for _, kept := range []string{"e3786dbf310600be", "yt-dlp/2026.10.26", "ABCDEF0123456789", "8bf972a3ae3184f7-old", "0123456789abcdef"} {
		if _, err := os.Stat(filepath.Join(root, kept)); err != nil {
			t.Errorf("%s removed, want kept", kept)
		}
	}
}
