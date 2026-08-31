package op

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/utils/cache"
)

var accessPlanCache = cache.New[int, model.AccessPlan](16)
var accessPlanSlugCache = cache.New[string, int](16)
var apiKeyAccessPlanCache = cache.New[int, []model.APIKeyAccessPlan](16)
var userAccessPlanCache = cache.New[int, []model.UserAccessPlan](16)

func AccessPlanList(ctx context.Context) ([]model.AccessPlan, error) {
	if err := ensureAccessPlanCache(ctx); err != nil {
		return nil, err
	}
	plans := make([]model.AccessPlan, 0, accessPlanCache.Len())
	for _, plan := range accessPlanCache.GetAll() {
		plans = append(plans, plan)
	}
	sortAccessPlans(plans)
	return plans, nil
}

func AccessRouteProfileList(ctx context.Context) ([]model.AccessRouteProfile, error) {
	profiles := []model.AccessRouteProfile{}
	if err := db.GetDB().WithContext(ctx).Preload("Rules.Targets").Find(&profiles).Error; err != nil {
		return nil, err
	}
	sort.Slice(profiles, func(i, j int) bool {
		return profiles[i].ID < profiles[j].ID
	})
	return profiles, nil
}

func AccessBillingProfileList(ctx context.Context) ([]model.AccessBillingProfile, error) {
	profiles := []model.AccessBillingProfile{}
	if err := db.GetDB().WithContext(ctx).Preload("ModelRules").Find(&profiles).Error; err != nil {
		return nil, err
	}
	sort.Slice(profiles, func(i, j int) bool {
		return profiles[i].ID < profiles[j].ID
	})
	return profiles, nil
}

func AccessPlanCreate(plan *model.AccessPlan, ctx context.Context) error {
	if err := normalizeAccessPlan(plan); err != nil {
		return err
	}
	if err := ensureAccessPlanProfiles(plan, ctx); err != nil {
		return err
	}
	if plan.IsDefault {
		plan.Enabled = true
	}
	tx := db.GetDB().WithContext(ctx).Begin()
	if err := tx.Omit("RouteProfile", "BillingProfile").Create(plan).Error; err != nil {
		tx.Rollback()
		return err
	}
	if plan.IsDefault {
		if err := tx.Model(&model.AccessPlan{}).Where("id <> ?", plan.ID).Update("is_default", false).Error; err != nil {
			tx.Rollback()
			return err
		}
	}
	if err := tx.Commit().Error; err != nil {
		return err
	}
	return accessPlanRefreshCache(ctx)
}

func AccessPlanUpdate(plan *model.AccessPlan, ctx context.Context) error {
	if plan.ID <= 0 {
		return fmt.Errorf("invalid access plan id")
	}
	var existing model.AccessPlan
	if err := db.GetDB().WithContext(ctx).First(&existing, plan.ID).Error; err != nil {
		return err
	}
	if plan.RouteProfileID == 0 {
		plan.RouteProfileID = existing.RouteProfileID
	}
	if plan.BillingProfileID == 0 {
		plan.BillingProfileID = existing.BillingProfileID
	}
	if plan.Sort == 0 {
		plan.Sort = existing.Sort
	}
	if err := normalizeAccessPlan(plan); err != nil {
		return err
	}
	if err := ensureAccessPlanProfiles(plan, ctx); err != nil {
		return err
	}
	if plan.IsDefault {
		plan.Enabled = true
	}
	tx := db.GetDB().WithContext(ctx).Begin()
	if err := tx.Omit("RouteProfile", "BillingProfile").Save(plan).Error; err != nil {
		tx.Rollback()
		return err
	}
	if plan.IsDefault {
		if err := tx.Model(&model.AccessPlan{}).Where("id <> ?", plan.ID).Update("is_default", false).Error; err != nil {
			tx.Rollback()
			return err
		}
	}
	if err := tx.Commit().Error; err != nil {
		return err
	}
	if err := accessPlanEnsureDefault(ctx); err != nil {
		return err
	}
	return accessPlanRefreshCache(ctx)
}

func AccessPlanDelete(id int, ctx context.Context) error {
	if id <= 0 {
		return fmt.Errorf("invalid access plan id")
	}
	tx := db.GetDB().WithContext(ctx).Begin()
	if err := tx.Where("access_plan_id = ?", id).Delete(&model.UserAccessPlan{}).Error; err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Where("access_plan_id = ?", id).Delete(&model.APIKeyAccessPlan{}).Error; err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Delete(&model.AccessPlan{}, id).Error; err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit().Error; err != nil {
		return err
	}
	if err := accessPlanEnsureDefault(ctx); err != nil {
		return err
	}
	return accessPlanRefreshCache(ctx)
}

func AccessRouteProfileCreate(profile *model.AccessRouteProfile, ctx context.Context) error {
	profile.Name = strings.TrimSpace(profile.Name)
	if profile.Name == "" {
		return fmt.Errorf("route profile name is required")
	}
	if err := db.GetDB().WithContext(ctx).Omit("Rules").Create(profile).Error; err != nil {
		return err
	}
	return accessPlanRefreshCache(ctx)
}

func AccessRouteProfileUpdate(profile *model.AccessRouteProfile, ctx context.Context) error {
	if profile.ID <= 0 {
		return fmt.Errorf("invalid route profile id")
	}
	profile.Name = strings.TrimSpace(profile.Name)
	if profile.Name == "" {
		return fmt.Errorf("route profile name is required")
	}
	if err := db.GetDB().WithContext(ctx).Omit("Rules").Save(profile).Error; err != nil {
		return err
	}
	return accessPlanRefreshCache(ctx)
}

func AccessRouteProfileDelete(id int, ctx context.Context) error {
	tx := db.GetDB().WithContext(ctx).Begin()
	var ruleIDs []int
	if err := tx.Model(&model.AccessRouteRule{}).Where("route_profile_id = ?", id).Pluck("id", &ruleIDs).Error; err != nil {
		tx.Rollback()
		return err
	}
	if len(ruleIDs) > 0 {
		if err := tx.Where("route_rule_id IN ?", ruleIDs).Delete(&model.AccessRouteTarget{}).Error; err != nil {
			tx.Rollback()
			return err
		}
	}
	if err := tx.Where("route_profile_id = ?", id).Delete(&model.AccessRouteRule{}).Error; err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Delete(&model.AccessRouteProfile{}, id).Error; err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit().Error; err != nil {
		return err
	}
	return accessPlanRefreshCache(ctx)
}

func AccessRouteRuleCreate(rule *model.AccessRouteRule, ctx context.Context) error {
	normalizeAccessRouteRule(rule)
	if err := validateAccessRouteRule(rule); err != nil {
		return err
	}
	if err := db.GetDB().WithContext(ctx).Omit("Targets").Create(rule).Error; err != nil {
		return err
	}
	return accessPlanRefreshCache(ctx)
}

func AccessRouteRuleUpdate(rule *model.AccessRouteRule, ctx context.Context) error {
	if rule.ID <= 0 {
		return fmt.Errorf("invalid route rule id")
	}
	normalizeAccessRouteRule(rule)
	if err := validateAccessRouteRule(rule); err != nil {
		return err
	}
	if err := db.GetDB().WithContext(ctx).Omit("Targets").Save(rule).Error; err != nil {
		return err
	}
	return accessPlanRefreshCache(ctx)
}

func AccessRouteRuleDelete(id int, ctx context.Context) error {
	tx := db.GetDB().WithContext(ctx).Begin()
	if err := tx.Where("route_rule_id = ?", id).Delete(&model.AccessRouteTarget{}).Error; err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Delete(&model.AccessRouteRule{}, id).Error; err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit().Error; err != nil {
		return err
	}
	return accessPlanRefreshCache(ctx)
}

func AccessRouteTargetCreate(target *model.AccessRouteTarget, ctx context.Context) error {
	normalizeAccessRouteTarget(target)
	if err := validateAccessRouteTarget(target); err != nil {
		return err
	}
	if err := db.GetDB().WithContext(ctx).Create(target).Error; err != nil {
		return err
	}
	return accessPlanRefreshCache(ctx)
}

