package mcp

// 生命周期 API 测试（oagentkit/mcpkit#63）：CloseServer 按 server 断连（幂等、
// 下次调用透明重建）+ Drain 停机收口（拒新取连 → 等在飞收尾 → 断全部连接）。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// gatingCallClient 调用可门控的假体：CallTool 阻塞到 release 关闭才返回
// （Drain「等在飞收尾」的时序锚点）；ListTools 立即返回单工具目录。
type gatingCallClient struct {
	client.MCPClient
	closed  bool
	release chan struct{}
}

func (f *gatingCallClient) ListTools(context.Context, mcp.ListToolsRequest) (*mcp.ListToolsResult, error) {
	return &mcp.ListToolsResult{Tools: []mcp.Tool{{Name: "gated"}}}, nil
}

func (f *gatingCallClient) CallTool(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	<-f.release // 门控：Drain 应等这次调用收尾
	return mcp.NewToolResultText("gated-ok:" + req.Params.Name), nil
}

func (f *gatingCallClient) Close() error {
	f.closed = true
	return nil
}

// TestCloseServerDropsConnection 按 server 断连：连接关闭、目录连带失效、幂等、
// 未配置的 server 静默。断连是热换装的确定性处置——不依赖闲置回收兜底。
func TestCloseServerDropsConnection(t *testing.T) {
	p := NewPool()
	a, b := &fakeClient{}, &fakeClient{}
	seedFake(p, "a", a)
	seedFake(p, "b", b)
	// 预填目录：断连须连带失效（与 evict/ReapIdle 同构），否则重建后命中旧目录
	setConn(p, "a", func(st *connState) { st.catalog = []toolEntry{{tool: mcp.Tool{Name: "stale"}}} })

	p.CloseServer("a")
	if !a.closed {
		t.Fatal("CloseServer 应关闭目标连接")
	}
	if b.closed {
		t.Fatal("其余 server 连接不应受影响")
	}
	if hasConn(p, "a") {
		t.Fatal("CloseServer 后连接应出缓存")
	}
	st := p.conn("a")
	st.mu.Lock()
	catalog := st.catalog
	st.mu.Unlock()
	if catalog != nil {
		t.Fatal("CloseServer 应连带失效目录缓存")
	}
	p.CloseServer("a")  // 幂等
	p.CloseServer("no") // 未配置 server：静默
	if b.closed || hasConn(p, "b") != true {
		t.Fatal("幂等重入/未知 server 不应有副作用")
	}
}

// TestCloseServerTransparentRedial 断连后下次调用透明重建（lazy 建连同形）：
// 真实 HTTP server 上 Close → Tools 自动重拨成功。
func TestCloseServerTransparentRedial(t *testing.T) {
	ts := rawFidelityServer(t)
	p := NewPool(ServerConfig{Name: "srv", URL: ts.URL + "/mcp", Timeout: 5 * time.Second})
	defer p.Close()
	res, err := p.Tools(context.Background(), []ToolSpec{{Server: "srv", AllowAll: true}})
	if err != nil || len(res.Tools) != 1 {
		t.Fatalf("首装失败: %v tools=%d", err, len(res.Tools))
	}
	p.CloseServer("srv")
	if hasConn(p, "srv") {
		t.Fatal("断连后应无缓存连接")
	}
	res, err = p.Tools(context.Background(), []ToolSpec{{Server: "srv", AllowAll: true}})
	if err != nil {
		t.Fatalf("断连后重装失败（应透明重建）: %v", err)
	}
	if len(res.Tools) != 1 {
		t.Fatalf("透明重建后应拿到工具，得 %d", len(res.Tools))
	}
}

