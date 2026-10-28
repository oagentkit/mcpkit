// Package mcp MCP 工具池：MCP server 连接管理 → eino tool.BaseTool（对齐 eino-ext
// components/tool/mcp 的能力面，补连接生命周期与白名单池化）。
//
// 定位（对应 Argus design §9.2 方向 A）：R3/builtin agent 需要 diff 之外的上下文
// 时，把外部 MCP server 的工具挂进 agent 工具表（工单系统/内部文档/linter 等）。
//
// 用法：
//
//	pool := mcp.NewPool(
//	    mcp.ServerConfig{Name: "docs", URL: "http://docs.svc/mcp",
//	        Headers: map[string]string{"Authorization": "Bearer " + tok}},
//	    mcp.ServerConfig{Name: "lint", Command: []string{"lint-mcp", "serve"}},
//	)
//	defer pool.Close()
//	res, err := pool.Tools(ctx, []mcp.ToolSpec{{Server: "docs", Allow: []string{"search_docs"}}})
//	// res.Tools = 装配成功的工具；res.Failed = 被容忍的失败面（server → 原因）
//
// 安全模型：stdio 子进程环境走白名单透传（绝不继承密钥，经 procx.ChildEnv
// 单一纪律）；工具白名单按 spec 收敛（未声明的 server/工具不暴露给 agent）。
package mcp

import (
	"context"
	"errors"
	"fmt"
	"github.com/oekit/concurrency/async"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/oekit/concurrency/singleflight"

	"github.com/oagentkit/runtime/procx"
)

// DefaultTimeout 连接与初始化的缺省超时。
const DefaultTimeout = 30 * time.Second

// ServerConfig 单个 MCP server 连接声明。
// stdio（Command 非空）与 HTTP（URL 非空）二选一；都空视为无效配置（跳过并告警）。
type ServerConfig struct {
	// Name 逻辑名：ToolSpec.Server 引用。
	Name string
	// Command stdio 传输：可执行 + 参数（子进程由本包启动与回收）。
	Command []string
	// Env stdio 子进程环境白名单：条目为纯变量名（如 "GITHUB_TOKEN"）时按名从
	// 当前进程透传；含 "=" 时按 KEY=VALUE 字面透传。未列出的变量一律不继承
	// （基础集见 procx.ChildEnv），防止本进程密钥泄漏给外部 MCP server。
	Env []string
	// URL HTTP 传输（streamable http）。
	URL string
	// Headers HTTP 请求头（如 Bearer 鉴权）。
	Headers map[string]string
	// Timeout 连接 + Initialize 超时（默认 30s）。仅作用于建连期（dial +
	// Initialize 的 ctx 预算），不约束后续请求——HTTP 传输的每请求兜底见
	// RequestTimeout。
	Timeout time.Duration
	// RequestTimeout HTTP 传输的单请求兜底上限（默认 10 分钟）。mcp-go 的
	// WithHTTPTimeout 直接设 httpClient.Timeout、作用于该连接的每一次请求
	// （含 tools/call）——此前误用连接级 Timeout（30s）会让任何长工具调用
	// 在 HTTP 传输下永远失败（v0.10.29 审计修复）；工具调用的正常时长控制
	// 归调用方 ctx，这里只是挂死兜底。stdio 不受影响。
	RequestTimeout time.Duration
	// ProbeTimeout 探活（PingServer）独立预算（默认 5s；租约面 lease.go——
	// 探活是轻量协议 ping，不该吃连接级 30s 预算）。
	ProbeTimeout time.Duration
	// OAuth URL 模式 OAuth2 客户端配置（批次五十六 C，对标 ZCode oauth 全栈；
	// 仅 URL 传输生效）。非 nil 时 dial 走 NewOAuthStreamableHttpClient——401 触发
	// client.OAuthAuthorizationRequiredError（errors.As 可穿透池的 %w 包装），
	// 调用方经 client.GetOAuthHandler 取 handler 驱动授权码+PKCE 流程；
	// TokenStore 由调用方提供（进程内缓存失效后重 dial 仍能取回已授权 token）。
	OAuth *OAuthClientConfig
}

