package web

import (
	"net/http"
	"strconv"
	"time"

	"github.com/algonormative/lookout/internal/store"
)

// Handler serves the web UI pages.
type Handler struct {
	store store.Store
	ring  *store.Ring
}

// NewHandler creates a new web UI handler.
func NewHandler(s store.Store, ring *store.Ring) *Handler {
	return &Handler{store: s, ring: ring}
}

// Register mounts all UI routes on the given mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /traces", h.handleTraces)
	mux.HandleFunc("GET /traces/{traceID}", h.handleTraceDetail)
	mux.HandleFunc("GET /sessions", h.handleSessions)
	mux.HandleFunc("GET /sessions/{sessionID}", h.handleSessionDetail)
	mux.HandleFunc("GET /metrics", h.handleMetrics)
	mux.HandleFunc("GET /anomalies", h.handleAnomalies)

	// Partials for htmx polling
	mux.HandleFunc("GET /partials/stats", h.handlePartialStats)
	mux.HandleFunc("GET /partials/recent", h.handlePartialRecent)
	mux.HandleFunc("GET /partials/anomalies", h.handlePartialAnomalies)

	// Dashboard at root (registered last — catch-all)
	mux.HandleFunc("GET /{$}", h.handleDashboard)
}

func (h *Handler) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	stats, err := h.store.GetStats(ctx, store.SpanFilter{})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	modelStats, _ := h.store.GetStatsByModel(ctx, store.SpanFilter{})
	agentStats, _ := h.store.GetStatsByAgent(ctx, store.SpanFilter{})
	recent := h.ring.Recent(20)

	data := DashboardData{
		Stats:       stats,
		ModelStats:  modelStats,
		AgentStats:  agentStats,
		RecentSpans: recent,
	}
	dashboardPage(data).Render(ctx, w)
}

func (h *Handler) handleTraces(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	filter := store.SpanFilter{
		Agent:  r.URL.Query().Get("agent"),
		Model:  r.URL.Query().Get("model"),
		Status: r.URL.Query().Get("status"),
		SortBy: r.URL.Query().Get("sort_by"),
		Limit:  queryInt(r, "limit", 20),
		Since:  queryTime(r, "since"),
	}
	if filter.SortBy == "" {
		filter.SortBy = "time"
	}

	spans, err := h.store.QuerySpans(ctx, filter)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	tracesPage(spans).Render(ctx, w)
}

func (h *Handler) handleTraceDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	traceID := r.PathValue("traceID")

	spans, err := h.store.GetTrace(ctx, traceID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	traceDetailPage(traceID, spans).Render(ctx, w)
}

func (h *Handler) handleSessions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	filter := store.SessionFilter{
		Agent: r.URL.Query().Get("agent"),
		Limit: queryInt(r, "limit", 20),
	}

	sessions, err := h.store.GetSessions(ctx, filter)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	sessionsPage(sessions).Render(ctx, w)
}

func (h *Handler) handleSessionDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sessionID := r.PathValue("sessionID")

	spans, err := h.store.GetSession(ctx, sessionID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	sessionDetailPage(sessionID, spans).Render(ctx, w)
}

func (h *Handler) handleMetrics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	names, err := h.store.ListMetricNames(ctx)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	data := MetricsPageData{
		Names:  names,
		Bucket: r.URL.Query().Get("bucket"),
	}
	if data.Bucket == "" {
		data.Bucket = "1m"
	}

	data.Current = r.URL.Query().Get("name")
	if data.Current == "" && len(names) > 0 {
		data.Current = names[0]
	}

	if data.Current != "" {
		filter := store.MetricFilter{
			Name: data.Current,
		}
		switch data.Bucket {
		case "1h":
			filter.BucketWidth = 3600
		case "1d":
			filter.BucketWidth = 86400
		default:
			filter.BucketWidth = 60
		}
		data.Rollups, _ = h.store.QueryMetricRollups(ctx, filter)
	}

	metricsPage(data).Render(ctx, w)
}

func (h *Handler) handleAnomalies(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	filter := store.SpanFilter{Limit: 50}

	spans, err := h.store.GetAnomalies(ctx, filter)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	anomaliesPage(spans).Render(ctx, w)
}

// --- Partials (htmx polling) ---

func (h *Handler) handlePartialStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	stats, err := h.store.GetStats(ctx, store.SpanFilter{})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	statsCards(stats).Render(ctx, w)
}

func (h *Handler) handlePartialRecent(w http.ResponseWriter, r *http.Request) {
	recent := h.ring.Recent(20)
	spanTable(recent, true).Render(r.Context(), w)
}

func (h *Handler) handlePartialAnomalies(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	spans, err := h.store.GetAnomalies(ctx, store.SpanFilter{Limit: 50})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	spanTable(spans, true).Render(ctx, w)
}

// --- Helpers ---

func queryInt(r *http.Request, key string, def int) int {
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

func queryTime(r *http.Request, key string) time.Time {
	v := r.URL.Query().Get(key)
	if v == "" {
		return time.Time{}
	}
	// Parse relative duration: "15m", "1h", "24h", "7d"
	if len(v) > 1 {
		numStr := v[:len(v)-1]
		unit := v[len(v)-1]
		if n, err := strconv.Atoi(numStr); err == nil {
			switch unit {
			case 'm':
				return time.Now().Add(-time.Duration(n) * time.Minute)
			case 'h':
				return time.Now().Add(-time.Duration(n) * time.Hour)
			case 'd':
				return time.Now().Add(-time.Duration(n) * 24 * time.Hour)
			}
		}
	}
	t, _ := time.Parse(time.RFC3339, v)
	return t
}

