// judge.go 把一次探测的观测值翻译成结论。
//
// 为什么观测与判定分开：观测（Attempt）只记录「上游回了我什么」，判定（Finding）
// 才是「这意味着什么」。两者分开后，每个判定分支都能用构造的 Attempt 直接测，
// 不必真打上游；上游改协议时也只需改判定表，不动采集代码。
package probe

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// 已知业务码。数值码走 JSON 的 code 字段；"11-128" 是带横线的字符串码。
//
// 这张表是本次探测的核心价值所在：上游收紧风控时通常**先加新码**，而不是把旧码
// 改成失败。未登记的码必须有独立出口并带上原文，否则新征兆会被静默吞掉。
type codeMeta struct {
	Severity string // critical / high / medium / low / silent
	Meaning  string
	Dual     bool // 建议换模型交叉验证（模型级拒绝时置位）
}

var knownCodes = map[string]codeMeta{
	"11-128": {Severity: "critical", Meaning: "通道校验拒绝（上游判定请求不是官方客户端发的）"},
	"11158":  {Severity: "high", Meaning: "该账号类型无权使用此模型", Dual: true},
	"11102":  {Severity: "high", Meaning: "该后端无此模型", Dual: true},
	"12153":  {Severity: "medium", Meaning: "offline session 失效（需重新登录）"},
	"6004":   {Severity: "silent", Meaning: "模型级限流（常态，不报）"},
	"6021":   {Severity: "low", Meaning: "渠道容量满载"},
	"11101":  {Severity: "high", Meaning: "请求体解析失败（参数问题）"},
	"11140":  {Severity: "high", Meaning: "账号级授权故障"},
	"14017":  {Severity: "high", Meaning: "trial 未激活"},
	"11115":  {Severity: "high", Meaning: "prompt 超出上下文上限"},
}

// codeRe 从错误体里提取业务码。覆盖三种形态（实测上游三种都出现过）：
//
//	{"code":11102,...}        数值
//	{"code":"11102",...}      字符串
//	{"code":"11-128",...}     带横线的通道校验码
var codeRe = regexp.MustCompile(`"code"\s*:\s*"?([0-9][0-9\-]{0,9})"?`)

// codeEqRe 兜底：internal/upstream 把错误信封归一化成 `code=11140 msg=...` 的文案
// 形态（见 doJSON 的 Error.Msg 构造），此时 JSON 键值正则匹配不到。
// 两条都要有，否则「上游明确拒绝」会被降级成笼统的 HTTP 错误，错误码分流整个失效。
var codeEqRe = regexp.MustCompile(`\bcode=([0-9][0-9\-]{0,9})`)

// msgRe 提取伴随文案，未知码要原样带出才可诊断。
var msgRe = regexp.MustCompile(`"(?:msg|message)"\s*:\s*"((?:[^"\\]|\\.){0,200})"`)

// msgEqRe 兜底：同一归一化文案里的 `msg=...`（取到行尾或下一个 key 为止）。
var msgEqRe = regexp.MustCompile(`\bmsg=([^\n]{0,200})`)

// ExtractCode 从上游错误体中提取业务码与文案。
//
// 不用 json.Unmarshal 解析整包：上游错误体形态不统一（有的包一层 envelope，有的
// 直接是错误对象，WAF 拦截时甚至是 HTML），正则对全部形态都能稳定命中。
func ExtractCode(body string) (code, msg string) {
	if body == "" {
		return "", ""
	}
	if m := codeRe.FindStringSubmatch(body); len(m) > 1 {
		code = m[1]
	} else if m := codeEqRe.FindStringSubmatch(body); len(m) > 1 {
		code = m[1]
	}
	if m := msgRe.FindStringSubmatch(body); len(m) > 1 {
		msg = truncate(unescapeJSON(m[1]), 200)
	} else if m := msgEqRe.FindStringSubmatch(body); len(m) > 1 {
		msg = truncate(strings.TrimSpace(m[1]), 200)
	}
	return code, msg
}

func unescapeJSON(s string) string {
	// 只处理最常见的两种转义；解析失败的极端情况直接返回原文，不影响判定。
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var out string
	if err := json.Unmarshal([]byte(`"`+s+`"`), &out); err != nil {
		return s
	}
	return out
}

// wafLike 判断 403 是否属于「非业务信封」——即 APISIX WAF 拦截页 / 空体。
//
// 判据：403 且提取不到业务码。业务层的 403 一定带 code 字段，WAF 页面不会。
// 这个区分很关键：前者是账号问题（软冷却），后者是通道级封锁（critical）。
func wafLike(att Attempt) bool {
	return att.Status == 403 && att.Code == ""
}

