package upstream

import (
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 回归：官方客户端**主动删除** x-stainless-* 头，本网关也不得发送。
//
// 依据（A 级，官方 5.8.3 安装包 app.asar.unpacked/cli/dist/codebuddy-lite-wb.mjs）：
// 内置 CLI 给 openai-node 客户端装了 transformRequest，把前缀 `x-stainless-`
// 的头逐个 delete。同一名单里还有 User-Agent / X-Domain 等——官方在把 SDK
// 自带的默认头换成自己的。
//
// 为什么要钉死：此前一度按「抓包看到 stainless」加过七头，方向正好相反——
// 发了反而呈 SDK 直连形态。这条测试防止再次误加。
func TestChatHeadersDoNotSendStainless(t *testing.T) {
	c := New()
	a := &auth.Auth{UID: "u1", AccessToken: "at", Domain: "copilot.tencent.com", ExpiresAt: 9999999999}
	a.BackfillRealm()

	req := httptest.NewRequest("POST", "https://copilot.tencent.com/v2/chat/completions", nil)
	c.ChatHeaders(req, a, "1.2.3.4", ChatMeta{ConversationID: "conv-1"})

	for k := range req.Header {
		if len(k) >= 11 && k[:11] == "X-Stainless" {
			t.Errorf("不应发送 x-stainless-* 头，实际发了 %q（官方客户端会删除该族）", k)
		}
	}
	// 防御直接写死在别处的可能。
	for _, h := range []string{
		"x-stainless-arch", "x-stainless-lang", "x-stainless-os",
		"x-stainless-package-version", "x-stainless-retry-count",
		"x-stainless-runtime", "x-stainless-runtime-version",
	} {
		if v := req.Header.Get(h); v != "" {
			t.Errorf("不应发送 %s=%q", h, v)
		}
	}
}

// 硬件档案按账号稳定派生：同账号恒定、不同账号不同。
//
// 为什么重要：machineId 已按 uid 派生，若 cpuCores/memorySize 全池恒为常量，
// 就形成「几十台不同机器完全同配置」的矛盾特征，比不伪装更可疑。
func TestDeriveHardwareProfileIsPerAccountAndStable(t *testing.T) {
	a := &auth.Auth{UID: "acct-a"}

	// 同账号重复调用必须一致（同一台机器不该每次上报换配置）。
	h1, h2 := deriveHardwareProfile(a), deriveHardwareProfile(a)
	if h1 != h2 {
		t.Errorf("同账号两次派生不一致：%+v vs %+v", h1, h2)
	}

	// 取值必须落在合理区间，不能出现 0 核 / 8GB 这类明显不像开发机的值。
	for _, uid := range []string{"acct-a", "acct-b", "acct-c", "acct-d", "acct-e"} {
		h := deriveHardwareProfile(&auth.Auth{UID: uid})
		if h.cpuCores < 12 || h.cpuCores > 16 {
			t.Errorf("uid=%s cpuCores=%d 超出合理区间 [12,16]", uid, h.cpuCores)
		}
		if h.memorySize != 16 && h.memorySize != 32 {
			t.Errorf("uid=%s memorySize=%d 应为 16 或 32", uid, h.memorySize)
		}
		if h.osVersion != "10.0.26220" && h.osVersion != "10.0.26200" {
			t.Errorf("uid=%s osVersion=%q 不在已知 Windows 11 build 内", uid, h.osVersion)
		}
	}

	// 多个账号不应全部落成同一套配置（否则「按账号派生」名不副实）。
	seen := map[hardwareProfile]struct{}{}
	for i := 0; i < 20; i++ {
		uid := "spread-" + string(rune('a'+i))
		seen[deriveHardwareProfile(&auth.Auth{UID: uid})] = struct{}{}
	}
	if len(seen) < 3 {
		t.Errorf("20 个账号只派生出 %d 种硬件档案，区分度不足", len(seen))
	}
}

// nil 账号要有确定兜底，不能 panic（历史调用点存在 nil）。
func TestDeriveHardwareProfileNilSafe(t *testing.T) {
	h := deriveHardwareProfile(nil)
	if h.cpuCores == 0 || h.memorySize == 0 || h.osVersion == "" {
		t.Errorf("nil 账号兜底不完整：%+v", h)
	}
}
