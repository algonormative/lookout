package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/algonormative/lookout/internal/ai"
	"github.com/algonormative/lookout/internal/store"
	"github.com/algonormative/lookout/internal/web"
)

// Server is the analytics HTTP API server.
type Server struct {
	store  store.Store
	ring   *store.Ring
	server *http.Server
	addr   string

	// WebSocket broadcast
	wsMu      sync.RWMutex
	wsClients map[chan []byte]struct{}
}

// NewServer creates a new analytics API server.
func NewServer(addr string, s store.Store, ring *store.Ring) *Server {
	srv := &Server{
		store:     s,
		ring:      ring,
		addr:      addr,
		wsClients: make(map[chan []byte]struct{}),
	}

	mux := http.NewServeMux()

	// Traces
	mux.HandleFunc("GET /v1/traces", srv.handleTraces)
	mux.HandleFunc("GET /v1/traces/{traceID}", srv.handleTraceByID)
	mux.HandleFunc("GET /v1/traces/{traceID}/conversation", srv.handleTraceConversation)
	mux.HandleFunc("GET /v1/recent", srv.handleRecent)

	// Sessions
	mux.HandleFunc("GET /v1/sessions", srv.handleSessions)
	mux.HandleFunc("GET /v1/sessions/{sessionID}", srv.handleSessionByID)

	// Stats
	mux.HandleFunc("GET /v1/stats", srv.handleStats)
	mux.HandleFunc("GET /v1/stats/by-model", srv.handleStatsByModel)
	mux.HandleFunc("GET /v1/stats/by-agent", srv.handleStatsByAgent)
	mux.HandleFunc("GET /v1/stats/cost", srv.handleCostReport)

	// Metrics
	mux.HandleFunc("GET /v1/metrics/names", srv.handleMetricNames)
	mux.HandleFunc("GET /v1/metrics/{name}", srv.handleMetricByName)

	// Anomalies
	mux.HandleFunc("GET /v1/anomalies", srv.handleAnomalies)

	// Live + Health
	mux.HandleFunc("GET /v1/live", srv.handleWebSocket)
	mux.HandleFunc("GET /health", srv.handleHealth)

	// Web UI
	webHandler := web.NewHandler(s, ring)
	webHandler.Register(mux)

	srv.server = &http.Server{
		Addr:    addr,
		Handler: mux,
	}
	return srv
}

// Start begins listening.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("api listen: %w", err)
	}
	log.Printf("Analytics API listening on %s", s.addr)
	go s.server.Serve(ln)
	return nil
}

// Stop gracefully shuts down the server.
func (s *Server) Stop(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

// BroadcastSpans sends enriched spans to all WebSocket clients.
func (s *Server) BroadcastSpans(spans []store.SpanRecord) {
	data, err := json.Marshal(spans)
	if err != nil {
		return
	}
	s.wsMu.RLock()
	defer s.wsMu.RUnlock()
	for ch := range s.wsClients {
		select {
		case ch <- data:
		default:
			// drop if client is slow
		}
	}
}

// --- Handlers ---

func (s *Server) handleTraces(w http.ResponseWriter, r *http.Request) {
	filter := parseSpanFilter(r)
	spans, err := s.store.QuerySpans(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, spans)
}

func (s *Server) handleTraceByID(w http.ResponseWriter, r *http.Request) {
	traceID := r.PathValue("traceID")
	spans, err := s.store.GetTrace(r.Context(), traceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, spans)
}

func (s *Server) handleTraceConversation(w http.ResponseWriter, r *http.Request) {
	traceID := r.PathValue("traceID")
	spans, err := s.store.GetTrace(r.Context(), traceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if len(spans) == 0 {
		writeError(w, http.StatusNotFound, fmt.Errorf("trace %s not found", traceID))
		return
	}
	writeJSON(w, ai.DecodeConversation(spans))
}

func (s *Server) handleRecent(w http.ResponseWriter, r *http.Request) {
	limit := intParam(r, "limit", 50)
	spans := s.ring.Recent(limit)
	writeJSON(w, spans)
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	filter := store.SessionFilter{
		Agent: r.URL.Query().Get("agent"),
		Since: parseTimeParam(r, "since"),
		Limit: intParam(r, "limit", 20),
	}
	sessions, err := s.store.GetSessions(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, sessions)
}

func (s *Server) handleSessionByID(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("sessionID")
	spans, err := s.store.GetSession(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, spans)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	filter := parseSpanFilter(r)
	stats, err := s.store.GetStats(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, stats)
}

func (s *Server) handleStatsByModel(w http.ResponseWriter, r *http.Request) {
	filter := parseSpanFilter(r)
	stats, err := s.store.GetStatsByModel(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, stats)
}

func (s *Server) handleStatsByAgent(w http.ResponseWriter, r *http.Request) {
	filter := parseSpanFilter(r)
	stats, err := s.store.GetStatsByAgent(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, stats)
}

func (s *Server) handleCostReport(w http.ResponseWriter, r *http.Request) {
	filter := parseSpanFilter(r)
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" {
		bucket = "hour"
	}
	groupBy := r.URL.Query().Get("group_by")
	report, err := s.store.GetCostReport(r.Context(), filter, bucket, groupBy)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, report)
}

