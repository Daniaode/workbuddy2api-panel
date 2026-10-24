# 官方客户端对照

记录网关出站请求与官方 WorkBuddy 桌面端的对齐依据、当前对齐到哪一步、以及还没对齐的地方。官方升级后照着本文的「核对方法」逐项复查。

## 依据的可靠性分级

本文每条结论都标了来源等级。**只有 A 级可以当作事实使用**：

| 级别 | 含义 | 处理方式 |
|---|---|---|
| **A 实测** | 本仓库在本机对真实上游发请求，观察到响应 | 可直接依赖；有测试或日志佐证 |
| **B 交叉参考** | 来自另一维护分支 `yreidev/workbuddy2api-panel` 的 `docs/official-client.md`（其依据是解包官方安装包） | **未经本仓库独立验证**，只作方向参考 |
| **C 未知** | 无任何依据，或本机无法验证 | 不做改动，或做开关默认关闭 |

**为什么必须这样分级**：B 级内容看起来最权威（对方解包过安装包），但我无法在本机复现——官网下载入口在我这里不可达（`codebuddy.cn/download`、`workbuddy.cn/download` 均 404）。把 B 当 A 用，等于把别人的结论当自己的证据，官方一升级就会集体失效且无从发现。

## 一、已对齐（A 级实测）

### 1.1 官方不发的五个头

**结论**：官方客户端不发 `Origin`、`Referer`、`X-CodeBuddy-Request`、`X-Machine-ID`、`X-Session-ID`。本仓库原先五个全发，现已默认不发。

**来源**：B 级提法 + **A 级实测验证**（下面四组）。

验证方法：同一账号、同一模型、只改请求头，四组对照。

| 端点 | A 全发（旧） | B 官方形状 | C 仅去五头 | 结论 |
|---|---|---|---|---|
| `chat/completions` | 200 | 200 | 200 | 全部通过 |
| `billing/meter/get-user-resource` | 200 | 200 | 200 | 全部通过 |
| `activity/growth/tasks` | 200 | 200 | 200 | 全部通过 |
| `activity/growth/streak` | 200 | 200 | 200 | 全部通过 |

**这意味着什么**：上游当前**并不校验**这五个头。去掉它们不是为了修一个正在发生的故障，而是减少指纹暴露面——在「上游开始限制反代」的背景下，少一个可被识别的特征就少一分风险。

**实现**：`internal/upstream/headers.go` 的 `CommonHeaders`，由 `Client.HeaderProfile` 控制。

- `official`（默认）：不发那五个头，`Accept: application/json, text/plain, */*`；
- `legacy`：完整保留旧行为，一键还原，不必改代码重新构建。

配置项：`upstream.header_profile`。

### 1.2 会话 ID 的形态

**结论**：`X-Conversation-ID` 应是**带连字符**的 36 位 uuidv7；`X-Conversation-Request-ID` 与 `X-Conversation-Message-ID` 是**去掉连字符**的 32 位。

**来源**：B 级提法 + **A 级实测**。

验证：两种形态各发一次真实对话，都返回 200 且收到完整 `[DONE]`：

```
32 hex（旧）         conv=5e30456d94aefcba806b4f1f7513376c   HTTP=200 done=true
36 带连字符（官方）   conv=01a16da7-69ee-7636-8715-ae7f43d3aa HTTP=200 done=true
```

所以切换形态不会触发 11-128。**这也说明上游此前并不校验这个形态**——我们一直发 32 位也没出问题。改它是为了「看起来像官方」，不是修故障。

**实现**：`internal/session/ids.go` 新增 `NewUUIDv7()` / `NewUUIDv7Hex()`，后者保证版本位 7 与 RFC 9562 变体位（有单测 `uuid_shape_test.go` 覆盖形态与唯一性）。

### 1.3 我们自己伪造的 ID（已清理）

**这是本文最重要的一条，也是此前最明显的指纹。**

旧实现在 25 处用字面量拼会话/请求 ID：

```
wb2api-mp-1750000000000-0
wb2api-pb-1750000000000
wb2api-chat5-1750000000000-3
wb2api-adopt-1750000000000
```

这些字符串**直接出现在请求体与请求头里**，任何按 ID 形状做判断的实现都能一眼分出「不是官方客户端」。比 UA、比请求头都更直接。

**现已全部替换为官方形态的 uuidv7**（`session.NewUUIDv7Hex()`），涉及 `scheduler`、`panel/autotask`、`upstream/{blackcat,desktop,school}`。

### 1.4 UA 版本不影响模型列表

**结论**：把 UA 从 `WorkBuddy/5.5.4 ... CLI/2.137.1` 换成 `5.8.3 / 2.161.0`，模型目录下发集合**完全一致**（33 个模型，字节数都相同）。

**为什么必须先测这个**：仓库注释写过「该端点对不同 UA 下发的模型集合不同」。若换 UA 会改变模型列表，那就是功能变更，不是「对齐」。

```
5.5.4 / CLI 2.137.1    模型数=33  (bytes=30162)
5.8.3 / CLI 2.161.0    模型数=33  (bytes=30162)
5.8.4 / CLI 2.161.0    模型数=33  (bytes=30162)
```

所以升版本号是安全的。**但当前仍未升**——见第二节。

## 二、未对齐 / 待验证

