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
	"context"
	"encoding/json"
	"errors"
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

// availTTLManual 手动模式下的探测结果保留时长。
//
//	自动探测关闭时（panel.avail_probe_interval = 0）只有人点按钮才更新证据，
//	此时取 24h：比模型锁的 6h 长，覆盖「一天看一次面板」的节奏；又短于上游模型
//	目录的变更周期，不至于把一周前的结论当成现状。
const availTTLManual = 24 * time.Hour

// availTTL 探测结果的实际保留时长。
//
//	自动探测开启时不能再用固定 24h：间隔 30m 意味着每半小时就有新证据，若仍按
//	24h 保留，一次探测静默失败后面板会把一天前的结论当成现状——这比显示「待验证」
//	更糟，因为「待验证」至少是诚实的。
//
//	取 interval 的 3 倍（容忍连丢两轮），钳在 [15m, 24h]：间隔很短时不至于让
//	证据在两轮之间凭空过期；间隔很长时不超过手动模式的 24h 上限。
func (p *Panel) availTTL() time.Duration {
	iv := time.Duration(p.availInterval.Load())
	if iv <= 0 {
		return availTTLManual
	}
	ttl := iv * 3
	if ttl < 15*time.Minute {
		ttl = 15 * time.Minute
	}
	if ttl > availTTLManual {
		ttl = availTTLManual
	}
	return ttl
}

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
		if e.Model == "" || now.Sub(e.CheckedAt) > p.availTTL() {
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
		"ttl_sec": int(p.availTTL().Seconds()),
	})
}

// runAvailProbe 执行一轮探测并落盘，供手动端点与后台定时器共用。
//
// manual=true 时全量探测（人点了按钮，就要一份完整现状）。
// manual=false 时**增量**探测：只探「没有记录」或「记录已过期」的模型。
//
// 为什么自动探测必须增量，而不是每轮全量：
//
//	全量一轮 = 35 个 chat 请求。若每 30 分钟跑一次，一天就是 1680 个请求，
//	而池里通常只有一两个 global 账号——单账号每天 800+ 个 max_tokens=1 的
//	请求，这个密度本身就是异常信号。省积分不等于不冒险：不扣费不代表不触发风控。
//
//	增量之后，常规轮次的请求数是 0（所有模型都有新鲜记录），只有上游目录里
//	**新出现**的模型才会被探测。这正是「待验证」消失后应有的行为。
//
// 返回 (entries, err)。err 只在「这轮没能完成探测」时返回：
// 没有可用 global 号、上游模型目录为空。此时调用方应保持**原有**探测结果
// 不动——用一次失败去清空已有的有效证据，比不探测更糟。
func (p *Panel) runAvailProbe(manual bool) ([]AvailEntry, error) {
	if p.cfg.Upstream == nil || p.cfg.Pool == nil {
		return nil, errNoUpstream
	}
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
		return nil, errNoGlobalAccount
	}

	// 只探 global 前缀的模型（见文件头注释的成本与边界说明）。
	models := make([]string, 0, 40)
	for _, mi := range p.cfg.Upstream.FetchGlobalModels(acct) {
		if mi != "" {
			models = append(models, "global:"+mi)
		}
	}
	if len(models) == 0 {
		return nil, errNoModelCatalog
	}

	// 增量模式：跳过已有新鲜记录的模型。
	fresh := map[string]AvailEntry{}
	if !manual {
		fresh = p.loadAvail(time.Now())
	}

	// 保留其它模型的旧结论，只覆盖本轮探测的条目——否则增量会把没探的
	// 全部丢掉，面板反而变回「待验证」。
	merged := make(map[string]AvailEntry, len(models))
	for k, v := range fresh {
		merged[k] = v
	}

	probed := 0
	for _, m := range models {
		if !manual {
			if _, ok := fresh[m]; ok {
				continue // 记录仍新鲜，跳过
			}
		}
		e := p.probeOne(acct, m)
		merged[m] = e
		probed++
	}

	entries := make([]AvailEntry, 0, len(merged))
	for _, m := range models { // 只保留当前目录里仍存在的模型
		if e, ok := merged[m]; ok {
			entries = append(entries, e)
		}
	}
	if probed > 0 {
		if err := p.saveAvail(entries); err != nil {
			// 落盘失败不影响本次结果返回，只是下次要重探。
			log.Printf("panel: 模型可用性探测结果落盘失败: %v", err)
		}
	}
	return entries, nil
}

var (
	errNoUpstream      = errors.New("upstream not available")
	errNoGlobalAccount = errors.New("池中没有可用的国际版账号，无法探测")
	errNoModelCatalog  = errors.New("上游未返回国际版模型目录，无法探测")
)

