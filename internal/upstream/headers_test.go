package upstream

import (
	"net/http"
	"regexp"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestChatHeadersAlwaysSendConversationTrio 回归 issue #190：国际端点收紧通道
// 校验后，缺 X-Conversation-ID 的聊天请求会 400 / 11-128（conversation id
// missing）。即使调用方没给会话 ID（零值 ChatMeta），三元组也必须齐全。
//
// 形态按官方逐一区分（依据：官方 5.8.3 Windows 安装包，见 docs/official-client.md）：
//   - X-Conversation-ID：带连字符的 UUID **v4**（36 位）。桌面端 SessionImpl 构造
//     函数里 `this.id = generateUUUID()`，其版本位是 `i[6]=15&i[6]|64`（0x40 = v4）；
//   - X-Conversation-Request-ID / X-Conversation-Message-ID：UUID **v7** 去掉
//     连字符（32 位）。`generateConversationRequestId()` = `uuidv7().replace(/-/g,"")`。
//
// 三者版本位不同是有意为之，不是笔误：会话是 v4，轮次与消息是 v7。
// 此前这里统一断言 32 hex（更早）与统一 v7（更晚）都不对，已按包内证据修正。
func TestChatHeadersAlwaysSendConversationTrio(t *testing.T) {
	c := &Client{}
	req, err := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.ChatHeaders(req, testAuth(), "", ChatMeta{})

	// 36 位带连字符的 UUID v4（版本位 4、RFC 9562 变体位）。
	uuid4Dashed := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	// 32 位十六进制，且版本位为 7（uuidv7 去连字符）。
	uuid7Hex := regexp.MustCompile(`^[0-9a-f]{12}7[0-9a-f]{3}[89ab][0-9a-f]{15}$`)

	conv := req.Header.Get("X-Conversation-ID")
	if conv == "" {
		t.Fatal("X-Conversation-ID missing (通道校验会拒绝缺少会话三元组的请求)")
	}
	if !uuid4Dashed.MatchString(conv) {
		t.Errorf("X-Conversation-ID = %q, want 36 位带连字符 UUID v4（官方 SessionImpl 形态）", conv)
	}
	for _, h := range []string{
		"X-Conversation-Request-ID",
		"X-Conversation-Message-ID",
	} {
		v := req.Header.Get(h)
		if v == "" {
			t.Fatalf("%s missing (通道校验会拒绝缺少会话三元组的请求)", h)
		}
		if !uuid7Hex.MatchString(v) {
			t.Errorf("%s = %q, want 32 hex 且版本位为 7（官方 uuidv7 去连字符）", h, v)
		}
	}
}

// TestChatHeadersPreservesConversationID 入站带来的会话 ID 优先透传，不被兜底覆盖。
func TestChatHeadersPreservesConversationID(t *testing.T) {
	c := &Client{}
	req, _ := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
	const conv = "0123456789abcdef0123456789abcdef"
	c.ChatHeaders(req, testAuth(), "", ChatMeta{ConversationID: conv})
	if got := req.Header.Get("X-Conversation-ID"); got != conv {
		t.Errorf("X-Conversation-ID = %q, want supplied %q", got, conv)
	}
}

// testAuth 返回最小的 CN 账号（ChatHeaders 会读 UID/凭证，nil 会解引用）。
func testAuth() *auth.Auth {
	return &auth.Auth{UID: "u1", AccessToken: "at"}
}
