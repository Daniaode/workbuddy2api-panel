#!/usr/bin/env bash
# upstream_check.sh — 检查上游 linguo2625469/workbuddy2api-panel 是否有新提交。
#
# 上游 2026-10-21 已声明停更，但「声明停更」与「永不提交」不是一回事：
# 作者仍可能悄悄修一个致命 bug。这里只在**确有新提交**时出声，其余时候
# 输出 NO_REPLY 让自动化静默（命令型自动化按沉默标记处理）。
#
# 判据用 git 而不是 gh API：Gateway 的定时任务环境没有 GH_TOKEN，
# 而公开仓库的 fetch 匿名可用，也不吃 API 限流。
#   - 已合并的上游提交是 main 的祖先，merge-base == upstream HEAD 即「无新提交」；
#   - 上游前进后 merge-base 落在后面，二者之间就是待同步的提交。
#
# 用法: bash scripts/upstream_check.sh
# 退出码: 0 正常（含无更新）；2 fetch 失败（自动化应视为异常，别当成没更新）。
set -uo pipefail

UPSTREAM_URL="https://github.com/linguo2625469/workbuddy2api-panel.git"
cd "$(dirname "$0")/.." || exit 3

# 上游 remote 可能不存在（新 clone 只带 origin）；补齐，不动 origin。
git remote get-url upstream >/dev/null 2>&1 || git remote add upstream "$UPSTREAM_URL"

if ! git fetch --quiet upstream main 2>/dev/null; then
    echo "upstream_check: 拉取上游失败（网络或仓库不可达）" >&2
    exit 2
fi

head=$(git rev-parse upstream/main)
base=$(git merge-base main upstream/main 2>/dev/null)

if [[ -z "$base" ]]; then
    echo "upstream_check: 本地 main 与上游无共同祖先，需人工确认" >&2
    exit 2
fi

if [[ "$head" == "$base" ]]; then
    echo "NO_REPLY"
    exit 0
fi

count=$(git rev-list --count "${base}..${head}")
echo "**上游有新提交**（linguo2625469/workbuddy2api-panel，${count} 个）"
echo
git log --oneline --no-decorate -10 "${base}..${head}" | sed 's/^/- /'
[[ "$count" -gt 10 ]] && echo "- …（另有 $((count - 10)) 个）"
cat <<'EOF'

同步方式（先看差异再合，别让上游盖掉本分支的面板与积分监视改动）：

```bash
cd /root/.openclaw/workspace/workbuddy2api-panel
git log --oneline upstream/main ^main
git merge upstream/main     # 冲突通常集中在 README / workflows
```
EOF
