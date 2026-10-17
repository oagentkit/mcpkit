package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

type stubTool struct{ err error }

func (s *stubTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: "stub"}, nil
}
func (s *stubTool) InvokableRun(context.Context, string, ...tool.Option) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	return "ok", nil
}

// isError:true 的 MCP 业务失败必须降级为观察文本（LLM 可见可修正），不得上抛炸步。
func TestWrapErrorAsObservation(t *testing.T) {
	isErr := errors.New(`failed to call mcp tool, mcp server return error: {"content":[{"type":"text","text":"Failed to get pod 'x': pods \"x\" not found"}],"isError":true}`)
	wrapped := WrapErrorAsObservation([]tool.BaseTool{&stubTool{err: isErr}})
	inv, ok := wrapped[0].(tool.InvokableTool)
	if !ok {
		t.Fatalf("InvokableTool 未被包装")
	}
	out, err := inv.InvokableRun(context.Background(), "{}")
	if err != nil {
		t.Fatalf("isError 被上抛（会炸整步）: %v", err)
	}
	if !strings.Contains(out, "Failed to get pod 'x'") {
		t.Fatalf("错误文本未剥出: %q", out)
	}

	transport := errors.New("client connection refused")
	wrapped = WrapErrorAsObservation([]tool.BaseTool{&stubTool{err: transport}})
	inv2, ok := wrapped[0].(tool.InvokableTool)
	if !ok {
		t.Fatalf("InvokableTool 未被包装")
	}
	if _, err := inv2.InvokableRun(context.Background(), "{}"); err == nil {
		t.Fatalf("传输层错误被吞: 应原样上抛")
	}
}
