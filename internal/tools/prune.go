package tools

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
)

// helperSetName matches the directories Extract creates under
// <data>/helpers: a manifest Version, the first 16 hex characters of the
// bundle's sha256 (internal/bundle/manifestgen).
var helperSetName = regexp.MustCompile(`^[0-9a-f]{16}$`)

// PruneOtherSets removes the helper sets earlier builds extracted under
// <data>/helpers, keeping keep (the running build's manifest Version).
// Only directories named like a helper set are touched: the yt-dlp folder
// and anything else stay, and nothing happens unless keep itself is a
// helper set name. It reports how many sets it removed and the bytes they
// held; removal is best effort (a helper still running on Windows stays
// until a later run).
func PruneOtherSets(dataDir, keep string) (removed int, freed int64, err error) {
	if !helperSetName.MatchString(keep) {
		return 0, 0, nil
	}
	root := filepath.Join(dataDir, helpersSubdir)
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	var errs []error
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || name == keep || !helperSetName.MatchString(name) {
			continue
		}
		dir := filepath.Join(root, name)
		var n int64
		_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, werr error) error {
			if werr == nil && d.Type().IsRegular() {
				if info, ierr := d.Info(); ierr == nil {
					n += info.Size()
				}
			}
			return nil
		})
		if rerr := os.RemoveAll(dir); rerr != nil {
			errs = append(errs, rerr)
			continue
		}
		removed++
		freed += n
	}
	return removed, freed, errors.Join(errs...)
}
