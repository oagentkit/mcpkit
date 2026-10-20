package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// SelfcheckOptions 自检参数。
type SelfcheckOptions struct {
	// Server server 名（结果标识 + 缓存键）。
	Server string
	// Timeout 单次探测的超时预算（默认 10s）。
	Timeout time.Duration
	// TTL 目录缓存有效期（默认 30s）。<= 0 表示不缓存（每次都实拉）。
	//
	// 为什么需要缓存：自检是「高频拉目录」的动作，而 MCP 侧的工具目录
	// 在连接生命周期内视为静态（见 catalog.go）。没有 TTL 时，每次探活
	// 都会打一轮 tools/list —— 部署机的巡检会把 server 压垮。
	TTL time.Duration
	// ProbeTools 是否逐工具做一次**调用**探测（默认 false）。
	//
	// true = 每个工具都真调一次（最彻底，但有副作用：会真的执行工具）。
	// 默认 false = 只探连通性 + 拉目录（无副作用）。
	// 工具「存在但调不通」这种情形，用 tools/call 探针单点确认（见 ProbeTool）。
	ProbeTools bool
	// ProbeArgs 逐工具探测时的参数（nil = 用空对象）。
	ProbeArgs map[string]any
	// Initialize 是否在自检前先发 initialize 握手。
	//
	// 默认 false：调用方通常传**已建连**的 client（Pool 的 client 都已
	// 握手过）。但若传的是裸 client（未 initialize），协议请求会返回
	// "client not initialized"——那时自检会误判成 server 不可达。
	// 自检是「诊断工具」，不该因为握手缺失而给出错误结论，
	// 所以提供这个开关。
	Initialize bool
	// Expected 声明期望的工具面（可选）。
	//
	// 给了它，自检会逐个核：这���工具在不在 server 目录里、在但调不通。
	// 这是「装包≠开洞」这类治理规则的输入——只有把声明面与实际面对照，
	// 才能发现「我以为有的工具其实没有」。
	Expected Catalog
}

// DefaultSelfcheckTimeout 自检默认超时。
const DefaultSelfcheckTimeout = 10 * time.Second

// DefaultSelfcheckTTL 自检默认目录缓存 TTL。
const DefaultSelfcheckTTL = 30 * time.Second

// ToolStatus 单个工具的自检状态。
type ToolStatus string

const (
	// ToolOK 工具在目录里且连通。
	ToolOK ToolStatus = "ok"
	// ToolMissing 工具在声明面里但 server 目录里没有（契约/配置问题）。
	ToolMissing ToolStatus = "missing"
	// ToolUnreachable server 连不上，工具状态未知（**不是** missing）。
	ToolUnreachable ToolStatus = "unreachable"
	// ToolFailed 工具存在但调用失败。
	ToolFailed ToolStatus = "failed"
)

// ToolCheck 单个工具的自检结果。
type ToolCheck struct {
	// Tool 工具名。
	Tool string
	// Status 状态。
	Status ToolStatus
	// Kind 失败时的错误分类（Status==ToolOK 时为 KindUnknown）。
	Kind ErrorKind
	// Reason 人类可读的失败原因（Status==ToolOK 时为空）。
	Reason string
	// Err 原始错误（供 errors.Is/As 穿透）。
	Err error
}

// SelfcheckResult 单个 server 的自检结果。
type SelfcheckResult struct {
	// Server server 名。
	Server string
	// Reachable 连通性（server 能否应答协议请求）。
	Reachable bool
	// Tools 逐工具结果（按工具名排序）。
	Tools []ToolCheck
	// Catalog server 实际提供的工具目录（连通时有效）。
	Catalog Catalog
	// Expected 声明期望的工具面（可选，供差集比对）。
	Expected Catalog
	// Diff 期望 vs 实际的差集（Expected 非空时有效）。
	Diff CatalogDiff
	// PingErr ping 探测的错误（**不代表不可达**——server 可能没实现 ping；
	// 真正的连通性判据是列举成功）。留在这里供诊断。
	PingErr error
	// Kind 整体失败的错误分类（Reachable==false 时有效）。
	Kind ErrorKind
	// Err 整体失败的原始错误。
	Err error
	// Cached 本次结果是否来自 TTL 缓存。
	Cached bool
	// At 探测时刻。
	At time.Time
}

