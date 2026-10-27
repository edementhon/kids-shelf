// Package update implements kids-shelf self-update (PLAN.md 1a "Updates and
// data"). The launcher supervises the app as a child process and is the only
// process that activates or rolls back an update; the app itself only
// downloads and stages.
//
// Install layout (the exe name carries ".exe" on Windows):
//
//	<install>/kids-shelf(.exe)           the launcher, never replaced by the updater
//	<install>/versions/<v>/kids-shelf(.exe)  one directory per installed version
//
// The launcher tells the app child where the install root is with KS_INSTALL,
// so a build under versions/<v>/ still stages into the same root.
//
// Update journal under the data directory (written atomically): current,
// previous, staged, trying {version, snapshot, started}, rejected versions
// and the highest version ever confirmed:
//
//	<data>/update/state.json             {current, previous, high_water, staged, trying, rejected}
//	<data>/update/pre-<v>.db             database snapshot taken before trying v
//
// Launcher side (with the instance lock held and no child running):
// Activate snapshots the database and journals a trial when a version is
// staged, and treats a leftover "trying" from a previous start as a failed
// trial. Confirm and Reject settle the health watch: 30 s of /healthz
// answers confirm the trial; a non-zero exit or no health within 60 s
// restores the snapshot, marks the version rejected and runs the previous
// version instead.
//
// App side (the supervised child or the in-process launcher): a daily check
// of GitHub Releases; the candidate must be a canonical vMAJOR.MINOR.PATCH
// version above the high-water mark and not rejected. SHA256SUMS (one line
// per zip asset, "<64 hex>  <name>") and its ed25519 signature
// SHA256SUMS.sig are verified against the compiled-in PublicKeyHex (empty
// key: updates are off, shown in the dashboard). The zip is reserved
// against the disk floor, downloaded capped at 300 MB, hash-checked, and
// only the expected binary is extracted (capped at 300 MB) into
// versions/<v>.tmp/ before one rename publishes versions/<v>.
package update

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

const (
	// envInstall carries the install root from the launcher to the app
	// child it supervises.
	envInstall = "KS_INSTALL"

	// databaseFilename is the shared data-directory database name, matching
	// PLAN.md 1a "one database file in the data dir"; the store opens it.
	databaseFilename = "kids-shelf.db"
)

// versionPattern is the canonical release version: vMAJOR.MINOR.PATCH with
// no leading zeros in any component (v02.0.0, v2.00.1 and v2.0.01 are not
// usable versions).
var versionPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// exeSuffixFor returns the executable suffix for goos: ".exe" on windows,
// "" everywhere else. Taking goos instead of reading runtime.GOOS lets
// tests exercise every platform's names on any host.
func exeSuffixFor(goos string) string {
	if goos == "windows" {
		return ".exe"
	}
	return ""
}

func exeSuffix() string { return exeSuffixFor(runtime.GOOS) }

// EntryName is the executable file name of the launcher and of every
// versioned build, `kids-shelf` or `kids-shelf.exe`.
func EntryName() string { return "kids-shelf" + exeSuffix() }

// zipEntryName returns the zip member that carries a release binary for
// goos: darwin releases ship a kids-shelf.app bundle, every other platform
// a plain executable at the zip root.
func zipEntryName(goos string) string {
	if goos == "darwin" {
		return "kids-shelf.app/Contents/MacOS/kids-shelf"
	}
	return "kids-shelf" + exeSuffixFor(goos)
}

// ZipEntryName is the release zip member that carries the binary for the
// running OS: inside the kids-shelf.app bundle on darwin, the plain
// executable name everywhere else. Only the zip member differs; the
// extracted file always lands at the versioned build path (EntryName).
func ZipEntryName() string { return zipEntryName(runtime.GOOS) }

// InstallDir returns the install root: the directory KS_INSTALL points at
// (set by the launcher for the child it supervises), falling back to the
// running executable's own directory.
func InstallDir() (string, error) {
	if d := os.Getenv(envInstall); d != "" {
		return d, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("update: locating the running executable: %w", err)
	}
	return filepath.Dir(exe), nil
}

// EntryPath returns the launcher binary path in install.
func EntryPath(install string) string {
	return filepath.Join(install, EntryName())
}

// VersionDir returns <install>/versions/<v>.
func VersionDir(install, v string) string {
	return filepath.Join(install, "versions", v)
}

// VersionBinary returns the versioned build at <install>/versions/<v>.
func VersionBinary(install, v string) string {
	return filepath.Join(VersionDir(install, v), EntryName())
}

// DatabasePath returns the shared database path inside the data directory.
func DatabasePath(dataDir string) string {
	return filepath.Join(dataDir, databaseFilename)
}

// validVersion reports whether v is a canonical vMAJOR.MINOR.PATCH version,
// the only form the release contract allows and the only safe name for a
// versions/ directory.
func validVersion(v string) bool {
	return versionPattern.MatchString(v)
}

// versionParts splits vMAJOR.MINOR.PATCH; ok is false for anything that is
// not a canonical version.
func versionParts(v string) (major, minor, patch int, ok bool) {
	if !versionPattern.MatchString(v) {
		return 0, 0, 0, false
	}
	parts := strings.Split(v[1:], ".")
	major, err := strconv.Atoi(parts[0])
	if err != nil || major < 0 {
		return 0, 0, 0, false
	}
	minor, err = strconv.Atoi(parts[1])
	if err != nil || minor < 0 {
		return 0, 0, 0, false
	}
	patch, err = strconv.Atoi(parts[2])
	if err != nil || patch < 0 {
		return 0, 0, 0, false
	}
	return major, minor, patch, true
}

// versionAbove reports whether v is a canonical version strictly newer than
// high. An empty high-water mark means nothing has ever run, so any
// canonical version qualifies.
func versionAbove(v, high string) bool {
	maj, min, pat, ok := versionParts(v)
	if !ok {
		return false
	}
	if high == "" {
		return true
	}
	hmaj, hmin, hpat, hok := versionParts(high)
	if !hok {
		return true
	}
	switch {
	case maj != hmaj:
		return maj > hmaj
	case min != hmin:
		return min > hmin
	default:
		return pat > hpat
	}
}

// replaceFile writes data to path atomically: a temp file in the same
// directory is fsynced and renamed over the destination, so a crash never
// leaves a half-written file.
func replaceFile(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := replace(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return syncParent(path)
}

// copyFile copies src to dst with mode, creating dst's parent directory.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	return nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// hasEntry reports whether err means the path is absent.
func isNotExist(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}
