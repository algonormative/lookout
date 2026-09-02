package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/algonormative/lookout/internal/ai"
	"github.com/algonormative/lookout/internal/store"
)

func newTestServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.NewSQLiteStore(filepath.Join(dir, "api_test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	ring := store.NewRing(100)
	return NewServer(":0", s, ring), s
}

func TestHandleTraceConversation(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()

	chatEvents, _ := json.Marshal([]store.SpanEvent{
		{Name: "gen_ai.content.prompt", Attributes: map[string]any{"gen_ai.prompt": "ping"}},
		{Name: "gen_ai.content.completion", Attributes: map[string]any{"gen_ai.completion": "pong"}},
	})
	toolEvents, _ := json.Marshal([]store.SpanEvent{
		{Name: "gen_ai.tool.input", Attributes: map[string]any{"gen_ai.tool.input": `{"k":"v"}`}},
		{Name: "gen_ai.tool.output", Attributes: map[string]any{"gen_ai.tool.output": "ok"}},
	})
	toolAttrs, _ := json.Marshal(map[string]any{ai.AttrGenAIToolName: "lookup"})

	spans := []store.SpanRecord{
		{
			TraceID:           "trace-conv-1",
			SpanID:            "span-chat",
			Name:              ai.SpanChatCompletion,
			StartTimeUnixNano: 1000,
			EndTimeUnixNano:   2000,
			AIModel:           "claude-sonnet-4",
			AIInputTokens:     5,
			AIOutputTokens:    7,
			CostUSD:           0.001,
			StatusCode:        1,
			EventsJSON:        string(chatEvents),
			InsertedAt:        time.Now(),
		},
		{
			TraceID:           "trace-conv-1",
			SpanID:            "span-tool",
			ParentSpanID:      "span-chat",
			Name:              ai.SpanToolCall,
			StartTimeUnixNano: 3000,
			EndTimeUnixNano:   3500,
			StatusCode:        1,
			AttributesJSON:    string(toolAttrs),
			EventsJSON:        string(toolEvents),
			InsertedAt:        time.Now(),
		},
	}
	if err := st.InsertSpans(ctx, spans); err != nil {
		t.Fatalf("insert spans: %v", err)
	}

	req := httptest.NewRequest("GET", "/v1/traces/trace-conv-1/conversation", nil)
	rec := httptest.NewRecorder()
	srv.server.Handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	body, _ := io.ReadAll(rec.Body)
	var turns []ai.ConversationTurn
	if err := json.Unmarshal(body, &turns); err != nil {
		t.Fatalf("decode body: %v (body=%s)", err, body)
	}
	if len(turns) != 3 {
		t.Fatalf("want 3 turns, got %d (%+v)", len(turns), turns)
	}
	if turns[0].Role != "user" || turns[0].Content != "ping" {
		t.Errorf("turn[0] unexpected: %+v", turns[0])
	}
	if turns[1].Role != "assistant" || turns[1].Content != "pong" {
		t.Errorf("turn[1] unexpected: %+v", turns[1])
	}
	if turns[2].Role != "tool" || turns[2].ToolName != "lookup" || turns[2].ToolOutput != "ok" {
		t.Errorf("turn[2] unexpected: %+v", turns[2])
	}
}

func TestHandleTraceConversation_NotFound(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest("GET", "/v1/traces/no-such-trace/conversation", nil)
	rec := httptest.NewRecorder()
	srv.server.Handler.ServeHTTP(rec, req)

	if rec.Code != 404 {
		t.Fatalf("want 404, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] == "" {
		t.Errorf("want error body, got %v", body)
	}
}
