package op

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

func mustStopBody(content string) string {
	msg := map[string]any{"role": "assistant", "content": content}
	choice := map[string]any{"message": msg, "finish_reason": "stop"}
	b, _ := json.Marshal(map[string]any{"choices": []any{choice}})
	return string(b)
}

func TestComputePassiveSummaryValidReply(t *testing.T) {
	body := mustStopBody("你好 hello\n\n# Title\n- item\n```go\ncode\n```\n")
	s := computePassiveSummary(model.RelayLog{ResponseContent: body})
	if s == nil || s.Status != model.PassiveStatusInsufficientEvidence || s.Reason != "no_calibrated_profile" {
		t.Fatalf("want insufficient_evidence/no_calibrated_profile, got %#v", s)
	}
	if s.Version != "v1" || s.Features == nil {
		t.Fatalf("want v1 + features, got %#v", s)
	}
	// Seven newline characters produce eight lines, including the trailing empty line.
	if s.Features.Chars != 40 || s.Features.Paragraphs != 2 || s.Features.CodeBlocks != 1 ||
		s.Features.Lines != 8 || s.Features.MarkdownLines != 2 || s.Features.CodeRatio != 0.375 {
		t.Fatalf("unexpected features: %#v", s.Features)
	}
	if s.Language != "en" {
		t.Fatalf("want en, got %q", s.Language)
	}
}

func TestComputePassiveSummaryMultipleChoicesRejected(t *testing.T) {
	body := `{"choices":[{"message":{"content":"a"},"finish_reason":"stop"},{"message":{"content":"b"},"finish_reason":"stop"}]}`
	s := computePassiveSummary(model.RelayLog{ResponseContent: body})
	if s.Status != model.PassiveStatusNotApplicable || s.Reason != model.PassiveReasonMultipleChoices {
		t.Fatalf("want multiple_choices, got %#v", s)
	}
	if s.Features != nil {
		t.Fatal("not_applicable must not carry features")
	}
}

func TestComputePassiveSummaryRoleAndNonTextGuards(t *testing.T) {
	mk := func(message string) string {
		return `{"choices":[{"message":` + message + `,"finish_reason":"stop"}]}`
	}
	// tool 角色 → non_text。
	s := computePassiveSummary(model.RelayLog{ResponseContent: mk(`{"role":"tool","content":"result"}`)})
	if s.Reason != model.PassiveReasonNonText {
		t.Fatalf("tool role must be non_text, got %#v", s)
	}
	// 缺 role 保守 non_text。
	s = computePassiveSummary(model.RelayLog{ResponseContent: mk(`{"content":"no role"}`)})
	if s.Reason != model.PassiveReasonNonText {
		t.Fatalf("missing role must be non_text, got %#v", s)
	}
	// legacy function_call 即使另有文本也拒绝。
	s = computePassiveSummary(model.RelayLog{ResponseContent: mk(`{"role":"assistant","content":"txt","function_call":{"name":"f"}}`)})
	if s.Reason != model.PassiveReasonNonText {
		t.Fatalf("legacy function_call must be non_text, got %#v", s)
	}
	// audio 部件即使另有 text 部件也拒绝。
	s = computePassiveSummary(model.RelayLog{ResponseContent: mk(`{"role":"assistant","content":[{"type":"text","text":"hi"},{"type":"input_audio","audio":"x"}]}`)})
	if s.Reason != model.PassiveReasonNonText {
		t.Fatalf("audio part must be non_text, got %#v", s)
	}
	// content 为数字/对象/坏数组 → 保守 non_text。
	for _, c := range []string{
		mk(`{"role":"assistant","content":42}`),
		mk(`{"role":"assistant","content":{"weird":true}}`),
		mk(`{"role":"assistant","content":[{"type":123}]}`),
	} {
		s = computePassiveSummary(model.RelayLog{ResponseContent: c})
		if s.Reason != model.PassiveReasonNonText {
			t.Fatalf("odd content shape must be non_text: %s got %#v", c, s)
		}
	}
	// 空白文本仍是 blank, 不冒充有效。
	s = computePassiveSummary(model.RelayLog{ResponseContent: mk(`{"role":"assistant","content":"   \n\t "}`)})
	if s.Reason != model.PassiveReasonBlank {
		t.Fatalf("whitespace content must be blank, got %#v", s)
	}
}

