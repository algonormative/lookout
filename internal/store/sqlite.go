package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// SQLiteStore implements Store using modernc.org/sqlite.
type SQLiteStore struct {
	db         *sql.DB
	opts       Options
	lastVacuum time.Time // only touched by the single retention goroutine

	backfillCancel context.CancelFunc
	backfillDone   chan struct{} // closed when the background backfill returns
}

// NewSQLiteStore opens or creates a SQLite database at the given path.
//
// An optional Options value enables the size cap, span archive, etc.; without
// one the store behaves as plain retention-only storage.
func NewSQLiteStore(dbPath string, opts ...Options) (*SQLiteStore, error) {
	// modernc.org/sqlite takes pragmas as _pragma=name(value); the mattn-style
	// _busy_timeout/_journal_mode keys are silently ignored, which left writes
	// failing at once with SQLITE_BUSY whenever a host-side reader held the file.
	// The journal stays in rollback (DELETE) mode on purpose: the writer runs in
	// Docker's Linux VM and readers on the macOS host, and WAL's shared-memory
	// index is not coherent across that file share.
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)

	s := &SQLiteStore{db: db}
	if len(opts) > 0 {
		s.opts = opts[0]
	}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	s.startBackfill()
	return s, nil
}

func (s *SQLiteStore) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS spans (
			trace_id          TEXT NOT NULL,
			span_id           TEXT NOT NULL PRIMARY KEY,
			parent_span_id    TEXT NOT NULL DEFAULT '',
			name              TEXT NOT NULL,
			kind              INTEGER NOT NULL DEFAULT 0,
			start_time_unix_nano INTEGER NOT NULL,
			end_time_unix_nano   INTEGER NOT NULL,
			attributes_json   TEXT NOT NULL DEFAULT '{}',
			resource_json     TEXT NOT NULL DEFAULT '{}',
			status_code       INTEGER NOT NULL DEFAULT 0,
			status_message    TEXT NOT NULL DEFAULT '',
			ai_system         TEXT NOT NULL DEFAULT '',
			ai_model          TEXT NOT NULL DEFAULT '',
			ai_input_tokens   INTEGER NOT NULL DEFAULT 0,
			ai_output_tokens  INTEGER NOT NULL DEFAULT 0,
			agent_name        TEXT NOT NULL DEFAULT '',
			agent_task_id     TEXT NOT NULL DEFAULT '',
			agent_session_id  TEXT NOT NULL DEFAULT '',
			service_name      TEXT NOT NULL DEFAULT '',
			events_json       TEXT NOT NULL DEFAULT '[]',
			cost_usd          REAL NOT NULL DEFAULT 0,
			tokens_per_sec    REAL NOT NULL DEFAULT 0,
			anomaly           TEXT NOT NULL DEFAULT '',
			inserted_at       TEXT NOT NULL DEFAULT (datetime('now'))
		);

		CREATE INDEX IF NOT EXISTS idx_spans_trace_id ON spans(trace_id);
		CREATE INDEX IF NOT EXISTS idx_spans_start_time ON spans(start_time_unix_nano);
		CREATE INDEX IF NOT EXISTS idx_spans_ai_model ON spans(ai_model);
		CREATE INDEX IF NOT EXISTS idx_spans_agent_name ON spans(agent_name);
		CREATE INDEX IF NOT EXISTS idx_spans_agent_session_id ON spans(agent_session_id);
		CREATE INDEX IF NOT EXISTS idx_spans_status_code ON spans(status_code);
		CREATE INDEX IF NOT EXISTS idx_spans_anomaly ON spans(anomaly) WHERE anomaly != '';

		CREATE TABLE IF NOT EXISTS metric_rollups (
			name         TEXT    NOT NULL,
			labels_json  TEXT    NOT NULL DEFAULT '{}',
			bucket_start INTEGER NOT NULL,
			bucket_width INTEGER NOT NULL,
			metric_type  TEXT    NOT NULL,
			count        INTEGER NOT NULL DEFAULT 0,
			sum          REAL    NOT NULL DEFAULT 0,
			min          REAL    NOT NULL DEFAULT 0,
			max          REAL    NOT NULL DEFAULT 0,
			last         REAL    NOT NULL DEFAULT 0,
			PRIMARY KEY (name, labels_json, bucket_start, bucket_width)
		);
	`)
	if err != nil {
		return err
	}

	// Migration: add events_json column if missing
	s.db.Exec(`ALTER TABLE spans ADD COLUMN events_json TEXT NOT NULL DEFAULT '[]'`)

	// Migration: spans.service_name (constant default, so O(1) on existing
	// databases; the index build is a single table scan at startup, before the
	// receivers listen). Existing rows are filled by the batched backfill.
	s.db.Exec(`ALTER TABLE spans ADD COLUMN service_name TEXT NOT NULL DEFAULT ''`)
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_spans_service_name ON spans(service_name)`); err != nil {
		return err
	}

	// Migration: Langfuse identity columns (constant defaults, O(1)); existing
	// rows are filled by the batched backfill.
	for _, col := range []string{"user_id", "trace_name", "obs_type"} {
		s.db.Exec(`ALTER TABLE spans ADD COLUMN ` + col + ` TEXT NOT NULL DEFAULT ''`)
	}
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_spans_user_id ON spans(user_id) WHERE user_id != ''`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_spans_trace_name ON spans(trace_name) WHERE trace_name != ''`); err != nil {
		return err
	}
	if err := s.migratePayloads(); err != nil {
		return err
	}
	if err := s.migrateFTS(); err != nil {
		return err
	}
	return s.migrateScores()
}

