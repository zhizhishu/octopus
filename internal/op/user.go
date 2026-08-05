package op

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/utils/cache"
	"github.com/bestruirui/octopus/internal/utils/log"
	"gorm.io/gorm"
)

var userCache = cache.New[int, model.User](8)
var userNameIDMap = cache.New[string, int](8)

// userRelayIPSaveTimeout 是一轮 relay IP 审计批量落库的超时。
const userRelayIPSaveTimeout = 30 * time.Second

// userRelayIPRecord 是一条待落库的 relay IP 审计(同一用户只留最后一次)。
type userRelayIPRecord struct {
	ip      string
	relayAt int64
}

// userRelayIPPending 暂存待落库的 relay IP 审计。last_relay_ip/at 是纯审计字段,
// 每请求一次同步 UPDATE users 等于把一次 DB 写钉死在转发路径上; 改成内存即时更新
// (读接口走 userCache, 看到的仍是最新值) + 周期任务批量补齐 DB(见 UserRelayIPSaveDBTask)。
// 计费相关的 UserRecordUsage(月卡/余额)不走这条路, 仍然同步扣减、额度判断零滞后。
var userRelayIPPending = make(map[int]userRelayIPRecord)
var userRelayIPPendingLock sync.Mutex

// userLocks serialises the per-user read-modify-write of the cached User — balance /
// monthly_used in UserRecordUsage, last_relay_ip/at in UserRecordRelayIP. Without it two
// concurrent completions for the SAME user both read the old cached struct and the second
// userCache.Set clobbers the first's deduction: a lost update that systematically
// undercharges, and a relay-IP Set that rolls the cached balance back to a pre-deduction
// value the next quota check then trusts. A fixed shard array bounds memory; the same user
// always hashes to one lock, different users only contend on hash collision (rare, benign).
// This keeps the exact maxFloat(...) floor semantics and stays cross-DB — no dialect-specific
// atomic SQL (SQLite max() vs MySQL/Postgres GREATEST()).
const userLockShards = 256

var userLocks [userLockShards]sync.Mutex

func lockUser(userID int) *sync.Mutex {
	idx := userID % userLockShards
	if idx < 0 {
		idx += userLockShards
	}
	return &userLocks[idx]
}

func UserInit() error {
	ctx := context.Background()
	conn := db.GetDB().WithContext(ctx)

	var count int64
	if err := conn.Model(&model.User{}).Count(&count).Error; err != nil {
		return fmt.Errorf("failed to count users: %w", err)
	}
	if count == 0 {
		admin := model.User{
			Username: "admin",
			Password: "admin",
			Role:     model.UserRoleAdmin,
			Status:   model.UserStatusActive,
		}
		if err := admin.HashPassword(); err != nil {
			return err
		}
		if err := conn.Create(&admin).Error; err != nil {
			return err
		}
		log.Infof("initial user: admin,password: admin")
	} else {
		var first model.User
		if err := conn.Order("id ASC").First(&first).Error; err != nil {
			return fmt.Errorf("failed to load default admin user: %w", err)
		}
		updates := map[string]any{}
		if first.Role == "" || first.Role != model.UserRoleAdmin {
			updates["role"] = model.UserRoleAdmin
		}
		if first.Status == "" || first.Status != model.UserStatusActive {
			updates["status"] = model.UserStatusActive
		}
		if len(updates) > 0 {
			if err := conn.Model(&model.User{}).Where("id = ?", first.ID).Updates(updates).Error; err != nil {
				return fmt.Errorf("failed to normalize default admin user: %w", err)
			}
		}
	}

	if err := backfillLegacyOwnership(ctx); err != nil {
		return err
	}
	return userRefreshCache(ctx)
}

func backfillLegacyOwnership(ctx context.Context) error {
	admin, err := UserDefaultAdmin(ctx)
	if err != nil {
		return err
	}
	conn := db.GetDB().WithContext(ctx)
	if conn.Migrator().HasColumn(&model.APIKey{}, "user_id") {
		if err := conn.Model(&model.APIKey{}).Where("user_id = 0 OR user_id IS NULL").Update("user_id", admin.ID).Error; err != nil {
			return fmt.Errorf("failed to backfill api key owner: %w", err)
		}
	}
	if conn.Migrator().HasColumn(&model.RelayLog{}, "user_id") {
		if err := conn.Model(&model.RelayLog{}).Where("user_id = 0 OR user_id IS NULL").Update("user_id", admin.ID).Error; err != nil {
			return fmt.Errorf("failed to backfill relay log owner: %w", err)
		}
	}
	return nil
}