// OAuthClientConfig URL 传输的 OAuth2 装配输入（映射 mcp-go client.OAuthConfig；
// TokenStore 必填——缺省会落内存 store，进程内自愈场景重连后丢授权态）。
type OAuthClientConfig struct {
	ClientID     string // 空=依赖 IdP DCR（handler.RegisterClient 由调用方驱动）
	ClientSecret string
	Scopes       []string
	MetadataURL  string // 空=从 base URL .well-known 发现
	RedirectURI  string // 授权回调落点（无头服务端=固定平台端点）
	TokenStore   client.TokenStore
}

// ToolSpec 工具白名单：某 server 的哪些工具暴露给 agent。
type ToolSpec struct {
	// Server ServerConfig.Name。
	Server string
	// Allow 工具名白名单。
	//
	// ⚠️ **破坏式变更**（v0.9.0）：**空 = 不授予任何工具**（缺省拒绝）。
	// 此前空 = 该 server 的**全部**工具——而这个默认值在配置里**不可见**：
	// 一份 `allow: []` 看起来像「没限制」，实际是把该 server 的全部工具
	// （含删除/执行类破坏性工具）都授予了模型，最小权限的责任整个落到了下游
	// （下游只能靠事后门禁兜——那时配置已生效、模型已拿到工具）。
	//
	// 要「全给」必须**显式**写：AllowAll: true，或 Allow 里含 "*"。
	Allow []string
	// AllowAll 显式授予该 server 的全部工具（缺省拒绝的对立面，必须写在配置里）。
	//
	// 与 Allow 里的 "*" 等价；同时存在时以本字段为准（Allow 被忽略）。
	AllowAll bool
}

// allowAllWildcard 白名单里的通配符：显式「全给」。
const allowAllWildcard = "*"

// grantsAll 该 spec 是否显式授予全部工具。
//
// **破坏式变更**（v0.9.0）：空 Allow **不再**等于「全部」——缺省是拒绝。
// 全给必须显式：AllowAll 为真，或 Allow 里含 "*"。
func (s ToolSpec) grantsAll() bool {
	if s.AllowAll {
		return true
	}
	for _, n := range s.Allow {
		if n == allowAllWildcard {
			return true
		}
	}
	return false
}

// Pool MCP 连接池：lazy 建连（首次 Tools 时）、连接缓存复用、Close 全量回收。
// 闲置回收/探活租约面见 lease.go（ReapIdle/StartIdleReaper/PingServer）。
type Pool struct {
	// mu 护生命周期与 per-server 状态表（closed/cfgs/conns/reapStop）。
	// 单 server 的连接/目录/闲置/在途拆到 connState 自锁——Tools 跨 server
	// 并行后，原先一把大锁会把不同 server 的 cache-hit/touch 全串行。
	mu sync.Mutex
	// closed 置位后拒绝新建连接（Close 与在途建连并发时，新连接会被立即关闭，
	// 不产生泄漏）。Close 后池不可复用。
	closed bool
	cfgs   map[string]ServerConfig
	conns  map[string]*connState
	// dialGroup 同 server 并发建连收敛：N 路并发只拨一条（stdio 场景避免
	// 子进程惊群），其余复用 first 成果。
	dialGroup singleflight.Flight[client.MCPClient]
	// errMu 串行化 OnError（Tools 跨 server 并行后回调不得被并发调用）。
	errMu sync.Mutex
	// reapStop 闲置回收协程停止信号（StartIdleReaper 置位；Close 关闭）。
	reapStop chan struct{}
	// baseCtx 连接生命周期 ctx（NewPool 建、Close 收口）：stdio transport 的
	// 存活期 ctx 与 client 的双向 handler 注册共用——不得绑在拨号预算 cctx 上
	//（dialAndRegister 返回即 cancel，绑上去会让带外 server→client 请求全部
	// 撞已取消 ctx；第八轮审计）。
	baseCtx    context.Context
	baseCancel context.CancelFunc
	// OnError 建连/列举失败回调（nil 安全）。返回错误不中断其余 server——
	// 部分失败容忍：可用的工具照常返回，失败的 server 由调用方经钩子观测。
	OnError func(server string, err error)
	// RequestMeta 每笔工具调用 _meta 注入来源（nil 安全；批次五十六 B，对标 ZCode
	// request-context）。metaFn 在调用方 ctx 上取值，池不感知业务键；经 metaClient
	// 装饰器注入 req.Params.Meta（见 meta.go）。返回 nil/空 = 不注入。
	RequestMeta func(ctx context.Context) map[string]any
	// Elicitation server→client 征集输入处理器（mcp-go v1.1 SEP-2322）：工具执行中
	// server 反向请求补充输入（表单/URL 模式）时回调；nil 时该类请求得到
	// "no elicitation handler configured" 错误（调用失败呈现，与不声明能力的服务端
	// 行为一致）。池级单点——建连/自愈 redial/Initialize 能力声明共用。
	ElicitationHandler client.ElicitationHandler
}

