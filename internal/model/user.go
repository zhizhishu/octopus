package model

import (
	"fmt"
	"math"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type UserRole string

const (
	UserRoleAdmin UserRole = "admin"
	UserRoleUser  UserRole = "user"
)

type UserStatus string

const (
	UserStatusActive   UserStatus = "active"
	UserStatusDisabled UserStatus = "disabled"
)

type User struct {
	ID              int        `json:"id" gorm:"primaryKey"`
	Username        string     `json:"username" gorm:"unique;not null"`
	Password        string     `json:"-" gorm:"not null"`
	Role            UserRole   `json:"role" gorm:"default:admin;index"`
	Status          UserStatus `json:"status" gorm:"default:active;index"`
	Balance         float64    `json:"balance" gorm:"type:real;default:0"`
	MonthlyLimit    float64    `json:"monthly_limit" gorm:"type:real;default:0"`
	MonthlyUsed     float64    `json:"monthly_used" gorm:"type:real;default:0"`
	MonthlyExpireAt int64      `json:"monthly_expire_at" gorm:"default:0"`
	MonthlyResetAt  int64      `json:"monthly_reset_at" gorm:"default:0"`
	RegisterIP      string     `json:"register_ip,omitempty" gorm:"size:128"`
	LastRelayIP     string     `json:"last_relay_ip,omitempty" gorm:"size:128"`
	LastRelayAt     int64      `json:"last_relay_at" gorm:"default:0"`
	Note            string     `json:"note,omitempty"`
	Email           string     `json:"email,omitempty" gorm:"size:255;index"`
	EmailVerified   bool       `json:"email_verified" gorm:"default:false"`
	CreatedAt       int64      `json:"created_at" gorm:"autoCreateTime"`
	UpdatedAt       int64      `json:"updated_at" gorm:"autoUpdateTime"`

	AccessPlanIDs       []int        `json:"access_plan_ids,omitempty" gorm:"-"`
	DefaultAccessPlanID int          `json:"default_access_plan_id,omitempty" gorm:"-"`
	AccessPlans         []AccessPlan `json:"access_plans,omitempty" gorm:"-"`
}

type UserMonthlyQuotaSummary struct {
	DailyLimit         float64 `json:"daily_limit"`
	DailyUsed          float64 `json:"daily_used"`
	DailyRemaining     float64 `json:"daily_remaining"`
	MonthlyActive      bool    `json:"monthly_active"`
	MonthlyStatus      string  `json:"monthly_status"`
	MonthlyExpireAtISO string  `json:"monthly_expire_at_iso"`
	MonthlyResetAtISO  string  `json:"monthly_reset_at_iso"`
	NextResetAtISO     string  `json:"next_reset_at_iso"`
	DaysLeft           int     `json:"days_left"`
}

type UserResponse struct {
	User
	DailyLimit         float64 `json:"daily_limit"`
	DailyUsed          float64 `json:"daily_used"`
	DailyRemaining     float64 `json:"daily_remaining"`
	MonthlyActive      bool    `json:"monthly_active"`
	MonthlyStatus      string  `json:"monthly_status"`
	MonthlyExpireAtISO string  `json:"monthly_expire_at_iso"`
	MonthlyResetAtISO  string  `json:"monthly_reset_at_iso"`
	NextResetAtISO     string  `json:"next_reset_at_iso"`
	DaysLeft           int     `json:"days_left"`
}

type UserLogin struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Expire   int    `json:"expire"`
}

type UserRegister struct {
	Username         string `json:"username"`
	Password         string `json:"password"`
	InviteCode       string `json:"invite_code"`
	Email            string `json:"email"`
	VerificationCode string `json:"verification_code"`
	Expire           int    `json:"expire"`
}

type UserRegistrationOptions struct {
	OpenRegistration         bool `json:"open_registration"`
	InviteRequired           bool `json:"invite_required"`
	EmailVerificationEnabled bool `json:"email_verification_enabled"`
}

