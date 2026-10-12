package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestBackfillServiceAndSession(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bf.db")
	ctx := context.Background()

	s, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s.WaitBackfill() // empty DB: marker gets set

	// Simulate rows written before this migration: no service_name, session
	// only inside the attribute JSON.
	now := uint64(time.Now().UnixNano())
	mk := func(id, attrs, res, sess string) SpanRecord {
		return SpanRecord{TraceID: "t", SpanID: id, Name: "n", AttributesJSON: attrs, ResourceJSON: res,
			AgentSessionID: sess, StartTimeUnixNano: now, EndTimeUnixNano: now + 1, InsertedAt: time.Now()}
	}
	spans := []SpanRecord{
		mk("native", `{"session.id":"claude-1"}`, `{"service.name":"nanoclaw-agent"}`, ""),
		mk("langfuse", `{"langfuse.session.id":"lf-1"}`, `{"service.name":"llm-proxy"}`, ""),
		mk("both", `{"langfuse.session.id":"lf-2","session.id":"claude-2"}`, `{"service.name":"a"}`, ""),
		mk("existing", `{"session.id":"claude-3"}`, `{"service.name":"a"}`, "agent-sess"),
		mk("resource", `{}`, `{"service.name":"a","session.id":"res-1"}`, ""),
		mk("none", `{"x":1}`, `{}`, ""),
		mk("badjson", `{not json session.id`, `also bad`, ""),
		mk("numeric", `{"session.id":42}`, `{"service.name":"a"}`, ""),
	}
	if err := s.InsertSpans(ctx, spans); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM meta WHERE key = ?`, backfillKey); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// Reopening runs the migration again, now over existing rows.
	s, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.WaitBackfill()

	rows, _ := s.GetTrace(ctx, "t")
	got := map[string]SpanRecord{}
	for _, r := range rows {
		got[r.SpanID] = r
	}
	want := map[string][2]string{ // service, session
		"native":   {"nanoclaw-agent", "claude-1"},
		"langfuse": {"llm-proxy", "lf-1"},
		"both":     {"a", "claude-2"},   // session.id beats langfuse.session.id
		"existing": {"a", "agent-sess"}, // agent.session_id beats both
		"resource": {"a", "res-1"},
		"none":     {"", ""},
		"badjson":  {"", ""}, // malformed JSON must not abort the batch
		"numeric":  {"a", "42"},
	}
	for id, w := range want {
		if got[id].ServiceName != w[0] || got[id].AgentSessionID != w[1] {
			t.Errorf("%s: service=%q session=%q want %v", id, got[id].ServiceName, got[id].AgentSessionID, w)
		}
	}

	// Sessions now group the backfilled rows.
	sess, _ := s.GetSessions(ctx, SessionFilter{Limit: 20})
	if len(sess) < 5 {
		t.Errorf("sessions after backfill: %d", len(sess))
	}

	// Marker set: a second run is a no-op even if rows look unfilled.
	s.db.Exec(`UPDATE spans SET service_name = '' WHERE span_id = 'native'`)
	if err := s.backfill(ctx); err != nil {
		t.Fatal(err)
	}
	r, _ := s.GetTrace(ctx, "t")
	for _, sp := range r {
		if sp.SpanID == "native" && sp.ServiceName != "" {
			t.Errorf("backfill ran again after completion")
		}
	}
}

// Many rows across several rowid windows: every window is processed.
func TestBackfillSpansMultipleBatches(t *testing.T) {
	s := testStore(t)
	s.WaitBackfill()
	ctx := context.Background()
	n := backfillBatch*2 + 17
	tx, _ := s.db.Begin()
	for i := 0; i < n; i++ {
		tx.Exec(`INSERT INTO spans (trace_id, span_id, name, start_time_unix_nano, end_time_unix_nano, resource_json, attributes_json)
			VALUES ('t', ?, 'n', 1, 2, '{"service.name":"svc"}', '{"session.id":"s"}')`, fmt.Sprintf("sp%d", i))
	}
	tx.Commit()
	s.db.Exec(`DELETE FROM meta WHERE key = ?`, backfillKey)
	if err := s.backfill(ctx); err != nil {
		t.Fatal(err)
	}
	var missing int
	s.db.QueryRow(`SELECT COUNT(*) FROM spans WHERE service_name <> 'svc' OR agent_session_id <> 's'`).Scan(&missing)
	if missing != 0 {
		t.Errorf("%d of %d rows not backfilled", missing, n)
	}
}
