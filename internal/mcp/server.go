package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/algonormative/lookout/internal/ai"
	"github.com/algonormative/lookout/internal/store"
	gomcp "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// NewServer creates the lookout MCP server with all tools and resources.
func NewServer(s store.Store) *server.MCPServer {
	srv := server.NewMCPServer(
		"lookout",
		"0.1.0",
		server.WithToolCapabilities(true),
		server.WithResourceCapabilities(true, false),
	)

	h := &handlers{store: s}

	// -- Query tools --
	srv.AddTool(gomcp.NewTool("query_traces",
		gomcp.WithDescription("Query trace spans with filters. Returns matching spans sorted by the specified field."),
		gomcp.WithString("trace_id", gomcp.Description("Filter by trace ID")),
		gomcp.WithString("agent", gomcp.Description("Filter by agent name")),
		gomcp.WithString("model", gomcp.Description("Filter by AI model")),
		gomcp.WithString("since", gomcp.Description("Time range start (e.g. 1h, 24h, 7d, or RFC3339)")),
		gomcp.WithString("until", gomcp.Description("Time range end")),
		gomcp.WithNumber("duration_gt", gomcp.Description("Minimum duration in seconds")),
		gomcp.WithString("status", gomcp.Description("Filter by status: ok, error, unset")),
		gomcp.WithNumber("cost_gt", gomcp.Description("Minimum cost in USD")),
		gomcp.WithString("sort_by", gomcp.Description("Sort field: time, duration, cost, tokens")),
		gomcp.WithNumber("limit", gomcp.Description("Max results (default 20)")),
	), h.queryTraces)

	srv.AddTool(gomcp.NewTool("query_sessions",
		gomcp.WithDescription("List agent sessions with aggregated stats (cost, tokens, errors)."),
		gomcp.WithString("agent", gomcp.Description("Filter by agent name")),
		gomcp.WithString("since", gomcp.Description("Time range start")),
		gomcp.WithNumber("limit", gomcp.Description("Max results (default 20)")),
	), h.querySessions)

	srv.AddTool(gomcp.NewTool("get_session",
		gomcp.WithDescription("Get all spans for a specific agent session."),
		gomcp.WithString("session_id", gomcp.Description("Session ID"), gomcp.Required()),
	), h.getSession)

	srv.AddTool(gomcp.NewTool("get_stats",
		gomcp.WithDescription("Get aggregate statistics: total spans, traces, sessions, tokens, cost, errors. Optionally group by model or agent."),
		gomcp.WithString("since", gomcp.Description("Time range start")),
		gomcp.WithString("group_by", gomcp.Description("Group by: model or agent")),
	), h.getStats)

	srv.AddTool(gomcp.NewTool("get_anomalies",
		gomcp.WithDescription("Get spans flagged with anomalies (high cost, slow response, excessive tokens)."),
		gomcp.WithString("since", gomcp.Description("Time range start")),
		gomcp.WithString("agent", gomcp.Description("Filter by agent")),
		gomcp.WithNumber("limit", gomcp.Description("Max results (default 20)")),
	), h.getAnomalies)

	srv.AddTool(gomcp.NewTool("query_metrics",
		gomcp.WithDescription("Query OTLP metric rollups by name with time bucketing."),
		gomcp.WithString("name", gomcp.Description("Metric name"), gomcp.Required()),
		gomcp.WithString("since", gomcp.Description("Time range start (default 1h)")),
		gomcp.WithString("bucket", gomcp.Description("Bucket width: 1m, 1h, 1d (default 1m)")),
	), h.queryMetrics)

	srv.AddTool(gomcp.NewTool("get_trace_conversation",
		gomcp.WithDescription("Return the structured conversation (chat messages, tool calls) for a specific trace ID. Uses the same decoder as the web UI."),
		gomcp.WithString("trace_id", gomcp.Description("Trace ID"), gomcp.Required()),
	), h.getTraceConversation)

	// -- Scores and trace query (judge loop) --
	srv.AddTool(gomcp.NewTool("query_trace_list",
		gomcp.WithDescription("List traces (one row per trace: root span, services, start, duration, span count, models, tokens, error flag, session, scores). Cursor-paged, newest first by default. Judge loop: filter with missing_score to find traces a judge has not scored yet."),
		gomcp.WithString("service", gomcp.Description("Trace has a span from this service.name")),
		gomcp.WithString("root_name", gomcp.Description("Root span name, e.g. claude_code.interaction")),
		gomcp.WithArray("attr", gomcp.Description("key=value span attribute filters (all must match)"), gomcp.WithStringItems()),
		gomcp.WithString("since", gomcp.Description("Trace start >= (e.g. 1h, 7d, or RFC3339); strongly recommended")),
		gomcp.WithString("until", gomcp.Description("Trace start <=")),
		gomcp.WithArray("missing_score", gomcp.Description("name:source; trace has no such score"), gomcp.WithStringItems()),
		gomcp.WithArray("has_score", gomcp.Description("name:source; trace has such a score"), gomcp.WithStringItems()),
		gomcp.WithString("order", gomcp.Description("asc or desc (default desc = newest first)")),
		gomcp.WithString("cursor", gomcp.Description("next_cursor from a previous page")),
		gomcp.WithNumber("limit", gomcp.Description("Max results (default 50, max 500)")),
	), h.queryTraceList)

	srv.AddTool(gomcp.NewTool("search_traces",
		gomcp.WithDescription("Full-text search over trace content (span names, errors, Claude Code user prompts, LLM input/output message text). All terms must match; \"quoted phrases\" and trailing * prefixes work. Returns matching traces newest first with a snippet. Cursor-paged."),
		gomcp.WithString("q", gomcp.Description("Search terms"), gomcp.Required()),
		gomcp.WithString("service", gomcp.Description("Trace has a span from this service.name")),
		gomcp.WithString("since", gomcp.Description("Trace start >= (e.g. 1h, 7d, or RFC3339)")),
		gomcp.WithString("until", gomcp.Description("Trace start <=")),
		gomcp.WithString("cursor", gomcp.Description("next_cursor from a previous page")),
		gomcp.WithNumber("limit", gomcp.Description("Max results (default 50, max 500)")),
	), h.searchTraces)

	srv.AddTool(gomcp.NewTool("score_trace",
		gomcp.WithDescription("Post a judge verdict (score) on a trace or span. Upserts on id, so retries are safe."),
		gomcp.WithString("trace_id", gomcp.Description("Trace ID"), gomcp.Required()),
		gomcp.WithString("name", gomcp.Description("Score name, e.g. promise_only"), gomcp.Required()),
		gomcp.WithString("data_type", gomcp.Description("NUMERIC, BOOLEAN (value_num 0/1) or CATEGORICAL (value_str)"), gomcp.Required()),
		gomcp.WithString("source", gomcp.Description("Who judged: jev, minicheck, qwen3.5-4b, eval, human..."), gomcp.Required()),
		gomcp.WithNumber("value_num", gomcp.Description("Numeric value / 0-1 boolean / probability")),
		gomcp.WithString("value_str", gomcp.Description("Category label for CATEGORICAL")),
		gomcp.WithString("span_id", gomcp.Description("Span ID; omit for a trace-level score")),
		gomcp.WithString("session_id", gomcp.Description("Defaults to the trace's session")),
		gomcp.WithString("id", gomcp.Description("Client-supplied id for idempotency")),
		gomcp.WithString("judge_version", gomcp.Description("Judge/prompt version")),
		gomcp.WithString("comment", gomcp.Description("Free-text rationale")),
		gomcp.WithObject("metadata", gomcp.Description("Arbitrary JSON object")),
	), h.scoreTrace)

	srv.AddTool(gomcp.NewTool("get_scores",
		gomcp.WithDescription("Query scores posted by judges. Newest first unless order=asc; cursor-paged."),
		gomcp.WithString("trace_id", gomcp.Description("Filter by trace")),
		gomcp.WithString("span_id", gomcp.Description("Filter by span")),
		gomcp.WithString("name", gomcp.Description("Filter by score name")),
		gomcp.WithString("source", gomcp.Description("Filter by judge source")),
		gomcp.WithString("since", gomcp.Description("created_at >= (e.g. 1h, 7d, or RFC3339)")),
		gomcp.WithString("order", gomcp.Description("asc or desc (default desc)")),
		gomcp.WithString("cursor", gomcp.Description("next_cursor from a previous page")),
		gomcp.WithNumber("limit", gomcp.Description("Max results (default 50, max 1000)")),
	), h.getScores)

	// -- Analytical tools --
	srv.AddTool(gomcp.NewTool("analyze_session",
		gomcp.WithDescription("Deep analysis of a session: timeline, cost breakdown by model, error summary, and performance stats."),
		gomcp.WithString("session_id", gomcp.Description("Session ID to analyze"), gomcp.Required()),
	), h.analyzeSession)

	srv.AddTool(gomcp.NewTool("compare_models",
		gomcp.WithDescription("Compare AI model performance: cost per token, throughput, error rates, and usage volume."),
		gomcp.WithString("since", gomcp.Description("Time range start (default 24h)")),
	), h.compareModels)

	srv.AddTool(gomcp.NewTool("suggest_optimizations",
		gomcp.WithDescription("Analyze recent usage and suggest cost/performance optimizations based on model choice, error patterns, and anomalies."),
		gomcp.WithString("since", gomcp.Description("Time range to analyze (default 24h)")),
	), h.suggestOptimizations)

	// -- Resources --
	srv.AddResource(gomcp.NewResource(
		"lookout://stats",
		"Aggregate Statistics",
		gomcp.WithResourceDescription("Current aggregate stats: spans, traces, sessions, tokens, cost, errors"),
		gomcp.WithMIMEType("application/json"),
	), h.resourceStats)

	srv.AddResource(gomcp.NewResource(
		"lookout://sessions/recent",
		"Recent Sessions",
		gomcp.WithResourceDescription("Most recent 10 agent sessions with cost and token summaries"),
		gomcp.WithMIMEType("application/json"),
	), h.resourceRecentSessions)

	return srv
}

