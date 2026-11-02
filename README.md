# StillMark

### Prompt Cache Integrity & Diagnostics for Bifrost

**StillMark** is an open-source, operator-focused diagnostic plugin for the [Bifrost AI Gateway](https://github.com/maximhq/bifrost). It detects, attributes, and explains prompt-cache degradation and structural request drift across LLM calls without logging or exposing sensitive prompt text or credentials.

---

## The Real Problem: Silent Cache Invalidation

Modern LLM providers (Anthropic, OpenAI, DeepSeek, Google Gemini via OpenRouter) offer prompt caching that reduces input token costs by 50% to 90% and cuts response latency when identical prefixes or tool schemas are reused.

However, in production:
1. **Silent Schema Drift**: Subtle client changes—such as updating a tool parameter description, altering property types, or modifying static system prompts—silently bust provider prompt caches, resulting in sudden latency spikes and billing surprises.
2. **Attribution Ambiguity**: When a prompt cache miss occurs, operators struggle to identify the root cause:
   - Did the **client** modify the prompt or tool schemas?
   - Did the **gateway** inject or alter parameters (such as dynamic MCP tools or cache-control markers)?
   - Did the **provider** evict the cache due to cold-starts, TTL expiration, or internal cluster routing?
3. **Privacy Hazards**: Operators diagnosing caching issues often resort to capturing raw request/response payloads, risking leakage of confidential user prompts, internal customer IDs, and API secrets into shared log stores.

---

## What StillMark Does

StillMark intercepts requests within the Bifrost gateway pipeline (`PreLLMHook`, `PostLLMHook`, `HTTPTransportPreHook`, and `HTTPTransportResponseHeadersHook`) to provide:

- **Keyed Cryptographic Fingerprints (HMAC-SHA256)**: Hashes prompts, system instructions, and schema paths using process-local random keys generated in RAM, preventing rainbow-table attacks.
- **Canonical Nested Schema Flattening**: Recursively traverses arbitrary JSON schemas in tool definitions down to 16 levels deep (capped at 512 paths), pinpointing exact field mutations (e.g. `tools[0].function.parameters.properties.node_id.type`).
- **Conversation Prefix Awareness**: Distinguishes static cached prompt prefixes from normal multi-turn message appending. Appending new turns to an ongoing conversation preserves the static prefix and does not trigger false drift alarms.
- **Tenant, Provider & Model Isolation**: Session state is isolated strictly by authenticated tenant (`customer:<id>` or `virtual_key:<id>`), provider, and model. Unauthenticated or unverified requests are safely marked `untracked` or `refused`.
- **Fail-Open Fault Tolerance**: Every hook wraps internal operations in panic recovery handlers. Even in the event of an unexpected internal error, client inference requests are never interrupted.
- **Gateway Transformation Detection**: Proves when Bifrost legitimately inserts or clamps cache-control markers (e.g. Anthropic auto-injection) before dispatching to the provider.
- **Actionable Diagnostic Codes (`CG001`–`CG007`)**: Translates observations into stable codes with plain-English operator guidance directly in response headers and rate-limited logs.

---

## What StillMark Does NOT Prove

To maintain operator trust and avoid misleading claims:
- **Observational Only**: StillMark is an observational diagnostic tool. It **does not guarantee** provider cache hits or provider billing reductions.
- **No Automatic Request Rewriting**: StillMark does not mutate, reorder, or alter client prompts. It is fail-open by design.
- **No False Accusations of Provider Billing Errors**: StillMark distinguishes between observed data and inferred causes. For example, a stable miss is diagnosed as unknown-cause (`CG003`), rather than an unproven claim of provider malfunction or eviction.
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

## Repository Layout & Bifrost Integration Requirements

StillMark is built as an open-source Go plugin for Bifrost. Because Bifrost is structured as a multi-module Go workspace, StillMark's `go.mod` references Bifrost core schemas via relative replacement directives:

```go
replace github.com/maximhq/bifrost/core => ../../core
replace github.com/maximhq/bifrost/framework => ../../framework
```

### Development & Build Setup
To build or develop StillMark with a local Bifrost gateway:
1. Clone the Bifrost repository:
   ```bash
   git clone https://github.com/maximhq/bifrost.git
   cd bifrost
   ```
2. Place or link StillMark under `plugins/cacheguard`:
   ```bash
   # Clone or symlink StillMark into plugins/cacheguard
   git clone https://github.com/Rahulkumar0407/StillMark.git plugins/cacheguard
   ```
3. Add StillMark to `go.work` in the Bifrost root:
   ```go
   use (
       ./core
       ./framework
       ./plugins/cacheguard
       ./transports
   )
   ```
4. Build the plugin shared object (`.so`):
   ```bash
   cd plugins/cacheguard
   make build
   ```

Detailed configuration options, performance numbers, and deep-dive internals can be found in the [Plugin Documentation](plugins/cacheguard/README.md).

---

## Quickstart & 7-Step Reproducible Demo

You can run the full 7-step demonstration locally with **zero paid APIs** using synthetic credentials and local mock backends:

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

From within the `plugins/cacheguard` directory inside a Bifrost workspace:

```bash
# Run 41 unit tests
make test

# Run race condition detector
make test-race

# Run 3-mode real gateway performance benchmark
make bench

# Run 7-step reproducible demo
make demo

# Run live gateway integration test suite
make integration
```

---

## Limitations

- **Go Plugin ABI**: Standard Go plugins require matching Go compiler version, CGO settings (`CGO_ENABLED=1`), and package dependencies between the host gateway binary and the plugin shared object.
- **Operating Systems**: Supported on Linux (`amd64`, `arm64`) and macOS (`darwin/amd64`, `darwin/arm64`). Windows is not supported by Go's standard `plugin` package.
- **Outbound Inspection**: Production deployments should keep `raw_outbound_inspection: false` to avoid copying request bodies into memory during normal inference.

---

## License & Attribution

StillMark is distributed under the **Apache-2.0 License**, matching the Bifrost project.
Bifrost is an open-source project by Maxim AI Inc. (Apache-2.0).