func TestComputePassiveSummaryUnknownFinish(t *testing.T) {
	for _, body := range []string{
		`{"choices":[{"message":{"content":"a"}}]}`,
		`{"choices":[{"message":{"content":"a"},"finish_reason":"length"}]}`,
	} {
		s := computePassiveSummary(model.RelayLog{ResponseContent: body})
		if s.Reason != model.PassiveReasonUnknownFinish {
			t.Fatalf("want unknown_finish for %s, got %#v", body, s)
		}
	}
}

func TestComputePassiveSummaryErrorAndCanceled(t *testing.T) {
	s := computePassiveSummary(model.RelayLog{ErrorStatus: 500, ResponseContent: mustStopBody("x")})
	if s.Reason != model.PassiveReasonError {
		t.Fatalf("want error, got %#v", s)
	}
	// 取消先于 generic error 判。
	s = computePassiveSummary(model.RelayLog{
		ErrorCode:       "octopus_client_canceled",
		ErrorStatus:     499,
		ResponseContent: mustStopBody("x"),
	})
	if s.Reason != model.PassiveReasonCanceled {
		t.Fatalf("want canceled before generic error, got %#v", s)
	}
}

func TestComputePassiveSummaryToolCallsAndRefusal(t *testing.T) {
	s := computePassiveSummary(model.RelayLog{ResponseContent: `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c"}]},"finish_reason":"stop"}]}`})
	if s.Reason != model.PassiveReasonToolOutput {
		t.Fatalf("want tool_output, got %#v", s)
	}
	s = computePassiveSummary(model.RelayLog{ResponseContent: `{"choices":[{"message":{"role":"assistant","content":"","refusal":"no"},"finish_reason":"stop"}]}`})
	if s.Reason != model.PassiveReasonToolOutput {
		t.Fatalf("want refusal->tool_output, got %#v", s)
	}
}

func TestComputePassiveSummaryNonTextParts(t *testing.T) {
	body := `{"choices":[{"message":{"content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"x"}}]},"finish_reason":"stop"}]}`
	s := computePassiveSummary(model.RelayLog{ResponseContent: body})
	if s.Reason != model.PassiveReasonNonText {
		t.Fatalf("want non_text, got %#v", s)
	}
	// 全 text 部件合并有效(补 role=assistant)。
	body = `{"choices":[{"message":{"role":"assistant","content":[{"type":"text","text":"part1"},{"type":"text","text":"part2"}]},"finish_reason":"stop"}]}`
	s = computePassiveSummary(model.RelayLog{ResponseContent: body})
	if s.Status != model.PassiveStatusInsufficientEvidence || s.Features == nil {
		t.Fatalf("all-text parts must merge, got %#v", s)
	}
}

func TestComputePassiveSummaryOversize(t *testing.T) {
	// 文本 >32KiB: 整块跳过, 不静默截断。
	envelope := `{"choices":[{"message":{"role":"assistant","content":"` + strings.Repeat("字", 33*1024) + `"},"finish_reason":"stop"}]}`
	s := computePassiveSummary(model.RelayLog{ResponseContent: envelope})
	if s.Reason != model.PassiveReasonOversizedText {
		t.Fatalf("want oversized_text, got %#v", s)
	}
	// 信封 >128KiB: 直接跳过。
	s = computePassiveSummary(model.RelayLog{ResponseContent: `{"choices":[{"message":{"content":"` + strings.Repeat("a", 129*1024) + `"},"finish_reason":"stop"}]}`})
	if s.Reason != model.PassiveReasonOversizedEnvelope {
		t.Fatalf("want oversized_envelope, got %#v", s)
	}
}

