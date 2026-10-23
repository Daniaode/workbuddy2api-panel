#!/usr/bin/env python3
"""credit_watch.py — WorkBuddy 积分活动监视 / 报表（走面板 HTTP API）。

为什么不用 auths/ 直连上游：面板已经把「余额基线 + 积分流水 + 包明细 + 补偿状态」
聚合成只读接口，直连上游既要每号自己刷 token，又会和网关的冷却/租约抢额度。
这里只做**观测**，一个 GET 都不触发上游写操作。

用法
  python3 scripts/credit_watch.py report            # 全量报表（人读）
  python3 scripts/credit_watch.py report --json     # 原始快照
  python3 scripts/credit_watch.py watch             # 只报「有变化/需要处理」的项
  python3 scripts/credit_watch.py watch --quiet     # 无变化时输出 NO_REPLY（给自动化用）

退出码
  0 正常；2 面板不可达 / 鉴权失败（自动化里应当据此告警，而不是当成「没有变化」）
"""
from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.error
import urllib.request
from datetime import date, datetime, timedelta, timezone

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CONFIG = os.path.join(ROOT, "config.json")
STATE = os.path.join(ROOT, "data", "credit-watch-state.json")

# 与 config.json 的 pool.credit_floor 同口径：低于它，收费模型会被选号剔除。
DEFAULT_FLOOR = 100
# 快过期提醒窗口：到期前 72h 仍未用完的余额，等于要白扔。
EXPIRY_WARN_HOURS = 72
# 单次余额增长超过它才值得单独说一句（低于此值多半是基线抖动 / 小数折算）。
GAIN_NOISE = 50
# 单次余额下降超过它才提示（正常调用消耗本来就该扣，别天天喊）。
DROP_NOISE = 500
# 签到连续未成功多少天算异常。取 2 而不是 1：调度时点是 10:00 / 21:00，
# 当天 21:00 之前「今天还没签」是正常的，用 1 天会在每天上午稳定误报一次。
CHECKIN_STALE_DAYS = 2


class PanelError(RuntimeError):
    pass


def load_config() -> tuple[str, str]:
    """读面板地址与 api_key。配置缺失直接报错——不猜默认密钥。"""
    if not os.path.exists(CONFIG):
        raise PanelError(f"找不到 {CONFIG}")
    cfg = json.load(open(CONFIG, encoding="utf-8"))
    key = (cfg.get("api_key") or "").strip()
    listen = (cfg.get("listen") or ":7863").strip()
    if listen.startswith(":"):
        listen = "127.0.0.1" + listen
    elif listen.startswith("0.0.0.0"):
        listen = listen.replace("0.0.0.0", "127.0.0.1", 1)
    return f"http://{listen}", key


