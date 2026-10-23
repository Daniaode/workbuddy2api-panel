// cmd/probe 上游协议主动探测的宿主侧执行器。
//
// 为什么是独立二进制而不是进程内 scheduler：进程内采集需要重建镜像并重启容器，
// 而这个容器正是本机 workbuddy 模型的提供方——重启会打断正在进行的推理。独立
// 二进制复用同一份 internal/upstream 出站管线（同一套指纹），零重启即可上线。
// 代价是 OpenClaw 停机期间不探测；这一点在 README 里写明，等下次低峰部署再把
// 采集挪进进程内。
//
// 用法：
//
//	probe -mode l1            # 通道探活（每 15 分钟）
//	probe -mode l2            # 全量扫描（每 6 小时）
//	probe -mode l3            # 全保真，带网关提示词（每天）
//	probe -mode l1 -dry-run   # 只选号不请求，验证选择规则
//
// 输出：无异常时打印 NO_REPLY（供自动化静默），有结论时打印 markdown。
//
// 退出码（探测自身状态，与「上游是否可用」严格分离）：
//
//	0  探测完成（含"发现异常"——上游异常是正常结果，不是执行失败）
//	2  探测自身故障：配置/凭证/状态文件读不到，或参数非法
//	3  覆盖为零：所有账号都被保护规则跳过（静默失败比误报更危险，必须单独告警）
//	4  全部尝试网络层失败（DNS/TLS/connect/超时），归因不清：可能上游封禁，也可能本机出网
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/probe"
	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const (
	exitOK           = 0
	exitSelfFault    = 2
	exitZeroCoverage = 3
	exitAllNetwork   = 4
)

// 探测模型。主模型选实测最便宜的（0.259 积分/次），备用用于交叉验证。
const (
	defaultPrimary  = "deepseek-v4.1-flash"
	defaultFallback = "glm-5.3-flash"
)

// dedupWindow 同一 (账号 × 结论) 的静默窗口。
const dedupWindow = 6 * time.Hour