type handlers struct {
	store store.Store
}

// -- Query tool handlers --

func (h *handlers) queryTraces(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	filter := store.SpanFilter{
		TraceID:    req.GetString("trace_id", ""),
		Agent:      req.GetString("agent", ""),
		Model:      req.GetString("model", ""),
		Since:      parseDuration(req.GetString("since", "")),
		Until:      parseDuration(req.GetString("until", "")),
		DurationGt: req.GetFloat("duration_gt", 0),
		Status:     req.GetString("status", ""),
		CostGt:     req.GetFloat("cost_gt", 0),
		SortBy:     req.GetString("sort_by", "time"),
		Limit:      req.GetInt("limit", 20),
	}

	spans, err := h.store.QuerySpans(ctx, filter)
	if err != nil {
		return gomcp.NewToolResultError(err.Error()), nil
	}
	return jsonResult(spans)
}

func (h *handlers) querySessions(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	filter := store.SessionFilter{
		Agent: req.GetString("agent", ""),
		Since: parseDuration(req.GetString("since", "")),
		Limit: req.GetInt("limit", 20),
	}

	sessions, err := h.store.GetSessions(ctx, filter)
	if err != nil {
		return gomcp.NewToolResultError(err.Error()), nil
	}
	return jsonResult(sessions)
}