// connState 单 server 连接侧状态（自锁：不同 server 的调用互不阻塞）。
type connState struct {
	mu       sync.Mutex
	cli      client.MCPClient
	lastUsed time.Time // 建连/取工具/探活成功/调用结束
	inFlight int       // 在途工具调用（ReapIdle 跳过）
	catalog  []toolEntry
	modern   bool
	// protoVersion 协商协议版本（Initialize 后可知；raw 目录通道的现代协议
	// _meta/头双写依据。v0.12.5）。
	protoVersion string
	// tr transport 句柄（dial 登记一次后只读——目录 raw 通道用；测试注入的 cli
	// 无此句柄=nil，listTools 自动回落类型化通道）。
	tr transport.Interface
}

// NewPool 创建连接池（建连延后到 Tools 调用时——允许启动期 MCP server 未就绪）。
// Name 为空的配置忽略；重名保留先到者（后者忽略）——配置错误尽早收敛为确定性
// 行为而非静默覆盖。
func NewPool(cfgs ...ServerConfig) *Pool {
	baseCtx, cancel := context.WithCancel(context.Background())
	p := &Pool{cfgs: map[string]ServerConfig{}, conns: map[string]*connState{},
		baseCtx: baseCtx, baseCancel: cancel}
	for _, c := range cfgs {
		if c.Name == "" {
			continue
		}
		if _, dup := p.cfgs[c.Name]; !dup {
			p.cfgs[c.Name] = c
		}
	}
	return p
}

// connLocked 取/建 server 状态（调用方须持 p.mu）。
func (p *Pool) connLocked(name string) *connState {
	c := p.conns[name]
	if c == nil {
		c = &connState{}
		p.conns[name] = c
	}
	return c
}

// conn 取 server 状态（不建；无则 nil）。
func (p *Pool) conn(name string) *connState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conns[name]
}

// connLockedOrNil 取/建 server 状态；池已关闭返回 nil。
func (p *Pool) connLockedOrNil(name string) *connState {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	return p.connLocked(name)
}

// Servers 返回全部配置的 server 名（字母序，可观测用）。
func (p *Pool) Servers() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	names := make([]string, 0, len(p.cfgs))
	for n := range p.cfgs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ToolsResult Tools 的结果面（v0.10.38 破坏式：此前返回 []BaseTool，部分失败只
// 经 OnError 侧面观测——调用方在返回值层面无法区分「失败被容忍」与「真的没
// 工具」，只能把失败当空表静默降级）。
type ToolsResult struct {
	// Tools 装配成功的工具（跨 server 并行拉取，按 specs 序稳定）。
	Tools []tool.BaseTool
	// Failed server → 被容忍的失败原因（建连/列举失败、白名单无一命中）。
	// 失败连接已摘除，下次调用透明重建。空 = 全部成功。OnError 仍发射同源
	// 通知（遥测钩子），失败面的程序化消费一律走这里。
	Failed map[string]error
}

