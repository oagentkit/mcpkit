// 自检 / 目录差集 / 健康聚合 / 错误分类的测试。
//
// 全部用**假 server**（in-process MCP server + 编排型 client 假体），
// 不依赖真实 MCP 服务端、不连网。
package mcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// ---------------------------------------------------------------- 假 server

// scriptedClient 编排型 client 假体：按预设行为回应 ListTools / Ping / CallTool。
//
// 与既有 fakeClient（只编排 Ping 死活）不同，这里要编排三件事：
// 连通性、工具目录、逐工具调用结果。
type scriptedClient struct {
	client.MCPClient // embedding：未实现方法 panic 即「测试不应触达」

	// pingErr 非 nil → 探活失败（连不上）。
	pingErr error
	// listErr 非 nil → 列举失败。
	listErr error
	// tools 目录（nil = 空目录）。
	tools []string
	// callErrs 工具名 → 调用错误（nil = 调用成功）。
	callErrs map[string]error
	// callCount 统计 CallTool 次数。
	callCount int
	// listCount 统计 ListTools 次数。
	listCount int
	// pingCount 统计 Ping 次数。
	pingCount int
}

func (c *scriptedClient) Ping(context.Context) error {
	c.pingCount++
	return c.pingErr
}

func (c *scriptedClient) ListTools(context.Context, mcp.ListToolsRequest) (*mcp.ListToolsResult, error) {
	c.listCount++
	if c.listErr != nil {
		return nil, c.listErr
	}
	res := &mcp.ListToolsResult{}
	for _, n := range c.tools {
		res.Tools = append(res.Tools, mcp.Tool{Name: n})
	}
	return res, nil
}

func (c *scriptedClient) CallTool(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	c.callCount++
	if err, ok := c.callErrs[req.Params.Name]; ok && err != nil {
		return nil, err
	}
	return mcp.NewToolResultText("ok"), nil
}

func (c *scriptedClient) Close() error { return nil }

// newFakeServer 造一个 in-process MCP server（含指定工具）。
func newFakeServer(t *testing.T, toolNames ...string) *server.MCPServer {
	t.Helper()
	srv := server.NewMCPServer("fake-mcp", "1.0.0", server.WithToolCapabilities(false))
	for _, n := range toolNames {
		srv.AddTool(mcp.NewTool(n), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("ok"), nil
		})
	}
	return srv
}

// ---------------------------------------------------------------- 错误分类

func TestClassifyErrorCoversFourKinds(t *testing.T) {
	// ① 连不上：net.Error（超时）
	var ne net.Error
	_ = ne
	timeoutErr := &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}
	if got := ClassifyError(timeoutErr); got != KindUnreachable {
		t.Fatalf("net 错误应归 unreachable，实得 %v", got)
	}
	// 连接被拒
	if got := ClassifyError(syscall.ECONNREFUSED); got != KindUnreachable {
		t.Fatalf("ECONNREFUSED 应归 unreachable，实得 %v", got)
	}
	// ② 协议错：JSON-RPC INTERNAL_ERROR（官方哨兵路径）
	if got := ClassifyError(fmt.Errorf("call: %w", mcp.ErrInternalError)); got != KindProtocol {
		t.Fatalf("ErrInternalError 应归 protocol，实得 %v", got)
	}
	// ③ 工具不存在：JSON-RPC METHOD_NOT_FOUND（官方哨兵路径）
	if got := ClassifyError(fmt.Errorf("call: %w", mcp.ErrMethodNotFound)); got != KindToolMissing {
		t.Fatalf("ErrMethodNotFound 应归 tool_missing，实得 %v", got)
	}
	// ④ 参数错：JSON-RPC INVALID_PARAMS（官方哨兵路径）
	if got := ClassifyError(fmt.Errorf("call: %w", mcp.ErrInvalidParams)); got != KindInvalidArgs {
		t.Fatalf("ErrInvalidParams 应归 invalid_args，实得 %v", got)
	}
}

// TestClassifyDistinguishesMissingFromInvalidArgs 关键语义：
// 「工具不存在」是配置/契约问题，「参数错」是调用问题——处置完全不同。
func TestClassifyDistinguishesMissingFromInvalidArgs(t *testing.T) {
	missing := ClassifyError(fmt.Errorf("call: %w", mcp.ErrMethodNotFound))
	invalid := ClassifyError(fmt.Errorf("call: %w", mcp.ErrInvalidParams))
	if missing == invalid {
		t.Fatal("工具不存在与参数错必须可区分（处置不同）")
	}
	// 重试语义也不同：这两个都不该重试（重试不会让工具出现/参数变合法）。
	if missing.Retryable() || invalid.Retryable() {
		t.Fatal("工具不存在/参数错不应可重试")
	}
	if !KindUnreachable.Retryable() {
		t.Fatal("连不上应可重试")
	}
}