func (s *Server) handleMetricNames(w http.ResponseWriter, r *http.Request) {
	names, err := s.store.ListMetricNames(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, names)
}

func (s *Server) handleMetricByName(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	filter := store.MetricFilter{
		Name:  name,
		Since: parseTimeParam(r, "since"),
	}

	bucketStr := r.URL.Query().Get("bucket")
	switch bucketStr {
	case "1h":
		filter.BucketWidth = 3600
	case "1d":
		filter.BucketWidth = 86400
	default:
		filter.BucketWidth = 60
	}

	if labels := r.URL.Query().Get("labels"); labels != "" {
		filter.Labels = parseLabels(labels)
	}

	rollups, err := s.store.QueryMetricRollups(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, rollups)
}

func (s *Server) handleAnomalies(w http.ResponseWriter, r *http.Request) {
	filter := parseSpanFilter(r)
	spans, err := s.store.GetAnomalies(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, spans)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

// --- Helpers ---

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if v == nil {
		w.Write([]byte("[]"))
		return
	}
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func intParam(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func floatParam(r *http.Request, key string) float64 {
	v := r.URL.Query().Get(key)
	if v == "" {
		return 0
	}
	f, _ := strconv.ParseFloat(v, 64)
	return f
}

func parseTimeParam(r *http.Request, key string) time.Time {
	v := r.URL.Query().Get(key)
	if v == "" {
		return time.Time{}
	}
	return parseDurationOrTime(v)
}

func parseDurationOrTime(s string) time.Time {
	// Try relative duration: "1h", "24h", "7d"
	if len(s) > 1 {
		numStr := s[:len(s)-1]
		unit := s[len(s)-1]
		if n, err := strconv.Atoi(numStr); err == nil {
			switch unit {
			case 'h':
				return time.Now().Add(-time.Duration(n) * time.Hour)
			case 'd':
				return time.Now().Add(-time.Duration(n) * 24 * time.Hour)
			case 'm':
				return time.Now().Add(-time.Duration(n) * time.Minute)
			case 's':
				return time.Now().Add(-time.Duration(n) * time.Second)
			}
		}
	}
	// Try absolute time
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func parseSpanFilter(r *http.Request) store.SpanFilter {
	return store.SpanFilter{
		TraceID:    r.URL.Query().Get("trace_id"),
		Agent:      r.URL.Query().Get("agent"),
		Model:      r.URL.Query().Get("model"),
		Since:      parseTimeParam(r, "since"),
		Until:      parseTimeParam(r, "until"),
		DurationGt: floatParam(r, "duration_gt"),
		Status:     r.URL.Query().Get("status"),
		CostGt:     floatParam(r, "cost_gt"),
		SortBy:     r.URL.Query().Get("sort_by"),
		Limit:      intParam(r, "limit", 20),
	}
}

func parseLabels(s string) map[string]string {
	labels := make(map[string]string)
	for _, pair := range strings.Split(s, ",") {
		parts := strings.SplitN(pair, ":", 2)
		if len(parts) == 2 {
			labels[parts[0]] = parts[1]
		}
	}
	return labels
}
