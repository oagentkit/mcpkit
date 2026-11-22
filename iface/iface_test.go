package iface

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
)

// iface_test.go —— 协议中立面的行为钉子。完整分类学测试在 mcp/mcp
// （errclass_test.go，含 JSON-RPC 码与业务 payload）；这里钉 stdlib 判据
// 与解包优先级——零 mcp-go 消费方（toolgate）依赖的正是这两条。

func TestKindOfUnwrapWins(t *testing.T) {
	wrapped := &ClassifiedError{Kind: KindToolMissing, Err: io.EOF}
	// 解包优先：载体在链上就以包定分类为准，不再走 stdlib 判据（EOF 在
	// stdlib 判据里是 unreachable，但载体说了算）。
	if got := KindOf(fmt.Errorf("outer: %w", wrapped)); got != KindToolMissing {
		t.Errorf("KindOf(链上有载体)=%v，期望 tool_missing", got)
	}
	if got := KindOf(fmt.Errorf("outer: %w", io.EOF)); got != KindUnreachable {
		t.Errorf("KindOf(裸 EOF)=%v，期望 unreachable（stdio server 死亡经典形态）", got)
	}
	if got := KindOf(nil); got != KindUnknown {
		t.Errorf("KindOf(nil)=%v，期望 unknown", got)
	}
}

func TestClassifyStdlibKind(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want ErrorKind
	}{
		{"连接被拒", fmt.Errorf("dial: %w", syscall.ECONNREFUSED), KindUnreachable},
		{"主机不可达", syscall.EHOSTUNREACH, KindUnreachable},
		{"net.Error", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, KindUnreachable},
		{"ctx 超时", context.DeadlineExceeded, KindUnreachable},
		{"ctx 取消", context.Canceled, KindUnreachable},
		{"可执行不存在", &os.PathError{Op: "fork/exec", Path: "/no/such/bin"}, KindUnreachable},
		{"确定性业务失败不猜", errors.New("pod not found"), KindUnknown},
	}
	for _, tc := range cases {
		if got := ClassifyStdlibKind(tc.err); got != tc.want {
			t.Errorf("%s: ClassifyStdlibKind=%v，期望 %v", tc.name, got, tc.want)
		}
	}
}

func TestClassifyWrapsAndKeepsChain(t *testing.T) {
	inner := fmt.Errorf("dial: %w", syscall.ECONNREFUSED)
	wrapped := Classify(fmt.Errorf("outer: %w", inner))
	if wrapped == nil {
		t.Fatal("Classify(nil 以外) 不得返回 nil")
	}
	var ce *ClassifiedError
	if !errors.As(wrapped, &ce) || ce.Kind != KindUnreachable {
		t.Fatalf("应包成 KindUnreachable 载体: %+v", wrapped)
	}
	// 原始错误链可穿透：errors.Is 仍能命中底层哨兵
	if !errors.Is(wrapped, syscall.ECONNREFUSED) {
		t.Errorf("errors.Is 穿透失败: %v", wrapped)
	}
	if Classify(nil) != nil {
		t.Errorf("Classify(nil) 必须返回 nil")
	}
}

func TestKindStringStable(t *testing.T) {
	// 稳定标识（进日志/指标标签/监控配置），改名即破坏下游过滤——冻结。
	if got := (KindToolMissing).String(); got != "tool_missing" {
		t.Errorf("tool_missing 序列化漂移: %q", got)
	}
	if got := (KindUnknown).String(); got != "unknown" {
		t.Errorf("unknown 序列化漂移: %q", got)
	}
}
