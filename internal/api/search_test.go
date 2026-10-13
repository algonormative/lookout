package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/algonormative/lookout/internal/store"
)

func TestHandleSearch(t *testing.T) {
	srv, st := newTestServer(t)
	now := uint64(time.Now().UnixNano())
	st.InsertSpans(context.Background(), []store.SpanRecord{
		{TraceID: "t1", SpanID: "s1", Name: "claude_code.interaction", ServiceName: "nanoclaw-agent",
			AttributesJSON: `{"user_prompt":"please rotate the api keys"}`, StartTimeUnixNano: now, EndTimeUnixNano: now + 1, InsertedAt: time.Now()},
	})
	get := func(url string) (int, store.SearchPage) {
		w := httptest.NewRecorder()
		srv.server.Handler.ServeHTTP(w, httptest.NewRequest("GET", url, nil))
		var page store.SearchPage
		json.Unmarshal(w.Body.Bytes(), &page)
		return w.Code, page
	}
	code, page := get("/v1/search?q=rotate+keys&service=nanoclaw-agent&since=1h")
	if code != 200 || len(page.Traces) != 1 || page.Traces[0].TraceID != "t1" ||
		page.Traces[0].RootSpanName != "claude_code.interaction" || page.Traces[0].Snippet == "" {
		t.Fatalf("code=%d page=%+v", code, page)
	}
	if _, page = get("/v1/search?q=rotate&service=other"); len(page.Traces) != 0 {
		t.Errorf("service filter ignored: %+v", page)
	}
	if code, _ = get("/v1/search"); code != 400 {
		t.Errorf("missing q -> %d, want 400", code)
	}
	if code, _ = get("/v1/search?q=x&limit=abc"); code != 400 {
		t.Errorf("bad limit -> %d, want 400", code)
	}
}