func userRefreshCache(ctx context.Context) error {
	var users []model.User
	if err := db.GetDB().WithContext(ctx).Find(&users).Error; err != nil {
		return err
	}
	byID := make(map[int]model.User, len(users))
	byName := make(map[string]int, len(users))
	for _, user := range users {
		user.Normalize()
		byID[user.ID] = user
		byName[normalizeUsername(user.Username)] = user.ID
	}
	userCache.ReplaceAll(byID)
	userNameIDMap.ReplaceAll(byName)
	return nil
}

func UserList(ctx context.Context) ([]model.User, error) {
	users := make([]model.User, 0)
	if err := db.GetDB().WithContext(ctx).Order("id ASC").Find(&users).Error; err != nil {
		return nil, err
	}
	if err := ensureAccessPlanCache(ctx); err != nil {
		return nil, err
	}
	for i := range users {
		users[i].Normalize()
		attachUserAccessPlans(&users[i])
	}
	return users, nil
}

func UserDefaultAdmin(ctx context.Context) (model.User, error) {
	var user model.User
	if err := db.GetDB().WithContext(ctx).Where("role = ?", model.UserRoleAdmin).Order("id ASC").First(&user).Error; err != nil {
		return model.User{}, fmt.Errorf("default admin user not found: %w", err)
	}
	user.Normalize()
	return user, nil
}

func UserGet(id int) (model.User, error) {
	user, ok := userCache.Get(id)
	if !ok {
		return model.User{}, fmt.Errorf("user not found")
	}
	user.Normalize()
	return user, nil
}

func UserGetByUsername(username string) (model.User, error) {
	id, ok := userNameIDMap.Get(normalizeUsername(username))
	if !ok {
		return model.User{}, fmt.Errorf("user not found")
	}
	return UserGet(id)
}

func UserVerify(username, password string) (model.User, error) {
	user, err := UserGetByUsername(username)
	if err != nil {
		return model.User{}, fmt.Errorf("incorrect username")
	}
	if !user.IsActive() {
		return model.User{}, fmt.Errorf("user disabled")
	}
	if err := user.ComparePassword(password); err != nil {
		return model.User{}, fmt.Errorf("incorrect password")
	}
	return user, nil
}

func UserRegistrationOptions() model.UserRegistrationOptions {
	open, err := SettingGetBool(model.SettingKeyUserRegistrationEnabled)
	if err != nil {
		open = false
	}
	emailVerification, err := SettingGetBool(model.SettingKeyEmailVerificationEnabled)
	if err != nil {
		emailVerification = false
	}
	return model.UserRegistrationOptions{
		OpenRegistration:         open,
		InviteRequired:           !open,
		EmailVerificationEnabled: emailVerification,
	}
}

func UserRegister(req model.UserRegister, registerIP string, ctx context.Context) (model.User, error) {
	username := strings.TrimSpace(req.Username)
	password := strings.TrimSpace(req.Password)
	inviteCode := strings.TrimSpace(req.InviteCode)
	if username == "" {
		return model.User{}, fmt.Errorf("username is required")
	}
	if password == "" {
		return model.User{}, fmt.Errorf("password is required")
	}

	options := UserRegistrationOptions()
	if inviteCode == "" && !options.OpenRegistration {
		return model.User{}, fmt.Errorf("invite code is required")
	}

	email := strings.TrimSpace(strings.ToLower(req.Email))
	if options.EmailVerificationEnabled {
		if email == "" {
			return model.User{}, fmt.Errorf("email is required")
		}
		if !validateEmail(email) {
			return model.User{}, fmt.Errorf("invalid email address")
		}
		code := strings.TrimSpace(req.VerificationCode)
		if code == "" {
			return model.User{}, fmt.Errorf("verification code is required")
		}
		if !VerifyEmailCode(email, code) {
			return model.User{}, fmt.Errorf("invalid or expired verification code")
		}
	}

	registerIP = normalizeRequestIP(registerIP)
	var registeredUser model.User
	err := db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if email != "" {
			var emailCount int64
			if err := tx.Model(&model.User{}).Where("email = ?", email).Count(&emailCount).Error; err != nil {
				return err
			}
			if emailCount > 0 {
				return fmt.Errorf("email already registered")
			}
		}
		user := model.User{
			Username:      username,
			Password:      password,
			Role:          model.UserRoleUser,
			Status:        model.UserStatusActive,
			RegisterIP:    registerIP,
			Email:         email,
			EmailVerified: options.EmailVerificationEnabled && email != "",
		}
		if err := user.HashPassword(); err != nil {
			return err
		}
		if err := tx.Create(&user).Error; err != nil {
			return fmt.Errorf("failed to create user: %w", err)
		}
		user.Normalize()

		if inviteCode != "" {
			redeemedUser, err := applyRedeemCodeTx(tx, inviteCode, user)
			if err != nil {
				return err
			}
			user = redeemedUser
		}

		registeredUser = user
		return nil
	})
	if err != nil {
		return model.User{}, err
	}
	userCache.Set(registeredUser.ID, registeredUser)
	userNameIDMap.Set(normalizeUsername(registeredUser.Username), registeredUser.ID)
	if options.EmailVerificationEnabled && email != "" {
		ConsumeEmailCode(email)
	}
	if err := UserAccessPlanSet(registeredUser.ID, nil, 0, ctx); err != nil {
		return model.User{}, err
	}
	attachUserAccessPlans(&registeredUser)
	return registeredUser, nil
}

