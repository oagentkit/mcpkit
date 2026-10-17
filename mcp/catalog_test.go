package mcp

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func boolPtr(b bool) *bool { return &b }

// countingClient 统计 ListTools 线上次数（缓存行为验证用；其余方法透传）。
type countingClient struct {
	client.MCPClient
	listCalls int
}

func (c *countingClient) ListTools(ctx context.Context, req mcp.ListToolsRequest) (*mcp.ListToolsResult, error) {
	c.listCalls++
	return c.MCPClient.ListTools(ctx, req)
}

func newAnnotatedServer(t *testing.T) *server.MCPServer {
	t.Helper()
	srv := server.NewMCPServer("annotated-mcp", "1.0.0",
		server.WithToolCapabilities(false))
	ro := true
	srv.AddTool(mcp.NewTool("reader",
		mcp.WithDescription("read-only idempotent tool"),
		mcp.WithToolAnnotation(mcp.ToolAnnotation{
			ReadOnlyHint: &ro, IdempotentHint: &ro, DestructiveHint: boolPtr(false),
		}),
	), func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	// 无注解工具：mcp-go 缺省 readOnly=false/destructive=true
	srv.AddTool(mcp.NewTool("writer", mcp.WithDescription("no annotation")),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("ok"), nil
		})
	return srv
}

// TestCatalogListOnce 每连接只走一次线上 ListTools，后续 Tools() 命中缓存。
func TestCatalogListOnce(t *testing.T) {
	srv := newAnnotatedServer(t)
	cli := &countingClient{MCPClient: mustInProcessClient(t, srv)}
	p := NewPool(ServerConfig{Name: "srv", URL: "http://seed.invalid/mcp"})
	injectClient(t, p, "srv", cli)

	for i := 0; i < 3; i++ {
		tr, err := p.Tools(context.Background(), []ToolSpec{{Server: "srv", Allow: []string{"reader"}}})
		if err != nil {
			t.Fatalf("Tools 失败: %v", err)
		}
		if len(tr.Tools) != 1 {
			t.Fatalf("期望 1 个工具，得 %d", len(tr.Tools))
		}
	}
	if cli.listCalls != 1 {
		t.Fatalf("ListTools 应只走线一次，实际 %d 次", cli.listCalls)
	}
}

// TestHintsExtracted 注解随目录提取：显式注解 + mcp-go 缺省注解两形态。
func TestHintsExtracted(t *testing.T) {
	srv := newAnnotatedServer(t)
	p := NewPool(ServerConfig{Name: "srv", URL: "http://seed.invalid/mcp"})
	injectClient(t, p, "srv", mustInProcessClient(t, srv))

	if _, err := p.Tools(context.Background(), []ToolSpec{{Server: "srv"}}); err != nil {
		t.Fatalf("Tools 失败: %v", err)
	}
	hints := p.Hints("srv")
	if hints == nil {
		t.Fatal("Hints 不应为 nil")
	}
	ro := hints["reader"]
	if !ro.ReadOnly || !ro.Idempotent || ro.Destructive {
		t.Fatalf("reader 注解归一错误: %+v", ro)
	}
	if !ro.ConcurrentSafe() || ro.RiskLevel() != "low" {
		t.Fatalf("reader 应为并发安全/low: %+v", ro)
	}
	w := hints["writer"]
	if w.ReadOnly || w.Idempotent || !w.Destructive {
		t.Fatalf("writer 应为缺省非只读/破坏: %+v", w)
	}
	if w.ConcurrentSafe() || w.RiskLevel() != "high" {
		t.Fatalf("writer 应为非并发安全/high: %+v", w)
	}
	if got := p.Hints("nope"); got != nil {
		t.Fatalf("未列举 server 的 Hints 应为 nil，得 %v", got)
	}
}

// TestCatalogInvalidatedOnEvict 连接被摘除后目录同失效，重建连接重新列举。
func TestCatalogInvalidatedOnEvict(t *testing.T) {
	srv := newAnnotatedServer(t)
	cli1 := &countingClient{MCPClient: mustInProcessClient(t, srv)}
	p := NewPool(ServerConfig{Name: "srv", URL: "http://seed.invalid/mcp"})
	injectClient(t, p, "srv", cli1)
	if _, err := p.Tools(context.Background(), []ToolSpec{{Server: "srv"}}); err != nil {
		t.Fatalf("Tools 失败: %v", err)
	}
	if cli1.listCalls != 1 {
		t.Fatalf("首次应列举一次，实际 %d", cli1.listCalls)
	}
	// 模拟连接死亡：evict 后重灌新连接
	cli2 := &countingClient{MCPClient: mustInProcessClient(t, srv)}
	st := p.conn("srv")
	st.mu.Lock()
	cached := st.cli
	st.mu.Unlock()
	p.evict("srv", cached)
	injectClient(t, p, "srv", cli2)
	if _, err := p.Tools(context.Background(), []ToolSpec{{Server: "srv"}}); err != nil {
		t.Fatalf("重建后 Tools 失败: %v", err)
	}
	if cli2.listCalls != 1 {
		t.Fatalf("重建连接应重新列举一次，实际 %d", cli2.listCalls)
	}
	if p.Hints("srv")["reader"].ReadOnly != true {
		t.Fatal("重建后注解表应重新填充")
	}
}

// TestConvToolIsErrorObservation 锚点文案：isError 结果须以 "mcp server return error"
// 文本上抛（errorAsObservation 降级器的识别锚点）。
func TestConvToolIsErrorObservation(t *testing.T) {
	srv := server.NewMCPServer("err-mcp", "1.0.0", server.WithToolCapabilities(false))
	srv.AddTool(mcp.NewTool("boom", mcp.WithDescription("always fails")),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultError("目标不存在"), nil
		})
	p := NewPool(ServerConfig{Name: "srv", URL: "http://seed.invalid/mcp"})
	injectClient(t, p, "srv", mustInProcessClient(t, srv))
	tr, err := p.Tools(context.Background(), []ToolSpec{{Server: "srv", Allow: []string{"boom"}}})
	if err != nil {
		t.Fatalf("Tools 失败: %v", err)
	}
	it, ok := tr.Tools[0].(tool.InvokableTool)
	if !ok {
		t.Fatal("工具应为 InvokableTool")
	}
	out, err := it.InvokableRun(context.Background(), "{}")
	if err != nil {
		t.Fatalf("isError 应被降级为观察而非 error: %v", err)
	}
	if out == "" {
		t.Fatal("观察文本不应为空")
	}
}

func mustInProcessClient(t *testing.T, srv *server.MCPServer) client.MCPClient {
	t.Helper()
	cli, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatalf("in-process client 创建失败: %v", err)
	}
	return cli
}
