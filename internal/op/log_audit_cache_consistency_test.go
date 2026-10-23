package op

import (
	"context"
	"testing"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

// 「检测结论写回」写完数据库就完事，内存缓存里的同一条日志还是旧的 ——
// 详情接口优先读缓存，于是刚检测完的那一行，详情页仍然是「没检测过」。
//
// 【缺陷形态】一次检测结束时的实际顺序是：
//  1. op.RelayLogMarkAudited 单条 UPDATE，把 verdict/score/finding_n/report 写进库
//  2. 该行的内存副本（relayLogCache）**一个字段都没动**
//  3. 前端调 GET /api/v1/log/{id} → RelayLogGetByID 先扫 relayLogCache，
//     命中就 return —— 返回的是没有 verdict、没有 report 的旧结构
//  4. 要么等缓存的这条被挤出（默认上限内可能几小时），要么重启进程，才看得见
//
// 用户看到的现象：点了「检测」，转圈结束，弹窗里徽标还是空的。
// 于是「检测结论回写日志行」这条链路在**最快的路径上**等于没做。
//
// 【为什么这条测试要真的走一遍 MarkAudited】缺陷不在 MarkAudited 里，也不在
// GetByID 里 —— 两个函数各自看都正确（一个诚实地写库，一个诚实地读缓存）。
// 错的是它们之间的**一致性缺口**。只测任一函数永远抓不到。

// seedCachedLog 造一条「已落库 + 已在缓存」的日志，返回它的 id。
func seedCachedLog(t *testing.T) int64 {
	t.Helper()
	ctx := setupRelayLogTest(t)

	// ID 是 autoIncrement:false，必须自己给；否则 Create 不报错但 ID 留 0，
	// 后续按 id 取行全都 record not found（第一版就是这么假绿的）。
	row := model.RelayLog{
		ID:                9_000_001,
		Time:              1,
		RequestEndpoint:   "chat",
		RequestModelName:  "cache-consistency-model",
		ActualModelName:   "cache-consistency-model",
		ChannelId:         1,
		ChannelName:       "upstream.example",
		TotalAttempts:     1,
		RequestContent:    "{}",
		ResponseContent:   "{}",
		ModelAuditVerdict: "",
	}
	if err := db.GetDB().WithContext(ctx).Create(&row).Error; err != nil {
		t.Fatalf("造日志行: %v", err)
	}

	// 把它放进内存缓存 —— 模拟"这条日志正是刚产生、还热在缓存里的那条"。
	relayLogCacheLock.Lock()
	relayLogCache = append(relayLogCache, row)
	relayLogCacheLock.Unlock()

	t.Cleanup(func() {
		relayLogCacheLock.Lock()
		kept := relayLogCache[:0]
		for _, item := range relayLogCache {
			if item.ID != row.ID {
				kept = append(kept, item)
			}
		}
		relayLogCache = kept
		relayLogCacheLock.Unlock()
		_ = db.GetDB().WithContext(context.Background()).Delete(&model.RelayLog{}, row.ID).Error
	})

	return row.ID
}

// 检测结论写回之后，详情接口（缓存路径）必须看得到它。
//
// 这是 model_audit_report 在界面上永远空白的根因：不是前端没画，
// 是它拿到的数据本来就是空的。
func TestAuditMarkIsVisibleThroughDetailCachePath(t *testing.T) {
	id := seedCachedLog(t)
	ctx := context.Background()

	const reportJSON = `{"verdict":"high","score":42,"plain":{"title":"看着像换过模型"}}`
	if err := RelayLogMarkAudited(ctx, id, "high", 42, 3, "manual", reportJSON); err != nil {
		t.Fatalf("RelayLogMarkAudited: %v", err)
	}

	got, err := RelayLogGetByID(ctx, id, nil)
	if err != nil {
		t.Fatalf("RelayLogGetByID: %v", err)
	}

	if got.ModelAuditVerdict != "high" {
		t.Fatalf("检测结论写回后，详情接口仍读到 verdict=%q（期望 high）。"+
			"这条日志还在内存缓存里，缓存副本没被同步 —— 用户点完检测，弹窗里徽标依然是空的",
			got.ModelAuditVerdict)
	}
	if got.ModelAuditScore != 42 {
		t.Fatalf("详情接口读到 score=%d（期望 42）：结论字段没同步进缓存", got.ModelAuditScore)
	}
	if got.ModelAuditReport != reportJSON {
		t.Fatalf("详情接口读到 report=%q（期望完整报告）：前端就算想画明细也拿不到数据", got.ModelAuditReport)
	}
}

// 缓存被挤出之后走数据库路径，结论当然要在 —— 这条守住"别为了修缓存把库写坏"。
func TestAuditMarkSurvivesAfterCacheEviction(t *testing.T) {
	id := seedCachedLog(t)
	ctx := context.Background()

	const reportJSON = `{"verdict":"medium","score":11}`
	if err := RelayLogMarkAudited(ctx, id, "medium", 11, 1, "manual", reportJSON); err != nil {
		t.Fatalf("RelayLogMarkAudited: %v", err)
	}

	// 模拟缓存被挤出。
	relayLogCacheLock.Lock()
	kept := relayLogCache[:0]
	for _, item := range relayLogCache {
		if item.ID != id {
			kept = append(kept, item)
		}
	}
	relayLogCache = kept
	relayLogCacheLock.Unlock()

	got, err := RelayLogGetByID(ctx, id, nil)
	if err != nil {
		t.Fatalf("RelayLogGetByID: %v", err)
	}
	if got.ModelAuditVerdict != "medium" || got.ModelAuditScore != 11 || got.ModelAuditReport != reportJSON {
		t.Fatalf("走出缓存后结论丢失: verdict=%q score=%d report=%q",
			got.ModelAuditVerdict, got.ModelAuditScore, got.ModelAuditReport)
	}
}
