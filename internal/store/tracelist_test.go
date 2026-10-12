package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// seedTraceFixture inserts four traces shaped like the real producers:
//
//	t1 (+1s)  nanoclaw-agent claude_code.interaction > llm_request > tool > tool.execution,
//	          plus a llm-proxy "Received Proxy Server Request" joined via traceparent
//	t2 (+2s)  nanoclaw-agent claude_code.interaction with an error span
//	t3 (+3s)  service "batch", root "job"
//	t4 (+4s)  nanoclaw-agent claude_code.interaction
//
// Scores: t1 promise_only/jev, t2 promise_only/minicheck.
func seedTraceFixture(t *testing.T, s *SQLiteStore) uint64 {
	t.Helper()
	ctx := context.Background()
	base := uint64(time.Now().Add(-time.Hour).UnixNano())
	sec := uint64(time.Second)
	mk := func(trace, span, parent, name, svc string, startOff, dur uint64) SpanRecord {
		return SpanRecord{
			TraceID: trace, SpanID: span, ParentSpanID: parent, Name: name, ServiceName: svc,
			StartTimeUnixNano: base + startOff, EndTimeUnixNano: base + startOff + dur,
			AttributesJSON: "{}", InsertedAt: time.Now(),
		}
	}
	ns := "nanoclaw-agent"
	ci := "claude_code.interaction"

	t1root := mk("t1", "t1-root", "", ci, ns, 1*sec, 10*sec)
	t1root.AttributesJSON = `{"user.email":"a@b.c","session.id":"sess-1","n":5}`
	t1root.AgentSessionID = "sess-1"
	t1llm := mk("t1", "t1-llm", "t1-root", "claude_code.llm_request", ns, 2*sec, 3*sec)
	t1llm.AIModel, t1llm.AIInputTokens, t1llm.AIOutputTokens = "claude-sonnet", 100, 50
	t1tool := mk("t1", "t1-tool", "t1-root", "claude_code.tool", ns, 6*sec, 2*sec)
	t1exec := mk("t1", "t1-exec", "t1-tool", "claude_code.tool.execution", ns, 6*sec, 1*sec)
	t1proxy := mk("t1", "t1-proxy", "t1-llm", "Received Proxy Server Request", "llm-proxy", 2*sec+1, 2*sec)
	t1proxy.AIModel = "claude-sonnet-4-5"

	t2root := mk("t2", "t2-root", "", ci, ns, 2*sec, 5*sec)
	t2root.AgentSessionID = "sess-2"
	t2err := mk("t2", "t2-err", "t2-root", "claude_code.tool", ns, 3*sec, 1*sec)
	t2err.StatusCode = 2

	t3root := mk("t3", "t3-root", "", "job", "batch", 3*sec, 1*sec)
	t4root := mk("t4", "t4-root", "", ci, ns, 4*sec, 1*sec)
	t4root.AttributesJSON = `{"session.id":"sess-4"}`

	if err := s.InsertSpans(ctx, []SpanRecord{t1root, t1llm, t1tool, t1exec, t1proxy,
		t2root, t2err, t3root, t4root}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertScores(ctx, []Score{
		{TraceID: "t1", Name: "promise_only", DataType: ScoreBoolean, ValueNum: f64(1), Source: "jev"},
		{TraceID: "t2", Name: "promise_only", DataType: ScoreNumeric, ValueNum: f64(0.1), Source: "minicheck"},
		{TraceID: "t2", SpanID: "t2-err", Name: "grounded", DataType: ScoreCategorical, ValueStr: "no", Source: "jev"},
	}); err != nil {
		t.Fatal(err)
	}
	return base
}

func traceIDs(p *TraceListPage) []string {
	ids := []string{}
	for _, tr := range p.Traces {
		ids = append(ids, tr.TraceID)
	}
	return ids
}

func TestTraceListSummary(t *testing.T) {
	s := testStore(t)
	base := seedTraceFixture(t, s)
	ctx := context.Background()

	page, err := s.QueryTraceList(ctx, TraceListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"t4", "t3", "t2", "t1"}; !equalStrings(traceIDs(page), want) {
		t.Fatalf("default order (newest first): got %v want %v", traceIDs(page), want)
	}

	var t1 TraceSummary
	for _, tr := range page.Traces {
		if tr.TraceID == "t1" {
			t1 = tr
		}
	}
	if t1.RootSpanName != "claude_code.interaction" || t1.RootSpanID != "t1-root" {
		t.Errorf("root: %q %q (the llm-proxy span must not become root)", t1.RootSpanName, t1.RootSpanID)
	}
	if !equalStrings(t1.Services, []string{"nanoclaw-agent", "llm-proxy"}) {
		t.Errorf("services: %v", t1.Services)
	}
	if t1.SpanCount != 5 || t1.InputTokens != 100 || t1.OutputTokens != 50 || t1.TotalTokens != 150 {
		t.Errorf("counts: %+v", t1)
	}
	if !equalStrings(t1.Models, []string{"claude-sonnet", "claude-sonnet-4-5"}) {
		t.Errorf("models: %v", t1.Models)
	}
	if t1.StartNano != base+uint64(time.Second) || t1.DurationMs != 10000 {
		t.Errorf("start/duration: %d %v", t1.StartNano, t1.DurationMs)
	}
	if t1.SessionID != "sess-1" || t1.Error {
		t.Errorf("session/error: %+v", t1)
	}
	if len(t1.Scores) != 1 || t1.Scores[0].Name != "promise_only" || t1.Scores[0].Source != "jev" {
		t.Errorf("scores summary: %+v", t1.Scores)
	}
	for _, tr := range page.Traces {
		if tr.TraceID == "t2" {
			if !tr.Error || len(tr.Scores) != 2 {
				t.Errorf("t2: %+v", tr)
			}
		}
	}
}

func TestTraceListFilters(t *testing.T) {
	s := testStore(t)
	base := seedTraceFixture(t, s)
	ctx := context.Background()

	cases := []struct {
		name string
		f    TraceListFilter
		want []string // newest first
	}{
		{"service", TraceListFilter{Service: "nanoclaw-agent"}, []string{"t4", "t2", "t1"}},
		{"service second of two on one trace", TraceListFilter{Service: "llm-proxy"}, []string{"t1"}},
		{"service none", TraceListFilter{Service: "nope"}, []string{}},
		{"root_name", TraceListFilter{RootName: "claude_code.interaction"}, []string{"t4", "t2", "t1"}},
		{"root_name ignores non-root spans", TraceListFilter{RootName: "claude_code.tool"}, []string{}},
		{"root_name proxy span is not a root", TraceListFilter{RootName: "Received Proxy Server Request"}, []string{}},
		{"service+root", TraceListFilter{Service: "nanoclaw-agent", RootName: "claude_code.interaction"}, []string{"t4", "t2", "t1"}},
		{"attr string", TraceListFilter{Attrs: []AttrFilter{{"session.id", "sess-1"}}}, []string{"t1"}},
		{"attr number as text", TraceListFilter{Attrs: []AttrFilter{{"n", "5"}}}, []string{"t1"}},
		{"attr miss", TraceListFilter{Attrs: []AttrFilter{{"session.id", "nope"}}}, []string{}},
		{"attr AND", TraceListFilter{Attrs: []AttrFilter{{"session.id", "sess-1"}, {"user.email", "a@b.c"}}}, []string{"t1"}},
		{"missing exact", TraceListFilter{Service: "nanoclaw-agent", Missing: []ScoreRef{{"promise_only", "jev"}}}, []string{"t4", "t2"}},
		{"missing any source", TraceListFilter{Service: "nanoclaw-agent", Missing: []ScoreRef{{Name: "promise_only"}}}, []string{"t4"}},
		{"missing both", TraceListFilter{Missing: []ScoreRef{{"promise_only", "jev"}, {"promise_only", "minicheck"}}}, []string{"t4", "t3"}},
		{"has", TraceListFilter{Has: []ScoreRef{{"promise_only", "minicheck"}}}, []string{"t2"}},
		{"has span-level counts for the trace", TraceListFilter{Has: []ScoreRef{{"grounded", "jev"}}}, []string{"t2"}},
		{"has and missing", TraceListFilter{Has: []ScoreRef{{"promise_only", ""}}, Missing: []ScoreRef{{"grounded", "jev"}}}, []string{"t1"}},
		{"since", TraceListFilter{Since: time.Unix(0, int64(base)+int64(2500*time.Millisecond))}, []string{"t4", "t3"}},
		{"until", TraceListFilter{Until: time.Unix(0, int64(base)+int64(2500*time.Millisecond))}, []string{"t2", "t1"}},
		{"asc", TraceListFilter{Asc: true, Service: "nanoclaw-agent"}, []string{"t1", "t2", "t4"}},
	}
	for _, tc := range cases {
		page, err := s.QueryTraceList(ctx, tc.f)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got := traceIDs(page); !equalStrings(got, tc.want) {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}

	// Judge loop: unscored first, oldest first; scoring removes it from the list.
	loop := TraceListFilter{Service: "nanoclaw-agent", RootName: "claude_code.interaction",
		Missing: []ScoreRef{{"promise_only", "jev"}}, Asc: true, Limit: 50}
	page, _ := s.QueryTraceList(ctx, loop)
	if want := []string{"t2", "t4"}; !equalStrings(traceIDs(page), want) {
		t.Fatalf("judge loop: %v", traceIDs(page))
	}
	s.UpsertScores(ctx, []Score{{TraceID: "t2", Name: "promise_only", DataType: ScoreBoolean, ValueNum: f64(0), Source: "jev"}})
	page, _ = s.QueryTraceList(ctx, loop)
	if want := []string{"t4"}; !equalStrings(traceIDs(page), want) {
		t.Fatalf("after scoring: %v", traceIDs(page))
	}

	if _, err := s.QueryTraceList(ctx, TraceListFilter{Attrs: []AttrFilter{{`bad"key`, "v"}}}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("quote in attr key: %v", err)
	}
	if _, err := s.QueryTraceList(ctx, TraceListFilter{Cursor: "@@"}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("bad cursor: %v", err)
	}
}

func TestTraceListCursorPaging(t *testing.T) {
	s := testStore(t)
	seedTraceFixture(t, s)
	// Two traces with the same start as t4, to exercise the trace_id tiebreak.
	ctx := context.Background()
	var t4start uint64
	s.db.QueryRow(`SELECT start_time_unix_nano FROM spans WHERE trace_id='t4'`).Scan(&t4start)
	s.InsertSpans(ctx, []SpanRecord{
		{TraceID: "t4b", SpanID: "t4b-root", Name: "claude_code.interaction", ServiceName: "nanoclaw-agent",
			StartTimeUnixNano: t4start, EndTimeUnixNano: t4start + 1, InsertedAt: time.Now()},
		{TraceID: "t4a", SpanID: "t4a-root", Name: "claude_code.interaction", ServiceName: "nanoclaw-agent",
			StartTimeUnixNano: t4start, EndTimeUnixNano: t4start + 1, InsertedAt: time.Now()},
	})

	for _, asc := range []bool{false, true} {
		want := []string{"t4b", "t4a", "t4", "t3", "t2", "t1"} // desc: (start, id) descending
		if asc {
			want = []string{"t1", "t2", "t3", "t4", "t4a", "t4b"}
		}
		for _, limit := range []int{1, 2, 4, 6, 10} {
			var seen []string
			cursor := ""
			for pages := 0; pages < 20; pages++ {
				page, err := s.QueryTraceList(ctx, TraceListFilter{Asc: asc, Limit: limit, Cursor: cursor})
				if err != nil {
					t.Fatal(err)
				}
				seen = append(seen, traceIDs(page)...)
				if page.NextCursor == "" {
					break
				}
				cursor = page.NextCursor
			}
			if !equalStrings(seen, want) {
				t.Errorf("asc=%v limit=%d: got %v want %v", asc, limit, seen, want)
			}
		}
	}

	// Exactly-full last page must not emit a cursor.
	page, _ := s.QueryTraceList(ctx, TraceListFilter{Limit: 6})
	if page.NextCursor != "" {
		t.Errorf("cursor on final page: %q", page.NextCursor)
	}
}

func TestTraceListRootFallbacks(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := uint64(time.Now().UnixNano())
	// Remote parent: the root's parent is not in the trace (propagated traceparent).
	s.InsertSpans(ctx, []SpanRecord{
		{TraceID: "r1", SpanID: "a", ParentSpanID: "remote", Name: "claude_code.interaction", ServiceName: "svc",
			StartTimeUnixNano: now, EndTimeUnixNano: now + 10, InsertedAt: time.Now()},
		{TraceID: "r1", SpanID: "b", ParentSpanID: "a", Name: "child", ServiceName: "svc",
			StartTimeUnixNano: now + 1, EndTimeUnixNano: now + 5, InsertedAt: time.Now()},
		// Parent cycle, no root-like span: falls back to the earliest span.
		{TraceID: "r2", SpanID: "x", ParentSpanID: "y", Name: "cx", StartTimeUnixNano: now, EndTimeUnixNano: now + 1, InsertedAt: time.Now()},
		{TraceID: "r2", SpanID: "y", ParentSpanID: "x", Name: "cy", StartTimeUnixNano: now + 1, EndTimeUnixNano: now + 2, InsertedAt: time.Now()},
	})
	page, err := s.QueryTraceList(ctx, TraceListFilter{RootName: "claude_code.interaction"})
	if err != nil || !equalStrings(traceIDs(page), []string{"r1"}) {
		t.Fatalf("remote-parent root: %v %v", traceIDs(page), err)
	}
	page, _ = s.QueryTraceList(ctx, TraceListFilter{})
	for _, tr := range page.Traces {
		if tr.TraceID == "r2" && tr.RootSpanName != "cx" {
			t.Errorf("cycle fallback root: %q", tr.RootSpanName)
		}
	}
}
