package upstream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// Web 域 UA 必须是**官方 Electron 客户端形态**，且全池恒定。
//
// 依据（A 级实证）：官方 5.9.3 客户端 pcap + SSLKEYLOGFILE 解密，
// workbuddy.cn 全部请求的 UA 逐字为：
//
//	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko)
//	WorkBuddy/5.9.3 Chrome/138.0.7204.251 Electron/37.10.3 Safari/537.36
//
// 这条测试防止再次退回「按账号派生国产浏览器」的错误方向——那是真人浏览网页的
// 分布，不是官方客户端发请求的形态；多账号各带不同 UA 反而暴露代理池特征。
func TestWebUALooksLikeOfficialElectron(t *testing.T) {
	c := New()
	cn := &auth.Auth{UID: "u1", Domain: "copilot.tencent.com"}
	cn.BackfillRealm()

	ua := webUAFor(c, cn)

	// 四段必须齐全：Mozilla 头、WorkBuddy 版本、Chrome 内核、Electron 运行时。
	for _, want := range []string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64)",
		"AppleWebKit/537.36",
		"WorkBuddy/" + defaultClientVersionCN,
		"Chrome/" + webDomainChromeVersion,
		"Electron/" + webDomainElectronVersion,
		"Safari/537.36",
	} {
		if !strings.Contains(ua, want) {
			t.Errorf("UA 缺片段 %q，实际 %q", want, ua)
		}
	}
}

// 全池恒定：不同 uid 必须得到**同一个** UA（官方一台机器多账号共用客户端）。
func TestWebUAConstantAcrossAccounts(t *testing.T) {
	c := New()
	var first string
	for i, uid := range []string{"acct-a", "acct-b", "acct-c", "acct-d", "acct-e"} {
		a := &auth.Auth{UID: uid, Domain: "copilot.tencent.com"}
		a.BackfillRealm()
		got := webUAFor(c, a)
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("uid=%s 的 UA 与其他账号不同：\n  %q\n  %q（官方全池恒定）", uid, first, got)
		}
	}
}

// 版本段按 realm 走：CN 5.9.3 / global 5.9.4，其余字节一致。
func TestWebUAVersionFollowsRealm(t *testing.T) {
	c := New()
	cn := &auth.Auth{UID: "u1", Domain: "copilot.tencent.com"}
	cn.BackfillRealm()
	gl := &auth.Auth{UID: "u2", Domain: "www.workbuddy.ai"}
	gl.BackfillRealm()

	cnUA, glUA := webUAFor(c, cn), webUAFor(c, gl)
	if !strings.Contains(cnUA, "WorkBuddy/"+defaultClientVersionCN) {
		t.Errorf("CN UA 版本段不对：%q", cnUA)
	}
	if !strings.Contains(glUA, "WorkBuddy/"+defaultClientVersionGlobal) {
		t.Errorf("global UA 版本段不对：%q", glUA)
	}
	// Chrome / Electron 段两域一致（同一个 Electron 运行时）。
	if !strings.Contains(glUA, "Chrome/"+webDomainChromeVersion) ||
		!strings.Contains(glUA, "Electron/"+webDomainElectronVersion) {
		t.Errorf("global UA 的 Chrome/Electron 段不对：%q", glUA)
	}
}

// nil 账号 / 空 client 也要给出可用 UA，不能 panic。
func TestWebUANilSafe(t *testing.T) {
	if ua := webUAFor(nil, nil); !strings.HasPrefix(ua, "Mozilla/5.0") {
		t.Errorf("nil 兜底 UA 形态不合规：%q", ua)
	}
	if ua := webUAFor(New(), nil); !strings.Contains(ua, "Electron/") {
		t.Errorf("nil 账号 UA 缺 Electron 段：%q", ua)
	}
}

// 浏览器提示头必须齐全：缺这族头的 Electron 应用不存在（Chromium 自己会加），
// 少了它们「Mozilla ... Electron/37」这个 UA 反而成了孤证。
func TestInjectWebClientHints(t *testing.T) {
	req := httptest.NewRequest("POST", "https://www.workbuddy.cn/v2/report", nil)
	injectWebClientHints(req, "cross-site", "cors")

	want := map[string]string{
		"sec-ch-ua":          `"Not)A;Brand";v="8", "Chromium";v="138",?0`,
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": `"Windows"`,
		"Sec-Fetch-Site":     "cross-site",
		"Sec-Fetch-Mode":     "cors",
		"Sec-Fetch-Dest":     "empty",
		"Accept-Language":    "zh-CN",
		"Accept-Encoding":    "gzip, deflate, br, zstd",
		"Priority":           "u=1, i",
	}
	for k, v := range want {
		if got := req.Header.Get(k); got != v {
			t.Errorf("%s = %q want %q", k, got, v)
		}
	}
	_ = http.NoBody
}

// ReportWebEvent 的出站必须带上 Electron UA 与提示头（端到端，打到本地 sink）。
func TestReportWebEventSendsElectronIdentity(t *testing.T) {
	var gotUA, gotCH, gotSite, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotCH = r.Header.Get("sec-ch-ua")
		gotSite = r.Header.Get("Sec-Fetch-Site")
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()

	c := New()
	c.WebBaseCN = srv.URL
	a := &auth.Auth{UID: "02b6f3fb-0000-0000-0000-000000000000", AccessToken: "at", Nickname: "n"}
	if err := c.ReportWebEvent(a, "web_element_click", "https://www.workbuddy.cn/space/d/x",
		"library_doc_intro_click", "WorkBuddy资料库介绍"); err != nil {
		t.Fatalf("ReportWebEvent: %v", err)
	}

	if !strings.Contains(gotUA, "Electron/") || !strings.Contains(gotUA, "WorkBuddy/") {
		t.Errorf("出站 UA 不是官方 Electron 形态：%q", gotUA)
	}
	if gotCH == "" {
		t.Error("未发送 sec-ch-ua（Electron 客户端必有）")
	}
	if gotSite != "cross-site" {
		t.Errorf("Sec-Fetch-Site = %q want cross-site（XHR 形态）", gotSite)
	}
	// 事件体里的 userAgent 必须与头一致，否则是两套指纹。
	if strings.Contains(gotBody, "userAgent") {
		uaIdx := strings.Index(gotBody, `"userAgent":"`)
		if uaIdx >= 0 {
			rest := gotBody[uaIdx+len(`"userAgent":"`):]
			end := strings.Index(rest, `"`)
			if end > 0 && rest[:end] != gotUA {
				t.Errorf("事件体 userAgent(%q) 与请求头 UA(%q) 不一致", rest[:end], gotUA)
			}
		}
	}
}
