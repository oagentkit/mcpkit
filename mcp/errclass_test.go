package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"testing"

	"errors"

	"github.com/mark3labs/mcp-go/mcp"
)

// 裸 EOF 落 unreachable（mcpkit#52）：stdio server 死亡的经典形态 = stdout
// 读到裸 EOF——此前不认 io.EOF 落 KindUnknown（不重试不自愈）。
func TestClassifyBareEOFUnreachable(t *testing.T) {
	if k := KindOf(io.EOF); k != KindUnreachable {
		t.Fatalf("裸 EOF 应落 unreachable: %s", k)
	}
	var ce *ClassifiedError
	if !errors.As(Classify(fmt.Errorf("read stdout: %w", io.EOF)), &ce) || ce.Kind != KindUnreachable {
		t.Fatalf("包装 EOF 应落 unreachable")
	}
	if !KindUnreachable.Retryable() {
		t.Fatal("unreachable 应可重试")
	}
}

// ErrorKind 反解未知名串 fail-fast（mcpkit#54）：监控配置 typo 此前静默归
// KindUnknown（零可见性降级 = 不可重试），与序列化侧严格性不对称。
func TestErrorKindUnmarshalStrict(t *testing.T) {
	var k ErrorKind
	if err := json.Unmarshal([]byte(`"unreachable"`), &k); err != nil || k != KindUnreachable {
		t.Fatalf("合法名应反解: %v %s", err, k)
	}
	if err := json.Unmarshal([]byte(`"unknown"`), &k); err != nil || k != KindUnknown {
		t.Fatalf("unknown 名应反解: %v %s", err, k)
	}
	if err := json.Unmarshal([]byte(`"unreachble"`), &k); err == nil {
		t.Fatal("typo 名应 fail-fast")
	}
	// 回环：Marshal→Unmarshal 各 kind 恒等
	for _, want := range []ErrorKind{KindUnknown, KindUnreachable, KindProtocol, KindToolMissing, KindInvalidArgs} {
		b, _ := json.Marshal(want)
		var got ErrorKind
		if err := json.Unmarshal(b, &got); err != nil || got != want {
			t.Fatalf("回环失败 %s: %v %s", b, err, got)
		}
	}
}

// 探活刷新目录缓存（mcpkit#53）：探活成功路径比对指纹，变化即失效——
// 探活周期 < maxIdle 时 server 工具面变化不再永久不可见。
func TestProbeFingerprintChanges(t *testing.T) {
	t1 := []mcp.Tool{{Name: "a"}}
	t2 := []mcp.Tool{{Name: "a"}, {Name: "b"}}
	if probeFingerprint(t1) == probeFingerprint(t2) {
		t.Fatal("不同工具面指纹应不同")
	}
	if probeFingerprint(t1) != probeFingerprint(t1) {
		t.Fatal("同工具面指纹应恒等")
	}
	// schema 变化也要触发（描述不参与，InputSchema 参与）
	t3 := []mcp.Tool{{Name: "a", InputSchema: mcp.ToolInputSchema{Type: "object", Properties: map[string]any{"x": map[string]any{"type": "string"}}}}}
	t4 := []mcp.Tool{{Name: "a"}}
	if probeFingerprint(t3) == probeFingerprint(t4) {
		t.Fatal("schema 变化指纹应不同")
	}
}
