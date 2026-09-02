package cli

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/algonormative/lookout/internal/store"
)

// FormatSpans writes spans in the specified format.
func FormatSpans(w io.Writer, spans []store.SpanRecord, format string) {
	switch format {
	case "json":
		json.NewEncoder(w).Encode(spans)
	case "csv":
		writeSpansCSV(w, spans)
	default:
		writeSpansTable(w, spans)
	}
}

// FormatSessions writes sessions in the specified format.
func FormatSessions(w io.Writer, sessions []store.Session, format string) {
	switch format {
	case "json":
		json.NewEncoder(w).Encode(sessions)
	case "csv":
		writeSessionsCSV(w, sessions)
	default:
		writeSessionsTable(w, sessions)
	}
}

// FormatStats writes stats in the specified format.
func FormatStats(w io.Writer, stats *store.Stats, format string) {
	switch format {
	case "json":
		json.NewEncoder(w).Encode(stats)
	case "csv":
		cw := csv.NewWriter(w)
		cw.Write([]string{"total_spans", "total_traces", "total_sessions", "input_tokens", "output_tokens", "cost_usd", "ai_spans", "errors"})
		cw.Write([]string{
			fmt.Sprintf("%d", stats.TotalSpans),
			fmt.Sprintf("%d", stats.TotalTraces),
			fmt.Sprintf("%d", stats.TotalSessions),
			fmt.Sprintf("%d", stats.TotalInputTokens),
			fmt.Sprintf("%d", stats.TotalOutputTokens),
			fmt.Sprintf("%.4f", stats.TotalCost),
			fmt.Sprintf("%d", stats.AISpanCount),
			fmt.Sprintf("%d", stats.ErrorCount),
		})
		cw.Flush()
	default:
		fmt.Fprintf(w, "Total Spans:    %d\n", stats.TotalSpans)
		fmt.Fprintf(w, "Total Traces:   %d\n", stats.TotalTraces)
		fmt.Fprintf(w, "Total Sessions: %d\n", stats.TotalSessions)
		fmt.Fprintf(w, "AI Spans:       %d\n", stats.AISpanCount)
		fmt.Fprintf(w, "Input Tokens:   %d\n", stats.TotalInputTokens)
		fmt.Fprintf(w, "Output Tokens:  %d\n", stats.TotalOutputTokens)
		fmt.Fprintf(w, "Total Cost:     $%.4f\n", stats.TotalCost)
		fmt.Fprintf(w, "Errors:         %d\n", stats.ErrorCount)
	}
}

// FormatModelStats writes model stats in the specified format.
func FormatModelStats(w io.Writer, stats []store.ModelStats, format string) {
	switch format {
	case "json":
		json.NewEncoder(w).Encode(stats)
	default:
		fmt.Fprintf(w, "%-25s %8s %12s %12s %10s %8s\n", "MODEL", "SPANS", "IN_TOKENS", "OUT_TOKENS", "COST", "ERRORS")
		fmt.Fprintf(w, "%s\n", strings.Repeat("-", 80))
		for _, ms := range stats {
			fmt.Fprintf(w, "%-25s %8d %12d %12d $%8.4f %8d\n",
				truncate(ms.Model, 25), ms.SpanCount, ms.InputTokens, ms.OutputTokens, ms.TotalCost, ms.ErrorCount)
		}
	}
}

// FormatAgentStats writes agent stats in the specified format.
func FormatAgentStats(w io.Writer, stats []store.AgentStats, format string) {
	switch format {
	case "json":
		json.NewEncoder(w).Encode(stats)
	default:
		fmt.Fprintf(w, "%-20s %8s %10s %12s %12s %10s %8s\n", "AGENT", "SPANS", "SESSIONS", "IN_TOKENS", "OUT_TOKENS", "COST", "ERRORS")
		fmt.Fprintf(w, "%s\n", strings.Repeat("-", 85))
		for _, as := range stats {
			fmt.Fprintf(w, "%-20s %8d %10d %12d %12d $%8.4f %8d\n",
				truncate(as.Agent, 20), as.SpanCount, as.SessionCount, as.InputTokens, as.OutputTokens, as.TotalCost, as.ErrorCount)
		}
	}
}

// FormatMetricRollups writes metric rollups in the specified format.
func FormatMetricRollups(w io.Writer, rollups []store.MetricRollup, format string) {
	switch format {
	case "json":
		json.NewEncoder(w).Encode(rollups)
	default:
		fmt.Fprintf(w, "%-20s %12s %8s %12s %12s %12s %12s\n", "TIME", "COUNT", "TYPE", "SUM", "MIN", "MAX", "LAST")
		fmt.Fprintf(w, "%s\n", strings.Repeat("-", 95))
		for _, m := range rollups {
			t := time.Unix(m.BucketStart, 0).Format("2006-01-02 15:04")
			fmt.Fprintf(w, "%-20s %12d %8s %12.2f %12.2f %12.2f %12.2f\n",
				t, m.Count, m.MetricType, m.Sum, m.Min, m.Max, m.Last)
		}
	}
}

