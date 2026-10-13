package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T) *SQLiteStore {
	t.Helper()
	dir := t.TempDir()
	s, err := NewSQLiteStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestInsertAndQuery(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	spans := []SpanRecord{
		{
			TraceID: "trace1", SpanID: "span1", Name: "gen_ai.chat_completion",
			StartTimeUnixNano: uint64(time.Now().Add(-time.Minute).UnixNano()),
			EndTimeUnixNano:   uint64(time.Now().UnixNano()),
			AIModel: "claude-sonnet-4", AIInputTokens: 1000, AIOutputTokens: 500,
			AgentName: "bosun", AgentSessionID: "sess1",
			CostUSD: 0.0105, StatusCode: 1,
			InsertedAt: time.Now(),
		},
		{
			TraceID: "trace1", SpanID: "span2", Name: "gen_ai.tool_call",
			ParentSpanID: "span1",
			StartTimeUnixNano: uint64(time.Now().Add(-30 * time.Second).UnixNano()),
			EndTimeUnixNano:   uint64(time.Now().Add(-25 * time.Second).UnixNano()),
			AgentName: "bosun", AgentSessionID: "sess1",
			StatusCode: 1, InsertedAt: time.Now(),
		},
		{
			TraceID: "trace2", SpanID: "span3", Name: "gen_ai.chat_completion",
			StartTimeUnixNano: uint64(time.Now().Add(-2 * time.Minute).UnixNano()),
			EndTimeUnixNano:   uint64(time.Now().Add(-time.Minute).UnixNano()),
			AIModel: "gpt-4o", AIInputTokens: 2000, AIOutputTokens: 800,
			CostUSD: 0.013, StatusCode: 2, StatusMessage: "rate_limit",
			Anomaly: "error: rate_limit",
			InsertedAt: time.Now(),
		},
	}

	if err := s.InsertSpans(ctx, spans); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Query all
	result, err := s.QuerySpans(ctx, SpanFilter{Limit: 10})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(result) != 3 {
		t.Fatalf("expected 3 spans, got %d", len(result))
	}

	// Query by model
	result, err = s.QuerySpans(ctx, SpanFilter{Model: "gpt-4o", Limit: 10})
	if err != nil {
		t.Fatalf("query by model: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 span, got %d", len(result))
	}

	// Get trace
	result, err = s.GetTrace(ctx, "trace1")
	if err != nil {
		t.Fatalf("get trace: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 spans in trace, got %d", len(result))
	}
}

func TestStats(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	spans := []SpanRecord{
		{
			TraceID: "t1", SpanID: "s1", Name: "chat",
			StartTimeUnixNano: uint64(time.Now().UnixNano()),
			EndTimeUnixNano:   uint64(time.Now().UnixNano()),
			AIModel: "claude-sonnet-4", AIInputTokens: 1000, AIOutputTokens: 500,
			AgentName: "bosun", AgentSessionID: "sess1",
			CostUSD: 0.0105, InsertedAt: time.Now(),
		},
		{
			TraceID: "t2", SpanID: "s2", Name: "chat",
			StartTimeUnixNano: uint64(time.Now().UnixNano()),
			EndTimeUnixNano:   uint64(time.Now().UnixNano()),
			AIModel: "gpt-4o", AIInputTokens: 2000, AIOutputTokens: 800,
			CostUSD: 0.013, StatusCode: 2,
			InsertedAt: time.Now(),
		},
	}
	s.InsertSpans(ctx, spans)

	stats, err := s.GetStats(ctx, SpanFilter{})
	if err != nil {
		t.Fatalf("get stats: %v", err)
	}
	if stats.TotalSpans != 2 {
		t.Errorf("expected 2 total spans, got %d", stats.TotalSpans)
	}
	if stats.TotalTraces != 2 {
		t.Errorf("expected 2 traces, got %d", stats.TotalTraces)
	}
	if stats.ErrorCount != 1 {
		t.Errorf("expected 1 error, got %d", stats.ErrorCount)
	}
	if stats.TotalSessions != 1 {
		t.Errorf("expected 1 session, got %d", stats.TotalSessions)
	}

	// By model
	modelStats, err := s.GetStatsByModel(ctx, SpanFilter{})
	if err != nil {
		t.Fatalf("get stats by model: %v", err)
	}
	if len(modelStats) != 2 {
		t.Fatalf("expected 2 model stats, got %d", len(modelStats))
	}
}

func TestSessions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	spans := []SpanRecord{
		{
			TraceID: "t1", SpanID: "s1", Name: "chat",
			StartTimeUnixNano: uint64(time.Now().Add(-time.Minute).UnixNano()),
			EndTimeUnixNano:   uint64(time.Now().UnixNano()),
			AgentName: "bosun", AgentSessionID: "sess1",
			AIModel: "claude-sonnet-4", CostUSD: 0.01,
			InsertedAt: time.Now(),
		},
		{
			TraceID: "t1", SpanID: "s2", Name: "tool",
			StartTimeUnixNano: uint64(time.Now().UnixNano()),
			EndTimeUnixNano:   uint64(time.Now().UnixNano()),
			AgentName: "bosun", AgentSessionID: "sess1",
			InsertedAt: time.Now(),
		},
	}
	s.InsertSpans(ctx, spans)

	sessions, err := s.GetSessions(ctx, SessionFilter{Limit: 10})
	if err != nil {
		t.Fatalf("get sessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].SpanCount != 2 {
		t.Errorf("expected 2 spans in session, got %d", sessions[0].SpanCount)
	}
}

func TestAnomalies(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	spans := []SpanRecord{
		{
			TraceID: "t1", SpanID: "s1", Name: "normal",
			StartTimeUnixNano: uint64(time.Now().UnixNano()),
			EndTimeUnixNano:   uint64(time.Now().UnixNano()),
			InsertedAt: time.Now(),
		},
		{
			TraceID: "t1", SpanID: "s2", Name: "anomalous",
			StartTimeUnixNano: uint64(time.Now().UnixNano()),
			EndTimeUnixNano:   uint64(time.Now().UnixNano()),
			Anomaly: "error: timeout", StatusCode: 2,
			InsertedAt: time.Now(),
		},
	}
	s.InsertSpans(ctx, spans)

	anomalies, err := s.GetAnomalies(ctx, SpanFilter{Limit: 10})
	if err != nil {
		t.Fatalf("get anomalies: %v", err)
	}
	if len(anomalies) != 1 {
		t.Fatalf("expected 1 anomaly, got %d", len(anomalies))
	}
}

func TestMetricRollups(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	now := time.Now().Unix()
	bucket := (now / 60) * 60

	// Upsert twice - should aggregate
	r := MetricRollup{
		Name: "test.metric", LabelsJSON: "{}",
		BucketStart: bucket, BucketWidth: 60,
		MetricType: "sum", Count: 1, Sum: 10, Min: 10, Max: 10, Last: 10,
	}
	s.UpsertMetricRollup(ctx, r)
	r.Sum = 5
	r.Min = 5
	r.Max = 15
	r.Last = 5
	s.UpsertMetricRollup(ctx, r)

	rollups, err := s.QueryMetricRollups(ctx, MetricFilter{
		Name: "test.metric", BucketWidth: 60,
	})
	if err != nil {
		t.Fatalf("query rollups: %v", err)
	}
	if len(rollups) != 1 {
		t.Fatalf("expected 1 rollup, got %d", len(rollups))
	}
	if rollups[0].Count != 2 {
		t.Errorf("expected count 2, got %d", rollups[0].Count)
	}
	if rollups[0].Sum != 15 {
		t.Errorf("expected sum 15, got %.1f", rollups[0].Sum)
	}
	if rollups[0].Min != 5 {
		t.Errorf("expected min 5, got %.1f", rollups[0].Min)
	}
	if rollups[0].Max != 15 {
		t.Errorf("expected max 15, got %.1f", rollups[0].Max)
	}

	// List names
	names, err := s.ListMetricNames(ctx)
	if err != nil {
		t.Fatalf("list names: %v", err)
	}
	if len(names) != 1 || names[0] != "test.metric" {
		t.Errorf("expected [test.metric], got %v", names)
	}
}

func TestCleanup(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Insert old span
	old := SpanRecord{
		TraceID: "old", SpanID: "old1", Name: "old",
		StartTimeUnixNano: uint64(time.Now().Add(-30 * 24 * time.Hour).UnixNano()),
		EndTimeUnixNano:   uint64(time.Now().Add(-30 * 24 * time.Hour).UnixNano()),
		InsertedAt: time.Now(),
	}
	// Insert recent span
	recent := SpanRecord{
		TraceID: "new", SpanID: "new1", Name: "new",
		StartTimeUnixNano: uint64(time.Now().UnixNano()),
		EndTimeUnixNano:   uint64(time.Now().UnixNano()),
		InsertedAt: time.Now(),
	}
	s.InsertSpans(ctx, []SpanRecord{old, recent})

	n, err := s.Cleanup(ctx, 7)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 deleted, got %d", n)
	}

	// Verify remaining
	all, _ := s.QuerySpans(ctx, SpanFilter{Limit: 10})
	if len(all) != 1 || all[0].SpanID != "new1" {
		t.Errorf("expected only new span remaining, got %v", all)
	}
}

