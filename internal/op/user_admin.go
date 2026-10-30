package op

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
)

// 这一组是用户页（管理员）用到的写操作：部分更新、原子加减余额、余额流水、批量、单人用量。
//
// 共同的规矩：
//   - 改同一个用户的余额 / 月卡字段一律在 lockUser 里做，并且从锁内读到的最新缓存出发，
//     与 UserRecordUsage 的扣费串行，不会把扣费写回成扣费前的旧值。
//   - 管理员不扣费、有月卡先扣月卡这两条只在 UserCanRelay / UserRecordUsage 里，这里不碰。
//   - 最后一个启用的管理员不能被降级或停用（ensureAdminKeepsAccess）。

var errUserBalanceNotEnough = errors.New("balance is not enough to subtract")

const userBalanceReasonMaxLen = 255

const userBatchMaxIDs = 500

// userAdminAccessMu 把「还剩几个启用的管理员」的检查和随后的写库绑成一步。
// 没有它，两个并发的停用（比如批量停用两个管理员）都会先数到「另外还有 1 个」，
// 然后各自写库，最后一个管理员也没了。锁序：lockUser -> userAdminAccessMu，
// 持有 userAdminAccessMu 时绝不再去拿 lockUser。
var userAdminAccessMu sync.Mutex

func userActorName(actorID int) string {
	if actorID <= 0 {
		return ""
	}
	if actor, err := UserGet(actorID); err == nil {
		return actor.Username
	}
	return ""
}

func trimBalanceReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if r := []rune(reason); len(r) > userBalanceReasonMaxLen {
		reason = string(r[:userBalanceReasonMaxLen])
	}
	return reason
}

// UserPatch 只改请求里带了的字段，没带的保持原值。
func UserPatch(req model.UserPatchRequest, actorID int, ctx context.Context) (model.User, error) {
	if req.ID <= 0 {
		return model.User{}, fmt.Errorf("invalid user id")
	}
	mu := lockUser(req.ID)
	mu.Lock()
	user, oldUsername, err := userPatchLocked(req, actorID, ctx)
	mu.Unlock()
	if err != nil {
		return model.User{}, err
	}
	if oldUsername != user.Username {
		userNameIDMap.Del(normalizeUsername(oldUsername))
	}
	userNameIDMap.Set(normalizeUsername(user.Username), user.ID)

	if req.AccessPlanIDs != nil || (req.DefaultAccessPlanID != nil && *req.DefaultAccessPlanID > 0) {
		var planIDs []int
		if req.AccessPlanIDs != nil {
			planIDs = *req.AccessPlanIDs
		}
		defaultID := 0
		if req.DefaultAccessPlanID != nil {
			defaultID = *req.DefaultAccessPlanID
		}
		if err := UserAccessPlanSet(user.ID, planIDs, defaultID, ctx); err != nil {
			return model.User{}, err
		}
	}
	if latest, err := UserGet(user.ID); err == nil {
		user = latest
	}
	attachUserAccessPlans(&user)
	return user, nil
}

