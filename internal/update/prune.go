package update

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Pruned counts what Prune removed.
type Pruned struct {
	Versions  int   // version builds under <install>/versions
	Snapshots int   // pre-update database snapshots under <data>/update
	Bytes     int64 // bytes freed, as measured before removal
}

// Prune deletes what earlier updates left behind and nothing it cannot
// prove is dead (Eric, 2026-10-31: "Don't delete anything you're not sure
// about"):
//
//   - <install>/versions/<v> and <v>.tmp for a canonical version v older
//     than the confirmed current version. Nothing runs or rolls back to
//     them: only a trial rolls back, and it rolls back to current.
//   - <data>/update/pre-<v>.db for a canonical v no newer than current:
//     the snapshot taken before activating an update that has since been
//     confirmed (or that lost its trial to an older current).
//
// It does nothing at all while a trial is open or before any version has
// been confirmed, and it never touches the current version, anything newer
// (a staged build, or one the app is staging right now), names that are
// not canonical versions, or any other file. Removal is best effort: a
// file still in use (Windows) stays and goes on a later run; the joined
// error reports what could not be removed.
func Prune(installDir, dataDir string) (Pruned, error) {
	var p Pruned
	st, err := ReadState(dataDir)
	if err != nil {
		return p, fmt.Errorf("update: reading the journal: %w", err)
	}
	if st.Trying != nil || !validVersion(st.Current) {
		return p, nil
	}
	older := func(v string) bool { return validVersion(v) && versionAbove(st.Current, v) }

	var errs []error
	remove := func(path string) bool {
		n := sizeOf(path)
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, err)
			return false
		}
		p.Bytes += n
		return true
	}

	if installDir != "" {
		entries, err := os.ReadDir(filepath.Join(installDir, "versions"))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			v := strings.TrimSuffix(name, ".tmp")
			if older(v) && remove(filepath.Join(installDir, "versions", name)) && v == name {
				p.Versions++
			}
		}
	}

	updateDir := filepath.Join(dataDir, "update")
	entries, err := os.ReadDir(updateDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, err)
	}
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasPrefix(name, "pre-") || !strings.HasSuffix(name, ".db") {
			continue
		}
		v := strings.TrimSuffix(strings.TrimPrefix(name, "pre-"), ".db")
		if (v == st.Current || older(v)) && remove(filepath.Join(updateDir, name)) {
			p.Snapshots++
		}
	}
	return p, errors.Join(errs...)
}

// sizeOf sums the regular files under path (or path itself); it reports
// what a removal frees and is only used for the log line.
func sizeOf(path string) int64 {
	var n int64
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, ierr := d.Info(); ierr == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}
