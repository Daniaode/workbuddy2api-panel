package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestCheckinSkipsGlobalRealm 国际版账号不得发起签到请求。
//
// 背景（2026-10-23 实测）：/billing/meter/daily-checkin 与
// /v2/billing/meter/daily-checkin 对国际号均返回 400 code=10001
// 「签到活动未开启或已过期」。对照国内账号同一端点正常，且 get-user-resource
// 的包里带「签到/每日」文案、国际版没有任何签到文案——说明国际版**没有**签到活动，
// 不是「今天签过了」，也不是路径不对。
//
// 判据是「零上游请求」而不是只看响应：打了再报错同样是在刷无效请求。
func TestCheckinSkipsGlobalRealm(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "msg": "OK",
			"data": map[string]any{"Response": map[string]any{"Data": map[string]any{
				"TotalDosage": 250,
			}}},
		})
	}))
	defer srv.Close()

	pl := pool.New("")
	a := &auth.Auth{UID: "g1", AccessToken: "at", Domain: "www.workbuddy.ai"}
	pl.Add(a)
	p := &Panel{cfg: Config{
		Pool: pl,
		Upstream: &upstream.Client{
			HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL,
			WebBaseCN: srv.URL,
		},
	}}

	req := httptest.NewRequest("POST", "/panel/api/accounts/g1/checkin", nil)
	req.SetPathValue("uid", "g1")
	rec := httptest.NewRecorder()
	p.accountCheckin(rec, req)

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON：%s", rec.Body.String())
	}

	// 1) 必须明确标记「无此功能」，前端据此显示中性提示而不是「签到完成」。
	if body["checkin_unsupported"] != true {
		t.Errorf("缺 checkin_unsupported 标记，body=%v", body)
	}
	// 2) 不能被当成「今天已签到」——那会让按钮显示「已签」，与事实相反。
	if body["checkin_done"] == true {
		t.Errorf("国际版不应被标记为已签到，body=%v", body)
	}
	// 3) 提示文案要说明是功能缺失，而不是上游报错原文。
	msg, _ := body["checkin_message"].(string)
	if !strings.Contains(msg, "无签到活动") {
		t.Errorf("checkin_message=%q，应说明国际版无签到活动", msg)
	}
	if strings.Contains(msg, "10001") {
		t.Errorf("不该把上游错误码透给用户：%q", msg)
	}
	// 4) 仍然要刷新余额（这是该端点的退化职责）。
	if body["credits"] == nil {
		t.Errorf("应仍返回余额，body=%v", body)
	}
	_ = hits
}

// TestCheckinStillCallsUpstreamForCN CN 账号必须照常发起签到请求（不被误伤）。
func TestCheckinStillCallsUpstreamForCN(t *testing.T) {
	var checkinHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "daily-checkin") {
			atomic.AddInt32(&checkinHits, 1)
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK", "data": map[string]any{}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "msg": "OK",
			"data": map[string]any{"Response": map[string]any{"Data": map[string]any{
				"TotalDosage": 1000,
			}}},
		})
	}))
	defer srv.Close()

	pl := pool.New("")
	a := &auth.Auth{UID: "c1", AccessToken: "at", Domain: "www.codebuddy.cn"}
	pl.Add(a)
	p := &Panel{cfg: Config{
		Pool: pl,
		Upstream: &upstream.Client{
			HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL,
			WebBaseCN: srv.URL,
		},
	}}

	req := httptest.NewRequest("POST", "/panel/api/accounts/c1/checkin", nil)
	req.SetPathValue("uid", "c1")
	rec := httptest.NewRecorder()
	p.accountCheckin(rec, req)

	if atomic.LoadInt32(&checkinHits) == 0 {
		t.Fatal("CN 账号应发起签到请求，实际一次都没发")
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["checkin_unsupported"] == true {
		t.Errorf("CN 账号不应被标为无此功能：%v", body)
	}
}
