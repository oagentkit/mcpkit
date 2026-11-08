package mcp

// 渐进披露门禁契约测试（oagentkit/mcpkit#64）：三态语义（激活可重试/目录外
// 终止）+ 模型面 Guidance（内置模板与宿主覆写）+ 激活器「拒但顺手激活、重试
// 必成」的闭环。

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
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

// TestDisclosureGuidanceNoVerbVerbatim mcpkit#76 回归：无占位动词的宿主模板
// 原样输出——Guidance 是模型面文案，此前 fmt.Sprintf 把多余实参以
// %!(EXTRA string=…) 伪影追加进拒绝文本，宿主侧零信号。
func TestDisclosureGuidanceNoVerbVerbatim(t *testing.T) {
	g := NewDisclosureGate(newGateCatalog(), nil,
		WithDisclosureGuidance(DisclosureGuidance{
			Retryable: "工具未披露，请重试。",
			Terminal:  "工具不在目录中，请改用目录内工具。",
		}))
	if got := g.Check("write").Guidance; got != "工具未披露，请重试。" {
		t.Fatalf("无占位模板应原样输出（mcpkit#76）: %q", got)
	}
	if got := g.Check("drop").Guidance; got != "工具不在目录中，请改用目录内工具。" {
		t.Fatalf("无占位模板应原样输出（mcpkit#76）: %q", got)
	}
}

// TestDisclosureGuidanceMultiVerb 多占位模板：第一个动词吃工具名、后续动词
// 依次吃 server 名（与缺省 Terminal 模板「工具+server」双占位同约定）；
// %% 折叠为字面 %；任何模板形态都不产生 fmt 伪影。
func TestDisclosureGuidanceMultiVerb(t *testing.T) {
	g := NewDisclosureGate(newGateCatalog(), nil,
		WithDisclosureGuidance(DisclosureGuidance{
			Retryable: "%s 于 %s：已激活，请原样重试。",
			Terminal:  "100%% 不行：%s 在 %s 找不到，别再试 %s 了。",
		}))
	if got := g.Check("write").Guidance; got != "write 于 srv：已激活，请原样重试。" {
		t.Fatalf("双占位应依次注入工具名/server 名: %q", got)
	}
	want := "100% 不行：drop 在 srv 找不到，别再试 srv 了。"
	if got := g.Check("drop").Guidance; got != want {
		t.Fatalf("%% 转义与多占位异常: got %q want %q", got, want)
	}
}

// TestDisclosureGuidanceQuotedVerb %q 占位带引号注入（缺省模板同形态）。
func TestDisclosureGuidanceQuotedVerb(t *testing.T) {
	g := NewDisclosureGate(newGateCatalog(), nil,
		WithDisclosureGuidance(DisclosureGuidance{Terminal: "缺失：%q。"}))
	if got := g.Check("drop").Guidance; got != `缺失："drop"。` {
		t.Fatalf("%%q 应带引号注入: %q", got)
	}
}

// TestDisclosureSlowActivatorNoSerialize mcpkit#75 回归：激活器在门禁锁外
// 执行——慢激活进行中时，其他工具的 Check 与 Loaded() 不得被串行阻塞
// （此前 g.mu 全程持有，模型面门禁退化为热路径上的全局串行段）。
func TestDisclosureSlowActivatorNoSerialize(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	g := NewDisclosureGate(newGateCatalog(), nil,
		WithDisclosureActivator(func(name string) error {
			if name == "write" {
				close(entered)
				<-release // 慢激活：直到测试放行才返回
			}
			return nil
		}))

	leader := make(chan DisclosureVerdict, 1)
	go func() { leader <- g.Check("write") }()
	<-entered // 激活器已进入（此时旧实现正持 g.mu 阻塞所有后来者）

	probe := make(chan DisclosureVerdict, 1)
	go func() { probe <- g.Check("read") }() // 另一目录内工具：须即时判定+快激活
	snap := make(chan []string, 1)
	go func() { snap <- g.Loaded() }()

	for i := 0; i < 2; i++ {
		select {
		case <-probe:
		case <-snap:
		case <-time.After(2 * time.Second):
			t.Fatal("慢激活串行了其他 Check/Loaded（mcpkit#75）")
		}
	}

	close(release)
	if v := <-leader; !v.Retryable() || !v.Activated {
		t.Fatalf("leader 应拒但已激活: %+v", v)
	}
	if v := g.Check("write"); !v.Allowed() {
		t.Fatalf("激活完成后重试应放行: %+v", v)
	}
}

// TestDisclosureReentrantActivator mcpkit#75 回归：激活器回查门禁（观测面
// Loaded/Catalog）不得自死锁——此前激活器在 g.mu 临界区内被调用，sync.Mutex
// 不可重入，首个未披露工具的 Check 即确定性死锁。
func TestDisclosureReentrantActivator(t *testing.T) {
	var g *DisclosureGate // 先声明后赋值：激活器闭包回查 g 自身（激活器只在 Check 内触发，此时赋值早已完成）
	g = NewDisclosureGate(newGateCatalog(), nil,
		WithDisclosureActivator(func(name string) error {
			_ = g.Loaded()  // 回查已披露集
			_ = g.Catalog() // 回查目录
			return nil
		}))
	done := make(chan struct{})
	go func() {
		g.Check("write")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("激活器回查门禁不应自死锁（mcpkit#75）")
	}
	if got := g.Loaded(); len(got) != 1 || got[0] != "write" {
		t.Fatalf("激活应已入集: %v", got)
	}
}

// TestDisclosureActivationJoin 并发同工具激活的单飞语义回归：激活器一次激活
// 代恰好执行一次；等待者等的是进行中的激活（done），在 leader 成功后与旧
// 串行实现同观测——直接放行、不二次激活。
func TestDisclosureActivationJoin(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	g := NewDisclosureGate(newGateCatalog(), nil,
		WithDisclosureActivator(func(string) error {
			mu.Lock()
			calls++
			mu.Unlock()
			close(entered)
			<-release
			return nil
		}))

	leader := make(chan DisclosureVerdict, 1)
	go func() { leader <- g.Check("write") }()
	<-entered
	join := make(chan DisclosureVerdict, 1)
	go func() { join <- g.Check("write") }() // 加入进行中的激活：等 done 非等锁
	select {
	case v := <-join:
		t.Fatalf("等待者不得在激活完成前返回: %+v", v)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	lv := <-leader
	jv := <-join
	if !lv.Retryable() || !lv.Activated {
		t.Fatalf("leader 应拒但已激活: %+v", lv)
	}
	if !jv.Allowed() {
		t.Fatalf("等待者应在激活完成后放行: %+v", jv)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("激活器应恰好执行一次，实际 %d", calls)
	}
}
