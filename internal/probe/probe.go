// Package probe 上游协议主动探测：在通道真正失效之前发现征兆。
//
// 为什么是旁路而不是走网关主链路
//
//	经 /v1/chat/completions 发探测会同时踩三件事：Usage.Add 无差别记账污染用量；
//	applyErrorPolicy 在错误时被调用，一次 11-128 就把正常账号打进冷却；Pool.Pick
//	与在途租约被探测抢占。在 handler 加「探测标记头」跳过这些是错的方向——那等于
//	给客户端留一个「别惩罚我」的后门，会掩盖真实的上游故障。
//
//	本包直接复用 internal/upstream 的出站管线（同一套 ChatHeaders 指纹），但
//	完全不接触 pool / usage / reqlog / handler 四个对象，因此：
//	  - 探测失败不会触发任何冷却或熔断；
//	  - 探测流量不进 usage.json 与 requests-*.jsonl（spike 实测 sha256 不变）。
//
// 分层
//
//	L1 通道探活：最小请求（max_tokens=1），轮转单账号，判通道是否还能用；
//	L2 全量扫描：每个账号一次，判账号级是否被单独收紧；
//	L3 全保真：带网关自有 system 提示词走完整 prompt.Rewrite，判提示词层是否被针对。
//	   L3 比其他层多依赖一个变量（网关自己的提示词模板），归因不纯，故它的失败
//	   只产出 prompt_layer_diff，永不升级为通道故障。
package probe

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// Level 探测层标识。
type Level string

const (
	LevelL1 Level = "l1" // 通道探活（最小请求）
	LevelL2 Level = "l2" // 全量扫描（每号一次）
	LevelL3 Level = "l3" // 全保真（带网关提示词）
)

// Chatter 探测所需的最小上游能力。生产用 *upstream.Client，测试注入假实现。
// 之所以抽接口：判定逻辑（协议漂移 / 错误码分流 / 双模型交叉）必须能脱离网络
// 覆盖，否则每个分支都要真打上游，既烧额度又不可重复。
type Chatter interface {
	ChatStream(a *auth.Auth, body []byte, clientIP string, meta upstream.ChatMeta) (io.ReadCloser, int, []byte, error)
}

// Attempt 一次探测尝试的全部观测值。
type Attempt struct {
	Level      Level  `json:"level"`
	UID        string `json:"uid"`
	Nick       string `json:"nick"`
	Model      string `json:"model"`     // 响应回显的模型名（流内最后一帧覆盖）
	ReqModel   string `json:"req_model"` // 请求时用的模型名（裸名）
	Status     int    `json:"status"`
	Kind       string `json:"kind,omitempty"`
	ErrText    string `json:"err,omitempty"`
	BodyPrefix string `json:"body_prefix,omitempty"`

	// Code 上游业务码原文（"11-128" / "11102" / "12153" …）；空 = 未提取到。
	Code string `json:"code,omitempty"`
	// Msg 业务码伴随的文案（截断），未知码告警时要原样带出。
	Msg string `json:"msg,omitempty"`

	TTFBMs  int64          `json:"ttfb_ms"`
	TotalMs int64          `json:"total_ms"`
	Frames  int            `json:"frames"`
	SawDone bool           `json:"saw_done"`
	Finish  string         `json:"finish_reason,omitempty"`
	Usage   map[string]any `json:"usage,omitempty"`

	// Drift 协议漂移项（缺字段 / 形态不符）；空 = 结构符合预期。
	Drift []string `json:"drift,omitempty"`

	// Network 传输层失败（DNS / TLS / connect / 超时）：与「上游明确拒绝」不同，
	// 归因在本地出网而非上游策略。
	Network bool `json:"network,omitempty"`

	// Cold 本次是否为该进程内的第一次出站（冷连接：全新 TLS 握手）。
	//
	// 为什么必须记：实测同一账号同一模型，冷连接 TTFB 6.6s、复用连接 1.0s，
	// 相差 6 倍。L1 每轮都是新进程（恒冷），L2 一轮 6 个号（首冷后热）。若把
	// 两者混进同一个延迟基线桶，中位数会变成双峰，任何阈值都判不准。故基线
	// 按（来源 × 冷/热）分桶。
	Cold bool `json:"cold,omitempty"`
}

