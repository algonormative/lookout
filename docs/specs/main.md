# lookout-go — Spec

OTEL trace + metrics collector for AI workflows. Go implementation.

## Overview

lookout-go accepts OpenTelemetry traces and metrics via standard OTLP (gRPC + HTTP), stores
them in SQLite, and serves an analytics API. It understands AI workflow semantics
— chat completions, tool calls, agent sessions — and auto-enriches spans with
cost, throughput, and anomaly data.

Designed for the berth agentic coding stack. Works with OpenClaw, Claude Code,
bosun agent containers, and any OTEL-instrumented service.

## Goals

- **Standard ingest**: OTLP gRPC (:4317) and HTTP/protobuf (:4318) for traces + metrics
- **AI-native**: first-class support for GenAI semantic conventions + agent workflows
- **Simple ops**: single binary, SQLite storage, no external dependencies at runtime
- **Fast reads**: in-memory ring buffer for live dashboard, SQLite for historical queries
- **Actionable**: cost tracking, throughput metrics, anomaly detection out of the box
- **Metrics rollups**: aggregate OTLP metrics on ingest into time-bucketed rollups (1m, 1h, 1d)

## Non-Goals

- Full APM/distributed tracing platform (use Jaeger/Tempo for that)
- Logs collection (traces + metrics only)
- Multi-tenant / multi-user access control
- Horizontal scaling (single-node by design)

## Architecture

```
┌──────────────────────────────────────────────────────────┐
│                      lookout-go                          │
│                                                          │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────────┐  │
│  │ OTLP gRPC   │  │ OTLP HTTP   │  │  Analytics API  │  │
│  │ :4317       │  │ :4318       │  │  :4320          │  │
│  └──────┬──────┘  └──────┬──────┘  └────────┬────────┘  │
│         │                │                   │           │
│         └────────┬───────┘                   │           │
│                  ▼                           │           │
│         ┌────────────────┐                   │           │
│         │   Enrichment   │                   │           │
│         │ cost, tok/s,   │                   │           │
│         │ anomaly flags  │                   │           │
│         └───────┬────────┘                   │           │
│                 ▼                            │           │
│  ┌──────────────────────────────┐            │           │
│  │           Store              │◄───────────┘           │
│  │  ┌──────────┐ ┌───────────┐ │                         │
│  │  │   Ring   │ │  SQLite   │ │──► Broadcast (WS)       │
│  │  │  Buffer  │ │   WAL     │ │                         │
│  │  └──────────┘ └───────────┘ │                         │
│  └──────────────────────────────┘                        │
│                                                          │
│  ┌─────────────┐ ┌─────────────┐ ┌──────────────┐       │
│  │  MCP Server │ │  Web UI     │ │  TUI (dash)  │       │
│  │  stdio      │ │  templ+htmx │ │  bubbletea   │       │
│  └─────────────┘ └─────────────┘ └──────────────┘       │
└──────────────────────────────────────────────────────────┘
```

## Data Model

### SpanRecord (flattened for storage)

| Field | Type | Source |
|-------|------|--------|
| trace_id | string | OTLP |
| span_id | string | OTLP |
| parent_span_id | string | OTLP |
| name | string | OTLP |
| kind | int | OTLP (0=unspecified, 1=internal, 2=server, 3=client, 4=producer, 5=consumer) |
| start_time_unix_nano | uint64 | OTLP |
| end_time_unix_nano | uint64 | OTLP |
| attributes_json | string | OTLP attributes serialized |
| resource_json | string | OTLP resource attributes serialized |
| status_code | int | OTLP (0=unset, 1=ok, 2=error) |
| status_message | string | OTLP |
| ai_system | string? | Extracted from `gen_ai.system` |
| ai_model | string? | Extracted from `gen_ai.request.model` or `gen_ai.response.model` |
| ai_input_tokens | int64? | Extracted from `gen_ai.usage.input_tokens` |
| ai_output_tokens | int64? | Extracted from `gen_ai.usage.output_tokens` |
| agent_name | string? | Extracted from `agent.name` |
| agent_task_id | string? | Extracted from `agent.task_id` |
| agent_session_id | string? | Extracted from `agent.session_id` |
| cost_usd | float64 | Computed on ingest |
| tokens_per_sec | float64 | Computed on ingest |
| anomaly | string | Computed on ingest |
| inserted_at | datetime | Server-side timestamp |

### MetricRollup

