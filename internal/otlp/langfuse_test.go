package otlp

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	collectorpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/algonormative/lookout/internal/ai"
	"github.com/algonormative/lookout/internal/store"
)

func dblKV(k string, v float64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: v}}}
}

func convert1(name string, attrs []*commonpb.KeyValue, res ...*commonpb.KeyValue) store.SpanRecord {
	req := []*tracepb.ResourceSpans{{
		Resource:   &resourcepb.Resource{Attributes: append([]*commonpb.KeyValue{strKV("service.name", "svc")}, res...)},
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{mkSpan("spanspan", "", name, attrs...)}}},
	}}
	return ConvertTraceRequest(nil, req)[0]
}

func TestLangfuseIdentityMapping(t *testing.T) {
	cases := []struct {
		name                    string
		span                    string
		attrs                   []*commonpb.KeyValue
		res                     []*commonpb.KeyValue
		user, traceName, obsTyp string
	}{
		{"langfuse attrs", "n", []*commonpb.KeyValue{strKV("langfuse.user.id", "u1"), strKV("langfuse.trace.name", "chat"),
			strKV("langfuse.observation.type", "generation")}, nil, "u1", "chat", "generation"},
		{"user.id fallback", "n", []*commonpb.KeyValue{strKV("user.id", "hash")}, nil, "hash", "", "span"},
		{"langfuse beats user.id", "n", []*commonpb.KeyValue{strKV("user.id", "hash"), strKV("langfuse.user.id", "u2")}, nil, "u2", "", "span"},
		{"resource user", "n", nil, []*commonpb.KeyValue{strKV("langfuse.user.id", "ru")}, "ru", "", "span"},
		{"span user beats resource", "n", []*commonpb.KeyValue{strKV("user.id", "su")}, []*commonpb.KeyValue{strKV("user.id", "ru")}, "su", "", "span"},
		{"infer generation", "n", []*commonpb.KeyValue{strKV("gen_ai.request.model", "m")}, nil, "", "", "generation"},
		{"infer tool", "claude_code.tool", nil, nil, "", "", "tool"},
		{"tool child is a span", "claude_code.tool.execution", nil, nil, "", "", "span"},
		{"explicit type wins", "claude_code.tool", []*commonpb.KeyValue{strKV("langfuse.observation.type", "event")}, nil, "", "", "event"},
		{"default span", "n", nil, nil, "", "", "span"},
	}
	for _, tc := range cases {
		r := convert1(tc.span, tc.attrs, tc.res...)
		if r.UserID != tc.user || r.TraceName != tc.traceName || r.ObsType != tc.obsTyp {
			t.Errorf("%s: user=%q trace_name=%q obs_type=%q, want %q %q %q", tc.name, r.UserID, r.TraceName, r.ObsType, tc.user, tc.traceName, tc.obsTyp)
		}
	}
}

func TestLangfuseSessionStillMapped(t *testing.T) {
	r := convert1("n", []*commonpb.KeyValue{strKV("langfuse.session.id", "lf-1")})
	if r.AgentSessionID != "lf-1" {
		t.Errorf("session = %q", r.AgentSessionID)
	}
}

func TestCostFromAttributes(t *testing.T) {
	rec := func(attrs ...*commonpb.KeyValue) store.SpanRecord {
		r := convert1("n", append([]*commonpb.KeyValue{strKV("gen_ai.request.model", "claude-sonnet-4"),
			intKV("gen_ai.usage.input_tokens", 1000), intKV("gen_ai.usage.output_tokens", 1000)}, attrs...))
		ai.Enrich(&r)
		return r
	}
	if c := rec(dblKV("gen_ai.cost.total_cost", 0.1234)).CostUSD; c != 0.1234 {
		t.Errorf("total_cost: %v", c)
	}
	if c := rec(dblKV("gen_ai.usage.cost", 0.5)).CostUSD; c != 0.5 {
		t.Errorf("usage.cost: %v", c)
	}
	if c := rec(dblKV("gen_ai.cost.total_cost", 0.2), dblKV("gen_ai.usage.cost", 0.5)).CostUSD; c != 0.2 {
		t.Errorf("total_cost should win: %v", c)
	}
	pricing := ai.CalculateCost("claude-sonnet-4", 1000, 1000)
	if pricing <= 0 {
		t.Fatal("test assumes claude-sonnet-4 is in the pricing table")
	}
	// LiteLLM reports 0 for models it cannot price: fall back to the table.
	if c := rec(dblKV("gen_ai.cost.total_cost", 0)).CostUSD; c != pricing {
		t.Errorf("zero reported cost should fall back to pricing table: %v vs %v", c, pricing)
	}
	if c := rec().CostUSD; c != pricing {
		t.Errorf("no reported cost: %v vs %v", c, pricing)
	}
}

