package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 模型级阻塞的状态码必须分两类。
//
// 背景（实测）：国际号上 global:deepseek-v4.1-flash 被上游 6004 限流后，
// 网关对每次请求都返回 400 model_unavailable。400 的语义是「请求本身错了，
// 重试无用」，而真实情况是「到点自愈，稍后再试」。客户端于是陷入自相矛盾的
// 处理：既不认为它可重试，又因为不是终态而反复重试——
// 单个模型一天刷出 206 个 400，重试间隔中位数 12s、最短 1s。
//
// 正确语义：
//
//	6004 等限流类     → 503 + Retry-After（暂时不可用）
//	11102 / 11158     → 400（确定性拒绝，等下去也不会恢复，该换模型）
func TestModelBlockRateLimitedReturns503WithRetryAfter(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "only", AccessToken: "at-only", ExpiresAt: 9999999999})
	// 唯一的号对该模型进入 6004 冷却，带上游重置墙钟。
	reset := time.Now().Add(45 * time.Minute)
	p.CooldownSoftForModel("only", 600*time.Second, reset, "deepseek-v4.1-flash",
		`6004 usage exceeds frequency limit, will reset at wall clock`)

	up := newFakeUpstream(t, func(string) (int, string, bool) {
		t.Error("模型级阻塞时不应再打上游：选号就该返回 nil")
		return 500, "", false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"1"}]}`)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("限流类模型阻塞应返回 503（暂时不可用），实际 %d body=%s", rec.Code, rec.Body)
	}
	// Retry-After 必须存在且指向解封时刻：客户端据此退避才不会每 12 秒撞一次。
	ra := rec.Header().Get("Retry-After")
	if ra == "" {
		t.Fatal("503 必须带 Retry-After 头（否则客户端无从知道该等多久）")
	}
	secs, err := strconv.Atoi(ra)
	if err != nil {
		t.Fatalf("Retry-After=%q 不是整数秒: %v", ra, err)
	}
	// 上限截断到 soft_rate_max（默认 2h），所以只断言「在合理区间内且非 0」。
	if secs <= 0 {
		t.Fatalf("Retry-After=%d 应大于 0", secs)
	}

	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON: %v body=%s", err, rec.Body)
	}
	if resp.Error.Code != "model_unavailable" {
		t.Errorf("code=%q want model_unavailable（code 不变，只改 HTTP 状态码语义）", resp.Error.Code)
	}
}

// 确定性拒绝（11102 / 11158）必须保持 400：等到期也不会恢复，客户端该换模型。
// 这条同时是回归保护——不能为了修限流语义把确定性拒绝也改成可重试。
func TestModelBlockDeterministicStays400(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "only", AccessToken: "at-only", ExpiresAt: 9999999999})
	p.BlockModelBackoff("only", "glm-5.3", `11102 model [glm-5.3] service info not found`)

	up := newFakeUpstream(t, func(string) (int, string, bool) {
		t.Error("确定性阻塞时不应再打上游")
		return 500, "", false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[{"role":"user","content":"1"}]}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("确定性拒绝应保持 400（不可重试），实际 %d body=%s", rec.Code, rec.Body)
	}
	if ra := rec.Header().Get("Retry-After"); ra != "" {
		t.Errorf("400 不应带 Retry-After（会暗示可重试），得到 %q", ra)
	}
}

// 混合场景：一个号 11158、另一个号 6004。等 6004 到点就能恢复，
// 所以整体仍应按「可重试」处理，不能报成不可恢复。
func TestModelBlockMixedTreatedAsRetryable(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "denied", AccessToken: "at-d", ExpiresAt: 9999999999},
		&auth.Auth{UID: "limited", AccessToken: "at-l", ExpiresAt: 9999999999},
	)
	p.BlockModelBackoff("denied", "glm-5.3", `11158 model is not allowed for user type`)
	p.CooldownSoftForModel("limited", 600*time.Second, time.Now().Add(20*time.Minute), "glm-5.3", `6004 rate limited`)

	if st := p.ModelBlocked("glm-5.3"); st.Deterministic {
		t.Fatalf("混合场景不应判为确定性拒绝（6004 到点会恢复）: %+v", st)
	}

	up := newFakeUpstream(t, func(string) (int, string, bool) { return 500, "", false })
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[{"role":"user","content":"1"}]}`)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("混合场景应按可重试处理（503），实际 %d body=%s", rec.Code, rec.Body)
	}
}

// 全部锁都是确定性拒绝时才算 Deterministic —— 与 isDeterministicReason 同口径。
func TestModelBlockedDeterministicFlag(t *testing.T) {
	cases := []struct {
		name string
		why  []string
		want bool
	}{
		{"全部 11102", []string{"11102 not found", "11102 not found"}, true},
		{"全部 11158", []string{"11158 not allowed", "11158 not allowed"}, true},
		{"11102 + 11158 混合", []string{"11102 not found", "11158 not allowed"}, true},
		{"含 6004", []string{"11102 not found", "6004 usage exceeds"}, false},
		{"全部 6004", []string{"6004 a", "6004 b"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := testPoolWith(
				&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999},
				&auth.Auth{UID: "a2", AccessToken: "at2", ExpiresAt: 9999999999},
			)
			p.BlockModelBackoff("a1", "m", c.why[0])
			p.BlockModelBackoff("a2", "m", c.why[1])
			st := p.ModelBlocked("m")
			if !st.Blocked {
				t.Fatalf("两号都锁该模型时应 Blocked: %+v", st)
			}
			if st.Deterministic != c.want {
				t.Errorf("Deterministic=%v want %v (reasons=%v)", st.Deterministic, c.want, c.why)
			}
		})
	}
}
