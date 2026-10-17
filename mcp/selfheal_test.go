package mcp

// 调用期自愈单测（批次五十）：分类器/透传/自愈重试/自愈失败上抛。

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// scriptedTool 脚本化工具（可编程返回序列）。
type scriptedTool struct {
	name  string
	calls []error
}

func (t *scriptedTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: t.name}, nil
}

func (t *scriptedTool) InvokableRun(_ context.Context, _ string, _ ...tool.Option) (string, error) {
	if len(t.calls) == 0 {
		return "exhausted", nil
	}
	err := t.calls[0]
	t.calls = t.calls[1:]
	if err != nil {
		return "", err
	}
	return "ok", nil
}

func TestIsTransportDead(t *testing.T) {
	cases := map[string]bool{
		"not connected":                    true,
		"client is not initialized":        true,
		"connection closed":                true,
		"write |1: broken pipe":            true,
		"use of closed network connection": true,
		"mcp server return error":          false, // 业务失败不可自愈
		"invalid arguments":                false,
		"":                                 false,
	}
	for msg, want := range cases {
		if got := isTransportDead(errors.New(msg)); got != want {
			t.Fatalf("isTransportDead(%q) = %v, want %v", msg, got, want)
		}
	}
	if !isTransportDead(io.EOF) {
		t.Fatal("io.EOF 应判传输死亡")
	}
}

func TestSelfHealPassesThroughBusinessError(t *testing.T) {
	inner := &scriptedTool{name: "calc", calls: []error{errors.New("mcp server return error: boom")}}
	redialed := false
	sut := &selfHealTool{name: "calc", inner: inner, redial: func(context.Context) (tool.InvokableTool, error) {
		redialed = true
		return nil, errors.New("should not redial")
	}}
	_, err := sut.InvokableRun(context.Background(), "{}")
	if err == nil || redialed {
		t.Fatalf("业务错误应原样上抛且不自愈: err=%v redialed=%v", err, redialed)
	}
}

func TestSelfHealRetryOnTransportDead(t *testing.T) {
	inner := &scriptedTool{name: "calc", calls: []error{errors.New("not connected")}}
	fresh := &scriptedTool{name: "calc", calls: []error{nil}}
	evicted := false
	sut := &selfHealTool{name: "calc", inner: inner, redial: func(ctx context.Context) (tool.InvokableTool, error) {
		if evicted {
			return fresh, nil
		}
		evicted = true
		return fresh, nil
	}}
	out, err := sut.InvokableRun(context.Background(), "{}")
	if err != nil || out != "ok" {
		t.Fatalf("自愈重试应成功: out=%q err=%v", out, err)
	}
}

func TestSelfHealRedialFailureSurfaces(t *testing.T) {
	inner := &scriptedTool{name: "calc", calls: []error{errors.New("not connected")}}
	sut := &selfHealTool{name: "calc", inner: inner, redial: func(context.Context) (tool.InvokableTool, error) {
		return nil, errors.New("reconnect refused")
	}}
	_, err := sut.InvokableRun(context.Background(), "{}")
	if err == nil || !strings.Contains(err.Error(), "自愈重连失败") {
		t.Fatalf("重连失败应上抛: %v", err)
	}
}
