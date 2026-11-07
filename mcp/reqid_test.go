package mcp

// request-ID 单源回归（oagentkit/mcpkit#65）：raw 目录通道与类型化 client 共用
// 同一条 transport，wire ID 必须同源分配——两套互不知晓的稠密计数器必然重号，
// stdio transport 按 ID 键路由（同号在途静默覆盖、无匹配静默丢弃）导致响应
// 串台/调用挂死/目录毒成空表。两层测试：
//  1. sharedIDTransport 单元面：并发同号调用方 ID → wire ID 互异 + 响应 ID 还原
//     + 可选接口转发；
//  2. 脚本化 stdio server 端到端：raw tools/list 与 typed tools/call 并发在途，
//     应答不串台（探针同款场景）。

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// recordingTransport 记录型假 transport：同号在途注册显式报错（把 stdio 的
// 「静默覆盖」本地显式化——重号即测试失败），并记录可选接口转发证据。
type recordingTransport struct {
	mu       sync.Mutex
	inFlight map[string]string // wire ID → method（在途登记）
	wireIDs  map[string]int    // 出现过的 wire ID 计数
	reqH     transport.RequestHandler
	protos   []string
}

func newRecordingTransport() *recordingTransport {
	return &recordingTransport{inFlight: map[string]string{}, wireIDs: map[string]int{}}
}

func (r *recordingTransport) Start(context.Context) error { return nil }

func (r *recordingTransport) SendRequest(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
	key := request.ID.String()
	r.mu.Lock()
	if prev, dup := r.inFlight[key]; dup {
		r.mu.Unlock()
		return nil, fmt.Errorf("重复在途 wire id %s（%s vs %s）——单源分配器失效", key, prev, request.Method)
	}
	r.inFlight[key] = request.Method
	r.wireIDs[key]++
	r.mu.Unlock()

	time.Sleep(5 * time.Millisecond) // 在途窗口：让并发请求真正同时在飞

	r.mu.Lock()
	delete(r.inFlight, key)
	r.mu.Unlock()
	return &transport.JSONRPCResponse{
		JSONRPC: mcp.JSONRPC_VERSION,
		ID:      request.ID,
		Result:  json.RawMessage(`{"echo":"` + request.Method + `"}`),
	}, nil
}

func (r *recordingTransport) SendNotification(context.Context, mcp.JSONRPCNotification) error {
	return nil
}
func (r *recordingTransport) SetNotificationHandler(func(mcp.JSONRPCNotification)) {}
func (r *recordingTransport) Close() error                                         { return nil }
func (r *recordingTransport) GetSessionId() string                                 { return "" }

// recordingTransport 实现 BidirectionalInterface + HTTPConnection + legacy 判定，
// 供转发断言用。
func (r *recordingTransport) SetRequestHandler(h transport.RequestHandler) { r.reqH = h }
func (r *recordingTransport) SetProtocolVersion(v string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.protos = append(r.protos, v)
}
func (r *recordingTransport) RequiresLegacyProtocol() bool { return false }

var (
	_ transport.BidirectionalInterface = (*recordingTransport)(nil)
	_ transport.HTTPConnection         = (*recordingTransport)(nil)
)

// TestSharedIDTransportSingleSource 单元面（oagentkit/mcpkit#65）：并发 32 路调用方
// **同号** ID（各自独立计数器起步重号的极端形态）经包装器出站——wire ID 必须
// 全部互异（单源原子分配）、响应 ID 还原为各自调用方原值、无重号在途。
func TestSharedIDTransportSingleSource(t *testing.T) {
	inner := newRecordingTransport()
	shared := &sharedIDTransport{Interface: inner}

	const n = 32
	callerID := mcp.NewRequestId(7)
	results := make([]*transport.JSONRPCResponse, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := shared.SendRequest(context.Background(), transport.JSONRPCRequest{
				JSONRPC: mcp.JSONRPC_VERSION,
				ID:      callerID, // 调用方同号：模拟双计数器重号形态
				Method:  fmt.Sprintf("m%d", i%4),
			})
			results[i], errs[i] = resp, err
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("第 %d 路失败（重号在途即单源失效）: %v", i, errs[i])
		}
		if results[i].ID != callerID {
			t.Fatalf("响应 ID 应还原调用方原值，得 %s", results[i].ID.String())
		}
	}
	inner.mu.Lock()
	defer inner.mu.Unlock()
	if len(inner.wireIDs) != n {
		t.Fatalf("wire ID 应 %d 个互异值，得 %d 个", n, len(inner.wireIDs))
	}
	for id, c := range inner.wireIDs {
		if c != 1 {
			t.Fatalf("wire id %s 出现 %d 次（应恰一次）", id, c)
		}
	}
}

