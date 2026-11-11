package mcp

// 2026-11-11 第二轮清零回归（#118）：probeFingerprint 的配对判别力。

import (
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func mkTool(name, schema string) mcp.Tool {
	t := mcp.Tool{Name: name}
	if err := json.Unmarshal([]byte(schema), &t.InputSchema); err != nil {
		panic(err)
	}
	return t
}

func TestProbeFingerprintPairwise_Issue118(t *testing.T) {
	// schema 互换：指纹必须不同（独立 sort 丢失配对关系的形态）。
	a := []mcp.Tool{mkTool("a", `{"type":"object"}`), mkTool("b", `{"type":"string"}`)}
	b := []mcp.Tool{mkTool("a", `{"type":"string"}`), mkTool("b", `{"type":"object"}`)}
	if probeFingerprint(a) == probeFingerprint(b) {
		t.Fatal("schema 互换的指纹必须不同（mcpkit#118）")
	}
	// 同一工具集（不同顺序）：指纹必须相同（同工具面不触发缓存失效）。
	same := []mcp.Tool{mkTool("b", `{"type":"string"}`), mkTool("a", `{"type":"object"}`)}
	if probeFingerprint(a) != probeFingerprint(same) {
		t.Fatal("同工具集不同顺序指纹应相同")
	}
	// 含分隔符的恶意名：不得构造碰撞。
	tricky := []mcp.Tool{mkTool("a|b", `{"type":"object"}`), mkTool("c", `{"type":"string"}`)}
	tricky2 := []mcp.Tool{mkTool("a", `{"type":"object"}`), mkTool("b|c", `{"type":"string"}`)}
	if probeFingerprint(tricky) == probeFingerprint(tricky2) {
		t.Fatal("含分隔符名的碰撞形态应被区分")
	}
	// 空目录稳定。
	if probeFingerprint(nil) == probeFingerprint(a) {
		t.Fatal("空目录与非空指纹应不同")
	}
}
