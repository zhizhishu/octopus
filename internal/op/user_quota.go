package op

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var userQuotaNow = time.Now

var (
	ErrUserQuotaExceeded = errors.New("user period quota has been used up")
	ErrUserQuotaModel    = errors.New("model not supported by user quota policy")
	ErrUserQuotaRealtime = errors.New("realtime is unavailable while a user quota policy is enabled")
	ErrUserQuotaConflict = errors.New("quota policy changed; reload before saving")
)

// UserQuotaAdmission has no client-controlled receipt identifier. A relay
// generates one server ID; durable jobs use their existing stable job ID.
type UserQuotaAdmission struct {
	SourceKey    string
	SourceKind   string
	UserID       int
	RequestModel string
	Units        int64
	InitialCost  float64
	// KnownCost uses the full initial charge for admission. Unknown-cost relays
	// reserve a small amount and always settle their actual existing bill.
	KnownCost bool
	// Batch holds must still fit the legacy balance under the same user lock.
	RequireBalance bool
}

func UserQuotaPolicyGet(ctx context.Context, userID int) (*model.UserQuotaPolicy, error) {
	if userID <= 0 || db.GetDB() == nil {
		return nil, nil
	}
	var policy model.UserQuotaPolicy
	err := db.GetDB().WithContext(ctx).First(&policy, "user_id = ?", userID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &policy, nil
}

func UserQuotaEnabled(ctx context.Context, userID int) (bool, error) {
	p, err := UserQuotaPolicyGet(ctx, userID)
	if err != nil || p == nil || !p.Enabled {
		return false, err
	}
	user, err := UserGet(userID)
	if err != nil {
		return false, err
	}
	return !user.IsAdmin(), nil
}

func UserQuotaPolicySave(ctx context.Context, wanted model.UserQuotaPolicy) (*model.UserQuotaPolicy, error) {
	if wanted.UserID <= 0 {
		return nil, fmt.Errorf("invalid user id")
	}
	for _, amount := range []float64{wanted.FiveHourCost, wanted.WeeklyCost, wanted.MonthlyCost} {
		if math.IsNaN(amount) || math.IsInf(amount, 0) || amount < 0 || amount > 1e12 {
			return nil, fmt.Errorf("cost limits must be finite non-negative amounts")
		}
	}
	for _, count := range []int64{wanted.FiveHourRequests, wanted.WeeklyRequests, wanted.MonthlyRequests} {
		if count < 0 || count > 1e12 {
			return nil, fmt.Errorf("request limits must be non-negative integers")
		}
	}
	if len(wanted.AllowedModels) > 16384 {
		return nil, fmt.Errorf("model list is too long")
	}
	wanted.AllowedModels = strings.Join(SupportedModelsList(wanted.AllowedModels), ",")
	mu := lockUser(wanted.UserID)
	mu.Lock()
	defer mu.Unlock()
	user, err := UserGet(wanted.UserID)
	if err != nil {
		return nil, err
	}
	if user.IsAdmin() && wanted.Enabled {
		return nil, fmt.Errorf("quota policies are for ordinary users")
	}
	now := userQuotaNow()
	var saved model.UserQuotaPolicy
	err = db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var old model.UserQuotaPolicy
		found := tx.First(&old, "user_id = ?", wanted.UserID).Error
		if found != nil && !errors.Is(found, gorm.ErrRecordNotFound) {
			return found
		}
		if found == nil {
			// A real write acquires the database writer/row lock on SQLite as
			// well as MySQL/PostgreSQL. In-process lock order remains lockUser.
			if err := userQuotaLockPolicy(tx, wanted.UserID); err != nil {
				return err
			}
			if err := tx.First(&old, "user_id = ?", wanted.UserID).Error; err != nil {
				return err
			}
			if wanted.Revision != old.Revision {
				return ErrUserQuotaConflict
			}
			if wanted.Timezone == "" {
				wanted.Timezone = old.Timezone
			}
			if old.AnchorAt == 0 {
				// Before first enablement the client cannot choose the anchor.
				wanted.AnchorAt = 0
			} else if wanted.AnchorAt == 0 {
				wanted.AnchorAt = old.AnchorAt
			}
			var used int64
			if err := tx.Model(&model.UserQuotaWindow{}).Where("user_id = ? AND accepted > 0", wanted.UserID).Count(&used).Error; err != nil {
				return err
			}
			if used > 0 && (wanted.Timezone != old.Timezone || wanted.AnchorAt != old.AnchorAt) {
				return fmt.Errorf("time zone and anchor cannot change after usage")
			}
			wanted.CreatedAt = old.CreatedAt
			wanted.Serial = old.Serial
		} else {
			if wanted.Revision != 0 {
				return ErrUserQuotaConflict
			}
			if wanted.Timezone == "" {
				name, err := userQuotaDefaultTimezone()
				if err != nil {
					return err
				}
				wanted.Timezone = name
			}
			wanted.CreatedAt = now.Unix()
			wanted.AnchorAt = 0
		}
		if _, err := userQuotaLocation(wanted.Timezone); err != nil {
			return fmt.Errorf("invalid policy time zone: %w", err)
		}
		if wanted.AnchorAt < 0 || wanted.AnchorAt > now.Unix() {
			return fmt.Errorf("quota anchor must be a non-negative time not in the future")
		}
		if wanted.Enabled && wanted.AnchorAt == 0 {
			wanted.AnchorAt = now.Unix()
		}
		wanted.Revision = old.Revision + 1
		wanted.UpdatedAt = now.Unix()
		if err := tx.Save(&wanted).Error; err != nil {
			return err
		}
		saved = wanted
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &saved, nil
}

func userQuotaLockPolicy(tx *gorm.DB, userID int) error {
	result := tx.Model(&model.UserQuotaPolicy{}).Where("user_id = ?", userID).
		UpdateColumn("serial", gorm.Expr("serial + 1"))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("quota policy is unavailable")
	}
	return nil
}

