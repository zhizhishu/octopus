package op

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zhizhishu/octopus/internal/db"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
)

// 提示词规则写路径：无缓存直打 DB，写显式列防 bool/时间列被零值吞掉。
var (
	// ErrPromptRuleNotFound 定位不到规则；ErrPromptRuleInvalid 字段形状/枚举越界；
	// ErrPromptRuleReorder 重排序非全量。
	ErrPromptRuleNotFound = errors.New("prompt rule not found")
	ErrPromptRuleInvalid  = errors.New("invalid prompt rule")
	ErrPromptRuleReorder  = errors.New("prompt rule reorder mismatched")
)

// promptRuleUpdateColumns 是 Update 的显式允许列：不含 id/created_at；updated_at
// 每次修改由服务端时间推进，created_at/updated_at 不从 body 伪造。
var promptRuleUpdateColumns = []string{
	"name", "enabled", "text", "position",
	"plan_mode", "plan_ids", "include_no_plan",
	"channel_mode", "channel_ids", "model_mode", "models",
	"audience", "sort", "updated_at",
}

// validatePromptRule 校验一条规则的写入形状：name 非空且 <=191（按 rune 数，
// 中文等多字节字符不重复计），text 按字节（bytes）计 <=262144，position/作用域/
// 受众枚举合法、引用 ID 去重且为正数、尾* 通配符唯一。audience 只收 all/admin_only；
// migration 031 的迁移保留拒绝。
func validatePromptRule(rule *dbmodel.PromptRule) error {
	if strings.TrimSpace(rule.Name) == "" {
		return fmt.Errorf("%w: name is required", ErrPromptRuleInvalid)
	}
	if utf8.RuneCountInString(rule.Name) > 191 {
		return fmt.Errorf("%w: name too long (max 191 runes)", ErrPromptRuleInvalid)
	}
	if len(rule.Text) > 262144 {
		return fmt.Errorf("%w: text too long (max 262144 bytes)", ErrPromptRuleInvalid)
	}
	if !rule.PositionValid() {
		return fmt.Errorf("%w: unknown position %q", ErrPromptRuleInvalid, rule.Position)
	}
	if !dbmodel.ValidPromptRulePlanMode(rule.PlanMode) {
		return fmt.Errorf("%w: unknown plan mode %q", ErrPromptRuleInvalid, rule.PlanMode)
	}
	if !dbmodel.ValidPromptRuleChannelMode(rule.ChannelMode) {
		return fmt.Errorf("%w: unknown channel mode %q", ErrPromptRuleInvalid, rule.ChannelMode)
	}
	if !dbmodel.ValidPromptRuleModelMode(rule.ModelMode) {
		return fmt.Errorf("%w: unknown model mode %q", ErrPromptRuleInvalid, rule.ModelMode)
	}
	if rule.Audience != dbmodel.PromptAudienceAll && rule.Audience != dbmodel.PromptAudienceAdminOnly {
		return fmt.Errorf("%w: unknown audience %q", ErrPromptRuleInvalid, rule.Audience)
	}
	if err := validatePromptRuleScope(rule); err != nil {
		return err
	}
	return validatePromptRuleIDList(rule.PlanIDs, "plan_ids", rule.ChannelIDs, "channel_ids")
}

