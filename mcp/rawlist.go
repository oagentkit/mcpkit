// 有线 schema 保真通道（v0.12.5）：mcp-go 类型化 Tool 反序列化丢 anyOf——
// ToolArgumentsSchema 仅 $defs/type/properties/required/additionalProperties 五类键，
// outputSchema 的 anyOf/oneOf 联合声明经类型化 tools/list 即残废（inputSchema 同理）。
// 建连路径本就自建 transport（dialStdio/dialHTTP），句柄入 connState 后 tools/list
// 走原始 JSON-RPC 自解析，双 schema 无损；transport 未登记（测试注入 InProcess
// client 等）自动回落类型化通道，outputSchema 置空——宁缺勿假。
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// sharedIDTransport 单源 request-ID 分配器（oagentkit/mcpkit#65）。
//
// 背景：raw 目录通道与 mcp-go 类型化 client 写向**同一条 transport**，但两套
// 互不知晓的计数器（此前：raw 用包级全局、typed 用 per-client，都从 1 起步的
// 稠密小整数）必然重号——stdio transport 按 request.ID 键路由响应，同号在途
// 注册**静默覆盖**（后写胜）、无匹配应答**静默丢弃**：typed 调用挂死到 ctx
// 超时、raw 列举收到串台的 tools/call 结果解析出 0 条目 + nil error 把空目录
// 写进缓存（grantsAll 场景该 server 全部工具静默消失）、tools/call 收到串台的
// tools/list 结果（LLM 拿到空信封当工具输出）。
//
// 修法：建连单点（dialStdio/dialHTTP）把 transport 包进本类型——**该连接上
// 所有**出站请求的 wire ID 都从同一个原子计数器取号（类型化 client 自带的
// per-client 计数器在包装器上被改写，不再上 wire）。SendRequest 同步返回，
// 改号/还原对调用方无感：请求侧覆盖 ID、响应侧还原调用方原始 ID（typed
// client 的响应对账/追踪按它的 ID 走）。
//
// 稠密小整数从 1 起步（与 JSON 数字 float64 解析路径相容，无 2^53 失真面）。
type sharedIDTransport struct {
	transport.Interface
	next atomic.Int64
}

// SendRequest 改号转发：wire ID 单源分配，响应 ID 还原为调用方原值。
func (s *sharedIDTransport) SendRequest(ctx context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
	callerID := request.ID
	request.ID = mcp.NewRequestId(s.next.Add(1))
	resp, err := s.Interface.SendRequest(ctx, request)
	if resp != nil {
		resp.ID = callerID
	}
	return resp, err
}

// SetRequestHandler 转发 BidirectionalInterface（elicitation 等带外请求通道——
// 不转发则 client.Start 的类型断言落到包装器上失败，带外请求全部 METHOD_NOT_FOUND）。
func (s *sharedIDTransport) SetRequestHandler(handler transport.RequestHandler) {
	if bi, ok := s.Interface.(transport.BidirectionalInterface); ok {
		bi.SetRequestHandler(handler)
	}
}

// SetProtocolVersion 转发 HTTPConnection（协商版本回写 transport——不转发则
// 现代 HTTP 连接的逐请求版本头不生效）。
func (s *sharedIDTransport) SetProtocolVersion(version string) {
	if hc, ok := s.Interface.(transport.HTTPConnection); ok {
		hc.SetProtocolVersion(version)
	}
}

// RequiresLegacyProtocol 转发 StreamableHTTP 的 legacy 判定（WithListeningEnabled
// 场景要求 legacy 协议——client 协议协商按本方法分流，不转发会改变协商行为）。
func (s *sharedIDTransport) RequiresLegacyProtocol() bool {
	if lr, ok := s.Interface.(interface{ RequiresLegacyProtocol() bool }); ok {
		return lr.RequiresLegacyProtocol()
	}
	return false
}

