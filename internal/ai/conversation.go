package ai

import (
	"encoding/json"
	"sort"

	"github.com/algonormative/lookout/internal/store"
)

// ConversationTurn is a single logical turn (user/assistant message or tool
// call) decoded from one or more OTEL GenAI spans. API and MCP consumers can
// render this shape without needing to know the underlying gen_ai.* event
// vocabulary.
type ConversationTurn struct {
	Role         string `json:"role"`
	Content      string `json:"content,omitempty"`
	ToolName     string `json:"tool_name,omitempty"`
	ToolInput    string `json:"tool_input,omitempty"`
	ToolOutput   string `json:"tool_output,omitempty"`
	Model        string `json:"model,omitempty"`
	InputTokens  int64  `json:"input_tokens,omitempty"`
	OutputTokens int64  `json:"output_tokens,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
	StartNano    uint64 `json:"start_time_unix_nano,omitempty"`
	EndNano      uint64 `json:"end_time_unix_nano,omitempty"`
	StatusCode   int    `json:"status_code,omitempty"`
}

// DecodeConversation walks the provided spans and emits a structured
// conversation of ConversationTurn values. Only spans whose Name matches
// SpanChatCompletion or SpanToolCall contribute turns. Turns are returned
// sorted by span start time.
func DecodeConversation(spans []store.SpanRecord) []ConversationTurn {
	// Work on a start-time-ordered copy so callers don't depend on input order.
	ordered := make([]store.SpanRecord, len(spans))
	copy(ordered, spans)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].StartTimeUnixNano < ordered[j].StartTimeUnixNano
	})

	var turns []ConversationTurn
	for _, s := range ordered {
		switch s.Name {
		case SpanChatCompletion:
			turns = append(turns, decodeChatTurns(s)...)
		case SpanToolCall:
			if t, ok := decodeToolTurn(s); ok {
				turns = append(turns, t)
			}
		}
	}
	return turns
}

func decodeChatTurns(s store.SpanRecord) []ConversationTurn {
	var out []ConversationTurn
	for _, ev := range s.ParsedEvents() {
		switch ev.Name {
		case "gen_ai.content.prompt":
			if c := eventStringAttr(ev, "gen_ai.prompt"); c != "" {
				out = append(out, newChatTurn(s, "user", c))
			}
		case "gen_ai.content.completion":
			if c := eventStringAttr(ev, "gen_ai.completion"); c != "" {
				out = append(out, newChatTurn(s, "assistant", c))
			}
		}
	}
	return out
}

func decodeToolTurn(s store.SpanRecord) (ConversationTurn, bool) {
	var input, output string
	for _, ev := range s.ParsedEvents() {
		switch ev.Name {
		case "gen_ai.tool.input":
			if v := eventStringAttr(ev, "gen_ai.tool.input"); v != "" {
				input = v
			}
		case "gen_ai.tool.output":
			if v := eventStringAttr(ev, "gen_ai.tool.output"); v != "" {
				output = v
			}
		}
	}
	if input == "" && output == "" {
		return ConversationTurn{}, false
	}
	return ConversationTurn{
		Role:       "tool",
		ToolName:   toolNameFromAttrs(s.AttributesJSON),
		ToolInput:  input,
		ToolOutput: output,
		StartNano:  s.StartTimeUnixNano,
		EndNano:    s.EndTimeUnixNano,
		StatusCode: s.StatusCode,
	}, true
}

func newChatTurn(s store.SpanRecord, role, content string) ConversationTurn {
	return ConversationTurn{
		Role:         role,
		Content:      content,
		Model:        s.AIModel,
		InputTokens:  s.AIInputTokens,
		OutputTokens: s.AIOutputTokens,
		CostUSD:      s.CostUSD,
		StartNano:    s.StartTimeUnixNano,
		EndNano:      s.EndTimeUnixNano,
		StatusCode:   s.StatusCode,
	}
}

func eventStringAttr(ev store.SpanEvent, key string) string {
	v, ok := ev.Attributes[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

func toolNameFromAttrs(attrsJSON string) string {
	if attrsJSON == "" {
		return ""
	}
	var attrs map[string]any
	if err := json.Unmarshal([]byte(attrsJSON), &attrs); err != nil {
		return ""
	}
	if v, ok := attrs[AttrGenAIToolName]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