// TestClassifyToolResultPayload isError:true 的 payload 文本判别。
func TestClassifyToolResultPayload(t *testing.T) {
	cases := []struct {
		payload string
		want    ErrorKind
	}{
		{"{\"error\":\"tool not found: read\"}", KindToolMissing},
		{"{\"error\":\"invalid params: missing required field\"}", KindInvalidArgs},
		{"{\"error\":\"some other business failure\"}", KindProtocol},
	}
	for _, c := range cases {
		got := ClassifyError(&ToolResultError{payload: []byte(c.payload)})
		if got != c.want {
			t.Fatalf("payload %q：期望 %v，实得 %v", c.payload, c.want, got)
		}
	}
}

// TestClassifyPreservesErrorChain 归类后仍能 errors.Is/As 穿透。
func TestClassifyPreservesErrorChain(t *testing.T) {
	base := fmt.Errorf("底层: %w", mcp.ErrInvalidParams)
	wrapped := fmt.Errorf("mcp: 调用失败: %w", base)
	c := Classify(wrapped)
	if !errors.Is(c, mcp.ErrInvalidParams) {
		t.Fatal("归类包装不应破坏 errors.Is 链")
	}
	if KindOf(c) != KindInvalidArgs {
		t.Fatalf("归类包装应带得出 invalid_args，实得 %v", KindOf(c))
	}
}

// ---------------------------------------------------------------- 目录差集

func TestDiffBidirectional(t *testing.T) {
	expected := NewCatalog("srv", []string{"read", "write", "delete"})
	actual := NewCatalog("srv", []string{"read", "write", "extra"})
	d := Diff(expected, actual)
	if len(d.Missing) != 1 || d.Missing[0] != "delete" {
		t.Fatalf("Missing 应为 [delete]，实得 %v", d.Missing)
	}
	if len(d.Extra) != 1 || d.Extra[0] != "extra" {
		t.Fatalf("Extra 应为 [extra]，实得 %v", d.Extra)
	}
	if d.Equal() {
		t.Fatal("有差集时 Equal 应为 false")
	}
}

// TestDiffSortedDeterministic 差集有序可复现。
func TestDiffSortedDeterministic(t *testing.T) {
	expected := NewCatalog("srv", []string{"c", "a", "b"})
	actual := NewCatalog("srv", []string{"z", "y"})
	d := Diff(expected, actual)
	if strings.Join(d.Missing, ",") != "a,b,c" {
		t.Fatalf("Missing 应有序，实得 %v", d.Missing)
	}
	if strings.Join(d.Extra, ",") != "y,z" {
		t.Fatalf("Extra 应有序，实得 %v", d.Extra)
	}
}

// TestDiffEmptyBothWays 两向都空才是相等。
func TestDiffEmptyBothWays(t *testing.T) {
	c := NewCatalog("srv", []string{"a", "b"})
	if !Diff(c, c).Equal() {
		t.Fatal("相同目录的差集应为空")
	}
}

// ---------------------------------------------------------------- 自检

// TestSelfcheckStructuredResult 自检返回结构化结果（每工具一条）。
func TestSelfcheckStructuredResult(t *testing.T) {
	cli := &scriptedClient{tools: []string{"read", "write"}}
	sc := NewSelfchecker()
	res := sc.Check(context.Background(), cli, SelfcheckOptions{
		Server:   "srv",
		Expected: NewCatalog("srv", []string{"read", "write", "delete"}),
	})
	if !res.Reachable {
		t.Fatal("server 应可达")
	}
	if len(res.Tools) != 3 {
		t.Fatalf("应有 3 条工具结果（2 在 + 1 缺），实得 %d", len(res.Tools))
	}
	byName := map[string]ToolCheck{}
	for _, tc := range res.Tools {
		byName[tc.Tool] = tc
	}
	if byName["read"].Status != ToolOK {
		t.Fatalf("read 应 ok，实得 %v", byName["read"].Status)
	}
	if byName["delete"].Status != ToolMissing {
		t.Fatalf("delete 应 missing，实得 %v", byName["delete"].Status)
	}
	if byName["delete"].Kind != KindToolMissing {
		t.Fatalf("delete 的分类应是 tool_missing，实得 %v", byName["delete"].Kind)
	}
	// 差集
	if len(res.Diff.Missing) != 1 || res.Diff.Missing[0] != "delete" {
		t.Fatalf("差集 Missing 应为 [delete]，实得 %v", res.Diff.Missing)
	}
}

