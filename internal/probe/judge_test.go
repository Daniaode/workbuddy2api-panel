package probe

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// —— 错误码提取 ——

func TestExtractCodeForms(t *testing.T) {
	cases := []struct {
		name, body, wantCode string
		wantMsg              bool
	}{
		{"数值码", `{"code":11102,"msg":"model not available"}`, "11102", true},
		{"字符串码", `{"code":"11158","msg":"not allowed"}`, "11158", true},
		{"带横线通道码", `{"code":"11-128","msg":"request illegal"}`, "11-128", true},
		{"空格容差", `{"code" : 6004 ,"msg":"rate"}`, "6004", true},
		{"无码", `oops`, "", false},
		{"HTML WAF 页", `<html><body>403 Forbidden</body></html>`, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, msg := ExtractCode(c.body)
			if code != c.wantCode {
				t.Fatalf("code=%q want %q", code, c.wantCode)
			}
			if c.wantMsg && msg == "" {
				t.Error("msg 应有值")
			}
		})
	}
}

// —— 判定：通道级 ——

func TestJudgeChannelRefusedOnWAF(t *testing.T) {
	// 403 无业务码 = WAF 拦截页，不是账号问题 → critical。
	fs := Judge(Attempt{UID: "u1", Nick: "A", Status: 403, BodyPrefix: "<html>403</html>"})
	if len(fs) != 1 || fs[0].Code != "channel_refused" || fs[0].Level != "critical" {
		t.Fatalf("got %+v", fs)
	}
}

func TestJudgeChannelRefusedOn11128(t *testing.T) {
	fs := Judge(Attempt{UID: "u1", Status: 403, Code: "11-128", Msg: "request illegal"})
	if len(fs) != 1 || fs[0].Code != "channel_refused" || fs[0].Level != "critical" {
		t.Fatalf("got %+v", fs)
	}
}

// 业务 403（带 code）不能判成 WAF —— 那会把账号问题升级成通道故障。
func TestJudgeBusiness403IsNotWAF(t *testing.T) {
	fs := Judge(Attempt{UID: "u1", Status: 403, Code: "11140", Msg: "request illegal"})
	if len(fs) != 1 || fs[0].Code == "channel_refused" {
		t.Fatalf("业务 403 被判成通道拒绝：%+v", fs)
	}
}

// —— 判定：模型级 ——

func TestJudgeModelRejectedSeeksDual(t *testing.T) {
	for _, code := range []string{"11102", "11158"} {
		fs := Judge(Attempt{UID: "u1", Status: 400, Code: code})
		if len(fs) != 1 || fs[0].Code != "model_rejected" || !fs[0].Dual {
			t.Fatalf("%s: got %+v", code, fs)
		}
	}
}

func TestJudgeRateLimitIsSilent(t *testing.T) {
	// 6004 是常态模型限流，报它只会造成告警疲劳。
	if fs := Judge(Attempt{UID: "u1", Status: 429, Code: "6004"}); len(fs) != 0 {
		t.Fatalf("6004 应静默，得到 %+v", fs)
	}
}

// —— 判定：未知码是核心价值 ——

func TestJudgeUnknownCodeSurfaces(t *testing.T) {
	fs := Judge(Attempt{
		UID: "u1", Status: 400, Code: "99999", Msg: "new policy",
		BodyPrefix: `{"code":99999,"msg":"new policy"}`,
	})
	if len(fs) != 1 {
		t.Fatalf("got %+v", fs)
	}
	if fs[0].Code != "unknown_code:99999" || fs[0].Level != "high" {
		t.Fatalf("未知码必须独立出口：%+v", fs[0])
	}
	if !strings.Contains(fs[0].Detail, "new policy") {
		t.Error("未知码必须带上原文，否则无法诊断")
	}
}

// —— 判定：协议漂移 ——

