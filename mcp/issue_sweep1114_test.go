package mcp

// 2026-11-14 issue 清零战回归测试（#141-#150）：每条用例钉一个失效形态，
// 标题带 issue 号——回归时按号检索。

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// ---------------------------------------------------------------- #143

// budgetProbeClient 探针预算假体：目录三工具，slow 调用阻塞到 ctx 完结
// （确定性吃光整轮预算——不赌 sleep 时长），其余调用按 fastErr 回错。
// slowBlock=false 时 slow 也走 fastErr（对照组：预算充足下的真失败）。
type budgetProbeClient struct {
	client.MCPClient // embedding：未编排的方法 panic 即「测试不应触达」
	fastErr          error
	slowBlock        bool
}

func (c *budgetProbeClient) Ping(context.Context) error { return nil }

func (c *budgetProbeClient) ListTools(context.Context, mcp.ListToolsRequest) (*mcp.ListToolsResult, error) {
	return &mcp.ListToolsResult{Tools: []mcp.Tool{
		{Name: "fast1"}, {Name: "slow"}, {Name: "fast2"},
	}}, nil
}

func (c *budgetProbeClient) CallTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if req.Params.Name == "slow" && c.slowBlock {
		<-ctx.Done() // 探针预算在本调用中途耗尽
		return nil, ctx.Err()
	}
	return nil, c.fastErr
}

func toolStatusOf(r SelfcheckResult, name string) ToolCheck {
	for _, tc := range r.Tools {
		if tc.Tool == name {
			return tc
		}
	}
	return ToolCheck{}
}

// TestProbeToolsBudgetExhaustedIsUnreachable_Issue143 整轮探针预算耗尽后，
// 剩余工具与中途超时的工具必须标 ToolUnreachable（状态未知）而非 ToolFailed
// ——本地预算不够 ≠ 工具坏了，两者处置动作不同（调预算 vs 修工具）；
// ToolFailed + KindUnreachable 的组合还会让 Status 与 Kind 自相矛盾。
func TestProbeToolsBudgetExhaustedIsUnreachable_Issue143(t *testing.T) {
	cli := &budgetProbeClient{slowBlock: true}
	sc := NewSelfchecker()
	res := sc.Check(context.Background(), cli, SelfcheckOptions{
		Server:     "srv",
		Timeout:    50 * time.Millisecond, // slow 阻塞到 ctx 完结，确定性耗尽
		ProbeTools: true,
	})

	if !res.Reachable {
		t.Fatalf("列举成功应 Reachable，Err=%v", res.Err)
	}
	fast1 := toolStatusOf(res, "fast1")
	if fast1.Status != ToolOK {
		t.Fatalf("预算耗尽前的成功探针应 ok, got %s (%s)", fast1.Status, fast1.Reason)
	}
	slow := toolStatusOf(res, "slow")
	if slow.Status != ToolUnreachable || slow.Kind != KindUnreachable {
		t.Fatalf("预算中途耗尽的探针应 unreachable/unreachable, got %s/%s (%s)", slow.Status, slow.Kind, slow.Reason)
	}
	if slow.Reason == "" || !strings.Contains(slow.Reason, "预算耗尽") {
		t.Fatalf("slow 应声明预算耗尽, got %q", slow.Reason)
	}
	fast2 := toolStatusOf(res, "fast2")
	if fast2.Status != ToolUnreachable || fast2.Kind != KindUnreachable {
		t.Fatalf("预算耗尽后的未核实探针应 unreachable/unreachable, got %s/%s (%s)", fast2.Status, fast2.Kind, fast2.Reason)
	}
	if !strings.Contains(fast2.Reason, "未逐个核实") {
		t.Fatalf("fast2 应声明未逐个核实, got %q", fast2.Reason)
	}
	if fast2.Err == nil || !errors.Is(fast2.Err, context.DeadlineExceeded) {
		t.Fatalf("fast2 应透传预算错误供 errors.Is 穿透, got %v", fast2.Err)
	}
}

// 对照组：预算充足时，真失败的探针仍标 ToolFailed（修法不得误伤正常判定）。
func TestProbeToolsRealFailureStillFailed_Issue143(t *testing.T) {
	cli := &budgetProbeClient{fastErr: errors.New("boom")}
	sc := NewSelfchecker()
	res := sc.Check(context.Background(), cli, SelfcheckOptions{
		Server:     "srv",
		Timeout:    5 * time.Second,
		ProbeTools: true,
	})
	for _, n := range []string{"fast1", "slow", "fast2"} {
		tc := toolStatusOf(res, n)
		if tc.Status != ToolFailed {
			t.Fatalf("工具 %s 真失败应标 failed, got %s (%s)", n, tc.Status, tc.Reason)
		}
	}
}

