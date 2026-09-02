package otlp

import (
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/algonormative/lookout/internal/ai"
	"github.com/algonormative/lookout/internal/store"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

// ConvertTraceRequest converts an OTLP ExportTraceServiceRequest into SpanRecords.
func ConvertTraceRequest(resourceSpans []*tracepb.ResourceSpans) []store.SpanRecord {
	var spans []store.SpanRecord
	now := time.Now()

	for _, rs := range resourceSpans {
		resJSON := marshalAttributes(rs.GetResource())

		for _, ss := range rs.GetScopeSpans() {
			for _, span := range ss.GetSpans() {
				rec := store.SpanRecord{
					TraceID:          hex.EncodeToString(span.GetTraceId()),
					SpanID:           hex.EncodeToString(span.GetSpanId()),
					ParentSpanID:     hex.EncodeToString(span.GetParentSpanId()),
					Name:             span.GetName(),
					Kind:             int(span.GetKind()),
					StartTimeUnixNano: span.GetStartTimeUnixNano(),
					EndTimeUnixNano:   span.GetEndTimeUnixNano(),
					AttributesJSON:   marshalKVList(span.GetAttributes()),
					ResourceJSON:     resJSON,
					StatusCode:       int(span.GetStatus().GetCode()),
					StatusMessage:    span.GetStatus().GetMessage(),
					InsertedAt:       now,
				}

				// Extract AI/agent semantic attributes
				extractSemanticAttrs(&rec, span.GetAttributes())
				// Also check resource attributes for agent info
				if rs.GetResource() != nil {
					extractSemanticAttrs(&rec, rs.GetResource().GetAttributes())
				}

				// Capture span events (prompts, completions, tool messages)
				rec.EventsJSON = marshalEvents(span.GetEvents())

				spans = append(spans, rec)
			}
		}
	}
	return spans
}

func extractSemanticAttrs(rec *store.SpanRecord, attrs []*commonpb.KeyValue) {
	for _, kv := range attrs {
		switch kv.GetKey() {
		case ai.AttrGenAISystem:
			rec.AISystem = kv.GetValue().GetStringValue()
		case ai.AttrGenAIRequestModel:
			if rec.AIModel == "" {
				rec.AIModel = kv.GetValue().GetStringValue()
			}
		case ai.AttrGenAIResponseModel:
			rec.AIModel = kv.GetValue().GetStringValue()
		case ai.AttrGenAIInputTokens:
			rec.AIInputTokens = kv.GetValue().GetIntValue()
		case ai.AttrGenAIOutputTokens:
			rec.AIOutputTokens = kv.GetValue().GetIntValue()
		case ai.AttrAgentName:
			rec.AgentName = kv.GetValue().GetStringValue()
		case ai.AttrAgentTaskID:
			rec.AgentTaskID = kv.GetValue().GetStringValue()
		case ai.AttrAgentSessionID:
			rec.AgentSessionID = kv.GetValue().GetStringValue()
		}
	}
}

func marshalAttributes(res *resourcepb.Resource) string {
	if res == nil {
		return "{}"
	}
	return marshalKVList(res.GetAttributes())
}

func marshalKVList(attrs []*commonpb.KeyValue) string {
	if len(attrs) == 0 {
		return "{}"
	}
	m := make(map[string]any, len(attrs))
	for _, kv := range attrs {
		m[kv.GetKey()] = kvToValue(kv.GetValue())
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func marshalEvents(events []*tracepb.Span_Event) string {
	if len(events) == 0 {
		return "[]"
	}
	type spanEvent struct {
		Name       string         `json:"name"`
		TimeNano   uint64         `json:"time_unix_nano,omitempty"`
		Attributes map[string]any `json:"attributes,omitempty"`
	}
	out := make([]spanEvent, 0, len(events))
	for _, e := range events {
		se := spanEvent{
			Name:     e.GetName(),
			TimeNano: e.GetTimeUnixNano(),
		}
		if attrs := e.GetAttributes(); len(attrs) > 0 {
			se.Attributes = make(map[string]any, len(attrs))
			for _, kv := range attrs {
				se.Attributes[kv.GetKey()] = kvToValue(kv.GetValue())
			}
		}
		out = append(out, se)
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func kvToValue(v *commonpb.AnyValue) any {
	if v == nil {
		return nil
	}
	switch val := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return val.StringValue
	case *commonpb.AnyValue_IntValue:
		return val.IntValue
	case *commonpb.AnyValue_DoubleValue:
		return val.DoubleValue
	case *commonpb.AnyValue_BoolValue:
		return val.BoolValue
	case *commonpb.AnyValue_ArrayValue:
		if val.ArrayValue == nil {
			return nil
		}
		arr := make([]any, len(val.ArrayValue.GetValues()))
		for i, v := range val.ArrayValue.GetValues() {
			arr[i] = kvToValue(v)
		}
		return arr
	case *commonpb.AnyValue_KvlistValue:
		if val.KvlistValue == nil {
			return nil
		}
		m := make(map[string]any, len(val.KvlistValue.GetValues()))
		for _, kv := range val.KvlistValue.GetValues() {
			m[kv.GetKey()] = kvToValue(kv.GetValue())
		}
		return m
	default:
		return nil
	}
}