func (h *handlers) getSession(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	sessionID := req.GetString("session_id", "")
	if sessionID == "" {
		return gomcp.NewToolResultError("session_id is required"), nil
	}

	spans, err := h.store.GetSession(ctx, sessionID)
	if err != nil {
		return gomcp.NewToolResultError(err.Error()), nil
	}
	return jsonResult(spans)
}

func (h *handlers) getStats(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	filter := store.SpanFilter{
		Since: parseDuration(req.GetString("since", "")),
	}

	groupBy := req.GetString("group_by", "")
	switch groupBy {
	case "model":
		stats, err := h.store.GetStatsByModel(ctx, filter)
		if err != nil {
			return gomcp.NewToolResultError(err.Error()), nil
		}
		return jsonResult(stats)
	case "agent":
		stats, err := h.store.GetStatsByAgent(ctx, filter)
		if err != nil {
			return gomcp.NewToolResultError(err.Error()), nil
		}
		return jsonResult(stats)
	default:
		stats, err := h.store.GetStats(ctx, filter)
		if err != nil {
			return gomcp.NewToolResultError(err.Error()), nil
		}
		return jsonResult(stats)
	}
}

func (h *handlers) getAnomalies(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	filter := store.SpanFilter{
		Since: parseDuration(req.GetString("since", "")),
		Agent: req.GetString("agent", ""),
		Limit: req.GetInt("limit", 20),
	}

	spans, err := h.store.GetAnomalies(ctx, filter)
	if err != nil {
		return gomcp.NewToolResultError(err.Error()), nil
	}
	return jsonResult(spans)
}