func newReceiverWithStore(t *testing.T) (*HTTPReceiver, *store.SQLiteStore) {
	t.Helper()
	s, err := store.NewSQLiteStore(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return NewHTTPReceiver("127.0.0.1:0", s, nil, nil, nil), s
}

func exportBody(t *testing.T, gz bool) *bytes.Reader {
	t.Helper()
	req := &collectorpb.ExportTraceServiceRequest{ResourceSpans: mkReq("llm-proxy",
		mkSpan("spanspan", "", "generation", strKV("langfuse.user.id", "u9"), strKV("langfuse.trace.name", "t-name")))}
	b, err := proto.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if gz {
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		w.Write(b)
		w.Close()
		b = buf.Bytes()
	}
	return bytes.NewReader(b)
}

func post(r *HTTPReceiver, path string, body *bytes.Reader, mutate func(*http.Request)) int {
	req := httptest.NewRequest("POST", path, body)
	req.Header.Set("Content-Type", "application/x-protobuf")
	if mutate != nil {
		mutate(req)
	}
	w := httptest.NewRecorder()
	r.server.Handler.ServeHTTP(w, req)
	return w.Code
}

func TestLangfuseOTLPPathAndAuth(t *testing.T) {
	const path = "/api/public/otel/v1/traces"

	// No key configured: anything is accepted, including no credentials.
	r, s := newReceiverWithStore(t)
	if code := post(r, path, exportBody(t, false), nil); code != 200 {
		t.Fatalf("open ingest: %d", code)
	}
	page, err := s.QueryTraceList(t.Context(), store.TraceListFilter{User: "u9", TraceName: "t-name"})
	if err != nil || len(page.Traces) != 1 || page.Traces[0].UserID != "u9" || page.Traces[0].TraceName != "t-name" {
		t.Fatalf("trace not stored with identity: %v %+v", err, page)
	}

	// With a key: Basic password or Bearer; wrong or missing is 401. Plain
	// /v1/traces stays open.
	r, _ = newReceiverWithStore(t)
	r.SetIngestKey("sk-secret")
	basic := func(user, pass string) func(*http.Request) {
		return func(req *http.Request) { req.SetBasicAuth(user, pass) }
	}
	for name, tc := range map[string]struct {
		mutate func(*http.Request)
		want   int
	}{
		"basic ok":     {basic("pk-anything", "sk-secret"), 200},
		"basic wrong":  {basic("pk", "nope"), 401},
		"no auth":      {nil, 401},
		"bearer ok":    {func(q *http.Request) { q.Header.Set("Authorization", "Bearer sk-secret") }, 200},
		"bearer wrong": {func(q *http.Request) { q.Header.Set("Authorization", "Bearer x") }, 401},
		"gzip body ok": {func(q *http.Request) { q.SetBasicAuth("p", "sk-secret"); q.Header.Set("Content-Encoding", "gzip") }, 200},
	} {
		gz := tc.mutate != nil && name == "gzip body ok"
		if code := post(r, path, exportBody(t, gz), tc.mutate); code != tc.want {
			t.Errorf("%s: %d, want %d", name, code, tc.want)
		}
	}
	if code := post(r, "/v1/traces", exportBody(t, false), nil); code != 200 {
		t.Errorf("/v1/traces must not require the Langfuse key: %d", code)
	}
}