// TestSelfcheckUnreachableIsNotMissing 核心语义：
// server 连不上时，工具状态是「未知」，**不是** missing。
func TestSelfcheckUnreachableIsNotMissing(t *testing.T) {
	cli := &scriptedClient{listErr: errors.New("connection refused")}
	sc := NewSelfchecker()
	res := sc.Check(context.Background(), cli, SelfcheckOptions{
		Server:   "srv",
		Expected: NewCatalog("srv", []string{"read", "write"}),
	})
	if res.Reachable {
		t.Fatal("列举失败应判不可达")
	}
	for _, tc := range res.Tools {
		if tc.Status == ToolMissing {
			t.Fatalf("server 不可达时工具不该判 missing（那是契约问题）：%s", tc.Tool)
		}
		if tc.Status != ToolUnreachable {
			t.Fatalf("应判 unreachable，实得 %v", tc.Status)
		}
	}
}

// TestSelfcheckTTLCache TTL 生效：TTL 内不重复拉目录。
func TestSelfcheckTTLCache(t *testing.T) {
	cli := &scriptedClient{tools: []string{"read"}}
	sc := NewSelfchecker()
	opts := SelfcheckOptions{Server: "srv", TTL: time.Minute}

	first := sc.Check(context.Background(), cli, opts)
	if first.Cached {
		t.Fatal("首次不应是缓存")
	}
	second := sc.Check(context.Background(), cli, opts)
	if !second.Cached {
		t.Fatal("TTL 内第二次应命中缓存")
	}
	if cli.listCount != 1 {
		t.Fatalf("TTL 内只应拉一次目录，实拉 %d 次", cli.listCount)
	}
}

// TestSelfcheckTTLExpiry TTL 过期后重新拉。
func TestSelfcheckTTLExpiry(t *testing.T) {
	cli := &scriptedClient{tools: []string{"read"}}
	sc := NewSelfchecker()
	now := time.Now()
	sc.now = func() time.Time { return now }
	sc.Check(context.Background(), cli, SelfcheckOptions{Server: "srv", TTL: time.Minute})

	now = now.Add(2 * time.Minute)
	res := sc.Check(context.Background(), cli, SelfcheckOptions{Server: "srv", TTL: time.Minute})
	if res.Cached {
		t.Fatal("TTL 过期后不应命中缓存")
	}
	if cli.listCount != 2 {
		t.Fatalf("过期后应重新拉目录，实拉 %d 次", cli.listCount)
	}
}

// TestSelfcheckProbeToolsDetectsUncallable 探针能发现「工具在但调不通」。
func TestSelfcheckProbeToolsDetectsUncallable(t *testing.T) {
	cli := &scriptedClient{
		tools: []string{"read", "broken"},
		callErrs: map[string]error{
			"broken": fmt.Errorf("call: %w", mcp.ErrInvalidParams),
		},
	}
	sc := NewSelfchecker()
	res := sc.Check(context.Background(), cli, SelfcheckOptions{
		Server: "srv", ProbeTools: true,
	})
	byName := map[string]ToolCheck{}
	for _, tc := range res.Tools {
		byName[tc.Tool] = tc
	}
	if byName["broken"].Status != ToolFailed {
		t.Fatalf("broken 应 failed，实得 %v", byName["broken"].Status)
	}
	if byName["broken"].Kind != KindInvalidArgs {
		t.Fatalf("broken 的分类应是 invalid_args，实得 %v", byName["broken"].Kind)
	}
	if byName["read"].Status != ToolOK {
		t.Fatalf("read 应 ok，实得 %v", byName["read"].Status)
	}
}

// ---------------------------------------------------------------- 健康聚合

