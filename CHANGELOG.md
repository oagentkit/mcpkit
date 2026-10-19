# mcpkit CHANGELOG

> 本文件由 `matrix-tools/scripts/gen_changelogs.py` 从 git 历史生成，按 tag 切段、取每段内的 conventional commit 渲染。

## [v0.8.1] — 2026-10-20

### 修复(docs)：Allow 是字段不是方法——改指 Pool.Tools 与 ToolSpec.Allow (#6)

Co-authored-by: Claude Opus 4.8 (1M context) <noreply@anthropic.com>

## [v0.8.0] — 2026-10-19

- 杂务(deps)：依赖地板对齐远端最新（runtime v0.7.0） (13)

## [v0.7.0] — 2026-10-19

- 杂务(deps)：依赖地板对齐远端最新（runtime v0.6.0 / concurrency v0.17.1） (#13)

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

- 杂务(deps)：依赖地板升到「历史最新 + 0.1.0」 (#4)
- 杂务(deps)：跟进最新 tag (#3)
- 杂务(deps)：oekit 依赖升级 (#2)
- 杂务(deps)：跨仓依赖升级 (#1)
- 杂务(deps)：oekit/concurrency v0.12.0 -> v0.13.1
- 杂务(deps)：oekit 三模块升至最新（pkg v0.13.0 / concurrency v0.12.0 / logx v0.7.0）
- 杂务(deps)：oekit/concurrency v0.6.0 -> v0.9.0

- CI：接入 org 共享门禁（规则本体在 oagentkit/.github）
