// Command mcp-selfcheck 在部署机上直接跑 MCP server 自检。
//
// 用途：把「server 起来了没」「工具在不在」「调不调得通」三件事从
// 「看日志猜」变成一条命令的机器可读输出（JSON / 文本）。
//
// 用法:
//
//	mcp-selfcheck selfcheck --url http://127.0.0.1:8931/mcp --server fs
//	mcp-selfcheck selfcheck --url http://127.0.0.1:8931/mcp --server fs \
//	    --expect read --expect write --json
//	mcp-selfcheck selfcheck --command ./my-mcp-server --server local --json
//
// 退出码：0 = healthy；1 = degraded；2 = down；3 = 用法/连接错误。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/client"

	"github.com/oagentkit/mcpkit/mcp"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(3)
	}
	switch os.Args[1] {
	case "selfcheck":
		os.Exit(runSelfcheck(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n\n", os.Args[1])
		usage()
		os.Exit(3)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `mcp-selfcheck —— MCP server 自检工具

子命令:
  selfcheck     拉工具目录 + 探连通性 + 核工具面，输出健康结论

退出码:
  0 = healthy   1 = degraded   2 = down   3 = 用法/连接错误
`)
}

// stringList 逗号分隔的字符串列表（可重复给）。
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func runSelfcheck(args []string) int {
	fs := flag.NewFlagSet("selfcheck", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `用法: mcp-selfcheck selfcheck [flags]

拉取 server 的工具目录、探测连通性、核对声明面，输出健康结论。

flags:
`)
		fs.PrintDefaults()
	}
	var (
		url     = fs.String("url", "", "server URL（HTTP / streamable http 传输）")
		command = fs.String("command", "", "server 可执行文件（stdio 传输，逗号分隔参数）")
		server  = fs.String("server", "default", "server 逻辑名")
		timeout = fs.Duration("timeout", 10*time.Second, "单次探测超时")
		ttl     = fs.Duration("ttl", 30*time.Second, "目录缓存 TTL（0 = 不缓存）")
		asJSON  = fs.Bool("json", false, "以 JSON 输出")
		probe   = fs.Bool("probe-tools", false, "逐工具实调探测（有副作用）")
		doInit  = fs.Bool("init", true, "自检前先发 initialize 握手")
	)
	var expect stringList
	fs.Var(&expect, "expect", "声明期望存在的工具名（可重复给）")
	if err := fs.Parse(args); err != nil {
		return 3
	}
	if *url == "" && *command == "" {
		fmt.Fprintln(os.Stderr, "必须给 --url 或 --command")
		return 3
	}

	ctx, cancel := context.WithTimeout(context.Background(), (*timeout)*2)
	defer cancel()

	var (
		cli client.MCPClient
		err error
	)
	if *command != "" {
		parts := strings.Split(*command, ",")
		cli, err = client.NewStdioMCPClient(parts[0], env, parts[1:]...)
	} else {
		cli, err = client.NewStreamableHttpClient(*url)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接失败: %v\n", err)
		return 3
	}
	defer func() { _ = cli.Close() }()

	opts := mcp.SelfcheckOptions{
		Server:     *server,
		Timeout:    *timeout,
		TTL:        *ttl,
		ProbeTools: *probe,
		Initialize: *doInit,
	}
	if len(expect) > 0 {
		opts.Expected = mcp.NewCatalog(*server, expect)
	}

	res := mcp.NewSelfchecker().Check(ctx, cli, opts)
	health := mcp.Aggregate([]mcp.SelfcheckResult{res})

	if *asJSON {
		b, merr := json.MarshalIndent(jsonOutput(res, health), "", "  ")
		if merr != nil {
			fmt.Fprintf(os.Stderr, "序列化失败: %v\n", merr)
			return 3
		}
		fmt.Println(string(b))
	} else {
		printText(res, health)
	}

	switch health.State {
	case mcp.HealthHealthy:
		return 0
	case mcp.HealthDegraded:
		return 1
	default:
		return 2
	}
}

