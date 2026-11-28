// 契约矩阵（对标 cachetest.Contract + Run 形态）：每个命名子测试钉一条必须
// 跨实现一致的语义；实现方经 Contract.New 把被测工具面装配到套件给定的
// 脚本化 Backend 上，套件只断言出口事实（分类 / 信封 / 能力接口口径）。
package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"syscall"
	"testing"

	"github.com/oagentkit/mcpkit/iface"
)

// JSON-RPC 2.0 错误码（spec 固定值）。分类映射的判定单源在
// mcp/mcp.ClassifyRPCCode（-32602→invalid_args、-32601→tool_missing、
// 其余协议码→protocol）；本包把同一映射写进契约（wire 事实 → ErrorKind）。
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// RawContent 信封里的内容块（wire 层协议中立形状）。
type RawContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// RawResult wire 层 CallToolResult：分类与信封断言的输入事实。
//
// IsError=true 是「工具执行了但业务失败」（mcpkit#144 形态）——没有 JSON-RPC
// 错误码，只有 payload 文案；分类下限见 iface.KindUnknown 的处置口径。
type RawResult struct {
	Content           []RawContent    `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
	Meta              map[string]any  `json:"_meta,omitempty"`
}

// RPCError wire 层 JSON-RPC 错误。分类必须锚在 Code（协议层事实）上，
// 不得看 Message（实现文案）——「不随文案漂移」钉的就是这条。
type RPCError struct {
	Code    int
	Message string
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}

// Backend 脚本化原始 server：被测工具面背后的 seam（对标 cachetest 每场景
// 交给 factory 的 source）。每个子测试按自己的语义脚本一个 Backend，
// 实现方的 Factory 把自己的分类/出口层装配到它上面。
type Backend interface {
	// CallTool 处理一次 tools/call：
	//   - 成功 / 业务失败（IsError:true）回 *RawResult；
	//   - 传输层故障（net/EOF/ctx 系 Go error）与 JSON-RPC 错误（*RPCError）回 error。
	CallTool(ctx context.Context, name string, args json.RawMessage) (*RawResult, error)
}

// Surface 被测工具面（消费方看到的形态）。
type Surface interface {
	// Invoke 调一次工具：
	//   - 成功：返回**完整 CallToolResult JSON 信封**（含 _meta 元数据信封，不许剥）；
	//   - 失败：返回 error，链上分类经 iface.KindOf 取到稳定 ErrorKind。
	Invoke(ctx context.Context, tool, args string) (string, error)
}

// Factory 按脚本化 Backend 装配被测工具面。每个场景调用一次，
// 被测面的后台资源经 t.Cleanup 清理。
type Factory func(t *testing.T, backend Backend) Surface

// Contract 被测实现的构造入口（对标 cachetest.Contract 的抽象位）。
type Contract struct {
	// New 按脚本化 backend 构造全新被测 Surface。
	New Factory
}

// retryAttempts 「重试一万次也不会出现」的字面口径（iface.ErrorKind 文档：
// KindToolMissing 不要重试——重试一万次它也不会出现）。内存 fake 无 IO，
// 一万次调用毫秒级，直接钉字面量。
const retryAttempts = 10000

// Run 跑完整契约矩阵（命名子测试，每子测试钉一条语义；任何子测试失败=违反契约）。
//
// 每个子测试覆盖一条必须跨实现一致的语义：
//
//  1. errkind_unreachable_shape        连不上的典型形态（连接被拒/EOF/ctx 超时）
//     → KindUnreachable；逆文案（提到别的类关键词）不得把分类带偏
//  2. errkind_protocol_shape           协议错（JSON-RPC 内部错/解析错）
//     → KindProtocol；分类锚错误码不锚 Message 文案
//  3. errkind_toolmissing_shape        工具不存在（JSON-RPC method-not-found 码）
//     → KindToolMissing；文案说「connection refused」也翻不了案
//  4. errkind_invalidargs_shape        参数错（JSON-RPC invalid-params 码）
//     → KindInvalidArgs；同上
//  5. errkind_unknown_business_failure isError:true 业务失败（mcpkit#144）
//     → KindUnknown；业务文案漂移不许上浮成别的类
//  6. no_retry_toolmissing_invalidargs 两类确定性失败不重试：一万次调用分类
//     恒定且 Retryable() 恒 false
//  7. no_retry_unknown_conservative    KindUnknown 保守不重试
//  8. envelope_meta_not_stripped       工具结果出口必带元数据信封，
//     消费方视角断言 content/structuredContent/_meta 不被剥离
//  9. rawnamer_zero_default            iface.RawNamer 零值/缺省的消费口径
//
// 10. outputschemer_zero_default      iface.OutputSchemer 零值/缺省的消费口径
func Run(t *testing.T, c Contract) {
	t.Helper()

	t.Run("errkind_unreachable_shape", func(t *testing.T) { mustPass(t, checkUnreachable(t, c)) })
	t.Run("errkind_protocol_shape", func(t *testing.T) { mustPass(t, checkProtocol(t, c)) })
	t.Run("errkind_toolmissing_shape", func(t *testing.T) { mustPass(t, checkToolMissing(t, c)) })
	t.Run("errkind_invalidargs_shape", func(t *testing.T) { mustPass(t, checkInvalidArgs(t, c)) })
	t.Run("errkind_unknown_business_failure", func(t *testing.T) { mustPass(t, checkUnknown(t, c)) })
	t.Run("no_retry_toolmissing_invalidargs", func(t *testing.T) { mustPass(t, checkNoRetryPermanent(t, c)) })
	t.Run("no_retry_unknown_conservative", func(t *testing.T) { mustPass(t, checkNoRetryUnknown(t, c)) })
	t.Run("envelope_meta_not_stripped", func(t *testing.T) { mustPass(t, checkEnvelope(t, c)) })
	t.Run("rawnamer_zero_default", func(t *testing.T) { mustPass(t, checkRawNamer(t, c)) })
	t.Run("outputschemer_zero_default", func(t *testing.T) { mustPass(t, checkOutputSchemer(t, c)) })
}

func mustPass(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// backendFunc 函数形态的 Backend（各子测试的脚本闭包，对标 cachetest 的
// source 闭包形态）。
type backendFunc func(ctx context.Context, name string, args json.RawMessage) (*RawResult, error)

func (f backendFunc) CallTool(ctx context.Context, name string, args json.RawMessage) (*RawResult, error) {
	return f(ctx, name, args)
}

// cyclingBackend 按调用序在 texts 间轮转文案、按 shape 产出 wire 响应：
// 同一事实换文案，正是「分类不随文案漂移」的考法。
func cyclingBackend(texts []string, shape func(text string) (*RawResult, error)) Backend {
	calls := 0
	return backendFunc(func(ctx context.Context, name string, args json.RawMessage) (*RawResult, error) {
		text := texts[calls%len(texts)]
		calls++
		return shape(text)
	})
}

// rpcShape 事实=JSON-RPC 错误码、文案=Message 的 wire 形态。
func rpcShape(code int) func(text string) (*RawResult, error) {
	return func(text string) (*RawResult, error) {
		return nil, &RPCError{Code: code, Message: text}
	}
}

// transportShape 事实=错误链上的 stdlib 哨兵（net/EOF/ctx 系）、文案=包装文本
// 的 wire 形态。合规实现必须按链上事实分类（errors.Is/As），不得看包装文案。
func transportShape(fact error) func(text string) (*RawResult, error) {
	return func(text string) (*RawResult, error) {
		return nil, fmt.Errorf("%s: %w", text, fact)
	}
}

// businessShape 事实=isError:true（无协议码信号）、文案=payload 文本的 wire 形态。
func businessShape(text string) (*RawResult, error) {
	return &RawResult{
		IsError: true,
		Content: []RawContent{{Type: "text", Text: text}},
	}, nil
}

// classifyCase 一类错误形态的钉子：事实固定，文案变体轮转。
type classifyCase struct {
	name  string
	shape func(text string) (*RawResult, error)
	want  iface.ErrorKind
	// variants 文案变体：首个为顺文案（与事实同向），其余为逆文案（文案里
	// 带别的类关键词）——逆文案下分类仍停在 want 才叫「不随文案漂移」。
	variants []string
}

// runClassify 共用钉子：逐文案变体 Invoke 一次，KindOf 必须停在 want。
func runClassify(t *testing.T, c Contract, tc classifyCase) error {
	t.Helper()
	face := c.New(t, cyclingBackend(tc.variants, tc.shape))
	for _, text := range tc.variants {
		_, err := face.Invoke(context.Background(), "conformance.echo", "{}")
		if err == nil {
			return fmt.Errorf("%s（文案 %q）：期望分类失败，got 成功", tc.name, text)
		}
		if got := iface.KindOf(err); got != tc.want {
			return fmt.Errorf("%s（文案 %q）：KindOf=%s，期望 %s——分类随文案漂移", tc.name, text, got, tc.want)
		}
	}
	return nil
}

// checkUnreachable 场景 1：连不上的典型形态（连接被拒 / io.EOF / ctx 超时）
// → KindUnreachable。逆文案里带 tool-missing / invalid-args 关键词也不得翻案。
func checkUnreachable(t *testing.T, c Contract) error {
	t.Helper()
	cases := []classifyCase{
		{
			name:  "连接被拒",
			shape: transportShape(syscall.ECONNREFUSED),
			want:  iface.KindUnreachable,
			variants: []string{
				"dial tcp 127.0.0.1:1",
				"tool not found in upstream registry",
			},
		},
		{
			name:  "stdio server 死亡（io.EOF）",
			shape: transportShape(io.EOF),
			want:  iface.KindUnreachable,
			variants: []string{
				"read stdout",
				"invalid params: unexpected",
			},
		},
		{
			name:  "ctx 超时",
			shape: transportShape(context.DeadlineExceeded),
			want:  iface.KindUnreachable,
			variants: []string{
				"probe deadline",
				"tool not found after wait",
			},
		},
	}
	for _, tc := range cases {
		if err := runClassify(t, c, tc); err != nil {
			return err
		}
	}
	return nil
}

// checkProtocol 场景 2：JSON-RPC 协议错（内部错 / 解析错）→ KindProtocol。
// 分类锚错误码；Message 说「connection refused」也不得翻成 unreachable。
func checkProtocol(t *testing.T, c Contract) error {
	t.Helper()
	cases := []classifyCase{
		{
			name:  "JSON-RPC 内部错",
			shape: rpcShape(CodeInternalError),
			want:  iface.KindProtocol,
			variants: []string{
				"internal error: schema mismatch",
				"connection refused while handling request",
			},
		},
		{
			name:  "JSON-RPC 解析错",
			shape: rpcShape(CodeParseError),
			want:  iface.KindProtocol,
			variants: []string{
				"parse error: bad json",
				"tool not found while parsing",
			},
		},
	}
	for _, tc := range cases {
		if err := runClassify(t, c, tc); err != nil {
			return err
		}
	}
	return nil
}

// checkToolMissing 场景 3：工具不存在（JSON-RPC method-not-found 码）
// → KindToolMissing。文案说「connection refused」也翻不了案——工具不存在
// 是配置/契约事实，不随服务端话术变。
func checkToolMissing(t *testing.T, c Contract) error {
	t.Helper()
	return runClassify(t, c, classifyCase{
		name:  "工具不存在（method-not-found）",
		shape: rpcShape(CodeMethodNotFound),
		want:  iface.KindToolMissing,
		variants: []string{
			"tool not found: echo",
			"connection refused while dialing registry",
		},
	})
}

// checkInvalidArgs 场景 4：参数错（JSON-RPC invalid-params 码）
// → KindInvalidArgs。文案说「tool not found」也翻不了案。
func checkInvalidArgs(t *testing.T, c Contract) error {
	t.Helper()
	return runClassify(t, c, classifyCase{
		name:  "参数错（invalid-params）",
		shape: rpcShape(CodeInvalidParams),
		want:  iface.KindInvalidArgs,
		variants: []string{
			"invalid params: field count",
			"tool not found in catalog",
		},
	})
}

// checkUnknown 场景 5：isError:true 业务失败（无协议码信号，mcpkit#144）
// → KindUnknown。业务文案里带「connection refused」等词不得上浮成别的类——
// payload 兜底面是确定性业务失败的海洋，保守归 unknown。
func checkUnknown(t *testing.T, c Contract) error {
	t.Helper()
	return runClassify(t, c, classifyCase{
		name:  "isError:true 业务失败",
		shape: businessShape,
		want:  iface.KindUnknown,
		variants: []string{
			"pod not found in workspace",
			"connection refused while listing pods",
			"quota exhausted for workspace",
		},
	})
}

// checkNoRetryPermanent 场景 6：KindToolMissing / KindInvalidArgs 不重试——
// 重试一万次也不会出现（iface 文档口径）。一万次调用里分类必须恒定，
// 且 Retryable() 恒 false；文案轮转（顺/逆）也不许把不重试口径带偏。
func checkNoRetryPermanent(t *testing.T, c Contract) error {
	t.Helper()
	cases := []struct {
		name  string
		code  int
		want  iface.ErrorKind
		texts []string
	}{
		{
			name:  "tool_missing",
			code:  CodeMethodNotFound,
			want:  iface.KindToolMissing,
			texts: []string{"tool not found: echo", "connection refused while dialing registry"},
		},
		{
			name:  "invalid_args",
			code:  CodeInvalidParams,
			want:  iface.KindInvalidArgs,
			texts: []string{"invalid params: field count", "tool not found in catalog"},
		},
	}
	for _, tc := range cases {
		face := c.New(t, cyclingBackend(tc.texts, rpcShape(tc.code)))
		for i := 0; i < retryAttempts; i++ {
			_, err := face.Invoke(context.Background(), "conformance.echo", "{}")
			if err == nil {
				return fmt.Errorf("%s 第 %d 次：期望失败（%s 是确定性失败）", tc.name, i, tc.want)
			}
			kind := iface.KindOf(err)
			if kind != tc.want {
				return fmt.Errorf("%s 第 %d 次：KindOf=%s，期望 %s——重试一万次也不会出现的失败不得漂移", tc.name, i, kind, tc.want)
			}
			if kind.Retryable() {
				return fmt.Errorf("%s 第 %d 次：%s 报 Retryable——不重试口径被破坏", tc.name, i, kind)
			}
		}
	}
	return nil
}

// checkNoRetryUnknown 场景 7：KindUnknown 保守不重试（mcpkit#144）——
// isError:true 业务失败里混着确定性失败（资源不存在/权限不足/业务校验拒绝），
// 盲重试只会把失败放大。分类恒 unknown 且 Retryable() 恒 false。
func checkNoRetryUnknown(t *testing.T, c Contract) error {
	t.Helper()
	texts := []string{
		"pod not found in workspace",
		"connection refused while listing pods",
		"quota exhausted for workspace",
	}
	face := c.New(t, cyclingBackend(texts, businessShape))
	for _, text := range texts {
		_, err := face.Invoke(context.Background(), "conformance.echo", "{}")
		if err == nil {
			return fmt.Errorf("业务失败（文案 %q）：期望 error", text)
		}
		kind := iface.KindOf(err)
		if kind != iface.KindUnknown {
			return fmt.Errorf("业务失败（文案 %q）：KindOf=%s，期望 unknown（保守下限）", text, kind)
		}
		if kind.Retryable() {
			return fmt.Errorf("业务失败（文案 %q）：unknown 报 Retryable——保守不重试被破坏", text)
		}
	}
	return nil
}

// envelopeView 消费方视角的信封视图（CallToolResult JSON 的解析形状）。
// 断言从这里出发：解析不出信封 / 元数据缺失 = 契约被破坏。
type envelopeView struct {
	Content           []RawContent    `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
	Meta              map[string]any  `json:"_meta"`
}

