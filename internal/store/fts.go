package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/algonormative/lookout/internal/genai"
)

// Full-text search over span content.
//
// span_fts is a contentless FTS5 table (content=”, contentless_delete=1): it
// stores only the inverted index, not a second copy of the text, so it stays a
// fraction of the text size. The price is that snippet()/highlight() are
// unavailable; snippets are cut from the re-extracted text in Go.
//
// FTS rowids come from span_search(id INTEGER PRIMARY KEY), not spans.rowid:
// spans has a TEXT primary key, so its implicit rowid is not guaranteed to
// survive VACUUM (which the retention path runs).
//
// Indexed per span: name column = span name, status message, error.message;
// body column = claude_code user_prompt, the text of gen_ai.output.messages,
// and the newest user text plus the tool results that follow it in
// gen_ai.input.messages, capped at searchTextBytes in total.
const (
	searchTextBytes = 16 * 1024
	ftsBackfillKey  = "backfill_fts_v1"
	ftsBackfillRows = 200 // rowids per transaction (payload decompression dominates)
)

func (s *SQLiteStore) migrateFTS() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS span_search (
			id      INTEGER PRIMARY KEY AUTOINCREMENT,
			span_id TEXT NOT NULL UNIQUE
		);
		CREATE VIRTUAL TABLE IF NOT EXISTS span_fts USING fts5(
			name, body, content='', contentless_delete=1,
			tokenize='unicode61 remove_diacritics 2');
		CREATE TRIGGER IF NOT EXISTS spans_fts_ad AFTER DELETE ON spans BEGIN
			DELETE FROM span_fts WHERE rowid IN (SELECT id FROM span_search WHERE span_id = old.span_id);
			DELETE FROM span_search WHERE span_id = old.span_id;
		END;
	`)
	return err
}

// searchColumns builds the two indexed texts of a span from its full
// (uncompressed) attributes and events.
func searchColumns(name, statusMsg, attrsJSON, eventsJSON string) (nameCol, body string) {
	nameParts := []string{name, statusMsg}
	var attrs map[string]any
	if strings.Contains(attrsJSON, "gen_ai.") || strings.Contains(attrsJSON, "user_prompt") ||
		strings.Contains(attrsJSON, "error.message") || strings.Contains(attrsJSON, "langfuse.observation.") {
		json.Unmarshal([]byte(attrsJSON), &attrs)
	}
	str := func(k string) string { v, _ := attrs[k].(string); return v }
	nameParts = append(nameParts, str("error.message"))

	budget := searchTextBytes
	var bodies []string
	add := func(t string, max int) {
		if t == "" || budget <= 0 {
			return
		}
		if max > budget {
			max = budget
		}
		t = clipRunes(t, max)
		budget -= len(t)
		bodies = append(bodies, t)
	}
	add(str("user_prompt"), budget)
	add(messagesText(str("gen_ai.output.messages"), false), budget/2)
	add(messagesText(str("gen_ai.input.messages"), true), budget/2)
	// Langfuse observation input/output: message arrays or plain text.
	add(observationText(str("langfuse.observation.output")), budget/2)
	add(observationText(str("langfuse.observation.input")), budget)
	return strings.Join(nonEmpty(nameParts), " "), strings.Join(bodies, "\n")
}

// messagesText returns the searchable text of a messages attribute. For input
// it walks newest-first and keeps user text and tool results until (and
// including) the newest user text message, skipping system and assistant
// turns (the history is indexed on the spans that produced it).
func messagesText(attr string, input bool) string {
	if attr == "" {
		return ""
	}
	msgs, ok := genai.ParseMessages(attr)
	if !ok { // not an array (e.g. head-truncated string): index its head
		return clipRunes(attr, searchTextBytes)
	}
	var segs []string
	if !input {
		for _, m := range msgs {
			if m.Role == "assistant" || m.Role == "" {
				if t := m.Text(); t != "" {
					segs = append(segs, t)
				}
			}
		}
		return strings.Join(segs, "\n")
	}
	size := 0
	for i := len(msgs) - 1; i >= 0 && size < searchTextBytes; i-- {
		m := msgs[i]
		if m.Role == "system" || m.Role == "assistant" || m.Marker {
			continue
		}
		if t := m.Text(); t != "" {
			segs = append(segs, t)
			size += len(t)
		}
		if m.HasUserText() {
			break
		}
	}
	return strings.Join(segs, "\n")
}

// observationText reads a langfuse.observation.input/output value: a chat
// message array when it parses as one, otherwise the text itself.
func observationText(v string) string {
	if v == "" {
		return ""
	}
	if msgs, ok := genai.ParseMessages(v); ok && len(msgs) > 0 {
		return messagesText(v, true)
	}
	return v
}

func nonEmpty(ss []string) []string {
	out := ss[:0:0]
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// clipRunes cuts s to at most max bytes on a rune boundary.
func clipRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

// indexSpan adds one span's text to the index inside tx. It is a no-op when
// the span is already indexed or no longer exists.
func indexSpan(ctx context.Context, tx *sql.Tx, spanID, nameCol, body string) error {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO span_search(span_id) SELECT span_id FROM spans WHERE span_id = ? ON CONFLICT(span_id) DO NOTHING`, spanID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO span_fts(rowid, name, body) VALUES (?, ?, ?)`, id, nameCol, body)
	return err
}

// backfillFTS indexes spans that existed before the index did. Batched by
// rowid window in short transactions, resumable (already-indexed spans are
// skipped), marked done in meta.
func (s *SQLiteStore) backfillFTS(ctx context.Context) error {
	var done string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, ftsBackfillKey).Scan(&done)
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
		var n int64
		for from := lo.Int64; from <= hi.Int64; from += ftsBackfillRows {
			c, err := s.backfillFTSWindow(ctx, from, from+ftsBackfillRows)
			if err != nil {
				return err
			}
			n += c
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backfillPause):
			}
		}
		log.Printf("backfill: indexed %d spans for search in %s", n, time.Since(start).Round(time.Millisecond))
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES (?, '1')
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, ftsBackfillKey)
	return err
}

func (s *SQLiteStore) backfillFTSWindow(ctx context.Context, from, to int64) (int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT span_id, name, status_message, attributes_json, events_json FROM spans
		WHERE rowid >= ? AND rowid < ?
		  AND NOT EXISTS (SELECT 1 FROM span_search m WHERE m.span_id = spans.span_id)`, from, to)
	if err != nil {
		return 0, err
	}
	var batch []SpanRecord
	for rows.Next() {
		var sp SpanRecord
		if err := rows.Scan(&sp.SpanID, &sp.Name, &sp.StatusMessage, &sp.AttributesJSON, &sp.EventsJSON); err != nil {
			rows.Close()
			return 0, err
		}
		batch = append(batch, sp)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(batch) == 0 {
		return 0, nil
	}
	if err := s.restorePayloads(ctx, batch); err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, sp := range batch {
		n, b := searchColumns(sp.Name, sp.StatusMessage, sp.AttributesJSON, sp.EventsJSON)
		if err := indexSpan(ctx, tx, sp.SpanID, n, b); err != nil {
			return 0, err
		}
	}
	return int64(len(batch)), tx.Commit()
}

