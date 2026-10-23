#!/usr/bin/env bash
# spike_bypass.sh — 方案第 1 步 spike 的一键执行器。
#
# 为什么需要它：裸 `go run` 每次都在新容器里重下依赖（goproxy.cn 走外网），
# 单次 3~5 分钟且容易撞超时；这里把 GOMODCACHE 挂到宿主机缓存目录、
# 先 build 再 run，第二次起 1 秒内出结果。
#
# 用法:
#   bash scripts/spike_bypass.sh            # 旁路直连
#   bash scripts/spike_bypass.sh gateway    # 经网关（对照组）
set -uo pipefail
cd "$(dirname "$0")/.." || exit 3

MODE="${1:-bypass}"
CACHE="$PWD/.gocache"
mkdir -p "$CACHE/mod" "$CACHE/build"
GOENV=(-e GOPROXY=https://goproxy.cn,direct -e GOMODCACHE=/gocache/mod -e GOCACHE=/gocache/build)

# 只校验 usage.json：state.json 由池子自己的 flusher 每 5s 重写（无探测时也会变），
# 拿它当判据会产生假阴性。这一点已用「无探测对照」实测确认过。
hash_state() {
    sha256sum data/usage.json 2>/dev/null
}

if [[ "$MODE" == "bypass" ]]; then
    echo "== 旁路直连（internal/upstream.ChatStream，不经 Pool/Usage/handler）" >&2
    before=$(hash_state)
    docker run --rm \
        -v "$PWD":/src:ro -v "$CACHE":/gocache \
        "${GOENV[@]}" -w /src golang:1.22-alpine \
        sh -c 'go run scripts/spike_bypass.go -auths /src/auths -mode bypass' 2>/dev/null
    rc=$?
    after=$(hash_state)
    echo "---- 台账指纹 ----" >&2
    echo "before: $before" >&2
    echo "after : $after" >&2
    [[ "$before" == "$after" ]] && echo "usage.json 未变化 ✅（探测未记账）" >&2 || echo "usage.json 被污染 ❌" >&2
    exit $rc
fi

# gateway 对照组：同一个账号前缀、同一个模型、同样的最小 body，经网关发一次。
# 这一组**会**污染台账——正是要拿它当反证。
echo "== 经网关 /v1/chat/completions（对照组，会污染台账）" >&2
before=$(hash_state)
python3 - <<'PY'
import json, time, urllib.request

cfg = json.load(open("config.json", encoding="utf-8"))
key = cfg["api_key"]
body = {
    "model": "cn:deepseek-v4.1-flash",
    "messages": [{"role": "user", "content": "1"}],
    "max_tokens": 1,
    "stream": False,
}
req = urllib.request.Request(
    "http://127.0.0.1:7863/v1/chat/completions",
    data=json.dumps(body).encode(),
    headers={"Authorization": "Bearer " + key, "Content-Type": "application/json",
             "User-Agent": "OpenClaw-probe-spike/1"},
)
t0 = time.time()
with urllib.request.urlopen(req, timeout=60) as r:
    status = r.status
    raw = r.read()
total = (time.time() - t0) * 1000
d = json.loads(raw)
usage = d.get("usage") or {}
print(json.dumps({
    "mode": "gateway",
    "status": status,
    "total_ms": int(total),
    "model": d.get("model"),
    "finish_reason": (d.get("choices") or [{}])[0].get("finish_reason"),
    "usage_keys": sorted(usage.keys()),
    "usage": usage,
}, ensure_ascii=False, indent=2))
PY
rc=$?
after=$(hash_state)
echo "---- 台账指纹 ----" >&2
echo "before: $before" >&2
echo "after : $after" >&2
[[ "$before" == "$after" ]] && echo "未变化（异常：对照组本应记账）" >&2 || echo "已变化（预期：对照组本就记账）" >&2
exit $rc
