package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/algonormative/lookout/internal/store"
)

func do(t *testing.T, srv *Server, method, url, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.server.Handler.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
}

func TestPostScoresSingleAndArray(t *testing.T) {
	srv, st := newTestServer(t)

	rec := do(t, srv, "POST", "/v1/scores",
		`{"id":"s1","trace_id":"t1","name":"promise_only","data_type":"BOOLEAN","value_num":1,"source":"jev","judge_version":"v2","metadata":{"a":1}}`)
	if rec.Code != 200 {
		t.Fatalf("single: %d %s", rec.Code, rec.Body.String())
	}
	var ids struct{ IDs []string }
	decode(t, rec, &ids)
	if len(ids.IDs) != 1 || ids.IDs[0] != "s1" {
		t.Errorf("ids: %+v", ids)
	}

	rec = do(t, srv, "POST", "/v1/scores", `[
		{"trace_id":"t1","span_id":"sp","name":"tone","data_type":"CATEGORICAL","value_str":"flat","source":"qwen3.5-4b"},
		{"id":"s1","trace_id":"t1","name":"promise_only","data_type":"BOOLEAN","value_num":0,"source":"jev"}]`)
	if rec.Code != 200 {
		t.Fatalf("array: %d %s", rec.Code, rec.Body.String())
	}
	decode(t, rec, &ids)
	if len(ids.IDs) != 2 || ids.IDs[0] == "" || ids.IDs[1] != "s1" {
		t.Errorf("array ids: %+v", ids)
	}

	// Idempotent on id: s1 was overwritten, not duplicated.
	n, _ := st.CountScores(context.Background(), store.ScoreFilter{})
	if n != 2 {
		t.Errorf("rows=%d want 2", n)
	}

	rec = do(t, srv, "GET", "/v1/scores?trace_id=t1&name=promise_only", "")
	var list struct {
		Scores []store.Score `json:"scores"`
	}
	decode(t, rec, &list)
	if len(list.Scores) != 1 || *list.Scores[0].ValueNum != 0 || list.Scores[0].JudgeVersion != "" {
		t.Errorf("overwrite: %+v", list.Scores)
	}
}

func TestPostScoresBadInput(t *testing.T) {
	srv, st := newTestServer(t)
	bad := []string{
		`not json`,
		`{}`,
		`{"trace_id":"t","name":"n","source":"x","data_type":"NUMERIC"}`,
		`{"trace_id":"t","name":"n","source":"x","data_type":"BOOLEAN","value_num":2}`,
		`{"trace_id":"t","name":"n","source":"x","data_type":"CATEGORICAL","value_num":1}`,
		`{"trace_id":"t","name":"n","source":"x","data_type":"WEIRD","value_num":1}`,
		`{"trace_id":"t","name":"n","source":"x","data_type":"NUMERIC","value_num":"high"}`,
		`[]`,
		`[{"trace_id":"t","name":"ok","source":"x","data_type":"NUMERIC","value_num":1},{"name":"missing trace"}]`,
	}
	for _, b := range bad {
		rec := do(t, srv, "POST", "/v1/scores", b)
		if rec.Code != 400 {
			t.Errorf("%s: want 400 got %d (%s)", b, rec.Code, rec.Body.String())
		}
	}
	if n, _ := st.CountScores(context.Background(), store.ScoreFilter{}); n != 0 {
		t.Errorf("bad requests persisted %d rows", n)
	}
	for _, u := range []string{"/v1/scores?since=banana", "/v1/scores?limit=x", "/v1/scores?cursor=!!", "/v1/scores?order=sideways"} {
		if rec := do(t, srv, "GET", u, ""); rec.Code != 400 {
			t.Errorf("%s: want 400 got %d", u, rec.Code)
		}
	}
}

