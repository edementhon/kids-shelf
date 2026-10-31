package update

import (
	"errors"
	"fmt"
	"os"
)

// ErrTrialPending reports that the installer refused to stage a version on
// top of an unsettled trial. Staging would be lost the moment the trial
// settles (Confirm clears Staged), so the installer leaves the journal alone
// and lets the launcher finish the trial first, exactly like the app-side
// updater refuses to stage while a trial exists (manage.go).
var ErrTrialPending = errors.New("Kids Shelf is finishing an update; nothing new is staged until it settles")

// ValidVersion reports whether v is a canonical vMAJOR.MINOR.PATCH release
// version, the only name the installer may publish under versions/.
func ValidVersion(v string) bool { return validVersion(v) }

// SyncParent is the durable-publication half of a rename: it fsyncs the
// directory containing path so the rename survives a crash (no-op on
// Windows, where the rename is already durable). The install flow uses it
// after publishing a version directory or replacing the root launcher.
func SyncParent(path string) error { return syncParent(path) }

// ReplaceFile atomically writes data to path (a temp file in the same
// directory, fsynced and renamed over, parent fsynced), the journal's
// durability pattern. The installer reuses it for files that must survive
// a crash but live outside the journal (the removal request).
func ReplaceFile(path string, data []byte, mode os.FileMode) error {
	return replaceFile(path, data, mode)
}

// InstallState reports whether installDir holds a valid installation for the
// journal in dataDir: the root launcher and versions/<Current> must both
// exist (PLAN 1a "Install, reinstall, remove"). Anything else - a missing
// journal, an empty Current, a layout the installer did not make - is not a
// valid installation and is treated as fresh.
func InstallState(installDir, dataDir string) (valid bool, current string, err error) {
	st, err := ReadState(dataDir)
	if err != nil {
		return false, "", err
	}
	if st.Current == "" {
		return false, "", nil
	}
	if _, err := os.Stat(EntryPath(installDir)); err != nil {
		return false, st.Current, nil
	}
	if _, err := os.Stat(VersionBinary(installDir, st.Current)); err != nil {
		return false, st.Current, nil
	}
	return true, st.Current, nil
}

// VersionClass is the installer's view of the running build's version
// against the journal.
type VersionClass int

const (
	// OlderVersion: below the highest version ever run, or rejected.
	OlderVersion VersionClass = iota
	// SameVersion: the currently confirmed version.
	SameVersion
	// NewerVersion: newer than what is running now.
	NewerVersion
)

// InstallCheck classifies v against the journal: older (v < HighWater or v
// already rejected), the same as Current, or newer. The classification
// drives the installer's journal step; an equal-to-HighWater version that is
// not Current counts as newer, so reinstalling the newest known version
// stages it again.
func InstallCheck(dataDir, v string) (VersionClass, error) {
	st, err := ReadState(dataDir)
	if err != nil {
		return 0, err
	}
	for _, r := range st.Rejected {
		if r == v {
			return OlderVersion, nil
		}
	}
	if st.HighWater != "" && st.HighWater != v && !versionAbove(v, st.HighWater) {
		return OlderVersion, nil
	}
	if v == st.Current {
		return SameVersion, nil
	}
	return NewerVersion, nil
}

// InstallFresh journals a fresh installation of v over whatever the data
// directory retained (review round 3, section 5 item 6): Current = v,
// HighWater = max(HighWater, v), and the stale Staged, Trying and Previous
// pointers cleared - the monotonic version floor survives, the executable
// pointers do not. One durable journal write, so a crash cannot split the
// transition.
func InstallFresh(dataDir, v string) error {
	st, err := ReadState(dataDir)
	if err != nil {
		return err
	}
	st.Current = v
	if versionAbove(v, st.HighWater) {
		st.HighWater = v
	}
	st.Previous = ""
	st.Staged = ""
	st.Trying = nil
	if err := writeState(dataDir, st); err != nil {
		return fmt.Errorf("update: journaling the fresh install of %s: %w", v, err)
	}
	return nil
}

// InstallStage journals Staged = v for a valid installation whose running
// build is newer than Current (the normal activation and trial run it). It
// refuses while a trial exists - staging would be silently discarded when
// the trial confirms - the same invariant the app-side updater enforces
// (manage.go).
func InstallStage(dataDir, v string) error {
	st, err := ReadState(dataDir)
	if err != nil {
		return err
	}
	if st.Trying != nil {
		return fmt.Errorf("%w: %s is being health-checked", ErrTrialPending, st.Trying.Version)
	}
	st.Staged = v
	if err := writeState(dataDir, st); err != nil {
		return fmt.Errorf("update: journaling the staged install of %s: %w", v, err)
	}
	return nil
}