func TestComputePassiveSummaryBlankUnparseableOutOfRange(t *testing.T) {
	cases := []struct {
		relay  model.RelayLog
		reason string
	}{
		{model.RelayLog{ResponseContent: "   "}, model.PassiveReasonBlank},
		{model.RelayLog{ResponseContent: "not json"}, model.PassiveReasonUnparseable},
		{model.RelayLog{RequestEndpoint: "model_test_chat", ResponseContent: mustStopBody("ok")}, model.PassiveReasonOutOfRange},
		{model.RelayLog{RequestEndpoint: "embeddings_create", ResponseContent: mustStopBody("ok")}, model.PassiveReasonOutOfRange},
		{model.RelayLog{RequestEndpoint: "images_generations", ResponseContent: mustStopBody("ok")}, model.PassiveReasonOutOfRange},
	}
	for _, c := range cases {
		s := computePassiveSummary(c.relay)
		if s.Status != model.PassiveStatusNotApplicable || s.Reason != c.reason {
			t.Fatalf("want %s, got %#v", c.reason, s)
		}
	}
}

func TestComputePassiveSummaryLanguageLabels(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"这是一段足够长的中文文本，用来确认汉字计数占据主导地位。", "zh"},
		{"This is an English sentence with enough latin letters to dominate.", "en"},
		{"这是一段中英混排的文本，english words appear often enough here to count.", "mixed"},
		{"123456 !!! ...", "other"},
	}
	for _, c := range cases {
		s := computePassiveSummary(model.RelayLog{ResponseContent: mustStopBody(c.text)})
		if s == nil || s.Language != c.want {
			t.Fatalf("text %q: want %s, got %#v", c.text, c.want, s)
		}
	}
}

func TestComputePassiveSummaryDoesNotMutateInput(t *testing.T) {
	body := mustStopBody("稳定正文")
	relay := model.RelayLog{ResponseContent: body, InputTokens: 7}
	_ = computePassiveSummary(relay)
	if relay.ResponseContent != body || relay.InputTokens != 7 || relay.PassiveSummary != nil {
		t.Fatalf("input must not be mutated: %#v", relay)
	}
}

func TestScanLogAnomaliesPassiveBucketsNoMixing(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	nowU := now.Unix()
	logs := []model.RelayLog{
		{ID: 1, Time: nowU, ChannelId: 5, ActualModelName: "m-a", RequestModelName: "alias",
			RequestEndpoint: "chat_completions", IsStream: false,
			OutputTokens: 10, UseTime: 100,
			PassiveSummary: &model.RelayLogPassiveSummary{Version: "v1",
				Status: model.PassiveStatusInsufficientEvidence, Reason: "no_calibrated_profile",
				Language: "zh", Features: &model.RelayLogPassiveFeatures{Chars: 10}}},
		{ID: 2, Time: nowU, ChannelId: 6, ActualModelName: "m-b", RequestModelName: "alias",
			RequestEndpoint: "chat_completions", IsStream: false,
			OutputTokens: 10, UseTime: 100,
			PassiveSummary: &model.RelayLogPassiveSummary{Version: "v1",
				Status: model.PassiveStatusInsufficientEvidence, Reason: "no_calibrated_profile",
				Language: "zh", Features: &model.RelayLogPassiveFeatures{Chars: 10}}},
		// endpoint/is_stream 不同 → 不混桶。
		{ID: 3, Time: nowU, ChannelId: 5, ActualModelName: "m-a", RequestModelName: "alias",
			RequestEndpoint: "chat_completions", IsStream: true,
			OutputTokens: 10, UseTime: 100,
			PassiveSummary: &model.RelayLogPassiveSummary{Version: "v1",
				Status: model.PassiveStatusInsufficientEvidence, Reason: "no_calibrated_profile",
				Language: "zh", Features: &model.RelayLogPassiveFeatures{Chars: 10}}},
		// not_applicable 无 features → 不计。
		{ID: 4, Time: nowU, ChannelId: 5, ActualModelName: "m-a", RequestEndpoint: "chat_completions",
			OutputTokens: 10, UseTime: 100,
			PassiveSummary: &model.RelayLogPassiveSummary{Version: "v1",
				Status: model.PassiveStatusNotApplicable, Reason: model.PassiveReasonError}},
		// 旧 nil 行不计。
		{ID: 5, Time: nowU, ChannelId: 5, ActualModelName: "m-a", RequestEndpoint: "chat_completions",
			OutputTokens: 10, UseTime: 100},
	}
	report := ScanLogAnomalies(logs, now.Add(time.Second))
	if len(report.PassiveObservations) != 3 {
		t.Fatalf("want 3 passive buckets, got %#v", report.PassiveObservations)
	}
	for _, b := range report.PassiveObservations {
		if b.Count != 1 || b.NoProfileCount != 1 {
			t.Fatalf("bucket %#v: each distinct row counts once (no mixing)", b)
		}
	}
}

