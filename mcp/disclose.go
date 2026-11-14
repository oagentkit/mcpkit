// 渐进披露的模型面门禁契约（oagentkit/mcpkit#64）。
//
// 分工写死：mcpkit.Catalog 是 **server 面**（server 实际提供哪些工具——哪些已
// 加载是装配层的事，目录不感知）；本文件是 **模型面**（LLM 发起调用时，这个
// 名字「现在能不能给」）。「这个工具存在但我现在不能给你」与「这个工具不
// 存在」需要**不同的失败语义**——前者激活后可重试，后者该换工具。此前这套
// 三态门禁 + 模型面文案只活在各宿主手搓的 Gate 里（拼法散落必然漂移），升格
// 为上游契约：机器可辨的三态 + 库内置的模型面文案（宿主可覆写），处置层
// （guardx/toolfail 等）按 State 做确定性分流，不再解析文案。
package mcp

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// DisclosureState 门禁三态（机器可辨；处置层按它分流，勿解析文案）。
type DisclosureState string

const (
	// DisclosureAllowed 已披露：放行，调用照常下发。
	DisclosureAllowed DisclosureState = "allowed"
	// DisclosureRetryableDenied 目录内未披露：拒绝但**可重试**——工具在
	// server 目录里，只是尚未披露给本次会话；激活（披露）后重试必成。
	// 模型应原样重试同一调用，不要改换工具。
	DisclosureRetryableDenied DisclosureState = "retryable_denied"
	// DisclosureTerminalDenied 目录外：拒绝且**不可重试**——server 目录里
	// 没有这个工具，重试不会成功；模型应换用目录内工具或修正工具名。
	DisclosureTerminalDenied DisclosureState = "terminal_denied"
)

// DisclosureGuidance 模型面文案模板（宿主可覆写；零值字段回落内置缺省）。
// %s 占位：工具名（两个占位时依次为工具名、server 名）。
type DisclosureGuidance struct {
	// Retryable 目录内未披露的拒绝文案：要点名工具、声明已触发激活、
	// 明示「原样重试必成、不要换工具」。
	Retryable string
	// Terminal 目录外的拒绝文案：要点名工具、明示「重试不会成功、请换
	// 目录内工具或修正工具名」。
	Terminal string
}

// 内置缺省文案：与三态语义逐字对齐（改文案须同步 godoc 示意）。
const (
	defaultRetryableGuidance = "工具 %q 存在但尚未对本次会话披露；已触发激活，请原样重试同一调用（激活后重试必成），不要改用其他工具。"
	defaultTerminalGuidance  = "工具 %q 不在 server %q 的目录中；重试不会成功，请改用目录内的工具或修正工具名。"
)

// DisclosureVerdict 门禁判定结果。
type DisclosureVerdict struct {
	// State 三态结论（Allowed 时为 DisclosureAllowed）。
	State DisclosureState
	// Tool 被判定的工具名（原样回显）。
	Tool string
	// Activated 本次判定是否已顺手完成激活（仅三态=Retryable 且激活器
	// 成功时为真——「拒但顺手激活」：模型收到拒绝文案重试时，工具已就位）。
	Activated bool
	// Guidance 模型面文案（Allowed 时为空）。拒绝必须给模型可读的理由与
	// 下一步指示——「该重试还是该换工具」是三态语义的模型面投影。
	Guidance string
	// Err 激活器失败时的原始错误（激活器未配置或成功时为 nil）。三态仍按
	// 目录事实判定（Err 不改变 State）；处置层可据此告警激活通道故障。
	Err error
}

// Allowed 是否放行。
func (v DisclosureVerdict) Allowed() bool { return v.State == DisclosureAllowed }

// Retryable 拒绝后重试是否可能成功（目录内未披露=true；目录外=false）。
func (v DisclosureVerdict) Retryable() bool { return v.State == DisclosureRetryableDenied }

