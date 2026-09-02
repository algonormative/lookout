package ai

import (
	"fmt"
	"strings"

	"github.com/algonormative/lookout/internal/store"
)

// Enrich applies cost calculation, throughput, and anomaly detection to a span.
func Enrich(span *store.SpanRecord) {
	enrichCost(span)
	enrichThroughput(span)
	enrichAnomaly(span)
}

func enrichCost(span *store.SpanRecord) {
	if span.AIModel == "" {
		return
	}
	span.CostUSD = CalculateCost(span.AIModel, span.AIInputTokens, span.AIOutputTokens)
}

func enrichThroughput(span *store.SpanRecord) {
	dur := span.DurationSeconds()
	if dur <= 0 || span.AIOutputTokens <= 0 {
		return
	}
	span.TokensPerSec = float64(span.AIOutputTokens) / dur
}

func enrichAnomaly(span *store.SpanRecord) {
	var flags []string

	// Error status
	if span.StatusCode == 2 {
		msg := span.StatusMessage
		if msg == "" {
			msg = "unknown"
		}
		flags = append(flags, fmt.Sprintf("error: %s", msg))
	}

	dur := span.DurationSeconds()

	// Long duration (>10 min)
	if dur > 600 {
		flags = append(flags, fmt.Sprintf("long_duration: %.0fs", dur))
	}

	// High output tokens (>100k)
	if span.AIOutputTokens > 100_000 {
		flags = append(flags, fmt.Sprintf("high_output_tokens: %d", span.AIOutputTokens))
	}

	// AI span with 0 output tokens (>1s duration)
	if span.AIModel != "" && span.AIOutputTokens == 0 && dur > 1 {
		flags = append(flags, "zero_output_tokens")
	}

	// Low throughput (<5 tok/s for spans >5s)
	if span.AIModel != "" && span.AIOutputTokens > 0 && dur > 5 && span.TokensPerSec < 5 {
		flags = append(flags, fmt.Sprintf("low_throughput: %.1f tok/s", span.TokensPerSec))
	}

	if len(flags) > 0 {
		span.Anomaly = strings.Join(flags, "; ")
	}
}
