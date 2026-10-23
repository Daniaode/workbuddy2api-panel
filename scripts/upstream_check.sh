#!/usr/bin/env bash
# upstream_check.sh — 检查上游 linguo2625469/workbuddy2api-panel 是否有新提交。
#
# 上游 2026-10-21 已声明停更，但「声明停更」与「永不提交」不是一回事：
# 作者仍可能悄悄修一个致命 bug。这里只在**确有新提交**时出声，其余时候
# 输出 NO_REPLY 让自动化静默（命令型自动化按沉默标记处理）。
#
# 用法: bash scripts/upstream_check.sh
# 退出码: 0 正常（含无更新）；非 0 = gh 查询失败，自动化应视为异常。
set -uo pipefail

UPSTREAM_OWNER="linguo2625469"
UPSTREAM_REPO="${UPSTREAM_OWNER}/workbuddy2api-panel"
FORK_REPO="Daniaode/workbuddy2api-panel"

if ! command -v gh >/dev/null 2>&1; then
    echo "upstream_check: 未安装 gh" >&2
    exit 3
fi

# 跨仓库 compare 的 head 必须写成 "<owner>:<branch>"，写成完整 repo 路径会 404
# （GitHub 只认 fork 关系里的 owner 前缀，不解析第二段路径）。
if ! cmp=$(gh api "repos/${FORK_REPO}/compare/main...${UPSTREAM_OWNER}:main" 2>&1); then
    echo "upstream_check: 查询失败: ${cmp}" >&2
    exit 2
fi

ahead=$(printf '%s' "$cmp" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("ahead_by",0))')
status=$(printf '%s' "$cmp" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("status",""))')

if [[ "$ahead" == "0" ]]; then
    echo "NO_REPLY"
    exit 0
fi

echo "**上游有新提交**（${UPSTREAM_REPO}，${ahead} 个，status=${status}）"
echo
printf '%s' "$cmp" | python3 - <<'PY'
import json, sys
d = json.load(sys.stdin)
for c in d.get("commits", [])[-10:]:
    msg = (c["commit"]["message"].splitlines() or [""])[0]
    print(f"- {c['sha'][:7]} {msg}")
print()
print("同步方式：")
print("```bash")
print("cd /root/.openclaw/workspace/workbuddy2api-panel")
print("git fetch upstream && git log --oneline upstream/main ^main   # 先看差异")
print("git merge upstream/main   # 或按提交 cherry-pick")
print("```")
PY