// checkEnvelope 场景 8：元数据信封契约——工具结果出口必带完整 CallToolResult
// JSON 信封，消费方视角断言 content / structuredContent / _meta 不被剥掉
// （跨仓集成契约：信封包装是特性不是缺陷，别在调用方剥掉）。空结果也必须
// 是信封形态（「必带」），不得回裸文本/空串。
func checkEnvelope(t *testing.T, c Contract) error {
	t.Helper()
	ctx := context.Background()

	cases := []struct {
		name string
		res  *RawResult
		// check 在信封视图上做消费方断言。
		check func(env *envelopeView) error
	}{
		{
			name: "text+meta",
			res: &RawResult{
				Content: []RawContent{{Type: "text", Text: "hello"}},
				Meta:    map[string]any{"trace_id": "t-42", "tenant": "acme"},
			},
			check: func(env *envelopeView) error {
				if len(env.Content) != 1 || env.Content[0].Type != "text" || env.Content[0].Text != "hello" {
					return fmt.Errorf("content 被剥/改写: %+v", env.Content)
				}
				if env.Meta["trace_id"] != "t-42" || env.Meta["tenant"] != "acme" {
					return fmt.Errorf("_meta 被剥: %+v", env.Meta)
				}
				return nil
			},
		},
		{
			name: "structuredContent+meta",
			res: &RawResult{
				StructuredContent: json.RawMessage(`{"count":3}`),
				Meta:              map[string]any{"trace_id": "t-43"},
			},
			check: func(env *envelopeView) error {
				if !json.Valid(env.StructuredContent) || string(env.StructuredContent) != `{"count":3}` {
					return fmt.Errorf("structuredContent 被剥: %s", env.StructuredContent)
				}
				if env.Meta["trace_id"] != "t-43" {
					return fmt.Errorf("_meta 被剥: %+v", env.Meta)
				}
				return nil
			},
		},
		{
			name: "空结果仍必带信封",
			res:  &RawResult{},
			check: func(env *envelopeView) error {
				// 能落进信封视图（而非裸文本）即「必带」成立。
				return nil
			},
		},
	}
	for _, tc := range cases {
		res := tc.res
		face := c.New(t, backendFunc(func(ctx context.Context, name string, args json.RawMessage) (*RawResult, error) {
			return res, nil
		}))
		out, err := face.Invoke(ctx, "conformance.echo", "{}")
		if err != nil {
			return fmt.Errorf("%s：期望成功结果，got %v", tc.name, err)
		}
		var env envelopeView
		if jerr := json.Unmarshal([]byte(out), &env); jerr != nil {
			return fmt.Errorf("%s：出口不是元数据信封（被剥成裸文本/非 JSON）：%v，out=%q", tc.name, jerr, out)
		}
		if cerr := tc.check(&env); cerr != nil {
			return fmt.Errorf("%s：%w", tc.name, cerr)
		}
	}
	return nil
}

