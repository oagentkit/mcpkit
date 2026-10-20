package mcp

import "sort"

// Catalog 工具面快照：某个 server（或一组 server）实际提供的工具清单。
//
// 这是「装包≠开洞」「授权面 ⊄ 契约面」这类治理规则的**数据基础**：
// 只有把「声明要用的」与「实际有的」摆在一起做差集，
// 才能答出「我以为有的工具其实没有」与「多出来的没声明的工具」两个问题。
type Catalog struct {
	// Server 所属 server 名（空 = 跨 server 聚合）。
	Server string
	// Tools 工具名清单（已去重、已排序）。
	Tools []string
}

// NewCatalog 构造目录（去重 + 排序，保证可比与可比较）。
func NewCatalog(server string, tools []string) Catalog {
	seen := make(map[string]struct{}, len(tools))
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	sort.Strings(out)
	return Catalog{Server: server, Tools: out}
}

// Contains 目录是否含某工具。
func (c Catalog) Contains(name string) bool {
	for _, t := range c.Tools {
		if t == name {
			return true
		}
	}
	return false
}

// CatalogDiff 双向差集结果。
type CatalogDiff struct {
	// Missing 声明了但实际没有（expected - actual）——「我以为有的其实没有」。
	Missing []string
	// Extra 实际有但没声明（actual - expected）——「多出来的、没走授权面」。
	Extra []string
}

// Equal 双向差集是否都为空。
func (d CatalogDiff) Equal() bool { return len(d.Missing) == 0 && len(d.Extra) == 0 }

// Diff 双向有序差集。
//
// expected = 声明/期望要用的工具面；actual = server 实际提供的工具面。
// 两个方向都要：**只看一个方向会漏掉一半的治理问题**——
// 只看 Missing 漏掉「没声明却存在」的工具（授权面之外的开洞），
// 只看 Extra 漏掉「声明了却没有」的工具（装包≠开洞）。
//
// 结果按字典序（可复现、可比较）。
func Diff(expected, actual Catalog) CatalogDiff {
	exp := make(map[string]struct{}, len(expected.Tools))
	for _, t := range expected.Tools {
		exp[t] = struct{}{}
	}
	act := make(map[string]struct{}, len(actual.Tools))
	for _, t := range actual.Tools {
		act[t] = struct{}{}
	}
	var d CatalogDiff
	for _, t := range expected.Tools {
		if _, ok := act[t]; !ok {
			d.Missing = append(d.Missing, t)
		}
	}
	for _, t := range actual.Tools {
		if _, ok := exp[t]; !ok {
			d.Extra = append(d.Extra, t)
		}
	}
	sort.Strings(d.Missing)
	sort.Strings(d.Extra)
	return d
}

// DiffNames 便捷形态：两个工具名清单的差集。
func DiffNames(expected, actual []string) CatalogDiff {
	return Diff(NewCatalog("", expected), NewCatalog("", actual))
}