func TestJudgeProtocolDrift(t *testing.T) {
	fs := Judge(Attempt{UID: "u1", Status: 200, Drift: []string{"usage_missing", "delta_missing"}})
	if len(fs) != 1 || fs[0].Code != "protocol_drift" {
		t.Fatalf("got %+v", fs)
	}
	if !strings.Contains(fs[0].Detail, "usage_missing") {
		t.Error("漂移项要逐条列出，否则无从排查")
	}
}

func TestJudgeHealthyIsQuiet(t *testing.T) {
	if fs := Judge(Attempt{UID: "u1", Status: 200}); len(fs) != 0 {
		t.Fatalf("健康不该产结论：%+v", fs)
	}
}

// —— 双模型交叉 ——

// 关键防误报：上游下线一个模型时，不能报成通道故障。
func TestDualModelLevelChangeNotChannelFailure(t *testing.T) {
	primary := Attempt{UID: "u1", Nick: "A", Model: "m1", Status: 400, Code: "11102"}
	fallback := Attempt{UID: "u1", Nick: "A", Model: "m2", Status: 200}
	fs := JudgeWithDual(primary, fallback)
	if len(fs) != 1 || fs[0].Code != "model_level_change" || fs[0].Level != "medium" {
		t.Fatalf("模型级变化不应升级为通道故障：%+v", fs)
	}
}

func TestDualModelBothFailIsChannelFailure(t *testing.T) {
	primary := Attempt{UID: "u1", Model: "m1", Status: 400, Code: "11102"}
	fallback := Attempt{UID: "u1", Model: "m2", Status: 400, Code: "11102"}
	fs := JudgeWithDual(primary, fallback)
	if len(fs) != 1 || fs[0].Code != "channel_failure" || fs[0].Level != "critical" {
		t.Fatalf("主备皆败应判通道级：%+v", fs)
	}
}

func TestDualNotTriggeredForNonModelCodes(t *testing.T) {
	// 12153 是 session 问题，不该触发备用模型重试。
	primary := Attempt{UID: "u1", Status: 401, Code: "12153"}
	fs := JudgeWithDual(primary, Attempt{})
	if len(fs) != 1 || fs[0].Code != "session_dead" {
		t.Fatalf("got %+v", fs)
	}
}

// —— L3 归因纯度 ——

// L3 多依赖一个变量（网关自己的提示词），失败不得升级为通道故障。
func TestL3NeverEscalatesToChannelFailure(t *testing.T) {
	l3 := Attempt{UID: "u1", Status: 403, Code: "11-128", Msg: "request illegal"}
	fs := JudgePromptLayer(l3)
	if len(fs) != 1 || fs[0].Code != "prompt_layer_diff" {
		t.Fatalf("L3 必须只产 prompt_layer_diff：%+v", fs)
	}
	if fs[0].Level == "critical" {
		t.Error("L3 不应产出 critical")
	}
}

func TestL3HealthyIsQuiet(t *testing.T) {
	if fs := JudgePromptLayer(Attempt{UID: "u1", Status: 200}); len(fs) != 0 {
		t.Fatalf("got %+v", fs)
	}
}

// —— 传输层失败归因 ——

func TestNetworkErrorIsNotPolicyRejection(t *testing.T) {
	att := Attempt{UID: "u1", Status: 0, Network: true, ErrText: "dial tcp: timeout"}
	fs := Judge(att)
	if len(fs) != 1 || fs[0].Code != "network_error" {
		t.Fatalf("got %+v", fs)
	}
}

// —— 账号选择 ——

func mkAuth(uid, nick string, expIn time.Duration) *auth.Auth {
	return &auth.Auth{
		UID: uid, Nickname: nick, AccessToken: "at-" + uid,
		ExpiresAt: time.Now().Add(expIn).Unix(),
	}
}

