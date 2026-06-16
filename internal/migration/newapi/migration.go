package newapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/glebarez/sqlite"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

const (
	DefaultQuotaPerUnit = 500_000
	defaultBatchSize    = 500
	defaultAPIKeyPrefix = "sk-octopus-newapi-"

	newAPILogTypeConsume = 2
	newAPILogTypeError   = 5

	conflictSkip   = "skip"
	conflictMerge  = "merge"
	conflictRename = "rename"

	passwordPreserve = "preserve"
	passwordRandom   = "random"
	passwordDisabled = "disabled"
)

type Config struct {
	SourceDB          *gorm.DB
	SourceLogDB       *gorm.DB
	TargetDB          *gorm.DB
	Apply             bool
	IncludeLogs       bool
	IncludeAPIKeys    bool
	QuotaPerUnit      float64
	BatchSize         int
	ConflictStrategy  string
	PasswordMode      string
	APIKeyPrefix      string
	PreserveAdminRole bool
	Now               time.Time
	Progress          func(ProgressUpdate)
}

type ProgressUpdate struct {
	Stage   string `json:"stage"`
	Percent int    `json:"percent"`
	Message string `json:"message"`
}

type Summary struct {
	DryRun               bool     `json:"dry_run"`
	QuotaPerUnit         float64  `json:"quota_per_unit"`
	SourceUsers          int64    `json:"source_users"`
	ActiveUsers          int      `json:"active_users"`
	InactiveUsersSkipped int64    `json:"inactive_users_skipped"`
	UsersCreated         int      `json:"users_created"`
	UsersMerged          int      `json:"users_merged"`
	UsersRenamed         int      `json:"users_renamed"`
	UsersSkippedConflict int      `json:"users_skipped_conflict"`
	UsersSkippedInvalid  int      `json:"users_skipped_invalid"`
	APIKeysConsidered    int64    `json:"api_keys_considered"`
	APIKeysCreated       int      `json:"api_keys_created"`
	APIKeysSkipped       int      `json:"api_keys_skipped"`
	LogsConsidered       int64    `json:"logs_considered"`
	LogsCreated          int      `json:"logs_created"`
	LogsSkipped          int      `json:"logs_skipped"`
	StatsUpdated         bool     `json:"stats_updated"`
	ImportedBalance      float64  `json:"imported_balance"`
	ImportedUsedQuota    float64  `json:"imported_used_quota"`
	ImportedLogCost      float64  `json:"imported_log_cost"`
	Warnings             []string `json:"warnings,omitempty"`
	AppliedAt            string   `json:"applied_at,omitempty"`
	SourceReference      string   `json:"source_reference"`
	ImportedAPIKeyPrefix string   `json:"imported_api_key_prefix,omitempty"`
	PasswordMode         string   `json:"password_mode"`
	ConflictStrategy     string   `json:"conflict_strategy"`
	PreserveAdminRole    bool     `json:"preserve_admin_role"`
	IncludedLogs         bool     `json:"included_logs"`
	IncludedAPIKeys      bool     `json:"included_api_keys"`
}

type sourceUser struct {
	ID           int    `gorm:"column:id"`
	Username     string `gorm:"column:username"`
	Password     string `gorm:"column:password"`
	DisplayName  string `gorm:"column:display_name"`
	Role         int    `gorm:"column:role"`
	Status       int    `gorm:"column:status"`
	Email        string `gorm:"column:email"`
	Quota        int    `gorm:"column:quota"`
	UsedQuota    int    `gorm:"column:used_quota"`
	RequestCount int    `gorm:"column:request_count"`
	Group        string `gorm:"column:group"`
	Remark       string `gorm:"column:remark"`
	CreatedAt    int64  `gorm:"column:created_at"`
	LastLoginAt  int64  `gorm:"column:last_login_at"`
}

func (sourceUser) TableName() string {
	return "users"
}

type sourceLog struct {
	ID                int    `gorm:"column:id"`
	UserID            int    `gorm:"column:user_id"`
	CreatedAt         int64  `gorm:"column:created_at"`
	Type              int    `gorm:"column:type"`
	Content           string `gorm:"column:content"`
	Username          string `gorm:"column:username"`
	TokenName         string `gorm:"column:token_name"`
	ModelName         string `gorm:"column:model_name"`
	Quota             int    `gorm:"column:quota"`
	PromptTokens      int    `gorm:"column:prompt_tokens"`
	CompletionTokens  int    `gorm:"column:completion_tokens"`
	UseTime           int    `gorm:"column:use_time"`
	ChannelID         int    `gorm:"column:channel_id"`
	TokenID           int    `gorm:"column:token_id"`
	Group             string `gorm:"column:group"`
	IP                string `gorm:"column:ip"`
	RequestID         string `gorm:"column:request_id"`
	UpstreamRequestID string `gorm:"column:upstream_request_id"`
	Other             string `gorm:"column:other"`
}

func (sourceLog) TableName() string {
	return "logs"
}

