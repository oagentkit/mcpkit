// 工具出参 schema 能力面（v0.12.5，MCP 2025-06-18 outputSchema）：目录层经原始
// tools/list 无损透传（rawlist.go），invokableTool 以可选接口暴露——eino
// tool.BaseTool 为封闭接口，schema 进不了 ToolInfo，故走类型断言。断言失败=该
// 工具无声明（typed 回退/第三方未声明），消费方按「未声明」处理。
package mcp

import (
	"encoding/json"

	"github.com/cloudwego/eino/components/tool"
)

// OutputSchemer 工具出参 schema 的可选能力接口。
type OutputSchemer interface {
	OutputSchema() json.RawMessage
}

// OutputSchemaOf 取工具的声明 outputSchema（尽力：装饰器链各自转发；断言失败或
// 未声明返回 false）。
func OutputSchemaOf(t tool.BaseTool) (json.RawMessage, bool) {
	os, ok := t.(OutputSchemer)
	if !ok {
		return nil, false
	}
	raw := os.OutputSchema()
	return raw, len(raw) > 0
}
