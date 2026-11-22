package mcp

import (
	"errors"
	"strings"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/oagentkit/mcpkit/iface"
)

// ErrorKind 与 Kind* 常量：协议中立声明单源在 github.com/oagentkit/mcpkit/iface
// （下游零 mcp-go 消费面），本包别名透传——存量 import mcp/mcp 的消费方不动。
type ErrorKind = iface.ErrorKind

const (
	// KindUnknown 未归类（兜底）。语义表见 iface.ErrorKind。
	KindUnknown = iface.KindUnknown
	// KindUnreachable 连不上（网络/进程层）。
	KindUnreachable = iface.KindUnreachable
	// KindProtocol 协议层错误。
	KindProtocol = iface.KindProtocol
	// KindToolMissing 工具不存在。
	KindToolMissing = iface.KindToolMissing
	// KindInvalidArgs 参数错。
	KindInvalidArgs = iface.KindInvalidArgs
)

// ClassifyError 把任意错误归到稳定分类。
//
// 判定顺序（**先具体后笼统**，避免被笼统分支抢走）：
//  1. 工具业务失败（isError:true）→ 按 payload 信号细分，判不出归 KindUnknown
//     （oagentkit/mcpkit#144：此前兜底归 KindProtocol 且 Retryable——把确定性
//     业务失败推给「修 server」处置面并触发盲重试，与协议错的真实语义错位）
//  2. JSON-RPC 哨兵错误（mcp.ErrInvalidParams / ErrMethodNotFound …）——
//     这是 mcp-go 把 JSON-RPC 错误码转成 Go error 的**官方路径**
//     （JSONRPCErrorDetails.AsError），按哨兵判比按错误码判更可靠：
//     它扛得住错误码被包进自定义 error 的情况。
//  3. 错误链里有 net.Error / os.ErrNotExist / syscall.ECONNREFUSED 等
//     → KindUnreachable
//  4. ctx 超时/取消 → KindUnreachable（连不上的一种）
//  5. 兜底 KindUnknown
func ClassifyError(err error) ErrorKind {
	if err == nil {
		return KindUnknown
	}
	// 工具业务失败：payload 里带明确信号时能进一步判到 tool_missing / invalid_args
	if tre, ok := asToolResultError(err); ok {
		return classifyToolResultPayload(string(tre.Payload()))
	}
	// JSON-RPC 哨兵错误（官方转换路径）
	switch {
	case errors.Is(err, mcp.ErrInvalidParams):
		return KindInvalidArgs
	case errors.Is(err, mcp.ErrMethodNotFound):
		return KindToolMissing
	case errors.Is(err, mcp.ErrParseError),
		errors.Is(err, mcp.ErrInvalidRequest),
		errors.Is(err, mcp.ErrInternalError),
		errors.Is(err, mcp.ErrRequestInterrupted),
		errors.Is(err, mcp.ErrResourceNotFound):
		return KindProtocol
	}
	// 网络 / 进程层（stdlib 判据单源在 iface.ClassifyStdlibKind；本包只补
	// mcp-go 类型化断连 transport.ErrTransportClosed 一项）
	if isUnreachable(err) || errors.Is(err, transport.ErrTransportClosed) {
		return KindUnreachable
	}
	return KindUnknown
}

// ClassifyRPCCode JSON-RPC 错误码 → 分类。
//
// 供「已知错误码但还没转成 Go error」的场景用（如自检时读了原始响应）。
// 走 mcp-go 官方转换路径（JSONRPCErrorDetails.AsError）时不必用本函数——
// ClassifyError 已经按哨兵错误判好了。
func ClassifyRPCCode(code int) ErrorKind {
	switch code {
	case mcp.INVALID_PARAMS:
		return KindInvalidArgs
	case mcp.METHOD_NOT_FOUND:
		return KindToolMissing
	default:
		return KindProtocol
	}
}

// classifyToolResultPayload 工具业务失败（isError:true）的 payload → 分类。
//
// 这里是**唯一**必须碰文本的地方：isError:true 的响应没有错误码，
// 只有服务端自己写的 message。此时按「明确的工具不存在/参数错信号」判，
// 判不出归 KindUnknown（oagentkit/mcpkit#144）：payload 兜底面是**业务失败**
// 的海洋（"pod not found"、权限不足、业务校验拒绝都是确定性失败），归
// KindProtocol 会把处置面推向「修 server / 升协议」且 Retryable==true 触发
// 盲重试——两个下游决策同时被带偏；KindUnknown 保守不重试，处置交给「读
// 工具反馈人工研判」。宁欠勿滥：误判成 tool_missing 误导调用方改契约、
// 误判成 protocol 推给 server 运维，都比「未知待人工」代价高。
func classifyToolResultPayload(payload string) ErrorKind {
	low := strings.ToLower(payload)
	switch {
	case strings.Contains(low, "tool not found"),
		strings.Contains(low, "unknown tool"),
		strings.Contains(low, "no such tool"),
		strings.Contains(low, "tool_not_found"):
		return KindToolMissing
	case strings.Contains(low, "invalid params"),
		strings.Contains(low, "invalid argument"),
		strings.Contains(low, "validation error"),
		strings.Contains(low, "missing required"):
		return KindInvalidArgs
	default:
		return KindUnknown
	}
}

// isUnreachable 错误是否属于「连不上」类。
func isUnreachable(err error) bool {
	// stdlib 判据单源下沉 iface.ClassifyStdlibKind（net/syscall/EOF/PathError/ctx）；
	// transport.ErrTransportClosed（mcp-go 类型化断连，selfheal 面同源）由
	// ClassifyError 调用点单独补判。
	return iface.ClassifyStdlibKind(err) == KindUnreachable
}

// ClassifiedError 分类载体单源在 iface；别名透传（errors.As 判定跨包同型）。
type ClassifiedError = iface.ClassifiedError

// Classify 归类并包装（保留原始错误链，errors.Is/As 仍可穿透）。
func Classify(err error) error {
	if err == nil {
		return nil
	}
	return &ClassifiedError{Kind: ClassifyError(err), Err: err}
}

// KindOf 取错误分类（未包装时也能直接判——协议判定单源仍在本包，需要
// inspect mcp-go）。零 mcp-go 消费面用 iface.KindOf（纯解包，出口已包定）。
func KindOf(err error) ErrorKind {
	var ce *ClassifiedError
	if errors.As(err, &ce) {
		return ce.Kind
	}
	return ClassifyError(err)
}
