// baseline.go 延迟基线与历史留痕。
//
// 为什么不能一上来就判「延迟劣化」：TTFB 的中位数需要足够样本才稳定，用不稳的
// 基准去判异常必然误报。误报几次之后人就会对告警麻木，比不报更糟。因此前
// WarmupDays 天只积累、只输出观测值，不产出 latency_degraded。
//
// 另一个必须记住的坑：旁路探测是独立进程 + 冷连接，每次全新 TLS，实测 TTFB
// 6.5s；经网关是复用热连接，实测 1.8s。两者不可跨路径比较，基线必须按来源
// 分桶——否则第一次跑就误报。
package probe

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// HistoryEntry 一次探测的落盘记录（一行一条 JSONL）。
type HistoryEntry struct {
	Time    time.Time `json:"time"`
	Level   Level     `json:"level"`
	Source  string    `json:"source"` // bypass / gateway：基线按来源分桶
	UID     string    `json:"uid"`
	Nick    string    `json:"nick"`
	Model   string    `json:"model"`
	Status  int       `json:"status"`
	Code    string    `json:"code,omitempty"`
	TTFBMs  int64     `json:"ttfb_ms"`
	TotalMs int64     `json:"total_ms"`
	OK      bool      `json:"ok"`
	// Cold 冷连接标记（进程内第一次出站）。基线按它分桶——冷/热 TTFB 实测差 6 倍。
	Cold  bool     `json:"cold,omitempty"`
	Drift []string `json:"drift,omitempty"`
}

// BaselineOptions 判定参数。
type BaselineOptions struct {
	// WarmupDays 只积累不判定的天数（默认 14）。
	WarmupDays int
	// MinSamples 中位数所需的最少样本数。
	MinSamples int
	// Multiplier 当前 TTFB 超过中位数的倍数阈值（默认 2.5）。
	Multiplier float64
	// Floor 绝对下限：低于它的绝对延迟不值得报（默认 3s）。
	Floor time.Duration
	// Consecutive 需要连续命中几次才产出结论（默认 2）。
	Consecutive int
}

func (o BaselineOptions) withDefaults() BaselineOptions {
	if o.WarmupDays <= 0 {
		o.WarmupDays = 14
	}
	if o.MinSamples <= 0 {
		o.MinSamples = 20
	}
	if o.Multiplier <= 0 {
		o.Multiplier = 2.5
	}
	if o.Floor <= 0 {
		o.Floor = 3 * time.Second
	}
	if o.Consecutive <= 0 {
		o.Consecutive = 2
	}
	return o
}

// baselineKey 基线分桶键：来源 + 冷/热 + 账号 + 模型。
//
// 三层都不能省：
//   - 来源：旁路独立进程（全新 TLS，实测 6.5s）与网关热连接（1.8s）不可比；
//   - 冷/热：同一进程内首个请求 6.6s、后续 1.0s，混在一起中位数变双峰；
//   - 账号 × 模型：不按账号分桶会把「快号」和「慢号」混在一起，掩盖单号劣化。
func baselineKey(source string, cold bool, uid, model string) string {
	ck := "hot"
	if cold {
		ck = "cold"
	}
	return source + "|" + ck + "|" + shortUID(uid) + "|" + model
}

// History 滚动历史文件。读写都在同一把锁里做，因为一次运行内可能既要读基线
// 又要追加记录；跨进程并发由「同一时刻只有一个探测任务」的调度保证。
type History struct {
	mu   sync.Mutex
	path string
	// Keep 保留时长（默认 14 天 + 余量）。基线窗口本身就是 14 天，历史必须
	// 至少覆盖窗口长度，否则「刚热起来就丢样本」，永远到不了判定条件。
	Keep time.Duration
}

// NewHistory 创建历史文件句柄。path 为空 = 纯内存（测试用）。
func NewHistory(path string) *History {
	return &History{path: path, Keep: 30 * 24 * time.Hour}
}

// Load 读回 keep 窗口内的记录。文件缺失/行损坏都不报错——丢历史好过让
// 探测跑不起来；损坏行跳过而不是中断整个文件。
func (h *History) Load(now time.Time) []HistoryEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.loadLocked(now)
}

