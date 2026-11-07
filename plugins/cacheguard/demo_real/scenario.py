"""StillMark real demo: real requests through Bifrost to a local llama.cpp model.
Prints a live timeline of cache reuse and StillMark's findings. No API keys, no simulated numbers."""
import json, os, sys, time, urllib.error, urllib.request

GATEWAY = os.environ.get("GATEWAY_URL", "http://127.0.0.1:8081")
VK = "sk-bf-demo-vk"
REPORT = os.environ.get("REPORT_URL", "http://127.0.0.1:8099/stillmark/report")
C = {"g": "\033[32m", "r": "\033[31m", "y": "\033[33m", "c": "\033[36m", "b": "\033[1m", "d": "\033[2m", "x": "\033[0m"}
if not sys.stdout.isatty():
    C = {k: "" for k in C}

POLICY = " ".join(f"Rule {i}: For order {i:04d}, verify the customer record, confirm the shipping region, "
                  f"check the {i % 30 + 1}-day refund window, and log the outcome." for i in range(1, 60))
TOOL_V1 = "Look up an order by its ID."
TOOL_V2 = "Look up a customer order by its ID."
REFUND = "Issue a refund for an order."


def now():
    return time.strftime("%Y-%m-%d %H:%M:%S")


def tool(name, desc, swap_keys=False):
    params = {"type": "object", "properties": {"order_id": {"type": "string"}}, "required": ["order_id"]}
    if swap_keys:  # same schema, different JSON key order
        params = {"required": ["order_id"], "properties": {"order_id": {"type": "string"}}, "type": "object"}
    return {"type": "function", "function": {"name": name, "description": desc, "parameters": params}}


def tools(get_desc, reorder=False, swap_keys=False):
    t = [tool("get_order", get_desc, swap_keys), tool("refund_order", REFUND, swap_keys)]
    return t[::-1] if reorder else t


def send(system, user, tool_list):
    body = {"model": "openai/qwen", "max_tokens": 8,
            "messages": [{"role": "system", "content": system}, {"role": "user", "content": user}],
            "tools": tool_list}
    req = urllib.request.Request(f"{GATEWAY}/v1/chat/completions", data=json.dumps(body).encode(), headers={
        "Content-Type": "application/json", "Authorization": f"Bearer {VK}", "x-bf-vk": VK})  # no StillMark header needed
    try:
        with urllib.request.urlopen(req, timeout=180) as r:
            usage, h = json.load(r).get("usage", {}), r.headers
    except urllib.error.HTTPError as e:
        sys.exit(f"Gateway returned HTTP {e.code}: {e.read().decode()[:300]}")
    return usage, h


totals = {"requests": 0, "prompt": 0, "cached": 0}


def step(n, label, system, user, tool_list):
    usage, h = send(system, user, tool_list)
    p = usage.get("prompt_tokens", 0)
    c = (usage.get("prompt_tokens_details") or {}).get("cached_tokens", 0)
    totals["requests"] += 1; totals["prompt"] += p; totals["cached"] += c
    pct = 100 * c / p if p else 0
    col = C["g"] if pct >= 90 else C["y"] if pct >= 50 else C["r"]
    bar = "█" * round(pct / 5) + "░" * (20 - round(pct / 5))
    print(f" {n:>2}. {label:<34} {col}{bar} {pct:5.1f}%{C['x']}  {C['d']}cached {c}/{p}{C['x']}")
    diag, code = h.get("X-Cacheguard-Diagnosis", "?"), h.get("X-Cacheguard-Finding-Code")
    line = f"     StillMark: {diag}"
    if h.get("X-Cacheguard-Drift-Path"):
        line += f" · changed {C['b']}{h['X-Cacheguard-Drift-Path']}{C['x']}"
    if code:
        line += f" · {C['b']}{code}{C['x']}"
    print(line)
    if h.get("X-Cacheguard-Cache-Tokens-Lost"):
        print(f"     {C['r']}{h['X-Cacheguard-Cache-Tokens-Lost']} tokens billed at full price instead of cached ≈ "
              f"${h['X-Cacheguard-Est-Extra-Cost-Per-1k-Requests-USD']} extra per 1,000 requests{C['x']}")
    if code and h.get("X-Cacheguard-Finding-Message"):
        print(f"     {C['c']}{h['X-Cacheguard-Finding-Message']}{C['x']}")
    time.sleep(1.1)  # make sure the clock-based timestamp changes between requests


def act(title):
    print(f"\n{C['b']}{title}{C['x']}")


print(f"{C['b']}StillMark live demo{C['x']} — real requests · Bifrost gateway · local llama.cpp model")
print(f"{C['d']}Zero-config: the app sends no StillMark headers. Requests are grouped by virtual key + model.{C['x']}")
print(f"{C['d']}Cost uses reference prices $3.00 / $0.30 per million input tokens (configurable).{C['x']}")

act("Act 1 — The bug: a timestamp at the top of the system prompt")
for i in range(1, 4):
    step(i, "agent request (time in system prompt)", f"Current time: {now()}\nYou are a support agent.\n{POLICY}",
         "Say OK.", tools(TOOL_V1))

act("Act 2 — The fix: same system prompt every time, time moved to the user message")
for i in range(4, 7):
    step(i, "agent request (time in user message)", f"You are a support agent.\n{POLICY}",
         f"Current time: {now()}. Say OK.", tools(TOOL_V1))

act("Act 3 — A teammate edits one tool description")
step(7, "tool description edited", f"You are a support agent.\n{POLICY}", f"Current time: {now()}. Say OK.", tools(TOOL_V2))
step(8, "next request", f"You are a support agent.\n{POLICY}", f"Current time: {now()}. Say OK.", tools(TOOL_V2))

act("Act 4 — An MCP server returns the same tools in a different order")
step(9, "same tools, new order", f"You are a support agent.\n{POLICY}", f"Current time: {now()}. Say OK.", tools(TOOL_V2, True))
step(10, "next request", f"You are a support agent.\n{POLICY}", f"Current time: {now()}. Say OK.", tools(TOOL_V2, True))

act("Act 5 — Client sends the same tool schema with a different key order (Bifrost normalizes it: no false alarm)")
step(11, "same schema, new key order", f"You are a support agent.\n{POLICY}", f"Current time: {now()}. Say OK.", tools(TOOL_V2, True, True))
step(12, "next request", f"You are a support agent.\n{POLICY}", f"Current time: {now()}. Say OK.", tools(TOOL_V2, True, True))

print(f"\n{C['b']}Session total:{C['x']} {totals['requests']} real requests, "
      f"{100 * totals['cached'] / max(totals['prompt'], 1):.1f}% of prompt tokens served from cache.")

try:
    with urllib.request.urlopen(REPORT, timeout=5) as r:
        rep = json.load(r)
    t = rep["totals"]
    print(f"\n{C['b']}Operator report{C['x']} {C['d']}(GET {REPORT}){C['x']}")
    print(f"  {t['drift_events']} cache-breaking changes · {t['tokens_lost']} tokens billed at full price · "
          f"est. ${t['est_extra_usd']:.4f} extra in this session")
    print(f"  {'code':<6} {'what changed':<34} {'times':>5} {'tokens lost':>11}")
    for i in rep["top_issues"][:5]:
        print(f"  {i['code']:<6} {(i['path'] or i['component']):<34} {i['occurrences']:>5} {i['tokens_lost']:>11}")
except Exception as e:  # report is optional
    print(f"{C['d']}(operator report unavailable: {e}){C['x']}")
