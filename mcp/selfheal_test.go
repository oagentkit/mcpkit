package mcp

// 调用期自愈单测（批次五十）：分类器/透传/自愈重试/自愈失败上抛。

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// scriptedTool 脚本化工具（可编程返回序列）。
type scriptedTool struct {
	name  string
	calls []error
}

func (t *scriptedTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: t.name}, nil
}

func (t *scriptedTool) InvokableRun(_ context.Context, _ string, _ ...tool.Option) (string, error) {
	if len(t.calls) == 0 {
		return "exhausted", nil
	}
	err := t.calls[0]
	t.calls = t.calls[1:]
	if err != nil {
		return "", err
	}
	return "ok", nil
}

func TestIsTransportDead(t *testing.T) {
	cases := map[string]bool{
		"not connected":                    true,
		"client is not initialized":        true,
		"connection closed":                true,
		"write |1: broken pipe":            true,
		"use of closed network connection": true,
		"mcp server return error":          false, // 业务失败不可自愈
		"invalid arguments":                false,
		"":                                 false,
	}
	for msg, want := range cases {
		if got := isTransportDead(errors.New(msg)); got != want {
			t.Fatalf("isTransportDead(%q) = %v, want %v", msg, got, want)
		}
	}
	if !isTransportDead(io.EOF) {
		t.Fatal("io.EOF 应判传输死亡")
	}
}

func TestSelfHealPassesThroughBusinessError(t *testing.T) {
	inner := &scriptedTool{name: "calc", calls: []error{errors.New("mcp server return error: boom")}}
	redialed := false
	sut := &selfHealTool{name: "calc", inner: inner, redial: func(context.Context, client.MCPClient) (tool.InvokableTool, error) {
		redialed = true
		return nil, errors.New("should not redial")
	}}
	_, err := sut.InvokableRun(context.Background(), "{}")
	if err == nil || redialed {
		t.Fatalf("业务错误应原样上抛且不自愈: err=%v redialed=%v", err, redialed)
	}
}

func TestSelfHealRetryOnTransportDead(t *testing.T) {
	inner := &scriptedTool{name: "calc", calls: []error{errors.New("not connected")}}
	fresh := &scriptedTool{name: "calc", calls: []error{nil}}
	evicted := false
	sut := &selfHealTool{name: "calc", inner: inner, redial: func(ctx context.Context, _ client.MCPClient) (tool.InvokableTool, error) {
		if evicted {
			return fresh, nil
		}
		evicted = true
		return fresh, nil
	}}
	out, err := sut.InvokableRun(context.Background(), "{}")
	if err != nil || out != "ok" {
		t.Fatalf("自愈重试应成功: out=%q err=%v", out, err)
	}
}

func TestSelfHealRedialFailureSurfaces(t *testing.T) {
	inner := &scriptedTool{name: "calc", calls: []error{errors.New("not connected")}}
	sut := &selfHealTool{name: "calc", inner: inner, redial: func(context.Context, client.MCPClient) (tool.InvokableTool, error) {
		return nil, errors.New("reconnect refused")
	}}
	_, err := sut.InvokableRun(context.Background(), "{}")
	if err == nil || !strings.Contains(err.Error(), "自愈重连失败") {
		t.Fatalf("重连失败应上抛: %v", err)
	}
}

// countingDeadTool 恒死的工具：统计被调用次数（#62 回归锚点——自愈成功后
// 不回写 inner 时，此后每次调用都会先撞一次这个死工具）。
type countingDeadTool struct {
	name  string
	calls int
}

func (t *countingDeadTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: t.name}, nil
}

func (t *countingDeadTool) InvokableRun(_ context.Context, _ string, _ ...tool.Option) (string, error) {
	t.calls++
	return "", errors.New("not connected") // 恒死：传输层错误
}

// TestSelfHealSwapsInnerAfterRedial 回归（oagentkit/mcpkit#62）：自愈成功后必须
// 回写 inner——此前实现从不替换，此后每次调用都先撞一次死连接（多一轮注定失败
// 的调用 + 一次 redial + 一次目录装配）再重试。断言：首调自愈成功后，后续调用
// 直达新工具，死工具调用计数不再增长。
func TestSelfHealSwapsInnerAfterRedial(t *testing.T) {
	dead := &countingDeadTool{name: "calc"}
	// fresh 恒成功：脚本给足 nil（首调重试 1 次 + 后续直达 3 次）
	fresh := &scriptedTool{name: "calc", calls: []error{nil, nil, nil, nil}}
	redials := 0
	sut := &selfHealTool{name: "calc", inner: dead, redial: func(context.Context, client.MCPClient) (tool.InvokableTool, error) {
		redials++
		return fresh, nil
	}}

	// 首调：撞死连接 → redial → fresh 重试成功（dead.calls=1, redials=1）
	out, err := sut.InvokableRun(context.Background(), "{}")
	if err != nil || out != "ok" {
		t.Fatalf("首调自愈应成功: out=%q err=%v", out, err)
	}
	// 后续多次调用：全部直达 fresh——死连接不再被撞、不再 redial
	for i := 0; i < 3; i++ {
		out, err := sut.InvokableRun(context.Background(), "{}")
		if err != nil || out != "ok" {
			t.Fatalf("第 %d 次调用应直达新工具: out=%q err=%v", i+2, out, err)
		}
	}
	if dead.calls != 1 {
		t.Fatalf("自愈成功后死工具不应再被调用，实际调用 %d 次", dead.calls)
	}
	if redials != 1 {
		t.Fatalf("自愈成功后不应再 redial，实际 %d 次", redials)
	}
	// 回写后读取面随新工具走：Info 直达 fresh
	info, err := sut.Info(context.Background())
	if err != nil || info.Name != "calc" {
		t.Fatalf("回写后 Info 异常: %+v %v", info, err)
	}
}