// modelsProbe POST /panel/api/models/probe —— 手动探测全部 global 模型。
//
// 同步执行：35 个模型约 6–25 秒。前端带「探测中」提示即可；改成异步 + 轮询
// 会让面板多一套状态机，而这是低频操作，不值得。
func (p *Panel) modelsProbe(w http.ResponseWriter, r *http.Request) {
	if !availProbeMu.TryLock() {
		writeErr(w, http.StatusConflict, "探测正在进行中，请稍候")
		return
	}
	defer availProbeMu.Unlock()

	entries, err := p.runAvailProbe(true) // 手动：全量
	if err != nil {
		switch {
		case errors.Is(err, errNoUpstream):
			writeErr(w, http.StatusNotImplemented, err.Error())
		case errors.Is(err, errNoGlobalAccount):
			writeErr(w, http.StatusServiceUnavailable, err.Error())
		default:
			writeErr(w, http.StatusBadGateway, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"checked": len(entries),
		"entries": entries,
	})
}

// StartAvailProbe 后台周期性探测模型可用性（独立 ticker goroutine，ctx 取消即停）。
//
// 为什么需要它：探测端点此前**只能手动点**，而面板的「待验证」是静默的——
// 没人点按钮，三十多个国际版模型就永远是灰字。用户看到的表象是「探测间隔太长」，
// 实际是「根本没有自动探测」。
//
// 首轮立即执行（不等一个 interval）：进程重启后模型目录可能已变，而重启本身
// 往往发生在部署/排障之后，此时最需要一份新鲜结论。
func (p *Panel) StartAvailProbe(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	p.availInterval.Store(int64(interval))
	go func() {
		// 启动后先等一小段：与容器启动期的凭据加载 / 模型目录拉取错开，
		// 否则首轮可能因为 auths 还没就绪而空跑。
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
		// 首轮失败要重试，不能直接滑到下一个整间隔。
		//
		// 实测：容器重启后 30s 时池子往往还没把 global 号标为可用，
		// 首轮必然报「池中没有可用的国际版账号」——按原实现这一轮就废了，
		// 面板要再等 30 分钟才可能有结论。而重启恰恰发生在部署/排障之后，
		// 正是最需要新鲜结论的时刻。
		for attempt := 0; attempt < 5; attempt++ {
			if attempt > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Duration(attempt) * time.Minute):
				}
			}
			yielded, err := p.probeOnceLogged("startup")
			if yielded || err == nil {
				break
			}
		}
		var logged time.Duration
		for {
			cur := time.Duration(p.availInterval.Load())
			if cur != logged {
				log.Printf("panel: 模型可用性自动探测每 %s", cur)
				logged = cur
			}
			timer := time.NewTimer(cur)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				p.probeOnceLogged("schedule")
			}
		}
	}()
}

// probeOnceLogged 跑一轮探测并按结果记日志。
//
// 返回 (本轮是否让位给手动探测, 探测错误)。调用方据 err 决定是否值得重试：
// 启动阶段池子还没热起来导致的失败是暂态的，等到下一个整间隔就白等半小时。
func (p *Panel) probeOnceLogged(trigger string) (bool, error) {
	if !availProbeMu.TryLock() {
		return true, nil // 手动探测正在进行，本轮让位
	}
	defer availProbeMu.Unlock()
	entries, err := p.runAvailProbe(false) // 自动：增量
	if err != nil {
		log.Printf("panel: 模型可用性自动探测（%s）跳过: %v", trigger, err)
		return false, err
	}
	counts := map[string]int{}
	for _, e := range entries {
		counts[e.Level]++
	}
	log.Printf("panel: 模型可用性自动探测（%s）完成 %d 个模型 %v", trigger, len(entries), counts)
	return false, nil
}

// SetAvailProbeInterval 热改探测间隔（下一轮生效；0 = 暂停）。
func (p *Panel) SetAvailProbeInterval(d time.Duration) {
	p.availInterval.Store(int64(d))
}

// probeBody 构造探测请求体。
//
// 出站必须发**裸模型名**：realm 前缀（cn: / global:）是网关侧的路由协议，上游不认。
// 绕过网关直调 ChatStream 就绕过了 handler.go:887 的 rewriteModel，前缀原样发给
// 上游 → 每个模型都返回 11102 model_blocked（实测 35/35 全中，面板因此把所有模型
// 都标成「上游无此模型」）。
//
// 条目仍以带前缀的 id 作为 key（面板按 global:xxx 取用），只改请求体的 model。
// 请求体压到最小：max_tokens=1 + 单字符 prompt，实测 credit=0。
func probeBody(model string) []byte {
	bare := strings.TrimPrefix(model, "global:")
	bare = strings.TrimPrefix(bare, "cn:")
	return []byte(`{"model":` + quoteJSON(bare) +
		`,"messages":[{"role":"user","content":"1"}],"max_tokens":1}`)
}

// probeOne 对单个模型发一次最小请求并归类。
func (p *Panel) probeOne(acct *auth.Auth, model string) AvailEntry {
	e := AvailEntry{Model: model, CheckedAt: time.Now()}

	body := probeBody(model)

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