func AccessRouteTargetUpdate(target *model.AccessRouteTarget, ctx context.Context) error {
	if target.ID <= 0 {
		return fmt.Errorf("invalid route target id")
	}
	normalizeAccessRouteTarget(target)
	if err := validateAccessRouteTarget(target); err != nil {
		return err
	}
	if err := db.GetDB().WithContext(ctx).Save(target).Error; err != nil {
		return err
	}
	return accessPlanRefreshCache(ctx)
}

func AccessRouteTargetDelete(id int, ctx context.Context) error {
	if err := db.GetDB().WithContext(ctx).Delete(&model.AccessRouteTarget{}, id).Error; err != nil {
		return err
	}
	return accessPlanRefreshCache(ctx)
}

func AccessBillingProfileCreate(profile *model.AccessBillingProfile, ctx context.Context) error {
	normalizeAccessBillingProfile(profile)
	if err := validateAccessBillingProfile(profile); err != nil {
		return err
	}
	if err := db.GetDB().WithContext(ctx).Omit("ModelRules").Create(profile).Error; err != nil {
		return err
	}
	return accessPlanRefreshCache(ctx)
}

func AccessBillingProfileUpdate(profile *model.AccessBillingProfile, ctx context.Context) error {
	if profile.ID <= 0 {
		return fmt.Errorf("invalid billing profile id")
	}
	normalizeAccessBillingProfile(profile)
	if err := validateAccessBillingProfile(profile); err != nil {
		return err
	}
	if err := db.GetDB().WithContext(ctx).Omit("ModelRules").Save(profile).Error; err != nil {
		return err
	}
	return accessPlanRefreshCache(ctx)
}

func AccessBillingProfileDelete(id int, ctx context.Context) error {
	tx := db.GetDB().WithContext(ctx).Begin()
	if err := tx.Where("billing_profile_id = ?", id).Delete(&model.AccessBillingModelRule{}).Error; err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Delete(&model.AccessBillingProfile{}, id).Error; err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit().Error; err != nil {
		return err
	}
	return accessPlanRefreshCache(ctx)
}

func AccessBillingModelRuleCreate(rule *model.AccessBillingModelRule, ctx context.Context) error {
	normalizeAccessBillingModelRule(rule)
	if err := validateAccessBillingModelRule(rule); err != nil {
		return err
	}
	if err := db.GetDB().WithContext(ctx).Create(rule).Error; err != nil {
		return err
	}
	return accessPlanRefreshCache(ctx)
}

func AccessBillingModelRuleUpdate(rule *model.AccessBillingModelRule, ctx context.Context) error {
	if rule.ID <= 0 {
		return fmt.Errorf("invalid billing model rule id")
	}
	normalizeAccessBillingModelRule(rule)
	if err := validateAccessBillingModelRule(rule); err != nil {
		return err
	}
	if err := db.GetDB().WithContext(ctx).Save(rule).Error; err != nil {
		return err
	}
	return accessPlanRefreshCache(ctx)
}

func AccessBillingModelRuleDelete(id int, ctx context.Context) error {
	if err := db.GetDB().WithContext(ctx).Delete(&model.AccessBillingModelRule{}, id).Error; err != nil {
		return err
	}
	return accessPlanRefreshCache(ctx)
}

func AccessPlanSelect(apiKeyID int, headerPlan string, ctx context.Context) (*model.AccessPlan, error) {
	if err := ensureAccessPlanCache(ctx); err != nil {
		return nil, err
	}
	normalizedHeader := normalizeAccessPlanSlug(headerPlan)
	bindings, hasBindings := apiKeyAccessPlanCache.Get(apiKeyID)
	if apiKeyID > 0 && !hasBindings {
		if err := APIKeyAccessPlanBindDefaultIfEmpty(apiKeyID, ctx); err != nil {
			return nil, err
		}
		bindings, hasBindings = apiKeyAccessPlanCache.Get(apiKeyID)
	}
	bindings = enabledBindings(bindings)

	if normalizedHeader != "" {
		if hasBindings {
			if len(bindings) == 0 {
				return nil, fmt.Errorf("no access plan is available for this API key")
			}
			for _, binding := range bindings {
				plan, ok := accessPlanCache.Get(binding.AccessPlanID)
				if ok && plan.Enabled && plan.Slug == normalizedHeader {
					return copyAccessPlan(plan), nil
				}
			}
			return nil, fmt.Errorf("access plan %q is not available for this API key", normalizedHeader)
		}
		id, ok := accessPlanSlugCache.Get(normalizedHeader)
		if !ok {
			return nil, fmt.Errorf("access plan %q not found", normalizedHeader)
		}
		plan, ok := accessPlanCache.Get(id)
		if !ok || !plan.Enabled {
			return nil, fmt.Errorf("access plan %q is disabled", normalizedHeader)
		}
		return copyAccessPlan(plan), nil
	}

	// A key whose bound plans are ALL disabled must not have every request hard-fail:
	// that bricks the key (e.g. Cursor shows the full model list but every call 403s)
	// the moment an admin disables the one plan it was bound to. Treat it like an
	// unbound key and fall back to the default plan / model pool — matching the
	// /v1/models LIST path (accessPlansForAPIKeyRoutes returns no-plan → pool). This
	// keeps disabling a single plan from taking a key bound to it fully offline; an
	// explicit header-requested plan (handled above) still errors, since that is a
	// deliberate ask for a specific unavailable plan.
	if hasBindings && len(bindings) == 0 {
		return accessPlanDefault(ctx)
	}
	if len(bindings) > 0 {
		sort.Slice(bindings, func(i, j int) bool {
			if bindings[i].IsDefault != bindings[j].IsDefault {
				return bindings[i].IsDefault
			}
			return bindings[i].AccessPlanID < bindings[j].AccessPlanID
		})
		plan, ok := accessPlanCache.Get(bindings[0].AccessPlanID)
		if ok && plan.Enabled {
			return copyAccessPlan(plan), nil
		}
	}

	return accessPlanDefault(ctx)
}

func AccessPlanGroupForModel(plan *model.AccessPlan, requestModel string, ctx context.Context) (model.Group, *model.AccessRouteRule, bool, error) {
	if plan == nil || plan.RouteProfile == nil {
		return model.Group{}, nil, false, nil
	}
	requestModel = model.CleanOneMillionCapabilityModelName(requestModel)
	for _, rule := range plan.RouteProfile.Rules {
		if !strings.EqualFold(model.CleanOneMillionCapabilityModelName(rule.RequestModel), requestModel) {
			continue
		}
		ruleCopy := rule
		normalizeAccessRouteRule(&ruleCopy)
		items := make([]model.GroupItem, 0, len(rule.Targets))
		for _, target := range rule.Targets {
			if !accessRouteTargetAvailable(target) {
				continue
			}
			items = append(items, model.GroupItem{
				ChannelID:            target.ChannelID,
				ModelName:            model.CleanOneMillionCapabilityModelName(target.UpstreamModel),
				Priority:             target.Priority,
				Weight:               target.Weight,
				RoutingWeight:        target.Weight,
				BillingModelSource:   ruleCopy.BillingModelSource,
				BillingModelOverride: ruleCopy.BillingModelOverride,
			})
		}
		if len(items) == 0 {
			return model.Group{}, &ruleCopy, false, nil
		}
		// 模型池已砍，规则 Mode 是唯一来源；ModeLocked=true 保护画布显式选择不被全局默认覆盖。
		return model.Group{
			Name:       model.CleanOneMillionCapabilityModelName(requestModel),
			Mode:       ruleCopy.Mode,
			ModeLocked: true,
			Items:      items,
		}, &ruleCopy, true, nil
	}
	return model.Group{}, nil, false, nil
}