// Tools 按 spec 白名单返回 eino 工具与失败面（跨 server 并行拉取）。
// spec.Server 未配置 → 报错（配置错误应显式暴露）；server 建连/列举失败 →
// 容忍：记入 Failed（同时经 OnError 发射），可用工具照常返回——列举失败视为
// 该连接失效，已从缓存摘除关闭（server 崩溃后下次调用自动重建）。
// OAuth 授权必需错误 fail-fast 直通（错误返回，不进 Failed）。
func (p *Pool) Tools(ctx context.Context, specs []ToolSpec) (ToolsResult, error) {
	// 先全量校验：配置错误是调用方 bug，不应半路并行半路报错。
	// cfgs 读持 p.mu（与其余读路径同纪律——当前生产无写者，但任何未来的
	// 配置更新 API 都不应把这里变成 data race）
	p.mu.Lock()
	var missing string
	cfgs := make([]ServerConfig, len(specs))
	for i, spec := range specs {
		cfg, ok := p.cfgs[spec.Server]
		if !ok {
			missing = spec.Server
			break
		}
		cfgs[i] = cfg
	}
	p.mu.Unlock()
	if missing != "" {
		return ToolsResult{}, fmt.Errorf("mcp: ToolSpec 引用未配置的 server %q（已配置: %v）",
			missing, p.Servers())
	}
	// stdio/http 二选一前置校验（第九轮挂账落地）：此前要到 dial 期才暴露，
	// 经 OnError 容忍后难以排查；配置错误在这里一次性显式报出
	for i, spec := range specs {
		if cfgs[i].Name == "" {
			continue
		}
		if len(cfgs[i].Command) == 0 && cfgs[i].URL == "" {
			return ToolsResult{}, fmt.Errorf("mcp: server %q stdio（command）与 http（url）均未配置", spec.Server)
		}
		if len(cfgs[i].Command) > 0 && cfgs[i].URL != "" {
			return ToolsResult{}, fmt.Errorf("mcp: server %q 同时配置了 command 与 url（二选一）", spec.Server)
		}
	}

	results := make([][]tool.BaseTool, len(specs))
	var (
		mu       sync.Mutex
		oauthErr error
		ctxFatal error // 调用方取消：本地取消不得记成 server 失败
		failed   = map[string]error{}
	)
	var wg sync.WaitGroup
	for i := range specs {
		wg.Add(1)
		// async.GoSafe：fanout 体跑 dial/Initialize/SDK 解析——外部 SDK panic
		// 不得打死进程（ekit 裸 go 收编纪律）
		async.GoSafe(func() {
			defer wg.Done()
			i := i
			tools, err := p.toolsForSpec(ctx, cfgs[i], specs[i])
			if err != nil {
				if client.IsOAuthAuthorizationRequiredError(err) {
					mu.Lock()
					if oauthErr == nil {
						oauthErr = err
					}
					mu.Unlock()
					return
				}
				if cerr := ctx.Err(); cerr != nil {
					mu.Lock()
					if ctxFatal == nil {
						ctxFatal = cerr
					}
					mu.Unlock()
					return
				}
				mu.Lock()
				failed[cfgs[i].Name] = err
				mu.Unlock()
				return // 失败已在 toolsForSpec 内经 OnError 发射、连接已摘除
			}
			results[i] = tools
		})
	}
	wg.Wait()
	if oauthErr != nil {
		return ToolsResult{}, oauthErr
	}
	var out []tool.BaseTool
	for _, t := range results {
		out = append(out, t...)
	}
	if len(out) == 0 && ctxFatal != nil {
		// 缓存冷 + ctx 取消：调用方拿到 (空表, nil) 会把取消当"server 没有
		// 工具"静默降级；缓存热时同样的已取消 ctx 走快路径正常返回，两种
		// 形态行为须一致（第六轮审计）
		return ToolsResult{}, ctxFatal
	}
	if len(failed) == 0 {
		failed = nil
	}
	return ToolsResult{Tools: out, Failed: failed}, nil
}

// toolsForSpec 单 spec 的建连→列目录→装配（Tools 的并发单元）。
// 返回 error：OAuth 授权态（调用方 fail-fast）或被容忍的失败（调用方记入
// ToolsResult.Failed；OnError 已发射同源通知）。
func (p *Pool) toolsForSpec(ctx context.Context, cfg ServerConfig, spec ToolSpec) ([]tool.BaseTool, error) {
	cli, err := p.client(ctx, cfg)
	if err != nil {
		if client.IsOAuthAuthorizationRequiredError(err) {
			return nil, err
		}
		p.notifyError(cfg.Name, err)
		return nil, err
	}
	entries, err := p.catalogFor(ctx, cfg, cli)
	if err != nil {
		if client.IsOAuthAuthorizationRequiredError(err) {
			return nil, err
		}
		p.notifyError(cfg.Name, err)
		return nil, err
	}
	tools := p.assembleTools(cfg, spec, cli, entries)
	// Allow 白名单全部未命中（拼写错误等）：转换层静默返回空表——经 OnError
	// 告警并记入 Failed，避免 agent 拿到空工具表却无从排查
	// （显式「全给」时不算：一台没有工具的 server 就是 0 个，不是白名单打错）
	if len(tools) == 0 && len(spec.Allow) > 0 && !spec.grantsAll() {
		err := fmt.Errorf("mcp: %s 白名单 %v 无一命中（返回 0 个工具）", cfg.Name, spec.Allow)
		p.notifyError(cfg.Name, err)
		return nil, err
	}
	return p.wrapSelfHeal(ctx, cfg, spec, cli, tools), nil
}