func UserCreate(req model.UserCreateRequest, ctx context.Context) (model.User, error) {
	username := strings.TrimSpace(req.Username)
	if username == "" {
		return model.User{}, fmt.Errorf("username is required")
	}
	if strings.TrimSpace(req.Password) == "" {
		return model.User{}, fmt.Errorf("password is required")
	}
	role := req.Role
	if role == "" {
		role = model.UserRoleUser
	}
	status := req.Status
	if status == "" {
		status = model.UserStatusActive
	}
	if err := validateUserRoleStatus(role, status); err != nil {
		return model.User{}, err
	}

	monthlyLimit := resolveMonthlyLimit(req.MonthlyLimit, req.DailyLimit, req.DailyQuota)
	monthlyUsed := resolveMonthlyUsed(req.MonthlyUsed, req.DailyUsed)
	monthlyExpireAt, err := resolveUnixSeconds(req.MonthlyExpireAt, req.MonthlyExpireAtISO)
	if err != nil {
		return model.User{}, err
	}
	monthlyResetAt, err := resolveUnixSeconds(req.MonthlyResetAt, req.NextResetAtISO, req.MonthlyResetAtISO)
	if err != nil {
		return model.User{}, err
	}
	monthlyResetAt = ensureMonthlyResetAt(monthlyLimit, monthlyExpireAt, monthlyResetAt, time.Now())

	user := model.User{
		Username:        username,
		Password:        req.Password,
		Role:            role,
		Status:          status,
		Balance:         maxFloat(req.Balance, 0),
		MonthlyLimit:    monthlyLimit,
		MonthlyUsed:     monthlyUsed,
		MonthlyExpireAt: monthlyExpireAt,
		MonthlyResetAt:  monthlyResetAt,
		Note:            req.Note,
	}
	if err := user.HashPassword(); err != nil {
		return model.User{}, err
	}
	if err := db.GetDB().WithContext(ctx).Create(&user).Error; err != nil {
		return model.User{}, fmt.Errorf("failed to create user: %w", err)
	}
	userCache.Set(user.ID, user)
	userNameIDMap.Set(normalizeUsername(user.Username), user.ID)
	if err := UserAccessPlanSet(user.ID, req.AccessPlanIDs, req.DefaultAccessPlanID, ctx); err != nil {
		return model.User{}, err
	}
	attachUserAccessPlans(&user)
	return user, nil
}

func UserUpdate(req model.UserUpdateRequest, actorID int, ctx context.Context) (model.User, error) {
	user, err := UserGet(req.ID)
	if err != nil {
		return model.User{}, err
	}
	username := strings.TrimSpace(req.Username)
	if username == "" {
		return model.User{}, fmt.Errorf("username is required")
	}
	role := req.Role
	if role == "" {
		role = model.UserRoleUser
	}
	status := req.Status
	if status == "" {
		status = model.UserStatusActive
	}
	if err := validateUserRoleStatus(role, status); err != nil {
		return model.User{}, err
	}
	if err := ensureAdminKeepsAccess(user, role, status, actorID, ctx); err != nil {
		return model.User{}, err
	}

	oldUsername := user.Username
	user.Username = username
	user.Role = role
	user.Status = status
	user.Balance = maxFloat(req.Balance, 0)
	monthlyLimit := resolveMonthlyLimit(req.MonthlyLimit, req.DailyLimit, req.DailyQuota)
	monthlyUsed := resolveMonthlyUsed(req.MonthlyUsed, req.DailyUsed)
	monthlyExpireAt, err := resolveUnixSeconds(req.MonthlyExpireAt, req.MonthlyExpireAtISO)
	if err != nil {
		return model.User{}, err
	}
	monthlyResetAt, err := resolveUnixSeconds(req.MonthlyResetAt, req.NextResetAtISO, req.MonthlyResetAtISO)
	if err != nil {
		return model.User{}, err
	}
	monthlyResetAt = ensureMonthlyResetAt(monthlyLimit, monthlyExpireAt, monthlyResetAt, time.Now())

	user.MonthlyLimit = monthlyLimit
	user.MonthlyUsed = monthlyUsed
	user.MonthlyExpireAt = monthlyExpireAt
	user.MonthlyResetAt = monthlyResetAt
	user.Note = req.Note

	if err := db.GetDB().WithContext(ctx).Model(&model.User{}).Where("id = ?", user.ID).Updates(map[string]any{
		"username":          user.Username,
		"role":              user.Role,
		"status":            user.Status,
		"balance":           user.Balance,
		"monthly_limit":     user.MonthlyLimit,
		"monthly_used":      user.MonthlyUsed,
		"monthly_expire_at": user.MonthlyExpireAt,
		"monthly_reset_at":  user.MonthlyResetAt,
		"note":              user.Note,
	}).Error; err != nil {
		return model.User{}, fmt.Errorf("failed to update user: %w", err)
	}
	userCache.Set(user.ID, user)
	if oldUsername != user.Username {
		userNameIDMap.Del(normalizeUsername(oldUsername))
	}
	userNameIDMap.Set(normalizeUsername(user.Username), user.ID)
	if req.AccessPlanIDs != nil || req.DefaultAccessPlanID > 0 {
		if err := UserAccessPlanSet(user.ID, req.AccessPlanIDs, req.DefaultAccessPlanID, ctx); err != nil {
			return model.User{}, err
		}
	}
	attachUserAccessPlans(&user)
	return user, nil
}

