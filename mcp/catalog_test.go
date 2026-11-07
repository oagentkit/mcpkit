package mcp

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

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

// TestTextResultUnwrap TextResult 开关双档（oagentkit/mcpkit#39）：缺省返回
// 完整 CallToolResult JSON 信封（eino-ext toolHelper 同口径）；TextResult=true
// 返回剥壳后的文本内容——宿主按 tool.Tool 文本惯例直喂 LLM/前端时不必自行
// 维护剥壳配对。
func TestTextResultUnwrap(t *testing.T) {
	srv := server.NewMCPServer("text-mcp", "1.0.0", server.WithToolCapabilities(false))
	srv.AddTool(mcp.NewTool("hello", mcp.WithDescription("returns plain text")),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("hello world"), nil
		})

	run := func(p *Pool) string {
		t.Helper()
		tr, err := p.Tools(context.Background(), []ToolSpec{{Server: "srv", Allow: []string{"hello"}}})
		if err != nil {
			t.Fatalf("Tools 失败: %v", err)
		}
		it, ok := tr.Tools[0].(tool.InvokableTool)
		if !ok {
			t.Fatal("工具应为 InvokableTool")
		}
		out, err := it.InvokableRun(context.Background(), "{}")
		if err != nil {
			t.Fatalf("InvokableRun 失败: %v", err)
		}
		return out
	}

	// 缺省：信封原样（含 content[] 包装层）
	p0 := NewPool(ServerConfig{Name: "srv", URL: "http://seed.invalid/mcp"})
	injectClient(t, p0, "srv", mustInProcessClient(t, srv))
	env := run(p0)
	if !strings.Contains(env, `"content"`) || !strings.Contains(env, "hello world") {
		t.Fatalf("缺省应返回完整信封: %q", env)
	}

	// TextResult=true：剥壳文本
	p := NewPool(ServerConfig{Name: "srv", URL: "http://seed.invalid/mcp"})
	p.TextResult = true
	injectClient(t, p, "srv", mustInProcessClient(t, srv))
	if got := run(p); got != "hello world" {
		t.Fatalf("TextResult 应返回剥壳文本，得 %q", got)
	}
}

// TestCatalogForTransportSnapshotRace -race 回归（oagentkit/mcpkit#61）：
// catalogFor 对 st.tr/st.protoVersion 的读取必须在 st.mu 临界区内快照——
// 写方 registerConn 持 st.mu 写同字段（自愈 evict + 重新 dial 窗口），读方若在
// 临界区外读（v0.10.0 形态）即构成数据竞争。G1 反复装配（缓存未命中 → 快照 tr
// 走 raw 通道列举）；G2 反复摘连接（触发 G1 侧重 dial → registerConn 写入）。
// 回归态（读移出临界区）在 -race 下必红。
func TestCatalogForTransportSnapshotRace(t *testing.T) {
	ts := rawFidelityServer(t)
	p := NewPool(ServerConfig{Name: "srv", URL: ts.URL + "/mcp", Timeout: 5 * time.Second})
	defer p.Close()

	const rounds = 20
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // G1：装配循环——catalogFor 的快照读方
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			// 失败容忍：与 G2 的摘连接竞争时列举可失败（连接失效语义，非断言对象）
			_, _ = p.Tools(context.Background(), []ToolSpec{{Server: "srv", AllowAll: true}})
		}
	}()
	go func() { // G2：摘连接循环——模拟自愈 redial 的前半段；后半段（registerConn
		// 写 st.tr）由 G1 的重 dial 完成，一读一写就此交错
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			if st := p.conn("srv"); st != nil {
				st.mu.Lock()
				cached := st.cli
				st.mu.Unlock()
				if cached != nil {
					p.evict("srv", cached)
				}
			}
			time.Sleep(time.Millisecond) // 让 G1 有机会进入列举窗口
		}
	}()
	wg.Wait()

	// 收尾断言：竞态轮过后池应仍可正常装配（连接/目录缓存未被竞态破坏）
	res, err := p.Tools(context.Background(), []ToolSpec{{Server: "srv", AllowAll: true}})
	if err != nil {
		t.Fatalf("收尾 Tools 失败: %v", err)
	}
	if len(res.Tools) != 1 || res.Failed != nil {
		t.Fatalf("收尾装配异常: tools=%d failed=%v", len(res.Tools), res.Failed)
	}
}