// assembleTools 目录条目 → eino 工具 + 在途登记（leaseUse 注入）单源：初始装配
// （toolsForSpec）与自愈 redial（selfHealRedial）共用。此前 redial 路径裸
// convTools——lease 注入漏迁移，自愈重试期间 inFlight==0，重试时长超过
// maxIdle 时连接可被闲置回收（第六轮审计）。leaseUse 是唯一带 lease 的装配点，
// 双路径手工对齐的漂移面就此消除。
func (p *Pool) assembleTools(cfg ServerConfig, spec ToolSpec, cli client.MCPClient, entries []toolEntry) []tool.BaseTool {
	tools := convTools(cli, entries, spec.Allow, spec.grantsAll(), cfg.Name)
	lease := p.leaseUse(cfg.Name)
	for _, t := range tools {
		if it, ok := t.(*invokableTool); ok {
			it.lease = lease
		}
	}
	return tools
}

// wrapSelfHeal 调用期自愈（批次五十）：自愈层在最外（传输层死亡重连重试），
// 业务错误（isError:true）仍由 errorAsObservation 层转观察回喂 LLM。
func (p *Pool) wrapSelfHeal(ctx context.Context, cfg ServerConfig, spec ToolSpec, cli client.MCPClient, tools []tool.BaseTool) []tool.BaseTool {
	var out []tool.BaseTool
	for _, w := range WrapErrorAsObservation(tools) {
		if it, ok := w.(tool.InvokableTool); ok {
			info, ierr := it.Info(ctx)
			if ierr != nil {
				out = append(out, w)
				continue
			}
			out = append(out, &selfHealTool{
				name:   info.Name,
				inner:  it,
				redial: p.selfHealRedial(cfg, spec, info.Name, cli),
			})
			continue
		}
		out = append(out, w)
	}
	return out
}

// notifyError 串行化 OnError（Tools 跨 server 并行后回调不得被并发调用）。
func (p *Pool) notifyError(server string, err error) {
	if p.OnError == nil {
		return
	}
	p.errMu.Lock()
	defer p.errMu.Unlock()
	p.OnError(server, err)
}

// client 获取（lazy 建连 + 缓存）。失败时不缓存——下次调用重试。
func (p *Pool) client(ctx context.Context, cfg ServerConfig) (client.MCPClient, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errors.New("mcp: 池已关闭")
	}
	st := p.connLocked(cfg.Name)
	p.mu.Unlock()

	st.mu.Lock()
	if st.cli != nil {
		st.lastUsed = time.Now()
		cli := st.cli
		st.mu.Unlock()
		return cli, nil
	}
	st.mu.Unlock()

	// 同 server 并发建连收敛到一条：stdio 场景原先 N 路并发各起一个子进程，
	// 双检后 N-1 条立刻关掉——惊群 + 进程抖动。singleflight 内完成 dial+Initialize+登记。
	// DoChan 内部对 leader 做 WithoutCancel：连接是池共享资源，不应被先到调用方的
	// 取消拖死；各等待方按自己的 ctx 退出（oekit concurrency v0.6.0，x/sync 退役）。
	ch := p.dialGroup.DoChan(ctx, cfg.Name, func(dialCtx context.Context) (client.MCPClient, error) {
		return p.dialAndRegister(dialCtx, cfg)
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		if res.Val == nil {
			return nil, fmt.Errorf("mcp: %s 建连结果为空", cfg.Name)
		}
		return res.Val, nil
	}
}

