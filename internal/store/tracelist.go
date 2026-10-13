package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// TraceListFilter selects traces for QueryTraceList. Zero values are ignored.
type TraceListFilter struct {
	Service   string       // some span of the trace has this resource service.name
	RootName  string       // a root span of the trace has this name
	User      string       // some span of the trace has this user_id
	TraceName string       // some span of the trace has this trace_name
	Match     string       // a span of the trace matches this FTS5 query (see BuildMatch)
	Attrs     []AttrFilter // some span of the trace has attribute key == value (all must hold)
	Since     time.Time    // trace start >= Since
	Until     time.Time    // trace start <= Until
	// Missing / Has require the trace to have no / at least one score (any
	// span_id) with that name (and source, when set).
	Missing []ScoreRef
	Has     []ScoreRef
	Asc     bool   // oldest first; default newest first
	Cursor  string // opaque, from a previous page's NextCursor
	Limit   int    // default 50, max 500
}

// AttrFilter matches a span attribute by its text form.
type AttrFilter struct{ Key, Value string }

// ScoreRef names a score by name and (optionally) source.
type ScoreRef struct{ Name, Source string }

// ParseScoreRef parses "name:source" (split on the last colon, so names may
// contain colons) or a bare "name" meaning any source.
func ParseScoreRef(v string) (ScoreRef, error) {
	name, source := v, ""
	if i := strings.LastIndex(v, ":"); i >= 0 {
		name, source = v[:i], v[i+1:]
	}
	if name == "" {
		return ScoreRef{}, invalidf("score ref %q needs a name (name:source)", v)
	}
	return ScoreRef{Name: name, Source: source}, nil
}

// ParseAttr parses "key=value".
func ParseAttr(v string) (AttrFilter, error) {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return AttrFilter{}, invalidf("attr %q must be key=value", v)
	}
	return AttrFilter{Key: k, Value: val}, nil
}

// TraceSummary is the trace read model returned by QueryTraceList. It is
// computed from the spans at query time; nothing is stored per trace.
type TraceSummary struct {
	TraceID      string         `json:"trace_id"`
	RootSpanName string         `json:"root_span_name"`
	RootSpanID   string         `json:"root_span_id"`
	Services     []string       `json:"services"`
	StartNano    uint64         `json:"start_time_unix_nano"`
	EndNano      uint64         `json:"end_time_unix_nano"`
	DurationMs   float64        `json:"duration_ms"`
	SpanCount    int            `json:"span_count"`
	Models       []string       `json:"models"`
	InputTokens  int64          `json:"input_tokens"`
	OutputTokens int64          `json:"output_tokens"`
	TotalTokens  int64          `json:"total_tokens"`
	Error        bool           `json:"error"`
	SessionID    string         `json:"session_id,omitempty"`
	UserID       string         `json:"user_id,omitempty"`    // first non-empty user_id of the trace
	TraceName    string         `json:"trace_name,omitempty"` // first non-empty trace_name
	ObsType      string         `json:"obs_type,omitempty"`   // observation type of the root span
	Scores       []ScoreSummary `json:"scores"`
}

// ScoreSummary is the compact form of a score embedded in a TraceSummary.
type ScoreSummary struct {
	Name     string   `json:"name"`
	Source   string   `json:"source"`
	DataType string   `json:"data_type"`
	SpanID   string   `json:"span_id,omitempty"`
	ValueNum *float64 `json:"value_num,omitempty"`
	ValueStr string   `json:"value_str,omitempty"`
}

