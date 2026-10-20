package mcp

import (
	"encoding/json"
	"sort"
)

// HealthState 健康三态。
type HealthState string

const (
	// HealthHealthy 全绿：server 全可达，声明的工具全在且可调。
	HealthHealthy HealthState = "healthy"
	// HealthDegraded 降级：server 活着，但**部分**能力不可用
	//（工具缺失 / 个别工具调用失败）。服务还在提供部分能力。
	HealthDegraded HealthState = "degraded"
	// HealthDown 全挂：server 连不上（基础设施故障）。
	HealthDown HealthState = "down"
)

// MarshalJSON 输出可读状态名。
func (s HealthState) MarshalJSON() ([]byte, error) { return json.Marshal(string(s)) }

// Health 健康聚合结果。
//
// 判据可观察：**能看出是哪个 server 的哪个工具出的问题**——不是只给一个总体布尔。
// Issues 逐条给出 server / tool / 分类 / 原因。
type Health struct {
	// State 三态结论。
	State HealthState
	// Servers 参与聚合的 server 名（排序）。
	Servers []string
	// Issues 问题清单（按 server → tool 排序）。
	Issues []HealthIssue
	// Total 参与聚合的 server 数。
	Total int
	// HealthyCount 完全健康的 server 数。
	HealthyCount int
	// DegradedCount 降级的 server 数。
	DegradedCount int
	// DownCount 不可达的 server 数。
	DownCount int
}

// HealthIssue 单条问题。
type HealthIssue struct {
	// Server 出问题的 server 名（空 = 跨 server 汇总项）。
	Server string
	// Tool 出问题的工具名（空 = server 级问题，如连不上）。
	Tool string
	// Kind 错误分类。
	Kind ErrorKind
	// Reason 人类可读原因。
	Reason string
}

// Healthy 是否完全健康。
func (h Health) Healthy() bool { return h.State == HealthHealthy }

// Degraded 是否降级。
func (h Health) Degraded() bool { return h.State == HealthDegraded }

// Down 是否不可用。
func (h Health) Down() bool { return h.State == HealthDown }

// OK 是否可服务（healthy 或 degraded 都还有能力在提供）。
func (h Health) OK() bool { return h.State == HealthHealthy || h.State == HealthDegraded }

// Aggregate 把多个 server 的自检结果聚合成一个健康结论。
//
// ## 三态判定依据（明确写死，不留给调用方猜）
//
//	down     —— **有 server 连不上**（Reachable==false）。
//	           这是基础设施故障：server 进程挂了 / 网络不通 / 建连超时。
//
//	degraded —— **全部 server 都连得上，但至少一个能力不可用**：
//	           工具缺失（声明了但目录里没有）或个别工具调用失败。
//
//	healthy  —— 全部 server 连得上，且声明的工具全部在目录里、可调。
//
// ## 关键歧义：「工具缺失」归 degraded，**不是** down
//
// 这是下游踩过的坑：把「工具缺失」当「server 挂了」→ 依赖故障被
// 放大成平台故障。理由是二者的**处置完全不同**：
//
//   - server 连不上 ⇒ 该 server 的能力**全部**不可用 ⇒ 影响面是整个 server。
//   - 工具缺失 ⇒ server **活着**且在正常提供服务，只是没有某个工具
//     ⇒ 影响面是那一个工具。
//
// 若把工具缺失算成 down：一个 server 少了个工具，平台就被判全挂，
// 告警风暴 + 流量全量切走 —— 而实际上 server 还能服务其他工具。
//
// ## 「server 挂了」时，工具状态标 unreachable 而非 missing
//
// server 连不上时**无从知道**它有没有那个工具（没应答），
// 所以自检层把工具标成 ToolUnreachable（未知）而不是 ToolMissing（确认没有）。
// 聚合时 unreachable 的工具**不计入** missing 计数——
// 否则「server 挂了」会被算成「N 个工具缺失」，
// 再次把基础设施故障伪装成契约故障。
//
// ## 部分 server 挂掉时，整体是 down 还是 degraded？
//
// 取**最严重**态：任一 server 不可达 ⇒ 整体 down。
// 理由：聚合结论回答的是「平台能不能按声明提供服务」——
// 声明了却完全拿不到的那个 server，其能力是真缺失的。
// 但 Issues 里会逐条列出，调用方能看出是「哪个 server 挂了」
// 而不是笼统一个 down。
func Aggregate(results []SelfcheckResult) Health {
	h := Health{Total: len(results)}
	issues := make([]HealthIssue, 0)
	servers := make([]string, 0, len(results))

	anyDown := false
	anyDegraded := false

	for _, r := range results {
		servers = append(servers, r.Server)
		if !r.Reachable {
			// server 级问题：连不上。
			anyDown = true
			h.DownCount++
			issues = append(issues, HealthIssue{
				Server: r.Server,
				Kind:   r.Kind,
				Reason: serverDownReason(r),
			})
			// 该 server 的工具状态为 unknown（不可达 ≠ 缺失），不计入 missing。
			continue
		}
		// server 活着：逐工具看。
		degraded := false
		for _, t := range r.Tools {
			switch t.Status {
			case ToolMissing:
				degraded = true
				issues = append(issues, HealthIssue{
					Server: r.Server,
					Tool:   t.Tool,
					Kind:   t.Kind,
					Reason: t.Reason,
				})
			case ToolFailed:
				degraded = true
				issues = append(issues, HealthIssue{
					Server: r.Server,
					Tool:   t.Tool,
					Kind:   t.Kind,
					Reason: t.Reason,
				})
			case ToolUnreachable:
				// 状态未知：server 活着但这工具的状态不明（罕见：单工具探测
				// 时 server 中途失联）。**不**当缺失算，但当降级算——
				// 至少要知道它不可用。
				degraded = true
				issues = append(issues, HealthIssue{
					Server: r.Server,
					Tool:   t.Tool,
					Kind:   t.Kind,
					Reason: t.Reason,
				})
			}
		}
		if degraded {
			anyDegraded = true
			h.DegradedCount++
		} else {
			h.HealthyCount++
		}
	}

	sort.Strings(servers)
	h.Servers = servers
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Server != issues[j].Server {
			return issues[i].Server < issues[j].Server
		}
		return issues[i].Tool < issues[j].Tool
	})
	h.Issues = issues

	switch {
	case anyDown:
		h.State = HealthDown
	case anyDegraded:
		h.State = HealthDegraded
	default:
		h.State = HealthHealthy
	}
	return h
}

// serverDownReason server 不可达的原因描述。
func serverDownReason(r SelfcheckResult) string {
	if r.Err != nil {
		return "server 不可达: " + r.Err.Error()
	}
	return "server 不可达"
}
