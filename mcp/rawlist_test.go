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
	inv := newInvokableTool(cli, entries[0], entries[0].tool.Name, nil)
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
	if _, ok := OutputSchemaOf(newInvokableTool(cli, entries[0], entries[0].tool.Name, nil)); ok {
		t.Error("typed 回退工具不应声明 OutputSchemer 能力")
	}
}
