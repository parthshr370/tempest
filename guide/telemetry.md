# Telemetry

Tempest exports OpenTelemetry traces and metrics for agent runs. Export is OTLP over
HTTP with JSON encoding, written with the Go standard library. `serve -listen` also
serves the metrics as Prometheus text at `GET /metrics`.

Spans and metrics come from the agent event stream that also feeds the journal and
ACP/A2A clients. No second instrumentation path exists, so a trace shows what the
journal records.

Export never slows a run. Finished spans go into a bounded queue. When the queue is
full, spans are dropped and counted, not waited on. Network I/O runs on exporter
goroutines. At exit the final flush waits at most 5 seconds.

## Enabling export

Point tempest at an OTLP/HTTP collector:

```sh
export OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318
./bin/tempest -p "summarize README.md"
```

Traces go to `<endpoint>/v1/traces` and metrics to `<endpoint>/v1/metrics`. An OTel
Collector or Jaeger (`jaegertracing/all-in-one`, OTLP/HTTP on port 4318) both accept
this directly.

A one-shot run with no endpoint exports nothing and keeps no metrics.
`serve -listen` always keeps metrics for `/metrics` unless telemetry is disabled.

## Settings

Each setting has a key under `otel` in `settings.json` (see
[Configuration](configuration.md)) and a standard `OTEL_*` variable as its
environment input.

| Setting | Environment variable | Default | Meaning |
|---|---|---|---|
| `otel.disabled` | `OTEL_SDK_DISABLED` | `false` | `true` turns off all export and `/metrics`. |
| `otel.endpoint` | `OTEL_EXPORTER_OTLP_ENDPOINT` | none | Base URL; `/v1/traces` and `/v1/metrics` are appended. |
| `otel.traces_endpoint` | `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | from `endpoint` | Full traces URL, used as given. |
| `otel.metrics_endpoint` | `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | from `endpoint` | Full metrics URL, used as given. |
| `otel.protocol` | `OTEL_EXPORTER_OTLP_PROTOCOL` | `http/json` | Only `http/json` is supported. Any other value is a startup error. |
| `otel.traces_protocol` | `OTEL_EXPORTER_OTLP_TRACES_PROTOCOL` | `http/json` | Same rule, traces only. |
| `otel.metrics_protocol` | `OTEL_EXPORTER_OTLP_METRICS_PROTOCOL` | `http/json` | Same rule, metrics only. |
| `otel.headers` | `OTEL_EXPORTER_OTLP_HEADERS` | none | Request headers, such as an API key. The variable uses `key=value,key=value` with percent-encoded values. |
| `otel.compression` | `OTEL_EXPORTER_OTLP_COMPRESSION` | `none` | `gzip` or `none`. |
| `otel.timeout_ms` | `OTEL_EXPORTER_OTLP_TIMEOUT` | `10000` | Per-request timeout in milliseconds. |
| `otel.service_name` | `OTEL_SERVICE_NAME` | `tempest` | `service.name` resource attribute. |
| `otel.resource_attributes` | `OTEL_RESOURCE_ATTRIBUTES` | none | Extra resource attributes, in `key=value,...` form. |
| `otel.capture_message_content` | `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT` | `false` | Adds prompts, responses, tool arguments and tool results to spans. |
| `otel.metric_export_interval_ms` | `OTEL_METRIC_EXPORT_INTERVAL` | `60000` | How often metrics are pushed over OTLP. |

Endpoints must be absolute `http` or `https` URLs. An error message shows the URL
with its userinfo and query removed.

A project `.harness/settings.json` can send spans, header credentials and message
content to a server of its choosing. For that reason, project `otel` settings apply
only under provider trust: `-trust-project-providers`,
`HARNESS_TRUST_PROJECT_PROVIDERS=true`, or the project path in user
`trusted_provider_projects`. User settings, environment variables and flags always
apply.

## Span tree

The span shape follows the OpenTelemetry GenAI semantic conventions and the Pydantic
AI instrumentation (format 5):

