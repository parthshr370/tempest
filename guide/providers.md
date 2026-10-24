# Providers

Tempest keeps one model registry and one set of protocol adapters for every route.
A model ID is the endpoint's exact route ID, never rewritten or normalized. Four
concepts stay separate:

| Concept | Meaning |
|---|---|
| **Model** | The endpoint's exact route ID. |
| **Endpoint** | The HTTP service and credential that serve that ID. An explicit `@<base-url>` overrides the configured endpoint. |
| **Protocol adapter** | `anthropic`, `openai-chat`, `google`, `openai-responses`, `bedrock`, or `vertex`. |
| **Catalog metadata** | Embedded context and output limits, capabilities, compatibility controls, and USD-per-million prices. Catalog membership is not proof of endpoint access, protocol compatibility, or account permission. |

Unknown model IDs still run with protocol fallback limits and **unknown price**, not a
fabricated free price. Chat-compatible, Responses and Bedrock routes assume a
128,000-token context; Anthropic and OpenRouter use 200,000, and Google and Vertex
use 1,048,576. These fallbacks are not a model specification.
Configure `models[].contextWindow` and `maxTokens` for an unlisted model's actual
limits. Every protocol reserves room for the estimated prompt and a
4,096-token safety margin, and caps requested output at the model's output limit.
ChatGPT plan usage is an exception: Tempest drops internal temperature and output-token
budgets from requests, including compaction and overflow recovery. Explicit user
output caps (`-max-tokens`, `max_tokens`, `HARNESS_MAX_TOKENS`, or SDK `MaxTokens`)
are rejected; use turn and wall-time limits instead.
Computed JSON costs encode `{"unknown":true}` rather than zero-dollar components.
Unknown provider prefixes are configuration errors. Bare IDs use the Anthropic route.

## Routes

`-model` selects the model. The shape is `<provider>:<model-id>`, with an optional
`@<base-url>` override:

| Route | Credential | Notes |
|---|---|---|
| `<id>` | `ANTHROPIC_API_KEY` | Bare IDs use the Anthropic route. |
| `anthropic:<id>` | `ANTHROPIC_API_KEY`, `ANTHROPIC_OAUTH_TOKEN`, `ANTHROPIC_AUTH_TOKEN` | Anthropic Messages. |
| `google:<id>` | `GEMINI_API_KEY` or `GOOGLE_API_KEY` | Google Generative AI. Point it elsewhere with `google:<id>@<base-url>`. |
| `openrouter:<id>` | `OPENROUTER_API_KEY` | OpenAI Chat Completions with OpenRouter's endpoint. Model IDs may carry a `~` prefix. Point it elsewhere with `openrouter:<id>@<base-url>`. |
| `openai:<id>` | `OPENAI_API_KEY` | Endpoint comes from `-openai-base-url`, `HARNESS_OPENAI_BASE_URL`, settings `openai_base_url`, or `OPENAI_BASE_URL`, in that order. |
| `openai:<id>@<base-url>` | `OPENAI_API_KEY` | Explicit endpoint override. |
| `openai-chatgpt:<id>` | Saved Sign in with ChatGPT grant | Public Responses endpoint, ChatGPT plan usage, no API-key fallback or endpoint overrides. |
| `<custom>:<id>` | Depends on the registry entry | Any route configured in the `providers` registry. |

The model ID is opaque and endpoint-specific: `claude-fable-5-1` on Anthropic
differs from `anthropic/claude-fable-5.1` on OpenRouter, whose aliases include a
literal `~` prefix.
Only `@http://` or `@https://` starts an endpoint override. Vertex version IDs such as
`vertex:claude-haiku-4-5@20251001` keep their version suffix.


### Route resolution

The router parses the route, looks up the provider registry, resolves the endpoint
and credential, applies catalog and model metadata, then selects the protocol
adapter.