// alertState 跨轮次的状态：轮转游标 + 告警去重记录。
type alertState struct {
	Cursor int `json:"cursor"`
	// Notified key = "<uid>|<code>" → 上次告警时间。
	Notified map[string]time.Time `json:"notified"`
	// 上一次的 critical 集合，用于「状态翻转必须立刻报」——critical 是通道级
	// 封锁，静默 6 小时的代价太大。
	LastCritical []string  `json:"last_critical,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func main() {
	var (
		mode      = flag.String("mode", "l1", "探测模式：l1 通道探活 / l2 全量 / l3 全保真")
		cfgPath   = flag.String("config", "config.json", "网关配置")
		authDir   = flag.String("auths", "auths", "凭证目录")
		statePath = flag.String("state", "data/state.json", "池状态快照（只读）")
		dataDir   = flag.String("data", "data", "探测自身数据目录")
		primary   = flag.String("model", defaultPrimary, "主探测模型（裸名）")
		fallback  = flag.String("fallback", defaultFallback, "交叉验证用备用模型")
		dryRun    = flag.Bool("dry-run", false, "只做选号，不发请求")
		chatBase  = flag.String("chat-base", "", "覆盖上游 chat 地址（仅诊断：区分上游拒绝与本机出网故障）")
	)
	flag.Parse()

	level := probe.Level(*mode)
	switch level {
	case probe.LevelL1, probe.LevelL2, probe.LevelL3, probe.LevelL4:
	default:
		fmt.Fprintf(os.Stderr, "probe: 未知 mode %q（l1 / l2 / l3 / l4）\n", *mode)
		os.Exit(exitSelfFault)
	}

	// —— 配置与凭证：读不到就是探测自身故障，不是上游故障 ——
	cfg, floor, promptText, err := loadConfig(*cfgPath, level)
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe: 读取配置失败: %v\n", err)
		os.Exit(exitSelfFault)
	}
	_ = cfg

	auths, err := auth.LoadDir(*authDir)
	if err != nil || len(auths) == 0 {
		fmt.Fprintf(os.Stderr, "probe: 读取凭证失败: %v（目录 %s）\n", err, *authDir)
		os.Exit(exitSelfFault)
	}

	states := probe.LoadAccountStates(*statePath)

	histPath := filepath.Join(*dataDir, "probe-history.jsonl")
	stateFile := filepath.Join(*dataDir, "probe-state.json")
	hist := probe.NewHistory(histPath)

	st := loadAlertState(stateFile)

	cands := probe.BuildCandidates(auths, states, probe.SelectOptions{
		CreditFloor: floor,
		TokenMargin: 2 * time.Hour,
	})

	// 目标集合：L1/L3/L4 轮转一个号；L2 全部可探号。
	// L4 也轮转而不是全量：辅助链路每号 3 次调用，全量会让每轮打到 18 次，
	// 而它要发现的是「域级」变化，单号足以代表。
	var targets []probe.Candidate
	if level == probe.LevelL2 {
		for _, c := range cands {
			if c.Skip == "" {
				targets = append(targets, c)
			}
		}
	} else {
		if c, ok := probe.Pick(cands, st.Cursor); ok {
			targets = append(targets, c)
			st.Cursor++
		}
	}

	if len(targets) == 0 {
		// 覆盖为零是探测自身的问题：静默失败比误报更危险。
		fmt.Printf("**探测覆盖为零**：本轮没有可探测账号（%s）。\n\n", *mode)
		fmt.Println("跳过原因：" + summarizeSkips(cands))
		fmt.Println()
		fmt.Println("这通常意味着所有号都在冷却 / 余额触底 / token 即将到期。请检查池状态——")
		fmt.Println("注意：这条告警说明**探测本身没能工作**，不代表上游不可用。")
		os.Exit(exitZeroCoverage)
	}

	if *dryRun {
		for _, t := range targets {
			fmt.Printf("would probe: %s(%s) model=%s level=%s\n",
				t.Nick, shortUID(t.UID), *primary, level)
		}
		return
	}

	chat := upstream.New()
	if *chatBase != "" {
		// 诊断开关：指到一个不可达/可观测的地址，用来回答「是上游封了还是本机出网断了」。
		// 正常路径不设置它，因此不改变生产判定口径。
		chat.ChatBaseCN = *chatBase
	}
	p := &probe.Probe{
		Chat:       chat,
		Aux:        chat, // *upstream.Client 同时实现 Chatter 与 Aux
		Primary:    *primary,
		Fallback:   *fallback,
		PromptText: promptText,
		Timeout:    25 * time.Second,
	}

	entries := hist.Load(time.Now())
	var findings []probe.Finding
	var attempts []probe.Attempt
	var auxAttempts []probe.AuxAttempt

	// L4 走独立路径：辅助链路没有 SSE 流，也没有模型维度，因此不走 Run/Judge。
	// 它同样不参与延迟基线——余额查询的耗时与 chat 的 TTFB 不是同一个量纲。
	if level == probe.LevelL4 {
		for _, t := range targets {
			got := p.RunAux(level, t.Auth)
			auxAttempts = append(auxAttempts, got...)
			for _, a := range got {
				_ = hist.Append(auxToHistory(a))
			}
			findings = append(findings, probe.JudgeAux(got)...)
		}

		allNetwork := len(auxAttempts) > 0
		for _, a := range auxAttempts {
			if !a.Network {
				allNetwork = false
				break
			}
		}
		report := renderAux(level, auxAttempts, findings, &st)
		if err := saveAlertState(stateFile, st); err != nil {
			fmt.Fprintf(os.Stderr, "probe: 保存状态失败: %v\n", err)
		}
		if strings.TrimSpace(report) == "" {
			fmt.Println("NO_REPLY")
		} else {
			fmt.Println(report)
		}
		if allNetwork {
			os.Exit(exitAllNetwork)
		}
		os.Exit(exitOK)
	}

	for _, t := range targets {
		att := p.Run(level, t.Auth, level == probe.LevelL3)
		attempts = append(attempts, att)
		_ = hist.Append(toHistory(att))

		switch level {
		case probe.LevelL3:
			findings = append(findings, probe.JudgePromptLayer(att)...)
		default:
			fs := probe.Judge(att)
			// 模型级拒绝时用备用模型交叉验证，避免把「模型下线」报成「通道被封锁」。
			if len(fs) > 0 && fs[0].Dual {
				fb := p.Run(level, t.Auth, false)
				_ = hist.Append(toHistory(fb))
				attempts = append(attempts, fb)
				fs = probe.JudgeWithDual(att, fb)
			}
			findings = append(findings, fs...)
		}

		// 延迟基线：按来源分桶（旁路冷连接 ≠ 网关热连接），热身期不判定。
		findings = append(findings, probe.JudgeLatency(att, "bypass", entries, probe.BaselineOptions{})...)
	}

	// 全部网络失败 → 归因不清，单独退出码，交给人判断是上游封禁还是本机出网。
	allNetwork := len(attempts) > 0
	for _, a := range attempts {
		if !a.Network {
			allNetwork = false
			break
		}
	}

	report := render(level, attempts, findings, &st)
	if err := saveAlertState(stateFile, st); err != nil {
		fmt.Fprintf(os.Stderr, "probe: 保存状态失败: %v\n", err)
	}

	if strings.TrimSpace(report) == "" {
		fmt.Println("NO_REPLY")
	} else {
		fmt.Println(report)
	}

	if allNetwork {
		os.Exit(exitAllNetwork)
	}
	os.Exit(exitOK)
}

// loadConfig 取积分保底与（L3 用的）网关提示词。
//
// 只读 config.json 而不调面板 API：探测要能在网关进程没起、或网关自己就是故障源
// 的时候继续工作。面板挂了不该连带让探测也瞎掉。
func loadConfig(path string, level probe.Level) (raw []byte, floor int64, promptText string, err error) {
	raw, err = os.ReadFile(path)
	if err != nil {
		return nil, 0, "", err
	}
	var c struct {
		Pool struct {
			CreditFloor int64 `json:"credit_floor"`
		} `json:"pool"`
		Prompt struct {
			Mode string `json:"mode"`
			File string `json:"file"`
		} `json:"prompt"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, 0, "", err
	}
	if level != probe.LevelL3 {
		return raw, c.Pool.CreditFloor, "", nil
	}
	// L3 必须用网关实际的提示词：探的是「网关发出去的东西」，自己拼一份
	// 等于在探另一条不存在的路径。custom 模式才做替换；其它模式 L3 无意义。
	if strings.EqualFold(c.Prompt.Mode, "custom") || c.Prompt.Mode == "" {
		text, err := prompt.Load(c.Prompt.Mode, c.Prompt.File)
		if err != nil {
			return nil, 0, "", err
		}
		promptText = text
	}
	return raw, c.Pool.CreditFloor, promptText, nil
}

