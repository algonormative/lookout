package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func f64(v float64) *float64 { return &v }

func TestScoreUpsertIdempotentAndSessionCopy(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	s.InsertSpans(ctx, []SpanRecord{{TraceID: "t1", SpanID: "s1", Name: "root", AgentSessionID: "sess-1",
		StartTimeUnixNano: 1, EndTimeUnixNano: 2, InsertedAt: time.Now()}})

	ids, err := s.UpsertScores(ctx, []Score{{
		ID: "sc-1", TraceID: "t1", Name: "promise_only", DataType: "numeric",
		ValueNum: f64(0.25), Source: "jev", JudgeVersion: "v1", Comment: "first",
		Metadata: []byte(`{"k":"v"}`),
	}})
	if err != nil || len(ids) != 1 || ids[0] != "sc-1" {
		t.Fatalf("upsert: ids=%v err=%v", ids, err)
	}
	got, _, _ := s.QueryScores(ctx, ScoreFilter{TraceID: "t1"})
	if len(got) != 1 {
		t.Fatalf("want 1 score, got %d", len(got))
	}
	first := got[0]
	if first.SessionID != "sess-1" {
		t.Errorf("session not copied from trace: %q", first.SessionID)
	}
	if first.DataType != ScoreNumeric || first.ValueNum == nil || *first.ValueNum != 0.25 {
		t.Errorf("bad row: %+v", first)
	}
	if string(first.Metadata) != `{"k":"v"}` || first.SpanID != "" {
		t.Errorf("metadata/span: %+v", first)
	}

	// Re-post the same id with a new value: still one row, created_at kept.
	time.Sleep(2 * time.Millisecond)
	if _, err := s.UpsertScores(ctx, []Score{{
		ID: "sc-1", TraceID: "t1", Name: "promise_only", DataType: ScoreNumeric,
		ValueNum: f64(0.75), Source: "jev", Comment: "second",
	}}); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.QueryScores(ctx, ScoreFilter{})
	if len(got) != 1 {
		t.Fatalf("upsert duplicated: %d rows", len(got))
	}
	if *got[0].ValueNum != 0.75 || got[0].Comment != "second" {
		t.Errorf("not updated: %+v", got[0])
	}
	if got[0].CreatedAt != first.CreatedAt {
		t.Errorf("created_at changed on re-post: %d -> %d", first.CreatedAt, got[0].CreatedAt)
	}
	if n, _ := s.CountScores(ctx, ScoreFilter{}); n != 1 {
		t.Errorf("count=%d", n)
	}
}