// TestDrainWaitsForInFlight Drain 三步语义（拒新 → 等在飞 → 断连）：
// 在飞调用收尾前 Drain 不得返回、新取连立即被拒；在飞收尾后才断连接。
func TestDrainWaitsForInFlight(t *testing.T) {
	p := NewPool()
	cli := &gatingCallClient{release: make(chan struct{})}
	seedFake(p, "srv", cli)
	// Tools 前置校验要求 stdio/http 二选一（dial 不会真发生——连接已直灌缓存）
	setCfg(p, "srv", ServerConfig{Name: "srv", URL: "http://seed.invalid/mcp"})
	res, err := p.Tools(context.Background(), []ToolSpec{{Server: "srv", AllowAll: true}})
	if err != nil || len(res.Tools) != 1 {
		t.Fatalf("装配失败: %v tools=%d", err, len(res.Tools))
	}
	assembled := res.Tools[0]

	callDone := make(chan struct{})
	go func() { // 在飞调用：阻塞在 gatingCallClient.CallTool
		defer close(callDone)
		inv, ok := assembled.(tool.InvokableTool)
		if !ok {
			t.Error("装配产物应为 InvokableTool")
			return
		}
		out, err := inv.InvokableRun(context.Background(), "{}")
		if err != nil {
			t.Errorf("在飞调用应优雅收尾: %v", err)
			return
		}
		if !strings.Contains(out, "gated-ok") {
			t.Errorf("在飞调用结果异常: %q", out)
		}
	}()

	// 等在飞登记落账（leaseUse 在 InvokableRun 进入时 +1）
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st := p.conn("srv")
		st.mu.Lock()
		n := st.inFlight
		st.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	drainDone := make(chan struct{})
	go func() { // 停机收口：应阻塞到在飞调用收尾
		defer close(drainDone)
		p.Drain(5 * time.Second)
	}()

	// 等 Drain 第 1 步生效（closed 置位；goroutine 调度有窗口，轮询对齐）
	dl := time.Now().Add(2 * time.Second)
	for time.Now().Before(dl) {
		p.mu.Lock()
		closed := p.closed
		p.mu.Unlock()
		if closed {
			break
		}
		time.Sleep(time.Millisecond)
	}

	// 第 1 步立即生效：新取连被拒（不等在飞；server 级失败进 Failed 面）
	rej, err := p.Tools(context.Background(), []ToolSpec{{Server: "srv", AllowAll: true}})
	if err != nil {
		t.Fatalf("server 级失败应容忍进 Failed 而非整体报错: %v", err)
	}
	if ferr, ok := rej.Failed["srv"]; !ok || !strings.Contains(ferr.Error(), "池已关闭") {
		t.Fatalf("Drain 后新装配应立即被拒: failed=%v", rej.Failed)
	}
	select {
	case <-drainDone:
		t.Fatal("在飞未收尾前 Drain 不得返回")
	case <-time.After(50 * time.Millisecond):
	}

	// 在飞收尾 → Drain 返回 → 连接已断
	close(cli.release) // 测试门控通道：单次关闭
	<-callDone
	select {
	case <-drainDone:
	case <-time.After(2 * time.Second):
		t.Fatal("在飞收尾后 Drain 应及时返回")
	}
	if !cli.closed {
		t.Fatal("Drain 结束应断全部连接")
	}
	if rej, err := p.Tools(context.Background(), []ToolSpec{{Server: "srv", AllowAll: true}}); err != nil ||
		len(rej.Failed) == 0 {
		t.Fatalf("Drain 后池不可复用: err=%v failed=%v", err, rej.Failed)
	}
}

// TestDrainTimeoutHardCloses 超时硬断：在飞调用超预算时 Drain 不得无限阻塞
// （带着仍在飞的调用断连——停机收口不得被单笔长调用卡死）。
func TestDrainTimeoutHardCloses(t *testing.T) {
	p := NewPool()
	cli := &gatingCallClient{release: make(chan struct{})}
	seedFake(p, "srv", cli)
	defer close(cli.release) // 测试门控通道：单次关闭

	setConn(p, "srv", func(st *connState) { st.inFlight = 1 }) // 白盒造在途（不必真发起调用）

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Drain(30 * time.Millisecond)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("超时后 Drain 应硬断返回")
	}
	if !cli.closed {
		t.Fatal("超时硬断应关闭连接")
	}
}

// TestDrainIdempotent Drain/Close 交叉幂等。
func TestDrainIdempotent(t *testing.T) {
	p := NewPool()
	cli := &fakeClient{}
	seedFake(p, "srv", cli)
	p.Drain(0) // <=0：不等在飞，立即断连
	p.Drain(time.Second)
	p.Close()
	if !cli.closed {
		t.Fatal("幂等路径后连接应已关闭")
	}
}

// 编译期假体形状自检。
var _ client.MCPClient = (*gatingCallClient)(nil)

// setCfg 白盒覆写 server 声明（测试路径）。
func setCfg(p *Pool, name string, cfg ServerConfig) {
	p.mu.Lock()
	p.cfgs[name] = cfg
	p.mu.Unlock()
}
