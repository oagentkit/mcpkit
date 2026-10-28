package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/mark3labs/mcp-go/client"
)

// TestPoolToolByName 钉住「按 server + 名字取单个工具」是一等入口。
//
// 此前调用方只能写 `res.Tools[0]` 这种**位置假设**：Allow 给多个工具时取到的
// 可能是另一个工具，server 列举部分失败时同样可能取错——两种失效都是**静默**的
// （调用成功、调的是别的工具）。
func TestPoolToolByName(t *testing.T) {
	cli, _ := client.NewInProcessClient(newTestServer(t))
	p := NewPool(ServerConfig{Name: "calc", URL: "http://seed.invalid/mcp"})
	injectClient(t, p, "calc", cli)
	defer p.Close()

	got, err := p.Tool(context.Background(), "calc", "calculate")
	if err != nil {
		t.Fatalf("取工具失败: %v", err)
	}
	info, err := got.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "calc.calculate" {
		t.Fatalf("工具名 = %q", info.Name)
	}

	it, ok := got.(tool.InvokableTool)
	if !ok {
		t.Fatal("应实现 tool.InvokableTool")
	}
	res, err := it.InvokableRun(context.Background(), `{"operation":"add","x":2,"y":3}`)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if !strings.Contains(res, "5") {
		t.Fatalf("calculate(2,3) 应为 5，得到 %s", res)
	}
}

// TestPoolToolNotFoundIsExplicitError 钉住「取不到」是显式错误。
//
// 不能静默返回「第 0 个」——那正是本 issue 要消灭的位置假设。
func TestPoolToolNotFoundIsExplicitError(t *testing.T) {
	cli, _ := client.NewInProcessClient(newTestServer(t))
	p := NewPool(ServerConfig{Name: "calc", URL: "http://seed.invalid/mcp"})
	injectClient(t, p, "calc", cli)
	defer p.Close()

	got, err := p.Tool(context.Background(), "calc", "no_such_tool")
	if err == nil {
		t.Fatalf("取不到工具应报错，实得工具 %v", got)
	}
	if got != nil {
		t.Fatalf("取不到时不得返回别的工具，实得 %v", got)
	}
	if !strings.Contains(err.Error(), "no_such_tool") {
		t.Fatalf("错误应指明工具名，实得 %v", err)
	}
}

// TestPoolToolUnknownServerIsExplicitError 钉住「server 未配置」是显式错误。
func TestPoolToolUnknownServerIsExplicitError(t *testing.T) {
	p := NewPool(ServerConfig{Name: "calc", URL: "http://seed.invalid/mcp"})
	defer p.Close()

	if _, err := p.Tool(context.Background(), "nope", "calculate"); err == nil {
		t.Fatal("未配置的 server 应报错")
	}
}

// TestToolsResultByServer 钉住「工具归属」的结构化面。
//
// 不让调用方靠位置假设去猜某个工具来自哪个 server。
func TestToolsResultByServer(t *testing.T) {
	cliA, _ := client.NewInProcessClient(newTestServer(t))
	cliB, _ := client.NewInProcessClient(newTestServer(t))
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
	if len(tr.Tools) != 4 {
		t.Fatalf("两个 server 各 2 个工具，应得 4 个，得到 %d", len(tr.Tools))
	}
	for _, srv := range []string{"ask-ops", "upgrade-assessor"} {
		got, ok := tr.ByServer[srv]
		if !ok {
			t.Fatalf("ByServer 缺 %q（归属面缺失 ⇒ 调用方只能靠位置猜）", srv)
		}
		if len(got) != 2 {
			t.Fatalf("ByServer[%q] 应有 2 个工具，得到 %d", srv, len(got))
		}
		for _, tl := range got {
			info, ierr := tl.Info(context.Background())
			if ierr != nil {
				t.Fatal(ierr)
			}
			if !strings.HasPrefix(info.Name, srv+".") {
				t.Fatalf("ByServer[%q] 里混进了别的 server 的工具: %q", srv, info.Name)
			}
		}
	}
}
