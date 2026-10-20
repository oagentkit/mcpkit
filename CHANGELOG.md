# mcpkit CHANGELOG

> 本文件由 `matrix-tools/scripts/gen_changelogs.py` 从 git 历史生成，按 tag 切段、取每段内的 conventional commit 渲染。

## [v0.8.2] — 2026-10-20

### 新增(mcp)：自检 / 工具目录差集 / 健康聚合 / 错误分类原语

补一组**通用 MCP 客户端能力**——此前这四件事在下游各写一遍
（自研 server 自检命令、自研 /health 深检聚合、自研工具面缺失比对，
另一个下游项目还写了形态不同的第四遍）。它们与「运维诊断」这个领域无关，
是任何 MCP 客户端都可能需要的原语。
## 新增 API
### 错误分类（mcp/errclass.go）
```go
type ErrorKind int
const (
    KindUnknown ErrorKind = iota
    KindUnreachable   // 连不上 —— 基础设施问题：重试/换节点/报警
    KindProtocol      // 协议错 —— server 实现不合规：修 server/升级
    KindToolMissing   // 工具不存在 —— 配置或契约问题：改配置/改契约，不要重试
    KindInvalidArgs   // 参数错 —— 调用问题：改调用方，不要重试
)
func ClassifyError(err error) ErrorKind
func Classify(err error) error   // 包装但保留 errors.Is/As 链
func KindOf(err error) ErrorKind
```
判据锚在**协议层事实**上，不做字符串匹配：JSON-RPC 走 mcp-go 官方哨兵
（`mcp.ErrInvalidParams` / `mcp.ErrMethodNotFound` / `mcp.ErrInternalError` …），
网络层走 `net.Error` / `syscall.ECONNREFUSED` 等错误链类型。
下游此前各自 `strings.Contains(err.Error(), "connection refused")` 判类型，
文案一改就失效。
**关键区分**：工具不存在（契约问题）与参数错（调用问题）处置完全不同——
前者重试一万次那个工具也不会出现，后者重试不会让非法参数变合法。
两者 `Retryable()` 均为 false。
### 工具目录差集（mcp/catalog_diff.go）
```go
type Catalog struct { Server string; Tools []string }
func NewCatalog(server string, tools []string) Catalog
func (c Catalog) Contains(name string) bool
type CatalogDiff struct {
    Missing []string  // 声明了但实际没有（expected - actual）
    Extra   []string  // 实际有但没声明（actual - expected）
}
func Diff(expected, actual Catalog) CatalogDiff
func DiffNames(expected, actual []string) CatalogDiff
```
**双向**是刻意的：只看一个方向会漏掉一半的治理问题——
只看 Missing 漏掉「没声明却存在」的工具（授权面之外的开洞），
只看 Extra 漏掉「声明了却没有」的工具（装包≠开洞）。
结果是「装包≠开洞」「授权面 ⊄ 契约面」这类规则的数据基础。
### 自检（mcp/selfcheck.go）
```go
type SelfcheckOptions struct {
    Server     string
    Timeout    time.Duration
    TTL        time.Duration        // 目录缓存 TTL（0=不缓存）
    ProbeTools bool                 // 逐工具实调探测（有副作用）
    ProbeArgs  map[string]any
    Initialize bool                 // 自检前先发 initialize 握手
    Expected   Catalog              // 声明期望的工具面
}
type SelfcheckResult struct {
    Server    string
    Reachable bool
    Tools     []ToolCheck           // 每工具一条：可用/失败/原因
    Catalog   Catalog
    Expected  Catalog
    Diff      CatalogDiff
    Kind      ErrorKind
    Err       error
    Cached    bool
    At        time.Time
}
type Selfchecker struct{ /* ... */ }
func NewSelfchecker() *Selfchecker
func (s *Selfchecker) Check(ctx, cli client.MCPClient, opts SelfcheckOptions) SelfcheckResult
```
TTL 缓存的理由：MCP 侧工具目录在连接生命周期内视为静态（见 catalog.go），
没有 TTL 时每次巡检都会打一轮 tools/list——部署机的巡检会把 server 压垮。
**连通性判据是「列举成功」而非「ping 成功」**：ping 并非所有 server 都支持
（实测未 Initialize 的 client 直接回 "client not initialized"，而它活得好好的）。
只认 ping 会把「server 活着但没实现/没初始化 ping」误判成 down。
### 健康聚合（mcp/health.go）
```go
type HealthState string
const (
    HealthHealthy  HealthState = "healthy"
    HealthDegraded HealthState = "degraded"
    HealthDown     HealthState = "down"
)
type Health struct {
    State HealthState
    Servers []string
    Issues []HealthIssue   // 逐条：server / tool / 分类 / 原因
    Total, HealthyCount, DegradedCount, DownCount int
}
type HealthIssue struct { Server, Tool string; Kind ErrorKind; Reason string }
func Aggregate(results []SelfcheckResult) Health
```
## 三态判定依据（写死，不留给调用方猜）
- **down** —— 有 server 连不上（基础设施故障：进程挂了/网络不通）。
- **degraded** —— 全部 server 连得上，但至少一个能力不可用
  （工具缺失 / 个别工具调用失败）。
