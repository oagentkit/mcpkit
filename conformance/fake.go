// fake transport 实现（被测工具面的内存替身）：合规 + 故意违规（负对照）。
// 负对照存在的唯一理由是证明套件判得红——contract_test.go 钉住「违规必判红」。
package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/oagentkit/mcpkit/iface"
)

// CompliantTransport 合规 fake transport（正对照）：
//   - 分类锚 wire 事实（JSON-RPC 错误码 / 错误链 stdlib 事实 / isError 信号），
//     不看 Message 文案；
//   - 成功结果原样序列化完整 CallToolResult JSON 信封（含 _meta，不剥）；
//   - isError:true 业务失败保守归 KindUnknown（mcpkit#144 下限）。
//
// 分类逻辑是**参考实现**（wire 事实 → ErrorKind 的对照样板）；判定单源
// 仍在 mcp/mcp.ClassifyError，本类型不构成第二判定源。
type CompliantTransport struct {
	backend Backend
}

// NewCompliantTransport 用给定脚本化 backend 装配合规 fake transport。
func NewCompliantTransport(backend Backend) *CompliantTransport {
	return &CompliantTransport{backend: backend}
}

// NewCompliantContract 正对照 Contract：Run(t, NewCompliantContract()) 必须全绿。
func NewCompliantContract() Contract {
	return Contract{New: func(t *testing.T, backend Backend) Surface {
		return NewCompliantTransport(backend)
	}}
}

func (f *CompliantTransport) Invoke(ctx context.Context, tool, args string) (string, error) {
	if args == "" {
		args = "{}"
	}
	res, err := f.backend.CallTool(ctx, tool, json.RawMessage(args))
	if err != nil {
		return "", classifyByFact(err)
	}
	if res == nil {
		// backend 违约（既无结果也无错误）= 协议层事实，归 protocol。
		return "", &iface.ClassifiedError{Kind: iface.KindProtocol, Err: errors.New("conformance: backend 返回空结果且无错误")}
	}
	payload, merr := json.Marshal(res)
	if merr != nil {
		return "", fmt.Errorf("conformance: 结果信封序列化失败: %w", merr)
	}
	if res.IsError {
		// 业务失败：无协议码信号，保守归 KindUnknown（契约下限；mcpkit 单源
		// 允许对 payload 窄信号上浮精化，见包注释）。
		return "", &iface.ClassifiedError{
			Kind: iface.KindUnknown,
			Err:  fmt.Errorf("tool business failure: %s", payload),
		}
	}
	return string(payload), nil
}

// classifyByFact 事实锚定分类：JSON-RPC 错误码定类（不看 Message）；
// 其余走 iface.Classify（stdlib 链上事实：net/EOF/PathError/ctx）。
func classifyByFact(err error) error {
	if err == nil {
		return nil
	}
	var rpc *RPCError
	if errors.As(err, &rpc) {
		return &iface.ClassifiedError{Kind: kindOfRPCCode(rpc.Code), Err: err}
	}
	return iface.Classify(err)
}

// kindOfRPCCode JSON-RPC 错误码 → 分类（映射与 mcp.ClassifyRPCCode 同口径）。
func kindOfRPCCode(code int) iface.ErrorKind {
	switch code {
	case CodeInvalidParams:
		return iface.KindInvalidArgs
	case CodeMethodNotFound:
		return iface.KindToolMissing
	default:
		return iface.KindProtocol
	}
}

// ViolatingTransport 故意违规 fake transport（负对照）：两处经典反模式，
// 合起来保证套件的分类漂移/信封子测试对它判红：
//
//  1. **按文案猜分类**（历史反模式：strings.Contains(err.Error(), ...)）——
//     同一事实换文案，分类就漂；
//  2. **剥元数据信封**——只回裸文本，_meta/content 结构丢失。
type ViolatingTransport struct {
	backend Backend
}

// NewViolatingTransport 用给定脚本化 backend 装配违规 fake transport。
func NewViolatingTransport(backend Backend) *ViolatingTransport {
	return &ViolatingTransport{backend: backend}
}

// NewViolatingContract 负对照 Contract：套件必须对它判红
// （contract_test.go 的 TestNegativeControlViolatingTransport 钉住）。
func NewViolatingContract() Contract {
	return Contract{New: func(t *testing.T, backend Backend) Surface {
		return NewViolatingTransport(backend)
	}}
}

func (f *ViolatingTransport) Invoke(ctx context.Context, tool, args string) (string, error) {
	if args == "" {
		args = "{}"
	}
	res, err := f.backend.CallTool(ctx, tool, json.RawMessage(args))
	if err != nil {
		// VIOLATION：按错误文案猜分类，wire 事实（错误码/错误链）被无视。
		return "", &iface.ClassifiedError{Kind: textKind(err.Error()), Err: err}
	}
	if res == nil {
		return "", &iface.ClassifiedError{Kind: iface.KindUnknown, Err: errors.New("conformance: empty result")}
	}
	payload, merr := json.Marshal(res)
	if merr != nil {
		return "", fmt.Errorf("conformance: 结果信封序列化失败: %w", merr)
	}
	if res.IsError {
		// VIOLATION：业务失败同样按文案猜类。
		return "", &iface.ClassifiedError{
			Kind: textKind(string(payload)),
			Err:  fmt.Errorf("tool business failure: %s", payload),
		}
	}
	// VIOLATION：剥掉元数据信封，只回首块裸文本。
	if len(res.Content) > 0 && res.Content[0].Type == "text" {
		return res.Content[0].Text, nil
	}
	return "", nil
}

// textKind 按文案猜分类（反模式样板）：关键词命中顺序敏感，文案一改结论就变。
func textKind(s string) iface.ErrorKind {
	low := strings.ToLower(s)
	switch {
	case strings.Contains(low, "tool not found"),
		strings.Contains(low, "unknown tool"),
		strings.Contains(low, "no such tool"):
		return iface.KindToolMissing
	case strings.Contains(low, "invalid"), strings.Contains(low, "bad param"):
		return iface.KindInvalidArgs
	case strings.Contains(low, "internal"), strings.Contains(low, "parse error"), strings.Contains(low, "protocol"):
		return iface.KindProtocol
	case strings.Contains(low, "refused"), strings.Contains(low, "timeout"),
		strings.Contains(low, "eof"), strings.Contains(low, "deadline"), strings.Contains(low, "exceeded"):
		return iface.KindUnreachable
	default:
		return iface.KindUnknown
	}
}