// 同 ID 重复的去重只发生在 logAnomalyLogs 的 DB/缓存合并层(按 ID); 直接传给
// ScanLogAnomalies 的切片不去重——本测试如实记录该行为: 重复行各计一次,
// 并发重复样本由调用方(合并层)负责, 不在扫描器里兜底。
func TestScanLogAnomaliesDuplicateIDDedupeLivesInLogAnomalyLogs(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	dup := model.RelayLog{ID: 42, Time: now.Unix(), ChannelId: 1, ActualModelName: "m",
		RequestEndpoint: "chat_completions", OutputTokens: 10, UseTime: 100,
		PassiveSummary: &model.RelayLogPassiveSummary{Version: "v1",
			Status: model.PassiveStatusInsufficientEvidence, Reason: "no_calibrated_profile",
			Language: "en", Features: &model.RelayLogPassiveFeatures{Chars: 3}}}
	logs := []model.RelayLog{dup, dup, dup}
	report := ScanLogAnomalies(logs, now.Add(time.Second))
	found := false
	for _, b := range report.PassiveObservations {
		found = true
		if b.Count != 3 {
			t.Fatalf("ScanLogAnomalies does not dedupe its input: want count=3, got %d", b.Count)
		}
	}
	if !found {
		t.Fatal("expected one passive bucket")
	}
}

func TestScanLogAnomaliesAliasVariantExcludedFromMismatchRisk(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	yes := true
	logs := make([]model.RelayLog, 0, 8)
	for i := 0; i < 8; i++ {
		logs = append(logs, model.RelayLog{
			ID: int64(i + 1), Time: now.Unix(), ChannelId: 7, ActualModelName: "m",
			RequestEndpoint: "chat_completions", OutputTokens: 80, UseTime: 400,
			UpstreamResponseModel: "claude-variant-x",
			UpstreamModelMismatch: &yes,
			UpstreamMismatchCode:  "alias_variant",
		})
	}
	report := ScanLogAnomalies(logs, now.Add(time.Second))
	for _, f := range report.Findings {
		if f.Code == "echo_mismatch_rate" {
			t.Fatal("alias_variant must not count toward echo_mismatch_rate risk")
		}
	}
	// 原始事实保留: 该行 mismatch 本值仍是 true。
	if *logs[0].UpstreamModelMismatch != true || logs[0].UpstreamMismatchCode != "alias_variant" {
		t.Fatal("row's original mismatch facts must be preserved")
	}
}