// AccessPlanSyncEnabledChannels reconciles channel targets for every access plan that
// opted into AutoSyncChannels. For each existing route rule (request model) it:
//
//   - ADDS: any currently-enabled channel that serves that model but is not yet a
//     target (priority mirrors the channel's own Priority — so equal-priority channels
//     stay genuinely parallel under Spread and the value stops churning on every
//     re-sync; weight 1, enabled). Hand-tuned priorities/weights of surviving targets
//     are not touched.
//   - REMOVES: any existing target whose channel is no longer enabled or no longer
//     serves the rule's model, so a disabled channel is automatically evicted without
//     requiring a manual rebuild.
//
// Plans that did NOT opt in (AutoSyncChannels=false) are never touched — they remain
// strict allow-lists. New request models (no rule yet) are left to the group fallback,
// not force-created here. Idempotent: safe to call after any channel enable/disable /
// sync / create.
func AccessPlanSyncEnabledChannels(ctx context.Context) error {
	if err := ensureAccessPlanCache(ctx); err != nil {
		return err
	}

	// Build a map: channelID → the models it serves (enabled channels only). Each entry
	// maps a cleaned+lowercased client-facing model name to how the channel serves it:
	// the upstream model name sent on the wire, plus whether it came from an explicit
	// model_mapping (authoritative for the upstream name) or a plainly-selected model
	// (identity — client name == upstream name).
	type servedModel struct {
		upstream string
		mapped   bool
	}
	type enabledChannel struct {
		id       int
		priority int
		models   map[string]servedModel
	}
	allChannels := channelCache.GetAll()
	// Fail-safe: an empty channel cache almost certainly means it isn't loaded yet
	// (a real deployment always has channels). Evicting every AutoSync target on a
	// cache miss would nuke all routes; a genuine "all channels disabled" state still
	// has entries here (just none enabled), so it is unaffected by this guard.
	if len(allChannels) == 0 {
		return nil
	}
	enabledByID := make(map[int]enabledChannel)
	for _, ch := range allChannels {
		if !ch.Enabled {
			continue
		}
		served := make(map[string]servedModel)
		selectedSet := make(map[string]struct{})
		for _, name := range model.ChannelSelectedModelNames(ch) {
			clean := model.CleanOneMillionCapabilityModelName(name)
			if key := strings.ToLower(clean); key != "" {
				served[key] = servedModel{upstream: clean} // identity: client name == upstream name
				selectedSet[key] = struct{}{}
			}
		}
		// Honor the channel's model_mapping: each mapping key is an additional
		// client-facing model this channel serves, rewritten to the mapped upstream
		// name on the wire. This is what lets a mapped channel (e.g. NVIDIA whose
		// upstream name is "deepseek-ai/deepseek-v4-pro") join the pool's canonical
		// "deepseek-v4-pro" route on the canvas/plan instead of sitting alone under
		// its ugly upstream alias.
		for clientName, upstreamName := range ch.ModelMapping {
			clientClean := model.CleanOneMillionCapabilityModelName(clientName)
			upstreamClean := model.CleanOneMillionCapabilityModelName(upstreamName)
			key := strings.ToLower(clientClean)
			if key == "" || upstreamClean == "" {
				continue
			}
			// Only expose the alias when the channel actually serves the mapped upstream
			// model (it must be a selected model). accessRouteTargetAvailable validates a
			// target's UpstreamModel against selected_models, so aliasing to an unselected
			// upstream would only sync a target that route selection immediately drops.
			if _, servesUpstream := selectedSet[strings.ToLower(upstreamClean)]; !servesUpstream {
				continue
			}
			served[key] = servedModel{upstream: upstreamClean, mapped: true} // mapping wins
		}
		if len(served) > 0 {
			enabledByID[ch.ID] = enabledChannel{id: ch.ID, priority: ch.Priority, models: served}
		}
	}
	// NOTE: we intentionally do NOT early-return when enabledByID is empty — we still
	// need to evict stale targets from AutoSyncChannels plans if every channel was disabled.

	// Deterministic channel-ID order reused by both the per-rule target sync
	// and the missing-rule creation pass below.
	addIDs := make([]int, 0, len(enabledByID))
	for id := range enabledByID {
		addIDs = append(addIDs, id)
	}
	sort.Ints(addIDs)

	changed := false
	gormDB := db.GetDB().WithContext(ctx)
	for _, plan := range accessPlanCache.GetAll() {
		if !plan.AutoSyncChannels || plan.RouteProfile == nil {
			continue
		}
		for _, rule := range plan.RouteProfile.Rules {
			ruleModel := strings.ToLower(model.CleanOneMillionCapabilityModelName(rule.RequestModel))
			if ruleModel == "" {
				continue
			}

			// --- REMOVE: targets whose channel is disabled or no longer serves this model ---
			for _, t := range rule.Targets {
				ec, enabled := enabledByID[t.ChannelID]
				if !enabled {
					// Channel disabled or deleted — evict.
					if err := gormDB.Delete(&model.AccessRouteTarget{}, t.ID).Error; err != nil {
						// Best-effort: log-worthy but non-fatal; keep processing.
						continue
					}
					changed = true
					continue
				}
				if _, serves := ec.models[ruleModel]; !serves {
					// Channel enabled but no longer serves this rule's model — evict.
					if err := gormDB.Delete(&model.AccessRouteTarget{}, t.ID).Error; err != nil {
						continue
					}
					changed = true
				}
			}

			// --- ADD: enabled channels that serve this model but are not yet targets ---
			existing := make(map[int]struct{}, len(rule.Targets))
			for _, t := range rule.Targets {
				existing[t.ChannelID] = struct{}{}
			}
			// Deterministic order + STABLE per-target priority. Previously each newly
			// auto-synced target got a distinct, monotonically-increasing priority
			// (max+1, max+2, …) in Go-map (random) iteration order, so every re-sync /
			// channel toggle / post-deploy re-save churned the canvas priorities and
			// manufactured artificial fill-first tiers that quietly defeated round-robin.
			// A synced target's priority now mirrors its CHANNEL's own Priority (the same
			// value that buckets Spread/round-robin at routing time), so equal-priority
			// channels stay genuinely parallel ("并列"), the layout stops reshuffling on
			// every deploy, and the priority the operator sees matches what routing uses.
			// Hand-tuned priorities of surviving targets are never touched (they are not in
			// this ADD path).
			for _, chID := range addIDs {
				ec := enabledByID[chID]
				sm, serves := ec.models[ruleModel]
				if !serves {
					continue
				}
				if _, already := existing[ec.id]; already {
					continue
				}
				// A plainly-selected model keeps the rule's own casing exactly as before;
				// an explicit model_mapping is authoritative for the upstream name (even a
				// case-only remap).
				upstreamModel := model.CleanOneMillionCapabilityModelName(rule.RequestModel)
				if sm.mapped {
					upstreamModel = sm.upstream
				}
				target := model.AccessRouteTarget{
					RouteRuleID:   rule.ID,
					ChannelID:     ec.id,
					UpstreamModel: upstreamModel,
					Priority:      ec.priority,
					Weight:        1,
					Enabled:       true,
				}
				if err := gormDB.Create(&target).Error; err != nil {
					// Best-effort: a unique-constraint race just means it already exists;
					// skip it and keep syncing the rest.
					continue
				}
				existing[ec.id] = struct{}{}
				changed = true
			}
		}
	}

	// --- CREATE missing rules for AutoSync plans: models served by enabled channels
	// that have no RouteRule yet in the plan. Without this pass, a newly-onboarded
	// channel/mapping brings a model that sits in the pool search dropdown but never
	// appears on the canvas — because AccessPlanSyncEnabledChannels only managed
	// targets of *existing* rules. This pass creates the rule + its initial targets
	// in one shot, using the same channel-priority ordering as the existing ADD path.
	for _, plan := range accessPlanCache.GetAll() {
		if !plan.AutoSyncChannels || plan.RouteProfile == nil {
			continue
		}
		// Build a set of request_model names that already have a rule.
		ruleModels := make(map[string]int, len(plan.RouteProfile.Rules)) // model → ruleID
		for _, rule := range plan.RouteProfile.Rules {
			key := strings.ToLower(model.CleanOneMillionCapabilityModelName(rule.RequestModel))
			if key != "" {
				ruleModels[key] = rule.ID
			}
		}
		// Collect every model that at least one enabled channel serves and that is
		// NOT covered by an existing rule. Keep the first channel's casing of each
		// model as the rule's RequestModel and its upstream name.
		type pendingModel struct {
			requestModel string // the cleaned client-visible name for the rule
			channels     []struct {
				chID     int
				priority int
				upstream string // upstream name for this channel's target
			}
		}
		missing := make(map[string]*pendingModel)
		for _, chID := range addIDs {
			ec := enabledByID[chID]
			for modelKey, sm := range ec.models {
				if _, has := ruleModels[modelKey]; has {
					continue
				}
				pm, ok := missing[modelKey]
				if !ok {
					pm = &pendingModel{requestModel: sm.upstream}
					missing[modelKey] = pm
				}
				pm.channels = append(pm.channels, struct {
					chID     int
					priority int
					upstream string
				}{chID: ec.id, priority: ec.priority, upstream: sm.upstream})
			}
		}
		if len(missing) == 0 {
			continue
		}
		for _, pm := range missing {
			rule := model.AccessRouteRule{
				RouteProfileID: plan.RouteProfile.ID,
				RequestModel:   pm.requestModel,
				// Match the project-wide default for a fresh rule: fill_first (3),
				// the same value as the AccessRouteRule `gorm:"default:3"` column and
				// the frontend contract (normalizeRouteMode defaults to 3). Using the
				// named constant so the value can never drift from its meaning again
				// (GroupModeSpread is 1 — do NOT write a bare 3 and call it "spread").
				Mode:         model.GroupModeFillFirst,
				FallbackMode: model.AccessRouteFallbackGroup,
			}
			if err := gormDB.Create(&rule).Error; err != nil {
				continue
			}
			for _, ch := range pm.channels {
				target := model.AccessRouteTarget{
					RouteRuleID:   rule.ID,
					ChannelID:     ch.chID,
					UpstreamModel: ch.upstream,
					Priority:      ch.priority,
					Weight:        1,
					Enabled:       true,
				}
				if err := gormDB.Create(&target).Error; err != nil {
					continue
				}
			}
			changed = true
		}
	}

	if changed {
		return accessPlanRefreshCache(ctx)
	}
	return nil
}

