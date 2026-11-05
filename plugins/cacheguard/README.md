# StillMark (formerly CacheGuard)

### Bifrost Prompt-Cache Integrity & Diagnostics Plugin

**StillMark** is an open-source, operator-focused diagnostic plugin for the [Bifrost AI Gateway](https://github.com/maximhq/bifrost). It detects, attributes, and explains prompt-cache degradation and structural request drift across LLM calls without logging or exposing sensitive prompt text or credentials.

---

## The Real Operator Problem

Modern LLM providers (Anthropic, OpenAI, DeepSeek, Google Gemini via OpenRouter) offer prompt-caching features that discount input token pricing by 50% to 90% when prefixes or tool definitions remain identical across requests.

However, in production environments:
1. **Silent Drift**: Minor client changes (e.g., modifying tool schema parameter descriptions, changing parameter types, altering static system instructions) silently bust the provider's prompt cache, leading to unexpected latency spikes and token costs.
2. **Attribution Ambiguity**: When a prompt cache miss occurs, operators struggle to know whether:
   - The **client** modified the prompt or tool definitions.
   - The **gateway** injected or altered parameters (such as dynamic MCP tools or cache-control markers).
   - The **provider** evicted or failed to reuse the cache due to cold-start, TTL expiry, or internal capacity routing.
3. **Privacy Hazards**: Operators diagnosing caching issues often resort to capturing raw request/response payloads, risking leakage of confidential user prompts, API credentials, and internal customer IDs into shared log stores.

---

## What StillMark Does

StillMark runs inside the Bifrost gateway pipeline (using `PreLLMHook`, `PostLLMHook`, `HTTPTransportPreHook`, and `HTTPTransportResponseHeadersHook`) and provides:

- **Keyed Cryptographic Fingerprints**: Hashes prompts, system instructions, and schema paths using process-local HMAC-SHA256 secrets. Rainbow-table attacks against predictable strings are prevented.
- **Canonical Nested Schema Flattening**: Recursively walks arbitrary JSON schemas in tool definitions up to a bounded depth of 16 levels and 512 flattened paths, indexing individual leaf properties (e.g. `tools[0].function.parameters.properties.node_id.type`) to pinpoint exact field mutations.
- **Conversation Prefix Awareness**: Distinguishes static cached prompt prefixes from normal multi-turn message appending. Appending new user/assistant turns does not falsely trigger drift; only mutations to previously established static prefixes or tool schemas are flagged.
- **Tenant & Domain Isolation**: Session state is isolated strictly by authenticated tenant (`customer:<id>` or `virtual_key:<id>`), provider, and model. Unauthenticated or unverified requests are safely marked `untracked` or `refused` and never cross-compared.
- **Fail-Open Fault Tolerance**: Every plugin hook wraps internal operations in panic recovery handlers. Even in the event of an unexpected internal error, client inference requests are never failed or interrupted.
- **Error & Missing-Usage Protection**: Failed provider requests (HTTP 500, timeouts) are never committed to session history, and missing usage metadata is never falsely labeled as a cache miss.
- **Verified Within-Request Transformation Detection**: Detects when Bifrost injects or clamps cache-control markers (e.g. Anthropic prompt caching auto-injection) before dispatching to the provider.
- **Privacy-Safe Human-Readable Diagnostics**: Translates observations into stable codes (`CG001`–`CG007`) with plain-English operator guidance, severity ratings, and actionable troubleshooting steps.
- **Safe Rate-Limited Operator Logging**: Emits structured logs directly to `ctx.Log(...)` and standard output, rate-limited per trusted scope to avoid log flooding.

---

## What StillMark Does NOT Prove

To maintain operator trust and avoid misleading claims:
- **Observational Only**: StillMark is an observational diagnostic tool. It does **not** guarantee provider cache hits or provider billing reductions.
- **No Automatic Request Rewriting**: StillMark does not mutate, reorder, or alter client prompts. It is fail-open by design.
- **No False Accusations of Provider Billing Errors**: StillMark distinguishes between observed data and inferred causes. For example, a stable miss is diagnosed as unknown-cause (`CG003`), rather than an unproven claim of provider malfunction or eviction.
- **No Unproven Provider Compatibilities**: For providers like OpenRouter where multiple Gemini cache-control markers are documented to use only the final breakpoint as the cache prefix, StillMark notes the configuration (`CG006`) without falsely claiming double billing or API failure.
- **Synthetic Metrics Notice**: All local mock metrics in tests and demonstrations are explicitly labeled **SIMULATED** and do not represent real-world dollar savings without live provider benchmarking.

---

## Architecture & Request Flow

```
Client HTTP Request
  │
  ▼
[HTTPTransportPreHook] ───► Extract and validate opt-in X-Cacheguard-Session-Id
  │                         Initialize request diagnostic container
  ▼
[PreLLMHook] ─────────────► Compute HMAC-SHA256 fingerprint of static prefix & tools
  │                         Compare against session snapshot in bounded LRU memory
  │                         Evaluate drift: client_input_changed vs stable
  ▼
[Provider API Dispatch] ──► Forward request to AI Provider (e.g. OpenAI / Anthropic)
  │
  ▼
[PostLLMHook] ────────────► Extract provider usage (prompt_tokens, cached_tokens)
  │                         If outbound body captured, detect gateway transformations
  │                         Synthesize diagnosis (CG001–CG007) & rate-limited log
  ▼
[ResponseHeadersHook] ────► Attach non-sensitive diagnostic headers (X-Cacheguard-*)
  │
  ▼
Client HTTP Response
```

### Note on Streaming (SSE) Requests
In HTTP streaming mode (`stream: true`), HTTP response headers (`Content-Type: text/event-stream`, `HTTP/1.1 200 OK`) are sent over the wire before streaming chunks arrive. 
- Structural drift (`confirmed_drift`, `tools`, `system_prompt`) and transformation diagnostics (`CG001`, `CG002`, `CG007`) are evaluated during `PreLLMHook` and reflected in response headers.
- Provider token usage metrics (`X-Cacheguard-Prompt-Tokens`, `X-Cacheguard-Cached-Tokens`) and `stable_hit`/`stable_miss` are evaluated in `PostLLMHook` once stream chunks finish accumulating. They are recorded in operator logs, but cannot be added to HTTP headers after streaming has already commenced.

---

## Diagnostic Findings Reference (CG001–CG007)

| Code | Severity | Component | Evidence Source | Plain-English Explanation | Suggested Operator Action |
|:---:|:---:|:---:|:---:|:---|:---|
| **`CG001`** | `warning` | `tools` | `client_input` | Tool parameter structure changed within this session. This may reduce provider prompt-cache reuse. | Keep shared tool definitions consistent across requests when possible. |
| **`CG002`** | `info` | `cache_control` | `provider_output` | Bifrost inserted a cache-control marker before forwarding the request. | This may be expected. Review provider prompt-cache settings if cache usage is unexpected. |
| **`CG003`** | `info` | `provider_usage` | `provider_usage` | The provider reported zero cached tokens although the observed request structure was stable. The cause is not established. | Check provider cache eligibility, minimum prompt length, TTL, routing, and relevant caching configuration. |
| **`CG004`** | `info` | `gateway_state` | `gateway_state` | CacheGuard cannot determine whether provider-bound content changed because outbound inspection is disabled. | Use the isolated diagnostic workflow with synthetic requests to inspect outbound transformations. |
| **`CG005`** | `info` | `session` | `client_input` | Cache history was isolated because this request used a different provider or model. | Ensure requests within the same cache session target the same provider and model. |
| **`CG006`** | `info` | `cache_control` | `client_input` | Multiple cache-control breakpoints observed on OpenRouter request. OpenRouter uses only the final breakpoint for Gemini models. | Verify that the final breakpoint corresponds to your intended static prompt prefix. |
| **`CG007`** | `info` | `cache_control` | `provider_output` | Bifrost reduced the number of cache-control markers before forwarding to the provider. | Ensure client markers do not exceed provider-supported breakpoint limits. |

---

## Response Headers

When active, StillMark adds non-sensitive diagnostic headers to HTTP responses:

- `X-Cacheguard-Status`: `active`
- `X-Cacheguard-Observed`: Total count of observed requests by this plugin instance.
- `X-Cacheguard-Diagnosis`: `initial_request`, `confirmed_drift`, `stable_hit`, `stable_miss`, `untracked`, or `refused`.
- `X-Cacheguard-Attribution`: `client_input_changed`, `gateway_output_changed`, `origin_unverified`, or `none`.
- `X-Cacheguard-Drift-Component`: `tools` or `system_prompt` (when drift is detected).
- `X-Cacheguard-Drift-Path`: Sanitized schema path (e.g. `tools[0].function.parameters.properties.node_id.type`).
- `X-Cacheguard-Drift-Type`: `modified`, `added`, or `removed`.
- `X-Cacheguard-Transformation`: `cache_control_injected`, `cache_control_clamped`, or `multi_marker_observed`.
- `X-Cacheguard-Finding-Code`: Primary diagnostic code (e.g. `CG001`, `CG002`, `CG003`).
- `X-Cacheguard-Finding-Severity`: `info`, `warning`, or `error`.
- `X-Cacheguard-Finding-Message`: Plain-English explanation.
- `X-Cacheguard-Cached-Tokens`: Cached token count reported by provider (or mock).

---

## One-Command Installation & Management

StillMark includes an auditable, safe Go installer that verifies prerequisites, compiles the `.so` plugin against your exact Bifrost checkout, checks binary symbol compatibility, and safely activates the plugin in `config.json`.

### Prerequisites
- **Operating System**: Linux (`amd64`, `arm64`) or macOS (`darwin/amd64`, `darwin/arm64`). *(Windows is not supported by Go's standard `plugin` package).*
- **Go Toolchain**: Go 1.24+ (matching your Bifrost build compiler).
- **CGO Compiler**: `gcc` or `clang` in PATH.

### 1. Build and Install (Without Activation)
```bash
make install BIFROST_DIR=/path/to/Bifrost
```
Compiles `cacheguard.so`, places it at `<BIFROST_DIR>/plugins/cacheguard/build/cacheguard.so`, and verifies its symbols without altering any gateway configuration.

### 2. One-Command Build, Install, and Automatic Activation
```bash
make install BIFROST_DIR=/path/to/Bifrost CONFIG_PATH=/path/to/Bifrost/config.json
```
- Creates an automatic, timestamped backup (`config.json.bak.<timestamp>`).
- Safely registers or updates the `cacheguard` entry in `config.json`.
- Idempotent: running multiple times never duplicates configuration entries.

### 3. Dry-Run Mode
Preview exactly what the installer would do without modifying any files:
```bash
make install BIFROST_DIR=/path/to/Bifrost CONFIG_PATH=/path/to/Bifrost/config.json DRY_RUN=1
```

### 4. Verify Installed Plugin Binary
Verify that an installed plugin `.so` is valid and exports all required Bifrost hook symbols:
```bash
make verify BIFROST_DIR=/path/to/Bifrost
```

### 5. Safe One-Command Uninstall
```bash
make uninstall BIFROST_DIR=/path/to/Bifrost CONFIG_PATH=/path/to/Bifrost/config.json
```
- Removes the compiled `cacheguard.so` file and its empty directory.
- Deactivates StillMark from `config.json` (with automatic backup).
- **Preserves** all other Bifrost plugins and configuration.
- Refuses dangerous or ambiguous paths (such as `/` or empty arguments).

---

## Configuration Reference

Manual configuration entry for `config.json`:

```json
{
  "plugins": [
    {
      "name": "cacheguard",
      "type": "shared_object",
      "path": "/absolute/path/to/plugins/cacheguard/build/cacheguard.so",
      "enabled": true,
      "config": {
        "enabled": true,
        "diagnostic_logging": true,
        "severity_threshold": "info",
        "max_sessions": 10000,
        "max_entries_per_session": 10,
        "session_ttl": "30m",
        "raw_outbound_inspection": false
      }
    }
  ]
}
```

### Client Request Header
To track cache drift across requests, clients provide an opt-in session header:
```http
X-Cacheguard-Session-Id: user-session-12345678
```
Valid format: 8–64 alphanumeric characters, underscores, or hyphens (`^[a-zA-Z0-9_-]{8,64}$`). Requests without this header pass through normally without session state tracking.

---

## Real Gateway Performance Benchmarks

Measured on an AMD Ryzen 5 Linux environment (600 requests per mode, concurrency = 6, local mock provider):

| Mode | Gateway Setup | Requests/sec | Mean Latency | p50 Latency | p95 Latency | p99 Latency | p50 Delta vs Stock |
|:---|:---|:---:|:---:|:---:|:---:|:---:|:---:|
| **Mode A** | Stock Bifrost (Baseline, No Plugin) | 2,457.0 | 2.38 ms | 2.08 ms | 4.23 ms | 5.72 ms | — |
| **Mode B** | StillMark Enabled (No Session ID) | 2,369.5 | 2.46 ms | 2.16 ms | 4.06 ms | 5.48 ms | **+85 µs (+4.08%)** |
| **Mode C** | StillMark Enabled (Session Tracking + Nested Tools) | 1,705.3 | 3.43 ms | 2.86 ms | 6.73 ms | 11.40 ms | **+783 µs (+37.71%)** |

### Micro-Benchmarks (`go test -bench=. -benchmem`)
```text
BenchmarkComputeHMAC-8            468,705 ops    2,412 ns/op       664 B/op      9 allocs/op
BenchmarkFlattenToolSchemas-8      11,298 ops  109,314 ns/op    19,788 B/op    233 allocs/op
BenchmarkSessionTrackerLookup-8 11,687,527 ops    101.7 ns/op        0 B/op      0 allocs/op
BenchmarkDiagnosticEvaluation-8   461,870 ops    3,131 ns/op       672 B/op     10 allocs/op
```

---

## Quickstart & 7-Step Reproducible Demo

Run the full 7-step demonstration locally with **zero paid APIs** using synthetic credentials and local mock backends:

```bash
cd plugins/cacheguard
make demo
```

The automated demo walks through:
1. Running stock Bifrost without the plugin.
2. Compiling and loading the StillMark `.so` plugin into stock Bifrost.
3. Sending an initial tracked request (`initial_request`).
4. Sending an identical request with simulated cache hit (`stable_hit`, 150 cached tokens).
5. Mutating a nested tool schema parameter (`string` -> `integer`), verifying `confirmed_drift` and `CG001`.
6. Sending a stable request with zero cached tokens, verifying unknown-cause miss diagnosis `CG003`.
7. Sending an Anthropic message through Bifrost, proving detection of automatic cache-control injection (`CG002`).

---

## Testing Commands

From within the `plugins/cacheguard` directory:

```bash
# Run 43 unit and security regression tests
make test

# Run race condition detector
make test-race

# Run 3-mode real gateway performance benchmark
make bench

# Run 7-step reproducible demo
make demo

# Run live gateway integration test suite
make integration

# Run comprehensive installer test suite (Tests A through J)
make test-installer
```

---

## Troubleshooting Guide

### 1. `plugin was built with a different version of package`
Go plugins require that both the plugin (`.so`) and the host binary (`bifrost-http`) are compiled with:
- The exact same Go compiler version
- The exact same CGO settings (`CGO_ENABLED=1`)
- Identical shared dependency package versions

**Resolution**: Recompile using the installer pointing to your Bifrost directory:
```bash
make install BIFROST_DIR=/path/to/Bifrost
```

### 2. Missing `X-Cacheguard-*` Response Headers
- Ensure `X-Cacheguard-Session-Id` matches `^[a-zA-Z0-9_-]{8,64}$`.
- Confirm that CacheGuard is listed and `enabled: true` in `config.json`.
- Verify the virtual key or tenant ID is recognized by Bifrost governance.

### 3. Outbound Verification Unavailable (`CG004`)
By default, `raw_outbound_inspection` is `false` to avoid copying raw outbound bytes in production. `CG004` is only emitted when client inputs are identical but outbound bytes cannot be inspected. To inspect outbound transformations during diagnostic testing, enable `send_back_raw_request: true` on the provider config in an isolated test environment.

---

## License & Attribution

StillMark is distributed under the **Apache-2.0 License**, matching the Bifrost project.  
Bifrost is an open-source project by Maxim AI Inc. (Apache-2.0).
