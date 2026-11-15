# StillMark CLI (`stillmark`)

> One-command installer, live verifier and diagnostics for **StillMark**, the prompt-cache integrity plugin for Bifrost.

---

## What is StillMark?

[Bifrost](https://github.com/maximhq/bifrost) is a high-performance AI gateway unifying 20+ LLM providers behind an OpenAI-compatible API.

**StillMark** is an operator-focused Bifrost plugin for Bifrost that tracks prompt cache prefix stability, attributes cache-busting drift across multi-turn sessions, and surfaces actionable diagnostic findings (`CG001`-`CG007`) without storing sensitive user payloads.

`stillmark` automates the whole workflow on a stock Bifrost source checkout: copy the plugin in, register it in `go.work`, build the gateway and `stillmark.so` from the same source, activate it in `config.json` (with backup), and verify it live on an isolated port against a local mock provider.

---

## Requirements

- macOS or Linux (Go plugins do not support Windows)
- Node.js 18+
- Go 1.22+ (Go downloads the exact version Bifrost pins automatically)
- A C compiler (`xcode-select --install` on macOS, `gcc` on Linux)
- A Bifrost **source checkout**: `git clone --depth 1 https://github.com/maximhq/bifrost.git`

Precompiled Bifrost binaries and Docker images are not supported: a Go plugin only loads into a gateway built from the same source and dependency versions, so the CLI builds both.

---

## Quick Start

> **Running from a clone instead of npm?** Replace `npx stillmark` with `node cli/bin/stillmark.mjs` (from the repo root). No `npm install` is needed; the CLI has zero dependencies.

```bash
git clone --depth 1 https://github.com/maximhq/bifrost.git
npx stillmark install --bifrost ./bifrost
```

Expected result (about 1–2 minutes the first time):

```text
  • Initial Turn Baseline Recording: PASS
  • Structurally Stable Repeat Turn: PASS
  • Changed System Prompt Flagged as Drift: PASS (confirmed_drift, system_prompt, SM008)
  • Untracked Request Pass-through: PASS
```

More:

```bash
npx stillmark doctor  --bifrost ./bifrost                                  # check prerequisites
npx stillmark install --bifrost ./bifrost --config ./config.json --dry-run # preview, writes nothing
npx stillmark install --bifrost ./bifrost --config ./config.json           # install + activate
npx stillmark verify  --bifrost ./bifrost                                  # re-run live checks
npx stillmark uninstall --bifrost ./bifrost --config ./config.json --clean-artifacts
```

---

## Command Reference

### Commands
- `doctor`: Checks Node.js, Go, and that `--bifrost` is a buildable Bifrost source checkout.
- `install`: Copies the plugin into `<bifrost>/plugins/stillmark`, registers it in `go.work` (created if missing), builds `<bifrost>/dist/bifrost-http` and `<bifrost>/plugins/stillmark/build/stillmark.so`, activates it in `--config` if given, then verifies.
- `verify`: Starts the built gateway with the plugin on an isolated port (default 8086, never 8080) against a local mock provider and checks the `X-Stillmark-*` headers, including that a changed system prompt is flagged as drift.
- `uninstall`: Deactivates StillMark in `config.json` (backup first, other plugins untouched); with `--clean-artifacts`, removes the built gateway and `stillmark.so`.

### Options
- `--bifrost <path>`: Bifrost source checkout.
- `--config <path>`: Bifrost `config.json` to activate/deactivate StillMark in.
- `--plugin-src <path>`: StillMark plugin source (default: this repo's `plugins/stillmark`, or the copy bundled in the npm package).
- `--dry-run`: Show what would happen without writing files or building.
- `--port <port>`: Preferred port for verification (default 8086).
- `--skip-verify`: Skip live verification during install.
- `--clean-artifacts`: Remove built binaries during uninstall.
- `--verbose`: Show gateway logs and build output.
- `--json`: Machine-readable output.
- `--help, -h` / `--version, -v`

---

## Security Guarantees

1. **No Untrusted Shell Execution**: All external commands (`go`, `git`) use `child_process.execFile` with direct argument arrays. No shell interpolation or `sh -c` is ever used.
2. **Never Modifies Running Binaries**: Builds write to a temporary file and are atomically renamed into place.
3. **Atomic Configuration Updates**: `config.json` updates are written to `.config.<pid>.<time>.tmp` in the same directory, verified as valid JSON, fsynced to disk, and atomically renamed.
4. **Collision-Safe Backups**: Backups use `<config>.bak.<timestamp>_<attempt>` and preserve original file permissions (capped at `0o600`).
5. **Symlink Escape & Path Traversal Prevention**: All file paths are strictly checked to ensure they do not traverse outside their base directories or operate through symbolic links.
6. **No Port 8080 Interference**: Isolated gateway tests use dynamic/ephemeral ports (default: 8086), strictly protecting development and production instances.
7. **Zero Hidden Telemetry**: No analytics, phone-home, or credential logging.

---

## License

Apache License 2.0. See [LICENSE](./LICENSE) for details.