func UserDelete(id int, actorID int, ctx context.Context) error {
	user, err := UserGet(id)
	if err != nil {
		return err
	}
	if err := ensureAdminKeepsAccess(user, model.UserRoleUser, model.UserStatusDisabled, actorID, ctx); err != nil {
		return err
	}
	var keyCount int64
	if err := db.GetDB().WithContext(ctx).Model(&model.APIKey{}).Where("user_id = ?", id).Count(&keyCount).Error; err != nil {
		return err
	}
	if keyCount > 0 {
		return fmt.Errorf("user still owns API keys; disable the user or move/delete keys first")
	}
	tx := db.GetDB().WithContext(ctx).Begin()
	if err := tx.Where("user_id = ?", id).Delete(&model.UserAccessPlan{}).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to delete user access plans: %w", err)
	}
	if err := tx.Delete(&model.User{}, id).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to delete user: %w", err)
	}
	if err := tx.Commit().Error; err != nil {
		return err
	}
	userCache.Del(id)
	userNameIDMap.Del(normalizeUsername(user.Username))
	_ = accessPlanRefreshCache(ctx)
	return nil
}

func UserResetPassword(id int, password string, ctx context.Context) error {
	if strings.TrimSpace(password) == "" {
		return fmt.Errorf("password is required")
	}
	user, err := UserGet(id)
	if err != nil {
		return err
	}
	user.Password = password
	if err := user.HashPassword(); err != nil {
		return err
	}
	if err := db.GetDB().WithContext(ctx).Model(&model.User{}).Where("id = ?", id).Update("password", user.Password).Error; err != nil {
		return fmt.Errorf("failed to update password: %w", err)
	}
	userCache.Set(user.ID, user)
	return nil
}

func UserChangePassword(userID int, oldPassword, newPassword string) error {
	user, err := UserGet(userID)
	if err != nil {
		return err
	}
	if err := user.ComparePassword(oldPassword); err != nil {
		return fmt.Errorf("incorrect old password: %w", err)
	}
	return UserResetPassword(userID, newPassword, context.Background())
}

func UserChangeUsername(userID int, newUsername string) error {
	user, err := UserGet(userID)
	if err != nil {
		return err
	}
	req := model.UserUpdateRequest{
		ID:              user.ID,
		Username:        newUsername,
		Role:            user.Role,
		Status:          user.Status,
		Balance:         user.Balance,
		MonthlyLimit:    user.MonthlyLimit,
		MonthlyUsed:     user.MonthlyUsed,
		MonthlyExpireAt: user.MonthlyExpireAt,
		MonthlyResetAt:  user.MonthlyResetAt,
		Note:            user.Note,
	}
	_, err = UserUpdate(req, userID, context.Background())
	return err
}

func UserCanRelay(userID int, ctx context.Context) error {
	user, err := UserGet(userID)
	if err != nil {
		return err
	}
	if !user.IsActive() {
		return fmt.Errorf("user is disabled")
	}
	if user.IsAdmin() {
		return nil
	}
	user, err = UserRefreshMonthlyWindow(user.ID, ctx)
	if err != nil {
		return err
	}
	if userMonthlyActive(user) {
		if user.MonthlyUsed < user.MonthlyLimit {
			return nil
		}
		return fmt.Errorf("monthly quota has been used up")
	}
	if user.Balance > 0 {
		return nil
	}
	return fmt.Errorf("user quota has been used up")
}

