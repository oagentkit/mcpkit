package mcp

import (
	"context"
	"github.com/cloudwego/eino/schema"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// newNamedToolServer 造一个只注册**指定工具名**的 server（同名场景用）。
func newNamedToolServer(t *testing.T, toolName string) *server.MCPServer {
	t.Helper()
	srv := server.NewMCPServer("test-mcp", "1.0.0", server.WithToolCapabilities(false))
	tool := mcp.NewTool(toolName, mcp.WithDescription("same-named tool"))
	srv.AddTool(tool, func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// 回显**收到的工具名**：线上必须用原始名，发带命名空间的名字 server 认不出。
		return mcp.NewToolResultText("called:" + req.Params.Name), nil
	})
	return srv
}

// TestToolsExposedNameCarriesServerNamespace 钉住暴露名带 server 命名空间
// （issue #29）。
//
// 同一个工具名可以来自不同 server——不带命名空间时，给模型的工具清单里就有
// 两组同名工具，模型无法从名字分辨来源，命中哪个取决于装配顺序。
func TestToolsExposedNameCarriesServerNamespace(t *testing.T) {
	cliA, _ := client.NewInProcessClient(newNamedToolServer(t, "get_inventory"))
	cliB, _ := client.NewInProcessClient(newNamedToolServer(t, "get_inventory"))
	p := NewPool(
		ServerConfig{Name: "ask-ops", URL: "http://seed.invalid/mcp"},
		ServerConfig{Name: "upgrade-assessor", URL: "http://seed.invalid/mcp"},
	)
	injectClient(t, p, "ask-ops", cliA)
	injectClient(t, p, "upgrade-assessor", cliB)
	defer p.Close()

	tr, err := p.Tools(context.Background(), []ToolSpec{
		{Server: "ask-ops", AllowAll: true},
		{Server: "upgrade-assessor", AllowAll: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Tools) != 2 {
		t.Fatalf("两个 server 各 1 个工具，应得 2 个，得到 %d", len(tr.Tools))
	}

	names := map[string]bool{}
	for _, tl := range tr.Tools {
		info, ierr := tl.Info(context.Background())
		if ierr != nil {
			t.Fatal(ierr)
		}
		if names[info.Name] {
			t.Fatalf("暴露名冲突：%q 出现两次（模型侧无法分辨来源）", info.Name)
		}
		names[info.Name] = true
	}
	for _, want := range []string{"ask-ops.get_inventory", "upgrade-assessor.get_inventory"} {
		if !names[want] {
			t.Fatalf("暴露名应为 %q，实得 %v", want, names)
		}
	}
}

// TestToolsCallStillUsesRawName 钉住「暴露名是给模型看的，线上仍是原始名」。
//
// 这是本次改动最容易踩的坑：暴露名带命名空间后，若调用时也跟着带，server
// 那边登记的是原始名 → 工具一调就「找不到工具」。
func TestToolsCallStillUsesRawName(t *testing.T) {
	cli, _ := client.NewInProcessClient(newNamedToolServer(t, "get_inventory"))
	p := NewPool(ServerConfig{Name: "ask-ops", URL: "http://seed.invalid/mcp"})
	injectClient(t, p, "ask-ops", cli)
	defer p.Close()

	tr, err := p.Tools(context.Background(), []ToolSpec{{Server: "ask-ops", AllowAll: true}})
	if err != nil {
		t.Fatal(err)
	}
	it, ok := tr.Tools[0].(tool.InvokableTool)
	if !ok {
		t.Fatal("返回的工具应实现 tool.InvokableTool")
	}
	res, err := it.InvokableRun(context.Background(), "{}")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if !strings.Contains(res, "called:get_inventory") {
		t.Fatalf("server 侧必须收到**原始**工具名，实得 %q", res)
	}
}

// TestToolsWhitelistAcceptsBothForms 钉住白名单两种写法都命中。
//
// 下游配置里两种都常见：server 内的原始名，以及带 server 的全名
// （跨 server 同名时只有全名是确定的）。
func TestToolsWhitelistAcceptsBothForms(t *testing.T) {
	for _, allow := range [][]string{
		{"calculate"},
		{"calc.calculate"},
	} {
		srv := newTestServer(t)
		cli, _ := client.NewInProcessClient(srv)
		p := NewPool(ServerConfig{Name: "calc", URL: "http://seed.invalid/mcp"})
		injectClient(t, p, "calc", cli)

		tr, err := p.Tools(context.Background(), []ToolSpec{
			{Server: "calc", Allow: allow},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(tr.Tools) != 1 {
			t.Fatalf("白名单 %v 应命中 1 个工具，得到 %d", allow, len(tr.Tools))
		}
		info, _ := tr.Tools[0].Info(context.Background())
		if info.Name != "calc.calculate" {
			t.Fatalf("白名单 %v：暴露名 = %q", allow, info.Name)
		}
		p.Close()
	}
}

// RawToolName 官方 API（mcpkit#46）：原始名经类型断言取（比字符串剥前缀
// 可靠）；包装层（selfheal/observation）透传；RawToolNameOf 回落 Info().Name。
func TestRawToolNameOf(t *testing.T) {
	// convTool 产出的 invokableTool：暴露名带命名空间、RawName 是原始名
	tset := NewCatalog("ask-ops", []string{"run_x"})
	_ = tset
	// 直接构造 invokableTool（包内测试可见）
	it := &invokableTool{wireName: "run_x", info: &schema.ToolInfo{Name: "ask-ops.run_x"}}
	if got := RawToolNameOf(it); got != "run_x" {
		t.Fatalf("RawToolNameOf = %q, want run_x", got)
	}
	// 包装层透传
	w := WrapErrorAsObservation([]tool.BaseTool{it})[0]
	if got := RawToolNameOf(w); got != "run_x" {
		t.Fatalf("包装层应透传 RawToolName: %q", got)
	}
	// 无实现回落 Info().Name
	stub := rawStubTool{}
	if got := RawToolNameOf(stub); got != "stub.name" {
		t.Fatalf("无 RawNamer 应回落暴露名: %q", got)
	}
}

type rawStubTool struct{}

func (rawStubTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: "stub.name"}, nil
}
func (rawStubTool) InvokableRun(context.Context, string, ...tool.Option) (string, error) {
	return "", nil
}
