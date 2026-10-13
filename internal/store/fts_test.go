package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func msgSpan(trace, id, service string, start time.Time, input, output string) SpanRecord {
	attrs := map[string]any{}
	if input != "" {
		attrs["gen_ai.input.messages"] = input
	}
	if output != "" {
		attrs["gen_ai.output.messages"] = output
	}
	return SpanRecord{
		TraceID: trace, SpanID: id, Name: "Received Proxy Server Request", ServiceName: service,
		AttributesJSON:    mustJSON(attrs),
		StartTimeUnixNano: uint64(start.UnixNano()), EndTimeUnixNano: uint64(start.UnixNano()) + 1,
		InsertedAt: time.Now(),
	}
}

const inputMsgs = `[{"role":"system","parts":[{"type":"text","content":"SECRETSYSTEMPROMPT you are helpful"}]},
{"role":"user","parts":[{"type":"text","content":"first question about zebras"}]},
{"role":"assistant","parts":[{"type":"text","content":"zebras are striped"}]},
{"role":"user","parts":[{"type":"text","text":"Please deploy the Köln release"}]}]`

const outputMsgs = `[{"role":"assistant","parts":[{"type":"text","content":"Deployment finished successfully"}],"finish_reason":"stop"}]`

func searchIDs(t *testing.T, s *SQLiteStore, f SearchFilter) []string {
	t.Helper()
	page, err := s.SearchTraces(context.Background(), f)
	if err != nil {
		t.Fatalf("search %q: %v", f.Q, err)
	}
	var ids []string
	for _, r := range page.Traces {
		ids = append(ids, r.TraceID)
	}
	return ids
}

func TestSearchIndexesMessageTextNotJSONKeys(t *testing.T) {
	s := optStore(t, Options{CompressMinBytes: 4096})
	ctx := context.Background()
	now := time.Now()
	if err := s.InsertSpans(ctx, []SpanRecord{msgSpan("t1", "s1", "llm-proxy", now, inputMsgs, outputMsgs)}); err != nil {
		t.Fatal(err)
	}
	want := func(q string, n int) {
		t.Helper()
		if got := searchIDs(t, s, SearchFilter{Q: q}); len(got) != n {
			t.Errorf("q=%q -> %v, want %d hits", q, got, n)
		}
	}
	want("deploy", 1)              // newest user text ("text" part key)
	want("deployment finished", 1) // output text ("content" part key), AND of terms
	want("koln", 1)                // diacritics folded
	want("Köln", 1)
	want("depl*", 1)              // prefix
	want(`"deploy the"`, 1)       // phrase
	want(`"the deploy"`, 0)       // phrase order matters
	want("zebras", 0)             // older history is not indexed
	want("SECRETSYSTEMPROMPT", 0) // system prompt is not indexed
	want("parts", 0)              // JSON keys are not indexed
	want("finish_reason", 0)
	want("role", 0)
	want("nonexistentterm", 0)

	// Matching works across a compressed payload (full text came from the
	// original attributes at ingest, not the lite column).
	big := strings.Repeat("filler ", 800)
	sp := msgSpan("t2", "s2", "llm-proxy", now.Add(time.Second),
		`[{"role":"user","parts":[{"type":"text","content":"`+big+` needle"}]}]`, "")
	s.InsertSpans(ctx, []SpanRecord{sp})
	if payloadCount(t, s) != 1 {
		t.Fatal("expected span 2 compressed")
	}
	want("needle", 1)
}

