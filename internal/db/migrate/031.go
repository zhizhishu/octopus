package migrate

import (
	"fmt"
	"strings"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 31,
		Up:      migratePromptRules,
	})
}

// migratePromptRules 把旧四层的系统提示词（全局设置 + 方案 + 模型路由 + 渠道上的
// 三元组）一次性迁到 prompt_rules 表。旧字段/旧写接口一律不动（回退语义见
// internal/model/prompt_rule.go 顶部注释）。
//
// 幂等与防复活：不依赖外层 runMigrationsWithRecord 的「Up 结束才记账」——崩溃间隙
// 会把 Up 重跑一遍。031 自己在**同一事务**里先 ensureMigrationRecordTable，再查
// version 031 是否 success，然后插入规则、upsert 本版本记录；已有 success 直接
// return。规则插入与记录同事务提交，独立直跑两次 Up 也不重复；删掉一条规则再跑
// Up 不会复活。
func migratePromptRules(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := ensureMigrationRecordTable(tx); err != nil {
			return fmt.Errorf("ensure migration record table: %w", err)
		}
		var rec MigrationRecord
		err := tx.Where("version = ?", 31).Limit(1).Find(&rec).Error
		if err != nil {
			return fmt.Errorf("read migration record 031: %w", err)
		}
		if rec.Version == 31 && rec.Status == MigrationRecordStatusSuccess {
			return nil
		}

		if err := insertPromptRulesFromOldFields(tx); err != nil {
			return err
		}
		if err := upsertMigrationRecord(tx, 31, MigrationRecordStatusSuccess); err != nil {
			return fmt.Errorf("record migration 031 success: %w", err)
		}
		log.Infof("migration 031: prompt rules seeded from legacy four-layer fields")
		return nil
	})
}

// MigratePromptRulesForTest 把 031 的 Up 暴露给 relay 的迁移 golden 测试
// （golden 样本必须来自真实迁移产物，不能手抄）；仅内部包，不是公开 API。
func MigratePromptRulesForTest(db *gorm.DB) error {
	return migratePromptRules(db)
}

// insertPromptRulesFromOldFields 从旧四层字段生成规则并写入（旧字段只读，不改）。
// 层级：规则的具体度由三条作用域里最高受限的那条维度决定（channel > model > plan），
// 迁移行 Sort=0、同层按 id；admin_only 受众沿用旧取值。共享 RouteProfile 的旧路由
// 提示词合并到引用它的方案集合；孤儿 profile 保留为 selected 空 PlanIDs +
// IncludeNoPlan=false（永不命中，绝不放大范围）；同一 (profile, 请求模型) 的重复
// 规范化 RequestModel 只迁旧首条（首条为空/关闭也不能让次条生效）。
func insertPromptRulesFromOldFields(tx *gorm.DB) error {
	// 全局：三个设置合一（开/文本/位置）；受众沿用 prompt_audience。
	globalRules, err := promptRulesFromGlobalSettings(tx)
	if err != nil {
		return err
	}

	// 方案：有文本才迁（开/关沿用 PromptOverrideActive 语义）。
	if tx.Migrator().HasTable(&model.AccessPlan{}) {
		plans := make([]model.AccessPlan, 0)
		if err := tx.Order("id ASC").Find(&plans).Error; err != nil {
			return fmt.Errorf("read access_plans: %w", err)
		}
		for i := range plans {
			plan := &plans[i]
			if strings.TrimSpace(plan.SystemPromptOverride) == "" {
				continue
			}
			globalRules = append(globalRules, model.PromptRule{
				Name:     promptRuleName("plan:" + plan.Slug),
				Text:     plan.SystemPromptOverride,
				Position: promptRulePositionFrom(plan.PromptOverrideMode),
				PlanMode: model.PromptRulePlanModeSelected,
				PlanIDs:  []int{plan.ID},

				ChannelMode: model.PromptRuleChannelModeAll,
				ModelMode:   model.PromptRuleModelModeAll,
				Audience:    plan.PromptAudience,
			})
			rule := &globalRules[len(globalRules)-1]
			rule.Enabled = model.PromptOverrideActive(plan.PromptOverrideEnabled, plan.PromptOverrideMode, plan.SystemPromptOverride)
		}
	}

	// 模型路由：共享 RouteProfile 只生成一次（合并所有引用它的方案 ID），重复的
	// 规范化 RequestModel 只迁旧首条（含空文本/关闭的——它们不激活次条）。
	if tx.Migrator().HasTable(&model.AccessRouteRule{}) {
		rules := make([]model.AccessRouteRule, 0)
		if err := tx.Order("id ASC").Find(&rules).Error; err != nil {
			return fmt.Errorf("read access_route_rules: %w", err)
		}
		if tx.Migrator().HasTable(&model.AccessPlan{}) {
			plans := make([]model.AccessPlan, 0)
			if err := tx.Order("id ASC").Find(&plans).Error; err != nil {
				return fmt.Errorf("read access_plans (route merge): %w", err)
			}
			merged, err := promptRulesFromRouteRules(tx, rules, plans)
			if err != nil {
				return err
			}
			globalRules = append(globalRules, merged...)
		}
	}

	// 渠道。
	if tx.Migrator().HasTable(&model.Channel{}) {
		channels := make([]model.Channel, 0)
		if err := tx.Order("id ASC").Find(&channels).Error; err != nil {
			return fmt.Errorf("read channels: %w", err)
		}
		for i := range channels {
			channel := &channels[i]
			if strings.TrimSpace(channel.SystemPromptOverride) == "" {
				continue
			}
			globalRules = append(globalRules, model.PromptRule{
				Name:     promptRuleName("channel:" + channel.Name),
				Text:     channel.SystemPromptOverride,
				Position: promptRulePositionFrom(channel.PromptOverrideMode),
				PlanMode: model.PromptRulePlanModeAll,

				ChannelMode: model.PromptRuleChannelModeInclude,
				ChannelIDs:  []int{channel.ID},
				ModelMode:   model.PromptRuleModelModeAll,
				Audience:    channel.PromptAudience,
			})
			rule := &globalRules[len(globalRules)-1]
			rule.Enabled = model.PromptOverrideActive(channel.PromptOverrideEnabled, channel.PromptOverrideMode, channel.SystemPromptOverride)
		}
	}

	if len(globalRules) > 0 {
		if err := tx.Create(&globalRules).Error; err != nil {
			return fmt.Errorf("create prompt rules: %w", err)
		}
	}
	return nil
}