// DisclosureGate 渐进披露门禁：server 面目录 + 已披露集 + 激活动作 → 模型面
// 三态判定。
//
// 用法（宿主装配层）：
//
//	gate := mcp.NewDisclosureGate(catalog, []string{"srv.search"}, // 已装配（已披露）的工具
//	    mcp.WithDisclosureActivator(func(ctx context.Context, name string) error { return reload(ctx, name) }))
//	v := gate.Check(ctx, "srv.write") // 目录内未披露 → 拒 + 顺手激活 + 可重试文案
//
// 名字空间约定：Check 的名字与 Catalog/loaded 的名字须同空间（原始名或带
// server 的暴露名均可——门禁只做集合比对，不猜命名）。并发安全：Check 可被
// 多 goroutine 调用；激活成功即入已披露集（之后判定为放行）。激活器在门禁
// 锁外执行（oagentkit/mcpkit#75）：慢激活不阻塞任何 Check/Loaded/Catalog，
// 锁只保护 loaded/inflight 两个可变结构（catalog/guidance 构造后不可变）。
// 取消语义（oagentkit/mcpkit#145）：Check 的 ctx 取消/超时可退出——等待者
// 不再干等 leader 的激活收尾，leader 自身也不把已取消的 ctx 带进激活器
// （激活是宿主外部动作，可挂可慢，调用方必须留有逃生门）。
type DisclosureGate struct {
	mu      sync.Mutex
	catalog Catalog
	loaded  map[string]struct{}
	// inflight 单飞去重（oagentkit/mcpkit#75）：同名工具的并发激活收敛到
	// 一次执行——首个注册者在锁外跑激活器，后来者等其 done（等的是**别的
	// goroutine** 在锁外干活，不是等锁，无死锁面；等待者可随自身 ctx 退出，
	// oagentkit/mcpkit#145）。激活失败的代不留痕迹，下一次 Check 起新一代重试。
	inflight map[string]*activationCall
	// activate 宿主提供的激活动作（把工具从「未披露」转「已披露」——如把
	// 工具装进模型的工具表并热刷新）；ctx 是触发激活的 Check 调用方 ctx
	//（oagentkit/mcpkit#145：激活是宿主外部动作，实现应在 reload/热刷新等
	// 长操作上响应取消）；nil = 门禁不代激活（宿主自管，判定仍给出可重试语义）。
	activate func(ctx context.Context, tool string) error
	// guidance 文案模板（零值字段回落内置缺省）。
	guidance DisclosureGuidance
}

// activationCall 一次进行中的激活：done 关闭即完成（err 为激活器结果）。
// close(done) 前先在锁内完成 loaded 入集/回滚——等待者醒来必见终态。
type activationCall struct {
	done chan struct{}
	err  error
}

// DisclosureGateOption 门禁构造选项（XxxOption 命名纪律：多包 co-import 防冲突）。
type DisclosureGateOption func(*DisclosureGate)

// WithDisclosureActivator 配置激活动作：判定为「目录内未披露」时顺手调用
// （成功即入已披露集，模型重试时直接放行——「拒但顺手激活，重试必成」的
// 落地）。未配置时门禁只判定不激活（可重试语义仍在，激活由宿主另行完成）。
// fn 的 ctx 为触发激活的 Check 调用方 ctx（oagentkit/mcpkit#145）：调用方
// 取消/超时后激活不应继续——reload/热刷新等长操作应响应 ctx。
//
// 锁契约（oagentkit/mcpkit#75）：激活器在门禁锁**外**执行——慢激活（装配
// 工具、热刷新等）不阻塞其他工具的 Check/Loaded/Catalog；激活器允许回查
// 本门禁（Loaded/Catalog/对其他工具的 Check 均安全，不会自死锁）。同名工具
// 的并发激活由单飞去重：同一次激活代内激活器只执行一次。唯一禁止：对
// **正在激活的同一工具**递归 Check——该调用会等待本次激活完成（即等待自身；
// 该递归 Check 的 ctx 取消可退出，#145）。激活器 panic 被门禁转为激活通道
// 故障（oagentkit/mcpkit#89）：inflight 清理照走、等待者醒来，Check 返回可重
// 态 + Err（与返回 error 同形态），不再向调用方上抛。
func WithDisclosureActivator(fn func(ctx context.Context, tool string) error) DisclosureGateOption {
	return func(g *DisclosureGate) { g.activate = fn }
}

// WithDisclosureGuidance 覆写模型面文案（零值字段回落内置缺省）。
func WithDisclosureGuidance(guidance DisclosureGuidance) DisclosureGateOption {
	return func(g *DisclosureGate) { g.guidance = guidance }
}

