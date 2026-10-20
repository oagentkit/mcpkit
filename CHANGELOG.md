# mcpkit CHANGELOG

> 本文件由 `matrix-tools/scripts/gen_changelogs.py` 从 git 历史生成，按 tag 切段、取每段内的 conventional commit 渲染。

## [v0.8.3] — 2026-10-20

- 杂务(deps)：依赖地板抬到最新

地板对齐到发版时刻的各依赖最新 tag：
- oekit/pkg、oekit/concurrency 抬到 v0.18.0（oekit/logx 已是最新 v0.7.1）
- 矩阵内跨仓依赖抬到各仓最新 tag
依据 matrix-tools#44：最新 tag 的依赖地板在「打 tag 那一刻」就已落后一格，
只能靠后续版本补齐（tag 受 protect-tags ruleset 保护不能重打）。


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

matrix-tools 的 verify_dep_floors.py 首次全矩阵扫描查出的地板漂移，本仓是受影响仓之一。
本轮按 tier 拓扑顺序推进：tier0（textx / runtime / guardx / queue）先发，
本仓再把地板跟到它们的新 tag——否则就是「发了新的、旧的立刻变落后」，永远收敛不了。
所升版本与原钉版本相比源码零改动，逐版实测确认：
- textx v0.4.0→v0.5.0 / runtime v0.6.0→v0.7.0 / guardx v0.4.1→v0.5.0
  全树逐字节相同（diff -rq 退出码 0）
- oekit/pkg v0.14.0→v0.15.1  仅新增 urlguard 包（纯标准库，pkg 的 go.mod
  连 require 块都没有，零新增三方依赖）
- oekit/concurrency v0.17.0→v0.17.1  仅三个文档文件
- oekit/logx v0.7.0→v0.7.1  仅 CI 配置与文档
无 API 变更、无兼容风险。
本地门禁：go mod tidy -diff 无输出 / go build / go vet / gofmt / go test -race 全通过。
关联 oagentkit/matrix-tools#13


## [v0.7.0] — 2026-10-19

- 杂务(deps)：依赖地板对齐远端最新（runtime v0.6.0 / concurrency v0.17.1）

matrix-tools 的 verify_dep_floors.py 首次全矩阵扫描查出：43 条第一方依赖里
28 条落后，横跨 11 个仓；本仓是其中之一。
所升版本与本仓原钉版本相比源码零改动，逐版实测确认：
- textx v0.3.0→v0.4.0 / obs v0.4.1→v0.5.0 / runtime v0.5.0→v0.6.0 /
  mcpkit v0.5.1→v0.6.0  全树逐字节相同（diff -rq 退出码 0）
- oekit/pkg v0.14.0→v0.15.1  仅新增 urlguard 包（纯标准库，pkg 的 go.mod
  连 require 块都没有，零新增三方依赖）
- oekit/concurrency v0.17.0→v0.17.1  仅三个文档文件
- oekit/logx v0.7.0→v0.7.1  仅 CI 配置与文档
无 API 变更、无兼容风险。
本地门禁：go mod tidy -diff 无输出 / go build / go vet / gofmt / go test -race 全通过。


## [v0.6.0] — 2026-10-19

- 杂务(deps)：oekit 依赖升级（concurrency v0.16.0→v0.17.0）

本 PR 含两项改动。
## 1. oekit 依赖地板
姊妹矩阵 oekit 的 `concurrency` 出了 v0.17.0，依赖地板一并对齐。
升级前已核对 `oekit/concurrency` v0.16.0...v0.17.0 的全部 2 个提交：
- `feat(ratelimit): KeyedLimiter.Create 护栏内原子登记 (Fixes #33)` — `ratelimit/keyed.go` +28 行
- `docs: 矩阵总览表 DESC 口径同步` — 仅 AGENTS.md / README.md / CHANGELOG.md
无 breaking / remove 标记，未触及任何已有导出签名，故为纯补丁式升级。
## 2. 迁出被 force 移动过的 tag
本仓原先依赖的姊妹仓 tag（发布后被强推到了重写后的 main）内容已与
sum.golang.org 记录不一致，于是任何走 `proxy.golang.org` 或 `GOPROXY=direct`
的构建都会 SECURITY ERROR。本地往往不报错——模块缓存预热 + goproxy.cn
还留着移动前的字节，把问题掩盖了。
已为 8 个模块重打不可变的**附注 tag**（内容与被移动 tag 指向同一 commit，
只换版本号，让校验和数据库重新记录一次正确哈希），并把依赖迁过去。
`go.sum` 的 h1 全部取自 sum.golang.org，仅就地替换目标模块的两行
（zip h1 + go.mod h1），其余行不动。


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