// ---------------------------------------------------------------- #144

// TestToolResultPayloadDefaultIsUnknown_Issue144 isError:true 业务失败的
// payload 无明确信号时必须落 KindUnknown（保守不重试），不得落 KindProtocol
// （Retryable==true 会把确定性业务失败当抖动盲重试、处置面错推给「修 server」）
// ——oagentkit/mcpkit#144。
func TestToolResultPayloadDefaultIsUnknown_Issue144(t *testing.T) {
	// 确定性业务失败（无工具不存在/参数错信号）
	for _, payload := range []string{
		`pod not found in cluster`,
		`permission denied for resource`,
		`quota exceeded for project`,
	} {
		if k := classifyToolResultPayload(payload); k != KindUnknown {
			t.Fatalf("payload %q 应落 unknown, got %s", payload, k)
		}
		if classifyToolResultPayload(payload).Retryable() {
			t.Fatalf("payload %q 判 unknown 后不得可重试", payload)
		}
	}
	// 端到端：ToolResultError 经 ClassifyError 同口径（isError:true 唯一入口）
	tre := &ToolResultError{payload: []byte(`{"content":[{"type":"text","text":"pod not found"}]}`)}
	if k := ClassifyError(tre); k != KindUnknown {
		t.Fatalf("ToolResultError 无信号 payload 应落 unknown, got %s", k)
	}
	// 明确信号面不回退：工具不存在 / 参数错照常细分
	if k := classifyToolResultPayload("tool not found: fs_read"); k != KindToolMissing {
		t.Fatalf("tool not found 应落 tool_missing, got %s", k)
	}
	if k := classifyToolResultPayload("invalid params: path"); k != KindInvalidArgs {
		t.Fatalf("invalid params 应落 invalid_args, got %s", k)
	}
}

// ---------------------------------------------------------------- #145

// TestDisclosureWaiterCtxTimeout_Issue145 激活器挂死时，等待者必须随自身 ctx
// 超时退出（可重态 + ctx.Err()），不得不可取消地阻塞在 call.done 上——激活是
// 宿主外部动作（reload/热刷新），可慢可挂，门禁必须留逃生门。等待者先退是
// 安全的：失败代由 leader 收尾，激活成功后已入集。假体走 testing/synctest
// 假时钟（无真实 sleep）——oagentkit/mcpkit#145。
func TestDisclosureWaiterCtxTimeout_Issue145(t *testing.T) {
	var (
		waiterVerdict  DisclosureVerdict
		waiterReturned bool
	)
	// 假体与信号 channel 全部在 bubble 内构造：synctest 只对 bubble 内创建的
	// channel/时钟判「durable 阻塞」——挂死在 bubble 外 channel 上的 goroutine
	// 会让假时钟永不推进（本轮实现时踩过：外部 release 卡住 leader，bubble 失聪）。
	synctest.Test(t, func(*testing.T) {
		entered := make(chan struct{})
		release := make(chan struct{})
		g := NewDisclosureGate(newGateCatalog(), nil,
			WithDisclosureActivator(func(_ context.Context, _ string) error {
				close(entered)
				<-release // 挂死模拟：直到测试放行
				return nil
			}))
		leaderRes := make(chan DisclosureVerdict, 1)
		go func() { leaderRes <- g.Check(context.Background(), "write") }()
		<-entered // 激活器已挂死

		waiterCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		waiterRes := make(chan DisclosureVerdict, 1)
		go func() { waiterRes <- g.Check(waiterCtx, "write") }()
		select {
		case v := <-waiterRes:
			waiterVerdict, waiterReturned = v, true
		case <-time.After(2 * time.Second): // bubble 假时钟：到点即「等待者未退」
			waiterReturned = false
		}
		close(release) // 放 leader 收尾：验证先退的等待者不破坏门禁状态
		<-leaderRes
	})
	if !waiterReturned {
		t.Fatal("激活器挂死时等待者未随 ctx 超时退出（oagentkit/mcpkit#145）")
	}
	if !waiterVerdict.Retryable() || waiterVerdict.Activated {
		t.Fatalf("等待者超时应保可重态: %+v", waiterVerdict)
	}
	if !errors.Is(waiterVerdict.Err, context.DeadlineExceeded) {
		t.Fatalf("等待者应透传 ctx.Err(), got %v", waiterVerdict.Err)
	}
}

