package otlp

import (
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"

	"github.com/algonormative/lookout/internal/ai"
	"github.com/algonormative/lookout/internal/store"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

// ConvertTraceRequest converts an OTLP ExportTraceServiceRequest into SpanRecords.
//
// The filter (may be nil) is applied here so the HTTP and gRPC receivers behave
// identically: denylisted spans are dropped, large attributes are slimmed or
// truncated. Children of a dropped span are re-parented to the nearest
// non-dropped ancestor whose id is reachable through dropped spans in the same
// request (so a dropped chain A -> dropped B -> C yields C.parent = A). If the
// dropped span arrived in a different batch, the child keeps its original
// (now dangling) parent id, since that information is not available here.
func ConvertTraceRequest(f *Filter, resourceSpans []*tracepb.ResourceSpans) []store.SpanRecord {
	var spans []store.SpanRecord
	now := time.Now()
	dropped := map[string]string{} // dropped span id -> its parent id

	for _, rs := range resourceSpans {
		resJSON := marshalAttributes(rs.GetResource())
		service := serviceName(rs.GetResource())

		for _, ss := range rs.GetScopeSpans() {
			for _, span := range ss.GetSpans() {
				if f.shouldDrop(service, span.GetName()) {
					dropped[hex.EncodeToString(span.GetSpanId())] = hex.EncodeToString(span.GetParentSpanId())
					continue
				}
				slim := f.isSlim(service, span.GetName())
				rec := store.SpanRecord{
					TraceID:          hex.EncodeToString(span.GetTraceId()),
					SpanID:           hex.EncodeToString(span.GetSpanId()),
					ParentSpanID:     hex.EncodeToString(span.GetParentSpanId()),
					Name:             span.GetName(),
					Kind:             int(span.GetKind()),
					StartTimeUnixNano: span.GetStartTimeUnixNano(),
					EndTimeUnixNano:   span.GetEndTimeUnixNano(),
					AttributesJSON:   marshalKVListFiltered(f, span.GetAttributes(), slim),
					ResourceJSON:     resJSON,
					StatusCode:       int(span.GetStatus().GetCode()),
					StatusMessage:    span.GetStatus().GetMessage(),
					ServiceName:      service,
					InsertedAt:       now,
				}

				// Extract AI/agent semantic attributes
				extractSemanticAttrs(&rec, span.GetAttributes())
				// Also check resource attributes for agent info
				if rs.GetResource() != nil {
					extractSemanticAttrs(&rec, rs.GetResource().GetAttributes())
				}
				if rec.AgentSessionID == "" {
					rec.AgentSessionID = fallbackSessionID(span.GetAttributes(), rs.GetResource().GetAttributes())
				}
				extractIdentity(&rec, span.GetAttributes(), rs.GetResource().GetAttributes())
				extractCost(&rec, span.GetAttributes())

				// Capture span events (prompts, completions, tool messages)
				rec.EventsJSON = marshalEvents(f, span.GetEvents(), slim)

				spans = append(spans, rec)
			}
		}
	}
	if len(dropped) > 0 {
		for i := range spans {
			spans[i].ParentSpanID = resolveParent(spans[i].ParentSpanID, dropped)
		}
	}
	return spans
}

// resolveParent walks up through dropped ancestors to the first non-dropped
// (or unknown) parent id. The hop limit guards against malformed cyclic input.
func resolveParent(parent string, dropped map[string]string) string {
	for hops := 0; hops <= len(dropped); hops++ {
		up, ok := dropped[parent]
		if !ok {
			return parent
		}
		parent = up
	}
	return parent
}

// fallbackSessionID resolves the session when agent.session_id is absent:
// session.id (Claude Code's native telemetry), then langfuse.session.id. For
// each key the span's attributes win over the resource's. The backfill in
// store/backfill.go mirrors this order.
func fallbackSessionID(spanAttrs, resAttrs []*commonpb.KeyValue) string {
	for _, key := range []string{"session.id", "langfuse.session.id"} {
		for _, attrs := range [][]*commonpb.KeyValue{spanAttrs, resAttrs} {
			for _, kv := range attrs {
				if kv.GetKey() == key {
					if v := kvString(kv.GetValue()); v != "" {
						return v
					}
				}
			}
		}
	}
	return ""
}

// lookupAttr returns the first non-empty text value for keys, trying each key
// in order and, per key, span attributes before resource attributes.
func lookupAttr(keys []string, spanAttrs, resAttrs []*commonpb.KeyValue) string {
	for _, key := range keys {
		for _, attrs := range [][]*commonpb.KeyValue{spanAttrs, resAttrs} {
			for _, kv := range attrs {
				if kv.GetKey() == key {
					if v := kvString(kv.GetValue()); v != "" {
						return v
					}
				}
			}
		}
	}
	return ""
}

// extractIdentity maps Langfuse-style attributes: user (langfuse.user.id, else
// user.id), trace name (langfuse.trace.name) and observation type
// (langfuse.observation.type; inferred when absent: generation when a model is
// set, tool for claude_code.tool spans, else span). The backfill in
// store/backfill.go mirrors these rules.
func extractIdentity(rec *store.SpanRecord, spanAttrs, resAttrs []*commonpb.KeyValue) {
	rec.UserID = lookupAttr([]string{"langfuse.user.id", "user.id"}, spanAttrs, resAttrs)
	rec.TraceName = lookupAttr([]string{"langfuse.trace.name"}, spanAttrs, resAttrs)
	rec.ObsType = lookupAttr([]string{"langfuse.observation.type"}, spanAttrs, nil)
	if rec.ObsType == "" {
		switch {
		case rec.AIModel != "":
			rec.ObsType = "generation"
		case rec.Name == "claude_code.tool":
			rec.ObsType = "tool"
		default:
			rec.ObsType = "span"
		}
	}
}

// extractCost reads a producer-reported USD cost (LiteLLM:
// gen_ai.cost.total_cost; others: gen_ai.usage.cost). Zero or absent leaves
// CostUSD unset so enrichment can fall back to the pricing table.
func extractCost(rec *store.SpanRecord, attrs []*commonpb.KeyValue) {
	for _, key := range []string{ai.AttrGenAICostTotal, ai.AttrGenAIUsageCost} {
		for _, kv := range attrs {
			if kv.GetKey() != key {
				continue
			}
			var c float64
			switch x := kv.GetValue().GetValue().(type) {
			case *commonpb.AnyValue_DoubleValue:
				c = x.DoubleValue
			case *commonpb.AnyValue_IntValue:
				c = float64(x.IntValue)
			case *commonpb.AnyValue_StringValue:
				c, _ = strconv.ParseFloat(x.StringValue, 64)
			}
			if c > 0 {
				rec.CostUSD = c
				return
			}
		}
	}
}

// kvString renders scalar attribute values as text (session ids are sometimes
// sent as ints).
func kvString(v *commonpb.AnyValue) string {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	}
	return ""
}

func serviceName(res *resourcepb.Resource) string {
	for _, kv := range res.GetAttributes() {
		if kv.GetKey() == "service.name" {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
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
	return marshalKVListFiltered(nil, attrs, false)
}

func marshalKVListFiltered(f *Filter, attrs []*commonpb.KeyValue, slim bool) string {
	if len(attrs) == 0 {
		return "{}"
	}
	b, _ := json.Marshal(f.attrMap(attrs, slim))
	return string(b)
}

func marshalEvents(f *Filter, events []*tracepb.Span_Event, slim bool) string {
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
			se.Attributes = f.attrMap(attrs, slim)
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