// Judge 把观测翻译成结论。dual 为 true 时表示需要换模型再验一次。
func Judge(att Attempt) (findings []Finding) {
	base := Finding{UID: att.UID, Nick: att.Nick, Model: att.Model, Evidence: att.BodyPrefix}

	// 1. 传输层失败：DNS / TLS / connect / 超时。归因在本地出网或线路，不在上游策略。
	if att.Network {
		base.Level, base.Code = "high", "network_error"
		base.Detail = fmt.Sprintf("%s 网络层失败（%s）", label(att), att.ErrText)
		return append(findings, base)
	}

	// 2. 通道级封锁：403 无业务信封（WAF）或 11-128。
	if wafLike(att) {
		base.Level, base.Code = "critical", "channel_refused"
		base.Detail = fmt.Sprintf("%s 被 WAF 拦截（403 无业务信封）", label(att))
		return append(findings, base)
	}
	if att.Code == "11-128" {
		meta := knownCodes["11-128"]
		base.Level, base.Code = meta.Severity, "channel_refused"
		base.Detail = fmt.Sprintf("%s 被通道校验拒绝（11-128）：%s", label(att), att.Msg)
		return append(findings, base)
	}

	// 3. 已知业务码分流。6004 静默——模型限流是常态，报它只会造成告警疲劳。
	if meta, ok := knownCodes[att.Code]; ok {
		if meta.Severity == "silent" {
			return nil
		}
		code := "known_code:" + att.Code
		switch att.Code {
		case "12153":
			code = "session_dead"
		case "11102", "11158":
			code = "model_rejected"
		case "6021":
			code = "upstream_busy"
		}
		base.Level, base.Code = meta.Severity, code
		base.Dual = meta.Dual
		base.Detail = fmt.Sprintf("%s 返回 %s（%s）", label(att), att.Code, meta.Meaning)
		if att.Msg != "" {
			base.Detail += "：" + att.Msg
		}
		return append(findings, base)
	}

	// 4. 未登记的业务码 —— 本次探测最重要的出口。
	if att.Code != "" {
		base.Level, base.Code = "high", "unknown_code:"+att.Code
		base.Detail = fmt.Sprintf("%s 返回未登记业务码 %s", label(att), att.Code)
		if att.Msg != "" {
			base.Detail += "：" + att.Msg
		}
		base.Evidence = att.BodyPrefix
		return append(findings, base)
	}

	// 5. HTTP 层异常但无法归因到业务码。
	if att.Status != 200 {
		base.Level, base.Code = "high", "http_error"
		base.Detail = fmt.Sprintf("%s 返回 HTTP %d，且无法提取业务码", label(att), att.Status)
		base.Evidence = att.BodyPrefix
		return append(findings, base)
	}

	// 6. 200 但结构不符 —— 上游改协议了。
	if len(att.Drift) > 0 {
		base.Level, base.Code = "high", "protocol_drift"
		base.Detail = fmt.Sprintf("%s 响应结构不符：%s", label(att), strings.Join(att.Drift, ", "))
		return append(findings, base)
	}

	return nil
}

// JudgeWithDual 在 Judge 之上处理「模型级拒绝 vs 通道级故障」的归因。
//
// 这是防误报的关键：上游下线一个模型、或调整某类账号的模型权限，都会让主模型
// 返回 11102/11158。若不交叉验证，这会天天被报成「通道被封」。
//
//	主失败 + 备成功 → 模型级变化（不是通道故障）
//	主备皆失败     → 通道级故障
//	主成功 + 备失败 → model_regression（备用模型退化了，主模型没事）
func JudgeWithDual(primary, fallback Attempt) []Finding {
	fs := Judge(primary)
	if len(fs) == 0 || !fs[0].Dual {
		return fs
	}
	if fallback.OK() {
		f := fs[0]
		f.Level = "medium"
		f.Code = "model_level_change"
		f.Detail = fmt.Sprintf("%s 主模型 %s 被拒，但备用模型 %s 正常——是模型级变化，不是通道故障",
			label(primary), primary.Model, fallback.Model)
		return []Finding{f}
	}
	// 备用模型也失败：确实不是单个模型的事。
	fs[0].Detail += fmt.Sprintf("；备用模型 %s 同样失败，判定为通道级故障", fallback.Model)
	fs[0].Level = "critical"
	fs[0].Code = "channel_failure"
	fs[0].Dual = false
	return fs
}

// JudgePromptLayer L3 专用判定。
//
// L3 比其他层多依赖一个变量（网关自己的提示词模板），归因不纯。因此它**永不**
// 产出 channel_failure / channel_refused —— 只产出 prompt_layer_diff，交由人判断
// 是「提示词被针对」还是「网关模板本身有问题」。
func JudgePromptLayer(l3 Attempt) []Finding {
	if l3.OK() {
		return nil
	}
	fs := Judge(l3)
	if len(fs) == 0 {
		return nil
	}
	return []Finding{{
		Level:    "medium",
		Code:     "prompt_layer_diff",
		UID:      l3.UID,
		Nick:     l3.Nick,
		Model:    l3.Model,
		Detail:   fmt.Sprintf("%s 带网关提示词的请求失败（%s）：%s", label(l3), fs[0].Code, fs[0].Detail),
		Evidence: l3.BodyPrefix,
	}}
}

func label(a Attempt) string {
	if a.Nick != "" {
		return a.Nick + "(" + shortUID(a.UID) + ")"
	}
	return shortUID(a.UID)
}

func shortUID(uid string) string {
	if len(uid) >= 8 {
		return uid[:8]
	}
	return uid
}