// Failed 失败的工具条目（Status != ToolOK）。
func (r SelfcheckResult) Failed() []ToolCheck {
	var out []ToolCheck
	for _, t := range r.Tools {
		if t.Status != ToolOK {
			out = append(out, t)
		}
	}
	return out
}

// Selfchecker 带 TTL 缓存的自检器。
//
// 用法（部署机上的常驻自检）：
//
//	sc := NewSelfchecker()
//	res := sc.Check(ctx, client, SelfcheckOptions{Server: "fs", Expected: exp})
//
// 零值不可用——用 NewSelfchecker 构造。
type Selfchecker struct {
	mu    sync.Mutex
	cache map[string]selfcheckEntry
	// now 可注入（测试用；nil = time.Now）。
	now func() time.Time
}

type selfcheckEntry struct {
	result SelfcheckResult
	expire time.Time
}

// NewSelfchecker 构造自检器。
func NewSelfchecker() *Selfchecker {
	return &Selfchecker{cache: map[string]selfcheckEntry{}}
}

// Check 对单个 server 做自检：拉工具目录 + 探连通性 + 逐工具核状态。
//
// client 非 nil 时用它拉目录与探活；nil 时只做「声明面 vs 已有目录」的核，
// 不发任何请求（离线比对模式）。
func (s *Selfchecker) Check(ctx context.Context, cli client.MCPClient, opts SelfcheckOptions) SelfcheckResult {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultSelfcheckTimeout
	}
	if opts.TTL == 0 {
		opts.TTL = DefaultSelfcheckTTL
	}
	now := s.now
	if now == nil {
		now = time.Now
	}

	// 缓存命中：直接返回（连通性结论也一并复用——TTL 内的目录视为静态）。
	if opts.TTL > 0 {
		s.mu.Lock()
		e, ok := s.cache[opts.Server]
		s.mu.Unlock()
		if ok && now().Before(e.expire) {
			r := e.result
			r.Cached = true
			return r
		}
	}

	res := s.probe(ctx, cli, opts)
	res.At = now()
	if opts.TTL > 0 {
		s.mu.Lock()
		if s.cache == nil {
			s.cache = map[string]selfcheckEntry{}
		}
		s.cache[opts.Server] = selfcheckEntry{result: res, expire: res.At.Add(opts.TTL)}
		s.mu.Unlock()
	}
	return res
}

