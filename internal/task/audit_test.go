package task

import (
	"testing"
)

func TestScheduledTargetKeyKeepsChannelAndModelApart(t *testing.T) {
	left := scheduledTargetKey(1, "claude-opus-4-8")
	right := scheduledTargetKey(2, "claude-opus-4-8")
	if left == right {
		t.Fatal("different channels must not share a target key")
	}
}

// 任务注册表无自动检测任务：Init 的接线面证据由 init_wiring_test.go 的 AST 断言负责；
// 此处只断言本测试二进制的注册表内容，不真跑 Init。回显跟进不走注册表，
// 由 op 包运行时用例覆盖。
func TestTaskRegistryHasNoAutoAuditTasks(t *testing.T) {
	tasksMu.RLock()
	defer tasksMu.RUnlock()
	for name := range tasks {
		if name == TaskModelAuditQuick {
			t.Errorf("task registry must not contain auto audit task %q (disconnected)", TaskModelAuditQuick)
		}
	}
	// 正面钉一下注册表键的常量本身仍然存在且拼写稳定（供运维核对历史任务名），
	// 但不要求它被注册。
	if TaskModelAuditQuick != "model_audit_quick" {
		t.Fatalf("TaskModelAuditQuick constant changed: %q", TaskModelAuditQuick)
	}
}