func (h *handlers) queryMetrics(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	name := req.GetString("name", "")
	if name == "" {
		return gomcp.NewToolResultError("name is required"), nil
	}

	since := req.GetString("since", "1h")
	filter := store.MetricFilter{
		Name:  name,
		Since: parseDuration(since),
	}

	switch req.GetString("bucket", "1m") {
	case "1h":
		filter.BucketWidth = 3600
	case "1d":
		filter.BucketWidth = 86400
	default:
		filter.BucketWidth = 60
	}

	rollups, err := h.store.QueryMetricRollups(ctx, filter)
	if err != nil {
		return gomcp.NewToolResultError(err.Error()), nil
	}
	return jsonResult(rollups)
}

func (h *handlers) getTraceConversation(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	traceID := req.GetString("trace_id", "")
	if traceID == "" {
		return gomcp.NewToolResultError("trace_id is required"), nil
	}
	spans, err := h.store.GetTrace(ctx, traceID)
	if err != nil {
		return gomcp.NewToolResultError(err.Error()), nil
	}
	if len(spans) == 0 {
		return gomcp.NewToolResultError(fmt.Sprintf("trace %s not found", traceID)), nil
	}
	return jsonResult(ai.DecodeConversation(spans))
}

// -- Scores / trace list handlers --

func (h *handlers) queryTraceList(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	f := store.TraceListFilter{
		Service:  req.GetString("service", ""),
		RootName: req.GetString("root_name", ""),
		Since:    parseDuration(req.GetString("since", "")),
		Until:    parseDuration(req.GetString("until", "")),
		Asc:      req.GetString("order", "") == "asc",
		Cursor:   req.GetString("cursor", ""),
		Limit:    req.GetInt("limit", 0),
	}
	for _, v := range req.GetStringSlice("attr", nil) {
		a, err := store.ParseAttr(v)
		if err != nil {
			return gomcp.NewToolResultError(err.Error()), nil
		}
		f.Attrs = append(f.Attrs, a)
	}
	for _, v := range req.GetStringSlice("missing_score", nil) {
		r, err := store.ParseScoreRef(v)
		if err != nil {
			return gomcp.NewToolResultError(err.Error()), nil
		}
		f.Missing = append(f.Missing, r)
	}
	for _, v := range req.GetStringSlice("has_score", nil) {
		r, err := store.ParseScoreRef(v)
		if err != nil {
			return gomcp.NewToolResultError(err.Error()), nil
		}
		f.Has = append(f.Has, r)
	}
	page, err := h.store.QueryTraceList(ctx, f)
	if err != nil {
		return gomcp.NewToolResultError(err.Error()), nil
	}
	return jsonResult(page)
}

