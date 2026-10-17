// 连接租约面测试（批次五十三沉淀）：PingServer 探活/摘除/无连接哨兵 + 闲置回收。
// 假体只实现用到的接口面（MCPClient 大接口，embedding 兜底未用方法）。
package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
)

// fakeClient MCPClient 假体：Ping 可编排死活。
type fakeClient struct {
	client.MCPClient // embedding：未实现方法 panic 即「测试不应触达」
	dead             bool
	closed           bool
}

func (f *fakeClient) Ping(ctx context.Context) error {
	if f.dead {
		return errors.New("connection refused")
	}
	return nil
}

func (f *fakeClient) Close() error {
	f.closed = true
	return nil
}

// seedFake 直塞缓存（绕过真实 dial）。
func seedFake(p *Pool, name string, cli client.MCPClient) {
	p.mu.Lock()
	p.cfgs[name] = ServerConfig{Name: name}
	st := p.connLocked(name)
	p.mu.Unlock()
	st.mu.Lock()
	st.cli = cli
	st.lastUsed = time.Now()
	st.mu.Unlock()
}

func TestPingServerAliveAndDead(t *testing.T) {
	p := NewPool()
	alive := &fakeClient{}
	seedFake(p, "ok", alive)
	if err := p.PingServer(context.Background(), "ok"); err != nil {
		t.Fatalf("存活探活不应失败: %v", err)
	}
	if alive.closed {
		t.Fatal("存活连接不应被关闭")
	}

	dead := &fakeClient{dead: true}
	seedFake(p, "dead", dead)
	if err := p.PingServer(context.Background(), "dead"); err == nil {
		t.Fatal("死亡探活应报错")
	}
	if !dead.closed {
		t.Fatal("死亡连接应被摘除关闭")
	}
	still := hasConn(p, "dead")
	if still {
		t.Fatal("死亡连接应出缓存")
	}
}

func TestPingServerNotConnected(t *testing.T) {
	p := NewPool(ServerConfig{Name: "ghost", URL: "http://127.0.0.1:1/mcp"})
	err := p.PingServer(context.Background(), "ghost")
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("无缓存连接应返回哨兵: %v", err)
	}
}

func TestReapIdle(t *testing.T) {
	p := NewPool()
	old := &fakeClient{}
	fresh := &fakeClient{}
	seedFake(p, "old", old)
	seedFake(p, "fresh", fresh)
	setConn(p, "old", func(st *connState) { st.lastUsed = time.Now().Add(-10 * time.Minute) })

	n := p.ReapIdle(time.Minute)
	if n != 1 {
		t.Fatalf("应回收 1 条: %d", n)
	}
	if !old.closed {
		t.Fatal("闲置连接应被关闭")
	}
	if fresh.closed {
		t.Fatal("新鲜连接不应被回收")
	}
	oldIn, freshIn := hasConn(p, "old"), hasConn(p, "fresh")
	if oldIn || !freshIn {
		t.Fatal("缓存状态与回收结果不一致")
	}
	// 无新增闲置：再跑回收 0 条。
	if n := p.ReapIdle(time.Minute); n != 0 {
		t.Fatalf("二次回收应为 0: %d", n)
	}
}

func TestStartIdleReaperStopsOnClose(t *testing.T) {
	p := NewPool()
	p.StartIdleReaper(time.Millisecond, time.Minute)
	p.StartIdleReaper(time.Millisecond, time.Minute) // 幂等
	time.Sleep(5 * time.Millisecond)
	p.Close()
	// Close 后 reapStop 关闭，协程退出（无泄漏断言靠 race/无阻塞；这里只验证不 panic）。
}

func TestReapIdleSkipsInFlight(t *testing.T) {
	p := NewPool()
	cli := &fakeClient{}
	seedFake(p, "busy", cli)
	setConn(p, "busy", func(st *connState) {
		st.lastUsed = time.Now().Add(-time.Hour) // 已超闲置阈值
		st.inFlight = 2                          // 但在途
	})

	if n := p.ReapIdle(time.Minute); n != 0 {
		t.Fatalf("在途连接不应被回收，got %d", n)
	}
	alive := hasConn(p, "busy")
	setConn(p, "busy", func(st *connState) {
		st.inFlight = 0
		st.lastUsed = time.Now().Add(-time.Hour)
	})
	if !alive {
		t.Fatal("在途连接应仍存活")
	}
	if n := p.ReapIdle(time.Minute); n != 1 {
		t.Fatalf("空闲后应回收 1 条，got %d", n)
	}
}

func TestLeaseUseCountsInFlight(t *testing.T) {
	p := NewPool()
	begin := p.leaseUse("s")
	end1 := begin()
	end2 := begin()
	p.mu.Lock()
	n := p.connLocked("s").inFlight
	p.mu.Unlock()
	if n != 2 {
		t.Fatalf("inFlight 应为 2，got %d", n)
	}
	end1()
	end2()
	p.mu.Lock()
	st := p.connLocked("s")
	n0 := st.inFlight
	p.mu.Unlock()
	exists := n0 != 0
	if exists {
		t.Fatal("计数归零应清掉条目")
	}
}

// blockClient Ping 阻塞到 ctx 取消（调用方取消/探活预算耗尽形态）。
type blockClient struct {
	client.MCPClient
	closed bool
}

func (f *blockClient) Ping(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (f *blockClient) Close() error { f.closed = true; return nil }

// TestPingServerLocalCancelKeepsConn 调用方 ctx 取消 ≠ 连接死亡（第六轮审计）：
// 探活预算派生自调用方 ctx，自身过期不得摘除健康连接——外部短周期巡检带短
// ctx，若据此摘连接，好连接会被反复摘除（stdio 场景即子进程反复重启）。
func TestPingServerLocalCancelKeepsConn(t *testing.T) {
	p := NewPool()
	cli := &blockClient{}
	seedFake(p, "s", cli)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	err := p.PingServer(ctx, "s")
	if err == nil || !strings.Contains(err.Error(), "本地取消") {
		t.Fatalf("本地取消应报「连接保留」形态错误: %v", err)
	}
	if cli.closed {
		t.Fatal("本地取消不得关闭连接")
	}
	if !hasConn(p, "s") {
		t.Fatal("本地取消不得摘除缓存连接")
	}
}
