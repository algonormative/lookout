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

# Full-text search (prompts, replies, errors)
curl "localhost:4320/v1/search?q=deploy&since=24h"

# Compress spans stored before compression existed (manual; see Configuration)
lookout compact

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
| `GET /v1/trace-list?service=&root_name=&user=&trace_name=&attr=key=value&since=&until=&missing_score=name:source&has_score=name:source&order=asc\|desc&cursor=&limit=` | `{"traces": [...], "next_cursor": "..."}`; default newest first by trace start. `attr`, `missing_score`, `has_score` may repeat (AND). A bare `name` matches any source. Existing `/v1/traces` is unchanged |
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
| `--max-db-mb` | `LOOKOUT_MAX_DB_MB` | `1024` | Cap on used DB size (`(page_count - freelist_count) * page_size`). Checked after the hourly retention cleanup; above it, whole traces are deleted oldest-first until under 90% of the cap, then a `VACUUM` runs regardless of the daily throttle. Scores are never deleted. `0` disables |
| `--archive-dir` | `LOOKOUT_ARCHIVE_DIR` | _(empty)_ | Directory that receives every span before retention or the cap deletes it, as `spans-YYYY-MM-DD.jsonl.gz` (see [Archive format](#archive-format)). Empty disables |
| `--compress-min-bytes` | `LOOKOUT_COMPRESS_MIN_BYTES` | `4096` | Spans whose attributes+events JSON exceed this keep large values in a compressed side table (see [Payload compression](#payload-compression)). `0` disables |
| _(none)_ | `LOOKOUT_INGEST_KEY` | _(empty)_ | When set, `POST /api/public/otel/v1/traces` requires it as the HTTP Basic password or a Bearer token. Empty accepts anything. `/v1/traces` and gRPC are not affected |

### Migration on upgrade

New in this version: `span_payloads`, the search index (`span_fts`, `span_search`) and `spans.user_id/trace_name/obs_type` (instant `ALTER`s plus two partial indexes). Existing rows are filled by background batched passes (search index, identity columns), each resumable and marked in the `meta` table. On a ~260 MB database expect roughly 10-30 s of background work in total. Old rows are not compressed until you run `lookout compact` (expected ~30 s for ~3,500 large spans, shrinking the file to roughly 40-50% after a `VACUUM`).

On first start after upgrading, `spans.service_name` is added (instant) and indexed (one table scan), then a background pass fills `service_name` from the stored resource and, where `agent_session_id` is empty, derives it from `session.id` / `langfuse.session.id`. It works in 2000-row batches with a short pause between them, so ingest is never blocked for long (about 5 s of background work per 300k spans / 350 MB in testing). It is idempotent and resumes if interrupted; a `backfill:` log line reports completion.

### Ingest volume controls

- **Drop list.** A span whose resource `service.name` and span name match a pair is not stored. Children of a dropped span are re-parented to the nearest kept ancestor when the dropped span(s) are in the same OTLP export request; if the dropped span was exported in a different request, the child keeps its original (now dangling) parent id.
- **Attribute cap.** Applies to span and event attributes (not resource attributes), including strings nested in arrays/maps. If an over-cap string is a JSON array (e.g. `gen_ai.input.messages`), elements are dropped from the front so the newest items are kept, with a leading `{"role":"system","content":"[lookout: N earlier items truncated, M bytes]"}` marker; the result stays valid JSON.
- **Size cap.** See `LOOKOUT_MAX_DB_MB` above. Deletes whole traces oldest-first, so no trace is left half-present.
- **Disk space.** SQLite never shrinks its file on `DELETE`. When the hourly retention cleanup deletes spans, lookout runs a full `VACUUM` (then truncates the WAL), at most once per 24 hours per process. It holds the write lock for the duration, so ingest briefly waits; free disk equal to the DB size is needed while it runs.

### Archive format

With `LOOKOUT_ARCHIVE_DIR` set, spans are written out before they are deleted (by retention or the size cap). Order is write, fsync, delete: if the write fails nothing is deleted and the cleanup logs the error and retries next hour.

- One file per UTC day of the span's start time: `spans-YYYY-MM-DD.jsonl.gz`.
- Each cleanup batch is appended as one gzip member, so the file is a valid multi-member gzip stream (`zcat`, `gzip.NewReader` and `rclone` handle it as-is).
- One JSON object per line: the same span JSON `GET /v1/traces/{id}` returns, with `attributes_json` and `events_json` fully expanded (compression is undone).
- Delivery is at-least-once: if a delete fails after its batch was archived, the next run archives those spans again. Consumers dedupe on `span_id`.
- Closed days are immutable except for late appends, so an external uploader (for example `rclone copy "$LOOKOUT_ARCHIVE_DIR" r2:bucket/lookout`, or sync-then-delete files older than a day) can pick them up. Lookout does not upload anything itself.

### Payload compression

About 99% of stored bytes are in the 30% of spans larger than 4 KB (LLM message payloads, images). Spans over `LOOKOUT_COMPRESS_MIN_BYTES` keep a "lite" `attributes_json` in `spans`, where every string value over 1 KB becomes `{"$lookout_ref":"payload","bytes":N}`, and store the exact original attributes/events flate-compressed in `span_payloads(span_id, codec, data)`. `json_extract` filters (`attr=key=value`) work on the lite column for small keys; a filter on a large value no longer matches. Every read path (API, MCP, CLI, web, archive) returns the full original values; deleting spans deletes their payload rows. Measured ratio on real data: about 2.3x.

New spans are compressed as they arrive. To convert rows stored earlier run, once and by hand:

```bash
lookout compact [--db-path ...] [--compress-min-bytes 4096] [--vacuum]
```

It works in 100-row transactions with a short pause between them, so it can run next to `serve`; it is idempotent and safe to interrupt and re-run. Freed pages are reused by SQLite; the file shrinks at the next `VACUUM` (the server's after a retention delete, or `--vacuum` here, which blocks writers while it rewrites the file and needs free disk equal to the DB size).

### Search

`GET /v1/search?q=&service=&since=&until=&limit=&cursor=` (also the MCP tool `search_traces` and the search box on `/traces`) returns `{"traces": [{trace_id, root_span_name, start_time_unix_nano, snippet, span_id, services, span_count, error}], "next_cursor": ""}`, newest trace first. All terms must match; `"quoted phrase"` and a trailing `*` (prefix) work; other punctuation is literal. `service` means some span of the trace is from that service, like `/v1/trace-list`.

Indexed per span (full-text, contentless FTS5, about 4% of DB size): span name, status message, `error.message`, Claude Code `user_prompt`, the text of `gen_ai.output.messages`, the newest user text and the tool results after it from `gen_ai.input.messages` (system prompts and older history are skipped), and `langfuse.observation.input/output`; at most 16 KB per span. Existing spans are indexed by a background batched pass on first start (resumable, ~2 ms per span; `backfill: indexed ...` log line).

### Langfuse ingest and attribute mapping

A Langfuse OTel exporter can point at lookout unchanged: `POST http://host:4318/api/public/otel/v1/traces` (protobuf or JSON, optionally gzip) is handled like `/v1/traces`. Set `LOOKOUT_INGEST_KEY` to require the key as the Basic password (the Langfuse secret key; the username is ignored) or a Bearer token.

| Attribute (span, then resource) | Stored as |
|---|---|
| `langfuse.session.id` | session (`agent_session_id`), after `agent.session_id` and `session.id` |
| `langfuse.user.id`, else `user.id` | `user_id` (indexed); filter `user=` |
| `langfuse.trace.name` | `trace_name`; filter `trace_name=` |
| `langfuse.observation.type` | `obs_type`; inferred when absent: `generation` if a model is set, `tool` for `claude_code.tool`, else `span` |
| `langfuse.observation.input/output` | kept as attributes; searchable; compressed when large |
| `gen_ai.cost.total_cost`, else `gen_ai.usage.cost` | `cost_usd` when > 0; otherwise the pricing table |

Trace-list rows add `user_id`, `trace_name` and `obs_type` (of the root span). `/v1/traces/{id}/conversation` decodes `gen_ai.input/output.messages` attributes (OTel GenAI parts with `content` or `text` keys, `tool_call`, `tool_result`, and the `[lookout: ... truncated]` marker) in addition to event-based spans; history repeated across a trace's requests appears once and system prompts are omitted.

## Seed Data

Generate test data for development:

```bash
go build -o bin/seed ./cmd/seed
bin/seed --sessions 5 --traces 10
```

## License

MIT
