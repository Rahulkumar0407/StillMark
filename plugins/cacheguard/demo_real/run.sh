#!/usr/bin/env bash
# One-command real demo: local llama.cpp model -> Bifrost (with StillMark) -> live timeline.
# Usage: BIFROST_DIR=/path/to/bifrost ./run.sh   (default: ../bifrost next to this repo)
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
PLUGIN_SRC="$(cd "$HERE/.." && pwd)"
BIFROST_DIR="${BIFROST_DIR:-$(cd "$PLUGIN_SRC/../../.." && pwd)/bifrost}"
MODEL="${MODEL:-Qwen/Qwen2.5-1.5B-Instruct-GGUF:Q4_K_M}"
LLAMA_PORT=8090; GW_PORT=8081
WORK="$(mktemp -d)"; PIDS=()

cleanup() { for p in "${PIDS[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null || true; done; rm -rf "$WORK"; }
trap cleanup EXIT

need() { command -v "$1" >/dev/null || { echo "Missing '$1'. $2"; exit 1; }; }
need go "Install with: brew install go"
need llama-server "Install with: brew install llama.cpp"
need python3 "Install Python 3."
[ -d "$BIFROST_DIR/core" ] || { echo "Bifrost checkout not found at $BIFROST_DIR (git clone https://github.com/maximhq/bifrost.git)"; exit 1; }
for port in $LLAMA_PORT $GW_PORT 8099; do
  if lsof -nP -iTCP:$port -sTCP:LISTEN >/dev/null 2>&1; then echo "Port $port is busy. Stop whatever is using it and retry."; exit 1; fi
done

echo "==> Building StillMark plugin and Bifrost (first run takes a few minutes)"
rsync -a --delete --exclude build --exclude demo_real "$PLUGIN_SRC/" "$BIFROST_DIR/plugins/cacheguard/"
( cd "$BIFROST_DIR" && [ -f go.work ] || make setup-workspace >/dev/null )
mkdir -p "$BIFROST_DIR/transports/bifrost-http/ui" "$BIFROST_DIR/tmp"
[ -n "$(ls -A "$BIFROST_DIR/transports/bifrost-http/ui")" ] || echo '<html></html>' > "$BIFROST_DIR/transports/bifrost-http/ui/index.html"
( cd "$BIFROST_DIR/transports/bifrost-http" && go build -o ../../tmp/bifrost-http . )
( cd "$BIFROST_DIR/plugins/cacheguard" && make build >/dev/null )

echo "==> Starting local model (fresh process = cold cache)"
llama-server -hf "$MODEL" --jinja -c 8192 --port $LLAMA_PORT --host 127.0.0.1 >"$WORK/llama.log" 2>&1 & PIDS+=($!)
for _ in $(seq 1 300); do curl -sf "http://127.0.0.1:$LLAMA_PORT/health" >/dev/null && break; sleep 1; done
curl -sf "http://127.0.0.1:$LLAMA_PORT/health" >/dev/null || { echo "Model failed to start:"; tail -20 "$WORK/llama.log"; exit 1; }

echo "==> Starting Bifrost with StillMark"
cat >"$WORK/config.json" <<JSON
{"\$schema":"https://www.getbifrost.ai/schema","setup_token":"local-demo-token","source_of_truth":"config.json",
 "client_config":{"port":"$GW_PORT","host":"127.0.0.1"},
 "plugins":[{"name":"cacheguard","type":"shared_object","path":"$BIFROST_DIR/plugins/cacheguard/build/cacheguard.so","enabled":true,
   "config":{"expose_schema_paths":true,"report_addr":"127.0.0.1:8099"}}],
 "providers":{"openai":{"send_back_raw_request":true,"keys":[{"name":"local","value":"sk-local","models":["qwen"]}],
   "network_config":{"base_url":"http://127.0.0.1:$LLAMA_PORT","allow_private_network":true}}},
 "governance":{"virtual_keys":[{"name":"Demo VK","id":"vk-demo","value":"sk-bf-demo-vk","is_active":true,
   "provider_configs":[{"provider":"openai","allowed_models":["*"],"key_ids":["*"],"weight":1.0}]}]}}
JSON
"$BIFROST_DIR/tmp/bifrost-http" -port $GW_PORT -host 127.0.0.1 -app-dir "$WORK" -log-level warn >"$WORK/bifrost.log" 2>&1 & PIDS+=($!)
for _ in $(seq 1 60); do curl -sf "http://127.0.0.1:$GW_PORT/health" >/dev/null && break; sleep 1; done
curl -sf "http://127.0.0.1:$GW_PORT/health" >/dev/null || { echo "Bifrost failed to start:"; tail -20 "$WORK/bifrost.log"; exit 1; }

python3 -I "${SCENARIO:-$HERE/scenario.py}"
