// 请求侧上下文透传（批次五十六 B，对标 ZCode request-context：请求侧
// _meta 携带 trace/workspace 上下文，adapters/src/mcp/index.ts:1767-1770）：
// Pool.RequestMeta 在每笔工具调用的 ctx 上取值，经 metaClient 装饰器注入
// req.Params.Meta（stdio 与 HTTP 同走 JSON-RPC params，server 侧通用）。
// 池不感知业务键——metaFn 由装配方提供（bianque 侧从 ctx 取 session/trace/caller）。
package mcp

import (
	"context"
	"maps"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// metaClient _meta 注入装饰器：仅覆写 CallTool，其余方法 embedding 透传。
// 已有 Meta 时按 AdditionalFields 合并，metaFn 键胜（平台命名空间 com.bianque/
// 不该被业务方占用）。
type metaClient struct {
	client.MCPClient
	metaFn func(ctx context.Context) map[string]any
}

func (c *metaClient) CallTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if c.metaFn != nil {
		if kv := c.metaFn(ctx); len(kv) > 0 {
			merged := make(map[string]any, len(kv)+2)
			var progressToken any
			if req.Params.Meta != nil {
				maps.Copy(merged, req.Params.Meta.AdditionalFields)
				progressToken = req.Params.Meta.ProgressToken // 结构化字段
				// 不随 AdditionalFields 走——重建时显式回填（第十轮审计：
				// 丢 token 即丢进度通知关联）
			}
			maps.Copy(merged, kv)
			req.Params.Meta = mcp.NewMetaFromMap(merged)
			if progressToken != nil {
				req.Params.Meta.ProgressToken = progressToken
			}
		}
	}
	return c.MCPClient.CallTool(ctx, req)
}

var _ client.MCPClient = (*metaClient)(nil)

// callMetaKey 任务级调用元数据的 ctx 键（包级单键；与 metaClient 配套的
// 装配方标准实现——此前 argus/bianque 各写一份同型键）。
type callMetaKey struct{}

// WithCallMeta 在 ctx 上注入任务级调用元数据（Runner/任务层调用一次，随 ctx
// 流入该任务全部 MCP 工具调用）。
func WithCallMeta(ctx context.Context, meta map[string]any) context.Context {
	return context.WithValue(ctx, callMetaKey{}, meta)
}

// CallMeta 从 ctx 取调用元数据；无注入返回 nil（metaClient 对空不注入 _meta）。
// 返回值可直接作 Pool.RequestMeta：pool.RequestMeta = mcp.CallMeta。
func CallMeta(ctx context.Context) map[string]any {
	meta, _ := ctx.Value(callMetaKey{}).(map[string]any)
	return meta
}