func UserRecordUsage(userID int, cost float64, ctx context.Context) error {
	if cost <= 0 {
		return nil
	}
	// Serialise this user's read-modify-write so concurrent completions cannot lose a
	// deduction (see userLocks). Held across the DB write on purpose — the read and the
	// write must be one atomic step for this user; other users run in parallel (other shard).
	mu := lockUser(userID)
	mu.Lock()
	defer mu.Unlock()
	user, err := UserGet(userID)
	if err != nil {
		return err
	}
	if user.IsAdmin() {
		return nil
	}
	user, err = UserRefreshMonthlyWindow(user.ID, ctx)
	if err != nil {
		return err
	}
	if userMonthlyActive(user) {
		user.MonthlyUsed += cost
		if err := db.GetDB().WithContext(ctx).Model(&model.User{}).Where("id = ?", user.ID).Update("monthly_used", user.MonthlyUsed).Error; err != nil {
			return fmt.Errorf("failed to record monthly usage: %w", err)
		}
		userCache.Set(user.ID, user)
		return nil
	}
	user.Balance = maxFloat(user.Balance-cost, 0)
	if err := db.GetDB().WithContext(ctx).Model(&model.User{}).Where("id = ?", user.ID).Update("balance", user.Balance).Error; err != nil {
		return fmt.Errorf("failed to deduct balance: %w", err)
	}
	userCache.Set(user.ID, user)
	return nil
}

// UserRecordRelayIP 记录用户最近一次中继的 IP 与时间。只更新内存缓存 + 挂进待落库表,
// DB 由 UserRelayIPSaveDBTask 周期批量补齐(转发路径上不再每请求同步 UPDATE users)。
// ctx 保留是为了调用方签名不变。
func UserRecordRelayIP(userID int, requestIP string, relayAt int64, ctx context.Context) error {
	requestIP = normalizeRequestIP(requestIP)
	if userID <= 0 || requestIP == "" {
		return nil
	}
	if relayAt <= 0 {
		relayAt = time.Now().Unix()
	}
	// Same per-user lock as UserRecordUsage: read the latest cached user and write back only
	// the relay-IP fields under it, so this can never overwrite a concurrent balance
	// deduction with a pre-deduction snapshot of the struct.
	mu := lockUser(userID)
	mu.Lock()
	defer mu.Unlock()
	user, err := UserGet(userID)
	if err != nil {
		return err
	}
	user.LastRelayIP = requestIP
	user.LastRelayAt = relayAt
	userCache.Set(user.ID, user)

	userRelayIPPendingLock.Lock()
	userRelayIPPending[user.ID] = userRelayIPRecord{ip: requestIP, relayAt: relayAt}
	userRelayIPPendingLock.Unlock()
	return nil
}

// UserRelayIPSaveDB 把待落库的 relay IP 审计批量写入数据库(每个用户只写最后一次)。
// 目前由周期任务 UserRelayIPSaveDBTask 驱动。
func UserRelayIPSaveDB(ctx context.Context) error {
	userRelayIPPendingLock.Lock()
	if len(userRelayIPPending) == 0 {
		userRelayIPPendingLock.Unlock()
		return nil
	}
	pending := userRelayIPPending
	userRelayIPPending = make(map[int]userRelayIPRecord, len(pending))
	userRelayIPPendingLock.Unlock()

	var firstErr error
	for userID, record := range pending {
		// 写失败不回队: 与中继日志 flush 的坏行丢弃同理, 免得一条写不进去的记录(如用户已删)
		// 让之后每轮都重试失败; 内存里的 userCache 仍然是最新值。
		if err := db.GetDB().WithContext(ctx).Model(&model.User{}).Where("id = ?", userID).Updates(map[string]any{
			"last_relay_ip": record.ip,
			"last_relay_at": record.relayAt,
		}).Error; err != nil && firstErr == nil {
			firstErr = fmt.Errorf("failed to record relay ip: %w", err)
		}
	}
	return firstErr
}

// UserRelayIPSaveDBTask 是周期任务入口(签名对齐 StatsSaveDBTask)。
func UserRelayIPSaveDBTask() {
	ctx, cancel := context.WithTimeout(context.Background(), userRelayIPSaveTimeout)
	defer cancel()
	if err := UserRelayIPSaveDB(ctx); err != nil {
		log.Warnf("user relay ip save db error: %v", err)
	}
}