func (s *SQLiteStore) Close() error {
	if s.backfillCancel != nil {
		s.backfillCancel()
		<-s.backfillDone
	}
	return s.db.Close()
}

func (s *SQLiteStore) InsertSpans(ctx context.Context, spans []SpanRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// A re-sent span replaces the old row. The delete is explicit (rather than
	// INSERT OR REPLACE) so delete triggers fire and dependent rows (payloads)
	// are cleaned up.
	del, err := tx.PrepareContext(ctx, `DELETE FROM spans WHERE span_id = ?`)
	if err != nil {
		return err
	}
	defer del.Close()
	putPayload, err := tx.PrepareContext(ctx, `INSERT INTO span_payloads(span_id, codec, data) VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer putPayload.Close()

	indexStmts, err := prepareIndex(ctx, tx)
	if err != nil {
		return err
	}
	defer indexStmts.close()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO spans (
			trace_id, span_id, parent_span_id, name, kind,
			start_time_unix_nano, end_time_unix_nano,
			attributes_json, resource_json,
			status_code, status_message,
			ai_system, ai_model, ai_input_tokens, ai_output_tokens,
			agent_name, agent_task_id, agent_session_id, service_name,
			events_json, cost_usd, tokens_per_sec, anomaly, inserted_at,
			user_id, trace_name, obs_type
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for i := range spans {
		sp := &spans[i]
		eventsJSON := sp.EventsJSON
		if eventsJSON == "" {
			eventsJSON = "[]"
		}
		fullEvents := eventsJSON
		attrsJSON, eventsJSON, payload, err := packSpan(sp.AttributesJSON, eventsJSON, s.opts.CompressMinBytes)
		if err != nil {
			return fmt.Errorf("compress span %s: %w", sp.SpanID, err)
		}
		if _, err := del.ExecContext(ctx, sp.SpanID); err != nil {
			return fmt.Errorf("replace span %s: %w", sp.SpanID, err)
		}
		_, err = stmt.ExecContext(ctx,
			sp.TraceID, sp.SpanID, sp.ParentSpanID, sp.Name, sp.Kind,
			sp.StartTimeUnixNano, sp.EndTimeUnixNano,
			attrsJSON, sp.ResourceJSON,
			sp.StatusCode, sp.StatusMessage,
			sp.AISystem, sp.AIModel, sp.AIInputTokens, sp.AIOutputTokens,
			sp.AgentName, sp.AgentTaskID, sp.AgentSessionID, sp.ServiceName,
			eventsJSON, sp.CostUSD, sp.TokensPerSec, sp.Anomaly,
			sp.InsertedAt.UTC().Format(time.RFC3339),
			sp.UserID, sp.TraceName, sp.ObsType,
		)
		if err != nil {
			return fmt.Errorf("insert span %s: %w", sp.SpanID, err)
		}
		if payload != nil {
			if _, err := putPayload.ExecContext(ctx, sp.SpanID, codecFlate, payload); err != nil {
				return fmt.Errorf("insert payload %s: %w", sp.SpanID, err)
			}
		}
		nameCol, body := searchColumns(sp.Name, sp.StatusMessage, sp.AttributesJSON, fullEvents)
		if err := indexSpanTx(ctx, indexStmts, sp.SpanID, nameCol, body); err != nil {
			return fmt.Errorf("index span %s: %w", sp.SpanID, err)
		}
	}

	return tx.Commit()
}

func (s *SQLiteStore) QuerySpans(ctx context.Context, filter SpanFilter) ([]SpanRecord, error) {
	where, args := buildSpanWhere(filter)
	orderBy := "start_time_unix_nano DESC"
	switch filter.SortBy {
	case "duration":
		orderBy = "(end_time_unix_nano - start_time_unix_nano) DESC"
	case "cost":
		orderBy = "cost_usd DESC"
	case "tokens":
		orderBy = "(ai_input_tokens + ai_output_tokens) DESC"
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = 20
	}

	query := fmt.Sprintf("SELECT %s FROM spans %s ORDER BY %s LIMIT ?", spanColumns, where, orderBy)
	args = append(args, limit)

	return s.querySpanRows(ctx, query, args...)
}

func (s *SQLiteStore) GetTrace(ctx context.Context, traceID string) ([]SpanRecord, error) {
	query := fmt.Sprintf("SELECT %s FROM spans WHERE trace_id = ? ORDER BY start_time_unix_nano", spanColumns)
	return s.querySpanRows(ctx, query, traceID)
}

func (s *SQLiteStore) GetStats(ctx context.Context, filter SpanFilter) (*Stats, error) {
	where, args := buildSpanWhere(filter)
	query := fmt.Sprintf(`
		SELECT
			COUNT(*) as total_spans,
			COUNT(DISTINCT trace_id) as total_traces,
			COUNT(DISTINCT CASE WHEN agent_session_id != '' THEN agent_session_id END) as total_sessions,
			COALESCE(SUM(ai_input_tokens), 0) as total_input_tokens,
			COALESCE(SUM(ai_output_tokens), 0) as total_output_tokens,
			COALESCE(SUM(cost_usd), 0) as total_cost,
			COUNT(CASE WHEN ai_model != '' THEN 1 END) as ai_span_count,
			COUNT(CASE WHEN status_code = 2 THEN 1 END) as error_count
		FROM spans %s
	`, where)

	var stats Stats
	err := s.db.QueryRowContext(ctx, query, args...).Scan(
		&stats.TotalSpans, &stats.TotalTraces, &stats.TotalSessions,
		&stats.TotalInputTokens, &stats.TotalOutputTokens,
		&stats.TotalCost, &stats.AISpanCount, &stats.ErrorCount,
	)
	if err != nil {
		return nil, err
	}
	return &stats, nil
}

func (s *SQLiteStore) GetStatsByModel(ctx context.Context, filter SpanFilter) ([]ModelStats, error) {
	where, args := buildSpanWhere(filter)
	if where == "" {
		where = "WHERE ai_model != ''"
	} else {
		where += " AND ai_model != ''"
	}

	query := fmt.Sprintf(`
		SELECT
			ai_model,
			COUNT(*) as span_count,
			COALESCE(SUM(ai_input_tokens), 0),
			COALESCE(SUM(ai_output_tokens), 0),
			COALESCE(SUM(cost_usd), 0),
			AVG(CASE WHEN end_time_unix_nano > start_time_unix_nano
				THEN (end_time_unix_nano - start_time_unix_nano) / 1e6 ELSE 0 END),
			COUNT(CASE WHEN status_code = 2 THEN 1 END)
		FROM spans %s
		GROUP BY ai_model ORDER BY span_count DESC
	`, where)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []ModelStats
	for rows.Next() {
		var ms ModelStats
		if err := rows.Scan(&ms.Model, &ms.SpanCount, &ms.InputTokens, &ms.OutputTokens,
			&ms.TotalCost, &ms.AvgDurationMs, &ms.ErrorCount); err != nil {
			return nil, err
		}
		result = append(result, ms)
	}
	return result, rows.Err()
}

func (s *SQLiteStore) GetStatsByAgent(ctx context.Context, filter SpanFilter) ([]AgentStats, error) {
	where, args := buildSpanWhere(filter)
	if where == "" {
		where = "WHERE agent_name != ''"
	} else {
		where += " AND agent_name != ''"
	}

	query := fmt.Sprintf(`
		SELECT
			agent_name,
			COUNT(*) as span_count,
			COUNT(DISTINCT CASE WHEN agent_session_id != '' THEN agent_session_id END),
			COALESCE(SUM(ai_input_tokens), 0),
			COALESCE(SUM(ai_output_tokens), 0),
			COALESCE(SUM(cost_usd), 0),
			COUNT(CASE WHEN status_code = 2 THEN 1 END)
		FROM spans %s
		GROUP BY agent_name ORDER BY span_count DESC
	`, where)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []AgentStats
	for rows.Next() {
		var as AgentStats
		if err := rows.Scan(&as.Agent, &as.SpanCount, &as.SessionCount,
			&as.InputTokens, &as.OutputTokens, &as.TotalCost, &as.ErrorCount); err != nil {
			return nil, err
		}
		result = append(result, as)
	}
	return result, rows.Err()
}

func (s *SQLiteStore) GetCostReport(ctx context.Context, filter SpanFilter, bucket string, groupBy string) ([]CostBucket, error) {
	where, args := buildSpanWhere(filter)

	var timeFmt string
	switch bucket {
	case "day":
		timeFmt = "%Y-%m-%d"
	default:
		timeFmt = "%Y-%m-%dT%H:00"
	}

	var groupCol string
	switch groupBy {
	case "agent":
		groupCol = "agent_name"
	case "model":
		groupCol = "ai_model"
	default:
		groupCol = "'all'"
	}

	query := fmt.Sprintf(`
		SELECT
			strftime('%s', datetime(start_time_unix_nano / 1000000000, 'unixepoch')) as bucket,
			%s as group_by,
			COALESCE(SUM(cost_usd), 0),
			COUNT(*)
		FROM spans %s
		GROUP BY bucket, group_by
		ORDER BY bucket
	`, timeFmt, groupCol, where)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []CostBucket
	for rows.Next() {
		var cb CostBucket
		if err := rows.Scan(&cb.Bucket, &cb.GroupBy, &cb.Cost, &cb.SpanCount); err != nil {
			return nil, err
		}
		result = append(result, cb)
	}
	return result, rows.Err()
}

func (s *SQLiteStore) GetSessions(ctx context.Context, filter SessionFilter) ([]Session, error) {
	var wheres []string
	var args []any

	wheres = append(wheres, "agent_session_id != ''")

	if filter.Agent != "" {
		wheres = append(wheres, "agent_name = ?")
		args = append(args, filter.Agent)
	}
	if !filter.Since.IsZero() {
		wheres = append(wheres, "start_time_unix_nano >= ?")
		args = append(args, filter.Since.UnixNano())
	}

	where := "WHERE " + strings.Join(wheres, " AND ")
	limit := filter.Limit
	if limit <= 0 {
		limit = 20
	}

	query := fmt.Sprintf(`
		SELECT
			agent_session_id,
			COALESCE(MAX(agent_name), '') as agent_name,
			COUNT(*) as span_count,
			COUNT(DISTINCT trace_id) as trace_count,
			datetime(MIN(start_time_unix_nano) / 1000000000, 'unixepoch') as start_time,
			datetime(MAX(end_time_unix_nano) / 1000000000, 'unixepoch') as end_time,
			COALESCE(SUM(cost_usd), 0) as total_cost,
			COALESCE(SUM(ai_input_tokens), 0) as total_input_tokens,
			COALESCE(SUM(ai_output_tokens), 0) as total_output_tokens,
			COUNT(CASE WHEN status_code = 2 THEN 1 END) as error_count
		FROM spans %s
		GROUP BY agent_session_id
		ORDER BY start_time DESC
		LIMIT ?
	`, where)
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []Session
	for rows.Next() {
		var sess Session
		var startStr, endStr string
		if err := rows.Scan(
			&sess.SessionID, &sess.AgentName, &sess.SpanCount, &sess.TraceCount,
			&startStr, &endStr,
			&sess.TotalCost, &sess.TotalInputTokens, &sess.TotalOutputTokens,
			&sess.ErrorCount,
		); err != nil {
			return nil, err
		}
		sess.StartTime, _ = time.Parse("2006-01-02 15:04:05", startStr)
		sess.EndTime, _ = time.Parse("2006-01-02 15:04:05", endStr)
		result = append(result, sess)
	}
	return result, rows.Err()
}

func (s *SQLiteStore) GetSession(ctx context.Context, sessionID string) ([]SpanRecord, error) {
	query := fmt.Sprintf("SELECT %s FROM spans WHERE agent_session_id = ? ORDER BY start_time_unix_nano", spanColumns)
	return s.querySpanRows(ctx, query, sessionID)
}

func (s *SQLiteStore) GetAnomalies(ctx context.Context, filter SpanFilter) ([]SpanRecord, error) {
	where, args := buildSpanWhere(filter)
	if where == "" {
		where = "WHERE anomaly != ''"
	} else {
		where += " AND anomaly != ''"
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = 20
	}

	query := fmt.Sprintf("SELECT %s FROM spans %s ORDER BY start_time_unix_nano DESC LIMIT ?", spanColumns, where)
	args = append(args, limit)
	return s.querySpanRows(ctx, query, args...)
}

func (s *SQLiteStore) UpsertMetricRollup(ctx context.Context, rollup MetricRollup) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO metric_rollups (name, labels_json, bucket_start, bucket_width, metric_type, count, sum, min, max, last)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name, labels_json, bucket_start, bucket_width) DO UPDATE SET
			count = count + excluded.count,
			sum = sum + excluded.sum,
			min = MIN(min, excluded.min),
			max = MAX(max, excluded.max),
			last = excluded.last
	`, rollup.Name, rollup.LabelsJSON, rollup.BucketStart, rollup.BucketWidth,
		rollup.MetricType, rollup.Count, rollup.Sum, rollup.Min, rollup.Max, rollup.Last)
	return err
}

