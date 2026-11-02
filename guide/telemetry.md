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
Collector with an OTLP/HTTP receiver accepts both.

Jaeger (`jaegertracing/all-in-one`, OTLP/HTTP on port 4318) accepts traces only and
answers `404` to `/v1/metrics`. Give it the traces endpoint alone, and scrape metrics
from `/metrics` or send them to a collector that takes metrics:

```sh
export OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://127.0.0.1:4318/v1/traces
./bin/tempest -p "summarize README.md"
```

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
| `otel.timeout_ms` | `OTEL_EXPORTER_OTLP_TIMEOUT` | `10000` | Timeout of one export in milliseconds, retries included. |
| `otel.service_name` | `OTEL_SERVICE_NAME` | `tempest` | `service.name` resource attribute. |
| `otel.resource_attributes` | `OTEL_RESOURCE_ATTRIBUTES` | none | Extra resource attributes, in `key=value,...` form. |
| `otel.capture_message_content` | `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT` | `false` | Adds prompts, responses, tool arguments and tool results to spans. |
| `otel.metric_export_interval_ms` | `OTEL_METRIC_EXPORT_INTERVAL` | `60000` | How often metrics are pushed over OTLP. |

Endpoints must be absolute `http` or `https` URLs. An error message or log line shows
the URL with its userinfo and query removed, both at startup and when an export
fails.

A project `.harness/settings.json` can send spans, header credentials and message
content to a server of its choosing. For that reason, every project `otel` setting
applies only under provider trust: `-trust-project-providers`,
`HARNESS_TRUST_PROJECT_PROVIDERS=true`, or the project path in user
`trusted_provider_projects`. User settings, environment variables and flags always
apply. A key is matched by its exact spelling: `OTel`, `otel.Headers` or any other
case variant is an unknown setting, ignored with a warning, in every file and in
`-c`.

## Span tree

The span shape follows the OpenTelemetry GenAI semantic conventions as published in
semantic conventions v1.41.0, where they have development status, and the Pydantic
AI instrumentation (format 5). `gen_ai.agent.call.id` and `gen_ai.aggregated_usage.*`
come from Pydantic AI, not from OpenTelemetry:

```
invoke_agent <agent>              one per run (internal)
├── chat <model>                  one per model request (client)
│   └── chat <model>              a failed attempt that was retried or replaced by a backup
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
`gen_ai.usage.*` over a trace counts each request once. As the GenAI conventions
define it, `input_tokens` counts every input token, cached ones included;
`cache_read.input_tokens` and `cache_creation.input_tokens` say how many of them were
read from or written to the provider's cache.

### Backup models

When the model is unavailable and the run switches to a backup (see
[Backup models](providers.md#backup-models)), the backup answers the request. The
request span is then named for the backup, and its `gen_ai.provider.name`,
`gen_ai.request.model`, `server.address` and `server.port` are the backup's. Every
attempt the failed model made stays a child span under that model's name, the last
one with `tempest.attempt.outcome` `fallback`. The request span gets one
`tempest.model.fallback` event per switch, at the time of the switch:

| Event attribute | Meaning |
|---|---|
| `tempest.fallback.from_provider`, `tempest.fallback.from_model` | the model that was unavailable |
| `tempest.fallback.to_provider`, `tempest.fallback.to_model` | the backup that gets the request |
| `tempest.fallback.reason` | the retry failure category, such as `model_not_found`, `server_failure`, `rate_limited` or `network_failure` |
| `tempest.fallback.attempts` | how many attempts the failed model got |
| `tempest.fallback.index` | the backup's position in the chain; the primary is 0 |

The run stays on the backup, so later requests are the backup's from the start. The
request's duration includes the failed model's attempts; its tokens, time to first
chunk and cost are the backup's. A failed attempt's own reported usage is measured
under the failed model.

### Attributes

| Span | Attributes |
|---|---|
| `invoke_agent` | `gen_ai.operation.name`, `gen_ai.agent.name` (`tempest`, or the subagent type), `gen_ai.conversation.id` (session id), `gen_ai.agent.call.id` (run id), `tempest.run.stop_reason`, `tempest.run.turns`, `gen_ai.aggregated_usage.{input_tokens,output_tokens,cache_read.input_tokens,cache_creation.input_tokens}`, `tempest.cost.usd` or `tempest.cost.unknown`, `tempest.loop.pattern`, `tempest.loop.action` (`nudge`, `stop`) and `tempest.loop.nudges` when the [loop guard](sessions-and-durability.md#loops) reported during the run (its last report and its nudge count), and `error.type` when the run ends in `error`, `output_invalid`, `repeated_failure` or `loop_detected`. |
| `chat` (request) | `gen_ai.operation.name`, `gen_ai.provider.name`, `gen_ai.request.model`, `gen_ai.request.max_tokens`, `gen_ai.request.temperature`, `tempest.request.reasoning`, `server.address`, `server.port`, `gen_ai.response.model`, `gen_ai.response.id`, `gen_ai.response.finish_reasons`, `gen_ai.usage.{input_tokens,output_tokens,cache_read.input_tokens,cache_creation.input_tokens}`, `tempest.cost.usd` or `tempest.cost.unknown`, and `error.type` on failure (`incomplete` when the run ended mid-request). |
| `chat` (failed attempt) | `gen_ai.operation.name`, `gen_ai.provider.name`, `gen_ai.request.model`, `error.type`, `tempest.attempt`, `tempest.attempt.outcome` (`retry`, or `fallback` when a backup model takes the request), and the usage and cost the attempt reported before it failed. |
| `execute_tool` | `gen_ai.operation.name`, `gen_ai.tool.name`, `gen_ai.tool.call.id`, `tempest.permission.decision` (`allow`, `deny`), `tempest.permission.source`, `tempest.tool.outcome` (`success`, `error`), and `error.type` (`tool_error`, or `incomplete` when the run ended first). |

Failed spans have status `ERROR`. Their status message is redacted and cut to 1024
characters.

### Message content

Content capture is off by default. With `otel.capture_message_content`, request spans
also carry `gen_ai.system_instructions`, `gen_ai.input.messages` and
`gen_ai.output.messages`, in the GenAI conventions' JSON formats. Tool spans also
carry `gen_ai.tool.call.arguments` and `gen_ai.tool.call.result`. Redacted reasoning
blocks are left out.

Each value keeps at most 64 KiB of text, copied when the event arrives, so a span
waiting for a slow collector never holds the whole history. Messages are kept in
order until the limit; the last kept text ends in `…[truncated]`. Tool arguments cut
at the limit become a JSON string. The JSON is encoded when the span is exported, off
the turn loop, and is always valid JSON.

## Context propagation

Tempest uses W3C Trace Context (`traceparent` and `tracestate`):

- **Incoming.** A `traceparent` header on `serve` requests becomes the run's parent.
  This covers the `/acp` WebSocket upgrade, `/a2a`, and a prompt's
  `_meta.traceparent` in ACP, which wins over the upgrade header, together with
  its `_meta.tracestate`. A `tracestate`
  header that comes with a valid `traceparent` is kept with it; repeated
  `tracestate` headers are joined into one list. A malformed `tracestate` (a bad key
  or value, a repeated key, or more than 32 entries) is dropped, and the
  `traceparent` still applies.
- **Outgoing.** Provider HTTP requests and MCP Streamable HTTP requests send the
  current request span as their `traceparent` header, and the received `tracestate`
  unchanged. Tempest adds no `tracestate` entry of its own.
- **Subprocesses.** Hook commands and `bash` tool commands get `TRACEPARENT`, and
  `TRACESTATE` when the trace has one, in their environment: foreground,
  background, named and `pty` commands alike, each with the span of the `bash` call
  that started it. Values inherited from tempest's own environment are replaced.
- **Subagents.** A `task` call's child run starts under that call's `execute_tool
  task` span, though it runs as a background job.

Exported spans carry the received `tracestate` as their OTLP `traceState`. When a
remote parent arrives with the sampled flag off, context is still propagated, but
its spans are not exported.

## Metrics

Metrics are aggregated in memory with cumulative temporality. They are pushed over
OTLP every `otel.metric_export_interval_ms`, flushed once more at exit, and served at
`/metrics`. Attribute values are always strings.

Each instrument keeps at most 2000 attribute sets, the OpenTelemetry SDK's default
cardinality limit. Measurements with a new attribute set beyond that, such as a
model-invented tool name in a long-running server, are added to one overflow series
labelled `otel.metric.overflow="true"`. Recording locks only the series it updates,
and a scrape or push copies one series at a time and never holds a lock across the
whole set: a collection delays a recording only while it copies that series, or,
for a new attribute set, that instrument's list of series.

The `gen_ai.*` metric names are those of semantic conventions v1.41.0. The GenAI
conventions have since moved to their own repository, which names the first-chunk
metric `gen_ai.client.inference.time_to_first_chunk` and the cache-write attribute
`gen_ai.usage.cache_write.input_tokens`; tempest keeps the v1.41.0 names.
`gen_ai.client.operation.cost` is tempest's own.

| OTLP name | Prometheus name | Type | Unit | Attributes |
|---|---|---|---|---|
| `gen_ai.client.token.usage` | `gen_ai_client_token_usage` | histogram | `{token}` | `gen_ai.operation.name`, `gen_ai.provider.name`, `gen_ai.request.model`, `gen_ai.response.model`, `gen_ai.token.type` (`input` with cached input included, `output`); a retried attempt's reported usage is recorded too |
| `gen_ai.client.operation.duration` | `gen_ai_client_operation_duration_seconds` | histogram | `s` | as above without `gen_ai.token.type`, plus `error.type` on failure; retries included |
| `gen_ai.client.operation.time_to_first_chunk` | `gen_ai_client_operation_time_to_first_chunk_seconds` | histogram | `s` | `gen_ai.operation.name`, `gen_ai.provider.name`, `gen_ai.request.model`; from sending an attempt to its first text, reasoning or tool-call name or arguments, not to an empty block start or a tool call start that carries only its ID |
| `gen_ai.client.operation.cost` | `gen_ai_client_operation_cost_usd` | histogram | `USD` | as for token usage without `gen_ai.token.type`; requests with an unknown price are not recorded, while a retried attempt's known cost is recorded |
| `tempest.agent.runs` | `tempest_agent_runs_total` | counter | `{run}` | `gen_ai.agent.name`, `tempest.run.stop_reason` |
| `tempest.tool.calls` | `tempest_tool_calls_total` | counter | `{call}` | `gen_ai.tool.name`, `tempest.tool.outcome` |
| `tempest.model.failed_attempts` | `tempest_model_failed_attempts_total` | counter | `{attempt}` | `gen_ai.provider.name`, `gen_ai.request.model`, `error.type`, `tempest.attempt.outcome` (`retry`, `fallback`) |
| `tempest.model.fallbacks` | `tempest_model_fallbacks_total` | counter | `{switch}` | `tempest.fallback.reason`, `tempest.fallback.from_provider`, `tempest.fallback.from_model`, `tempest.fallback.to_provider`, `tempest.fallback.to_model`; the models come from the configured chain |
| `tempest.permission.decisions` | `tempest_permission_decisions_total` | counter | `{decision}` | `gen_ai.tool.name`, `tempest.permission.decision`, `tempest.permission.source` |
| `tempest.hook.runs` | `tempest_hook_runs_total` | counter | `{run}` | `tempest.hook.event`, `tempest.hook.outcome` (`success`, `blocked` for exit 2, `failed`, `error`, `timeout`) |
| `tempest.sessions` | `tempest_sessions` | gauge | `{session}` | none; open root sessions |
| `tempest.jobs` | `tempest_jobs` | gauge | `{job}` | none; background jobs not yet finished, across open sessions |
| `tempest.telemetry.spans.lost` | `tempest_telemetry_spans_lost_total` | counter | `{span}` | `reason` (`queue_full`, `export_failed`, or `rejected` for spans a collector refused in an export it otherwise accepted); present only when traces are exported |

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
| Export timeout | `otel.timeout_ms` (10 s) for each export, retries included |
| Retries | an answer of 429, 502, 503 or 504 is retried after about 1 s, doubling up to 5 s, with jitter, and never sooner than its `Retry-After`; when the next try would not fit in the export timeout, the export fails. Other failures are not retried |
| Partial success | a collector that accepts an export but reports rejected spans or data points in `partialSuccess` is not sent them again; rejected spans count as lost, and the rejection is logged like a failure |
| Exit flush | at most 5 seconds; anything still queued then is dropped |
| Collector failures | the first failure is logged as a warning, later ones at debug level; a success logs recovery once |
| Collector redirects | followed within the endpoint's scheme, host and port only, so configured headers never reach another origin; a redirect elsewhere fails the export |

## Limits

- OTLP/HTTP with JSON encoding only. gRPC and protobuf are not implemented, and
  asking for them is a startup error.
- No logs signal, and no sampler setting: every local root trace is sampled, and a
  remote parent's sampled flag is respected.
- Metrics are kept per process. A restart starts the cumulative counters again
  from zero, with a new start time.