func UserApplyBalance(userID int, amount float64, ctx context.Context) (model.User, error) {
	if amount <= 0 {
		return model.User{}, fmt.Errorf("balance amount must be positive")
	}
	user, err := UserGet(userID)
	if err != nil {
		return model.User{}, err
	}
	user.Balance += amount
	if err := db.GetDB().WithContext(ctx).Model(&model.User{}).Where("id = ?", user.ID).Update("balance", user.Balance).Error; err != nil {
		return model.User{}, fmt.Errorf("failed to apply balance: %w", err)
	}
	userCache.Set(user.ID, user)
	return user, nil
}

func UserCheckInStatus(userID int, ctx context.Context) (model.UserCheckInStatus, error) {
	return UserCheckInStatusAt(userID, time.Now(), ctx)
}

func UserCheckInStatusAt(userID int, now time.Time, ctx context.Context) (model.UserCheckInStatus, error) {
	user, err := UserGet(userID)
	if err != nil {
		return model.UserCheckInStatus{}, err
	}
	cfg := loadCheckInConfig()
	day := checkInDay(now)
	status := model.UserCheckInStatus{
		Enabled:       cfg.enabled,
		Today:         day,
		RewardMode:    cfg.rewardMode,
		RewardAmount:  cfg.rewardAmount,
		RewardMin:     cfg.rewardMin,
		RewardMax:     cfg.rewardMax,
		Balance:       user.Balance,
		NextCheckInAt: nextCheckInAt(now).Unix(),
	}

	var checkIn model.UserCheckIn
	err = db.GetDB().WithContext(ctx).
		Where("user_id = ? AND day = ?", user.ID, day).
		First(&checkIn).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return status, nil
	}
	if err != nil {
		return model.UserCheckInStatus{}, err
	}
	status.CheckedToday = true
	status.LastAmount = checkIn.Amount
	status.CheckedAt = checkIn.CreatedAt
	return status, nil
}

func UserCheckIn(userID int, ctx context.Context) (model.UserCheckInResponse, error) {
	return UserCheckInAt(userID, time.Now(), ctx)
}

func UserCheckInAt(userID int, now time.Time, ctx context.Context) (model.UserCheckInResponse, error) {
	cfg := loadCheckInConfig()
	if !cfg.enabled {
		return model.UserCheckInResponse{}, fmt.Errorf("daily check-in is disabled")
	}

	day := checkInDay(now)
	reward := checkInReward(cfg)
	if reward <= 0 {
		return model.UserCheckInResponse{}, fmt.Errorf("check-in reward must be positive")
	}

	var checkedUser model.User
	var checkedAt int64
	err := db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing model.UserCheckIn
		err := tx.Where("user_id = ? AND day = ?", userID, day).First(&existing).Error
		if err == nil {
			return fmt.Errorf("already checked in today")
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		var user model.User
		if err := tx.First(&user, userID).Error; err != nil {
			return err
		}
		user.Normalize()
		if !user.IsActive() {
			return fmt.Errorf("user disabled")
		}

		checkIn := model.UserCheckIn{
			UserID: user.ID,
			Day:    day,
			Amount: reward,
		}
		if err := tx.Create(&checkIn).Error; err != nil {
			return fmt.Errorf("already checked in today")
		}
		checkedAt = checkIn.CreatedAt

		user.Balance = roundBalance(user.Balance + reward)
		if err := tx.Model(&model.User{}).Where("id = ?", user.ID).Update("balance", user.Balance).Error; err != nil {
			return fmt.Errorf("failed to apply check-in reward: %w", err)
		}
		checkedUser = user
		return nil
	})
	if err != nil {
		return model.UserCheckInResponse{}, err
	}
	userCache.Set(checkedUser.ID, checkedUser)

	status := model.UserCheckInStatus{
		Enabled:       cfg.enabled,
		CheckedToday:  true,
		Today:         day,
		RewardMode:    cfg.rewardMode,
		RewardAmount:  cfg.rewardAmount,
		RewardMin:     cfg.rewardMin,
		RewardMax:     cfg.rewardMax,
		LastAmount:    reward,
		Balance:       checkedUser.Balance,
		CheckedAt:     checkedAt,
		NextCheckInAt: nextCheckInAt(now).Unix(),
	}
	return model.UserCheckInResponse{
		User:   model.NewUserResponseAt(checkedUser, now),
		Status: status,
		Reward: reward,
	}, nil
}

