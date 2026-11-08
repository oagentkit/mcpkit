package mcp

// 调用期自愈（批次五十，对标 ZCode callTool 自愈：先查 disconnected 再重连、SDK 裸
// "Not connected" 竞态重试一次）：MCP 工具调用遇到传输层死亡类错误（连接关闭/未
// 连接/管道断裂）时，摘除死连接→重连→解析同名工具重试一次。业务类错误（工具真的
// 执行了但失败，isError:true）不触发自愈——那是 errorAsObservation 层的语义。
//
// 与 evict 的关系：evict 在目录列举失败时触发（连接摘除+目录失效）；本层覆盖「目录
// 拿到后、调用时连接才死」的窗口（长会话里 tools 子进程中途死亡的真形态）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"os"
	"strings"
	"sync"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// isTransportDead 传输层死亡类错误判定（自愈可救）：连接关闭/未连接/管道断裂类；
// 业务类错误（MCP isError:true 已被 errorAsObservation 转观察）不在此列。
//
// 判定面层次（oagentkit/mcpkit#50）：结构化优先——mcp-go 的
// transport.ErrTransportClosed 走 errors.Is；文本 marker 是兜底（mcp-go 多数
// 断连路径仍只有文案）。⚠️ 已知边界：非英文环境/自定义措辞/驱动特有用语的
// 断连判不出来 = 不自愈——上游暴露更多结构化错误前，这是文案兜底的天花板。
func isTransportDead(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := asToolResultError(err); ok {
		return false // 业务失败（isError:true）不可自愈
	}
	if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) || errors.Is(err, transport.ErrTransportClosed) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"not connected", "connection closed", "connection refused",
		"broken pipe", "transport closed", "client is not initialized",
		"use of closed network connection", "use of closed",
		"connection reset by peer",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// mcpClienter 暴露底层 MCP 连接的工具能力面（包内装配链自省用，不外露）：
// selfHealTool 自愈时经它解析「当前 inner 实际持有的连接」，redial 才能对
// 当前代死连接做 evict 实例比对（oagentkit/mcpkit#88）。
type mcpClienter interface {
	mcpClient() client.MCPClient
}

// selfHealTool 调用期自愈包装：传输层死亡时经 redial 重连并解析同名工具重试一次。
// redial 由 Pool 提供闭包（摘死连接→重连→GetTools→同名解析），包装层不感知池内部。
type selfHealTool struct {
	name string // 工具名（重连后按名解析同名工具）
	// mu 护 inner：自愈成功后的回写与各读取路径并发安全（oagentkit/mcpkit#62）
	// ——同一工具对象可被多 goroutine 并发调用，inner 替换不得裸写。
	mu sync.Mutex
	// inner 当前装配代工具（errorAsObservation 包装，底层绑某代连接）。
	inner tool.InvokableTool
	// redial 重连闭包（Pool 提供）。dead 连接由本工具在 redial 时刻经
	// currentClient() 从当前 inner 解析后传入（oagentkit/mcpkit#88）——闭包
	// 不得固化装配代连接，否则 swapInner 换代后比对基准过时，第二代断连
	// 恒跳过 evict、死连接永驻缓存。
	redial func(ctx context.Context, dead client.MCPClient) (tool.InvokableTool, error)
}

// swapInner 自愈成功后替换 inner（oagentkit/mcpkit#62）：此前注释称「redial 换
// inner 后 schema 随新工具走」但实现从不替换——此后每次调用都先撞一次死连接
// （一轮注定失败的调用 + 一次 redial + 一次目录装配）再重试。长会话里同一工具
// 被反复调用时失败调用按调用次数线性放大。
func (t *selfHealTool) swapInner(fresh tool.InvokableTool) {
	t.mu.Lock()
	t.inner = fresh
	t.mu.Unlock()
}

// currentInner 锁内取当前 inner。
func (t *selfHealTool) currentInner() tool.InvokableTool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.inner
}

// currentClient 解析当前 inner 实际持有的底层连接（redial 的死连接比对基准）。
// swapInner 换代后随新 inner 走（oagentkit/mcpkit#88）——比对基准必须是「刚在
// 本工具上失败的那条连接」，而非装配期固化的某一代。链上无连接暴露面（测试桩/
// 外部实现）返回 nil：redial 侧对 nil dead 跳过实例比对，直接走建连单飞。
func (t *selfHealTool) currentClient() client.MCPClient {
	if c, ok := t.currentInner().(mcpClienter); ok {
		return c.mcpClient()
	}
	return nil
}