func writeSpansTable(w io.Writer, spans []store.SpanRecord) {
	fmt.Fprintf(w, "%-12s %-30s %-20s %10s %10s %8s %s\n", "SPAN_ID", "NAME", "MODEL", "DURATION", "COST", "TOK/S", "ANOMALY")
	fmt.Fprintf(w, "%s\n", strings.Repeat("-", 110))
	for _, sp := range spans {
		dur := sp.DurationSeconds()
		durStr := formatDuration(dur)
		costStr := ""
		if sp.CostUSD > 0 {
			costStr = fmt.Sprintf("$%.4f", sp.CostUSD)
		}
		tpsStr := ""
		if sp.TokensPerSec > 0 {
			tpsStr = fmt.Sprintf("%.1f", sp.TokensPerSec)
		}
		anomaly := truncate(sp.Anomaly, 30)

		fmt.Fprintf(w, "%-12s %-30s %-20s %10s %10s %8s %s\n",
			truncate(sp.SpanID, 12),
			truncate(sp.Name, 30),
			truncate(sp.AIModel, 20),
			durStr, costStr, tpsStr, anomaly)
	}
}

func writeSpansCSV(w io.Writer, spans []store.SpanRecord) {
	cw := csv.NewWriter(w)
	cw.Write([]string{"span_id", "trace_id", "name", "model", "agent", "duration_s", "cost_usd", "tokens_per_sec", "input_tokens", "output_tokens", "status", "anomaly"})
	for _, sp := range spans {
		status := "unset"
		switch sp.StatusCode {
		case 1:
			status = "ok"
		case 2:
			status = "error"
		}
		cw.Write([]string{
			sp.SpanID, sp.TraceID, sp.Name, sp.AIModel, sp.AgentName,
			fmt.Sprintf("%.3f", sp.DurationSeconds()),
			fmt.Sprintf("%.6f", sp.CostUSD),
			fmt.Sprintf("%.1f", sp.TokensPerSec),
			fmt.Sprintf("%d", sp.AIInputTokens),
			fmt.Sprintf("%d", sp.AIOutputTokens),
			status, sp.Anomaly,
		})
	}
	cw.Flush()
}

func writeSessionsTable(w io.Writer, sessions []store.Session) {
	fmt.Fprintf(w, "%-20s %-15s %8s %8s %12s %12s %10s %8s\n", "SESSION_ID", "AGENT", "SPANS", "TRACES", "IN_TOKENS", "OUT_TOKENS", "COST", "ERRORS")
	fmt.Fprintf(w, "%s\n", strings.Repeat("-", 100))
	for _, s := range sessions {
		fmt.Fprintf(w, "%-20s %-15s %8d %8d %12d %12d $%8.4f %8d\n",
			truncate(s.SessionID, 20),
			truncate(s.AgentName, 15),
			s.SpanCount, s.TraceCount,
			s.TotalInputTokens, s.TotalOutputTokens,
			s.TotalCost, s.ErrorCount)
	}
}

func writeSessionsCSV(w io.Writer, sessions []store.Session) {
	cw := csv.NewWriter(w)
	cw.Write([]string{"session_id", "agent", "spans", "traces", "start_time", "end_time", "cost_usd", "input_tokens", "output_tokens", "errors"})
	for _, s := range sessions {
		cw.Write([]string{
			s.SessionID, s.AgentName,
			fmt.Sprintf("%d", s.SpanCount), fmt.Sprintf("%d", s.TraceCount),
			s.StartTime.Format(time.RFC3339), s.EndTime.Format(time.RFC3339),
			fmt.Sprintf("%.6f", s.TotalCost),
			fmt.Sprintf("%d", s.TotalInputTokens), fmt.Sprintf("%d", s.TotalOutputTokens),
			fmt.Sprintf("%d", s.ErrorCount),
		})
	}
	cw.Flush()
}

func formatDuration(seconds float64) string {
	if seconds < 0.001 {
		return "<1ms"
	}
	if seconds < 1 {
		return fmt.Sprintf("%.0fms", seconds*1000)
	}
	if seconds < 60 {
		return fmt.Sprintf("%.1fs", seconds)
	}
	return fmt.Sprintf("%.1fm", seconds/60)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "~"
}
