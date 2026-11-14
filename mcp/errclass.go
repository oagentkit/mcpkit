package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// ErrorKind MCP 调用失败的稳定分类。
//
// 为什么要有枚举：下游此前各自做**字符串匹配**（`strings.Contains(err.Error(),
// "connection refused")`）判「连不上」还是「工具不存在」，文案一改就失效。
// 枚举把判定锚在**协议层事实**上（JSON-RPC 错误码 / net.Error / 错误链类型），
// 不随实现文案漂移。
//
// 处置口径（这四类的处置完全不同，不要混）：
//
//	KindUnreachable —— 连不上。基础设施问题：重试 / 换节点 / 报警。
//	KindProtocol    —— 协议错。server 实现不合规或版本不匹配：修 server / 升级。
//	KindToolMissing —— 工具不存在。**配置或契约问题**：server 起来了但没有这个
//	                   工具（装包≠开洞、工具面未声明）。不要重试——重试一万次
//	                   它也不会出现。改配置或改契约。
//	KindInvalidArgs —— 参数错。**调用问题**：工具在、但传参不合法。改调用方。
//	KindUnknown     —— 判不出（兜底，含 isError:true 业务失败的**无明确信号**
//	                   payload，oagentkit/mcpkit#144）。保守不重试：确定性业务
//	                   失败（"pod not found"、权限不足、业务校验拒绝）混在里面，
//	                   盲重试只会把失败放大——读工具反馈人工研判后再决定处置。
type ErrorKind int

const (
	// KindUnknown 未归类（兜底）。
	KindUnknown ErrorKind = iota
	// KindUnreachable 连不上（网络/进程层）。
	KindUnreachable
	// KindProtocol 协议层错误（JSON-RPC 错误码非「工具不存在/参数错」）。
	KindProtocol
	// KindToolMissing 工具不存在（契约/配置问题）。
	KindToolMissing
	// KindInvalidArgs 参数错（调用问题）。
	KindInvalidArgs
)

// String 分类名（稳定标识，可进日志/指标标签）。
func (k ErrorKind) String() string {
	switch k {
	case KindUnreachable:
		return "unreachable"
	case KindProtocol:
		return "protocol"
	case KindToolMissing:
		return "tool_missing"
	case KindInvalidArgs:
		return "invalid_args"
	default:
		return "unknown"
	}
}

// MarshalJSON 输出可读分类名（不是数字）——JSON 消费方（监控/告警）不该
// 看到裸 int 枚举值。
func (k ErrorKind) MarshalJSON() ([]byte, error) {
	return json.Marshal(k.String())
}

// UnmarshalJSON 支持从分类名反解。未知名串 fail-fast（oagentkit/mcpkit#54）：
// 此前静默归 KindUnknown——监控/告警配置里的 typo（"unreachble"）零可见性地
// 降级为 unknown（=不可重试），与序列化侧「监控不该看到裸 int」的用心不对称。
//
// 空串显式归 KindUnknown（oagentkit/mcpkit#90，#54 fail-fast 的唯一例外）：
// 本包序列化侧（String/MarshalJSON）从不产出空串（KindUnknown 序列化为
// "unknown"），空串只来自手写配置——监控/告警过滤器里 `"kind": ""`（留空表示
// 「不筛选」）是最常见形态；字段缺失（{}）不进本方法，显式留空却曾落 default
// fail-fast——「看起来最该宽容的写法最严」。空串无 typo 可言（零信息量），
// 与错误面口径一致：KindOf(nil)/ClassifyError(nil) 均归 KindUnknown。
func (k *ErrorKind) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	switch s {
	case "", "unknown": // 空串：显式留空 = 未知/不筛选（手写配置合法形态，oagentkit/mcpkit#90）
		*k = KindUnknown
	case "unreachable":
		*k = KindUnreachable
	case "protocol":
		*k = KindProtocol
	case "tool_missing":
		*k = KindToolMissing
	case "invalid_args":
		*k = KindInvalidArgs
	default:
		return fmt.Errorf("errclass: 未知错误分类名 %q（typo 会在监控配置里静默降级，故 fail-fast）", s)
	}
	return nil
}

// Retryable 该类错误重试是否有意义。
//
// 关键：**工具不存在与参数错重试没有意义**——重试不会让 server 长出那个工具，
// 也不会让非法参数变合法。盲目重试会把「配置问题」伪装成「偶发抖动」。
// 同理，KindUnknown（判不出，含 isError:true 无明确信号的业务失败）也**不重试**
// （oagentkit/mcpkit#144）：这类 payload 里混着大量确定性业务失败（资源不存在、
// 权限不足、业务校验拒绝），保守不重试——错在安全侧，宁可漏掉可重试的真抖动
// （调用方还有传输层自愈兜底），不把确定性失败刷成重试风暴。
func (k ErrorKind) Retryable() bool {
	switch k {
	case KindUnreachable:
		return true
	case KindProtocol:
		return true // server 短暂异常时重试可能恢复
	default:
		return false
	}
}

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
	// 网络 / 进程层
	if isUnreachable(err) {
		return KindUnreachable
	}
	// 上下文超时或取消
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
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
	// net.Error（含超时）
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	// 系统层：连接被拒 / 主机不可达 / 管道破裂；io.EOF（stdio server 死亡的
	// 经典形态 = stdout 读到裸 EOF，此前落 KindUnknown 不重试不自愈——
	// mcpkit#52「最常见故障在分类盲区」）；transport.ErrTransportClosed（
	// mcp-go 类型化断连，selfheal 面同源）
	for _, target := range []error{
		os.ErrNotExist,
		syscall.ECONNREFUSED,
		syscall.ECONNRESET,
		syscall.EHOSTUNREACH,
		syscall.ENETUNREACH,
		syscall.EPIPE,
		io.EOF,
		transport.ErrTransportClosed,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	// 进程启动失败（stdio 传输的可执行文件不存在）
	var execErr *os.PathError
	return errors.As(err, &execErr)
}

// ClassifiedError 带分类的错误（便于 errors.As 取分类，同时保留原始错误链）。
type ClassifiedError struct {
	Kind ErrorKind
	Err  error
}

func (e *ClassifiedError) Error() string {
	return e.Err.Error()
}

func (e *ClassifiedError) Unwrap() error { return e.Err }

// Classify 归类并包装（保留原始错误链，errors.Is/As 仍可穿透）。
func Classify(err error) error {
	if err == nil {
		return nil
	}
	return &ClassifiedError{Kind: ClassifyError(err), Err: err}
}

// KindOf 取错误分类（未包装时也能直接判）。
func KindOf(err error) ErrorKind {
	var ce *ClassifiedError
	if errors.As(err, &ce) {
		return ce.Kind
	}
	return ClassifyError(err)
}