type sourceToken struct {
	ID                 int    `gorm:"column:id"`
	UserID             int    `gorm:"column:user_id"`
	Key                string `gorm:"column:key"`
	Status             int    `gorm:"column:status"`
	Name               string `gorm:"column:name"`
	CreatedTime        int64  `gorm:"column:created_time"`
	AccessedTime       int64  `gorm:"column:accessed_time"`
	ExpiredTime        int64  `gorm:"column:expired_time"`
	RemainQuota        int    `gorm:"column:remain_quota"`
	UnlimitedQuota     bool   `gorm:"column:unlimited_quota"`
	ModelLimitsEnabled bool   `gorm:"column:model_limits_enabled"`
	ModelLimits        string `gorm:"column:model_limits"`
	UsedQuota          int    `gorm:"column:used_quota"`
	Group              string `gorm:"column:group"`
}

func (sourceToken) TableName() string {
	return "tokens"
}

type sourceOption struct {
	Key   string `gorm:"column:key;primaryKey"`
	Value string `gorm:"column:value"`
}

func (sourceOption) TableName() string {
	return "options"
}

type usageStats struct {
	count            int
	quota            int
	usedTokens       int
	lastAt           int64
	lastIP           string
	firstIP          string
	firstAt          int64
	requestCount     int
	promptTokens     int
	completionTokens int
}

type userPlan struct {
	source     sourceUser
	username   string
	targetID   int
	action     string
	skipReason string
	usage      usageStats
}

type targetIndex struct {
	usersByName map[string]model.User
	apiKeys     map[string]model.APIKey
}

type importAccumulators struct {
	total     model.StatsMetrics
	daily     map[string]model.StatsMetrics
	hourly    map[int]model.StatsMetrics
	apiKey    map[int]model.StatsMetrics
	today     string
	logCost   float64
	usedQuota float64
	balance   float64
}

func OpenDatabase(dbType, dsn string, debug bool) (*gorm.DB, func() error, error) {
	dbType = normalizeDBType(dbType)
	if strings.TrimSpace(dsn) == "" {
		return nil, nil, fmt.Errorf("database dsn is required")
	}
	cfg := &gorm.Config{Logger: logger.Discard}
	if debug {
		cfg.Logger = logger.Default.LogMode(logger.Info)
	}

	var (
		conn *gorm.DB
		err  error
	)
	switch dbType {
	case "sqlite":
		conn, err = gorm.Open(sqlite.Open(sqliteDSN(dsn)), cfg)
	case "mysql":
		conn, err = gorm.Open(mysql.Open(dsn), cfg)
	case "postgres":
		conn, err = gorm.Open(postgres.Open(dsn), cfg)
	default:
		return nil, nil, fmt.Errorf("unsupported database type %q", dbType)
	}
	if err != nil {
		return nil, nil, err
	}
	closeFn := func() error {
		sqlDB, err := conn.DB()
		if err != nil {
			return err
		}
		return sqlDB.Close()
	}
	return conn, closeFn, nil
}

