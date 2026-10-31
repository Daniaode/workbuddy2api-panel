// webua.go Web 域行为事件的浏览器 UA 池：按 uid 稳定派生，每账号固定一台「真实浏览器」。
//
// 为什么需要
//
//	ReportWebEvent 往 www.workbuddy.cn/v2/report 上报 web 域页面事件，出站是**浏览器
//	形态**（带 x-client-platform: web + Origin/Referer，事件体里也带 userAgent/os/
//	machineId）。此前 UA 是一行硬编码的 Chrome 152，于是全池账号呈现：
//
//	    machineId = 按 uid 派生（不同账号 = 不同机器）
//	    userAgent = 恒定 Chrome/152.0.0.0（所有机器同一个浏览器版本）
//
//	「几十台不同机器却装着完全相同的浏览器版本」本身就是矛盾特征——真实用户群里
//	不可能全员同一版本，国产环境更是 Chrome / Edge / QQ / 360 / 搜狗 / UC / 微信
//	混杂。这与 desktopFingerprint 硬件档案那处是同一类问题（见 deriveHardwareProfile）。
//
// 设计约束
//
//  1. **每账号固定**：同一 uid 永远得到同一个浏览器与版本（真实用户的浏览器不会
//     每隔几小时换一次）。派生用 uid 的稳定哈希，不引入随机源。
//  2. **池内取值真实**：每个条目都是实际存在过的浏览器 + 真实版本号区间，
//     版本在小范围内派生偏移，避免「6 个号 6 个版本号但都是不存在的号」。
//  3. **与事件字段自洽**：池内全部为 Windows 桌面端浏览器（Chrome 内核或国产
//     双核），与事件体 os=Win32 / osVersion=10.0 不冲突。
//  4. **chat 链路不参与**：这条只服务 web 域事件。chat 出站必须保持官方桌面端
//     UA（WorkBuddy/<ver> ... CLI/<ver>），换浏览器 UA 会与同请求里的 X-IDE-*
//     头族直接矛盾（详见 headers.go 的 ChatHeaders）。
package upstream

import (
	"crypto/sha256"
	"fmt"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// webBrowser 一个真实浏览器指纹族：UA 模板 + 可派生版本区间。
type webBrowser struct {
	// template 含两个 %d 占位：Chrome 内核主版本、版本次段（都传 int）。
	// 用 fmt.Sprintf 拼装，避免每个版本写一条完整 UA。
	template string
	// baseChrome 该浏览器内核对应的 Chrome 主版本候选（取模选择）。
	baseChrome []int
	// build 版本号次段候选。
	build []int
}

// webBrowserPool Web 域事件可用的浏览器池。
//
// 覆盖国内常见组合：Chrome / Edge（Windows 自带，占比最高）、QQ 浏览器、
// 360 安全+极速、搜狗、UC、以及微信内置浏览器（PC 端微信打开链接的场景）。
// 权重用重复条目表达——Chrome / Edge 占多数，国产浏览器各一条，
// 贴近真实国内 Windows 桌面分布，不至于「多数账号都是小众浏览器」这种反向异常。
var webBrowserPool = []webBrowser{
	// Chrome（占比最高）
	{template: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.%d.0 Safari/537.36",
		baseChrome: []int{152, 151, 150}, build: []int{0, 86, 132}},
	{template: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.%d.0 Safari/537.36",
		baseChrome: []int{149, 148}, build: []int{0, 64, 110}},
	// Edge（Windows 自带）
	{template: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.%d.0 Safari/537.36 Edg/%d.0.%d.0",
		baseChrome: []int{152, 151}, build: []int{0, 86}},
	// QQ 浏览器（Chromium 内核，国内常见）
	{template: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.%d.0 Safari/537.36 QQBrowser/19.5.8107.400",
		baseChrome: []int{128, 122}, build: []int{0, 64}},
	// 360 安全浏览器（双核，切到极速模式时上报 Chromium 内核）
	{template: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.%d.0 Safari/537.36 QIHU 360SE",
		baseChrome: []int{122, 118}, build: []int{0, 54}},
	// 360 极速浏览器
	{template: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.%d.0 Safari/537.36 QIHU 360EE",
		baseChrome: []int{122, 114}, build: []int{0, 48}},
	// 搜狗高速浏览器
	{template: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.%d.0 Safari/537.36 SE 2.X MetaSr 1.0",
		baseChrome: []int{118, 108}, build: []int{0, 40}},
	// UC 浏览器（PC）
	{template: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.%d.0 Safari/537.36 UBrowser/7.0.185.1002",
		baseChrome: []int{108, 102}, build: []int{0, 36}},
	// 微信内置浏览器（PC 微信打开链接；MicroMessenger 段是它的识别特征）
	{template: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.%d.0 Safari/537.36 MicroMessenger/7.0.20.1781(0x6700143B) WindowsWechat(0x63090a13)",
		baseChrome: []int{81, 78}, build: []int{0, 32}},
}

// deriveWebUA 按 uid 稳定派生该账号的浏览器 UA。
//
// 同一 uid 恒定（同一台机器不换浏览器）、不同 uid 分散（不同机器不同浏览器）。
// uid 为空时回落到池首条（Chrome 当前版），保证调用方总能拿到可用 UA。
func deriveWebUA(a *auth.Auth) string {
	if a == nil || a.UID == "" {
		b := webBrowserPool[0]
		return formatWebUA(b, b.baseChrome[0], b.build[0])
	}
	sum := sha256.Sum256([]byte("webua:" + a.UID))
	b := webBrowserPool[int(sum[0])%len(webBrowserPool)]
	chrome := b.baseChrome[int(sum[1])%len(b.baseChrome)]
	build := b.build[int(sum[2])%len(b.build)]
	return formatWebUA(b, chrome, build)
}

// formatWebUA 用两个 %d 填 Chrome 主版本与 build 段。
//
// Edge 的模板里 Edg/ 段复用同一组版本值（真实 Edge 的 Edg 版本与 Chrome 主版本
// 通常同号，这样写不会造出「Chrome 152 + Edg 120」这种不存在的组合）。
func formatWebUA(b webBrowser, chrome, build int) string {
	if countPlaceholders(b.template) == 4 {
		// Edge：Chrome/ 与 Edg/ 两段都填同一组版本（真实 Edge 的 Edg 主版本
		// 通常与 Chrome 同号，避免造出「Chrome 152 + Edg 120」这种不存在的组合）。
		return fmt.Sprintf(b.template, chrome, build, chrome, build)
	}
	return fmt.Sprintf(b.template, chrome, build)
}

// countPlaceholders 统计模板里的格式动词个数（本文件只用 %d）。
func countPlaceholders(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+1 < len(s) && (s[i+1] == 's' || s[i+1] == 'd') {
			n++
		}
	}
	return n
}