本轮做了版本线重置：清空全部历史 tag，按「历史最新 + 0.1.0」重打新版本
（如 llm v0.6.0 → v0.7.0、toolgate v0.6.1 → v0.7.0、agentkit v1.3.0 → v1.4.0）。
各仓 go.mod 的矩阵内依赖地板同步跟进到新版本号。
**go.sum 的 oagentkit 行已整段删除**，由 CI 的 `go mod tidy` 重算：
新 tag 的内容与任何历史版本都不同，`h1` 必须现算；本地没有 Go 工具链，
且 `/go.mod` 的 dirhash 无法自行复现（zip hash 可复现，gomod hash 不可复现）。
重算依赖 `.github` 的 go-ci 配了 `GOPRIVATE=github.com/oagentkit/*` 直连 VCS：
goproxy.cn 缓存着旧版本，若走 proxy 会拉回**旧代码**并算出旧 hash，
门禁全绿但依赖是错的。
（第二个提交会补上重算好的 go.sum。）
* chore(deps): 依赖地板升到「历史最新 + 0.1.0」
chore(deps): 依赖地板升到「历史最新 + 0.1.0」
本轮做了版本线重置：清空全部历史 tag，按「历史最新 + 0.1.0」重打新版本
（如 llm v0.6.0 → v0.7.0、toolgate v0.6.1 → v0.7.0、agentkit v1.3.0 → v1.4.0）。
各仓 go.mod 的矩阵内依赖地板同步跟进到新版本号。
**go.sum 的 oagentkit 行已整段删除**，由 CI 的 `go mod tidy` 重算：
新 tag 的内容与任何历史版本都不同，`h1` 必须现算；本地没有 Go 工具链，
且 `/go.mod` 的 dirhash 无法自行复现（zip hash 可复现，gomod hash 不可复现）。
重算依赖 `.github` 的 go-ci 配了 `GOPRIVATE=github.com/oagentkit/*` 直连 VCS：
goproxy.cn 缓存着旧版本，若走 proxy 会拉回**旧代码**并算出旧 hash，
门禁全绿但依赖是错的。
（第二个提交会补上重算好的 go.sum。）
* chore(deps): 回填 CI 重算的 go.sum
`h1` 由 CI 里真实的 `go mod tidy` 算出后回填——本地没有 Go 工具链，
且 `/go.mod` 的 dirhash 无法自行复现（zip hash 可复现，gomod hash 不可复现），
所以这是唯一权威来源。
重算依赖 `.github` go-ci 里的 `GOPRIVATE=github.com/oagentkit/*` 直连 VCS：
goproxy.cn 仍缓存着版本线重置前的旧内容，走 proxy 会拉回**旧代码**并算出旧 hash，
门禁全绿但依赖是错的。
验证方式就是本 PR 的 `go mod tidy -diff`：若回填有任何偏差，它会照样报差异。
* chore(deps): 回填 CI 重算的 go.sum
`h1` 由 CI 里真实的 `go mod tidy` 算出后回填——本地没有 Go 工具链，
且 `/go.mod` 的 dirhash 无法自行复现（zip hash 可复现，gomod hash 不可复现），
所以这是唯一权威来源。
重算依赖 `.github` go-ci 里的 `GOPRIVATE=github.com/oagentkit/*` 直连 VCS：
goproxy.cn 仍缓存着版本线重置前的旧内容，走 proxy 会拉回**旧代码**并算出旧 hash，
门禁全绿但依赖是错的。
验证方式就是本 PR 的 `go mod tidy -diff`：若回填有任何偏差，它会照样报差异。
* chore(deps): 回填 CI 重算的 go.sum
`h1` 由 CI 里真实的 `go mod tidy` 算出后回填——本地没有 Go 工具链，
且 `/go.mod` 的 dirhash 无法自行复现（zip hash 可复现，gomod hash 不可复现），
所以这是唯一权威来源。
重算依赖 `.github` go-ci 里的 `GOPRIVATE=github.com/oagentkit/*` 直连 VCS：
goproxy.cn 仍缓存着版本线重置前的旧内容，走 proxy 会拉回**旧代码**并算出旧 hash，
门禁全绿但依赖是错的。
验证方式就是本 PR 的 `go mod tidy -diff`：若回填有任何偏差，它会照样报差异。