func Run(ctx context.Context, cfg Config) (*Summary, error) {
	requestedLogs := cfg.IncludeLogs
	requestedAPIKeys := cfg.IncludeAPIKeys
	cfg = normalizeConfig(cfg)
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if cfg.SourceDB == nil {
		return nil, fmt.Errorf("source database is required")
	}
	if cfg.SourceLogDB == nil {
		cfg.SourceLogDB = cfg.SourceDB
	}
	if cfg.Apply && cfg.TargetDB == nil {
		return nil, fmt.Errorf("target database is required in apply mode")
	}

	if err := requireSourceTables(cfg.SourceDB, cfg.SourceLogDB); err != nil {
		return nil, err
	}

	reportProgress(cfg, "scan_users", 8, "checking source users and quota settings")
	quotaPerUnit, quotaSource, err := resolveQuotaPerUnit(ctx, cfg.SourceDB, cfg.QuotaPerUnit)
	if err != nil {
		return nil, err
	}
	cfg.QuotaPerUnit = quotaPerUnit

	summary := &Summary{
		DryRun:               !cfg.Apply,
		QuotaPerUnit:         quotaPerUnit,
		SourceReference:      quotaSource,
		ImportedAPIKeyPrefix: cfg.APIKeyPrefix,
		PasswordMode:         cfg.PasswordMode,
		ConflictStrategy:     cfg.ConflictStrategy,
		PreserveAdminRole:    cfg.PreserveAdminRole,
		IncludedLogs:         cfg.IncludeLogs,
		IncludedAPIKeys:      cfg.IncludeAPIKeys,
	}
	if cfg.Apply {
		summary.AppliedAt = cfg.Now.UTC().Format(time.RFC3339)
	}

	if err := cfg.SourceDB.WithContext(ctx).Model(&sourceUser{}).Count(&summary.SourceUsers).Error; err != nil {
		return nil, fmt.Errorf("count source users: %w", err)
	}

	reportProgress(cfg, "scan_logs", 24, "scanning New API usage logs for active users")
	activeUsage, err := collectActiveUsage(ctx, cfg.SourceLogDB, cfg.BatchSize)
	if err != nil {
		return nil, err
	}
	activeIDs := sortedUserIDs(activeUsage)
	summary.ActiveUsers = len(activeIDs)
	summary.InactiveUsersSkipped = summary.SourceUsers - int64(summary.ActiveUsers)
	if summary.InactiveUsersSkipped < 0 {
		summary.InactiveUsersSkipped = 0
	}

	users, err := loadUsers(ctx, cfg.SourceDB, activeIDs)
	if err != nil {
		return nil, err
	}

	index, err := loadTargetIndex(ctx, cfg.TargetDB)
	if err != nil {
		return nil, err
	}

	plans := buildUserPlans(users, activeUsage, index, cfg, summary)
	for _, plan := range plans {
		if plan.action == "skip" {
			continue
		}
		summary.ImportedBalance += quotaToCost(plan.source.Quota, cfg.QuotaPerUnit)
		summary.ImportedUsedQuota += quotaToCost(plan.source.UsedQuota, cfg.QuotaPerUnit)
	}

	mappedSourceUserIDs := mappedPlanSourceIDs(plans)
	if cfg.IncludeAPIKeys {
		n, err := countTokens(ctx, cfg.SourceDB, mappedSourceUserIDs)
		if err != nil {
			return nil, err
		}
		summary.APIKeysConsidered = n
	}
	if cfg.IncludeLogs {
		n, err := countLogs(ctx, cfg.SourceLogDB, mappedSourceUserIDs)
		if err != nil {
			return nil, err
		}
		summary.LogsConsidered = n
	}

	if !cfg.Apply {
		reportProgress(cfg, "dry_run", 82, "building dry-run report")
		for _, plan := range plans {
			if plan.action == "create" {
				summary.UsersCreated++
			}
		}
		summary.Warnings = append(summary.Warnings, "dry-run only; target database was not modified")
		appendSummaryOnlyWarnings(summary, requestedLogs, requestedAPIKeys)
		reportProgress(cfg, "complete", 100, "dry-run report completed")
		return summary, nil
	}

	reportProgress(cfg, "import", 70, "importing users, balances, and usage summaries")
	err = cfg.TargetDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		sourceToTargetUser, err := createUsers(ctx, tx, plans, cfg, summary)
		if err != nil {
			return err
		}
		_ = sourceToTargetUser

		sourceTokenToTargetKey := map[int]int{}
		if cfg.IncludeAPIKeys {
			if err := createAPIKeys(ctx, tx, cfg.SourceDB, sourceToTargetUser, index, sourceTokenToTargetKey, cfg, summary); err != nil {
				return err
			}
		}

		if cfg.IncludeLogs {
			acc := newImportAccumulators(cfg.Now)
			if err := createRelayLogs(ctx, tx, cfg.SourceLogDB, sourceToTargetUser, sourceTokenToTargetKey, cfg, summary, acc); err != nil {
				return err
			}
			if summary.LogsCreated > 0 {
				if err := applyStats(ctx, tx, acc); err != nil {
					return err
				}
				summary.StatsUpdated = true
				summary.ImportedLogCost = acc.logCost
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	appendSummaryOnlyWarnings(summary, requestedLogs, requestedAPIKeys)
	reportProgress(cfg, "complete", 100, "migration completed")
	return summary, nil
}

func appendSummaryOnlyWarnings(summary *Summary, requestedLogs bool, requestedAPIKeys bool) {
	summary.Warnings = append(summary.Warnings, "summary-only migration: imported users keep balance and usage summary notes; detailed New API logs are not copied into Octopus relay logs")
	summary.Warnings = append(summary.Warnings, "summary-only migration: New API tokens/API keys are not imported; migrated users must create fresh Octopus API keys")
	if requestedLogs {
		summary.Warnings = append(summary.Warnings, "include_logs was requested but ignored by the summary-only migration policy")
	}
	if requestedAPIKeys {
		summary.Warnings = append(summary.Warnings, "include_api_keys was requested but ignored by the summary-only migration policy")
	}
}

func reportProgress(cfg Config, stage string, percent int, message string) {
	if cfg.Progress == nil {
		return
	}
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	cfg.Progress(ProgressUpdate{
		Stage:   stage,
		Percent: percent,
		Message: message,
	})
}

func normalizeConfig(cfg Config) Config {
	cfg.IncludeLogs = false
	cfg.IncludeAPIKeys = false
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = defaultBatchSize
	}
	if cfg.QuotaPerUnit <= 0 || math.IsNaN(cfg.QuotaPerUnit) || math.IsInf(cfg.QuotaPerUnit, 0) {
		cfg.QuotaPerUnit = 0
	}
	cfg.ConflictStrategy = strings.ToLower(strings.TrimSpace(cfg.ConflictStrategy))
	if cfg.ConflictStrategy == "" {
		cfg.ConflictStrategy = conflictSkip
	}
	cfg.PasswordMode = strings.ToLower(strings.TrimSpace(cfg.PasswordMode))
	if cfg.PasswordMode == "" {
		cfg.PasswordMode = passwordPreserve
	}
	cfg.APIKeyPrefix = strings.TrimSpace(cfg.APIKeyPrefix)
	if cfg.APIKeyPrefix == "" {
		cfg.APIKeyPrefix = defaultAPIKeyPrefix
	}
	if cfg.Now.IsZero() {
		cfg.Now = time.Now()
	}
	return cfg
}

func validateConfig(cfg Config) error {
	switch cfg.ConflictStrategy {
	case conflictSkip, conflictMerge, conflictRename:
	default:
		return fmt.Errorf("unsupported conflict strategy %q", cfg.ConflictStrategy)
	}
	switch cfg.PasswordMode {
	case passwordPreserve, passwordRandom, passwordDisabled:
	default:
		return fmt.Errorf("unsupported password mode %q", cfg.PasswordMode)
	}
	return nil
}

func requireSourceTables(sourceDB, logDB *gorm.DB) error {
	if !sourceDB.Migrator().HasTable("users") {
		return fmt.Errorf("source database does not contain users table")
	}
	if !logDB.Migrator().HasTable("logs") {
		return fmt.Errorf("source log database does not contain logs table")
	}
	return nil
}

func resolveQuotaPerUnit(ctx context.Context, sourceDB *gorm.DB, override float64) (float64, string, error) {
	if override > 0 && !math.IsNaN(override) && !math.IsInf(override, 0) {
		return override, "flag", nil
	}
	if sourceDB.Migrator().HasTable("options") {
		var option sourceOption
		err := sourceDB.WithContext(ctx).First(&option, "key = ?", "QuotaPerUnit").Error
		if err == nil {
			if parsed, parseErr := strconv.ParseFloat(strings.TrimSpace(option.Value), 64); parseErr == nil && parsed > 0 {
				return parsed, "new-api options.QuotaPerUnit", nil
			}
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, "", fmt.Errorf("read source QuotaPerUnit: %w", err)
		}
	}
	return DefaultQuotaPerUnit, "default 500000", nil
}

func collectActiveUsage(ctx context.Context, logDB *gorm.DB, batchSize int) (map[int]usageStats, error) {
	result := make(map[int]usageStats)
	var batch []sourceLog
	query := logDB.WithContext(ctx).
		Model(&sourceLog{}).
		Where("type = ?", newAPILogTypeConsume).
		Where("(quota <> 0 OR prompt_tokens <> 0 OR completion_tokens <> 0 OR model_name <> '')")

	err := query.FindInBatches(&batch, batchSize, func(tx *gorm.DB, batchNo int) error {
		for _, row := range batch {
			stats := result[row.UserID]
			stats.count++
			stats.quota += row.Quota
			stats.promptTokens += row.PromptTokens
			stats.completionTokens += row.CompletionTokens
			stats.usedTokens += row.PromptTokens + row.CompletionTokens
			stats.requestCount++
			if row.IP != "" && stats.firstIP == "" {
				stats.firstIP = truncate(row.IP, 128)
			}
			if stats.firstAt == 0 || (row.CreatedAt > 0 && row.CreatedAt < stats.firstAt) {
				stats.firstAt = row.CreatedAt
			}
			if row.CreatedAt >= stats.lastAt {
				stats.lastAt = row.CreatedAt
				stats.lastIP = truncate(row.IP, 128)
			}
			result[row.UserID] = stats
		}
		return nil
	}).Error
	if err != nil {
		return nil, fmt.Errorf("collect active New API usage: %w", err)
	}
	return result, nil
}

func loadUsers(ctx context.Context, sourceDB *gorm.DB, ids []int) ([]sourceUser, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var users []sourceUser
	err := sourceDB.WithContext(ctx).
		Where("id IN ?", ids).
		Order("id asc").
		Find(&users).Error
	if err != nil {
		return nil, fmt.Errorf("load source users: %w", err)
	}
	return users, nil
}

func loadTargetIndex(ctx context.Context, targetDB *gorm.DB) (targetIndex, error) {
	index := targetIndex{
		usersByName: map[string]model.User{},
		apiKeys:     map[string]model.APIKey{},
	}
	if targetDB == nil {
		return index, nil
	}
	if !targetDB.Migrator().HasTable(&model.User{}) {
		return index, nil
	}
	var users []model.User
	if err := targetDB.WithContext(ctx).Find(&users).Error; err != nil {
		return index, fmt.Errorf("load target users: %w", err)
	}
	for _, user := range users {
		index.usersByName[normalizeName(user.Username)] = user
	}
	var keys []model.APIKey
	if targetDB.Migrator().HasTable(&model.APIKey{}) {
		if err := targetDB.WithContext(ctx).Find(&keys).Error; err != nil {
			return index, fmt.Errorf("load target api keys: %w", err)
		}
		for _, key := range keys {
			index.apiKeys[key.APIKey] = key
		}
	}
	return index, nil
}

func buildUserPlans(users []sourceUser, activeUsage map[int]usageStats, index targetIndex, cfg Config, summary *Summary) []userPlan {
	plans := make([]userPlan, 0, len(users))
	for _, src := range users {
		username := strings.TrimSpace(src.Username)
		if username == "" {
			username = fmt.Sprintf("newapi_user_%d", src.ID)
		}
		plan := userPlan{
			source:   src,
			username: username,
			usage:    activeUsage[src.ID],
			action:   "create",
		}
		normalized := normalizeName(username)
		if existing, ok := index.usersByName[normalized]; ok {
			switch cfg.ConflictStrategy {
			case conflictMerge:
				plan.action = "merge"
				plan.targetID = existing.ID
				summary.UsersMerged++
			case conflictRename:
				plan.action = "create"
				plan.username = nextRenamedUsername(username, src.ID, index.usersByName)
				index.usersByName[normalizeName(plan.username)] = model.User{Username: plan.username}
				summary.UsersRenamed++
			default:
				plan.action = "skip"
				plan.skipReason = "username conflict"
				summary.UsersSkippedConflict++
			}
		} else {
			index.usersByName[normalized] = model.User{Username: username}
		}
		if plan.action != "skip" && strings.TrimSpace(plan.username) == "" {
			plan.action = "skip"
			plan.skipReason = "invalid username"
			summary.UsersSkippedInvalid++
		}
		plans = append(plans, plan)
	}
	return plans
}

func createUsers(ctx context.Context, tx *gorm.DB, plans []userPlan, cfg Config, summary *Summary) (map[int]int, error) {
	sourceToTarget := make(map[int]int, len(plans))
	for i := range plans {
		plan := &plans[i]
		if plan.action == "skip" {
			continue
		}
		if plan.action == "merge" {
			sourceToTarget[plan.source.ID] = plan.targetID
			continue
		}
		password, err := mapPassword(plan.source.Password, cfg.PasswordMode)
		if err != nil {
			return nil, fmt.Errorf("map password for New API user %d: %w", plan.source.ID, err)
		}
		user := model.User{
			Username:        plan.username,
			Password:        password,
			Role:            mapRole(plan.source.Role, cfg.PreserveAdminRole),
			Status:          mapStatus(plan.source.Status, cfg.PasswordMode),
			Balance:         roundCost(quotaToCost(plan.source.Quota, cfg.QuotaPerUnit)),
			RegisterIP:      truncate(plan.usage.firstIP, 128),
			LastRelayIP:     truncate(plan.usage.lastIP, 128),
			LastRelayAt:     plan.usage.lastAt,
			Note:            migrationNote(plan.source, plan.usage, cfg),
			CreatedAt:       nonZeroUnix(plan.source.CreatedAt, cfg.Now.Unix()),
			UpdatedAt:       cfg.Now.Unix(),
			MonthlyLimit:    0,
			MonthlyUsed:     0,
			MonthlyExpireAt: 0,
			MonthlyResetAt:  0,
		}
		if err := tx.WithContext(ctx).Create(&user).Error; err != nil {
			return nil, fmt.Errorf("create target user for New API user %d: %w", plan.source.ID, err)
		}
		sourceToTarget[plan.source.ID] = user.ID
		plan.targetID = user.ID
		summary.UsersCreated++
	}
	return sourceToTarget, nil
}

func createAPIKeys(ctx context.Context, tx *gorm.DB, sourceDB *gorm.DB, sourceToTarget map[int]int, index targetIndex, sourceTokenToTargetKey map[int]int, cfg Config, summary *Summary) error {
	if len(sourceToTarget) == 0 || !sourceDB.Migrator().HasTable("tokens") {
		return nil
	}
	var batch []sourceToken
	ids := sortedMapKeys(sourceToTarget)
	err := sourceDB.WithContext(ctx).
		Model(&sourceToken{}).
		Where("user_id IN ?", ids).
		Order("id asc").
		FindInBatches(&batch, cfg.BatchSize, func(batchTx *gorm.DB, batchNo int) error {
			for _, token := range batch {
				targetUserID, ok := sourceToTarget[token.UserID]
				if !ok {
					summary.APIKeysSkipped++
					continue
				}
				apiKeyValue := importedAPIKeyValue(token.Key, cfg.APIKeyPrefix)
				if apiKeyValue == "" {
					summary.APIKeysSkipped++
					continue
				}
				if existing, exists := index.apiKeys[apiKeyValue]; exists {
					if existing.UserID == targetUserID {
						sourceTokenToTargetKey[token.ID] = existing.ID
					}
					summary.APIKeysSkipped++
					continue
				}
				apiKey := model.APIKey{
					UserID:          targetUserID,
					Name:            tokenName(token),
					APIKey:          apiKeyValue,
					Enabled:         tokenEnabled(token, cfg.Now),
					ExpireAt:        tokenExpireAt(token),
					MaxCost:         tokenMaxCost(token, cfg.QuotaPerUnit),
					SupportedModels: tokenSupportedModels(token),
				}
				if err := tx.WithContext(ctx).Create(&apiKey).Error; err != nil {
					return fmt.Errorf("create target api key for New API token %d: %w", token.ID, err)
				}
				index.apiKeys[apiKey.APIKey] = apiKey
				sourceTokenToTargetKey[token.ID] = apiKey.ID
				summary.APIKeysCreated++
			}
			return nil
		}).Error
	if err != nil {
		return fmt.Errorf("load source tokens: %w", err)
	}
	return nil
}

func createRelayLogs(ctx context.Context, tx *gorm.DB, logDB *gorm.DB, sourceToTarget map[int]int, sourceTokenToTargetKey map[int]int, cfg Config, summary *Summary, acc *importAccumulators) error {
	if len(sourceToTarget) == 0 {
		return nil
	}
	ids := sortedMapKeys(sourceToTarget)

	var batch []sourceLog
	err := logDB.WithContext(ctx).
		Model(&sourceLog{}).
		Where("user_id IN ? AND type IN ?", ids, []int{newAPILogTypeConsume, newAPILogTypeError}).
		Order("id asc").
		FindInBatches(&batch, cfg.BatchSize, func(batchTx *gorm.DB, batchNo int) error {
			existingIDs, err := existingRelayLogIDsForBatch(ctx, tx, batch)
			if err != nil {
				return err
			}
			rows := make([]model.RelayLog, 0, len(batch))
			for _, row := range batch {
				targetUserID, ok := sourceToTarget[row.UserID]
				if !ok {
					summary.LogsSkipped++
					continue
				}
				relayID := importedRelayLogID(row.ID)
				if _, exists := existingIDs[relayID]; exists {
					summary.LogsSkipped++
					continue
				}
				relayLog := mapRelayLog(row, targetUserID, sourceTokenToTargetKey[row.TokenID], cfg)
				rows = append(rows, relayLog)
				existingIDs[relayID] = struct{}{}
			}
			if len(rows) == 0 {
				return nil
			}
			if err := tx.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&rows).Error; err != nil {
				return fmt.Errorf("create relay logs: %w", err)
			}
			for _, row := range rows {
				summary.LogsCreated++
				acc.addRelayLog(row)
			}
			return nil
		}).Error
	if err != nil {
		return fmt.Errorf("load source logs: %w", err)
	}
	return nil
}

