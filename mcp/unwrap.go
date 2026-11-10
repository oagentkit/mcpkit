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
	if text, ok := envelopeTextOf(raw); ok {
		return text
	}
	return raw
}

// envelopeTextOf 解 MCP CallToolResult 信封取内层文本（**全仓单源**——
// UnwrapMCPText 与 observe 层的 observationFromPayload 共用，mcpkit#111：
// 此前错误路径手写「取 Content[0].Text」，首块是 image/audio/resource 时
// 空壳观察，与成功路径口径分裂）。三级回落：
//
//  1. content[] 里**逐块**找 type=text 且文本非空的块（不看首块类型）；
//  2. structuredContent 非空时序列化返回；
//  3. 信封解析成功但无可提取（image/audio-only 等）：紧凑占位——回吐完整
//     信封会把 base64 图像（数百 KB~MB）当「内层文本」返回，与「snippet
//     配额留给有效数据」的目标正相反（第十轮审计）。
//
// ok=false 表示非信封形态（JSON 解析失败 / content 与 structuredContent
// 双空）——调用方按自己的兜底语义处置（原样返回或带前缀回吐）。
func envelopeTextOf(raw string) (string, bool) {
	var env struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return "", false
	}
	for _, c := range env.Content {
		if c.Type == "text" && c.Text != "" {
			return c.Text, true
		}
	}
	if len(env.StructuredContent) > 0 && string(env.StructuredContent) != "null" {
		return string(env.StructuredContent), true
	}
	if len(env.Content) > 0 {
		return fmt.Sprintf("[mcp tool result: %d content block(s), no text payload]", len(env.Content)), true
	}
	return "", false
}