// TestAggregateThreeStates 三态判定。
func TestAggregateThreeStates(t *testing.T) {
	// healthy：全通
	h := Aggregate([]SelfcheckResult{{
		Server: "a", Reachable: true,
		Tools: []ToolCheck{{Tool: "read", Status: ToolOK}},
	}})
	if h.State != HealthHealthy {
		t.Fatalf("期望 healthy，实得 %v", h.State)
	}
	if h.HealthyCount != 1 {
		t.Fatalf("healthy 计数应为 1，实得 %d", h.HealthyCount)
	}

	// degraded：server 活着但工具缺失
	h = Aggregate([]SelfcheckResult{{
		Server: "a", Reachable: true,
		Tools: []ToolCheck{
			{Tool: "read", Status: ToolOK},
			{Tool: "delete", Status: ToolMissing, Kind: KindToolMissing,
				Reason: "声明要用的工具 \"delete\" 不在 server 目录里"},
		},
	}})
	if h.State != HealthDegraded {
		t.Fatalf("期望 degraded，实得 %v", h.State)
	}
	if h.DegradedCount != 1 {
		t.Fatalf("degraded 计数应为 1，实得 %d", h.DegradedCount)
	}

	// down：server 连不上
	h = Aggregate([]SelfcheckResult{{
		Server: "a", Reachable: false, Kind: KindUnreachable,
		Err: errors.New("connection refused"),
	}})
	if h.State != HealthDown {
		t.Fatalf("期望 down，实得 %v", h.State)
	}
	if h.DownCount != 1 {
		t.Fatalf("down 计数应为 1，实得 %d", h.DownCount)
	}
}

// TestAggregateToolMissingIsNotDown 核心语义（下游踩过的坑）：
// 「工具缺失」是 degraded，**不是** down——server 还活着还在服务。
func TestAggregateToolMissingIsNotDown(t *testing.T) {
	h := Aggregate([]SelfcheckResult{{
		Server: "fs", Reachable: true,
		Tools: []ToolCheck{
			{Tool: "read", Status: ToolOK},
			{Tool: "write", Status: ToolMissing, Kind: KindToolMissing,
				Reason: "声明要用的工具 \"write\" 不在 server 目录里"},
		},
	}})
	if h.State != HealthDegraded {
		t.Fatalf("工具缺失应判 degraded（server 活着），实得 %v", h.State)
	}
	if h.State == HealthDown {
		t.Fatal("工具缺失绝不能判 down（依赖故障会被放大成平台故障）")
	}
	if !h.OK() {
		t.Fatal("degraded 仍算可服务")
	}
	// 还能看出是哪个 server 的哪个工具
	if len(h.Issues) != 1 {
		t.Fatalf("应有 1 条 issue，实得 %d", len(h.Issues))
	}
	if h.Issues[0].Server != "fs" || h.Issues[0].Tool != "write" {
		t.Fatalf("issue 应指出 fs/write，实得 %+v", h.Issues[0])
	}
	if h.Issues[0].Kind != KindToolMissing {
		t.Fatalf("issue 分类应是 tool_missing，实得 %v", h.Issues[0].Kind)
	}
}

// TestAggregateUnreachableServerNotCountedAsMissingTools 连带语义：
// server 挂了时，它有多少工具**不能**被算成「N 个工具缺失」。
func TestAggregateUnreachableServerNotCountedAsMissingTools(t *testing.T) {
	h := Aggregate([]SelfcheckResult{{
		Server: "fs", Reachable: false, Kind: KindUnreachable,
		Err: errors.New("connection refused"),
		Tools: []ToolCheck{
			{Tool: "read", Status: ToolUnreachable, Kind: KindUnreachable},
			{Tool: "write", Status: ToolUnreachable, Kind: KindUnreachable},
		},
	}})
	if h.State != HealthDown {
		t.Fatalf("server 不可达应判 down，实得 %v", h.State)
	}
	// issue 只有 server 级一条（连不上），不是每工具一条
	if len(h.Issues) != 1 {
		t.Fatalf("server 级问题应只报 1 条 issue（不是每工具一条），实得 %d", len(h.Issues))
	}
	if h.Issues[0].Tool != "" {
		t.Fatalf("server 级 issue 不该带工具名，实得 %q", h.Issues[0].Tool)
	}
}

