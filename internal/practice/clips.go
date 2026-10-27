package practice

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// bundledClips holds the spelling clips for the built-in word bank: one
// <word>.wav per word, generated once with Kokoro (voice af_heart) for the
// legacy app and copied from it.
//
//go:embed clips/*.wav
var bundledClips embed.FS

// InstallClips writes every bundled clip that is missing from dir, each
// through a temp file and a rename so a crash never leaves a partial clip.
// Existing files are left alone.
func InstallClips(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("practice: clips dir: %w", err)
	}
	entries, err := fs.ReadDir(bundledClips, "clips")
	if err != nil {
		return fmt.Errorf("practice: bundled clips: %w", err)
	}
	for _, e := range entries {
		dst := filepath.Join(dir, e.Name())
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		b, err := bundledClips.ReadFile("clips/" + e.Name())
		if err != nil {
			return fmt.Errorf("practice: bundled clip %s: %w", e.Name(), err)
		}
		tmp := dst + ".tmp"
		if err := os.WriteFile(tmp, b, 0o644); err != nil {
			return fmt.Errorf("practice: writing clip %s: %w", e.Name(), err)
		}
		if err := os.Rename(tmp, dst); err != nil {
			os.Remove(tmp)
			return fmt.Errorf("practice: installing clip %s: %w", e.Name(), err)
		}
	}
	return nil
}