// TraceListPage is one page of traces plus the cursor for the next one.
type TraceListPage struct {
	Traces     []TraceSummary `json:"traces"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

const (
	defaultTraceListLimit = 50
	maxTraceListLimit     = 500
)

// QueryTraceList lists traces (one row per trace_id) ordered by trace start,
// then trace_id, with keyset pagination.
//
// Phase 1 picks the page of trace ids with a GROUP BY over spans; every filter
// is a trace-level predicate (trace_id [NOT] IN subquery) so a trace matches on
// the whole trace, not on one row. When Since is set it is also pushed into
// each subquery as a span start bound (safe: no span starts before its trace's
// first span) so the start_time index prunes the scan. Phase 2 loads the
// light span columns of just that page and builds the summaries in Go, so the
// root/service/model/token logic lives in one place.
func (s *SQLiteStore) QueryTraceList(ctx context.Context, f TraceListFilter) (*TraceListPage, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultTraceListLimit
	}
	if limit > maxTraceListLimit {
		limit = maxTraceListLimit
	}

	var wheres, havings []string
	var args, havingArgs []any

	bound := ""
	var boundArgs []any
	if !f.Since.IsZero() {
		bound = " AND start_time_unix_nano >= ?"
		boundArgs = []any{f.Since.UnixNano()}
		wheres = append(wheres, "trace_id IN (SELECT trace_id FROM spans WHERE start_time_unix_nano >= ?)")
		args = append(args, f.Since.UnixNano())
		havings = append(havings, "MIN(start_time_unix_nano) >= ?")
		havingArgs = append(havingArgs, f.Since.UnixNano())
	}
	if !f.Until.IsZero() {
		havings = append(havings, "MIN(start_time_unix_nano) <= ?")
		havingArgs = append(havingArgs, f.Until.UnixNano())
	}
	if f.Service != "" {
		wheres = append(wheres, "trace_id IN (SELECT trace_id FROM spans WHERE service_name = ?"+bound+")")
		args = append(args, f.Service)
		args = append(args, boundArgs...)
	}
	if f.User != "" {
		wheres = append(wheres, "trace_id IN (SELECT trace_id FROM spans WHERE user_id = ?"+bound+")")
		args = append(args, f.User)
		args = append(args, boundArgs...)
	}
	if f.TraceName != "" {
		wheres = append(wheres, "trace_id IN (SELECT trace_id FROM spans WHERE trace_name = ?"+bound+")")
		args = append(args, f.TraceName)
		args = append(args, boundArgs...)
	}
	if f.RootName != "" {
		// Root-like: no parent, or a parent that is not in the same trace
		// (remote parent, dropped span, or parent not exported yet).
		wheres = append(wheres, `trace_id IN (SELECT r.trace_id FROM spans r WHERE r.name = ?`+
			strings.ReplaceAll(bound, "start_time", "r.start_time")+
			` AND (r.parent_span_id = '' OR NOT EXISTS (
				SELECT 1 FROM spans p WHERE p.span_id = r.parent_span_id AND p.trace_id = r.trace_id)))`)
		args = append(args, f.RootName)
		args = append(args, boundArgs...)
	}
	if f.Match != "" {
		wheres = append(wheres, `trace_id IN (SELECT sp.trace_id FROM span_fts
			JOIN span_search ss ON ss.id = span_fts.rowid
			JOIN spans sp ON sp.span_id = ss.span_id
			WHERE span_fts MATCH ?)`)
		args = append(args, f.Match)
	}
	for _, a := range f.Attrs {
		if a.Key == "" || strings.ContainsAny(a.Key, `"`) {
			return nil, invalidf("attr key %q is empty or contains a quote", a.Key)
		}
		quoted, _ := json.Marshal(a.Key) // same encoding the ingest path stored
		// instr() is a cheap literal prefilter before parsing the JSON.
		wheres = append(wheres, `trace_id IN (SELECT trace_id FROM spans WHERE instr(attributes_json, ?) > 0
			AND CASE WHEN json_valid(attributes_json)
				THEN CAST(json_extract(attributes_json, ?) AS TEXT) END = ?`+bound+`)`)
		args = append(args, string(quoted), `$."`+a.Key+`"`, a.Value)
		args = append(args, boundArgs...)
	}
	scoreSub := func(r ScoreRef) (string, []any) {
		if r.Source == "" {
			return "SELECT trace_id FROM scores WHERE name = ?", []any{r.Name}
		}
		return "SELECT trace_id FROM scores WHERE name = ? AND source = ?", []any{r.Name, r.Source}
	}
	for _, r := range f.Has {
		sub, a := scoreSub(r)
		wheres = append(wheres, "trace_id IN ("+sub+")")
		args = append(args, a...)
	}
	for _, r := range f.Missing {
		sub, a := scoreSub(r)
		wheres = append(wheres, "trace_id NOT IN ("+sub+")")
		args = append(args, a...)
	}

	dir, cmp := "DESC", "<"
	if f.Asc {
		dir, cmp = "ASC", ">"
	}
	if f.Cursor != "" {
		n, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, err
		}
		havings = append(havings, fmt.Sprintf("(MIN(start_time_unix_nano), trace_id) %s (?, ?)", cmp))
		havingArgs = append(havingArgs, n, id)
	}

	q := "SELECT trace_id FROM spans"
	if len(wheres) > 0 {
		q += " WHERE " + strings.Join(wheres, " AND ")
	}
	q += " GROUP BY trace_id"
	if len(havings) > 0 {
		q += " HAVING " + strings.Join(havings, " AND ")
	}
	q += fmt.Sprintf(" ORDER BY MIN(start_time_unix_nano) %s, trace_id %s LIMIT ?", dir, dir)
	args = append(args, havingArgs...)
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	hasMore := len(ids) > limit
	if hasMore {
		ids = ids[:limit]
	}
	page := &TraceListPage{Traces: []TraceSummary{}}
	if len(ids) == 0 {
		return page, nil
	}

	summaries, err := s.summarizeTraces(ctx, ids)
	if err != nil {
		return nil, err
	}
	page.Traces = summaries
	if hasMore {
		last := summaries[len(summaries)-1]
		page.NextCursor = encodeCursor(int64(last.StartNano), last.TraceID)
	}
	return page, nil
}

