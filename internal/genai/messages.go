// Package genai decodes the message JSON that GenAI-instrumented producers put
// in span attributes (gen_ai.input.messages / gen_ai.output.messages).
//
// The OTel GenAI shape is
//
//	[{"role":"user","parts":[{"type":"text","content":"hi"}]},
//	 {"role":"assistant","parts":[{"type":"tool_call","id":"c1","name":"f","arguments":{...}}]},
//	 {"role":"tool","parts":[{"type":"tool_result","id":"c1","content":[{"type":"text","text":"ok"}]}]}]
//
// but producers vary: text parts carry "content" or "text"; tool results carry
// "content" (string or text parts) or "response"; older producers use
// OpenAI-style messages with a plain "content" string. Lookout's ingest cap may
// also have prepended a "[lookout: N earlier items truncated, B bytes]" marker
// message. This package is shared by the conversation decoder and the
// full-text indexer so both read messages the same way.
package genai

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Part kinds.
const (
	PartText       = "text"
	PartToolCall   = "tool_call"
	PartToolResult = "tool_result"
)

// MarkerPrefix starts the content of the message lookout inserts when it drops
// earlier elements of an over-cap message array.
const MarkerPrefix = "[lookout:"

// Part is one decoded piece of a message.
type Part struct {
	Kind string // PartText, PartToolCall or PartToolResult
	Text string // text content, or the tool result text
	Name string // tool name (tool calls)
	ID   string // tool call id (calls and results)
	Args string // tool call arguments as JSON text
}

// Message is a decoded chat message.
type Message struct {
	Role  string
	Parts []Part
	// Marker is true for lookout's truncation marker (its text is in Parts[0]).
	Marker bool
}

// Text returns the message's text and tool-result text joined by newlines.
func (m Message) Text() string {
	var sb strings.Builder
	for _, p := range m.Parts {
		if p.Kind == PartToolCall || p.Text == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(p.Text)
	}
	return sb.String()
}

// HasUserText reports whether the message is a user message with a text part
// (as opposed to one carrying only tool results).
func (m Message) HasUserText() bool {
	if m.Role != "user" {
		return false
	}
	for _, p := range m.Parts {
		if p.Kind == PartText && p.Text != "" {
			return true
		}
	}
	return false
}

// ParseMessages decodes a gen_ai.*.messages attribute value. ok is false when
// the value is not a JSON array of messages (for example a head-truncated
// string); callers then treat it as opaque text.
func ParseMessages(s string) (msgs []Message, ok bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") {
		return nil, false
	}
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, false
	}
	for _, r := range raw {
		if m, ok := parseMessage(r); ok {
			msgs = append(msgs, m)
		}
	}
	return msgs, true
}

func parseMessage(raw json.RawMessage) (Message, bool) {
	var m struct {
		Role       string            `json:"role"`
		Parts      []json.RawMessage `json:"parts"`
		Content    json.RawMessage   `json:"content"`
		ToolCallID string            `json:"tool_call_id"`
		ToolCalls  []json.RawMessage `json:"tool_calls"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return Message{}, false
	}
	msg := Message{Role: m.Role}
	for _, p := range m.Parts {
		if part, ok := parsePart(p); ok {
			msg.Parts = append(msg.Parts, part)
		}
	}
	if len(m.Parts) == 0 && len(m.Content) > 0 {
		text := textOf(m.Content)
		if text != "" {
			kind := PartText
			if m.Role == "tool" || m.Role == "function" {
				kind = PartToolResult
			}
			msg.Parts = append(msg.Parts, Part{Kind: kind, Text: text, ID: m.ToolCallID})
			if m.Role == "system" && strings.HasPrefix(text, MarkerPrefix) {
				msg.Marker = true
			}
		}
	}
	for _, tc := range m.ToolCalls { // OpenAI style
		var c struct {
			ID       string `json:"id"`
			Function struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"function"`
		}
		if json.Unmarshal(tc, &c) == nil && c.Function.Name != "" {
			msg.Parts = append(msg.Parts, Part{Kind: PartToolCall, ID: c.ID, Name: c.Function.Name, Args: argsText(c.Function.Arguments)})
		}
	}
	if len(msg.Parts) == 0 {
		return Message{}, false
	}
	return msg, true
}

func parsePart(raw json.RawMessage) (Part, bool) {
	var p struct {
		Type       string          `json:"type"`
		Content    json.RawMessage `json:"content"`
		Text       json.RawMessage `json:"text"`
		Response   json.RawMessage `json:"response"`
		ID         string          `json:"id"`
		ToolUseID  string          `json:"tool_use_id"`
		ToolCallID string          `json:"tool_call_id"`
		Name       string          `json:"name"`
		Arguments  json.RawMessage `json:"arguments"`
		Input      json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return Part{}, false
	}
	id := firstNonEmpty(p.ID, p.ToolUseID, p.ToolCallID)
	switch p.Type {
	case "tool_call", "tool_use", "function_call":
		args := p.Arguments
		if len(args) == 0 {
			args = p.Input
		}
		return Part{Kind: PartToolCall, ID: id, Name: p.Name, Args: argsText(args)}, true
	case "tool_result", "tool_call_response", "function_call_output":
		body := p.Content
		if len(body) == 0 {
			body = p.Response
		}
		return Part{Kind: PartToolResult, ID: id, Text: textOf(body)}, true
	default: // "text" and anything else carrying text
		body := p.Content
		if len(body) == 0 {
			body = p.Text
		}
		text := textOf(body)
		if text == "" {
			return Part{}, false
		}
		return Part{Kind: PartText, Text: text}, true
	}
}

// textOf renders a JSON value that holds text: a string, an array of text
// parts, or (as a last resort) the raw JSON.
func textOf(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	switch raw[0] {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	case '[':
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) == nil {
			var parts []string
			for _, it := range items {
				if t := textOfItem(it); t != "" {
					parts = append(parts, t)
				}
			}
			return strings.Join(parts, "\n")
		}
	}
	return string(raw)
}

func textOfItem(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '"' {
		return textOf(raw)
	}
	var it struct {
		Text    json.RawMessage `json:"text"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &it) != nil {
		return ""
	}
	if len(it.Text) > 0 {
		return textOf(it.Text)
	}
	return textOf(it.Content)
}

func argsText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' { // arguments sent as a JSON string
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	return string(raw)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