// validatePromptRuleScope 校验 selected 作用域「至少选一项」约束（不查库）：
// 方案 selected 要 plan 非空或 include_no_plan 兜底（尾星通配与精确名分命名空间，
// 不互为重复）；渠道 include/exclude 要 channel 非空。模型 pattern trim 后必须非空：
// bare * 显式支持（代表全部），拒绝中间星/多星（foo**）与首星（*foo）。
func validatePromptRuleScope(rule *dbmodel.PromptRule) error {
	seenExact := make(map[string]struct{}, len(rule.Models))
	seenWild := make(map[string]struct{}, len(rule.Models))
	for _, raw := range rule.Models {
		pattern := strings.ToLower(strings.TrimSpace(raw))
		if pattern == "" {
			return fmt.Errorf("%w: model pattern must be non-empty", ErrPromptRuleInvalid)
		}
		if stars := strings.Count(pattern, "*"); stars > 1 || (stars == 1 && !strings.HasSuffix(pattern, "*")) {
			return fmt.Errorf("%w: model pattern %q must have one trailing star", ErrPromptRuleInvalid, pattern)
		}
		wild := strings.HasSuffix(pattern, "*")
		// map 本身是引用类型，直接取 map 值即可共享同一份去重集合（不能取 map 指针再索引）。
		seen := seenExact
		if wild {
			seen = seenWild
			pattern = strings.TrimSuffix(pattern, "*")
		}
		if _, dup := seen[pattern]; dup {
			return fmt.Errorf("%w: duplicate model pattern %q", ErrPromptRuleInvalid, pattern)
		}
		seen[pattern] = struct{}{}
	}
	switch {
	case rule.PlanMode == dbmodel.PromptRulePlanModeSelected && len(rule.PlanIDs) == 0 && !rule.IncludeNoPlan:
		return fmt.Errorf("%w: plan scope is empty", ErrPromptRuleInvalid)
	case rule.ChannelMode != dbmodel.PromptRuleChannelModeAll && len(rule.ChannelIDs) == 0:
		return fmt.Errorf("%w: channel scope is empty", ErrPromptRuleInvalid)
	case rule.ModelMode == dbmodel.PromptRuleModelModeSelected && len(rule.Models) == 0:
		return fmt.Errorf("%w: model scope is empty", ErrPromptRuleInvalid)
	}
	return nil
}

// promptRuleIDsExist 校验 selected 引用的一批 ID 真实存在：不去重（selected
// 匹配本来就是集合语义），数量不齐给显式错误，绝不静默丢弃。
func promptRuleIDsExist(tx *gorm.DB, name string, table any, ids []int) error {
	if len(ids) == 0 {
		return nil
	}
	var count int64
	if err := tx.Model(table).Where("id IN ?", ids).Count(&count).Error; err != nil {
		return err
	}
	if int(count) != len(ids) {
		return fmt.Errorf("%w: %s references %d missing ids", ErrPromptRuleInvalid, name, len(ids)-int(count))
	}
	return nil
}

// validatePromptRuleIDList 做引用 ID 列表的去重/正值校验：无论作用域切到哪个
// all，残留的无效旧 ids 都拒绝（让 UI 清除），重复也不接受——集合语义下
// 多余项只会让 UI 困惑，显式报错不静默去重。
func validatePromptRuleIDList(planIDs []int, planField string, channelIDs []int, channelField string) error {
	if err := validatePromptRuleIDs(planIDs, planField); err != nil {
		return err
	}
	return validatePromptRuleIDs(channelIDs, channelField)
}

// validatePromptRuleIDs 一条列表的去重/正值校验：全为正值且无重复。
func validatePromptRuleIDs(ids []int, field string) error {
	seen := make(map[int]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return fmt.Errorf("%w: %s contains non-positive %d", ErrPromptRuleInvalid, field, id)
		}
		if _, dup := seen[id]; dup {
			return fmt.Errorf("%w: %s contains duplicate %d", ErrPromptRuleInvalid, field, id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

// validatePromptRuleInTx 在事务里做带查库的部分：引用 ID 的存在性，显式报错不静默。
func validatePromptRuleInTx(tx *gorm.DB, rule *dbmodel.PromptRule) error {
	if err := validatePromptRule(rule); err != nil {
		return err
	}
	if err := promptRuleIDsExist(tx, "access_plans", &dbmodel.AccessPlan{}, rule.PlanIDs); err != nil {
		return err
	}
	return promptRuleIDsExist(tx, "channels", &dbmodel.Channel{}, rule.ChannelIDs)
}

// PromptRuleCreate 新建一条规则：body id 忽略（服务端分配）、时间列交给 GORM；
// slice 列按 serializer 落库。校验失败显式报 ErrPromptRuleInvalid。
func PromptRuleCreate(rule *dbmodel.PromptRule, ctx context.Context) error {
	rule.ID = 0
	rule.CreatedAt, rule.UpdatedAt = time.Time{}, time.Time{}
	rdb := db.GetDB()
	if rdb == nil {
		return ErrPromptRuleStoreUnavailable
	}
	return rdb.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := validatePromptRuleInTx(tx, rule); err != nil {
			return err
		}
		return tx.Create(rule).Error
	})
}

// PromptRuleUpdate 按 body.id 定位一条完整规则（完整覆盖式更新）：先读旧行，
// 校验失败显式报错；成功后按 Select 允许列整行覆盖，updated_at 每次修改由服务端
// 时间推进（而非冻结），created_at 保留库里的原值。
func PromptRuleUpdate(rule *dbmodel.PromptRule, ctx context.Context) error {
	if rule.ID <= 0 {
		return fmt.Errorf("%w: bad id", ErrPromptRuleInvalid)
	}
	rdb := db.GetDB()
	if rdb == nil {
		return ErrPromptRuleStoreUnavailable
	}
	return rdb.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing dbmodel.PromptRule
		if err := tx.First(&existing, rule.ID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrPromptRuleNotFound
			}
			return err
		}
		if err := validatePromptRuleInTx(tx, rule); err != nil {
			return err
		}
		rule.CreatedAt = existing.CreatedAt
		rule.UpdatedAt = time.Now()
		return tx.Model(&dbmodel.PromptRule{}).
			Where("id = ?", rule.ID).
			Select(promptRuleUpdateColumns).
			Updates(rule).Error
	})
}

