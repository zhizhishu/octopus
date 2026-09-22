package op

import (
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/model"
)

func TestScheduledAuditCooldown(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	if ScheduledAuditOnCooldown(3, "claude-opus-4-8", now, 6*time.Hour) {
		t.Fatal("empty cooldown map must not block")
	}
	ScheduledAuditMarkHit(3, "claude-opus-4-8", now)
	if !ScheduledAuditOnCooldown(3, "claude-opus-4-8", now.Add(time.Hour), 6*time.Hour) {
		t.Fatal("same target should stay on cooldown")
	}
	if ScheduledAuditOnCooldown(3, "claude-opus-4-8", now.Add(7*time.Hour), 6*time.Hour) {
		t.Fatal("cooldown should expire")
	}
	if ScheduledAuditOnCooldown(4, "claude-opus-4-8", now.Add(time.Hour), 6*time.Hour) {
		t.Fatal("other channel must not inherit cooldown")
	}
}

func TestShouldTriggerAuditFollowUpOnlyOnEchoMismatch(t *testing.T) {
	yes := true
	no := false
	if ShouldTriggerAuditFollowUp(model.RelayLog{ChannelId: 1, RequestModelName: "claude-opus-4-8"}) {
		t.Fatal("undeclared echo must not follow up")
	}
	if ShouldTriggerAuditFollowUp(model.RelayLog{ChannelId: 1, RequestModelName: "claude-opus-4-8", UpstreamModelMismatch: &no}) {
		t.Fatal("matching echo must not follow up")
	}
	if !ShouldTriggerAuditFollowUp(model.RelayLog{ChannelId: 1, RequestModelName: "claude-opus-4-8", UpstreamModelMismatch: &yes}) {
		t.Fatal("mismatch should follow up")
	}
	if ShouldTriggerAuditFollowUp(model.RelayLog{
		ChannelId:             1,
		RequestModelName:      "claude-opus-4-8",
		UpstreamModelMismatch: &yes,
		ErrorCode:             model.RelayLogErrorCodeClientEmptyRequest,
	}) {
		t.Fatal("local empty request must not follow up")
	}
}

func TestScheduledAuditAppendKeepsNewestFirst(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	prev := ScheduledAuditGet()
	t.Cleanup(func() { ScheduledAuditStore(prev) })
	ScheduledAuditStore(model.ScheduledAuditSnapshot{})
	ScheduledAuditAppend(model.ScheduledAuditResult{ChannelID: 1, Model: "a", Trigger: "echo_mismatch"}, now)
	ScheduledAuditAppend(model.ScheduledAuditResult{ChannelID: 2, Model: "b", Trigger: "echo_mismatch"}, now.Add(time.Minute))
	snap := ScheduledAuditGet()
	if len(snap.Results) != 2 || snap.Results[0].ChannelID != 2 {
		t.Fatalf("newest follow-up should be first, got %#v", snap.Results)
	}
}
