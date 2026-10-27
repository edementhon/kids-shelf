package app

import "github.com/edementhon/kids-shelf/internal/store"

// appMigrations run after every package's migrations (allMigrations appends
// them last), so they may read across packages' tables.
//
// v1 backfills episodes.issue and episodes.fix (library v6) for a database
// that already ran the show-first ingest before those columns existed: its
// shown episodes have empty fields although their better copies are still
// queued (an improvement job: priority 1 or more, queued, running or
// paused) or already failed (failed with shown true). Without it those
// episodes would keep trying to play their unplayable originals instead of
// saying "Getting ready". Payload keys are the ingest payload's JSON tags
// (episode_id, action, shown). Only rows still at the defaults are touched,
// and Migrate records the version, so it runs once.
var appMigrations = []store.Migration{{
	Owner:   "app",
	Version: 1,
	SQL: `
UPDATE episodes SET
	issue = CASE j.action WHEN 'audio' THEN 'sound' WHEN 'full' THEN 'picture' ELSE 'format' END,
	fix   = CASE j.state WHEN 'failed' THEN 'failed' ELSE 'pending' END
FROM (
	SELECT json_extract(payload, '$.episode_id') AS episode,
	       json_extract(payload, '$.action')     AS action,
	       state
	FROM jobs
	WHERE kind = 'ingest'
	  AND json_extract(payload, '$.action') IN ('audio', 'full', 'remux')
	  AND ((state IN ('queued', 'running', 'paused') AND priority >= 1)
	    OR (state = 'failed' AND json_extract(payload, '$.shown') = 1))
) AS j
WHERE episodes.id = j.episode
  AND episodes.app_owned = 0
  AND episodes.issue = '' AND episodes.fix = '';`,
}, {
	// v2: small key/value state the app keeps for itself; first key
	// "retried_version" (retryFailedAfterUpgrade).
	Owner:   "app",
	Version: 2,
	SQL: `
CREATE TABLE app_state (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);`,
}}
