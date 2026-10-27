package apptest

import (
	"fmt"
	"os"

	"github.com/edementhon/kids-shelf/internal/store"
)

// writeRow opens the database the way a new app version with a schema
// change does: it creates a table the old snapshot does not have and writes
// a row into it. The launcher's rollback must restore the snapshot, so
// after it neither the table nor the row exists.
func writeRow(db string) {
	s, err := store.Open(db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake app: opening the database: %v\n", err)
		os.Exit(2)
	}
	defer s.Close()
	if _, err := s.DB().Exec(`CREATE TABLE IF NOT EXISTS probe_v2 (v TEXT)`); err != nil {
		fmt.Fprintf(os.Stderr, "fake app: migrating: %v\n", err)
		os.Exit(2)
	}
	if _, err := s.DB().Exec(`INSERT INTO probe_v2 (v) VALUES ('new')`); err != nil {
		fmt.Fprintf(os.Stderr, "fake app: writing: %v\n", err)
		os.Exit(2)
	}
}
