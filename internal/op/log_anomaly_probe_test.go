package op

import (
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
)

// 探针汇总的测试重点在**分母**: 只给命中数会让人以为全部日志都出问题,
// 而分母算错比不打分更糟——它会让真实命中率看起来被人为稀释或放大。

func probeLog(channelID int, modelName, resp string, findingsCost int) model.RelayLog {
	findings := make([]model.RelayProbeFinding, 0, findingsCost)
	for i := 0; i < findingsCost; i++ {
		findings = append(findings, model.RelayProbeFinding{
			Code:     model.ProbeCodeCacheExceedsPrompt,
			Severity: model.RelayProbeSeverityMedium,
			Title:    "缓存超过总输入",
		})
	}
	return model.RelayLog{
		ChannelId:       channelID,
		ActualModelName: modelName,
		ChannelName:     "测试渠道",
		Time:            time.Now().Unix(),
		InputTokens:     100,
		OutputTokens:    10,
		ResponseContent: resp,
		PassiveProbes:   findings,
	}
}

// TestScanLogAnomaliesProbeDenominatorCountsEachLogOnce 守一个我实际写错过的 bug:
// 分母不能按"这条日志命中了几个码"重复累加, 必须一条日志只计一次。
func TestScanLogAnomaliesProbeDenominatorCountsEachLogOnce(t *testing.T) {
	logs := []model.RelayLog{
		// 这条命中同一种码三次(模拟一条日志产出多条同码结论), 分母仍只该是 1。
		probeLog(1, "claude-opus-4-6", chatBody("ok"), 3),
		probeLog(1, "claude-opus-4-6", chatBody("ok"), 0),
		probeLog(1, "claude-opus-4-6", chatBody("ok"), 0),
	}
	report := ScanLogAnomalies(logs, time.Now())
	if len(report.ProbeFindings) != 1 {
		t.Fatalf("应聚合成 1 个探针桶, 实际 %d 个", len(report.ProbeFindings))
	}
	b := report.ProbeFindings[0]
	if b.Count != 3 {
		t.Fatalf("命中数应为 3, 实际 %d", b.Count)
	}
	if b.Total != 3 {
		t.Fatalf("分母应为 3 条日志(不是按命中数累加的 3 也不是更大), 实际 %d", b.Total)
	}
}

// TestScanLogAnomaliesProbeExcludesErroredLogs 失败请求不进分母。
//
// 错误请求本来就没被检查过, 算进分母会让命中率显得比实际低, 真问题被稀释。
func TestScanLogAnomaliesProbeExcludesErroredLogs(t *testing.T) {
	bad := probeLog(1, "claude-opus-4-6", chatBody("ok"), 1)
	bad.Error = "upstream 500"
	bad.ErrorStatus = 500
	logs := []model.RelayLog{
		probeLog(1, "claude-opus-4-6", chatBody("ok"), 1),
		bad,
	}
	report := ScanLogAnomalies(logs, time.Now())
	if len(report.ProbeFindings) != 1 {
		t.Fatalf("应聚合 1 个桶, 实际 %d", len(report.ProbeFindings))
	}
	if report.ProbeFindings[0].Total != 1 {
		t.Fatalf("失败请求不该进分母, 分母应为 1, 实际 %d", report.ProbeFindings[0].Total)
	}
}

// TestScanLogAnomaliesProbeExcludesEmptyBodies 没有任何正文的旧行不进分母。
func TestScanLogAnomaliesProbeExcludesEmptyBodies(t *testing.T) {
	empty := probeLog(1, "claude-opus-4-6", "", 1)
	logs := []model.RelayLog{empty}
	report := ScanLogAnomalies(logs, time.Now())
	// 有结论就该输出桶, 但分母不能因为"没有正文"而虚增。
	for _, b := range report.ProbeFindings {
		if b.Total != 0 {
			t.Fatalf("无正文的日志不该进分母, 实际 %d", b.Total)
		}
	}
}

// TestScanLogAnomaliesProbeMediumSortsFirst medium 必须排在 low 前面。
//
// 界面把"确定的硬问题"和"擦边线索"混排会让前者失去分量。
func TestScanLogAnomaliesProbeMediumSortsFirst(t *testing.T) {
	low := probeLog(1, "m1", chatBody("ok"), 1)
	low.PassiveProbes[0].Severity = model.RelayProbeSeverityLow
	low.PassiveProbes[0].Code = model.ProbeCodeCharTokenRatio
	med := probeLog(2, "m2", chatBody("ok"), 1)

	report := ScanLogAnomalies([]model.RelayLog{low, med}, time.Now())
	if len(report.ProbeFindings) != 2 {
		t.Fatalf("应有 2 个桶, 实际 %d", len(report.ProbeFindings))
	}
	if report.ProbeFindings[0].Severity != model.RelayProbeSeverityMedium {
		t.Fatalf("medium 应排第一, 实际 %s", report.ProbeFindings[0].Severity)
	}
}