func AccessPlanRouteModels(ctx context.Context) ([]string, error) {
	if err := ensureAccessPlanCache(ctx); err != nil {
		return nil, err
	}
	plans := enabledAccessPlans()
	return accessPlanRouteModelsFromPlans(plans), nil
}

func AccessPlanRouteModelsForAPIKey(apiKeyID int, ctx context.Context) ([]string, error) {
	plans, err := accessPlansForAPIKeyRoutes(apiKeyID, ctx)
	if err != nil {
		return nil, err
	}
	return accessPlanRouteModelsFromPlans(plans), nil
}

func AccessPlanRouteModelsForPlan(plan *model.AccessPlan) []string {
	if plan == nil {
		return nil
	}
	return accessPlanRouteModelsFromPlans([]model.AccessPlan{*plan})
}

func accessPlanRouteModelsFromPlans(plans []model.AccessPlan) []string {
	seen := make(map[string]struct{})
	models := make([]string, 0)
	for _, plan := range plans {
		if !plan.Enabled || plan.RouteProfile == nil {
			continue
		}
		for _, rule := range plan.RouteProfile.Rules {
			name := model.CleanOneMillionCapabilityModelName(rule.RequestModel)
			if name == "" {
				continue
			}
			hasAvailableTarget := false
			for _, target := range rule.Targets {
				if accessRouteTargetAvailable(target) {
					hasAvailableTarget = true
					break
				}
			}
			if !hasAvailableTarget {
				continue
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			models = append(models, name)
		}
	}
	sort.Strings(models)
	return models
}

func enabledAccessPlans() []model.AccessPlan {
	plans := make([]model.AccessPlan, 0, accessPlanCache.Len())
	for _, plan := range accessPlanCache.GetAll() {
		if plan.Enabled {
			plans = append(plans, plan)
		}
	}
	sortAccessPlans(plans)
	return plans
}

func accessPlansForAPIKeyRoutes(apiKeyID int, ctx context.Context) ([]model.AccessPlan, error) {
	if err := ensureAccessPlanCache(ctx); err != nil {
		return nil, err
	}
	if apiKeyID <= 0 {
		return enabledAccessPlans(), nil
	}

	bindings, hasBindings := apiKeyAccessPlanCache.Get(apiKeyID)
	if !hasBindings || len(bindings) == 0 {
		if err := APIKeyAccessPlanBindDefaultIfEmpty(apiKeyID, ctx); err != nil {
			return nil, err
		}
		bindings, hasBindings = apiKeyAccessPlanCache.Get(apiKeyID)
	}
	if !hasBindings {
		return enabledAccessPlans(), nil
	}

	bindings = enabledBindings(bindings)
	if len(bindings) == 0 {
		return nil, nil
	}
	plans := make([]model.AccessPlan, 0, len(bindings))
	for _, binding := range bindings {
		plan, ok := accessPlanCache.Get(binding.AccessPlanID)
		if !ok || !plan.Enabled {
			continue
		}
		plans = append(plans, plan)
	}
	sortAccessPlans(plans)
	return plans, nil
}

func accessRouteTargetAvailable(target model.AccessRouteTarget) bool {
	if !target.Enabled {
		return false
	}
	channel, ok := channelCache.Get(target.ChannelID)
	if !ok || !channel.Enabled {
		return false
	}

	models := normalizeRouteChannelModels(model.ChannelSelectedModelNames(channel)...)
	if len(models) == 0 {
		return false
	}
	_, ok = models[strings.ToLower(model.CleanOneMillionCapabilityModelName(target.UpstreamModel))]
	return ok
}

func normalizeRouteChannelModels(values ...string) map[string]struct{} {
	models := make(map[string]struct{})
	for _, value := range values {
		for _, modelName := range strings.Split(value, ",") {
			name := strings.ToLower(model.CleanOneMillionCapabilityModelName(modelName))
			if name != "" {
				models[name] = struct{}{}
			}
		}
	}
	return models
}

func APIKeyAccessPlanSet(apiKeyID int, planIDs []int, defaultPlanID int, ctx context.Context) error {
	if apiKeyID <= 0 {
		return fmt.Errorf("invalid API key id")
	}
	if err := ensureAPIKeyExists(apiKeyID, ctx); err != nil {
		return err
	}
	apiKey, err := apiKeyForAccessPlanBinding(apiKeyID, ctx)
	if err != nil {
		return err
	}
	uniqueIDs := make([]int, 0, len(planIDs)+1)
	seen := make(map[int]struct{}, len(planIDs)+1)
	for _, id := range planIDs {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		uniqueIDs = append(uniqueIDs, id)
	}
	if defaultPlanID > 0 {
		if _, ok := seen[defaultPlanID]; !ok {
			uniqueIDs = append(uniqueIDs, defaultPlanID)
		}
	}
	if len(uniqueIDs) == 0 {
		return fmt.Errorf("at least one access plan is required")
	}
	if defaultPlanID <= 0 {
		defaultPlanID = uniqueIDs[0]
	}
	if err := ensureAccessPlanCache(ctx); err != nil {
		return err
	}
	allowedPlanIDs, restricted, err := userAllowedAccessPlanIDSet(apiKey.UserID, ctx)
	if err != nil {
		return err
	}
	for _, id := range uniqueIDs {
		plan, ok := accessPlanCache.Get(id)
		if !ok || !plan.Enabled {
			return fmt.Errorf("access plan %d not found or disabled", id)
		}
		if restricted {
			if _, ok := allowedPlanIDs[id]; !ok {
				return fmt.Errorf("access plan %q is not available for API key owner", plan.Slug)
			}
		}
	}

	tx := db.GetDB().WithContext(ctx).Begin()
	if err := tx.Where("api_key_id = ?", apiKeyID).Delete(&model.APIKeyAccessPlan{}).Error; err != nil {
		tx.Rollback()
		return err
	}
	rows := make([]model.APIKeyAccessPlan, 0, len(uniqueIDs))
	for _, id := range uniqueIDs {
		rows = append(rows, model.APIKeyAccessPlan{
			APIKeyID:     apiKeyID,
			AccessPlanID: id,
			IsDefault:    id == defaultPlanID,
		})
	}
	if err := tx.Create(&rows).Error; err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit().Error; err != nil {
		return err
	}
	return accessPlanRefreshCache(ctx)
}

func UserAccessPlanSet(userID int, planIDs []int, defaultPlanID int, ctx context.Context) error {
	if userID <= 0 {
		return fmt.Errorf("invalid user id")
	}
	if _, err := UserGet(userID); err != nil {
		return err
	}
	if err := ensureAccessPlanCache(ctx); err != nil {
		return err
	}
	uniqueIDs := make([]int, 0, len(planIDs)+1)
	seen := make(map[int]struct{}, len(planIDs)+1)
	for _, id := range planIDs {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		uniqueIDs = append(uniqueIDs, id)
	}
	if defaultPlanID > 0 {
		if _, ok := seen[defaultPlanID]; !ok {
			seen[defaultPlanID] = struct{}{}
			uniqueIDs = append(uniqueIDs, defaultPlanID)
		}
	}
	if len(uniqueIDs) == 0 {
		defaultPlan, err := accessPlanDefault(ctx)
		if err != nil {
			return err
		}
		if defaultPlan == nil {
			return fmt.Errorf("no enabled access plan is available")
		}
		uniqueIDs = append(uniqueIDs, defaultPlan.ID)
		defaultPlanID = defaultPlan.ID
	}
	if defaultPlanID <= 0 {
		defaultPlanID = uniqueIDs[0]
	}
	for _, id := range uniqueIDs {
		plan, ok := accessPlanCache.Get(id)
		if !ok || !plan.Enabled {
			return fmt.Errorf("access plan %d not found or disabled", id)
		}
	}

	tx := db.GetDB().WithContext(ctx).Begin()
	if err := tx.Where("user_id = ?", userID).Delete(&model.UserAccessPlan{}).Error; err != nil {
		tx.Rollback()
		return err
	}
	rows := make([]model.UserAccessPlan, 0, len(uniqueIDs))
	for _, id := range uniqueIDs {
		rows = append(rows, model.UserAccessPlan{
			UserID:       userID,
			AccessPlanID: id,
			IsDefault:    id == defaultPlanID,
		})
	}
	if err := tx.Create(&rows).Error; err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit().Error; err != nil {
		return err
	}
	if err := accessPlanRefreshCache(ctx); err != nil {
		return err
	}
	return trimAPIKeyBindingsForUser(userID, ctx)
}

func AccessPlanUpdateBillingRules(accessPlanID int, defaultMultiplier float64, rules []model.AccessBillingModelRule, ctx context.Context) (model.AccessPlan, error) {
	plan, err := accessPlanLoadForWrite(accessPlanID, ctx)
	if err != nil {
		return model.AccessPlan{}, err
	}
	if defaultMultiplier <= 0 {
		defaultMultiplier = 1
	}

	tx := db.GetDB().WithContext(ctx).Begin()
	if err := tx.Model(&model.AccessBillingProfile{}).
		Where("id = ?", plan.BillingProfileID).
		Update("default_multiplier", defaultMultiplier).Error; err != nil {
		tx.Rollback()
		return model.AccessPlan{}, err
	}
	if err := tx.Where("billing_profile_id = ?", plan.BillingProfileID).Delete(&model.AccessBillingModelRule{}).Error; err != nil {
		tx.Rollback()
		return model.AccessPlan{}, err
	}

	newRules := make([]model.AccessBillingModelRule, 0, len(rules))
	seen := make(map[string]struct{}, len(rules))
	for _, rule := range rules {
		rule.BillingProfileID = plan.BillingProfileID
		normalizeAccessBillingModelRule(&rule)
		if rule.ModelName == "" {
			continue
		}
		key := strings.ToLower(rule.ModelName)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		newRules = append(newRules, rule)
	}
	if len(newRules) > 0 {
		if err := tx.Create(&newRules).Error; err != nil {
			tx.Rollback()
			return model.AccessPlan{}, err
		}
	}
	if err := tx.Commit().Error; err != nil {
		return model.AccessPlan{}, err
	}
	if err := accessPlanRefreshCache(ctx); err != nil {
		return model.AccessPlan{}, err
	}
	return accessPlanGetCached(accessPlanID, ctx)
}

func AccessPlanUpdateRouteTargets(accessPlanID int, targets []model.AccessRouteTarget, ctx context.Context) (model.AccessPlan, error) {
	plan, err := accessPlanLoadForWrite(accessPlanID, ctx)
	if err != nil {
		return model.AccessPlan{}, err
	}

	tx := db.GetDB().WithContext(ctx).Begin()
	var existingRules []model.AccessRouteRule
	if err := tx.Where("route_profile_id = ?", plan.RouteProfileID).Find(&existingRules).Error; err != nil {
		tx.Rollback()
		return model.AccessPlan{}, err
	}
	existingRuleByModel := make(map[string]model.AccessRouteRule, len(existingRules))
	for _, rule := range existingRules {
		existingRuleByModel[strings.ToLower(model.CleanOneMillionCapabilityModelName(rule.RequestModel))] = rule
	}

	var ruleIDs []int
	if err := tx.Model(&model.AccessRouteRule{}).Where("route_profile_id = ?", plan.RouteProfileID).Pluck("id", &ruleIDs).Error; err != nil {
		tx.Rollback()
		return model.AccessPlan{}, err
	}
	if len(ruleIDs) > 0 {
		if err := tx.Where("route_rule_id IN ?", ruleIDs).Delete(&model.AccessRouteTarget{}).Error; err != nil {
			tx.Rollback()
			return model.AccessPlan{}, err
		}
	}
	if err := tx.Where("route_profile_id = ?", plan.RouteProfileID).Delete(&model.AccessRouteRule{}).Error; err != nil {
		tx.Rollback()
		return model.AccessPlan{}, err
	}

	type routeBucket struct {
		rule    model.AccessRouteRule
		targets []model.AccessRouteTarget
	}
	buckets := make(map[string]*routeBucket)
	order := make([]string, 0)
	for _, target := range targets {
		target.AccessPlanID = accessPlanID
		normalizeAccessRouteTarget(&target)
		requestModel := strings.TrimSpace(target.RequestModel)
		if requestModel == "" || target.ChannelID <= 0 || target.UpstreamModel == "" {
			continue
		}
		key := strings.ToLower(requestModel)
		bucket, ok := buckets[key]
		if !ok {
			mode := target.Mode
			if mode == 0 {
				if existing, ok := existingRuleByModel[key]; ok {
					mode = existing.Mode
				}
			}
			if mode == 0 {
				mode = model.GroupModeFailover
			}
			rule := model.AccessRouteRule{
				RouteProfileID:       plan.RouteProfileID,
				RequestModel:         requestModel,
				Mode:                 mode,
				BillingModelSource:   target.BillingModelSource,
				BillingModelOverride: model.CleanOneMillionCapabilityModelName(target.BillingModelOverride),
				FallbackMode:         target.FallbackMode,
				SystemPromptOverride: strings.TrimSpace(target.SystemPromptOverride),
				PromptOverrideMode:   target.PromptOverrideMode,
			}
			normalizeAccessRouteRule(&rule)
			bucket = &routeBucket{rule: rule}
			buckets[key] = bucket
			order = append(order, key)
		} else if bucket.rule.Mode == 0 && target.Mode != 0 {
			bucket.rule.Mode = target.Mode
		}
		bucket.targets = append(bucket.targets, model.AccessRouteTarget{
			ChannelID:     target.ChannelID,
			UpstreamModel: target.UpstreamModel,
			Priority:      target.Priority,
			Weight:        target.Weight,
			Enabled:       target.Enabled,
		})
	}

	for _, key := range order {
		bucket := buckets[key]
		if err := tx.Create(&bucket.rule).Error; err != nil {
			tx.Rollback()
			return model.AccessPlan{}, err
		}
		for i := range bucket.targets {
			bucket.targets[i].RouteRuleID = bucket.rule.ID
		}
		if len(bucket.targets) > 0 {
			if err := tx.Create(&bucket.targets).Error; err != nil {
				tx.Rollback()
				return model.AccessPlan{}, err
			}
		}
	}
	if err := tx.Commit().Error; err != nil {
		return model.AccessPlan{}, err
	}
	if err := accessPlanRefreshCache(ctx); err != nil {
		return model.AccessPlan{}, err
	}
	return accessPlanGetCached(accessPlanID, ctx)
}

func APIKeyAccessPlanBindDefaultIfEmpty(apiKeyID int, ctx context.Context) error {
	if apiKeyID <= 0 {
		return fmt.Errorf("invalid API key id")
	}
	if err := ensureAPIKeyExists(apiKeyID, ctx); err != nil {
		return err
	}
	if err := ensureAccessPlanCache(ctx); err != nil {
		return err
	}
	if bindings, ok := apiKeyAccessPlanCache.Get(apiKeyID); ok && len(bindings) > 0 {
		return nil
	}
	apiKey, err := apiKeyForAccessPlanBinding(apiKeyID, ctx)
	if err != nil {
		return err
	}
	plan, err := accessPlanDefaultForUser(apiKey.UserID, ctx)
	if err != nil || plan == nil {
		return err
	}
	row := model.APIKeyAccessPlan{
		APIKeyID:     apiKeyID,
		AccessPlanID: plan.ID,
		IsDefault:    true,
	}
	if err := db.GetDB().WithContext(ctx).Create(&row).Error; err != nil {
		return err
	}
	return accessPlanRefreshCache(ctx)
}

func apiKeyForAccessPlanBinding(apiKeyID int, ctx context.Context) (model.APIKey, error) {
	if apiKey, ok := apiKeyCache.Get(apiKeyID); ok {
		return apiKey, nil
	}
	var apiKey model.APIKey
	if err := db.GetDB().WithContext(ctx).First(&apiKey, apiKeyID).Error; err != nil {
		return model.APIKey{}, fmt.Errorf("API key not found")
	}
	return apiKey, nil
}

func accessPlanDefaultForUser(userID int, ctx context.Context) (*model.AccessPlan, error) {
	if err := ensureAccessPlanCache(ctx); err != nil {
		return nil, err
	}
	if userID > 0 {
		bindings, ok := userAccessPlanCache.Get(userID)
		if ok {
			bindings = enabledUserBindings(bindings)
			if len(bindings) == 0 {
				return nil, fmt.Errorf("no access plan is available for this user")
			}
			sort.Slice(bindings, func(i, j int) bool {
				if bindings[i].IsDefault != bindings[j].IsDefault {
					return bindings[i].IsDefault
				}
				return bindings[i].AccessPlanID < bindings[j].AccessPlanID
			})
			plan, ok := accessPlanCache.Get(bindings[0].AccessPlanID)
			if ok && plan.Enabled {
				return copyAccessPlan(plan), nil
			}
		}
	}
	return accessPlanDefault(ctx)
}

func userAllowedAccessPlanIDSet(userID int, ctx context.Context) (map[int]struct{}, bool, error) {
	if err := ensureAccessPlanCache(ctx); err != nil {
		return nil, false, err
	}
	if userID <= 0 {
		return nil, false, nil
	}
	bindings, ok := userAccessPlanCache.Get(userID)
	if !ok {
		return nil, false, nil
	}
	allowed := make(map[int]struct{}, len(bindings))
	for _, binding := range enabledUserBindings(bindings) {
		allowed[binding.AccessPlanID] = struct{}{}
	}
	return allowed, true, nil
}

func trimAPIKeyBindingsForUser(userID int, ctx context.Context) error {
	if userID <= 0 {
		return nil
	}
	if err := ensureAccessPlanCache(ctx); err != nil {
		return err
	}
	allowed, restricted, err := userAllowedAccessPlanIDSet(userID, ctx)
	if err != nil || !restricted {
		return err
	}
	keys, err := APIKeyListAll(ctx)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if key.UserID != userID {
			continue
		}
		nextIDs := make([]int, 0, len(key.AccessPlanIDs))
		defaultID := key.DefaultAccessPlanID
		for _, id := range key.AccessPlanIDs {
			if _, ok := allowed[id]; ok {
				nextIDs = append(nextIDs, id)
			}
		}
		if len(nextIDs) == 0 {
			defaultPlan, err := accessPlanDefaultForUser(userID, ctx)
			if err != nil {
				return err
			}
			if defaultPlan == nil {
				return fmt.Errorf("no access plan is available for user %d", userID)
			}
			nextIDs = append(nextIDs, defaultPlan.ID)
			defaultID = defaultPlan.ID
		} else if _, ok := allowed[defaultID]; !ok {
			defaultID = nextIDs[0]
		}
		if err := APIKeyAccessPlanSet(key.ID, nextIDs, defaultID, ctx); err != nil {
			return err
		}
	}
	return nil
}