// OK 本次尝试是否拿到了可用的通道响应（200 + 结构无漂移）。
func (a Attempt) OK() bool {
	return a.Status == 200 && len(a.Drift) == 0 && !a.Network
}

// Finding 一条判定结论。Level 是严重度，Code 是结论类型（用于去重与自动化分流）。
type Finding struct {
	Level  string `json:"level"` // critical / high / medium / low
	Code   string `json:"code"`  // channel_refused / protocol_drift / unknown_code / …
	UID    string `json:"uid,omitempty"`
	Nick   string `json:"nick,omitempty"`
	Model  string `json:"model,omitempty"`
	Detail string `json:"detail"`
	// Evidence 判据原文（错误体前缀 / 漂移项列表），便于人工复核。
	Evidence string `json:"evidence,omitempty"`
	// Dual 建议对该账号再用备用模型交叉验证一次（模型级拒绝时置位）。
	Dual bool `json:"-"`
}

// Probe 一次探测的执行体。
type Probe struct {
	Chat Chatter
	// Primary 主探测模型（裸名，不带 realm 前缀）。
	Primary string
	// Fallback 交叉验证用备用模型。
	Fallback string
	// PromptText 非空时 L3 用它做 prompt.Rewrite；空 = L3 不可用。
	PromptText string
	// Timeout 单次尝试的硬上限（读流也受它约束）。
	Timeout time.Duration

	// sent 本 Probe 实例已发出的请求数，用于判定冷/热连接。
	// 只增不减：一个进程内的第一次出站必然要付 TLS 握手成本。
	sent int
}

// BuildBody 出站请求体。刻意压到最小：max_tokens=1，prompt 单字符。
// 实测成本 credit=0（prompt 31 token），比方案预估的 0.01 还低。
func BuildBody(model string) []byte {
	return []byte(fmt.Sprintf(
		`{"model":%q,"messages":[{"role":"user","content":"1"}],"max_tokens":1}`,
		model,
	))
}

// rewriteBody L3 用网关自有提示词替换 system（与网关 custom 模式同一条管线）。
// 走 prompt.Rewrite 而不是自己拼接：L3 要探的是「网关实际发出去的东西」，
// 自己拼一份就等于在探另一条不存在的路径。
func rewriteBody(body []byte, promptText string) []byte {
	return prompt.Rewrite(body, promptText)
}

// Run 对指定账号发一次探测。fullPrompt=true 时按 L3 语义叠加网关提示词。
//
// 返回的 Attempt 已包含协议漂移检测结果；判定结论由 Judge 产出，两者分开是
// 为了让「观测」与「结论」各自可测。
func (p *Probe) Run(level Level, a *auth.Auth, fullPrompt bool) Attempt {
	model := p.Primary
	body := BuildBody(model)
	if fullPrompt && p.PromptText != "" {
		body = rewriteBody(body, p.PromptText)
	}

	att := Attempt{
		Level: level, UID: a.UID, Nick: a.Nickname, Model: model, ReqModel: model,
		Cold: p.sent == 0,
	}
	p.sent++
	start := time.Now()

	rc, status, respBody, err := p.Chat.ChatStream(a, body, "", upstream.ChatMeta{})
	att.Status = status
	if err != nil {
		att.ErrText = err.Error()
		att.Network = status == 0 // 传输层失败时 status 恒 0，见 upstream.ChatStream 契约
		if ue, ok := err.(*upstream.Error); ok {
			att.Kind = ue.Kind.String()
		}
		if len(respBody) > 0 {
			att.BodyPrefix = truncate(string(respBody), 300)
			att.Code, att.Msg = ExtractCode(string(respBody))
		}
		att.TotalMs = time.Since(start).Milliseconds()
		return att
	}
	if rc == nil {
		att.ErrText = "nil body with nil error"
		att.Network = true
		att.TotalMs = time.Since(start).Milliseconds()
		return att
	}
	defer rc.Close()

	if status != 200 {
		// 无 err 但非 200 属防御分支（ChatStream 契约上不会出现）；仍按拒绝处理。
		if len(respBody) > 0 {
			att.BodyPrefix = truncate(string(respBody), 300)
			att.Code, att.Msg = ExtractCode(string(respBody))
		}
		att.TotalMs = time.Since(start).Milliseconds()
		return att
	}

	readStream(&att, rc, start)
	return att
}

