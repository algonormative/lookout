package otlp

import (
	"compress/gzip"
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"

	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/algonormative/lookout/internal/ai"
	"github.com/algonormative/lookout/internal/store"
	collectorpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	metricscollectorpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
)

// SpanCallback is called after spans are enriched and before storage.
type SpanCallback func(spans []store.SpanRecord)

// HTTPReceiver accepts OTLP HTTP requests on :4318.
type HTTPReceiver struct {
	store    store.Store
	ring     *store.Ring
	onSpans  SpanCallback
	filter   *Filter
	server  *http.Server
	addr     string
	ingestKey string // when set, the Langfuse path requires it (see SetIngestKey)
}

// NewHTTPReceiver creates a new OTLP HTTP receiver.
func NewHTTPReceiver(addr string, s store.Store, ring *store.Ring, onSpans SpanCallback, filter *Filter) *HTTPReceiver {
	r := &HTTPReceiver{
		filter:  filter,
		store:   s,
		ring:    ring,
		onSpans: onSpans,
		addr:    addr,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/traces", r.handleTraces)
	// Langfuse's OTLP path: lets a Langfuse-configured exporter point here unchanged.
	mux.HandleFunc("POST /api/public/otel/v1/traces", r.handleLangfuseTraces)
	mux.HandleFunc("POST /v1/metrics", r.handleMetrics)

	r.server = &http.Server{
		Addr:    addr,
		Handler: mux,
	}
	return r
}

// Start begins listening for OTLP HTTP requests.
func (r *HTTPReceiver) Start() error {
	ln, err := net.Listen("tcp", r.addr)
	if err != nil {
		return fmt.Errorf("otlp http listen: %w", err)
	}
	log.Printf("OTLP HTTP receiver listening on %s", r.addr)
	go r.server.Serve(ln)
	return nil
}

// Stop gracefully shuts down the receiver.
func (r *HTTPReceiver) Stop(ctx context.Context) error {
	return r.server.Shutdown(ctx)
}

// SetIngestKey makes the Langfuse path (/api/public/otel/v1/traces) require
// key as the HTTP Basic password (Langfuse's secret key; the username is
// ignored) or as a Bearer token. An empty key accepts any request. The plain
// OTLP path /v1/traces is not affected.
func (r *HTTPReceiver) SetIngestKey(key string) { r.ingestKey = key }

func (r *HTTPReceiver) handleLangfuseTraces(w http.ResponseWriter, req *http.Request) {
	if !r.authorized(req) {
		w.Header().Set("WWW-Authenticate", `Basic realm="lookout"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	r.handleTraces(w, req)
}

func (r *HTTPReceiver) authorized(req *http.Request) bool {
	if r.ingestKey == "" {
		return true
	}
	want := []byte(r.ingestKey)
	if _, pass, ok := req.BasicAuth(); ok {
		return subtle.ConstantTimeCompare([]byte(pass), want) == 1
	}
	if tok, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer "); ok {
		return subtle.ConstantTimeCompare([]byte(tok), want) == 1
	}
	return false
}

func (r *HTTPReceiver) handleTraces(w http.ResponseWriter, req *http.Request) {
	var rd io.Reader = req.Body
	if strings.EqualFold(req.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(req.Body)
		if err != nil {
			http.Error(w, "gzip: "+err.Error(), http.StatusBadRequest)
			return
		}
		defer gz.Close()
		rd = gz
	}
	body, err := io.ReadAll(rd)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	var pbReq collectorpb.ExportTraceServiceRequest
	if isJSON(req) {
		if err := protojson.Unmarshal(body, &pbReq); err != nil {
			http.Error(w, "unmarshal json: "+err.Error(), http.StatusBadRequest)
			return
		}
	} else {
		if err := proto.Unmarshal(body, &pbReq); err != nil {
			http.Error(w, "unmarshal: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	spans := ConvertTraceRequest(r.filter, pbReq.GetResourceSpans())

	// Enrich spans
	for i := range spans {
		ai.Enrich(&spans[i])
	}

	// Store
	if err := r.store.InsertSpans(req.Context(), spans); err != nil {
		log.Printf("ERROR insert spans: %v", err)
		http.Error(w, "store: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Push to ring buffer
	if r.ring != nil {
		r.ring.PushBatch(spans)
	}

	// Callback (e.g., WebSocket broadcast)
	if r.onSpans != nil {
		r.onSpans(spans)
	}

	// Return OTLP response in matching format
	resp := &collectorpb.ExportTraceServiceResponse{}
	if isJSON(req) {
		respBytes, _ := protojson.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(respBytes)
	} else {
		respBytes, _ := proto.Marshal(resp)
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
		w.Write(respBytes)
	}
}

func (r *HTTPReceiver) handleMetrics(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	var pbReq metricscollectorpb.ExportMetricsServiceRequest
	if isJSON(req) {
		if err := protojson.Unmarshal(body, &pbReq); err != nil {
			http.Error(w, "unmarshal json: "+err.Error(), http.StatusBadRequest)
			return
		}
	} else {
		if err := proto.Unmarshal(body, &pbReq); err != nil {
			http.Error(w, "unmarshal: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	rollups := ConvertMetricsRequest(pbReq.GetResourceMetrics())

	for _, rollup := range rollups {
		if err := r.store.UpsertMetricRollup(req.Context(), rollup); err != nil {
			log.Printf("ERROR upsert metric rollup: %v", err)
		}
	}

	resp := &metricscollectorpb.ExportMetricsServiceResponse{}
	if isJSON(req) {
		respBytes, _ := protojson.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(respBytes)
	} else {
		respBytes, _ := proto.Marshal(resp)
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
		w.Write(respBytes)
	}
}

func isJSON(req *http.Request) bool {
	ct := req.Header.Get("Content-Type")
	return strings.Contains(ct, "application/json")
}
