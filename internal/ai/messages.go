package ai

import (
	"encoding/json"

	"github.com/algonormative/lookout/internal/genai"
	"github.com/algonormative/lookout/internal/store"
)

// Attribute keys carrying whole chat messages (OTel GenAI, as emitted by
// LiteLLM). See package genai for the accepted shapes.
const (
	AttrGenAIInputMessages  = "gen_ai.input.messages"
	AttrGenAIOutputMessages = "gen_ai.output.messages"
)

// messageState dedups turns across the spans of one trace: every LLM request
// repeats the whole history in its input, so a message is emitted once, from
// the first span that contains it.
type messageState struct {
	seen        map[string]bool
	toolByID    map[string]string
	markerAdded bool
}

func newMessageState() *messageState {
	return &messageState{seen: map[string]bool{}, toolByID: map[string]string{}}
}

// decode returns the new turns carried by a span's message attributes.
//
// System prompts are skipped (large and constant); lookout's truncation marker
// is kept, once per trace, so the reader knows earlier history is missing.
// Output turns carry the span's model, tokens and cost.
func (ms *messageState) decode(s store.SpanRecord) []ConversationTurn {
	if s.AttributesJSON == "" || s.AttributesJSON == "{}" {
		return nil
	}
	var attrs map[string]any
	if json.Unmarshal([]byte(s.AttributesJSON), &attrs) != nil {
		return nil
	}
	in, _ := attrs[AttrGenAIInputMessages].(string)
	out, _ := attrs[AttrGenAIOutputMessages].(string)
	if in == "" && out == "" {
		return nil
	}
	var turns []ConversationTurn
	turns = ms.appendTurns(turns, s, in, "user", false)
	turns = ms.appendTurns(turns, s, out, "assistant", true)
	return turns
}

func (ms *messageState) appendTurns(turns []ConversationTurn, s store.SpanRecord, raw, defaultRole string, isOutput bool) []ConversationTurn {
	if raw == "" {
		return turns
	}
	base := func(role string) ConversationTurn {
		t := ConversationTurn{Role: role, StartNano: s.StartTimeUnixNano, EndNano: s.EndTimeUnixNano, StatusCode: s.StatusCode}
		if isOutput {
			t.Model, t.InputTokens, t.OutputTokens, t.CostUSD = s.AIModel, s.AIInputTokens, s.AIOutputTokens, s.CostUSD
		}
		return t
	}
	msgs, ok := genai.ParseMessages(raw)
	if !ok { // not a message array (e.g. head-truncated): keep the text as one turn
		if key := defaultRole + "\x00" + raw; !ms.seen[key] {
			ms.seen[key] = true
			t := base(defaultRole)
			t.Content = raw
			turns = append(turns, t)
		}
		return turns
	}
	for _, m := range msgs {
		role := m.Role
		if role == "" {
			role = defaultRole
		}
		if m.Marker {
			if !ms.markerAdded {
				ms.markerAdded = true
				t := base("system")
				t.Content = m.Parts[0].Text
				turns = append(turns, t)
			}
			continue
		}
		if role == "system" {
			continue
		}
		for _, p := range m.Parts {
			var t ConversationTurn
			var key string
			switch p.Kind {
			case genai.PartText:
				t = base(role)
				t.Content = p.Text
				key = role + "\x00t\x00" + p.Text
			case genai.PartToolCall:
				ms.toolByID[p.ID] = p.Name
				t = base("assistant")
				t.ToolName, t.ToolInput = p.Name, p.Args
				key = "call\x00" + p.ID + "\x00" + p.Name + "\x00" + p.Args
			case genai.PartToolResult:
				t = base("tool")
				t.ToolName, t.ToolOutput = ms.toolByID[p.ID], p.Text
				key = "result\x00" + p.ID + "\x00" + p.Text
			}
			if ms.seen[key] {
				continue
			}
			ms.seen[key] = true
			turns = append(turns, t)
		}
	}
	return turns
}