// UserQuotaAdmit returns nil when no policy is enabled. In that case persist is
// not called and the caller executes its unchanged legacy path. For configured
// jobs, creation, precharge, counters and receipt commit together.
func UserQuotaAdmit(ctx context.Context, input UserQuotaAdmission, persist func(*gorm.DB) error) (*model.UserQuotaReceipt, error) {
	if input.UserID <= 0 {
		return nil, nil
	}
	policy, err := UserQuotaPolicyGet(ctx, input.UserID)
	if err != nil || policy == nil || !policy.Enabled {
		return nil, err
	}
	if input.SourceKey == "" || len(input.SourceKey) > 128 || input.Units <= 0 || !userQuotaValidCost(input.InitialCost) {
		return nil, fmt.Errorf("invalid quota admission")
	}
	mu := lockUser(input.UserID)
	mu.Lock()
	defer mu.Unlock()
	user, err := UserGet(input.UserID)
	if err != nil {
		return nil, err
	}
	if user.IsAdmin() {
		return nil, nil
	}
	var receipt *model.UserQuotaReceipt
	var updatedUser *model.User
	err = db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := userQuotaLockPolicy(tx, input.UserID); err != nil {
			return err
		}
		var p model.UserQuotaPolicy
		if err := tx.First(&p, "user_id = ?", input.UserID).Error; err != nil {
			return err
		}
		if !p.Enabled {
			return nil
		}
		if !IsModelSupported(p.AllowedModels, input.RequestModel) {
			return ErrUserQuotaModel
		}
		var existing model.UserQuotaReceipt
		lookup := tx.First(&existing, "source_key = ?", input.SourceKey).Error
		if lookup == nil {
			if existing.UserID != input.UserID || existing.SourceKind != input.SourceKind ||
				existing.RequestModel != input.RequestModel || existing.Units != input.Units {
				return fmt.Errorf("quota receipt identity conflict")
			}
			receipt = &existing
			return nil
		}
		if !errors.Is(lookup, gorm.ErrRecordNotFound) {
			return lookup
		}
		now := userQuotaNow()
		windows, err := userQuotaWindows(p, now)
		if err != nil {
			return err
		}
		reserve := 0.0
		if !input.KnownCost {
			reserve = userQuotaReserveDefault
		}
		for i := range windows {
			w := &windows[i]
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(w).Error; err != nil {
				return err
			}
			if err := tx.First(w, "user_id = ? AND period = ? AND start_at = ?", input.UserID, w.Period, w.StartAt).Error; err != nil {
				return err
			}
			cap, count := userQuotaLimits(p, w.Period)
			if count > 0 && input.Units > count-w.Accepted {
				return fmt.Errorf("%w: %s requests", ErrUserQuotaExceeded, w.Period)
			}
			if cap > 0 {
				available := cap - w.CostUsed - w.CostReserved
				if available <= userQuotaEpsilon || input.InitialCost > available+userQuotaEpsilon {
					return fmt.Errorf("%w: %s cost", ErrUserQuotaExceeded, w.Period)
				}
				if !input.KnownCost && reserve > available {
					reserve = available
				}
			}
		}
		if input.RequireBalance && input.InitialCost > userQuotaAvailable(user, now)+userQuotaEpsilon {
			return fmt.Errorf("%w: insufficient balance for precharge", ErrUserQuotaExceeded)
		}
		// The old daily-card/balance algorithm remains authoritative. Do not
		// call its public locking wrapper while already holding lockUser.
		if input.InitialCost > 0 {
			next, err := userQuotaApplyBillTx(tx, user, input.InitialCost, now)
			if err != nil {
				return err
			}
			updatedUser = &next
		}
		r := model.UserQuotaReceipt{
			SourceKey: input.SourceKey, SourceKind: input.SourceKind, UserID: input.UserID,
			RequestModel: input.RequestModel, PolicyRevision: p.Revision,
			AcceptedAt: now.Unix(), Units: input.Units, Cost: input.InitialCost,
			Reserved: reserve, Outcome: "accepted",
			FiveHourStart: windows[0].StartAt, WeekStart: windows[1].StartAt, MonthStart: windows[2].StartAt,
		}
		if persist != nil {
			if err := persist(tx); err != nil {
				return err
			}
		}
		if err := tx.Create(&r).Error; err != nil {
			return err
		}
		for _, w := range windows {
			if err := tx.Model(&model.UserQuotaWindow{}).
				Where("user_id = ? AND period = ? AND start_at = ?", w.UserID, w.Period, w.StartAt).
				Updates(map[string]any{
					"cost_used":     w.CostUsed + r.Cost,
					"cost_reserved": w.CostReserved + r.Reserved,
					"accepted":      w.Accepted + r.Units,
				}).Error; err != nil {
				return err
			}
		}
		receipt = &r
		return nil
	})
	if err != nil {
		return nil, err
	}
	if updatedUser != nil {
		userCache.Set(updatedUser.ID, *updatedUser)
	}
	return receipt, nil
}

