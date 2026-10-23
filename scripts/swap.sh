#!/usr/bin/env bash
# swap.sh — 把 :next 镜像切换为线上容器，失败自动回滚。
#
# 为什么写成独立脚本并后台跑：这个容器正是当前对话所用模型的提供方，
# 切换过程中正在进行的推理会中断。脚本必须能脱离 agent 回合独立跑完，
# 否则「中断后没人收尾」会留下半启动状态。
#
# 判据（任一不满足即回滚）：
#   1) 容器 30s 内进入 healthy
#   2) /healthz 返回 service=workbuddy2api 且 healthy>0
#   3) /panel/api/overview 返回 200（证明 auths 与面板都正常）
set -uo pipefail

ROOT="/root/.openclaw/workspace/workbuddy2api-panel"
LOG="/root/.openclaw/workspace/workbuddy2api-panel/data/swap.log"
NEW_IMAGE="workbuddy2api-panel-wb2api:next"
OLD_IMAGE="workbuddy2api-panel-wb2api:rollback-20261023-2238"

exec >>"$LOG" 2>&1
echo "======== $(date '+%F %T') swap start ========"

cd "$ROOT" || exit 1

KEY=$(python3 -c "import json;print(json.load(open('config.json'))['api_key'])")
AUTH="Bear""er $KEY"

fail() {
    echo "[FAIL] $1"
    echo "---- 回滚到 $OLD_IMAGE ----"
    docker rm -f workbuddy2api >/dev/null 2>&1
    docker run -d --name workbuddy2api --restart unless-stopped \
        --user 10001:10001 -e TZ=Asia/Shanghai -p 7863:7863 \
        -v "$ROOT/auths:/app/auths" \
        -v "$ROOT/data:/app/data" \
        -v "$ROOT/config.json:/app/config.json" \
        "$OLD_IMAGE" >/dev/null
    sleep 8
    echo "---- 回滚后 healthz ----"
    curl -s -m 5 http://127.0.0.1:7863/healthz || echo "(回滚后仍不可达)"
    echo "======== $(date '+%F %T') swap FAILED, rolled back ========"
    exit 1
}

echo "---- 停旧容器 ----"
docker rm -f workbuddy2api >/dev/null 2>&1
sleep 1

echo "---- 起新容器 ----"
docker run -d --name workbuddy2api --restart unless-stopped \
    --user 10001:10001 -e TZ=Asia/Shanghai -p 7863:7863 \
    -v "$ROOT/auths:/app/auths" \
    -v "$ROOT/data:/app/data" \
    -v "$ROOT/config.json:/app/config.json" \
    "$NEW_IMAGE" >/dev/null || fail "docker run 失败"

echo "---- 等待 healthy（最长 60s）----"
for i in $(seq 1 30); do
    st=$(docker inspect -f '{{.State.Health.Status}}' workbuddy2api 2>/dev/null || echo none)
    if [[ "$st" == "healthy" ]]; then
        echo "容器 healthy（${i}00ms 内）"
        break
    fi
    if [[ "$i" == "30" ]]; then
        echo "容器未在 60s 内 healthy（当前 $st）"
        docker logs --tail 30 workbuddy2api
        fail "健康检查超时"
    fi
    sleep 2
done

echo "---- 校验 /healthz ----"
HZ=$(curl -s -m 8 http://127.0.0.1:7863/healthz)
echo "$HZ"
echo "$HZ" | grep -q '"service":"workbuddy2api"' || fail "/healthz 缺 service 标识"
echo "$HZ" | grep -qE '"healthy":[1-9]' || fail "/healthz healthy=0（没有可用账号）"

echo "---- 校验面板 overview ----"
CODE=$(curl -s -m 15 -o /tmp/ov.json -w '%{http_code}' \
    -H "Authorization: $AUTH" http://127.0.0.1:7863/panel/api/overview)
echo "overview HTTP=$CODE"
[[ "$CODE" == "200" ]] || fail "面板 overview 返回 $CODE"

echo "---- 校验版本与新增字段 ----"
docker exec workbuddy2api sh -c 'ls /app/wb2api' >/dev/null || fail "二进制缺失"
python3 - <<'PY'
import json
d = json.load(open('/tmp/ov.json'))
print("账号数:", d.get("total"), "healthy:", d.get("healthy"), "version:", d.get("version"))
PY

echo "---- 新镜像转正为 latest ----"
docker tag "$NEW_IMAGE" workbuddy2api-panel-wb2api:latest
echo "======== $(date '+%F %T') swap OK ========"
