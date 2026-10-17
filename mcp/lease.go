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
	"errors"
	"fmt"
	"github.com/oekit/concurrency/async"
	"time"

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
//   - legacy（≤2025-11-25）：协议 ping；
//   - modern（2026-07-28+）：改发真实轻量 RPC（tools/list）——比 ping 重，
//     但这是该代际下唯一能证明连接活着的信号。
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
		// 现代协议：ping 是 no-op，用 tools/list 作真实探活
		_, err = cli.ListTools(cctx, mcp.ListToolsRequest{})
	} else {
		err = cli.Ping(cctx)
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

// ReapIdle 关闭闲置超过 maxIdle 的缓存连接，返回回收数。闲置时钟 = 最近一次
// 建连/取工具/探活成功。被回收的 server 下次调用透明重建（evict+lazy 建连）。
// 在途工具调用（leaseUse 登记）中的连接不回收——原先契约要求 maxIdle 显著大于
// 最大工具超时，现在由在途计数直接挡下。
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
		// 经 notifyError 串行化：ReapIdle 与 Tools（跨 server 并行）并发直调
		// 用户 OnError 是数据竞争——errMu 契约见 pool.go（第六轮审计 C 级）
		p.notifyError(v.name, fmt.Errorf("mcp: 闲置超过 %s，连接已回收（下次调用重建）", maxIdle))
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

// StartIdleReaper 启动后台闲置回收协程：每 interval 周期执行 ReapIdle(maxIdle)。
// Close 停止；重复启动幂等（只起一个）。
func (p *Pool) StartIdleReaper(interval, maxIdle time.Duration) {
	p.mu.Lock()
	if p.reapStop != nil || p.closed {
		// 已在跑；或池已 Close——Close 后再启动的协程永无人停（泄漏），拒绝
		p.mu.Unlock()
		return
	}
	p.reapStop = make(chan struct{})
	stop := p.reapStop
	p.mu.Unlock()
	// async.GoSafe：回收协程静默 panic 死亡 = 闲置回收从此失效且无任何信号
	//（ekit 裸 go 收编纪律）
	async.GoSafe(func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				p.ReapIdle(maxIdle)
			}
		}
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
