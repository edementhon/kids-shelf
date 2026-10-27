package app

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/edementhon/kids-shelf/internal/core"
	"github.com/edementhon/kids-shelf/internal/ingest"
	"github.com/edementhon/kids-shelf/internal/jobs"
	"github.com/edementhon/kids-shelf/internal/store"
)

// retryFailedAfterUpgrade gives failed folder and NAS videos one more try
// the first time a new app version opens the data directory: the update may
// carry the fix for what made them fail (2026-10-27: a probe that mixed
// ffprobe warnings into its JSON rejected 25 Blu-ray MKVs, and the helpers
// lacked the MP2, Vorbis and FLAC decoders), and failed folder videos have
// no Retry button of their own. YouTube downloads keep their own Retry.
// Builds without a release version (dev, test, ks-migrate) never retry.
// It returns how many jobs went back to the queue.
func retryFailedAfterUpgrade(st *store.Store, q *jobs.Queue, version string) (int, error) {
	if !strings.HasPrefix(version, "v") {
		return 0, nil
	}
	var last string
	err := st.DB().QueryRow(`SELECT value FROM app_state WHERE key = 'retried_version'`).Scan(&last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("app: read retried version: %w", err)
	}
	if last == version {
		return 0, nil
	}
	n, err := q.RetryFailedSources(ingest.JobKind, string(core.SourceFolder), string(core.SourceNAS))
	if err != nil {
		return 0, err
	}
	if _, err := st.DB().Exec(`INSERT INTO app_state (key, value) VALUES ('retried_version', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, version); err != nil {
		return n, fmt.Errorf("app: record retried version: %w", err)
	}
	return n, nil
}