// fetchRawToolsList 原始 tools/list（单页，与类型化通道同不翻页——目录场景页大小足够）。
// protoVersion 为空/旧协议 = 裸请求；现代协议（2026-07-28 起）逐请求要求 _meta
// protocolVersion 与 MCP-Protocol-Version 头双写且一致——与 client.do 的
// applyRequestMeta/applyStandardHeaders 同口径（tools/list 无调用方 _meta，归因键不适用）。
// ID 为占位 0：生产路径 tr 恒为 sharedIDTransport（dial 单点包装），wire ID 由
// 其单源改号；直构 transport 的白盒测试路径单线程串行调用，占位号无碰撞面。
func fetchRawToolsList(ctx context.Context, tr transport.Interface, protoVersion string) (json.RawMessage, error) {
	params := map[string]any{}
	header := http.Header{}
	if mcp.IsModernProtocol(protoVersion) {
		params["_meta"] = map[string]any{
			mcp.MetaKeyProtocolVersion: protoVersion,
			// 空对象=无可选能力（server 侧每请求必需键；与 client.effectiveCapabilities
			// 的逐请求快照不同——目录列举不消费可选能力，如实声明空集）。
			mcp.MetaKeyClientCapabilities: map[string]any{},
		}
		encoded, merr := json.Marshal(params)
		if merr == nil {
			for name, value := range mcp.StandardHeaders(protoVersion, mcp.MethodToolsList, encoded) {
				header.Set(name, value)
			}
		}
	}
	resp, err := tr.SendRequest(ctx, transport.JSONRPCRequest{
		JSONRPC: mcp.JSONRPC_VERSION,
		ID:      mcp.NewRequestId(0),
		Method:  "tools/list",
		Params:  params,
		Header:  header,
	})
	if err != nil {
		return nil, fmt.Errorf("mcp: 原始 tools/list 失败: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("mcp: 原始 tools/list 被拒: %w", resp.Error.AsError())
	}
	return resp.Result, nil
}

// parseRawToolsList 解析 tools/list result。注解缺省语义与 hintsOf 同源
// （destructive 缺省 true，除非声明只读）。
func parseRawToolsList(raw json.RawMessage) ([]toolEntry, error) {
	var payload struct {
		Tools []struct {
			Name         string          `json:"name"`
			Description  string          `json:"description"`
			InputSchema  json.RawMessage `json:"inputSchema"`
			OutputSchema json.RawMessage `json:"outputSchema"`
			Annotations  *struct {
				ReadOnlyHint    *bool `json:"readOnlyHint"`
				IdempotentHint  *bool `json:"idempotentHint"`
				DestructiveHint *bool `json:"destructiveHint"`
			} `json:"annotations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("mcp: tools/list 原始解析失败: %w", err)
	}
	entries := make([]toolEntry, 0, len(payload.Tools))
	for _, t := range payload.Tools {
		h := ToolHints{}
		if t.Annotations != nil {
			h.ReadOnly = deref(t.Annotations.ReadOnlyHint)
			h.Idempotent = deref(t.Annotations.IdempotentHint)
			if t.Annotations.DestructiveHint != nil {
				h.Destructive = *t.Annotations.DestructiveHint
			} else if !h.ReadOnly {
				h.Destructive = true
			}
		} else {
			h.Destructive = true
		}
		tool := mcp.Tool{Name: t.Name, Description: t.Description}
		// inputSchema 缺席的 server（极老形态）回落空对象——与类型化通道零值同形
		input := orEmptyObject(t.InputSchema)
		// 指纹单源（oagentkit/mcpkit#66）：typed Unmarshal 尽力回填 InputSchema。
		// 此前 raw 条目的 tool.InputSchema 恒零值（schema 只存 rawInput），而探活
		// 指纹（probeFingerprint）按 json.Marshal(t.InputSchema) 摘要——零值序列化
		// 与类型化反序列化的真实 schema 永不相等，现代协议 server 每次探活都
		// 判「工具面变了」清 catalog（#53 修复自身引入的回归：探活周期驱动的
		// tools/list 风暴 +「连接时一次列举」不变量被打破）。同一份 wire JSON
		// 经两条通道各自 Unmarshal 进同一结构，序列化指纹必相等；Unmarshal 失败
		// 置零值（与类型化通道同败同形）。rawInput 仍保留原文——anyOf 等类型化
		// 表达不了的联合声明无损面（见文件头），指纹只锚类型化可见面（探活侧
		// 也只见该面，同口径即相等）。
		_ = json.Unmarshal(input, &tool.InputSchema)
		entries = append(entries, toolEntry{
			tool:         tool,
			rawInput:     input,
			outputSchema: t.OutputSchema,
			hints:        h,
		})
	}
	return entries, nil
}

func deref(b *bool) bool { return b != nil && *b }

func orEmptyObject(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	return raw
}
