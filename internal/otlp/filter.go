package otlp

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

// spanKey identifies a span by (service.name, span name).
type spanKey struct {
	service string
	name    string
}

// Filter controls what is kept at ingest time: which spans are dropped
// outright, how large a single attribute string may be, and which spans have
// their large attribute values stripped. A nil *Filter keeps everything
// unchanged.
type Filter struct {
	drop         map[spanKey]struct{}
	slim         map[spanKey]struct{}
	maxAttrBytes int // truncate string values longer than this; 0 disables
	slimBytes    int // on slim spans, drop attribute values larger than this; 0 disables
}

// NewFilter builds a Filter from comma-separated "service:span name" lists.
//
//   - drop: spans matching any pair are not stored at all.
//   - slim: on matching spans, attributes whose value is larger than slimBytes
//     are removed (the small ones, e.g. model/tokens/timings, are kept).
//   - maxAttrBytes: any remaining string value longer than this is truncated
//     (head kept) with a "…[truncated N bytes]" suffix. 0 disables.
//
// An empty list disables the corresponding rule.
func NewFilter(drop, slim string, maxAttrBytes, slimBytes int) (*Filter, error) {
	d, err := ParseSpanList(drop)
	if err != nil {
		return nil, fmt.Errorf("drop list: %w", err)
	}
	s, err := ParseSpanList(slim)
	if err != nil {
		return nil, fmt.Errorf("slim list: %w", err)
	}
	return &Filter{drop: d, slim: s, maxAttrBytes: maxAttrBytes, slimBytes: slimBytes}, nil
}

// ParseSpanList parses "svc:name,svc:name". The service is everything before
// the first colon, so span names may themselves contain colons. Whitespace
// around entries is trimmed; empty entries are ignored.
func ParseSpanList(s string) (map[spanKey]struct{}, error) {
	out := map[spanKey]struct{}{}
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		svc, name, ok := strings.Cut(entry, ":")
		svc, name = strings.TrimSpace(svc), strings.TrimSpace(name)
		if !ok || svc == "" || name == "" {
			return nil, fmt.Errorf("invalid entry %q, want service:span-name", entry)
		}
		out[spanKey{svc, name}] = struct{}{}
	}
	return out, nil
}

func (f *Filter) shouldDrop(service, name string) bool {
	if f == nil || len(f.drop) == 0 {
		return false
	}
	_, ok := f.drop[spanKey{service, name}]
	return ok
}

func (f *Filter) isSlim(service, name string) bool {
	if f == nil || f.slimBytes <= 0 || len(f.slim) == 0 {
		return false
	}
	_, ok := f.slim[spanKey{service, name}]
	return ok
}

func (f *Filter) maxBytes() int {
	if f == nil {
		return 0
	}
	return f.maxAttrBytes
}

// truncateString keeps the head of s up to max bytes (on a rune boundary) and
// appends a marker recording how many bytes were removed.
func truncateString(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	if out, ok := truncateJSONArray(s, max); ok {
		return out
	}
	return headTruncate(s, max)
}

func headTruncate(s string, max int) string {
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return fmt.Sprintf("%s…[truncated %d bytes]", s[:cut], len(s)-cut)
}

func arrayMarker(n, bytes int) string {
	return fmt.Sprintf(`{"role":"system","content":"[lookout: %d earlier items truncated, %d bytes]"}`, n, bytes)
}

// truncateJSONArray handles over-cap strings that are JSON arrays (chat
// message lists, newest last). It drops elements from the front until the
// result fits under max, prepending a marker element, so the newest items
// survive and the output is still valid JSON. If even the last element alone
// does not fit, that element is head-truncated and embedded as a JSON string.
// ok is false when s is not a JSON array (caller falls back to head-truncation).
func truncateJSONArray(s string, max int) (string, bool) {
	if !strings.HasPrefix(strings.TrimSpace(s), "[") {
		return "", false
	}
	var elems []json.RawMessage
	if err := json.Unmarshal([]byte(s), &elems); err != nil || len(elems) == 0 {
		return "", false
	}
	n := len(elems)
	// tail[k] = serialized size of elems[k:] joined with commas.
	tail := make([]int, n+1)
	for i := n - 1; i >= 0; i-- {
		tail[i] = tail[i+1] + len(elems[i])
		if i < n-1 {
			tail[i]++
		}
	}
	droppedBytes := 0
	for k := 1; k < n; k++ {
		droppedBytes += len(elems[k-1])
		marker := arrayMarker(k, droppedBytes)
		if len(marker)+1+tail[k]+2 <= max {
			parts := make([]string, 0, n-k+1)
			parts = append(parts, marker)
			for _, e := range elems[k:] {
				parts = append(parts, string(e))
			}
			return "[" + strings.Join(parts, ",") + "]", true
		}
	}
	// Even the last element alone is too big: drop all earlier items and
	// head-truncate the last one, embedded as a JSON string.
	last := string(elems[n-1])
	droppedBytes = 0
	for _, e := range elems[:n-1] {
		droppedBytes += len(e)
	}
	marker := arrayMarker(n-1, droppedBytes)
	budget := max - len(marker) - 6
	if n == 1 {
		marker = ""
		budget = max - 4
	}
	for ; budget > 0; budget /= 2 {
		enc, _ := json.Marshal(headTruncate(last, budget))
		out := "[" + string(enc) + "]"
		if marker != "" {
			out = "[" + marker + "," + string(enc) + "]"
		}
		if len(out) <= max || budget < 16 {
			return out, true
		}
	}
	return "", false
}

// capValue applies the string cap recursively to nested arrays and maps.
func capValue(v any, max int) any {
	if max <= 0 {
		return v
	}
	switch t := v.(type) {
	case string:
		return truncateString(t, max)
	case []any:
		for i := range t {
			t[i] = capValue(t[i], max)
		}
	case map[string]any:
		for k := range t {
			t[k] = capValue(t[k], max)
		}
	}
	return v
}

// valueSize approximates the stored size of an attribute value.
func valueSize(v any) int {
	switch t := v.(type) {
	case string:
		return len(t)
	case nil, bool, int64, float64:
		return 0
	default:
		b, _ := json.Marshal(t)
		return len(b)
	}
}

// attrMap converts proto attributes to a map, applying the slim rule (when
// slim is true) and then the string cap. Safe on a nil receiver when slim is
// false.
func (f *Filter) attrMap(attrs []*commonpb.KeyValue, slim bool) map[string]any {
	m := make(map[string]any, len(attrs))
	for _, kv := range attrs {
		v := kvToValue(kv.GetValue())
		if slim && valueSize(v) > f.slimBytes {
			continue
		}
		m[kv.GetKey()] = capValue(v, f.maxBytes())
	}
	return m
}
