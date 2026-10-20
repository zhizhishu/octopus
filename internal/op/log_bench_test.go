package op

// 日志页性能基准。跑的是真实调用路径（RelayLogList / RelayLogSeverityCounts），
// 不是模拟 SQL —— 否则优化了假的东西也没人知道。
//
// 用法（数据量大，默认跳过，需显式开启）：
//
//	OCT_LOG_BENCH_ROWS=50000 go test ./internal/op/ -run '^$' -bench 'BenchmarkRelayLog' -benchtime 3x
//
// 为什么默认跳过：造几万行要几十秒，挂在常规 go test 里会拖慢每次验证。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

// benchLogRows 读环境变量决定造多少行；未设置返回 0 表示跳过。
func benchLogRows(tb testing.TB) int {
	tb.Helper()
	raw := os.Getenv("OCT_LOG_BENCH_ROWS")
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		tb.Fatalf("OCT_LOG_BENCH_ROWS 非法: %q", raw)
	}
	return n
}

// setupLogBench 建一个装满真实形状数据的库。
// 关键点：request_content/response_content 故意写大（2KB/4KB），复刻真实日志的
// overflow page —— 那才是 SELECT * 慢的原因。
func setupLogBench(tb testing.TB, rows int) {
	tb.Helper()

	if err := db.InitDB("sqlite", filepath.Join(tb.TempDir(), "bench.db"), false); err != nil {
		tb.Fatalf("init db: %v", err)
	}
	tb.Cleanup(func() { _ = db.Close() })

	// 关同步：造数据阶段不需要落盘可靠性，否则几十万行要跑很久。
	db.GetDB().Exec("PRAGMA journal_mode=WAL")
	db.GetDB().Exec("PRAGMA synchronous=OFF")

	// RelayLogList 第一步就读 relay_log_keep_enabled 这个 setting；不刷新缓存会
	// 直接报 "setting not found"。与 setupRelayLogTest 的做法保持一致。
	tb.Cleanup(func() {})
	if err := settingRefreshCache(context.Background()); err != nil {
		tb.Fatalf("refresh setting cache: %v", err)
	}

	models := []string{
		"claude-opus-4-8", "claude-sonnet-5", "gpt-5.6-sol", "gpt-6-astra",
		"gemini-4-argon", "deepseek-v4-pro", "glm-5.3", "kimi-k3",
		"qwen3.8-max", "grok-4.7",
	}
	channels := []string{"primary-relay", "backup-relay", "cheap-relay", "test-relay"}
	users := []string{"alice", "bob", "carol", "dave", "admin"}

	bigReq := `{"messages":[{"role":"user","content":"` + strings.Repeat("x", 2000) + `"}]}`
	bigResp := `{"choices":[{"message":{"content":"` + strings.Repeat("y", 4000) + `"}}]}`

	const base = int64(1750000000000)
	// SQLite 的 SQL 变量上限按 row×列数算：RelayLog 列多，批量必须小，
	// 否则 createInBatches 会撞 "too many SQL variables"。
	const batchSize = 50
	batch := make([]model.RelayLog, 0, batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := db.GetDB().CreateInBatches(batch, batchSize).Error; err != nil {
			tb.Fatalf("seed: %v", err)
		}
		batch = batch[:0]
	}

	for i := 0; i < rows; i++ {
		m := models[i%len(models)]
		lg := model.RelayLog{
			ID:                    base + int64(i),
			UserID:                i % 5,
			APIKeyID:              i % 8,
			RequestIP:             fmt.Sprintf("10.%d.%d.%d", i%256, (i/256)%256, i%251),
			Time:                  base + int64(i)*1000,
			RequestEndpoint:       "chat",
			RequestPath:           "/v1/chat/completions",
			RequestUserAgent:      "claude-cli/2.0.1",
			RequestModelName:      m,
			RequestAPIKeyName:     fmt.Sprintf("key-%d", i%8),
			UserName:              users[i%len(users)],
			ChannelId:             i%4 + 1,
			ChannelName:           channels[i%len(channels)],
			ActualModelName:       m,
			UpstreamResponseModel: m,
			RequestContent:        bigReq,
			ResponseContent:       bigResp,
		}
		// 5% 错误率，让 severity 分桶有真实选择性。
		if i%20 == 0 {
			lg.Error = "upstream returned 529 overloaded"
			lg.ErrorCode = "upstream_5xx"
		}
		batch = append(batch, lg)
		if len(batch) == cap(batch) {
			flush()
		}
	}
	flush()
	db.GetDB().Exec("ANALYZE")

	// 投影表要跟主表一致, 否则搜索会降级到主表 LIKE, 量出来的就是"没优化"的数字。
	// 生产里由落库路径和迁移 014 维护, 基准直接灌主表, 所以这里显式重建。
	if err := relayLogSearchIndexRebuild(context.Background()); err != nil {
		tb.Fatalf("rebuild search index: %v", err)
	}

	var got int64
	db.GetDB().Model(&model.RelayLog{}).Count(&got)
	if got != int64(rows) {
		tb.Fatalf("造数据不完整: 期望 %d 实际 %d", rows, got)
	}
}

