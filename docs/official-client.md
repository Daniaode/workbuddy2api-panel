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
| 客户端版本 | **`5.8.3`** | `product.json` 的 `genieVersion`；安装包文件名 `...-5.8.3.41334564-...` |
| CLI 版本 | **`2.161.0`** | `dist/codebuddy-lite-wb.mjs` 内 `publishConfig.customPackage = {name:"@tencent-ai/codebuddy-code", version:"2.161.0"}` |
| `deploymentType` | `SaaS` | `product.json` |
| `applicationName` / `productName` | `WorkBuddy` | `product.json` |
| `commit` / `date` | `db0e9b51…` / `2026-10-15T01:58:12.880Z` | `product.json` |

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

## 二、未对齐 / 待验证

| 项 | 现状 | 原因 |
|---|---|---|
| **TLS 指纹** | Go 标准库 | 官方是 Node/Electron 握手。要对齐需引入 uTLS，风险高、收益不确定 |
| **HTTP/2** | 有意禁用 | 禁 h2 是历史故障（半死连接）的修复，回退会引入老问题 |
| **`X-Department-Info`** | 不发 | 只有企业号才有该值；池里没有带部门的企业号，无法验证 |
| **`Accept-Encoding`** | Go 自动加 `gzip` | 官方是 axios 的 `gzip, compress, deflate, br`。精确控制需关闭 Go 自动压缩并自行解压，改动面偏大 |
| **`traceparent`** | 不发 | 官方用 OTel 注入；我们没有 OTel，且格式影响面未评估 |
| **国际版 UA 平台段** | `WorkBuddy AI` | 本轮解的是 Windows 国内版包，国际版包未取到 |

## 三、官方升级后怎么核对

1. 从官网下载新安装包（国内版 Windows：`download.codebuddy.cn/workbuddy/saas/win32-x64-user/...exe`）。
2. 解包（只读，不运行）：
   ```bash
   apt-get install -y p7zip-full
   7z x -y -oext1 WorkBuddy-win32-x64-user-X.Y.Z.exe
   7z x -y -oext2 'ext1/$PLUGINSDIR/app-64.7z'
   # 关键文件在 ext2/resources/app.asar.unpacked/cli/
   ```
3. 读 `product.json`：`genieVersion`、`deploymentType`、`applicationName`、`commit`、`date`。
4. 在 `dist/codebuddy-lite-wb.mjs` 里搜 `"@tencent-ai/codebuddy-code","version":"` 取 CLI 版本。
5. 在 `app.asar` 里搜 `Accept-Language`、`fetchResourcePackagesPage` 核对语言头。
6. 改常量后跑 `go test ./internal/upstream/`，并按下面方式抓一次真实出站复核。

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
| 伪造不存在的版本号 | 已改为报真实版本（5.8.3 / 2.161.0），有包内证据 |

## 五、变更记录

| 日期 | 变更 | 依据 |
|---|---|---|
| 2026-10-24 | 去掉五个官方不发的头（`HeaderProfile=official` 默认） | 包内证据 + 四端点三组对照 |
| 2026-10-24 | `X-Conversation-ID` → UUID **v4** 带连字符 | `SessionImpl.id = generateUUUID()` |
| 2026-10-24 | 轮次/消息 ID → UUID **v7** 去连字符，派生值强制 v7 版本位 | `generateConversationRequestId` |
| 2026-10-24 | 25 处伪造 ID（`wb2api-*`）替换 | — |
| 2026-10-24 | `X-Product` → `SaaS`（与 `X-IDE-*` 解耦） | `deploymentType` |
| 2026-10-24 | UA `5.5.4` → `5.8.3`，CLI `2.137.1` → `2.161.0` | `product.json` + bundle `publishConfig` |
| 2026-10-24 | `Accept-Language` 仅积分包查询发短值 `zh`/`en` | `fetchResourcePackagesPage` |
| 2026-10-24 | 成长任务事件三处自造取值（任务名/模板 id/messageId）替换 | 官方 registry + 模板目录 |