func (s *SQLiteStore) QueryMetricRollups(ctx context.Context, filter MetricFilter) ([]MetricRollup, error) {
	var wheres []string
	var args []any

	wheres = append(wheres, "name = ?")
	args = append(args, filter.Name)

	if filter.BucketWidth > 0 {
		wheres = append(wheres, "bucket_width = ?")
		args = append(args, filter.BucketWidth)
	}
	if !filter.Since.IsZero() {
		wheres = append(wheres, "bucket_start >= ?")
		args = append(args, filter.Since.Unix())
	}
	if len(filter.Labels) > 0 {
		labelsJSON, _ := json.Marshal(filter.Labels)
		wheres = append(wheres, "labels_json = ?")
		args = append(args, string(labelsJSON))
	}

	where := "WHERE " + strings.Join(wheres, " AND ")
	query := fmt.Sprintf(`
		SELECT name, labels_json, bucket_start, bucket_width, metric_type, count, sum, min, max, last
		FROM metric_rollups %s ORDER BY bucket_start
	`, where)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []MetricRollup
	for rows.Next() {
		var m MetricRollup
		if err := rows.Scan(&m.Name, &m.LabelsJSON, &m.BucketStart, &m.BucketWidth,
			&m.MetricType, &m.Count, &m.Sum, &m.Min, &m.Max, &m.Last); err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

func (s *SQLiteStore) ListMetricNames(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT DISTINCT name FROM metric_rollups ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// Cleanup deletes spans older than the retention period, then enforces the
// size cap (Options.MaxDBBytes). It returns the number of spans deleted by
// both. Every deletion is archived first when Options.ArchiveDir is set; if the
// archive write fails nothing further is deleted and the error is returned.
func (s *SQLiteStore) Cleanup(ctx context.Context, retentionDays int) (int64, error) {
	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour).UnixNano()
	n, err := s.deleteExpired(ctx, cutoff)
	if err != nil {
		return n, err
	}

	// Also cleanup old metric rollups
	metricCutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour).Unix()
	s.db.ExecContext(ctx, "DELETE FROM metric_rollups WHERE bucket_start < ?", metricCutoff)

	capN, capErr := s.enforceCap(ctx)
	n += capN
	if n > 0 {
		s.maybeVacuum(ctx, capN > 0)
	}
	return n, capErr
}

// vacuumInterval is the minimum time between full VACUUMs.
const vacuumInterval = 24 * time.Hour

// maybeVacuum reclaims file space after a cleanup that deleted rows. SQLite
// never shrinks the file on DELETE; freed pages are only reused. Existing
// databases were created with auto_vacuum=NONE, and switching that mode itself
// requires a full VACUUM, so the simplest approach that works for old and new
// databases alike is a plain VACUUM, throttled to once per vacuumInterval
// (tracked in memory, so at most once per day per process start). This is a
// small single-user DB, so the rewrite is quick, but it holds the write lock
// (and our single connection) while it runs, so concurrent ingest briefly
// waits (busy_timeout 5s; the receivers return an error on a longer stall and
// the sender retries). The WAL is then truncated so the -wal file shrinks too.
//
// force skips the throttle; the size cap uses it because deleting for the cap
// is only useful once the file actually shrinks.
func (s *SQLiteStore) maybeVacuum(ctx context.Context, force bool) {
	if !force && !s.lastVacuum.IsZero() && time.Since(s.lastVacuum) < vacuumInterval {
		return
	}
	s.lastVacuum = time.Now()
	if _, err := s.db.ExecContext(ctx, "VACUUM"); err != nil {
		log.Printf("vacuum error: %v", err)
		return
	}
	s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
}

// Helpers

const spanColumns = `trace_id, span_id, parent_span_id, name, kind,
	start_time_unix_nano, end_time_unix_nano,
	attributes_json, resource_json,
	status_code, status_message,
	ai_system, ai_model, ai_input_tokens, ai_output_tokens,
	agent_name, agent_task_id, agent_session_id, service_name,
	events_json, cost_usd, tokens_per_sec, anomaly, inserted_at,
	user_id, trace_name, obs_type`

func (s *SQLiteStore) querySpanRows(ctx context.Context, query string, args ...any) ([]SpanRecord, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}

	var result []SpanRecord
	for rows.Next() {
		var sp SpanRecord
		var insertedStr string
		if err := rows.Scan(
			&sp.TraceID, &sp.SpanID, &sp.ParentSpanID, &sp.Name, &sp.Kind,
			&sp.StartTimeUnixNano, &sp.EndTimeUnixNano,
			&sp.AttributesJSON, &sp.ResourceJSON,
			&sp.StatusCode, &sp.StatusMessage,
			&sp.AISystem, &sp.AIModel, &sp.AIInputTokens, &sp.AIOutputTokens,
			&sp.AgentName, &sp.AgentTaskID, &sp.AgentSessionID, &sp.ServiceName,
			&sp.EventsJSON, &sp.CostUSD, &sp.TokensPerSec, &sp.Anomaly, &insertedStr,
			&sp.UserID, &sp.TraceName, &sp.ObsType,
		); err != nil {
			rows.Close()
			return nil, err
		}
		sp.InsertedAt, _ = time.Parse(time.RFC3339, insertedStr)
		result = append(result, sp)
	}
	// The cursor must be closed before restorePayloads: one connection.
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.restorePayloads(ctx, result); err != nil {
		return nil, err
	}
	return result, nil
}

