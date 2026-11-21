package balancer

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// 熔断探测状态在既有 circuit_state.json 里的往返与兼容性。
//
// 文件版本仍是 1、原有键一个都没动，只多了一个可选的 "probe" 对象：旧文件读进来
// 就是 nil，新文件里多出来的键对旧读取器只是未知键。

func TestCircuitProbeStateSurvivesCheckpointRoundTrip(t *testing.T) {
	env := newProbeTestEnv(t)
	lastCheckpointBody = nil

	env.routeText(71, 1, "m-ok")
	env.openEntry(71, 1, "m-ok", "chat", "", 1, 0)
	env.routeText(72, 1, "m-bad")
	env.openEntry(72, 1, "m-bad", "chat", "stream", 1, 100*time.Millisecond)

	if _, ok := env.runOnce(CircuitProbeReport{Recovered: true, Outcome: "ok", Prompt: "12 + 30 = ?", Answer: "42", Duration: 1200 * time.Millisecond}); !ok {
		t.Fatal("the recovered probe must run")
	}
	if _, ok := env.runOnce(CircuitProbeReport{Attributable: true, StatusCode: 429, Outcome: "upstream_error", Answer: "no digits here"}); !ok {
		t.Fatal("the failing probe must run")
	}

	beforeHistory := SnapshotCircuitProbe(71).History
	beforeGlobal := SnapshotCircuitProbeGlobal()
	beforeTotals := beforeGlobal.Totals
	beforeDay := beforeGlobal.Day
	beforeStreak := probeStreakValue(circuitKey(72, 1, "m-bad"))
	if beforeTotals.Used != 2 || beforeTotals.Recovered != 1 || beforeTotals.Failed != 1 {
		t.Fatalf("totals before save = %+v", beforeTotals)
	}
	if beforeStreak != 1 {
		t.Fatalf("streak before save = %d, want 1", beforeStreak)
	}

	path := filepath.Join(t.TempDir(), "circuit_state.json")
	if err := SaveCheckpoint(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read checkpoint: %v", err)
	}
	if !bytes.Contains(data, []byte(`"probe"`)) {
		t.Fatalf("checkpoint must carry the probe block, got %s", data)
	}

	// 状态没变就不重写（探测快照里没有随当前时间变化的字段）。
	first, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := SaveCheckpoint(path); err != nil {
		t.Fatalf("second save: %v", err)
	}
	second, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !first.ModTime().Equal(second.ModTime()) {
		t.Fatal("an unchanged checkpoint must not be rewritten")
	}

	// 模拟重启：内存清空，检查点装回来。
	ResetCircuitProbe()
	globalBreaker = sync.Map{}
	globalHolds = sync.Map{}
	env.refresh()

	circuits, holds, err := LoadCheckpoint(path)
	if err != nil || circuits != 1 || holds != 0 {
		t.Fatalf("load = %d circuits, %d holds, %v; want 1, 0, nil", circuits, holds, err)
	}

	after := SnapshotCircuitProbeGlobal()
	if after.Day != beforeDay || after.Totals != beforeTotals {
		t.Fatalf("restored totals = %+v (day %s), want %+v (day %s)", after.Totals, after.Day, beforeTotals, beforeDay)
	}
	if got := probeStreakValue(circuitKey(72, 1, "m-bad")); got != beforeStreak {
		t.Fatalf("restored streak = %d, want %d", got, beforeStreak)
	}
	restoredHistory := SnapshotCircuitProbe(71).History
	if len(restoredHistory) != len(beforeHistory) {
		t.Fatalf("restored history rows = %d, want %d", len(restoredHistory), len(beforeHistory))
	}
	for i := range beforeHistory {
		if restoredHistory[i] != beforeHistory[i] {
			t.Fatalf("restored history row %d = %+v, want %+v", i, restoredHistory[i], beforeHistory[i])
		}
	}
	if len(restoredHistory) > 0 {
		if restoredHistory[0].Answer != "42" || restoredHistory[0].Result != "recovered" || restoredHistory[0].Prompt != "12 + 30 = ?" {
			t.Fatalf("restored history row = %+v", restoredHistory[0])
		}
	}
	channelTotals := SnapshotCircuitProbe(72).Totals
	if channelTotals.Failed != 1 {
		t.Fatalf("restored channel totals = %+v, want failed 1", channelTotals)
	}
}

func TestCircuitProbeCheckpointReadsLegacyFilesWithoutProbeBlock(t *testing.T) {
	env := newProbeTestEnv(t)
	lastCheckpointBody = nil

	path := filepath.Join(t.TempDir(), "circuit_state.json")
	legacy := fmt.Sprintf(`{"version":1,"saved_at_unix_ms":0,"circuits":[{"key":%q,"consecutive_failures":10,"last_failure_unix_ms":%d,"trip_count":1,"policy":{}}],"holds":[]}`,
		circuitKey(73, 1, "m"), env.clock.now.UnixMilli())
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy checkpoint: %v", err)
	}

	circuits, holds, err := LoadCheckpoint(path)
	if err != nil || circuits != 1 || holds != 0 {
		t.Fatalf("legacy load = %d circuits, %d holds, %v; want 1, 0, nil", circuits, holds, err)
	}
	// 旧文件没有 probe 键：探测状态保持干净（不是错误，也不是半截数据）。
	globalView := SnapshotCircuitProbeGlobal()
	if globalView.Totals != (CircuitProbeTotals{}) || globalView.Day != circuitProbeDay(env.clock.now) {
		t.Fatalf("probe state after a legacy load = %+v (day %s)", globalView.Totals, globalView.Day)
	}
	if got := probeStreakValue(circuitKey(73, 1, "m")); got != 0 {
		t.Fatalf("streak after a legacy load = %d, want 0", got)
	}
}

func TestScopedHoldMetadataSurvivesTheCheckpoint(t *testing.T) {
	env := newProbeTestEnv(t)
	lastCheckpointBody = nil

	ApplyHoldScoped(74, 1, "m", "model", 10*time.Minute, "model-not-found", "chat", "stream")
	path := filepath.Join(t.TempDir(), "circuit_state.json")
	if err := SaveCheckpoint(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	ResetCircuitProbe()
	globalBreaker = sync.Map{}
	globalHolds = sync.Map{}
	env.refresh()
	if _, holds, err := LoadCheckpoint(path); err != nil || holds != 1 {
		t.Fatalf("load holds = %d, %v; want 1, nil", holds, err)
	}

	// 作用域元数据跟着 hold 一起回来了：这条 hold 行仍然可探。
	env.routeText(74, 1, "m")
	entry, ok := env.entryView(74, 1, "m", "chat", "stream")
	if !ok || entry.Probe.State != CircuitProbeStateScheduled {
		t.Fatalf("restored scoped hold = %+v (found=%v), want scheduled", entry.Probe, ok)
	}
}