func (h *handlers) searchTraces(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	page, err := h.store.SearchTraces(ctx, store.SearchFilter{
		Q:       req.GetString("q", ""),
		Service: req.GetString("service", ""),
		Since:   parseDuration(req.GetString("since", "")),
		Until:   parseDuration(req.GetString("until", "")),
		Cursor:  req.GetString("cursor", ""),
		Limit:   req.GetInt("limit", 0),
	})
	if err != nil {
		return gomcp.NewToolResultError(err.Error()), nil
	}
	return jsonResult(page)
}

func (h *handlers) scoreTrace(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	sc := store.Score{
		ID:           req.GetString("id", ""),
		TraceID:      req.GetString("trace_id", ""),
		SpanID:       req.GetString("span_id", ""),
		SessionID:    req.GetString("session_id", ""),
		Name:         req.GetString("name", ""),
		DataType:     req.GetString("data_type", ""),
		ValueStr:     req.GetString("value_str", ""),
		Source:       req.GetString("source", ""),
		JudgeVersion: req.GetString("judge_version", ""),
		Comment:      req.GetString("comment", ""),
	}
	args := req.GetArguments()
	if _, ok := args["value_num"]; ok {
		v := req.GetFloat("value_num", 0)
		sc.ValueNum = &v
	}
	if m, ok := args["metadata"]; ok && m != nil {
		b, err := json.Marshal(m)
		if err != nil {
			return gomcp.NewToolResultError("metadata: " + err.Error()), nil
		}
		sc.Metadata = b
	}
	scores := []store.Score{sc}
	ids, err := h.store.UpsertScores(ctx, scores)
	if err != nil {
		return gomcp.NewToolResultError(err.Error()), nil
	}
	return jsonResult(map[string]any{"ids": ids, "score": scores[0]})
}

func (h *handlers) getScores(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	scores, next, err := h.store.QueryScores(ctx, store.ScoreFilter{
		TraceID: req.GetString("trace_id", ""),
		SpanID:  req.GetString("span_id", ""),
		Name:    req.GetString("name", ""),
		Source:  req.GetString("source", ""),
		Since:   parseDuration(req.GetString("since", "")),
		Asc:     req.GetString("order", "") == "asc",
		Cursor:  req.GetString("cursor", ""),
		Limit:   req.GetInt("limit", 0),
	})
	if err != nil {
		return gomcp.NewToolResultError(err.Error()), nil
	}
	if scores == nil {
		scores = []store.Score{}
	}
	return jsonResult(map[string]any{"scores": scores, "next_cursor": next})
}

// -- Analytical tool handlers --

func (h *handlers) analyzeSession(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	sessionID := req.GetString("session_id", "")
	if sessionID == "" {
		return gomcp.NewToolResultError("session_id is required"), nil
	}

	spans, err := h.store.GetSession(ctx, sessionID)
	if err != nil {
		return gomcp.NewToolResultError(err.Error()), nil
	}
	if len(spans) == 0 {
		return gomcp.NewToolResultError("session not found"), nil
	}

	// Build analysis
	var totalCost float64
	var totalInput, totalOutput int64
	var errorCount int
	costByModel := map[string]float64{}
	tokensByModel := map[string][2]int64{} // [input, output]
	var anomalies []string

	for _, s := range spans {
		totalCost += s.CostUSD
		totalInput += s.AIInputTokens
		totalOutput += s.AIOutputTokens
		if s.StatusCode == 2 {
			errorCount++
		}
		if s.AIModel != "" {
			costByModel[s.AIModel] += s.CostUSD
			t := tokensByModel[s.AIModel]
			t[0] += s.AIInputTokens
			t[1] += s.AIOutputTokens
			tokensByModel[s.AIModel] = t
		}
		if s.Anomaly != "" {
			anomalies = append(anomalies, fmt.Sprintf("%s: %s (span %s)", s.Name, s.Anomaly, s.SpanID[:8]))
		}
	}

	startTime := time.Unix(0, int64(spans[0].StartTimeUnixNano))
	endTime := time.Unix(0, int64(spans[len(spans)-1].EndTimeUnixNano))

	// Build model breakdown
	type modelBreakdown struct {
		Model        string  `json:"model"`
		Cost         float64 `json:"cost_usd"`
		InputTokens  int64   `json:"input_tokens"`
		OutputTokens int64   `json:"output_tokens"`
	}
	var models []modelBreakdown
	for m, c := range costByModel {
		t := tokensByModel[m]
		models = append(models, modelBreakdown{Model: m, Cost: c, InputTokens: t[0], OutputTokens: t[1]})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Cost > models[j].Cost })

	analysis := map[string]any{
		"session_id":     sessionID,
		"span_count":     len(spans),
		"duration":       endTime.Sub(startTime).String(),
		"start_time":     startTime.Format(time.RFC3339),
		"end_time":       endTime.Format(time.RFC3339),
		"total_cost_usd": totalCost,
		"total_input_tokens":  totalInput,
		"total_output_tokens": totalOutput,
		"error_count":    errorCount,
		"model_breakdown": models,
		"anomalies":      anomalies,
	}

	return jsonResult(analysis)
}

