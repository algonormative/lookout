package config

import (
	"os"
	"path/filepath"
	"strconv"
)

type Config struct {
	GRPCAddr      string
	HTTPAddr      string
	APIAddr       string
	DBPath        string
	RingSize      int
	RetentionDays int
	PushURL       string
	PushInterval  int
	APIURL        string // Remote API URL for CLI queries (e.g., http://host:4320)

	// Ingest volume controls (see internal/otlp/filter.go).
	DropSpans     string // "service:span,service:span" pairs not stored; "" disables
	MaxAttrBytes  int    // truncate attribute strings longer than this; 0 disables
	SlimSpans     string // "service:span" pairs whose large attribute values are removed; "" disables
	SlimAttrBytes int    // on SlimSpans, remove attribute values larger than this

	// Storage controls (see internal/store/retention.go).
	MaxDBMB    int    // cap on used DB size; oldest whole traces are deleted above it. 0 disables
	ArchiveDir string // spans are written here (gzip JSONL per day) before deletion; "" disables

	// CompressMinBytes: spans whose attributes+events JSON exceed this are
	// stored with compressed payloads (internal/store/payload.go). 0 disables.
	CompressMinBytes int

	// IngestKey, when set, is required (Basic password or Bearer) on the
	// Langfuse OTLP path /api/public/otel/v1/traces.
	IngestKey string
}

// DefaultDropSpans is the LiteLLM (llm-proxy) plumbing that has no analytic value.
const DefaultDropSpans = "llm-proxy:postgres,llm-proxy:auth,llm-proxy:self," +
	"llm-proxy:proxy_pre_call,llm-proxy:batch_write_to_db,llm-proxy:router"

// DefaultSlimSpans repeats the root span's prompt/response content.
const DefaultSlimSpans = "llm-proxy:raw_gen_ai_request"

func Default() Config {
	home, _ := os.UserHomeDir()
	return Config{
		GRPCAddr:      "0.0.0.0:4317",
		HTTPAddr:      "0.0.0.0:4318",
		APIAddr:       "0.0.0.0:4320",
		DBPath:        filepath.Join(home, ".lookout", "traces.db"),
		RingSize:      10000,
		RetentionDays: 7,
		PushURL:       "",
		PushInterval:  60,
		DropSpans:     DefaultDropSpans,
		MaxAttrBytes:  32 * 1024,
		SlimSpans:     DefaultSlimSpans,
		SlimAttrBytes: 2 * 1024,
		MaxDBMB:       1024,

		CompressMinBytes: 4096,
	}
}

func (c *Config) ApplyEnv() {
	if v := os.Getenv("LOOKOUT_GRPC_ADDR"); v != "" {
		c.GRPCAddr = v
	}
	if v := os.Getenv("LOOKOUT_HTTP_ADDR"); v != "" {
		c.HTTPAddr = v
	}
	if v := os.Getenv("LOOKOUT_API_ADDR"); v != "" {
		c.APIAddr = v
	}
	if v := os.Getenv("LOOKOUT_DB_PATH"); v != "" {
		c.DBPath = v
	}
	if v := os.Getenv("LOOKOUT_RING_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.RingSize = n
		}
	}
	if v := os.Getenv("LOOKOUT_RETENTION_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.RetentionDays = n
		}
	}
	if v := os.Getenv("LOOKOUT_PUSH_URL"); v != "" {
		c.PushURL = v
	}
	if v := os.Getenv("LOOKOUT_PUSH_INTERVAL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.PushInterval = n
		}
	}
	if v := os.Getenv("LOOKOUT_API_URL"); v != "" {
		c.APIURL = v
	}
	// For these, a set-but-empty value is meaningful: it disables the rule.
	if v, ok := os.LookupEnv("LOOKOUT_DROP_SPANS"); ok {
		c.DropSpans = v
	}
	if v, ok := os.LookupEnv("LOOKOUT_SLIM_SPANS"); ok {
		c.SlimSpans = v
	}
	if v := os.Getenv("LOOKOUT_MAX_ATTR_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxAttrBytes = n
		}
	}
	if v := os.Getenv("LOOKOUT_SLIM_ATTR_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.SlimAttrBytes = n
		}
	}
	if v := os.Getenv("LOOKOUT_MAX_DB_MB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxDBMB = n
		}
	}
	if v := os.Getenv("LOOKOUT_COMPRESS_MIN_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.CompressMinBytes = n
		}
	}
	if v := os.Getenv("LOOKOUT_INGEST_KEY"); v != "" {
		c.IngestKey = v
	}
	if v := os.Getenv("LOOKOUT_ARCHIVE_DIR"); v != "" {
		c.ArchiveDir = v
	}
}