func TestSearchToolResultsAndOtherFields(t *testing.T) {
	s := optStore(t, Options{})
	ctx := context.Background()
	now := time.Now()
	in := `[{"role":"user","parts":[{"type":"text","content":"ancient prompt"}]},
{"role":"assistant","parts":[{"type":"text","content":"ok"}]},
{"role":"user","parts":[{"type":"text","content":"recent prompt"}]},
{"role":"assistant","parts":[{"type":"tool_call","id":"c1","name":"Bash","arguments":{"cmd":"ls"}}]},
{"role":"user","parts":[{"type":"tool_result","id":"c1","content":[{"type":"text","text":"file-list-output"}]}]}]`
	spans := []SpanRecord{
		msgSpan("t1", "s1", "llm-proxy", now, in, ""),
		{TraceID: "t2", SpanID: "s2", Name: "claude_code.interaction", ServiceName: "nanoclaw-agent",
			AttributesJSON: `{"user_prompt":"make a low-stock reminder"}`, StartTimeUnixNano: uint64(now.UnixNano()), EndTimeUnixNano: uint64(now.UnixNano()) + 1, InsertedAt: now},
		{TraceID: "t3", SpanID: "s3", Name: "http.request", StatusMessage: "upstream exploded", StatusCode: 2,
			AttributesJSON: `{"error.message":"connection refused by flamingo"}`, StartTimeUnixNano: uint64(now.UnixNano()), EndTimeUnixNano: uint64(now.UnixNano()) + 1, InsertedAt: now},
	}
	s.InsertSpans(ctx, spans)
	for q, want := range map[string]string{
		"file-list-output": "t1", // tool_result text; tokenizer splits on '-'
		"reminder":         "t2", // claude_code user_prompt
		"flamingo":         "t3", // error.message
		"exploded":         "t3", // status message
		"http":             "t3", // span name
	} {
		got := searchIDs(t, s, SearchFilter{Q: q})
		if len(got) != 1 || got[0] != want {
			t.Errorf("q=%q -> %v, want [%s]", q, got, want)
		}
	}
	if got := searchIDs(t, s, SearchFilter{Q: "ancient"}); len(got) != 0 {
		t.Errorf("history before the newest user text must not be indexed: %v", got)
	}
}

func TestSearchFiltersOrderAndPaging(t *testing.T) {
	s := optStore(t, Options{})
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)
	var spans []SpanRecord
	for i, svc := range []string{"a", "b", "a", "b", "a"} {
		id := string(rune('a' + i))
		spans = append(spans, SpanRecord{TraceID: "t" + id, SpanID: "s" + id, Name: "common phrase", ServiceName: svc,
			StartTimeUnixNano: uint64(base.Add(time.Duration(i) * time.Minute).UnixNano()), EndTimeUnixNano: 1, InsertedAt: time.Now()})
	}
	s.InsertSpans(ctx, spans)

	if got := searchIDs(t, s, SearchFilter{Q: "common"}); strings.Join(got, ",") != "te,td,tc,tb,ta" {
		t.Errorf("newest first: %v", got)
	}
	if got := searchIDs(t, s, SearchFilter{Q: "common", Service: "b"}); strings.Join(got, ",") != "td,tb" {
		t.Errorf("service filter: %v", got)
	}
	if got := searchIDs(t, s, SearchFilter{Q: "common", Since: base.Add(110 * time.Second)}); strings.Join(got, ",") != "te,td,tc" {
		t.Errorf("since: %v", got)
	}
	if got := searchIDs(t, s, SearchFilter{Q: "common", Until: base.Add(90 * time.Second)}); strings.Join(got, ",") != "tb,ta" {
		t.Errorf("until: %v", got)
	}
	p1, _ := s.SearchTraces(ctx, SearchFilter{Q: "common", Limit: 2})
	if len(p1.Traces) != 2 || p1.NextCursor == "" {
		t.Fatalf("page 1: %+v", p1)
	}
	p2, _ := s.SearchTraces(ctx, SearchFilter{Q: "common", Limit: 2, Cursor: p1.NextCursor})
	if len(p2.Traces) != 2 || p2.Traces[0].TraceID != "tc" {
		t.Errorf("page 2: %+v", p2)
	}
	if p1.Traces[0].Snippet == "" || p1.Traces[0].RootSpanName != "common phrase" {
		t.Errorf("result fields: %+v", p1.Traces[0])
	}
}

func TestSearchQuerySanitizing(t *testing.T) {
	s := optStore(t, Options{})
	s.InsertSpans(context.Background(), []SpanRecord{{TraceID: "t", SpanID: "s", Name: "alpha-beta gamma", InsertedAt: time.Now(),
		StartTimeUnixNano: 1, EndTimeUnixNano: 2}})
	for _, q := range []string{"alpha-beta", `alpha AND OR NOT`, `"unterminated`, "a:b", "(alpha)", "alpha*", "NEAR(", "col:alpha"} {
		if _, err := s.SearchTraces(context.Background(), SearchFilter{Q: q}); err != nil {
			t.Errorf("q=%q errored: %v", q, err)
		}
	}
	for _, q := range []string{"", "   ", "---", `""`} {
		if _, err := s.SearchTraces(context.Background(), SearchFilter{Q: q}); err == nil {
			t.Errorf("q=%q should be rejected", q)
		}
	}
}

