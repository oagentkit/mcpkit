package mcp

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// TestModernProbeEvictsDeadConnection 回归（v0.10.29 审计）：现代协议
// （2026-07-28）连接上 Client.Ping 是 no-op——探活曾虚报存活且 touch 给死连接
// 续命。修复后现代连接改发真实 tr.Tools/list：server 停掉后探活必须失败并摘除。
func TestModernProbeEvictsDeadConnection(t *testing.T) {
	srv := server.NewMCPServer("probe-mcp", "1.0.0", server.WithToolCapabilities(false))
	srv.AddTool(mcp.NewTool("noop", mcp.WithDescription("noop")),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("ok"), nil
		})
	ts := httptest.NewServer(server.NewStreamableHTTPServer(srv))
	defer ts.Close()

	p := NewPool(ServerConfig{Name: "srv", URL: ts.URL + "/mcp", Timeout: 5 * time.Second})
	defer p.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := p.Tools(ctx, []ToolSpec{{Server: "srv"}}); err != nil {
		t.Fatalf("Tools 失败: %v", err)
	}
	st := p.conn("srv")
	st.mu.Lock()
	isModern := st.modern
	cached := st.cli != nil
	st.mu.Unlock()
	if !isModern {
		t.Fatal("mcp-go v1.1 HTTP 对端应协商到现代协议（代际记录生效的前提）")
	}
	if !cached {
		t.Fatal("应已有缓存连接")
	}
	// 探活成功（真实 RPC 走通）
	if err := p.PingServer(ctx, "srv"); err != nil {
		t.Fatalf("活连接探活应通过: %v", err)
	}
	// 杀掉 server：现代协议下探活必须暴露死亡（ping no-op 曾虚报）
	ts.Close()
	if err := p.PingServer(context.Background(), "srv"); err == nil {
		t.Fatal("死连接探活必须失败（现代协议 no-op ping 曾虚报存活）")
	}
	still := hasConn(p, "srv")
	if still {
		t.Fatal("探活失败应摘除连接")
	}
}

// TestLegacyProbeRefreshesCatalogOnFingerprintChange 回归（oagentkit/mcpkit#100）：
// legacy（≤2025-11-25）代际连接的探活此前只 ping 不比对目录指纹——探活成功即
// touch 续命，探活周期 < maxIdle（或未起回收轮）时连接永不回收、catalogFor 缓存
// 优先，server 工具面变化整个连接生命周期不可见（与现代分支 #53 修复语义不一致）。
// 修复后 legacy 探活同口径：ping 证活后走 tools/list 比对指纹，变化即失效缓存。
func TestLegacyProbeRefreshesCatalogOnFingerprintChange(t *testing.T) {
	srv := server.NewMCPServer("legacy-probe-mcp", "1.0.0", server.WithToolCapabilities(false))
	srv.AddTool(mcp.NewTool("a", mcp.WithDescription("a")),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("ok"), nil
		})

	p := NewPool(ServerConfig{Name: "srv", URL: "http://seed.invalid/mcp"})
	defer p.Close()
	injectClient(t, p, "srv", mustInProcessClient(t, srv)) // 注入连接 modern 缺省 false → legacy 代际

	ctx := context.Background()
	if _, err := p.Tools(ctx, []ToolSpec{{Server: "srv", AllowAll: true}}); err != nil {
		t.Fatalf("Tools 失败: %v", err)
	}
	st := p.conn("srv")
	st.mu.Lock()
	isModern, cached := st.modern, st.catalog != nil
	st.mu.Unlock()
	if isModern {
		t.Fatal("注入连接应处于 legacy 代际（否则测的不是 #100 的分支）")
	}
	if !cached {
		t.Fatal("Tools 后应有目录缓存")
	}

	// 连接生命周期内 server 工具面变化：新增工具 b
	srv.AddTool(mcp.NewTool("b", mcp.WithDescription("b")),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("ok"), nil
		})

	// 对齐 issue 探针：连续三次探活，第一次就该发现指纹变化失效缓存
	for i := 0; i < 3; i++ {
		if err := p.PingServer(ctx, "srv"); err != nil {
			t.Fatalf("活连接探活应通过: %v", err)
		}
		st := p.conn("srv")
		st.mu.Lock()
		cleared := st.catalog == nil
		st.mu.Unlock()
		if !cleared {
			t.Fatalf("第 %d 次探活后工具面变化应失效目录缓存（#100：legacy 只 ping 不比对指纹）", i+1)
		}
	}

	// 端到端：缓存失效后下次 Tools 重列，新工具对装配可见
	tr, err := p.Tools(ctx, []ToolSpec{{Server: "srv", AllowAll: true}})
	if err != nil {
		t.Fatalf("失效后 Tools 失败: %v", err)
	}
	if len(tr.Tools) != 2 {
		t.Fatalf("探活失效重列应看到 2 个工具，得到 %d", len(tr.Tools))
	}
}

// TestLegacyProbeListFailureEvicts 锚定 #100 引入的失败语义与现代分支同口径：
// legacy 探活 ping 证活后补 tools/list，列举失败随探活失败同路径处理——连接
// 摘除（下次调用重建），不得因 ping 已过而虚报存活。
func TestLegacyProbeListFailureEvicts(t *testing.T) {
	p := NewPool()
	defer p.Close()
	cli := &fakeClient{listErr: errors.New("tools/list refused")}
	seedFake(p, "srv", cli)
	if err := p.PingServer(context.Background(), "srv"); err == nil {
		t.Fatal("ping 过但列举失败应报探活失败")
	}
	if !cli.closed {
		t.Fatal("列举失败应摘除关闭连接")
	}
	if hasConn(p, "srv") {
		t.Fatal("列举失败应出缓存")
	}
}

// TestReapIdleInvalidatesCatalog 回归（v0.10.29）：闲置回收必须连带目录失效——
// 否则重建连接后命中旧目录不重新列举，闲置期间工具面变化永久不可见。
func TestReapIdleInvalidatesCatalog(t *testing.T) {
	p := NewPool(ServerConfig{Name: "srv", URL: "http://seed.invalid/mcp"})
	injectClient(t, p, "srv", mustInProcessClient(t, newTestServer(t)))
	setConn(p, "srv", func(st *connState) {
		st.catalog = []toolEntry{{}}
		st.lastUsed = time.Now().Add(-2 * time.Hour)
	})
	if n := p.ReapIdle(time.Hour); n != 1 {
		t.Fatalf("应回收 1 条: %d", n)
	}
	st2 := p.conn("srv")
	st2.mu.Lock()
	hasCatalog := st2.catalog != nil
	st2.mu.Unlock()
	if hasCatalog {
		t.Fatal("回收应连带目录失效")
	}
}

// TestStartIdleReaperAfterCloseRejected 回归（v0.10.29）：Close 后再启动回收
// 协程会永无人停（泄漏）——必须拒绝。
func TestStartIdleReaperAfterCloseRejected(t *testing.T) {
	p := NewPool()
	p.Close()
	p.StartIdleReaper(time.Millisecond, time.Hour)
	p.mu.Lock()
	started := p.reaping
	p.mu.Unlock()
	if started {
		t.Fatal("Close 后不应再启动回收循环")
	}
}