```mermaid
flowchart TD
  M["-model provider:id@base-url"] --> P["parse route"]
  P --> D{"provider set?"}
  D -->|no| A["anthropic default"]
  D -->|yes| R["registry: presets and settings"]
  A --> R
  R --> E["endpoint: @base-url, flags, preset"]
  E --> C["credential: api_key_env, api_key, auth header or query, cloud chain"]
  C --> K["catalog and model metadata: limits, cost, compatibility"]
  K --> X["protocol adapter"]
  X --> S["retrying stream"]
```

## Endpoint registry

Define or override endpoints in `~/.config/harness/settings.json` under `providers`,
keyed by lowercase route name:

```json
{
  "providers": {
    "local": {
      "protocol": "openai-chat",
      "base_url": "http://localhost:8080/v1",
      "api_key_env": "LOCAL_API_KEY",
      "compatibility": {"supportsPromptCacheKey": false}
    }
  }
}
```

| Field | Meaning |
|---|---|
| `protocol` | One of `openai-chat`, `anthropic`, `google`, `openai-responses`, `bedrock`, `vertex`. |
| `base_url` | Endpoint origin or base URL. Each protocol decides how much path it appends. |
| `api_key_env` | Name of the environment variable holding the key, never the key itself. |
| `api_key` | Fallback key. The environment variable wins. |
| `headers` | Extra request headers. |
| `query` | Extra query parameters. |
| `auth_header` | Header used for the credential. |
| `auth_query` | Query parameter used for the credential. The runtime key becomes the query value and no bearer is sent. |
| `auth_prefix` | Prefix before the key. Defaults to `Bearer ` (with the space); an explicit empty string sends the raw key. |
| `no_auth` | Skips credential lookup. |
| `compatibility` | Endpoint quirks, in camelCase. See [Compatibility keys](#compatibility-keys). |
| `models` | Per-model metadata: `id`, `name`, `contextWindow`, `maxTokens`, `reasoning`, `input`, and `cost` (`input`, `output`, `cacheRead`, `cacheWrite`). Only supplied fields override catalog defaults. A model's `maxTokens` caps the startup limit, even when a larger default is in effect. An absent cost preserves a catalog price, or stays unknown when none exists. JSON can explicitly set a zero-dollar cost; an all-zero Go model literal without JSON field presence does not declare a free model. |

A preset supplies defaults for its fields. A registry entry may override one field
or add a new route. A new route needs a `protocol` and, for protocols that need one,
a `base_url`.

### Compatibility keys

`compatibility` accepts these camelCase keys:

| Key | Values | Effect |
|---|---|---|
| `reasoningDisableMode` | Provider-specific | How reasoning is turned off. |
| `maxTokensField` | `max_tokens`, `max_completion_tokens` | Output-limit field name. |
| `reasoningFormat` | `openai`, `openrouter`, `none` | How reasoning effort is sent. |
| `reasoningContentField` | `reasoning_content`, `reasoning`, `reasoning_text` | Field used to replay reasoning content alongside tool history. |
| `supportsPromptCacheKey` | boolean | Send `prompt_cache_key`; `false` omits it. |
| `supportsSessionId` | boolean | Send `session_id` for sticky routing. |
| `supportsUsageInStreaming` | boolean | Expect usage in stream events. |
| `supportsToolChoice` | boolean | Send `tool_choice: "auto"` when tools are available, or a caller's explicit tool choice. `false` omits the field. |
| `cacheControlFormat` | `anthropic`, `none` | Emit a top-level `cache_control` block. |
| `supportsEncryptedReasoning` | boolean | Include `reasoning.encrypted_content` and replay encrypted history; `false` omits both. A nil value keeps OpenAI behavior. |
| `requiresMistralToolIds` | boolean | Use Mistral's 9-character alphanumeric tool IDs. An explicit `false` overrides the preset. |

Most chat-compatible presets use `max_tokens` and no OpenAI prompt cache fields.
Mistral and Cerebras also send no `stream_options`. Ollama disables `tool_choice`.
`xai` is a chat-compatible preset today; set its protocol to `openai-responses` for
models that need the Responses API.

The Cerebras omission follows its [published request schema](https://inference-docs.cerebras.ai/api-reference/chat-completions),
which has no `stream_options` member. Mistral's [current validator](https://github.com/mistralai/mistral-common/blob/main/src/mistral_common/protocol/instruct/validator.py)
permits a user message directly after tool results, so the adapter doesn't insert a
fabricated assistant turn.

## Presets

| Route | Protocol | Credential | Default endpoint |
|---|---|---|---|
| `anthropic` | `anthropic` | `ANTHROPIC_API_KEY` | `https://api.anthropic.com` |
| `openai` | `openai-chat` | `OPENAI_API_KEY` | `https://api.openai.com/v1` |
| `google` | `google` | `GEMINI_API_KEY` | `https://generativelanguage.googleapis.com` |
| `openrouter` | `openai-chat` | `OPENROUTER_API_KEY` | `https://openrouter.ai/api/v1` |
| `groq` | `openai-chat` | `GROQ_API_KEY` | `https://api.groq.com/openai/v1` |
| `mistral` | `openai-chat` | `MISTRAL_API_KEY` | `https://api.mistral.ai/v1` |
| `xai` | `openai-chat` | `XAI_API_KEY` | `https://api.x.ai/v1` |
| `deepseek` | `openai-chat` | `DEEPSEEK_API_KEY` | `https://api.deepseek.com` |
| `together` | `openai-chat` | `TOGETHER_API_KEY` | `https://api.together.ai/v1` |
| `fireworks` | `openai-chat` | `FIREWORKS_API_KEY` | `https://api.fireworks.ai/inference/v1` |
| `cerebras` | `openai-chat` | `CEREBRAS_API_KEY` | `https://api.cerebras.ai/v1` |
| `perplexity` | `openai-responses` | `PERPLEXITY_API_KEY` | `https://api.perplexity.ai/v1` |
| `ollama` | `openai-chat` | none | `http://localhost:11434/v1` |
| `lmstudio` | `openai-chat` | none | `http://localhost:1234/v1` |
| `vllm` | `openai-chat` | none | `http://localhost:8000/v1` |
| `llamacpp` | `openai-chat` | none | `http://localhost:8080/v1` |
| `azure` | `openai-chat` | `AZURE_OPENAI_API_KEY` | `AZURE_OPENAI_ENDPOINT`, a deployment path, or `/openai/v1` |
| `openai-responses` | `openai-responses` | `OPENAI_API_KEY` | `https://api.openai.com/v1` |
| `openai-chatgpt` | `openai-responses` | Saved ChatGPT sign-in | `https://api.openai.com/v1` |
| `bedrock` | `bedrock` | `AWS_BEARER_TOKEN_BEDROCK`, or AWS credentials | `https://bedrock-runtime.<region>.amazonaws.com` |
| `vertex` | `vertex` | Google service account or `GOOGLE_CLOUD_API_KEY` | `https://aiplatform.googleapis.com` (global) or `https://<location>-aiplatform.googleapis.com` |

The `deepseek` preset replays `reasoning_content` on every assistant turn, including
an empty string when thinking is unavailable after a model switch, as
[DeepSeek's thinking-mode documentation](https://api-docs.deepseek.com/guides/thinking_mode)
requires for requests with tools. The `perplexity` preset disables encrypted reasoning and
`prompt_cache_key`, avoiding OpenAI-only fields. Other Responses routes keep native
OpenAI behavior unless a compatibility flag sets it to `false`. The `google` preset
is the origin only; the adapter appends `/v1beta/models/<model>:streamGenerateContent`.

### Azure

Azure sends the key in `api-key` and uses `max_completion_tokens`, which
[Azure reasoning models require](https://learn.microsoft.com/en-us/azure/foundry/openai/how-to/reasoning).
A resource root gets `/openai/deployments/<model-id>/chat/completions` appended,
with `api-version=2025-04-01-preview`. A deployment-specific
`/openai/deployments/<name>` base URL works too. `AZURE_OPENAI_API_VERSION` overrides
only that query parameter, preserving other configured query parameters.

The current [Azure v1 API](https://learn.microsoft.com/en-us/azure/foundry/openai/api-version-lifecycle)
also works: set `base_url` or `AZURE_OPENAI_ENDPOINT` to
`https://RESOURCE.openai.azure.com/openai/v1/`. That path gets only
`/chat/completions` appended and no dated `api-version` parameter.

```json
{
  "providers": {
    "azure": {
      "base_url": "https://RESOURCE.openai.azure.com",
      "query": {"api-version": "2025-04-01-preview"}
    }
  }
}
```

Select it with `-model azure:<deployment>`.

## Project provider definitions

Provider definitions in `.harness/settings.json` and
`.harness/settings.local.json` **never apply without explicit operator trust**. Use
`-trust-project-providers`, `HARNESS_TRUST_PROJECT_PROVIDERS=true`, or add the
workspace's absolute canonical path to user `trusted_provider_projects`. A project
cannot grant itself trust.

An untrusted project definition is ignored entirely, including a definition that
would replace a built-in route, so a cloned repository cannot redirect a credential.
After opt-in, a project entry replaces a user entry of the same name, with
`settings.local.json` winning by name, and preset defaults merge field by field
underneath. An entirely new route is supported after opt-in.

Trust grants access to **any environment variable** named by a project's
`api_key_env`, not just built-in provider keys. A trusted project can send that value
to its configured endpoint, including a value such as `GITHUB_TOKEN`. Inspect the
project's provider definitions before granting trust.

## Protocol adapters

OpenRouter uses the OpenAI Chat Completions adapter with its own endpoint and key.
The adapters are `anthropic`, `openai-chat`, `google`, `openai-responses`, `bedrock`
and `vertex`. Each maps tools, images, reasoning and usage into the shared message
format. Responses, Bedrock and Vertex are covered by shared conformance tests and
isolated CLI runs against local fake servers; see [testing](testing.md#provider-conformance).

All protocol adapters normalize history before conversion: failed assistant
attempts are dropped, unanswered tool calls get synthetic results, foreign tool IDs
are normalized for the target protocol, and unsupported images become placeholders.

### OpenAI Responses

`openai-responses` posts to `/responses` with `store=false`. A model
`@<base-url>` override applies here and must include `/v1`. Reasoning-capable models
request `reasoning.encrypted_content`. Replay requires nonempty encrypted content
and the same provider and model; set `supportsEncryptedReasoning: false` to turn
both off. The session ID doubles as `prompt_cache_key`; IDs longer than 64 bytes
use their SHA-256 hex digest. Reasoning effort and summary are sent when configured
and supported by the model, and cached and reasoning token usage is read back.

### Sign in with ChatGPT

Use your own ChatGPT account's authorized plan usage with the explicit
`openai-chatgpt:` route. Existing `openai:` and `openai-responses:` routes keep
using API keys. Signing in never changes their billing source.

```sh
tempest login openai
tempest auth status openai
tempest -model openai-chatgpt:<account-model-id> -p "Explain this repository"
tempest logout openai
```

`Continue with ChatGPT` opens the system browser. Tempest starts a
`http://127.0.0.1:<available-port>/auth/callback` listener first. It uses an
authorization code, S256 PKCE, state and an OIDC nonce. First use registers
`dynamic_agent_client` with the truthful name `Tempest` and a persistent UUID
host ID. Later sign-ins reuse the issued client ID. Tokens come from the
documented `/api/accounts/oauth/token` endpoint, not Codex's legacy endpoints.
Tempest verifies the ID token's RSA signature with OpenAI's JWKS, issuer,
audience, expiry and nonce before saving the account identity.

Use `--no-browser` to print the ordinary authorization URL without launching a
browser. Open it on the same host. This is not a device-code or paste-a-token
flow. No undocumented headless login is provided. Authorization URLs omit the
optional retained ID-token hint so printed URLs contain no tokens.

Each account registration has a stable local label. The first label is
`default`; `tempest login openai --profile personal` adds or reauthorizes
`personal` and selects it after successful identity validation and plan authorization.
`tempest auth list openai` lists the saved labels.
Use `--profile <label>` with status or logout to target another saved account.
Reauthorizing a label with a different verified subject is rejected. Choose a new
label for a different account or workspace, even if its email is identical.
Running credential sources retain their selected label and never follow a later
change to the active selection.
If plan usage was declined, the identity stays saved without replacing a working
active profile. Run `tempest login openai --profile personal --consent` to explicitly
request the full scope set again with OAuth `prompt=consent`. Ordinary reauthorization
does not force a consent screen. Tempest does not send `force_reconsent=true` because
OpenAI has not confirmed its deployment for this integration.

Credentials live in `$XDG_CONFIG_HOME/tempest/openai` when set, otherwise under
`os.UserConfigDir()/tempest/openai`. The directory is `0700`, and credentials,
host identity, active selection and lock files are `0600`. Writes use fsync and
atomic rename. Per-profile advisory file locks serialize credential reads and
updates across processes. Login releases the lock during the browser wait, then
compares the current record before saving so concurrent refresh or logout wins.
Before requests, near-expiry credentials refresh with the saved issued client ID
and `resource=https://api.openai.com/v1`. Refresh has its own 30-second timeout,
independent of turn cancellation. The store rereads after locking and immediately
saves the rotated token pair before ID-token checks; an unvalidated refreshed
identity cannot authorize inference, including after a restart. Temporary renewal
outages can still use an unexpired access token.
Tokens never enter session configuration, tool journals or doctor output.
Doctor stays offline and shows only the account label, identity and expiry.
This implementation uses the documented protected-file option on macOS too.
The `security` CLI would pass a newly stored secret through process arguments
and introduce a second store to coordinate during rotation, so Tempest does not
use it.

Logout attempts the discovery document's revocation endpoint, retries transient
failures, and clears access, refresh and ID tokens locally. Account/client mapping
and host identity remain for reauthorization. If remote revocation cannot be
confirmed, Tempest says so. Disconnect the app in ChatGPT Settings in that case.
Expired or revoked grants stop inference and tell you to run
`tempest login openai`. They never fall back to paid API keys.

Plan requests use bearer authentication against the public `/v1/responses`
endpoint, `store:false`, `stream:true`, full input history and namespaced local
tools. They do not use legacy Codex account-ID or beta headers. Credential,
header, provider and endpoint overrides are rejected on this route. Internal
temperature and output-token options are omitted, not rejected; explicitly configured
user output caps are rejected before inference. Unrelated `openai_base_url` or
`OPENAI_BASE_URL` settings do not affect this route. An incomplete response is not
permission to execute a tool. A `subscription_sharing_*` HTTP 429 pauses requests
without retrying; check [ChatGPT usage settings](https://chatgpt.com/settings/usage).
Use a model available to your account. Catalog metadata is not entitlement.
Plan and app limits apply, and authorized ChatGPT credits may be used.
Dollar cost stays unknown, so `-max-cost` fails closed. Manage usage and access at
[ChatGPT Settings](https://chatgpt.com/settings/usage).

CLI one-shot and plan/smol roles use the same credential source. Single-owner ACP,
A2A and MCP servers enable that source only when an operator-configured default,
plan or smol model uses `openai-chatgpt:`; a client model override cannot enable
an otherwise unconfigured grant. Child sessions inherit it. SDK users must opt in
explicitly instead of inheriting credentials from an ambient home directory:

```go
provider := tempest.ProviderConfig{
    ChatGPTCredentials: tempest.OpenAIChatGPTCredentials("personal"),
}
```

Pass `provider` in `tempest.Options.Provider`, and select an
`openai-chatgpt:<model-id>` session model. SDKs can instead supply a request-local
credential callback. Keep saved profiles and grants scoped to one owner. Do not
pool a ChatGPT account across unrelated remote users.

Protocol tests may use `tempest login openai --no-browser --issuer
http://127.0.0.1:<fixture-port>` with an isolated config directory. Only production
OpenAI or an explicitly selected literal-loopback fixture authority is accepted.
Fixture grants remain bound to that authority's `/v1` endpoint.

The wire contract follows OpenAI's [registration and sign-in](https://developers.openai.com/siwc/token-sharing-open-source/sign-in),
[accounts and sessions](https://developers.openai.com/siwc/token-sharing-open-source/profiles-and-sessions),
[models and inference](https://developers.openai.com/siwc/token-sharing-open-source/models-and-inference)
and [preview limitations](https://developers.openai.com/siwc/token-sharing-open-source/preview-limitations).
Consent and plan-limit recovery follow OpenAI's
[errors and recovery](https://developers.openai.com/siwc/token-sharing-open-source/errors-and-recovery);
rotation follows the [token reference](https://developers.openai.com/siwc/token-sharing-open-source/token-reference).


### Bedrock

`bedrock` calls ConverseStream and receives the AWS binary event stream, with CRC
checks, using the standard library. With `Transport: "http"` it uses non-streaming
Converse instead. The model ID is passed verbatim as one escaped URL segment, so a
foundation model ID, an inference-profile ID, or an ARN all work.

Auth precedence:

1. An explicit API key or `AWS_BEARER_TOKEN_BEDROCK`.
2. `AWS_ACCESS_KEY_ID` with `AWS_SECRET_ACCESS_KEY` and optional
   `AWS_SESSION_TOKEN`, signed with SigV4.
3. Shared credentials from `~/.aws/credentials`, honoring `AWS_PROFILE` and
   `AWS_SHARED_CREDENTIALS_FILE`.

The origin defaults to `https://bedrock-runtime.<region>.amazonaws.com`, where the
region comes from `AWS_REGION` or `AWS_DEFAULT_REGION`. A model `@<base-url>`
override applies to the origin only. Tools, images and reasoning content are
mapped. Reasoning is signed and replayed. Thinking requests are sent only to
Anthropic models (IDs containing `anthropic`), with a budget or adaptive
compatibility, and Bedrock cache checkpoints are not created automatically.
Tool-result `status` is sent only to Claude 3 and 4 and Amazon Nova, as
[AWS documents](https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_ToolResultBlock.html).
Guardrail and content-filter stop errors retain their original stop reason.


### Vertex

`vertex` chooses the Anthropic adapter when the model ID starts with `claude-` or
contains `/anthropic/`, and the Gemini adapter otherwise. Gemini uses
`streamGenerateContent`; Claude uses `streamRawPredict`. With `Transport: "http"`,
Gemini uses `generateContent` and Claude uses `rawPredict` instead. Tools, images,
thinking signatures and cache usage follow those native converters.
Publisher resource IDs and full project resource IDs preserve their path separators.
A full `streamRawPredict` override selects Claude even when the model is an alias.
Vertex keeps configured `anthropic-beta` headers, including interleaved-thinking
betas.


Auth precedence:

1. An explicit `Authorization` header.
2. For Gemini only, a configured key or `GOOGLE_CLOUD_API_KEY` selects Express mode
   (`/v1/publishers/google/models/...`, no project). Claude skips this key and
   continues to the OAuth credential chain.
3. A service account JSON file from `GOOGLE_APPLICATION_CREDENTIALS`, with the
   project from `GOOGLE_CLOUD_PROJECT`, `GCLOUD_PROJECT`, or the account's
   `project_id`.
4. `gcloud config config-helper --format=json --min-expiry=60s`, used only when no
   service account is configured. Its active project is a fallback when no project
   environment variable is set.

`GOOGLE_CLOUD_LOCATION` defaults to `global`. A model `@<base-url>` override may be
an origin or a full `streamGenerateContent` or `streamRawPredict` endpoint. Service
account and gcloud OAuth tokens are cached until their reported expiry, with a
one-minute refresh margin. Tokens with no reported expiry are not cached.
`no_auth` skips the credential chain.

## Costs

Cost comes from the provider's bill when the route reports one. OpenRouter returns
each request's billed cost in its usage (`usage.cost`). With a bring-your-own-key
upstream, the request costs OpenRouter's fee plus
`cost_details.upstream_inference_cost`. That amount, which already prices cached and
cache-write tokens, is the call's cost and counts against `-max-cost`, so an
OpenRouter model needs no catalog entry for a cost budget.

Billed costs carry only a total, no per-component split. The catalog price is the
fallback when a response reports no billed cost. With neither, the call's cost stays
unknown.

## Prompt caching

Within a session, every request extends the previous one byte for byte: history is
only rewritten by compaction, and the tool list is in name order.

- On OpenAI's endpoint and on OpenRouter, each request carries the session id as
  `prompt_cache_key`.
- OpenRouter also gets it as `session_id`, its sticky-routing key, so later turns
  reach the endpoint that holds the cache.
- Claude models on OpenRouter get a top-level `cache_control`, without which they
  cache nothing.
- Other OpenAI-compatible endpoints get none of these fields unless their registry
  entry enables them.

## Catalog metadata

`internal/catalog/models.json` records import provenance (`generated_from`, `as_of`)
and each entry's `source` URL and `verified_at` date. Most direct-provider entries
retain the imported snapshot's date. GPT-5.6 Luna's route and metadata were checked
against OpenAI's model page on 2026-10-21. OpenRouter entries use the public
discovery snapshot from that date. Discovery confirms exact IDs and advertised
metadata, not a successful generation request.

Routes use the vendor family for catalog lookup without changing the wire model ID:
Responses uses OpenAI metadata, Vertex uses Google or Anthropic metadata (ignoring
the Claude `@version` suffix for lookup), and known Bedrock Anthropic model families
share Anthropic metadata. Perplexity's model IDs have no entries in the embedded
catalog and remain unknown-priced instead of inheriting unrelated OpenAI prices.

To validate the embedded JSON and the live IDs:

```sh
go test ./internal/catalog/...
TEMPEST_LIVE=1 go test ./internal/catalog/ -run TestVerifyOpenRouter
```

## Doctor

`./bin/tempest doctor` is offline and never prints credentials. It prints a one-line
catalog count, lists the provider registry with credential presence and whether auth
is required, and `-models` lists every catalog model with its prices. Keyless local
presets show `present=no auth_required=no`, not a pretend API key. Bedrock and Vertex
are not rejected offline for an absent API-key variable; `--ping` verifies the full
credential chain. Configured headers, query parameters and API keys are never
displayed.

`./bin/tempest doctor --ping` makes one billable request (prompt `hi`, output cap 16
tokens) to each configured model: the default model, plus the plan and smol models
when they differ. Each line names the roles and the exact model it pinged, then
latency on success, or the typed failure code with the provider's HTTP status and
bounded, redacted message, for example `provider HTTP 404: {...}` for an
account/endpoint restriction. A missing credential error names the configured
`api_key_env`.

## Output

Output is text by default. `-output-format json` prints a run report and
`-output-format stream-json` streams NDJSON updates followed by that report. See
[sessions and durability](sessions-and-durability.md#output-formats).

[Guide index](README.md) · [Tempest](../README.md)