// TestSelfHealKeepsOldInnerWhenRetryFails 重试失败不回写：下次调用仍从旧 inner
// 触发自愈（半换装状态不得进入 inner）。
func TestSelfHealKeepsOldInnerWhenRetryFails(t *testing.T) {
	dead := &countingDeadTool{name: "calc"}
	alsoDead := &countingDeadTool{name: "calc"} // redial 出的新工具也死
	sut := &selfHealTool{name: "calc", inner: dead, redial: func(context.Context, client.MCPClient) (tool.InvokableTool, error) {
		return alsoDead, nil
	}}
	if _, err := sut.InvokableRun(context.Background(), "{}"); err == nil {
		t.Fatal("重试仍失败应上抛")
	}
	if alsoDead.calls != 1 {
		t.Fatalf("新工具应被重试一次，实际 %d 次", alsoDead.calls)
	}
	// 再调一次：inner 仍是 dead（未回写），触发新一轮自愈
	if _, err := sut.InvokableRun(context.Background(), "{}"); err == nil {
		t.Fatal("第二轮仍应失败")
	}
	if dead.calls != 2 || alsoDead.calls != 2 {
		t.Fatalf("未回写时应各调用: dead=%d alsoDead=%d", dead.calls, alsoDead.calls)
	}
}

// ---------------------------------------------------------------- 自愈重连的代际解析（oagentkit/mcpkit#88）

// TestSelfHealRedialEvictDecision 生产侧 selfHealRedial 的 evict 实例比对决策
// （真 Pool 状态 + 真闭包，仅 client 假体）：dead 由 selfHealTool 在 redial 时刻
// 传参（从当前 inner 解析），闭包不再固化装配代连接。
//
//   - 当前代死连接必须被摘——两连断的第二代（#88 回归核心：旧实现闭包里凝固
//     装配代 dead，第二代 cached(新代) != dead(装配代) 恒跳过 evict，死连接
//     永驻缓存、自愈恒败）；
//   - 并发自愈保护不回退：缓存已是别的自愈刚换上的新连接时不得误摘（#50 契约），
//     且 fast path 命中后 fresh 绑定缓存里的当前连接；
//   - dead 为 nil（测试桩无连接暴露面）时跳过比对直接走建连，不误摘。
func TestSelfHealRedialEvictDecision(t *testing.T) {
	cfg := ServerConfig{Name: "srv"}
	spec := ToolSpec{Server: "srv", AllowAll: true}
	entries := []toolEntry{{tool: mcp.Tool{Name: "calc"}}}

	t.Run("第二代死连接必须被摘", func(t *testing.T) {
		p := NewPool(cfg)
		defer p.Close()
		gen1 := &scriptedClient{tools: []string{"calc"}}
		seedFake(p, "srv", gen1)
		setConn(p, "srv", func(st *connState) { st.catalog = entries })
		redial := p.selfHealRedial(cfg, spec, "srv.calc")

		// dead=gen1（redial 时刻从当前 inner 解析到的装配代）：cached==dead →
		// evict 摘除 gen1 → 缓存空、无建连替身 → 建连失败上抛（摘除已先行完成）
		if _, err := redial(context.Background(), gen1); err == nil {
			t.Fatal("evict 后无建连替身应失败上抛")
		}
		if gen1.closes != 1 {
			t.Fatalf("第二代死连接应被 evict 关闭，实际 closes=%d（#88：跳过 evict 即死连接永驻缓存）", gen1.closes)
		}
	})

	t.Run("并发自愈保护：缓存已是新连接时不得误摘", func(t *testing.T) {
		p := NewPool(cfg)
		defer p.Close()
		fresh := &scriptedClient{tools: []string{"calc"}} // 并发自愈刚换上的新连接
		seedFake(p, "srv", fresh)
		setConn(p, "srv", func(st *connState) { st.catalog = entries })
		dead := &scriptedClient{} // 本工具失败的那条（已被并发自愈先行摘除）
		redial := p.selfHealRedial(cfg, spec, "srv.calc")

		got, err := redial(context.Background(), dead)
		if err != nil {
			t.Fatalf("fast path 命中应装配成功: %v", err)
		}
		if fresh.closes != 0 {
			t.Fatalf("不得摘除并发自愈刚建好的新连接（#50 契约）")
		}
		if dead.closes != 0 {
			t.Fatalf("比对不中只跳过 evict，不负责关闭——死连接由先行 evict 的赢家关闭，实际 closes=%d", dead.closes)
		}
		c, ok := got.(mcpClienter)
		if !ok || c.mcpClient() != fresh {
			t.Fatalf("redial 结果应绑定缓存中的当前连接")
		}
	})

	t.Run("nil dead 跳过比对直接走建连", func(t *testing.T) {
		p := NewPool(cfg)
		defer p.Close()
		cached := &scriptedClient{tools: []string{"calc"}}
		seedFake(p, "srv", cached)
		setConn(p, "srv", func(st *connState) { st.catalog = entries })
		redial := p.selfHealRedial(cfg, spec, "srv.calc")

		got, err := redial(context.Background(), nil)
		if err != nil {
			t.Fatalf("nil dead 应走 fast path: %v", err)
		}
		if cached.closes != 0 {
			t.Fatalf("nil dead 不得触发 evict")
		}
		if c, ok := got.(mcpClienter); !ok || c.mcpClient() != cached {
			t.Fatalf("fast path 应返回缓存连接装配的工具")
		}
	})
}