func existingRelayLogIDsForBatch(ctx context.Context, tx *gorm.DB, batch []sourceLog) (map[int64]struct{}, error) {
	result := make(map[int64]struct{})
	if len(batch) == 0 {
		return result, nil
	}
	ids := make([]int64, 0, len(batch))
	for _, row := range batch {
		ids = append(ids, importedRelayLogID(row.ID))
	}
	var existing []int64
	if err := tx.WithContext(ctx).Model(&model.RelayLog{}).Where("id IN ?", ids).Pluck("id", &existing).Error; err != nil {
		return nil, fmt.Errorf("load existing imported relay log ids: %w", err)
	}
	for _, id := range existing {
		result[id] = struct{}{}
	}
	return result, nil
}

func applyStats(ctx context.Context, tx *gorm.DB, acc *importAccumulators) error {
	if err := addStatsTotal(ctx, tx, acc.total); err != nil {
		return err
	}
	for date, metrics := range acc.daily {
		if err := addStatsDaily(ctx, tx, date, metrics); err != nil {
			return err
		}
	}
	for hour, metrics := range acc.hourly {
		if err := addStatsHourly(ctx, tx, hour, acc.today, metrics); err != nil {
			return err
		}
	}
	for apiKeyID, metrics := range acc.apiKey {
		if err := addStatsAPIKey(ctx, tx, apiKeyID, metrics); err != nil {
			return err
		}
	}
	return nil
}