// PromptRuleDelete 按 id 删一条；孤儿（migration 迁来的历史行）也要能删除。
// 单行写不走事务。
func PromptRuleDelete(id int, ctx context.Context) error {
	if id <= 0 {
		return fmt.Errorf("%w: bad id", ErrPromptRuleInvalid)
	}
	rdb := db.GetDB()
	if rdb == nil {
		return ErrPromptRuleStoreUnavailable
	}
	result := rdb.WithContext(ctx).Delete(&dbmodel.PromptRule{}, id)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrPromptRuleNotFound
	}
	return nil
}

// PromptRuleToggle 按 id 只开关 enabled 一列，显式列写 false 不丢；单行写不走事务。
func PromptRuleToggle(id int, enabled bool, ctx context.Context) error {
	if id <= 0 {
		return fmt.Errorf("%w: bad id", ErrPromptRuleInvalid)
	}
	rdb := db.GetDB()
	if rdb == nil {
		return ErrPromptRuleStoreUnavailable
	}
	res := rdb.WithContext(ctx).Model(&dbmodel.PromptRule{}).
		Where("id = ?", id).Update("enabled", enabled)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrPromptRuleNotFound
	}
	return nil
}

// PromptRuleReorder 按 ids 全量精确排列 sort：重复/遗漏/未知项整失败不静默修正；
// 全部写在一个事务里，原子提交，中途错误整体回滚。
func PromptRuleReorder(ids []int, ctx context.Context) error {
	seen := make(map[int]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return fmt.Errorf("%w: non-positive id %d", ErrPromptRuleReorder, id)
		}
		if _, dup := seen[id]; dup {
			return fmt.Errorf("%w: duplicate id %d", ErrPromptRuleReorder, id)
		}
		seen[id] = struct{}{}
	}
	rdb := db.GetDB()
	if rdb == nil {
		return ErrPromptRuleStoreUnavailable
	}
	return rdb.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing []dbmodel.PromptRule
		if err := tx.Model(&dbmodel.PromptRule{}).Select("id").Find(&existing).Error; err != nil {
			return err
		}
		if len(existing) != len(ids) {
			return fmt.Errorf("%w: got %d ids for %d rules", ErrPromptRuleReorder, len(ids), len(existing))
		}
		current := make(map[int]struct{}, len(existing))
		for _, rule := range existing {
			current[rule.ID] = struct{}{}
		}
		for _, id := range ids {
			if _, ok := current[id]; !ok {
				return fmt.Errorf("%w: unknown id %d", ErrPromptRuleReorder, id)
			}
		}
		for index, id := range ids {
			if err := tx.Model(&dbmodel.PromptRule{}).Where("id = ?", id).Update("sort", index).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
