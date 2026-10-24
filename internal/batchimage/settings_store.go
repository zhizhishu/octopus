package batchimage

import (
	"context"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/utils/log"
)

// SettingsFromStore 从设置表读一份配置快照。
//
// 【为什么这一份要放在包里而不是各处各读一遍】提交路径（handlers）和执行路径
// （task 推进器）读**必须**是同一套口径。两边各写一份的必然结果是"提交时说开着、
// 执行时说关着"这种鬼故事，而且只在生产上、只在特定时序下复现。
//
// 【读失败的方向全部偏保守】
//   - 总开关读不到 → 当**关**（与 async_image_enabled 同口径）。反过来会让一次
//     数据库抖动变成"这个花钱的功能突然对所有人生效"。
//   - 单价读不到 → 当**0**（不计费）。凭空多扣钱是最不能接受的失败方向。
//   - 其余数值读不到 → 由 Normalize 补默认值。
func SettingsFromStore(_ context.Context) Settings {
	unitPrice, err := op.SettingGetFloat(model.SettingKeyBatchImageUnitPrice)
	if err != nil {
		log.Warnf("batch image: read unit price failed (%v), charging is disabled for this round", err)
		unitPrice = 0
	}
	maxItems, _ := op.SettingGetInt(model.SettingKeyBatchImageMaxItems)
	maxActive, _ := op.SettingGetInt(model.SettingKeyBatchImageMaxActive)
	retentionHours, _ := op.SettingGetInt(model.SettingKeyBatchImageRetentionHours)

	enabled, err := op.SettingGetBool(model.SettingKeyBatchImageEnabled)
	if err != nil {
		enabled = false
	}

	return Settings{
		Enabled:                   enabled,
		MaxItems:                  maxItems,
		MaxConcurrentJobsPerOwner: maxActive,
		RetentionHours:            retentionHours,
		UnitPrice:                 unitPrice,
	}.Normalize()
}