func addStatsTotal(ctx context.Context, tx *gorm.DB, metrics model.StatsMetrics) error {
	var existing model.StatsTotal
	err := tx.WithContext(ctx).First(&existing, "id = ?", 1).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		existing = model.StatsTotal{ID: 1, StatsMetrics: metrics}
		return tx.WithContext(ctx).Create(&existing).Error
	}
	if err != nil {
		return err
	}
	existing.StatsMetrics.Add(metrics)
	return tx.WithContext(ctx).Save(&existing).Error
}

func addStatsDaily(ctx context.Context, tx *gorm.DB, date string, metrics model.StatsMetrics) error {
	var existing model.StatsDaily
	err := tx.WithContext(ctx).First(&existing, "date = ?", date).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		existing = model.StatsDaily{Date: date, StatsMetrics: metrics}
		return tx.WithContext(ctx).Create(&existing).Error
	}
	if err != nil {
		return err
	}
	existing.StatsMetrics.Add(metrics)
	return tx.WithContext(ctx).Save(&existing).Error
}

func addStatsHourly(ctx context.Context, tx *gorm.DB, hour int, date string, metrics model.StatsMetrics) error {
	var existing model.StatsHourly
	err := tx.WithContext(ctx).First(&existing, "hour = ?", hour).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		existing = model.StatsHourly{Hour: hour, Date: date, StatsMetrics: metrics}
		return tx.WithContext(ctx).Create(&existing).Error
	}
	if err != nil {
		return err
	}
	if existing.Date != date {
		existing.Date = date
		existing.StatsMetrics = model.StatsMetrics{}
	}
	existing.StatsMetrics.Add(metrics)
	return tx.WithContext(ctx).Save(&existing).Error
}

