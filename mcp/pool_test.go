package mcp

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/oagentkit/runtime/procx"
)

// newTestServer 起 in-process MCP server：注册 calculate + other 两个工具。
func newTestServer(t *testing.T) *server.MCPServer {
	t.Helper()
	srv := server.NewMCPServer("test-mcp", "1.0.0",
		server.WithToolCapabilities(false))

	calc := mcp.NewTool("calculate",
		mcp.WithDescription("Perform the add operation"),
		mcp.WithString("operation", mcp.Required(), mcp.Enum("add")),
		mcp.WithNumber("x", mcp.Required()),
		mcp.WithNumber("y", mcp.Required()),
	)
	srv.AddTool(calc, func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		x, _ := req.RequireFloat("x")
		y, _ := req.RequireFloat("y")
		return mcp.NewToolResultText(intToStr(x + y)), nil
	})

	other := mcp.NewTool("other", mcp.WithDescription("another tool"))
	srv.AddTool(other, func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("other"), nil
	})
	return srv
}

// injectClient 把已 Initialize 的 in-process client 直灌池缓存（白盒测试路径）。
func injectClient(t *testing.T, p *Pool, name string, cli client.MCPClient) {
	t.Helper()
	if _, err := cli.Initialize(context.Background(), newInitRequest()); err != nil {
		t.Fatalf("in-process client Initialize 失败: %v", err)
	}
	setConn(p, name, func(st *connState) { st.cli = cli })
}

func newInitRequest() mcp.InitializeRequest {
	req := mcp.InitializeRequest{}
	req.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	req.Params.ClientInfo = mcp.Implementation{Name: "agentkit-test", Version: "1.0"}
	return req
}

