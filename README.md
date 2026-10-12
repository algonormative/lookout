# Lookout

OTEL trace collector purpose-built for AI agent workflows. Accepts OpenTelemetry traces and metrics via gRPC and HTTP, stores in SQLite, enriches spans with cost calculation and anomaly detection, and serves analytics through a web UI, REST API, MCP server, and TUI dashboard.

## Features

- **Dual OTLP ingest** -- gRPC (`:4317`) and HTTP (`:4318`)
- **AI-aware enrichment** -- extracts `gen_ai.*` and `agent.*` semantic attributes, calculates cost from token counts, detects anomalies
- **Built-in pricing** -- Claude, GPT-4o/4.1, o3/o4 models with per-token cost tracking
- **Session grouping** -- groups spans by `agent.session_id` (falling back to `session.id`, then `langfuse.session.id`) for conversation-level analytics
- **Scores and trace query** -- judge processes poll for unscored traces and post verdicts back as scores (see [Scores and judges](#scores-and-judges))
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
| `query_trace_list` | One row per trace (root, services, duration, tokens, error, scores), filterable incl. `missing_score` |
| `score_trace` | Post a judge verdict (score) on a trace or span |
| `get_scores` | Query scores by trace, span, name, source, time |
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

## Scores and judges

Lookout is the trace sink and search layer; judges are separate processes that poll for traces they have not scored and post verdicts back. Lookout never runs a judge.

A judge's loop:

```bash
# 1. traces this judge has not scored yet, oldest first
curl -s 'localhost:4320/v1/trace-list?service=nanoclaw-agent&root_name=claude_code.interaction&missing_score=promise_only:jev&order=asc&limit=50'

# 2. fetch a trace (spans; add &include=scores for existing verdicts too)
curl -s localhost:4320/v1/traces/<trace_id>

# 3. judge it, post the verdict (idempotent on id; arrays accepted)
curl -s -X POST localhost:4320/v1/scores -d '{
  "id": "<trace_id>:promise_only:jev", "trace_id": "<trace_id>", "name": "promise_only",
  "data_type": "BOOLEAN", "value_num": 1, "source": "jev", "judge_version": "v3", "comment": "..."}'
```

Once scored, the trace drops out of the `missing_score` query. Claude Code roots export last, so filtering on `root_name` also skips traces still in flight; add `until=2m` to leave a margin for late spans.

### Endpoints

| Endpoint | Description |
|----------|-------------|
| `POST /v1/scores` | One score object or an array; upsert on `id` (generated when absent); returns `{"ids": [...]}`; `400` on bad input (nothing from a bad batch is stored) |
| `GET /v1/scores?trace_id=&span_id=&name=&source=&since=&order=&cursor=&limit=` | `{"scores": [...], "next_cursor": "..."}`, newest first (`order=asc` for oldest first) |
| `GET /v1/trace-list?service=&root_name=&attr=key=value&since=&until=&missing_score=name:source&has_score=name:source&order=asc\|desc&cursor=&limit=` | `{"traces": [...], "next_cursor": "..."}`; default newest first by trace start. `attr`, `missing_score`, `has_score` may repeat (AND). A bare `name` matches any source. Existing `/v1/traces` is unchanged |
| `GET /v1/traces/{id}?include=scores` | Default body is still the span array; with `include=scores` it is `{"trace_id", "spans": [...], "scores": [...]}` |
| `POST /api/public/scores` | Langfuse-shaped: `id, traceId, observationId, sessionId, name, value, dataType, comment, configId, metadata`. `source` comes from `metadata.source` (default `api`); `dataType` is inferred from `value` when absent; `configId` is kept in `metadata.configId` |
| `GET /api/public/scores?page=&limit=&name=&traceId=&observationId=&sessionId=&dataType=&source=` | `{"data": [...], "meta": {"page", "limit", "totalItems", "totalPages"}}` |

Score fields: `id`, `trace_id`, `span_id` (empty = trace-level), `session_id` (copied from the trace when absent), `name`, `data_type` (`NUMERIC`, `BOOLEAN` with `value_num` 0/1, `CATEGORICAL` with `value_str`; a `CATEGORICAL` score may also carry a probability in `value_num`), `source` (who judged), `judge_version`, `comment`, `metadata`, `created_at` (unix ns, kept when an id is re-posted). Scores are not deleted when their spans age out.

`since`/`until` take `15m`, `1h`, `7d` or RFC3339; a bad value is a `400`. Cursors are opaque; pass `next_cursor` back unchanged.

Trace rows carry `trace_id, root_span_name, root_span_id, services, start_time_unix_nano, end_time_unix_nano, duration_ms, span_count, models, input_tokens, output_tokens, total_tokens, error, session_id, scores`. The root is the earliest-starting span with no parent or a parent absent from the trace, so a LiteLLM (`llm-proxy`) span joined under a `claude_code.interaction` span never becomes the root. `root_name` matches any such root-like span. `attr` compares the span attribute's text form. Pass `since` on large databases: it lets the start-time index prune the scan.

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
| `agent.session_id` | Session identifier for grouping (else `session.id`, else `langfuse.session.id`) |

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

### Migration on upgrade

On first start after upgrading, `spans.service_name` is added (instant) and indexed (one table scan), then a background pass fills `service_name` from the stored resource and, where `agent_session_id` is empty, derives it from `session.id` / `langfuse.session.id`. It works in 2000-row batches with a short pause between them, so ingest is never blocked for long (about 5 s of background work per 300k spans / 350 MB in testing). It is idempotent and resumes if interrupted; a `backfill:` log line reports completion.

### Ingest volume controls

- **Drop list.** A span whose resource `service.name` and span name match a pair is not stored. Children of a dropped span are re-parented to the nearest kept ancestor when the dropped span(s) are in the same OTLP export request; if the dropped span was exported in a different request, the child keeps its original (now dangling) parent id.
- **Attribute cap.** Applies to span and event attributes (not resource attributes), including strings nested in arrays/maps. If an over-cap string is a JSON array (e.g. `gen_ai.input.messages`), elements are dropped from the front so the newest items are kept, with a leading `{"role":"system","content":"[lookout: N earlier items truncated, M bytes]"}` marker; the result stays valid JSON.
- **Disk space.** SQLite never shrinks its file on `DELETE`. When the hourly retention cleanup deletes spans, lookout runs a full `VACUUM` (then truncates the WAL), at most once per 24 hours per process. It holds the write lock for the duration, so ingest briefly waits; free disk equal to the DB size is needed while it runs.

## Seed Data

Generate test data for development:

```bash
go build -o bin/seed ./cmd/seed
bin/seed --sessions 5 --traces 10
```

## License

MIT
