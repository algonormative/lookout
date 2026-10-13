package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/algonormative/lookout/internal/store"
)

const maxScoreBody = 4 << 20

func (s *Server) registerScoreRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/scores", s.handlePostScores)
	mux.HandleFunc("GET /v1/scores", s.handleGetScores)
	mux.HandleFunc("GET /v1/trace-list", s.handleTraceList)
	mux.HandleFunc("GET /v1/search", s.handleSearch)

	// Langfuse-shaped aliases for tools that already speak that API.
	mux.HandleFunc("POST /api/public/scores", s.handleLangfusePostScore)
	mux.HandleFunc("GET /api/public/scores", s.handleLangfuseGetScores)
}

// writeStoreError maps store.ErrInvalidInput to 400 and everything else to 500.
func writeStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrInvalidInput) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeError(w, http.StatusInternalServerError, err)
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxScoreBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("read body: %w", err))
		return nil, false
	}
	return bytes.TrimSpace(body), true
}

// parseSince is the strict form of parseTimeParam: "15m"/"1h"/"7d" or RFC3339,
// and anything else is an error (the lenient helper silently drops it, which
// for a polling judge means "scan everything").
func parseSince(r *http.Request, key string) (time.Time, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return time.Time{}, nil
	}
	t := parseDurationOrTime(v)
	if t.IsZero() {
		return t, fmt.Errorf("%w: bad %s %q (want 15m, 1h, 7d or RFC3339)", store.ErrInvalidInput, key, v)
	}
	return t, nil
}

func strictInt(r *http.Request, key string, def int) (int, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%w: bad %s %q", store.ErrInvalidInput, key, v)
	}
	return n, nil
}

func parseOrder(r *http.Request) (asc bool, err error) {
	switch r.URL.Query().Get("order") {
	case "", "desc":
		return false, nil
	case "asc":
		return true, nil
	}
	return false, fmt.Errorf("%w: order must be asc or desc", store.ErrInvalidInput)
}

// --- native endpoints ---

func (s *Server) handlePostScores(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var scores []store.Score
	var err error
	if bytes.HasPrefix(body, []byte("[")) {
		err = json.Unmarshal(body, &scores)
	} else {
		var one store.Score
		if err = json.Unmarshal(body, &one); err == nil {
			scores = []store.Score{one}
		}
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
		return
	}
	ids, err := s.store.UpsertScores(r.Context(), scores)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ids": ids})
}

func scoreFilterFromQuery(r *http.Request) (store.ScoreFilter, error) {
	q := r.URL.Query()
	f := store.ScoreFilter{
		TraceID:   q.Get("trace_id"),
		SpanID:    q.Get("span_id"),
		SessionID: q.Get("session_id"),
		Name:      q.Get("name"),
		Source:    q.Get("source"),
		DataType:  q.Get("data_type"),
		Cursor:    q.Get("cursor"),
	}
	var err error
	if f.Since, err = parseSince(r, "since"); err != nil {
		return f, err
	}
	if f.Limit, err = strictInt(r, "limit", 0); err != nil {
		return f, err
	}
	f.Asc, err = parseOrder(r)
	return f, err
}

func (s *Server) handleGetScores(w http.ResponseWriter, r *http.Request) {
	f, err := scoreFilterFromQuery(r)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	scores, next, err := s.store.QueryScores(r.Context(), f)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if scores == nil {
		scores = []store.Score{}
	}
	resp := map[string]any{"scores": scores}
	if next != "" {
		resp["next_cursor"] = next
	}
	writeJSON(w, resp)
}

// --- Langfuse-shaped aliases ---

type langfuseScoreIn struct {
	ID            string          `json:"id"`
	TraceID       string          `json:"traceId"`
	ObservationID string          `json:"observationId"`
	SessionID     string          `json:"sessionId"`
	Name          string          `json:"name"`
	Value         json.RawMessage `json:"value"`
	DataType      string          `json:"dataType"`
	Comment       string          `json:"comment"`
	ConfigID      string          `json:"configId"`
	Metadata      json.RawMessage `json:"metadata"`
}

