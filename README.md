# mcpkit

MCP 工具池——客户端封装/工具目录/建连单飞/自愈与租约管理 —— oagentkit 模块矩阵 **tier 2**（集成层，依赖 tier0/tier1 + 重第三方地板）。

```
go get github.com/oagentkit/mcpkit
```

| 包 | 职责 |
|---|---|
| `mcp` | MCP 客户端/工具池：工具目录、建连单飞、自愈与租约管理 |

## 定位

**tier 2** — 集成层，依赖 tier0/tier1 + 重第三方地板。

- 本仓依赖的矩阵内仓：`runtime`。

## 外部依赖地板

- `github.com/mark3labs/mcp-go`
- `github.com/eino-contrib/jsonschema`
- `github.com/cloudwego/eino`

## 与 oekit 的关系

本矩阵（oagentkit，Agent/LLM 编排工具链）**依赖** oekit（Open Easy Kit，Go 微服务工具链）：
`oekit/pkg` / `oekit/concurrency` / `oekit/logx` 等。oekit 是另一套独立矩阵，不是子品牌。

## 开发

```bash
go test ./... -race
go vet ./...
gofmt -l .
```

规范见 [AGENTS.md](AGENTS.md)。

## License

MIT
- **建连必须单飞。** 同一 server 的并发建连要合并为一次，
  不得每次 `Pool.Tools`（带 `ToolSpec.Allow` 白名单）都新建连接。
- **等待方的 ctx 取消要各自生效**，不能干等 leader。
- 工具注解（`mcp.ToolAnnotation`）归一时，`nil` 视为规范缺省，**不得当成「无限制」**。