func addStatsAPIKey(ctx context.Context, tx *gorm.DB, apiKeyID int, metrics model.StatsMetrics) error {
	if apiKeyID <= 0 {
		return nil
	}
	var existing model.StatsAPIKey
	err := tx.WithContext(ctx).First(&existing, "api_key_id = ?", apiKeyID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		existing = model.StatsAPIKey{APIKeyID: apiKeyID, StatsMetrics: metrics}
		return tx.WithContext(ctx).Create(&existing).Error
	}
	if err != nil {
		return err
	}
	existing.StatsMetrics.Add(metrics)
	return tx.WithContext(ctx).Save(&existing).Error
}

func newImportAccumulators(now time.Time) *importAccumulators {
	return &importAccumulators{
		daily:  map[string]model.StatsMetrics{},
		hourly: map[int]model.StatsMetrics{},
		apiKey: map[int]model.StatsMetrics{},
		today:  now.Local().Format("20060102"),
	}
}

func (acc *importAccumulators) addRelayLog(relayLog model.RelayLog) {
	metrics := relayLogMetrics(relayLog)
	acc.total.Add(metrics)
	date := time.Unix(relayLog.Time, 0).Local().Format("20060102")
	daily := acc.daily[date]
	daily.Add(metrics)
	acc.daily[date] = daily
	if date == acc.today {
		hour := time.Unix(relayLog.Time, 0).Local().Hour()
		hourly := acc.hourly[hour]
		hourly.Add(metrics)
		acc.hourly[hour] = hourly
	}
	if relayLog.APIKeyID > 0 {
		apiKey := acc.apiKey[relayLog.APIKeyID]
		apiKey.Add(metrics)
		acc.apiKey[relayLog.APIKeyID] = apiKey
	}
	acc.logCost += relayLog.Cost
}