func (h *handlers) compareModels(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	since := req.GetString("since", "24h")
	filter := store.SpanFilter{
		Since: parseDuration(since),
	}

	stats, err := h.store.GetStatsByModel(ctx, filter)
	if err != nil {
		return gomcp.NewToolResultError(err.Error()), nil
	}
	if len(stats) == 0 {
		return gomcp.NewToolResultText("No model data found in the specified time range."), nil
	}

	type comparison struct {
		Model             string  `json:"model"`
		SpanCount         int64   `json:"span_count"`
		TotalCost         float64 `json:"total_cost_usd"`
		InputTokens       int64   `json:"input_tokens"`
		OutputTokens      int64   `json:"output_tokens"`
		CostPerKInputTok  float64 `json:"cost_per_1k_input_tokens"`
		CostPerKOutputTok float64 `json:"cost_per_1k_output_tokens"`
		AvgDurationMs     float64 `json:"avg_duration_ms"`
		ErrorRate         float64 `json:"error_rate_pct"`
	}

	var comparisons []comparison
	for _, s := range stats {
		c := comparison{
			Model:         s.Model,
			SpanCount:     s.SpanCount,
			TotalCost:     s.TotalCost,
			InputTokens:   s.InputTokens,
			OutputTokens:  s.OutputTokens,
			AvgDurationMs: s.AvgDurationMs,
		}
		totalTokens := s.InputTokens + s.OutputTokens
		if totalTokens > 0 && s.TotalCost > 0 {
			if s.InputTokens > 0 {
				c.CostPerKInputTok = (s.TotalCost * 1000) / float64(s.InputTokens)
			}
			if s.OutputTokens > 0 {
				c.CostPerKOutputTok = (s.TotalCost * 1000) / float64(s.OutputTokens)
			}
		}
		if s.SpanCount > 0 {
			c.ErrorRate = float64(s.ErrorCount) / float64(s.SpanCount) * 100
		}
		comparisons = append(comparisons, c)
	}

	sort.Slice(comparisons, func(i, j int) bool {
		return comparisons[i].TotalCost > comparisons[j].TotalCost
	})

	return jsonResult(comparisons)
}