func userQuotaValidCost(cost float64) bool {
	return !math.IsNaN(cost) && !math.IsInf(cost, 0) && cost >= 0
}

// UserQuotaSettle applies a cumulative target, never a replayable delta.
// handled=false means this request/job predates or never had a quota policy.
// A job terminal update may share this transaction via persist.
func UserQuotaSettle(ctx context.Context, sourceKey string, userID int, targetCost float64, failedUnits int64, outcome string, persist func(*gorm.DB) error) (handled bool, err error) {
	if sourceKey == "" || userID <= 0 || db.GetDB() == nil {
		return false, nil
	}
	if !userQuotaValidCost(targetCost) || failedUnits < 0 || (outcome != "completed" && outcome != "failed" && outcome != "cancelled") {
		return false, fmt.Errorf("invalid quota settlement")
	}
	var first model.UserQuotaReceipt
	lookup := db.GetDB().WithContext(ctx).First(&first, "source_key = ? AND user_id = ?", sourceKey, userID).Error
	if errors.Is(lookup, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if lookup != nil {
		return false, lookup
	}
	mu := lockUser(userID)
	mu.Lock()
	defer mu.Unlock()
	user, err := UserGet(userID)
	if err != nil {
		return true, err
	}
	var updatedUser *model.User
	err = db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := userQuotaLockPolicy(tx, userID); err != nil {
			return err
		}
		var receipt model.UserQuotaReceipt
		if err := tx.First(&receipt, "source_key = ? AND user_id = ?", sourceKey, userID).Error; err != nil {
			return err
		}
		if receipt.SettledAt > 0 {
			if math.Abs(receipt.Cost-targetCost) > userQuotaEpsilon || receipt.FailedUnits != failedUnits || receipt.Outcome != outcome {
				return fmt.Errorf("quota receipt already settled with a different result")
			}
			return nil
		}
		if failedUnits > receipt.Units {
			return fmt.Errorf("failed unit count exceeds accepted units")
		}
		if persist != nil {
			if err := persist(tx); err != nil {
				return err
			}
		}
		delta := targetCost - receipt.Cost
		if delta != 0 {
			next, err := userQuotaApplyBillTx(tx, user, delta, userQuotaNow())
			if err != nil {
				return err
			}
			updatedUser = &next
		}
		for period, start := range userQuotaReceiptStarts(receipt) {
			var w model.UserQuotaWindow
			if err := tx.First(&w, "user_id = ? AND period = ? AND start_at = ?", userID, period, start).Error; err != nil {
				return err
			}
			if err := tx.Model(&model.UserQuotaWindow{}).Where("user_id = ? AND period = ? AND start_at = ?", userID, period, start).
				Updates(map[string]any{
					"cost_used":       maxFloat(w.CostUsed+delta, 0),
					"cost_reserved":   maxFloat(w.CostReserved-receipt.Reserved, 0),
					"failed_accepted": w.FailedAccepted + failedUnits,
				}).Error; err != nil {
				return err
			}
		}
		return tx.Model(&model.UserQuotaReceipt{}).Where("source_key = ?", sourceKey).Updates(map[string]any{
			"cost": targetCost, "reserved": 0, "failed_units": failedUnits,
			"outcome": outcome, "settled_at": userQuotaNow().Unix(),
		}).Error
	})
	if err == nil && updatedUser != nil {
		userCache.Set(updatedUser.ID, *updatedUser)
	}
	return true, err
}

