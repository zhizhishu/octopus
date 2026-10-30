package op

import (
	"fmt"
	"sort"
	"testing"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

// 深翻页 / 导出(dbLimit 超过两段式上限)改成"先只取 (id, time) 定位窗口, 再取这一页整行"之后,
// 结果必须与旧算法逐行一致: 把前 dbLimit 行整行读出来、缓存在前合并去重(缓存副本优先)、
// 按 time 降序 / id 降序稳定排序、再切片。这里用旧算法在测试里算一遍期望值逐页比对。
//
// 数据刻意制造了旧算法里所有会影响顺序的情形: time 与 id 不同序(慢请求 id 小 time 大)、
// 同一秒多条、缓存里有一条与库里同 id 但字段不同(必须用缓存副本)、缓存里还有库里没有的行。
func TestRelayLogDeepPageMatchesFullMergeSort(t *testing.T) {
	ctx := setupRelayLogTest(t)

	const n = 1500
	logs := make([]model.RelayLog, 0, n)
	for i := 1; i <= n; i++ {
		// time 每 7 条同一秒, 且每 5 条里有一条 time 被往后推(id 小、time 大)。
		tm := int64(10000 + i/7)
		if i%5 == 0 {
			tm += 3
		}
		logs = append(logs, model.RelayLog{ID: int64(50000 + i), Time: tm, RequestModelName: fmt.Sprintf("db-%d", i), Attempts: []model.ChannelAttempt{{Msg: "x"}}})
	}
	if err := db.GetDB().CreateInBatches(&logs, 100).Error; err != nil {
		t.Fatalf("create: %v", err)
	}

	cached := []model.RelayLog{
		// 与库里 50003 同 id, 字段不同: 旧算法保留缓存这一份。
		{ID: 50003, Time: logs[2].Time, RequestModelName: "cache-copy-of-50003"},
		// 只在缓存里(还没落库)的两条。
		{ID: 60001, Time: 10100, RequestModelName: "cache-only-1"},
		{ID: 60002, Time: 10100, RequestModelName: "cache-only-2"},
	}
	relayLogCacheLock.Lock()
	saved := relayLogCache
	relayLogCache = append([]model.RelayLog(nil), cached...)
	relayLogCacheLock.Unlock()
	t.Cleanup(func() {
		relayLogCacheLock.Lock()
		relayLogCache = saved
		relayLogCacheLock.Unlock()
	})

	// 旧算法的期望值。
	expected := func(page, pageSize int) []model.RelayLog {
		offset := (page - 1) * pageSize
		var all []model.RelayLog
		for i := len(cached) - 1; i >= 0; i-- { // RelayLogList 把缓存反转成新的在前
			all = append(all, cached[i])
		}
		dbLimit := offset + pageSize + len(cached)
		var dbLogs []model.RelayLog
		if err := db.GetDB().Order("id DESC").Limit(dbLimit).
			Omit("request_content", "response_content", "upstream_request_body", "model_audit_report").
			Find(&dbLogs).Error; err != nil {
			t.Fatalf("reference query: %v", err)
		}
		all = relayLogDedupe(append(all, dbLogs...))
		sort.SliceStable(all, relayLogPageLess(all))
		if offset >= len(all) {
			return nil
		}
		end := offset + pageSize
		if end > len(all) {
			end = len(all)
		}
		return all[offset:end]
	}

	pageSize := relayLogTwoPhaseMaxIDs + 1 // 强制走大 limit 分支
	for _, page := range []int{1, 2, 3} {
		got, err := RelayLogList(ctx, nil, nil, page, pageSize, &model.RelayLogScope{})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		want := expected(page, pageSize)
		if len(got) != len(want) {
			t.Fatalf("page %d: len got %d want %d", page, len(got), len(want))
		}
		for i := range want {
			if got[i].ID != want[i].ID || got[i].RequestModelName != want[i].RequestModelName || got[i].Time != want[i].Time {
				t.Fatalf("page %d row %d: got (%d,%d,%s) want (%d,%d,%s)", page, i,
					got[i].ID, got[i].Time, got[i].RequestModelName, want[i].ID, want[i].Time, want[i].RequestModelName)
			}
			if len(want[i].Attempts) != len(got[i].Attempts) {
				t.Fatalf("page %d row %d: attempts got %d want %d", page, i, len(got[i].Attempts), len(want[i].Attempts))
			}
		}
	}
	// 深翻页(每页 20): 第 46 页起 dbLimit 超过两段式上限。
	for _, page := range []int{46, 60, 75, 76} {
		got, err := RelayLogList(ctx, nil, nil, page, 20, &model.RelayLogScope{})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		want := expected(page, 20)
		if len(got) != len(want) {
			t.Fatalf("deep page %d: len got %d want %d", page, len(got), len(want))
		}
		for i := range want {
			if got[i].ID != want[i].ID || got[i].RequestModelName != want[i].RequestModelName {
				t.Fatalf("deep page %d row %d: got %d/%s want %d/%s", page, i, got[i].ID, got[i].RequestModelName, want[i].ID, want[i].RequestModelName)
			}
		}
	}
	// 缓存副本优先这一条必须真的出现在某一页里, 否则上面的比对没测到它。
	all, err := RelayLogList(ctx, nil, nil, 1, n+10, &model.RelayLogScope{})
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	found := false
	for _, l := range all {
		if l.ID == 50003 {
			found = l.RequestModelName == "cache-copy-of-50003"
		}
	}
	if !found || len(all) != n+2 {
		t.Fatalf("缓存副本优先/缓存独有行未生效: found=%v len=%d want %d", found, len(all), n+2)
	}
}
