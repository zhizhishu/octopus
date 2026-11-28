package balancer

import (
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
)

// pack-1128 D：救援轮/重排队会换掉迭代器。接力迭代器 SeedCount 之后，尝试序号必须
// 从已记录尝试数继续（生产事故证据：单条日志 200+ 次救援尝试 attempt_num 全是 1）。
func TestIteratorSeedCountContinuesAttemptNumbering(t *testing.T) {
	candidates := []model.GroupItem{{ChannelID: 1, ModelName: "m"}}
	first := &Iterator{candidates: candidates}
	if span := first.StartAttempt(1, 11, "ch"); span.attempt.AttemptNum != 1 {
		t.Fatalf("first attempt num = %d, want 1", span.attempt.AttemptNum)
	}

	relay := &Iterator{candidates: candidates}
	relay.SeedCount(3)
	if span := relay.StartAttempt(1, 11, "ch"); span.attempt.AttemptNum != 4 {
		t.Fatalf("seeded attempt num = %d, want 4", span.attempt.AttemptNum)
	}

	// 只升不降：用小值重复 seed 不得把计数拉回去。
	relay.SeedCount(1)
	if relay.count != 4 {
		t.Fatalf("count regressed to %d, want 4", relay.count)
	}

	// Skip/SkipCircuitBreak 也走同一个计数器。
	relay.Skip(2, 22, "ch2", "skip")
	if got := relay.attempts[len(relay.attempts)-1].AttemptNum; got != 5 {
		t.Fatalf("skip attempt num = %d, want 5", got)
	}
}