```
invoke_agent <agent>              one per run (internal)
├── chat <model>                  one per model request (client)
│   └── chat <model>              a failed attempt that was retried
├── execute_tool <tool>           one per tool call (internal)
│   └── invoke_agent <subagent>   a task child's run, under the tool that started it
└── chat <model>
```

`chat` and `execute_tool` are siblings under the run. A tool span is never a child of
a request span, because the request span has already ended when the tool runs. A
failed attempt is a child of the request it belongs to, so the request span covers
its retries.

The run span carries the run totals as `gen_ai.aggregated_usage.*`. Request spans
carry `gen_ai.usage.*`. The two namespaces are kept apart, so summing
`gen_ai.usage.*` over a trace counts each request once.

### Attributes

| Span | Attributes |
|---|---|
| `invoke_agent` | `gen_ai.operation.name`, `gen_ai.agent.name` (`tempest`, or the subagent type), `gen_ai.conversation.id` (session id), `gen_ai.agent.call.id` (run id), `tempest.run.stop_reason`, `tempest.run.turns`, `gen_ai.aggregated_usage.{input_tokens,output_tokens,cache_read.input_tokens,cache_creation.input_tokens}`, `tempest.cost.usd` or `tempest.cost.unknown`, and `error.type` when the run ends in `error`, `output_invalid` or `repeated_failure`. |
| `chat` (request) | `gen_ai.operation.name`, `gen_ai.provider.name`, `gen_ai.request.model`, `gen_ai.request.max_tokens`, `gen_ai.request.temperature`, `tempest.request.reasoning`, `server.address`, `server.port`, `gen_ai.response.model`, `gen_ai.response.id`, `gen_ai.response.finish_reasons`, `gen_ai.usage.{input_tokens,output_tokens,cache_read.input_tokens,cache_creation.input_tokens}`, `tempest.cost.usd` or `tempest.cost.unknown`, and `error.type` on failure (`incomplete` when the run ended mid-request). |
| `chat` (failed attempt) | `gen_ai.operation.name`, `gen_ai.provider.name`, `gen_ai.request.model`, `error.type`, `tempest.attempt`, and `tempest.attempt.outcome` (`retry`). |
| `execute_tool` | `gen_ai.operation.name`, `gen_ai.tool.name`, `gen_ai.tool.call.id`, `tempest.permission.decision` (`allow`, `deny`), `tempest.permission.source`, `tempest.tool.outcome` (`success`, `error`), and `error.type` (`tool_error`, or `incomplete` when the run ended first). |

Failed spans have status `ERROR`. Their status message is redacted and cut to 1024
characters.

### Message content

Content capture is off by default. With `otel.capture_message_content`, request spans
also carry `gen_ai.system_instructions`, `gen_ai.input.messages` and
`gen_ai.output.messages`, in the GenAI conventions' JSON message format. Tool spans
also carry `gen_ai.tool.call.arguments` and `gen_ai.tool.call.result`. Each value is
cut at 64 KiB, and redacted reasoning blocks are left out. Content is encoded only
when a span is exported, not on the turn loop.

## Context propagation

Tempest uses W3C Trace Context (`traceparent`):

- **Incoming.** A `traceparent` header on `serve` requests becomes the run's parent.
  This covers the `/acp` WebSocket upgrade, `/a2a`, and a prompt's
  `_meta.traceparent` in ACP, which wins over the upgrade header.
- **Outgoing.** Provider HTTP requests and MCP Streamable HTTP requests send the
  current request span as their `traceparent` header.
- **Subprocesses.** Hook commands and `bash` tool commands get `TRACEPARENT` in their
  environment.

When a remote parent arrives with the sampled flag off, context is still propagated,
but its spans are not exported.

## Metrics

Metrics are aggregated in memory with cumulative temporality. They are pushed over
OTLP every `otel.metric_export_interval_ms`, flushed once more at exit, and served at
`/metrics`. Attribute values are always strings.