type UserChangePassword struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

type UserChangeUsername struct {
	NewUsername string `json:"new_username"`
}

type UserLoginResponse struct {
	Token    string       `json:"token"`
	ExpireAt string       `json:"expire_at"`
	User     UserResponse `json:"user"`
}

type UserCreateRequest struct {
	Username            string     `json:"username"`
	Password            string     `json:"password"`
	Role                UserRole   `json:"role"`
	Status              UserStatus `json:"status"`
	Balance             float64    `json:"balance"`
	MonthlyLimit        float64    `json:"monthly_limit"`
	MonthlyUsed         float64    `json:"monthly_used"`
	MonthlyExpireAt     int64      `json:"monthly_expire_at"`
	MonthlyResetAt      int64      `json:"monthly_reset_at"`
	DailyLimit          *float64   `json:"daily_limit,omitempty"`
	DailyQuota          *float64   `json:"daily_quota,omitempty"`
	DailyUsed           *float64   `json:"daily_used,omitempty"`
	MonthlyExpireAtISO  string     `json:"monthly_expire_at_iso,omitempty"`
	MonthlyResetAtISO   string     `json:"monthly_reset_at_iso,omitempty"`
	NextResetAtISO      string     `json:"next_reset_at_iso,omitempty"`
	Note                string     `json:"note"`
	AccessPlanIDs       []int      `json:"access_plan_ids,omitempty"`
	DefaultAccessPlanID int        `json:"default_access_plan_id,omitempty"`
}

type UserUpdateRequest struct {
	ID                  int        `json:"id"`
	Username            string     `json:"username"`
	Role                UserRole   `json:"role"`
	Status              UserStatus `json:"status"`
	Balance             float64    `json:"balance"`
	MonthlyLimit        float64    `json:"monthly_limit"`
	MonthlyUsed         float64    `json:"monthly_used"`
	MonthlyExpireAt     int64      `json:"monthly_expire_at"`
	MonthlyResetAt      int64      `json:"monthly_reset_at"`
	DailyLimit          *float64   `json:"daily_limit,omitempty"`
	DailyQuota          *float64   `json:"daily_quota,omitempty"`
	DailyUsed           *float64   `json:"daily_used,omitempty"`
	MonthlyExpireAtISO  string     `json:"monthly_expire_at_iso,omitempty"`
	MonthlyResetAtISO   string     `json:"monthly_reset_at_iso,omitempty"`
	NextResetAtISO      string     `json:"next_reset_at_iso,omitempty"`
	Note                string     `json:"note"`
	AccessPlanIDs       []int      `json:"access_plan_ids,omitempty"`
	DefaultAccessPlanID int        `json:"default_access_plan_id,omitempty"`
}

type UserResetPasswordRequest struct {
	ID       int    `json:"id"`
	Password string `json:"password"`
}

type UserQuotaUpdateRequest struct {
	ID                 int      `json:"id"`
	Balance            float64  `json:"balance"`
	MonthlyLimit       float64  `json:"monthly_limit"`
	MonthlyUsed        float64  `json:"monthly_used"`
	MonthlyExpireAt    int64    `json:"monthly_expire_at"`
	MonthlyResetAt     int64    `json:"monthly_reset_at"`
	DailyLimit         *float64 `json:"daily_limit,omitempty"`
	DailyQuota         *float64 `json:"daily_quota,omitempty"`
	DailyUsed          *float64 `json:"daily_used,omitempty"`
	MonthlyExpireAtISO string   `json:"monthly_expire_at_iso,omitempty"`
	MonthlyResetAtISO  string   `json:"monthly_reset_at_iso,omitempty"`
	NextResetAtISO     string   `json:"next_reset_at_iso,omitempty"`
}