// rawNamed 实现 iface.RawNamer 的工具双（带原始名）。
type rawNamed struct{ raw string }

func (r rawNamed) RawToolName() string { return r.raw }

// plainTool 两个能力接口都不实现的工具双（缺省形态）。
type plainTool struct{}

// schemed 实现 iface.OutputSchemer 的工具双（带出参 schema）。
type schemed struct{ schema json.RawMessage }

func (s schemed) OutputSchema() json.RawMessage { return s.schema }

// rawNameOf 消费方解析原始名的契约口径（对齐 mcp.RawToolNameOf 的缺省语义）：
// 能力在且非空直取；缺能力或回空串（零值）一律回落暴露名。
func rawNameOf(exposed string, impl any) string {
	if rn, ok := impl.(iface.RawNamer); ok {
		if raw := rn.RawToolName(); raw != "" {
			return raw
		}
	}
	return exposed
}

// outputSchemaOf 消费方解析出参 schema 的契约口径（对齐 mcp.OutputSchemaOf）：
// 缺能力、或 OutputSchema() 回空（零值）一律「未声明」。
func outputSchemaOf(impl any) (json.RawMessage, bool) {
	os, ok := impl.(iface.OutputSchemer)
	if !ok {
		return nil, false
	}
	raw := os.OutputSchema()
	return raw, len(raw) > 0
}