func TestBuildCandidatesSkipsUnsafeAccounts(t *testing.T) {
	now := time.Now()
	auths := []*auth.Auth{
		mkAuth("u-ok", "ok", 100*time.Hour),
		mkAuth("u-dis", "dis", 100*time.Hour),
		mkAuth("u-pau", "pau", 100*time.Hour),
		mkAuth("u-cool", "cool", 100*time.Hour),
		mkAuth("u-poor", "poor", 100*time.Hour),
		mkAuth("u-exp", "exp", 30*time.Minute),
		mkAuth("u-nocred", "nocred", 100*time.Hour),
	}
	auths[6].AccessToken = ""
	states := map[string]AccountState{
		"u-ok":     {Credits: 5000},
		"u-dis":    {Credits: 5000, Disabled: true},
		"u-pau":    {Credits: 5000, Paused: true},
		"u-cool":   {Credits: 5000, Until: now.Add(30 * time.Minute)},
		"u-poor":   {Credits: 150}, // < floor(100)*2
		"u-exp":    {Credits: 5000},
		"u-nocred": {Credits: 5000},
	}

	cands := BuildCandidates(auths, states, SelectOptions{CreditFloor: 100, Now: now})
	want := map[string]SkipReason{
		"u-ok":     "",
		"u-dis":    SkipDisabled,
		"u-pau":    SkipPaused,
		"u-cool":   SkipCooling,
		"u-poor":   SkipLowCredit,
		"u-exp":    SkipTokenDue,
		"u-nocred": SkipNoCreds,
	}
	for _, c := range cands {
		if got := c.Skip; got != want[c.UID] {
			t.Errorf("%s skip=%q want %q", c.UID, got, want[c.UID])
		}
	}

	// 只剩一个可探号时，Pick 必须稳定选中它（而不是因为游标漂移返回空）。
	got, ok := Pick(cands, 3)
	if !ok || got.UID != "u-ok" {
		t.Fatalf("Pick -> %+v ok=%v", got, ok)
	}
}

func TestPickReportsZeroCoverage(t *testing.T) {
	cands := []Candidate{{UID: "a", Skip: SkipCooling}, {UID: "b", Skip: SkipDisabled}}
	if _, ok := Pick(cands, 0); ok {
		t.Fatal("全部跳过时必须返回 ok=false，让调用方告警而不是静默")
	}
	summary := SkipSummary(cands)
	if summary[SkipCooling] != 1 || summary[SkipDisabled] != 1 {
		t.Fatalf("summary=%v", summary)
	}
}

// —— 延迟基线 ——

func mkEntries(n int, ttfb int64, start time.Time, source, uid, model string) []HistoryEntry {
	out := make([]HistoryEntry, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, HistoryEntry{
			Time: start.Add(time.Duration(i) * time.Minute), Source: source,
			UID: uid, Model: model, OK: true, TTFBMs: ttfb,
		})
	}
	return out
}

// 热身期内绝不报：样本少时中位数不稳，误报比不报更糟。
func TestLatencyQuietDuringWarmup(t *testing.T) {
	now := time.Now()
	entries := mkEntries(50, 1000, now.Add(-3*24*time.Hour), "bypass", "u1", "m1")
	att := Attempt{UID: "u1", Model: "m1", Status: 200, TTFBMs: 20000}
	if fs := JudgeLatency(att, "bypass", entries, BaselineOptions{}); len(fs) != 0 {
		t.Fatalf("热身期不应报：%+v", fs)
	}
}

func TestLatencyReportsAfterWarmup(t *testing.T) {
	now := time.Now()
	// 20 天前开始积累，样本 30 个，中位 1s。
	entries := mkEntries(30, 1000, now.Add(-20*24*time.Hour), "bypass", "u1", "m1")
	// 上一次已经慢过一次（Consecutive=2 要求历史里已有 1 次命中）。
	entries = append(entries, HistoryEntry{
		Time: now.Add(-10 * time.Minute), Source: "bypass", UID: "u1", Model: "m1",
		OK: true, TTFBMs: 20000,
	})
	att := Attempt{UID: "u1", Model: "m1", Status: 200, TTFBMs: 20000}
	fs := JudgeLatency(att, "bypass", entries, BaselineOptions{})
	if len(fs) != 1 || fs[0].Code != "latency_degraded" {
		t.Fatalf("got %+v", fs)
	}
}

