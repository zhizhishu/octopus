package op

import (
	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
)

// GORM 在 Create 时会跳过「带 default 标签且为零值」的字段，改用默认值，并把默认值回写进结构体。
// AccessRouteTarget.Enabled / AccessBillingModelRule.Enabled 都是 `gorm:"default:true"`，
// 所以方案页关掉的线路 / 单独倍率，整表保存（先删后建）后又变回开着 —— 界面上的
// 「使用」「启用」开关等于没用。做法：建之前记下哪些本该关着，建完按 ID 补写成 false，
// 并把内存里的结构体也改回 false。

func disabledRouteTargetIndexes(targets []model.AccessRouteTarget) []int {
	out := make([]int, 0)
	for i, target := range targets {
		if !target.Enabled {
			out = append(out, i)
		}
	}
	return out
}

func restoreDisabledRouteTargets(tx *gorm.DB, targets []model.AccessRouteTarget, disabled []int) error {
	ids := make([]int, 0, len(disabled))
	for _, i := range disabled {
		if i < 0 || i >= len(targets) || targets[i].ID <= 0 {
			continue
		}
		targets[i].Enabled = false
		ids = append(ids, targets[i].ID)
	}
	if len(ids) == 0 {
		return nil
	}
	return tx.Model(&model.AccessRouteTarget{}).Where("id IN ?", ids).Update("enabled", false).Error
}

func disabledBillingRuleIndexes(rules []model.AccessBillingModelRule) []int {
	out := make([]int, 0)
	for i, rule := range rules {
		if !rule.Enabled {
			out = append(out, i)
		}
	}
	return out
}

func restoreDisabledBillingRules(tx *gorm.DB, rules []model.AccessBillingModelRule, disabled []int) error {
	ids := make([]int, 0, len(disabled))
	for _, i := range disabled {
		if i < 0 || i >= len(rules) || rules[i].ID <= 0 {
			continue
		}
		rules[i].Enabled = false
		ids = append(ids, rules[i].ID)
	}
	if len(ids) == 0 {
		return nil
	}
	return tx.Model(&model.AccessBillingModelRule{}).Where("id IN ?", ids).Update("enabled", false).Error
}