func TestScoreGeneratedIDAndTypes(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	ids, err := s.UpsertScores(ctx, []Score{
		{TraceID: "t", Name: "ok", DataType: ScoreBoolean, ValueNum: f64(1), Source: "eval"},
		{TraceID: "t", SpanID: "sp", Name: "label", DataType: ScoreCategorical, ValueStr: "refusal", ValueNum: f64(0.9), Source: "eval"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] == "" || ids[0] == ids[1] {
		t.Fatalf("ids: %v", ids)
	}
	got, _, _ := s.QueryScores(ctx, ScoreFilter{SpanID: "sp"})
	if len(got) != 1 || got[0].ValueStr != "refusal" || *got[0].ValueNum != 0.9 {
		t.Errorf("span filter: %+v", got)
	}
}

func TestScoreValidation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	bad := []Score{
		{Name: "n", DataType: ScoreNumeric, ValueNum: f64(1), Source: "x"},                                        // no trace
		{TraceID: "t", DataType: ScoreNumeric, ValueNum: f64(1), Source: "x"},                                     // no name
		{TraceID: "t", Name: "n", DataType: ScoreNumeric, ValueNum: f64(1)},                                       // no source
		{TraceID: "t", Name: "n", Source: "x", ValueNum: f64(1)},                                                  // no data_type
		{TraceID: "t", Name: "n", DataType: "FLOAT", ValueNum: f64(1), Source: "x"},                               // unknown type
		{TraceID: "t", Name: "n", DataType: ScoreNumeric, Source: "x"},                                            // numeric w/o value
		{TraceID: "t", Name: "n", DataType: ScoreNumeric, ValueNum: f64(1), ValueStr: "a", Source: "x"},           // numeric w/ label
		{TraceID: "t", Name: "n", DataType: ScoreBoolean, ValueNum: f64(0.5), Source: "x"},                        // bool not 0/1
		{TraceID: "t", Name: "n", DataType: ScoreCategorical, ValueNum: f64(1), Source: "x"},                      // categorical w/o label
		{TraceID: "t", Name: "n", DataType: ScoreNumeric, ValueNum: f64(1), Source: "x", Metadata: []byte(`[1]`)}, // metadata not an object
	}
	for i, sc := range bad {
		if _, err := s.UpsertScores(ctx, []Score{sc}); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("case %d: want ErrInvalidInput, got %v", i, err)
		}
	}
	// A bad score in a batch rejects the whole batch.
	_, err := s.UpsertScores(ctx, []Score{
		{TraceID: "t", Name: "good", DataType: ScoreNumeric, ValueNum: f64(1), Source: "x"},
		bad[0],
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("batch: %v", err)
	}
	if n, _ := s.CountScores(ctx, ScoreFilter{}); n != 0 {
		t.Errorf("partial batch persisted: %d rows", n)
	}
}

func TestScoreQueryFiltersAndCursor(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		src := "jev"
		if i%2 == 1 {
			src = "minicheck"
		}
		if _, err := s.UpsertScores(ctx, []Score{{
			ID: string(rune('a' + i)), TraceID: "t", Name: "n", DataType: ScoreNumeric,
			ValueNum: f64(float64(i)), Source: src,
		}}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond) // distinct created_at
	}

	if got, _, _ := s.QueryScores(ctx, ScoreFilter{Source: "minicheck"}); len(got) != 2 {
		t.Errorf("source filter: %d", len(got))
	}
	if got, _, _ := s.QueryScores(ctx, ScoreFilter{Name: "nope"}); len(got) != 0 {
		t.Errorf("name filter: %d", len(got))
	}
	if got, _, _ := s.QueryScores(ctx, ScoreFilter{Since: time.Now().Add(time.Hour)}); len(got) != 0 {
		t.Errorf("since filter: %d", len(got))
	}

	// Page through newest-first with limit 2 and make sure every row appears once.
	var seen []string
	cursor := ""
	for pages := 0; pages < 10; pages++ {
		got, next, err := s.QueryScores(ctx, ScoreFilter{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, sc := range got {
			seen = append(seen, sc.ID)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if want := []string{"e", "d", "c", "b", "a"}; !equalStrings(seen, want) {
		t.Errorf("desc paging: got %v want %v", seen, want)
	}

	// Ascending.
	seen, cursor = nil, ""
	for pages := 0; pages < 10; pages++ {
		got, next, _ := s.QueryScores(ctx, ScoreFilter{Limit: 2, Cursor: cursor, Asc: true})
		for _, sc := range got {
			seen = append(seen, sc.ID)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if want := []string{"a", "b", "c", "d", "e"}; !equalStrings(seen, want) {
		t.Errorf("asc paging: got %v want %v", seen, want)
	}

	if _, _, err := s.QueryScores(ctx, ScoreFilter{Cursor: "!!!"}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("bad cursor: %v", err)
	}
	// Offset paging (Langfuse alias).
	got, _, _ := s.QueryScores(ctx, ScoreFilter{Limit: 2, Offset: 4})
	if len(got) != 1 || got[0].ID != "a" {
		t.Errorf("offset: %+v", got)
	}
}

func TestScoresSurviveCleanup(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	old := uint64(time.Now().Add(-30 * 24 * time.Hour).UnixNano())
	s.InsertSpans(ctx, []SpanRecord{{TraceID: "t", SpanID: "s", Name: "x", StartTimeUnixNano: old, EndTimeUnixNano: old + 1, InsertedAt: time.Now()}})
	s.UpsertScores(ctx, []Score{{TraceID: "t", Name: "n", DataType: ScoreBoolean, ValueNum: f64(0), Source: "x"}})
	if n, err := s.Cleanup(ctx, 7); err != nil || n != 1 {
		t.Fatalf("cleanup n=%d err=%v", n, err)
	}
	if n, _ := s.CountScores(ctx, ScoreFilter{TraceID: "t"}); n != 1 {
		t.Errorf("score deleted with its spans: %d", n)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
