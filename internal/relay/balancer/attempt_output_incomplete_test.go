package balancer

import (
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
)

// 标记必须落在 End 追加的那条 attempt 上: relay 的成功出口就在 End 之前调用它。
func TestSetOutputIncompleteReasonLandsOnEndedAttempt(t *testing.T) {
	group := model.Group{Items: []model.GroupItem{{ChannelID: 1, ModelName: "m", Priority: 0}}}
	it := NewIteratorWithSessionKey(group, 1, "m", "")
	if !it.Next() {
		t.Fatal("expected a candidate")
	}
	span := it.StartAttempt(1, 11, "ch-1")
	span.SetOutputIncompleteReason(model.RelayLogOutputIncompleteUpstreamMissingTerminal)
	span.End(model.AttemptSuccess, 200, "")

	attempts := it.Attempts()
	if len(attempts) != 1 {
		t.Fatalf("expected exactly 1 attempt, got %d", len(attempts))
	}
	got := attempts[0]
	if got.OutputIncompleteReason != model.RelayLogOutputIncompleteUpstreamMissingTerminal {
		t.Fatalf("reason = %q, want %q", got.OutputIncompleteReason, model.RelayLogOutputIncompleteUpstreamMissingTerminal)
	}
	// 纯观测: 状态、消息、通道身份一律不变。
	if got.Status != model.AttemptSuccess || got.Msg != "" || got.ChannelID != 1 || got.ChannelKeyID != 11 || got.ChannelName != "ch-1" {
		t.Fatalf("the mark must not touch anything else, got %#v", got)
	}
	if !model.RelayLogOutputIncomplete(attempts) {
		t.Fatal("the derived boolean must see the mark")
	}
}

// End 之后设置是空操作 (End 追加的是值拷贝), nil span 也必须安全。
func TestSetOutputIncompleteReasonAfterEndIsNoop(t *testing.T) {
	group := model.Group{Items: []model.GroupItem{{ChannelID: 2, ModelName: "m", Priority: 0}}}
	it := NewIteratorWithSessionKey(group, 1, "m", "")
	if !it.Next() {
		t.Fatal("expected a candidate")
	}
	span := it.StartAttempt(2, 22, "ch-2")
	span.End(model.AttemptSuccess, 200, "")
	span.SetOutputIncompleteReason(model.RelayLogOutputIncompleteUpstreamMissingTerminal)

	attempts := it.Attempts()
	if len(attempts) != 1 {
		t.Fatalf("expected exactly 1 attempt, got %d", len(attempts))
	}
	if attempts[0].OutputIncompleteReason != "" {
		t.Fatalf("a late set must be dropped, got %q", attempts[0].OutputIncompleteReason)
	}
	if model.RelayLogOutputIncomplete(attempts) {
		t.Fatal("a clean success must stay unflagged")
	}

	var nilSpan *AttemptSpan
	nilSpan.SetOutputIncompleteReason(model.RelayLogOutputIncompleteUpstreamMissingTerminal)
}