func relayLogMetrics(relayLog model.RelayLog) model.StatsMetrics {
	metrics := model.StatsMetrics{
		InputToken:      int64(relayLog.InputTokens),
		OutputToken:     int64(relayLog.OutputTokens),
		OutputCost:      relayLog.Cost,
		WaitTime:        int64(relayLog.UseTime),
		CacheHitToken:   int64(relayLog.CacheHitTokens),
		CacheWriteToken: int64(relayLog.CacheWriteTokens),
		CacheInputToken: int64(relayLog.CacheInputTokens),
	}
	if strings.TrimSpace(relayLog.Error) == "" {
		metrics.RequestSuccess = 1
	} else {
		metrics.RequestFailed = 1
	}
	return metrics
}

func mapRelayLog(row sourceLog, targetUserID int, targetAPIKeyID int, cfg Config) model.RelayLog {
	cost := roundCost(quotaToCost(row.Quota, cfg.QuotaPerUnit))
	requestModel := strings.TrimSpace(row.ModelName)
	if requestModel == "" {
		requestModel = "unknown"
	}
	relayLog := model.RelayLog{
		ID:                importedRelayLogID(row.ID),
		UserID:            targetUserID,
		APIKeyID:          targetAPIKeyID,
		RequestIP:         truncate(row.IP, 128),
		Time:              nonZeroUnix(row.CreatedAt, cfg.Now.Unix()),
		RequestModelName:  requestModel,
		RequestAPIKeyName: strings.TrimSpace(row.TokenName),
		ChannelId:         row.ChannelID,
		ActualModelName:   requestModel,
		InputTokens:       row.PromptTokens,
		OutputTokens:      row.CompletionTokens,
		UseTime:           row.UseTime * 1000,
		Cost:              cost,
		RequestContent:    row.Content,
		AccessPlanSlug:    strings.TrimSpace(row.Group),
		AccessPlanName:    strings.TrimSpace(row.Group),
		BillingModel:      requestModel,
		FinalOutputCost:   cost,
		FinalMultiplier:   1,
		DefaultMultiplier: 1,
		ModelMultiplier:   1,
		TotalAttempts:     1,
	}
	if row.Type == newAPILogTypeError {
		relayLog.Error = strings.TrimSpace(row.Content)
		relayLog.ErrorCode = "newapi_imported_error"
		relayLog.ErrorStatus = 0
		relayLog.ErrorStrategy = "imported"
		relayLog.Cost = 0
		relayLog.FinalOutputCost = 0
	} else {
		relayLog.Attempts = []model.ChannelAttempt{{
			ChannelID:  row.ChannelID,
			ModelName:  requestModel,
			AttemptNum: 1,
			Status:     model.AttemptSuccess,
			Duration:   row.UseTime * 1000,
		}}
	}
	return relayLog
}

func countTokens(ctx context.Context, db *gorm.DB, userIDs []int) (int64, error) {
	if len(userIDs) == 0 || !db.Migrator().HasTable("tokens") {
		return 0, nil
	}
	var count int64
	if err := db.WithContext(ctx).Model(&sourceToken{}).Where("user_id IN ?", userIDs).Count(&count).Error; err != nil {
		return 0, fmt.Errorf("count source tokens: %w", err)
	}
	return count, nil
}

func countLogs(ctx context.Context, db *gorm.DB, userIDs []int) (int64, error) {
	if len(userIDs) == 0 {
		return 0, nil
	}
	var count int64
	if err := db.WithContext(ctx).
		Model(&sourceLog{}).
		Where("user_id IN ? AND type IN ?", userIDs, []int{newAPILogTypeConsume, newAPILogTypeError}).
		Count(&count).Error; err != nil {
		return 0, fmt.Errorf("count source logs: %w", err)
	}
	return count, nil
}

func mapPassword(sourceHash, mode string) (string, error) {
	switch mode {
	case passwordRandom, passwordDisabled:
		return randomBcryptPassword()
	case passwordPreserve:
		if strings.HasPrefix(sourceHash, "$2a$") || strings.HasPrefix(sourceHash, "$2b$") || strings.HasPrefix(sourceHash, "$2y$") {
			return sourceHash, nil
		}
		return randomBcryptPassword()
	default:
		return "", fmt.Errorf("unsupported password mode %q", mode)
	}
}

func randomBcryptPassword() (string, error) {
	secret := make([]byte, 24)
	if _, err := rand.Read(secret); err != nil {
		return "", err
	}
	hashed, err := bcrypt.GenerateFromPassword([]byte(hex.EncodeToString(secret)), bcrypt.DefaultCost)
	return string(hashed), err
}

func mapRole(role int, preserveAdmin bool) model.UserRole {
	if preserveAdmin && role >= 10 {
		return model.UserRoleAdmin
	}
	return model.UserRoleUser
}

func mapStatus(status int, passwordMode string) model.UserStatus {
	if passwordMode == passwordDisabled {
		return model.UserStatusDisabled
	}
	if status == 1 {
		return model.UserStatusActive
	}
	return model.UserStatusDisabled
}

