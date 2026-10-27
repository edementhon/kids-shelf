package update

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestInstallFreshClearsPointersKeepsHighWater is review round 3, section 5
// item 6: a fresh layout over retained data clears the obsolete
// trial/stage/executable pointers while preserving the monotonic version
// floor (R3).
func TestInstallFreshClearsPointersKeepsHighWater(t *testing.T) {
	dataDir := t.TempDir()
	if err := writeState(dataDir, State{
		Current:   "v1.0.0",
		Previous:  "v0.9.0",
		HighWater: "v2.0.0",
		Staged:    "v3.0.0",
		Trying:    &Trial{Version: "v3.0.0", Snapshot: "pre.db"},
	}); err != nil {
		t.Fatal(err)
	}

	if err := InstallFresh(dataDir, "v1.5.0"); err != nil {
		t.Fatalf("InstallFresh: %v", err)
	}
	st, err := ReadState(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Current != "v1.5.0" {
		t.Errorf("current = %q, want v1.5.0", st.Current)
	}
	if st.HighWater != "v2.0.0" {
		t.Errorf("high_water = %q, want v2.0.0 (the retained floor, never lowered)", st.HighWater)
	}
	if st.Previous != "" || st.Staged != "" || st.Trying != nil {
		t.Errorf("obsolete pointers not cleared: previous=%q staged=%q trying=%+v", st.Previous, st.Staged, st.Trying)
	}

	// A lower version cannot lower the floor either.
	if err := InstallFresh(dataDir, "v1.0.0"); err != nil {
		t.Fatalf("InstallFresh lower: %v", err)
	}
	st, _ = ReadState(dataDir)
	if st.HighWater != "v2.0.0" {
		t.Errorf("high_water after a lower fresh install = %q, want v2.0.0", st.HighWater)
	}
	if st.Current != "v1.0.0" {
		t.Errorf("current = %q, want v1.0.0", st.Current)
	}
}

// TestInstallFreshSeedsHighWater is review round 3 R3's one-write rule: a
// fresh install must land Current and HighWater in the same durable write,
// so a crash between publishing the root launcher and journaling leaves no
// journal whose HighWater is empty.
func TestInstallFreshSeedsHighWater(t *testing.T) {
	dataDir := t.TempDir() // no journal at all
	if err := InstallFresh(dataDir, "v1.0.0"); err != nil {
		t.Fatalf("InstallFresh: %v", err)
	}
	st, err := ReadState(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Current != "v1.0.0" || st.HighWater != "v1.0.0" {
		t.Errorf("journal = current %q high_water %q, want both v1.0.0", st.Current, st.HighWater)
	}
}

// TestInstallStageRefusesWhileTrying pins the manage.go stage gate for the
// installer: a Staged write on top of an unsettled trial would be erased by
// Confirm, so it is refused (review round 3 R2).
func TestInstallStageRefusesWhileTrying(t *testing.T) {
	dataDir := t.TempDir()
	if err := writeState(dataDir, State{Current: "v1.0.0", HighWater: "v1.0.0",
		Trying: &Trial{Version: "v2.0.0", Snapshot: "pre.db"}}); err != nil {
		t.Fatal(err)
	}

	err := InstallStage(dataDir, "v3.0.0")
	if !errors.Is(err, ErrTrialPending) {
		t.Fatalf("InstallStage while trying = %v, want ErrTrialPending", err)
	}
	st, _ := ReadState(dataDir)
	if st.Staged != "" {
		t.Errorf("staged = %q after the refusal, want unchanged", st.Staged)
	}
	if st.Trying == nil {
		t.Error("the unsettled trial was disturbed by a refused stage")
	}

	// Without a trial the stage lands.
	if err := writeState(dataDir, State{Current: "v1.0.0", HighWater: "v1.0.0"}); err != nil {
		t.Fatal(err)
	}
	if err := InstallStage(dataDir, "v3.0.0"); err != nil {
		t.Fatalf("InstallStage: %v", err)
	}
	st, _ = ReadState(dataDir)
	if st.Staged != "v3.0.0" {
		t.Errorf("staged = %q, want v3.0.0", st.Staged)
	}
}

// TestInstallCheckClassifies covers the older (below HighWater or rejected),
// same and newer cases, including the reinstall-of-the-highest-known case
// (v == HighWater > Current is newer, so it stages again).
func TestInstallCheckClassifies(t *testing.T) {
	dataDir := t.TempDir()

	// No journal: everything is newer than nothing.
	cls, err := InstallCheck(dataDir, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if cls != NewerVersion {
		t.Errorf("empty journal classifies v1.0.0 as %v, want newer", cls)
	}

	if err := writeState(dataDir, State{Current: "v2.0.0", HighWater: "v2.0.0"}); err != nil {
		t.Fatal(err)
	}
	for v, want := range map[string]VersionClass{
		"v1.0.0": OlderVersion, // below the high-water mark
		"v2.0.0": SameVersion,
		"v3.0.0": NewerVersion,
	} {
		cls, err := InstallCheck(dataDir, v)
		if err != nil {
			t.Fatalf("InstallCheck(%s): %v", v, err)
		}
		if cls != want {
			t.Errorf("InstallCheck(%s) = %v, want %v", v, cls, want)
		}
	}

	// Current rolled back below HighWater (a trial confirmed then a newer
	// install replaced it): the high-water version itself is still newer
	// than Current, so reinstalling it stages.
	if err := writeState(dataDir, State{Current: "v2.0.0", HighWater: "v3.0.0"}); err != nil {
		t.Fatal(err)
	}
	if cls, _ := InstallCheck(dataDir, "v2.0.0"); cls != OlderVersion {
		t.Errorf("v2.0.0 below high_water v3.0.0 = %v, want older", cls)
	}
	if cls, _ := InstallCheck(dataDir, "v3.0.0"); cls != NewerVersion {
		t.Errorf("v3.0.0 == high_water != current = %v, want newer", cls)
	}
	if cls, _ := InstallCheck(dataDir, "v4.0.0"); cls != NewerVersion {
		t.Errorf("v4.0.0 above high_water = %v, want newer", cls)
	}

	// A rejected version is older no matter where it sits.
	if err := writeState(dataDir, State{Current: "v2.0.0", HighWater: "v2.0.0", Rejected: []string{"v3.0.0"}}); err != nil {
		t.Fatal(err)
	}
	if cls, _ := InstallCheck(dataDir, "v3.0.0"); cls != OlderVersion {
		t.Errorf("rejected v3.0.0 = %v, want older", cls)
	}
}

// TestInstallStateValid checks the validity rule: the root launcher and
// versions/<Current> must both exist; an empty Current or a missing piece
// means "not a valid installation" (fresh).
func TestInstallStateValid(t *testing.T) {
	install := t.TempDir()
	dataDir := t.TempDir()

	// Missing journal: not valid, no error.
	valid, current, err := InstallState(install, dataDir)
	if err != nil || valid || current != "" {
		t.Fatalf("no journal: valid=%v current=%q err=%v, want false \"\" nil", valid, current, err)
	}

	if err := writeState(dataDir, State{Current: "v1.0.0", HighWater: "v1.0.0"}); err != nil {
		t.Fatal(err)
	}
	// Root launcher present but no version build: not valid.
	if err := os.WriteFile(EntryPath(install), []byte("launcher"), 0o755); err != nil {
		t.Fatal(err)
	}
	if valid, current, _ := InstallState(install, dataDir); valid {
		t.Errorf("launcher without versions/%s: valid=%v current=%q, want false", "v1.0.0", valid, current)
	}

	// Both present: valid.
	bin := VersionBinary(install, "v1.0.0")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("child"), 0o755); err != nil {
		t.Fatal(err)
	}
	valid, current, err = InstallState(install, dataDir)
	if err != nil || !valid || current != "v1.0.0" {
		t.Errorf("complete layout: valid=%v current=%q err=%v, want true v1.0.0 nil", valid, current, err)
	}

	// An empty Current with files on disk is still not valid.
	if err := writeState(dataDir, State{}); err != nil {
		t.Fatal(err)
	}
	if valid, _, _ := InstallState(install, dataDir); valid {
		t.Error("empty Current journaled over a complete layout is still valid; want false")
	}

	// A corrupt journal surfaces the read error.
	if err := os.WriteFile(StatePath(dataDir), []byte("{nonsense"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := InstallState(install, dataDir); err == nil {
		t.Error("corrupt journal: expected an error")
	}
}