| 项 | 现状 | 为什么没做 |
|---|---|---|
| **UA 版本号** | 仍是 `5.5.4` / CLI `2.137.1` | 目标值 `5.8.3` / `2.161.0` 只有 B 级来源。虽已验证换版本不影响模型列表，但**报一个自己没确认存在的版本号**本身就是不实陈述。等你提供安装包（或确认接受 B 级依据）后再改 |
| **`X-Product` 取值** | 已改为 `SaaS`（官方 `deploymentType`） | 改动基于 B 级依据；与 `X-IDE-Name/Type="WorkBuddy"` 是两个不同来源，不能合并（见下） |
| **`Accept-Language`** | 发 `zh-CN` / `en-US` | B 级说官方只在积分包查询时发 `zh`/`en`，且是短值。未实测 |
| **`Accept-Encoding`** | Go 自动加 `gzip` | B 级说官方是 `gzip, compress, deflate, br`（axios 行为）。要精确控制需关闭 Go 的自动压缩并自己解压，改动面偏大，收益不明 |
| **`X-Department-Info`** | 不发 | 只有企业号才有该值，池里没有带部门的企业号，无法验证 |
| **`X-Private-Data`** | 已发 `true` | 取值依据是 B 级；我们确实不做模型优化，语义相符。无企业号/无对照组，未能验证是否必需 |
| **TLS 指纹** | Go 标准库 | B 级明确说桌面端是 Node/Electron 的 TLS 握手，要对齐需引入 uTLS。改动风险高、收益不确定，不做 |
| **HTTP/2** | 有意禁用（见 `transport.go`） | 同上，且禁 h2 是历史故障（半死连接）的修复，回退会引入老问题 |

### 2.1 `X-Product` 与 `X-IDE-*` 不是同一个值

这条容易搞错，单独说明：

| 头 | 取值 | 来源 |
|---|---|---|
| `X-Product` | `SaaS` | `product.json` 的 `deploymentType`，由 `ProductEndpointHttpInterceptor` 加到每个请求 |
| `X-IDE-Name` / `X-IDE-Type` | `WorkBuddy` | 桌面端传给内置 CLI 的应用名（`IDE_TYPE`） |
| `X-IDE-Version` | 桌面端版本号 | 同上 |

旧实现把 `X-Product` 也设成 `WorkBuddy`，与官方取值不同。现已分开：`X-Product` 固定 `SaaS`，`X-IDE-*` 仍跟随 `client_name`。

## 三、官方升级后怎么核对

前提：拿到新版安装包。本机目前下载不到，需要人工从官网取。

1. 解包（只读，不运行）。看三处：
   - `app.asar.unpacked/cli/product.json`：版本、`applicationName`、`deploymentType`、`productFeatures`；
   - `app.asar.unpacked/cli/dist/codebuddy-lite-wb.mjs`：内置 CLI，对话/刷新令牌/模型目录由它发；
   - `app.asar` 里的 `main/`：桌面端后台服务，积分/签到/上报由它发。
2. 对照本文第一节，逐项确认是否变化。
3. 若有变化，改这几处常量：
   - `internal/upstream/headers.go`：`defaultClientVersion`、`defaultCliVersion`、`deploymentType`；
   - 改完跑 `go test ./internal/upstream/`，并抓一次真实出站复核（见下）。
4. 更新本文的来源等级：把新验证的项从 B 升到 A。

### 抓一次真实出站复核

读代码只能确认「我写了什么」，抓包才能确认「实际发出去什么」。本仓库的做法：

```go
c := upstream.New()
req, _ := http.NewRequest("POST", "https://copilot.tencent.com/v2/chat/completions", nil)
c.ChatHeaders(req, a, "", upstream.ChatMeta{})
// 逐条打印 req.Header
```

判据分两组：**必须存在**（`X-Request-ID`、`X-User-Id`、`X-Domain`、会话三元组、`X-Agent-Purpose`、`X-IDE-*`、`X-Product`）与**必须消失**（`Origin`、`Referer`、`X-CodeBuddy-Request`、`X-Machine-ID`、`X-Session-ID`）。

当前 `official` 形态实测为 26 个头：

```
Accept, Accept-Language, Authorization, Content-Type, User-Agent,
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
| 照搬参考仓库的代码 | 该仓库自 v1.4.0 起是 **AGPL-3.0-only**，并附 Section 7 附加条款（保留声明、README 署名、界面署名、不得冒充原版）。本仓库是 MIT，直接复制会造成许可冲突。因此只借鉴**结论**，实现全部重写 |
| TLS 指纹对齐（uTLS） | 风险高（换传输库），收益不确定；且需要先在真机抓包才能确认目标值 |
| 伪造并不存在的版本号 | 报一个自己没验证过的版本，比报旧版本更糟——它会让「对齐」这件事失去可信度 |

## 五、变更记录

| 日期 | 变更 | 验证 |
|---|---|---|
| 2026-10-24 | 去掉五个官方不发的头（`HeaderProfile=official` 默认） | 四端点 × 三组对照全部 200 |
| 2026-10-24 | `X-Conversation-ID` 改为 36 位带连字符 uuidv7 | 两种形态均 200 |
| 2026-10-24 | 25 处伪造 ID（`wb2api-*`）替换为 uuidv7 | 全仓库测试通过 |
| 2026-10-24 | `X-Product` 改为 `SaaS`（与 `X-IDE-*` 解耦） | 单测 + 抓包复核 |
| 2026-10-24 | 确认 UA 版本不影响模型列表 | 三组 UA 模型集合完全一致 |
