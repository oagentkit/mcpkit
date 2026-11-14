// 连接租约面（批次五十三自 bianque 对标 ZCode pool 沉淀）：探活 + 闲置回收。
//
// 语义裁定——ZCode 原型是「引用计数归零 + 30s 惰性关闭」，本池不可照搬：工具对象
// （eino tool）持有连接且生命周期调用方不可见，refcount 无处挂钩。借用模型的等价
// 租约 = **闲置回收**（ReapIdle/StartIdleReaper）+ **调用期透明重建**（既有 evict
// + lazy 建连）。在途工具调用经 leaseUse 登记 inFlight，回收据此跳过在用连接
// （无须再要求 maxIdle 显著大于最大工具超时）。
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/oekit/concurrency/loop"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// ErrNotConnected PingServer 探活时该 server 无缓存连接——懒建连模型下属正常态
// （与「有连接但已死」区分，探活无意义不算失败）。
var ErrNotConnected = errors.New("mcp: server 无缓存连接（未建连或已被回收）")

// DefaultProbeTimeout 探活独立预算（批次五十六 B：ping 是轻量协议方法，不该吃
// 连接级 30s 预算；对标 ZCode MCP_PING_TIMEOUT_MS=5s）。
const DefaultProbeTimeout = 5 * time.Second

// PingServer 探活指定 server 的缓存连接。死亡连接立即摘除出缓存并关闭，下次
// 调用透明重建——HTTP server 被停掉不派发断连回调，「无声死亡」只有显式探活
// 才能暴露（ZCode pool 同款问题与解法）。
//
// 探活方法按建连协商的协议代际分流（v0.10.29 审计修复——mcp-go v1.1 起现代
// 协议 2026-07-28 移除了 ping RPC，Client.Ping 在该代际是 no-op，曾让探活
// 虚报存活且 touch 给死连接续命、闲置回收永远收不走）：
//   - legacy（≤2025-11-25）：协议 ping 证活；
//   - modern（2026-07-28+）：改发真实轻量 RPC（tools/list）——比 ping 重，
//     但这是该代际下唯一能证明连接活着的信号。
//
// 两代际在探活成功路径**同口径比对目录指纹**（oagentkit/mcpkit#100——此前
// legacy 只 ping 不比对，探活周期 < maxIdle 时工具面/schema 变化整个连接
// 生命周期不可见，与现代分支的 #53 语义不一致）：变化即失效缓存，下次
// catalogFor 重列全量转换。目录指纹比对需 tools/list，modern 分支本身即以
// tools/list 证活；legacy 分支 ping 证活后补一发 tools/list——该列举失败随
// 探活失败同路径处理（连接摘除、下次调用重建；本地取消甄别同款豁免）。
//
// 返回 ErrNotConnected = 无缓存连接（不算失败）；其余错误 = 探活失败（连接已摘除）。
func (p *Pool) PingServer(ctx context.Context, name string) error {
	p.mu.Lock()
	cfg := p.cfgs[name]
	st := p.conns[name]
	p.mu.Unlock()
	if st == nil {
		return ErrNotConnected
	}
	st.mu.Lock()
	cli := st.cli
	isModern := st.modern
	st.mu.Unlock()
	if cli == nil {
		return ErrNotConnected
	}

	budget := firstPositive(cfg.ProbeTimeout, DefaultProbeTimeout)
	cctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var err error
	if isModern {
		// 现代协议：ping 是 no-op，用 tools/list 作真实探活。**结果不丢弃**
		//（oagentkit/mcpkit#53）：探活周期 < maxIdle 时连接永不被回收、
		// catalogFor 缓存优先 ⇒ server 工具面/schema 变化永久不可见（与
		// ReapIdle 的「目录须随失效重列」理由矛盾）。探活成功路径比对目录
		// 指纹，变化即失效缓存（下次 catalogFor 重列全量转换）。
		var res *mcp.ListToolsResult
		res, err = cli.ListTools(cctx, mcp.ListToolsRequest{})
		if err == nil {
			p.refreshCatalogIfChanged(name, res.Tools)
		}
	} else {
		// legacy 协议：协议 ping 证活。ping 成功后同 modern 口径补 tools/list
		// 比对目录指纹（oagentkit/mcpkit#100）——此前只 ping 不比对，探活成功
		// 即 touch 续命，探活周期 < maxIdle 时连接永不回收、catalogFor 缓存
		// 优先，server 工具面变化整个连接生命周期不可见（现代分支注释里记载的
		// 同款失败模式，只在 modern 代际被修）。列举失败随探活失败同路径处理：
		// 本地取消豁免保留连接，其余摘除下次调用重建。
		err = cli.Ping(cctx)
		if err == nil {
			var res *mcp.ListToolsResult
			res, err = cli.ListTools(cctx, mcp.ListToolsRequest{})
			if err == nil {
				p.refreshCatalogIfChanged(name, res.Tools)
			}
		}
	}
	if err != nil {
		if cctx.Err() != nil {
			// 本地取消（调用方 ctx 已死/探活预算耗尽）≠ 连接死亡：探活预算派生
			// 自调用方 ctx，外部短周期巡检一旦自身过期，若据此摘连接，健康连接
			// 会被反复摘除（stdio 场景即子进程反复重启）。连接保留，如实报错
			//（第六轮审计——dial 路径早有 WithoutCancel 同款甄别，探活漏对齐）。
			return fmt.Errorf("mcp: %s 探活未完成（本地取消，连接保留）: %w", name, err)
		}
		p.evict(name, cli)
		return fmt.Errorf("mcp: %s 探活失败（连接已摘除，下次调用重建）: %w", name, err)
	}
	p.touch(name)
	return nil
}