func userPatchLocked(req model.UserPatchRequest, actorID int, ctx context.Context) (model.User, string, error) {
	if req.Role != nil || req.Status != nil {
		userAdminAccessMu.Lock()
		defer userAdminAccessMu.Unlock()
	}
	user, err := UserGet(req.ID)
	if err != nil {
		return model.User{}, "", err
	}
	oldUsername := user.Username
	updates := map[string]any{}

	if req.Username != nil {
		username := strings.TrimSpace(*req.Username)
		if username == "" {
			return model.User{}, "", fmt.Errorf("username is required")
		}
		if username != user.Username {
			if otherID, ok := userNameIDMap.Get(normalizeUsername(username)); ok && otherID != user.ID {
				return model.User{}, "", fmt.Errorf("username already exists")
			}
		}
		user.Username = username
		updates["username"] = username
	}

	role, status := user.Role, user.Status
	if req.Role != nil {
		role = *req.Role
		if role == "" {
			role = model.UserRoleUser
		}
	}
	if req.Status != nil {
		status = *req.Status
		if status == "" {
			status = model.UserStatusActive
		}
	}
	if req.Role != nil || req.Status != nil {
		if err := validateUserRoleStatus(role, status); err != nil {
			return model.User{}, "", err
		}
		if err := ensureAdminKeepsAccess(user, role, status, actorID, ctx); err != nil {
			return model.User{}, "", err
		}
		user.Role, user.Status = role, status
		updates["role"] = role
		updates["status"] = status
	}

	if req.Note != nil {
		user.Note = *req.Note
		updates["note"] = user.Note
	}

	monthlyTouched := false
	if req.DailyQuota != nil || req.DailyLimit != nil || req.MonthlyLimit != nil {
		current := user.MonthlyLimit
		if req.MonthlyLimit != nil {
			current = *req.MonthlyLimit
		}
		user.MonthlyLimit = resolveMonthlyLimit(current, req.DailyLimit, req.DailyQuota)
		monthlyTouched = true
	}
	if req.DailyUsed != nil || req.MonthlyUsed != nil {
		current := user.MonthlyUsed
		if req.MonthlyUsed != nil {
			current = *req.MonthlyUsed
		}
		user.MonthlyUsed = resolveMonthlyUsed(current, req.DailyUsed)
		updates["monthly_used"] = user.MonthlyUsed
	}
	if expireAt, ok, err := patchUnix(req.MonthlyExpireAt, req.MonthlyExpireAtISO); err != nil {
		return model.User{}, "", err
	} else if ok {
		user.MonthlyExpireAt = expireAt
		monthlyTouched = true
	}
	if resetAt, ok, err := patchUnix(req.MonthlyResetAt, req.NextResetAtISO, req.MonthlyResetAtISO); err != nil {
		return model.User{}, "", err
	} else if ok {
		user.MonthlyResetAt = resetAt
		monthlyTouched = true
	}
	if monthlyTouched {
		user.MonthlyResetAt = ensureMonthlyResetAt(user.MonthlyLimit, user.MonthlyExpireAt, user.MonthlyResetAt, time.Now())
		updates["monthly_limit"] = user.MonthlyLimit
		updates["monthly_expire_at"] = user.MonthlyExpireAt
		updates["monthly_reset_at"] = user.MonthlyResetAt
	}

	var balanceLog *model.UserBalanceLog
	if req.Balance != nil {
		next := maxFloat(*req.Balance, 0)
		if next != user.Balance {
			reason := "保存资料时直接改"
			if req.BalanceReason != nil && strings.TrimSpace(*req.BalanceReason) != "" {
				reason = trimBalanceReason(*req.BalanceReason)
			}
			balanceLog = &model.UserBalanceLog{
				UserID:        user.ID,
				ActorID:       actorID,
				ActorName:     userActorName(actorID),
				Mode:          model.UserBalanceModeSet,
				Delta:         roundBalance(next - user.Balance),
				BalanceBefore: user.Balance,
				BalanceAfter:  next,
				Reason:        reason,
			}
			user.Balance = next
			updates["balance"] = next
		}
	}

	if len(updates) > 0 {
		err := db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := tx.Model(&model.User{}).Where("id = ?", user.ID).Updates(updates).Error; err != nil {
				return fmt.Errorf("failed to update user: %w", err)
			}
			if balanceLog != nil {
				if err := tx.Create(balanceLog).Error; err != nil {
					return fmt.Errorf("failed to record balance change: %w", err)
				}
			}
			return nil
		})
		if err != nil {
			return model.User{}, "", err
		}
		userCache.Set(user.ID, user)
	}
	return user, oldUsername, nil
}

// patchUnix 解析一个可选的时间字段：任一 ISO 别名非空就用它，否则用原始秒数；都没带返回 ok=false。
func patchUnix(raw *int64, aliases ...*string) (int64, bool, error) {
	for _, alias := range aliases {
		if alias == nil || strings.TrimSpace(*alias) == "" {
			continue
		}
		parsed, err := parseUnixSecondsAlias(strings.TrimSpace(*alias))
		if err != nil {
			return 0, false, err
		}
		return maxInt64(parsed, 0), true, nil
	}
	if raw != nil {
		return maxInt64(*raw, 0), true, nil
	}
	return 0, false, nil
}