// readStream 读完整 SSE 流并按实测形状校验结构。
//
// 关键教训：L1 请求 max_tokens=1 时 content 恒为空、由 reasoning_content 承载
// 那一个 token，finish_reason 为 length。因此 content 非空**不能**作为校验项，
// 否则每次探测都误报协议漂移。
func readStream(att *Attempt, rc io.ReadCloser, start time.Time) {
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)

	var (
		ttfb      time.Duration
		sawChoice bool
		sawDelta  bool
		sawUsage  bool
		sawModel  bool
		usageKeys = map[string]bool{}
		modelSeen string
		finish    string
	)

	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if ttfb == 0 {
			ttfb = time.Since(start)
		}
		if payload == "[DONE]" {
			att.SawDone = true
			continue
		}
		att.Frames++

		var frame struct {
			Model   string `json:"model"`
			Choices []struct {
				FinishReason string         `json:"finish_reason"`
				Delta        map[string]any `json:"delta"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			att.Drift = append(att.Drift, "frame_not_json")
			continue
		}
		if frame.Model != "" {
			sawModel = true
			if modelSeen == "" {
				modelSeen = frame.Model
			}
		}
		if len(frame.Choices) > 0 {
			sawChoice = true
			if frame.Choices[0].Delta != nil {
				sawDelta = true
			}
			if frame.Choices[0].FinishReason != "" {
				finish = frame.Choices[0].FinishReason
			}
		}
		if frame.Usage != nil {
			sawUsage = true
			for k := range frame.Usage {
				usageKeys[k] = true
			}
			att.Usage = frame.Usage
		}
	}
	if err := sc.Err(); err != nil {
		att.ErrText = "read stream: " + err.Error()
		att.Network = true
	}

	att.TTFBMs = ttfb.Milliseconds()
	att.TotalMs = time.Since(start).Milliseconds()
	att.Finish = finish
	if modelSeen != "" {
		att.Model = modelSeen
	}

	// —— 协议兼容性校验 ——
	// 上游改协议时通常不是「直接失败」，而是少个字段 / 换个键名。这里逐项检查，
	// 缺哪项就记哪项，避免一条笼统的「结构异常」让人无从排查。
	switch {
	case att.Frames == 0:
		att.Drift = append(att.Drift, "no_frames")
	case !att.SawDone:
		att.Drift = append(att.Drift, "no_done_frame")
	}
	if !sawChoice {
		att.Drift = append(att.Drift, "choices_missing")
	}
	if !sawDelta {
		att.Drift = append(att.Drift, "delta_missing")
	}
	if !sawModel {
		att.Drift = append(att.Drift, "model_missing")
	} else if !sameModel(att.Model, att.ReqModel) {
		// 上游把请求模型换了名（含 realm 前缀差异以外的情况）——可能是路由口径变化。
		att.Drift = append(att.Drift, "model_mismatch:"+att.Model)
	}
	if !sawUsage {
		att.Drift = append(att.Drift, "usage_missing")
	} else {
		if v, ok := numField(att.Usage, "total_tokens"); !ok || v <= 0 {
			att.Drift = append(att.Drift, "usage_total_tokens_invalid")
		}
		if _, ok := att.Usage["credit"]; !ok {
			att.Drift = append(att.Drift, "usage_credit_missing")
		}
	}
	if finish != "" && !validFinish(finish) {
		att.Drift = append(att.Drift, "unknown_finish_reason:"+finish)
	}
}

func validFinish(s string) bool {
	switch s {
	case "length", "stop", "tool_calls", "content_filter", "null":
		return true
	}
	return false
}

func sameModel(a, b string) bool {
	return stripRealm(a) == stripRealm(b)
}

func stripRealm(m string) string {
	if i := strings.IndexByte(m, ':'); i >= 0 {
		return m[i+1:]
	}
	return m
}

func numField(m map[string]any, key string) (float64, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	f, ok := v.(float64)
	return f, ok
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
