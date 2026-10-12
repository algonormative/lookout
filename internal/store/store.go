package store

import "context"

// Store is the main storage interface for lookout.
type Store interface {
	// Spans
	InsertSpans(ctx context.Context, spans []SpanRecord) error
	QuerySpans(ctx context.Context, filter SpanFilter) ([]SpanRecord, error)
	GetTrace(ctx context.Context, traceID string) ([]SpanRecord, error)

	// Stats
	GetStats(ctx context.Context, filter SpanFilter) (*Stats, error)
	GetStatsByModel(ctx context.Context, filter SpanFilter) ([]ModelStats, error)
	GetStatsByAgent(ctx context.Context, filter SpanFilter) ([]AgentStats, error)
	GetCostReport(ctx context.Context, filter SpanFilter, bucket string, groupBy string) ([]CostBucket, error)

	// Sessions
	GetSessions(ctx context.Context, filter SessionFilter) ([]Session, error)
	GetSession(ctx context.Context, sessionID string) ([]SpanRecord, error)

	// Anomalies
	GetAnomalies(ctx context.Context, filter SpanFilter) ([]SpanRecord, error)

	// Metrics
	UpsertMetricRollup(ctx context.Context, rollup MetricRollup) error
	QueryMetricRollups(ctx context.Context, filter MetricFilter) ([]MetricRollup, error)
	ListMetricNames(ctx context.Context) ([]string, error)

	// Scores (verdicts posted back by judge processes)
	UpsertScores(ctx context.Context, scores []Score) ([]string, error)
	QueryScores(ctx context.Context, filter ScoreFilter) ([]Score, string, error)
	CountScores(ctx context.Context, filter ScoreFilter) (int64, error)

	// Trace read model
	QueryTraceList(ctx context.Context, filter TraceListFilter) (*TraceListPage, error)

	// Maintenance
	Cleanup(ctx context.Context, retentionDays int) (int64, error)
	Close() error
}
