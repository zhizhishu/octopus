package op

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"gorm.io/gorm"
)

func RedeemCodeGenerate(req model.RedeemCodeGenerateRequest, creatorID int, ctx context.Context) ([]model.RedeemCode, error) {
	if req.Count <= 0 {
		req.Count = 1
	}
	if req.Count > 500 {
		return nil, fmt.Errorf("count cannot exceed 500")
	}
	if err := validateRedeemCodeRequest(req); err != nil {
		return nil, err
	}
	enabled := req.Enabled
	if !enabled {
		enabled = true
	}
	dailyQuota := resolveRedeemDailyQuota(req)
	validDays := resolveRedeemValidDays(req)

	codes := make([]model.RedeemCode, 0, req.Count)
	for len(codes) < req.Count {
		code, err := newRedeemCodeValue()
		if err != nil {
			return nil, err
		}
		codes = append(codes, model.RedeemCode{
			Code:            code,
			Type:            req.Type,
			Enabled:         enabled,
			BalanceAmount:   maxFloat(req.BalanceAmount, 0),
			MonthlyLimit:    dailyQuota,
			MonthlyDays:     validDays,
			CreatedByUserID: creatorID,
			Note:            req.Note,
		})
	}
	if err := db.GetDB().WithContext(ctx).Create(&codes).Error; err != nil {
		return nil, fmt.Errorf("failed to create redeem codes: %w", err)
	}
	return codes, nil
}

func RedeemCodeList(ctx context.Context) ([]model.RedeemCode, error) {
	var codes []model.RedeemCode
	if err := db.GetDB().WithContext(ctx).Order("id DESC").Find(&codes).Error; err != nil {
		return nil, err
	}
	return codes, nil
}

func RedeemCodeUpdate(req model.RedeemCodeUpdateRequest, ctx context.Context) (model.RedeemCode, error) {
	var code model.RedeemCode
	if err := db.GetDB().WithContext(ctx).First(&code, req.ID).Error; err != nil {
		return model.RedeemCode{}, fmt.Errorf("redeem code not found: %w", err)
	}
	if code.Used {
		return model.RedeemCode{}, fmt.Errorf("used redeem code cannot be changed")
	}
	code.Enabled = req.Enabled
	code.Note = req.Note
	if err := db.GetDB().WithContext(ctx).Save(&code).Error; err != nil {
		return model.RedeemCode{}, fmt.Errorf("failed to update redeem code: %w", err)
	}
	return code, nil
}

func RedeemCodeDelete(id int, ctx context.Context) error {
	var code model.RedeemCode
	if err := db.GetDB().WithContext(ctx).First(&code, id).Error; err != nil {
		return fmt.Errorf("redeem code not found: %w", err)
	}
	if code.Used {
		return fmt.Errorf("used redeem code cannot be deleted")
	}
	return db.GetDB().WithContext(ctx).Delete(&model.RedeemCode{}, id).Error
}

func RedeemCodeRedeem(rawCode string, userID int, ctx context.Context) (model.User, error) {
	codeValue := strings.TrimSpace(rawCode)
	if codeValue == "" {
		return model.User{}, fmt.Errorf("redeem code is required")
	}

	var redeemedUser model.User
	err := db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user model.User
		if err := tx.First(&user, userID).Error; err != nil {
			return fmt.Errorf("user not found: %w", err)
		}
		user.Normalize()
		nextUser, err := applyRedeemCodeTx(tx, codeValue, user)
		if err != nil {
			return err
		}
		redeemedUser = nextUser
		return nil
	})
	if err != nil {
		return model.User{}, err
	}
	userCache.Set(redeemedUser.ID, redeemedUser)
	return redeemedUser, nil
}

func applyRedeemCodeTx(tx *gorm.DB, codeValue string, user model.User) (model.User, error) {
	var code model.RedeemCode
	if err := tx.Where("code = ?", codeValue).First(&code).Error; err != nil {
		return model.User{}, fmt.Errorf("redeem code not found: %w", err)
	}
	if !code.Enabled {
		return model.User{}, fmt.Errorf("redeem code is disabled")
	}
	if code.Used {
		return model.User{}, fmt.Errorf("redeem code has already been used")
	}

	user.Normalize()
	switch code.Type {
	case model.RedeemCodeTypeBalance:
		if code.BalanceAmount <= 0 {
			return model.User{}, fmt.Errorf("invalid balance redeem code")
		}
		user.Balance += code.BalanceAmount
	case model.RedeemCodeTypeMonthly:
		if code.MonthlyDays <= 0 || code.MonthlyLimit <= 0 {
			return model.User{}, fmt.Errorf("invalid monthly redeem code")
		}
		now := time.Now()
		expireAt := now.Add(time.Duration(code.MonthlyDays) * 24 * time.Hour).Unix()
		if user.MonthlyExpireAt > now.Unix() {
			expireAt = time.Unix(user.MonthlyExpireAt, 0).Add(time.Duration(code.MonthlyDays) * 24 * time.Hour).Unix()
		}
		user.MonthlyLimit = maxFloat(code.MonthlyLimit, 0)
		user.MonthlyUsed = 0
		user.MonthlyExpireAt = expireAt
		user.MonthlyResetAt = nextMonthlyReset(now).Unix()
	default:
		return model.User{}, fmt.Errorf("invalid redeem code type")
	}

	if err := tx.Model(&model.User{}).Where("id = ?", user.ID).Updates(map[string]any{
		"balance":           user.Balance,
		"monthly_limit":     user.MonthlyLimit,
		"monthly_used":      user.MonthlyUsed,
		"monthly_expire_at": user.MonthlyExpireAt,
		"monthly_reset_at":  user.MonthlyResetAt,
	}).Error; err != nil {
		return model.User{}, fmt.Errorf("failed to apply redeem code: %w", err)
	}

	now := time.Now().Unix()
	result := tx.Model(&model.RedeemCode{}).Where("id = ? AND used = ?", code.ID, false).Updates(map[string]any{
		"used":            true,
		"used_by_user_id": user.ID,
		"used_at":         now,
	})
	if result.Error != nil {
		return model.User{}, fmt.Errorf("failed to mark redeem code used: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return model.User{}, fmt.Errorf("redeem code has already been used")
	}

	return user, nil
}

func validateRedeemCodeRequest(req model.RedeemCodeGenerateRequest) error {
	switch req.Type {
	case model.RedeemCodeTypeBalance:
		if req.BalanceAmount <= 0 {
			return fmt.Errorf("balance amount must be positive")
		}
	case model.RedeemCodeTypeMonthly:
		if resolveRedeemValidDays(req) <= 0 {
			return fmt.Errorf("valid days must be positive")
		}
		if resolveRedeemDailyQuota(req) <= 0 {
			return fmt.Errorf("daily quota must be positive")
		}
	default:
		return fmt.Errorf("invalid redeem code type")
	}
	return nil
}

func resolveRedeemDailyQuota(req model.RedeemCodeGenerateRequest) float64 {
	if req.DailyQuota != nil {
		return maxFloat(*req.DailyQuota, 0)
	}
	return maxFloat(req.MonthlyLimit, 0)
}

func resolveRedeemValidDays(req model.RedeemCodeGenerateRequest) int {
	if req.ValidDays != nil {
		return maxInt(*req.ValidDays, 0)
	}
	return maxInt(req.MonthlyDays, 0)
}

func newRedeemCodeValue() (string, error) {
	bytes := make([]byte, 12)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return "rd-" + hex.EncodeToString(bytes), nil
}

func maxInt(value int, min int) int {
	if value < min {
		return min
	}
	return value
}
