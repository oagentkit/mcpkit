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
	"fmt"
	"sort"
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
//	    mcp.WithDisclosureActivator(func(name string) error { return reload(name) }))
//	v := gate.Check("srv.write") // 目录内未披露 → 拒 + 顺手激活 + 可重试文案
//
// 名字空间约定：Check 的名字与 Catalog/loaded 的名字须同空间（原始名或带
// server 的暴露名均可——门禁只做集合比对，不猜命名）。并发安全：Check 可被
// 多 goroutine 调用；激活成功即入已披露集（之后判定为放行）。
type DisclosureGate struct {
	mu      sync.Mutex
	catalog Catalog
	loaded  map[string]struct{}
	// activate 宿主提供的激活动作（把工具从「未披露」转「已披露」——如把
	// 工具装进模型的工具表并热刷新）；nil = 门禁不代激活（宿主自管，判定
	// 仍给出可重试语义）。
	activate func(tool string) error
	// guidance 文案模板（零值字段回落内置缺省）。
	guidance DisclosureGuidance
}

// DisclosureGateOption 门禁构造选项（XxxOption 命名纪律：多包 co-import 防冲突）。
type DisclosureGateOption func(*DisclosureGate)

// WithDisclosureActivator 配置激活动作：判定为「目录内未披露」时顺手调用
// （成功即入已披露集，模型重试时直接放行——「拒但顺手激活，重试必成」的
// 落地）。未配置时门禁只判定不激活（可重试语义仍在，激活由宿主另行完成）。
func WithDisclosureActivator(fn func(tool string) error) DisclosureGateOption {
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
func (g *DisclosureGate) Check(tool string) DisclosureVerdict {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.loaded[tool]; ok {
		return DisclosureVerdict{State: DisclosureAllowed, Tool: tool}
	}
	if !g.catalog.Contains(tool) {
		return DisclosureVerdict{
			State:    DisclosureTerminalDenied,
			Tool:     tool,
			Guidance: g.terminalGuidance(tool),
		}
	}
	v := DisclosureVerdict{
		State:    DisclosureRetryableDenied,
		Tool:     tool,
		Guidance: g.retryableGuidance(tool),
	}
	if g.activate == nil {
		return v // 宿主自管激活：可重试语义照给，不代激活
	}
	if err := g.activate(tool); err != nil {
		v.Err = err // 激活通道故障：目录事实不变（仍可重试），Err 交处置层告警
		return v
	}
	g.loaded[tool] = struct{}{}
	v.Activated = true
	return v
}

// Loaded 当前已披露集快照（排序；观测/对账用）。
func (g *DisclosureGate) Loaded() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, 0, len(g.loaded))
	for n := range g.loaded {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Catalog 门禁持有的 server 面目录快照。
func (g *DisclosureGate) Catalog() Catalog {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.catalog
}

func (g *DisclosureGate) retryableGuidance(tool string) string {
	if g.guidance.Retryable != "" {
		return fmt.Sprintf(g.guidance.Retryable, tool)
	}
	return fmt.Sprintf(defaultRetryableGuidance, tool)
}

func (g *DisclosureGate) terminalGuidance(tool string) string {
	if g.guidance.Terminal != "" {
		return fmt.Sprintf(g.guidance.Terminal, tool)
	}
	return fmt.Sprintf(defaultTerminalGuidance, tool, g.catalog.Server)
}
