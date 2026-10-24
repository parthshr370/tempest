# Testing and development

Runs need no provider when they use the faux model, and the repository has build,
test and smoke targets for local checks.

## Testing without a provider

`-faux-script <path>` (also `HARNESS_FAUX_SCRIPT` or settings `faux_script`)
replaces the model with a scripted one, so a run needs no API key or network.
It works for one-shot runs, `serve` and `mcp`, and overrides `-model`; reports
name the model `faux-1`. The script is a JSON array with one entry per model
call, used in order:

```json
[
  {"text": "Listing files.", "toolCalls": [{"id": "b1", "name": "bash", "arguments": {"command": "ls"}}]},
  {"text": "done"}
]
```

- `text` is the assistant text for that call; it may be omitted.
- `toolCalls` lists the tool calls the model makes: `name` is required,
  `arguments` is the tool's JSON input (default `{}`) and `id` is the tool-call
  id. The tools really run, through the normal permission gate.
- After an entry with tool calls, the tool results go back to the model and the
  next entry answers them. An entry without tool calls ends the run.
- `error` fails that provider call with the supplied message, for example
  `{"error":"context_length_exceeded"}` exercises overflow recovery.
- `waitForFile` holds a response until the named file exists. It can accompany
  `text`, `toolCalls` or `error`; cancelling the provider context releases the
  wait. These controls only affect the explicit faux provider, not live models.
- An empty array is a startup error. A model call after the last entry fails
  the run with `No more faux responses queued`.

## Provider conformance

The provider adapters share one conformance suite:

- Responses and the Bedrock binary event stream have shared conformance cases.
- Vertex Gemini and Vertex Claude run through native conformance factories.
- Tests cover a published AWS SigV4 vector, CRC corruption and truncation, shared
  profile and bearer credentials, JWT RS256 signature and claims with cache expiry,
  encrypted reasoning round trips, native tools, images, thinking and usage, and
  in-stream retry codes.
- Isolated CLI runs exercise Responses, Bedrock, Vertex Gemini and Vertex Claude
  against local fake HTTP servers, asserting endpoints, auth, tool payloads and
  request bodies. No live cloud account is required.

## Development

```sh
go build ./...
go test ./...
make check   # build + vet + test + race
make smoke   # end-to-end run in a scratch dir with the faux provider
```

## Learn how it works

The [`education/`](../education/README.md) directory is a guided, subsystem-by-subsystem
tour of the codebase: the streaming engine and agent loop, providers and SSE
transport, the tool layer and hash-anchored editing, config and model roles, retry
and typed errors, concurrency, compaction, the prompt system, permissions, and the
idiomatic-Go decisions behind each.

[Guide index](README.md) · [Tempest](../README.md)