func TestToolsWhitelist(t *testing.T) {
	srv := newTestServer(t)
	cli, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatal(err)
	}
	p := NewPool(ServerConfig{Name: "calc", URL: "http://seed.invalid/mcp"})
	injectClient(t, p, "calc", cli)
	defer p.Close()

	// 白名单只暴露 calculate（other 不给 agent）
	tr, err := p.Tools(context.Background(), []ToolSpec{
		{Server: "calc", Allow: []string{"calculate"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Tools) != 1 {
		t.Fatalf("白名单应只出 1 个工具，得到 %d", len(tr.Tools))
	}
	info, err := tr.Tools[0].Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "calculate" {
		t.Fatalf("工具名 = %q", info.Name)
	}

	// 调用链路：eino tool → MCP server
	it, ok := tr.Tools[0].(tool.InvokableTool)
	if !ok {
		t.Fatal("返回的工具应实现 tool.InvokableTool")
	}
	res, err := it.InvokableRun(context.Background(), `{"operation":"add","x":2,"y":3}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "5") {
		t.Fatalf("calculate(2,3) 应为 5，得到 %s", res)
	}
}

func TestToolsEmptyAllowAll(t *testing.T) {
	srv := newTestServer(t)
	cli, _ := client.NewInProcessClient(srv)
	p := NewPool(ServerConfig{Name: "calc", URL: "http://seed.invalid/mcp"})
	injectClient(t, p, "calc", cli)
	defer p.Close()

	// 空 Allow = 全部工具
	tr, err := p.Tools(context.Background(), []ToolSpec{{Server: "calc"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Tools) != 2 {
		t.Fatalf("空白名单应出全部 2 个工具，得到 %d", len(tr.Tools))
	}
}

func TestToolsUnknownServer(t *testing.T) {
	p := NewPool(ServerConfig{Name: "a", URL: "http://seed.invalid/mcp"})
	_, err := p.Tools(context.Background(), []ToolSpec{{Server: "nope"}})
	if err == nil || !strings.Contains(err.Error(), "未配置") {
		t.Fatalf("引用未配置 server 应报错: %v", err)
	}
}

func TestToolsPartialFailureTolerated(t *testing.T) {
	// server-a 的 command 不存在（建连失败），server-b in-process 正常
	srv := newTestServer(t)
	cli, _ := client.NewInProcessClient(srv)

	var errCount atomic.Int32
	p := NewPool(
		ServerConfig{Name: "broken", Command: []string{"/nonexistent/mcp-server"}},
		ServerConfig{Name: "good", URL: "http://seed.invalid/mcp"},
	)
	injectClient(t, p, "good", cli)
	p.OnError = func(server string, err error) {
		if server == "broken" {
			errCount.Add(1)
		}
	}
	defer p.Close()

	tr, err := p.Tools(context.Background(), []ToolSpec{
		{Server: "broken"},
		{Server: "good", Allow: []string{"other"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if errCount.Load() != 1 {
		t.Fatalf("broken server 应触发一次 OnError，实际 %d", errCount.Load())
	}
	if len(tr.Tools) != 1 {
		t.Fatalf("部分失败容忍：应返回 good 的 1 个工具，得到 %d", len(tr.Tools))
	}
	// 失败面程序化可见（收口轮）：broken 的失败原因进 Failed，无须消费 OnError 副作用
	if tr.Failed == nil || tr.Failed["broken"] == nil {
		t.Fatalf("broken 应进 Failed 面: %+v", tr.Failed)
	}
}

func TestStdioDialFailure(t *testing.T) {
	p := NewPool(ServerConfig{Name: "x", Command: []string{"/nonexistent/mcp"}, Timeout: 2 * time.Second})
	_, err := p.client(context.Background(), ServerConfig{Name: "x", Command: []string{"/nonexistent/mcp"}})
	if err == nil {
		t.Fatal("不存在的命令应建连失败")
	}
}

func TestDialBothEmpty(t *testing.T) {
	p := NewPool()
	if _, _, err := p.dial(context.Background(), ServerConfig{Name: "x"}); err == nil ||
		!strings.Contains(err.Error(), "均未配置") {
		t.Fatalf("stdio/http 均空应报错: %v", err)
	}
}

// TestDialRejectsOptionCommand 锁死 exec 层守卫：command[0] 以 "-" 开头即
// argument injection 面（Command 来自装配配置，越界即配置错误）。
func TestDialRejectsOptionCommand(t *testing.T) {
	p := NewPool()
	if _, _, err := p.dial(context.Background(), ServerConfig{Name: "x", Command: []string{"-evil"}}); err == nil ||
		!strings.Contains(err.Error(), "非法 command") {
		t.Fatalf("command[0] 以 - 开头应被拒绝: %v", err)
	}
	if _, _, err := p.dial(context.Background(), ServerConfig{Name: "x", Command: []string{""}}); err == nil {
		t.Fatal("空 command[0] 应被拒绝")
	}
	// 均 nil/空切片：落到"stdio 与 http 均未配置"分支
	if _, _, err := p.dial(context.Background(), ServerConfig{Name: "x"}); err == nil ||
		!strings.Contains(err.Error(), "均未配置") {
		t.Fatalf("无 command 无 url 应报配置错误: %v", err)
	}
}

func TestPoolClose(t *testing.T) {
	srv := newTestServer(t)
	cli, _ := client.NewInProcessClient(srv)
	p := NewPool(ServerConfig{Name: "calc", URL: "http://seed.invalid/mcp"})
	injectClient(t, p, "calc", cli)

	p.Close()
	p.Close() // 幂等
	if hasConn(p, "calc") {
		t.Fatal("Close 后缓存应清空")
	}
	// Close 后再建连应被拒绝，且不得把新连接塞回缓存（泄漏）
	if _, err := p.client(context.Background(), ServerConfig{Name: "calc"}); err == nil {
		t.Fatal("Close 后 client() 应报错")
	}
	if hasConn(p, "calc") {
		t.Fatal("Close 后 client() 不得写入缓存")
	}
}

func TestWhitelistEnv(t *testing.T) {
	t.Setenv("AK_MCP_SECRET_TOKEN", "s3cret")
	t.Setenv("AK_MCP_PLAIN", "v=1") // 值里含 = 的字面透传

	out := procx.ChildEnv([]string{"AK_MCP_SECRET_TOKEN", "AK_MCP_MISSING_VAR", "FOO=bar"})
	joined := strings.Join(out, "\n")

	if !strings.Contains(joined, "AK_MCP_SECRET_TOKEN=s3cret") {
		t.Fatalf("白名单变量应按名透传: %v", out)
	}
	if strings.Contains(joined, "AK_MCP_MISSING_VAR") {
		t.Fatalf("不存在的变量应跳过: %v", out)
	}
	if !strings.Contains(joined, "FOO=bar") {
		t.Fatalf("KEY=VALUE 字面条目应透传: %v", out)
	}
	// 核心安全断言：当前进程的其它环境变量（哪怕名字可疑）一律不继承。
	// 允许集 = procx.ChildEnv 基础白名单（并集口径）+ 白名单条目 + 禁交互追加项。
	allowed := map[string]bool{
		"PATH": true, "HOME": true, "TMPDIR": true, "USER": true, "LOGNAME": true,
		"SHELL": true, "LANG": true, "LC_ALL": true, "TERM": true,
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true,
		"http_proxy": true, "https_proxy": true, "no_proxy": true,
		"SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
		"CLAUDE_CONFIG_DIR": true, "OPENCODE_CONFIG": true,
		"XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true,
		"GIT_TERMINAL_PROMPT": true, "CI": true, "FOO": true,
	}
	for _, kv := range out {
		name := strings.SplitN(kv, "=", 2)[0]
		if !allowed[name] && !strings.HasPrefix(name, "AK_MCP_") {
			t.Fatalf("出现白名单之外的变量 %q: %v", name, out)
		}
	}
}

func TestWhitelistEnvRealSubprocess(t *testing.T) {
	// 端到端：env 命令输出子进程真实环境，验证父进程的非白名单变量确实没被继承
	t.Setenv("AK_MCP_PARENT_SECRET", "topsecret") // 未列入白名单 → 不得继承
	t.Setenv("AK_MCP_ALLOWED", "okvalue")         // 列入白名单 → 应透传
	cmd := exec.Command("env")
	cmd.Env = procx.ChildEnv([]string{"AK_MCP_ALLOWED"})
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("无法运行 env: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "topsecret") || strings.Contains(s, "AK_MCP_PARENT_SECRET") {
		t.Fatalf("子进程不应继承白名单之外的变量: %s", s)
	}
	if !strings.Contains(s, "AK_MCP_ALLOWED=okvalue") {
		t.Fatalf("白名单变量应出现在子进程环境: %s", s)
	}
}

func intToStr(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64) // 整数值场景输出 "5"
}

func TestServerConfigTimeoutOr(t *testing.T) {
	if (&ServerConfig{}).timeoutOr(9*time.Second) != 9*time.Second {
		t.Fatal("零值应回退缺省")
	}
	if (&ServerConfig{Timeout: 3 * time.Second}).timeoutOr(9*time.Second) != 3*time.Second {
		t.Fatal("显式值应保留")
	}
}

// setConn 直塞单 server 状态（白盒测试路径）。
func setConn(p *Pool, name string, mutate func(*connState)) {
	p.mu.Lock()
	st := p.connLocked(name)
	p.mu.Unlock()
	st.mu.Lock()
	mutate(st)
	st.mu.Unlock()
}

// hasConn 是否缓存着连接。
func hasConn(p *Pool, name string) bool {
	st := p.conn(name)
	if st == nil {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.cli != nil
}

// TestToolsCancelledCtxColdCacheReturnsError 缓存冷 + ctx 取消（第六轮审计）：
// Tools 不得返回 (空表, nil)——调用方会把取消当"server 没有工具"静默降级；
// 缓存热时同样的 ctx 走快路径正常返回，两种形态行为须一致。
func TestToolsCancelledCtxColdCacheReturnsError(t *testing.T) {
	p := NewPool(ServerConfig{Name: "x", Command: []string{"true"}})
	defer p.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tr, err := p.Tools(ctx, []ToolSpec{{Server: "x"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应返回 ctx.Err()，得到 tr.Tools=%d err=%v", len(tr.Tools), err)
	}
}

// TestStdioServerHelper 测试二进制自举的 stdio MCP server（父测试以自身可执行文件
// 作 Command spawn；env 标记守护，正常 go test 运行时直接 Skip）。
func TestStdioServerHelper(t *testing.T) {
	if os.Getenv("BQ_MCP_STDIO_HELPER") != "1" {
		t.Skip("helper：仅由 TestStdioChildSurvivesDialCancel 以 env 标记拉起")
	}
	srv := server.NewMCPServer("stdio-helper", "1.0.0", server.WithToolCapabilities(false))
	srv.AddTool(mcp.NewTool("noop", mcp.WithDescription("noop")),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("ok"), nil
		})
	if err := server.NewStdioServer(srv).Listen(context.Background(), os.Stdin, os.Stdout); err != nil {
		t.Fatalf("stdio server 退出: %v", err)
	}
}

// TestStdioChildSurvivesDialCancel 回归（v0.10.33）：stdio 子进程生命周期必须与
// dial ctx 解绑——dial ctx 是连接级短预算，client() 返回即 cancel，绑上去会在建连
// 成功瞬间杀掉 server（下次 tr.Tools/list 报 transport closed）。
func TestStdioChildSurvivesDialCancel(t *testing.T) {
	testBin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := NewPool(ServerConfig{Name: "srv", Timeout: 10 * time.Second,
		Env:     []string{"BQ_MCP_STDIO_HELPER=1"}, // ChildEnv 白名单字面量透传（helper 守卫标记）
		Command: []string{testBin, "-test.run=TestStdioServerHelper", "-test.v=false"}})
	defer p.Close()

	// 拨号 ctx：建连成功后立即取消（复刻 client() 返回即 cancel 的时序）
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 10*time.Second)
	st, err := p.client(dialCtx, p.cfgs["srv"])
	if err != nil {
		t.Fatalf("client 失败: %v", err)
	}
	_ = st
	dialCancel()
	time.Sleep(100 * time.Millisecond) // 给误杀路径（回归态）留触发窗口

	// cancel 后的全新 ctx 上列举工具：子进程被误杀时这里必现 transport closed
	tr, err := p.Tools(context.Background(), []ToolSpec{{Server: "srv", Allow: []string{"noop"}}})
	if err != nil {
		t.Fatalf("dial ctx 取消后 tr.Tools/list 应存活（子进程误杀回归）: %v", err)
	}
	if len(tr.Tools) != 1 {
		t.Fatalf("应返回 1 个工具，得到 %d", len(tr.Tools))
	}
}