// TestAggregatePartialDownTakesWorst 任一 server 挂 → 整体 down，
// 但 Issues 里能看出是哪个 server。
func TestAggregatePartialDownTakesWorst(t *testing.T) {
	h := Aggregate([]SelfcheckResult{
		{Server: "ok-srv", Reachable: true,
			Tools: []ToolCheck{{Tool: "read", Status: ToolOK}}},
		{Server: "dead-srv", Reachable: false, Kind: KindUnreachable,
			Err: errors.New("connection refused")},
	})
	if h.State != HealthDown {
		t.Fatalf("有 server 挂应判 down，实得 %v", h.State)
	}
	if h.HealthyCount != 1 || h.DownCount != 1 {
		t.Fatalf("计数应 healthy=1 down=1，实得 h=%d d=%d", h.HealthyCount, h.DownCount)
	}
	var found bool
	for _, i := range h.Issues {
		if i.Server == "dead-srv" {
			found = true
		}
	}
	if !found {
		t.Fatal("应能看出是 dead-srv 出的问题")
	}
}

// TestAggregateIssuesObservable 判据可观察：能看出哪个 server 的哪个工具。
func TestAggregateIssuesObservable(t *testing.T) {
	h := Aggregate([]SelfcheckResult{
		{Server: "a", Reachable: true,
			Tools: []ToolCheck{{Tool: "t1", Status: ToolMissing, Kind: KindToolMissing,
				Reason: "缺 t1"}}},
		{Server: "b", Reachable: true,
			Tools: []ToolCheck{{Tool: "t2", Status: ToolFailed, Kind: KindInvalidArgs,
				Reason: "参数错"}}},
	})
	if len(h.Issues) != 2 {
		t.Fatalf("应有 2 条 issue，实得 %d", len(h.Issues))
	}
	if h.Issues[0].Server != "a" || h.Issues[0].Tool != "t1" {
		t.Fatalf("issue[0] 应为 a/t1，实得 %+v", h.Issues[0])
	}
	if h.Issues[1].Server != "b" || h.Issues[1].Tool != "t2" {
		t.Fatalf("issue[1] 应为 b/t2，实得 %+v", h.Issues[1])
	}
}

// ---------------------------------------------------------------- 端到端

// TestSelfcheckWithRealInProcessServer 用真 in-process MCP server 端到端跑一遍
// （仍不连网、不起外部进程）。
func TestSelfcheckWithRealInProcessServer(t *testing.T) {
	srv := newFakeServer(t, "read", "write")
	cli := mustInProcessClient(t, srv)
	// in-process client 与真实 client 一样需先 Initialize 才可用
	// （未 Initialize 时 Ping/ListTools 都报 "client not initialized"）。
	initRes, err := cli.Initialize(context.Background(), mcp.InitializeRequest{
		Params: mcp.InitializeParams{ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo: mcp.Implementation{Name: "selfcheck-test", Version: "0.0.1"}},
	})
	if err != nil {
		t.Fatalf("Initialize 失败: %v", err)
	}
	_ = initRes
	sc := NewSelfchecker()
	res := sc.Check(context.Background(), cli, SelfcheckOptions{
		Server:   "inproc",
		Expected: NewCatalog("inproc", []string{"read", "write", "delete"}),
	})
	if !res.Reachable {
		t.Fatal("in-process server 应可达")
	}
	if len(res.Catalog.Tools) != 2 {
		t.Fatalf("目录应有 2 个工具，实得 %v", res.Catalog.Tools)
	}
	if len(res.Diff.Missing) != 1 || res.Diff.Missing[0] != "delete" {
		t.Fatalf("缺 delete，实得 %v", res.Diff.Missing)
	}
	h := Aggregate([]SelfcheckResult{res})
	if h.State != HealthDegraded {
		t.Fatalf("应 degraded，实得 %v", h.State)
	}
}

// TestSelfcheckInitializeOption 裸 client（未握手）配 Initialize=true 时可自检成功。
//
// 覆盖「诊断工具不该因握手缺失给出错误结论」这条。
func TestSelfcheckInitializeOption(t *testing.T) {
	srv := newFakeServer(t, "read")
	cli := mustInProcessClient(t, srv)
	sc := NewSelfchecker()

	// 不 initialize：协议请求被拒 → 判不可达
	res := sc.Check(context.Background(), cli, SelfcheckOptions{Server: "s"})
	if res.Reachable {
		t.Fatal("未握手且未开 Initialize 时应判不可达")
	}

	// initialize 开关打开 → 走通
	res = sc.Check(context.Background(), cli, SelfcheckOptions{Server: "s2", Initialize: true})
	if !res.Reachable {
		t.Fatalf("Initialize=true 后应可达: %v", res.Err)
	}
	if len(res.Catalog.Tools) != 1 || res.Catalog.Tools[0] != "read" {
		t.Fatalf("目录应有 read，实得 %v", res.Catalog.Tools)
	}
}
