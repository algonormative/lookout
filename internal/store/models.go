package store

import (
	"encoding/json"
	"time"
)

// SpanRecord is the flattened representation of an OTLP span stored in SQLite.
type SpanRecord struct {
	TraceID          string  `json:"trace_id"`
	SpanID           string  `json:"span_id"`
	ParentSpanID     string  `json:"parent_span_id,omitempty"`
	Name             string  `json:"name"`
	Kind             int     `json:"kind"`
	StartTimeUnixNano uint64 `json:"start_time_unix_nano"`
	EndTimeUnixNano   uint64 `json:"end_time_unix_nano"`
	AttributesJSON   string  `json:"attributes_json"`
	ResourceJSON     string  `json:"resource_json"`
	StatusCode       int     `json:"status_code"`
	StatusMessage    string  `json:"status_message,omitempty"`

	// AI semantic convention fields (extracted from attributes)
	AISystem       string `json:"ai_system,omitempty"`
	AIModel        string `json:"ai_model,omitempty"`
	AIInputTokens  int64  `json:"ai_input_tokens,omitempty"`
	AIOutputTokens int64  `json:"ai_output_tokens,omitempty"`

	// Agent fields
	AgentName      string `json:"agent_name,omitempty"`
	AgentTaskID    string `json:"agent_task_id,omitempty"`
	AgentSessionID string `json:"agent_session_id,omitempty"`

	// ServiceName is the resource service.name of the producer.
	ServiceName string `json:"service_name,omitempty"`

	// Langfuse-style identity: user (langfuse.user.id, else user.id), trace
	// name (langfuse.trace.name) and observation type (langfuse.observation.type,
	// else inferred: generation / tool / span).
	UserID    string `json:"user_id,omitempty"`
	TraceName string `json:"trace_name,omitempty"`
	ObsType   string `json:"obs_type,omitempty"`

	// Span events (prompts, completions, tool messages)
	EventsJSON string `json:"events_json,omitempty"`

	// Enrichment (computed on ingest)
	CostUSD     float64  `json:"cost_usd,omitempty"`
	TokensPerSec float64 `json:"tokens_per_sec,omitempty"`
	Anomaly     string   `json:"anomaly,omitempty"`

	InsertedAt time.Time `json:"inserted_at"`
}

// SpanEvent represents a parsed span event.
type SpanEvent struct {
	Name       string         `json:"name"`
	TimeNano   uint64         `json:"time_unix_nano,omitempty"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

// ParsedEvents returns the span events parsed from EventsJSON.
func (s *SpanRecord) ParsedEvents() []SpanEvent {
	if s.EventsJSON == "" || s.EventsJSON == "[]" {
		return nil
	}
	var events []SpanEvent
	json.Unmarshal([]byte(s.EventsJSON), &events)
	return events
}

// DurationSeconds returns the span duration in seconds.
func (s *SpanRecord) DurationSeconds() float64 {
	if s.EndTimeUnixNano <= s.StartTimeUnixNano {
		return 0
	}
	return float64(s.EndTimeUnixNano-s.StartTimeUnixNano) / 1e9
}

// MetricRollup stores pre-aggregated metric data in time buckets.
type MetricRollup struct {
	Name        string  `json:"name"`
	LabelsJSON  string  `json:"labels_json"`
	BucketStart int64   `json:"bucket_start"`
	BucketWidth int64   `json:"bucket_width"` // 60, 3600, or 86400
	MetricType  string  `json:"metric_type"`  // sum, gauge, histogram
	Count       int64   `json:"count"`
	Sum         float64 `json:"sum"`
	Min         float64 `json:"min"`
	Max         float64 `json:"max"`
	Last        float64 `json:"last"`
}

// Session represents an agent session with aggregated stats.
type Session struct {
	SessionID  string    `json:"session_id"`
	AgentName  string    `json:"agent_name"`
	SpanCount  int       `json:"span_count"`
	TraceCount int       `json:"trace_count"`
	StartTime  time.Time `json:"start_time"`
	EndTime    time.Time `json:"end_time"`
	TotalCost  float64   `json:"total_cost"`
	TotalInputTokens  int64 `json:"total_input_tokens"`
	TotalOutputTokens int64 `json:"total_output_tokens"`
	ErrorCount int       `json:"error_count"`
}

// Stats holds aggregate statistics.
type Stats struct {
	TotalSpans    int64   `json:"total_spans"`
	TotalTraces   int64   `json:"total_traces"`
	TotalSessions int64   `json:"total_sessions"`
	TotalInputTokens  int64 `json:"total_input_tokens"`
	TotalOutputTokens int64 `json:"total_output_tokens"`
	TotalCost     float64 `json:"total_cost"`
	AISpanCount   int64   `json:"ai_span_count"`
	ErrorCount    int64   `json:"error_count"`
}

// ModelStats holds stats grouped by model.
type ModelStats struct {
	Model         string  `json:"model"`
	SpanCount     int64   `json:"span_count"`
	InputTokens   int64   `json:"input_tokens"`
	OutputTokens  int64   `json:"output_tokens"`
	TotalCost     float64 `json:"total_cost"`
	AvgDurationMs float64 `json:"avg_duration_ms"`
	ErrorCount    int64   `json:"error_count"`
}

// AgentStats holds stats grouped by agent.
type AgentStats struct {
	Agent         string  `json:"agent"`
	SpanCount     int64   `json:"span_count"`
	SessionCount  int64   `json:"session_count"`
	InputTokens   int64   `json:"input_tokens"`
	OutputTokens  int64   `json:"output_tokens"`
	TotalCost     float64 `json:"total_cost"`
	ErrorCount    int64   `json:"error_count"`
}

// CostBucket holds cost data for a time bucket.
type CostBucket struct {
	Bucket    string  `json:"bucket"`    // time bucket label
	GroupBy   string  `json:"group_by"`  // model or agent name
	Cost      float64 `json:"cost"`
	SpanCount int64   `json:"span_count"`
}

// SpanFilter specifies query filters for spans.
type SpanFilter struct {
	TraceID    string
	Agent      string
	Model      string
	Since      time.Time
	Until      time.Time
	DurationGt float64 // minimum duration in seconds
	Status     string  // "ok", "error", "unset"
	CostGt     float64
	SortBy     string // "time", "duration", "cost", "tokens"
	Limit      int
	Offset     int
}

// SessionFilter specifies query filters for sessions.
type SessionFilter struct {
	Agent string
	Since time.Time
	Limit int
}

// MetricFilter specifies query filters for metric rollups.
type MetricFilter struct {
	Name        string
	Since       time.Time
	BucketWidth int64             // 60, 3600, or 86400
	Labels      map[string]string // optional label filter
}
