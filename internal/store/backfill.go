package store

import (
	"context"
	"database/sql"
	"log"
	"time"
)

const (
	backfillKey   = "backfill_service_session_v1"
	backfillBatch = 2000                 // rowids per UPDATE
	backfillPause = 5 * time.Millisecond // yield between batches so ingest interleaves
)

// startBackfill launches the one-time migration of pre-existing rows in the
// background. The store has a single connection, so each batch is one short
// transaction; between batches the connection is free for ingest and queries.
// Close cancels it; an interrupted run is resumed (idempotently) on next start.
func (s *SQLiteStore) startBackfill() {
	ctx, cancel := context.WithCancel(context.Background())
	s.backfillCancel = cancel
	s.backfillDone = make(chan struct{})
	go func() {
		defer close(s.backfillDone)
		if err := s.backfill(ctx); err != nil && ctx.Err() == nil {
			log.Printf("backfill error: %v", err)
		}
		if ctx.Err() == nil {
			if err := s.backfillFTS(ctx); err != nil && ctx.Err() == nil {
				log.Printf("search backfill error: %v", err)
			}
		}
	}()
}

// WaitBackfill blocks until the background backfill has finished (or was
// cancelled). Intended for tests and tooling.
func (s *SQLiteStore) WaitBackfill() {
	if s.backfillDone != nil {
		<-s.backfillDone
	}
}

// backfill fills spans.service_name from resource_json and, where
// agent_session_id is empty, derives it from session.id / langfuse.session.id
// in the span attributes (then resource attributes), mirroring ingest.
//
// It walks rowid windows rather than re-querying for rows with an empty service_name so a
// span with no service.name cannot be revisited forever. The expensive part
// (JSON parsing) is skipped for rows whose attributes cannot contain a session
// key (cheap LIKE prefilter).
func (s *SQLiteStore) backfill(ctx context.Context) error {
	var done string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, backfillKey).Scan(&done)
	if err == nil && done == "1" {
		return nil
	}
	if err != nil && err != sql.ErrNoRows {
		return err
	}

	var lo, hi sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MIN(rowid), MAX(rowid) FROM spans`).Scan(&lo, &hi); err != nil {
		return err
	}
	if lo.Valid {
		start := time.Now()
		var updated int64
		for from := lo.Int64; from <= hi.Int64; from += backfillBatch {
			res, err := s.db.ExecContext(ctx, backfillSQL, from, from+backfillBatch)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			updated += n
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backfillPause):
			}
		}
		log.Printf("backfill: service_name/session ids for %d spans in %s", updated, time.Since(start).Round(time.Millisecond))
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES (?, '1')
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, backfillKey)
	return err
}

// backfillSQL updates one rowid window. Rows that already carry a service_name
// were written by the new ingest path and are skipped.
const backfillSQL = `
UPDATE spans SET
	service_name = CASE WHEN json_valid(resource_json)
		THEN COALESCE(CAST(json_extract(resource_json, '$."service.name"') AS TEXT), '')
		ELSE '' END,
	agent_session_id = CASE
		WHEN agent_session_id <> '' THEN agent_session_id
		WHEN (attributes_json LIKE '%session.id%' AND json_valid(attributes_json))
		  OR (resource_json LIKE '%session.id%' AND json_valid(resource_json)) THEN COALESCE(
			CASE WHEN json_valid(attributes_json) THEN NULLIF(CAST(json_extract(attributes_json, '$."session.id"') AS TEXT), '') END,
			CASE WHEN json_valid(resource_json) THEN NULLIF(CAST(json_extract(resource_json, '$."session.id"') AS TEXT), '') END,
			CASE WHEN json_valid(attributes_json) THEN NULLIF(CAST(json_extract(attributes_json, '$."langfuse.session.id"') AS TEXT), '') END,
			CASE WHEN json_valid(resource_json) THEN NULLIF(CAST(json_extract(resource_json, '$."langfuse.session.id"') AS TEXT), '') END,
			'')
		ELSE '' END
WHERE rowid >= ? AND rowid < ? AND service_name = ''`
