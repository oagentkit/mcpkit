package mcp

import (
	"context"
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
	stopped := p.reapStop
	p.mu.Unlock()
	if stopped != nil {
		t.Fatal("Close 后不应再启动回收协程")
	}
}