type lightSpan struct {
	id, parent, name, service, model, session string
	user, traceName, obsType                  string
	in, out                                   int64
	status                                    int
	start, end                                uint64
}

// summarizeTraces builds the read model for the given trace ids, preserving
// their order.
func (s *SQLiteStore) summarizeTraces(ctx context.Context, ids []string) ([]TraceSummary, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT trace_id, span_id, parent_span_id, name, service_name, ai_model, agent_session_id,
			ai_input_tokens, ai_output_tokens, status_code, start_time_unix_nano, end_time_unix_nano,
			user_id, trace_name, obs_type
		FROM spans WHERE trace_id IN (%s)`, placeholders(len(ids))), toAny(ids)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byTrace := map[string][]lightSpan{}
	for rows.Next() {
		var tid string
		var sp lightSpan
		if err := rows.Scan(&tid, &sp.id, &sp.parent, &sp.name, &sp.service, &sp.model, &sp.session,
			&sp.in, &sp.out, &sp.status, &sp.start, &sp.end, &sp.user, &sp.traceName, &sp.obsType); err != nil {
			return nil, err
		}
		byTrace[tid] = append(byTrace[tid], sp)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	scores, err := s.scoresForTraces(ctx, ids)
	if err != nil {
		return nil, err
	}

	out := make([]TraceSummary, 0, len(ids))
	for _, id := range ids {
		sum := summarizeSpans(id, byTrace[id])
		sum.Scores = []ScoreSummary{}
		for _, sc := range scores[id] {
			sum.Scores = append(sum.Scores, ScoreSummary{
				Name: sc.Name, Source: sc.Source, DataType: sc.DataType,
				SpanID: sc.SpanID, ValueNum: sc.ValueNum, ValueStr: sc.ValueStr,
			})
		}
		out = append(out, sum)
	}
	return out, nil
}

// summarizeSpans folds one trace's spans into its summary. Services and models
// are listed in order of first appearance (by span start). The root is the
// earliest-starting root-like span (no parent, or parent absent from the
// trace); tokens are summed over all spans.
func summarizeSpans(traceID string, spans []lightSpan) TraceSummary {
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		return spans[i].id < spans[j].id
	})
	sum := TraceSummary{TraceID: traceID, SpanCount: len(spans), Services: []string{}, Models: []string{}}
	present := make(map[string]struct{}, len(spans))
	for _, sp := range spans {
		present[sp.id] = struct{}{}
	}
	seenSvc, seenModel := map[string]bool{}, map[string]bool{}
	rootIdx := -1
	for i, sp := range spans {
		if rootIdx < 0 {
			if _, ok := present[sp.parent]; sp.parent == "" || !ok {
				rootIdx = i
			}
		}
		if i == 0 || sp.start < sum.StartNano {
			sum.StartNano = sp.start
		}
		if sp.end > sum.EndNano {
			sum.EndNano = sp.end
		}
		if sp.service != "" && !seenSvc[sp.service] {
			seenSvc[sp.service] = true
			sum.Services = append(sum.Services, sp.service)
		}
		if sp.model != "" && !seenModel[sp.model] {
			seenModel[sp.model] = true
			sum.Models = append(sum.Models, sp.model)
		}
		sum.InputTokens += sp.in
		sum.OutputTokens += sp.out
		if sp.status == 2 {
			sum.Error = true
		}
		if sum.SessionID == "" {
			sum.SessionID = sp.session
		}
		if sum.UserID == "" {
			sum.UserID = sp.user
		}
		if sum.TraceName == "" {
			sum.TraceName = sp.traceName
		}
	}
	if rootIdx < 0 && len(spans) > 0 {
		rootIdx = 0 // parent cycle; fall back to the earliest span
	}
	if rootIdx >= 0 {
		sum.RootSpanName, sum.RootSpanID = spans[rootIdx].name, spans[rootIdx].id
		sum.ObsType = spans[rootIdx].obsType
	}
	sum.TotalTokens = sum.InputTokens + sum.OutputTokens
	if sum.EndNano > sum.StartNano {
		sum.DurationMs = float64(sum.EndNano-sum.StartNano) / 1e6
	}
	return sum
}