// TestDisclosureLeaderCtxDeadSkipsActivation_Issue145 leader 的 ctx 已死时不得
// 启动激活器（不给已取消的调用放大等待面）；inflight 必须照常收尾（等待者/
// 后续 Check 起新代），门禁可恢复——oagentkit/mcpkit#145。
func TestDisclosureLeaderCtxDeadSkipsActivation_Issue145(t *testing.T) {
	calls := 0
	g := NewDisclosureGate(newGateCatalog(), nil,
		WithDisclosureActivator(func(_ context.Context, _ string) error {
			calls++
			return nil
		}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	v := g.Check(ctx, "write")
	if !v.Retryable() || v.Activated {
		t.Fatalf("ctx 已死应保可重态且不激活: %+v", v)
	}
	if !errors.Is(v.Err, context.Canceled) {
		t.Fatalf("应透传 context.Canceled, got %v", v.Err)
	}
	if calls != 0 {
		t.Fatalf("ctx 已死不得启动激活器, calls=%d", calls)
	}
	if got := g.Loaded(); len(got) != 0 {
		t.Fatalf("取消代不得入已披露集: %v", got)
	}
	// inflight 已收尾：好 ctx 的下一次 Check 起新代并成功（泄漏态下这里会等死）
	if v2 := g.Check(t.Context(), "write"); !v2.Retryable() || !v2.Activated {
		t.Fatalf("后续 Check 应起新代激活成功: %+v", v2)
	}
	if calls != 1 {
		t.Fatalf("新代应执行激活器一次, calls=%d", calls)
	}
}

// TestDisclosureActivatorReceivesCallerCtx_Issue145 激活器拿到的是 Check 调用方
// ctx——调用方取消时挂在外部资源上的激活可感知并中止（Err 链透传 Canceled，
// 目录事实不变仍可重试）——oagentkit/mcpkit#145。
func TestDisclosureActivatorReceivesCallerCtx_Issue145(t *testing.T) {
	entered := make(chan struct{})
	var gotCtx context.Context
	g := NewDisclosureGate(newGateCatalog(), nil,
		WithDisclosureActivator(func(ctx context.Context, _ string) error {
			gotCtx = ctx
			close(entered)
			<-ctx.Done() // 激活器在长操作上响应调用方取消
			return ctx.Err()
		}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	leader := make(chan DisclosureVerdict, 1)
	go func() { leader <- g.Check(ctx, "write") }()
	<-entered
	cancel()
	lv := <-leader
	if !lv.Retryable() || lv.Activated {
		t.Fatalf("激活被取消应保可重态: %+v", lv)
	}
	if !errors.Is(lv.Err, context.Canceled) {
		t.Fatalf("激活器取消错误应穿透 Err 链, got %v", lv.Err)
	}
	if gotCtx == nil {
		t.Fatal("激活器应拿到调用方 ctx")
	}
}

// ---------------------------------------------------------------- #146

// genListClient 目录代际假体：ListTools 可阻塞（entered/release 拉住列举窗口），
// 用于确定性复现「列举期间连接换代」。
type genListClient struct {
	client.MCPClient // embedding：未编排的方法 panic 即「测试不应触达」
	tools            []string
	entered          chan struct{} // 非 nil：进入 ListTools 时关闭
	release          chan struct{} // 非 nil：等它放行再返回
}

func (c *genListClient) Initialize(context.Context, mcp.InitializeRequest) (*mcp.InitializeResult, error) {
	return &mcp.InitializeResult{}, nil
}

func (c *genListClient) Close() error { return nil }

func (c *genListClient) ListTools(context.Context, mcp.ListToolsRequest) (*mcp.ListToolsResult, error) {
	if c.entered != nil {
		close(c.entered)
		<-c.release
	}
	res := &mcp.ListToolsResult{}
	for _, n := range c.tools {
		res.Tools = append(res.Tools, mcp.Tool{Name: n})
	}
	return res, nil
}

func catalogNames(entries []toolEntry) []string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.tool.Name)
	}
	return names
}

// TestCatalogForWritesOnlyCurrentGeneration_Issue146 catalogFor 落缓存必须绑定
// 连接代际：列举期间连接被 evict/redial 换代时，旧连接的目录不得写到新连接上
// （列表不持 lease、connState 跨重建复用、registerConn 不清目录——窗口结构性
// 存在；写进去的旧目录让热换装的工具面变化静默不可见，probeFingerprint 比对
// 基准也被污染）——oagentkit/mcpkit#146。
func TestCatalogForWritesOnlyCurrentGeneration_Issue146(t *testing.T) {
	cfg := ServerConfig{Name: "srv", URL: "http://seed.invalid/mcp"}
	p := NewPool(cfg)
	c1 := &genListClient{tools: []string{"stale-tool"}, entered: make(chan struct{}), release: make(chan struct{})}
	p.dialFn = func(context.Context, ServerConfig) (client.MCPClient, transport.Interface, error) {
		return c1, nil, nil
	}

	// G1：建连（注册 c1）→ catalogFor 列举（阻塞在窗口内）
	g1done := make(chan struct{})
	go func() {
		defer close(g1done)
		cli, err := p.client(t.Context(), cfg)
		if err != nil {
			t.Errorf("G1 建连失败: %v", err)
			return
		}
		// G1 返回的列举结果属于已死连接——照常返回给调用方（工具绑它自己
		// 持有的连接，传输层死亡由自愈接管），但**不得**落缓存。
		entries, err := p.catalogFor(t.Context(), cfg, cli)
		if err != nil {
			t.Errorf("G1 列举失败: %v", err)
			return
		}
		if got := catalogNames(entries); len(got) != 1 || got[0] != "stale-tool" {
			t.Errorf("G1 应拿到自己列举的结果, got %v", got)
		}
	}()
	<-c1.entered // G1 已进入列举（此时 st.cli == c1）

	// 列举窗口内换代：evict c1（detach 清 catalog/cli）→ 注册 c2
	p.evict("srv", c1)
	st := p.conn("srv")
	if st == nil {
		t.Fatal("connState 应已存在")
	}
	c2 := &genListClient{tools: []string{"fresh-tool"}}
	if _, err := p.registerConn("srv", st, c2, nil, ""); err != nil {
		t.Fatalf("注册 c2 失败: %v", err)
	}

	close(c1.release) // 放 G1 收尾：其写缓存必须因代际比对被跳过
	<-g1done

	st.mu.Lock()
	leftover := st.catalog
	st.mu.Unlock()
	if leftover != nil {
		t.Fatalf("换代后的旧目录不得落缓存, got %v", catalogNames(leftover))
	}

	// c2 的下一次 catalogFor 正常重列并落缓存
	entries, err := p.catalogFor(t.Context(), cfg, c2)
	if err != nil {
		t.Fatalf("c2 列举失败: %v", err)
	}
	if got := catalogNames(entries); len(got) != 1 || got[0] != "fresh-tool" {
		t.Fatalf("新连接应重列出新工具面, got %v", got)
	}
	st.mu.Lock()
	cached := st.catalog
	st.mu.Unlock()
	if len(cached) != 1 || cached[0].tool.Name != "fresh-tool" {
		t.Fatalf("新连接的目录应落缓存, got %v", catalogNames(cached))
	}
}

// TestRegisterConnClearsLeftoverCatalog_Issue146 防御层：连接注册必须清掉
// 残留目录（detach 后、注册前 catalogFor 并发写入的窗口产物）——连接换了
// 目录必须重列——oagentkit/mcpkit#146。
func TestRegisterConnClearsLeftoverCatalog_Issue146(t *testing.T) {
	p := NewPool(ServerConfig{Name: "srv", URL: "http://seed.invalid/mcp"})
	st := p.connLocked("srv")
	st.mu.Lock()
	st.catalog = []toolEntry{{tool: mcp.Tool{Name: "ghost"}}}
	st.mu.Unlock()
	if _, err := p.registerConn("srv", st, &genListClient{}, nil, ""); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.catalog != nil {
		t.Fatalf("registerConn 必须清残留目录, got %v", catalogNames(st.catalog))
	}
}

// ---------------------------------------------------------------- #147

// TestAggregateEmptyInputIsUnknown_Issue147 零个 server 参与聚合必须返回
// unknown（不是 healthy）——「没查」与「查了都好」在告警面必须可区分，空输入
// 判 healthy 会让巡检脚本/上游过滤故障静默变绿——oagentkit/mcpkit#147。
func TestAggregateEmptyInputIsUnknown_Issue147(t *testing.T) {
	for name, in := range map[string][]SelfcheckResult{
		"nil":   nil,
		"empty": {},
	} {
		h := Aggregate(in)
		if h.State != HealthUnknown {
			t.Fatalf("%s: 空输入应返回 unknown, got %s", name, h.State)
		}
		if h.Total != 0 || h.OK() || h.Healthy() || h.Down() || h.Degraded() {
			t.Fatalf("%s: unknown 态下 Total=0 且四态谓词皆假, got %+v", name, h)
		}
	}
	// 全绿（非空）仍判 healthy：修法不得误伤正常判定
	h := Aggregate([]SelfcheckResult{{Server: "srv", Reachable: true}})
	if h.State != HealthHealthy {
		t.Fatalf("单 server 全绿应仍判 healthy, got %s", h.State)
	}
}

// ---------------------------------------------------------------- #149

// TestReapIdleNotifiesOnReapNotError_Issue149 闲置回收是设计内行为（下次调用
// 透明重建），必须走专属 OnReap 通道，不得借道 OnError——按失败率消费 OnError
// 的宿主（告警/指标）此前被周期性回收灌满错误面，真失败信号被淹没
// ——oagentkit/mcpkit#149。
func TestReapIdleNotifiesOnReapNotError_Issue149(t *testing.T) {
	var (
		mu       sync.Mutex
		errs     []string
		reaps    []string
		reapIdle time.Duration
	)
	p := NewPool()
	old := &fakeClient{}
	seedFake(p, "old", old)
	setConn(p, "old", func(st *connState) { st.lastUsed = time.Now().Add(-10 * time.Minute) })
	p.OnError = func(server string, err error) {
		mu.Lock()
		errs = append(errs, server+": "+err.Error())
		mu.Unlock()
	}
	p.OnReap = func(server string, idle time.Duration) {
		mu.Lock()
		reaps = append(reaps, server)
		reapIdle = idle
		mu.Unlock()
	}

	if n := p.ReapIdle(time.Minute); n != 1 {
		t.Fatalf("应回收 1 条: %d", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(errs) != 0 {
		t.Fatalf("回收不得经 OnError 发射, got %v", errs)
	}
	if len(reaps) != 1 || reaps[0] != "old" {
		t.Fatalf("回收应经 OnReap 点名 server, got %v", reaps)
	}
	if reapIdle != time.Minute {
		t.Fatalf("OnReap 应携带闲置阈值, got %s", reapIdle)
	}
}

// ---------------------------------------------------------------- #150

// captureInitClient initialize 握手捕获假体：记下自报的 ClientInfo。
type captureInitClient struct {
	client.MCPClient
	got mcp.Implementation
}

func (c *captureInitClient) Initialize(_ context.Context, req mcp.InitializeRequest) (*mcp.InitializeResult, error) {
	c.got = req.Params.ClientInfo
	return &mcp.InitializeResult{}, nil
}

func (c *captureInitClient) Close() error { return nil }

// TestInitializeClientIdentity_Issue150 initialize 客户端身份必须可归因：缺省
// mcpkit@包版本（发版 ldflags 注入锚点），宿主经 ServerConfig.ClientInfo 覆写
// 时以宿主为准——恒定伪名（"agentkit"/"1.0"）让 server 侧归因/审计失去锚点、
// mcpkit 升版在 server 侧不可见——oagentkit/mcpkit#150。
func TestInitializeClientIdentity_Issue150(t *testing.T) {
	// 缺省身份
	p := NewPool(ServerConfig{Name: "srv", URL: "http://seed.invalid/mcp"})
	var def captureInitClient
	p.dialFn = func(context.Context, ServerConfig) (client.MCPClient, transport.Interface, error) {
		return &def, nil, nil
	}
	if _, err := p.client(t.Context(), ServerConfig{Name: "srv", URL: "x"}); err != nil {
		t.Fatalf("建连失败: %v", err)
	}
	if def.got.Name != "mcpkit" || def.got.Version != Version {
		t.Fatalf("缺省身份应 mcpkit@%s, got %+v", Version, def.got)
	}

	// 宿主覆写优先
	host := &mcp.Implementation{Name: "platform-x", Version: "9.9.0"}
	cfgHost := ServerConfig{Name: "srv", URL: "http://seed.invalid/mcp", ClientInfo: host}
	p2 := NewPool(cfgHost)
	var cap2 captureInitClient
	p2.dialFn = func(context.Context, ServerConfig) (client.MCPClient, transport.Interface, error) {
		return &cap2, nil, nil
	}
	if _, err := p2.client(t.Context(), cfgHost); err != nil {
		t.Fatalf("建连失败: %v", err)
	}
	if cap2.got.Name != "platform-x" || cap2.got.Version != "9.9.0" {
		t.Fatalf("宿主 ClientInfo 应原样自报, got %+v", cap2.got)
	}
}
