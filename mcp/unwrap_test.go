package mcp

import "testing"

func TestUnwrapMCPText(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"单条", `{"content":[{"type":"text","text":"hello"}]}`, "hello"},
		{"多条取首个非空", `{"content":[{"type":"text","text":""},{"type":"text","text":"second"}]}`, "second"},
		{"非信封", `plain text`, `plain text`},
		{"纯文本 JSON 字符串", `"just string"`, `"just string"`},
		{"全空回原样", `{"content":[]}`, `{"content":[]}`},
	}
	for _, c := range cases {
		if got := UnwrapMCPText(c.in); got != c.want {
			t.Errorf("%s: UnwrapMCPText(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}
