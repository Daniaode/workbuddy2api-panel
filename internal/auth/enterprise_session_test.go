package auth

import (
	"encoding/base64"
	"testing"
)

// mustJWT 用给定 payload 拼一个最小 JWT（header.payload.signature）。
// IsEnterpriseSession 不校验签名，只解 payload，故签名段可为任意占位。
func mustJWT(t *testing.T, payload string) string {
	t.Helper()
	return "hdr." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".sig"
}

// TestIsEnterpriseSession 企业上下文判定：JWT enterprise_id 非空视为企业上下文；
// 无该声明 / 空串 / 非 JWT / 空 token 一律否（老凭证零回归）；
// auth 文件 enterpriseId 非空短路为真（与 IsEnterprise 同口径）。
func TestIsEnterpriseSession(t *testing.T) {
	cases := []struct {
		name  string
		token string
		want  bool
	}{
		{"enterprise_id 非空", mustJWT(t, `{"sub":"u1","enterprise_id":"ent-123"}`), true},
		{"enterprise_id 空串", mustJWT(t, `{"sub":"u1","enterprise_id":""}`), false},
		{"无 enterprise_id 声明", mustJWT(t, `{"sub":"u1","scope":"openid profile"}`), false},
		{"token_source=enterprise_switch 但 enterprise_id 空", mustJWT(t, `{"sub":"u1","token_source":"enterprise_switch","enterprise_id":""}`), false},
		{"带填充 base64url", "hdr." + base64.URLEncoding.EncodeToString([]byte(`{"enterprise_id":"ent-1"}`)) + ".sig", true},
		{"payload 非 JSON", "hdr." + base64.RawURLEncoding.EncodeToString([]byte("not-json")) + ".sig", false},
		{"非 JWT 形态", "opaque-token", false},
		{"空 token", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &Auth{UID: "u1", AccessToken: c.token}
			if got := a.IsEnterpriseSession(); got != c.want {
				t.Errorf("IsEnterpriseSession()=%v want %v", got, c.want)
			}
		})
	}

	t.Run("enterpriseId 非空短路为真", func(t *testing.T) {
		a := &Auth{UID: "u1", EnterpriseID: "e1", AccessToken: mustJWT(t, `{"sub":"u1"}`)}
		if !a.IsEnterpriseSession() {
			t.Error("IsEnterprise()=true 时 IsEnterpriseSession() 应恒为 true")
		}
	})

	t.Run("企业切换会话不影响 IsEnterprise（计费口径隔离）", func(t *testing.T) {
		a := &Auth{UID: "u1", AccessToken: mustJWT(t, `{"enterprise_id":"ent-1"}`)}
		if a.IsEnterprise() {
			t.Error("仅 JWT enterprise_id 非空不应翻转 IsEnterprise（额度查询仍走个人口径）")
		}
	})
}

// TestJwtEnterpriseID 回归：手机号注册的个人账号 token 带 token_source=enterprise_switch
// 但 enterprise_id 为空，必须判为「非企业上下文」（否则一键任务被错误门控）。
// 这是 2026-10-27 用户反馈的 s 账号误判场景的直接复现。
func TestJwtEnterpriseIDEmptyStringNotEnterprise(t *testing.T) {
	// 完整复刻 s 账号 token payload 的关键字段：enterprise_id 空 + token_source=enterprise_switch。
	payload := `{"sub":"ddd41515-7b8d-40d8-a00a-4fb4b56fcb0d","token_source":"enterprise_switch","enterprise_id":"","oneid_account_id":"","oneid_union_id":"","preferred_username":"13862170370"}`
	a := &Auth{UID: "ddd41515", AccessToken: mustJWT(t, payload)}
	if a.IsEnterpriseSession() {
		t.Fatal("enterprise_id 为空 + token_source=enterprise_switch 应判为非企业（手机号注册个人账号），实际判为 true")
	}
}