// render 产出人读报告；无新结论时返回空串（调用方转成 NO_REPLY）。
//
// 去重规则：
//   - 同一 (账号 × 结论 code) 6 小时内只报一次——避免每 15 分钟刷同一条；
//   - critical 状态翻转（新增或消失）不受去重限制：通道级封锁静默 6 小时代价太大。
func render(level probe.Level, attempts []probe.Attempt, findings []probe.Finding, st *alertState) string {
	now := time.Now()

	// critical 集合比对（翻转必报）。
	var critNow []string
	for _, f := range findings {
		if f.Level == "critical" {
			critNow = append(critNow, f.UID+"|"+f.Code)
		}
	}
	sort.Strings(critNow)
	flipped := diffStrings(st.LastCritical, critNow)

	if st.Notified == nil {
		st.Notified = map[string]time.Time{}
	}
	var fresh []probe.Finding
	for _, f := range findings {
		key := f.UID + "|" + f.Code
		if last, ok := st.Notified[key]; ok && now.Sub(last) < dedupWindow && !contains(flipped, key) {
			continue
		}
		fresh = append(fresh, f)
		st.Notified[key] = now
	}

	// 清理过期去重记录，避免文件无限增长。
	for k, ts := range st.Notified {
		if now.Sub(ts) > 7*24*time.Hour {
			delete(st.Notified, k)
		}
	}
	st.LastCritical = critNow
	st.UpdatedAt = now

	if len(fresh) == 0 {
		// 全部结论都被去重：静默。但「已经恢复」也值得一提——critical 从有到无
		// 属于状态翻转，diffStrings 已把它算进 flipped。
		if len(flipped) > 0 {
			var lines []string
			lines = append(lines, fmt.Sprintf("**探测状态变化**（%s）", level))
			lines = append(lines, "")
			for _, k := range flipped {
				lines = append(lines, "- 解除："+k)
			}
			return strings.Join(lines, "\n")
		}
		return ""
	}

	sort.SliceStable(fresh, func(i, j int) bool {
		return sevRank(fresh[i].Level) < sevRank(fresh[j].Level)
	})

	var b strings.Builder
	fmt.Fprintf(&b, "**WorkBuddy 上游探测**（%s）\n\n", level)
	for _, f := range fresh {
		fmt.Fprintf(&b, "- [%s] %s\n", f.Level, f.Detail)
		if f.Evidence != "" {
			fmt.Fprintf(&b, "  ```\n  %s\n  ```\n", strings.ReplaceAll(truncate(f.Evidence, 200), "\n", " "))
		}
	}
	if len(flipped) > 0 {
		b.WriteString("\n状态翻转：")
		b.WriteString(strings.Join(flipped, ", "))
		b.WriteString("\n")
	}

	// 附一行观测摘要，便于对照「报的时候到底是什么状态」。
	b.WriteString("\n观测：")
	for i, a := range attempts {
		if i > 0 {
			b.WriteString(" · ")
		}
		fmt.Fprintf(&b, "%s=%d/%dms", a.Nick, a.Status, a.TTFBMs)
	}
	b.WriteString("\n")
	return b.String()
}