// promptRuleName 把迁移生成的规则名收敛到列宽内（prompt_rules.name 是 191）。
// 方案 slug / profile 名 / 渠道名都是用户可填的长文本，直接拼进名字在 MySQL 上会被
// 截断或报错，所以写库前统一按 []rune 截：按字符（而不是字节）截不会切断中文或 emoji，
// 也不会把“半个字”留给前端。只动 name，不改 text / 作用域 / 开关 / 旧字段。
func promptRuleName(name string) string {
	const maxRunes = 191
	runes := []rune(name)
	if len(runes) <= maxRunes {
		return name
	}
	return string(runes[:maxRunes])
}

// promptRulePositionFrom 把旧四层模式映射到规则位置（同 NormalizePromptOverrideMode
// 语义：append_system/replace_system/空 → before）。
func promptRulePositionFrom(mode model.PromptOverrideMode) model.PromptRulePosition {
	if model.NormalizePromptOverrideMode(mode) == model.PromptOverrideModeAfter {
		return model.PromptRulePositionAfter
	}
	return model.PromptRulePositionBefore
}

// promptRulesFromGlobalSettings 读全局三设置合一：
// 有文本才迁（开/关沿用 PromptOverrideActive 语义，legacy 模式按开算）；
// 空文本无可迁内容，不建壳规则。
// 受众：无该设置时按 all；设置存在但值无效（含未知非空）时原样保留，
// 由 PromptAudienceAllows 的默认拒绝方向继续拒绝，不放大受众。
func promptRulesFromGlobalSettings(tx *gorm.DB) ([]model.PromptRule, error) {
	rules := make([]model.PromptRule, 0)
	if !tx.Migrator().HasTable(&model.Setting{}) {
		return rules, nil
	}
	read := func(key model.SettingKey) (model.Setting, error) {
		var row model.Setting
		if err := tx.Where(model.SettingKeyIs(key)).Limit(1).Find(&row).Error; err != nil {
			return row, fmt.Errorf("read %s: %w", key, err)
		}
		return row, nil
	}
	text, err := read(model.SettingKeyPromptOverrideSystem)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(text.Value) == "" {
		return rules, nil
	}
	modeRow, err := read(model.SettingKeyPromptOverrideMode)
	if err != nil {
		return nil, err
	}
	enabledRow, err := read(model.SettingKeyPromptOverrideEnabled)
	if err != nil {
		return nil, err
	}
	audienceRow, err := read(model.SettingKeyPromptAudience)
	if err != nil {
		return nil, err
	}
	active := model.PromptOverrideActive(enabledRow.Value == "true", model.PromptOverrideMode(modeRow.Value), text.Value)
	audience := model.PromptAudienceAll
	if audienceRow.Key != "" {
		audience = model.PromptAudience(audienceRow.Value)
	}
	rules = append(rules, model.PromptRule{
		Name:        promptRuleName("global"),
		Enabled:     active,
		Text:        text.Value,
		Position:    promptRulePositionFrom(model.PromptOverrideMode(modeRow.Value)),
		PlanMode:    model.PromptRulePlanModeAll,
		ChannelMode: model.PromptRuleChannelModeAll,
		ModelMode:   model.PromptRuleModelModeAll,
		Audience:    audience,
	})
	return rules, nil
}