// toScore maps a Langfuse score body onto the native model. value may be a
// number, a boolean or a string; with no dataType it is inferred from the
// value's JSON type. source comes from metadata.source, default "api";
// configId has no column and is kept in metadata.configId.
func (in langfuseScoreIn) toScore() (store.Score, error) {
	sc := store.Score{
		ID: in.ID, TraceID: in.TraceID, SpanID: in.ObservationID, SessionID: in.SessionID,
		Name: in.Name, Comment: in.Comment, Source: "api",
		DataType: strings.ToUpper(in.DataType),
	}

	var meta map[string]any
	if m := bytes.TrimSpace(in.Metadata); len(m) > 0 && string(m) != "null" {
		if err := json.Unmarshal(m, &meta); err != nil {
			return sc, fmt.Errorf("%w: metadata must be a JSON object", store.ErrInvalidInput)
		}
	}
	if src, ok := meta["source"].(string); ok && src != "" {
		sc.Source = src
	}
	if in.ConfigID != "" {
		if meta == nil {
			meta = map[string]any{}
		}
		meta["configId"] = in.ConfigID
	}
	if meta != nil {
		b, _ := json.Marshal(meta)
		sc.Metadata = b
	}

	raw := bytes.TrimSpace(in.Value)
	if len(raw) == 0 || string(raw) == "null" {
		return sc, fmt.Errorf("%w: value is required", store.ErrInvalidInput)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return sc, fmt.Errorf("%w: bad value", store.ErrInvalidInput)
	}
	switch x := v.(type) {
	case float64:
		sc.ValueNum = &x
		if sc.DataType == "" {
			sc.DataType = store.ScoreNumeric
		}
	case bool:
		n := 0.0
		if x {
			n = 1
		}
		sc.ValueNum = &n
		if sc.DataType == "" {
			sc.DataType = store.ScoreBoolean
		}
	case string:
		sc.ValueStr = x
		if sc.DataType == "" {
			sc.DataType = store.ScoreCategorical
		}
	default:
		return sc, fmt.Errorf("%w: value must be a number, boolean or string", store.ErrInvalidInput)
	}
	return sc, nil
}

func (s *Server) handleLangfusePostScore(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var in langfuseScoreIn
	if err := json.Unmarshal(body, &in); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
		return
	}
	sc, err := in.toScore()
	if err != nil {
		writeStoreError(w, err)
		return
	}
	ids, err := s.store.UpsertScores(r.Context(), []store.Score{sc})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, map[string]string{"id": ids[0]})
}

type langfuseScoreOut struct {
	ID            string          `json:"id"`
	TraceID       string          `json:"traceId"`
	ObservationID *string         `json:"observationId"`
	SessionID     *string         `json:"sessionId"`
	Name          string          `json:"name"`
	Value         *float64        `json:"value"`
	StringValue   *string         `json:"stringValue"`
	DataType      string          `json:"dataType"`
	Source        string          `json:"source"`
	Comment       *string         `json:"comment"`
	ConfigID      *string         `json:"configId"`
	Metadata      json.RawMessage `json:"metadata"`
	CreatedAt     string          `json:"createdAt"`
	Timestamp     string          `json:"timestamp"`
}

func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func toLangfuse(sc store.Score) langfuseScoreOut {
	out := langfuseScoreOut{
		ID: sc.ID, TraceID: sc.TraceID, ObservationID: optStr(sc.SpanID),
		SessionID: optStr(sc.SessionID), Name: sc.Name, Value: sc.ValueNum,
		StringValue: optStr(sc.ValueStr), DataType: sc.DataType, Source: sc.Source,
		Comment: optStr(sc.Comment), Metadata: sc.Metadata,
	}
	if sc.DataType == store.ScoreBoolean && sc.ValueNum != nil {
		label := "False"
		if *sc.ValueNum == 1 {
			label = "True"
		}
		out.StringValue = &label
	}
	if len(sc.Metadata) > 0 {
		var m map[string]any
		if json.Unmarshal(sc.Metadata, &m) == nil {
			if c, ok := m["configId"].(string); ok {
				out.ConfigID = &c
			}
		}
	} else {
		out.Metadata = json.RawMessage("{}")
	}
	ts := time.Unix(0, sc.CreatedAt).UTC().Format(time.RFC3339Nano)
	out.CreatedAt, out.Timestamp = ts, ts
	return out
}

// handleLangfuseGetScores serves page-numbered results newest first:
// {data: [...], meta: {page, limit, totalItems, totalPages}}.
func (s *Server) handleLangfuseGetScores(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, err := strictInt(r, "page", 1)
	if err == nil && page < 1 {
		err = fmt.Errorf("%w: page must be >= 1", store.ErrInvalidInput)
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	limit, err := strictInt(r, "limit", 50)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if limit == 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}
	f := store.ScoreFilter{
		TraceID:   q.Get("traceId"),
		SpanID:    q.Get("observationId"),
		SessionID: q.Get("sessionId"),
		Name:      q.Get("name"),
		Source:    q.Get("source"),
		DataType:  q.Get("dataType"),
		Limit:     limit,
		Offset:    (page - 1) * limit,
	}
	total, err := s.store.CountScores(r.Context(), f)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	scores, _, err := s.store.QueryScores(r.Context(), f)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	data := make([]langfuseScoreOut, 0, len(scores))
	for _, sc := range scores {
		data = append(data, toLangfuse(sc))
	}
	totalPages := int((total + int64(limit) - 1) / int64(limit))
	writeJSON(w, map[string]any{
		"data": data,
		"meta": map[string]any{"page": page, "limit": limit, "totalItems": total, "totalPages": totalPages},
	})
}
