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

// countingDeadTool 恒死的工具：统计被调用次数（#62 回归锚点——自愈成功后
// 不回写 inner 时，此后每次调用都会先撞一次这个死工具）。
type countingDeadTool struct {
	name  string
	calls int
}

func (t *countingDeadTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: t.name}, nil
}

func (t *countingDeadTool) InvokableRun(_ context.Context, _ string, _ ...tool.Option) (string, error) {
	t.calls++
	return "", errors.New("not connected") // 恒死：传输层错误
}

// TestSelfHealSwapsInnerAfterRedial 回归（oagentkit/mcpkit#62）：自愈成功后必须
// 回写 inner——此前实现从不替换，此后每次调用都先撞一次死连接（多一轮注定失败
// 的调用 + 一次 redial + 一次目录装配）再重试。断言：首调自愈成功后，后续调用
// 直达新工具，死工具调用计数不再增长。
func TestSelfHealSwapsInnerAfterRedial(t *testing.T) {
	dead := &countingDeadTool{name: "calc"}
	// fresh 恒成功：脚本给足 nil（首调重试 1 次 + 后续直达 3 次）
	fresh := &scriptedTool{name: "calc", calls: []error{nil, nil, nil, nil}}
	redials := 0
	sut := &selfHealTool{name: "calc", inner: dead, redial: func(context.Context) (tool.InvokableTool, error) {
		redials++
		return fresh, nil
	}}

	// 首调：撞死连接 → redial → fresh 重试成功（dead.calls=1, redials=1）
	out, err := sut.InvokableRun(context.Background(), "{}")
	if err != nil || out != "ok" {
		t.Fatalf("首调自愈应成功: out=%q err=%v", out, err)
	}
	// 后续多次调用：全部直达 fresh——死连接不再被撞、不再 redial
	for i := 0; i < 3; i++ {
		out, err := sut.InvokableRun(context.Background(), "{}")
		if err != nil || out != "ok" {
			t.Fatalf("第 %d 次调用应直达新工具: out=%q err=%v", i+2, out, err)
		}
	}
	if dead.calls != 1 {
		t.Fatalf("自愈成功后死工具不应再被调用，实际调用 %d 次", dead.calls)
	}
	if redials != 1 {
		t.Fatalf("自愈成功后不应再 redial，实际 %d 次", redials)
	}
	// 回写后读取面随新工具走：Info 直达 fresh
	info, err := sut.Info(context.Background())
	if err != nil || info.Name != "calc" {
		t.Fatalf("回写后 Info 异常: %+v %v", info, err)
	}
}

// TestSelfHealKeepsOldInnerWhenRetryFails 重试失败不回写：下次调用仍从旧 inner
// 触发自愈（半换装状态不得进入 inner）。
func TestSelfHealKeepsOldInnerWhenRetryFails(t *testing.T) {
	dead := &countingDeadTool{name: "calc"}
	alsoDead := &countingDeadTool{name: "calc"} // redial 出的新工具也死
	sut := &selfHealTool{name: "calc", inner: dead, redial: func(context.Context) (tool.InvokableTool, error) {
		return alsoDead, nil
	}}
	if _, err := sut.InvokableRun(context.Background(), "{}"); err == nil {
		t.Fatal("重试仍失败应上抛")
	}
	if alsoDead.calls != 1 {
		t.Fatalf("新工具应被重试一次，实际 %d 次", alsoDead.calls)
	}
	// 再调一次：inner 仍是 dead（未回写），触发新一轮自愈
	if _, err := sut.InvokableRun(context.Background(), "{}"); err == nil {
		t.Fatal("第二轮仍应失败")
	}
	if dead.calls != 2 || alsoDead.calls != 2 {
		t.Fatalf("未回写时应各调用: dead=%d alsoDead=%d", dead.calls, alsoDead.calls)
	}
}
