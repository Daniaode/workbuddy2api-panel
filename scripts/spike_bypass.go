//go:build ignore

// spike_bypass.go — 方案第 1 步 spike：验证「旁路直连」与「经网关」的判定是否一致。
//
// 目的只有一个：确认 internal/upstream.Client.ChatStream 在不经过 Pool / Usage /
// reqlog / handler 的情况下，仍能以同样的出站指纹拿到 200。若被 11-128 拦而经网关
// 正常，说明缺了后者独有的东西，方案要停下来重估。
//
// 跑法（容器内，见 scripts/spike_bypass.sh）：
//
//	go run spike_bypass.go -auths /app/auths -mode bypass
//
// 输出一行 JSON 到 stdout，便于与网关对照组逐字段比对。
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

type result struct {
	Mode       string         `json:"mode"`
	UID        string         `json:"uid"`
	Status     int            `json:"status"`
	Kind       string         `json:"kind,omitempty"`
	Err        string         `json:"err,omitempty"`
	TTFBMs     int64          `json:"ttfb_ms"`
	TotalMs    int64          `json:"total_ms"`
	Frames     int            `json:"frames"`
	SawDone    bool           `json:"saw_done"`
	Model      string         `json:"model"`
	Finish     string         `json:"finish_reason"`
	UsageKeys  []string       `json:"usage_keys"`
	Usage      map[string]any `json:"usage"`
	BodyPrefix string         `json:"body_prefix,omitempty"`
}

func main() {
	authDir := flag.String("auths", "auths", "auth 目录")
	mode := flag.String("mode", "bypass", "标签（写进输出，便于比对）")
	model := flag.String("model", "deepseek-v4.1-flash", "模型原名（不带 cn: 前缀）")
	prefix := flag.String("prefix", "02b6f3fb", "账号 uid 前缀")
	flag.Parse()

	files, _ := filepath.Glob(filepath.Join(*authDir, "workbuddy-"+*prefix+"*.json"))
	if len(files) == 0 {
		fmt.Fprintf(os.Stderr, "no auth for prefix %s in %s\n", *prefix, *authDir)
		os.Exit(2)
	}
	sort.Strings(files)
	raw, err := os.ReadFile(files[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "read auth:", err)
		os.Exit(2)
	}
	a, err := auth.Parse(raw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "parse auth:", err)
		os.Exit(2)
	}

	body := []byte(fmt.Sprintf(
		`{"model":%q,"messages":[{"role":"user","content":"1"}],"max_tokens":1}`,
		*model,
	))

	c := upstream.New()
	c.IdleTimeout = 25 * time.Second

	start := time.Now()
	rc, status, respBody, cerr := c.ChatStream(a, body, "", upstream.ChatMeta{})
	res := result{Mode: *mode, UID: a.UID[:8], Status: status, Model: *model}
	if cerr != nil {
		res.Err = cerr.Error()
		if ue, ok := cerr.(*upstream.Error); ok {
			res.Kind = ue.Kind.String()
		}
		if len(respBody) > 0 {
			res.BodyPrefix = truncate(string(respBody), 300)
		}
		res.TotalMs = time.Since(start).Milliseconds()
		emit(res)
		return
	}
	if rc == nil {
		res.Err = "nil body with nil error"
		emit(res)
		return
	}
	defer rc.Close()

	// 手工读 SSE：记录首帧到达时间（TTFB）与帧数、[DONE]、末帧 usage。
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var ttfb time.Duration
	var usage map[string]any
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
			res.SawDone = true
			continue
		}
		res.Frames++
		var frame struct {
			Model   string `json:"model"`
			Choices []struct {
				FinishReason string `json:"finish_reason"`
				Delta        map[string]any `json:"delta"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if json.Unmarshal([]byte(payload), &frame) == nil {
			if frame.Model != "" {
				res.Model = frame.Model
			}
			if len(frame.Choices) > 0 && frame.Choices[0].FinishReason != "" {
				res.Finish = frame.Choices[0].FinishReason
			}
			if frame.Usage != nil {
				usage = frame.Usage
			}
		}
	}
	if err := sc.Err(); err != nil {
		res.Err = "read stream: " + err.Error()
	}
	res.TTFBMs = ttfb.Milliseconds()
	res.TotalMs = time.Since(start).Milliseconds()
	if usage != nil {
		res.Usage = usage
		keys := make([]string, 0, len(usage))
		for k := range usage {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		res.UsageKeys = keys
	}
	emit(res)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func emit(r result) {
	out, _ := json.MarshalIndent(r, "", "  ")
	fmt.Println(string(out))
}