def get(base: str, key: str, path: str, timeout: float = 20.0):
    req = urllib.request.Request(
        base + path,
        headers={"Authorization": "Bearer " + key, "Accept": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return json.loads(r.read().decode("utf-8"))
    except urllib.error.HTTPError as e:
        body = e.read().decode("utf-8", "replace")[:200]
        raise PanelError(f"{path} -> HTTP {e.code}: {body}") from e
    except (urllib.error.URLError, TimeoutError, OSError) as e:
        raise PanelError(f"{path} 不可达: {e}") from e


def parse_ts(s: str) -> datetime | None:
    if not s:
        return None
    try:
        return datetime.fromisoformat(s)
    except ValueError:
        return None


def snapshot(base: str, key: str) -> dict:
    """一次性拉齐四类观测。任何一个接口失败都算失败——半份快照会得出错结论。"""
    ov = get(base, key, "/panel/api/overview")
    pk = get(base, key, "/panel/api/packages")
    comp = get(base, key, "/panel/api/compensation")
    hist = get(base, key, "/panel/api/credit_history?limit=200")

    pkgs: dict[str, list] = {}
    for a in pk.get("accounts") or []:
        pkgs[a["uid"]] = a.get("packages") or []

    accounts = []
    for a in ov.get("accounts") or []:
        uid = a["uid"]
        accounts.append(
            {
                "uid": uid,
                "nick": a.get("nickname") or uid[:8],
                "credits": a.get("credits") or 0,
                "total": a.get("credits_total") or 0,
                "earliest_expiry": a.get("credits_earliest_expiry") or "",
                "earliest_remain": a.get("credits_earliest_remaining") or 0,
                "buckets": a.get("credits_expiry_buckets") or [],
                "checkin_done": bool(a.get("checkin_done")),
                # realm 决定该号有没有签到体系：global 账号上游无签到入口，
                # 调度器对它们直接 continue。不带上这个字段，就会把「本来不签到
                # 的号」当成「签到坏了」天天报。
                "realm": a.get("realm") or "cn",
                "cooling": bool(a.get("cooling")),
                "disabled": bool(a.get("disabled")),
                "paused": bool(a.get("paused")),
                "errors": a.get("consecutive_fails") or 0,
                "packages": pkgs.get(uid, []),
            }
        )

    comps = {
        c["uid"]: {
            "nick": c.get("nickname") or c["uid"][:8],
            "status": c.get("status") or {},
        }
        for c in comp.get("accounts") or []
    }

    return {
        "checked_at": datetime.now().astimezone().isoformat(timespec="seconds"),
        "accounts": accounts,
        "compensation": comps,
        "history": hist.get("entries") or [],
    }


def load_pool_state() -> dict:
    """直接读 data/state.json 取 last_checkin_day 等字段。

    为什么不走面板 API：overview 只透出 checkin_done（布尔），没有「最后一次成功
    是哪天」。要判断「连续几天没签到」必须拿到原始日期。state.json 是原子替换
    写入的，只读它不会与网关写路径冲突。
    """
    path = os.path.join(ROOT, "data", "state.json")
    try:
        raw = json.load(open(path, encoding="utf-8"))
    except (OSError, ValueError):
        return {}
    out = {}
    for uid, a in (raw.get("accounts") or {}).items():
        out[uid] = {
            "last_checkin_day": a.get("last_checkin_day") or "",
            "disabled": bool(a.get("disabled")),
            "paused": bool(a.get("paused")),
        }
    return out


def load_schedule() -> dict:
    """读签到排程配置。签到被关掉时不该报「没签到」——那不是故障。"""
    try:
        cfg = json.load(open(CONFIG, encoding="utf-8"))
    except (OSError, ValueError):
        return {"enabled": True, "hours": [10, 21]}
    sch = cfg.get("schedule") or {}
    return {
        "enabled": sch.get("checkin_enabled", True),
        "hours": sch.get("checkin_hours") or [10, 21],
        "include_disabled": sch.get("include_disabled_in_tasks", False),
    }


def load_state() -> dict:
    if not os.path.exists(STATE):
        return {}
    try:
        return json.load(open(STATE, encoding="utf-8"))
    except (OSError, ValueError):
        return {}


def save_state(s: dict) -> None:
    os.makedirs(os.path.dirname(STATE), exist_ok=True)
    tmp = STATE + ".tmp"
    json.dump(s, open(tmp, "w", encoding="utf-8"), ensure_ascii=False, indent=1)
    os.replace(tmp, STATE)


def pkg_key(p: dict) -> str:
    """包的稳定身份。上游不保证每个包都有 package_code（企业/老账号可能缺），
    缺失时用 名称+面额+到期 兜底——否则每个轮次都会把它当新包重复提醒。"""
    code = p.get("package_code")
    if code:
        return code
    return f"{p.get('name','')}|{p.get('size',0)}|{p.get('end_time','')}"


def fmt_pkg(p: dict) -> str:
    return f"{p.get('name','?')} {p.get('remain',0)}/{p.get('size',0)}（到期 {(p.get('end_time') or '')[:10]}）"


def checkin_findings(snap: dict, pool: dict, schedule: dict, prev: dict) -> tuple[list[str], dict]:
    """签到链路健康检查：把「静默失效」变成显式告警。

    为什么需要：签到失败只写进容器日志（易失、重启即丢），而失败的表现不是报错，
    而是「积分不再增长」。连续几天没人发现，直到余额见底。

    四类必须排除的误报
      1. global 账号上游无签到入口，调度器直接跳过 —— 永远不会有 last_checkin_day；
      2. 签到排程被关掉（checkin_enabled=false）—— 不是故障，是配置；
      3. 刚入池的新号 —— 还没来得及跑第一次签到；
      4. 当天还没到签到时点 —— 「今天还没签」在上午是正常的。

    返回 (findings, state)，state 含 first_seen 与更新后的 notified。
    纯函数式：不改传入的 prev，便于测试与重放。
    """
    first_seen = dict(prev.get("first_seen") or {})
    notified = dict(prev.get("notified") or {})
    now = datetime.now().astimezone()
    today = now.date()

    if not schedule.get("enabled", True):
        return [], {"first_seen": first_seen, "notified": notified}

    hours = sorted(int(h) for h in (schedule.get("hours") or [10, 21]))
    last_slot = hours[-1] if hours else 21

    out: list[str] = []
    for a in snap["accounts"]:
        uid = a["uid"]
        first_seen.setdefault(uid, now.isoformat(timespec="seconds"))

        # 兜底 1：global 无签到体系。
        if (a.get("realm") or "cn") == "global":
            continue
        # 兜底 2：禁用号默认不跑保号任务。
        if a.get("disabled") and not schedule.get("include_disabled"):
            continue

        st = pool.get(uid) or {}
        last_day = (st.get("last_checkin_day") or "").strip()

        # 兜底 3：新号宽限。
        try:
            age_days = (now - datetime.fromisoformat(first_seen[uid])).days
        except (ValueError, TypeError):
            # 记录损坏按「已满期」处理：宁可报也不静默。
            age_days = CHECKIN_STALE_DAYS

        # 兜底 4：今天最后一个签到时点还没到，不算失败。
        # 例：排程 [10, 21]，此刻 11:00 且昨天签过 → 今天还没签是正常的。
        before_last_slot = now.hour < last_slot

        if not last_day:
            if age_days < CHECKIN_STALE_DAYS:
                continue
            key = f"checkin-never:{uid}"
            if notified.get(key):
                continue
            out.append(
                f"🚨 **{a['nick']}** 入池 {age_days} 天从未签到成功"
                f"（`last_checkin_day` 为空）"
            )
            notified[key] = now.isoformat(timespec="seconds")
            continue

        try:
            days = (today - date.fromisoformat(last_day)).days
        except ValueError:
            # 日期格式不认识：静默跳过好过误报。
            continue

        if days < CHECKIN_STALE_DAYS:
            continue
        # 距上次成功已有 N 天，但今天还没到签到时点且 N==CHECKIN_STALE_DAYS
        # 时仍可能今晚补上 —— 只在时点已过时报。
        if before_last_slot and days == CHECKIN_STALE_DAYS:
            continue

        key = f"checkin-stale:{uid}:{today.isoformat()}"
        if notified.get(key):
            continue
        suffix = "（今日排程已过仍未成功）" if not before_last_slot else ""
        out.append(
            f"🚨 **{a['nick']}** 签到已 **{days} 天**未成功（最后成功 {last_day}）{suffix}"
        )
        notified[key] = now.isoformat(timespec="seconds")

    return out, {"first_seen": first_seen, "notified": notified}


def report(snap: dict, floor: int) -> str:
    """全量报表：每次调用都完整，用于人工随时查看。"""
    accts = sorted(snap["accounts"], key=lambda a: -a["credits"])
    total = sum(a["credits"] for a in accts)
    lines = [
        f"## WorkBuddy 积分总览（{snap['checked_at']}）",
        "",
        f"- 账号 {len(accts)} 个 · 总积分 **{total}**",
        "",
        "| 账号 | 剩余 | 累计获得 | 最早到期 | 状态 |",
        "|---|---:|---:|---|---|",
    ]
    for a in accts:
        flags = []
        if a["disabled"]:
            flags.append("已禁用")
        if a["paused"]:
            flags.append("已暂停")
        if a["cooling"]:
            flags.append("冷却中")
        # global 账号上游无签到入口，调度器直接跳过——不该显示「今日未签到」，
        # 那会把「本来就没有这个功能」显示成「今天漏了」。
        if not a["checkin_done"] and (a.get("realm") or "cn") != "global":
            flags.append("今日未签到")
        if a["credits"] < floor:
            flags.append("低于保底")
        if a["errors"]:
            flags.append(f"连败{a['errors']}")
        earliest = a["earliest_expiry"][:10] if a["earliest_expiry"] else "—"
        if a["earliest_expiry"] and a["earliest_remain"]:
            earliest += f"（{a['earliest_remain']}）"
        lines.append(
            f"| {a['nick']} | {a['credits']} | {a['total']} | {earliest} | "
            f"{'·'.join(flags) if flags else '正常'} |"
        )

    pend = [
        (uid, c)
        for uid, c in snap["compensation"].items()
        if c["status"].get("active") and not c["status"].get("claimed") and (c["status"].get("credit_num") or 0) > 0
    ]
    lines += ["", "### 限时补偿"]
    if pend:
        for uid, c in pend:
            st = c["status"]
            lines.append(
                f"- **{c['nick']}** 可领 {st.get('credit_num')} 积分，截止 {st.get('end_time')} ← 未领取"
            )
    else:
        lines.append("- 无待领补偿")

    lines += ["", "### 最近积分变动（最新 15 条）"]
    for e in snap["history"][:15]:
        sign = "+" if e["delta"] >= 0 else ""
        lines.append(
            f"- {(e.get('time') or '')[:19]} {e.get('account') or e['uid'][:8]} "
            f"{sign}{e['delta']} → {e['after']}"
        )
    if not snap["history"]:
        lines.append("- （暂无流水）")
    return "\n".join(lines)


def watch(snap: dict, prev: dict, floor: int) -> list[str]:
    """只把「需要动作 / 值得知道」的差异挑出来。首次运行只建基线。"""
    first = not prev
    out: list[str] = []
    prev_accts = {a["uid"]: a for a in prev.get("accounts") or []}
    prev_pkgs = {a["uid"]: {pkg_key(p) for p in a.get("packages") or []} for a in prev.get("accounts") or []}
    prev_comp = prev.get("compensation") or {}
    now = datetime.now().astimezone()

    for a in snap["accounts"]:
        p = prev_accts.get(a["uid"])
        if p:
            delta = a["credits"] - p["credits"]
            if delta >= GAIN_NOISE:
                out.append(f"➕ **{a['nick']}** 积分 +{delta} → {a['credits']}")
            elif delta <= -DROP_NOISE:
                out.append(f"➖ **{a['nick']}** 积分 {delta} → {a['credits']}（单次消耗偏大）")
            if p["credits"] >= floor > a["credits"]:
                out.append(f"⚠️ **{a['nick']}** 跌破积分保底（{a['credits']} < {floor}），收费模型不再参与选号")

        # 新到账的积分包：只报未见过的 package_code；首次运行不报，避免把历史包刷一屏。
        if not first:
            seen = prev_pkgs.get(a["uid"], set())
            fresh = [pkg for pkg in a["packages"] if pkg_key(pkg) not in seen]
            granted = sum(pkg.get("size") or 0 for pkg in fresh)
            if fresh and granted:
                detail = "；".join(fmt_pkg(pkg) for pkg in fresh[:3])
                more = "" if len(fresh) <= 3 else f" 等 {len(fresh)} 个"
                out.append(f"🎁 **{a['nick']}** 新积分包 +{granted}：{detail}{more}")

        # 快过期余额：同一批次只提醒一次（用 uid+到期桶作键）。
        for b in a["buckets"]:
            exp = parse_ts(b.get("expires_at") or "")
            if not exp or not b.get("remain"):
                continue
            days = (exp - now).total_seconds() / 3600
            if 0 <= days <= EXPIRY_WARN_HOURS:
                key = f"expiry:{a['uid']}:{b.get('date')}:{b.get('remain')}"
                if not prev.get("notified", {}).get(key):
                    out.append(
                        f"⏳ **{a['nick']}** {b['remain']} 积分将在 {b.get('date')} 到期"
                        f"（剩 {days:.0f}h）"
                    )

        if p and p.get("checkin_done") is False and a["checkin_done"]:
            out.append(f"✅ **{a['nick']}** 今日签到已完成")
        if p and p.get("disabled") is False and a["disabled"]:
            out.append(f"⛔ **{a['nick']}** 被禁用（选号已排除该号）")
        if p and p.get("cooling") is False and a["cooling"]:
            out.append(f"🧊 **{a['nick']}** 进入冷却")

    for uid, c in snap["compensation"].items():
        st = c["status"]
        if not (st.get("active") and not st.get("claimed") and (st.get("credit_num") or 0) > 0):
            continue
        key = f"comp:{uid}:{st.get('end_time')}"
        if prev.get("notified", {}).get(key):
            continue
        out.append(
            f"🎫 **{c['nick']}** 有 {st.get('credit_num')} 积分补偿待领，截止 {st.get('end_time')}"
        )

    return out


def merge_notified(prev: dict, snap: dict, findings: list[str]) -> dict:
    """把本轮已提醒的到期桶 / 补偿记进 state，避免同一件事每 20 分钟喊一次。"""
    notified = dict(prev.get("notified") or {})
    now = datetime.now().astimezone()
    for a in snap["accounts"]:
        for b in a["buckets"]:
            exp = parse_ts(b.get("expires_at") or "")
            if not exp or not b.get("remain"):
                continue
            if 0 <= (exp - now).total_seconds() / 3600 <= EXPIRY_WARN_HOURS:
                notified[f"expiry:{a['uid']}:{b.get('date')}:{b.get('remain')}"] = snap["checked_at"]
    for uid, c in snap["compensation"].items():
        st = c["status"]
        if st.get("active") and not st.get("claimed"):
            notified[f"comp:{uid}:{st.get('end_time')}"] = snap["checked_at"]
    # 只保留最近 7 天的提醒记录，state 不无限膨胀。
    cutoff = (now - timedelta(days=7)).isoformat(timespec="seconds")
    notified = {k: v for k, v in notified.items() if isinstance(v, str) and v >= cutoff}
    return notified


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("mode", choices=["report", "watch"])
    ap.add_argument("--json", action="store_true", help="输出原始快照/差异 JSON")
    ap.add_argument("--quiet", action="store_true", help="watch 无变化时输出 NO_REPLY")
    ap.add_argument("--floor", type=int, default=DEFAULT_FLOOR)
    args = ap.parse_args()

    try:
        base, key = load_config()
        snap = snapshot(base, key)
    except PanelError as e:
        print(f"积分监视失败：{e}", file=sys.stderr)
        return 2

    if args.mode == "report":
        if args.json:
            print(json.dumps(snap, ensure_ascii=False, indent=1))
        else:
            print(report(snap, args.floor))
        return 0

    prev = load_state()
    findings = watch(snap, prev, args.floor)

    # 签到链路检查：与积分变化是两条独立信号，合并到同一份报告里。
    # 它读 state.json（不读面板），因此面板 401/不可达时仍能工作——不过那时
    # 上面的 snapshot 已经失败了，这里是「顺手多查一层」而非替代。
    try:
        pool = load_pool_state()
        schedule = load_schedule()
        ck_findings, ck_state = checkin_findings(snap, pool, schedule, prev)
    except Exception as e:  # noqa: BLE001
        # 签到检查自身出错不能拖垮整轮：它只是附加信号。
        print(f"积分监视：签到检查跳过（{e}）", file=sys.stderr)
        ck_findings, ck_state = [], {}

    findings = ck_findings + findings

    notified = merge_notified(prev, snap, findings)
    notified.update(ck_state.get("notified") or {})
    save_state(
        {
            "checked_at": snap["checked_at"],
            "accounts": snap["accounts"],
            "compensation": snap["compensation"],
            "notified": notified,
            "first_seen": ck_state.get("first_seen") or prev.get("first_seen") or {},
        }
    )
    if args.json:
        print(json.dumps({"findings": findings, "checked_at": snap["checked_at"]}, ensure_ascii=False, indent=1))
        return 0
    if not findings:
        print("NO_REPLY" if args.quiet else "（无变化）")
        return 0
    total = sum(a["credits"] for a in snap["accounts"])
    print(f"**WorkBuddy 积分动态** · 总积分 {total}")
    print()
    for f in findings:
        print("- " + f)
    return 0


if __name__ == "__main__":
    sys.exit(main())
