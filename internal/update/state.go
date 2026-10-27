package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// State is the durable update journal, <data>/update/state.json. The
// launcher owns it once an instance lock is held; the app reads it to
// decide what to check for and stages into it.
type State struct {
	Current   string   `json:"current"`            // last confirmed version ("" = the launcher build itself)
	Previous  string   `json:"previous"`           // version that ran before Current
	HighWater string   `json:"high_water"`         // highest version ever confirmed
	Staged    string   `json:"staged,omitempty"`   // staged version, not yet activated
	Trying    *Trial   `json:"trying,omitempty"`   // version being health-checked right now
	Rejected  []string `json:"rejected,omitempty"` // versions that failed their trial, never retried
}

// Trial is one activated-but-unconfirmed update: the snapshot taken before
// the version ran, so a failed trial can restore the database.
type Trial struct {
	Version  string `json:"version"`
	Snapshot string `json:"snapshot"`
	Started  string `json:"started"`
}

// StatePath returns the journal path for the given data directory.
func StatePath(dataDir string) string {
	return filepath.Join(dataDir, "update", "state.json")
}

// ReadState returns the journal; a missing file is the zero state (nothing
// has ever been activated).
func ReadState(dataDir string) (State, error) {
	var st State
	b, err := os.ReadFile(StatePath(dataDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return st, nil
		}
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("update: reading %s: %w", StatePath(dataDir), err)
	}
	return st, nil
}

// writeState atomically replaces the journal (temp + fsync + rename),
// creating the update directory on first use.
func writeState(dataDir string, st State) error {
	if st.Trying != nil && st.Trying.Started == "" {
		st.Trying.Started = time.Now().UTC().Format(time.RFC3339)
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	path := StatePath(dataDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return replaceFile(path, append(b, '\n'), 0o600)
}