- **healthy** —— 全连得上，且声明的工具全在且可调。
**关键歧义：「工具缺失」归 degraded，不是 down。** 这是下游踩过的坑——
把工具缺失当 server 挂了，依赖故障被放大成平台故障。理由是处置完全不同：
server 连不上 ⇒ 该 server 能力**全部**不可用；工具缺失 ⇒ server **活着**且
在正常服务，只是没有某个工具。若把工具缺失算 down，一个 server 少个工具
平台就被判全挂 → 告警风暴 + 流量全量切走，而它其实还能服务其他工具。
**连带语义**：server 挂掉时，它的工具状态标 `unreachable`（未知）而非
`missing`（确认没有）——没应答时无从知道有没有那个工具。
聚合时 unreachable 的工具**不计入** missing 计数，否则「server 挂了」
会被算成「N 个工具缺失」，再次把基础设施故障伪装成契约故障。
**判据可观察**：Issues 逐条给出 server / tool / 分类 / 原因——
能看出是哪个 server 的哪个工具出的问题，不是只给一个总体布尔。
## CLI 入口
`cmd/mcp-selfcheck`：
    mcp-selfcheck selfcheck --url http://127.0.0.1:8931/mcp --server fs \
        --expect read --expect write --json
退出码：0=healthy、1=degraded、2=down、3=用法/连接错误。
## 测试
全部用**假 server**（编排型 client 假体 + in-process MCP server），不连网、
不依赖真实 MCP 服务端。覆盖：错误分类 4 类（且工具不存在与参数错可区分、
重试语义不同、错误链不丢）、Diff 双向差集与有序性、Selfcheck 结构化结果、
TTL 命中/过期、探针发现「工具在但调不通」、三态聚合、
**「工具缺失 ≠ server 挂」的具体用例**、「server 挂了不被算成 N 个工具缺失」。
## 零依赖
未引入任何新依赖——只用本仓已有的 mcp-go / eino。
* docs(changelog): 预写 v0.8.2 待发版段（自检/健康/错误分类原语）
打 tag 前必须先有自己那一段——tag 快照里缺自己版本的发布说明 = 发布说明残缺（见 verify_changelog_at_tag.py 与 matrix-tools#17）。

## [v0.8.1] — 2026-10-20

### 修复(docs)：Allow 是字段不是方法——改指 Pool.Tools 与 ToolSpec.Allow

Co-authored-by: Claude Opus 4.8 (1M context) <noreply@anthropic.com>

## [v0.8.0] — 2026-10-19

- 杂务(deps)：依赖地板对齐远端最新（runtime v0.7.0） (13)

## [v0.7.0] — 2026-10-19

- 杂务(deps)：依赖地板对齐远端最新（runtime v0.6.0 / concurrency v0.17.1）

## [v0.6.0] — 2026-10-19

- 杂务(deps)：oekit 依赖升级（concurrency v0.16.0→v0.17.0）

## [v0.5.1] — 2026-10-18

- 与 v0.5.0 相比无代码差异（版本号/打包变更）

## [v0.5.0] — 2026-10-18

### 新增：mcpkit 仓建立——MCP 工具池（1 包）

从 agentkit 单仓切出 tier1 能力层模块，module 路径 github.com/oagentkit/mcpkit。
收编 mcp 包：MCP 客户端/工具池、工具目录、建连单飞、自愈与租约管理、UnwrapMCPText。
矩阵内依赖：runtime（procx）。外部地板 mcp-go + eino + eino-contrib/jsonschema。
AGENTS.md 立了三条纪律：建连必须单飞、等待方 ctx 各自生效、工具注解 nil 视为规范缺省。
门禁：go build / go vet / go test -race 全绿，gofmt 干净。

- 杂务(deps)：依赖地板升到「历史最新 + 0.1.0」
- 杂务(deps)：跟进最新 tag
- 杂务(deps)：oekit 依赖升级
- 杂务(deps)：跨仓依赖升级
- 杂务(deps)：oekit/concurrency v0.12.0 -> v0.13.1
- 杂务(deps)：oekit 三模块升至最新（pkg v0.13.0 / concurrency v0.12.0 / logx v0.7.0）
- 杂务(deps)：oekit/concurrency v0.6.0 -> v0.9.0

- CI：接入 org 共享门禁（规则本体在 oagentkit/.github）
