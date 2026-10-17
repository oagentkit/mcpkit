package mcp

import (
	"context"
	"testing"
)

func TestExpandEnvRef(t *testing.T) {
	t.Setenv("MCP_TEST_TOK", "tk-1")
	if got := ExpandEnvRef("Bearer ${MCP_TEST_TOK}", nil); got != "Bearer tk-1" {
		t.Fatalf("got %q", got)
	}
	// 未定义保留原样 + 告警回调
	var missing string
	got := ExpandEnvRef("x ${MCP_TEST_UNDEF} y", func(name string) { missing = name })
	if got != "x ${MCP_TEST_UNDEF} y" || missing != "MCP_TEST_UNDEF" {
		t.Fatalf("got %q missing=%q", got, missing)
	}
	// 无闭合标记原样返回
	if got := ExpandEnvRef("a ${b c", nil); got != "a ${b c" {
		t.Fatalf("got %q", got)
	}
}

func TestExpandEnvRefs(t *testing.T) {
	t.Setenv("MCP_TEST_TOK", "tk-1")
	out := ExpandEnvRefs(map[string]string{"Authorization": "Bearer ${MCP_TEST_TOK}"}, nil)
	if out["Authorization"] != "Bearer tk-1" {
		t.Fatalf("got %v", out)
	}
	if ExpandEnvRefs(nil, nil) != nil {
		t.Fatal("空表应返回 nil")
	}
}

func TestCallMeta(t *testing.T) {
	ctx := WithCallMeta(context.Background(), map[string]any{"task": "t1"})
	if m := CallMeta(ctx); m["task"] != "t1" {
		t.Fatalf("got %v", m)
	}
	if m := CallMeta(context.Background()); m != nil {
		t.Fatalf("无注入应返回 nil, got %v", m)
	}
	// 直接作 RequestMeta 使用（赋给 Pool.RequestMeta 字段即签名形态校验）
	var p Pool
	p.RequestMeta = CallMeta
	_ = p.RequestMeta
}
