#!/usr/bin/env bash
# probe.sh — 上游协议主动探测的执行入口（供 OpenClaw 自动化调用）。
#
# 为什么要有这层包装：自动化直接调 probe_bin 会在「二进制不存在」时以 127 退出，
# 与库定义的退出码语义（0/2/3/4）冲突，告警会变成「不明原因失败」。这里统一
# 转成 2（探测自身故障），并顺手处理二进制的缺失重建。
#
# 用法:
#   bash scripts/probe.sh l1        # 通道探活（每 15 分钟）
#   bash scripts/probe.sh l2        # 全量扫描（每 6 小时）
#   bash scripts/probe.sh l3        # 全保真（每天）
#
# 退出码：与 cmd/probe 一致（0 完成 / 2 自身故障 / 3 覆盖为零 / 4 全网络失败）
set -uo pipefail

LEVEL="${1:-l1}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/probe_bin"

if [[ ! -x "$BIN" ]]; then
    # 二进制不进版本库（.gitignore 已排除）。缺失时尝试本地重建；容器里没有 Go
    # 工具链，所以这里只在宿主机有 go 时有效，否则报 2 让人处理。
    if command -v go >/dev/null 2>&1; then
        ( cd "$ROOT" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o probe_bin ./cmd/probe ) || {
            echo "probe: 重建 probe_bin 失败" >&2
            exit 2
        }
    else
        echo "probe: 缺少 $BIN，且本机无 go 工具链；请先构建（见 README「上游协议主动探测」）" >&2
        exit 2
    fi
fi

cd "$ROOT" || exit 2
exec "$BIN" -mode "$LEVEL" -config config.json -auths auths -state data/state.json -data data
