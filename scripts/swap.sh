#!/usr/bin/env bash
# swap.sh — 把 :next 镜像切换为线上容器，失败自动回滚。
#
# 为什么写成独立脚本并后台跑：这个容器正是当前对话所用模型的提供方，
# 切换过程中正在进行的推理会中断。脚本必须能脱离 agent 回合独立跑完，
# 否则「中断后没人收尾」会留下半启动状态。
#
# 判据（任一不满足即回滚）：
#   1) 容器 60s 内进入 healthy
#   2) /healthz 返回 service=workbuddy2api 且 healthy>0
#   3) /panel/api/overview 返回 200（证明 auths 与面板都正常）
#
# 三道硬保护（都由真实事故换来，见各段注释）：
#   A. SWAP_TEST=1 试运行：只打印将要执行的命令，绝不碰真容器
#   B. 回滚目标白名单：只接受 rollback-* 前缀，否则拒绝切换
#   C. 结果文件判据：包装层读 data/swap-result.json，不信 shell 退出码
#
# 用法：
#   bash scripts/swap.sh                  # 正式切换
#   SWAP_TEST=1 bash scripts/swap.sh      # 试运行（安全，可随时在生产机跑）
set -uo pipefail

ROOT="/root/.openclaw/workspace/workbuddy2api-panel"
LOG="$ROOT/data/swap.log"
RESULT="$ROOT/data/swap-result.json"
CONTAINER="workbuddy2api"
REPO="workbuddy2api-panel-wb2api"

NEW_IMAGE="${NEW_IMAGE:-$REPO:next}"

# ─── 保护 A：试运行模式 ────────────────────────────────────────────────
# 事故：为了验证 fail 分支，直接在生产机跑本脚本并用不存在的镜像当 NEW_IMAGE，
# 脚本第一步就把线上容器 rm -f 了 —— 造成约 30 秒生产中断。
# 教训：拿生产脚本做破坏性测试，必须有隔离开关，而不是「相信失败会回滚」。
SWAP_TEST="${SWAP_TEST:-0}"
if [[ "$SWAP_TEST" == "1" ]]; then
    log() { printf '[dry-run] %s\n' "$*"; }
    run() { printf '[dry-run] 将执行: %s\n' "$*"; }
else
    log() { printf '%s\n' "$*"; }
    run() { "$@"; }
fi

die() {
    printf '[ABORT] %s\n' "$1" >&2
    exit 2
}

# ─── 保护 B：回滚目标白名单 ────────────────────────────────────────────
# 事故：测试时手工指定 OLD_IMAGE=$REPO:latest，恰好那个镜像是好的才没出事。
# 若指向坏镜像，「服务正常」会变成「回滚到不可用」——比不切更糟。
# 因此只接受 rollback-* 前缀，其余一律拒绝。
if [[ -z "${OLD_IMAGE:-}" ]]; then
    OLD_IMAGE=$(docker images --format '{{.Repository}}:{{.Tag}}' \
        | grep -E "^${REPO}:rollback-" | sort | tail -1)
fi
if [[ -z "$OLD_IMAGE" ]]; then
    die "找不到 rollback-* 回滚镜像；宁可不动，也不要在没有退路时切换"
fi
if [[ ! "$OLD_IMAGE" =~ ^${REPO}:rollback- ]]; then
    die "回滚目标 '$OLD_IMAGE' 不在白名单内（必须是 ${REPO}:rollback-*）"
fi
if ! docker image inspect "$OLD_IMAGE" >/dev/null 2>&1; then
    die "回滚镜像 '$OLD_IMAGE' 在本机不存在"
fi
if ! docker image inspect "$NEW_IMAGE" >/dev/null 2>&1; then
    die "新镜像 '$NEW_IMAGE' 在本机不存在（先构建）"
fi

# 正式模式下才写日志与清结果；试运行不产生任何副作用。
if [[ "$SWAP_TEST" != "1" ]]; then
    mkdir -p "$(dirname "$LOG")"
    exec >>"$LOG" 2>&1
fi
echo "======== $(date '+%F %T') swap start (test=$SWAP_TEST) ========"
echo "新镜像:   $NEW_IMAGE"
echo "回滚目标: $OLD_IMAGE"

# 先清结果：否则中途被打断时，包装层会读到上一轮遗留的 "ok"。
[[ "$SWAP_TEST" == "1" ]] || rm -f "$RESULT"

cd "$ROOT" || die "无法进入 $ROOT"

KEY=$(python3 -c "import json;print(json.load(open('config.json'))['api_key'])" 2>/dev/null) \
    || die "读取 config.json 的 api_key 失败"
