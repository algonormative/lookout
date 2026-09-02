package cli

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/algonormative/lookout/internal/store"
)

// QueryTraces runs the "query traces" command.
func QueryTraces(ctx context.Context, s store.Store, args QueryTracesArgs) error {
	filter := store.SpanFilter{
		TraceID:    args.TraceID,
		Agent:      args.Agent,
		Model:      args.Model,
		Since:      parseSince(args.Since),
		Until:      parseSince(args.Until),
		DurationGt: parseDurationGt(args.DurationGt),
		Status:     args.Status,
		CostGt:     args.CostGt,
		SortBy:     args.SortBy,
		Limit:      args.Limit,
	}

	spans, err := s.QuerySpans(ctx, filter)
	if err != nil {
		return fmt.Errorf("query spans: %w", err)
	}

	FormatSpans(os.Stdout, spans, args.Format)
	return nil
}

// QuerySessions runs the "query sessions" command.
func QuerySessions(ctx context.Context, s store.Store, args QuerySessionsArgs) error {
	filter := store.SessionFilter{
		Agent: args.Agent,
		Since: parseSince(args.Since),
		Limit: args.Limit,
	}

	sessions, err := s.GetSessions(ctx, filter)
	if err != nil {
		return fmt.Errorf("query sessions: %w", err)
	}

	FormatSessions(os.Stdout, sessions, args.Format)
	return nil
}

// QueryStats runs the "query stats" command.
func QueryStats(ctx context.Context, s store.Store, args QueryStatsArgs) error {
	filter := store.SpanFilter{
		Since: parseSince(args.Since),
	}

	switch args.GroupBy {
	case "model":
		stats, err := s.GetStatsByModel(ctx, filter)
		if err != nil {
			return err
		}
		FormatModelStats(os.Stdout, stats, args.Format)
	case "agent":
		stats, err := s.GetStatsByAgent(ctx, filter)
		if err != nil {
			return err
		}
		FormatAgentStats(os.Stdout, stats, args.Format)
	default:
		stats, err := s.GetStats(ctx, filter)
		if err != nil {
			return err
		}
		FormatStats(os.Stdout, stats, args.Format)
	}
	return nil
}

// QueryAnomalies runs the "query anomalies" command.
func QueryAnomalies(ctx context.Context, s store.Store, args QueryAnomaliesArgs) error {
	filter := store.SpanFilter{
		Agent: args.Agent,
		Since: parseSince(args.Since),
		Limit: args.Limit,
	}

	spans, err := s.GetAnomalies(ctx, filter)
	if err != nil {
		return fmt.Errorf("query anomalies: %w", err)
	}

	FormatSpans(os.Stdout, spans, args.Format)
	return nil
}

// QueryMetrics runs the "query metrics" command.
func QueryMetrics(ctx context.Context, s store.Store, args QueryMetricsArgs) error {
	var bucketWidth int64
	switch args.Bucket {
	case "1h":
		bucketWidth = 3600
	case "1d":
		bucketWidth = 86400
	default:
		bucketWidth = 60
	}

	filter := store.MetricFilter{
		Name:        args.Name,
		Since:       parseSince(args.Since),
		BucketWidth: bucketWidth,
		Labels:      parseLabelsFlag(args.Labels),
	}

	rollups, err := s.QueryMetricRollups(ctx, filter)
	if err != nil {
		return fmt.Errorf("query metrics: %w", err)
	}

	FormatMetricRollups(os.Stdout, rollups, args.Format)
	return nil
}

// Arg structs

type QueryTracesArgs struct {
	TraceID    string
	Agent      string
	Model      string
	Since      string
	Until      string
	DurationGt string
	Status     string
	CostGt     float64
	SortBy     string
	Limit      int
	Format     string
}

type QuerySessionsArgs struct {
	Agent  string
	Since  string
	Limit  int
	Format string
}

type QueryStatsArgs struct {
	Since   string
	GroupBy string
	Format  string
}

type QueryAnomaliesArgs struct {
	Since  string
	Agent  string
	Limit  int
	Format string
}

type QueryMetricsArgs struct {
	Name   string
	Since  string
	Bucket string
	Labels string
	Format string
}

// parseSince parses a relative duration ("1h", "24h", "7d") or absolute time.
func parseSince(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
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
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func parseDurationGt(s string) float64 {
	if s == "" {
		return 0
	}
	if len(s) > 1 {
		numStr := s[:len(s)-1]
		unit := s[len(s)-1]
		if n, err := strconv.ParseFloat(numStr, 64); err == nil {
			switch unit {
			case 's':
				return n
			case 'm':
				return n * 60
			}
		}
	}
	n, _ := strconv.ParseFloat(s, 64)
	return n
}

func parseLabelsFlag(s string) map[string]string {
	if s == "" {
		return nil
	}
	labels := make(map[string]string)
	for _, pair := range strings.Split(s, ",") {
		parts := strings.SplitN(pair, ":", 2)
		if len(parts) == 2 {
			labels[parts[0]] = parts[1]
		}
	}
	return labels
}
