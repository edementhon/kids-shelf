// Package tools locates and extracts the helper binaries the server runs:
// ffmpeg and ffprobe for ingest, qjs as yt-dlp's JavaScript runtime, and
// yt-dlp itself. The helpers are looked up from an explicit directory
// (config), then $KS_HELPERS_DIR, then the versioned bundle extracted into
// the data dir by Extract. Validation hashes come from a Manifest built at
// release time; phase 4 embeds the real binaries, so Extract accepts any
// fs.FS and tests use fstest.MapFS. kiwix-serve (the offline-library
// server) lives in the same helper directory and is located here too,
// though it is a separate binary built from the libkiwix sources rather
// than the embedded bundle. ffmpeg-hw, the optional hardware-encoder
// helper (only shipped for linux/amd64), is picked up the same way when it
// is present and never required: a machine without it simply has no
// hardware encode path.
package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/edementhon/kids-shelf/internal/proc"
)

// EnvHelpersDir is the environment variable overriding where the helper
// binaries live (the orchestrator sets it to the built helper directory).
const EnvHelpersDir = "KS_HELPERS_DIR"

// helpersSubdir is the extraction root under the data dir.
const helpersSubdir = "helpers"

// selfTestTimeout bounds each helper's self-test invocation.
const selfTestTimeout = 10 * time.Second

// Manifest describes the helper binaries bundled with one release. Version
// names the extraction directory <data>/helpers/<version>, so a release that
// changes the helpers extracts a new directory and the old one can be
// deleted; re-extracting the same version is a no-op.
type Manifest struct {
	Version string // directory name under <data>/helpers/
	Files   []ManifestFile
}

// ManifestFile is one helper binary: Name is its slash-separated path inside
// the embedded filesystem (and under the version directory), SHA256 the
// expected digest, hex-encoded.
type ManifestFile struct {
	Name   string
	SHA256 string
}

// Dir holds the resolved paths of the helper binaries. Some (yt-dlp) may not
// exist yet on a fresh install; SelfTest reports which ones actually run.
// FFmpegHW is the optional hardware-encoder helper ("" when the machine has
// none: it is never present on Windows, and absent wherever the bundle or
// the helper directory does not carry it).
type Dir struct {
	FFmpeg     string
	FFprobe    string
	Qjs        string
	YtDlp      string
	KiwixServe string
	FFmpegHW   string
}

// Extract copies the files of fsys listed in m into <dataDir>/helpers/
// <version>, verifying each against its manifest SHA-256 and marking them
// executable, and returns the directory. Every file lands via a temp name
// and an atomic rename, so a crash never leaves a half-written helper; a file
// that is already present with the right digest is left alone, which makes
// concurrent launches safe.
func Extract(fsys fs.FS, m Manifest, dataDir string) (string, error) {
	if m.Version == "" {
		return "", errors.New("tools: manifest has no version")
	}
	if len(m.Files) == 0 {
		return "", errors.New("tools: manifest lists no files")
	}
	dir := filepath.Join(dataDir, helpersSubdir, m.Version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("tools: create %s: %w", dir, err)
	}
	for _, f := range m.Files {
		if err := extractOne(fsys, dir, f); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func extractOne(fsys fs.FS, dir string, f ManifestFile) error {
	if f.Name == "" {
		return errors.New("tools: manifest file has no name")
	}
	want, err := hex.DecodeString(f.SHA256)
	if err != nil || len(want) != sha256.Size {
		return fmt.Errorf("tools: %s: manifest sha256 must be %d hex digits", f.Name, 2*sha256.Size)
	}
	dst := filepath.Join(dir, filepath.FromSlash(f.Name))
	if ok, err := fileMatches(dst, want); err == nil && ok {
		return nil // already extracted correctly; leave the in-use copy alone
	}
	src, err := fsys.Open(f.Name)
	if err != nil {
		return fmt.Errorf("tools: %s: %w", f.Name, err)
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("tools: create %s: %w", filepath.Dir(dst), err)
	}
	tmp, err := os.CreateTemp(dir, ".extract-*")
	if err != nil {
		return fmt.Errorf("tools: %s: %w", f.Name, err)
	}
	tmpName := tmp.Name()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), src); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("tools: %s: %w", f.Name, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("tools: %s: %w", f.Name, err)
	}
	if !bytes.Equal(h.Sum(nil), want) {
		os.Remove(tmpName)
		return fmt.Errorf("tools: %s: sha256 mismatch (extraction would be corrupt)", f.Name)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("tools: %s: chmod: %w", f.Name, err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("tools: %s: rename: %w", f.Name, err)
	}
	return nil
}

// fileMatches reports whether path exists and hashes to want.
func fileMatches(path string, want []byte) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return bytes.Equal(h.Sum(nil), want), nil
}