func TestSearchIndexFollowsDeleteAndReplace(t *testing.T) {
	s := optStore(t, Options{})
	ctx := context.Background()
	old := time.Now().Add(-30 * 24 * time.Hour)
	s.InsertSpans(ctx, []SpanRecord{
		{TraceID: "old", SpanID: "o", Name: "oldword", StartTimeUnixNano: uint64(old.UnixNano()), EndTimeUnixNano: 1, InsertedAt: time.Now()},
		{TraceID: "cur", SpanID: "c", Name: "firstname", StartTimeUnixNano: uint64(time.Now().UnixNano()), EndTimeUnixNano: 1, InsertedAt: time.Now()},
	})
	s.Cleanup(ctx, 7)
	if got := searchIDs(t, s, SearchFilter{Q: "oldword"}); len(got) != 0 {
		t.Errorf("deleted span still searchable: %v", got)
	}
	count := func(table string) int {
		var n int
		s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n)
		return n
	}
	if count("span_search") != 1 {
		t.Errorf("span_search rows = %d", count("span_search"))
	}
	// Re-sending a span with new text replaces its index entry.
	s.InsertSpans(ctx, []SpanRecord{{TraceID: "cur", SpanID: "c", Name: "renamed", StartTimeUnixNano: uint64(time.Now().UnixNano()), EndTimeUnixNano: 1, InsertedAt: time.Now()}})
	if got := searchIDs(t, s, SearchFilter{Q: "firstname"}); len(got) != 0 {
		t.Errorf("stale text still searchable: %v", got)
	}
	if got := searchIDs(t, s, SearchFilter{Q: "renamed"}); len(got) != 1 {
		t.Errorf("new text not searchable: %v", got)
	}
	// The index survives VACUUM (FTS rowids do not depend on spans.rowid).
	s.Vacuum(ctx)
	if got := searchIDs(t, s, SearchFilter{Q: "renamed"}); len(got) != 1 {
		t.Errorf("search broken after vacuum: %v", got)
	}
}

func TestSearchBackfill(t *testing.T) {
	path := t.TempDir() + "/bf.db"
	s, err := NewSQLiteStore(path, Options{CompressMinBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	s.WaitBackfill()
	ctx := context.Background()
	big := strings.Repeat("filler ", 800)
	s.InsertSpans(ctx, []SpanRecord{
		msgSpan("t1", "s1", "x", time.Now(), inputMsgs, outputMsgs),
		msgSpan("t2", "s2", "x", time.Now(), `[{"role":"user","parts":[{"type":"text","content":"`+big+` hiddenword"}]}]`, ""),
	})
	// Simulate a database from before the index existed.
	if _, err := s.db.Exec(`DELETE FROM span_fts; DELETE FROM span_search; DELETE FROM meta WHERE key = ?`, ftsBackfillKey); err != nil {
		t.Fatal(err)
	}
	if got := searchIDs(t, s, SearchFilter{Q: "deploy"}); len(got) != 0 {
		t.Fatal("setup: index should be empty")
	}
	s.Close()

	s, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.WaitBackfill()
	if got := searchIDs(t, s, SearchFilter{Q: "deploy"}); len(got) != 1 {
		t.Errorf("backfill missed plain span: %v", got)
	}
	if got := searchIDs(t, s, SearchFilter{Q: "hiddenword"}); len(got) != 1 {
		t.Errorf("backfill missed compressed span: %v", got)
	}
	var done string
	s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, ftsBackfillKey).Scan(&done)
	if done != "1" {
		t.Error("backfill not marked done")
	}
	// Re-running is a no-op (no duplicate index rows).
	n := 0
	s.db.QueryRow(`SELECT COUNT(*) FROM span_search`).Scan(&n)
	if n != 2 {
		t.Errorf("span_search rows = %d", n)
	}
}

func TestSearchTextCap(t *testing.T) {
	huge := strings.Repeat("word ", 20000) // 100 KB
	_, body := searchColumns("n", "", mustJSON(map[string]any{"user_prompt": huge}), "[]")
	if len(body) > searchTextBytes {
		t.Errorf("body %d bytes, cap %d", len(body), searchTextBytes)
	}
}
