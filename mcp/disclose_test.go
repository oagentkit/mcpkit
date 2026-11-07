package mcp

// 渐进披露门禁契约测试（oagentkit/mcpkit#64）：三态语义（激活可重试/目录外
// 终止）+ 模型面 Guidance（内置模板与宿主覆写）+ 激活器「拒但顺手激活、重试
// 必成」的闭环。

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

func newGateCatalog() Catalog {
	return NewCatalog("srv", []string{"search", "write", "read"})
}

// TestDisclosureThreeStates 三态主路径：已披露放行；目录内未披露拒但可重试
// （带激活）；目录外拒且不可重试。
func TestDisclosureThreeStates(t *testing.T) {
	var activated []string
	g := NewDisclosureGate(newGateCatalog(), []string{"search"},
		WithDisclosureActivator(func(name string) error {
			activated = append(activated, name)
			return nil
		}))

	// 已披露 → 放行，无文案
	v := g.Check("search")
	if !v.Allowed() || v.State != DisclosureAllowed || v.Guidance != "" {
		t.Fatalf("已披露应放行: %+v", v)
	}

	// 目录内未披露 → 拒绝但可重试 + 顺手激活
	v = g.Check("write")
	if v.Allowed() || !v.Retryable() || v.State != DisclosureRetryableDenied {
		t.Fatalf("目录内未披露应拒但可重试: %+v", v)
	}
	if !v.Activated || len(activated) != 1 || activated[0] != "write" {
		t.Fatalf("应顺手激活 write: activated=%v verdict=%+v", activated, v)
	}
	if !strings.Contains(v.Guidance, "write") || !strings.Contains(v.Guidance, "重试") {
		t.Fatalf("可重试文案应点名工具并指示重试: %q", v.Guidance)
	}
	// 激活后重试必成：同名再次判定放行
	if v := g.Check("write"); !v.Allowed() {
		t.Fatalf("激活后重试应放行: %+v", v)
	}

	// 目录外 → 拒绝且不可重试，文案指示换工具
	v = g.Check("drop")
	if v.Allowed() || v.Retryable() || v.State != DisclosureTerminalDenied {
		t.Fatalf("目录外应拒且不可重试: %+v", v)
	}
	if v.Activated || len(activated) != 1 {
		t.Fatalf("目录外不得触发激活: activated=%v", activated)
	}
	if !strings.Contains(v.Guidance, "drop") || !strings.Contains(v.Guidance, "srv") {
		t.Fatalf("终止文案应点名工具与 server: %q", v.Guidance)
	}
}

// TestDisclosureNoActivator 未配置激活器：可重试语义照给（宿主自管激活），
// 门禁不代激活、不改已披露集。
func TestDisclosureNoActivator(t *testing.T) {
	g := NewDisclosureGate(newGateCatalog(), []string{"search"})
	v := g.Check("write")
	if !v.Retryable() || v.Activated {
		t.Fatalf("无激活器应可重试但不代激活: %+v", v)
	}
	if v := g.Check("write"); v.Allowed() {
		t.Fatal("未激活不得放行")
	}
	if got := g.Loaded(); len(got) != 1 || got[0] != "search" {
		t.Fatalf("已披露集不应被改动: %v", got)
	}
}

// TestDisclosureActivatorError 激活器失败：目录事实不变（仍判可重试），Err
// 上抛给处置层告警；不入已披露集（重试不得虚假放行）。
func TestDisclosureActivatorError(t *testing.T) {
	boom := errors.New("reload refused")
	g := NewDisclosureGate(newGateCatalog(), nil,
		WithDisclosureActivator(func(string) error { return boom }))
	v := g.Check("write")
	if !v.Retryable() || v.Activated || !errors.Is(v.Err, boom) {
		t.Fatalf("激活失败应保可重态并上抛 Err: %+v", v)
	}
	if v := g.Check("write"); v.Allowed() {
		t.Fatal("激活失败的调用不得进入已披露集")
	}
}

// TestDisclosureGuidanceOverride 文案覆写：宿主模板优先，占位符注入生效。
func TestDisclosureGuidanceOverride(t *testing.T) {
	g := NewDisclosureGate(newGateCatalog(), nil,
		WithDisclosureGuidance(DisclosureGuidance{
			Retryable: "WAIT:%s",
			Terminal:  "STOP:%s",
		}))
	if got := g.Check("write").Guidance; got != "WAIT:write" {
		t.Fatalf("覆写可重试文案未生效: %q", got)
	}
	if got := g.Check("drop").Guidance; got != "STOP:drop" {
		t.Fatalf("覆写终止文案未生效: %q", got)
	}
}

// TestDisclosureEmptyCatalog 空目录：一切判定都是目录外终止（没有任何工具可给）。
func TestDisclosureEmptyCatalog(t *testing.T) {
	g := NewDisclosureGate(NewCatalog("srv", nil), nil)
	if v := g.Check("any"); v.Allowed() || v.Retryable() {
		t.Fatalf("空目录应一律终止拒绝: %+v", v)
	}
}

// TestDisclosureConcurrentCheck 并发安全：并发 Check（含激活路径）不竞态、
// 激活器恰好调用一次（顺手激活的幂等由已披露集保证）。
func TestDisclosureConcurrentCheck(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	g := NewDisclosureGate(newGateCatalog(), []string{"search"},
		WithDisclosureActivator(func(name string) error {
			mu.Lock()
			calls[name]++
			mu.Unlock()
			return nil
		}))
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { // -race 回归：loaded 集的读改写须在锁内
			defer wg.Done()
			g.Check("write")
		}()
	}
	wg.Wait()
	if got := g.Loaded(); len(got) != 2 {
		t.Fatalf("并发激活后已披露集应含 search+write: %v", got)
	}
	// 激活器对 write 恰好一次：首个成功者入集，后来者直接放行不再激活
	mu.Lock()
	n := calls["write"]
	mu.Unlock()
	if n != 1 {
		t.Fatalf("激活器应恰好调用一次，实际 %d", n)
	}
}