func UserApplyMonthly(userID int, monthlyLimit float64, days int, ctx context.Context) (model.User, error) {
	if days <= 0 {
		return model.User{}, fmt.Errorf("monthly days must be positive")
	}
	if monthlyLimit <= 0 {
		return model.User{}, fmt.Errorf("monthly limit must be positive")
	}
	user, err := UserGet(userID)
	if err != nil {
		return model.User{}, err
	}
	now := time.Now()
	expireAt := now.Add(time.Duration(days) * 24 * time.Hour).Unix()
	if user.MonthlyExpireAt > now.Unix() {
		expireAt = time.Unix(user.MonthlyExpireAt, 0).Add(time.Duration(days) * 24 * time.Hour).Unix()
	}
	user.MonthlyLimit = maxFloat(monthlyLimit, 0)
	user.MonthlyUsed = 0
	user.MonthlyExpireAt = expireAt
	user.MonthlyResetAt = nextMonthlyReset(now).Unix()
	if err := db.GetDB().WithContext(ctx).Model(&model.User{}).Where("id = ?", user.ID).Updates(map[string]any{
		"monthly_limit":     user.MonthlyLimit,
		"monthly_used":      user.MonthlyUsed,
		"monthly_expire_at": user.MonthlyExpireAt,
		"monthly_reset_at":  user.MonthlyResetAt,
	}).Error; err != nil {
		return model.User{}, fmt.Errorf("failed to apply monthly card: %w", err)
	}
	userCache.Set(user.ID, user)
	return user, nil
}

type checkInConfig struct {
	enabled      bool
	rewardMode   model.CheckInRewardMode
	rewardAmount float64
	rewardMin    float64
	rewardMax    float64
}

func loadCheckInConfig() checkInConfig {
	enabled, err := SettingGetBool(model.SettingKeyCheckInEnabled)
	if err != nil {
		enabled = false
	}
	modeValue, err := SettingGetString(model.SettingKeyCheckInRewardMode)
	if err != nil {
		modeValue = string(model.CheckInRewardModeFixed)
	}
	mode := model.CheckInRewardMode(strings.TrimSpace(modeValue))
	if mode != model.CheckInRewardModeRandom {
		mode = model.CheckInRewardModeFixed
	}
	cfg := checkInConfig{
		enabled:      enabled,
		rewardMode:   mode,
		rewardAmount: settingFloat(model.SettingKeyCheckInRewardAmount, 100),
		rewardMin:    settingFloat(model.SettingKeyCheckInRewardMin, 100),
		rewardMax:    settingFloat(model.SettingKeyCheckInRewardMax, 200),
	}
	if cfg.rewardAmount <= 0 {
		cfg.rewardAmount = 100
	}
	if cfg.rewardMin <= 0 {
		cfg.rewardMin = 100
	}
	if cfg.rewardMax <= 0 {
		cfg.rewardMax = cfg.rewardMin
	}
	if cfg.rewardMax < cfg.rewardMin {
		cfg.rewardMin, cfg.rewardMax = cfg.rewardMax, cfg.rewardMin
	}
	return cfg
}

func settingFloat(key model.SettingKey, fallback float64) float64 {
	value, err := SettingGetString(key)
	if err != nil {
		return fallback
	}
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return fallback
	}
	return parsed
}

func checkInReward(cfg checkInConfig) float64 {
	if cfg.rewardMode != model.CheckInRewardModeRandom {
		return roundBalance(cfg.rewardAmount)
	}
	if cfg.rewardMax <= cfg.rewardMin {
		return roundBalance(cfg.rewardMin)
	}
	const precision = int64(1000000)
	n, err := rand.Int(rand.Reader, big.NewInt(precision+1))
	if err != nil {
		return roundBalance(cfg.rewardMin)
	}
	ratio := float64(n.Int64()) / float64(precision)
	return roundBalance(cfg.rewardMin + (cfg.rewardMax-cfg.rewardMin)*ratio)
}

func checkInDay(now time.Time) string {
	return now.Local().Format("2006-01-02")
}

func nextCheckInAt(now time.Time) time.Time {
	local := now.Local()
	return time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, local.Location())
}

func UserRefreshMonthlyWindow(userID int, ctx context.Context) (model.User, error) {
	user, err := UserGet(userID)
	if err != nil {
		return model.User{}, err
	}
	if !userMonthlyActive(user) {
		return user, nil
	}
	now := time.Now()
	nextResetAt := nextMonthlyReset(now).Unix()
	if user.MonthlyResetAt > now.Unix() && user.MonthlyResetAt <= nextResetAt {
		return user, nil
	}
	user.MonthlyUsed = 0
	user.MonthlyResetAt = nextResetAt
	if err := db.GetDB().WithContext(ctx).Model(&model.User{}).Where("id = ?", user.ID).Updates(map[string]any{
		"monthly_used":     user.MonthlyUsed,
		"monthly_reset_at": user.MonthlyResetAt,
	}).Error; err != nil {
		return model.User{}, err
	}
	userCache.Set(user.ID, user)
	return user, nil
}

