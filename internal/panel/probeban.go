// probeban.go 封禁账号「只读探活」：不惊动账号地问出上游当前状态。
//
// 为什么需要它
//
//	账号被 11140 判死后，唯一的确认方式是面板点「解冻」再发请求——但解冻会让它
//	立刻重新参与选号并再撞一次 11140，反复触发可能加重上游的风控判定。需要一个
//	「不改池状态、不占在途、不记连败」的问法。
//
// 做法
//
//	用 UserResource（余额查询）——最轻的只读端点：不进选号、不占在途名额、
//	不参与熔断与连败计数。**全程不改池状态**：探测是观测行为，改了就等于变相解冻。
//
// 判定（五类结论，各带下一步动作）
//
//	lifted        上游已解封（余额查询通了）→ 可以点解冻
//	still_banned  仍是 11140 账号限制       → 继续等待 / 去官方申诉
//	blocked       会话活着但被拦（非 11140）→ 稍后再试
//	session_dead  登录态失效（refresh 也被拒）→ 需要重新登录
//	inconclusive  网络/限流等无法判定       → 稍后再试
package panel

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// BanVerdict 只读探活的结论。
type BanVerdict string

const (
	BanLifted       BanVerdict = "lifted"
	BanStillBanned  BanVerdict = "still_banned"
	BanBlocked      BanVerdict = "blocked"
	BanSessionDead  BanVerdict = "session_dead"
	BanInconclusive BanVerdict = "inconclusive"
)

// accountProbeBan POST /panel/api/accounts/{uid}/probe_ban
//
// 只读探活：判断被封账号当前在上游是否仍受限。**绝不修改池状态**。
//
// 仅对国际版（realm=global）账号开放：11140「账号存在异常行为」是目前只在
// 国际号上观测到的封禁形态；国内号更多是 6004 限流 / 11158 无权限，用「余额」
// 按钮就能间接判断，给一个看似通用的按钮反而会误导。
func (p *Panel) accountProbeBan(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	if !a.IsGlobal() {
		writeErr(w, http.StatusBadRequest,
			"只读探活仅用于国际版账号（11140 封禁形态）；国内号请直接用「余额」按钮判断")
		return
	}
	if p.cfg.Upstream == nil {
		writeErr(w, http.StatusNotImplemented, "upstream not available")
		return
	}

	res := p.probeBanVerdict(a, uid)
	log.Printf("panel: probe_ban uid=%s verdict=%s detail=%s", uid, res.Verdict, res.Detail)

	resp := map[string]any{
		"ok":      true,
		"uid":     uid,
		"verdict": string(res.Verdict),
		"detail":  res.Detail,
		// 明确告知调用方：本次探测没有改动任何池状态。
		"state_changed": false,
	}
	switch res.Verdict {
	case BanLifted:
		resp["advice"] = "上游已恢复，可点「解冻」让该账号重新参与选号"
	case BanStillBanned:
		resp["advice"] = "仍被上游限制：可在 WorkBuddy 官方客户端提交反馈申诉；解封后再点「解冻」"
	case BanSessionDead:
		resp["advice"] = "登录态已失效：需要移除该账号后重新登录"
	case BanBlocked:
		resp["advice"] = "会话仍活但当前被拦（非 11140 封禁），稍后再试"
	default:
		resp["advice"] = "无法判定（网络/限流），稍后再试"
	}
	writeJSON(w, http.StatusOK, resp)
}

// banProbeResult 探活结论 + 说明。
type banProbeResult struct {
	Verdict BanVerdict
	Detail  string
}

// probeBanVerdict 用最轻的只读端点判定账号当前状态（只读，不改任何池状态）。
//
// 判据顺序（每一步都在前面的结论不成立时才走）：
//  1. 余额查询成功 → lifted（连最轻的只读端点都通了，账号已恢复）
//  2. 余额查询明确报 11140 → still_banned（账号级限制，最可靠信号）
//  3. 余额查询报会话失效 → 用 refresh 求证：
//     refresh 成功 = 会话还活着、只是这次调用被拦（blocked）
//     refresh 被拒 = 登录态真失效（session_dead）
//  4. 其余（WAF/限流/网络）→ inconclusive
//
// 第 3 步的求证很关键：实测 11140 封禁中的账号，余额端点返回的也可能是 401
// APISIX HTML 页，与登录失效的响应体完全一样，只按 401 判会误报 session_dead。
func (p *Panel) probeBanVerdict(a *auth.Auth, uid string) banProbeResult {
	// 只读探测：UserResource 不进选号、不占在途、不记连败。
	_, _, err := p.cfg.Upstream.UserResource(a)
	if err == nil {
		return banProbeResult{BanLifted, "余额查询成功，上游已恢复"}
	}

	var ue *upstream.Error
	isSessionDead := errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead

	// 会话失效时用 refresh 求证「真失效」还是「只是被拦」，避免误报。
	refreshOK := false
	if isSessionDead {
		refreshOK = p.cfg.Upstream.RefreshToken(a) == nil
	}

	res := classifyBanProbe(err, refreshOK, isSessionDead)
	// 保留一份原始错误摘要，便于用户反馈给上游。
	if res.Verdict != BanLifted && strings.TrimSpace(res.Detail) == "" {
		res.Detail = clipStr(err.Error(), 120)
	}
	return res
}

// classifyBanProbe 把「余额查询的错误」翻译成结论（纯函数，便于表驱动测试）。
//
// needRefresh 为真时 refreshOK 才有意义：
//   - refreshOK=true  → 会话仍活，只是本次调用被拦（blocked）
//   - refreshOK=false → 登录态真失效（session_dead）
func classifyBanProbe(balanceErr error, refreshOK bool, needRefresh bool) banProbeResult {
	if balanceErr == nil {
		return banProbeResult{BanLifted, "余额查询成功，上游已恢复"}
	}

	var ue *upstream.Error
	if errors.As(balanceErr, &ue) {
		// 明确 11140 账号级限制：最可靠的封禁信号，优先于 Kind 判定。
		if upstream.IsAccountRestricted(ue.Msg) {
			return banProbeResult{BanStillBanned, "上游返回 11140 账号限制"}
		}
		switch ue.Kind {
		case upstream.ErrSessionDead:
			if needRefresh && refreshOK {
				return banProbeResult{BanBlocked, "会话仍活（refresh 成功），本次调用被拦"}
			}
			return banProbeResult{BanSessionDead, "会话失效且 refresh 未通过，需重新登录"}
		case upstream.ErrSoftRate:
			return banProbeResult{BanInconclusive, "上游限流，无法判定"}
		case upstream.ErrWafBlock:
			return banProbeResult{BanInconclusive, "WAF 拦截页，无法判定"}
		}
		return banProbeResult{BanInconclusive,
			"上游返回 " + ue.Kind.String() + "（http " + strconv.Itoa(ue.Status) + "）"}
	}

	// 传输层错误（DNS/超时/连接）：与账号状态无关。
	return banProbeResult{BanInconclusive, "网络错误：" + clipStr(balanceErr.Error(), 100)}
}

// clipStr 截断过长文案。
func clipStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.TrimSpace(s[:max]) + "…"
}