```sql
CREATE TABLE metric_rollups (
  name         TEXT    NOT NULL,
  labels_json  TEXT    NOT NULL DEFAULT '{}',
  bucket_start INTEGER NOT NULL,  -- unix seconds, aligned to interval
  bucket_width INTEGER NOT NULL,  -- 60, 3600, or 86400
  metric_type  TEXT    NOT NULL,  -- 'sum', 'gauge', 'histogram'
  count        INTEGER NOT NULL DEFAULT 0,
  sum          REAL    NOT NULL DEFAULT 0,
  min          REAL    NOT NULL DEFAULT 0,
  max          REAL    NOT NULL DEFAULT 0,
  last         REAL    NOT NULL DEFAULT 0,
  PRIMARY KEY (name, labels_json, bucket_start, bucket_width)
);
```

### AI Semantic Conventions

Follows [OpenTelemetry GenAI semconv](https://opentelemetry.io/docs/specs/semconv/gen-ai/) plus agent extensions:

| Span Name | Attributes | Example |
|-----------|-----------|---------|
| `gen_ai.chat_completion` | `gen_ai.system`, `gen_ai.request.model`, `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens` | Claude API call |
| `gen_ai.tool_call` | `gen_ai.tool.name`, `gen_ai.tool.call_id` | Bash, Read, Edit |
| `agent.session` | `agent.name`, `agent.session_id`, `agent.task_id`, `agent.repo` | bosun lifecycle |
| `agent.step` | `agent.step.type` (claim, work, pr, reset) | Single work unit |
| `agent.sandbox` | `container.id`, `container.image` | Spawned via skiff |

### Session Grouping

Sessions are grouped by `agent.session_id` only. Non-agent traces have no session grouping.

## Enrichment

Applied automatically on ingest:

### Cost Calculation

Lookup model in pricing table, compute: `(input_tokens * input_price + output_tokens * output_price) / 1M`

| Model | Input $/1M | Output $/1M |
|-------|-----------|------------|
| claude-opus-4 | 15.00 | 75.00 |
| claude-sonnet-4 | 3.00 | 15.00 |
| claude-haiku-4-5 | 0.80 | 4.00 |
| gpt-4o | 2.50 | 10.00 |
| gpt-4.1 | 2.00 | 8.00 |
| o3 | 10.00 | 40.00 |

### Token Throughput

`output_tokens / duration_seconds` — computed per span.

### Anomaly Detection

| Condition | Flag |
|-----------|------|
| `status_code == 2` | `error: {message}` |
| duration > 10 min | `long_duration: {N}s` |
| output_tokens > 100k | `high_output_tokens: {N}` |
| AI span with 0 output tokens (>1s) | `zero_output_tokens` |
| AI span with <5 tok/s (>5s) | `low_throughput: {N} tok/s` |

## Storage

### Ring Buffer

- Fixed capacity (default 10,000 spans)
- `sync.RWMutex` protected
- Write overwrites oldest on full
- Read returns most recent N in reverse chronological order
- Used by TUI dashboard and `/v1/recent` for zero-query-latency reads

### SQLite

- WAL journal mode, busy timeout 5s
- `spans` table with indexes on: trace_id, start_time, ai_model, agent_name, agent_session_id, status_code
- `metric_rollups` table with composite primary key for upsert aggregation
- Pure Go via `modernc.org/sqlite` (no CGO required)
- Configurable retention (default 7 days, hourly cleanup)

## Analytics API

All endpoints on `:4320`. All return JSON.

### Traces

| Method | Path | Description |
|--------|------|-------------|
| GET | `/v1/traces` | List spans (filter: agent, model, since, until, status, duration_gt, cost_gt, sort_by, limit) |
| GET | `/v1/traces/{trace_id}` | All spans for a trace (span tree) |
| GET | `/v1/recent` | Recent spans from ring buffer |

### Sessions

| Method | Path | Description |
|--------|------|-------------|
| GET | `/v1/sessions` | Agent sessions with aggregated stats (filter: agent, since, limit) |
| GET | `/v1/sessions/{session_id}` | All spans in a session, grouped by trace |

### Stats

| Method | Path | Description |
|--------|------|-------------|
| GET | `/v1/stats` | Aggregate stats (total spans, traces, tokens, cost, sessions) |
| GET | `/v1/stats/by-model` | Stats grouped by model |
| GET | `/v1/stats/by-agent` | Stats grouped by agent |
| GET | `/v1/stats/cost` | Cost report (bucket=hour|day, since, group_by=model|agent) |

### Metrics

| Method | Path | Description |
|--------|------|-------------|
| GET | `/v1/metrics/names` | List distinct metric names |
| GET | `/v1/metrics/{name}` | Query rollups (since, bucket=1m|1h|1d, labels) |

### Anomalies

| Method | Path | Description |
|--------|------|-------------|
| GET | `/v1/anomalies` | Anomalous spans (filter: agent, since, limit) |

### Live + Health

| Method | Path | Description |
|--------|------|-------------|
| WS | `/v1/live` | WebSocket stream of enriched spans |
| GET | `/health` | Health check |

## CLI

```
lookout-go <command> [flags]

Commands:
  serve     Start the collector daemon
  query     Query spans, sessions, metrics, stats, anomalies
  mcp       Start MCP server (stdio)
  dash      Launch TUI dashboard

Serve flags:
  --grpc-addr      OTLP gRPC address      (env: LOOKOUT_GRPC_ADDR,   default: 0.0.0.0:4317)
  --http-addr      OTLP HTTP address      (env: LOOKOUT_HTTP_ADDR,   default: 0.0.0.0:4318)
  --api-addr       Analytics API address   (env: LOOKOUT_API_ADDR,    default: 0.0.0.0:4320)
  --db-path        SQLite database path    (env: LOOKOUT_DB_PATH,     default: ~/.lookout/traces.db)
  --ring-size      Ring buffer capacity    (env: LOOKOUT_RING_SIZE,   default: 10000)
  --retention-days Retention period        (env: LOOKOUT_RETENTION_DAYS, default: 7)

Query subcommands:
  traces      --trace-id, --agent, --model, --since, --until, --duration-gt,
              --status, --cost-gt, --sort-by, --limit, --format
  sessions    --agent, --since, --limit, --format
  metrics     --name (required), --since, --bucket, --labels, --format
  stats       --since, --group-by, --format
  anomalies   --since, --agent, --limit, --format

Output formats: table (default) | json | csv
```

## MCP Server

MCP server runs over stdio. Launched via `lookout-go mcp`.

### Query Tools
- `query_traces` — filter spans by agent, model, time range, status, cost
- `query_sessions` — list agent sessions with stats
- `get_session` — all spans in a session with trace grouping
- `get_stats` — aggregate stats, optionally grouped by model/agent
- `get_anomalies` — list anomalous spans
- `query_metrics` — query metric rollups by name and time range

### Analytical Tools
- `analyze_session` — summarize a session: cost, duration, models, tools, errors, tokens
- `compare_models` — cost/performance comparison across models
- `suggest_optimizations` — identify expensive patterns, suggest improvements

### Resources
- `lookout://stats` — live stats summary
- `lookout://sessions/recent` — recent sessions list

## Web UI

Served on `:4320` under `/ui/`. Embedded in the Go binary.

### Pages
- `/ui/` — Dashboard: stats cards, recent activity, cost chart
- `/ui/traces` — Traces list: filterable/sortable table
- `/ui/traces/{trace_id}` — Trace detail: span tree with timing waterfall
- `/ui/sessions` — Sessions list
- `/ui/sessions/{id}` — Session detail: all traces, timeline
- `/ui/metrics` — Metrics: name selector, time-series chart
- `/ui/anomalies` — Anomalies list

### Tech
- `github.com/a-h/templ` for type-safe HTML templates
- htmx for dynamic updates
- Minimal CSS (classless or pico.css, embedded)
- No npm, no JS build step — ships in Go binary

## Configuration

### Environment / Flags

All CLI flags have corresponding environment variables (see CLI section).

### skiff.yml entry

```yaml
obs.lookout:
  image: ghcr.io/algonormative/lookout-go
  ports:
    - "4317:4317"
    - "4318:4318"
    - "4320:4320"
  volumes:
    - "~/.lookout:/data"
  env:
    LOOKOUT_DB_PATH: /data/traces.db
```

### Client configuration

Any OTEL SDK exporter pointed at lookout works:

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
export OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
```

## Dependencies

| Package | Purpose |
|---------|---------|
| google.golang.org/grpc | OTLP gRPC server |
| google.golang.org/protobuf | Protobuf runtime |
| go.opentelemetry.io/proto/otlp | OTLP proto definitions (no codegen) |
| modernc.org/sqlite | Pure-Go SQLite driver |
| nhooyr.io/websocket | WebSocket for /v1/live |
| github.com/a-h/templ | Type-safe HTML templates (Web UI) |
| github.com/mark3labs/mcp-go | MCP server framework |
| github.com/charmbracelet/bubbletea | TUI framework (deferred) |
| github.com/charmbracelet/lipgloss | TUI styling (deferred) |

## Phases

| Phase | Tasks | Outcome |
|-------|-------|---------|
| **1 — Core Ingest** | CLI scaffold, SQLite store, OTLP HTTP receiver, AI enrichment, seed script | Accepts traces+metrics, enriches, stores |
| **2 — Query Layer** | Rich CLI query, ring buffer, analytics API, metrics rollups, sessions | Full query + analytics |
| **3 — Interfaces** | MCP server, Web UI, WebSocket live stream | External integrations |
| **4 — Ops + Polish** | OTLP gRPC receiver, Dockerfile + CI, TUI dashboard, push metrics | Production-ready |