- 杂务(deps)：跟进最新 tag

oagentkit/runtime v0.3.1→v0.4.0
go.sum 取 sumdb 权威 h1，仅就地替换目标 mod 两行。

- 杂务(deps)：oekit 依赖升级

concurrency v0.13.1→v0.16.0
姊妹矩阵 oekit 的依赖地板此前被漏掉（上一轮只扫了 oagentkit 内部依赖），本次一并对齐到最新。
升级前已确认 oekit/concurrency v0.13.1..v0.16.0 的 8 个提交全是 feat/docs/chore，
无 breaking/remove 标记。其中 #28 订正了 `WithCooldown(0)` 的语义——
即 llms.txt 里警告过的那个陷阱，升上去正好拿到修正后的行为。
go.sum 的 h1 取自 sumdb，仅就地替换目标 mod 的两行、其余行不动。

- 杂务(deps)：跨仓依赖升级

runtime v0.1.0 → v0.3.1
全 org 跨仓依赖地板统一升级。go.sum 的 h1 取自 sumdb（经 goproxy.cn 代理），
即 `go mod tidy` 会写入的同一个权威值；只就地替换该 mod 的两行、其余行一个字节不动，
不重排全文件（go.sum 行序由 cmd/go 自己维护，手写重排会引入漂移）。

- 杂务(deps)：oekit/concurrency v0.12.0 -> v0.13.1

本仓不使用上游 breaker（熔断只在 llm 仓），v0.13.0 的 #25 修复与
v0.13.1 的 #26（RetryableWithHeaders 头值 OWS 修剪）对本仓无直接影响，
本次为矩阵内版本对齐。
门禁：go mod tidy -diff / build / vet /
test -race / gofmt 全绿。

- 杂务(deps)：oekit 三模块升至最新（pkg v0.13.0 / concurrency v0.12.0 / logx v0.7.0）

拿到上游新增能力：
- concurrency #22 RetryableWithHeaders：403 头感知判定（次级限流 / 主限流耗尽）
- concurrency #23 FanInLimit：受限并发扇出
- concurrency #24 WithTokenClock、#21 Lockout（keyed lockout/penalty）
- logx #12 TraceHandler：给裸 slog.Handler 注入 trace_id
破坏性变更核对（均无影响）：
- logx v0.7.0 ParseLogLevel 签名改为 (Level, error) —— 我们未使用该函数
- concurrency v0.9.0 的 Slot.Panicked 废弃 —— rag 已迁到 Slot.Err + errors.As
门禁：go build / go vet / go test -race 全绿。

- 杂务(deps)：oekit/concurrency v0.6.0 -> v0.9.0

拿到上游显式三态状态机（#17）与 singleflight 可观测折叠（#18），并为本次
breaker 替换评估了语义差异。
用到的子包只有 async / singleflight / syncx —— 上游 breaker 包我们并未引用
（runtime/breaker 是本仓自己的三态实现），所以本次升级零 breaking。
关于 breaker 替换的现状：#17 已落地（closed/open/half-open + 代绑定 + 探测时限 +
开放期失败不延长冷却），但两侧的触发语义仍不对齐，已在上游补两个需求：
- #19 冷却期与探测失联时限共用 probeTimeout，无法独立配置
- #20 缺「连续失败 N 次即熔断」（trip）触发
因此本包暂不删除 —— llm.TestResilientBreakerTrips 钉的是 trip 语义，
上游 v0.9.0 只有速率型触发（dropRatio > 0），现在换会让该测试失败。


- CI：接入 org 共享门禁（规则本体在 oagentkit/.github）
