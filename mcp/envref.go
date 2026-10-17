// envref.go —— ServerConfig 头部值的 ${VAR} 环境引用展开（token 不落配置
// 文件的纪律件；收编自 argus toolpool，v0.10.39）。未定义变量保留原样并经
// onMissing 上报——显式可见优于静默空值。
package mcp

import (
	"os"
	"sort"
	"strings"
)

// ExpandEnvRefs 对声明式头部表逐值做 ${VAR} 展开（nil/空表原样返回 nil）。
// onMissing 可 nil（未定义变量仍保留原样，只是无告警出口）。
func ExpandEnvRefs(headers map[string]string, onMissing func(name string)) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	out := make(map[string]string, len(headers))
	for k, v := range headers {
		out[k] = ExpandEnvRef(v, onMissing)
	}
	return out
}

// ExpandEnvRef 展开字符串中的 ${VAR} 环境引用。已定义取环境值；未定义保留
// 原样（扫描位置跳过本标记，防死循环）并回调 onMissing。
func ExpandEnvRef(v string, onMissing func(name string)) string {
	const marker = "${"
	var b strings.Builder
	for {
		start := strings.Index(v, marker)
		if start < 0 {
			b.WriteString(v)
			return b.String()
		}
		end := strings.Index(v[start:], "}")
		if end < 0 {
			b.WriteString(v)
			return b.String()
		}
		end += start
		name := v[start+len(marker) : end]
		if val, ok := os.LookupEnv(name); ok {
			b.WriteString(v[:start])
			b.WriteString(val)
		} else {
			if onMissing != nil {
				onMissing(name)
			}
			b.WriteString(v[:end+1])
		}
		v = v[end+1:]
	}
}

// sortedEnvNames map 键排序（通知确定性）。
func sortedEnvNames(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