// 摘要只带六个结构化数值, 不带任何业务正文: OmitContent/Redact 清空正文后,
// 摘要仍不携带原文片段。
func TestPassiveSummaryCarriesNoBodyText(t *testing.T) {
	secret := "SECRET-PLAINTEXT-投影应该绝不出现在摘要里"
	s := computePassiveSummary(model.RelayLog{ResponseContent: mustStopBody(secret)})
	if s == nil || s.Features == nil {
		t.Fatalf("expected valid summary, got %#v", s)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal summary: %v", err)
	}
	// 顶层字段只允许 version/status/reason/language/features。
	for k := range got {
		switch k {
		case "version", "status", "reason", "language", "features":
		default:
			t.Fatalf("unexpected top-level field %q in summary", k)
		}
	}
	// features 只允许六个数值键。
	f, _ := got["features"].(map[string]any)
	wantKeys := map[string]bool{"chars": true, "lines": true, "paragraphs": true,
		"code_blocks": true, "code_ratio": true, "markdown_lines": true}
	if len(f) != len(wantKeys) {
		t.Fatalf("features must have exactly 6 numeric fields, got %v", f)
	}
	for k := range f {
		if !wantKeys[k] {
			t.Fatalf("unexpected feature field %q", k)
		}
	}
	// 正文绝不出现在序列化摘要里。
	if strings.Contains(string(b), secret) {
		t.Fatal("summary must never carry body text")
	}
	// OmitContent/Redact 后摘要仍在且仍无正文。
	relay := model.RelayLog{ResponseContent: mustStopBody(secret), PassiveSummary: s}
	redacted := RelayLogRedact(RelayLogOmitContent(relay))
	if redacted.ResponseContent != "" || redacted.PassiveSummary == nil ||
		redacted.PassiveSummary.Features == nil {
		t.Fatalf("summary must survive omit/redact with features intact: %#v", redacted.PassiveSummary)
	}
	redactedJSON, _ := json.Marshal(redacted.PassiveSummary)
	if strings.Contains(string(redactedJSON), secret) {
		t.Fatal("redacted row's summary must not carry body text")
	}
}

// AutoMigrate 建表后 RelayLog 摘要列写/读往返一致; 旧行(nil)照常读回。
func TestRelayLogPassiveSummaryDBRoundTrip(t *testing.T) {
	ctx := setupRelayLogTest(t)

	summary := &model.RelayLogPassiveSummary{
		Version: "v1", Status: model.PassiveStatusInsufficientEvidence,
		Reason: "no_calibrated_profile", Language: "zh",
		Features: &model.RelayLogPassiveFeatures{Chars: 12, Lines: 2, Paragraphs: 1,
			CodeBlocks: 1, CodeRatio: 0.5, MarkdownLines: 1},
	}
	row := model.RelayLog{ID: 9000001, RequestModelName: "rt", ResponseContent: "body",
		PassiveSummary: summary}
	if err := db.GetDB().WithContext(ctx).Create(&row).Error; err != nil {
		t.Fatalf("create row: %v", err)
	}
	var got model.RelayLog
	if err := db.GetDB().WithContext(ctx).First(&got, 9000001).Error; err != nil {
		t.Fatalf("read row: %v", err)
	}
	if got.PassiveSummary == nil || got.PassiveSummary.Features == nil ||
		got.PassiveSummary.Features.Chars != 12 || got.PassiveSummary.Language != "zh" ||
		got.PassiveSummary.Version != "v1" {
		t.Fatalf("summary round-trip mismatch: %#v", got.PassiveSummary)
	}

	legacy := model.RelayLog{ID: 9000002, RequestModelName: "legacy"}
	if err := db.GetDB().WithContext(ctx).Create(&legacy).Error; err != nil {
		t.Fatalf("create legacy row: %v", err)
	}
	var gotLegacy model.RelayLog
	if err := db.GetDB().WithContext(ctx).First(&gotLegacy, 9000002).Error; err != nil {
		t.Fatalf("read legacy row: %v", err)
	}
	if gotLegacy.PassiveSummary != nil {
		t.Fatalf("legacy row must read back nil summary, got %#v", gotLegacy.PassiveSummary)
	}
}
