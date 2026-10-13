package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func payloadCount(t *testing.T, s *SQLiteStore) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM span_payloads`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func bigAttrsSpan(id string) SpanRecord {
	long := strings.Repeat("Héllo wörld <&> ", 400) // ~6.4 KB, non-ASCII, HTML chars
	now := uint64(time.Now().UnixNano())
	return SpanRecord{
		TraceID: "t-" + id, SpanID: id, Name: "llm",
		AttributesJSON:    `{"gen_ai.input.messages":` + mustJSON(long) + `,"small":"keep","n":12345678901234567,"ok":true,"gen_ai.output.messages":` + mustJSON(long+"out") + `}`,
		EventsJSON:        `[{"name":"e","attributes":{"a":` + mustJSON(long) + `}}]`,
		StartTimeUnixNano: now, EndTimeUnixNano: now + 1, InsertedAt: time.Now(),
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestPayloadRoundTrip(t *testing.T) {
	s := optStore(t, Options{CompressMinBytes: 4096})
	ctx := context.Background()
	in := bigAttrsSpan("a")
	small := SpanRecord{TraceID: "t-s", SpanID: "s", Name: "small", AttributesJSON: `{"k":"v"}`,
		EventsJSON: "[]", StartTimeUnixNano: 1, EndTimeUnixNano: 2, InsertedAt: time.Now()}
	if err := s.InsertSpans(ctx, []SpanRecord{in, small}); err != nil {
		t.Fatal(err)
	}
	if got := payloadCount(t, s); got != 1 {
		t.Fatalf("payload rows = %d, want 1 (small span stays inline)", got)
	}

	// The stored columns are lite: far smaller, refs in place of big strings.
	var rawAttrs, rawEvents string
	s.db.QueryRow(`SELECT attributes_json, events_json FROM spans WHERE span_id = 'a'`).Scan(&rawAttrs, &rawEvents)
	if len(rawAttrs) > 600 || !strings.Contains(rawAttrs, `"$lookout_ref":"payload"`) || !strings.Contains(rawAttrs, `"bytes":`) {
		t.Errorf("lite attrs not lite: %s", rawAttrs)
	}
	if rawEvents != eventsSentinel {
		t.Errorf("events column = %s", rawEvents)
	}

	// Every read path returns the original strings.
	check := func(name string, spans []SpanRecord, err error) {
		t.Helper()
		if err != nil || len(spans) == 0 {
			t.Fatalf("%s: %v %d", name, err, len(spans))
		}
		for _, sp := range spans {
			if sp.SpanID != "a" {
				continue
			}
			if sp.AttributesJSON != in.AttributesJSON || sp.EventsJSON != in.EventsJSON {
				t.Errorf("%s: round trip mismatch", name)
			}
		}
	}
	tr, err := s.GetTrace(ctx, "t-a")
	check("GetTrace", tr, err)
	q, err := s.QuerySpans(ctx, SpanFilter{Limit: 10})
	check("QuerySpans", q, err)
	an, err := s.GetAnomalies(ctx, SpanFilter{Limit: 10}) // no anomalies: empty is fine
	_ = an
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetTrace(ctx, "t-s"); got[0].AttributesJSON != `{"k":"v"}` {
		t.Errorf("small span changed: %s", got[0].AttributesJSON)
	}
}

func TestPayloadEventsOnlyAndNoGain(t *testing.T) {
	s := optStore(t, Options{CompressMinBytes: 1000})
	ctx := context.Background()
	// Big events, small attributes: only events move.
	ev := `[{"name":"e","attributes":{"a":"` + strings.Repeat("z", 3000) + `"}}]`
	a := SpanRecord{TraceID: "t", SpanID: "ev", Name: "n", AttributesJSON: `{"k":"v"}`, EventsJSON: ev,
		StartTimeUnixNano: 1, EndTimeUnixNano: 2, InsertedAt: time.Now()}
	// Many small attributes, nothing worth moving.
	var parts []string
	for i := 0; i < 100; i++ {
		parts = append(parts, `"k`+string(rune('a'+i%26))+strings.Repeat("x", i)+`":"vvvvvvvvvv"`)
	}
	b := SpanRecord{TraceID: "t", SpanID: "many", Name: "n", AttributesJSON: "{" + strings.Join(parts, ",") + "}",
		EventsJSON: "[]", StartTimeUnixNano: 1, EndTimeUnixNano: 2, InsertedAt: time.Now()}
	s.InsertSpans(ctx, []SpanRecord{a, b})
	if got := payloadCount(t, s); got != 1 {
		t.Errorf("payload rows = %d, want only the events span", got)
	}
	for _, sp := range mustTrace(t, s, "t") {
		want := map[string]SpanRecord{"ev": a, "many": b}[sp.SpanID]
		if sp.AttributesJSON != want.AttributesJSON || sp.EventsJSON != want.EventsJSON {
			t.Errorf("%s mismatch", sp.SpanID)
		}
	}
}

func mustTrace(t *testing.T, s *SQLiteStore, id string) []SpanRecord {
	t.Helper()
	spans, err := s.GetTrace(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return spans
}

func TestPayloadFiltersOnSmallKeysStillMatch(t *testing.T) {
	s := optStore(t, Options{CompressMinBytes: 4096})
	ctx := context.Background()
	sp := bigAttrsSpan("f")
	sp.AttributesJSON = strings.Replace(sp.AttributesJSON, `"small":"keep"`, `"small":"keep","gen_ai.request.model":"m1"`, 1)
	s.InsertSpans(ctx, []SpanRecord{sp})
	if payloadCount(t, s) != 1 {
		t.Fatal("expected a payload row")
	}
	page, err := s.QueryTraceList(ctx, TraceListFilter{Attrs: []AttrFilter{{Key: "small", Value: "keep"}}})
	if err != nil || len(page.Traces) != 1 {
		t.Fatalf("attr filter on small key: %v %+v", err, page)
	}
	page, _ = s.QueryTraceList(ctx, TraceListFilter{Attrs: []AttrFilter{{Key: "small", Value: "nope"}}})
	if len(page.Traces) != 0 {
		t.Error("filter should not match")
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM spans WHERE json_extract(attributes_json, '$."gen_ai.request.model"') = 'm1'`).Scan(&n)
	if n != 1 {
		t.Errorf("json_extract on small key matched %d", n)
	}
	// Large integers survive the lite rewrite exactly.
	var lite string
	s.db.QueryRow(`SELECT attributes_json FROM spans WHERE span_id='f'`).Scan(&lite)
	if !strings.Contains(lite, "12345678901234567") {
		t.Errorf("number mangled: %s", lite)
	}
}

