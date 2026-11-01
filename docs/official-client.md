# 官方客户端对照

记录网关出站请求与官方 WorkBuddy 桌面端的对齐依据、当前对齐到哪一步、以及还没对齐的地方。官方升级后照着本文的「核对方法」逐项复查。

## 依据的可靠性分级

| 级别 | 含义 | 处理方式 |
|---|---|---|
| **A 实测/证据** | 本仓库对真实上游发过请求并观察到响应；**或直接读官方安装包内的代码与配置** | 可直接依赖 |
| **B 交叉参考** | 来自另一维护分支 `yreidev/workbuddy2api-panel` 的文档，本仓库未独立验证 | 只作方向参考 |
| **C 未知** | 无依据，或本机无法验证 | 不做改动，或做开关默认关闭 |

### 本文当前的依据来源

**A 级证据（2026-10-24 解包）**：

```
WorkBuddy-win32-x64-user-5.8.3.41334564-db0e9b51.exe  （421 MB，NSIS + 内层 7z）
  下载地址  https://download.codebuddy.cn/workbuddy/saas/win32-x64-user/...
  解包方式  7z x 两次（外层 NSIS → $PLUGINSDIR/app-64.7z → 应用目录）
  只读解包，未运行安装程序
```

三处关键文件：

| 文件 | 内容 |
|---|---|
| `resources/app.asar.unpacked/cli/product.json` | 版本、`deploymentType`、`applicationName`、`commit`、`date` |
| `resources/app.asar.unpacked/cli/dist/codebuddy-lite-wb.mjs` | 内置 CLI（对话、刷新令牌、模型目录、会话 id、请求头） |
| `resources/app.asar` | 桌面端后台服务（积分、签到、上报、外观、专家列表） |

<details>
<summary>为什么不直接用 B 级文档的结论</summary>

B 级文档看起来最权威（对方声称解包过安装包），但有两个问题：

1. **无法证伪**——官方升级后，B 级结论失效时没有任何信号；
2. **实测发现它有一处不准确**：B 级说 `X-Conversation-ID` 是「带连字符的 uuidv7」，
   而包内代码是 `generateUUUID()`，版本位 `i[6]=15&i[6]|64`（0x40 = **v4**）。
   若照抄 B 级会与官方真实行为不符。

所以本轮全部结论改为从包内直接取证据。
</details>

## 一、已对齐（A 级）

### 1.1 版本号常量

| 项 | 官方真实值 | 出处 |
|---|---|---|
| 客户端版本 | **CN `5.9.3`** / **global `5.9.4`** | 两个安装包各自的 `product.json` 的 `genieVersion`（两条独立发布轨） |
| CLI 版本 | **`2.165.0`**（两包一致） | `dist/codebuddy-lite-wb.mjs` 内 `publishConfig.customPackage = {name:"@tencent-ai/codebuddy-code", version:"2.165.0"}` |
| `deploymentType` | `SaaS` | `product.json` |
| `applicationName` | CN `WorkBuddy` / global **`workbuddy-ai`** | `product.json`（注意两域不同，且 global 是连字符） |
| `productName` | CN `WorkBuddy` / global `WorkBuddy AI` | `product.json`（global 带空格） |
| `commit` | CN `d4692421…` / global `fa0a0901…` | `product.json` |

安装包（2026-11-01 实测）：

```
CN     WorkBuddy-win32-x64-user-5.9.3.42398134-d4692421.exe
global WorkBuddy-win32-x64-user-5.9.4.42593613-fa0a0901.exe
```

对应常量：`internal/upstream/headers.go` 的 `defaultClientVersion`、`defaultCliVersion`、`deploymentType`。

**升级 UA 会不会影响模型列表**：不会。三组 UA（5.5.4 / 5.8.3 / 5.8.4）拉模型目录，
模型集合完全一致（33 个，响应字节数相同 30162）。

### 1.2 官方不发的五个头

