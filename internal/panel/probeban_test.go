package panel

import (
	"errors"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// classifyBanProbe 的判定必须把「真封禁」「只是被拦」「登录失效」「无法判定」分开。
//
// 这四类对用户的下一步动作完全不同：
//   - still_banned  → 去官方申诉，等到期也不会好
//   - lifted        → 可以解冻
//   - session_dead  → 重新登录
//   - inconclusive  → 稍后再试，别乱动
//
// 判错方向的代价：把 still_banned 误报成 lifted，用户解冻后账号立刻再撞 11140，
// 反而加重风控；把 blocked 误报成 session_dead，用户会白重登一次。
func TestClassifyBanProbe(t *testing.T) {
	restricted := &upstream.Error{
		Kind:   upstream.ErrAccountFault,
		Status: 403,
		Msg:    `{"code":11140,"msg":"request illegal","displayMsg":{"zh":"账号存在异常行为，已被限制"}}`,
	}
	sessionDead := &upstream.Error{Kind: upstream.ErrSessionDead, Status: 401, Msg: "offline session not found"}
	softRate := &upstream.Error{Kind: upstream.ErrSoftRate, Status: 429, Msg: "rate limited"}
	waf := &upstream.Error{Kind: upstream.ErrWafBlock, Status: 403, Msg: "<html>APISIX</html>"}
	other := &upstream.Error{Kind: upstream.ErrServer, Status: 500, Msg: "boom"}

	cases := []struct {
		name        string
		err         error
		refreshOK   bool
		needRefresh bool
		want        BanVerdict
	}{
		{"余额查询成功 → 已解封", nil, false, false, BanLifted},
		{"11140 账号限制 → 仍封禁", restricted, false, false, BanStillBanned},
		{"会话失效 + refresh 成功 → 只是被拦", sessionDead, true, true, BanBlocked},
		{"会话失效 + refresh 被拒 → 真失效", sessionDead, false, true, BanSessionDead},
		{"限流 → 无法判定", softRate, false, false, BanInconclusive},
		{"WAF 页 → 无法判定", waf, false, false, BanInconclusive},
		{"5xx → 无法判定", other, false, false, BanInconclusive},
		{"网络错误 → 无法判定", errors.New("dial tcp: i/o timeout"), false, false, BanInconclusive},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyBanProbe(c.err, c.refreshOK, c.needRefresh)
			if got.Verdict != c.want {
				t.Errorf("verdict=%s want %s（detail=%s）", got.Verdict, c.want, got.Detail)
			}
			if got.Detail == "" {
				t.Error("detail 不应为空：用户需要知道判据是什么")
			}
		})
	}
}

// 11140 的判定优先于 Kind：上游把它归为 ErrAccountFault，但也可能以 403 信封出现。
// 只看 Kind 会把封禁误判成「暂时性账号故障」，让用户以为等等就好。
func TestClassifyBanProbeRestrictedTakesPrecedence(t *testing.T) {
	// 同一个 body，但 Kind 是 ErrSoftRate（限流语义）——11140 才是决定性判据。
	weird := &upstream.Error{
		Kind:   upstream.ErrSoftRate,
		Status: 429,
		Msg:    `{"code":11140,"msg":"request illegal"}`,
	}
	got := classifyBanProbe(weird, false, false)
	if got.Verdict != BanStillBanned {
		t.Errorf("verdict=%s want %s：11140 判据必须优先于 Kind", got.Verdict, BanStillBanned)
	}
}

// 11128（通道校验失败）不能被误判成封禁：它们 msg 同为 "request illegal"，
// 但 11128 是「不是官方客户端发的」，账号本身健康。
func TestClassifyBanProbeChannelRefusedIsNotBan(t *testing.T) {
	channel := &upstream.Error{
		Kind:   upstream.ErrChannelRefused,
		Status: 400,
		Msg:    `{"code":11128,"msg":"request illegal"}`,
	}
	got := classifyBanProbe(channel, false, false)
	if got.Verdict == BanStillBanned {
		t.Fatal("11128 通道校验失败被误判成账号封禁——账号其实健康，误报会让用户白去申诉")
	}
	if got.Verdict != BanInconclusive {
		t.Errorf("verdict=%s want %s", got.Verdict, BanInconclusive)
	}
}

// clipStr 截断行为。
func TestClipStr(t *testing.T) {
	if got := clipStr("short", 20); got != "short" {
		t.Errorf("短串不应被截断，得到 %q", got)
	}
	long := "0123456789012345678901234567890"
	got := clipStr(long, 10)
	if len([]rune(got)) > 12 {
		t.Errorf("截断后过长: %q", got)
	}
}
