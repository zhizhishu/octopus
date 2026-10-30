package op

import "testing"

// 74 次请求的基线录制不能被 300 秒整单超时掐掉：按计划请求数放大，封顶 30 分钟；
// 其余探针组合的超时一秒都不变。
func TestAuditTimeoutForScalesWithPlannedRequests(t *testing.T) {
	standard := []string{"liveness", "identity", "glitch", "token_delta", "echo_rewrite", "context_canary", "signature", "fingerprint"}
	if got := AuditTimeoutFor(standard, 300); got != 300 {
		t.Fatalf("standard probes must keep the old timeout, got %d", got)
	}
	if got := AuditTimeoutFor(nil, 180); got != 180 {
		t.Fatalf("no probes: %d", got)
	}
	baseline := AuditTimeoutFor([]string{"tokenizer_baseline"}, 300)
	if baseline != 300+74*auditSecondsPerPlannedRequest {
		t.Fatalf("baseline recording budget = %d, want %d", baseline, 300+74*auditSecondsPerPlannedRequest)
	}
	id := AuditTimeoutFor([]string{"liveness", " tokenizer_id "}, 300)
	if id != 300+22*auditSecondsPerPlannedRequest {
		t.Fatalf("tokenizer_id budget = %d", id)
	}
	both := AuditTimeoutFor(append(standard, "tokenizer_id", "tokenizer_baseline"), 300)
	if both != 300+96*auditSecondsPerPlannedRequest {
		t.Fatalf("both = %d", both)
	}
	capped := AuditTimeoutFor([]string{"tokenizer_baseline", "tokenizer_baseline", "tokenizer_baseline"}, 300)
	if capped != AuditScaledMaxTimeoutSeconds {
		t.Fatalf("scaled timeout must be capped at %d, got %d", AuditScaledMaxTimeoutSeconds, capped)
	}
}

func TestAuditEnqueueStoresScaledTimeout(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	ch := ensureAuditChannel(t, "超时渠道")
	cases := []struct {
		model     string
		probes    []string
		requested int
		want      int
	}{
		{"m-default", nil, 0, AuditDefaultTimeoutSeconds},
		{"m-capped", []string{"liveness"}, 1000, AuditMaxTimeoutSeconds},
		{"m-record", []string{"tokenizer_baseline"}, 0, 300 + 74*auditSecondsPerPlannedRequest},
		{"m-record-short", []string{"tokenizer_baseline"}, 60, 60 + 74*auditSecondsPerPlannedRequest},
		// 用户请求的超时仍先按旧上限 300 秒截断，再加请求量预算——不能借这条路绕过上限。
		{"m-record-huge", []string{"tokenizer_baseline"}, 99999, 300 + 74*auditSecondsPerPlannedRequest},
	}
	for _, tc := range cases {
		jobID, err := ModelAuditEnqueue(ctx, ModelAuditEnqueueInput{
			ChannelID: ch.ID, Models: []string{tc.model}, Probes: tc.probes, TimeoutSeconds: tc.requested,
		})
		if err != nil {
			t.Fatalf("%s: enqueue: %v", tc.model, err)
		}
		job, _, err := ModelAuditJobDetail(ctx, jobID)
		if err != nil {
			t.Fatal(err)
		}
		if job.TimeoutSeconds != tc.want {
			t.Fatalf("%s: stored timeout = %d, want %d", tc.model, job.TimeoutSeconds, tc.want)
		}
	}
}