func TestGetScoresCursorAndFilters(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, src := range []string{"a", "b", "c"} {
		rec := do(t, srv, "POST", "/v1/scores", `{"trace_id":"t","name":"n","data_type":"NUMERIC","value_num":1,"source":"`+src+`"}`)
		if rec.Code != 200 {
			t.Fatal(rec.Body.String())
		}
		time.Sleep(time.Millisecond)
	}
	var page struct {
		Scores []store.Score `json:"scores"`
		Next   string        `json:"next_cursor"`
	}
	decode(t, do(t, srv, "GET", "/v1/scores?limit=2&order=asc", ""), &page)
	if len(page.Scores) != 2 || page.Next == "" || page.Scores[0].Source != "a" {
		t.Fatalf("page1: %+v", page)
	}
	first := page.Next
	page.Next = ""
	decode(t, do(t, srv, "GET", "/v1/scores?limit=2&order=asc&cursor="+first, ""), &page)
	if len(page.Scores) != 1 || page.Scores[0].Source != "c" || page.Next != "" {
		t.Fatalf("page2: %+v", page)
	}
	decode(t, do(t, srv, "GET", "/v1/scores?source=b&since=1h", ""), &page)
	if len(page.Scores) != 1 || page.Scores[0].Source != "b" {
		t.Errorf("source filter: %+v", page.Scores)
	}
}

func TestLangfuseAliasShape(t *testing.T) {
	srv, st := newTestServer(t)
	st.InsertSpans(context.Background(), []store.SpanRecord{{TraceID: "t1", SpanID: "s", Name: "x",
		AgentSessionID: "sess", StartTimeUnixNano: 1, EndTimeUnixNano: 2, InsertedAt: time.Now()}})

	// numeric, source from metadata, configId preserved
	rec := do(t, srv, "POST", "/api/public/scores",
		`{"id":"lf1","traceId":"t1","observationId":"obs","name":"quality","value":0.8,"dataType":"NUMERIC","comment":"ok","configId":"cfg","metadata":{"source":"minicheck","k":"v"}}`)
	if rec.Code != 200 {
		t.Fatalf("post: %d %s", rec.Code, rec.Body.String())
	}
	var created map[string]string
	decode(t, rec, &created)
	if created["id"] != "lf1" {
		t.Errorf("response: %v", created)
	}
	// inferred types: bool, string; default source "api"
	for _, b := range []string{
		`{"traceId":"t1","name":"flag","value":true}`,
		`{"traceId":"t1","name":"label","value":"refusal"}`,
		`{"traceId":"t1","name":"flag01","dataType":"BOOLEAN","value":0}`,
	} {
		if rec := do(t, srv, "POST", "/api/public/scores", b); rec.Code != 200 {
			t.Fatalf("%s: %d %s", b, rec.Code, rec.Body.String())
		}
	}
	for _, b := range []string{
		`{"traceId":"t1","name":"n"}`,
		`{"traceId":"t1","name":"n","value":"x","dataType":"NUMERIC"}`,
		`{"traceId":"t1","name":"n","value":[1]}`,
		`{"name":"n","value":1}`,
		`{"traceId":"t1","name":"n","value":1,"metadata":"str"}`,
	} {
		if rec := do(t, srv, "POST", "/api/public/scores", b); rec.Code != 400 {
			t.Errorf("%s: want 400 got %d", b, rec.Code)
		}
	}

	rec = do(t, srv, "GET", "/api/public/scores?name=quality&limit=10", "")
	var resp struct {
		Data []map[string]any `json:"data"`
		Meta map[string]any   `json:"meta"`
	}
	decode(t, rec, &resp)
	if len(resp.Data) != 1 {
		t.Fatalf("data: %s", rec.Body.String())
	}
	d := resp.Data[0]
	if d["id"] != "lf1" || d["traceId"] != "t1" || d["observationId"] != "obs" || d["sessionId"] != "sess" ||
		d["name"] != "quality" || d["value"] != 0.8 || d["dataType"] != "NUMERIC" ||
		d["source"] != "minicheck" || d["comment"] != "ok" || d["configId"] != "cfg" {
		t.Errorf("score shape: %v", d)
	}
	if d["createdAt"] == nil || d["metadata"] == nil {
		t.Errorf("missing createdAt/metadata: %v", d)
	}
	if resp.Meta["page"] != 1.0 || resp.Meta["limit"] != 10.0 || resp.Meta["totalItems"] != 1.0 || resp.Meta["totalPages"] != 1.0 {
		t.Errorf("meta: %v", resp.Meta)
	}

	// Paging: 4 scores total, 3 per page -> 2 pages; second page has 1.
	decode(t, do(t, srv, "GET", "/api/public/scores?limit=3&page=2", ""), &resp)
	if len(resp.Data) != 1 || resp.Meta["totalItems"] != 4.0 || resp.Meta["totalPages"] != 2.0 || resp.Meta["page"] != 2.0 {
		t.Errorf("page 2: data=%d meta=%v", len(resp.Data), resp.Meta)
	}
	decode(t, do(t, srv, "GET", "/api/public/scores?traceId=t1&name=flag", ""), &resp)
	if len(resp.Data) != 1 || resp.Data[0]["source"] != "api" || resp.Data[0]["stringValue"] != "True" {
		t.Errorf("bool/default source: %v", resp.Data)
	}
	if rec := do(t, srv, "GET", "/api/public/scores?page=0", ""); rec.Code != 400 {
		t.Errorf("page=0: %d", rec.Code)
	}
}