// promptRulesFromRouteRules 从旧模型路由规则迁规则：共享 RouteProfile 只生成一次，
// PlanIDs 合并所有引用它的方案；孤儿 profile 保留为 selected 空 PlanIDs +
// IncludeNoPlan=false（永不命中，绝不放大范围）；同一 (profile, 规范化 RequestModel)
// 只迁旧首条（首条为空文本跳过不建规则，但仍占位去重，不能让次条生效）。
func promptRulesFromRouteRules(tx *gorm.DB, routeRules []model.AccessRouteRule, plans []model.AccessPlan) ([]model.PromptRule, error) {
	rules := make([]model.PromptRule, 0)
	profileNames := make(map[int]string)
	if tx.Migrator().HasTable(&model.AccessRouteProfile{}) {
		profiles := make([]model.AccessRouteProfile, 0)
		if err := tx.Order("id ASC").Find(&profiles).Error; err != nil {
			return nil, fmt.Errorf("read access_route_profiles: %w", err)
		}
		for i := range profiles {
			profileNames[profiles[i].ID] = profiles[i].Name
		}
	}
	planIDsByProfile := make(map[int][]int)
	for i := range plans {
		id := plans[i].RouteProfileID
		planIDsByProfile[id] = append(planIDsByProfile[id], plans[i].ID)
	}
	seen := make(map[string]bool)
	for i := range routeRules {
		rule := &routeRules[i]
		cleaned := strings.ToLower(model.CleanOneMillionCapabilityModelName(rule.RequestModel))
		key := fmt.Sprintf("%d|%s", rule.RouteProfileID, cleaned)
		if seen[key] {
			continue
		}
		seen[key] = true
		// 空文本不建规则也不建壳：旧语义里空文本本来就不注入，直接跳过；
		// seen 已占位，同键次条仍不会生效。
		if strings.TrimSpace(rule.SystemPromptOverride) == "" {
			continue
		}
		profileLabel := profileNames[rule.RouteProfileID]
		if profileLabel == "" {
			profileLabel = fmt.Sprintf("%d", rule.RouteProfileID)
		}
		var planIDs []int
		if ids := planIDsByProfile[rule.RouteProfileID]; len(ids) > 0 {
			planIDs = ids
		}
		r := model.PromptRule{
			Name:     promptRuleName("route:" + profileLabel + "/" + cleaned),
			Text:     rule.SystemPromptOverride,
			Position: promptRulePositionFrom(rule.PromptOverrideMode),
			PlanMode: model.PromptRulePlanModeSelected,
			PlanIDs:  planIDs,

			ChannelMode: model.PromptRuleChannelModeAll,
			ModelMode:   model.PromptRuleModelModeSelected,
			Models:      []string{cleaned},
			Audience:    rule.PromptAudience,
		}
		r.Enabled = model.PromptOverrideActive(rule.PromptOverrideEnabled, rule.PromptOverrideMode, rule.SystemPromptOverride)
		if planIDs == nil {
			r.IncludeNoPlan = false // 孤儿 profile：永不命中，绝不放大范围
		}
		rules = append(rules, r)
	}
	return rules, nil
}
