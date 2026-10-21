package model

import (
	"strings"
	"testing"
)

// 档位写法在真实流量里是"野生"的：不同客户端发 xhigh / x-high / x_high /
// extra-high，大小写和首尾空白更是随机。
//
// 分隔符这条是实测出来的真实缺口：改动前 x-high / x_high / X-HIGH /
// extra-high / extrahigh 五种写法全部落到默认值 32768，而正确答案是 48000。
// 也就是说「第二强的档位」在带分隔符时反而比 high 更省——客户端以为自己在
// 加强推理，实际被降了一档，全程无报错、日志里也看不出来。
func TestNormalizeEffortKeyHandlesWildVariants(t *testing.T) {
	cases := map[string]string{
		"max":        "max",
		"MAX":        "max",
		"Max":        "max",
		" max ":      "max",
		"\tmax\n":    "max",
		"xhigh":      "xhigh",
		"x-high":     "xhigh",
		"x_high":     "xhigh",
		"X-HIGH":     "xhigh",
		"X High":     "xhigh",
		"extra-high": "extrahigh",
		"extrahigh":  "extrahigh",
		" low ":      "low",
		"":           "",
		"   ":        "",
	}
	for in, want := range cases {
		if got := NormalizeEffortKey(in); got != want {
			t.Errorf("NormalizeEffortKey(%q) = %q，应为 %q", in, got, want)
		}
	}
}

// 剥分隔符不能把两个不同的合法档位压成同一个键。
func TestNormalizeEffortKeyKeepsLevelsDistinct(t *testing.T) {
	levels := []string{"low", "medium", "high", "xhigh", "max"}
	seen := map[string]string{}
	for _, l := range levels {
		key := NormalizeEffortKey(l)
		if prev, dup := seen[key]; dup {
			t.Fatalf("档位 %q 与 %q 归一后撞成同一个键 %q", l, prev, key)
		}
		seen[key] = l
	}
}

// 带分隔符的写法必须拿到与规范写法相同的预算，而不是默认值。
func TestEffortBudgetForSeparatorVariants(t *testing.T) {
	for _, root := range []string{"low", "medium", "high", "xhigh", "max"} {
		want := EffortBudget[root]
		for _, variant := range []string{root, strings.ToUpper(root), " " + root + " "} {
			if got := EffortBudgetFor(variant); got != want {
				t.Errorf("EffortBudgetFor(%q) = %d，应为 %d（等同 %q）", variant, got, want, root)
			}
		}
	}
	// xhigh 的分隔符变体是这次修的原始缺口，单独锁一遍。
	if got := EffortBudgetFor("x-high"); got != EffortBudget["xhigh"] {
		t.Errorf("EffortBudgetFor(\"x-high\") = %d，应为 %d", got, EffortBudget["xhigh"])
	}
}

// IsKnownEffort 是"这个档位我认识吗"的唯一入口，必须与映射表严格一致。
func TestIsKnownEffortMatchesTable(t *testing.T) {
	for level := range EffortBudget {
		if !IsKnownEffort(level) {
			t.Errorf("映射表里的档位 %q 应被识别为已知", level)
		}
	}
	for _, garbage := range []string{"", "  ", "ultra", "auto", "adaptive", "enabled", "none"} {
		if IsKnownEffort(garbage) {
			t.Errorf("%q 不在映射表里，不应被识别为已知", garbage)
		}
	}
}