// NewDisclosureGate 构造门禁。catalog 为 server 面目录（哪些工具**存在**）；
// loaded 为已披露（已加载给模型）的工具名子集。目录为空 Catalog 时一切判定
// 都是目录外终止（没有任何工具可给）。
func NewDisclosureGate(catalog Catalog, loaded []string, opts ...DisclosureGateOption) *DisclosureGate {
	g := &DisclosureGate{catalog: catalog, loaded: make(map[string]struct{}, len(loaded))}
	for _, n := range loaded {
		if n != "" {
			g.loaded[n] = struct{}{}
		}
	}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// Check 判定一个工具名当前可否给模型。三态语义（写死，勿改）：
//
//   - 已披露 → 放行（DisclosureAllowed）；
//   - 目录内未披露 → 拒绝但**可重试**（DisclosureRetryableDenied）：配置了
//     激活器则顺手激活（成功即 Activated=true 且入已披露集——同名的下一次
//     Check 放行，模型侧「原样重试必成」由此兑现）；
//   - 目录外 → 拒绝且**不可重试**（DisclosureTerminalDenied）：重试不会成功，
//     文案指示模型换工具。
//
// 取消语义（oagentkit/mcpkit#145）：激活是宿主外部动作（reload/热刷新——可慢
// 可挂），ctx 取消/超时是调用方的逃生门：等待者 select call.done 与 ctx.Done()
// 各自可退（等待者先退是安全的——失败代由 leader 收尾，激活成功后已入集，
// 同名的下一次 Check 照常放行）；leader 自身 ctx 已死时不启动激活器（inflight
// 照常收尾），等待中的激活器拿到调用方 ctx，长操作可感知取消。三态判定不受
// ctx 影响——取消只影响「激活等待」，目录事实照常输出。
//
// 锁纪律（oagentkit/mcpkit#75）：g.mu 只罩住 loaded/inflight 的读写与三态
// 判定——激活器调用在锁外执行，慢激活不把 Check/Loaded/Catalog 拖成全局
// 串行段，激活器回查门禁也不自死锁。
func (g *DisclosureGate) Check(ctx context.Context, tool string) DisclosureVerdict {
	// 三态判定与单飞注册同一临界区：loaded 复查、目录判定、inflight 注册
	// 原子完成。拆成两段锁会让迟到者漏看刚入集的 loaded 又看不到进行中的
	// 激活，从而重复激活（TestDisclosureConcurrentCheck 的「恰好一次」契约）。
	g.mu.Lock()
	if _, ok := g.loaded[tool]; ok {
		g.mu.Unlock()
		return DisclosureVerdict{State: DisclosureAllowed, Tool: tool}
	}
	if !g.catalog.Contains(tool) {
		g.mu.Unlock()
		return DisclosureVerdict{
			State:    DisclosureTerminalDenied,
			Tool:     tool,
			Guidance: g.terminalGuidance(tool),
		}
	}
	if g.activate == nil {
		g.mu.Unlock()
		return DisclosureVerdict{ // 宿主自管激活：可重试语义照给，不代激活
			State:    DisclosureRetryableDenied,
			Tool:     tool,
			Guidance: g.retryableGuidance(tool),
		}
	}
	call, leader := g.inflight[tool], false
	if call == nil {
		call = &activationCall{done: make(chan struct{})}
		if g.inflight == nil {
			g.inflight = make(map[string]*activationCall)
		}
		g.inflight[tool] = call
		leader = true
	}
	g.mu.Unlock()

	if !leader {
		// 等 leader 在锁外跑完激活器（等的是激活，不是锁）；ctx 可退
		//（oagentkit/mcpkit#145）——等待者先退是安全的：激活成功后已入集，
		// 同名的下一次 Check 照常放行；激活失败代由 leader 收尾。
		select {
		case <-call.done:
		case <-ctx.Done():
			return DisclosureVerdict{
				State:    DisclosureRetryableDenied,
				Tool:     tool,
				Guidance: g.retryableGuidance(tool),
				Err:      ctx.Err(),
			}
		}
		if call.err != nil {
			// 同代激活失败如实透传（Err 交处置层告警）；失败代已从 inflight
			// 移除，下一次 Check 自动起新代重试
			return DisclosureVerdict{
				State:    DisclosureRetryableDenied,
				Tool:     tool,
				Guidance: g.retryableGuidance(tool),
				Err:      call.err,
			}
		}
		return g.allowedIfLoaded(tool)
	}

	// leader：ctx 已死不启动激活器（激活是宿主外部动作，不给已取消的调用
	// 放大等待面）；inflight 照常收尾，等待者醒来拿到取消语义。
	if err := ctx.Err(); err != nil {
		err = fmt.Errorf("mcp: 披露激活未执行（调用方 ctx 已取消）: %w", err)
		g.mu.Lock()
		call.err = err
		delete(g.inflight, tool)
		close(call.done)
		g.mu.Unlock()
		return DisclosureVerdict{
			State:    DisclosureRetryableDenied,
			Tool:     tool,
			Guidance: g.retryableGuidance(tool),
			Err:      err,
		}
	}

	err := g.activateGuarded(ctx, tool) // 锁外：慢激活/re-entrant 回查均安全（#75）；panic 转错误（#89）
	g.mu.Lock()
	call.err = err
	if err == nil {
		if _, dup := g.loaded[tool]; !dup {
			g.loaded[tool] = struct{}{}
		}
	}
	delete(g.inflight, tool)
	close(call.done) // 先入集/回滚、后关闭：等待者醒来必见终态
	g.mu.Unlock()

	v := DisclosureVerdict{
		State:    DisclosureRetryableDenied,
		Tool:     tool,
		Guidance: g.retryableGuidance(tool),
	}
	if err != nil {
		v.Err = err // 激活通道故障：目录事实不变（仍可重试），Err 交处置层告警
		return v
	}
	v.Activated = true
	return v
}

// activateGuarded 锁外执行激活器，panic 转 error（oagentkit/mcpkit#89）：激活器
// panic 原样穿透曾让 Check 的 inflight 清理段永不执行——inflight[tool] 残留、
// call.done 永不关闭，宿主 agent 框架在工具调用外层 recover（GoSafe 类）进程存活
// 后，该工具的每一次 Check（含并发后来者）都永久阻塞在 done 上，门禁不可恢复地
// 卡死且无观测信号。与本仓依赖的 concurrency/singleflight.Do 同口径：panic 转
// error，leader 侧走既有失败收尾（call.err/delete/close——defer/finally 语义，
// panic 与 error 同一出口），等待者醒来拿到同代「激活通道故障」语义（#75），
// 失败代不留痕迹，下一次 Check 起新代重试。ctx 直通激活器（#145）：宿主长操作
// 可感知调用方取消。
func (g *DisclosureGate) activateGuarded(ctx context.Context, tool string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("mcp: 披露激活器 panic: %v", r)
		}
	}()
	return g.activate(ctx, tool)
}

