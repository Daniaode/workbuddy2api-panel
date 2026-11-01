// webua.go Web 域事件的浏览器身份：**官方 Electron 客户端的固定 UA**。
//
// 结论来源（A 级实证）：Windows 官方客户端 5.9.3 的 pcap + SSLKEYLOGFILE 抓包，
// 解密后 workbuddy.cn 全部请求的 User-Agent 逐字如下——
//
//	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko)
//	WorkBuddy/<ver> Chrome/138.0.7204.251 Electron/37.10.3 Safari/537.36
//
// 三个关键事实（此前两次都判断错了，以此为准）：
//
//  1. **官方是 Electron 应用**，UA 里同时带 WorkBuddy/<ver>、Chrome/138、Electron/37。
//  2. **UA 全池恒定**：抓包里所有请求同一个 UA，不随账号变化。官方一台机器装一份
//     客户端、多个账号共用，本来就不会按账号换浏览器。
//  3. **不是国产浏览器**：先前按「国内用户浏览器混杂（UC/搜狗/360/QQ/微信）」做
//     按 uid 派生的假设是错的——那是「真人浏览网页」的分布，不是「官方客户端
//     发请求」的形态。多账号各带不同浏览器 UA，反而暴露代理池特征。
//
// 历史沿革：最早的硬编码是纯 Chrome/152；第二轮改成按 uid 从国产浏览器池派生；
// 本次按抓包改回**固定的 Electron 形态**。前两次都没有流量实证，这次有。
package upstream

import (
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// webDomainChromeVersion / webDomainElectronVersion 官方 Electron 运行时版本。
//
// 取自抓包（5.9.3 客户端）。这两个版本只随**客户端版本**变化，与账号无关；
// 官方升级 Electron 后需要按新包重新核对。
const (
	webDomainChromeVersion   = "138.0.7204.251"
	webDomainElectronVersion = "37.10.3"
)

// webUAFor 返回官方 Electron 形态的 web 域 UA。
//
// 版本段跟账号域走（CN 5.9.3 / global 5.9.4），其余字节全池一致。
func webUAFor(c *Client, a *auth.Auth) string {
	ver := ""
	if c != nil {
		ver = c.clientVersionFor(a)
	}
	if ver == "" {
		ver = defaultClientVersion
	}
	return "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) " +
		"WorkBuddy/" + ver + " Chrome/" + webDomainChromeVersion +
		" Electron/" + webDomainElectronVersion + " Safari/537.36"
}

// injectWebClientHints 注入官方 Electron 客户端在 web 域实际发送的浏览器提示头。
//
// 抓包实证（workbuddy.cn 全部请求恒定）：
//
//	sec-ch-ua: "Not)A;Brand";v="8", "Chromium";v="138",?0
//	sec-ch-ua-mobile: ?0
//	sec-ch-ua-platform: "Windows"
//	sec-fetch-site: cross-site / none（导航请求为 none，XHR 为 cross-site）
//	sec-fetch-mode: cors / no-cors
//	sec-fetch-dest: empty
//	accept-language: zh-CN
//	accept-encoding: gzip, deflate, br, zstd
//	priority: u=1, i
//
// 缺这族头的 Electron 应用是不存在的：Chromium 内核自己就会加。少了它们，
// 「Mozilla/5.0 ... Electron/37」这个 UA 反而成了孤证。
func injectWebClientHints(req *http.Request, secFetchSite, secFetchMode string) {
	req.Header.Set("sec-ch-ua", `"Not)A;Brand";v="8", "Chromium";v="138",?0`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", `"Windows"`)
	req.Header.Set("sec-fetch-site", secFetchSite)
	req.Header.Set("sec-fetch-mode", secFetchMode)
	req.Header.Set("sec-fetch-dest", "empty")
	req.Header.Set("Accept-Language", "zh-CN")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	req.Header.Set("priority", "u=1, i")
}
