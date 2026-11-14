package mcp

// Version 包版本：发版经 ldflags 注入（-X github.com/oagentkit/mcpkit/mcp.Version=vX.Y.Z），
// 未注入时为 "dev"。
//
// 跨进程契约里的身份字符串必须可归因（oagentkit/mcpkit#150，同族纪律见
// llm#144 的 ErrKey 单源）：本包作为 MCP client 向 server 发 initialize 时
// 以 mcpkit@Version 自报身份——server 侧日志/审计按 ClientInfo 归因连接方，
// 硬编码 "agentkit"/"1.0" 会归到不存在的仓名、版本恒 1.0（mcpkit 升版在
// server 侧不可见，安全审计与兼容性排查失去锚点）。
var Version = "dev"