func (h *History) loadLocked(now time.Time) []HistoryEntry {
	if h.path == "" {
		return nil
	}
	f, err := os.Open(h.path)
	if err != nil {
		return nil
	}
	defer f.Close()

	cutoff := now.Add(-h.Keep)
	var out []HistoryEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e HistoryEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		if e.Time.Before(cutoff) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// Append 追加一条记录并顺手裁掉窗口外的旧行（重写而非原地追加：
// 探测频率低，一天 120 条，重写成本可忽略，换来文件不无限增长）。
func (h *History) Append(e HistoryEntry) error {
	if h.path == "" {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	all := append(h.loadLocked(time.Now()), e)

	if dir := filepath.Dir(h.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := h.path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, it := range all {
		raw, err := json.Marshal(it)
		if err != nil {
			continue
		}
		w.Write(raw)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, h.path)
}

// MedianTTFB 计算指定桶的 TTFB 中位数。样本不足时返回 (0, 0, false)。
func MedianTTFB(entries []HistoryEntry, source string, cold bool, uid, model string) (median time.Duration, n int, ok bool) {
	key := baselineKey(source, cold, uid, model)
	var vals []int64
	for _, e := range entries {
		if !e.OK || e.TTFBMs <= 0 {
			continue
		}
		if baselineKey(e.Source, e.Cold, e.UID, e.Model) != key {
			continue
		}
		vals = append(vals, e.TTFBMs)
	}
	if len(vals) == 0 {
		return 0, 0, false
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	mid := len(vals) / 2
	var m int64
	if len(vals)%2 == 1 {
		m = vals[mid]
	} else {
		m = (vals[mid-1] + vals[mid]) / 2
	}
	return time.Duration(m) * time.Millisecond, len(vals), true
}

// Warm 判断是否仍在热身期：从该桶最早样本算起，不满 WarmupDays 天就不判定。
func Warm(entries []HistoryEntry, source string, cold bool, uid, model string, now time.Time, days int) bool {
	key := baselineKey(source, cold, uid, model)
	var earliest time.Time
	for _, e := range entries {
		if baselineKey(e.Source, e.Cold, e.UID, e.Model) != key {
			continue
		}
		if earliest.IsZero() || e.Time.Before(earliest) {
			earliest = e.Time
		}
	}
	if earliest.IsZero() {
		return true
	}
	return now.Sub(earliest) < time.Duration(days)*24*time.Hour
}

// ConsecutiveSlow 统计「最近连续多少次超过阈值」（按时间倒序数）。
func ConsecutiveSlow(entries []HistoryEntry, source string, cold bool, uid, model string, threshold time.Duration) int {
	key := baselineKey(source, cold, uid, model)
	var mine []HistoryEntry
	for _, e := range entries {
		if !e.OK || baselineKey(e.Source, e.Cold, e.UID, e.Model) != key {
			continue
		}
		mine = append(mine, e)
	}
	sort.Slice(mine, func(i, j int) bool { return mine[i].Time.Before(mine[j].Time) })
	n := 0
	for i := len(mine) - 1; i >= 0; i-- {
		if time.Duration(mine[i].TTFBMs)*time.Millisecond > threshold {
			n++
			continue
		}
		break
	}
	return n
}

// JudgeLatency 产出延迟劣化结论。热身期、样本不足、未连续命中 → 一律不报。
func JudgeLatency(att Attempt, source string, entries []HistoryEntry, opt BaselineOptions) []Finding {
	opt = opt.withDefaults()
	if !att.OK() || att.TTFBMs <= 0 {
		return nil
	}
	now := time.Now()
	if Warm(entries, source, att.Cold, att.UID, att.Model, now, opt.WarmupDays) {
		return nil
	}
	median, n, ok := MedianTTFB(entries, source, att.Cold, att.UID, att.Model)
	if !ok || n < opt.MinSamples {
		return nil
	}
	threshold := time.Duration(float64(median) * opt.Multiplier)
	if threshold < opt.Floor {
		threshold = opt.Floor
	}
	cur := time.Duration(att.TTFBMs) * time.Millisecond
	if cur <= threshold {
		return nil
	}
	// 连续命中才报：单次抖动（TCP 重传、偶发排队）不构成征兆。
	if ConsecutiveSlow(entries, source, att.Cold, att.UID, att.Model, threshold) < opt.Consecutive-1 {
		return nil
	}
	return []Finding{{
		Level:  "medium",
		Code:   "latency_degraded",
		UID:    att.UID,
		Nick:   att.Nick,
		Model:  att.Model,
		Detail: formatLatency(att.TTFBMs, median, threshold, n),
	}}
}

func formatLatency(curMs int64, median, threshold time.Duration, samples int) string {
	return formatDur(curMs) + " vs 中位 " + formatDur(median.Milliseconds()) +
		"（阈值 " + formatDur(threshold.Milliseconds()) + "，样本 " + itoa(samples) + "）"
}

func formatDur(ms int64) string {
	if ms >= 1000 {
		return itoa(int(ms/1000)) + "." + itoa(int(ms%1000/100)) + "s"
	}
	return itoa(int(ms)) + "ms"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
