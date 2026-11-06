package model

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// 只认「最终选中的那条成功尝试」。这条规则必须与 relay.finalChannel 的倒序扫描一致,
// 否则竞速输家 / 早先一条被标记的尝试会把整行标成输出不完整。
func TestRelayLogOutputIncompleteUsesFinalSelectedSuccess(t *testing.T) {
	marked := ChannelAttempt{ChannelID: 2, Status: AttemptSuccess, OutputIncompleteReason: RelayLogOutputIncompleteUpstreamMissingTerminal}
	clean := ChannelAttempt{ChannelID: 3, Status: AttemptSuccess}
	failedMarked := ChannelAttempt{ChannelID: 4, Status: AttemptFailed, Msg: "upstream 500", OutputIncompleteReason: RelayLogOutputIncompleteUpstreamMissingTerminal}
	skippedMarked := ChannelAttempt{ChannelID: 5, Status: AttemptSkipped, OutputIncompleteReason: RelayLogOutputIncompleteUpstreamMissingTerminal}
	racedOutMarked := ChannelAttempt{ChannelID: 6, Status: AttemptFailed, Msg: AttemptMsgRacedOut, OutputIncompleteReason: RelayLogOutputIncompleteUpstreamMissingTerminal}
	unknownReason := ChannelAttempt{ChannelID: 7, Status: AttemptSuccess, OutputIncompleteReason: "some_future_reason"}

	cases := []struct {
		name     string
		attempts []ChannelAttempt
		want     bool
	}{
		{"empty", nil, false},
		{"clean single success", []ChannelAttempt{clean}, false},
		{"marked single success", []ChannelAttempt{marked}, true},
		{"marked failed attempt is not success", []ChannelAttempt{failedMarked}, false},
		{"marked skipped attempt is not success", []ChannelAttempt{skippedMarked}, false},
		{"marked raced-out loser only", []ChannelAttempt{racedOutMarked}, false},
		{"marked then clean success (final wins)", []ChannelAttempt{marked, clean}, false},
		{"marked failed then clean success", []ChannelAttempt{failedMarked, clean}, false},
		{"raced-out marked next to clean winner", []ChannelAttempt{racedOutMarked, clean}, false},
		{"raced-out marked next to marked winner", []ChannelAttempt{racedOutMarked, marked}, true},
		{"unknown reason is ignored", []ChannelAttempt{unknownReason}, false},
		{"marked success then later failure", []ChannelAttempt{marked, failedMarked}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RelayLogOutputIncomplete(tc.attempts); got != tc.want {
				t.Fatalf("RelayLogOutputIncomplete(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// 没有任何 output_incomplete 列: 标记只活在被序列化的 attempts JSON 里, 读回时由
// AfterFind 重算。这条用例就是「落库 → 读回」的无数据库替身。
func TestRelayLogOutputIncompleteSurvivesAttemptsJSONReload(t *testing.T) {
	persisted := RelayLog{
		ID:   4242,
		Time: 1_800_000_000,
		Attempts: []ChannelAttempt{
			{ChannelID: 1, Status: AttemptFailed, Msg: "upstream 500"},
			{ChannelID: 2, Status: AttemptSuccess, OutputIncompleteReason: RelayLogOutputIncompleteUpstreamMissingTerminal},
		},
		TotalAttempts: 2,
	}
	raw, err := json.Marshal(persisted)
	if err != nil {
		t.Fatalf("marshal relay log: %v", err)
	}
	if !strings.Contains(string(raw), `"output_incomplete_reason":"upstream_missing_terminal"`) {
		t.Fatalf("the mark must live in the attempts JSON, got %s", raw)
	}
	if strings.Contains(string(raw), `"output_incomplete":true`) {
		t.Fatalf("an underived row must not pretend to be flagged: %s", raw)
	}

	var hydrated RelayLog
	if err := json.Unmarshal(raw, &hydrated); err != nil {
		t.Fatalf("unmarshal relay log: %v", err)
	}
	if hydrated.OutputIncomplete {
		t.Fatalf("the boolean is derived, never transported: %#v", hydrated.OutputIncomplete)
	}
	if err := hydrated.AfterFind(nil); err != nil {
		t.Fatalf("AfterFind: %v", err)
	}
	if !hydrated.OutputIncomplete {
		t.Fatalf("AfterFind must re-derive the flag from the persisted attempts, got attempts=%#v", hydrated.Attempts)
	}
	if len(hydrated.Attempts) != 2 || hydrated.Attempts[1].OutputIncompleteReason != RelayLogOutputIncompleteUpstreamMissingTerminal {
		t.Fatalf("the per-attempt reason must round-trip verbatim, got %#v", hydrated.Attempts)
	}
	if hydrated.Attempts[1].Status != AttemptSuccess {
		t.Fatalf("the mark must not change the attempt status, got %q", hydrated.Attempts[1].Status)
	}

	// 用户摘要路径只看得到这个中性布尔量。
	summary := RelayLogUserSummary{OutputIncomplete: hydrated.OutputIncomplete}
	if !summary.OutputIncomplete {
		t.Fatalf("the summary must be able to carry the flag")
	}
}

// 契约: 这个字段永不建列。有列就等于新增 schema, 与「不新增列/不迁移」相冲突。
func TestRelayLogOutputIncompleteHasNoDBColumn(t *testing.T) {
	field, ok := reflect.TypeOf(RelayLog{}).FieldByName("OutputIncomplete")
	if !ok {
		t.Fatal("RelayLog.OutputIncomplete is required for the summary path")
	}
	if tag := field.Tag.Get("gorm"); tag != "-" {
		t.Fatalf(`RelayLog.OutputIncomplete must be gorm:"-", got %q`, tag)
	}
	if tag := field.Tag.Get("json"); tag != "output_incomplete,omitempty" {
		t.Fatalf("unexpected json tag %q", tag)
	}
	reason, ok := reflect.TypeOf(ChannelAttempt{}).FieldByName("OutputIncompleteReason")
	if !ok {
		t.Fatal("ChannelAttempt.OutputIncompleteReason is required")
	}
	if tag := reason.Tag.Get("json"); tag != "output_incomplete_reason,omitempty" {
		t.Fatalf("unexpected json tag %q", tag)
	}
}
