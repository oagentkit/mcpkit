package mcp

import (
	"encoding/json"
	"fmt"
)

// UnwrapMCPText 解 MCP 工具返回的信封取内层文本；非信封形态原样返回——快照/
// 结果进证据条目时，snippet 配额应留给有效数据而非包装层。
// 信封两种形态：content[].text（标准工具结果，人类可读文本优先）；缺失时
// structuredContent（较新 MCP 规范的结构化结果）序列化为 JSON——直接把结构化
// 对象丢给 LLM 优于丢弃。
func UnwrapMCPText(raw string) string {
	var env struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err == nil {
		for _, c := range env.Content {
			if c.Type == "text" && c.Text != "" {
				return c.Text
			}
		}
		if len(env.StructuredContent) > 0 && string(env.StructuredContent) != "null" {
			return string(env.StructuredContent)
		}
		// 信封解析成功但无可提取 payload（image/audio-only 等）：返回紧凑占位
		// ——回吐完整信封会把 base64 图像（数百 KB~MB）当「内层文本」返回，
		// 与「snippet 配额留给有效数据」的目标正相反（第十轮审计）
		if len(env.Content) > 0 {
			return fmt.Sprintf("[mcp tool result: %d content block(s), no text payload]", len(env.Content))
		}
	}
	return raw
}
