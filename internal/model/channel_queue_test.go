package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// 渠道「排队可调」的两个旋钮（QueueSize / QueueTimeoutMs）的模型层口径。
//
// 用例矩阵照 axonhub 的 biz.ValidateRateLimit 搬过来（负数一律拒、queueSize 需要
// maxConcurrent>0、只配 queueTimeoutMs 允许但不生效），再加本项目自己的两条：
// 创建侧 Channel.Validate 走的是同一口径；JSON 字段必须是 snake_case（前端/CSV/日志
// 都按它读写，改名等于静默丢配置）。

func TestValidateChannelQueueSettings(t *testing.T) {
	cases := []struct {
		name           string
		maxConcurrent  int
		queueSize      int
		queueTimeoutMs int
		wantErr        string
	}{
		{name: "defaults are allowed", maxConcurrent: 0, queueSize: 0, queueTimeoutMs: 0},
		{name: "maxConcurrent only is allowed", maxConcurrent: 5},
		{name: "full hard mode is allowed", maxConcurrent: 5, queueSize: 20, queueTimeoutMs: 30000},
		{name: "queueTimeoutMs without queueSize is allowed but inert", maxConcurrent: 5, queueTimeoutMs: 1000},
		{name: "queueSize with maxConcurrent is allowed", maxConcurrent: 1, queueSize: 1},
		{
			name:      "negative queueSize rejected",
			queueSize: -1,
			wantErr:   "queue_size must be >= 0",
		},
		{
			name:           "negative queueTimeoutMs rejected",
			queueTimeoutMs: -1,
			wantErr:        "queue_timeout_ms must be >= 0",
		},
		{
			name:      "queueSize without maxConcurrent rejected",
			queueSize: 10,
			wantErr:   "queue_size requires max_concurrent > 0",
		},
		{
			name:          "queueSize with zero maxConcurrent rejected",
			maxConcurrent: 0,
			queueSize:     10,
			wantErr:       "queue_size requires max_concurrent > 0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateChannelQueueSettings(tc.maxConcurrent, tc.queueSize, tc.queueTimeoutMs)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateChannelQueueSettings(%d,%d,%d) = %v, want nil",
						tc.maxConcurrent, tc.queueSize, tc.queueTimeoutMs, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateChannelQueueSettings(%d,%d,%d) = nil, want error containing %q",
					tc.maxConcurrent, tc.queueSize, tc.queueTimeoutMs, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// 创建侧（handler 调 Channel.Validate）必须真的拦下「QueueSize>0 但并发=0」——
// 那是评估器最容易配错的组合：以为配了排队，其实永远不会满。
func TestChannelValidateRejectsQueueWithoutConcurrency(t *testing.T) {
	channel := Channel{Name: "queue-no-concurrency", MaxConcurrent: 0, QueueSize: 5}
	if err := channel.Validate(); err == nil {
		t.Fatalf("a channel with queue_size>0 and max_concurrent=0 must be rejected on create")
	}
	channel.MaxConcurrent = 2
	if err := channel.Validate(); err != nil {
		t.Fatalf("queue_size=5 with max_concurrent=2 must be accepted, got %v", err)
	}
	// 存量渠道的零值组合（不排队、用默认等待）永远合法。
	if err := (&Channel{Name: "legacy"}).Validate(); err != nil {
		t.Fatalf("a legacy channel (both knobs 0) must stay valid, got %v", err)
	}
	channel.QueueTimeoutMs = -1
	if err := channel.Validate(); err == nil {
		t.Fatalf("negative queue_timeout_ms must be rejected on create")
	}
}

// JSON 字段名是前后端的合同：queue_size / queue_timeout_ms 写错了不会报错，只会让
// 运营商配的值静默丢失（update 请求里解析不到 → nil → 库里不动）。
func TestChannelQueueJSONFieldNames(t *testing.T) {
	payload := []byte(`{"name":"q","max_concurrent":3,"queue_size":7,"queue_timeout_ms":2500}`)
	var channel Channel
	if err := json.Unmarshal(payload, &channel); err != nil {
		t.Fatalf("unmarshal channel: %v", err)
	}
	if channel.QueueSize != 7 || channel.QueueTimeoutMs != 2500 {
		t.Fatalf("channel queue fields not decoded from snake_case: queue_size=%d queue_timeout_ms=%d",
			channel.QueueSize, channel.QueueTimeoutMs)
	}

	var req ChannelUpdateRequest
	if err := json.Unmarshal([]byte(`{"id":1,"queue_size":0,"queue_timeout_ms":0}`), &req); err != nil {
		t.Fatalf("unmarshal update request: %v", err)
	}
	// 指针语义：显式 0 必须解出来是「配了 0」，而不是「没提这个字段」。
	if req.QueueSize == nil || *req.QueueSize != 0 {
		t.Fatalf("explicit queue_size=0 must decode to a non-nil pointer, got %v", req.QueueSize)
	}
	if req.QueueTimeoutMs == nil || *req.QueueTimeoutMs != 0 {
		t.Fatalf("explicit queue_timeout_ms=0 must decode to a non-nil pointer, got %v", req.QueueTimeoutMs)
	}
	// 省略（老前端/老客户端）必须是 nil = 库里的值不动。
	var untouched ChannelUpdateRequest
	if err := json.Unmarshal([]byte(`{"id":1,"enabled":false}`), &untouched); err != nil {
		t.Fatalf("unmarshal partial update: %v", err)
	}
	if untouched.QueueSize != nil || untouched.QueueTimeoutMs != nil {
		t.Fatalf("omitted queue knobs must stay nil (stored values untouched), got %v/%v",
			untouched.QueueSize, untouched.QueueTimeoutMs)
	}
}

// 部分更新请求本身的 Validate 不做跨字段判断（它看不到库里的另一半），这条约束由
// op.ChannelUpdate 在合并后的整条渠道上执行——见 op/channel_queue_test.go。
func TestChannelUpdateRequestValidateLeavesQueueCrossFieldToOp(t *testing.T) {
	queue := 5
	if err := (&ChannelUpdateRequest{ID: 1, QueueSize: &queue}).Validate(); err != nil {
		t.Fatalf("a queue-only patch must not be rejected without seeing the stored max_concurrent, got %v", err)
	}
}
