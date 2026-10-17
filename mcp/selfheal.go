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
	"os"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// isTransportDead 传输层死亡类错误判定（自愈可救）：连接关闭/未连接/管道断裂类；
// 业务类错误（MCP isError:true 已被 errorAsObservation 转观察）不在此列。
func isTransportDead(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := asToolResultError(err); ok {
		return false // 业务失败（isError:true）不可自愈
	}
	if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"not connected", "connection closed", "connection refused",
		"broken pipe", "transport closed", "client is not initialized",
		"use of closed network connection",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// selfHealTool 调用期自愈包装：传输层死亡时经 redial 重连并解析同名工具重试一次。
// redial 由 Pool 提供闭包（摘死连接→重连→GetTools→同名解析），包装层不感知池内部。
type selfHealTool struct {
	name   string // 工具名（重连后按名解析同名工具）
	inner  tool.InvokableTool
	redial func(ctx context.Context) (tool.InvokableTool, error)
}

func (t *selfHealTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.inner.Info(ctx)
}

// OutputSchema 动态透传：redial 换 inner 后 schema 随新工具走（server 侧可热改声明）。
func (t *selfHealTool) OutputSchema() json.RawMessage {
	if o, ok := t.inner.(OutputSchemer); ok {
		return o.OutputSchema()
	}
	return nil
}

func (t *selfHealTool) InvokableRun(ctx context.Context, args string, opts ...tool.Option) (string, error) {
	out, err := t.inner.InvokableRun(ctx, args, opts...)
	if err == nil || !isTransportDead(err) {
		return out, err
	}
	fresh, ferr := t.redial(ctx)
	if ferr != nil {
		// %w 保链：OAuth 401 等可行动确定态须可被 errors.As 穿透（自愈重连
		// 撞上授权失效时，调用方要能拿到「需重新授权」而非笼统失败）
		return "", fmt.Errorf("mcp: %s 自愈重连失败: %w（原始错误: %v）", t.name, ferr, err)
	}
	out2, err2 := fresh.InvokableRun(ctx, args, opts...)
	if err2 != nil {
		return "", fmt.Errorf("mcp: %s 自愈重试仍失败: %w（原始错误: %v）", t.name, err2, err)
	}
	return out2, nil
}

var _ tool.InvokableTool = (*selfHealTool)(nil)

// selfHealRedial 生产侧重连闭包（Pool 提供装配）：摘「本工具失败的这条」连接 →
// 重连 → GetTools → 按名解析同名工具（errorAsObservation 包装保持与其他工具
// 同形态）。evict 带实例比对（cached == dead）——并发自愈时后来者不得摘除
// 别的自愈刚建好的新连接（曾把健康连接关掉、造成伪失败与重复建连）。
// 不接收装配期 ctx：重连应使用调用期 ctx（闭包参数），装配期 ctx 在此无义。
func (p *Pool) selfHealRedial(cfg ServerConfig, allow []string, name string, dead client.MCPClient) func(context.Context) (tool.InvokableTool, error) {
	return func(ctx context.Context) (tool.InvokableTool, error) {
		if st := p.conn(cfg.Name); st != nil {
			st.mu.Lock()
			cached := st.cli
			st.mu.Unlock()
			if cached == dead {
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
		for _, t := range WrapErrorAsObservation(p.assembleTools(cfg, allow, cli, entries)) {
			if it, ok := t.(tool.InvokableTool); ok {
				if info, ierr := it.Info(ctx); ierr == nil && info.Name == name {
					return it, nil
				}
			}
		}
		return nil, fmt.Errorf("mcp: 重连后未找到工具 %q", name)
	}
}
