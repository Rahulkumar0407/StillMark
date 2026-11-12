# StillMark CLI (`@stillmark/cli`)

> Professional one-command installer, verification engine, and diagnostics manager for **StillMark** — the native prompt-cache integrity plugin for Bifrost.

---

## What is StillMark?

[Bifrost](https://github.com/maximhq/bifrost) is a high-performance AI gateway unifying 20+ LLM providers behind an OpenAI-compatible API.

**StillMark** is an operator-focused native compile-time plugin for Bifrost that tracks prompt cache prefix stability, attributes cache-busting drift across multi-turn sessions, and surfaces actionable diagnostic findings (`CG001`-`CG007`) without storing sensitive user payloads.

`@stillmark/cli` automates the end-to-end workflow: pre-flight checks, gateway compilation into a staging directory, atomic configuration updates, safe backups, and live isolated verification against an ephemeral mock provider.

---

## Compatibility Matrix

Because Go compiles into static machine binaries, native plugins must be compiled into the Bifrost gateway executable.

| Scenario | Bifrost Deployment Mode | StillMark Integration Status | Supported by `@stillmark/cli` | Automated Actions |
| :--- | :--- | :--- | :--- | :--- |
| **A** | **Bifrost Source Checkout with StillMark** | Native StillMark source & registration present | **YES (Native Mode)** | Builds staging binary, atomically updates `config.json`, runs isolated verification |
| **B** | **Clean Bifrost Source Checkout** | Stock upstream checkout without StillMark | **NO (Manual branch required)** | Explains why installation is blocked and instructs operator to checkout StillMark branch |
| **C** | **Precompiled Binary / Docker** | Standalone precompiled binary or container image | **NO (Compiled Go limitation)** | Explains that native plugins cannot be injected into precompiled Go binaries |
| **D** | **Legacy Prototype Checkout** | Legacy shared-object source (`plugins/cacheguard`) | **YES (via `--legacy`)** | Builds `.so` plugin via CGo `-buildmode=plugin` and updates config |

---

## Quick Start

### 1. Pre-flight Environment & Checkout Check
Diagnose your system toolchain, Go workspace, and Bifrost source compatibility:

```bash
npx @stillmark/cli doctor --bifrost /path/to/Bifrost --config /path/to/config.json
```

### 2. Complete One-Command Installation
Compile native Bifrost with StillMark, configure `config.json`, and verify operation against an isolated test gateway:

```bash
npx @stillmark/cli install --bifrost /path/to/Bifrost --config /path/to/config.json
```

### 3. Verify an Existing Gateway
Run live end-to-end checks on an isolated gateway instance without touching production:

```bash
npx @stillmark/cli verify --bifrost /path/to/Bifrost/dist/bifrost-http
```

### 4. Safe Deactivation
Deactivate StillMark in configuration with automatic backup and optional build artifact cleanup:

```bash
npx @stillmark/cli uninstall --config /path/to/config.json --clean-artifacts
```

---

## Command Reference

### Commands
- `doctor`: Inspects Node.js version, Go toolchain, Bifrost source checkout, and config file.
- `install`: Compiles native Bifrost gateway in a staging directory, updates `config.json`, and verifies against an isolated test instance.
- `verify`: Launches an isolated Bifrost gateway on an ephemeral test port (e.g. 8086), runs a local mock provider, and verifies diagnostic headers (`X-Stillmark-Status`, `X-Stillmark-Diagnosis`).
- `uninstall`: Transactionally deconfigures StillMark in `config.json` with safe timestamped backup, preserving all other plugins.

### Options
- `--bifrost <path>`: Path to the Bifrost source checkout directory.
- `--config <path>`: Path to Bifrost `config.json` for activation/deactivation.
- `--dry-run`: Preview all operations without modifying files or compiling binaries.
- `--legacy`: Enable explicit legacy shared-object (`.so`) plugin compilation mode.
- `--output-dir <path>`: Custom staging directory for the compiled gateway (default: `<bifrost>/dist`).
- `--port <port>`: Preferred port for isolated verification testing (default: 8086, never 8080).
- `--skip-verify`: Skip the isolated gateway verification step during installation.
- `--clean-artifacts`: Remove compiled staging binaries during uninstallation.
- `--verbose`: Enable detailed debug logging and compiler output.
- `--json`: Output machine-readable JSON for CI/CD automation.
- `--help, -h`: Show command help.
- `--version, -v`: Show CLI version.

---

## Security Guarantees

1. **No Untrusted Shell Execution**: All external commands (`go`, `git`) use `child_process.execFile` with direct argument arrays. No shell interpolation or `sh -c` is ever used.
2. **Never Modifies Running Binaries**: Native compilation outputs to a staging directory (`<bifrost>/dist/`) using temporary files and atomic rename.
3. **Atomic Configuration Updates**: `config.json` updates are written to `.config.<pid>.<time>.tmp` in the same directory, verified as valid JSON, fsynced to disk, and atomically renamed.
4. **Collision-Safe Backups**: Backups use `<config>.bak.<timestamp>_<attempt>` and preserve original file permissions (capped at `0o600`).
5. **Symlink Escape & Path Traversal Prevention**: All file paths are strictly checked to ensure they do not traverse outside their base directories or operate through symbolic links.
6. **No Port 8080 Interference**: Isolated gateway tests use dynamic/ephemeral ports (default: 8086), strictly protecting development and production instances.
7. **Zero Hidden Telemetry**: No analytics, phone-home, or credential logging.

---

## License

Apache License 2.0. See [LICENSE](./LICENSE) for details.

