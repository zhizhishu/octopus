package op

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/log"
)

func withLogModeClock(t *testing.T, now time.Time) *time.Time {
	t.Helper()
	clock := now
	prev := logModeNow
	logModeNow = func() time.Time { return clock }
	prevLevel := log.GetLevel()
	t.Cleanup(func() {
		logModeNow = prev
		log.SetLevel(prevLevel)
	})
	return &clock
}

func setLogModeSetting(t *testing.T, key model.SettingKey, value string) {
	t.Helper()
	if err := (&model.Setting{Key: key, Value: value}).Validate(); err != nil {
		t.Fatalf("validate %s=%s: %v", key, value, err)
	}
	if err := SettingSetString(key, value); err != nil {
		t.Fatalf("set %s=%s: %v", key, value, err)
	}
	LogModeOnSettingChanged(key)
}

func TestLogModeDefaultsToNormalWarn(t *testing.T) {
	setupRelayLogTest(t)
	t.Setenv(logModeEnvLevel, "")
	withLogModeClock(t, time.Unix(1_800_000_000, 0))
	if mode, _ := SettingGetString(model.SettingKeyLogMode); mode != model.LogModeNormal {
		t.Fatalf("default log mode = %q, want normal", mode)
	}
	if hours, _ := SettingGetInt(model.SettingKeyLogDebugAutoOffHours); hours != 24 {
		t.Fatalf("default auto-off hours = %d, want 24", hours)
	}
	log.SetLevel("info")
	LogModeApply()
	if got := log.GetLevel(); got != "warn" {
		t.Fatalf("normal mode runtime level = %s, want warn", got)
	}
	if LogModeDebugActive() {
		t.Fatal("debug must be off by default")
	}
}

func TestLogModeExplicitEnvLevelWinsInNormalMode(t *testing.T) {
	setupRelayLogTest(t)
	withLogModeClock(t, time.Unix(1_800_000_000, 0))
	t.Setenv(logModeEnvLevel, "info")
	LogModeApply()
	if got := log.GetLevel(); got != "info" {
		t.Fatalf("explicit OCTOPUS_LOG_LEVEL must be honoured in normal mode, got %s", got)
	}
	t.Setenv(logModeEnvLevel, "nonsense")
	LogModeApply()
	if got := log.GetLevel(); got != "warn" {
		t.Fatalf("invalid env level must fall back to warn, got %s", got)
	}
}

func TestLogModeSwitchesLiveAndAutoTurnsOff(t *testing.T) {
	setupRelayLogTest(t)
	t.Setenv(logModeEnvLevel, "")
	start := time.Unix(1_800_000_000, 0)
	clock := withLogModeClock(t, start)

	setLogModeSetting(t, model.SettingKeyLogMode, model.LogModeDebug)
	if !LogModeDebugActive() {
		t.Fatal("debug must be active right after switching")
	}
	if got := log.GetLevel(); got != "debug" {
		t.Fatalf("debug mode runtime level = %s, want debug", got)
	}
	if got := LogDebugExpiresAt(); got != start.Unix()+24*3600 {
		t.Fatalf("expires at = %d, want %d", got, start.Unix()+24*3600)
	}

	// 改小时数：从开启时刻重新算，立即生效。
	setLogModeSetting(t, model.SettingKeyLogDebugAutoOffHours, "1")
	if got := LogDebugExpiresAt(); got != start.Unix()+3600 {
		t.Fatalf("expires at after change = %d, want %d", got, start.Unix()+3600)
	}

	*clock = start.Add(59 * time.Minute)
	LogModeApply()
	if !LogModeDebugActive() || log.GetLevel() != "debug" {
		t.Fatal("debug must stay on before the deadline")
	}

	*clock = start.Add(61 * time.Minute)
	if LogModeDebugActive() {
		t.Fatal("debug must read as off once the deadline has passed, even before the watcher runs")
	}
	LogModeApply()
	if mode, _ := SettingGetString(model.SettingKeyLogMode); mode != model.LogModeNormal {
		t.Fatalf("auto-off must switch the setting back to normal, got %q", mode)
	}
	if since, _ := SettingGetString(model.SettingKeyLogDebugSince); since != "0" {
		t.Fatalf("auto-off must clear the start time, got %q", since)
	}
	if got := log.GetLevel(); got != "warn" {
		t.Fatalf("auto-off must restore warn, got %s", got)
	}
}