// TestSelfHealRecoversAcrossTwoDisconnects #88 端到端回归（两连断）：真 Pool
// 装配链（Tools → wrapSelfHeal → selfHealRedial → evict → 建连单飞 → 目录重列举
// → 同名解析 → 重试回写），仅 dial 以替身注入（三代编排型假体，免真实子进程/网络）。
//
// 旧实现失败形态：redial 闭包固化装配代 dead——第一次断连自愈成功换代后，第二次
// 断连 cached(二代) != dead(一代) 恒跳过 evict，client() 快路径缓存命中返回死连接，
// 装配出的 fresh 也绑死连接 → 重试恒败、不再换代，该 server 全部工具永久瘫痪。
func TestSelfHealRecoversAcrossTwoDisconnects(t *testing.T) {
	gens := []*scriptedClient{
		{tools: []string{"calc"}, callErrs: map[string]error{"calc": errors.New("not connected")}}, // 装配代：已死
		{tools: []string{"calc"}}, // 第一代重建
		{tools: []string{"calc"}}, // 第二代重建
	}
	dials := 0
	p := NewPool(ServerConfig{Name: "srv", URL: "http://fake.invalid/mcp"}) // URL 仅为过 Tools 前置校验；dial 走替身不触网
	p.dialFn = func(context.Context, ServerConfig) (client.MCPClient, transport.Interface, error) {
		dials++
		g := gens[dials-1]
		if g == nil {
			return nil, nil, errors.New("替身连接已耗尽")
		}
		return g, nil, nil
	}
	defer p.Close()

	tr, err := p.Tools(context.Background(), []ToolSpec{{Server: "srv", AllowAll: true}})
	if err != nil || len(tr.Tools) != 1 {
		t.Fatalf("装配失败: err=%v tools=%d", err, len(tr.Tools))
	}
	it, ok := tr.Tools[0].(tool.InvokableTool)
	if !ok {
		t.Fatal("最外层应为 InvokableTool（自愈包装）")
	}

	// 第一连断：装配代死 → 自愈（摘 gen0 → 拨 gen1 → 重试成功 → 换代）
	out, err := it.InvokableRun(context.Background(), "{}")
	if err != nil || !strings.Contains(out, "ok") {
		t.Fatalf("第一连断自愈应成功: out=%q err=%v", out, err)
	}
	if gens[0].closes != 1 || gens[1].closes != 0 || dials != 2 {
		t.Fatalf("第一连断后状态异常: closes=%d/%d dials=%d", gens[0].closes, gens[1].closes, dials)
	}

	// 第二连断（#88 核心）：gen1 死 → redial 传参必须解析到当前代 gen1 并摘除
	// → 拨 gen2 → 重试成功。旧实现此处恒败（比对基准凝固在 gen0）。
	gens[1].callErrs = map[string]error{"calc": errors.New("not connected")}
	out, err = it.InvokableRun(context.Background(), "{}")
	if err != nil || !strings.Contains(out, "ok") {
		t.Fatalf("第二连断自愈应成功（#88）: out=%q err=%v", out, err)
	}
	if gens[0].closes != 1 || gens[1].closes != 1 || gens[2].closes != 0 || dials != 3 {
		t.Fatalf("第二连断后状态异常: closes=%d/%d/%d dials=%d",
			gens[0].closes, gens[1].closes, gens[2].closes, dials)
	}

	// 恢复后直达新连接：不再拨号、不再触发自愈
	out, err = it.InvokableRun(context.Background(), "{}")
	if err != nil || !strings.Contains(out, "ok") || dials != 3 {
		t.Fatalf("恢复后应直达新连接: out=%q err=%v dials=%d", out, err, dials)
	}
}
