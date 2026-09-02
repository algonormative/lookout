package ai

import (
	"strings"
	"testing"

	"github.com/algonormative/lookout/internal/store"
)

func TestEnrichCost(t *testing.T) {
	span := &store.SpanRecord{
		AIModel:        "claude-sonnet-4",
		AIInputTokens:  1000,
		AIOutputTokens: 500,
	}
	Enrich(span)

	// (1000 * 3.00 + 500 * 15.00) / 1_000_000 = (3000 + 7500) / 1_000_000 = 0.0105
	expected := 0.0105
	if span.CostUSD < expected-0.0001 || span.CostUSD > expected+0.0001 {
		t.Errorf("expected cost ~%.4f, got %.4f", expected, span.CostUSD)
	}
}

func TestEnrichThroughput(t *testing.T) {
	span := &store.SpanRecord{
		AIModel:           "claude-sonnet-4",
		AIOutputTokens:    1000,
		StartTimeUnixNano: 0,
		EndTimeUnixNano:   10_000_000_000, // 10s
	}
	Enrich(span)

	if span.TokensPerSec < 99.9 || span.TokensPerSec > 100.1 {
		t.Errorf("expected ~100 tok/s, got %.1f", span.TokensPerSec)
	}
}

func TestEnrichAnomalyError(t *testing.T) {
	span := &store.SpanRecord{
		StatusCode:    2,
		StatusMessage: "timeout",
	}
	Enrich(span)

	if !strings.Contains(span.Anomaly, "error: timeout") {
		t.Errorf("expected error anomaly, got %q", span.Anomaly)
	}
}

func TestEnrichAnomalyLongDuration(t *testing.T) {
	span := &store.SpanRecord{
		StartTimeUnixNano: 0,
		EndTimeUnixNano:   700_000_000_000, // 700s > 600s
	}
	Enrich(span)

	if !strings.Contains(span.Anomaly, "long_duration") {
		t.Errorf("expected long_duration anomaly, got %q", span.Anomaly)
	}
}

func TestEnrichAnomalyZeroOutputTokens(t *testing.T) {
	span := &store.SpanRecord{
		AIModel:           "claude-sonnet-4",
		AIOutputTokens:    0,
		StartTimeUnixNano: 0,
		EndTimeUnixNano:   5_000_000_000, // 5s > 1s
	}
	Enrich(span)

	if !strings.Contains(span.Anomaly, "zero_output_tokens") {
		t.Errorf("expected zero_output_tokens anomaly, got %q", span.Anomaly)
	}
}

func TestEnrichNoAnomalyForHealthySpan(t *testing.T) {
	span := &store.SpanRecord{
		AIModel:           "claude-sonnet-4",
		AIInputTokens:     1000,
		AIOutputTokens:    500,
		StartTimeUnixNano: 0,
		EndTimeUnixNano:   2_000_000_000, // 2s
		StatusCode:        1,
	}
	Enrich(span)

	if span.Anomaly != "" {
		t.Errorf("expected no anomaly, got %q", span.Anomaly)
	}
}
