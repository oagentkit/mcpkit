package mcp

// 本文件钉 oagentkit/mcpkit#134 的回归：入参 schema 解析失败不得被 `_ =`
// 静默吞掉。此前 newInvokableTool 的两处 `_ =` 吞掉 Marshal/Unmarshal 错误
// ⇒ inputSchema 保持零值 ⇒ eino-contrib/jsonschema 对零值序列化出 JSON Schema
// 布尔 `true`（接受任意实例）——server 侧声明的必填约束在 client 侧整个消失，
// 且 Pool.Tools 返回 err=nil、Failed 空、工具照常列出（零可见性）。eino-ext
// components/tool/mcp v0.0.9 对同一处是硬失败，convTool doc 承诺的「口径一致」
// 恰好被自己的实现证伪。

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// badSchemaRaw 违规 server 的入参 schema：JSON 字符串而非对象（MCP 协议要求
// 必须是对象，现实中存在违规 server）——Unmarshal 进 jsonschema.Schema 必失败。
const badSchemaRaw = `"not-an-object"`

// TestNewInvokableToolSchemaFailureExplicit_Issue134 单元面：schema 解析失败
// 显式返回 error，不返回一个带零值 schema 的工具（零值上模型面 = `true` 假契约）。
func TestNewInvokableToolSchemaFailureExplicit_Issue134(t *testing.T) {
	e := toolEntry{tool: mcp.Tool{Name: "bad"}, rawInput: json.RawMessage(badSchemaRaw)}
	inv, err := newInvokableTool(nil, e, "srv.bad", nil, false)
	if err == nil {
		t.Fatal("schema 解析失败必须返回 error（mcpkit#134，禁止 _ 吞错）")
	}
	if inv != nil {
		t.Fatal("解析失败不得返回工具：带空 schema 上模型就是「接受任意参数」的假契约")
	}
	if !strings.Contains(err.Error(), "入参 schema 解析失败") {
		t.Fatalf("错误应点名 schema 解析失败，实得: %v", err)
	}
	if !strings.Contains(err.Error(), "bad") {
		t.Fatalf("错误应带工具名（可定位），实得: %v", err)
	}
}

