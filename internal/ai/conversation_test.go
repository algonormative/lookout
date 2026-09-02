package ai

import (
	"encoding/json"
	"testing"

	"github.com/algonormative/lookout/internal/store"
)

func mustEventsJSON(t *testing.T, events []store.SpanEvent) string {
	t.Helper()
	b, err := json.Marshal(events)
	if err != nil {
		t.Fatalf("marshal events: %v", err)
	}
	return string(b)
}

func mustAttrsJSON(t *testing.T, m map[string]any) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal attrs: %v", err)
	}
	return string(b)
}

func TestDecodeConversation_ChatSpan(t *testing.T) {
	span := store.SpanRecord{
		Name:              SpanChatCompletion,
		StartTimeUnixNano: 1000,
		EndTimeUnixNano:   2000,
		AIModel:           "claude-sonnet-4",
		AIInputTokens:     10,
		AIOutputTokens:    20,
		CostUSD:           0.01,
		StatusCode:        1,
		EventsJSON: mustEventsJSON(t, []store.SpanEvent{
			{Name: "gen_ai.content.prompt", Attributes: map[string]any{"gen_ai.prompt": "hi"}},
			{Name: "gen_ai.content.completion", Attributes: map[string]any{"gen_ai.completion": "hello!"}},
		}),
	}

	turns := DecodeConversation([]store.SpanRecord{span})
	if len(turns) != 2 {
		t.Fatalf("want 2 turns, got %d", len(turns))
	}
	if turns[0].Role != "user" || turns[0].Content != "hi" {
		t.Errorf("unexpected user turn: %+v", turns[0])
	}
	if turns[1].Role != "assistant" || turns[1].Content != "hello!" {
		t.Errorf("unexpected assistant turn: %+v", turns[1])
	}
	if turns[0].Model != "claude-sonnet-4" || turns[0].InputTokens != 10 || turns[0].OutputTokens != 20 || turns[0].CostUSD != 0.01 {
		t.Errorf("metadata not propagated: %+v", turns[0])
	}
	if turns[0].StartNano != 1000 || turns[0].EndNano != 2000 {
		t.Errorf("timestamps not propagated: %+v", turns[0])
	}
}

func TestDecodeConversation_ToolSpan(t *testing.T) {
	span := store.SpanRecord{
		Name:              SpanToolCall,
		StartTimeUnixNano: 3000,
		EndTimeUnixNano:   3500,
		StatusCode:        1,
		AttributesJSON:    mustAttrsJSON(t, map[string]any{AttrGenAIToolName: "search"}),
		EventsJSON: mustEventsJSON(t, []store.SpanEvent{
			{Name: "gen_ai.tool.input", Attributes: map[string]any{"gen_ai.tool.input": `{"q":"cats"}`}},
			{Name: "gen_ai.tool.output", Attributes: map[string]any{"gen_ai.tool.output": `["result"]`}},
		}),
	}

	turns := DecodeConversation([]store.SpanRecord{span})
	if len(turns) != 1 {
		t.Fatalf("want 1 turn, got %d", len(turns))
	}
	turn := turns[0]
	if turn.Role != "tool" {
		t.Errorf("want role=tool, got %q", turn.Role)
	}
	if turn.ToolName != "search" {
		t.Errorf("want tool name=search, got %q", turn.ToolName)
	}
	if turn.ToolInput != `{"q":"cats"}` || turn.ToolOutput != `["result"]` {
		t.Errorf("tool io not captured: %+v", turn)
	}
	if turn.StartNano != 3000 || turn.EndNano != 3500 {
		t.Errorf("tool timestamps not captured: %+v", turn)
	}
}

func TestDecodeConversation_EmptyOrMissingEvents(t *testing.T) {
	spans := []store.SpanRecord{
		{
			Name:       SpanChatCompletion,
			EventsJSON: mustEventsJSON(t, []store.SpanEvent{{Name: "gen_ai.content.prompt", Attributes: map[string]any{"gen_ai.prompt": ""}}}),
		},
		{
			Name:       SpanChatCompletion,
			EventsJSON: "",
		},
		{
			Name:           SpanToolCall,
			EventsJSON:     mustEventsJSON(t, []store.SpanEvent{{Name: "gen_ai.tool.input", Attributes: map[string]any{"gen_ai.tool.input": ""}}}),
			AttributesJSON: mustAttrsJSON(t, map[string]any{AttrGenAIToolName: "noop"}),
		},
	}

	turns := DecodeConversation(spans)
	if len(turns) != 0 {
		t.Fatalf("want 0 turns (empty events skipped), got %d: %+v", len(turns), turns)
	}
}

func TestDecodeConversation_OrdersByStartTime(t *testing.T) {
	spans := []store.SpanRecord{
		{
			Name:              SpanChatCompletion,
			StartTimeUnixNano: 3000,
			EventsJSON: mustEventsJSON(t, []store.SpanEvent{
				{Name: "gen_ai.content.prompt", Attributes: map[string]any{"gen_ai.prompt": "third"}},
			}),
		},
		{
			Name:              SpanChatCompletion,
			StartTimeUnixNano: 1000,
			EventsJSON: mustEventsJSON(t, []store.SpanEvent{
				{Name: "gen_ai.content.prompt", Attributes: map[string]any{"gen_ai.prompt": "first"}},
			}),
		},
		{
			Name:              SpanChatCompletion,
			StartTimeUnixNano: 2000,
			EventsJSON: mustEventsJSON(t, []store.SpanEvent{
				{Name: "gen_ai.content.prompt", Attributes: map[string]any{"gen_ai.prompt": "second"}},
			}),
		},
	}

	turns := DecodeConversation(spans)
	if len(turns) != 3 {
		t.Fatalf("want 3 turns, got %d", len(turns))
	}
	wantOrder := []string{"first", "second", "third"}
	for i, want := range wantOrder {
		if turns[i].Content != want {
			t.Errorf("turn %d: want content=%q, got %q", i, want, turns[i].Content)
		}
	}
}

func TestDecodeConversation_IgnoresNonAISpans(t *testing.T) {
	spans := []store.SpanRecord{
		{
			Name:              "http.request",
			StartTimeUnixNano: 100,
			EventsJSON: mustEventsJSON(t, []store.SpanEvent{
				{Name: "gen_ai.content.prompt", Attributes: map[string]any{"gen_ai.prompt": "should be ignored"}},
			}),
		},
	}

	turns := DecodeConversation(spans)
	if len(turns) != 0 {
		t.Fatalf("want 0 turns for non-AI span, got %d", len(turns))
	}
}