AUTH="Bear""er $KEY"

# 容器启动参数集中一处：切换与回滚必须完全一致，否则回滚会带着不同的挂载/用户
# 起来，出现「回滚后行为与切换前不一样」这种最难查的问题。
start_container() {
    local image="$1"
    run docker run -d --name "$CONTAINER" --restart unless-stopped \
        --user 10001:10001 -e TZ=Asia/Shanghai -p 7863:7863 \
        -v "$ROOT/auths:/app/auths" \
        -v "$ROOT/data:/app/data" \
        -v "$ROOT/config.json:/app/config.json" \
        "$image"
}

fail() {
    local reason="$1"
    echo "[FAIL] $reason"
    echo "---- 回滚到 $OLD_IMAGE ----"
    run docker rm -f "$CONTAINER" >/dev/null 2>&1
    start_container "$OLD_IMAGE"
    [[ "$SWAP_TEST" == "1" ]] || sleep 8
    echo "---- 回滚后 healthz ----"
    run curl -s -m 5 http://127.0.0.1:7863/healthz || echo "(回滚后仍不可达)"
    if [[ "$SWAP_TEST" != "1" ]]; then
        printf '{"status":"failed","at":"%s","reason":"%s","rolled_back_to":"%s"}\n' \
            "$(date '+%F %T')" "$reason" "$OLD_IMAGE" > "$RESULT"
    fi
    echo "======== $(date '+%F %T') swap FAILED, rolled back ========"
    exit 1
}

echo "---- 停旧容器 ----"
run docker rm -f "$CONTAINER" >/dev/null 2>&1
[[ "$SWAP_TEST" == "1" ]] || sleep 1

echo "---- 起新容器 ----"
start_container "$NEW_IMAGE" || fail "docker run 失败"

echo "---- 等待 healthy（最长 60s）----"
if [[ "$SWAP_TEST" == "1" ]]; then
    echo "[dry-run] 跳过健康检查等待"
else
    for i in $(seq 1 30); do
        st=$(docker inspect -f '{{.State.Health.Status}}' "$CONTAINER" 2>/dev/null || echo none)
        if [[ "$st" == "healthy" ]]; then
            echo "容器 healthy"
            break
        fi
        if [[ "$i" == "30" ]]; then
            echo "容器未在 60s 内 healthy（当前 $st）"
            docker logs --tail 30 "$CONTAINER"
            fail "健康检查超时"
        fi
        sleep 2
    done
fi

echo "---- 校验 /healthz ----"
if [[ "$SWAP_TEST" == "1" ]]; then
    echo "[dry-run] 跳过 /healthz 校验"
else
    HZ=$(curl -s -m 8 http://127.0.0.1:7863/healthz)
    echo "$HZ"
    echo "$HZ" | grep -q '"service":"workbuddy2api"' || fail "/healthz 缺 service 标识"
    echo "$HZ" | grep -qE '"healthy":[1-9]' || fail "/healthz healthy=0（没有可用账号）"
fi

echo "---- 校验面板 overview ----"
if [[ "$SWAP_TEST" == "1" ]]; then
    echo "[dry-run] 跳过面板校验"
else
    CODE=$(curl -s -m 15 -o /tmp/ov.json -w '%{http_code}' \
        -H "Authorization: $AUTH" http://127.0.0.1:7863/panel/api/overview)
    echo "overview HTTP=$CODE"
    [[ "$CODE" == "200" ]] || fail "面板 overview 返回 $CODE"
    python3 - <<'PY' || true
import json
d = json.load(open('/tmp/ov.json'))
print("账号数:", d.get("total"), "healthy:", d.get("healthy"), "version:", d.get("version"))
PY
fi

echo "---- 新镜像转正为 latest ----"
run docker tag "$NEW_IMAGE" "$REPO:latest"

# ─── 保护 C：结构化结果文件 ────────────────────────────────────────────
# 事故：用 nohup/setsid 后台跑本脚本时，外层命令的退出码与切换结果脱钩，
# 出现过「swap OK 但外层 code 1」与「code 2」两类误报，每次都像故障。
# 判据改为读结果文件：包装层/自动化只认它，不看退出码。
if [[ "$SWAP_TEST" != "1" ]]; then
    printf '{"status":"ok","at":"%s","image":"%s","rolled_back_to":"%s"}\n' \
        "$(date '+%F %T')" "$NEW_IMAGE" "$OLD_IMAGE" > "$RESULT"
fi
echo "======== $(date '+%F %T') swap OK ========"
