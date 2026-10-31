package update

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// pruneFixture lays out an install and data dir the way a machine looks
// after several updates, journals st, and returns both roots.
func pruneFixture(t *testing.T, st State) (install, data string) {
	t.Helper()
	install, data = t.TempDir(), t.TempDir()
	for _, v := range []string{"v0.1.9", "v0.2.0", "v0.2.10", "v0.3.0", "v0.4.0", "v0.1.9.tmp", "v0.4.0.tmp", "notes", "v0.2"} {
		d := filepath.Join(install, "versions", v)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "kids-shelf"), make([]byte, 1000), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	upd := filepath.Join(data, "update")
	if err := os.MkdirAll(upd, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"pre-v0.1.9.db", "pre-v0.2.0.db", "pre-v0.2.10.db", "pre-v0.3.0.db", "pre-v0.4.0.db", "pre-v0.4.0.db-wal", "notes.db", "state.json.bak"} {
		if err := os.WriteFile(filepath.Join(upd, f), make([]byte, 500), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeState(data, st); err != nil {
		t.Fatal(err)
	}
	return install, data
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

// TestPruneRemovesOnlyWhatIsProvablyDead: with v0.2.10 confirmed and
// v0.3.0 staged, the builds and snapshots of older versions go (v0.2.0 is
// older than v0.2.10 by number, not by text), and everything else stays:
// the current build and its successors (staged, or being staged), names
// that are not versions, and any other file in the update folder.
func TestPruneRemovesOnlyWhatIsProvablyDead(t *testing.T) {
	install, data := pruneFixture(t, State{Current: "v0.2.10", Previous: "v0.2.0", HighWater: "v0.2.10", Staged: "v0.3.0"})
	p, err := Prune(install, data)
	if err != nil {
		t.Fatal(err)
	}
	vers := filepath.Join(install, "versions")
	for _, gone := range []string{"v0.1.9", "v0.2.0", "v0.1.9.tmp"} {
		if exists(t, filepath.Join(vers, gone)) {
			t.Errorf("versions/%s kept, want removed", gone)
		}
	}
	for _, kept := range []string{"v0.2.10", "v0.3.0", "v0.4.0", "v0.4.0.tmp", "notes", "v0.2"} {
		if !exists(t, filepath.Join(vers, kept)) {
			t.Errorf("versions/%s removed, want kept", kept)
		}
	}
	upd := filepath.Join(data, "update")
	for _, gone := range []string{"pre-v0.1.9.db", "pre-v0.2.0.db", "pre-v0.2.10.db"} {
		if exists(t, filepath.Join(upd, gone)) {
			t.Errorf("update/%s kept, want removed", gone)
		}
	}
	for _, kept := range []string{"pre-v0.3.0.db", "pre-v0.4.0.db", "pre-v0.4.0.db-wal", "notes.db", "state.json.bak", "state.json"} {
		if !exists(t, filepath.Join(upd, kept)) {
			t.Errorf("update/%s removed, want kept", kept)
		}
	}
	if p.Versions != 2 || p.Snapshots != 3 || p.Bytes != 3*1000+3*500 {
		t.Errorf("Pruned = %+v, want 2 versions, 3 snapshots, 4500 bytes", p)
	}
}

// TestPruneDoesNothingUnsettled: during a trial the running build may be
// rolled back and its snapshot restored, and with no confirmed version
// there is nothing to measure "older" against: nothing is removed.
func TestPruneDoesNothingUnsettled(t *testing.T) {
	for name, st := range map[string]State{
		"trial open":        {Current: "v0.2.10", HighWater: "v0.2.10", Trying: &Trial{Version: "v0.3.0", Snapshot: "pre-v0.3.0.db"}},
		"nothing confirmed": {},
	} {
		install, data := pruneFixture(t, st)
		p, err := Prune(install, data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if p != (Pruned{}) {
			t.Errorf("%s: removed %+v, want nothing", name, p)
		}
		if !exists(t, filepath.Join(install, "versions", "v0.1.9")) || !exists(t, filepath.Join(data, "update", "pre-v0.1.9.db")) {
			t.Errorf("%s: an old build or snapshot was removed", name)
		}
	}
}

// TestRunTidiesOnceTheRunningBuildIsConfirmed: while the running build is
// on trial nothing is removed (a rollback restores the old version and its
// snapshot); once Confirm settles it, Run removes the older builds and
// snapshots and calls Tidy, without waiting for the daily check.
func TestRunTidiesOnceTheRunningBuildIsConfirmed(t *testing.T) {
	old := BuildVersion
	BuildVersion = "v0.3.0"
	t.Cleanup(func() { BuildVersion = old })
	install, data := pruneFixture(t, State{Current: "v0.2.10", HighWater: "v0.2.10",
		Trying: &Trial{Version: "v0.3.0", Snapshot: "pre-v0.3.0.db"}})

	tidied := make(chan struct{}, 1)
	m := &Manager{DataDir: data, Install: install, BaseURL: "http://127.0.0.1:1/unreachable",
		CheckEvery: time.Hour, SettleEvery: 5 * time.Millisecond,
		Tidy: func() { tidied <- struct{}{} }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	// Run reads BuildVersion until it returns; stop it before the cleanup
	// above restores the variable.
	defer func() { cancel(); <-done }()

	time.Sleep(50 * time.Millisecond)
	if !exists(t, filepath.Join(install, "versions", "v0.2.10")) || !exists(t, filepath.Join(data, "update", "pre-v0.3.0.db")) {
		t.Fatal("removed during the trial")
	}
	select {
	case <-tidied:
		t.Fatal("Tidy ran during the trial")
	default:
	}

	if err := Confirm(data, "v0.3.0"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tidied:
	case <-time.After(5 * time.Second):
		t.Fatal("no tidy after the trial was confirmed")
	}
	for _, gone := range []string{filepath.Join(install, "versions", "v0.2.10"), filepath.Join(data, "update", "pre-v0.3.0.db")} {
		if exists(t, gone) {
			t.Errorf("%s kept after the confirmation", gone)
		}
	}
	if !exists(t, filepath.Join(install, "versions", "v0.3.0")) {
		t.Error("the running build was removed")
	}
}
