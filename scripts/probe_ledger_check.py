#!/usr/bin/env python3
"""probe_ledger_check.py — 证明探测请求没有进正式台账。

为什么不能用文件哈希
====================
`data/usage.json` 由池子的 flusher 每 5 秒原子重写一次，**没有探测时哈希也会变**
（实测：无探测的 24 秒内 f724… → bb7d…）。拿 sha256 当判据会得到假阴性/假阳性，
纯属掷骰子。

正确的判据是差分：探测只可能让「记账数」涨，不会让「真实流量」涨。因此比较

    Δusage.requests  vs  Δrequest_logs 行数

  - 探测若被记账：Δusage 会**大于** Δlogs（多出来的正是探测次数）；
  - 探测未记账  ：两者**相等**（都只由真实流量驱动）。

两个数都取自网关自身的观测面，与探测进程完全解耦，且容忍 flusher 的周期性重写
（重写不改内容语义）。

为什么还要注入真实流量
======================
只有 Δusage == Δlogs == 0 时，判据本身不可证伪——分不清「探测没被记账」和
「计数器压根没工作」。所以要主动打几次真实网关请求，先证明计数器是活的，
再证明探测没进去。这样失败才有可能是失败。

用法:
  python3 scripts/probe_ledger_check.py                    # 注入 3 次真实 + 跑一轮 L2
  python3 scripts/probe_ledger_check.py --real 0           # 不注入（弱判据，仅观察）
  python3 scripts/probe_ledger_check.py --mode l1 -n 3     # 换层 / 多轮
  python3 scripts/probe_ledger_check.py --user-agent-diff  # 见下

退出码: 0 通过；1 台账被污染；2 无法完成检查（面板不可达 / 探测未完成）
"""
from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import urllib.error
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def panel_key() -> str:
    cfg = json.load(open(os.path.join(ROOT, "config.json"), encoding="utf-8"))
    return cfg.get("api_key") or ""


def listen_addr() -> str:
    cfg = json.load(open(os.path.join(ROOT, "config.json"), encoding="utf-8"))
    listen = (cfg.get("listen") or ":7863").strip()
    if listen.startswith(":"):
        return "127.0.0.1" + listen
    return listen.replace("0.0.0.0", "127.0.0.1", 1)


def usage_totals(hours: int = 2) -> dict:
    """从面板取用量合计。读内存快照，不受 usage.json 重写节奏影响。"""
    url = "http://" + listen_addr() + "/panel/api/usage?hours=%d" % hours
    req = urllib.request.Request(url, headers={"Authorization": "Bearer " + panel_key()})
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            d = json.load(r)
    except (urllib.error.URLError, OSError, ValueError) as e:
        raise SystemExit("无法读取面板用量（%s）——本检查要求面板在线" % e)
    t = d.get("totals") or {}
    return {
        "requests": int(t.get("requests") or 0),
        "total_tokens": int(t.get("total_tokens") or 0),
        "credits": float(t.get("credits") or 0.0),
    }


def log_count() -> int:
    """request-logs 行数：真实流量的独立计数据。探测从不写这里。"""
    d = os.path.join(ROOT, "data/request-logs")
    if not os.path.isdir(d):
        return 0
    n = 0
    for f in os.listdir(d):
        if f.endswith(".jsonl"):
            with open(os.path.join(d, f), encoding="utf-8", errors="ignore") as fh:
                n += sum(1 for _ in fh)
    return n


def probe_count(mode: str) -> int:
    """本轮探测会发几次请求：L2 = 可探账号数，L1/L3 = 1。"""
    if mode != "l2":
        return 1
    out = subprocess.run([os.path.join(ROOT, "probe_bin"), "-mode", "l2", "-dry-run"],
                         cwd=ROOT, capture_output=True, text=True)
    return sum(1 for line in out.stdout.splitlines() if line.startswith("would probe:"))


def inject_real(n: int) -> int:
    """打 n 次真实网关请求，制造已知的记账增量。

    这是判据的对照组：没有它，Δ=0/0 无法区分「探测没记账」与「计数器坏了」。
    用最小请求（max_tokens=1），成本实测 credit=0。
    """
    ok = 0
    for _ in range(n):
        body = json.dumps({
            "model": "cn:deepseek-v4.1-flash",
            "messages": [{"role": "user", "content": "1"}],
            "max_tokens": 1,
            "stream": False,
        }).encode()
        req = urllib.request.Request(
            "http://" + listen_addr() + "/v1/chat/completions",
            data=body,
            headers={
                "Authorization": "Bearer " + panel_key(),
                "Content-Type": "application/json",
                "User-Agent": "probe-ledger-proof/1",
            },
        )
        try:
            with urllib.request.urlopen(req, timeout=60) as r:
                r.read()
            ok += 1
        except (urllib.error.URLError, OSError) as e:
            print("注入真实请求失败：%s" % e, file=sys.stderr)
            break
    return ok


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--mode", default="l2", choices=["l1", "l2", "l3"])
    ap.add_argument("-n", type=int, default=1, help="重复轮数")
    ap.add_argument("--real", type=int, default=3, help="注入的真实网关请求数（对照组）")
    args = ap.parse_args()

    expected = probe_count(args.mode) * args.n
    if expected == 0:
        print("没有可探测账号，检查无意义", file=sys.stderr)
        return 2

    u0, l0 = usage_totals(), log_count()
    real_ok = inject_real(args.real)
    for i in range(args.n):
        r = subprocess.run(["bash", "scripts/probe.sh", args.mode],
                           cwd=ROOT, capture_output=True, text=True)
        if r.returncode != 0:
            # 非零退出码说明探测没完成，此时比对无意义。
            print(r.stdout, r.stderr, file=sys.stderr)
            print("探测未正常完成（rc=%d），无法比对" % r.returncode, file=sys.stderr)
            return 2
    u1, l1 = usage_totals(), log_count()

    du = u1["requests"] - u0["requests"]
    dl = l1 - l0

    print("注入真实请求    : %d 次（对照组）" % real_ok)
    print("探测请求数      : %d（%s × %d 轮）" % (expected, args.mode, args.n))
    print("Δusage.requests : %d" % du)
    print("Δrequest-logs   : %d" % dl)
    print("Δcredits        : %.2f" % (u1["credits"] - u0["credits"]))
    print()

    failed = False

    # 第一道：计数器必须活着。否则后面的相等只是巧合。
    if args.real > 0:
        if dl < real_ok:
            print("✗ 计数器未随真实流量增长（Δlogs=%d < 注入 %d）——判据不可证伪" % (dl, real_ok))
            failed = True
        else:
            print("✓ 计数器是活的：%d 次真实请求被如实记账" % real_ok)

    # 第二道：探测不得记账。
    if du != dl:
        print("✗ usage 多出 %d 条未归因记录 —— 探测被记账了" % (du - dl))
        failed = True
    else:
        print("✓ 两者严格相等：%d 次探测一次都没进 usage（若被记账，Δusage 应为 %d）"
              % (expected, dl + expected))

    print()
    print("结论:", "FAIL ❌" if failed else "PASS 探测完全绕开用量台账 ✅")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
