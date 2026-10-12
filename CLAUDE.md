# CLAUDE.md

## Project

**lookout-go** -- OTEL trace collector for AI workflows (Go implementation).

Go 1.24, stdlib + minimal deps. Accepts OpenTelemetry traces and metrics via
gRPC and HTTP, stores in SQLite, serves an analytics API. AI-aware: enriches
spans with cost calculation, token throughput, and anomaly detection.

## Commands

```bash
templ generate ./internal/web/           # generate templ -> Go (run after editing .templ files)
go build -o bin/lookout ./cmd/lookout    # build
go build -o bin/seed ./cmd/seed          # build seed script
go test ./...                             # test all
bin/lookout serve                         # start collector
bin/lookout query traces --limit 5        # query recent spans
bin/lookout query stats                   # aggregate stats
bin/lookout query sessions                # agent sessions
bin/lookout mcp                           # start MCP server (stdio)
curl 'localhost:4320/v1/trace-list?service=nanoclaw-agent&root_name=claude_code.interaction&missing_score=promise_only:jev&order=asc&limit=50'
                                          # judge loop: unscored traces -> GET /v1/traces/{id} -> judge -> POST /v1/scores
bin/lookout dash                          # launch TUI dashboard
bin/seed --sessions 5 --traces 10         # seed test data
```

## Layout

```
cmd/lookout/main.go              # CLI entry point (serve, query, mcp)
cmd/seed/main.go                 # Test data generator (OTLP sender)
internal/
  config/config.go               # Shared configuration struct
  store/
    models.go                    # SpanRecord, MetricRollup, Session structs
    store.go                     # Store interface
    ring.go                      # In-memory ring buffer
    sqlite.go                    # SQLite implementation (schema, migrations, span queries)
    scores.go                    # Score model, validation, upsert/query, cursors
    tracelist.go                 # Trace read model (QueryTraceList) built from spans at query time
    backfill.go                  # One-time batched backfill: service_name, session ids
  otlp/
    http.go                      # OTLP HTTP receiver (traces + metrics)
    grpc.go                      # OTLP gRPC receiver
    convert.go                   # Proto -> internal model conversion (applies Filter)
    filter.go                    # Ingest drop list, attribute cap, slim-span rule
    metrics.go                   # OTLP metrics -> rollup conversion
  web/
    handlers.go                  # Web UI HTTP handlers (served at /)
    layout.templ                 # Base HTML layout (pico.css + htmx)
    dashboard.templ              # Dashboard page (stats + recent spans)
    traces.templ                 # Traces list + trace detail
    sessions.templ               # Sessions list + session detail
    anomalies.templ              # Anomalies page
    components.templ             # Shared components (span table, badges)
  mcp/
    server.go                    # MCP server (stdio) — tools + resources
  tui/
    tui.go                       # TUI dashboard (bubbletea + lipgloss)
  ai/
    semantic.go                  # GenAI semantic convention constants
    pricing.go                   # Model pricing table
    enrichment.go                # Cost calc, token throughput, anomaly detection
  api/
    server.go                    # Analytics HTTP API (net/http)
    scores.go                    # /v1/scores, /api/public/scores (Langfuse alias)
    tracelist.go                 # /v1/trace-list
    ws.go                        # WebSocket live stream
  cli/
    query.go                     # CLI query command implementation
    format.go                    # table/json/csv output formatting
```

## Key Design

- Dual ingest: OTLP gRPC (:4317) + HTTP (:4318), analytics API on :4320
- Dual storage: in-memory ring buffer (hot) + SQLite WAL (persistent)
- AI enrichment: auto-extracts gen_ai.* and agent.* semantic attributes
- Cost calculation: model + tokens -> USD using built-in pricing table
- Metrics rollups: OTLP metrics aggregated into 1m/1h/1d buckets on ingest
- Sessions: grouped by `agent_session_id`, populated from `agent.session_id`, else `session.id` (Claude Code), else `langfuse.session.id` (span attrs, then resource attrs)
- Scores + judges: judges are separate processes that poll `GET /v1/trace-list?...&missing_score=name:source` and post verdicts to `POST /v1/scores` (lookout never runs a judge). `scores` table upserts on id and is NOT touched by retention cleanup. Langfuse-shaped aliases at `/api/public/scores`. MCP: `score_trace`, `get_scores`, `query_trace_list`
- Trace read model: no `traces` table; `QueryTraceList` is a query over `spans` (keyset on trace start + trace_id, trace-level predicates as `trace_id IN (...)`, summaries built in Go for the page). `spans.service_name` (indexed) comes from resource `service.name`. Root = earliest span with no parent or a parent absent from the trace
- `GET /v1/traces/{id}` stays a bare span array; `?include=scores` returns `{trace_id, spans, scores}`
- Migrations: `ALTER TABLE ... ADD COLUMN` in `migrate()`; row-level backfills run in the background (`startBackfill`, marker in the `meta` table), batched by rowid window so single-connection ingest interleaves. Tests call `WaitBackfill()`
- Zero-config: sensible defaults, all options via CLI flags or env vars
- Retention: automatic cleanup of spans older than N days; when rows were deleted, a full VACUUM runs at most once per 24h to shrink the file
- Ingest volume (config env vars, all also `serve` flags): `LOOKOUT_DROP_SPANS` (default llm-proxy plumbing: postgres, auth, self, proxy_pre_call, batch_write_to_db, router; empty disables), `LOOKOUT_MAX_ATTR_BYTES` (default 32768; 0 disables), `LOOKOUT_SLIM_SPANS` (default `llm-proxy:raw_gen_ai_request`) + `LOOKOUT_SLIM_ATTR_BYTES` (default 2048). Pairs are `service.name:span name`. Applied in `otlp.ConvertTraceRequest` so HTTP and gRPC behave the same.
- Web UI: templ + htmx on :4320/ — pico.css dark theme, htmx polling for live updates
- Proto: uses go.opentelemetry.io/proto/otlp package (no codegen)