func ensureAPIKeyExists(apiKeyID int, ctx context.Context) error {
	if apiKeyID <= 0 {
		return fmt.Errorf("invalid API key id")
	}
	if _, ok := apiKeyCache.Get(apiKeyID); ok {
		return nil
	}
	var count int64
	if err := db.GetDB().WithContext(ctx).Model(&model.APIKey{}).Where("id = ?", apiKeyID).Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("API key not found")
	}
	return nil
}

func attachAPIKeyAccessPlans(apiKey *model.APIKey) {
	if apiKey == nil || apiKey.ID <= 0 {
		return
	}
	bindings, ok := apiKeyAccessPlanCache.Get(apiKey.ID)
	if !ok || len(bindings) == 0 {
		return
	}
	sort.Slice(bindings, func(i, j int) bool {
		if bindings[i].IsDefault != bindings[j].IsDefault {
			return bindings[i].IsDefault
		}
		return bindings[i].AccessPlanID < bindings[j].AccessPlanID
	})
	apiKey.AccessPlanIDs = make([]int, 0, len(bindings))
	apiKey.AccessPlans = make([]model.AccessPlan, 0, len(bindings))
	apiKey.DefaultAccessPlanID = 0
	for _, binding := range bindings {
		plan, ok := accessPlanCache.Get(binding.AccessPlanID)
		if !ok {
			continue
		}
		apiKey.AccessPlanIDs = append(apiKey.AccessPlanIDs, binding.AccessPlanID)
		apiKey.AccessPlans = append(apiKey.AccessPlans, plan)
		if binding.IsDefault {
			apiKey.DefaultAccessPlanID = binding.AccessPlanID
		}
	}
}

