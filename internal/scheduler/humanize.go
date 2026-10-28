// humanize.go 调度「人味化」：给机械节拍加稳定抖动，削弱机器人指纹。
//
// 背景（上游风控复盘，2026-10-24 同类事件 + 2026-10-28 本项目实测）：
//
//	池内多个账号在数分钟内被 11140「request illegal / 账号存在异常行为」集中封禁。
//	日志显示同出口 IP 上全部账号以**完全一致的机械节拍**活动：
//	  - 余额刷新：精确每 5 分钟整点全账号并发齐射
//	  - 补领清扫：精确每 15 分钟整点
//	真人不会在同一秒对全部账号做同一动作——「整点 + 全池并发」是最强的机器人指纹。
//
// 因此提供两层随机化，**缺省全部关闭**（permill=0，旧行为逐字不变）：
//  1. 间隔抖动：每轮的执行时刻在 ±permill/1000 内偏移，不再精确打卡
//  2. 账号摊开：同一轮里各账号的动作起点错开，消除「同一秒全池同动作」
package scheduler

import (
	"fmt"
	"hash/fnv"
	"time"
)

// jitterPermillRange 抖动比例上限（千分比）。500 = ±50%。
//
// 超过这个量级的「抖动」已经不是抖动，而是在改变调度语义（例如 5 分钟的刷新
// 被抖到 2.5～7.5 分钟），会让「多久刷一次」变得不可预期。配置超限直接拒绝，
// 不静默钳制——静默钳制会让用户以为自己设的值生效了。
const jitterPermillRange = 500

// accountSpreadWindow 账号摊开的基准窗口。
//
// 取 5 秒：足以把「同一毫秒并发」打散成「同一窗口内先后」，
// 又短到不影响任何时效性（余额刷新/清扫都是分钟级任务）。
// permill=1000 时铺满整个窗口，permill=250 时铺开约 1.25 秒。
const accountSpreadWindow = 5 * time.Second

// stableJitter 用 (seed, round) 派生一个稳定的 [-1, 1] 偏移量。
//
// 为什么不用 math/rand：
//   - rand 每次调用都变，同一账号相邻两轮的偏移毫无关系，观测面不可复现；
//   - 若为可复现而固定种子，进程重启后相位又会整体跳变。
//
// 用 FNV-1a 对 (seed, round) 取哈希：同进程内确定（可复现、可测试），
// 跨轮次变化（不是「每轮同一个偏移」那种假抖动），跨进程也稳定。
func stableJitter(seed string, round int64) float64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(seed))
	var buf [8]byte
	for i := 0; i < 8; i++ {
		buf[i] = byte(round >> (8 * i))
	}
	_, _ = h.Write(buf[:])
	sum := h.Sum64()
	// 20001 个离散点映射到 [-1, 1]，步长 0.0001。
	return float64(sum%20001)/10000.0 - 1.0
}

// jitterDuration 给基准时长施加 ±permill/1000 的稳定偏移。
//
// permill <= 0 或 base <= 0 时原样返回 —— 这是「缺省关闭、零回归」的保证。
func jitterDuration(base time.Duration, permill int64, seed string, round int64) time.Duration {
	if base <= 0 || permill <= 0 {
		return base
	}
	if permill > jitterPermillRange {
		permill = jitterPermillRange
	}
	f := 1.0 + stableJitter(seed, round)*(float64(permill)/1000.0)
	// 下限保护：抖动再大也不该把间隔压到接近 0（会变成高频轮询）。
	if f < 0.1 {
		f = 0.1
	}
	return time.Duration(float64(base) * f)
}

// accountOffset 返回某账号在本轮批量动作里应额外等待的时长。
//
// 把稳定哈希映射到 [0, window] 区间（单向延迟，不提前）。
// permill <= 0 时返回 0（同一轮仍然并发，保持旧行为）。
func accountOffset(uid string, permill int64, round int64) time.Duration {
	if permill <= 0 || uid == "" {
		return 0
	}
	if permill > jitterPermillRange {
		permill = jitterPermillRange
	}
	window := time.Duration(float64(accountSpreadWindow) * float64(permill) / 1000.0)
	if window <= 0 {
		return 0
	}
	// stableJitter 落在 [-1,1]，先抬到 [0,1] 再乘窗口。
	f := (stableJitter(fmt.Sprintf("spread:%s", uid), round) + 1.0) / 2.0
	return time.Duration(float64(window) * f)
}