// indexStmts are the prepared statements of the ingest path.
type indexStmts struct{ search, fts *sql.Stmt }

func prepareIndex(ctx context.Context, tx *sql.Tx) (*indexStmts, error) {
	search, err := tx.PrepareContext(ctx,
		`INSERT INTO span_search(span_id) SELECT span_id FROM spans WHERE span_id = ? ON CONFLICT(span_id) DO NOTHING`)
	if err != nil {
		return nil, err
	}
	fts, err := tx.PrepareContext(ctx, `INSERT INTO span_fts(rowid, name, body) VALUES (?, ?, ?)`)
	if err != nil {
		search.Close()
		return nil, err
	}
	return &indexStmts{search, fts}, nil
}

func (p *indexStmts) close() { p.search.Close(); p.fts.Close() }

func indexSpanTx(ctx context.Context, p *indexStmts, spanID, nameCol, body string) error {
	res, err := p.search.ExecContext(ctx, spanID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	_, err = p.fts.ExecContext(ctx, id, nameCol, body)
	return err
}

// --- query side ---

// SearchFilter selects traces whose indexed text matches Q.
type SearchFilter struct {
	Q       string // plain terms (all must match), "quoted phrases", trailing * for prefix
	Service string
	Since   time.Time // trace start >=
	Until   time.Time // trace start <=
	Cursor  string
	Limit   int
}

// SearchResult is one matching trace.
type SearchResult struct {
	TraceID      string   `json:"trace_id"`
	RootSpanName string   `json:"root_span_name"`
	StartNano    uint64   `json:"start_time_unix_nano"`
	Snippet      string   `json:"snippet"`
	SpanID       string   `json:"span_id,omitempty"` // best-matching span
	Services     []string `json:"services"`
	SpanCount    int      `json:"span_count"`
	Error        bool     `json:"error"`
}

// SearchPage is one page of results, newest trace first.
type SearchPage struct {
	Traces     []SearchResult `json:"traces"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

// BuildMatch turns user input into a safe FTS5 query: every term is quoted
// (so punctuation like "-" or ":" cannot be parsed as operators) and terms are
// ANDed. "double quotes" group a phrase; a trailing * on a term is a prefix.
func BuildMatch(q string) (string, []string, error) {
	var out, words []string
	rest := strings.TrimSpace(q)
	for rest != "" {
		var tok string
		if rest[0] == '"' {
			end := strings.IndexByte(rest[1:], '"')
			if end < 0 {
				tok, rest = rest[1:], ""
			} else {
				tok, rest = rest[1:1+end], rest[end+2:]
			}
		} else {
			end := strings.IndexAny(rest, " \t\r\n")
			if end < 0 {
				tok, rest = rest, ""
			} else {
				tok, rest = rest[:end], rest[end:]
			}
		}
		rest = strings.TrimSpace(rest)
		prefix := strings.HasSuffix(tok, "*") && !strings.HasPrefix(tok, `"`)
		tok = strings.TrimSpace(strings.TrimSuffix(tok, "*"))
		if strings.Trim(tok, " \t") == "" || !hasWordRune(tok) {
			continue
		}
		term := `"` + strings.ReplaceAll(tok, `"`, `""`) + `"`
		if prefix {
			term += "*"
		}
		out = append(out, term)
		words = append(words, tok)
	}
	if len(out) == 0 {
		return "", nil, invalidf("search query needs at least one word")
	}
	return strings.Join(out, " "), words, nil
}