// auxToHistory 辅助链路记录。TTFB 与 TotalMs 同值——这些端点没有流式语义，
// 首字节即响应体，拆两个时间点没有观测意义。
func auxToHistory(a probe.AuxAttempt) probe.HistoryEntry {
	return probe.HistoryEntry{
		Time: time.Now(), Level: a.Level, Source: "bypass",
		UID: a.UID, Nick: a.Nick, Model: a.Endpoint,
		Status: a.Status, Code: a.Code, TTFBMs: a.TotalMs, TotalMs: a.TotalMs,
		OK: a.OK(), Drift: a.Drift,
	}
}

// renderAux 辅助链路的报告。去重与翻转规则与 render 完全一致（同一份
// alertState、同一套 6 小时窗口），只是观测摘要换成端点维度。
func renderAux(level probe.Level, attempts []probe.AuxAttempt, findings []probe.Finding, st *alertState) string {
	now := time.Now()

	var critNow []string
	for _, f := range findings {
		if f.Level == "critical" {
			critNow = append(critNow, f.UID+"|"+f.Code)
		}
	}
	sort.Strings(critNow)
	flipped := diffStrings(st.LastCritical, critNow)

	if st.Notified == nil {
		st.Notified = map[string]time.Time{}
	}
	var fresh []probe.Finding
	for _, f := range findings {
		key := f.UID + "|" + f.Code
		if last, ok := st.Notified[key]; ok && now.Sub(last) < dedupWindow && !contains(flipped, key) {
			continue
		}
		fresh = append(fresh, f)
		st.Notified[key] = now
	}
	for k, ts := range st.Notified {
		if now.Sub(ts) > 7*24*time.Hour {
			delete(st.Notified, k)
		}
	}
	st.LastCritical = critNow
	st.UpdatedAt = now

	if len(fresh) == 0 {
		if len(flipped) > 0 {
			lines := []string{fmt.Sprintf("**探测状态变化**（%s）", level), ""}
			for _, k := range flipped {
				lines = append(lines, "- 解除："+k)
			}
			return strings.Join(lines, "\n")
		}
		return ""
	}

	sort.SliceStable(fresh, func(i, j int) bool {
		return sevRank(fresh[i].Level) < sevRank(fresh[j].Level)
	})

	var b strings.Builder
	fmt.Fprintf(&b, "**WorkBuddy 辅助链路探测**（%s）\n\n", level)
	for _, f := range fresh {
		fmt.Fprintf(&b, "- [%s] %s\n", f.Level, f.Detail)
		if f.Evidence != "" {
			fmt.Fprintf(&b, "  ```\n  %s\n  ```\n", strings.ReplaceAll(truncate(f.Evidence, 200), "\n", " "))
		}
	}
	if len(flipped) > 0 {
		b.WriteString("\n状态翻转：")
		b.WriteString(strings.Join(flipped, ", "))
		b.WriteString("\n")
	}

	// 观测摘要按端点列出：漏掉哪个端点比「哪个号失败」更常是根因。
	b.WriteString("\n观测：")
	for i, a := range attempts {
		if i > 0 {
			b.WriteString(" · ")
		}
		fmt.Fprintf(&b, "%s=%d/%dms", a.Endpoint, a.Status, a.TotalMs)
	}
	b.WriteString("\n")
	return b.String()
}

func toHistory(a probe.Attempt) probe.HistoryEntry {
	return probe.HistoryEntry{
		Time: time.Now(), Level: a.Level, Source: "bypass",
		UID: a.UID, Nick: a.Nick, Model: a.Model,
		Status: a.Status, Code: a.Code, TTFBMs: a.TTFBMs, TotalMs: a.TotalMs,
		OK: a.OK(), Cold: a.Cold, Drift: a.Drift,
	}
}

func loadAlertState(path string) alertState {
	var st alertState
	raw, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	_ = json.Unmarshal(raw, &st)
	return st
}

func saveAlertState(path string, st alertState) error {
	raw, err := json.MarshalIndent(st, "", " ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func summarizeSkips(cands []probe.Candidate) string {
	sum := probe.SkipSummary(cands)
	if len(sum) == 0 {
		return "（无候选）"
	}
	var parts []string
	for r, n := range sum {
		parts = append(parts, fmt.Sprintf("%s×%d", r, n))
	}
	sort.Strings(parts)
	return strings.Join(parts, "，")
}

func sevRank(s string) int {
	switch s {
	case "critical":
		return 0
	case "high":
		return 1
	case "medium":
		return 2
	default:
		return 3
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func diffStrings(old, cur []string) []string {
	var out []string
	for _, v := range cur {
		if !contains(old, v) {
			out = append(out, v)
		}
	}
	for _, v := range old {
		if !contains(cur, v) {
			out = append(out, v)
		}
	}
	return out
}

func shortUID(uid string) string {
	if len(uid) >= 8 {
		return uid[:8]
	}
	return uid
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