**结论**：官方客户端不发 `Origin`、`Referer`、`X-CodeBuddy-Request`、`X-Machine-ID`、`X-Session-ID`。

包内证据：

| 头 | 证据 |
|---|---|
| `X-Session-ID` | `SESSION_ID_HEADER="X-Session-ID"` 只在头枚举里出现 **2 次**（定义本身），代码从不 set |
| `X-CodeBuddy-Request` | 只出现在 CLI **自身 HTTP 服务**的 OpenAPI `securitySchemes` 里（豁免路径 `/api/v1/auth/*`、`/api/openapi.json`）——那是别人调用 CLI 时的鉴权头，不是出站头 |
| `Origin` / `Referer` | 包内两处均为「响应头枚举」与 CORS 逻辑，出站请求不设 |
| `X-Machine-ID` | 包内 **0 次** |

运行期证据（四端点 × 三组对照）：

| 端点 | 全发（旧） | 官方形状 | 仅去五头 |
|---|---|---|---|
| `chat/completions` | 200 | 200 | 200 |
| `billing/meter/get-user-resource` | 200 | 200 | 200 |
| `activity/growth/tasks` | 200 | 200 | 200 |
| `activity/growth/streak` | 200 | 200 | 200 |

**这意味着**：上游当前并不校验这五个头。去掉是为了减少指纹暴露面，不是修正在发生的故障。

### 1.3 ID 形态（三者版本位不同，不是笔误）

| 头 | 官方生成方式 | 形态 |
|---|---|---|
| `X-Conversation-ID` | `SessionImpl.id = generateUUUID()` | **UUID v4**，带连字符（36 位） |
| `X-Conversation-Request-ID` | `generateConversationRequestId()` = `uuidv7().replace(/-/g,"")` | **UUID v7**，去连字符（32 位） |
| `X-Conversation-Message-ID` | 同轮次 id | **UUID v7**，去连字符 |
| `X-Request-ID`（对话） | 同上，等于消息 id | UUID v7，去连字符 |
| `X-Request-ID`（非对话） | `injectRequestIdHeader` → `generateUUUID().replace(/-/g,"")` | **UUID v4**，去连字符 |

关键代码：

```js
// 会话 id：v4 带连字符
SessionImpl: this.id = (0,s.generateUUUID)()
generateUUUID: i[6] = 15&i[6]|64     // 0x40 = 版本 4
               ... c += "-" ...       // 带连字符

// 轮次/消息 id：v7 去连字符
generateConversationRequestId() { return (0,e5.nH)().replace(/-/g,"") }
// nH 已确认 = uuidv7
```

包内还有一条断言：

```js
"conversationRequestId must be a lowercase UUIDv7 encoded as 32 hexadecimal characters"
```

上游目前不校验（任意 32 hex 实测均 200），但派生出来的聚合 id 也强制了 v7 版本位。

### 1.4 对话专属头

| 头 | 官方取值 | 说明 |
|---|---|---|
| `X-Agent-Intent` | `craft` | `N.meta?.["codebuddy.ai/mode"] ?? "craft"` |
| `X-Agent-Type` | `main` | |
| `X-Agent-Purpose` | `conversation` | 主对话用途 |
| `X-Product` | `SaaS` | 来自 `deploymentType`，**与 `X-IDE-Name` 不同源** |
| `X-IDE-Name` / `X-IDE-Type` | `WorkBuddy` | 桌面端传给 CLI 的应用名 |
| `X-IDE-Version` | `5.8.3` | 桌面端版本 |
| `X-Private-Data` | `true` | `DisableYuanbaoChannel=true` 且未设 `enableModelOptimization` → 发 `"true"`；实测 `product.json` 的 `DisableYuanbaoChannel` 确为 `true` |

`X-Product` 曾被我误设为 `WorkBuddy`（跟着 `X-IDE-Name` 走），现已改为独立的 `SaaS`。

### 1.5 Accept-Language

官方**通用请求不发** `Accept-Language`；只有积分包查询带，且是**短值** `zh` / `en`：