// refreshCatalogIfChanged 探活侧目录缓存一致性（oagentkit/mcpkit#53）：
// 比对线上工具面指纹（数量+名集+schema 摘要）与缓存目录，变化即失效缓存
// ——下次 catalogFor 走全量列举（含 rawInput 双无损转换），server 工具面
// 变化在探活节奏内可见，不依赖闲置回收兜底。
func (p *Pool) refreshCatalogIfChanged(name string, tools []mcp.Tool) {
	st := p.connLockedOrNil(name)
	if st == nil {
		return
	}
	fp := probeFingerprint(tools)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.catalog == nil {
		return // 本就没有缓存，无须动作
	}
	if probeFingerprintOfEntries(st.catalog) != fp {
		st.catalog = nil
	}
}

// probeFingerprint 数量 + 名集 + schema 摘要（探活节奏的廉价一致性判据；
// 描述文本不参与——只有工具面/schema 变化才要求缓存失效）。
//
// 指纹锚定**类型化可见面**（t.InputSchema 的序列化）且两侧同源
// （oagentkit/mcpkit#66）：缓存侧的 raw 通道目录经 parseRawToolsList 以同口径
// 回填 InputSchema——同一份 wire JSON 双通道各自 Unmarshal 进同一结构，指纹必
// 相等；「探活」不是「重新拉目录」，比对基准是会话内已见目录的稳定表示，
// 同工具面不得触发缓存失效（否则探活周期驱动 tools/list 风暴，打破「连接时
// 一次列举」不变量）。
func probeFingerprint(tools []mcp.Tool) string {
	// per-tool 配对指纹（mcpkit#118）：names 与 schemas 此前**各自独立排序**
	// ——配对关系丢失，两工具 schema 互换时指纹不变（{a:X,b:Y} 与 {a:Y,b:X}
	// 同指纹 ⇒ 热更新不可见，缓存继续发旧 schema 绑定）。改为 name+\x00+
	// schema 成对拼串后整体排序；\x00 不会出现在合法工具名/schema JSON 里
	//（此前 "," join 可被含逗号名构造碰撞）。
	pairs := make([]string, len(tools))
	for i, t := range tools {
		b, _ := json.Marshal(t.InputSchema)
		pairs[i] = t.Name + "\x00" + string(b)
	}
	slices.Sort(pairs)
	return strconv.Itoa(len(tools)) + "|" + strings.Join(pairs, "|")
}

func probeFingerprintOfEntries(entries []toolEntry) string {
	tools := make([]mcp.Tool, len(entries))
	for i, e := range entries {
		tools[i] = e.tool
	}
	return probeFingerprint(tools)
}

