package otlp

import (
	"encoding/json"
	"sort"

	"github.com/algonormative/lookout/internal/store"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

// ConvertMetricsRequest converts OTLP metrics into MetricRollup records for each bucket width.
func ConvertMetricsRequest(resourceMetrics []*metricspb.ResourceMetrics) []store.MetricRollup {
	var rollups []store.MetricRollup

	for _, rm := range resourceMetrics {
		for _, sm := range rm.GetScopeMetrics() {
			for _, metric := range sm.GetMetrics() {
				rollups = append(rollups, convertMetric(metric)...)
			}
		}
	}
	return rollups
}

func convertMetric(metric *metricspb.Metric) []store.MetricRollup {
	name := metric.GetName()
	var rollups []store.MetricRollup

	switch data := metric.GetData().(type) {
	case *metricspb.Metric_Sum:
		for _, dp := range data.Sum.GetDataPoints() {
			val := dpValue(dp)
			labels := dpAttributesToJSON(dp.GetAttributes())
			ts := int64(dp.GetTimeUnixNano()) / 1e9
			for _, width := range []int64{60, 3600, 86400} {
				rollups = append(rollups, store.MetricRollup{
					Name:        name,
					LabelsJSON:  labels,
					BucketStart: alignBucket(ts, width),
					BucketWidth: width,
					MetricType:  "sum",
					Count:       1,
					Sum:         val,
					Min:         val,
					Max:         val,
					Last:        val,
				})
			}
		}
	case *metricspb.Metric_Gauge:
		for _, dp := range data.Gauge.GetDataPoints() {
			val := dpValue(dp)
			labels := dpAttributesToJSON(dp.GetAttributes())
			ts := int64(dp.GetTimeUnixNano()) / 1e9
			for _, width := range []int64{60, 3600, 86400} {
				rollups = append(rollups, store.MetricRollup{
					Name:        name,
					LabelsJSON:  labels,
					BucketStart: alignBucket(ts, width),
					BucketWidth: width,
					MetricType:  "gauge",
					Count:       1,
					Sum:         val,
					Min:         val,
					Max:         val,
					Last:        val,
				})
			}
		}
	case *metricspb.Metric_Histogram:
		for _, dp := range data.Histogram.GetDataPoints() {
			val := dp.GetSum()
			labels := histDPAttributesToJSON(dp.GetAttributes())
			ts := int64(dp.GetTimeUnixNano()) / 1e9
			for _, width := range []int64{60, 3600, 86400} {
				rollups = append(rollups, store.MetricRollup{
					Name:        name,
					LabelsJSON:  labels,
					BucketStart: alignBucket(ts, width),
					BucketWidth: width,
					MetricType:  "histogram",
					Count:       int64(dp.GetCount()),
					Sum:         val,
					Min:         dp.GetMin(),
					Max:         dp.GetMax(),
					Last:        val,
				})
			}
		}
	}

	return rollups
}

func dpValue(dp *metricspb.NumberDataPoint) float64 {
	switch v := dp.GetValue().(type) {
	case *metricspb.NumberDataPoint_AsInt:
		return float64(v.AsInt)
	case *metricspb.NumberDataPoint_AsDouble:
		return v.AsDouble
	default:
		return 0
	}
}

func alignBucket(ts, width int64) int64 {
	return (ts / width) * width
}

func dpAttributesToJSON(attrs []*commonpb.KeyValue) string {
	return attrsToJSON(attrs)
}

func histDPAttributesToJSON(attrs []*commonpb.KeyValue) string {
	return attrsToJSON(attrs)
}

func attrsToJSON(attrs []*commonpb.KeyValue) string {
	if len(attrs) == 0 {
		return "{}"
	}
	m := make(map[string]string, len(attrs))
	for _, kv := range attrs {
		m[kv.GetKey()] = kv.GetValue().GetStringValue()
	}
	// Sort keys for deterministic JSON
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sorted := make(map[string]string, len(m))
	for _, k := range keys {
		sorted[k] = m[k]
	}
	b, _ := json.Marshal(sorted)
	return string(b)
}
