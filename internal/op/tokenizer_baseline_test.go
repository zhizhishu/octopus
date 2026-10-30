package op

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/modelverify"
	"github.com/zhizhishu/octopus/internal/modelverify/behavior"
	"github.com/zhizhishu/octopus/internal/modelverify/tokenbank"
)

// countingSender 按"基线 + 每题计数"应答；计数是测试专用的合成数据。
type countingSender struct {
	bank   *tokenbank.Bank
	counts []int
	drift  int
	base   int
}

func (s *countingSender) Ask(_ context.Context, ask modelverify.Ask) (modelverify.Response, error) {
	if ask.Prompt == s.bank.Wrap("") {
		n := 31 + s.base*s.drift
		s.base++
		return modelverify.Response{Usage: modelverify.Usage{Input: n}}, nil
	}
	for j, p := range s.bank.Probes {
		if ask.Prompt == s.bank.Wrap(p.Text) {
			return modelverify.Response{Usage: modelverify.Usage{Input: 31 + s.counts[j]}}, nil
		}
	}
	return modelverify.Response{}, errors.New("unexpected prompt")
}

func syntheticCounts(t *testing.T, delta int) []int {
	t.Helper()
	b, err := tokenbank.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	c, _ := b.Class("glm-5")
	out := append([]int(nil), c.Counts...)
	for j := range out {
		out[j] += delta
	}
	return out
}

func seedRecording(t *testing.T, ctx context.Context, upstream string, counts []int, drift int, status string) int64 {
	t.Helper()
	b, _ := tokenbank.Builtin()
	results := behavior.Run(ctx, []behavior.ProbeID{behavior.ProbeTokenizerBaseline},
		&countingSender{bank: b, counts: counts, drift: drift}, upstream)
	raw, err := json.Marshal(behavior.ToView(behavior.BuildReport(results, "client-name")))
	if err != nil {
		t.Fatal(err)
	}
	row := model.ModelAuditResult{
		JobID: 1, ChannelID: 7, ChannelName: "可信渠道", Model: "client-name",
		UpstreamModel: upstream, Status: status, Report: string(raw),
	}
	if err := db.GetDB().Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row.ID
}

func TestTokenizerBaselineFromResultRoundTrip(t *testing.T) {
	ctx := setupRelayLogTest(t)
	counts := syntheticCounts(t, 3)
	id := seedRecording(t, ctx, "vendor/Claude-X:latest", counts, 0, model.AuditResultDone)

	row, err := TokenizerBaselineFromResult(ctx, TokenizerBaselineFromResultInput{ResultID: id, Label: "  官方直连  "})
	if err != nil {
		t.Fatal(err)
	}
	if row.Model != "claude-x" || row.Label != "官方直连" || row.SourceChannelID != 7 || row.SourceResultID != id {
		t.Fatalf("row = %+v", row)
	}
	locals, err := TokenizerBaselineLocals(ctx)
	if err != nil || len(locals) != 1 {
		t.Fatalf("locals = %+v, %v", locals, err)
	}
	b, _ := tokenbank.Builtin()
	for j, p := range b.Probes {
		if locals[0].Counts[p.ID] != counts[j] {
			t.Fatalf("count %s = %d, want %d", p.ID, locals[0].Counts[p.ID], counts[j])
		}
	}
	if _, err := b.WithLocal(locals); err != nil {
		t.Fatalf("stored baseline must merge: %v", err)
	}

	// 已有基线：不带 replace 拒绝，带 replace 覆盖同一行。
	id2 := seedRecording(t, ctx, "claude-x", syntheticCounts(t, 4), 0, model.AuditResultDone)
	if _, err := TokenizerBaselineFromResult(ctx, TokenizerBaselineFromResultInput{ResultID: id2}); !errors.Is(err, ErrTokenizerBaselineExists) {
		t.Fatalf("want ErrTokenizerBaselineExists, got %v", err)
	}
	replaced, err := TokenizerBaselineFromResult(ctx, TokenizerBaselineFromResultInput{ResultID: id2, Replace: true})
	if err != nil || replaced.ID != row.ID || replaced.SourceResultID != id2 {
		t.Fatalf("replace = %+v, %v", replaced, err)
	}
	all, _ := TokenizerBaselineList(ctx)
	if len(all) != 1 {
		t.Fatalf("replace must not add rows: %d", len(all))
	}

	if err := TokenizerBaselineDelete(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	if err := TokenizerBaselineDelete(ctx, row.ID); err == nil {
		t.Fatal("deleting twice should fail")
	}
}

func TestTokenizerBaselineFromResultRejects(t *testing.T) {
	ctx := setupRelayLogTest(t)
	cases := map[string]int64{
		"未完成":    seedRecording(t, ctx, "claude-x", syntheticCounts(t, 3), 0, model.AuditResultRunning),
		"不可用作基线": seedRecording(t, ctx, "claude-x", syntheticCounts(t, 3), 2, model.AuditResultDone),
	}
	// 没跑录制探针的报告。
	other := model.ModelAuditResult{JobID: 1, Model: "m", Status: model.AuditResultDone, Report: `{"results":[{"probe_id":"liveness","ok":true}]}`}
	if err := db.GetDB().Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	cases["录制分词器基线"] = other.ID
	// 计数被篡改成非整数。
	tampered := model.ModelAuditResult{JobID: 1, Model: "m", Status: model.AuditResultDone,
		Report: `{"results":[{"probe_id":"tokenizer_baseline","ok":true,"data":{"recordable":true,"counts":{"x":1.5}}}]}`}
	if err := db.GetDB().Create(&tampered).Error; err != nil {
		t.Fatal(err)
	}
	cases["不是整数"] = tampered.ID
	// 计数不全（库校验拒绝）。
	partial := model.ModelAuditResult{JobID: 1, Model: "m", Status: model.AuditResultDone,
		Report: `{"results":[{"probe_id":"tokenizer_baseline","ok":true,"data":{"recordable":true,"counts":{"my-prose":3}}}]}`}
	if err := db.GetDB().Create(&partial).Error; err != nil {
		t.Fatal(err)
	}
	cases[""] = partial.ID
	cases["不存在"] = 99999

	for want, id := range cases {
		_, err := TokenizerBaselineFromResult(ctx, TokenizerBaselineFromResultInput{ResultID: id})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("case %q: err = %v", want, err)
		}
	}
	if rows, _ := TokenizerBaselineList(ctx); len(rows) != 0 {
		t.Fatalf("rejected recordings must not be stored: %+v", rows)
	}
}
