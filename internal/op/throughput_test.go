package op

import (
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
)

// TPS 有效性闸门的用例（2026-10-27，astra 裁决落地）。
// 每条「不采信」都对应一类生产爆表/噪音形态；变异自检（删对应门槛）必须红。

func tpsSample(t *testing.T, log model.RelayLog) (float64, bool) {
	t.Helper()
	return relayLogTPSSample(log)
}

func TestTPSSampleRejectsMillisecondWindow(t *testing.T) {
	// 生产爆表原型：非流式上游的完整响应在最后一刻合成下推，ftut 落在
	// use_time-1ms，1ms 窗口把 5874 token 显示成 5,874,000 tok/s。
	if _, ok := tpsSample(t, model.RelayLog{
		OutputTokens: 5874, UseTime: 24133, Ftut: 24132,
	}); ok {
		t.Fatal("1ms 生成窗口必须判不可采信")
	}
}

func TestTPSSampleRejectsShareTooSmall(t *testing.T) {
	// 窗口时长达标但只占总耗时 10%——首字落在 90% 处，窗口依然不可采信。
	if _, ok := tpsSample(t, model.RelayLog{
		OutputTokens: 500, UseTime: 10000, Ftut: 9000,
	}); ok {
		t.Fatal("生成窗口占比不足 25% 必须判不可采信")
	}
}

func TestTPSSampleRejectsTinyOutput(t *testing.T) {
	// 几十个 token 的短回复：样本太短，窗口不足以代表生成速度。
	if _, ok := tpsSample(t, model.RelayLog{
		OutputTokens: 73, UseTime: 46271, Ftut: 3995,
	}); ok {
		t.Fatal("输出不足 100 token 必须判不可采信")
	}
}

func TestTPSSampleRejectsImplausibleSpeed(t *testing.T) {
	// 窗口勉强合格但算出来 2500 tok/s——超过当前展示上限，观测不足以采信
	// （不推断是测量问题还是真值）。
	if _, ok := tpsSample(t, model.RelayLog{
		OutputTokens: 5000, UseTime: 3000, Ftut: 1000,
	}); ok {
		t.Fatal("超出 plausible 上限必须判不可采信")
	}
}

func TestTPSSampleRejectsErrorAndMissingFtut(t *testing.T) {
	if _, ok := tpsSample(t, model.RelayLog{
		Error: "upstream failed", OutputTokens: 500, UseTime: 3000, Ftut: 1000,
	}); ok {
		t.Fatal("失败请求不参与")
	}
	// ftut=0（非流式路径从不记首字）：不退回全程平均，直接不采信。
	if _, ok := tpsSample(t, model.RelayLog{
		OutputTokens: 500, UseTime: 3000,
	}); ok {
		t.Fatal("无首字时间必须判不可采信")
	}
}

// 失败判据与全局唯一的分桶 model.ClassifyRelayLogSeverity 对齐（2026-11-06）：
// 后端日志带 error，普通用户拿到的是 RelayLogUserSummary（没有 error 字段、只有
// error_code / error_status）。两路载荷对同一条行必须得出同一个结论，否则管理员
// 看到「不显示」、用户看到一个编出来的速度。
func TestTPSSampleRejectsBySeverityNotJustError(t *testing.T) {
	timings := func() model.RelayLog {
		return model.RelayLog{OutputTokens: 500, UseTime: 1300, Ftut: 300}
	}
	if _, ok := tpsSample(t, timings()); !ok {
		t.Fatal("基线必须采信，否则下面的用例证明不了什么")
	}

	// 只有 error_code（用户摘要载荷的形态）。
	byCode := timings()
	byCode.ErrorCode = "server_error"
	if _, ok := tpsSample(t, byCode); ok {
		t.Fatal("只带 error_code 的失败行也必须拒采信")
	}

	// 只有 error_status。
	byStatus := timings()
	byStatus.ErrorStatus = 502
	if _, ok := tpsSample(t, byStatus); ok {
		t.Fatal("只带 error_status>=400 的失败行也必须拒采信")
	}

	// 空白失败字段等于没失败：与 strings.TrimSpace / 前端 trim 同口径。
	blank := timings()
	blank.Error = "   "
	blank.ErrorCode = "\t"
	if _, ok := tpsSample(t, blank); !ok {
		t.Fatal("空白 error/error_code 不得当成失败")
	}

	// 重试/故障转移后成功（warn）不是失败：照常采样，否则排行/健康会把
	// 每一次真实重试都误判为不可测。
	retried := timings()
	retried.TotalAttempts = 3
	if _, ok := tpsSample(t, retried); !ok {
		t.Fatal("重试后成功的行必须照常采样（只拒 error，不拒 warn）")
	}
}

func TestTPSSampleAcceptsHonestWindow(t *testing.T) {
	got, ok := tpsSample(t, model.RelayLog{
		OutputTokens: 500, UseTime: 1300, Ftut: 300,
	})
	if !ok {
		t.Fatal("正常流式窗口必须采信")
	}
	if got != 500 {
		t.Fatalf("500 token / 1s = 500 tok/s，实际 %f", got)
	}
}

func TestTPSMedian(t *testing.T) {
	if got := tpsMedian(nil); got != 0 {
		t.Fatalf("空样本应为 0，实际 %f", got)
	}
	if got := tpsMedian([]float64{9, 1, 5}); got != 5 {
		t.Fatalf("奇数中位数应为 5，实际 %f", got)
	}
	if got := tpsMedian([]float64{100, 300}); got != 200 {
		t.Fatalf("偶数中位数应为 200，实际 %f", got)
	}
}