| OTLP name | Prometheus name | Type | Unit | Attributes |
|---|---|---|---|---|
| `gen_ai.client.token.usage` | `gen_ai_client_token_usage` | histogram | `{token}` | `gen_ai.operation.name`, `gen_ai.provider.name`, `gen_ai.request.model`, `gen_ai.response.model`, `gen_ai.token.type` (`input`, `output`) |
| `gen_ai.client.operation.duration` | `gen_ai_client_operation_duration_seconds` | histogram | `s` | as above without `gen_ai.token.type`, plus `error.type` on failure; retries included |
| `gen_ai.client.operation.time_to_first_chunk` | `gen_ai_client_operation_time_to_first_chunk_seconds` | histogram | `s` | `gen_ai.operation.name`, `gen_ai.provider.name`, `gen_ai.request.model` |
| `gen_ai.client.operation.cost` | `gen_ai_client_operation_cost_usd` | histogram | `USD` | as for duration; requests with an unknown price are not recorded |
| `tempest.agent.runs` | `tempest_agent_runs_total` | counter | `{run}` | `gen_ai.agent.name`, `tempest.run.stop_reason` |
| `tempest.tool.calls` | `tempest_tool_calls_total` | counter | `{call}` | `gen_ai.tool.name`, `tempest.tool.outcome` |
| `tempest.model.failed_attempts` | `tempest_model_failed_attempts_total` | counter | `{attempt}` | `gen_ai.provider.name`, `gen_ai.request.model`, `error.type`, `tempest.attempt.outcome` (`retry`) |
| `tempest.permission.decisions` | `tempest_permission_decisions_total` | counter | `{decision}` | `gen_ai.tool.name`, `tempest.permission.decision`, `tempest.permission.source` |
| `tempest.hook.runs` | `tempest_hook_runs_total` | counter | `{run}` | `tempest.hook.event`, `tempest.hook.outcome` (`success`, `blocked` for exit 2, `failed`, `error`, `timeout`) |
| `tempest.sessions` | `tempest_sessions` | gauge | `{session}` | none; open root sessions |
| `tempest.jobs` | `tempest_jobs` | gauge | `{job}` | none; background jobs not yet finished, across open sessions |
| `tempest.telemetry.spans.lost` | `tempest_telemetry_spans_lost_total` | counter | `{span}` | `reason` (`queue_full`, `export_failed`); present only when traces are exported |

The token and duration buckets are the boundaries the GenAI conventions recommend.
Prometheus names follow the OpenTelemetry compatibility rules: dots become
underscores, `s` and `USD` units become `_seconds` and `_usd` suffixes, and counters
end in `_total`. Label names get the same character mapping.

### `/metrics`

`tempest serve -listen` mounts `GET /metrics` (and `HEAD`), which returns the
Prometheus text format 0.0.4. It is behind the same bearer token as `/acp` and
`/a2a`: when a token is configured, a scrape without
`Authorization: Bearer <token>` gets `401`. The listen line names the route:
`tempest serve listening on http://127.0.0.1:7777 (ACP /acp, A2A /a2a, metrics /metrics)`.

A Prometheus scrape job for a token-protected server:

```yaml
scrape_configs:
  - job_name: tempest
    authorization:
      credentials: change-me
    static_configs:
      - targets: ["127.0.0.1:7777"]
```

## Export behaviour

| Item | Value |
|---|---|
| Span queue | 2048 spans; when full, new spans are dropped and counted in `tempest.telemetry.spans.lost{reason="queue_full"}` |
| Span batch | up to 512 spans, sent at least every 5 seconds |
| Metric push | every `otel.metric_export_interval_ms` (60 s), plus a final push at exit |
| Request timeout | `otel.timeout_ms` (10 s) |
| Exit flush | at most 5 seconds; anything still queued then is dropped |
| Collector failures | the first failure is logged as a warning, later ones at debug level; a success logs recovery once |

## Limits

- OTLP/HTTP with JSON encoding only. gRPC and protobuf are not implemented, and
  asking for them is a startup error.
- No logs signal, and no sampler setting: every local root trace is sampled, and a
  remote parent's sampled flag is respected.
- Metrics are kept per process. A restart starts the cumulative counters again
  from zero, with a new start time.
