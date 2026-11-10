package mcp

// 2026-11-11 issue 清零战回归测试（#109-#114）：每条用例钉一个失效形态，
// 标题带 issue 号——回归时按号检索。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// TestSelfcheckCacheKeyIncludesDeclaration_Issue109_113 TTL 缓存键必须含声明面
// 指纹——不同 Expected / ProbeTools 的调用方不得互相串味还带 Cached=true 背书。
func TestSelfcheckCacheKeyIncludesDeclaration_Issue109_113(t *testing.T) {
	cli := &scriptedClient{tools: []string{"read"}}
	sc := NewSelfchecker()
	ctx := context.Background()

	first := sc.Check(ctx, cli, SelfcheckOptions{
		Server: "srv", TTL: time.Minute,
		Expected: NewCatalog("srv", []string{"read"}),
	})
	if first.Cached {
		t.Fatal("首次不应是缓存")
	}
	// 同 server、不同 Expected：必须现算（缺失面是 B 的，不是 A 的快照）。
	second := sc.Check(ctx, cli, SelfcheckOptions{
		Server: "srv", TTL: time.Minute,
		Expected: NewCatalog("srv", []string{"write"}),
	})
	if second.Cached {
		t.Fatal("不同 Expected 不得命中同一缓存条目（mcpkit#109/#113）")
	}
	if cli.listCount != 2 {
		t.Fatalf("不同 Expected 应实拉目录，实拉 %d 次", cli.listCount)
	}
	foundMissing := false
	for _, tc := range second.Tools {
		if tc.Tool == "write" && tc.Status == ToolMissing {
			foundMissing = true
		}
	}
	if !foundMissing {
		t.Fatalf("Expected=write 应判 missing，实际 %+v", second.Tools)
	}
	// 同 Expected 复查仍享受 TTL。
	third := sc.Check(ctx, cli, SelfcheckOptions{
		Server: "srv", TTL: time.Minute,
		Expected: NewCatalog("srv", []string{"write"}),
	})
	if !third.Cached {
		t.Fatal("同声明面 TTL 内应命中缓存")
	}
	// ProbeTools 翻转也换键：探过与没探过的结论不可混用。
	fourth := sc.Check(ctx, cli, SelfcheckOptions{
		Server: "srv", TTL: time.Minute,
		Expected:  NewCatalog("srv", []string{"write"}),
		ProbeTools: true,
	})
	if fourth.Cached {
		t.Fatal("ProbeTools 翻转不得命中缓存")
	}
}

// TestProbeToolPayloadIsEnvelope_Issue110_112 探针 isError 的 payload 必须是
// 完整 CallToolResult 信封——与正常路径同形态（同名类型三种 payload 形态
// = 至少一处消费错位）。
func TestProbeToolPayloadIsEnvelope_Issue110_112(t *testing.T) {
	cli := &scriptedClient{
		tools: []string{"broken"},
		callResults: map[string]*mcp.CallToolResult{
			"broken": {IsError: true, Content: []mcp.Content{
				mcp.TextContent{Type: "text", Text: "boom detail"},
			}},
		},
	}
	sc := NewSelfchecker()
	res := sc.Check(context.Background(), cli, SelfcheckOptions{
		Server: "srv", ProbeTools: true,
	})
	var failed *ToolCheck
	for i := range res.Tools {
		if res.Tools[i].Tool == "broken" && res.Tools[i].Status == ToolFailed {
			failed = &res.Tools[i]
		}
	}
	if failed == nil {
		t.Fatalf("broken 应判 ToolFailed，实际 %+v", res.Tools)
	}
	var tre *ToolResultError
	if !errors.As(failed.Err, &tre) {
		t.Fatalf("失败应可 errors.As 到 ToolResultError，实际 %T", failed.Err)
	}
	var env struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(tre.Payload(), &env); err != nil {
		t.Fatalf("payload 应为信封 JSON: %v（payload=%s）", err, tre.Payload())
	}
	if !env.IsError || len(env.Content) == 0 || env.Content[0].Type != "text" || env.Content[0].Text != "boom detail" {
		t.Fatalf("信封内容不符: %+v", env)
	}
}

// TestObservationFromPayloadSkipsNonTextFirstBlock_Issue111 错误观察剥壳与
// unwrap.go 同口径：逐块找 text、回落 structuredContent、无可提取给占位
// ——不再「剥首块的 text 字段」出空壳观察。
func TestObservationFromPayloadSkipsNonTextFirstBlock_Issue111(t *testing.T) {
	// 首块 image + 次块 text：取到的是次块文本，不是空串。
	envImg := `{"content":[{"type":"image","data":"x"},{"type":"text","text":"真实错误"}],"isError":true}`
	if got := observationFromPayload([]byte(envImg)); !strings.Contains(got, "真实错误") {
		t.Fatalf("应剥出次块文本，实际 %q", got)
	}
	// structuredContent-only 错误。
	envStruct := `{"structuredContent":{"code":42,"msg":"参数缺 name"}}`
	if got := observationFromPayload([]byte(envStruct)); !strings.Contains(got, "参数缺 name") {
		t.Fatalf("应回落 structuredContent，实际 %q", got)
	}
	// image-only：占位符而非空壳。
	envImgOnly := `{"content":[{"type":"image","data":"x"}],"isError":true}`
	got := observationFromPayload([]byte(envImgOnly))
	if strings.HasSuffix(got, "：\n") || !strings.Contains(got, "1 content block(s)") {
		t.Fatalf("无可提取应给占位符，实际 %q", got)
	}
	// 非 JSON（裸文本兜底形态）：原样回吐。
	if got := observationFromPayload([]byte("plain text failure")); !strings.Contains(got, "plain text failure") {
		t.Fatalf("非信封应原样回吐，实际 %q", got)
	}
}

// TestPoolToolsRejectsEmptyServerRef_Issue114 spec.Server=="" 的未配置引用
// 必须走「未配置的 server」显式报错——空串不得兼作「无缺失」哨兵。
func TestPoolToolsRejectsEmptyServerRef_Issue114(t *testing.T) {
	p := NewPool(ServerConfig{Name: "calc", URL: "http://127.0.0.1:1/mcp"})
	_, err := p.Tools(context.Background(), []ToolSpec{{Server: "", Allow: []string{"x"}}})
	if err == nil {
		t.Fatal("空名引用应显式报错（mcpkit#114）")
	}
	if !strings.Contains(err.Error(), `server ""`) {
		t.Fatalf("错误应指认空名引用，实际 %v", err)
	}
	// 正常未配置引用的既有报错不受影响。
	_, err = p.Tools(context.Background(), []ToolSpec{{Server: "nope", Allow: []string{"x"}}})
	if err == nil || !strings.Contains(err.Error(), `"nope"`) {
		t.Fatalf("未配置引用报错漂移: %v", err)
	}
}