func (h *handlers) suggestOptimizations(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	since := req.GetString("since", "24h")
	filter := store.SpanFilter{
		Since: parseDuration(since),
	}

	modelStats, err := h.store.GetStatsByModel(ctx, filter)
	if err != nil {
		return gomcp.NewToolResultError(err.Error()), nil
	}

	anomalies, err := h.store.GetAnomalies(ctx, store.SpanFilter{
		Since: parseDuration(since),
		Limit: 100,
	})
	if err != nil {
		return gomcp.NewToolResultError(err.Error()), nil
	}

	overallStats, err := h.store.GetStats(ctx, filter)
	if err != nil {
		return gomcp.NewToolResultError(err.Error()), nil
	}

	var suggestions []string

	// Check for high-cost models that could be swapped
	for _, ms := range modelStats {
		if ms.TotalCost > 1.0 && ms.SpanCount > 10 {
			avgCostPerCall := ms.TotalCost / float64(ms.SpanCount)
			if avgCostPerCall > 0.05 {
				suggestions = append(suggestions,
					fmt.Sprintf("Model %q costs $%.4f/call on average ($%.2f total over %d calls). Consider using a smaller model for simpler tasks.",
						ms.Model, avgCostPerCall, ms.TotalCost, ms.SpanCount))
			}
		}
		if ms.SpanCount > 0 {
			errorRate := float64(ms.ErrorCount) / float64(ms.SpanCount) * 100
			if errorRate > 10 {
				suggestions = append(suggestions,
					fmt.Sprintf("Model %q has a %.1f%% error rate (%d/%d). Investigate error patterns or consider fallback models.",
						ms.Model, errorRate, ms.ErrorCount, ms.SpanCount))
			}
		}
	}

	// Check anomaly patterns
	anomalyTypes := map[string]int{}
	for _, a := range anomalies {
		anomalyTypes[a.Anomaly]++
	}
	for aType, count := range anomalyTypes {
		if count >= 5 {
			suggestions = append(suggestions,
				fmt.Sprintf("Recurring anomaly %q detected %d times. This pattern should be investigated.", aType, count))
		}
	}

	// Overall cost check
	if overallStats != nil && overallStats.TotalCost > 10.0 {
		suggestions = append(suggestions,
			fmt.Sprintf("Total cost in this period: $%.2f. Review whether all calls are necessary and consider caching repeated queries.",
				overallStats.TotalCost))
	}

	if len(suggestions) == 0 {
		suggestions = append(suggestions, "No significant optimization opportunities found in the analyzed period.")
	}

	result := map[string]any{
		"period":      since,
		"suggestions": suggestions,
		"summary": map[string]any{
			"total_cost":    overallStats.TotalCost,
			"total_spans":   overallStats.TotalSpans,
			"models_used":   len(modelStats),
			"anomaly_count": len(anomalies),
			"error_count":   overallStats.ErrorCount,
		},
	}

	return jsonResult(result)
}

// -- Resource handlers --

func (h *handlers) resourceStats(ctx context.Context, req gomcp.ReadResourceRequest) ([]gomcp.ResourceContents, error) {
	stats, err := h.store.GetStats(ctx, store.SpanFilter{})
	if err != nil {
		return nil, err
	}
	data, _ := json.MarshalIndent(stats, "", "  ")
	return []gomcp.ResourceContents{
		gomcp.TextResourceContents{
			URI:      "lookout://stats",
			MIMEType: "application/json",
			Text:     string(data),
		},
	}, nil
}

func (h *handlers) resourceRecentSessions(ctx context.Context, req gomcp.ReadResourceRequest) ([]gomcp.ResourceContents, error) {
	sessions, err := h.store.GetSessions(ctx, store.SessionFilter{Limit: 10})
	if err != nil {
		return nil, err
	}
	data, _ := json.MarshalIndent(sessions, "", "  ")
	return []gomcp.ResourceContents{
		gomcp.TextResourceContents{
			URI:      "lookout://sessions/recent",
			MIMEType: "application/json",
			Text:     string(data),
		},
	}, nil
}

// -- Helpers --

func jsonResult(v any) (*gomcp.CallToolResult, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return gomcp.NewToolResultError(err.Error()), nil
	}
	return gomcp.NewToolResultText(string(data)), nil
}

func parseDuration(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	s = strings.TrimSpace(s)
	if len(s) > 1 {
		numStr := s[:len(s)-1]
		unit := s[len(s)-1]
		var n int
		if _, err := fmt.Sscanf(numStr, "%d", &n); err == nil {
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
	t, _ := time.Parse(time.RFC3339, s)
	return t
}
