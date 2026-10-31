package upstream

import (
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// Web 域事件的浏览器 UA 必须按 uid 稳定派生：同账号恒定、不同账号分散。
//
// 回归背景：此前 ReportWebEvent 的 UA 是硬编码的 Chrome 152，于是全池呈现
// 「machineId 各不相同（不同机器）但浏览器版本完全一致」的矛盾特征。
// 真实用户群里不可能全员同一浏览器版本，国产环境更是 Chrome/Edge/QQ/360/
// 搜狗/UC/微信 混杂。
func TestDeriveWebUAStablePerAccount(t *testing.T) {
	a := &auth.Auth{UID: "acct-a"}

	// 同账号重复调用必须完全一致（真人不换浏览器）。
	first := deriveWebUA(a)
	for i := 0; i < 20; i++ {
		if got := deriveWebUA(a); got != first {
			t.Fatalf("同账号第 %d 次派生出不同 UA：\n  %q\n  %q", i+1, first, got)
		}
	}
}

// 多个账号必须落到不同浏览器/版本上，不能全池同一套。
func TestDeriveWebUASpreadAcrossAccounts(t *testing.T) {
	seen := map[string]struct{}{}
	browsers := map[string]struct{}{}
	for i := 0; i < 40; i++ {
		uid := "spread-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		ua := deriveWebUA(&auth.Auth{UID: uid})
		seen[ua] = struct{}{}
		browsers[browserFamily(ua)] = struct{}{}
	}
	// 40 个账号应产出多种 UA（至少 8 种不同取值）。
	if len(seen) < 8 {
		t.Errorf("40 个账号只派生出 %d 种 UA，区分度不足", len(seen))
	}
	// 且覆盖面不止 Chrome 一家（说明池生效）。
	if len(browsers) < 3 {
		t.Errorf("只覆盖 %d 种浏览器族 %v，池未生效", len(browsers), keys(browsers))
	}
}

// UA 必须是合法形态：以 Mozilla/5.0 开头、包含 Windows NT 10.0、含 Chromium 内核版本。
func TestDeriveWebUAWellFormed(t *testing.T) {
	for i := 0; i < 30; i++ {
		uid := "fmt-" + string(rune('0'+i%10))
		ua := deriveWebUA(&auth.Auth{UID: uid})

		if !strings.HasPrefix(ua, "Mozilla/5.0 (Windows NT 10.0; Win64; x64)") {
			t.Errorf("UA 前缀不合规：%q", ua)
		}
		if !strings.Contains(ua, "AppleWebKit/537.36") {
			t.Errorf("UA 缺 AppleWebKit 段：%q", ua)
		}
		if !strings.Contains(ua, "Chrome/") {
			t.Errorf("UA 缺 Chrome 内核段：%q", ua)
		}
		// 不能留下未替换的格式占位（%s/%d），那是拼装失败的直接证据。
		if strings.Contains(ua, "%s") || strings.Contains(ua, "%d") {
			t.Errorf("UA 含未替换占位符：%q", ua)
		}
	}
}

// Edge 的 Edg/ 段必须与 Chrome 主版本同号，不能造出不存在的组合。
func TestDeriveWebUAEdgeVersionConsistent(t *testing.T) {
	for i := 0; i < 200; i++ {
		uid := "edge-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		ua := deriveWebUA(&auth.Auth{UID: uid})
		if !strings.Contains(ua, "Edg/") {
			continue
		}
		chrome := majorVersionAfter(ua, "Chrome/")
		edge := majorVersionAfter(ua, "Edg/")
		if chrome == "" || edge == "" {
			t.Fatalf("无法解析版本：%q", ua)
		}
		if chrome != edge {
			t.Errorf("Edge UA 的 Edg 主版本(%s) 与 Chrome 主版本(%s) 不一致：%q", edge, chrome, ua)
		}
	}
}

// nil / 空 uid 必须返回可用的兜底 UA，不能 panic 也不能返回空串。
func TestDeriveWebUANilSafe(t *testing.T) {
	for _, a := range []*auth.Auth{nil, {UID: ""}} {
		ua := deriveWebUA(a)
		if ua == "" {
			t.Fatal("兜底 UA 为空")
		}
		if !strings.HasPrefix(ua, "Mozilla/5.0") {
			t.Errorf("兜底 UA 形态不合规：%q", ua)
		}
	}
}

// ---------------------------------------------------------------------------

// browserFamily 从 UA 提取浏览器族名（用于统计覆盖面）。
func browserFamily(ua string) string {
	switch {
	case strings.Contains(ua, "MicroMessenger"):
		return "wechat"
	case strings.Contains(ua, "Edg/"):
		return "edge"
	case strings.Contains(ua, "QQBrowser"):
		return "qq"
	case strings.Contains(ua, "360SE"):
		return "360se"
	case strings.Contains(ua, "360EE"):
		return "360ee"
	case strings.Contains(ua, "MetaSr"):
		return "sogou"
	case strings.Contains(ua, "UBrowser"):
		return "uc"
	default:
		return "chrome"
	}
}

// majorVersionAfter 取 marker 之后的第一个版本主号（如 "Chrome/152.0" → "152"）。
func majorVersionAfter(s, marker string) string {
	i := strings.Index(s, marker)
	if i < 0 {
		return ""
	}
	rest := s[i+len(marker):]
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	return rest[:j]
}

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
