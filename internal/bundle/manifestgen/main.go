// Command manifestgen writes the manifest.json of one helper directory:
// internal/bundle/files/<goos>_<goarch>/. Release builds embed that file
// together with the binaries (internal/bundle), and tools.Extract uses the
// manifest to verify and install them.
//
// Usage:
//
//	go run ./internal/bundle/manifestgen -dir internal/bundle/files/linux_amd64
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/edementhon/kids-shelf/internal/tools"
)

// manifestName is the file this command writes; it is never listed among
// the helpers it describes.
const manifestName = "manifest.json"

// versionHexLen is the length of the manifest Version: the first 16 hex
// characters of the hash over the sorted helper list, long enough that any
// change to the helper set names a new extraction directory while the same
// set always names the same one.
const versionHexLen = 16

func main() {
	dir := flag.String("dir", "", "helper directory to scan (required)")
	flag.Parse()
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "manifestgen: -dir is required")
		os.Exit(2)
	}
	m, err := generate(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "manifestgen:", err)
		os.Exit(1)
	}
	if err := writeManifest(*dir, m); err != nil {
		fmt.Fprintln(os.Stderr, "manifestgen:", err)
		os.Exit(1)
	}
}

// generate scans dir for the helpers it contains and computes their
// manifest: one entry per regular file (manifest.json excluded), sorted by
// name, and a Version that is the first 16 hex characters of the sha256
// over the sorted "name sha\n" lines. An empty directory is refused: a
// manifest with no files would extract nothing and is always a mistake.
func generate(dir string) (tools.Manifest, error) {
	var files []tools.ManifestFile
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if d.Name() == manifestName {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: not a regular file", path)
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		sum, err := sha256File(path)
		if err != nil {
			return err
		}
		files = append(files, tools.ManifestFile{Name: filepath.ToSlash(rel), SHA256: sum})
		return nil
	})
	if err != nil {
		return tools.Manifest{}, err
	}
	if len(files) == 0 {
		return tools.Manifest{}, fmt.Errorf("%s: no helper files", dir)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	h := sha256.New()
	for _, f := range files {
		fmt.Fprintf(h, "%s %s\n", f.Name, f.SHA256)
	}
	return tools.Manifest{Version: hex.EncodeToString(h.Sum(nil))[:versionHexLen], Files: files}, nil
}

// writeManifest writes m to dir/manifest.json.
func writeManifest(dir string, m tools.Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(filepath.Join(dir, manifestName), data, 0o644)
}

// sha256File returns the hex sha256 of the file at path.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