func attachUserAccessPlans(user *model.User) {
	if user == nil || user.ID <= 0 {
		return
	}
	bindings, ok := userAccessPlanCache.Get(user.ID)
	if !ok || len(bindings) == 0 {
		return
	}
	sort.Slice(bindings, func(i, j int) bool {
		if bindings[i].IsDefault != bindings[j].IsDefault {
			return bindings[i].IsDefault
		}
		return bindings[i].AccessPlanID < bindings[j].AccessPlanID
	})
	user.AccessPlanIDs = make([]int, 0, len(bindings))
	user.AccessPlans = make([]model.AccessPlan, 0, len(bindings))
	user.DefaultAccessPlanID = 0
	for _, binding := range bindings {
		plan, ok := accessPlanCache.Get(binding.AccessPlanID)
		if !ok {
			continue
		}
		user.AccessPlanIDs = append(user.AccessPlanIDs, binding.AccessPlanID)
		user.AccessPlans = append(user.AccessPlans, plan)
		if binding.IsDefault {
			user.DefaultAccessPlanID = binding.AccessPlanID
		}
	}
}

func accessPlanRefreshCache(ctx context.Context) error {
	if err := accessPlanEnsureSeedDefaults(ctx); err != nil {
		return err
	}
	plans := []model.AccessPlan{}
	if err := db.GetDB().WithContext(ctx).
		Preload("RouteProfile.Rules.Targets").
		Preload("BillingProfile.ModelRules").
		Find(&plans).Error; err != nil {
		return err
	}
	byID := make(map[int]model.AccessPlan, len(plans))
	bySlug := make(map[string]int, len(plans))
	for _, plan := range plans {
		flattenAccessPlan(&plan)
		byID[plan.ID] = plan
		bySlug[plan.Slug] = plan.ID
	}

	bindings := []model.APIKeyAccessPlan{}
	if err := db.GetDB().WithContext(ctx).Find(&bindings).Error; err != nil {
		return err
	}
	byAPIKeyID := make(map[int][]model.APIKeyAccessPlan)
	for _, binding := range bindings {
		byAPIKeyID[binding.APIKeyID] = append(byAPIKeyID[binding.APIKeyID], binding)
	}

	userBindings := []model.UserAccessPlan{}
	if err := db.GetDB().WithContext(ctx).Find(&userBindings).Error; err != nil {
		return err
	}
	byUserID := make(map[int][]model.UserAccessPlan)
	for _, binding := range userBindings {
		byUserID[binding.UserID] = append(byUserID[binding.UserID], binding)
	}

	accessPlanCache.ReplaceAll(byID)
	accessPlanSlugCache.ReplaceAll(bySlug)
	apiKeyAccessPlanCache.ReplaceAll(byAPIKeyID)
	userAccessPlanCache.ReplaceAll(byUserID)
	return nil
}

