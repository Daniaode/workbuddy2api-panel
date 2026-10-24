#!/usr/bin/env bash
# deploy-status.sh — 一眼看清「线上跑的是哪个版本、有多少改动还没部署」。
#
# 为什么需要：改成「攒一批再统一推送」之后，最容易出的事是忘了自己攒了什么、
# 或者以为已经部署了其实没有。这个脚本不依赖任何人工记账，直接比对
# 三处事实：容器镜像的构建时间 / 线上二进制里的版本串 / 仓库当前 HEAD。
#
# 用法: bash scripts/deploy-status.sh
# 退出码: 0 无待部署；1 有未部署改动
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CONTAINER="workbuddy2api"
REPO="workbuddy2api-panel-wb2api"
cd "$ROOT" || exit 2

echo "=== 线上 ==="
if ! docker ps --format '{{.Names}}' | grep -qx "$CONTAINER"; then
    echo "  容器未运行"
    exit 2
fi

RUNNING_IMAGE=$(docker inspect -f '{{.Config.Image}}' "$CONTAINER" 2>/dev/null)
# 保留完整的 RFC3339（含时区偏移）。此前用 cut -c1-19 把 "+09:00" 截掉，
# 于是 date -d 按本地时区重新解释，算出的 epoch 偏了 8–9 小时——镜像明明比提交旧，
# 却被判成「无待部署」。时区必须跟着时间串一起留着。
IMAGE_CREATED_FULL=$(docker inspect -f '{{.Created}}' "$RUNNING_IMAGE" 2>/dev/null)
IMAGE_CREATED="${IMAGE_CREATED_FULL%%[.+Z]*}"   # 仅用于显示：去掉偏移/纳秒/Z
IMAGE_CREATED="${IMAGE_CREATED%%.*}"
echo "  容器:     $CONTAINER  $(docker ps --filter name=$CONTAINER --format '{{.Status}}')"
echo "  镜像:     $RUNNING_IMAGE"
echo "  构建于:   ${IMAGE_CREATED//T/ }"

# 镜像里烧进去的源码版本：比 mtime 可靠（重建同代码也会变 mtime）。
SRC_AT_BUILD=$(docker inspect -f '{{index .Config.Labels "org.opencontainers.image.revision"}}' \
    "$RUNNING_IMAGE" 2>/dev/null)
[[ -z "$SRC_AT_BUILD" || "$SRC_AT_BUILD" == "<no value>" ]] && SRC_AT_BUILD="(镜像未打 revision 标签)"

echo
echo "=== 仓库 ==="
HEAD_SHA=$(git rev-parse --short HEAD 2>/dev/null)
echo "  HEAD:     $HEAD_SHA  $(git log -1 --format='%s' | cut -c1-56)"

# 未提交改动：这些一定没进镜像。
DIRTY=$(git status --porcelain | grep -vE '^\?\? data/' | wc -l | tr -d ' ')
echo "  未提交:   $DIRTY 个文件"
if [[ "$DIRTY" != "0" ]]; then
    git status --porcelain | grep -vE '^\?\? data/' | sed 's/^/            /' | head -10
fi

# origin 落后/领先：提醒是否已推送。
if git rev-parse --verify --quiet origin/main >/dev/null; then
    AHEAD=$(git rev-list --count origin/main..HEAD 2>/dev/null || echo 0)
    echo "  未推送:   $AHEAD 个提交"
fi

echo
echo "=== 判定 ==="

# 关键区分：不是所有改动都需要重建镜像。
#
# 镜像里只有 cmd/ internal/ 的 Go 代码 + 内嵌前端 + Dockerfile 复制的少数脚本。
# scripts/ 下的运维脚本（swap.sh / credit_watch.py / probe.sh …）跑在**主机侧**，
# 改完立刻生效，重建镜像毫无意义。把它们算进「待部署」会让这个检查失去可信度
# ——每次都报「有未部署」，人就会开始无视它，比不检查更糟。
needs_rebuild() {
    case "$1" in
        cmd/*|internal/*|Dockerfile|go.mod|go.sum|config.example.json) return 0 ;;
        login.sh|signin.sh|credit.sh|scripts/probe_active.py) return 0 ;;
        *) return 1 ;;
    esac
}

REBUILD_FILES=0
HOST_FILES=0
while IFS= read -r line; do
    [[ -z "$line" ]] && continue
    path="${line:3}"                       # 去掉 " M "/"?? " 前缀
    if needs_rebuild "$path"; then
        REBUILD_FILES=$((REBUILD_FILES+1))
        echo "  需重建:   $path"
    else
        HOST_FILES=$((HOST_FILES+1))
    fi
done < <(git status --porcelain | grep -vE '^\?\? data/')

if [[ "$HOST_FILES" != "0" ]]; then
    echo "  主机侧:   $HOST_FILES 个改动（已生效，无需重建镜像）"
fi

# 已提交但尚未重建的部分：只看触及镜像内容的提交。
LAST_COMMIT_TS=$(git log -1 --format='%ct')
IMAGE_TS=$(date -d "$IMAGE_CREATED_FULL" +%s 2>/dev/null || echo 0)

PENDING_COMMITS=""
if [[ "$IMAGE_TS" -lt "$LAST_COMMIT_TS" ]]; then
    PENDING_COMMITS=$(git log --format='%h %s' --since="@$IMAGE_TS" HEAD --         cmd internal Dockerfile go.mod go.sum config.example.json         login.sh signin.sh credit.sh scripts/probe_active.py 2>/dev/null)
fi

if [[ "$REBUILD_FILES" != "0" ]]; then
    echo "  ⚠ $REBUILD_FILES 个触及镜像内容的文件未提交 —— 需重建"
    exit 1
fi
if [[ -n "$PENDING_COMMITS" ]]; then
    echo "  ⚠ 有提交触及镜像内容但镜像更旧 —— 需重建"
    echo "$PENDING_COMMITS" | sed 's/^/      /' | head -20
    exit 1
fi
echo "  ✓ 镜像内容无待部署改动"
echo "  （脚本类改动无需重建，直接生效）"
exit 0