// BenchmarkRelayLogPage：翻页（无搜索）。前端每翻一页 = 一次 list + 一次 counts。
func BenchmarkRelayLogPage(b *testing.B) {
	rows := benchLogRows(b)
	if rows == 0 {
		b.Skip("设置 OCT_LOG_BENCH_ROWS 才跑")
	}
	setupLogBench(b, rows)
	ctx := b.Context()
	scope := &model.RelayLogScope{}

	for _, page := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("page%d", page), func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := RelayLogList(ctx, nil, nil, page, 20, scope); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRelayLogSearch：搜索翻页。这是用户抱怨"慢"的主路径。
func BenchmarkRelayLogSearch(b *testing.B) {
	rows := benchLogRows(b)
	if rows == 0 {
		b.Skip("设置 OCT_LOG_BENCH_ROWS 才跑")
	}
	setupLogBench(b, rows)
	ctx := b.Context()

	// 命中率极低（1/10）与极低（唯一值）两种都测：前者是"能翻到"，后者是"翻不到"。
	cases := []struct {
		name   string
		search string
	}{
		{"hit10pct", "kimi-k3"},
		{"hit2pct", "backup-relay"},
		{"miss", "zzz-not-exist"},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			scope := &model.RelayLogScope{Search: tc.search}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := RelayLogList(ctx, nil, nil, 1, 20, scope); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRelayLogSeverityCounts：每次翻页都算一遍分桶。
func BenchmarkRelayLogSeverityCounts(b *testing.B) {
	rows := benchLogRows(b)
	if rows == 0 {
		b.Skip("设置 OCT_LOG_BENCH_ROWS 才跑")
	}
	setupLogBench(b, rows)
	ctx := b.Context()

	b.Run("noSearch", func(b *testing.B) {
		scope := &model.RelayLogScope{}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := RelayLogSeverityCounts(ctx, nil, nil, scope); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("withSearch", func(b *testing.B) {
		scope := &model.RelayLogScope{Search: "kimi-k3"}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := RelayLogSeverityCounts(ctx, nil, nil, scope); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkRelayLogPageEndToEnd：一次翻页在真实前端里做的全部工作（list + counts）。
// 这个数字最贴近用户体感。
func BenchmarkRelayLogPageEndToEnd(b *testing.B) {
	rows := benchLogRows(b)
	if rows == 0 {
		b.Skip("设置 OCT_LOG_BENCH_ROWS 才跑")
	}
	setupLogBench(b, rows)
	ctx := b.Context()

	for _, tc := range []struct {
		name  string
		scope *model.RelayLogScope
	}{
		{"plain", &model.RelayLogScope{}},
		{"search", &model.RelayLogScope{Search: "kimi-k3"}},
		{"severityError", &model.RelayLogScope{Severity: "error"}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := RelayLogList(ctx, nil, nil, 1, 20, tc.scope); err != nil {
					b.Fatal(err)
				}
				if _, err := RelayLogSeverityCounts(ctx, nil, nil, tc.scope); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
