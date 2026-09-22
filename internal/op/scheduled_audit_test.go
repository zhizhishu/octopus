package op

import (
	"testing"
	"time"
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