func migrationNote(user sourceUser, usage usageStats, cfg Config) string {
	parts := []string{
		fmt.Sprintf("Migrated from New API user_id=%d", user.ID),
		fmt.Sprintf("group=%s", blankAs(user.Group, "default")),
		fmt.Sprintf("email=%s", blankAs(user.Email, "-")),
		fmt.Sprintf("display_name=%s", blankAs(user.DisplayName, "-")),
		fmt.Sprintf("source_quota=%d", user.Quota),
		fmt.Sprintf("source_used_quota=%d", user.UsedQuota),
		fmt.Sprintf("source_request_count=%d", user.RequestCount),
		fmt.Sprintf("active_usage_logs=%d", usage.count),
		fmt.Sprintf("active_prompt_tokens=%d", usage.promptTokens),
		fmt.Sprintf("active_completion_tokens=%d", usage.completionTokens),
		fmt.Sprintf("active_used_quota=%d", usage.quota),
		fmt.Sprintf("first_relay_at=%d", usage.firstAt),
		fmt.Sprintf("last_relay_at=%d", usage.lastAt),
		fmt.Sprintf("quota_per_unit=%.0f", cfg.QuotaPerUnit),
		"migration_policy=summary_only_no_api_keys_no_detail_logs",
	}
	if strings.TrimSpace(user.Remark) != "" {
		parts = append(parts, "remark="+strings.TrimSpace(user.Remark))
	}
	return truncate(strings.Join(parts, "; "), 2000)
}

func tokenName(token sourceToken) string {
	name := strings.TrimSpace(token.Name)
	if name == "" {
		name = fmt.Sprintf("New API token %d", token.ID)
	}
	return name
}

func tokenEnabled(token sourceToken, now time.Time) bool {
	if token.Status != 1 {
		return false
	}
	if token.ExpiredTime > 0 && token.ExpiredTime < now.Unix() {
		return false
	}
	if !token.UnlimitedQuota && token.RemainQuota <= 0 {
		return false
	}
	return true
}

func tokenExpireAt(token sourceToken) int64 {
	if token.ExpiredTime <= 0 {
		return 0
	}
	return token.ExpiredTime
}

func tokenMaxCost(token sourceToken, quotaPerUnit float64) float64 {
	if token.UnlimitedQuota {
		return 0
	}
	totalQuota := token.UsedQuota + token.RemainQuota
	if totalQuota <= 0 {
		return 0
	}
	return roundCost(quotaToCost(totalQuota, quotaPerUnit))
}

func tokenSupportedModels(token sourceToken) string {
	if !token.ModelLimitsEnabled {
		return ""
	}
	return strings.TrimSpace(token.ModelLimits)
}

func importedAPIKeyValue(sourceKey, prefix string) string {
	key := strings.TrimSpace(sourceKey)
	if key == "" {
		return ""
	}
	if strings.HasPrefix(key, "sk-octopus-") {
		return key
	}
	key = strings.TrimPrefix(key, "sk-")
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	return prefix + key
}

func importedRelayLogID(sourceID int) int64 {
	if sourceID <= 0 {
		return -1
	}
	return -int64(sourceID)
}

func quotaToCost(quota int, quotaPerUnit float64) float64 {
	if quota <= 0 || quotaPerUnit <= 0 {
		return 0
	}
	return float64(quota) / quotaPerUnit
}

func roundCost(value float64) float64 {
	return math.Round(value*1_000_000) / 1_000_000
}

func sortedUserIDs(values map[int]usageStats) []int {
	ids := make([]int, 0, len(values))
	for id := range values {
		if id > 0 {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	return ids
}

func sortedMapKeys(values map[int]int) []int {
	ids := make([]int, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

func mappedPlanSourceIDs(plans []userPlan) []int {
	ids := make([]int, 0, len(plans))
	for _, plan := range plans {
		if plan.action != "skip" {
			ids = append(ids, plan.source.ID)
		}
	}
	sort.Ints(ids)
	return ids
}

func normalizeName(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func nextRenamedUsername(base string, sourceID int, existing map[string]model.User) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "newapi_user"
	}
	for i := 0; i < 1000; i++ {
		candidate := fmt.Sprintf("%s_newapi_%d", base, sourceID)
		if i > 0 {
			candidate = fmt.Sprintf("%s_newapi_%d_%d", base, sourceID, i)
		}
		if _, ok := existing[normalizeName(candidate)]; !ok {
			return candidate
		}
	}
	return fmt.Sprintf("newapi_user_%d_%d", sourceID, time.Now().Unix())
}

func nonZeroUnix(value, fallback int64) int64 {
	if value > 0 {
		return value
	}
	return fallback
}

func blankAs(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func truncate(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit]
}

func normalizeDBType(dbType string) string {
	switch strings.ToLower(strings.TrimSpace(dbType)) {
	case "", "sqlite", "sqlite3":
		return "sqlite"
	case "mysql", "mariadb":
		return "mysql"
	case "postgres", "postgresql":
		return "postgres"
	default:
		return dbType
	}
}

func sqliteDSN(dsn string) string {
	if strings.Contains(dsn, "?") || strings.HasPrefix(dsn, "file:") {
		return dsn
	}
	clean := filepath.Clean(dsn)
	return clean + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
}
