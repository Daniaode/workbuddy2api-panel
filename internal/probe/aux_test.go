package probe

import (
	"errors"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// fakeAux 用可控返回值替代真实上游。
//
// 为什么必须能注入：判定逻辑的每个分支（网络失败 / 业务码 / 结构漂移 / 全端点失败）
// 都要能重复验证。真打上游既烧额度，又不能制造出「三个端点同时挂」这种状态。
type fakeAux struct {
	balance func() (int64, int64, error)
	comp    func() (upstream.CompensationStatus, error)
	streak  func() (*upstream.StreakFull, error)
}

func (f fakeAux) UserResourceReadOnly(a *auth.Auth) (int64, int64, error) {
	if f.balance == nil {
		return 1000, 2000, nil
	}
	return f.balance()
}

func (f fakeAux) GetCompensationStatus(a *auth.Auth) (upstream.CompensationStatus, error) {
	if f.comp == nil {
		return upstream.CompensationStatus{Active: false}, nil
	}
	return f.comp()
}

func (f fakeAux) GrowthStreakFull(a *auth.Auth) (*upstream.StreakFull, error) {
	if f.streak == nil {
		s := &upstream.StreakFull{}
		s.Streak.Days = 5
		s.MakeupCards.Balance = 2
		return s, nil
	}
	return f.streak()
}

func okAux() fakeAux { return fakeAux{} }

func runAux(t *testing.T, aux Aux) []AuxAttempt {
	t.Helper()
	p := &Probe{Aux: aux}
	return p.RunAux(LevelL4, &auth.Auth{UID: "u1", Nickname: "A"})
}

// —— 全部正常时必须完全静默 ——

func TestAuxAllHealthyProducesNoFindings(t *testing.T) {
	atts := runAux(t, okAux())
	if len(atts) != 3 {
		t.Fatalf("应探测 3 个端点，得到 %d", len(atts))
	}
	for _, a := range atts {
		if !a.OK() {
			t.Fatalf("%s 应为 OK：%+v", a.Endpoint, a)
		}
	}
	if fs := JudgeAux(atts); len(fs) != 0 {
		t.Fatalf("健康不该产结论：%+v", fs)
	}
}

// —— 单端点失败：只报这一条，不升级 ——

func TestAuxSingleEndpointFailure(t *testing.T) {
	aux := okAux()
	aux.balance = func() (int64, int64, error) {
		return 0, 0, &upstream.Error{Kind: upstream.ErrClient, Status: 400, Msg: `code=11140 msg=request illegal`}
	}
	atts := runAux(t, aux)
	fs := JudgeAux(atts)

	if len(fs) != 1 {
		t.Fatalf("单端点失败应只产 1 条结论：%+v", fs)
	}
	if fs[0].Code != "aux_unknown_code:11140" && !strings.Contains(fs[0].Code, "11140") {
		t.Fatalf("应带出业务码：%+v", fs[0])
	}
	if fs[0].Model != EpBalance {
		t.Errorf("结论应标明端点，得到 %q", fs[0].Model)
	}
}

// —— 全端点失败：收敛成一条账号级结论，不刷三行 ——

func TestAuxAllEndpointsFailedCollapsesToOne(t *testing.T) {
	boom := func() error {
		return &upstream.Error{Kind: upstream.ErrClient, Status: 400, Msg: `code=11140 msg=request illegal`}
	}
	aux := fakeAux{
		balance: func() (int64, int64, error) { return 0, 0, boom() },
		comp:    func() (upstream.CompensationStatus, error) { return upstream.CompensationStatus{}, boom() },
		streak:  func() (*upstream.StreakFull, error) { return nil, boom() },
	}
	fs := JudgeAux(runAux(t, aux))

	if len(fs) != 1 {
		t.Fatalf("全端点失败应收敛为 1 条，得到 %d：%+v", len(fs), fs)
	}
	if fs[0].Code != "aux_all_endpoints_failed" || fs[0].Level != "critical" {
		t.Fatalf("应升级为账号级 critical：%+v", fs[0])
	}
	// 三个端点名都要出现在文案里，否则无法判断是哪一层被收紧。
	for _, ep := range []string{EpBalance, EpCompensation, EpGrowthStreak} {
		if !strings.Contains(fs[0].Detail, ep) {
			t.Errorf("文案应列出 %s：%s", ep, fs[0].Detail)
		}
	}
}

// —— 网络失败与业务拒绝必须分开 ——

func TestAuxNetworkFailureIsSeparate(t *testing.T) {
	aux := okAux()
	aux.streak = func() (*upstream.StreakFull, error) {
		return nil, errors.New(`Get "https://www.workbuddy.cn/activity/growth/streak": dial tcp: i/o timeout`)
	}
	fs := JudgeAux(runAux(t, aux))
	if len(fs) != 1 || fs[0].Code != "aux_network_error" {
		t.Fatalf("网络失败应独立归类：%+v", fs)
	}
}

func TestClassifyAuxErr(t *testing.T) {
	cases := []struct {
		msg         string
		wantStatus  int
		wantNetwork bool
	}{
		{`Get "https://x": dial tcp: i/o timeout`, 0, true},
		{`Post "https://x": dial tcp: lookup y: no such host`, 0, true},
		{`tls: failed to verify certificate`, 0, true},
		{`upstream client (http 400): code=11102`, 400, false},
		{`upstream soft_rate (http 429): code=6004`, 429, false},
		{`something weird`, 0, false},
	}
	for _, c := range cases {
		st, net := classifyAuxErr(c.msg)
		if st != c.wantStatus || net != c.wantNetwork {
			t.Errorf("%q -> status=%d network=%v，want %d/%v", c.msg, st, net, c.wantStatus, c.wantNetwork)
		}
	}
}

// —— 结构漂移 ——

func TestAuxBalanceDrift(t *testing.T) {
	aux := okAux()
	aux.balance = func() (int64, int64, error) { return -5, 10, nil }
	fs := JudgeAux(runAux(t, aux))
	if len(fs) != 1 || fs[0].Code != "aux_protocol_drift" {
		t.Fatalf("负余额应判结构漂移：%+v", fs)
	}
}

func TestAuxBalanceTotalLessThanRemainIsDrift(t *testing.T) {
	aux := okAux()
	aux.balance = func() (int64, int64, error) { return 500, 100, nil }
	fs := JudgeAux(runAux(t, aux))
	if len(fs) != 1 || fs[0].Code != "aux_protocol_drift" {
		t.Fatalf("total<remain 应判结构漂移：%+v", fs)
	}
}

// —— 通道校验拒绝走 critical ——

func TestAuxChannelRefused(t *testing.T) {
	aux := okAux()
	aux.comp = func() (upstream.CompensationStatus, error) {
		return upstream.CompensationStatus{}, &upstream.Error{Kind: upstream.ErrChannelRefused, Status: 403, Msg: `code=11-128 msg=request illegal`}
	}
	fs := JudgeAux(runAux(t, aux))
	if len(fs) != 1 || fs[0].Code != "channel_refused" || fs[0].Level != "critical" {
		t.Fatalf("11-128 应判 critical：%+v", fs)
	}
}

// —— 静默码不报（6004 是常态限流）——

func TestAuxSilentCodeIsQuiet(t *testing.T) {
	aux := okAux()
	aux.comp = func() (upstream.CompensationStatus, error) {
		return upstream.CompensationStatus{}, &upstream.Error{Kind: upstream.ErrSoftRate, Status: 429, Msg: `code=6004 msg=rate limit`}
	}
	if fs := JudgeAux(runAux(t, aux)); len(fs) != 0 {
		t.Fatalf("6004 应静默：%+v", fs)
	}
}

// —— 空响应体 ——

func TestAuxNilStreakIsDrift(t *testing.T) {
	aux := okAux()
	aux.streak = func() (*upstream.StreakFull, error) { return nil, nil }
	fs := JudgeAux(runAux(t, aux))
	if len(fs) != 1 || fs[0].Code != "aux_protocol_drift" {
		t.Fatalf("空响应应判漂移：%+v", fs)
	}
}

// —— 无 Aux 时不得 panic ——

func TestRunAuxWithoutAuxIsSafe(t *testing.T) {
	p := &Probe{}
	if atts := p.RunAux(LevelL4, &auth.Auth{UID: "u1"}); atts != nil {
		t.Fatalf("Aux 为 nil 时应返回 nil，得到 %+v", atts)
	}
}
