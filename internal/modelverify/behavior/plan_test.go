package behavior

import "testing"

// TestPlannedRequestsAreReal 钉死每项检查的计划请求数——前端展示用，不许猜。
func TestPlannedRequestsAreReal(t *testing.T) {
	cases := map[ProbeID]int{
		ProbeLiveness:      1,
		ProbeIdentity:      1,
		ProbeEchoRewrite:   1,
		ProbeContextCanary: 1,
		ProbeGlitch:        1,
		ProbeTokenDelta:    2,
		ProbeSignature:     2,
		ProbeFingerprint:   3,
	}
	for id, want := range cases {
		if got := PlannedRequests(id); got != want {
			t.Fatalf("%s requests = %d, want %d", id, got, want)
		}
	}
	if n := PlannedRequests(ProbeTokenizerID); n < 10 {
		t.Fatalf("tokenizer_id upper bound too small: %d", n)
	}
	if n := PlannedRequests(ProbeTokenizerBaseline); n < 70 {
		t.Fatalf("tokenizer_baseline too small: %d", n)
	}
}

// TestPlanForGreysSignatureOnNonClaude 非 Anthropic 渠道上签名项灰掉且请求数为 0。
func TestPlanForGreysSignatureOnNonClaude(t *testing.T) {
	plan := PlanFor("openai-chat", "gpt-4o")
	var sig *ProbePlan
	for i := range plan {
		if plan[i].ID == ProbeSignature {
			sig = &plan[i]
			break
		}
	}
	if sig == nil {
		t.Fatal("missing signature plan")
	}
	if sig.Applicable || sig.Requests != 0 || sig.SkipReason == "" {
		t.Fatalf("signature should be greyed: %+v", sig)
	}
}

// TestPlanForClaudeSignatureApplies Anthropic + Claude 型号才启用签名。
func TestPlanForClaudeSignatureApplies(t *testing.T) {
	plan := PlanFor("anthropic-messages", "claude-opus-4")
	for _, p := range plan {
		if p.ID == ProbeSignature {
			if !p.Applicable || p.Requests != 2 {
				t.Fatalf("signature should apply: %+v", p)
			}
			return
		}
	}
	t.Fatal("missing signature")
}

// TestSumPlannedRequestsSkipsInapplicable 合计时跳过不适用项。
func TestSumPlannedRequestsSkipsInapplicable(t *testing.T) {
	plan := PlanFor("openai-chat", "gpt-4o")
	ids := FullProbeIDs()
	total := SumPlannedRequests(plan, ids)
	// 全面方案含 fingerprint(3)+token_delta(2)+… 但不含 signature(0)
	if total < 8 {
		t.Fatalf("full plan total too small: %d", total)
	}
	quick := SumPlannedRequests(plan, QuickProbeIDs())
	if quick != 3 {
		t.Fatalf("quick = %d, want 3", quick)
	}
}
