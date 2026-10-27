package update

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStateRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	st := State{
		Current:   "v1.0.0",
		Previous:  "v0.9.0",
		HighWater: "v1.0.0",
		Staged:    "v2.0.0",
		Trying:    &Trial{Version: "v2.0.0", Snapshot: "<data>/update/pre-v2.0.0.db"},
		Rejected:  []string{"v1.9.0"},
	}
	if err := writeState(dataDir, st); err != nil {
		t.Fatal(err)
	}
	got, err := ReadState(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Current != st.Current || got.Previous != st.Previous || got.HighWater != st.HighWater ||
		got.Staged != st.Staged || got.Trying == nil || got.Trying.Version != "v2.0.0" ||
		got.Trying.Snapshot != st.Trying.Snapshot || len(got.Rejected) != 1 {
		t.Errorf("round trip = %+v, want %+v", got, st)
	}
	if got.Trying.Started == "" {
		t.Error("trial started time was not stamped")
	}
	// No temp file left behind, contents are valid JSON.
	entries, err := os.ReadDir(filepath.Join(dataDir, "update"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp journal file %s left behind", e.Name())
		}
	}
	b, err := os.ReadFile(StatePath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	var decoded State
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("journal is not valid JSON: %v", err)
	}
}

func TestReadStateMissingIsZero(t *testing.T) {
	st, err := ReadState(t.TempDir())
	if err != nil {
		t.Fatalf("ReadState on a missing journal: %v", err)
	}
	if st.Staged != "" || st.Current != "" || st.HighWater != "" || st.Trying != nil || len(st.Rejected) != 0 {
		t.Errorf("zero state = %+v", st)
	}
}

func TestWriteStateOverwritesStaged(t *testing.T) {
	dataDir := t.TempDir()
	if err := writeState(dataDir, State{Staged: "v2.0.0"}); err != nil {
		t.Fatal(err)
	}
	if err := writeState(dataDir, State{Staged: "v2.0.1"}); err != nil {
		t.Fatal(err)
	}
	st, _ := ReadState(dataDir)
	if st.Staged != "v2.0.1" {
		t.Errorf("staged = %q, want v2.0.1", st.Staged)
	}
}