// Locate resolves the directory the helper binaries run from, in order: an
// explicit directory (config override), then $KS_HELPERS_DIR, then the
// versioned extraction under <dataDir>, extracted from bundled on demand.
// It returns an error only when the chosen directory does not exist (or no
// bundle is available); whether the binaries inside it actually work is
// SelfTest's job, since a fresh install legitimately has no yt-dlp yet.
func Locate(explicit string, bundled fs.FS, m Manifest, dataDir string) (*Dir, error) {
	base := explicit
	if base == "" {
		base = os.Getenv(EnvHelpersDir)
	}
	if base != "" {
		st, err := os.Stat(base)
		if err != nil {
			return nil, fmt.Errorf("tools: helpers dir %s: %w", base, err)
		}
		if !st.IsDir() {
			return nil, fmt.Errorf("tools: helpers dir %s: not a directory", base)
		}
		return dirAt(base), nil
	}
	if bundled == nil || m.Version == "" {
		return nil, errors.New("tools: no helper directory configured and none bundled")
	}
	dir, err := Extract(bundled, m, dataDir)
	if err != nil {
		return nil, err
	}
	return dirAt(dir), nil
}

// dirAt builds the helper paths under base, adding the .exe suffix on
// Windows. The extracted bundle's manifest names match (phase 4 embeds
// per-OS names such as ffmpeg.exe). FFmpegHW is resolved only when the
// file is actually there: ffmpeg-hw.exe never exists, and a machine
// without the helper simply has no hardware path.
func dirAt(base string) *Dir {
	hw := filepath.Join(base, helperName("ffmpeg-hw"))
	if _, err := os.Stat(hw); err != nil {
		hw = ""
	}
	return &Dir{
		FFmpeg:     filepath.Join(base, helperName("ffmpeg")),
		FFprobe:    filepath.Join(base, helperName("ffprobe")),
		Qjs:        filepath.Join(base, helperName("qjs")),
		YtDlp:      filepath.Join(base, helperName("yt-dlp")),
		KiwixServe: filepath.Join(base, helperName("kiwix-serve")),
		FFmpegHW:   hw,
	}
}

func helperName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

// SelfTestResult reports which helpers answered a trivial invocation.
type SelfTestResult struct {
	FFmpeg     bool
	FFprobe    bool
	Qjs        bool
	KiwixServe bool
}

// SelfTest runs each helper once under a timeout, `ffmpeg -version`,
// `ffprobe -version`, `qjs -e '1'`, `kiwix-serve --version`, and reports
// which worked. A missing or broken binary is a false, never an error, so
// startup can show a plain "video conversion is unavailable" message instead
// of crashing.
func (d *Dir) SelfTest(ctx context.Context) SelfTestResult {
	return SelfTestResult{
		FFmpeg:     selfTestOne(ctx, d.FFmpeg, "-version"),
		FFprobe:    selfTestOne(ctx, d.FFprobe, "-version"),
		Qjs:        selfTestOne(ctx, d.Qjs, "-e", "1"),
		KiwixServe: selfTestOne(ctx, d.KiwixServe, "--version"),
	}
}

func selfTestOne(ctx context.Context, bin string, args ...string) bool {
	if bin == "" {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, selfTestTimeout)
	defer cancel()
	if err := proc.CommandContext(cctx, bin, args...).Run(); err != nil {
		return false
	}
	return true
}
