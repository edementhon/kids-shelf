// Command pack assembles one per-target kids-shelf release zip from a
// built launcher binary (PLAN.md 1a "Updates"; run with
// `go run ./build/pack` from the repository root). It implements the zip
// layout the updater's extractBinary expects (internal/update):
//
//	windows/amd64:  kids-shelf.exe at the zip root
//	linux/*:        kids-shelf at the zip root
//	darwin/*:       ONLY a kids-shelf.app/ bundle: Contents/Info.plist,
//	                Contents/MacOS/kids-shelf and
//	                Contents/Resources/kids-shelf.icns
//
// Executables get mode 0755 in the zip, all other files 0644. The zip and
// the launcher binary are both capped at 300 MiB, matching the updater's
// download and extraction limits (update.MaxDownloadBytes).
//
// Apple bundles are signed between staging and zipping, so the signing
// tool sees the finished .app:
//
//	pack -target darwin/amd64 -bin kids-shelf -version v1.2.3 -out rel -stage
//	rcodesign sign rel/staged/darwin_amd64/kids-shelf.app
//	pack -target darwin/amd64 -out rel -zip
//
// Linux and Windows ship in one step (nothing to sign):
//
//	pack -target linux/amd64 -bin kids-shelf -version v1.2.3 -out rel
//
// The staged layout lives at <out>/staged/<goos>_<goarch>/. The darwin
// Info.plist template (packaging/darwin/Info.plist.tmpl) and the app icon
// (internal/install/icons/kids-shelf.icns) resolve relative to the working
// directory, so pack is run from the repository root.
package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
)

// Size limits for a release: the compressed zip (the download cap) and the
// launcher binary (the extraction cap) are both 300 MiB, the same limits
// the updater enforces (internal/update/manage.go).
const (
	maxBinaryBytes = 300 << 20
	maxZipBytes    = 300 << 20
)

