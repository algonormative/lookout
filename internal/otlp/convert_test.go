package otlp

import (
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func intKV(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}

func TestConvertServiceAndSessionMapping(t *testing.T) {
	cases := []struct {
		name  string
		attrs []*commonpb.KeyValue
		res   []*commonpb.KeyValue
		want  string
	}{
		{"agent.session_id", []*commonpb.KeyValue{strKV("agent.session_id", "agent-1")}, nil, "agent-1"},
		{"session.id (Claude Code)", []*commonpb.KeyValue{strKV("session.id", "claude-1")}, nil, "claude-1"},
		{"langfuse.session.id", []*commonpb.KeyValue{strKV("langfuse.session.id", "lf-1")}, nil, "lf-1"},
		{"agent beats session.id", []*commonpb.KeyValue{strKV("session.id", "claude-1"), strKV("agent.session_id", "agent-1")}, nil, "agent-1"},
		{"session.id beats langfuse", []*commonpb.KeyValue{strKV("langfuse.session.id", "lf-1"), strKV("session.id", "claude-1")}, nil, "claude-1"},
		{"resource session.id", nil, []*commonpb.KeyValue{strKV("session.id", "res-1")}, "res-1"},
		{"span beats resource", []*commonpb.KeyValue{strKV("session.id", "span-1")}, []*commonpb.KeyValue{strKV("session.id", "res-1")}, "span-1"},
		{"int session.id", []*commonpb.KeyValue{intKV("session.id", 42)}, nil, "42"},
		{"none", []*commonpb.KeyValue{strKV("other", "x")}, nil, ""},
	}
	for _, tc := range cases {
		span := mkSpan("spanspan", "", "claude_code.interaction", tc.attrs...)
		req := []*tracepb.ResourceSpans{{
			Resource:   &resourcepb.Resource{Attributes: append([]*commonpb.KeyValue{strKV("service.name", "nanoclaw-agent")}, tc.res...)},
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{span}}},
		}}
		recs := ConvertTraceRequest(nil, req)
		if len(recs) != 1 {
			t.Fatalf("%s: %d records", tc.name, len(recs))
		}
		if recs[0].AgentSessionID != tc.want {
			t.Errorf("%s: session=%q want %q", tc.name, recs[0].AgentSessionID, tc.want)
		}
		if recs[0].ServiceName != "nanoclaw-agent" {
			t.Errorf("%s: service_name=%q", tc.name, recs[0].ServiceName)
		}
	}
}