type UserUsageSummary struct {
	UserID         int      `json:"user_id"`
	Username       string   `json:"username"`
	Role           UserRole `json:"role"`
	RequestSuccess int64    `json:"request_success"`
	RequestFailed  int64    `json:"request_failed"`
	InputToken     int64    `json:"input_token"`
	OutputToken    int64    `json:"output_token"`
	TotalCost      float64  `json:"total_cost"`
}

func (u *User) HashPassword() error {
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(u.Password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("failed to hash password: %w", err)
	}
	u.Password = string(hashedPassword)
	return nil
}

func (u *User) ComparePassword(password string) error {
	return bcrypt.CompareHashAndPassword([]byte(u.Password), []byte(password))
}

func (u *User) Normalize() {
	if u.Role == "" {
		u.Role = UserRoleUser
	}
	if u.Status == "" {
		u.Status = UserStatusActive
	}
}

func (u User) IsAdmin() bool {
	return u.Role == UserRoleAdmin
}

func (u User) IsActive() bool {
	return u.Status == UserStatusActive
}

func (u User) MonthlyQuotaSummary(now time.Time) UserMonthlyQuotaSummary {
	limit := nonNegativeFloat(u.MonthlyLimit)
	used := nonNegativeFloat(u.MonthlyUsed)
	active := limit > 0 && u.MonthlyExpireAt > now.Unix()
	remaining := 0.0
	if active {
		remaining = nonNegativeFloat(limit - used)
	}
	nextResetAt := u.MonthlyResetAt
	if active {
		nextDailyReset := nextDailyQuotaReset(now).Unix()
		if nextResetAt <= now.Unix() || nextResetAt > nextDailyReset {
			nextResetAt = nextDailyReset
		}
	}

	status := "inactive"
	if limit > 0 {
		status = "expired"
	}
	if active {
		status = "active"
		if remaining <= 0 {
			status = "exhausted"
		}
	}

	return UserMonthlyQuotaSummary{
		DailyLimit:         limit,
		DailyUsed:          used,
		DailyRemaining:     remaining,
		MonthlyActive:      active,
		MonthlyStatus:      status,
		MonthlyExpireAtISO: unixToISO(u.MonthlyExpireAt),
		MonthlyResetAtISO:  unixToISO(nextResetAt),
		NextResetAtISO:     unixToISO(nextResetAt),
		DaysLeft:           daysLeft(u.MonthlyExpireAt, now),
	}
}

func NewUserResponse(user User) UserResponse {
	return NewUserResponseAt(user, time.Now())
}

func NewUserResponseAt(user User, now time.Time) UserResponse {
	summary := user.MonthlyQuotaSummary(now)
	return UserResponse{
		User:               user,
		DailyLimit:         summary.DailyLimit,
		DailyUsed:          summary.DailyUsed,
		DailyRemaining:     summary.DailyRemaining,
		MonthlyActive:      summary.MonthlyActive,
		MonthlyStatus:      summary.MonthlyStatus,
		MonthlyExpireAtISO: summary.MonthlyExpireAtISO,
		MonthlyResetAtISO:  summary.MonthlyResetAtISO,
		NextResetAtISO:     summary.NextResetAtISO,
		DaysLeft:           summary.DaysLeft,
	}
}

func NewUserResponses(users []User) []UserResponse {
	responses := make([]UserResponse, 0, len(users))
	now := time.Now()
	for _, user := range users {
		responses = append(responses, NewUserResponseAt(user, now))
	}
	return responses
}

func unixToISO(value int64) string {
	if value <= 0 {
		return ""
	}
	return time.Unix(value, 0).UTC().Format(time.RFC3339)
}

func daysLeft(expireAt int64, now time.Time) int {
	if expireAt <= now.Unix() {
		return 0
	}
	return int(math.Ceil(time.Unix(expireAt, 0).Sub(now).Hours() / 24))
}

func nextDailyQuotaReset(now time.Time) time.Time {
	return time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
}

func nonNegativeFloat(value float64) float64 {
	if value < 0 {
		return 0
	}
	return value
}
