package scheduler

import (
	"testing"
	"time"
)

// 抖动关闭时（permill=0）必须逐字返回原值 —— 老配置零回归。
//
// 这是整个特性的安全底线：抖动是可选增强，默认不改变任何既有调度时序。
func TestJitterDurationDisabledIsIdentity(t *testing.T) {
	base := 5 * time.Minute
	for _, permill := range []int64{0, -1, -1000} {
		if got := jitterDuration(base, permill, "balance", 1); got != base {
			t.Errorf("permill=%d: jitterDuration=%s want 原值 %s（关闭时必须零回归）",
				permill, got, base)
		}
	}
	// base <= 0 同样原样返回（暂停语义不该被抖动改写）。
	if got := jitterDuration(0, 250, "balance", 1); got != 0 {
		t.Errorf("base=0 时应返回 0，得到 %s", got)
	}
}

// 抖动开启后必须真的落在 ±permill/1000 区间内，且不能变成 0/负值。
func TestJitterDurationWithinBand(t *testing.T) {
	base := 10 * time.Minute
	const permill = 250 // ±25% → [7.5m, 12.5m]

	lo := time.Duration(float64(base) * 0.75)
	hi := time.Duration(float64(base) * 1.25)

	seen := map[time.Duration]struct{}{}
	for round := int64(1); round <= 200; round++ {
		got := jitterDuration(base, permill, "balance", round)
		if got < lo || got > hi {
			t.Fatalf("round=%d: %s 超出区间 [%s, %s]", round, got, lo, hi)
		}
		if got <= 0 {
			t.Fatalf("round=%d: 抖动产出非正时长 %s", round, got)
		}
		seen[got] = struct{}{}
	}
	// 必须是「真抖动」而不是每轮同一值：200 轮里应出现多种取值。
	if len(seen) < 50 {
		t.Errorf("200 轮只产生 %d 种时长，抖动未实际变化", len(seen))
	}
}

// 同一 (seed, round) 必须稳定 —— 可复现、可测试，不因随机源引入 flaky。
func TestStableJitterIsDeterministic(t *testing.T) {
	for round := int64(1); round <= 50; round++ {
		a := stableJitter("balance", round)
		b := stableJitter("balance", round)
		if a != b {
			t.Fatalf("round=%d: 同一输入产出不同值 %v vs %v", round, a, b)
		}
		if a < -1 || a > 1 {
			t.Fatalf("round=%d: %v 超出 [-1,1]", round, a)
		}
	}
	// 不同 seed 应给出不同相位（否则余额与清扫会同步抖动，反而形成新的整齐节拍）。
	if stableJitter("balance", 7) == stableJitter("sweep", 7) {
		t.Error("不同 seed 在相同 round 得到同一偏移：两类任务会形成新的同步节拍")
	}
}

// 账号摊开：permill=0 时不延迟（旧行为并发照旧）。
func TestAccountOffsetDisabled(t *testing.T) {
	for _, permill := range []int64{0, -5} {
		if got := accountOffset("uid-1", permill, 1); got != 0 {
			t.Errorf("permill=%d: accountOffset=%s want 0", permill, got)
		}
	}
	if got := accountOffset("", 250, 1); got != 0 {
		t.Errorf("空 uid 应返回 0，得到 %s", got)
	}
}

// 账号摊开开启后：各账号起点必须分散，且都在窗口内（不能拖成分钟级）。
func TestAccountOffsetSpreadsAccounts(t *testing.T) {
	const permill = 500 // 铺满 5s 窗口
	window := accountSpreadWindow

	offsets := map[string]time.Duration{}
	for _, uid := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		d := accountOffset(uid, permill, 3)
		if d < 0 || d > window {
			t.Fatalf("uid=%s: offset=%s 超出 [0, %s]", uid, d, window)
		}
		offsets[uid] = d
	}
	// 8 个账号不应全落在同一时刻（这正是「同一秒全池齐射」的指纹）。
	distinct := map[time.Duration]struct{}{}
	for _, d := range offsets {
		distinct[d] = struct{}{}
	}
	if len(distinct) < 6 {
		t.Errorf("8 个账号只有 %d 个不同起点，摊开无效", len(distinct))
	}
}

// 抖动幅度上限：超过 500‰ 应被钳到 500，不能放大到改变调度语义。
func TestJitterDurationClampsPermill(t *testing.T) {
	base := 4 * time.Minute
	// 传 5000（500%）应被钳成 500（±50%） → 落在 [2m, 6m]
	lo := time.Duration(float64(base) * 0.5)
	hi := time.Duration(float64(base) * 1.5)
	for round := int64(1); round <= 100; round++ {
		got := jitterDuration(base, 5000, "balance", round)
		if got < lo || got > hi {
			t.Fatalf("round=%d: %s 超出钳制后区间 [%s, %s]", round, got, lo, hi)
		}
	}
}

// SetBalanceJitter 热改必须被钳制并唤醒循环。
func TestSetJitterClampsAndRearms(t *testing.T) {
	s := New(Config{})
	// 非阻塞排空：空通道上直接 <- 会永久阻塞（此前写成 <-s.rearmBalance 导致测试挂死）。
	select {
	case <-s.rearmBalance:
	default:
	}
	s.SetBalanceJitter(9999)
	if got := s.balanceJitter.Load(); got != jitterPermillRange {
		t.Errorf("balanceJitter=%d want 钳制到 %d", got, jitterPermillRange)
	}
	select {
	case <-s.rearmBalance:
	default:
		t.Error("SetBalanceJitter 未唤醒余额循环")
	}

	s.SetSweepJitter(300)
	if got := s.sweepJitter.Load(); got != 300 {
		t.Errorf("sweepJitter=%d want 300", got)
	}
	select {
	case <-s.rearmSweep:
	default:
		t.Error("SetSweepJitter 未唤醒清扫循环")
	}
}

// 构造时从 Config 读入抖动，且同样钳制。
func TestNewReadsJitterConfig(t *testing.T) {
	s := New(Config{BalanceJitterPermill: 250, SweepJitterPermill: 99999})
	if got := s.balanceJitter.Load(); got != 250 {
		t.Errorf("balanceJitter=%d want 250", got)
	}
	if got := s.sweepJitter.Load(); got != jitterPermillRange {
		t.Errorf("sweepJitter=%d want 钳制到 %d", got, jitterPermillRange)
	}
}