// checkRawNamer 场景 9：iface.RawNamer 零值/缺省的消费口径——能力在且非空
// 取原始名；缺能力/回零值回落暴露名；原始名含 "." 原样返回（mcpkit#46：
// 用类型断言而非字符串剥前缀，server 名含点时字符串剥离有歧义）。
// c 不参与本场景断言（纯 iface 语义，统一签名以 _ 占位）。
func checkRawNamer(t *testing.T, _ Contract) error {
	t.Helper()
	cases := []struct {
		name    string
		exposed string
		impl    any
		want    string
	}{
		{"有能力取原始名", "srv.echo", rawNamed{raw: "echo"}, "echo"},
		{"缺能力回落暴露名", "srv.echo", plainTool{}, "srv.echo"},
		{"零值（空串）回落暴露名", "srv.echo", rawNamed{raw: ""}, "srv.echo"},
		{"原始名含点不剥前缀", "srv.run.x", rawNamed{raw: "run.x"}, "run.x"},
	}
	for _, tc := range cases {
		if got := rawNameOf(tc.exposed, tc.impl); got != tc.want {
			return fmt.Errorf("%s：rawNameOf=%q，期望 %q", tc.name, got, tc.want)
		}
	}
	return nil
}

// checkOutputSchemer 场景 10：iface.OutputSchemer 零值/缺省的消费口径——
// 能力在且声明非空才算有 schema；缺能力、或 OutputSchema() 回空（零值）
// 一律「未声明」（ok=false），消费方不得把零值当真 schema。
// c 不参与本场景断言（统一签名，_ 占位）。
func checkOutputSchemer(t *testing.T, _ Contract) error {
	t.Helper()
	declared := json.RawMessage(`{"type":"object"}`)
	cases := []struct {
		name   string
		impl   any
		wantOK bool
		want   json.RawMessage
	}{
		{"有能力取声明值", schemed{schema: declared}, true, declared},
		{"零值（nil）按未声明", schemed{schema: nil}, false, nil},
		{"零值（空）按未声明", schemed{schema: json.RawMessage{}}, false, json.RawMessage{}},
		{"缺能力按未声明", plainTool{}, false, nil},
	}
	for _, tc := range cases {
		got, ok := outputSchemaOf(tc.impl)
		if ok != tc.wantOK {
			return fmt.Errorf("%s：outputSchemaOf ok=%v，期望 %v", tc.name, ok, tc.wantOK)
		}
		if string(got) != string(tc.want) {
			return fmt.Errorf("%s：outputSchemaOf 值=%s，期望 %s", tc.name, got, tc.want)
		}
	}
	return nil
}