// printText 文本形态输出（人看）。
func printText(res mcp.SelfcheckResult, h mcp.Health) {
	fmt.Printf("server: %s\n", res.Server)
	fmt.Printf("state:  %s\n", h.State)
	fmt.Printf("catalog: %s\n", strings.Join(res.Catalog.Tools, ", "))
	if len(res.Expected.Tools) > 0 {
		fmt.Printf("expected: %s\n", strings.Join(res.Expected.Tools, ", "))
		fmt.Printf("missing:  %s\n", strings.Join(res.Diff.Missing, ", "))
		fmt.Printf("extra:    %s\n", strings.Join(res.Diff.Extra, ", "))
	}
	if len(res.Tools) > 0 {
		fmt.Println("tools:")
		for _, t := range res.Tools {
			fmt.Printf("  %-24s %-12s %s\n", t.Tool, t.Status, t.Reason)
		}
	}
	if len(h.Issues) > 0 {
		fmt.Println("issues:")
		for _, i := range h.Issues {
			tool := i.Tool
			if tool == "" {
				tool = "-"
			}
			fmt.Printf("  %-16s %-16s %-12s %s\n", i.Server, tool, i.Kind, i.Reason)
		}
	}
}

// env 子进程环境（stdio 传输）。留空 = 不继承任何环境变量（防密钥泄漏）。
var env = []string{}

// jsonToolCheck 工具检查项的 JSON 形态。
type jsonToolCheck struct {
	Tool   string `json:"tool"`
	Status string `json:"status"`
	Kind   string `json:"kind"`
	Reason string `json:"reason,omitempty"`
}

// jsonIssue 健康问题的 JSON 形态。
type jsonIssue struct {
	Server string `json:"server"`
	Tool   string `json:"tool,omitempty"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

// jsonHealth 健康结论的 JSON 形态。
type jsonHealth struct {
	State         string      `json:"state"`
	Servers       []string    `json:"servers"`
	Issues        []jsonIssue `json:"issues"`
	Total         int         `json:"total"`
	HealthyCount  int         `json:"healthy_count"`
	DegradedCount int         `json:"degraded_count"`
	DownCount     int         `json:"down_count"`
}

// jsonResult 单 server 自检结果的 JSON 形态。
type jsonResult struct {
	Server    string          `json:"server"`
	Reachable bool            `json:"reachable"`
	State     string          `json:"state"`
	Tools     []jsonToolCheck `json:"tools"`
	Catalog   []string        `json:"catalog"`
	Expected  []string        `json:"expected,omitempty"`
	Missing   []string        `json:"missing,omitempty"`
	Extra     []string        `json:"extra,omitempty"`
	Kind      string          `json:"kind,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	Cached    bool            `json:"cached"`
	At        string          `json:"at"`
}

// jsonOutput CLI 的 JSON 输出（snake_case 键 + 可读枚举值）。
//
// 键名用 snake_case：这是给脚本/HTTP 端点消费的，不该泄漏 Go 字段名。
func jsonOutput(res mcp.SelfcheckResult, h mcp.Health) map[string]any {
	tools := make([]jsonToolCheck, 0, len(res.Tools))
	for _, t := range res.Tools {
		tools = append(tools, jsonToolCheck{
			Tool:   t.Tool,
			Status: string(t.Status),
			Kind:   t.Kind.String(),
			Reason: t.Reason,
		})
	}
	issues := make([]jsonIssue, 0, len(h.Issues))
	for _, i := range h.Issues {
		issues = append(issues, jsonIssue{
			Server: i.Server, Tool: i.Tool, Kind: i.Kind.String(), Reason: i.Reason,
		})
	}
	state := string(h.State)
	kind := ""
	reason := ""
	if res.Err != nil {
		kind = res.Kind.String()
		reason = res.Err.Error()
	}
	return map[string]any{
		"health": jsonHealth{
			State:         state,
			Servers:       h.Servers,
			Issues:        issues,
			Total:         h.Total,
			HealthyCount:  h.HealthyCount,
			DegradedCount: h.DegradedCount,
			DownCount:     h.DownCount,
		},
		"results": []jsonResult{{
			Server:    res.Server,
			Reachable: res.Reachable,
			State:     state,
			Tools:     tools,
			Catalog:   res.Catalog.Tools,
			Expected:  res.Expected.Tools,
			Missing:   res.Diff.Missing,
			Extra:     res.Diff.Extra,
			Kind:      kind,
			Reason:    reason,
			Cached:    res.Cached,
			At:        res.At.Format(time.RFC3339),
		}},
	}
}