// UserBalanceAdjust 加 / 减 / 直接改成余额，并记一条流水。
//
// 加和减是一条 `UPDATE users SET balance = balance + ? WHERE id = ? [AND balance + ? >= 0]`：
// 数据库在一条语句里读写，哪怕同一时刻有请求在扣费，也不会把扣费盖掉；减到负数直接报错、
// 一分不动。锁内执行是为了让进程内的缓存（扣费判断读的就是它）和库里的值保持一致。
func UserBalanceAdjust(req model.UserBalanceAdjustRequest, actorID int, ctx context.Context) (model.User, model.UserBalanceLog, error) {
	if req.ID <= 0 {
		return model.User{}, model.UserBalanceLog{}, fmt.Errorf("invalid user id")
	}
	amount := req.Amount
	if math.IsNaN(amount) || math.IsInf(amount, 0) {
		return model.User{}, model.UserBalanceLog{}, fmt.Errorf("invalid amount")
	}
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	switch mode {
	case model.UserBalanceModeAdd, model.UserBalanceModeSubtract:
		if amount <= 0 {
			return model.User{}, model.UserBalanceLog{}, fmt.Errorf("amount must be positive")
		}
	case model.UserBalanceModeSet:
		if amount < 0 {
			return model.User{}, model.UserBalanceLog{}, fmt.Errorf("balance cannot be negative")
		}
	default:
		return model.User{}, model.UserBalanceLog{}, fmt.Errorf("invalid mode, expected add, subtract or set")
	}
	amount = roundBalance(amount)

	mu := lockUser(req.ID)
	mu.Lock()
	defer mu.Unlock()
	user, err := UserGet(req.ID)
	if err != nil {
		return model.User{}, model.UserBalanceLog{}, err
	}

	entry := model.UserBalanceLog{
		UserID:    user.ID,
		ActorID:   actorID,
		ActorName: userActorName(actorID),
		Mode:      mode,
		Reason:    trimBalanceReason(req.Reason),
	}
	err = db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var before float64
		if err := tx.Model(&model.User{}).Where("id = ?", user.ID).Pluck("balance", &before).Error; err != nil {
			return err
		}
		switch mode {
		case model.UserBalanceModeSet:
			if err := tx.Model(&model.User{}).Where("id = ?", user.ID).Update("balance", amount).Error; err != nil {
				return fmt.Errorf("failed to set balance: %w", err)
			}
		default:
			delta := amount
			query := tx.Model(&model.User{}).Where("id = ?", user.ID)
			if mode == model.UserBalanceModeSubtract {
				delta = -amount
				query = query.Where("balance + ? >= 0", delta)
			}
			res := query.Update("balance", gorm.Expr("balance + ?", delta))
			if res.Error != nil {
				return fmt.Errorf("failed to adjust balance: %w", res.Error)
			}
			if res.RowsAffected == 0 {
				return errUserBalanceNotEnough
			}
		}
		var after float64
		if err := tx.Model(&model.User{}).Where("id = ?", user.ID).Pluck("balance", &after).Error; err != nil {
			return err
		}
		entry.BalanceBefore = before
		entry.BalanceAfter = after
		entry.Delta = roundBalance(after - before)
		return tx.Create(&entry).Error
	})
	if err != nil {
		return model.User{}, model.UserBalanceLog{}, err
	}
	user.Balance = entry.BalanceAfter
	userCache.Set(user.ID, user)
	attachUserAccessPlans(&user)
	return user, entry, nil
}

// UserBalanceLogList 返回某个用户最近的余额流水（新的在前）。
func UserBalanceLogList(userID int, limit int, ctx context.Context) ([]model.UserBalanceLog, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("invalid user id")
	}
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	logs := make([]model.UserBalanceLog, 0)
	err := db.GetDB().WithContext(ctx).
		Where("user_id = ?", userID).
		Order("id DESC").
		Limit(limit).
		Find(&logs).Error
	return logs, err
}