// 单次抖动不报：历史里没有前一次命中时，Consecutive=2 应当拦住。
func TestLatencySingleSpikeIsQuiet(t *testing.T) {
	now := time.Now()
	entries := mkEntries(30, 1000, now.Add(-20*24*time.Hour), "bypass", "u1", "m1")
	att := Attempt{UID: "u1", Model: "m1", Status: 200, TTFBMs: 20000}
	if fs := JudgeLatency(att, "bypass", entries, BaselineOptions{}); len(fs) != 0 {
		t.Fatalf("单次抖动不应报：%+v", fs)
	}
}

// 基线必须按来源分桶：旁路 6.5s 与网关 1.8s 不可混算，否则第一次就误报。
func TestLatencyBaselineIsPerSource(t *testing.T) {
	now := time.Now()
	gw := mkEntries(30, 1800, now.Add(-20*24*time.Hour), "gateway", "u1", "m1")
	att := Attempt{UID: "u1", Model: "m1", Status: 200, TTFBMs: 6500}
	if fs := JudgeLatency(att, "bypass", gw, BaselineOptions{}); len(fs) != 0 {
		t.Fatalf("跨来源比较导致误报：%+v", fs)
	}
}

func TestMedianTTFBIgnoresFailures(t *testing.T) {
	now := time.Now()
	entries := mkEntries(10, 1000, now, "bypass", "u1", "m1")
	entries = append(entries, HistoryEntry{
		Time: now, Source: "bypass", UID: "u1", Model: "m1", OK: false, TTFBMs: 99999,
	})
	med, n, ok := MedianTTFB(entries, "bypass", false, "u1", "m1")
	if !ok || n != 10 || med != time.Second {
		t.Fatalf("median=%v n=%d ok=%v（失败样本不应进基线）", med, n, ok)
	}
}

// —— 流解析：按实测形状校验（不依赖网络） ——

// fakeChat 用预置的 SSE 文本替代真实上游。
// 抽这个假实现的意义：协议漂移的每个分支都必须能重复验证，真打上游既烧额度
// 又不稳定（上游一改就可能"恰好正常"，掩盖漏检）。
type fakeChat struct {
	stream string
	status int
	body   []byte
	err    error
}

func (f fakeChat) ChatStream(a *auth.Auth, body []byte, ip string, meta upstream.ChatMeta) (io.ReadCloser, int, []byte, error) {
	if f.err != nil {
		return nil, f.status, f.body, f.err
	}
	return io.NopCloser(strings.NewReader(f.stream)), f.status, f.body, nil
}

// 实测形状（2026-10-23 spike）：max_tokens=1 时 content 为空，token 落在
// reasoning_content，finish_reason=length。这个形状必须被判定为健康。
const realShape = `data: {"id":"x","model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}

data: {"id":"x","model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"reasoning_content":"The"},"finish_reason":null}]}

data: {"id":"x","model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":31,"completion_tokens":1,"total_tokens":32,"credit":0}}

data: [DONE]

`

func TestRunAcceptsRealUpstreamShape(t *testing.T) {
	p := &Probe{Chat: fakeChat{stream: realShape, status: 200}, Primary: "deepseek-v4.1-flash"}
	att := p.Run(LevelL1, &auth.Auth{UID: "u1", Nickname: "A"}, false)

	if att.Status != 200 || !att.SawDone {
		t.Fatalf("status=%d sawDone=%v", att.Status, att.SawDone)
	}
	if len(att.Drift) != 0 {
		t.Fatalf("真实形状被判成漂移：%v", att.Drift)
	}
	if !att.OK() {
		t.Fatal("OK() 应为 true")
	}
	if att.Finish != "length" {
		t.Errorf("finish=%q", att.Finish)
	}
	if v, _ := numField(att.Usage, "total_tokens"); v != 32 {
		t.Errorf("total_tokens=%v", v)
	}
	if att.Frames != 3 {
		t.Errorf("frames=%d want 3（[DONE] 不计帧）", att.Frames)
	}
}

