package mcp

// oagentkit/mcpkit#99：InvokableRun 对 inner 双取——调用一次 currentInner()、
// redial 基准再经 currentClient() 重取一次。两取之间跨并发换代窗口时，G2 的
// dead 基准漂移到 G1 刚换上的健康连接 C2，selfHealRedial 的 cached == dead
// 命中 ⇒ 在用健康连接被逐出（正值在用）。修法：入口单次快照，调用与 dead
// 基准同源同代——对齐 #88 注释「比对基准必须是刚在本工具上失败的那条连接」。

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// genInner 代际 inner 假体：绑定具体 client 实例（mcpClienter 暴露面），可门控
// InvokableRun（gate 非 nil 时阻塞到 gate 关闭再返回 runErr）。
type genInner struct {
	name   string
	cli    client.MCPClient
	gate   chan struct{}
	runErr error
	out    string
	calls  int
}

func (b *genInner) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: b.name}, nil
}

func (b *genInner) InvokableRun(context.Context, string, ...tool.Option) (string, error) {
	b.calls++
	if b.gate != nil {
		<-b.gate
	}
	if b.runErr != nil {
		return "", b.runErr
	}
	return b.out, nil
}

func (b *genInner) mcpClient() client.MCPClient { return b.cli }

var _ mcpClienter = (*genInner)(nil)

// TestSelfHealDeadBasisIsCallGeneration 单元级回归（#99 核心）：G2 在 C1 代上
// 失败，失败解析到 redial 之前 G1 完成整轮换代（swapInner 换 C2）——dead 基准
// 必须仍是 G2 快照代的 C1，不得漂移到健康的 C2。旧实现二次解析 currentClient()
// 拿到 C2 当 dead ⇒ 生产侧 cached == dead 命中 ⇒ 健康连接被逐出。
func TestSelfHealDeadBasisIsCallGeneration(t *testing.T) {
	c1 := &scriptedClient{} // G2 快照代连接（失败的连接）
	c2 := &scriptedClient{} // G1 自愈刚换上的健康连接
	gate := make(chan struct{})
	inner1 := &genInner{name: "calc", cli: c1, gate: gate, runErr: errors.New("not connected")}
	inner2 := &genInner{name: "calc", cli: c2, out: "ok"}

	var dead client.MCPClient
	sut := &selfHealTool{name: "calc", inner: inner1, redial: func(_ context.Context, d client.MCPClient) (tool.InvokableTool, error) {
		dead = d
		return inner2, nil
	}}

	done := make(chan error, 1)
	go func() {
		_, err := sut.InvokableRun(context.Background(), "{}")
		done <- err
	}()

	// 等 G2 在 inner1 的调用里停稳（快照已定型为 C1 代），再扮演 G1 完成换代
	time.Sleep(50 * time.Millisecond)
	sut.swapInner(inner2)
	close(gate)

	if err := <-done; err != nil {
		t.Fatalf("G2 自愈重试应成功: %v", err)
	}
	if dead != c1 {
		t.Fatalf("dead 基准漂移到新代连接（#99）: dead=%v want c1(%v)——healthy c2 被当死连接逐出", dead, c1)
	}
	if inner1.calls != 1 || inner2.calls != 1 {
		t.Fatalf("调用次数异常: inner1=%d inner2=%d", inner1.calls, inner2.calls)
	}
}

