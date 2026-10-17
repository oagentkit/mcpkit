package mcp

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// acceptElicitHandler 应答式征集处理器：恒 accept 并回填 who=zhang。
type acceptElicitHandler struct{ calls int }

func (h *acceptElicitHandler) Elicit(ctx context.Context, req mcp.ElicitationRequest) (*mcp.ElicitationResult, error) {
	h.calls++
	return &mcp.ElicitationResult{
		ElicitationResponse: mcp.ElicitationResponse{
			Action:  mcp.ElicitationResponseActionAccept,
			Content: map[string]any{"who": "zhang"},
		},
	}, nil
}

// newElicitServer 征集演示 server（SEP-2322 多往返形态）：ask 工具首轮返回
// InputRequests 征集输入，client 应答后重试，第二轮消费应答回显。
func newElicitServer(t *testing.T) *server.MCPServer {
	t.Helper()
	srv := server.NewMCPServer("elicit-mcp", "1.0.0",
		server.WithToolCapabilities(false), server.WithElicitation())
	srv.AddTool(mcp.NewTool("ask", mcp.WithDescription("elicit demo")),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if answer := server.ElicitationResponse(req.Params.InputResponses, "who"); answer != nil {
				if answer.Action != mcp.ElicitationResponseActionAccept {
					return mcp.NewToolResultText("declined"), nil
				}
				content, _ := answer.Content.(map[string]any)
				who, _ := content["who"].(string)
				return mcp.NewToolResultText("answer=" + who), nil
			}
			return server.NewInputRequestBuilder("step=1").
				Elicit("who", mcp.ElicitationParams{
					Mode:    mcp.ElicitationModeForm,
					Message: "who?",
					RequestedSchema: map[string]any{
						"type":       "object",
						"properties": map[string]any{"who": map[string]any{"type": "string"}},
					},
				}).ToolResult(), nil
		})
	return srv
}

// TestElicitationHandlerWired 全链路：dial 装配 handler（client.NewClient 选项）→
// Initialize 能力声明 → 工具执行中 server 征集 → handler 应答 → 结果回显。
// 走真实 streamable HTTP（in-process 传输不带 server→client 请求通道）。
func TestElicitationHandlerWired(t *testing.T) {
	ts := httptest.NewServer(server.NewStreamableHTTPServer(newElicitServer(t)))
	defer ts.Close()

	p := NewPool(ServerConfig{Name: "srv", URL: ts.URL + "/mcp", Timeout: 5 * time.Second})
	h := &acceptElicitHandler{}
	p.ElicitationHandler = h
	defer p.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tr, err := p.Tools(ctx, []ToolSpec{{Server: "srv"}})
	if err != nil {
		t.Fatalf("Tools 失败: %v", err)
	}
	if len(tr.Tools) != 1 {
		t.Fatalf("应返回 1 个工具，得 %d", len(tr.Tools))
	}
	it, ok := tr.Tools[0].(tool.InvokableTool)
	if !ok {
		t.Fatal("应为 InvokableTool")
	}
	out, err := it.InvokableRun(ctx, "{}")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if !strings.Contains(out, "answer=zhang") {
		t.Fatalf("征集应答未回流: %s", out)
	}
	if h.calls != 1 {
		t.Fatalf("handler 应被回调 1 次，得 %d", h.calls)
	}
}

// TestElicitationNoHandler 未装 handler：client 无法应答 InputRequests →
// 多往返失败以调用错误冒泡（协议层缺失，非工具业务失败）。
func TestElicitationNoHandler(t *testing.T) {
	ts := httptest.NewServer(server.NewStreamableHTTPServer(newElicitServer(t)))
	defer ts.Close()

	p := NewPool(ServerConfig{Name: "srv", URL: ts.URL + "/mcp", Timeout: 5 * time.Second})
	defer p.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tr, err := p.Tools(ctx, []ToolSpec{{Server: "srv"}})
	if err != nil {
		t.Fatalf("Tools 失败: %v", err)
	}
	it := tr.Tools[0].(tool.InvokableTool)
	_, err = it.InvokableRun(ctx, "{}")
	if err == nil || !strings.Contains(err.Error(), "no elicitation handler") {
		t.Fatalf("缺 handler 应以协议错误可见: %v", err)
	}
}