// content 为空**不能**判为漂移——这是最容易写错的一条。
func TestRunEmptyContentIsNotDrift(t *testing.T) {
	p := &Probe{Chat: fakeChat{stream: realShape, status: 200}, Primary: "deepseek-v4.1-flash"}
	att := p.Run(LevelL1, &auth.Auth{UID: "u1"}, false)
	for _, d := range att.Drift {
		if strings.Contains(d, "content") {
			t.Fatalf("content 被当成漂移项：%v", att.Drift)
		}
	}
}

func TestRunDetectsMissingUsage(t *testing.T) {
	noUsage := `data: {"model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"content":""},"finish_reason":"stop"}]}

data: [DONE]

`
	p := &Probe{Chat: fakeChat{stream: noUsage, status: 200}, Primary: "deepseek-v4.1-flash"}
	att := p.Run(LevelL1, &auth.Auth{UID: "u1"}, false)
	if !hasDrift(att.Drift, "usage_missing") {
		t.Fatalf("drift=%v", att.Drift)
	}
}

func TestRunDetectsMissingDone(t *testing.T) {
	noDone := `data: {"model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}],"usage":{"total_tokens":5,"credit":0}}

`
	p := &Probe{Chat: fakeChat{stream: noDone, status: 200}, Primary: "deepseek-v4.1-flash"}
	att := p.Run(LevelL1, &auth.Auth{UID: "u1"}, false)
	if !hasDrift(att.Drift, "no_done_frame") {
		t.Fatalf("drift=%v", att.Drift)
	}
}

func TestRunDetectsModelMismatch(t *testing.T) {
	other := `data: {"model":"something-else","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}],"usage":{"total_tokens":5,"credit":0}}

data: [DONE]

`
	p := &Probe{Chat: fakeChat{stream: other, status: 200}, Primary: "deepseek-v4.1-flash"}
	att := p.Run(LevelL1, &auth.Auth{UID: "u1"}, false)
	if !hasDriftPrefix(att.Drift, "model_mismatch:") {
		t.Fatalf("drift=%v", att.Drift)
	}
}

func TestRunDetectsUnknownFinishReason(t *testing.T) {
	weird := `data: {"model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"something_new"}],"usage":{"total_tokens":5,"credit":0}}

data: [DONE]

`
	p := &Probe{Chat: fakeChat{stream: weird, status: 200}, Primary: "deepseek-v4.1-flash"}
	att := p.Run(LevelL1, &auth.Auth{UID: "u1"}, false)
	if !hasDriftPrefix(att.Drift, "unknown_finish_reason:") {
		t.Fatalf("drift=%v", att.Drift)
	}
}

// 上游明确拒绝：必须提取业务码，且归类为「不是网络问题」。
func TestRunCapturesBusinessCode(t *testing.T) {
	body := []byte(`{"code":"11-128","msg":"request illegal"}`)
	p := &Probe{
		Chat:    fakeChat{status: 403, body: body, err: &upstream.Error{Kind: upstream.ErrChannelRefused, Status: 403, Msg: "request illegal"}},
		Primary: "deepseek-v4.1-flash",
	}
	att := p.Run(LevelL1, &auth.Auth{UID: "u1"}, false)
	if att.Code != "11-128" {
		t.Fatalf("code=%q", att.Code)
	}
	if att.Network {
		t.Fatal("业务拒绝不应标成网络失败")
	}
	fs := Judge(att)
	if len(fs) != 1 || fs[0].Code != "channel_refused" {
		t.Fatalf("got %+v", fs)
	}
}