func (t *selfHealTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.currentInner().Info(ctx)
}

// RawToolName 透传（oagentkit/mcpkit#46）：按名判定的消费方拿到的最外层工具
// 通常是自愈包装——不透传的话类型断言断到外层就断了。
func (t *selfHealTool) RawToolName() string {
	if rn, ok := t.currentInner().(RawNamer); ok {
		return rn.RawToolName()
	}
	return ""
}

// OutputSchema 动态透传：redial 换 inner 后 schema 随新工具走（server 侧可热改声明）。
func (t *selfHealTool) OutputSchema() json.RawMessage {
	if o, ok := t.currentInner().(OutputSchemer); ok {
		return o.OutputSchema()
	}
	return nil
}

func (t *selfHealTool) InvokableRun(ctx context.Context, args string, opts ...tool.Option) (string, error) {
	out, err := t.currentInner().InvokableRun(ctx, args, opts...)
	if err == nil || !isTransportDead(err) {
		return out, err
	}
	fresh, ferr := t.redial(ctx, t.currentClient())
	if ferr != nil {
		// %w 保链：OAuth 401 等可行动确定态须可被 errors.As 穿透（自愈重连
		// 撞上授权失效时，调用方要能拿到「需重新授权」而非笼统失败）
		return "", fmt.Errorf("mcp: %s 自愈重连失败: %w（原始错误: %v）", t.name, ferr, err)
	}
	out2, err2 := fresh.InvokableRun(ctx, args, opts...)
	if err2 != nil {
		return "", fmt.Errorf("mcp: %s 自愈重试仍失败: %w（原始错误: %v）", t.name, err2, err)
	}
	// 重试成功才回写：失败的新工具不进 inner（下次调用仍从旧 inner 触发自愈，
	// 拿到最新的失败语义而非半换装状态）。
	t.swapInner(fresh)
	return out2, nil
}

var _ tool.InvokableTool = (*selfHealTool)(nil)

// selfHealRedial 生产侧重连闭包（Pool 提供装配）：摘「本工具失败的这条」连接 →
// 重连 → GetTools → 按名解析同名工具（errorAsObservation 包装保持与其他工具
// 同形态）。evict 带实例比对（cached == dead）——并发自愈时后来者不得摘除
// 别的自愈刚建好的新连接（曾把健康连接关掉、造成伪失败与重复建连）。
// dead 连接不在此固化：由 selfHealTool 在 redial 时刻经 currentClient() 从当前
// inner 解析后传参（oagentkit/mcpkit#88）——此前装配代连接被闭包捕获、swapInner
// 从不更新，第二次断连时 cached(新代死连接) != dead(装配代) 恒跳过 evict，
// client() 快路径缓存命中返回死连接，自愈恒败、该 server 全部工具永久瘫痪。
// 不接收装配期 ctx：重连应使用调用期 ctx（闭包参数），装配期 ctx 在此无义。
func (p *Pool) selfHealRedial(cfg ServerConfig, spec ToolSpec, name string) func(context.Context, client.MCPClient) (tool.InvokableTool, error) {
	return func(ctx context.Context, dead client.MCPClient) (tool.InvokableTool, error) {
		if st := p.conn(cfg.Name); st != nil {
			st.mu.Lock()
			cached := st.cli
			st.mu.Unlock()
			if dead != nil && cached == dead {
				p.evict(cfg.Name, cached)
			}
		}
		cli, err := p.client(ctx, cfg)
		if err != nil {
			return nil, err
		}
		entries, err := p.catalogFor(ctx, cfg, cli)
		if err != nil {
			return nil, err
		}
		for _, t := range WrapErrorAsObservation(p.assembleTools(cfg, spec, cli, entries)) {
			if it, ok := t.(tool.InvokableTool); ok {
				if info, ierr := it.Info(ctx); ierr == nil && info.Name == name {
					return it, nil
				}
			}
		}
		return nil, fmt.Errorf("mcp: 重连后未找到工具 %q", name)
	}
}