func TestLogModeZeroHoursNeverAutoOff(t *testing.T) {
	setupRelayLogTest(t)
	start := time.Unix(1_800_000_000, 0)
	clock := withLogModeClock(t, start)
	setLogModeSetting(t, model.SettingKeyLogDebugAutoOffHours, "0")
	setLogModeSetting(t, model.SettingKeyLogMode, model.LogModeDebug)
	*clock = start.Add(30 * 24 * time.Hour)
	LogModeApply()
	if !LogModeDebugActive() {
		t.Fatal("auto-off hours = 0 must keep debug on")
	}
	setLogModeSetting(t, model.SettingKeyLogMode, model.LogModeNormal)
	if LogModeDebugActive() {
		t.Fatal("switching back to normal must take effect immediately")
	}
}

func TestLogModeDebugWithoutStartTimeGetsOne(t *testing.T) {
	setupRelayLogTest(t)
	start := time.Unix(1_800_000_000, 0)
	withLogModeClock(t, start)
	// 例如导入备份：只有 mode=debug，没有开始时间。
	if err := SettingSetString(model.SettingKeyLogMode, model.LogModeDebug); err != nil {
		t.Fatal(err)
	}
	LogModeApply()
	if got := LogDebugExpiresAt(); got != start.Unix()+24*3600 {
		t.Fatalf("a debug mode without start time must start counting now, expires=%d", got)
	}
}

func TestLogModeSettingValidation(t *testing.T) {
	bad := []model.Setting{
		{Key: model.SettingKeyLogMode, Value: "verbose"},
		{Key: model.SettingKeyLogMode, Value: ""},
		{Key: model.SettingKeyLogDebugAutoOffHours, Value: "-1"},
		{Key: model.SettingKeyLogDebugAutoOffHours, Value: "169"},
		{Key: model.SettingKeyLogDebugAutoOffHours, Value: "1.5"},
		{Key: model.SettingKeyLogDebugSince, Value: "-5"},
	}
	for _, s := range bad {
		if err := s.Validate(); err == nil {
			t.Fatalf("%s=%q must be rejected", s.Key, s.Value)
		}
	}
	good := []model.Setting{
		{Key: model.SettingKeyLogMode, Value: "normal"},
		{Key: model.SettingKeyLogMode, Value: "debug"},
		{Key: model.SettingKeyLogDebugAutoOffHours, Value: "0"},
		{Key: model.SettingKeyLogDebugAutoOffHours, Value: "168"},
	}
	for _, s := range good {
		if err := s.Validate(); err != nil {
			t.Fatalf("%s=%q must be accepted: %v", s.Key, s.Value, err)
		}
	}
}

func TestRelayLogDebugInfoRetentionClearsOnlyTheColumn(t *testing.T) {
	ctx := setupRelayLogTest(t)
	now := time.Unix(1_800_000_000, 0)
	withLogModeClock(t, now)
	info := &model.RelayLogDebugInfo{Calls: []model.RelayLogDebugCall{{ChannelID: 1, Status: 200}}}
	old := model.RelayLog{ID: 101, Time: now.Add(-4 * 24 * time.Hour).Unix(), RequestModelName: "m", DebugInfo: info}
	fresh := model.RelayLog{ID: 102, Time: now.Add(-1 * time.Hour).Unix(), RequestModelName: "m", DebugInfo: info}
	if err := db.GetDB().WithContext(ctx).Create(&[]model.RelayLog{old, fresh}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	relayLogClearExpiredDebugInfo(ctx)

	var rows []model.RelayLog
	if err := db.GetDB().WithContext(ctx).Order("id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows must be kept, got %d", len(rows))
	}
	if rows[0].DebugInfo != nil {
		t.Fatalf("debug info older than %d days must be cleared", model.RelayLogDebugRetentionDays)
	}
	if rows[1].DebugInfo == nil || len(rows[1].DebugInfo.Calls) != 1 {
		t.Fatal("fresh debug info must be kept")
	}
}

func TestRelayLogDebugInfoIsOmittedAndCounted(t *testing.T) {
	info := &model.RelayLogDebugInfo{Calls: []model.RelayLogDebugCall{{ChannelID: 1, URL: "http://x/v1/chat"}}}
	l := model.RelayLog{ID: 1, DebugInfo: info}
	if RelayLogOmitContent(l).DebugInfo != nil {
		t.Fatal("list payload must omit debug_info")
	}
	if RelayLogRedact(l).DebugInfo != nil {
		t.Fatal("redacted payload must drop debug_info")
	}
	raw, _ := json.Marshal(info)
	if relayLogApproxBytes(l)-relayLogApproxBytes(model.RelayLog{ID: 1}) != int64(len(raw)) {
		t.Fatal("storage accounting must include debug_info bytes")
	}
}
