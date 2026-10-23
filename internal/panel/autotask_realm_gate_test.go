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

// TestAutoTaskRejectsGlobalRealm 国际版账号必须在发起任何上游请求前被拒。
//
// 背景：2026-10-23 实测发现 taskcenter/tasksweep 都门控了 global，但 autotask
// 只门控 enterprise——于是「一键完成」把 27 个国内任务码推给了一个国际号
// （日志：一键完成可自动任务 uid=c2580a18 共 27 项）。国际版任务清单只有 5 个
// code，且列表里没有任何 reward_credit 字段，这些调用全部是无效上游请求。
//
// 判据不能只看状态码：还必须确认「一次上游请求都没发出去」，
// 否则「拒了但已经打过上游」这种情况会漏检。
func TestAutoTaskRejectsGlobalRealm(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK", "data": map[string]any{}})
	}))
	defer srv.Close()

	p := &Panel{cfg: Config{Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}}}
	// Domain 落 workbuddy.ai → Realm() 判为 global
	a := &auth.Auth{UID: "g1", AccessToken: "at", Domain: "www.workbuddy.ai"}
	pl := pool.New("")
	pl.Add(a)
	p.cfg.Pool = pl

	cases := []struct {
		name string
		path string
		body string
		call http.HandlerFunc
	}{
		{
			name: "一键完成",
			path: "/panel/api/accounts/g1/tasks/auto_all",
			call: p.accountTaskAutoAll,
		},
		{
			name: "单任务",
			path: "/panel/api/accounts/g1/tasks/auto",
			body: `{"task_code":"first_chat"}`,
			call: p.accountTaskAuto,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := atomic.LoadInt32(&hits)
			req := httptest.NewRequest("POST", c.path, strings.NewReader(c.body))
			req.SetPathValue("uid", "g1")
			rec := httptest.NewRecorder()
			c.call(rec, req)

			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("%s: status=%d want 422，body=%s", c.name, rec.Code, rec.Body.String())
			}
			if got := atomic.LoadInt32(&hits) - before; got != 0 {
				t.Fatalf("%s: 门控后仍发出了 %d 个上游请求", c.name, got)
			}
			// 报错信息必须给出可执行的下步，而不是只说「不支持」。
			if !strings.Contains(rec.Body.String(), "连登") {
				t.Errorf("%s: 提示未说明国际版唯一可获积分入口，body=%s", c.name, rec.Body.String())
			}
		})
	}
}

// TestAutoTaskStillAllowsCN CN 账号不应被这个门控误伤。
func TestAutoTaskStillAllowsCN(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "msg": "OK",
			"data": map[string]any{"tasks": []map[string]any{}},
		})
	}))
	defer srv.Close()

	p := &Panel{cfg: Config{Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}}}
	a := &auth.Auth{UID: "c1", AccessToken: "at", Domain: "www.codebuddy.cn"}
	pl := pool.New("")
	pl.Add(a)
	p.cfg.Pool = pl

	req := httptest.NewRequest("POST", "/panel/api/accounts/c1/tasks/auto_all", nil)
	req.SetPathValue("uid", "c1")
	rec := httptest.NewRecorder()
	p.accountTaskAutoAll(rec, req)

	if rec.Code == http.StatusUnprocessableEntity {
		t.Fatalf("CN 账号被误拦：%s", rec.Body.String())
	}
	if atomic.LoadInt32(&hits) == 0 {
		t.Fatal("CN 账号应能发起上游请求，实际一次都没发")
	}
}
