// Package iface MCP 工具调用面的协议中立声明：失败分类学（ErrorKind）、
// 结果分类载体（ClassifiedError）与工具能力接口（RawNamer/OutputSchemer）。
//
// # 为什么单拆一个包（tier 叙事的最后一公里）
//
// mcp-go 是重依赖，矩阵约定「隔离在 mcpkit/mcp」。但 mcp/mcp 的出口符号里
// 混着几个**协议中立**的形状（RawNamer/OutputSchemer/ErrorKind/ClassifiedError
// ——签名不含任何 mcp-go 类型），下游 toolgate 只需要它们，却因为 import
// mcp/mcp 把 mcp-go 及其闭包拖进了自己的模块图（go.mod indirect 实锤）。
// 「mcp-go 隔离在 mcpkit」在 require 级为真、在模块图级为假——本包把协议
// 中立面拆出来，下游零重依赖消费成为可能。
//
// 约束：本包**只准标准库**——它服务的是「不要 mcp-go 的消费方」，自己先把
// 地板钉死（guardx/invoke 自 toolgate/invoke 迁出的同款纪律）。
//
// 判定行为仍单源在 mcp/mcp：ClassifyError（要 inspect mcp-go transport 错误
// 类型）与 Classify（包装）不进本包；mcp/mcp 在工具调用出口把错误包成
// [ClassifiedError]，本包的 [KindOf] 经 errors.As 解包即可取分类——协议层
// 事实在出口已落定，消费方无需再触协议实现。
//
// 生命周期: Experimental——未过一个 minor 无 breaking 窗口（api-compat.md 晋级条件一），初值不承诺兼容
package iface

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
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

// RawNamer 原始工具名能力接口（oagentkit/mcpkit#46）：暴露名带 server 命名
// 空间是给模型看的；按名做判定的消费方（动词启发式/只读白名单/审计归类）
// 要的是原始名。经 tool.BaseTool 类型断言本接口取原始名（比字符串剥前缀
// 可靠——server 名含 "." 时字符串剥离有歧义）。
type RawNamer interface {
	RawToolName() string
}

// OutputSchemer 工具出参 schema 的可选能力接口（MCP 2025-06-18
// outputSchema；eino tool.BaseTool 为封闭接口，schema 进不了 ToolInfo，
// 故走类型断言）。
type OutputSchemer interface {
	OutputSchema() json.RawMessage
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

// KindOf 取错误分类：errors.As 解包 ClassifiedError（协议层事实在 mcp/mcp
// 出口已包定——JSON-RPC 码/业务 payload 的判定单源在那边）；链上无分类载体
// 时回退 [ClassifyStdlibKind]（stdlib 可判的传输层事实——宿主自产的网络错误
// 也拿得到分类，不必为分类引 mcp-go）。两者都判不出才归 KindUnknown。
func KindOf(err error) ErrorKind {
	var ce *ClassifiedError
	if errors.As(err, &ce) {
		return ce.Kind
	}
	return ClassifyStdlibKind(err)
}

// Classify 归类并包装为 [ClassifiedError]（保留原始错误链，errors.Is/As 仍可
// 穿透）。判定用 [ClassifyStdlibKind]——本包只做 stdlib 可判的传输层事实；
// 协议层判定（JSON-RPC 码/业务 payload）单源在 mcp/mcp.Classify。
func Classify(err error) error {
	if err == nil {
		return nil
	}
	return &ClassifiedError{Kind: ClassifyStdlibKind(err), Err: err}
}

// ClassifyStdlibKind stdlib 可判的传输层分类（协议中立判据的全部）：
// net.Error / 连接族 syscall / io.EOF（stdio server 死亡的经典形态）/
// os.ErrNotExist 与 *os.PathError（进程启动失败）/ ctx 超时取消。
// JSON-RPC 码与 isError payload 的判定不在这里（要 inspect mcp-go，会破
// 本包地板）——那部分是 mcp/mcp.ClassifyError 的单源职责。
func ClassifyStdlibKind(err error) ErrorKind {
	if err == nil {
		return KindUnknown
	}
	// net.Error（含超时）
	var ne net.Error
	if errors.As(err, &ne) {
		return KindUnreachable
	}
	// 系统层：连接被拒 / 主机不可达 / 管道破裂；io.EOF（stdio server 死亡 =
	// stdout 读到裸 EOF，mcpkit#52「最常见故障在分类盲区」）
	for _, target := range []error{
		os.ErrNotExist,
		syscall.ECONNREFUSED,
		syscall.ECONNRESET,
		syscall.EHOSTUNREACH,
		syscall.ENETUNREACH,
		syscall.EPIPE,
		io.EOF,
	} {
		if errors.Is(err, target) {
			return KindUnreachable
		}
	}
	// 进程启动失败（stdio 传输的可执行文件不存在）
	var execErr *os.PathError
	if errors.As(err, &execErr) {
		return KindUnreachable
	}
	// 上下文超时或取消（连不上的一种）
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return KindUnreachable
	}
	return KindUnknown
}
