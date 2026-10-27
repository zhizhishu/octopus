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
	// 几十个 token 的短回复：窗口量的是网络冲刷，不是生成速度。
	if _, ok := tpsSample(t, model.RelayLog{
		OutputTokens: 73, UseTime: 46271, Ftut: 3995,
	}); ok {
		t.Fatal("输出不足 100 token 必须判不可采信")
	}
}

func TestTPSSampleRejectsImplausibleSpeed(t *testing.T) {
	// 窗口勉强合格但算出来 2500 tok/s——超出现实上限，测量坏了不是上游快。
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