```js
fetchResourcePackagesPage(e,t,n,r,i) {
  X.post(e, {...}, {headers:{"Accept-Language": n}})
}
// n = CODEBUDDY_IDE_STORAGE_LANG 以 en 开头 ? "en" : "zh"
```

对应端点：`/billing/meter/get-user-resource-paid-packages`、`-free-packages`、`-summary`。

实现：`CommonHeaders` 不再发该头；`BillingHeaders` 发短值（`internal/upstream/headers.go`）。

### 1.6 我们自己伪造的 ID（已清理）

旧实现在 25 处用字面量拼会话/请求 ID：

```
wb2api-mp-1750000000000-0     wb2api-pb-1750000000000
wb2api-chat5-...-3            wb2api-adopt-1750000000000
```

这些字符串**直接出现在请求体与请求头里**，按 ID 形状就能一眼分出不是官方客户端——
比 UA、比请求头都更直接。现已全部替换为官方形态的 uuidv7。

同类问题还有三处（成长任务事件）：

| 项 | 旧值 | 新值 |
|---|---|---|
| 定时任务名 | `"wb2api 自动化"` | `"每日资讯简报"`（官方是用户自填的普通名字） |
| 模板 id | `"1"`–`"5"` + 自拟名称 | 官方模板目录真实值：50/47/48/49/51 |
| 事件 messageId | `"msg-<后8位>"`、`"msg-pb"` | uuidv7 |

灵感案例 `pm-gtm-launch-plan` 已核实存在于官方 registry（1274 个案例之一，
标题「新产品上市 GTM 发布计划一页纸」），无需改动。

### 1.7 Web 域身份（Electron 客户端，2026-11-01 抓包实证）

`www.workbuddy.cn` 的页面行为类请求（`/v2/report` 等）由 **Electron 渲染进程**发出，
与 chat（Node CLI 子进程）是两套完全不同的身份。

**UA（三次抓包逐字一致，全池恒定）**：

```
Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko)
WorkBuddy/5.9.3 Chrome/138.0.7204.251 Electron/37.10.3 Safari/537.36
```

三个要点：

1. **是 Electron 应用**：同时带 `WorkBuddy/<ver>`、`Chrome/138`、`Electron/37`
2. **全池恒定**：官方一台机器装一份客户端、多个账号共用，不按账号换浏览器
3. **不是国产浏览器**：曾按「国内用户浏览器混杂」做过 UC/搜狗/360/QQ/微信 按 uid
   派生（commit 8d469b1），方向错误——那是真人浏览网页的分布，多账号各带不同 UA
   反而暴露代理池特征。已在 cff80c5 推翻

**浏览器提示头（九个，逐请求恒定）**：

| 头 | 值 |
|---|---|
| `sec-ch-ua` | `"Not)A;Brand";v="8", "Chromium";v="138",?0` |
| `sec-ch-ua-mobile` | `?0` |
| `sec-ch-ua-platform` | `"Windows"` |
| `sec-fetch-site` | `cross-site`（XHR）/ `none`（导航） |
| `sec-fetch-mode` | `cors`（XHR）/ `no-cors`（导航） |
| `sec-fetch-dest` | `empty` |
| `accept-language` | `zh-CN` |
| `accept-encoding` | `gzip, deflate, br, zstd` |
| `priority` | `u=1, i` |

缺这族头的 Electron 应用不存在（Chromium 内核自己会加），少了它们
「Mozilla ... Electron/37」这个 UA 反而成孤证。

实现：`internal/upstream/webua.go` 的 `webUAFor` + `injectWebClientHints`。

## 二、未对齐 / 待验证

2026-11-01 对 Windows 官方客户端 5.9.3 做了三次 pcap + SSLKEYLOGFILE 实抓解密，
下表按实测结论重写（旧表有 4 项判断已被推翻）。