// dialAndRegister 建连 + Initialize + 入池（仅经 singleflight 调用）。
// 锁序纪律：p.mu 与 st.mu 互不嵌套——取状态/判 closed 在 p.mu 内完成即释放，
// 再动 st；写完连接后二次核对 closed，防止与 Close 交错泄漏。
func (p *Pool) dialAndRegister(ctx context.Context, cfg ServerConfig) (client.MCPClient, error) {
	st, err := p.connForDial(cfg.Name)
	if err != nil {
		return nil, err
	}
	if cli := st.cachedClient(); cli != nil {
		return cli, nil
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 连接生命周期（transport 存活期 ctx + 双向 handler 注册）用池级 baseCtx；
	// cctx 只作 Initialize 的拨号预算（dialAndRegister 返回即 cancel）。
	// cli.Start 注册 server→client 请求/通知 handler（elicitation 等带外通道；
	// 不调 Start 则 handler 恒 nil，带外请求一律 METHOD_NOT_FOUND）——对已
	// Start 的 transport 幂等，仅补挂 handler（第八轮审计）。
	cli, tr, err := p.dial(p.baseCtx, cfg)
	if err != nil {
		return nil, fmt.Errorf("mcp: %s 建连失败: %w", cfg.Name, err)
	}
	if starter, ok := cli.(interface{ Start(context.Context) error }); ok {
		if serr := starter.Start(p.baseCtx); serr != nil {
			_ = cli.Close()
			return nil, fmt.Errorf("mcp: %s client 启动失败: %w", cfg.Name, serr)
		}
	}
	cli, protoVersion, err := initializeClient(cctx, cli, p.ElicitationHandler)
	if err != nil {
		return nil, fmt.Errorf("mcp: %s %w", cfg.Name, err)
	}

	if p.RequestMeta != nil {
		// _meta 请求上下文注入（meta.go）：装饰器在 client() 单点包装——初次建连
		// 与自愈 redial 同走此路，evict 指针比对基于装饰器一致。
		cli = &metaClient{MCPClient: cli, metaFn: p.RequestMeta}
	}
	return p.registerConn(cfg.Name, st, cli, tr, protoVersion)
}

// connForDial 取/建 server 状态（建连前）；池已关闭报错。
func (p *Pool) connForDial(name string) (*connState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errors.New("mcp: 池已关闭")
	}
	return p.connLocked(name), nil
}

// cachedClient 双检缓存命中（singleflight 复用方可能已等到别人建好连接）。
func (st *connState) cachedClient() client.MCPClient {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.cli == nil {
		return nil
	}
	st.lastUsed = time.Now()
	return st.cli
}

// initializeClient MCP 协议要求先 Initialize 才可调用；返回代际标记
// （Initialize 后才可知，现代协议 Ping 是 no-op，探活路径须分流）。
// 失败时关闭连接——调用方拿到 error 即连接已回收。
// 返回协商协议版本（raw 目录通道的现代协议判据；断言失败（测试桩）=空串=旧协议路径）。
func initializeClient(ctx context.Context, cli client.MCPClient, elicitation client.ElicitationHandler) (client.MCPClient, string, error) {
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "agentkit", Version: "1.0"}
	if elicitation != nil {
		// 能力声明与 handler 同装（不声明则支持 elicitation 的 server 不会发起）
		initReq.Params.Capabilities.Elicitation = &mcp.ElicitationCapability{}
	}
	if _, err := cli.Initialize(ctx, initReq); err != nil {
		_ = cli.Close()
		return nil, "", fmt.Errorf("mcp: initialize 握手失败: %w", err)
	}
	protoVersion := ""
	// 包装前 concrete 类型仍可断言（lease.go PingServer 分流依据）
	if cc, ok := cli.(*client.Client); ok {
		protoVersion = cc.ProtocolVersion()
	}
	return cli, protoVersion, nil
}

// registerConn 连接入池：双检占位 + 与 Close 交错的两次 closed 核对
// （写入后池已关则摘掉回收，防泄漏）。已存在连接时关掉传入的这条。
func (p *Pool) registerConn(name string, st *connState, cli client.MCPClient, tr transport.Interface, protoVersion string) (client.MCPClient, error) {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		_ = cli.Close()
		return nil, errors.New("mcp: 池已关闭")
	}

	st.mu.Lock()
	if st.cli != nil {
		existing := st.cli
		st.mu.Unlock()
		_ = cli.Close()
		return existing, nil
	}
	st.cli = cli
	st.tr = tr
	st.lastUsed = time.Now()
	st.modern = mcp.IsModernProtocol(protoVersion)
	st.protoVersion = protoVersion
	st.mu.Unlock()

	// 与 Close 交错兜底：写入后若池已关，摘掉并回收，防泄漏
	p.mu.Lock()
	closed = p.closed
	p.mu.Unlock()
	if closed {
		p.evict(name, cli)
		return nil, errors.New("mcp: 池已关闭")
	}
	return cli, nil
}