func accessPlanEnsureSeedDefaults(ctx context.Context) error {
	var existingCount int64
	if err := db.GetDB().WithContext(ctx).Model(&model.AccessPlan{}).Count(&existingCount).Error; err != nil {
		return err
	}
	if existingCount > 0 {
		return accessPlanEnsureDefault(ctx)
	}

	defaults := []struct {
		Slug        string
		DisplayName string
		Sort        int
		IsDefault   bool
	}{
		{Slug: "vip", DisplayName: "VIP", Sort: 10, IsDefault: true},
		{Slug: "svip", DisplayName: "SVIP", Sort: 20},
		{Slug: "ssvip", DisplayName: "SSVIP", Sort: 30},
	}

	for _, item := range defaults {
		routeProfile, err := accessRouteProfileGetOrCreate(item.Slug+" route profile", ctx)
		if err != nil {
			return err
		}
		billingProfile, err := accessBillingProfileGetOrCreate(item.Slug+" billing profile", ctx)
		if err != nil {
			return err
		}
		var count int64
		if err := db.GetDB().WithContext(ctx).Model(&model.AccessPlan{}).Where("slug = ?", item.Slug).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			continue
		}
		plan := model.AccessPlan{
			Slug:             item.Slug,
			DisplayName:      item.DisplayName,
			Sort:             item.Sort,
			Enabled:          true,
			IsDefault:        item.IsDefault,
			RouteProfileID:   routeProfile.ID,
			BillingProfileID: billingProfile.ID,
		}
		if err := db.GetDB().WithContext(ctx).Create(&plan).Error; err != nil {
			var raced model.AccessPlan
			if findErr := db.GetDB().WithContext(ctx).Where("slug = ?", item.Slug).First(&raced).Error; findErr == nil {
				continue
			}
			return err
		}
	}
	return accessPlanEnsureDefault(ctx)
}

func accessPlanGetCached(id int, ctx context.Context) (model.AccessPlan, error) {
	if err := ensureAccessPlanCache(ctx); err != nil {
		return model.AccessPlan{}, err
	}
	plan, ok := accessPlanCache.Get(id)
	if !ok {
		return model.AccessPlan{}, fmt.Errorf("access plan not found")
	}
	return plan, nil
}

func accessPlanLoadForWrite(id int, ctx context.Context) (model.AccessPlan, error) {
	if id <= 0 {
		return model.AccessPlan{}, fmt.Errorf("invalid access plan id")
	}
	var plan model.AccessPlan
	if err := db.GetDB().WithContext(ctx).First(&plan, id).Error; err != nil {
		return model.AccessPlan{}, err
	}
	if err := ensureAccessPlanProfiles(&plan, ctx); err != nil {
		return model.AccessPlan{}, err
	}
	if err := db.GetDB().WithContext(ctx).
		Model(&model.AccessPlan{}).
		Where("id = ?", plan.ID).
		Select("route_profile_id", "billing_profile_id").
		Updates(&model.AccessPlan{RouteProfileID: plan.RouteProfileID, BillingProfileID: plan.BillingProfileID}).Error; err != nil {
		return model.AccessPlan{}, err
	}
	return plan, nil
}

func ensureAccessPlanCache(ctx context.Context) error {
	if accessPlanCache.Len() > 0 {
		return nil
	}
	return accessPlanRefreshCache(ctx)
}

func accessPlanDefault(ctx context.Context) (*model.AccessPlan, error) {
	if err := ensureAccessPlanCache(ctx); err != nil {
		return nil, err
	}
	plans := make([]model.AccessPlan, 0, accessPlanCache.Len())
	for _, plan := range accessPlanCache.GetAll() {
		if plan.Enabled {
			plans = append(plans, plan)
		}
	}
	if len(plans) == 0 {
		return nil, nil
	}
	sortAccessPlans(plans)
	for _, plan := range plans {
		if plan.IsDefault {
			return copyAccessPlan(plan), nil
		}
	}
	return copyAccessPlan(plans[0]), nil
}

func accessPlanEnsureDefault(ctx context.Context) error {
	var count int64
	if err := db.GetDB().WithContext(ctx).Model(&model.AccessPlan{}).Where("enabled = ? AND is_default = ?", true, true).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	var plan model.AccessPlan
	if err := db.GetDB().WithContext(ctx).Where("enabled = ?", true).Order("sort ASC, id ASC").First(&plan).Error; err != nil {
		return nil
	}
	return db.GetDB().WithContext(ctx).Model(&model.AccessPlan{}).Where("id = ?", plan.ID).Update("is_default", true).Error
}

func enabledBindings(bindings []model.APIKeyAccessPlan) []model.APIKeyAccessPlan {
	if len(bindings) == 0 {
		return nil
	}
	result := make([]model.APIKeyAccessPlan, 0, len(bindings))
	for _, binding := range bindings {
		plan, ok := accessPlanCache.Get(binding.AccessPlanID)
		if !ok || !plan.Enabled {
			continue
		}
		result = append(result, binding)
	}
	return result
}

func enabledUserBindings(bindings []model.UserAccessPlan) []model.UserAccessPlan {
	if len(bindings) == 0 {
		return nil
	}
	result := make([]model.UserAccessPlan, 0, len(bindings))
	for _, binding := range bindings {
		plan, ok := accessPlanCache.Get(binding.AccessPlanID)
		if !ok || !plan.Enabled {
			continue
		}
		result = append(result, binding)
	}
	return result
}

func copyAccessPlan(plan model.AccessPlan) *model.AccessPlan {
	p := plan
	return &p
}

func flattenAccessPlan(plan *model.AccessPlan) {
	if plan == nil {
		return
	}
	plan.DefaultMultiplier = 1
	plan.BillingRules = nil
	plan.RouteTargets = nil
	if plan.BillingProfile != nil {
		if plan.BillingProfile.DefaultMultiplier > 0 {
			plan.DefaultMultiplier = plan.BillingProfile.DefaultMultiplier
		}
		plan.BillingRules = make([]model.AccessBillingModelRule, 0, len(plan.BillingProfile.ModelRules))
		for _, rule := range plan.BillingProfile.ModelRules {
			rule.AccessPlanID = plan.ID
			plan.BillingRules = append(plan.BillingRules, rule)
		}
	}
	if plan.RouteProfile != nil {
		for _, rule := range plan.RouteProfile.Rules {
			normalizeAccessRouteRule(&rule)
			for _, target := range rule.Targets {
				target.AccessPlanID = plan.ID
				target.RequestModel = rule.RequestModel
				target.Mode = rule.Mode
				target.BillingModelSource = rule.BillingModelSource
				target.BillingModelOverride = rule.BillingModelOverride
				target.FallbackMode = rule.FallbackMode
				target.SystemPromptOverride = rule.SystemPromptOverride
				target.PromptOverrideMode = rule.PromptOverrideMode
				normalizeAccessRouteTarget(&target)
				plan.RouteTargets = append(plan.RouteTargets, target)
			}
		}
	}
}

