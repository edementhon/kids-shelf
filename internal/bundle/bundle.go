// Package bundle embeds the platform's helper binaries (ffmpeg, ffprobe,
// qjs, kiwix-serve) and their manifest into release binaries. A release
// build compiles the per-target file embed_<goos>_<goarch>.go (build tag
// "bundle"), which embeds internal/bundle/files/<goos>_<goarch>/ and
// registers it here; every other build registers nothing, so FS() returns
// nil and Manifest() the zero value, and the helpers come from the config
// directory or $KS_HELPERS_DIR exactly as in development.
package bundle

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"runtime"

	"github.com/edementhon/kids-shelf/internal/tools"
)

// manifestName is the manifest file inside each helper directory, written
// by internal/bundle/manifestgen.
const manifestName = "manifest.json"

// fsys is the embedded helper directory (files/<goos>_<goarch>) of a bundle
// build, nil otherwise.
var fsys fs.FS

// manifest is the decoded helper manifest of a bundle build, the zero value
// otherwise.
var manifest tools.Manifest

// FS returns the embedded helper directory, or nil when the binary was not
// built with the bundle tag.
func FS() fs.FS {
	return fsys
}

// Manifest returns the embedded helper manifest, or the zero value when the
// binary was not built with the bundle tag. Its Version names the
// extraction directory <data>/helpers/<version> (internal/tools).
func Manifest() tools.Manifest {
	return manifest
}

// registerEmbedded installs the helper directory one of the per-target
// embed files embedded. A release binary whose embedded helpers are missing
// or whose manifest does not parse is a build bug, so any such problem
// panics here, at init, instead of silently shipping broken helpers.
func registerEmbedded(embedded embed.FS) {
	root := "files/" + runtime.GOOS + "_" + runtime.GOARCH
	sub, err := fs.Sub(embedded, root)
	if err != nil {
		panic(fmt.Sprintf("bundle: embedded helpers: %v", err))
	}
	data, err := fs.ReadFile(sub, manifestName)
	if err != nil {
		panic(fmt.Sprintf("bundle: embedded helpers: %v", err))
	}
	var m tools.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		panic(fmt.Sprintf("bundle: embedded helpers: malformed manifest: %v", err))
	}
	fsys = sub
	manifest = m
}