| 项 | 现状 | 实测结论 |
|---|---|---|
| **Web 域 UA** | 已对齐 | 官方是 **Electron 应用**：`Mozilla/5.0 (...) WorkBuddy/<ver> Chrome/138.0.7204.251 Electron/37.10.3 Safari/537.36`，**全池恒定、不按账号变**。见 §1.7 |
| **Web 域浏览器提示头** | 已对齐 | `sec-ch-ua` / `sec-ch-ua-mobile` / `sec-ch-ua-platform` / `sec-fetch-*` / `accept-language` / `accept-encoding` / `priority` 九头，见 §1.7 |
| **ALPN** | 与官方一致 | 官方**三种形态混用**（三次抓包：47/28/23 条不发、37/27/20 条 http/1.1、6/1/1 条 h2,http/1.1）。我们不发 ALPN，属其中一种，**不是差异** |
| **TLS 指纹** | Go 标准库 | 官方是 Node/Electron 握手。要对齐需引入 uTLS，风险高、收益不确定 |
| **HTTP/2** | 有意禁用 | chat 走内置 **Node CLI 子进程**，它本身也不发 ALPN → 实际就是 HTTP/1.1。禁 h2 与此一致 |
| **`X-Department-Info`** | 不发 | 只有企业号才有该值；池里没有带部门的企业号，无法验证 |
| **cookie** | 不发 | 官方 web 请求带 `session=...`（超长串）并回 `set-cookie`。复刻需完整会话管理，收益不确定，不做 |
| **chat 头族** | 推断对齐 | **三次抓包均无法观测**：chat 由 Node 子进程发出，它不读 `SSLKEYLOGFILE`、也覆盖 `NODE_OPTIONS`（实测 `nodekeys.log` 从未生成），且不发 ALPN → 该流量无法旁路解密。现依据取自安装包 `buildConversationHeaders` 全文 |

### 2.1 chat 流量为什么抓不到（重要）

三次抓包的对照统计完全一致：

```
ALPN 状态    流数      解密成功
有 ALPN      43–44     4–6
无 ALPN      47–37     0        ← 一条都解不开
```

原因链：

1. WorkBuddy 是 Electron 应用，但 **chat 请求由内置 Node.js CLI 子进程发出**
2. `SSLKEYLOGFILE` 只对 Chromium 网络栈生效，**Node 不自动写**
3. `NODE_OPTIONS=--tls-keylog=...` 被客户端覆盖（本机实测该参数本身可用，但子进程没继承）
4. Node 握手**不发 ALPN** → tshark 无法判定协议，即使有密钥也解不出

**结论**：chat 头族无法通过旁路抓包验证。要继续验证只能上 mitmproxy（需装 CA，
且可能撞证书固定），性价比低——字段清单已从安装包完整提取并逐项对齐。

### 2.2 为什么不再追求「头名大小写」

实测官方线上字节是 `X-Conversation-ID` / `X-IDE-Name` / `X-B3-SpanId`（末段全大写），
而 Go 的 `Header.Set` 会规范化成 `X-Conversation-Id` / `X-Ide-Name` / `X-B3-Spanid`。

曾尝试用 `setExact`（直接写 map）绕过，**已回退**，两个硬理由：

1. 破坏 `Header.Get` 语义（3 个测试当场失败）
2. 与 `Set` 混用会产生**重复头**（同一个 `X-Request-ID` 发两个不同值）

且 HTTP 头名按 RFC 7230 大小写不敏感，服务端必归一化——**不构成有效指纹**。

## 三、官方升级后怎么核对

1. 从官网下载新安装包（国内版 Windows：`download.codebuddy.cn/workbuddy/saas/win32-x64-user/...exe`）。
2. 解包（只读，不运行）：
   ```bash
   apt-get install -y p7zip-full
   7z x -y -oext1 WorkBuddy-win32-x64-user-X.Y.Z.exe
   7z x -y -oext2 'ext1/$PLUGINSDIR/app-64.7z'
   # 关键文件在 ext2/resources/app.asar.unpacked/cli/
   ```
