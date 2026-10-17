package op

import (
	"context"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/model"
)

// 本文件是 N01 的运行时证据：日志写入路径（RelayLogAdd）不得触发行为检测跟进，
// 无论旧的 model_audit_followup_enabled / relay_log_keep_enabled 开关是什么值。
// 与 scheduled_audit_persist_test.go 的既有用例互补：这里覆盖 keep enabled 两档
// × 普通/不一致(true)/错误 三类日志行。
//
// 限度说明（供主管定）：跟进回调经 safe.SafeGo 异步派发（见 maybeTriggerAuditFollowUp），
// 「没有调用」是否定式断言，只能用有界等待取界限。本文件沿用同包既有用例的
// 300ms 等待窗口：窗口内回调计数为 0 即通过。这是必要条件而非全时证明；
// 同步路径下若回调被同步调用，channel 会立即命中并使测试失败，不受窗口影响。

// followUpCounter 是原子的假回调计数器：不靠源码字符串，用真实调用计数。
type followUpCounter struct {
	calls chan struct{}
}

func newFollowUpCounter() *followUpCounter {
	return &followUpCounter{calls: make(chan struct{}, 64)}
}

func (c *followUpCounter) fn(channelID int, modelName string, logID int64) {
	c.calls <- struct{}{}
}

// drain 清空计数并返回是否收到过调用。
func (c *followUpCounter) invokedWithin(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-c.calls:
		return true
	case <-timer.C:
		return false
	}
}

// captureSetting restores only this test's temporary setting override.
func captureSetting(t *testing.T, key model.SettingKey) {
	t.Helper()
	value, err := SettingGetString(key)
	if err != nil {
		t.Fatalf("read setting %s: %v", key, err)
	}
	t.Cleanup(func() {
		if err := SettingSetString(key, value); err != nil {
			t.Errorf("restore setting %s: %v", key, err)
		}
	})
}

// TestRelayLogAddNeverTriggersFollowUpAllRows 遍历 keep enabled false/true 两档、
// 普通/回显不一致(true)/错误 三类日志行：即便旧 followup 开关显式残留 true，
// RelayLogAdd 也必须零回调（有界窗口辅证，非全时证明）。
func TestRelayLogAddNeverTriggersFollowUpAllRows(t *testing.T) {
	setupRelayLogTest(t)
	scheduledAuditMu.RLock()
	previousFollowUp := scheduledAuditFollowUp
	scheduledAuditMu.RUnlock()
	t.Cleanup(func() { SetScheduledAuditFollowUp(previousFollowUp) })

	// 旧开关显式置 true：旧实现里这个组合必触发跟进；现接线已断，必须零调用。
	// cleanup 恢复测试前的值，不强制写 false。
	captureSetting(t, model.SettingKeyModelAuditFollowUpEnabled)
	if err := SettingSetString(model.SettingKeyModelAuditFollowUpEnabled, "true"); err != nil {
		t.Fatalf("set model_audit_followup_enabled=true: %v", err)
	}

	mismatch := true
	rows := map[string]model.RelayLog{
		"ordinary": {
			ChannelId:        7,
			RequestModelName: "claude-opus-4-8",
			ActualModelName:  "claude-opus-4-8",
		},
		"mismatch_true": {
			ChannelId:             7,
			RequestModelName:      "claude-opus-4-8",
			ActualModelName:       "deepseek-v3",
			UpstreamModelMismatch: &mismatch,
		},
		"error": {
			ChannelId:        7,
			RequestModelName: "claude-opus-4-8",
			ErrorCode:        "upstream_error",
			Error:            "upstream exploded",
		},
	}

	for _, keepLogs := range []string{"false", "true"} {
		captureSetting(t, model.SettingKeyRelayLogKeepEnabled)
		if err := SettingSetString(model.SettingKeyRelayLogKeepEnabled, keepLogs); err != nil {
			t.Fatalf("set relay log persistence=%s: %v", keepLogs, err)
		}

		for name, row := range rows {
			counter := newFollowUpCounter()
			SetScheduledAuditFollowUp(counter.fn)

			if err := RelayLogAdd(context.Background(), row); err != nil {
				t.Fatalf("RelayLogAdd(%s, persistence=%s): %v", name, keepLogs, err)
			}

			// 否定式断言的有界等待（辅证，非全时证明）。
			if counter.invokedWithin(300 * time.Millisecond) {
				t.Fatalf("RelayLogAdd must not invoke audit follow-up (row=%s, persistence=%s)", name, keepLogs)
			}
		}
	}
}
