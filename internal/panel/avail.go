// avail.go 模型可用性探测：把「这个 global 模型当前账号能不能用」变成持久证据。
//
// 为什么需要它
//
//	面板「模型与档位」原本只用两类证据判断可用性：模型级冷却表（6h TTL）与最近
//	100 条请求。国际版账号通常只跑一两个模型，其余三十多个模型永远进不了这两个
//	窗口，于是长期显示「待验证」——信息量为零，用户看到的是一排灰字。
//
//	「待验证」本身是诚实的设计（上游列出的目录 ≠ 当前账号有权限），问题是缺少
//	一次主动取证的手段。本文件补这一环：主动发一次最小请求（max_tokens=1，实测
//	credit=0），把结果落盘保留 24h。
//
// 成本
//
//	每个模型一次最小请求。实测：global 免费模型 credit=0；被拒的模型（11158/
//	11102）不产生任何计费。35 个模型一轮下来积分消耗为 0。
//
// 边界
//
//	只探测 global 前缀的模型：cn 侧模型由国内号服务，走的是另一条额度，且 CN
//	目录里没有「个人免费账号不支持」这类大面积权限差异，实证价值低。
package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// availTTL 探测结果的保留时长。
//
//	取 24h：比模型锁的 6h 长，覆盖「一天看一次面板」的节奏；又短于上游模型目录
//	的变更周期，不至于把一周前的结论当成现状。
const availTTL = 24 * time.Hour

// AvailEntry 单个模型的一次探测结论。
type AvailEntry struct {
	Model     string    `json:"model"`             // 带前缀的完整 id（global:xxx）
	Status    int       `json:"status"`            // HTTP 状态码；0 = 传输层失败
	Code      string    `json:"code,omitempty"`    // 上游业务码（11158 / 11102 / …）
	Message   string    `json:"message,omitempty"` // 文案节选，供人工判断
	CheckedAt time.Time `json:"checked_at"`
	Level     string    `json:"level"` // ok / denied / missing / limited / error
}

// availFile 落盘结构。Version 便于将来加字段时兼容。
type availFile struct {
	Version int          `json:"version"`
	Saved   string       `json:"saved,omitempty"`
	Entries []AvailEntry `json:"entries"`
}

const availFileVersion = 1

// availProbeMu 串行化探测：并发点两次「探测」不应打两轮上游。
var availProbeMu sync.Mutex

// availPath 探测结果文件路径。
//
// 复用 ProbeFile 所在目录（同为 data/），不新增配置项：这两个文件都是「模型侧
// 的本地实测留痕」，放一起符合语义，也省掉一个需要用户理解的路径配置。
func (p *Panel) availPath() string {
	if p.cfg.ProbeFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(p.cfg.ProbeFile), "model_avail.json")
}

// loadAvail 读取未过期的探测结果。任何读取/解析失败都返回空 map（不报错）：
// 这是只读增强，坏文件不该让面板打不开。
func (p *Panel) loadAvail(now time.Time) map[string]AvailEntry {
	out := map[string]AvailEntry{}
	path := p.availPath()
	if path == "" {
		return out
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var f availFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return out
	}
	for _, e := range f.Entries {
		if e.Model == "" || now.Sub(e.CheckedAt) > availTTL {
			continue
		}
		out[e.Model] = e
	}
	return out
}

// saveAvail 原子落盘。写失败只记日志（探测结果丢了不要紧，下次再探）。
func (p *Panel) saveAvail(entries []AvailEntry) error {
	path := p.availPath()
	if path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(availFile{
		Version: availFileVersion,
		Saved:   time.Now().Format(time.RFC3339),
		Entries: entries,
	}, "", " ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// modelsAvail GET /panel/api/models/avail —— 返回未过期的探测结果。
func (p *Panel) modelsAvail(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	entries := p.loadAvail(now)
	list := make([]AvailEntry, 0, len(entries))
	for _, e := range entries {
		list = append(list, e)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Model < list[j].Model })
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": list,
		"ttl_sec": int(availTTL.Seconds()),
	})
}

