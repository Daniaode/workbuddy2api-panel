// select.go 探测账号的选择：先剔除不该碰的号，再轮转。
//
// 为什么需要这么多道闸：探测本身是「为了发现问题」，如果它把正常账号打进冷却
// 或把触底号的余额烧干，就成了新故障源。每条跳过规则都对应一个具体事故形态。
package probe

import (
	"encoding/json"
	"os"
	"sort"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// SkipReason 跳过原因。要能区分「这号本来就不该用」和「排除过头了一个都没探」
// ——后者是探测自身的故障，必须单独告警。
type SkipReason string

const (
	SkipDisabled  SkipReason = "disabled"   // 已禁用（授权/session 终态，等人工 revive）
	SkipPaused    SkipReason = "paused"     // 暂停选号（运维临时让位）
	SkipCooling   SkipReason = "cooling"    // 冷却中
	SkipLowCredit SkipReason = "low_credit" // 余额低于保底 2 倍
	SkipTokenDue  SkipReason = "token_due"  // token 即将到期（此时 401 不能归因）
	SkipNoCreds   SkipReason = "no_credentials"
)

// Candidate 一个可探测账号及其跳过判定。
type Candidate struct {
	Auth    *auth.Auth
	UID     string
	Nick    string
	Credits int64
	Skip    SkipReason // 空 = 可探测
}

// AccountState 从 state.json 读到的最小快照。
//
// 只读 data/state.json 而**不**调面板 API：探测要能在网关进程没起、或者网关
// 自己就是故障源的时候继续工作。面板挂了不该连带让探测也瞎掉。
type AccountState struct {
	Credits  int64     `json:"credits"`
	Disabled bool      `json:"disabled"`
	Paused   bool      `json:"paused"`
	Until    time.Time `json:"until"`
}

// LoadAccountStates 读 state.json 里每个账号的池状态。文件缺失或解析失败
// 不报错——返回空 map，调用方按「无状态信息」处理（仍可探测，只是少几道闸）。
func LoadAccountStates(path string) map[string]AccountState {
	out := map[string]AccountState{}
	raw, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var f struct {
		Accounts map[string]AccountState `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return out
	}
	return f.Accounts
}

// SelectOptions 选择参数。
type SelectOptions struct {
	// CreditFloor 账号余额低于它 2 倍时跳过（照 config 的 pool.credit_floor）。
	CreditFloor int64
	// TokenMargin token 到期前多久算「即将到期」。此窗口内 401 无法区分
	// 「该刷 token 了」和「session 真死了」，探测结论会被污染，故直接跳过。
	TokenMargin time.Duration
	// Now 注入时钟，便于测试。
	Now time.Time
}

// BuildCandidates 把 auth 列表按跳过规则分类。返回全部候选（含被跳过的），
// 调用方据此判断「一个都没剩下」并告警。
func BuildCandidates(auths []*auth.Auth, states map[string]AccountState, opt SelectOptions) []Candidate {
	if opt.Now.IsZero() {
		opt.Now = time.Now()
	}
	if opt.TokenMargin <= 0 {
		opt.TokenMargin = 2 * time.Hour
	}

	out := make([]Candidate, 0, len(auths))
	for _, a := range auths {
		c := Candidate{Auth: a, UID: a.UID, Nick: a.Nickname}
		st, hasState := states[a.UID]
		c.Credits = st.Credits

		switch {
		case a == nil || a.AccessToken == "":
			c.Skip = SkipNoCreds
		case hasState && st.Disabled:
			c.Skip = SkipDisabled
		case hasState && st.Paused:
			c.Skip = SkipPaused
		case hasState && !st.Until.IsZero() && st.Until.After(opt.Now):
			c.Skip = SkipCooling
		case opt.CreditFloor > 0 && hasState && st.Credits > 0 && st.Credits < opt.CreditFloor*2:
			c.Skip = SkipLowCredit
		case tokenDue(a, opt.Now, opt.TokenMargin):
			c.Skip = SkipTokenDue
		}
		out = append(out, c)
	}
	// 顺序固定：auth 文件的 glob 顺序随文件系统变化，不稳定会导致轮转游标漂移。
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}

// tokenDue 判断 token 是否即将到期。ExpiresAt 为 0 表示上游没给，按「未知但
// 不阻塞」处理——不能因为拿不到过期时间就永远跳过这个号。
func tokenDue(a *auth.Auth, now time.Time, margin time.Duration) bool {
	if a == nil || a.ExpiresAt == 0 {
		return false
	}
	return now.Add(margin).After(time.Unix(a.ExpiresAt, 0))
}

// Pick 按游标挑一个可探测账号。返回被选中的候选与「本轮全部跳过原因统计」。
//
// 纯函数式轮转（游标由调用方持久化）而不是随机：随机会让「同一个号连续失败」
// 难以复现，固定轮转能让问题账号在几轮内必然被覆盖到。
func Pick(cands []Candidate, cursor int) (Candidate, bool) {
	var ok []Candidate
	for _, c := range cands {
		if c.Skip == "" {
			ok = append(ok, c)
		}
	}
	if len(ok) == 0 {
		return Candidate{}, false
	}
	return ok[((cursor%len(ok))+len(ok))%len(ok)], true
}

// SkipSummary 统计跳过原因，供「覆盖为零」告警说清是被哪条规则拦完的。
func SkipSummary(cands []Candidate) map[SkipReason]int {
	out := map[SkipReason]int{}
	for _, c := range cands {
		if c.Skip != "" {
			out[c.Skip]++
		}
	}
	return out
}