// probe 实拉目录 + 探活 + 逐工具核状态。
func (s *Selfchecker) probe(ctx context.Context, cli client.MCPClient, opts SelfcheckOptions) SelfcheckResult {
	res := SelfcheckResult{Server: opts.Server, At: time.Now()}
	res.Expected = opts.Expected

	if cli == nil {
		// 离线模式：无 client 可用，只做声明面核（期望工具全部标 missing，
		// 因为无从知道 server 实际有什么）。
		res.Reachable = false
		res.Kind = KindUnreachable
		res.Err = fmt.Errorf("mcp: 自检未提供 client（离线模式）")
		res.Tools = markAllMissing(opts.Expected.Tools, res.Err)
		res.Diff = Diff(opts.Expected, res.Catalog)
		return res
	}

	cctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	// ⓪ 可选的 initialize 握手（调用方传裸 client 时用）。
	if opts.Initialize {
		initReq := mcp.InitializeRequest{}
		initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
		initReq.Params.ClientInfo = mcp.Implementation{Name: "mcp-selfcheck", Version: "0.1.0"}
		if _, err := cli.Initialize(cctx, initReq); err != nil {
			res.Reachable = false
			res.Kind = ClassifyError(err)
			res.Err = err
			res.Tools = markAllUnknown(opts.Expected.Tools, err)
			res.Diff = Diff(opts.Expected, res.Catalog)
			return res
		}
	}

	// ① 连通性探测：**先 ping，ping 失败回退到列举**。
	//
	// 为什么不能「ping 失败 = 连不上」：ping 并非所有 server 都支持
	// （实测 in-process client 未 Initialize 时 ping 直接报
	// "client not initialized"，而它其实活得好好的）。
	// 只认 ping 会把「server 活着但没实现/没初始化 ping」误判成 down。
	//
	// 判定口径：**列举成功 = 连通**（列举是 MCP 的必备能力）。
	// 两者都失败 ⇒ 不可达。
	pingErr := cli.Ping(cctx)
	res.PingErr = pingErr

	// ② 拉工具目录（同时充当连通性的权威判据）。
	listRes, listErr := cli.ListTools(cctx, mcp.ListToolsRequest{})
	if listErr != nil {
		res.Reachable = false
		res.Kind = ClassifyError(listErr)
		res.Err = listErr
		// 工具状态：server 不可达 ⇒ 全部「未知」，**不能**标 missing。
		// 理由：missing 是「server 说没有这个工具」，不可达是「server 没应答」，
		// 两者语义不同——把不可达算成 missing 会让「依赖故障被放大成
		// 契约故障」（下游踩过的坑）。
		res.Tools = markAllUnknown(opts.Expected.Tools, listErr)
		res.Diff = Diff(opts.Expected, res.Catalog)
		return res
	}
	res.Reachable = true
	names := make([]string, 0, len(listRes.Tools))
	for _, t := range listRes.Tools {
		names = append(names, t.Name)
	}
	res.Catalog = NewCatalog(opts.Server, names)
	res.Expected = opts.Expected

	// ③ 逐工具核状态。
	have := make(map[string]struct{}, len(names))
	for _, n := range names {
		have[n] = struct{}{}
	}
	// 声明面里的工具：目录没有 → missing。
	for _, want := range opts.Expected.Tools {
		if _, ok := have[want]; !ok {
			res.Tools = append(res.Tools, ToolCheck{
				Tool:   want,
				Status: ToolMissing,
				Kind:   KindToolMissing,
				Reason: fmt.Sprintf("声明要用的工具 %q 不在 server 目录里", want),
			})
		}
	}
	// 目录里的工具：默认 ok（连通即目录可见）；ProbeTools 时逐个实调。
	for _, n := range names {
		if opts.ProbeTools {
			if err := probeTool(cctx, cli, n, opts.ProbeArgs); err != nil {
				res.Tools = append(res.Tools, ToolCheck{
					Tool:   n,
					Status: ToolFailed,
					Kind:   ClassifyError(err),
					Reason: err.Error(),
					Err:    err,
				})
				continue
			}
		}
		res.Tools = append(res.Tools, ToolCheck{Tool: n, Status: ToolOK})
	}
	sort.Slice(res.Tools, func(i, j int) bool { return res.Tools[i].Tool < res.Tools[j].Tool })
	res.Diff = Diff(opts.Expected, res.Catalog)
	return res
}

// probeTool 单工具调用探针：确认「工具在但调不通」这类情形。
func probeTool(ctx context.Context, cli client.MCPClient, name string, args map[string]any) error {
	raw := []byte("{}")
	if len(args) > 0 {
		b, err := json.Marshal(args)
		if err != nil {
			return err
		}
		raw = b
	}
	res, err := cli.CallTool(ctx, mcp.CallToolRequest{
		Request: mcp.Request{Method: "tools/call"},
		Params:  mcp.CallToolParams{Name: name, Arguments: raw},
	})
	if err != nil {
		return err
	}
	if res != nil && res.IsError {
		payload := ""
		if len(res.Content) > 0 {
			if tc, ok := res.Content[0].(mcp.TextContent); ok {
				payload = tc.Text
			}
		}
		return &ToolResultError{payload: []byte(payload)}
	}
	return nil
}

// markAllMissing 全部标 missing（离线模式：无从知道 server 实际有什么）。
func markAllMissing(names []string, err error) []ToolCheck {
	out := make([]ToolCheck, 0, len(names))
	for _, n := range names {
		out = append(out, ToolCheck{
			Tool:   n,
			Status: ToolMissing,
			Kind:   KindToolMissing,
			Reason: "未连接 server，无法确认工具是否存在",
			Err:    err,
		})
	}
	return out
}

// markAllUnknown 全部标「状态未知」（server 不可达 ≠ 工具缺失）。
func markAllUnknown(names []string, err error) []ToolCheck {
	out := make([]ToolCheck, 0, len(names))
	for _, n := range names {
		out = append(out, ToolCheck{
			Tool:   n,
			Status: ToolUnreachable,
			Kind:   KindUnreachable,
			Reason: "server 不可达，工具状态未知（不等于缺失）",
			Err:    err,
		})
	}
	return out
}
