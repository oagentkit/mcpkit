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

var rawReqID atomic.Int64

// fetchRawToolsList 原始 tools/list（单页，与类型化通道同不翻页——目录场景页大小足够）。
// protoVersion 为空/旧协议 = 裸请求；现代协议（2026-07-28 起）逐请求要求 _meta
// protocolVersion 与 MCP-Protocol-Version 头双写且一致——与 client.do 的
// applyRequestMeta/applyStandardHeaders 同口径（tools/list 无调用方 _meta，归因键不适用）。
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
		ID:      mcp.NewRequestId(rawReqID.Add(1)),
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
		entries = append(entries, toolEntry{
			tool: mcp.Tool{Name: t.Name, Description: t.Description},
			// inputSchema 缺席的 server（极老形态）回落空对象——与类型化通道零值同形
			rawInput:     orEmptyObject(t.InputSchema),
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
