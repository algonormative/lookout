package ai

import (
	"encoding/json"
	"testing"

	"github.com/algonormative/lookout/internal/store"
)

// Real LiteLLM shape: JSON strings in OTel GenAI form, "content" and "text"
// part keys mixed, a lookout truncation marker first, a system prompt, tool
// calls and results.
const llmInput1 = `[{"role":"system","content":"[lookout: 1 earlier items truncated, 62259 bytes]"},` +
	`{"role": "system", "parts": [{"type": "text", "content": "You are Lemon. SYSTEM PROMPT"}]},` +
	`{"role": "user", "parts": [{"type": "text", "content": "make a task for low-stock reminders"}]}]`

const llmOutput1 = `[{"role": "assistant", "parts": [` +
	`{"type": "text", "content": "Creating it now."},` +
	`{"type": "tool_call", "id": "call_1", "name": "br_create", "arguments": {"title": "low-stock"}}], "finish_reason": "tool_calls"}]`

const llmInput2 = `[{"role":"system","content":"[lookout: 3 earlier items truncated, 70000 bytes]"},` +
	`{"role": "user", "parts": [{"type": "text", "content": "make a task for low-stock reminders"}]},` +
	`{"role": "assistant", "parts": [{"type": "text", "content": "Creating it now."},` +
	`{"type": "tool_call", "id": "call_1", "name": "br_create", "arguments": {"title": "low-stock"}}]},` +
	`{"role": "tool", "parts": [{"type": "tool_result", "id": "call_1", "content": [{"type": "text", "text": "created vault-3gujw"}]}]}]`

const llmOutput2 = `[{"role": "assistant", "parts": [{"type": "text", "text": "Task **vault-3gujw** created."}], "finish_reason": "stop"}]`

func llmSpan(id string, start uint64, in, out string) store.SpanRecord {
	return store.SpanRecord{
		SpanID: id, Name: "Received Proxy Server Request", ServiceName: "llm-proxy",
		StartTimeUnixNano: start, EndTimeUnixNano: start + 10,
		AIModel: "qwen3.5:35b", AIInputTokens: 38088, AIOutputTokens: 67, CostUSD: 0.002, StatusCode: 1,
		AttributesJSON: func() string {
			b, _ := json.Marshal(map[string]any{
				"gen_ai.input.messages": in, "gen_ai.output.messages": out, "gen_ai.system": "",
			})
			return string(b)
		}(),
	}
}

func TestDecodeConversation_LiteLLMMessageAttributes(t *testing.T) {
	spans := []store.SpanRecord{
		llmSpan("b", 2000, llmInput2, llmOutput2),
		llmSpan("a", 1000, llmInput1, llmOutput1),
		// A child span repeating the root's content must not duplicate turns.
		llmSpan("a-child", 1001, llmInput1, llmOutput1),
	}
	turns := DecodeConversation(spans)

	type want struct{ role, content, tool, in, out string }
	exp := []want{
		{role: "system", content: "[lookout: 1 earlier items truncated, 62259 bytes]"},
		{role: "user", content: "make a task for low-stock reminders"},
		{role: "assistant", content: "Creating it now."},
		{role: "assistant", tool: "br_create", in: `{"title": "low-stock"}`},
		{role: "tool", tool: "br_create", out: "created vault-3gujw"},
		{role: "assistant", content: "Task **vault-3gujw** created."},
	}
	if len(turns) != len(exp) {
		for _, tr := range turns {
			t.Logf("%+v", tr)
		}
		t.Fatalf("got %d turns, want %d", len(turns), len(exp))
	}
	for i, w := range exp {
		g := turns[i]
		if g.Role != w.role || g.Content != w.content || g.ToolName != w.tool || g.ToolInput != w.in || g.ToolOutput != w.out {
			t.Errorf("turn %d = %+v, want %+v", i, g, w)
		}
	}
	// Output turns carry model/tokens/cost; the system prompt never appears.
	if turns[2].Model != "qwen3.5:35b" || turns[2].OutputTokens != 67 || turns[2].CostUSD != 0.002 {
		t.Errorf("output turn metadata: %+v", turns[2])
	}
	for _, tr := range turns {
		if tr.Content == "You are Lemon. SYSTEM PROMPT" {
			t.Error("system prompt leaked into conversation")
		}
	}
}

func TestDecodeConversation_HeadTruncatedAttributeKeptAsText(t *testing.T) {
	sp := store.SpanRecord{Name: "Received Proxy Server Request", StartTimeUnixNano: 1,
		AttributesJSON: mustAttrsJSON(t, map[string]any{
			"gen_ai.input.messages": `plain prompt text…[truncated 900 bytes]`,
		})}
	turns := DecodeConversation([]store.SpanRecord{sp})
	if len(turns) != 1 || turns[0].Role != "user" || turns[0].Content != `plain prompt text…[truncated 900 bytes]` {
		t.Errorf("turns = %+v", turns)
	}
}

func TestDecodeConversation_EventsTakePrecedence(t *testing.T) {
	// A chat span with events and message attributes yields only the event turns.
	sp := store.SpanRecord{Name: SpanChatCompletion, StartTimeUnixNano: 1,
		EventsJSON:     mustEventsJSON(t, []store.SpanEvent{{Name: "gen_ai.content.prompt", Attributes: map[string]any{"gen_ai.prompt": "ev"}}}),
		AttributesJSON: mustAttrsJSON(t, map[string]any{"gen_ai.input.messages": llmInput1})}
	turns := DecodeConversation([]store.SpanRecord{sp})
	if len(turns) != 1 || turns[0].Content != "ev" {
		t.Errorf("turns = %+v", turns)
	}
}