**两条发布轨要分别取包**（版本号不同步）：

```
CN     https://download.codebuddy.cn/workbuddy/saas/win32-x64-user/...
global https://download.codebuddy.ai/workbuddy/saas/win32-x64-user/...
```

3. 读 `product.json`：`genieVersion`、`deploymentType`、`applicationName`、`productName`、`commit`、`date`。
4. 在 `dist/codebuddy-lite-wb.mjs` 里搜 `"@tencent-ai/codebuddy-code","version":"` 取 CLI 版本。
5. 在 `app.asar` 里搜 `Accept-Language`、`fetchResourcePackagesPage` 核对语言头。
6. 改常量后跑 `go test ./internal/upstream/`，并按下面方式抓一次真实出站复核。

### 3.1 用抓包复核 Web 域身份（可行，已验证）

Wireshark + `SSLKEYLOGFILE` 可以完整解出 Electron 的 web 域请求。**顺序很关键**：

```powershell
mkdir C:\wb
[Environment]::SetEnvironmentVariable("SSLKEYLOGFILE", "C:\wb\sslkeys.log", "User")
# 彻底退出 WorkBuddy（含托盘图标与 node 子进程），然后：
#   1) 先开 Wireshark 开始捕获
#   2) 再启动 WorkBuddy        ← 顺序反了会导致 keylog 与 pcap 对不上
#   3) 等界面加载完，操作几个页面
#   4) 停止捕获，连同 sslkeys.log 一起保存
```

判定 `tshark` 能否解密：

```bash
# ClientHello random 与 keylog 的交集必须 > 0，否则两份文件不是同一时段
comm -12 <(tshark -r cap.pcapng -Y 'tls.handshake.type==1' \
             -T fields -e tls.handshake.random | tr 'A-F' 'a-f' | sort -u) \
         <(awk '{print tolower($2)}' sslkeys.log | sort -u) | wc -l
```

解出请求与头：

```bash
tshark -r cap.pcapng -o tls.keylog_file:sslkeys.log \
  -Y "http2.headers.method" -T fields \
  -e http2.headers.method -e http2.headers.path -e http2.headers.authority
```

**两个实测坑**：

- 抓包前 WorkBuddy 已建立的持久连接，其 ClientHello 不在窗口内 → 密钥对不上
- 同一域名下会有大量**不解密**的流，那是 chat 用的 Node 子进程（见 §2.1），不是失败

### 3.2 chat 头族无法抓包验证（已知限制）

见 §2.1：Node 子进程不写 keylog、覆盖 `NODE_OPTIONS`、不发 ALPN，三位一体导致
该流量无法旁路解密。验证 chat 只能读安装包（`buildConversationHeaders`）。

### 抓一次真实出站复核

读代码只能确认「我写了什么」，抓包才能确认「实际发出去什么」：

```go
c := upstream.New()
req, _ := http.NewRequest("POST", "https://copilot.tencent.com/v2/chat/completions", nil)
c.ChatHeaders(req, a, "", upstream.ChatMeta{})
// 逐条打印 req.Header
```

判据分两组：**必须存在**（`X-Request-ID`、`X-User-Id`、`X-Domain`、会话三元组、
`X-Agent-*`、`X-IDE-*`、`X-Product`、`X-Private-Data`）与**必须消失**
（`Origin`、`Referer`、`X-CodeBuddy-Request`、`X-Machine-ID`、`X-Session-ID`）。

2026-10-24 实测 `official` 形态为 24 个头：

```
Accept, Authorization, Content-Type, User-Agent,
X-Agent-Intent, X-Agent-Purpose, X-Agent-Type,
X-B3-Sampled, X-B3-Spanid, X-B3-Traceid,
X-Conversation-Id, X-Conversation-Message-Id, X-Conversation-Request-Id,
X-Domain, X-Ide-Name, X-Ide-Type, X-Ide-Version, X-No-Enterprise-Id,
X-Private-Data, X-Product, X-Request-Id, X-Requested-With,
X-Root-Request-Id, X-Trace-Id, X-User-Id
```

