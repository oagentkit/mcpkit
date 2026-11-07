package mcp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// schemaAnyOf 含 anyOf 联合的出参声明（terminal 三形态同构的最小样本）。
const schemaAnyOf = `{
  "type": "object",
  "anyOf": [
    {"type": "object", "required": ["success"], "properties": {"success": {"const": true}}},
    {"type": "object", "required": ["error"], "properties": {"error": {"type": "string"}}}
  ]
}`

// rawFidelityServer 起真实 streamable HTTP server：工具带 anyOf 输入/输出声明
// （原文直设——类型化 Tool 结构表达不了联合）。
func rawFidelityServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := server.NewMCPServer("fid", "1.0.0")
	srv.AddTool(mcp.NewTool("shell",
		mcp.WithDescription("fidelity probe"),
		mcp.WithRawOutputSchema(json.RawMessage(schemaAnyOf)),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	ts := httptest.NewServer(server.NewStreamableHTTPServer(srv))
	t.Cleanup(ts.Close)
	return ts
}

// dialFidelity 建连并返回 (client, transport)——与生产 dialStdio/dialHTTP 同构
// （transport 句柄不进 client）。
func dialFidelity(t *testing.T, ts *httptest.Server) (*client.Client, transport.Interface) {
	t.Helper()
	tr, err := transport.NewStreamableHTTP(ts.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	cli := client.NewClient(tr)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := cli.Start(ctx); err != nil {
		t.Fatal(err)
	}
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "fid-test", Version: "1.0"}
	if _, err := cli.Initialize(ctx, initReq); err != nil {
		t.Fatal(err)
	}
	return cli, tr
}

// TestRawToolsListFidelity 保真对照（v0.12.5 核心）：raw 通道 anyOf 双 schema 原样
// 存活；类型化通道同 server 同工具 outputSchema 有损（Type 空）——宁缺勿假的依据。
func TestRawToolsListFidelity(t *testing.T) {
	ts := rawFidelityServer(t)
	cli, tr := dialFidelity(t, ts)

	raw, err := fetchRawToolsList(context.Background(), tr, cli.ProtocolVersion())
	if err != nil {
		t.Fatalf("raw fetch: %v", err)
	}
	entries, err := parseRawToolsList(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(entries) != 1 || entries[0].tool.Name != "shell" {
		t.Fatalf("目录异常: %+v", entries)
	}
	if !strings.Contains(string(entries[0].outputSchema), `"anyOf"`) {
		t.Errorf("raw outputSchema 丢 anyOf: %s", entries[0].outputSchema)
	}
	if !entries[0].hints.Destructive {
		t.Errorf("注解派生异常: %+v", entries[0].hints)
	}

	// 类型化对照：同 server 的 outputSchema 经 mcp-go Tool 反序列化后 anyOf 静默
	// 丢失（type 键还在、联合没了——回灌 marshal 无 anyOf 即有损实锤）。
	res, err := cli.ListTools(context.Background(), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 1 {
		t.Fatalf("typed 对照目录异常: %d", len(res.Tools))
	}
	typedBack, merr := json.Marshal(res.Tools[0].OutputSchema)
	if merr != nil {
		t.Fatal(merr)
	}
	if strings.Contains(string(typedBack), "anyOf") {
		t.Errorf("typed 通道应静默丢 anyOf，实得: %s", typedBack)
	}

	// 目录条目 → invokableTool → 可选接口读取
	inv := newInvokableTool(cli, entries[0], entries[0].tool.Name, nil, false)
	got, ok := OutputSchemaOf(inv)
	if !ok || !strings.Contains(string(got), `"anyOf"`) {
		t.Errorf("invokableTool outputSchema 异常: %s", got)
	}
	// 装饰器转发：降级包装不吞 schema
	wrapped := WrapErrorAsObservation([]tool.BaseTool{inv})
	if got, ok := OutputSchemaOf(wrapped[0]); !ok || !strings.Contains(string(got), `"anyOf"`) {
		t.Errorf("装饰器转发丢 schema: %q", got)
	}
}

// TestListToolsTypedFallback 无 transport 句柄（测试桩注入形态）回落类型化通道：
// 工具照常列出，outputSchema 置空（宁缺勿假）。
func TestListToolsTypedFallback(t *testing.T) {
	ts := rawFidelityServer(t)
	cli, _ := dialFidelity(t, ts)
	entries, err := listTools(context.Background(), cli, nil, "", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].tool.Name != "shell" {
		t.Fatalf("类型化回退目录异常: %+v", entries)
	}
	if len(entries[0].outputSchema) != 0 {
		t.Errorf("typed 回退不应带 outputSchema（有损）: %s", entries[0].outputSchema)
	}
	if _, ok := OutputSchemaOf(newInvokableTool(cli, entries[0], entries[0].tool.Name, nil, false)); ok {
		t.Error("typed 回退工具不应声明 OutputSchemer 能力")
	}
}

// fingerprintServer 带**属性化入参 schema** 的 server（#66 探针形态：零值
// InputSchema 与真实 schema 的指纹差异必须可辨——无属性工具两面同空，测不出
// 回归）。
func fingerprintServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := server.NewMCPServer("fp", "1.0.0")
	srv.AddTool(mcp.NewTool("search",
		mcp.WithDescription("fingerprint probe"),
		mcp.WithString("q", mcp.Required()),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	ts := httptest.NewServer(server.NewStreamableHTTPServer(srv))
	t.Cleanup(ts.Close)
	return ts
}

// TestFingerprintCrossChannelEqual 回归（oagentkit/mcpkit#66，探针同款）：raw 通道
// 目录与 typed ListTools 对**同一工具面**的指纹必相等——此前 raw 条目的
// tool.InputSchema 恒零值（schema 只存 rawInput），零值序列化
// （type:""）与 typed 真实 schema（type:"object"+properties）永不相等，现代协议
// server 每次 PingServer 探活都误判「工具面变了」清 catalog。
func TestFingerprintCrossChannelEqual(t *testing.T) {
	ts := fingerprintServer(t)
	cli, tr := dialFidelity(t, ts)

	// raw 通道目录（生产建连恒走：catalogFor 快照 tr 后的线上列举）
	raw, err := fetchRawToolsList(context.Background(), tr, cli.ProtocolVersion())
	if err != nil {
		t.Fatalf("raw fetch: %v", err)
	}
	entries, err := parseRawToolsList(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(entries) != 1 || entries[0].tool.Name != "search" {
		t.Fatalf("raw 目录异常: %+v", entries)
	}
	// 回填断言：raw 条目的 InputSchema 非零值（type/properties 与 typed 同形）
	if got := entries[0].tool.InputSchema.Type; got != "object" {
		t.Fatalf("raw 条目 InputSchema 应回填 type=object，得 %q（零值即 #66 回归）", got)
	}
	if _, ok := entries[0].tool.InputSchema.Properties["q"]; !ok {
		t.Fatalf("raw 条目 InputSchema 应回填 properties: %+v", entries[0].tool.InputSchema)
	}

	// typed 通道目录（探活路径 PingServer 的 ListTools 同款）
	res, err := cli.ListTools(context.Background(), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if probeFingerprintOfEntries(entries) != probeFingerprint(res.Tools) {
		t.Fatalf("同一工具面跨双通道指纹不等（探活会清目录）:\n raw=%s\n typed=%s",
			probeFingerprintOfEntries(entries), probeFingerprint(res.Tools))
	}
}

// catalogCleared 白盒判缓存目录是否已被清空。
func catalogCleared(p *Pool, name string) bool {
	st := p.conn(name)
	if st == nil {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.catalog == nil
}

// TestRefreshCatalogKeptOnSameFace 探活刷新不误伤：缓存目录来自 raw 通道、探活
// 结果来自 typed 通道——同工具面不得清缓存（回归态：每次探活必清 → 探活周期
// 驱动的 tools/list 风暴，「连接时一次列举」不变量被打破）；工具面真变（增属性）
// 才清。
func TestRefreshCatalogKeptOnSameFace(t *testing.T) {
	ts := fingerprintServer(t)
	cli, tr := dialFidelity(t, ts)

	raw, err := fetchRawToolsList(context.Background(), tr, cli.ProtocolVersion())
	if err != nil {
		t.Fatalf("raw fetch: %v", err)
	}
	entries, err := parseRawToolsList(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	p := NewPool()
	seedFake(p, "srv", cli) // 连接存在（refreshCatalogIfChanged 只对有缓存者动作）
	setConn(p, "srv", func(st *connState) { st.catalog = entries })

	// 同工具面：typed 探活结果 → 不清
	res, err := cli.ListTools(context.Background(), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	p.refreshCatalogIfChanged("srv", res.Tools)
	if catalogCleared(p, "srv") {
		t.Fatal("同工具面探活不得清 catalog（#66 回归：指纹两侧不同源）")
	}

	// 工具面真变（schema 增属性）→ 清（#53 的原语义保持）
	changed := make([]mcp.Tool, len(res.Tools))
	copy(changed, res.Tools)
	changed[0].InputSchema.Properties["extra"] = map[string]any{"type": "string"}
	p.refreshCatalogIfChanged("srv", changed)
	if !catalogCleared(p, "srv") {
		t.Fatal("工具面变化应清 catalog")
	}
}
