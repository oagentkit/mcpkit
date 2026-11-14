package main

// CLI 侧回归测试（2026-11-14 issue 清零战 #141/#148）：
//   - #141 stdio 子进程环境必须走 procx.ChildEnv 白名单（绝不继承宿主全量
//     环境——注释曾声称「防密钥泄漏」而实现全量继承，安全声明失真）；
//   - #148 --ttl 移除（单次运行的进程内 TTL 缓存不可能命中，flag 语义误导）。
//
// 不依赖真实网络：子进程环境断言用本机 env 命令直接执行产出验证。

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// TestStdioChildEnvNoHostSecrets_Issue141 CLI 拉起的 stdio 子进程环境不含宿主
// 密钥变量：白名单基础集（PATH 等）保留，白名单外的宿主变量（密钥/凭证名）
// 一律不透传——oagentkit/mcpkit#141。
func TestStdioChildEnvNoHostSecrets_Issue141(t *testing.T) {
	if _, err := exec.LookPath("env"); err != nil {
		t.Skip("本机无 env 命令，跳过")
	}
	t.Setenv("MCPKIT_TEST_SECRET", "topsecret-value")
	t.Setenv("MCPKIT_TEST_PASSTHROUGH", "passthrough-value")

	// 按 --env 语义给一个按名透传项与一个字面注入项
	cmd, err := stdioCommandFunc(context.Background(), "env",
		[]string{"MCPKIT_TEST_PASSTHROUGH", "MCPKIT_TEST_LITERAL=literal-value"},
		nil)
	if err != nil {
		t.Fatalf("构造子进程失败: %v", err)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("执行子进程失败: %v", err)
	}

	child := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && k != "" {
			child[k] = v
		}
	}
	if _, ok := child["MCPKIT_TEST_SECRET"]; ok {
		t.Fatal("宿主密钥变量不得进入子进程环境（oagentkit/mcpkit#141）")
	}
	if v := child["MCPKIT_TEST_PASSTHROUGH"]; v != "passthrough-value" {
		t.Fatalf("--env 纯名应按名透传, got %q", v)
	}
	if v := child["MCPKIT_TEST_LITERAL"]; v != "literal-value" {
		t.Fatalf("--env KEY=VALUE 应字面注入, got %q", v)
	}
	if _, ok := child["PATH"]; !ok {
		t.Fatal("白名单基础集（PATH）应保留")
	}
	if v := child["GIT_TERMINAL_PROMPT"]; v != "0" {
		t.Fatalf("缺省项 GIT_TERMINAL_PROMPT=0 应存在, got %q", v)
	}
}

// TestStdioCommandFuncEnvDirect_Issue141 白名单形态直查（不依赖 env 命令的
// 快速断言）：cmd.Env 是显式白名单集，不是 os.Environ() 全量拷贝。
func TestStdioCommandFuncEnvDirect_Issue141(t *testing.T) {
	t.Setenv("MCPKIT_TEST_SECRET", "topsecret-value")
	t.Setenv("PATH", "/usr/bin:/bin") // 固定基线，防宿主环境干扰
	cmd, err := stdioCommandFunc(context.Background(), "true", nil, nil)
	if err != nil {
		t.Fatalf("构造子进程失败: %v", err)
	}
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "MCPKIT_TEST_SECRET=") {
			t.Fatalf("宿主密钥变量不得进入 cmd.Env: %q", kv)
		}
	}
	found := false
	for _, kv := range cmd.Env {
		if kv == "PATH=/usr/bin:/bin" {
			found = true
		}
	}
	if !found {
		t.Fatalf("白名单 PATH 应透传, got %v", cmd.Env)
	}
}

// TestCLITTLFlagRemoved_Issue148 --ttl 已移除：传入应报用法错误（退出码 3），
// 而不是给一个在该二进制里不可能生效的语义（单次运行进程内 TTL 缓存不可能
// 命中）——oagentkit/mcpkit#148。
func TestCLITTLFlagRemoved_Issue148(t *testing.T) {
	if code := runSelfcheck([]string{"--ttl", "30s", "--command", "true"}); code != 3 {
		t.Fatalf("--ttl 已移除，应报用法错误（退出码 3）, got %d", code)
	}
}
