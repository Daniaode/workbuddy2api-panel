package panel

import (
	"encoding/json"
	"testing"
	"time"
)

// probeBody 必须发裸模型名。
//
// 回归背景：曾直接把手带前缀的 id（global:deepseek-v4.1-flash）塞进请求体，
// 而 realm 前缀是网关侧路由协议、上游不认——绕过网关直调 ChatStream 就绕过了
// handler.go:887 的 rewriteModel，结果 35 个模型全部返回 11102 model_blocked，
// 面板把所有国际版模型标成「上游无此模型」。这是把「探测结论」变成假证据的
// 最坏情况：面板会显示得很确定，而结论是错的。
func TestProbeBodyStripsRealmPrefix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"global:deepseek-v4.1-flash", "deepseek-v4.1-flash"},
		{"cn:glm-5.3", "glm-5.3"},
		{"deepseek-v4.1-flash", "deepseek-v4.1-flash"}, // 裸名不变
		{"global:gpt-6-astra", "gpt-6-astra"},
	}
	for _, c := range cases {
		raw := probeBody(c.in)
		var body struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("%s: 请求体不是合法 JSON: %v (%s)", c.in, err, raw)
		}
		if body.Model != c.want {
			t.Errorf("%s: 出站 model = %q，期望 %q（上游不认 realm 前缀）", c.in, body.Model, c.want)
		}
		// 成本约束：探测必须极轻，否则「不烧额度」这条硬要求守不住。
		if body.MaxTokens != 1 {
			t.Errorf("%s: max_tokens = %d，探测必须为 1", c.in, body.MaxTokens)
		}
	}
}

// 探测体必须带一条 user 消息：上游对空 messages 走另一条校验分支，
// 那种失败会被误判成「模型不可用」。
func TestProbeBodyHasMinimalMessage(t *testing.T) {
	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(probeBody("global:x"), &body); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v", err)
	}
	if len(body.Messages) != 1 || body.Messages[0].Role != "user" {
		t.Fatalf("探测体应只含一条 user 消息，实际: %+v", body.Messages)
	}
	if body.Messages[0].Content == "" {
		t.Error("user 消息内容不能为空")
	}
}

// availTTL 必须跟随自动探测间隔：固定 24h 会让一次静默失败后的旧结论冒充现状，
// 比诚实显示「待验证」更糟。
//
// 同时必须压住全量重探频率：TTL 直接决定「多久把 35 个模型全部重探一遍」，
// 而重探一轮是 35 个真实 chat 请求。早期按 interval×3 算，30m 间隔得到 90m
// TTL，折算 560 请求/天全压在单个 global 号上——所以下限提到 6h（140 请求/天）。
func TestAvailTTLFollowsInterval(t *testing.T) {
	cases := []struct {
		interval time.Duration
		want     time.Duration
		why      string
	}{
		{0, availTTLManual, "自动探测关闭 → 手动模式 24h"},
		{-1 * time.Second, availTTLManual, "负值视同关闭"},
		{30 * time.Minute, availTTLFloor, "3×间隔=90m 低于 6h 下限 → 取下限，压住重探频率"},
		{1 * time.Minute, availTTLFloor, "极短间隔同样被 6h 下限压住"},
		{1 * time.Hour, availTTLFloor, "3×间隔=3h 仍低于下限"},
		{3 * time.Hour, 9 * time.Hour, "超过下限后按 3 倍间隔（容忍连丢两轮）"},
		{12 * time.Hour, availTTLManual, "钳上限：不超过手动模式 24h"},
	}
	for _, c := range cases {
		var p Panel
		p.availInterval.Store(int64(c.interval))
		if got := p.availTTL(); got != c.want {
			t.Errorf("interval=%s: availTTL()=%s，期望 %s（%s）", c.interval, got, c.want, c.why)
		}
	}
}

// 全量重探的日均请求数必须留在「不显眼」的量级。
//
// 这条不是形式主义：间隔与 TTL 都是可配的，一次配置改动就可能把探测变成
// 对单个账号的密集轮询。用默认配置把上限钉住，改动时会被立刻拦下。
func TestAvailProbeDailyRequestBudget(t *testing.T) {
	const globalModels = 35 // 实测当前国际版目录规模
	const budget = 200      // 约定上限：日均探测 chat 请求数

	var p Panel
	p.availInterval.Store(int64(30 * time.Minute)) // 默认间隔
	ttl := p.availTTL()
	perDay := float64(globalModels) * (float64(24*time.Hour) / float64(ttl))
	if perDay > budget {
		t.Fatalf("默认配置下日均探测请求 %.0f 次，超过上限 %d（TTL=%s）——"+
			"探测会变成对单个 global 号的密集轮询", perDay, budget, ttl)
	}
	t.Logf("默认配置：TTL=%s，全量重探每 %.1fh，日均约 %.0f 请求（上限 %d）",
		ttl, ttl.Hours(), perDay, budget)
}