// TestConvToolsSchemaFailureFailsBatch_Issue134 转换面：坏 schema 整批失败
// （对齐 eino-ext GetTools 的硬失败口径），对照组正常 schema 照常转换。
func TestConvToolsSchemaFailureFailsBatch_Issue134(t *testing.T) {
	entries := []toolEntry{
		{tool: mcp.Tool{Name: "good"}, rawInput: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`)},
		{tool: mcp.Tool{Name: "bad"}, rawInput: json.RawMessage(badSchemaRaw)},
	}
	// 实验组：白名单含坏 schema 工具 ⇒ 整批 error、零工具。
	tools, err := convTools(nil, entries, nil, true, "srv", false)
	if err == nil {
		t.Fatal("坏 schema 应整批失败（对齐 eino-ext），实得 err=nil")
	}
	if tools != nil {
		t.Fatalf("整批失败不得携带任何工具，实得 %d 个", len(tools))
	}
	// 对照组：全部正常 schema ⇒ 照常转换。
	good := entries[:1]
	tools, err = convTools(nil, good, nil, true, "srv", false)
	if err != nil || len(tools) != 1 {
		t.Fatalf("正常 schema 不应受影响: tools=%d err=%v", len(tools), err)
	}
	// 白名单未授予的坏 schema 工具不参与转换（没有暴露面就没有假契约问题）。
	tools, err = convTools(nil, entries, []string{"good"}, false, "srv", false)
	if err != nil || len(tools) != 1 {
		t.Fatalf("白名单外的坏 schema 不应拖垮装配: tools=%d err=%v", len(tools), err)
	}
}

// badSchemaServer 真实 streamable HTTP server：声明非对象 inputSchema 的违规
// 工具（经 raw 通道原样上 wire——生产热路径）。
func badSchemaServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := server.NewMCPServer("badschema", "1.0.0", server.WithToolCapabilities(false))
	srv.AddTool(mcp.NewToolWithRawSchema("badtool", "declares a non-object input schema", json.RawMessage(badSchemaRaw)),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("ok"), nil
		})
	ts := httptest.NewServer(server.NewStreamableHTTPServer(srv))
	t.Cleanup(ts.Close)
	return ts
}

// goodSchemaServer 对照：正常对象 schema 的 server（同一 harness，差异只在 schema）。
func goodSchemaServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := server.NewMCPServer("goodschema", "1.0.0", server.WithToolCapabilities(false))
	srv.AddTool(mcp.NewTool("goodtool",
		mcp.WithDescription("normal tool"),
		mcp.WithString("q", mcp.Required(), mcp.Description("query")),
	), func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	ts := httptest.NewServer(server.NewStreamableHTTPServer(srv))
	t.Cleanup(ts.Close)
	return ts
}

// TestPoolToolsBadSchemaGoesToFailed_Issue134 端到端（生产 raw 通道）：
// 违规 server 的工具**不得**以空 schema 出现在模型面，失败汇入 Pool.Tools 的
// Failed 面并经 OnError 发射（而不是 err=nil 的静默通过）。
func TestPoolToolsBadSchemaGoesToFailed_Issue134(t *testing.T) {
	ts := badSchemaServer(t)
	var notified []string
	p := NewPool(ServerConfig{Name: "bad", URL: ts.URL + "/mcp"})
	p.OnError = func(server string, err error) {
		notified = append(notified, server+": "+err.Error())
	}
	t.Cleanup(p.Close)

	res, err := p.Tools(context.Background(), []ToolSpec{{Server: "bad", AllowAll: true}})
	if err != nil {
		t.Fatalf("坏 schema 应走容忍失败面（Failed + OnError），不该整体 error: %v", err)
	}
	if len(res.Tools) != 0 {
		t.Fatalf("坏 schema 工具不得出现在模型面，实得 %d 个", len(res.Tools))
	}
	ferr := res.Failed["bad"]
	if ferr == nil {
		t.Fatalf("Failed 应点名该 server 的失败原因，实得 %+v", res.Failed)
	}
	if !strings.Contains(ferr.Error(), "入参 schema") {
		t.Fatalf("Failed 原因应点名 schema 解析失败，实得: %v", ferr)
	}
	if len(notified) == 0 {
		t.Fatal("OnError 应发射同源通知（遥测钩子不得静默）")
	}
}

// TestPoolToolsGoodSchemaExposedWithRealContract_Issue134 对照组：正常 server
// 的工具照常出现在模型面，且模型面 schema 保留真实约束（不是布尔 `true`）。
func TestPoolToolsGoodSchemaExposedWithRealContract_Issue134(t *testing.T) {
	ts := goodSchemaServer(t)
	p := NewPool(ServerConfig{Name: "good", URL: ts.URL + "/mcp"})
	t.Cleanup(p.Close)

	res, err := p.Tools(context.Background(), []ToolSpec{{Server: "good", AllowAll: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 1 || len(res.Failed) != 0 {
		t.Fatalf("正常 server 应照常装配: tools=%d failed=%v", len(res.Tools), res.Failed)
	}
	info, err := res.Tools[0].Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"properties"`) || strings.Contains(string(raw), `"json_schema":true`) {
		t.Fatalf("模型面 schema 应保留真实约束（不得退化为 true）: %s", raw)
	}
}

// TestNewInvokableToolTypedFallbackPaths_Issue134 类型化回退通道（无 transport
// 句柄、rawInput 为空由 InputSchema 回填）的两条边界：截断 JSON 显式报错；
// 零值 InputSchema（空对象）是合法 schema 不得误伤。
func TestNewInvokableToolTypedFallbackPaths_Issue134(t *testing.T) {
	// 截断 JSON：Unmarshal 必失败 ⇒ 显式 error（与 raw 通道同一处置，单源）。
	e := toolEntry{tool: mcp.Tool{Name: "bad"}, rawInput: json.RawMessage(`{"type":"object","properties":`)}
	if _, err := newInvokableTool(nil, e, "bad", nil, false); err == nil {
		t.Fatal("截断 JSON 的 schema 应显式报错")
	}
	// 零值 InputSchema 的回填路径（rawInput 空）是合法空对象，不得误伤。
	e2 := toolEntry{tool: mcp.Tool{Name: "empty"}}
	inv, err := newInvokableTool(nil, e2, "empty", nil, false)
	if err != nil || inv == nil {
		t.Fatalf("零值 InputSchema（空对象）不应报错: inv=%v err=%v", inv, err)
	}
}