// ReapIdle 关闭闲置超过 maxIdle 的缓存连接，返回回收数。闲置时钟 = 最近一次
// 建连/取工具/探活成功。被回收的 server 下次调用透明重建（evict+lazy 建连）。
// 在途工具调用（leaseUse 登记）中的连接不回收——原先契约要求 maxIdle 显著大于
// 最大工具超时，现在由在途计数直接挡下。
//
// 回收事件经 OnReap 发射（oagentkit/mcpkit#149）：回收是设计内行为（透明重建），
// 不是失败——此前借道 OnError，按失败率消费回调的宿主错误面被周期性回收灌满
// （StartIdleReaper 短 interval 时尤甚），真失败信号被淹没。
func (p *Pool) ReapIdle(maxIdle time.Duration) int {
	if maxIdle <= 0 {
		return 0
	}
	deadline := time.Now().Add(-maxIdle)
	p.mu.Lock()
	names := make([]string, 0, len(p.conns))
	states := make([]*connState, 0, len(p.conns))
	for name, st := range p.conns {
		names = append(names, name)
		states = append(states, st)
	}
	p.mu.Unlock()

	type victim struct {
		name string
		cli  client.MCPClient
	}
	var closing []victim
	for i, st := range states {
		st.mu.Lock()
		if st.cli == nil || st.inFlight > 0 || !st.lastUsed.Before(deadline) {
			st.mu.Unlock()
			continue
		}
		// 目录/代际一并失效（与 evict 同构）：否则回收重建后命中旧目录不重新
		// 列举，闲置期间 server 工具面/schema 变化永久不可见（v0.10.29）
		cli := st.detachLocked()
		st.mu.Unlock()
		closing = append(closing, victim{names[i], cli})
	}
	for _, v := range closing {
		_ = v.cli.Close()
		// 经 notifyReap 串行化：ReapIdle 与 Tools（跨 server 并行）并发直调
		// 用户回调是数据竞争——errMu 契约见 pool.go（第六轮审计 C 级）。
		// 走 OnReap 不走 OnError（oagentkit/mcpkit#149）：回收是设计内行为。
		p.notifyReap(v.name, maxIdle)
	}
	return len(closing)
}

// leaseUse 返回「进入调用」闭包：调用期 inFlight+1，返回的 end 负责 -1。
// 挂在 invokableTool 上，闲置回收据此跳过在用连接。
func (p *Pool) leaseUse(server string) func() func() {
	return func() func() {
		st := p.connLockedOrNil(server)
		if st == nil {
			return func() {}
		}
		st.mu.Lock()
		st.inFlight++
		st.mu.Unlock()
		return func() {
			st.mu.Lock()
			st.inFlight--
			if st.inFlight < 0 {
				st.inFlight = 0
			}
			// 调用结束视为一次使用：刷新闲置时钟（原先调用中不触碰，长调用
			// 结束即濒临回收）
			st.lastUsed = time.Now()
			st.mu.Unlock()
		}
	}
}

// StartIdleReaper 启动后台闲置回收循环：每 interval 周期执行 ReapIdle(maxIdle)。
// Close 停止；重复启动幂等（只起一个）。
//
// 收编 concurrency/loop（ctx 感知 + 忙时丢 tick + 单轮 panic 收编 + 停机排水），
// 循环挂 baseCtx——Close 收口 baseCtx 即停轮。旧手搓 ticker 版的外层 GoSafe
// 只把 panic 记进全局日志，单轮 panic 打死回收协程后闲置回收**静默失效且无
// 任何信号**；loop 的逐轮收编让循环存活（panic 轮跳过、下轮照常）。
func (p *Pool) StartIdleReaper(interval, maxIdle time.Duration) {
	p.mu.Lock()
	if p.reaping || p.closed {
		// 已在跑；或池已 Close——Close 后再启动的循环永无人停（泄漏），拒绝
		p.mu.Unlock()
		return
	}
	p.reaping = true
	p.mu.Unlock()
	loop.Run(p.baseCtx, loop.Config{
		Interval:   interval,
		FirstDelay: interval, // 首跑对齐旧 ticker 节拍（首个 interval 后才首轮回收）
		Name:       "mcp.idle_reaper",
	}, func(context.Context) {
		p.ReapIdle(maxIdle)
	})
}

// touch 记录 server 连接最近使用时刻。
func (p *Pool) touch(name string) {
	st := p.conn(name)
	if st == nil {
		return
	}
	st.mu.Lock()
	st.lastUsed = time.Now()
	st.mu.Unlock()
}
