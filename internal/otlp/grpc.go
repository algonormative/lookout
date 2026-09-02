package otlp

import (
	"context"
	"fmt"
	"log"
	"net"

	"google.golang.org/grpc"

	"github.com/algonormative/lookout/internal/ai"
	"github.com/algonormative/lookout/internal/store"
	collectorpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	metricscollectorpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
)

// GRPCReceiver accepts OTLP gRPC requests on :4317.
type GRPCReceiver struct {
	store   store.Store
	ring    *store.Ring
	onSpans SpanCallback
	server  *grpc.Server
	addr    string
}

// NewGRPCReceiver creates a new OTLP gRPC receiver.
func NewGRPCReceiver(addr string, s store.Store, ring *store.Ring, onSpans SpanCallback) *GRPCReceiver {
	r := &GRPCReceiver{
		store:   s,
		ring:    ring,
		onSpans: onSpans,
		addr:    addr,
		server:  grpc.NewServer(),
	}

	collectorpb.RegisterTraceServiceServer(r.server, &traceService{r: r})
	metricscollectorpb.RegisterMetricsServiceServer(r.server, &metricsService{r: r})

	return r
}

// Start begins listening for OTLP gRPC requests.
func (r *GRPCReceiver) Start() error {
	ln, err := net.Listen("tcp", r.addr)
	if err != nil {
		return fmt.Errorf("otlp grpc listen: %w", err)
	}
	log.Printf("OTLP gRPC receiver listening on %s", r.addr)
	go r.server.Serve(ln)
	return nil
}

// Stop gracefully shuts down the receiver.
func (r *GRPCReceiver) Stop() {
	r.server.GracefulStop()
}

// traceService implements the OTLP TraceService gRPC server.
type traceService struct {
	collectorpb.UnimplementedTraceServiceServer
	r *GRPCReceiver
}

func (s *traceService) Export(ctx context.Context, req *collectorpb.ExportTraceServiceRequest) (*collectorpb.ExportTraceServiceResponse, error) {
	spans := ConvertTraceRequest(req.GetResourceSpans())

	for i := range spans {
		ai.Enrich(&spans[i])
	}

	if err := s.r.store.InsertSpans(ctx, spans); err != nil {
		log.Printf("ERROR grpc insert spans: %v", err)
		return nil, fmt.Errorf("store: %w", err)
	}

	if s.r.ring != nil {
		s.r.ring.PushBatch(spans)
	}

	if s.r.onSpans != nil {
		s.r.onSpans(spans)
	}

	return &collectorpb.ExportTraceServiceResponse{}, nil
}

// metricsService implements the OTLP MetricsService gRPC server.
type metricsService struct {
	metricscollectorpb.UnimplementedMetricsServiceServer
	r *GRPCReceiver
}

func (s *metricsService) Export(ctx context.Context, req *metricscollectorpb.ExportMetricsServiceRequest) (*metricscollectorpb.ExportMetricsServiceResponse, error) {
	rollups := ConvertMetricsRequest(req.GetResourceMetrics())

	for _, rollup := range rollups {
		if err := s.r.store.UpsertMetricRollup(ctx, rollup); err != nil {
			log.Printf("ERROR grpc upsert metric rollup: %v", err)
		}
	}

	return &metricscollectorpb.ExportMetricsServiceResponse{}, nil
}