func buildSpanWhere(filter SpanFilter) (string, []any) {
	var wheres []string
	var args []any

	if filter.TraceID != "" {
		wheres = append(wheres, "trace_id = ?")
		args = append(args, filter.TraceID)
	}
	if filter.Agent != "" {
		wheres = append(wheres, "agent_name = ?")
		args = append(args, filter.Agent)
	}
	if filter.Model != "" {
		wheres = append(wheres, "ai_model = ?")
		args = append(args, filter.Model)
	}
	if !filter.Since.IsZero() {
		wheres = append(wheres, "start_time_unix_nano >= ?")
		args = append(args, filter.Since.UnixNano())
	}
	if !filter.Until.IsZero() {
		wheres = append(wheres, "start_time_unix_nano <= ?")
		args = append(args, filter.Until.UnixNano())
	}
	if filter.DurationGt > 0 {
		nanos := int64(filter.DurationGt * 1e9)
		wheres = append(wheres, "(end_time_unix_nano - start_time_unix_nano) > ?")
		args = append(args, nanos)
	}
	if filter.Status != "" {
		switch filter.Status {
		case "ok":
			wheres = append(wheres, "status_code = 1")
		case "error":
			wheres = append(wheres, "status_code = 2")
		case "unset":
			wheres = append(wheres, "status_code = 0")
		}
	}
	if filter.CostGt > 0 {
		wheres = append(wheres, "cost_usd > ?")
		args = append(args, filter.CostGt)
	}

	if len(wheres) == 0 {
		return "", nil
	}
	return "WHERE " + strings.Join(wheres, " AND "), args
}