// evict 连接失效时从缓存摘除并关闭（仅当缓存中的仍是该连接），下次调用走重建；
// 同 server 目录缓存一并失效（重建连接后重新列举）。
func (p *Pool) evict(name string, cli client.MCPClient) {
	st := p.conn(name)
	if st == nil {
		_ = cli.Close()
		return
	}
	st.mu.Lock()
	if st.cli == cli {
		dead := st.detachLocked()
		st.mu.Unlock()
		_ = dead.Close()
		return
	}
	st.mu.Unlock()
	// 缓存里已是别的连接（并发自愈刚换上）：只关传入的这条，不动新连接
	_ = cli.Close()
}

// detachLocked 摘除连接并连带失效目录/代际（evict/ReapIdle/Close 三处同构的
// 失效逻辑单源；调用方须持 st.mu）。返回被摘下的连接供锁外 Close。
func (st *connState) detachLocked() client.MCPClient {
	cli := st.cli
	st.cli = nil
	st.catalog = nil
	st.modern = false
	return cli
}

// dial 按配置构造 MCP client（stdio 或 streamable http）。
// transport 直构 + client.NewClient 装配（mcp-go v1.1 起 client 级选项——elicitation
// 等——只能在 NewClient 时挂，便捷构造器不接收）；stdio 便捷构造器自动 Start 的行为
// 在此显式化（直构不 Start 子进程不会拉起）。
func (p *Pool) dial(ctx context.Context, cfg ServerConfig) (client.MCPClient, transport.Interface, error) {
	clientOpts := func() []client.ClientOption {
		if p.ElicitationHandler == nil {
			return nil
		}
		return []client.ClientOption{client.WithElicitationHandler(p.ElicitationHandler)}
	}
	switch {
	case len(cfg.Command) > 0:
		return dialStdio(ctx, cfg, clientOpts()...)
	case cfg.URL != "":
		return p.dialHTTP(ctx, cfg, clientOpts()...)
	default:
		return nil, nil, fmt.Errorf("mcp: server %s stdio（command）与 http（url）均未配置", cfg.Name)
	}
}

// dialStdio stdio 传输建连：argv[0] 只允许可执行名（防 argument injection），
// 环境只给白名单（procx.ChildEnv，绝不继承全量 os.Environ()）。
func dialStdio(ctx context.Context, cfg ServerConfig, clientOpts ...client.ClientOption) (client.MCPClient, transport.Interface, error) {
	if cfg.Command[0] == "" || strings.HasPrefix(cfg.Command[0], "-") {
		return nil, nil, fmt.Errorf("mcp: server %s 非法 command %q", cfg.Name, cfg.Command[0])
	}
	// 经 CommandFunc 接管 exec.Cmd：环境白名单纪律单一事实源。
	// 子进程生命周期必须与 dial ctx 解绑（WithoutCancel）：dial ctx 是连接级短预算
	// （Timeout，默认 30s），client() 返回即 cancel——绑上去会让 stdio server 在
	// 建连成功瞬间被杀，下次 tools/list 报 transport closed（v0.10.33 修复）。
	// 子进程由连接关闭路径收口（evict/闲置回收/Pool.Close → cli.Close）。
	t := transport.NewStdioWithOptions(cfg.Command[0], cfg.Env, cfg.Command[1:],
		transport.WithCommandFunc(func(ctx context.Context, command string, env []string, args []string) (*exec.Cmd, error) {
			cmd := exec.CommandContext(context.WithoutCancel(ctx), command, args...)
			cmd.Env = procx.ChildEnv(env)
			return cmd, nil
		}))
	if err := t.Start(ctx); err != nil {
		return nil, nil, fmt.Errorf("mcp: server %s stdio 启动失败: %w", cfg.Name, err)
	}
	return client.NewClient(t, clientOpts...), t, nil
}

