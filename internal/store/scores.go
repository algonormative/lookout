package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrInvalidInput marks caller errors (bad score, bad cursor, bad filter) so
// the API layer can answer 400 instead of 500. Test with errors.Is.
var ErrInvalidInput = errors.New("invalid input")

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidInput, fmt.Sprintf(format, args...))
}

// Score data types (Langfuse names).
const (
	ScoreNumeric     = "NUMERIC"
	ScoreBoolean     = "BOOLEAN"
	ScoreCategorical = "CATEGORICAL"
)

// Score is a verdict posted back to lookout by a judge process.
type Score struct {
	ID        string `json:"id"`
	TraceID   string `json:"trace_id"`
	SpanID    string `json:"span_id,omitempty"` // empty = trace-level
	SessionID string `json:"session_id,omitempty"`
	Name      string `json:"name"`
	DataType  string `json:"data_type"`
	// ValueNum holds NUMERIC values, BOOLEAN as 0/1, and (optionally) a
	// probability/confidence alongside a CATEGORICAL label.
	ValueNum     *float64        `json:"value_num,omitempty"`
	ValueStr     string          `json:"value_str,omitempty"` // CATEGORICAL label
	Source       string          `json:"source"`
	JudgeVersion string          `json:"judge_version,omitempty"`
	Comment      string          `json:"comment,omitempty"`
	Metadata     json.RawMessage `json:"metadata,omitempty"`
	CreatedAt    int64           `json:"created_at"` // unix ns, set on first insert
}

// ScoreFilter selects scores. Zero values are ignored.
type ScoreFilter struct {
	TraceID   string
	SpanID    string
	SessionID string
	Name      string
	Source    string
	DataType  string
	Since     time.Time
	Asc       bool   // oldest first; default newest first
	Cursor    string // opaque, from a previous page's next cursor
	Offset    int    // used instead of Cursor by the page-numbered Langfuse alias
	Limit     int    // default 50, max 1000
}

const (
	defaultScoreLimit = 50
	maxScoreLimit     = 1000
)

// Normalize validates the score and fills defaults that need no database
// (DataType case, CreatedAt is set by the store). It does not generate IDs.
func (sc *Score) Normalize() error {
	sc.DataType = strings.ToUpper(strings.TrimSpace(sc.DataType))
	switch {
	case sc.TraceID == "":
		return invalidf("trace_id is required")
	case sc.Name == "":
		return invalidf("name is required")
	case sc.Source == "":
		return invalidf("source is required")
	}
	hasStr := sc.ValueStr != ""
	switch sc.DataType {
	case ScoreNumeric:
		if sc.ValueNum == nil {
			return invalidf("NUMERIC score needs value_num")
		}
		if hasStr {
			return invalidf("NUMERIC score must not carry value_str")
		}
	case ScoreBoolean:
		if sc.ValueNum == nil || (*sc.ValueNum != 0 && *sc.ValueNum != 1) {
			return invalidf("BOOLEAN score needs value_num of 0 or 1")
		}
		if hasStr {
			return invalidf("BOOLEAN score must not carry value_str")
		}
	case ScoreCategorical:
		if !hasStr {
			return invalidf("CATEGORICAL score needs value_str")
		}
	case "":
		return invalidf("data_type is required (NUMERIC, BOOLEAN or CATEGORICAL)")
	default:
		return invalidf("unknown data_type %q (want NUMERIC, BOOLEAN or CATEGORICAL)", sc.DataType)
	}
	if sc.ValueNum != nil && (*sc.ValueNum != *sc.ValueNum) { // NaN
		return invalidf("value_num must be a finite number")
	}
	if len(sc.Metadata) > 0 {
		trimmed := strings.TrimSpace(string(sc.Metadata))
		if trimmed == "null" {
			sc.Metadata = nil
		} else if !json.Valid(sc.Metadata) || !strings.HasPrefix(trimmed, "{") {
			return invalidf("metadata must be a JSON object")
		}
	}
	return nil
}

func newScoreID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

