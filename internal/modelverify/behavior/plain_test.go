package behavior

import (
	"strings"
	"testing"
)

// TestPlainVerdictNeverClaimsAuthenticity 守一条硬规矩：
// 任何档位的人话结论都不许暗示"已验真"或"确认是假"。
//
// 这套检测测的是"行为像不像官方"，不是密码学验真。一旦措辞暗示"已验真"，
// 用户就会拿它去做本该人工复核的决定——那比不给结论更危险。
func TestPlainVerdictNeverClaimsAuthenticity(t *testing.T) {
	banned := []string{"已验真", "确认为真", "保证是真", "确认是假", "确定为假", "百分百"}
	verdicts := []Verdict{VerdictNone, VerdictLow, VerdictMedium, VerdictHigh, VerdictUnknown}
	families := []string{"", "anthropic", "openai", "qwen", "deepseek"}

	for _, v := range verdicts {
		for _, fam := range families {
			p := PlainVerdictFor(v, 50, 2, 5, fam)
			all := p.Headline + p.Meaning + p.Caveat + p.NextStep
			for _, b := range banned {
				if strings.Contains(all, b) {
					t.Fatalf("verdict=%s family=%q 出现断言真伪的措辞 %q: %s", v, fam, b, all)
				}
			}
		}
	}
}

// TestPlainVerdictAlwaysFillsFourFields 保证四个字段都不空。
// 空 caveat 会让人以为结论没有边界；空 next_step 会让人不知道下一步做什么。
func TestPlainVerdictAlwaysFillsFourFields(t *testing.T) {
	verdicts := []Verdict{VerdictNone, VerdictLow, VerdictMedium, VerdictHigh, VerdictUnknown}
	for _, v := range verdicts {
		p := PlainVerdictFor(v, 0, 0, 0, "")
		if p.Headline == "" || p.Meaning == "" || p.Caveat == "" || p.NextStep == "" {
			t.Fatalf("verdict=%s 有字段为空: %#v", v, p)
		}
		if p.Level != string(v) {
			t.Fatalf("verdict=%s Level 必须同值，得到 %q", v, p.Level)
		}
	}
}

// TestPlainVerdictCaveatDiffersByFamily 守家族差异：
// Anthropic 有可验证签名，其它家族没有。两者的话术必须不同，
// 否则用户会以为国产模型也拿到了同等强度的证据。
func TestPlainVerdictCaveatDiffersByFamily(t *testing.T) {
	anthropic := PlainVerdictFor(VerdictNone, 0, 0, 5, "anthropic").Caveat
	openai := PlainVerdictFor(VerdictNone, 0, 0, 5, "openai").Caveat
	qwen := PlainVerdictFor(VerdictNone, 0, 0, 5, "qwen").Caveat

	if anthropic == openai {
		t.Fatalf("Anthropic 与 OpenAI 的边界说明不应相同")
	}
	if !strings.Contains(anthropic, "签名") {
		t.Fatalf("Anthropic 的说明应点明签名是硬证据，实际: %s", anthropic)
	}
	if !strings.Contains(openai, "没有可验证的签名") {
		t.Fatalf("OpenAI 的说明应点明没有签名机制，实际: %s", openai)
	}
	if !strings.Contains(qwen, "国产") {
		t.Fatalf("国产家族的说明应有对应措辞，实际: %s", qwen)
	}
}

// TestPrimaryFamilyIsDeterministic 保证多家族命中时结果稳定。
// map 遍历序随机，若这里用 map 直接取，同一输入会给出不同家族、测试偶发失败。
func TestPrimaryFamilyIsDeterministic(t *testing.T) {
	in := []string{"qwen", "openai", "deepseek"}
	first := PrimaryFamily(in)
	for i := 0; i < 50; i++ {
		if got := PrimaryFamily(in); got != first {
			t.Fatalf("同一输入应稳定返回同一家族: %q vs %q", first, got)
		}
	}
	if first != "openai" {
		t.Fatalf("按固定顺序应优先 openai，得到 %q", first)
	}
	if PrimaryFamily(nil) != "" {
		t.Fatalf("空输入应返回空串")
	}
	if PrimaryFamily([]string{"unknown-family"}) != "" {
		t.Fatalf("未登记家族应返回空串")
	}
}

// TestFamilyDisplayNameCoversReferenceFamilies 覆盖参考项目里的全部家族。
// 漏一个就会在界面上显示成空白，用户看不出测的是什么。
func TestFamilyDisplayNameCoversReferenceFamilies(t *testing.T) {
	all := []string{
		"openai", "anthropic", "google", "xai",
		"qwen", "deepseek", "zhipu", "moonshot", "bytedance", "baidu", "minimax", "mimo",
		"meta", "mistral",
	}
	for _, f := range all {
		if FamilyDisplayName(f) == "" {
			t.Fatalf("家族 %q 缺少展示名", f)
		}
	}
	if FamilyDisplayName("") != "" {
		t.Fatalf("空家族应返回空串")
	}
}

// TestPlainVerdictUnknownExplainsInsufficient 守"没测出结果"的说法。
// 这个档位最容易被误解成"没问题"，必须明确写出它既不是好也不是坏。
func TestPlainVerdictUnknownExplainsInsufficient(t *testing.T) {
	p := PlainVerdictFor(VerdictUnknown, 0, 0, 1, "openai")
	if p.Headline != "没测出结果" {
		t.Fatalf("unknown 的标题应直白，实际 %q", p.Headline)
	}
	if !strings.Contains(p.Caveat, "没问题") || !strings.Contains(p.Caveat, "有问题") {
		t.Fatalf("必须同时否定「没问题」和「有问题」，实际: %s", p.Caveat)
	}
}
