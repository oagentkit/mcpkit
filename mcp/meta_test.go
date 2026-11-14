package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// newEchoMetaServer 注册 meta_echo 工具：把请求侧 _meta 原文回显进结果文本。
func newEchoMetaServer(t *testing.T) *server.MCPServer {
	t.Helper()
	srv := server.NewMCPServer("meta-mcp", "1.0.0", server.WithToolCapabilities(false))
	srv.AddTool(mcp.NewTool("meta_echo", mcp.WithDescription("echo request _meta")),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var parts []string
			if req.Params.Meta != nil {
				for k, v := range req.Params.Meta.AdditionalFields {
					// mcp-go v1.1+ 现代协议会在 _meta 附带协议自身键
					// （clientCapabilities 等非 string 值），只回显业务键。
					if s, ok := v.(string); ok {
						parts = append(parts, k+"="+s)
					}
				}
			}
			return mcp.NewToolResultText(strings.Join(parts, ",")), nil
		})
	return srv
}

// TestRequestMetaInjected metaFn 产物应逐键注入请求侧 _meta（server 侧可读）。
func TestRequestMetaInjected(t *testing.T) {
	p := NewPool(ServerConfig{Name: "srv", URL: "http://seed.invalid/mcp"})
	p.RequestMeta = func(ctx context.Context) map[string]any {
		if v, ok := ctx.Value(ctxMetaKey{}).(map[string]any); ok {
			return v
		}
		return nil
	}
	// 白盒：镜像 client() 的装饰点（in-process client 无法走真实 dial 路径）
	injectClient(t, p, "srv", &metaClient{MCPClient: mustInProcessClient(t, newEchoMetaServer(t)), metaFn: p.RequestMeta})

	tr, err := p.Tools(context.Background(), []ToolSpec{{Server: "srv", AllowAll: true}})
	if err != nil {
		t.Fatalf("Tools 失败: %v", err)
	}
	it, ok := tr.Tools[0].(tool.InvokableTool)
	if !ok {
		t.Fatal("应为 InvokableTool")
	}
	ctx := context.WithValue(context.Background(), ctxMetaKey{},
		map[string]any{"com.bianque/session_id": "s1", "com.bianque/caller": "expert-a"})
	out, err := it.InvokableRun(ctx, "{}")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if !strings.Contains(out, "com.bianque/session_id=s1") || !strings.Contains(out, "com.bianque/caller=expert-a") {
		t.Fatalf("_meta 未注入 server: %s", out)
	}
	// 无 meta 值的 ctx：不注入（server 侧 Meta 为 nil 或空）
	out2, err := it.InvokableRun(context.Background(), "{}")
	if err != nil {
		t.Fatalf("裸调用失败: %v", err)
	}
	if strings.Contains(out2, "com.bianque/") {
		t.Fatalf("空 meta 不应注入: %s", out2)
	}
}

type ctxMetaKey struct{}

// TestWithCallMetaSnapshot 回归（oagentkit/mcpkit#142）：注入即拷贝（快照语义）——
// 注入后改写原 map 不得影响 ctx 里已注入的元数据（并发工具调用下 metaClient 读
// 原活对象构成 data race；在途请求 _meta 被静默改写）。对齐 llm#68 同款纪律。
func TestWithCallMetaSnapshot(t *testing.T) {
	meta := map[string]any{"com.bianque/session_id": "s1"}
	ctx := WithCallMeta(context.Background(), meta)
	// 注入后改原 map：快照不变
	meta["com.bianque/session_id"] = "s2"
	meta["com.bianque/trace_id"] = "tr9"
	got := CallMeta(ctx)
	if got["com.bianque/session_id"] != "s1" {
		t.Fatalf("改原 map 不应影响已注入快照, got %v", got)
	}
	if _, ok := got["com.bianque/trace_id"]; ok {
		t.Fatalf("注入后新增键不应出现在快照, got %v", got)
	}
	// 空/nil 注入：零开销直通（原 ctx 原样返回，CallMeta 恒 nil）
	if WithCallMeta(context.Background(), nil) != context.Background() {
		t.Fatal("nil meta 应直通原 ctx")
	}
	empty := context.WithValue(context.Background(), ctxMetaKey{}, "sentinel")
	if WithCallMeta(empty, map[string]any{}) != empty {
		t.Fatal("空 meta 应直通原 ctx")
	}
}
