package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/algonormative/lookout/internal/store"
)

// APIClient implements store.Store over HTTP, targeting a remote lookout server.
// Only read methods used by CLI query commands are implemented; write methods
// return ErrReadOnly.
type APIClient struct {
	baseURL string
	client  *http.Client
}

// New creates an APIClient pointed at baseURL (e.g., "http://host:4320").
func New(baseURL string) *APIClient {
	return &APIClient{
		baseURL: baseURL,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *APIClient) QuerySpans(ctx context.Context, f store.SpanFilter) ([]store.SpanRecord, error) {
	q := url.Values{}
	if f.TraceID != "" {
		q.Set("trace_id", f.TraceID)
	}
	if f.Agent != "" {
		q.Set("agent", f.Agent)
	}
	if f.Model != "" {
		q.Set("model", f.Model)
	}
	if !f.Since.IsZero() {
		q.Set("since", f.Since.Format(time.RFC3339))
	}
	if !f.Until.IsZero() {
		q.Set("until", f.Until.Format(time.RFC3339))
	}
	if f.DurationGt > 0 {
		q.Set("duration_gt", strconv.FormatFloat(f.DurationGt, 'f', -1, 64))
	}
	if f.Status != "" {
		q.Set("status", f.Status)
	}
	if f.CostGt > 0 {
		q.Set("cost_gt", strconv.FormatFloat(f.CostGt, 'f', -1, 64))
	}
	if f.SortBy != "" {
		q.Set("sort_by", f.SortBy)
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}

	var spans []store.SpanRecord
	if err := c.get(ctx, "/v1/traces", q, &spans); err != nil {
		return nil, err
	}
	return spans, nil
}

func (c *APIClient) GetTrace(ctx context.Context, traceID string) ([]store.SpanRecord, error) {
	var spans []store.SpanRecord
	if err := c.get(ctx, "/v1/traces/"+traceID, nil, &spans); err != nil {
		return nil, err
	}
	return spans, nil
}

func (c *APIClient) GetSessions(ctx context.Context, f store.SessionFilter) ([]store.Session, error) {
	q := url.Values{}
	if f.Agent != "" {
		q.Set("agent", f.Agent)
	}
	if !f.Since.IsZero() {
		q.Set("since", f.Since.Format(time.RFC3339))
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}

	var sessions []store.Session
	if err := c.get(ctx, "/v1/sessions", q, &sessions); err != nil {
		return nil, err
	}
	return sessions, nil
}

func (c *APIClient) GetSession(ctx context.Context, sessionID string) ([]store.SpanRecord, error) {
	var spans []store.SpanRecord
	if err := c.get(ctx, "/v1/sessions/"+sessionID, nil, &spans); err != nil {
		return nil, err
	}
	return spans, nil
}

func (c *APIClient) GetStats(ctx context.Context, f store.SpanFilter) (*store.Stats, error) {
	q := spanFilterQuery(f)
	var stats store.Stats
	if err := c.get(ctx, "/v1/stats", q, &stats); err != nil {
		return nil, err
	}
	return &stats, nil
}

func (c *APIClient) GetStatsByModel(ctx context.Context, f store.SpanFilter) ([]store.ModelStats, error) {
	q := spanFilterQuery(f)
	var stats []store.ModelStats
	if err := c.get(ctx, "/v1/stats/by-model", q, &stats); err != nil {
		return nil, err
	}
	return stats, nil
}

func (c *APIClient) GetStatsByAgent(ctx context.Context, f store.SpanFilter) ([]store.AgentStats, error) {
	q := spanFilterQuery(f)
	var stats []store.AgentStats
	if err := c.get(ctx, "/v1/stats/by-agent", q, &stats); err != nil {
		return nil, err
	}
	return stats, nil
}

func (c *APIClient) GetCostReport(ctx context.Context, f store.SpanFilter, bucket string, groupBy string) ([]store.CostBucket, error) {
	q := spanFilterQuery(f)
	if bucket != "" {
		q.Set("bucket", bucket)
	}
	if groupBy != "" {
		q.Set("group_by", groupBy)
	}
	var buckets []store.CostBucket
	if err := c.get(ctx, "/v1/stats/cost", q, &buckets); err != nil {
		return nil, err
	}
	return buckets, nil
}

func (c *APIClient) GetAnomalies(ctx context.Context, f store.SpanFilter) ([]store.SpanRecord, error) {
	q := spanFilterQuery(f)
	var spans []store.SpanRecord
	if err := c.get(ctx, "/v1/anomalies", q, &spans); err != nil {
		return nil, err
	}
	return spans, nil
}

func (c *APIClient) QueryMetricRollups(ctx context.Context, f store.MetricFilter) ([]store.MetricRollup, error) {
	q := url.Values{}
	if !f.Since.IsZero() {
		q.Set("since", f.Since.Format(time.RFC3339))
	}
	switch f.BucketWidth {
	case 3600:
		q.Set("bucket", "1h")
	case 86400:
		q.Set("bucket", "1d")
	default:
		q.Set("bucket", "1m")
	}

	var rollups []store.MetricRollup
	if err := c.get(ctx, "/v1/metrics/"+f.Name, q, &rollups); err != nil {
		return nil, err
	}
	return rollups, nil
}

func (c *APIClient) ListMetricNames(ctx context.Context) ([]string, error) {
	var names []string
	if err := c.get(ctx, "/v1/metrics/names", nil, &names); err != nil {
		return nil, err
	}
	return names, nil
}

// Write methods — not supported on API client

func (c *APIClient) InsertSpans(_ context.Context, _ []store.SpanRecord) error {
	return fmt.Errorf("insert not supported via API client")
}

func (c *APIClient) UpsertMetricRollup(_ context.Context, _ store.MetricRollup) error {
	return fmt.Errorf("upsert not supported via API client")
}

func (c *APIClient) Cleanup(_ context.Context, _ int) (int64, error) {
	return 0, fmt.Errorf("cleanup not supported via API client")
}

func (c *APIClient) Close() error { return nil }

// --- helpers ---

func (c *APIClient) get(ctx context.Context, path string, params url.Values, dest any) error {
	u := c.baseURL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("api request %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("api %s returned %d: %s", path, resp.StatusCode, string(body))
	}

	if err := json.NewDecoder(resp.Body).Decode(dest); err != nil {
		return fmt.Errorf("decode response from %s: %w", path, err)
	}
	return nil
}

func spanFilterQuery(f store.SpanFilter) url.Values {
	q := url.Values{}
	if !f.Since.IsZero() {
		q.Set("since", f.Since.Format(time.RFC3339))
	}
	if !f.Until.IsZero() {
		q.Set("until", f.Until.Format(time.RFC3339))
	}
	if f.Agent != "" {
		q.Set("agent", f.Agent)
	}
	if f.Model != "" {
		q.Set("model", f.Model)
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	return q
}