// UserBatch 对一批用户做同一件事，逐个执行、逐个报结果。
// 逐个走单人的同一条路径，所以单人的所有规矩（最后一个管理员、不能停用自己、有 Key 不能删）照样生效。
func UserBatch(req model.UserBatchRequest, actorID int, ctx context.Context) (model.UserBatchResult, error) {
	ids := make([]int, 0, len(req.IDs))
	seen := make(map[int]struct{}, len(req.IDs))
	for _, id := range req.IDs {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return model.UserBatchResult{}, fmt.Errorf("no user selected")
	}
	if len(ids) > userBatchMaxIDs {
		return model.UserBatchResult{}, fmt.Errorf("too many users in one batch (max %d)", userBatchMaxIDs)
	}

	var apply func(id int) error
	switch req.Action {
	case model.UserBatchEnable, model.UserBatchDisable:
		status := model.UserStatusActive
		if req.Action == model.UserBatchDisable {
			status = model.UserStatusDisabled
		}
		apply = func(id int) error {
			_, err := UserPatch(model.UserPatchRequest{ID: id, Status: &status}, actorID, ctx)
			return err
		}
	case model.UserBatchAddBalance:
		if !(req.Amount > 0) || math.IsInf(req.Amount, 0) {
			return model.UserBatchResult{}, fmt.Errorf("amount must be positive")
		}
		apply = func(id int) error {
			_, _, err := UserBalanceAdjust(model.UserBalanceAdjustRequest{
				ID:     id,
				Mode:   model.UserBalanceModeAdd,
				Amount: req.Amount,
				Reason: req.Reason,
			}, actorID, ctx)
			return err
		}
	case model.UserBatchSetPlans:
		if len(req.AccessPlanIDs) == 0 {
			return model.UserBatchResult{}, fmt.Errorf("access_plan_ids is required")
		}
		apply = func(id int) error {
			return UserAccessPlanSet(id, req.AccessPlanIDs, req.DefaultAccessPlanID, ctx)
		}
	case model.UserBatchDelete:
		apply = func(id int) error {
			return UserDelete(id, actorID, ctx)
		}
	default:
		return model.UserBatchResult{}, fmt.Errorf("invalid action")
	}

	result := model.UserBatchResult{Done: make([]int, 0, len(ids)), Failed: make([]model.UserBatchFailure, 0)}
	for _, id := range ids {
		if err := apply(id); err != nil {
			result.Failed = append(result.Failed, model.UserBatchFailure{ID: id, Error: err.Error()})
			continue
		}
		result.Done = append(result.Done, id)
	}
	return result, nil
}

const (
	UserUsageRangeToday = "today"
	UserUsageRange7d    = "7d"
	UserUsageRange30d   = "30d"
	UserUsageRangeAll   = "all"

	// 「全部」时每天花费图只画最近 30 天（总数仍是全部）。
	userUsageAllChartDays = 30
	userUsageTopModels    = 5
)

func userUsageWindow(rangeKey string, now time.Time) (start int64, chartDays int, err error) {
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	switch rangeKey {
	case UserUsageRangeToday:
		return midnight.Unix(), 1, nil
	case UserUsageRange7d, "":
		return midnight.AddDate(0, 0, -6).Unix(), 7, nil
	case UserUsageRange30d:
		return midnight.AddDate(0, 0, -29).Unix(), 30, nil
	case UserUsageRangeAll:
		return 0, userUsageAllChartDays, nil
	default:
		return 0, 0, fmt.Errorf("invalid range, expected today, 7d, 30d or all")
	}
}

// UserUsage 算一个用户在一段时间里的用量：总数、每天花费、常用模型。
// 与 computeModelRequestRank 同一个口径：库里的日志 + 还没落库的内存日志，
// 同样跳过 relayLogExcludedFromModelTelemetry 排除的行，花费用 modelRankLogCost。
func UserUsage(userID int, rangeKey string, ctx context.Context) (model.UserUsageResponse, error) {
	return userUsageAt(userID, rangeKey, time.Now(), ctx)
}