## 四、明确不做的事

| 项 | 理由 |
|---|---|
| 照搬参考仓库代码 | 该仓库自 v1.4.0 起是 **AGPL-3.0-only** 并附 Section 7 附加条款（保留声明、README 署名、界面署名、不得冒充原版）。本仓库是 MIT，直接复制会造成许可冲突。只借鉴方向，实现全部重写 |
| TLS 指纹对齐（uTLS） | 换传输库风险高；需先在真机抓包才能确认目标值 |
| 伪造不存在的版本号 | 始终报真实版本（当前 CN 5.9.3 / global 5.9.4，CLI 2.165.0），有包内证据 |

## 五、变更记录

| 日期 | 变更 | 依据 |
|---|---|---|
| 2026-10-24 | 去掉五个官方不发的头（`HeaderProfile=official` 默认） | 包内证据 + 四端点三组对照 |
| 2026-10-24 | `X-Conversation-ID` → UUID **v4** 带连字符 | `SessionImpl.id = generateUUUID()` |
| 2026-10-24 | 轮次/消息 ID → UUID **v7** 去连字符，派生值强制 v7 版本位 | `generateConversationRequestId` |
| 2026-10-24 | 25 处伪造 ID（`wb2api-*`）替换 | — |
| 2026-10-24 | `X-Product` → `SaaS`（与 `X-IDE-*` 解耦） | `deploymentType` |
| 2026-10-24 | UA `5.5.4` → `5.8.3`，CLI `2.137.1` → `2.161.0` | `product.json` + bundle `publishConfig` |
| 2026-10-27 | UA `5.8.3` → `5.9.2`，CLI `2.161.0` → `2.165.0`；`desktop.go` 桌面指纹版本号从硬编码 5.5.6 改为复用 `defaultClientVersion`/`defaultCliVersion` 常量（此前 chat 出站已升 5.8.3，桌面行为上报仍停 5.5.6，不一致） | `product.json`（`5.9.2.42136757-0b5051be.exe` 解包）+ bundle `publishConfig` |
| 2026-10-24 | `Accept-Language` 仅积分包查询发短值 `zh`/`en` | `fetchResourcePackagesPage` |
| 2026-10-24 | 成长任务事件三处自造取值（任务名/模板 id/messageId）替换 | 官方 registry + 模板目录 |
| 2026-11-01 | 版本号双轨：CN `5.9.3` / global `5.9.4`，CLI 两域均 `2.165.0` | 两包 `product.json` + bundle `publishConfig` |
| 2026-11-01 | 国际版 UA 平台段定为 **`WorkBuddy AI`（空格）**：包内 `buildUserAgent` 取 `applicationName ?? productName`，而运行时未注入 `CLIENT_INFO_APPLICATION_NAME`，故落到 `productName`。反驳了外部 `workbuddy-ai`（连字符）的说法 | `app.asar` 的 `UserAgentHttpInterceptor` |
| 2026-11-01 | **移除 `x-stainless-*` 七头**：官方包内唯一 stainless 出现在删除逻辑（`i=["x-stainless-"]; delete s[n]`），发它反而呈 SDK 直连形态 | `app.asar.unpacked/cli/dist/codebuddy-lite-wb.mjs` |
| 2026-11-01 | Web 域 UA 定为 **Electron 固定形态**（全池恒定），推翻按 uid 派生国产浏览器的做法 | 三次 pcap + SSLKEYLOGFILE 解密实测 |
| 2026-11-01 | Web 域补九个浏览器提示头（`sec-ch-ua*` / `sec-fetch-*` / `priority` 等） | 同上 |
| 2026-11-01 | 桌面指纹硬件档案按 uid 派生（12/14/16 核 + 16/32G + 两个 Win11 build） | 原为全池恒定 20 核/24G，同配置多机属矛盾特征 |