## Dependencies

- go.opentelemetry.io/proto/otlp — OTLP proto definitions
- google.golang.org/protobuf — protobuf runtime
- modernc.org/sqlite — pure-Go SQLite (no CGO)
- github.com/a-h/templ — type-safe HTML templates (run `templ generate` after editing .templ files)
- github.com/mark3labs/mcp-go — MCP protocol server (stdio)
- github.com/charmbracelet/bubbletea — TUI framework
- github.com/charmbracelet/lipgloss — TUI styling
- nhooyr.io/websocket — WebSocket support

<!-- br-agent-instructions-v1 -->

---

## Beads Workflow Integration

This project uses [beads_rust](https://github.com/Dicklesworthstone/beads_rust) (`br`/`bd`) for issue tracking. Issues are stored in `.beads/` and tracked in git.

### Essential Commands

```bash
# View ready issues (unblocked, not deferred)
br ready              # or: bd ready

# List and search
br list --status=open # All open issues
br show <id>          # Full issue details with dependencies
br search "keyword"   # Full-text search

# Create and update
br create --title="..." --description="..." --type=task --priority=2
br update <id> --status=in_progress
br close <id> --reason="Completed"
br close <id1> <id2>  # Close multiple issues at once

# Sync with git
br sync --flush-only  # Export DB to JSONL
br sync --status      # Check sync status
```

### Workflow Pattern

1. **Start**: Run `br ready` to find actionable work
2. **Claim**: Use `br update <id> --status=in_progress`
3. **Work**: Implement the task
4. **Complete**: Use `br close <id>`
5. **Sync**: Always run `br sync --flush-only` at session end

### Key Concepts

- **Dependencies**: Issues can block other issues. `br ready` shows only unblocked work.
- **Priority**: P0=critical, P1=high, P2=medium, P3=low, P4=backlog (use numbers 0-4, not words)
- **Types**: task, bug, feature, epic, chore, docs, question
- **Blocking**: `br dep add <issue> <depends-on>` to add dependencies

### Session Protocol

**Before ending any session, run this checklist:**

```bash
git status              # Check what changed
git add <files>         # Stage code changes
br sync --flush-only    # Export beads changes to JSONL
git commit -m "..."     # Commit everything
git push                # Push to remote
```

### Best Practices

- Check `br ready` at session start to find available work
- Update status as you work (in_progress → closed)
- Create new issues with `br create` when you discover tasks
- Use descriptive titles and set appropriate priority/type
- Always sync before ending session

<!-- end-br-agent-instructions -->

<!-- bv-agent-instructions-v1 -->

---

## Beads Workflow Integration

This project uses [beads_viewer](https://github.com/Dicklesworthstone/beads_viewer) for issue tracking. Issues are stored in `.beads/` and tracked in git.

### Essential Commands

```bash
# View issues (launches TUI - avoid in automated sessions)
bv

# CLI commands for agents (use these instead)
bd ready              # Show issues ready to work (no blockers)
bd list --status=open # All open issues
bd show <id>          # Full issue details with dependencies
bd create --title="..." --type=task --priority=2
bd update <id> --status=in_progress
bd close <id> --reason="Completed"
bd close <id1> <id2>  # Close multiple issues at once
bd sync               # Commit and push changes
```

### Workflow Pattern

1. **Start**: Run `bd ready` to find actionable work
2. **Claim**: Use `bd update <id> --status=in_progress`
3. **Work**: Implement the task
4. **Complete**: Use `bd close <id>`
5. **Sync**: Always run `bd sync` at session end

### Key Concepts

- **Dependencies**: Issues can block other issues. `bd ready` shows only unblocked work.
- **Priority**: P0=critical, P1=high, P2=medium, P3=low, P4=backlog (use numbers, not words)
- **Types**: task, bug, feature, epic, question, docs
- **Blocking**: `bd dep add <issue> <depends-on>` to add dependencies

### Session Protocol

**Before ending any session, run this checklist:**

```bash
git status              # Check what changed
git add <files>         # Stage code changes
bd sync                 # Commit beads changes
git commit -m "..."     # Commit code
bd sync                 # Commit any new beads changes
git push                # Push to remote
```

### Best Practices

- Check `bd ready` at session start to find available work
- Update status as you work (in_progress → closed)
- Create new issues with `bd create` when you discover tasks
- Use descriptive titles and set appropriate priority/type
- Always `bd sync` before ending session

<!-- end-bv-agent-instructions -->
