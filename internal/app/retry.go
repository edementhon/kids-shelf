package app

import (
	"database/sql"
	"encoding/json"
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
//
// Only a source's current job is retried (see jobs.RetryFailedSources),
// and not one whose source another published episode already plays: the
// retry would put the same file on the shelf twice. A retried job restarts
// with ingest.RetryPayload, so a failed improvement keeps its episode id
// and goes back to "pending" through the show phase. The requeue and the
// retried_version marker commit in one transaction: if either fails,
// nothing changes and the next launch tries again. It returns how many
// jobs went back to the queue.
func retryFailedAfterUpgrade(st *store.Store, q *jobs.Queue, version string) (int, error) {
	if !strings.HasPrefix(version, "v") {
		return 0, nil
	}
	tx, err := st.DB().Begin()
	if err != nil {
		return 0, fmt.Errorf("app: retry failed videos: %w", err)
	}
	defer tx.Rollback()
	var last string
	err = tx.QueryRow(`SELECT value FROM app_state WHERE key = 'retried_version'`).Scan(&last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("app: read retried version: %w", err)
	}
	if last == version {
		return 0, nil
	}
	n, err := q.RetryFailedSources(tx, ingest.JobKind, []string{string(core.SourceFolder), string(core.SourceNAS)},
		func(j *jobs.Job) (json.RawMessage, bool, error) {
			payload, ep, src, err := ingest.RetryPayload(j.Payload)
			if err != nil {
				return nil, false, nil // not an ingest payload: leave it failed
			}
			var other int
			err = tx.QueryRow(`SELECT COUNT(*) FROM episodes WHERE src_kind = ? AND src_path = ? AND id != ?`,
				string(src.Kind), src.Path, string(ep)).Scan(&other)
			if err != nil {
				return nil, false, err
			}
			return payload, other == 0, nil
		})
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT INTO app_state (key, value) VALUES ('retried_version', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, version); err != nil {
		return 0, fmt.Errorf("app: record retried version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("app: retry failed videos: %w", err)
	}
	return n, nil
}
