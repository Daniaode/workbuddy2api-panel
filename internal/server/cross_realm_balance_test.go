package server

import (
	"testing"
)

// TestRealmForCatalog 跨域负载均衡的核心决策表。
//
// 语义（pool.cross_realm 开启时，仅对**不带前缀**的模型名生效）：
//   - 两域目录都有该模型 → realm=""，选号不加域过滤，两池合池加权分担；
//   - 只有 global 有 → 钉到 global；
//   - 只有 cn 有 → 钉到 cn；
//   - 两域都没有 → routed=false，调用方保持原语义（裸名默认 cn）。
//
// 最后一条最容易写错：把「都没有」也返回 ("", true) 会让目录尚未预热的第一个
// 请求就变成「静默合池」，被分到没有该模型的域上去撞 11102。routed 必须能区分
// 「确定的合池」与「无信息」。
func TestRealmForCatalog(t *testing.T) {
	cases := []struct {
		name       string
		hasCN      bool
		hasGlobal  bool
		wantRealm  string
		wantRouted bool
	}{
		{"两域都有 → 合池（不加域过滤）", true, true, "", true},
		{"只有 global → 钉住 global", false, true, "global", true},
		{"只有 cn → 钉住 cn", true, false, "cn", true},
		{"都没有 → 不改写路由", false, false, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			realm, routed := realmForCatalog(c.hasCN, c.hasGlobal)
			if realm != c.wantRealm || routed != c.wantRouted {
				t.Fatalf("realmForCatalog(%v,%v) = (%q,%v), want (%q,%v)",
					c.hasCN, c.hasGlobal, realm, routed, c.wantRealm, c.wantRouted)
			}
		})
	}
}

// TestModelHiddenWithEmptyRealm 合池模式（realm=""）下黑名单必须仍然生效。
//
// 回归点：此前黑名单检查是 `HiddenModels[realm+":"+bareModel]`，realm 为空时
// 拼出 ":model" 这种永远匹配不上的键——等于在跨域负载均衡路径上把黑名单
// 关掉了。黑名单是运维的安全阀，不该因为路由模式变化而半开。
func TestModelHiddenWithEmptyRealm(t *testing.T) {
	newH := func(hidden ...string) *Handler {
		return NewHandler(Config{ModelsHidden: hidden})
	}

	t.Run("合池时任一侧被禁即禁", func(t *testing.T) {
		if !newH("global:glm-5.2").modelHidden("", "glm-5.2") {
			t.Error("合池模式：global 侧被禁时应判定为隐藏")
		}
		if !newH("cn:glm-5.2").modelHidden("", "glm-5.2") {
			t.Error("合池模式：cn 侧被禁时应判定为隐藏")
		}
		if newH("cn:other").modelHidden("", "glm-5.2") {
			t.Error("合池模式：未列出的模型不应被判隐藏")
		}
	})

	t.Run("显式域只看该域", func(t *testing.T) {
		h := newH("global:glm-5.2")
		if h.modelHidden("cn", "glm-5.2") {
			t.Error("查 cn 时不应被 global 的条目影响")
		}
		if !h.modelHidden("global", "glm-5.2") {
			t.Error("查 global 时应命中")
		}
	})
}