func TestPayloadDeletionCascades(t *testing.T) {
	s := optStore(t, Options{CompressMinBytes: 4096})
	ctx := context.Background()
	old := bigAttrsSpan("old")
	old.StartTimeUnixNano = uint64(time.Now().Add(-30 * 24 * time.Hour).UnixNano())
	old.EndTimeUnixNano = old.StartTimeUnixNano + 1
	fresh := bigAttrsSpan("fresh")
	s.InsertSpans(ctx, []SpanRecord{old, fresh})
	if payloadCount(t, s) != 2 {
		t.Fatal("setup")
	}
	if n, err := s.Cleanup(ctx, 7); err != nil || n != 1 {
		t.Fatalf("cleanup %d %v", n, err)
	}
	if got := payloadCount(t, s); got != 1 {
		t.Errorf("retention left %d payload rows, want 1", got)
	}

	// Replacing a span with a small version drops its payload.
	small := fresh
	small.AttributesJSON, small.EventsJSON = `{"k":"v"}`, "[]"
	s.InsertSpans(ctx, []SpanRecord{small})
	if got := payloadCount(t, s); got != 0 {
		t.Errorf("replace left %d payload rows", got)
	}
	if sp := mustTrace(t, s, "t-fresh")[0]; sp.AttributesJSON != `{"k":"v"}` {
		t.Errorf("replaced span attrs = %s", sp.AttributesJSON)
	}

	// Cap deletion too.
	s2 := optStore(t, Options{CompressMinBytes: 4096, MaxDBBytes: 1})
	s2.InsertSpans(ctx, []SpanRecord{bigAttrsSpan("x"), bigAttrsSpan("y")})
	if n, err := s2.Cleanup(ctx, 7); err != nil || n != 2 {
		t.Fatalf("cap cleanup %d %v", n, err)
	}
	if got := payloadCount(t, s2); got != 0 {
		t.Errorf("cap left %d payload rows", got)
	}
}

func TestCompactConvertsExistingRows(t *testing.T) {
	s := optStore(t, Options{}) // compression off: rows land inline
	ctx := context.Background()
	var in []SpanRecord
	for _, id := range []string{"a", "b", "c"} {
		in = append(in, bigAttrsSpan(id))
	}
	in = append(in, SpanRecord{TraceID: "t-s", SpanID: "s", Name: "small", AttributesJSON: `{"k":"v"}`,
		EventsJSON: "[]", StartTimeUnixNano: 1, EndTimeUnixNano: 2, InsertedAt: time.Now()})
	s.InsertSpans(ctx, in)
	if payloadCount(t, s) != 0 {
		t.Fatal("setup: compression should be off")
	}
	var before int
	s.db.QueryRow(`SELECT SUM(length(attributes_json)+length(events_json)) FROM spans`).Scan(&before)

	st, err := s.Compact(ctx, 4096, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Converted != 3 || st.Scanned != 3 || st.BytesOut >= st.BytesIn {
		t.Errorf("stats %+v", st)
	}
	if payloadCount(t, s) != 3 {
		t.Errorf("payload rows %d", payloadCount(t, s))
	}
	var after int
	s.db.QueryRow(`SELECT SUM(length(attributes_json)+length(events_json)) FROM spans`).Scan(&after)
	if after > before/5 {
		t.Errorf("span columns shrank %d -> %d, want >5x", before, after)
	}
	for _, want := range in[:3] {
		got := mustTrace(t, s, want.TraceID)[0]
		if got.AttributesJSON != want.AttributesJSON || got.EventsJSON != want.EventsJSON {
			t.Errorf("%s changed by compact", want.SpanID)
		}
	}

	// Idempotent: a second run converts nothing.
	st, err = s.Compact(ctx, 4096, nil)
	if err != nil || st.Converted != 0 {
		t.Errorf("second run: %+v %v", st, err)
	}
}

func TestPackSpanDisabledAndThreshold(t *testing.T) {
	big := `{"k":"` + strings.Repeat("x", 5000) + `"}`
	if a, e, blob, err := packSpan(big, "[]", 0); err != nil || blob != nil || a != big || e != "[]" {
		t.Error("min=0 must disable")
	}
	if _, _, blob, _ := packSpan(big, "[]", 100000); blob != nil {
		t.Error("below threshold must stay inline")
	}
	if _, _, blob, _ := packSpan(`not json `+strings.Repeat("x", 5000), "[]", 4096); blob != nil {
		t.Error("invalid JSON attrs must stay inline")
	}
}