// allowedIfLoaded 激活完成后的放行复判：并发同代激活里，等待者在 leader 成功
// 入集后到达这里——与旧串行实现（后来者拿锁直接命中放行）同一观测语义。
// 激活成功必入集，未命中属理论不可达的保守回落：维持可重试语义不虚假放行。
func (g *DisclosureGate) allowedIfLoaded(tool string) DisclosureVerdict {
	g.mu.Lock()
	_, ok := g.loaded[tool]
	g.mu.Unlock()
	if ok {
		return DisclosureVerdict{State: DisclosureAllowed, Tool: tool}
	}
	return DisclosureVerdict{
		State:    DisclosureRetryableDenied,
		Tool:     tool,
		Guidance: g.retryableGuidance(tool),
	}
}

// Loaded 当前已披露集快照（排序；观测/对账用）。
func (g *DisclosureGate) Loaded() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, 0, len(g.loaded))
	for n := range g.loaded {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// Catalog 门禁持有的 server 面目录快照。
func (g *DisclosureGate) Catalog() Catalog {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.catalog
}

// retryableGuidance/terminalGuidance 的渲染单源：模板内的 %s/%q 占位按出现
// 顺序替换——第一个吃工具名、后续依次吃 server 名（缺省 Terminal 模板的
// 「工具+server」双占位即此形态）；不经过 fmt（#76）。
func (g *DisclosureGate) retryableGuidance(tool string) string {
	if g.guidance.Retryable != "" {
		return renderGuidance(g.guidance.Retryable, tool, g.catalog.Server)
	}
	return renderGuidance(defaultRetryableGuidance, tool, g.catalog.Server)
}

func (g *DisclosureGate) terminalGuidance(tool string) string {
	if g.guidance.Terminal != "" {
		return renderGuidance(g.guidance.Terminal, tool, g.catalog.Server)
	}
	return renderGuidance(defaultTerminalGuidance, tool, g.catalog.Server)
}

// renderGuidance 模板渲染：手工扫描替换 %s/%q 占位（%% 折叠为字面 %），按
// 出现顺序注入——第 1 个动词吃工具名、后续依次吃 server 名（缺省 Terminal
// 模板的「工具+server」双占位即此形态）；无占位动词的模板原样输出。绝不
// 经过 fmt，杜绝 %!(EXTRA string=…)/%!s(MISSING) 伪影泄漏进模型面拒绝文案
// （oagentkit/mcpkit#76：宿主模板无动词时 fmt.Sprintf 把多余实参追加成
// 乱码，配置期零信号）。其他动词形态（%v/%d 等）不在契约内，字面输出。
func renderGuidance(tpl, tool, server string) string {
	var b strings.Builder
	b.Grow(len(tpl) + 2*len(tool))
	n := 0
	for i := 0; i < len(tpl); i++ {
		c := tpl[i]
		if c != '%' || i+1 >= len(tpl) {
			b.WriteByte(c)
			continue
		}
		switch tpl[i+1] {
		case '%':
			b.WriteByte('%')
			i++
		case 's', 'q':
			arg := server
			if n == 0 {
				arg = tool // 第一个占位点名工具
			}
			n++
			if tpl[i+1] == 'q' {
				b.WriteString(strconv.Quote(arg))
			} else {
				b.WriteString(arg)
			}
			i++
		default:
			b.WriteByte(c) // 非契约动词：字面保留
		}
	}
	return b.String()
}