// TestSelfHealConcurrentHealNoHealthyEvict 端到端回归（#99）：真 Pool 装配链 +
// 真 selfHealRedial 闭包 + 建连单飞，仅 dial 以替身注入。G2 的调用停在死连接
// gen0 的 CallTool 里，G1 在此窗口完成整轮自愈（evict gen0 → 拨 gen1 → 重试
// 成功 → swapInner）；放行 G2 后——
//
//   - 修复形态：G2 的 dead = 快照代 gen0 ≠ cached(gen1) ⇒ 跳过 evict ⇒ fast
//     path 命中 gen1 ⇒ 在健康连接上重试成功（dials=2, gen1 关闭数=0）；
//   - 旧实现形态：G2 二次解析 currentClient() 到 gen1 当 dead ⇒ cached == dead
//     ⇒ 把 G1 刚换上、正值在用的健康连接逐出，再拨 gen2（dials=3, gen1 关闭数=1）。
func TestSelfHealConcurrentHealNoHealthyEvict(t *testing.T) {
	// gen0：恒死（CallTool 返回传输层死亡），首次调用门控——G2 停在生成代里
	gen0 := &gatedDeadClient{
		scriptedClient: scriptedClient{
			tools:    []string{"calc"},
			callErrs: map[string]error{"calc": errors.New("not connected")},
		},
		entered: make(chan struct{}),
		gate:    make(chan struct{}),
	}
	gens := []*scriptedClient{
		nil,                       // 占位：gen0 走独立注入（gatedDeadClient 类型不同）
		{tools: []string{"calc"}}, // G1 自愈换上的健康连接
		{tools: []string{"calc"}}, // 旧实现误杀 gen1 后再拨的一代
	}
	var mu sync.Mutex
	dials := 0
	p := NewPool(ServerConfig{Name: "srv", URL: "http://fake.invalid/mcp"})
	p.dialFn = func(context.Context, ServerConfig) (client.MCPClient, transport.Interface, error) {
		mu.Lock()
		dials++
		n := dials
		mu.Unlock()
		if n == 1 {
			return gen0, nil, nil
		}
		g := gens[n-1]
		if g == nil {
			return nil, nil, errors.New("替身连接已耗尽")
		}
		return g, nil, nil
	}
	defer p.Close()

	tr, err := p.Tools(context.Background(), []ToolSpec{{Server: "srv", AllowAll: true}})
	if err != nil || len(tr.Tools) != 1 {
		t.Fatalf("装配失败: err=%v tools=%d", err, len(tr.Tools))
	}
	it, ok := tr.Tools[0].(tool.InvokableTool)
	if !ok {
		t.Fatal("最外层应为 InvokableTool（自愈包装）")
	}

	// G2：调用停在 gen0 门控 CallTool 里（快照代定型为 gen0）
	g2done := make(chan error, 1)
	go func() {
		_, err := it.InvokableRun(context.Background(), "{}")
		g2done <- err
	}()
	<-gen0.entered

	// G1：同工具并发调用 → 撞 gen0（门控已消耗，立即报传输死亡）→ 整轮自愈
	// （evict gen0 → 拨 gen1 → 重试成功 → swapInner）
	if out, err := it.InvokableRun(context.Background(), "{}"); err != nil || !strings.Contains(out, "ok") {
		t.Fatalf("G1 自愈应成功: out=%q err=%v", out, err)
	}

	// 放行 G2：此刻 inner 已是 gen1 代健康工具，G2 的失败发生在 gen0 快照代
	close(gen0.gate)
	if err := <-g2done; err != nil {
		t.Fatalf("G2 应在健康连接上重试成功: %v", err)
	}

	if gen0.closes != 1 {
		t.Fatalf("gen0（真死连接）应被 G1 的自愈摘除，closes=%d", gen0.closes)
	}
	if gens[1].closes != 0 {
		t.Fatalf("gen1 是 G1 刚换上的健康连接，不得被 G2 的自愈误杀（#99）: closes=%d", gens[1].closes)
	}
	mu.Lock()
	defer mu.Unlock()
	if dials != 2 {
		t.Fatalf("健康连接未被误杀时不应再拨新代: dials=%d（旧实现形态 dials=3）", dials)
	}
}

// gatedDeadClient 死连接假体：首次 CallTool 门控（G2 停在生成代里），后续立即
// 报传输层死亡（G1 由此触发自愈）。⚠️ 不用 sync.Once——Once.Do 会阻塞并发
// 调用方直到首调完成，门控会把 G1 也锁进死等；CAS 武装位只拦第一个到达者。
type gatedDeadClient struct {
	scriptedClient
	entered chan struct{}
	gate    chan struct{}
	armed   atomic.Bool
}

func (c *gatedDeadClient) CallTool(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if c.armed.CompareAndSwap(false, true) {
		close(c.entered)
		<-c.gate
	}
	return c.scriptedClient.CallTool(context.Background(), req)
}
