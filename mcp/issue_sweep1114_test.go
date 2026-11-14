package mcp

// 2026-11-14 issue 清零战回归测试（#141-#150）：每条用例钉一个失效形态，
// 标题带 issue 号——回归时按号检索。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// ---------------------------------------------------------------- #143

// budgetProbeClient 探针预算假体：目录三工具，slow 调用阻塞到 ctx 完结
//（确定性吃光整轮预算——不赌 sleep 时长），其余调用按 fastErr 回错。
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