func (s *SQLiteStore) migrateScores() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS meta (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS scores (
			id            TEXT PRIMARY KEY,
			trace_id      TEXT NOT NULL,
			span_id       TEXT,
			session_id    TEXT,
			name          TEXT NOT NULL,
			data_type     TEXT NOT NULL,
			value_num     REAL,
			value_str     TEXT,
			source        TEXT NOT NULL,
			judge_version TEXT,
			comment       TEXT,
			metadata_json TEXT,
			created_at    INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_scores_trace_id ON scores(trace_id);
		CREATE INDEX IF NOT EXISTS idx_scores_name_source ON scores(name, source);
		CREATE INDEX IF NOT EXISTS idx_scores_created_at ON scores(created_at);
	`)
	return err
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// UpsertScores validates every score first, then writes them in one
// transaction (all or nothing). Scores are upserted by id: an empty id is
// generated, and re-posting an id replaces the row but keeps its original
// created_at. An empty session_id is copied from the trace's spans when any
// span of the trace carries one. The scores slice is updated in place with the
// generated ids / created_at / session_id; the ids are returned in order.
func (s *SQLiteStore) UpsertScores(ctx context.Context, scores []Score) ([]string, error) {
	if len(scores) == 0 {
		return nil, invalidf("no scores")
	}
	for i := range scores {
		if err := scores[i].Normalize(); err != nil {
			if len(scores) > 1 {
				return nil, fmt.Errorf("scores[%d]: %w", i, err)
			}
			return nil, err
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	upsert, err := tx.PrepareContext(ctx, `
		INSERT INTO scores (id, trace_id, span_id, session_id, name, data_type,
			value_num, value_str, source, judge_version, comment, metadata_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			trace_id = excluded.trace_id, span_id = excluded.span_id,
			session_id = excluded.session_id, name = excluded.name,
			data_type = excluded.data_type, value_num = excluded.value_num,
			value_str = excluded.value_str, source = excluded.source,
			judge_version = excluded.judge_version, comment = excluded.comment,
			metadata_json = excluded.metadata_json`)
	if err != nil {
		return nil, err
	}
	defer upsert.Close()

	now := time.Now().UnixNano()
	sessionByTrace := map[string]string{}
	ids := make([]string, len(scores))
	for i := range scores {
		sc := &scores[i]
		if sc.ID == "" {
			sc.ID = newScoreID()
		}
		if sc.SessionID == "" {
			sid, ok := sessionByTrace[sc.TraceID]
			if !ok {
				err := tx.QueryRowContext(ctx,
					`SELECT agent_session_id FROM spans WHERE trace_id = ? AND agent_session_id <> '' LIMIT 1`,
					sc.TraceID).Scan(&sid)
				if err != nil && err != sql.ErrNoRows {
					return nil, err
				}
				sessionByTrace[sc.TraceID] = sid
			}
			sc.SessionID = sid
		}
		sc.CreatedAt = now
		var num any
		if sc.ValueNum != nil {
			num = *sc.ValueNum
		}
		var meta any
		if len(sc.Metadata) > 0 {
			meta = string(sc.Metadata)
		}
		if _, err := upsert.ExecContext(ctx, sc.ID, sc.TraceID, nullStr(sc.SpanID), nullStr(sc.SessionID),
			sc.Name, sc.DataType, num, nullStr(sc.ValueStr), sc.Source,
			nullStr(sc.JudgeVersion), nullStr(sc.Comment), meta, sc.CreatedAt); err != nil {
			return nil, fmt.Errorf("upsert score %s: %w", sc.ID, err)
		}
		// A re-posted id keeps its original created_at; reflect that.
		if err := tx.QueryRowContext(ctx, `SELECT created_at FROM scores WHERE id = ?`, sc.ID).
			Scan(&sc.CreatedAt); err != nil {
			return nil, err
		}
		ids[i] = sc.ID
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

const scoreColumns = `id, trace_id, span_id, session_id, name, data_type, value_num,
	value_str, source, judge_version, comment, metadata_json, created_at`

func scanScore(sc *Score, scan func(...any) error) error {
	var span, sess, valStr, jv, comment, meta sql.NullString
	var num sql.NullFloat64
	if err := scan(&sc.ID, &sc.TraceID, &span, &sess, &sc.Name, &sc.DataType, &num,
		&valStr, &sc.Source, &jv, &comment, &meta, &sc.CreatedAt); err != nil {
		return err
	}
	sc.SpanID, sc.SessionID, sc.ValueStr = span.String, sess.String, valStr.String
	sc.JudgeVersion, sc.Comment = jv.String, comment.String
	if num.Valid {
		v := num.Float64
		sc.ValueNum = &v
	}
	if meta.Valid && meta.String != "" {
		sc.Metadata = json.RawMessage(meta.String)
	}
	return nil
}

func buildScoreWhere(f ScoreFilter) (string, []any) {
	var wheres []string
	var args []any
	add := func(col, v string) {
		if v != "" {
			wheres = append(wheres, col+" = ?")
			args = append(args, v)
		}
	}
	add("trace_id", f.TraceID)
	add("span_id", f.SpanID)
	add("session_id", f.SessionID)
	add("name", f.Name)
	add("source", f.Source)
	add("data_type", strings.ToUpper(f.DataType))
	if !f.Since.IsZero() {
		wheres = append(wheres, "created_at >= ?")
		args = append(args, f.Since.UnixNano())
	}
	if len(wheres) == 0 {
		return "", nil
	}
	return "WHERE " + strings.Join(wheres, " AND "), args
}

// QueryScores returns one page of scores ordered by (created_at, id), newest
// first unless Asc. The second return value is the cursor for the next page,
// empty when this was the last one.
func (s *SQLiteStore) QueryScores(ctx context.Context, f ScoreFilter) ([]Score, string, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultScoreLimit
	}
	if limit > maxScoreLimit {
		limit = maxScoreLimit
	}
	where, args := buildScoreWhere(f)
	dir, cmp := "DESC", "<"
	if f.Asc {
		dir, cmp = "ASC", ">"
	}
	if f.Cursor != "" {
		n, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, "", err
		}
		cond := fmt.Sprintf("(created_at, id) %s (?, ?)", cmp)
		if where == "" {
			where = "WHERE " + cond
		} else {
			where += " AND " + cond
		}
		args = append(args, n, id)
	}
	query := fmt.Sprintf(`SELECT %s FROM scores %s ORDER BY created_at %s, id %s LIMIT ? OFFSET ?`,
		scoreColumns, where, dir, dir)
	args = append(args, limit+1, max(f.Offset, 0))

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []Score
	for rows.Next() {
		var sc Score
		if err := scanScore(&sc, rows.Scan); err != nil {
			return nil, "", err
		}
		out = append(out, sc)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

// CountScores returns how many scores match the filter (Cursor/Offset/Limit
// are ignored).
func (s *SQLiteStore) CountScores(ctx context.Context, f ScoreFilter) (int64, error) {
	where, args := buildScoreWhere(f)
	var n int64
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM scores "+where, args...).Scan(&n)
	return n, err
}

// scoresForTraces returns all scores of the given traces, grouped by trace id,
// oldest first.
func (s *SQLiteStore) scoresForTraces(ctx context.Context, traceIDs []string) (map[string][]Score, error) {
	out := map[string][]Score{}
	if len(traceIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT %s FROM scores WHERE trace_id IN (%s) ORDER BY created_at, id`,
			scoreColumns, placeholders(len(traceIDs))), toAny(traceIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sc Score
		if err := scanScore(&sc, rows.Scan); err != nil {
			return nil, err
		}
		out[sc.TraceID] = append(out[sc.TraceID], sc)
	}
	return out, rows.Err()
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// Cursors are opaque base64url of "<int>|<id>": a keyset position (timestamp,
// id) so paging is stable under concurrent inserts.

func encodeCursor(n int64, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(n, 10) + "|" + id))
}

func decodeCursor(c string) (int64, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return 0, "", invalidf("bad cursor")
	}
	numStr, id, ok := strings.Cut(string(b), "|")
	if !ok || id == "" {
		return 0, "", invalidf("bad cursor")
	}
	n, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil {
		return 0, "", invalidf("bad cursor")
	}
	return n, id, nil
}
