package session

import (
	"regexp"
	"testing"
)

func TestNewUUIDv7Shape(t *testing.T) {
	hexRe := regexp.MustCompile(`^[0-9a-f]{32}$`)
	dashRe := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		h := NewUUIDv7Hex()
		if !hexRe.MatchString(h) {
			t.Fatalf("hex 形态不符: %q", h)
		}
		if h[12] != '7' {
			t.Fatalf("版本位不是 7: %q", h)
		}
		if v := h[16]; v != '8' && v != '9' && v != 'a' && v != 'b' {
			t.Fatalf("变体位不合法: %q", h)
		}
		if seen[h] {
			t.Fatalf("重复 ID: %q", h)
		}
		seen[h] = true

		d := NewUUIDv7()
		if !dashRe.MatchString(d) {
			t.Fatalf("带连字符形态不符: %q", d)
		}
	}
}