func UserUsageRank(ctx context.Context) ([]model.UserUsageSummary, error) {
	users, err := UserList(ctx)
	if err != nil {
		return nil, err
	}
	summaries := make(map[int]model.UserUsageSummary, len(users))
	for _, user := range users {
		summaries[user.ID] = model.UserUsageSummary{
			UserID:   user.ID,
			Username: user.Username,
			Role:     user.Role,
		}
	}
	keys, err := APIKeyListAll(ctx)
	if err != nil {
		return nil, err
	}
	for _, key := range keys {
		summary, ok := summaries[key.UserID]
		if !ok {
			continue
		}
		stats := StatsAPIKeyGet(key.ID)
		summary.RequestSuccess += stats.RequestSuccess
		summary.RequestFailed += stats.RequestFailed
		summary.InputToken += stats.InputToken
		summary.OutputToken += stats.OutputToken
		summary.TotalCost += stats.InputCost + stats.OutputCost
		summaries[key.UserID] = summary
	}
	result := make([]model.UserUsageSummary, 0, len(summaries))
	for _, summary := range summaries {
		result = append(result, summary)
	}
	return result, nil
}

func validateUserRoleStatus(role model.UserRole, status model.UserStatus) error {
	if role != model.UserRoleAdmin && role != model.UserRoleUser {
		return fmt.Errorf("invalid user role")
	}
	if status != model.UserStatusActive && status != model.UserStatusDisabled {
		return fmt.Errorf("invalid user status")
	}
	return nil
}

func ensureAdminKeepsAccess(user model.User, nextRole model.UserRole, nextStatus model.UserStatus, actorID int, ctx context.Context) error {
	if user.Role != model.UserRoleAdmin {
		return nil
	}
	if nextRole == model.UserRoleAdmin && nextStatus == model.UserStatusActive {
		return nil
	}
	if user.ID == actorID {
		return fmt.Errorf("cannot remove admin access from yourself")
	}
	var count int64
	err := db.GetDB().WithContext(ctx).
		Model(&model.User{}).
		Where("id <> ? AND role = ? AND status = ?", user.ID, model.UserRoleAdmin, model.UserStatusActive).
		Count(&count).Error
	if err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("at least one active admin is required")
	}
	return nil
}

func userMonthlyActive(user model.User) bool {
	return user.MonthlyExpireAt > time.Now().Unix() && user.MonthlyLimit > 0
}

func nextMonthlyReset(now time.Time) time.Time {
	return time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
}

func resolveMonthlyLimit(monthlyLimit float64, dailyLimit, dailyQuota *float64) float64 {
	if dailyQuota != nil {
		return maxFloat(*dailyQuota, 0)
	}
	if dailyLimit != nil {
		return maxFloat(*dailyLimit, 0)
	}
	return maxFloat(monthlyLimit, 0)
}

func resolveMonthlyUsed(monthlyUsed float64, dailyUsed *float64) float64 {
	if dailyUsed != nil {
		return maxFloat(*dailyUsed, 0)
	}
	return maxFloat(monthlyUsed, 0)
}

func resolveUnixSeconds(raw int64, aliases ...string) (int64, error) {
	for _, alias := range aliases {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			continue
		}
		parsed, err := parseUnixSecondsAlias(alias)
		if err != nil {
			return 0, err
		}
		return maxInt64(parsed, 0), nil
	}
	return maxInt64(raw, 0), nil
}

func parseUnixSecondsAlias(value string) (int64, error) {
	if unix, err := strconv.ParseInt(value, 10, 64); err == nil {
		return unix, nil
	}
	layouts := []string{
		time.RFC3339,
		"2006-01-02",
		"2006-01-02 15:04",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if parsed, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			return parsed.Unix(), nil
		}
	}
	return 0, fmt.Errorf("invalid time value %q, expected RFC3339 or YYYY-MM-DD", value)
}

func ensureMonthlyResetAt(monthlyLimit float64, monthlyExpireAt int64, monthlyResetAt int64, now time.Time) int64 {
	if monthlyLimit <= 0 || monthlyExpireAt <= now.Unix() {
		return monthlyResetAt
	}
	nextResetAt := nextMonthlyReset(now).Unix()
	if monthlyResetAt > now.Unix() && monthlyResetAt <= nextResetAt {
		return monthlyResetAt
	}
	return nextResetAt
}

func normalizeUsername(username string) string {
	return strings.ToLower(strings.TrimSpace(username))
}

func normalizeRequestIP(ip string) string {
	ip = strings.TrimSpace(ip)
	if len(ip) > 128 {
		return ip[:128]
	}
	return ip
}

func maxFloat(value float64, min float64) float64 {
	if value < min {
		return min
	}
	return value
}

func roundBalance(value float64) float64 {
	return math.Round(value*1000000) / 1000000
}

func maxInt64(value int64, min int64) int64 {
	if value < min {
		return min
	}
	return value
}