func TestCleanupVacuumsOncePerDay(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	old := time.Now().Add(-30 * 24 * time.Hour)
	mk := func(id string) SpanRecord {
		return SpanRecord{
			TraceID: id, SpanID: id, Name: "old", AttributesJSON: `{"k":"` + strings.Repeat("x", 200000) + `"}`,
			StartTimeUnixNano: uint64(old.UnixNano()), EndTimeUnixNano: uint64(old.UnixNano()), InsertedAt: time.Now(),
		}
	}
	freelist := func() int {
		var n int
		if err := s.db.QueryRow("PRAGMA freelist_count").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	s.InsertSpans(ctx, []SpanRecord{mk("a"), mk("b")})
	if n, _ := s.Cleanup(ctx, 7); n != 2 {
		t.Fatalf("deleted %d", n)
	}
	if fl := freelist(); fl != 0 {
		t.Errorf("first cleanup should vacuum, freelist=%d", fl)
	}

	// Second cleanup within a day deletes rows but does not vacuum again.
	s.InsertSpans(ctx, []SpanRecord{mk("c")})
	if n, _ := s.Cleanup(ctx, 7); n != 1 {
		t.Fatalf("deleted %d", n)
	}
	if freelist() == 0 {
		t.Error("vacuum should be throttled to once per day")
	}

	// After the interval it runs again.
	s.lastVacuum = time.Now().Add(-25 * time.Hour)
	s.InsertSpans(ctx, []SpanRecord{mk("d")})
	s.Cleanup(ctx, 7)
	if fl := freelist(); fl != 0 {
		t.Errorf("expected vacuum after interval, freelist=%d", fl)
	}
}

// Ensure db path doesn't leak temp dirs
func TestDBDir(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "subdir", "test.db")
	os.MkdirAll(filepath.Dir(dbPath), 0755)
	s, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	s.Close()
}

// A host-side reader (the status dashboard opens lookout.db directly) holds a
// SHARED lock while it reads; a write that lands meanwhile must wait for it,
// not fail at once with SQLITE_BUSY.
func TestWriteWaitsForOtherReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	s.InsertSpans(ctx, []SpanRecord{{TraceID: "t1", SpanID: "s1", Name: "root",
		StartTimeUnixNano: 1, EndTimeUnixNano: 2, InsertedAt: time.Now()}})

	<-s.backfillDone // keep the background backfill out of the measured window

	other, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	defer other.Close()
	tx, err := other.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	var n int
	if err := tx.QueryRow("SELECT count(*) FROM spans").Scan(&n); err != nil {
		t.Fatalf("read: %v", err)
	}
	time.AfterFunc(300*time.Millisecond, func() { tx.Rollback() })

	if _, err := s.UpsertScores(ctx, []Score{{TraceID: "t1", Name: "promise_only",
		DataType: "numeric", ValueNum: f64(1), Source: "jev"}}); err != nil {
		t.Fatalf("write while another connection reads: %v", err)
	}
}