func sortAccessPlans(plans []model.AccessPlan) {
	sort.Slice(plans, func(i, j int) bool {
		if plans[i].Sort != plans[j].Sort {
			return plans[i].Sort < plans[j].Sort
		}
		return plans[i].ID < plans[j].ID
	})
}

func normalizeAccessPlan(plan *model.AccessPlan) error {
	if plan == nil {
		return fmt.Errorf("access plan is required")
	}
	plan.Slug = normalizeAccessPlanSlug(plan.Slug)
	if plan.Slug == "" {
		return fmt.Errorf("access plan slug is required")
	}
	plan.DisplayName = strings.TrimSpace(plan.DisplayName)
	if plan.DisplayName == "" {
		plan.DisplayName = plan.Slug
	}
	plan.SystemPromptOverride = strings.TrimSpace(plan.SystemPromptOverride)
	plan.PromptOverrideMode = normalizePromptOverrideMode(plan.PromptOverrideMode)
	return nil
}

func normalizeAccessPlanSlug(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func ensureAccessPlanProfiles(plan *model.AccessPlan, ctx context.Context) error {
	if plan.RouteProfileID == 0 {
		name := plan.DisplayName + " route profile"
		profile, err := accessRouteProfileGetOrCreate(name, ctx)
		if err != nil {
			return err
		}
		plan.RouteProfileID = profile.ID
	}
	if plan.BillingProfileID == 0 {
		name := plan.DisplayName + " billing profile"
		profile, err := accessBillingProfileGetOrCreate(name, ctx)
		if err != nil {
			return err
		}
		plan.BillingProfileID = profile.ID
	}
	return nil
}

func accessRouteProfileGetOrCreate(name string, ctx context.Context) (model.AccessRouteProfile, error) {
	var profile model.AccessRouteProfile
	err := db.GetDB().WithContext(ctx).Where("name = ?", name).First(&profile).Error
	if err == nil {
		return profile, nil
	}
	profile = model.AccessRouteProfile{Name: name}
	if err := db.GetDB().WithContext(ctx).Create(&profile).Error; err != nil {
		if findErr := db.GetDB().WithContext(ctx).Where("name = ?", name).First(&profile).Error; findErr == nil {
			return profile, nil
		}
		return model.AccessRouteProfile{}, err
	}
	return profile, nil
}

func accessBillingProfileGetOrCreate(name string, ctx context.Context) (model.AccessBillingProfile, error) {
	var profile model.AccessBillingProfile
	err := db.GetDB().WithContext(ctx).Where("name = ?", name).First(&profile).Error
	if err == nil {
		return profile, nil
	}
	profile = model.AccessBillingProfile{Name: name, DefaultMultiplier: 1}
	if err := db.GetDB().WithContext(ctx).Create(&profile).Error; err != nil {
		if findErr := db.GetDB().WithContext(ctx).Where("name = ?", name).First(&profile).Error; findErr == nil {
			return profile, nil
		}
		return model.AccessBillingProfile{}, err
	}
	return profile, nil
}

func normalizeAccessRouteRule(rule *model.AccessRouteRule) {
	if rule == nil {
		return
	}
	rule.RequestModel = model.CleanOneMillionCapabilityModelName(rule.RequestModel)
	rule.BillingModelOverride = model.CleanOneMillionCapabilityModelName(rule.BillingModelOverride)
	rule.SystemPromptOverride = strings.TrimSpace(rule.SystemPromptOverride)
	if rule.Mode == 0 {
		rule.Mode = model.GroupModeFailover
	}
	if rule.BillingModelSource == "" {
		rule.BillingModelSource = model.AccessBillingModelSourceRequest
	}
	if rule.FallbackMode == "" {
		rule.FallbackMode = model.AccessRouteFallbackGroup
	}
	rule.PromptOverrideMode = normalizePromptOverrideMode(rule.PromptOverrideMode)
}

func validateAccessRouteRule(rule *model.AccessRouteRule) error {
	if rule == nil {
		return fmt.Errorf("route rule is required")
	}
	if rule.RouteProfileID <= 0 {
		return fmt.Errorf("route profile id is required")
	}
	if rule.RequestModel == "" {
		return fmt.Errorf("request model is required")
	}
	switch rule.BillingModelSource {
	case model.AccessBillingModelSourceRequest, model.AccessBillingModelSourceUpstream, model.AccessBillingModelSourceOverride:
	default:
		return fmt.Errorf("invalid billing model source")
	}
	switch rule.FallbackMode {
	case model.AccessRouteFallbackGroup, model.AccessRouteFallbackReturnGroup, model.AccessRouteFallbackNone:
	default:
		return fmt.Errorf("invalid fallback mode")
	}
	if err := validatePromptOverrideMode(rule.PromptOverrideMode); err != nil {
		return err
	}
	return nil
}

func normalizeAccessRouteTarget(target *model.AccessRouteTarget) {
	if target == nil {
		return
	}
	target.RequestModel = model.CleanOneMillionCapabilityModelName(target.RequestModel)
	target.UpstreamModel = model.CleanOneMillionCapabilityModelName(target.UpstreamModel)
	target.BillingModelOverride = model.CleanOneMillionCapabilityModelName(target.BillingModelOverride)
	target.SystemPromptOverride = strings.TrimSpace(target.SystemPromptOverride)
	if target.Weight <= 0 {
		target.Weight = 1
	} else if target.Weight > 1000 {
		target.Weight = 1000
	}
	target.PromptOverrideMode = normalizePromptOverrideMode(target.PromptOverrideMode)
}

func validateAccessRouteTarget(target *model.AccessRouteTarget) error {
	if target == nil {
		return fmt.Errorf("route target is required")
	}
	if target.RouteRuleID <= 0 {
		return fmt.Errorf("route rule id is required")
	}
	if target.ChannelID <= 0 {
		return fmt.Errorf("channel id is required")
	}
	if target.UpstreamModel == "" {
		return fmt.Errorf("upstream model is required")
	}
	return nil
}

func normalizePromptOverrideMode(mode model.PromptOverrideMode) model.PromptOverrideMode {
	switch mode {
	case "", model.PromptOverrideModeAppendSystem:
		return model.PromptOverrideModeAppendSystem
	case model.PromptOverrideModeReplaceSystem:
		return model.PromptOverrideModeReplaceSystem
	default:
		return mode
	}
}

func validatePromptOverrideMode(mode model.PromptOverrideMode) error {
	switch mode {
	case "", model.PromptOverrideModeAppendSystem, model.PromptOverrideModeReplaceSystem:
		return nil
	default:
		return fmt.Errorf("invalid prompt override mode")
	}
}

func normalizeAccessBillingProfile(profile *model.AccessBillingProfile) {
	if profile == nil {
		return
	}
	profile.Name = strings.TrimSpace(profile.Name)
	if profile.DefaultMultiplier <= 0 {
		profile.DefaultMultiplier = 1
	}
}

func validateAccessBillingProfile(profile *model.AccessBillingProfile) error {
	if profile == nil {
		return fmt.Errorf("billing profile is required")
	}
	if profile.Name == "" {
		return fmt.Errorf("billing profile name is required")
	}
	return nil
}

func normalizeAccessBillingModelRule(rule *model.AccessBillingModelRule) {
	if rule == nil {
		return
	}
	rule.ModelName = model.CleanOneMillionCapabilityModelName(rule.ModelName)
	if rule.Multiplier <= 0 {
		rule.Multiplier = 1
	}
}

func validateAccessBillingModelRule(rule *model.AccessBillingModelRule) error {
	if rule == nil {
		return fmt.Errorf("billing model rule is required")
	}
	if rule.BillingProfileID <= 0 {
		return fmt.Errorf("billing profile id is required")
	}
	if rule.ModelName == "" {
		return fmt.Errorf("model name is required")
	}
	return nil
}
