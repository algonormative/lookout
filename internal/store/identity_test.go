package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestTraceListUserTraceNameFilters(t *testing.T) {
	s := optStore(t, Options{})
	ctx := context.Background()
	now := uint64(time.Now().UnixNano())
	mk := func(trace, span, user, name, typ string) SpanRecord {
		return SpanRecord{TraceID: trace, SpanID: span, Name: "n", UserID: user, TraceName: name, ObsType: typ,
			StartTimeUnixNano: now, EndTimeUnixNano: now + 1, InsertedAt: time.Now()}
	}
	s.InsertSpans(ctx, []SpanRecord{
		mk("t1", "a", "alice", "chat", "generation"),
		mk("t1", "b", "", "", "tool"),
		mk("t2", "c", "bob", "batch", "span"),
	})
	ids := func(f TraceListFilter) string {
		p, err := s.QueryTraceList(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		out := ""
		for _, tr := range p.Traces {
			out += tr.TraceID + ","
		}
		return out
	}
	if got := ids(TraceListFilter{User: "alice"}); got != "t1," {
		t.Errorf("user filter: %q", got)
	}
	if got := ids(TraceListFilter{TraceName: "batch"}); got != "t2," {
		t.Errorf("trace_name filter: %q", got)
	}
	if got := ids(TraceListFilter{User: "alice", TraceName: "batch"}); got != "" {
		t.Errorf("combined filters should AND: %q", got)
	}
	p, _ := s.QueryTraceList(ctx, TraceListFilter{User: "alice"})
	if tr := p.Traces[0]; tr.UserID != "alice" || tr.TraceName != "chat" || tr.ObsType != "span" && tr.ObsType != "generation" {
		t.Errorf("summary fields: %+v", tr)
	}
}

func TestIdentityBackfill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "id.db")
	s, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s.WaitBackfill()
	ctx := context.Background()
	now := uint64(time.Now().UnixNano())
	mk := func(id, name, model, attrs, res string) SpanRecord {
		return SpanRecord{TraceID: "t", SpanID: id, Name: name, AIModel: model, AttributesJSON: attrs, ResourceJSON: res,
			StartTimeUnixNano: now, EndTimeUnixNano: now + 1, InsertedAt: time.Now()}
	}
	s.InsertSpans(ctx, []SpanRecord{
		mk("lf", "x", "", `{"langfuse.user.id":"u1","user.id":"h","langfuse.trace.name":"chat","langfuse.observation.type":"event"}`, `{}`),
		mk("userid", "x", "", `{"user.id":"h2"}`, `{}`),
		mk("res", "x", "", `{}`, `{"langfuse.user.id":"ru"}`),
		mk("gen", "x", "m", `{}`, `{}`),
		mk("tool", "claude_code.tool", "", `{}`, `{}`),
		mk("plain", "x", "", `not json`, `also not`),
	})
	// Simulate rows written before the columns existed.
	if _, err := s.db.Exec(`UPDATE spans SET user_id = '', trace_name = '', obs_type = ''; DELETE FROM meta WHERE key = ?`, identityKey); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.WaitBackfill()
	got := map[string][3]string{}
	rows, _ := s.GetTrace(ctx, "t")
	for _, r := range rows {
		got[r.SpanID] = [3]string{r.UserID, r.TraceName, r.ObsType}
	}
	want := map[string][3]string{
		"lf":     {"u1", "chat", "event"},
		"userid": {"h2", "", "span"},
		"res":    {"ru", "", "span"},
		"gen":    {"", "", "generation"},
		"tool":   {"", "", "tool"},
		"plain":  {"", "", "span"},
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: %v, want %v", id, got[id], w)
		}
	}
}

func TestSearchLangfuseObservationText(t *testing.T) {
	s := optStore(t, Options{})
	ctx := context.Background()
	s.InsertSpans(ctx, []SpanRecord{{TraceID: "t", SpanID: "s", Name: "generation",
		AttributesJSON:    `{"langfuse.observation.input":"summarise the quarterly zeppelin report","langfuse.observation.output":"done"}`,
		StartTimeUnixNano: 1, EndTimeUnixNano: 2, InsertedAt: time.Now()}})
	if got := searchIDs(t, s, SearchFilter{Q: "zeppelin"}); len(got) != 1 {
		t.Errorf("langfuse.observation.input not searchable: %v", got)
	}
}