// TestSharedIDTransportForwardsOptionalInterfaces 可选能力面转发：包装不得吞掉
// BidirectionalInterface（elicitation 带外通道）/HTTPConnection（版本头）/
// legacy 判定——client.Start/applyNegotiatedVersion 都按类型断言落点。
func TestSharedIDTransportForwardsOptionalInterfaces(t *testing.T) {
	inner := newRecordingTransport()
	shared := &sharedIDTransport{Interface: inner}

	var bi transport.BidirectionalInterface = shared
	bi.SetRequestHandler(func(context.Context, transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
		return nil, nil
	})
	if inner.reqH == nil {
		t.Fatal("SetRequestHandler 未转发到内层（elicitation 带外通道会断）")
	}
	var hc transport.HTTPConnection = shared
	hc.SetProtocolVersion("2026-07-28")
	if len(inner.protos) != 1 || inner.protos[0] != "2026-07-28" {
		t.Fatalf("SetProtocolVersion 未转发: %v", inner.protos)
	}
	var lr interface{ RequiresLegacyProtocol() bool } = shared
	if lr.RequiresLegacyProtocol() {
		t.Fatal("legacy 判定转发异常")
	}
}

// scriptedMCPHelper 脚本化 stdio server（#65 探针同款，经测试二进制拉起）：
// server/discover 报 method not found（逼 client 走 legacy 握手）→ initialize
// 回显请求的协议版本；tools/list 与 tools/call 各延迟应答（制造双请求同时在途
// 窗口），结果带方法标记（串台时结果面立刻可辨）。
func scriptedMCPHelper(t *testing.T) {
	t.Helper()
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	out := bufio.NewWriter(os.Stdout)
	respond := func(id json.RawMessage, payload string) {
		// errcheck：写测试 helper 的 stdout，失败无处置面，显式忽略
		_, _ = fmt.Fprintf(out, `{"jsonrpc":"2.0","id":%s,%s}`+"\n", id, payload)
		_ = out.Flush()
	}
	for sc.Scan() {
		var msg struct {
			ID     *json.RawMessage `json:"id"`
			Method string           `json:"method"`
			Params json.RawMessage  `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil || msg.ID == nil {
			continue // 通知（无 id）与坏行：忽略
		}
		switch msg.Method {
		case "server/discover":
			// 现代 probe 拒绝 → client 回落 legacy initialize 握手
			respond(*msg.ID, `"error":{"code":-32601,"message":"method not found"}`)
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			if p.ProtocolVersion == "" {
				p.ProtocolVersion = "2025-11-25"
			}
			respond(*msg.ID, fmt.Sprintf(`"result":{"protocolVersion":%q,"capabilities":{},"serverInfo":{"name":"scripted","version":"1.0"}}`, p.ProtocolVersion))
		case "tools/list":
			time.Sleep(20 * time.Millisecond) // 在途窗口：与 tools/call 同时在飞
			respond(*msg.ID, `"result":{"tools":[{"name":"t1","description":"scripted","inputSchema":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}}]}`)
		case "tools/call":
			time.Sleep(20 * time.Millisecond) // 在途窗口
			respond(*msg.ID, `"result":{"content":[{"type":"text","text":"CALL-OK"}],"isError":false}`)
		default:
			respond(*msg.ID, `"error":{"code":-32601,"message":"method not found"}`)
		}
	}
}

// TestScriptedMCPHelperEntry 测试二进制内的脚本 server 入口（仅以 env 标记拉起）。
func TestScriptedMCPHelperEntry(t *testing.T) {
	if os.Getenv("MCPKIT_SCRIPTED_HELPER") != "1" {
		t.Skip("helper：仅由 TestStdioRawTypedRequestIDSingleSource 以 env 标记拉起")
	}
	scriptedMCPHelper(t)
}

// TestStdioRawTypedRequestIDSingleSource 端到端回归（oagentkit/mcpkit#65，探针
// 同款）：真实 stdio 子进程上，raw tools/list（catalogFor 快照的 transport 句柄）
// 与 typed tools/call（池缓存 client）并发在途——重号时 stdio 按后写者路由：
// raw 收到串台的 call 结果解析出 0 条目（目录毒空）/typed 收到串台的 list 结果
// （空信封错答）或挂死到 ctx 超时。单源后两路结果每轮都必须正确。
func TestStdioRawTypedRequestIDSingleSource(t *testing.T) {
	testBin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := NewPool(ServerConfig{Name: "srv", Timeout: 10 * time.Second,
		Env:     []string{"MCPKIT_SCRIPTED_HELPER=1"}, // ChildEnv 白名单字面量透传（helper 守卫标记）
		Command: []string{testBin, "-test.run=TestScriptedMCPHelperEntry", "-test.v=false"}})
	defer p.Close()

	// 首装：dial（transport 单源包装）+ initialize + raw 列举
	res, err := p.Tools(context.Background(), []ToolSpec{{Server: "srv", AllowAll: true}})
	if err != nil {
		t.Fatalf("首装失败: %v", err)
	}
	if len(res.Tools) != 1 || res.Failed != nil {
		t.Fatalf("首装目录异常: tools=%d failed=%v", len(res.Tools), res.Failed)
	}

	// 抓连接快照：typed=池缓存 client；tr=catalogFor 同款句柄（生产路径即包装器）
	st := p.conn("srv")
	st.mu.Lock()
	cli, tr, protoVersion := st.cli, st.tr, st.protoVersion
	st.mu.Unlock()
	if cli == nil || tr == nil {
		t.Fatal("连接快照缺失")
	}

	const rounds = 10
	for i := 0; i < rounds; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var (
			wg              sync.WaitGroup
			entries         []toolEntry
			rawErr, callErr error
			callText        string
		)
		wg.Add(2)
		go func() { // raw 通道：catalogFor 缓存失效路径的同款线上列举
			defer wg.Done()
			raw, err := fetchRawToolsList(ctx, tr, protoVersion)
			if err != nil {
				rawErr = err
				return
			}
			entries, rawErr = parseRawToolsList(raw)
		}()
		go func() { // typed 通道：tools/call
			defer wg.Done()
			out, err := cli.CallTool(ctx, mcp.CallToolRequest{
				Request: mcp.Request{Method: "tools/call"},
				Params:  mcp.CallToolParams{Name: "t1", Arguments: json.RawMessage(`{"q":"x"}`)},
			})
			if err != nil {
				callErr = err
				return
			}
			if len(out.Content) > 0 {
				if tc, ok := out.Content[0].(mcp.TextContent); ok {
					callText = tc.Text
				}
			}
		}()
		wg.Wait()
		cancel()

		if rawErr != nil {
			t.Fatalf("第 %d 轮 raw 列举失败（串台/挂死形态）: %v", i+1, rawErr)
		}
		if len(entries) != 1 || entries[0].tool.Name != "t1" {
			t.Fatalf("第 %d 轮 raw 目录被毒（串台收进 call 结果 → 0 条目）: n=%d", i+1, len(entries))
		}
		if callErr != nil {
			t.Fatalf("第 %d 轮 typed 调用失败（串台挂死形态）: %v", i+1, callErr)
		}
		if callText != "CALL-OK" {
			t.Fatalf("第 %d 轮 typed 调用串台（拿到 list 结果空信封）: %q", i+1, callText)
		}
	}
}
