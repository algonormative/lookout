# Lookout

OTEL trace collector purpose-built for AI agent workflows. Accepts OpenTelemetry traces and metrics via gRPC and HTTP, stores in SQLite, enriches spans with cost calculation and anomaly detection, and serves analytics through a web UI, REST API, MCP server, and TUI dashboard.

## Features

- **Dual OTLP ingest** -- gRPC (`:4317`) and HTTP (`:4318`)
- **AI-aware enrichment** -- extracts `gen_ai.*` and `agent.*` semantic attributes, calculates cost from token counts, detects anomalies
- **Built-in pricing** -- Claude, GPT-4o/4.1, o3/o4 models with per-token cost tracking
- **Session grouping** -- groups spans by `agent.session_id` for conversation-level analytics
- **Metric rollups** -- OTLP metrics aggregated into 1-minute, 1-hour, and 1-day buckets
- **Web UI** -- dark-themed dashboard at `:4320/` with htmx live updates
- **MCP server** -- query and analyze traces from Claude Code, Cursor, or any MCP client
- **TUI dashboard** -- terminal UI with overview, spans, and anomalies tabs
- **REST API** -- full query API at `:4320/v1/`
- **WebSocket** -- live span stream at `/v1/live`
- **Zero-config** -- sensible defaults, all options via flags or env vars
- **Pure Go** -- no CGO required (uses `modernc.org/sqlite`)

## Quick Start

### From source

```bash
go build -o bin/lookout ./cmd/lookout
bin/lookout serve
```

### Docker

```bash
docker run -d \
  -p 4317:4317 -p 4318:4318 -p 4320:4320 \
  -v lookout-data:/data \
  ghcr.io/algonormative/lookout:latest
```

### Docker Compose

```bash
docker compose up -d
```

## Usage

```bash
# Start the collector
lookout serve

# Query recent spans
lookout query traces --limit 10

# Query by model
lookout query traces --model claude-opus-4-6 --since 1h

# Aggregate stats
lookout query stats
lookout query stats --group-by model

# Agent sessions
lookout query sessions --since 24h

# Anomalies
lookout query anomalies --since 1h

# Metric rollups
lookout query metrics --name gen_ai.tokens --bucket 1h

# TUI dashboard
lookout dash

# MCP server (stdio, for Claude Code / Cursor / etc)
lookout mcp
```

## MCP Server

Run `lookout mcp` to start an MCP server over stdio. Add it to your MCP client config:

```json
{
  "mcpServers": {
    "lookout": {
      "command": "lookout",
      "args": ["mcp"]
    }
  }
}
```

### Tools

| Tool | Description |
|------|-------------|
| `query_traces` | Query spans with filters (model, agent, time range, cost, duration) |
| `query_sessions` | List agent sessions with aggregated stats |
| `get_session` | Get all spans for a specific session |
| `get_trace_conversation` | Structured conversation (chat messages, tool calls) for a trace ID |
| `get_stats` | Aggregate statistics, optionally grouped by model or agent |
| `get_anomalies` | Spans flagged with anomalies |
| `query_metrics` | Query metric rollups by name and bucket width |
| `analyze_session` | Deep session analysis: timeline, cost breakdown, error summary |
| `compare_models` | Compare model performance: cost, throughput, error rates |
| `suggest_optimizations` | AI-driven optimization suggestions based on usage patterns |

### Resources

| URI | Description |
|-----|-------------|
| `lookout://stats` | Current aggregate statistics |
| `lookout://sessions/recent` | 10 most recent agent sessions |

## Sending Traces

Point any OpenTelemetry SDK at Lookout's OTLP endpoint:

```bash
# gRPC
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317

# HTTP
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
export OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
```

For AI spans, use [GenAI semantic conventions](https://opentelemetry.io/docs/specs/semconv/gen-ai/):

| Attribute | Description |
|-----------|-------------|
| `gen_ai.system` | AI provider (e.g. `anthropic`, `openai`) |
| `gen_ai.request.model` | Model name |
| `gen_ai.response.model` | Actual model used |
| `gen_ai.usage.input_tokens` | Input token count |
| `gen_ai.usage.output_tokens` | Output token count |
| `agent.name` | Agent name |
| `agent.session_id` | Session identifier for grouping |

## Configuration

All settings via CLI flags or environment variables:

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `--grpc-addr` | `LOOKOUT_GRPC_ADDR` | `0.0.0.0:4317` | OTLP gRPC address |
| `--http-addr` | `LOOKOUT_HTTP_ADDR` | `0.0.0.0:4318` | OTLP HTTP address |
| `--api-addr` | `LOOKOUT_API_ADDR` | `0.0.0.0:4320` | Analytics API address |
| `--db-path` | `LOOKOUT_DB_PATH` | `~/.lookout/traces.db` | SQLite database path |
| `--ring-size` | `LOOKOUT_RING_SIZE` | `10000` | In-memory ring buffer capacity |
| `--retention-days` | `LOOKOUT_RETENTION_DAYS` | `7` | Auto-cleanup after N days |
| `--drop-spans` | `LOOKOUT_DROP_SPANS` | `llm-proxy:postgres,llm-proxy:auth,llm-proxy:self,llm-proxy:proxy_pre_call,llm-proxy:batch_write_to_db,llm-proxy:router` | Comma-separated `service.name:span name` pairs dropped at ingest (before storage, HTTP and gRPC). Set to empty to disable |
| `--max-attr-bytes` | `LOOKOUT_MAX_ATTR_BYTES` | `32768` | Span/event attribute strings longer than this are truncated (head kept, `…[truncated N bytes]` suffix). `0` disables |
| `--slim-spans` | `LOOKOUT_SLIM_SPANS` | `llm-proxy:raw_gen_ai_request` | `service.name:span name` pairs whose large attribute values are removed (small ones such as model/tokens/timings are kept). Empty disables |
| `--slim-attr-bytes` | `LOOKOUT_SLIM_ATTR_BYTES` | `2048` | On slim spans, attribute values larger than this are removed |

### Ingest volume controls

- **Drop list.** A span whose resource `service.name` and span name match a pair is not stored. Children of a dropped span are re-parented to the nearest kept ancestor when the dropped span(s) are in the same OTLP export request; if the dropped span was exported in a different request, the child keeps its original (now dangling) parent id.
- **Attribute cap.** Applies to span and event attributes (not resource attributes), including strings nested in arrays/maps.
- **Disk space.** SQLite never shrinks its file on `DELETE`. When the hourly retention cleanup deletes spans, lookout runs a full `VACUUM` (then truncates the WAL), at most once per 24 hours per process. It holds the write lock for the duration, so ingest briefly waits; free disk equal to the DB size is needed while it runs.

## Seed Data

Generate test data for development:

```bash
go build -o bin/seed ./cmd/seed
bin/seed --sessions 5 --traces 10
```

## License

MIT