// 传输层失败：status 恒 0，必须标 Network 以区别于上游策略。
func TestRunMarksTransportFailure(t *testing.T) {
	p := &Probe{Chat: fakeChat{status: 0, err: errors.New("dial tcp: i/o timeout")}, Primary: "m"}
	att := p.Run(LevelL1, &auth.Auth{UID: "u1"}, false)
	if !att.Network {
		t.Fatal("传输层失败必须标 Network")
	}
	if att.OK() {
		t.Fatal("OK() 应为 false")
	}
}

// L3：带提示词的请求体必须真的含 system 消息，且 token 数明显高于 L1。
func TestL3BodyCarriesPrompt(t *testing.T) {
	l1 := BuildBody("m")
	l3 := rewriteBody(l1, "You are a helpful assistant with a long system prompt.")
	if !strings.Contains(string(l3), `"role":"system"`) {
		t.Fatalf("L3 body 缺 system：%s", l3)
	}
	if len(l3) <= len(l1) {
		t.Fatal("L3 body 应比 L1 长")
	}
}

func hasDrift(drift []string, want string) bool {
	for _, d := range drift {
		if d == want {
			return true
		}
	}
	return false
}

func hasDriftPrefix(drift []string, prefix string) bool {
	for _, d := range drift {
		if strings.HasPrefix(d, prefix) {
			return true
		}
	}
	return false
}

// 冷/热连接必须分桶：实测冷 6.6s、热 1.0s，差 6 倍。
// 混在一起会让中位数变成双峰，任何阈值都判不准——这是上线首日就撞到的真实缺陷。
func TestLatencyBaselineSplitsColdFromHot(t *testing.T) {
	now := time.Now()
	// 热基线：30 个样本，中位 1s。
	hot := mkEntries(30, 1000, now.Add(-20*24*time.Hour), "bypass", "u1", "m1")
	// 本次是冷连接（新进程首个请求），TTFB 6.6s —— 属正常冷启动成本，不该报。
	att := Attempt{UID: "u1", Model: "m1", Status: 200, TTFBMs: 6600, Cold: true}
	if fs := JudgeLatency(att, "bypass", hot, BaselineOptions{}); len(fs) != 0 {
		t.Fatalf("冷连接不应套用热基线：%+v", fs)
	}
	// 同样 6.6s 但标为热连接时，才进入判定路径（仍需连续命中）。
	attHot := att
	attHot.Cold = false
	if fs := JudgeLatency(attHot, "bypass", hot, BaselineOptions{}); len(fs) != 0 {
		t.Fatalf("首次超阈（无历史命中）不应报：%+v", fs)
	}
}

// 冷基线独立累积到足够样本后，冷连接的劣化仍然可以被判出来。
func TestLatencyColdBucketCanStillAlert(t *testing.T) {
	now := time.Now()
	cold := []HistoryEntry{}
	for i := 0; i < 30; i++ {
		cold = append(cold, HistoryEntry{
			Time:   now.Add(-20*24*time.Hour + time.Duration(i)*time.Minute),
			Source: "bypass", UID: "u1", Model: "m1", OK: true, TTFBMs: 6000, Cold: true,
		})
	}
	cold = append(cold, HistoryEntry{
		Time: now.Add(-10 * time.Minute), Source: "bypass", UID: "u1",
		Model: "m1", OK: true, TTFBMs: 20000, Cold: true,
	})
	att := Attempt{UID: "u1", Model: "m1", Status: 200, TTFBMs: 20000, Cold: true}
	fs := JudgeLatency(att, "bypass", cold, BaselineOptions{})
	if len(fs) != 1 || fs[0].Code != "latency_degraded" {
		t.Fatalf("冷桶自身劣化应可判出：%+v", fs)
	}
}
