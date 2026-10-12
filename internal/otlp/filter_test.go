package otlp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/algonormative/lookout/internal/config"
)

func strKV(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func mkSpan(id, parent, name string, attrs ...*commonpb.KeyValue) *tracepb.Span {
	s := &tracepb.Span{TraceId: []byte("trace-0000000001"), SpanId: []byte(id), Name: name, Attributes: attrs}
	if parent != "" {
		s.ParentSpanId = []byte(parent)
	}
	return s
}

func mkReq(service string, spans ...*tracepb.Span) []*tracepb.ResourceSpans {
	return []*tracepb.ResourceSpans{{
		Resource:   &resourcepb.Resource{Attributes: []*commonpb.KeyValue{strKV("service.name", service)}},
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}},
	}}
}

func hexOf(s string) string {
	const d = "0123456789abcdef"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		b.WriteByte(d[s[i]>>4])
		b.WriteByte(d[s[i]&15])
	}
	return b.String()
}

func TestParseSpanList(t *testing.T) {
	m, err := ParseSpanList(" llm-proxy:postgres , a:b:c ,, ")
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 2 {
		t.Fatalf("want 2 entries, got %v", m)
	}
	if _, ok := m[spanKey{"a", "b:c"}]; !ok {
		t.Errorf("service should split on first colon: %v", m)
	}
	if m, err := ParseSpanList(""); err != nil || len(m) != 0 {
		t.Errorf("empty must disable: %v %v", m, err)
	}
	for _, bad := range []string{"nocolon", ":x", "x:"} {
		if _, err := ParseSpanList(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestDefaultsAndEnv(t *testing.T) {
	cfg := config.Default()
	t.Setenv("LOOKOUT_DROP_SPANS", "")
	t.Setenv("LOOKOUT_MAX_ATTR_BYTES", "0")
	cfg.ApplyEnv()
	if cfg.DropSpans != "" || cfg.MaxAttrBytes != 0 {
		t.Fatalf("empty/0 env must disable, got %q %d", cfg.DropSpans, cfg.MaxAttrBytes)
	}

	cfg = config.Default()
	t.Setenv("LOOKOUT_DROP_SPANS", "svc:a,svc:b")
	cfg.ApplyEnv()
	f, err := NewFilter(cfg.DropSpans, cfg.SlimSpans, cfg.MaxAttrBytes, cfg.SlimAttrBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !f.shouldDrop("svc", "b") || f.shouldDrop("llm-proxy", "postgres") {
		t.Error("env override should replace the default list")
	}

	def, err := NewFilter(config.DefaultDropSpans, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"postgres", "auth", "self", "proxy_pre_call", "batch_write_to_db", "router"} {
		if !def.shouldDrop("llm-proxy", n) {
			t.Errorf("default should drop llm-proxy:%s", n)
		}
	}
	if def.shouldDrop("llm-proxy", "Received Proxy Server Request") || def.shouldDrop("other", "postgres") {
		t.Error("default over-drops")
	}
}

func TestDropAndReparent(t *testing.T) {
	f, _ := NewFilter("llm-proxy:router,llm-proxy:self", "", 0, 0)
	// root -> router(dropped) -> self(dropped) -> llm_call(kept)
	// root -> kept2
	req := mkReq("llm-proxy",
		mkSpan("root", "", "root"),
		mkSpan("rtr", "root", "router"),
		mkSpan("slf", "rtr", "self"),
		mkSpan("llm", "slf", "llm_call"),
		mkSpan("k2", "root", "other"),
	)
	got := ConvertTraceRequest(f, req)
	if len(got) != 3 {
		t.Fatalf("want 3 kept spans, got %d", len(got))
	}
	byName := map[string]string{}
	for _, s := range got {
		byName[s.Name] = s.ParentSpanID
	}
	if byName["llm_call"] != hexOf("root") {
		t.Errorf("llm_call should be re-parented to root, got %q", byName["llm_call"])
	}
	if byName["other"] != hexOf("root") {
		t.Errorf("unaffected parent changed: %q", byName["other"])
	}
}

func TestDropParentOutsideBatch(t *testing.T) {
	f, _ := NewFilter("llm-proxy:router", "", 0, 0)
	// child's dropped parent is not in this batch: keep original parent id.
	got := ConvertTraceRequest(f, mkReq("llm-proxy", mkSpan("c", "missing", "child")))
	if len(got) != 1 || got[0].ParentSpanID != hexOf("missing") {
		t.Errorf("want original parent kept, got %+v", got)
	}
	// dropped span whose own parent isn't in batch: child gets that parent id.
	got = ConvertTraceRequest(f, mkReq("llm-proxy", mkSpan("r", "up", "router"), mkSpan("c", "r", "child")))
	if len(got) != 1 || got[0].ParentSpanID != hexOf("up") {
		t.Errorf("want dropped span's parent, got %+v", got)
	}
}

func TestDropIsServiceScoped(t *testing.T) {
	f, _ := NewFilter("llm-proxy:auth", "", 0, 0)
	if n := len(ConvertTraceRequest(f, mkReq("other-svc", mkSpan("a", "", "auth")))); n != 1 {
		t.Errorf("same name in another service must be kept, got %d", n)
	}
	if n := len(ConvertTraceRequest(f, mkReq("llm-proxy", mkSpan("a", "", "auth")))); n != 0 {
		t.Errorf("llm-proxy:auth must be dropped, got %d", n)
	}
}

func TestDropCycleTerminates(t *testing.T) {
	f, _ := NewFilter("s:x", "", 0, 0)
	got := ConvertTraceRequest(f, mkReq("s", mkSpan("a", "b", "x"), mkSpan("b", "a", "x"), mkSpan("c", "a", "keep")))
	if len(got) != 1 {
		t.Fatalf("got %d", len(got))
	}
}

func attrsOf(t *testing.T, js string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(js), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestTruncation(t *testing.T) {
	f, _ := NewFilter("", "", 10, 0)
	long := strings.Repeat("a", 25)
	req := mkReq("svc", mkSpan("a", "", "n", strKV("big", long), strKV("small", "hi")))
	req[0].ScopeSpans[0].Spans[0].Events = []*tracepb.Span_Event{{Name: "e", Attributes: []*commonpb.KeyValue{strKV("body", long)}}}

	got := ConvertTraceRequest(f, req)[0]
	a := attrsOf(t, got.AttributesJSON)
	if a["big"] != strings.Repeat("a", 10)+"…[truncated 15 bytes]" {
		t.Errorf("span attr: %v", a["big"])
	}
	if a["small"] != "hi" {
		t.Errorf("small attr changed: %v", a["small"])
	}
	var ev []struct {
		Attributes map[string]any `json:"attributes"`
	}
	if err := json.Unmarshal([]byte(got.EventsJSON), &ev); err != nil {
		t.Fatal(err)
	}
	if ev[0].Attributes["body"] != strings.Repeat("a", 10)+"…[truncated 15 bytes]" {
		t.Errorf("event attr: %v", ev[0].Attributes["body"])
	}

	// 0 disables
	off, _ := NewFilter("", "", 0, 0)
	if a := attrsOf(t, ConvertTraceRequest(off, req)[0].AttributesJSON); a["big"] != long {
		t.Error("0 should disable truncation")
	}
	// nil filter is a no-op
	if a := attrsOf(t, ConvertTraceRequest(nil, req)[0].AttributesJSON); a["big"] != long {
		t.Error("nil filter should not modify")
	}
}

func TestTruncateJSONArrayKeepsNewest(t *testing.T) {
	var elems []string
	for i := 0; i < 20; i++ {
		elems = append(elems, `{"role":"user","content":"msg-`+string(rune('a'+i))+strings.Repeat("x", 90)+`"}`)
	}
	in := "[" + strings.Join(elems, ",") + "]"
	max := 600
	got := truncateString(in, max)
	if len(got) > max {
		t.Fatalf("result %d bytes over cap %d", len(got), max)
	}
	var parsed []map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, got)
	}
	if !strings.Contains(got, `"msg-t`) {
		t.Error("newest message must be kept")
	}
	if strings.Contains(got, `"msg-a`) {
		t.Error("oldest message should be dropped")
	}
	dropped := 20 - (len(parsed) - 1)
	total := 0
	for _, e := range elems[:dropped] {
		total += len(e)
	}
	want := arrayMarker(dropped, total)
	if parsed[0]["role"] != "system" || !strings.HasPrefix(got, "["+want+",") {
		t.Errorf("marker wrong, want %s in %.200s", want, got)
	}
	if !strings.HasSuffix(got, elems[19]+"]") {
		t.Error("last element must be intact and last")
	}
	// Greedy: one more element would not have fit.
	if dropped > 1 {
		if len(arrayMarker(dropped-1, total-len(elems[dropped-1])))+1+len(strings.Join(elems[dropped-1:], ","))+2 <= max {
			t.Error("dropped more than necessary")
		}
	}
}

func TestTruncateJSONArrayHugeLastElement(t *testing.T) {
	huge := `{"role":"user","content":"` + strings.Repeat("y", 5000) + `"}`
	in := `[{"role":"system","content":"sys"},` + huge + `]`
	got := truncateString(in, 500)
	if len(got) > 500 {
		t.Fatalf("over cap: %d", len(got))
	}
	var parsed []any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if len(parsed) != 2 {
		t.Fatalf("want marker + truncated string, got %d", len(parsed))
	}
	want := arrayMarker(1, len(`{"role":"system","content":"sys"}`))
	if !strings.HasPrefix(got, "["+want+",") {
		t.Errorf("marker wrong: %.150s", got)
	}
	s, ok := parsed[1].(string)
	if !ok || !strings.Contains(s, "…[truncated") || !strings.HasPrefix(s, `{"role":"user"`) {
		t.Errorf("last element should be head-truncated string, got %.100v", parsed[1])
	}
}

func TestTruncateNonArrayUnchanged(t *testing.T) {
	for _, in := range []string{
		strings.Repeat("z", 50),
		`{"a":"` + strings.Repeat("z", 50) + `"}`,
		"[not json " + strings.Repeat("z", 50),
	} {
		want := fmt.Sprintf("%s…[truncated %d bytes]", in[:20], len(in)-20)
		if got := truncateString(in, 20); got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
}

func TestTruncateRuneBoundary(t *testing.T) {
	// "é" is 2 bytes; cutting at 3 would split the second é.
	got := truncateString("ééé", 3)
	if !strings.HasPrefix(got, "é…[truncated 4 bytes]") {
		t.Errorf("got %q", got)
	}
}

func TestSlimSpan(t *testing.T) {
	f, _ := NewFilter("", "llm-proxy:raw_gen_ai_request", 0, 2048)
	big := strings.Repeat("x", 5000)
	attrs := []*commonpb.KeyValue{
		strKV("gen_ai.request.model", "gpt-x"),
		strKV("messages", big),
		strKV("response", big),
	}
	got := ConvertTraceRequest(f, mkReq("llm-proxy", mkSpan("a", "", "raw_gen_ai_request", attrs...)))[0]
	a := attrsOf(t, got.AttributesJSON)
	if a["gen_ai.request.model"] != "gpt-x" {
		t.Error("small attribute must be kept")
	}
	if _, ok := a["messages"]; ok {
		t.Error("large attribute must be dropped")
	}
	if got.AIModel != "gpt-x" {
		t.Error("semantic extraction must still see the model")
	}
	// Other span name / other service are untouched.
	for _, req := range [][]*tracepb.ResourceSpans{
		mkReq("llm-proxy", mkSpan("b", "", "other", attrs...)),
		mkReq("elsewhere", mkSpan("c", "", "raw_gen_ai_request", attrs...)),
	} {
		if _, ok := attrsOf(t, ConvertTraceRequest(f, req)[0].AttributesJSON)["messages"]; !ok {
			t.Error("non-matching span must keep large attrs")
		}
	}
}