// dialHTTP streamable http 传输建连（可挂 OAuth）。
func (p *Pool) dialHTTP(ctx context.Context, cfg ServerConfig, clientOpts ...client.ClientOption) (client.MCPClient, transport.Interface, error) {
	cfg = p.expandServerEnv(cfg) // ${VAR} dial 期展开（见 expandServerEnv）
	httpOpts := []transport.StreamableHTTPCOption{
		transport.WithHTTPHeaders(cfg.Headers),
		// 每请求兜底（含 tools/call）：与建连预算 Timeout 分离——误用连接级
		// 30s 会让长工具调用在 HTTP 传输下永远失败
		transport.WithHTTPTimeout(cfg.requestTimeoutOr(DefaultRequestTimeout)),
	}
	if cfg.OAuth != nil {
		if cfg.OAuth.TokenStore == nil {
			return nil, nil, fmt.Errorf("mcp: server %s OAuth 配置缺 TokenStore（缺省内存 store 会在重连后丢授权态）", cfg.Name)
		}
		// OAuth 客户端（批次五十六 C）：静态 headers 保留（可与 Bearer 共存——
		// 代理网关类附加头），token 注入由 transport 内 oauthHandler 承担（PKCE 恒开）。
		httpOpts = append(httpOpts, transport.WithHTTPOAuth(client.OAuthConfig{
			ClientID:              cfg.OAuth.ClientID,
			ClientSecret:          cfg.OAuth.ClientSecret,
			RedirectURI:           cfg.OAuth.RedirectURI,
			Scopes:                cfg.OAuth.Scopes,
			TokenStore:            cfg.OAuth.TokenStore,
			AuthServerMetadataURL: cfg.OAuth.MetadataURL,
			PKCEEnabled:           true,
		}))
	}
	tt, err := transport.NewStreamableHTTP(cfg.URL, httpOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf("mcp: server %s http transport 构造失败: %w", cfg.Name, err)
	}
	return client.NewClient(tt, clientOpts...), tt, nil
}

// firstPositive 非零胜出（Timeout/RequestTimeout/ProbeTimeout 三处回退单源）。
func firstPositive(vals ...time.Duration) time.Duration {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}

// timeoutOr cfg.Timeout 的非零回退。
func (c ServerConfig) timeoutOr(d time.Duration) time.Duration {
	return firstPositive(c.Timeout, d)
}

// DefaultRequestTimeout HTTP 传输单请求兜底缺省。
const DefaultRequestTimeout = 10 * time.Minute

func (c ServerConfig) requestTimeoutOr(d time.Duration) time.Duration {
	return firstPositive(c.RequestTimeout, d)
}

// Close 关闭全部缓存连接并拒绝后续建连（幂等）；停止闲置回收协程。Close 后池不可复用。
// 锁序：先在 p.mu 内标记 closed 并摘出各 connState 指针，释放后再逐个收连接——
// 绝不在持 p.mu 时抢 st.mu（与 dialAndRegister 的写入路径交叉会死锁）。
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	states := make([]*connState, 0, len(p.conns))
	for _, st := range p.conns {
		states = append(states, st)
	}
	stop := p.reapStop
	p.reapStop = nil
	cancel := p.baseCancel
	p.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	if cancel != nil {
		cancel() // 连接生命周期 ctx 收口（带外请求通道随连接关闭一并终结）
	}
	for _, st := range states {
		st.mu.Lock()
		cli := st.detachLocked()
		st.mu.Unlock()
		if cli != nil {
			_ = cli.Close()
		}
	}
}

// expandServerEnv dial 期展开 ${VAR}（第十轮落地）：Headers 全量 + OAuth
// 凭据类字符串。「token 不落配置文件」纪律此前纯靠调用方自觉先展开（零接线），
// 现在焊在池边界。展开结果只进本次 dial 副本、绝不回写 p.cfgs——redial 从
// 原始声明重新展开（环境轮转后新值生效；展开产物不含 ${ 标记，幂等）。
// 缺失变量保留字面量（ExpandEnvRef 既有语义）并经 OnError 聚合上报。
func (p *Pool) expandServerEnv(cfg ServerConfig) ServerConfig {
	missing := map[string]struct{}{}
	collect := func(name string) { missing[name] = struct{}{} }
	cfg.Headers = ExpandEnvRefs(cfg.Headers, collect)
	if cfg.OAuth != nil {
		o := *cfg.OAuth
		o.ClientID = ExpandEnvRef(o.ClientID, collect)
		o.ClientSecret = ExpandEnvRef(o.ClientSecret, collect)
		o.MetadataURL = ExpandEnvRef(o.MetadataURL, collect)
		o.RedirectURI = ExpandEnvRef(o.RedirectURI, collect)
		cfg.OAuth = &o
	}
	if len(missing) > 0 {
		p.notifyError(cfg.Name, fmt.Errorf("mcp: server %s 环境引用未定义（字面量保留上线）: %v",
			cfg.Name, sortedEnvNames(missing)))
	}
	return cfg
}
