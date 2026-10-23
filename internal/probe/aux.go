// aux.go 辅助链路探测：chat 之外的三条关键只读链路。
//
// 为什么必须单独一层
//
//	L1~L3 只打 /v2/chat/completions。但签到、积分、成长任务走的是另外两个域：
//	  - www.codebuddy.cn  /billing/meter/*      （余额、签到、补偿）
//	  - www.workbuddy.cn  /activity/growth/*    （连登、抽奖、热力图）
//	上游收紧时，chat 仍可能 200，而计费域已经开始返回新码或新结构——此时用户看到的
//	现象是「请求都正常，但积分不再增长」。这种静默失效比通道被拒更隐蔽，因为它
//	不产生任何错误，只是额度悄悄停止累积。
//
// 为什么全部选只读端点
//
//	探测绝不能产生业务副作用。本层只用四个**查询**语义的调用：查余额、查补偿状态、
//	查连登状态、查心跳图。没有签到、没有领奖、没有任务上报——那些是写操作，
//	放进探测就会变成「探测本身在刷业务」。
//
//	其中余额查询必须走 UserResourceReadOnly 而不是 UserResourceSnapshot：
//	后者会通知余额观察者，把探测看到的值写进积分流水，造出一条不存在的变动。
package probe

import (
	"fmt"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// Aux 辅助链路所需的只读上游能力。抽接口的原因与 Chatter 相同：判定逻辑要能
// 脱离网络覆盖，否则每个错误分支都得真打上游。
type Aux interface {
	// UserResourceReadOnly 查余额（不触发积分流水观察者）。
	UserResourceReadOnly(a *auth.Auth) (remain, total int64, err error)
	// GetCompensationStatus 查补偿活动状态（只读，不领取）。
	GetCompensationStatus(a *auth.Auth) (upstream.CompensationStatus, error)
	// GrowthStreakFull 查连登状态（GET，只读）。
	GrowthStreakFull(a *auth.Auth) (*upstream.StreakFull, error)
}

// 端点标识。落进 history/快照时必须稳定——运维按它判断「是哪条链路先坏」。
const (
	EpBalance      = "billing.balance"
	EpCompensation = "billing.compensation"
	EpGrowthStreak = "activity.growth.streak"
)

// AuxAttempt 一条辅助链路的探测观测。
type AuxAttempt struct {
	Level    Level  `json:"level"`
	Endpoint string `json:"endpoint"`
	UID      string `json:"uid"`
	Nick     string `json:"nick"`
	Status   int    `json:"status"` // 200 成功；0 = 未拿到 HTTP 响应
	Code     string `json:"code,omitempty"`
	Msg      string `json:"msg,omitempty"`
	ErrText  string `json:"err,omitempty"`
	TotalMs  int64  `json:"total_ms"`
	Network  bool   `json:"network,omitempty"`
	// Drift 结构漂移（字段缺失/语义不符）；空 = 结构符合预期。
	Drift []string `json:"drift,omitempty"`
	// Value 该端点的关键观测值摘要（如余额、连登天数），仅用于展示与人工比对。
	Value string `json:"value,omitempty"`
}

// OK 该端点是否可用且结构正常。
func (a AuxAttempt) OK() bool {
	return a.Status == 200 && len(a.Drift) == 0 && !a.Network
}

// RunAux 对指定账号跑一遍全部辅助链路。任一端点失败都会产出独立的 Attempt，
// 而不是整体失败——「余额查得到但成长任务查不到」正是需要区分的情形。
func (p *Probe) RunAux(level Level, a *auth.Auth) []AuxAttempt {
	if p.Aux == nil {
		return nil
	}
	out := make([]AuxAttempt, 0, 3)

	// 1) 余额。这是 credit_floor 判定的唯一数据源，坏了会直接影响选号决策。
	out = append(out, timed(level, EpBalance, a, func() (int, string, []string) {
		remain, total, err := p.Aux.UserResourceReadOnly(a)
		if err != nil {
			return 0, "", []string{err.Error()}
		}
		drift := []string{}
		if remain < 0 {
			drift = append(drift, "balance_negative")
		}
		if total < remain {
			drift = append(drift, "total_lt_remain")
		}
		return 200, fmt.Sprintf("remain=%d total=%d", remain, total), drift
	}))

	// 2) 补偿状态。鉴权或端点变化会让「有没有补偿可领」变成盲区。
	out = append(out, timed(level, EpCompensation, a, func() (int, string, []string) {
		st, err := p.Aux.GetCompensationStatus(a)
		if err != nil {
			return 0, "", []string{err.Error()}
		}
		return 200, fmt.Sprintf("active=%v claimed=%v credit=%d", st.Active, st.Claimed, st.CreditNum), nil
	}))

	// 3) 成长/连登。这里能看出 growth 域是否被单独收紧。
	out = append(out, timed(level, EpGrowthStreak, a, func() (int, string, []string) {
		s, err := p.Aux.GrowthStreakFull(a)
		if err != nil {
			return 0, "", []string{err.Error()}
		}
		if s == nil {
			return 200, "", []string{"empty_body"}
		}
		// 连登状态至少应带天数；完全空响应通常意味着上游换了信封结构。
		return 200, fmt.Sprintf("streak_days=%d cards=%d", s.Streak.Days, s.MakeupCards.Balance), nil
	}))

	return out
}

// timed 执行一次辅助调用并归一化结果。
//
// 错误归一化在这里统一做：upstream 的 *Error 带 Kind/Status，普通 error 只有文案。
// 探测层只关心「是不是网络问题」「HTTP 码是多少」「业务码是多少」，所以在这里
// 把差异抹平，判定层（JudgeAux）就只处理归一化后的 Attempt。
func timed(level Level, endpoint string, a *auth.Auth, fn func() (int, string, []string)) AuxAttempt {
	att := AuxAttempt{Level: level, Endpoint: endpoint, UID: a.UID, Nick: a.Nickname}
	start := time.Now()
	status, value, problems := fn()
	att.TotalMs = time.Since(start).Milliseconds()

	switch {
	case status != 0:
		att.Status = status
		att.Value = value
		att.Drift = problems
	case len(problems) == 0:
		att.Status = 200
	default:
		// 失败：problems[0] 是错误文案，从中提取业务码与网络归因。
		msg := problems[0]
		att.ErrText = msg
		att.Code, att.Msg = ExtractCode(msg)
		att.Status, att.Network = classifyAuxErr(msg)
	}
	return att
}

// classifyAuxErr 从错误文案里区分「传输层失败」与「上游明确拒绝」。
//
// 判据沿用 Go net 错误文案的稳定片段，而不是类型断言：upstream 在多层包装里
// 可能已经把原始 error 转成字符串（fmt.Errorf 带 %v），类型信息丢失。
func classifyAuxErr(msg string) (status int, network bool) {
	lower := strings.ToLower(msg)
	for _, m := range []string{
		"dial tcp", "no such host", "i/o timeout", "connection refused",
		"tls:", "x509:", "context deadline exceeded", "connection reset",
		"eof", "broken pipe",
	} {
		if strings.Contains(lower, m) {
			return 0, true
		}
	}
	// 上游错误信封：upstream %s (http %d) 或 code=N
	if i := strings.Index(msg, "(http "); i >= 0 {
		rest := msg[i+6:]
		n := 0
		for _, r := range rest {
			if r < '0' || r > '9' {
				break
			}
			n = n*10 + int(r-'0')
		}
		if n > 0 {
			return n, false
		}
	}
	return 0, false
}

// JudgeAux 把辅助链路的观测翻译成结论。
//
// 与 Judge 的区别：这里的端点没有「模型」维度，因此不存在双模型交叉；但多了一层
// **跨端点聚合**——同一账号多个端点同时失败，说明是该账号或该域的共性问题，
// 不是单个端点的偶发。
func JudgeAux(attempts []AuxAttempt) []Finding {
	if len(attempts) == 0 {
		return nil
	}

	// 按账号聚合：同一账号在本轮里失败的端点数。
	byUID := map[string][]AuxAttempt{}
	for _, a := range attempts {
		byUID[a.UID] = append(byUID[a.UID], a)
	}

	var out []Finding
	for uid, list := range byUID {
		var failed []AuxAttempt
		for _, a := range list {
			if !a.OK() {
				failed = append(failed, a)
			}
		}
		if len(failed) == 0 {
			continue
		}

		nick := list[0].Nick
		allFailed := len(failed) == len(list)

		for _, a := range failed {
			f := Finding{
				UID: uid, Nick: nick, Model: a.Endpoint,
				Evidence: a.ErrText,
			}
			switch {
			case a.Network:
				f.Level, f.Code = "high", "aux_network_error"
				f.Detail = fmt.Sprintf("%s %s 网络层失败（%s）", label2(nick, uid), a.Endpoint, a.ErrText)
			case a.Code == "11-128":
				f.Level, f.Code = "critical", "channel_refused"
				f.Detail = fmt.Sprintf("%s %s 被通道校验拒绝（11-128）", label2(nick, uid), a.Endpoint)
			case a.Code != "":
				if meta, ok := knownCodes[a.Code]; ok && meta.Severity == "silent" {
					continue
				}
				lvl := "high"
				if meta, ok := knownCodes[a.Code]; ok {
					lvl = meta.Severity
				}
				f.Level, f.Code = lvl, "aux_unknown_code:"+a.Code
				f.Detail = fmt.Sprintf("%s %s 返回业务码 %s：%s",
					label2(nick, uid), a.Endpoint, a.Code, a.Msg)
			case len(a.Drift) > 0:
				f.Level, f.Code = "high", "aux_protocol_drift"
				f.Detail = fmt.Sprintf("%s %s 结构不符：%s",
					label2(nick, uid), a.Endpoint, strings.Join(a.Drift, ", "))
			default:
				f.Level, f.Code = "high", "aux_http_error"
				f.Detail = fmt.Sprintf("%s %s 返回 HTTP %d", label2(nick, uid), a.Endpoint, a.Status)
			}
			out = append(out, f)
		}

		// 全端点同时失败 → 升级为账号级/域级结论。单个端点挂可能只是它自己改动，
		// 全部挂说明该账号的凭证或整个域被收紧，两者处理优先级不同。
		if allFailed && len(list) >= 2 {
			// 把刚加进去的逐条结论收拢成一条，避免同一原因刷三行。
			kept := out[:len(out)-len(failed)]
			kept = append(kept, Finding{
				Level: "critical",
				Code:  "aux_all_endpoints_failed",
				UID:   uid,
				Nick:  nick,
				Detail: fmt.Sprintf("%s 的 %d 个辅助端点全部失败（%s）——账号凭证或整个计费/成长域被收紧",
					label2(nick, uid), len(list), joinEndpoints(list)),
				Evidence: failed[0].ErrText,
			})
			out = kept
		}
	}
	return out
}

func joinEndpoints(list []AuxAttempt) string {
	names := make([]string, 0, len(list))
	for _, a := range list {
		names = append(names, a.Endpoint)
	}
	return strings.Join(names, ", ")
}

func label2(nick, uid string) string {
	if nick != "" {
		return nick + "(" + shortUID(uid) + ")"
	}
	return shortUID(uid)
}