// modelsProbe POST /panel/api/models/probe —— 主动探测全部 global 模型。
//
// 同步执行：35 个模型约 15–25 秒。前端带「探测中」提示即可；改成异步 + 轮询
// 会让面板多一套状态机，而这是低频手动操作，不值得。
func (p *Panel) modelsProbe(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Upstream == nil || p.cfg.Pool == nil {
		writeErr(w, http.StatusNotImplemented, "upstream not available")
		return
	}
	if !availProbeMu.TryLock() {
		writeErr(w, http.StatusConflict, "探测正在进行中，请稍候")
		return
	}
	defer availProbeMu.Unlock()

	// 选一个可用的 global 账号：可用性判定本来就和账号类型强相关，
	// 用池里第一个健康号即可代表「当前这个号能不能用」。
	var acct *auth.Auth
	for _, uid := range p.cfg.Pool.AvailableUIDsForRealm("global") {
		if a := p.cfg.Pool.AuthByUID(uid); a != nil {
			acct = a
			break
		}
	}
	if acct == nil {
		writeErr(w, http.StatusServiceUnavailable, "池中没有可用的国际版账号，无法探测")
		return
	}

	// 只探 global 前缀的模型（见文件头注释的成本与边界说明）。
	models := make([]string, 0, 40)
	for _, mi := range p.cfg.Upstream.FetchGlobalModels(acct) {
		if mi != "" {
			models = append(models, "global:"+mi)
		}
	}
	if len(models) == 0 {
		writeErr(w, http.StatusBadGateway, "上游未返回国际版模型目录，无法探测")
		return
	}

	entries := make([]AvailEntry, 0, len(models))
	for _, m := range models {
		entries = append(entries, p.probeOne(acct, m))
	}
	if err := p.saveAvail(entries); err != nil {
		// 落盘失败不影响本次结果返回，只是下次要重探。
		log.Printf("panel: 模型可用性探测结果落盘失败: %v", err)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"checked": len(entries),
		"entries": entries,
	})
}

// probeOne 对单个模型发一次最小请求并归类。
func (p *Panel) probeOne(acct *auth.Auth, model string) AvailEntry {
	e := AvailEntry{Model: model, CheckedAt: time.Now()}

	body := []byte(`{"model":` + quoteJSON(model) +
		`,"messages":[{"role":"user","content":"1"}],"max_tokens":1}`)

	rc, status, respBody, err := p.cfg.Upstream.ChatStream(acct, body, "", upstream.ChatMeta{})
	e.Status = status
	if rc != nil {
		rc.Close()
	}
	if err != nil {
		if status == 0 {
			e.Level = "error"
			e.Message = truncateStr(err.Error(), 120)
			return e
		}
		// 业务拒绝：从错误体里取业务码，供面板区分「无权限」与「无此模型」。
		e.Code = extractBizCode(string(respBody))
		e.Message = truncateStr(err.Error(), 120)
		switch {
		case strings.HasPrefix(e.Code, "11158"):
			e.Level = "denied"
		case strings.HasPrefix(e.Code, "11102"):
			e.Level = "missing"
		case e.Code == "6004" || status == 429:
			e.Level = "limited"
		default:
			e.Level = "error"
		}
		return e
	}
	e.Level = "ok"
	return e
}

// quoteJSON 把模型名安全地塞进 JSON 字符串（模型名只含 ASCII，但别依赖这一点）。
func quoteJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// extractBizCode 从上游错误体里取业务码。
//
// 只做「找到 code 字段」这一件事：面板只需区分 11158 / 11102 / 6004 三类，
// 不值得引入完整解析器；解析失败返回空串，调用方按 error 处理。
func extractBizCode(body string) string {
	if body == "" {
		return ""
	}
	var f struct {
		Code any `json:"code"`
	}
	if err := json.Unmarshal([]byte(body), &f); err == nil {
		switch v := f.Code.(type) {
		case string:
			return v
		case float64:
			return strings.TrimSuffix(strings.TrimRight(
				strings.TrimRight(jsonNumber(v), "0"), "."), ".")
		}
	}
	// 兜底：错误体可能不是纯 JSON（包了 envelope），扫一遍 "code"
	if i := strings.Index(body, `"code"`); i >= 0 {
		rest := body[i:]
		if j := strings.Index(rest, ":"); j >= 0 {
			rest = strings.TrimSpace(rest[j+1:])
			rest = strings.Trim(rest, `"`)
			if k := strings.IndexAny(rest, ",}\n "); k > 0 {
				rest = rest[:k]
			}
			return rest
		}
	}
	return ""
}

func jsonNumber(v float64) string {
	// 业务码都是整数；避免引入 strconv 只是为了这一步。
	n := int64(v)
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// clearModelLocks POST /panel/api/models/clear_locks —— 清除确定性模型锁。
//
// 参数 realm 可选（cn / global），空 = 两个域都清。
//
// 只清 11102（上游无此模型）与 11158（账号类型无权）：这两类是确定性拒绝，
// 到期重试必然再次被拒并重新上锁，等待毫无意义。6004 限流锁保持不动——它是
// 真的会恢复，清掉只会让下一个请求立刻再撞限流。
func (p *Panel) clearModelLocks(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Pool == nil {
		writeErr(w, http.StatusNotImplemented, "pool not available")
		return
	}
	var body struct {
		Realm string `json:"realm"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	realm := strings.TrimSpace(body.Realm)
	if realm != "" && realm != "cn" && realm != "global" {
		writeErr(w, http.StatusBadRequest, "realm 只能是 cn / global，或留空表示全部")
		return
	}
	n := p.cfg.Pool.ClearDeterministicLocks(realm)
	log.Printf("panel: 清除确定性模型锁 realm=%q 共 %d 条", realm, n)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cleared": n})
}