// versionPattern is the canonical release version, vMAJOR.MINOR.PATCH with
// no leading zeros; it is also the only version the updater will accept.
var versionPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// config is the parsed command line.
type config struct {
	goos      string
	goarch    string
	bin       string // built launcher binary (not needed for -zip)
	version   string // canonical vMAJOR.MINOR.PATCH (not needed for -zip)
	out       string
	stageOnly bool // -stage: write the staged layout and stop
	zipOnly   bool // -zip: zip the existing staged layout
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the CLI and returns the process exit code: 0 on success, 1
// on failure, 2 on a usage error.
func run(args []string, stdout, stderr io.Writer) int {
	cfg, err := parse(args)
	if err != nil {
		fmt.Fprintf(stderr, "pack: %v\n\n%s", err, usage)
		return 2
	}
	if !cfg.zipOnly {
		if err := stage(cfg); err != nil {
			fmt.Fprintf(stderr, "pack: %v\n", err)
			return 1
		}
	}
	if !cfg.stageOnly {
		// The staged executable can differ from the one stage checked:
		// the darwin flow signs the .app between stage and zip, and
		// signing grows the Mach-O. Re-check it here so a signed binary
		// that outgrew the extraction cap is refused, not published into
		// a zip every updater rejects.
		if err := checkStagedExecutable(cfg); err != nil {
			fmt.Fprintf(stderr, "pack: %v\n", err)
			return 1
		}
		outZip := filepath.Join(cfg.out, fmt.Sprintf("kids-shelf_%s_%s.zip", cfg.goos, cfg.goarch))
		if err := zipLayout(stagedDir(cfg), outZip, maxZipBytes); err != nil {
			fmt.Fprintf(stderr, "pack: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "wrote %s\n", outZip)
	}
	return 0
}

const usage = `pack assembles a kids-shelf release zip from a built launcher binary.

Usage:
  pack -target <goos>/<goarch> -bin <binary> -version <vX.Y.Z> -out <dir> [-stage|-zip]

  -target    target as goos/goarch (linux/amd64, linux/arm64, windows/amd64,
             darwin/amd64, darwin/arm64)
  -bin       path to the built launcher binary (omitted with -zip)
  -version   canonical release version, vMAJOR.MINOR.PATCH (omitted with -zip)
  -out       output directory; the zip and the staged layout live here
  -stage     write <out>/staged/<goos>_<goarch> and stop, so the darwin .app
             can be code-signed between staging and zipping
  -zip       zip the existing staged layout into the release zip
`

func parse(args []string) (config, error) {
	var cfg config
	var stage, zip bool
	fs := flag.NewFlagSet("pack", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Var(targetValue{&cfg}, "target", "")
	fs.StringVar(&cfg.bin, "bin", "", "")
	fs.StringVar(&cfg.version, "version", "", "")
	fs.StringVar(&cfg.out, "out", "", "")
	fs.BoolVar(&stage, "stage", false, "")
	fs.BoolVar(&zip, "zip", false, "")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() != 0 {
		return config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if stage && zip {
		return config{}, errors.New("-stage and -zip are two halves of the same run; pass one or neither")
	}
	cfg.stageOnly = stage
	cfg.zipOnly = zip

	if cfg.out == "" {
		return config{}, errors.New("-out is required")
	}
	if cfg.goos == "" || cfg.goarch == "" {
		return config{}, errors.New("-target is required, as goos/goarch (for example linux/amd64)")
	}
	if !cfg.zipOnly {
		if cfg.bin == "" {
			return config{}, errors.New("-bin is required except with -zip")
		}
		if !versionPattern.MatchString(cfg.version) {
			return config{}, fmt.Errorf("version %q is not a canonical vMAJOR.MINOR.PATCH release version", cfg.version)
		}
	}
	return cfg, nil
}

type targetValue struct{ c *config }

func (t targetValue) String() string {
	if t.c.goos == "" && t.c.goarch == "" {
		return ""
	}
	return t.c.goos + "/" + t.c.goarch
}

func (t targetValue) Set(s string) error {
	goos, goarch, ok := strings.Cut(s, "/")
	if !ok || goos == "" || goarch == "" || strings.Contains(goarch, "/") {
		return fmt.Errorf("target %q is not goos/goarch", s)
	}
	switch goos {
	case "windows", "linux", "darwin":
	default:
		return fmt.Errorf("pack has no zip layout for %q (want windows, linux or darwin)", goos)
	}
	t.c.goos, t.c.goarch = goos, goarch
	return nil
}

func stagedDir(cfg config) string {
	return filepath.Join(cfg.out, "staged", cfg.goos+"_"+cfg.goarch)
}

// stage writes the release layout for the target into <out>/staged/
// <goos>_<goarch>, refusing a launcher binary over the 300 MiB cap. It
// replaces any earlier staged layout so a failed run can never leak stale
// files into the zip.
func stage(cfg config) error {
	info, err := os.Stat(cfg.bin)
	if err != nil {
		return fmt.Errorf("reading %s: %w", cfg.bin, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", cfg.bin)
	}
	if info.Size() > maxBinaryBytes {
		return fmt.Errorf("%s is %d bytes, more than the %d MiB release cap", cfg.bin, info.Size(), maxBinaryBytes>>20)
	}

	dir := stagedDir(cfg)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	switch cfg.goos {
	case "darwin":
		return stageApp(cfg, dir)
	default:
		// Windows and Linux ship the bare executable at the zip root.
		name := "kids-shelf"
		if cfg.goos == "windows" {
			name += ".exe"
		}
		return copyFileMode(cfg.bin, filepath.Join(dir, name), 0o755)
	}
}

// stagedExecutable is the launcher binary inside a staged layout, the one
// entry the updater extracts and caps at 300 MiB (internal/update
// extractBinary).
func stagedExecutable(cfg config) string {
	if cfg.goos == "darwin" {
		return filepath.Join(stagedDir(cfg), "kids-shelf.app", "Contents", "MacOS", "kids-shelf")
	}
	name := "kids-shelf"
	if cfg.goos == "windows" {
		name += ".exe"
	}
	return filepath.Join(stagedDir(cfg), name)
}

// checkStagedExecutable refuses a staged launcher over the extraction cap.
// The check must run in the zip phase, not only at stage time, because the
// darwin release flow signs the .app between the two and signing can grow
// the binary past the cap.
func checkStagedExecutable(cfg config) error {
	path := stagedExecutable(cfg)
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("staged launcher %s: %w (run pack without -zip to stage it first)", path, err)
	}
	if info.Size() > maxBinaryBytes {
		return fmt.Errorf("%s is %d bytes, more than the %d MiB release cap", path, info.Size(), maxBinaryBytes>>20)
	}
	return nil
}

// stageApp builds kids-shelf.app: the Info.plist rendered from
// packaging/darwin/Info.plist.tmpl, the launcher binary and the app icon.
func stageApp(cfg config, dir string) error {
	contents := filepath.Join(dir, "kids-shelf.app", "Contents")
	for _, sub := range []string{"MacOS", "Resources"} {
		if err := os.MkdirAll(filepath.Join(contents, sub), 0o755); err != nil {
			return err
		}
	}
	plist, err := renderPlist(strings.TrimPrefix(cfg.version, "v"))
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(contents, "Info.plist"), plist, 0o644); err != nil {
		return err
	}
	if err := copyFileMode("internal/install/icons/kids-shelf.icns", filepath.Join(contents, "Resources", "kids-shelf.icns"), 0o644); err != nil {
		return err
	}
	return copyFileMode(cfg.bin, filepath.Join(contents, "MacOS", "kids-shelf"), 0o755)
}

// plistData is the template input for Info.plist.tmpl.
type plistData struct {
	Version string // MAJOR.MINOR.PATCH without the leading v
}

// renderPlist renders packaging/darwin/Info.plist.tmpl with the version
// without its leading v (a version of "v1.2.3" becomes "1.2.3").
func renderPlist(version string) ([]byte, error) {
	tmpl, err := template.ParseFiles("packaging/darwin/Info.plist.tmpl")
	if err != nil {
		return nil, fmt.Errorf("reading the Info.plist template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, plistData{Version: version}); err != nil {
		return nil, fmt.Errorf("rendering the Info.plist: %w", err)
	}
	return buf.Bytes(), nil
}

// copyFileMode copies src to dst with dst's mode set to mode.
func copyFileMode(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("reading %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("writing %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return fmt.Errorf("writing %s: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return fmt.Errorf("writing %s: %w", dst, err)
	}
	return nil
}

// zipLayout zips the staged layout into outZip. The zip is written to a
// temp file in the destination directory and only renamed into place once
// it is known to fit under max bytes, so an oversized release never leaves
// a half-written zip behind.
func zipLayout(stagedDirPath, outZip string, max int64) error {
	info, err := os.Stat(stagedDirPath)
	if err != nil {
		return fmt.Errorf("staged layout %s: %w (run pack without -zip to stage it first)", stagedDirPath, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("staged layout %s is not a directory", stagedDirPath)
	}

	tmp, err := os.CreateTemp(filepath.Dir(outZip), ".kids-shelf-*.zip")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	entryCount := 0
	zw := zip.NewWriter(tmp)
	walkErr := filepath.WalkDir(stagedDirPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("staged entry %s is a symlink; refusing to ship it", path)
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(stagedDirPath, path)
		if err != nil {
			return err
		}
		hdr, err := zip.FileInfoHeader(fi)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		hdr.Method = zip.Deflate
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(w, src)
		src.Close()
		if copyErr != nil {
			return copyErr
		}
		entryCount++
		return nil
	})
	if walkErr != nil || entryCount == 0 {
		zw.Close()
		tmp.Close()
		os.Remove(tmpPath)
		if walkErr != nil {
			return walkErr
		}
		return fmt.Errorf("staged layout %s is empty", stagedDirPath)
	}
	if err := zw.Close(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	fi, err := os.Stat(tmpPath)
	if err != nil {
		return err
	}
	if fi.Size() > max {
		os.Remove(tmpPath)
		return fmt.Errorf("the zip would be %d bytes, more than the %d MiB release cap", fi.Size(), max>>20)
	}
	if err := os.Rename(tmpPath, outZip); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("moving the zip into place: %w", err)
	}
	return nil
}