// userQuotaApplyBillTx uses exactly the existing UserRecordUsage/RefundUsage
// arithmetic, including the current daily card window and balance clamping.
// Only the transaction boundary is different; original-window attribution is
// solely for the independent quota counters.
func userQuotaApplyBillTx(tx *gorm.DB, user model.User, delta float64, now time.Time) (model.User, error) {
	if delta == 0 || user.IsAdmin() {
		return user, nil
	}
	updates := map[string]any{}
	if userMonthlyActive(user) {
		nextResetAt := nextMonthlyReset(now).Unix()
		if !(user.MonthlyResetAt > now.Unix() && user.MonthlyResetAt <= nextResetAt) {
			user.MonthlyUsed = 0
			user.MonthlyResetAt = nextResetAt
			updates["monthly_reset_at"] = nextResetAt
		}
		user.MonthlyUsed = maxFloat(user.MonthlyUsed+delta, 0)
		updates["monthly_used"] = user.MonthlyUsed
	} else {
		if delta > 0 {
			user.Balance = maxFloat(user.Balance-delta, 0)
		} else {
			user.Balance = roundBalance(user.Balance - delta)
		}
		updates["balance"] = user.Balance
	}
	if err := tx.Model(&model.User{}).Where("id = ?", user.ID).Updates(updates).Error; err != nil {
		return user, err
	}
	return user, nil
}

func UserQuotaStatusGet(ctx context.Context, userID int) (model.UserQuotaStatus, error) {
	status := model.UserQuotaStatus{
		Windows:           []model.UserQuotaWindowStatus{},
		RealtimeSupported: false,
		CountSemantics:    "accepted_units_including_later_failures",
		CostSemantics:     "settled_plus_reserved_actual_inflight_cost_may_exceed_limit",
	}
	mu := lockUser(userID)
	mu.Lock()
	defer mu.Unlock()
	p, err := UserQuotaPolicyGet(ctx, userID)
	if err != nil {
		return status, err
	}
	status.Policy = p
	if p == nil {
		status.RealtimeSupported = true
		return status, nil
	}
	status.RealtimeSupported = !p.Enabled
	if user, err := UserGet(userID); err == nil && user.IsAdmin() {
		status.RealtimeSupported = true
	}
	windows, err := userQuotaWindows(*p, userQuotaNow())
	if err != nil {
		return status, err
	}
	for _, w := range windows {
		var stored model.UserQuotaWindow
		query := db.GetDB().WithContext(ctx).First(&stored, "user_id = ? AND period = ? AND start_at = ?", userID, w.Period, w.StartAt).Error
		if query != nil && !errors.Is(query, gorm.ErrRecordNotFound) {
			return status, query
		}
		if query == nil {
			w = stored
		}
		cap, count := userQuotaLimits(*p, w.Period)
		status.Windows = append(status.Windows, model.UserQuotaWindowStatus{UserQuotaWindow: w, CostLimit: cap, RequestLimit: count})
	}
	err = db.GetDB().WithContext(ctx).Model(&model.UserQuotaReceipt{}).Where("user_id = ? AND settled_at = 0", userID).Count(&status.PendingReceipts).Error
	return status, err
}

// UserQuotaReceiptExists distinguishes historical jobs from configured jobs.
// Current policy disablement never changes a job's original accounting path.
func UserQuotaReceiptExists(ctx context.Context, sourceKey string, userID int) (bool, error) {
	if userID <= 0 || db.GetDB() == nil {
		return false, nil
	}
	var receipt model.UserQuotaReceipt
	err := db.GetDB().WithContext(ctx).Select("source_key").First(&receipt, "source_key = ? AND user_id = ?", sourceKey, userID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	return err == nil, err
}

func userQuotaAvailable(user model.User, now time.Time) float64 {
	available := user.Balance
	if userMonthlyActive(user) {
		used := user.MonthlyUsed
		next := nextMonthlyReset(now).Unix()
		if !(user.MonthlyResetAt > now.Unix() && user.MonthlyResetAt <= next) {
			used = 0
		}
		available = user.MonthlyLimit - used
	}
	return available - userQuotaInflightOf(user.ID)
}