func userUsageAt(userID int, rangeKey string, now time.Time, ctx context.Context) (model.UserUsageResponse, error) {
	if userID <= 0 {
		return model.UserUsageResponse{}, fmt.Errorf("invalid user id")
	}
	if _, err := UserGet(userID); err != nil {
		return model.UserUsageResponse{}, err
	}
	start, chartDays, err := userUsageWindow(rangeKey, now)
	if err != nil {
		return model.UserUsageResponse{}, err
	}
	if rangeKey == "" {
		rangeKey = UserUsageRange7d
	}

	var dbLogs []model.RelayLog
	query := db.GetDB().WithContext(ctx).
		Model(&model.RelayLog{}).
		Select(
			"id", "time", "user_id",
			"request_model_name", "actual_model_name", "billing_model",
			"input_tokens", "output_tokens",
			"cost", "final_input_cost", "final_output_cost", "final_cache_read_cost", "final_cache_write_cost",
			"error", "error_code", "error_strategy",
		).
		Where("user_id = ?", userID)
	if start > 0 {
		query = query.Where("time >= ?", start)
	}
	if err := query.Find(&dbLogs).Error; err != nil {
		return model.UserUsageResponse{}, err
	}

	seen := make(map[int64]struct{}, len(dbLogs))
	logs := make([]model.RelayLog, 0, len(dbLogs))
	for _, l := range dbLogs {
		seen[l.ID] = struct{}{}
		logs = append(logs, l)
	}
	relayLogCacheLock.Lock()
	for _, l := range relayLogCache {
		if l.UserID != userID || l.Time < start {
			continue
		}
		if _, ok := seen[l.ID]; ok {
			continue
		}
		logs = append(logs, l)
	}
	relayLogCacheLock.Unlock()

	loc := now.Location()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	days := make([]model.UserUsageDay, chartDays)
	dayIndex := make(map[string]int, chartDays)
	for i := 0; i < chartDays; i++ {
		d := today.AddDate(0, 0, i-(chartDays-1)).Format("2006-01-02")
		days[i] = model.UserUsageDay{Date: d}
		dayIndex[d] = i
	}

	out := model.UserUsageResponse{UserID: userID, Range: rangeKey, Start: start, End: now.Unix()}
	models := make(map[string]*model.UserUsageModel)
	for _, l := range logs {
		if relayLogExcludedFromModelTelemetry(l) {
			continue
		}
		cost := modelRankLogCost(l)
		if strings.TrimSpace(l.Error) == "" {
			out.RequestSuccess++
		} else {
			out.RequestFailed++
		}
		out.InputTokens += int64(l.InputTokens)
		out.OutputTokens += int64(l.OutputTokens)
		out.TotalCost += cost

		if i, ok := dayIndex[time.Unix(l.Time, 0).In(loc).Format("2006-01-02")]; ok {
			days[i].Requests++
			days[i].Cost += cost
		}
		name := modelRankRequestedModelName(l)
		m := models[name]
		if m == nil {
			m = &model.UserUsageModel{Model: name}
			models[name] = m
		}
		m.Requests++
		m.Tokens += int64(l.InputTokens) + int64(l.OutputTokens)
		m.Cost += cost
	}
	out.Daily = days
	out.TopModels = make([]model.UserUsageModel, 0, len(models))
	for _, m := range models {
		out.TopModels = append(out.TopModels, *m)
	}
	sort.Slice(out.TopModels, func(i, j int) bool {
		a, b := out.TopModels[i], out.TopModels[j]
		if a.Cost != b.Cost {
			return a.Cost > b.Cost
		}
		if a.Requests != b.Requests {
			return a.Requests > b.Requests
		}
		return a.Model < b.Model
	})
	if len(out.TopModels) > userUsageTopModels {
		out.TopModels = out.TopModels[:userUsageTopModels]
	}
	return out, nil
}