func TestTraceListAndTraceScoresEndpoints(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	now := uint64(time.Now().Add(-time.Minute).UnixNano())
	st.InsertSpans(ctx, []store.SpanRecord{
		{TraceID: "t1", SpanID: "a", Name: "claude_code.interaction", ServiceName: "nanoclaw-agent",
			StartTimeUnixNano: now, EndTimeUnixNano: now + 100, InsertedAt: time.Now()},
		{TraceID: "t2", SpanID: "b", Name: "claude_code.interaction", ServiceName: "nanoclaw-agent",
			StartTimeUnixNano: now + 10, EndTimeUnixNano: now + 100, InsertedAt: time.Now()},
	})
	do(t, srv, "POST", "/v1/scores", `{"trace_id":"t1","name":"promise_only","data_type":"BOOLEAN","value_num":1,"source":"jev"}`)

	rec := do(t, srv, "GET",
		"/v1/trace-list?service=nanoclaw-agent&root_name=claude_code.interaction&missing_score=promise_only:jev&order=asc&limit=50", "")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var page store.TraceListPage
	decode(t, rec, &page)
	if len(page.Traces) != 1 || page.Traces[0].TraceID != "t2" {
		t.Fatalf("judge loop query: %+v", page)
	}

	decode(t, do(t, srv, "GET", "/v1/trace-list?limit=1", ""), &page)
	if len(page.Traces) != 1 || page.NextCursor == "" || page.Traces[0].TraceID != "t2" {
		t.Fatalf("newest first + cursor: %+v", page)
	}
	if len(page.Traces[0].Scores) != 0 {
		t.Errorf("t2 should have no scores")
	}
	next := page.NextCursor
	page = store.TraceListPage{}
	decode(t, do(t, srv, "GET", "/v1/trace-list?limit=1&cursor="+next, ""), &page)
	if len(page.Traces) != 1 || page.Traces[0].TraceID != "t1" || len(page.Traces[0].Scores) != 1 || page.NextCursor != "" {
		t.Fatalf("page 2: %+v", page)
	}

	for _, u := range []string{"/v1/trace-list?attr=novalue", "/v1/trace-list?missing_score=:jev", "/v1/trace-list?cursor=zz",
		"/v1/trace-list?since=soon", "/v1/trace-list?order=up", "/v1/trace-list?limit=-1"} {
		if rec := do(t, srv, "GET", u, ""); rec.Code != 400 {
			t.Errorf("%s: want 400 got %d", u, rec.Code)
		}
	}
	// empty result is an empty array, not null
	rec = do(t, srv, "GET", "/v1/trace-list?service=nobody", "")
	if !strings.Contains(rec.Body.String(), `"traces":[]`) {
		t.Errorf("empty page: %s", rec.Body.String())
	}

	// /v1/traces/{id}: default body is still a bare span array...
	rec = do(t, srv, "GET", "/v1/traces/t1", "")
	var spans []store.SpanRecord
	decode(t, rec, &spans)
	if len(spans) != 1 {
		t.Fatalf("default shape: %s", rec.Body.String())
	}
	// ...and include=scores adds the scores alongside the spans.
	rec = do(t, srv, "GET", "/v1/traces/t1?include=scores", "")
	var withScores struct {
		TraceID string             `json:"trace_id"`
		Spans   []store.SpanRecord `json:"spans"`
		Scores  []store.Score      `json:"scores"`
	}
	decode(t, rec, &withScores)
	if withScores.TraceID != "t1" || len(withScores.Spans) != 1 || len(withScores.Scores) != 1 || withScores.Scores[0].Name != "promise_only" {
		t.Errorf("include=scores: %s", rec.Body.String())
	}
}
