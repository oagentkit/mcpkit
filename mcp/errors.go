package mcp

import "errors"

// ToolResultError MCP 工具执行了但业务失败（CallToolResult.IsError=true）。
// v0.11 起导出：包外消费方（经 agentkit/mcp 组装工具表的调用方）可 errors.As
// 穿透 %w 包装直达本类型取 Payload，无须对 Error() 文案做文本匹配——多错误
// 聚合文本的 marker 分类不可靠。Error() 文案是历史识别锚点（observe 兜底与
// 既有测试消费），改文案须同步；新代码优先 errors.As / IsToolResultError。
type ToolResultError struct {
	payload []byte // marshaled CallToolResult
}

func (e *ToolResultError) Error() string {
	return "failed to call mcp tool, mcp server return error: " + string(e.payload)
}

// Payload 原始 CallToolResult JSON（observe 层剥 content[].text 用）。
func (e *ToolResultError) Payload() []byte { return e.payload }

// IsToolResultError err 链上是否存在工具业务失败（isError:true）。
func IsToolResultError(err error) bool {
	var tre *ToolResultError
	return errors.As(err, &tre)
}

// asToolResultError 穿透 %w 包装取业务失败（isError:true）——包内便捷形态。
func asToolResultError(err error) (*ToolResultError, bool) {
	var tre *ToolResultError
	if errors.As(err, &tre) {
		return tre, true
	}
	return nil, false
}