func hasWordRune(s string) bool {
	for _, r := range s {
		if r > 127 || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			return true
		}
	}
	return false
}

// SearchTraces returns traces with a span matching the query, newest trace
// first, with a snippet from the best-matching span.
func (s *SQLiteStore) SearchTraces(ctx context.Context, f SearchFilter) (*SearchPage, error) {
	match, words, err := BuildMatch(f.Q)
	if err != nil {
		return nil, err
	}
	page, err := s.QueryTraceList(ctx, TraceListFilter{
		Match: match, Service: f.Service, Since: f.Since, Until: f.Until, Cursor: f.Cursor, Limit: f.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := &SearchPage{Traces: []SearchResult{}, NextCursor: page.NextCursor}
	if len(page.Traces) == 0 {
		return out, nil
	}
	ids := make([]string, len(page.Traces))
	for i, t := range page.Traces {
		ids[i] = t.TraceID
	}
	// Best hit per trace: rank is bm25, lower is better.
	args := append([]any{match}, toAny(ids)...)
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT sp.trace_id, sp.span_id FROM span_fts
		JOIN span_search m ON m.id = span_fts.rowid
		JOIN spans sp ON sp.span_id = m.span_id
		WHERE span_fts MATCH ? AND sp.trace_id IN (%s)
		ORDER BY span_fts.rank`, placeholders(len(ids))), args...)
	if err != nil {
		return nil, err
	}
	best := map[string]string{}
	for rows.Next() {
		var tid, sid string
		if err := rows.Scan(&tid, &sid); err != nil {
			rows.Close()
			return nil, err
		}
		if _, ok := best[tid]; !ok {
			best[tid] = sid
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var spanIDs []string
	for _, sid := range best {
		spanIDs = append(spanIDs, sid)
	}
	texts := map[string]string{}
	if len(spanIDs) > 0 {
		spans, err := s.querySpanRows(ctx, fmt.Sprintf("SELECT %s FROM spans WHERE span_id IN (%s)",
			spanColumns, placeholders(len(spanIDs))), toAny(spanIDs)...)
		if err != nil {
			return nil, err
		}
		for _, sp := range spans {
			n, b := searchColumns(sp.Name, sp.StatusMessage, sp.AttributesJSON, sp.EventsJSON)
			texts[sp.SpanID] = n + "\n" + b
		}
	}
	for _, t := range page.Traces {
		sid := best[t.TraceID]
		out.Traces = append(out.Traces, SearchResult{
			TraceID: t.TraceID, RootSpanName: t.RootSpanName, StartNano: t.StartNano,
			Snippet: makeSnippet(texts[sid], words), SpanID: sid,
			Services: t.Services, SpanCount: t.SpanCount, Error: t.Error,
		})
	}
	return out, nil
}

// makeSnippet cuts a ~160 byte window around the first term occurrence
// (case-insensitive), falling back to the start of the text (diacritic-folded
// matches are not found by the plain substring search).
func makeSnippet(text string, words []string) string {
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return ""
	}
	const width = 160
	lower := strings.ToLower(text)
	pos := -1
	if len(lower) == len(text) {
		for _, w := range words {
			if i := strings.Index(lower, strings.ToLower(w)); i >= 0 && (pos < 0 || i < pos) {
				pos = i
			}
		}
	}
	start := 0
	if pos > 40 {
		start = pos - 40
	}
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	end := start + width
	if end >= len(text) {
		end = len(text)
	} else {
		for end > start && !utf8.RuneStart(text[end]) {
			end--
		}
	}
	sn := text[start:end]
	if start > 0 {
		sn = "…" + sn
	}
	if end < len(text) {
		sn += "…"
	}
	return sn
}
