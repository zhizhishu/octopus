package model

import "testing"

// 渠道级探测覆写（迁移 030：probe_daily_cap / probe_max_failures / probe_model）的
// 校验口径：两个数字字段 0 都合法（0 = 沿用全局），负值与越界一律拒绝。
// ChannelUpdateRequest 只校验调用方真正传了的指针——nil 表示本次没提这个字段，
// 绝不能替存量数据拦人。

func TestChannelProbeOverrideValidation(t *testing.T) {
	cases := []struct {
		name          string
		channel       Channel
		expectInvalid bool
	}{
		// 0 合法 = 沿用全局（渠道级覆写的默认/关闭语义）。
		{name: "zeros follow global", channel: Channel{ProbeDailyCap: 0, ProbeMaxFailures: 0}},
		{name: "probe model set, zeros still fine", channel: Channel{ProbeDailyCap: 0, ProbeMaxFailures: 0, ProbeModel: "probe-model"}},
		{name: "daily cap at the upper bound", channel: Channel{ProbeDailyCap: 100000, ProbeMaxFailures: 0}},
		{name: "max failures at the upper bound", channel: Channel{ProbeDailyCap: 0, ProbeMaxFailures: 100}},
		{name: "both at the upper bounds", channel: Channel{ProbeDailyCap: 100000, ProbeMaxFailures: 100}},
		{name: "ordinary positive values", channel: Channel{ProbeDailyCap: 200, ProbeMaxFailures: 3}},
		// 越界必须被拒。
		{name: "negative daily cap", channel: Channel{ProbeDailyCap: -1, ProbeMaxFailures: 0}, expectInvalid: true},
		{name: "daily cap over the bound", channel: Channel{ProbeDailyCap: 100001, ProbeMaxFailures: 0}, expectInvalid: true},
		{name: "negative max failures", channel: Channel{ProbeDailyCap: 0, ProbeMaxFailures: -1}, expectInvalid: true},
		{name: "max failures over the bound", channel: Channel{ProbeDailyCap: 0, ProbeMaxFailures: 101}, expectInvalid: true},
		{name: "both negative", channel: Channel{ProbeDailyCap: -1, ProbeMaxFailures: -1}, expectInvalid: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			channel := tc.channel
			err := channel.Validate()
			if (err != nil) != tc.expectInvalid {
				t.Fatalf("ProbeDailyCap=%d ProbeMaxFailures=%d: Validate() = %v, expectInvalid = %v",
					channel.ProbeDailyCap, channel.ProbeMaxFailures, err, tc.expectInvalid)
			}
		})
	}
}

// 更新路径：三个探测字段都是指针，nil 时不校验不报错；两个数字字段的边界与
// Channel 相同；ProbeModel 设与不设都不影响这两个数字字段的校验。
func TestChannelUpdateRequestProbeOverrideValidation(t *testing.T) {
	negOne := -1
	zero := 0
	one := 1
	boundCap := 100000
	overCap := 100001
	boundFail := 100
	overFail := 101
	probeModel := "probe-model"

	cases := []struct {
		name          string
		req           ChannelUpdateRequest
		expectInvalid bool
	}{
		// 全部 nil：渠道存量数据不管是什么值，PATCH 没提就不校验。
		{name: "all probe fields nil", req: ChannelUpdateRequest{ID: 1}},
		{name: "nil numbers with only probe model", req: ChannelUpdateRequest{ID: 1, ProbeModel: &probeModel}},
		// 越界必须被拒。
		{name: "daily cap -1", req: ChannelUpdateRequest{ID: 1, ProbeDailyCap: &negOne}, expectInvalid: true},
		{name: "daily cap 100001", req: ChannelUpdateRequest{ID: 1, ProbeDailyCap: &overCap}, expectInvalid: true},
		{name: "max failures -1", req: ChannelUpdateRequest{ID: 1, ProbeMaxFailures: &negOne}, expectInvalid: true},
		{name: "max failures 101", req: ChannelUpdateRequest{ID: 1, ProbeMaxFailures: &overFail}, expectInvalid: true},
		// 0 与边界内合法。
		{name: "daily cap 0 (follow global)", req: ChannelUpdateRequest{ID: 1, ProbeDailyCap: &zero}},
		{name: "daily cap 100000", req: ChannelUpdateRequest{ID: 1, ProbeDailyCap: &boundCap}},
		{name: "max failures 0", req: ChannelUpdateRequest{ID: 1, ProbeMaxFailures: &zero}},
		{name: "max failures 1", req: ChannelUpdateRequest{ID: 1, ProbeMaxFailures: &one}},
		{name: "max failures 100", req: ChannelUpdateRequest{ID: 1, ProbeMaxFailures: &boundFail}},
		// 设了 ProbeModel 不改变两个数字字段的判定。
		{name: "probe model + valid numbers", req: ChannelUpdateRequest{ID: 1, ProbeModel: &probeModel,
			ProbeDailyCap: &boundCap, ProbeMaxFailures: &boundFail}},
		{name: "probe model + bad daily cap", req: ChannelUpdateRequest{ID: 1, ProbeModel: &probeModel,
			ProbeDailyCap: &negOne}, expectInvalid: true},
		{name: "probe model + bad max failures", req: ChannelUpdateRequest{ID: 1, ProbeModel: &probeModel,
			ProbeMaxFailures: &overFail}, expectInvalid: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req
			err := req.Validate()
			if (err != nil) != tc.expectInvalid {
				t.Fatalf("req.Validate() = %v, expectInvalid = %v", err, tc.expectInvalid)
			}
		})
	}
}
