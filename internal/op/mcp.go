package op

import (
	"context"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
)

type MCPUsage struct {
	Calls  int64   `json:"calls"`
	Images int64   `json:"images"`
	Cost   float64 `json:"cost"`
	Failed int64   `json:"failed"`
}

type MCPKeyUsage struct {
	APIKeyID int    `json:"api_key_id"`
	Name     string `json:"name"`
	MCPUsage
}

type MCPStats struct {
	Totals  MCPUsage      `json:"totals"`
	TopKeys []MCPKeyUsage `json:"top_keys"`
}

func MCPStatsGet(ctx context.Context, user model.User, days int) (MCPStats, error) {
	result := MCPStats{TopKeys: []MCPKeyUsage{}}
	query := db.GetDB().WithContext(ctx).Model(&model.RelayLog{}).
		Where("request_source = ? AND time >= ?", "mcp", time.Now().AddDate(0, 0, -days).Unix())
	if !user.IsAdmin() {
		query = query.Where("user_id = ?", user.ID)
	}
	metrics := "COUNT(*) AS calls, COALESCE(SUM(image_count), 0) AS images, " +
		"COALESCE(SUM(CASE WHEN error = '' THEN COALESCE(final_input_cost, 0) + COALESCE(final_output_cost, 0) + COALESCE(final_cache_read_cost, 0) + COALESCE(final_cache_write_cost, 0) ELSE 0 END), 0) AS cost, " +
		"COALESCE(SUM(CASE WHEN error <> '' THEN 1 ELSE 0 END), 0) AS failed"
	if err := query.Session(&gorm.Session{}).Select(metrics).Scan(&result.Totals).Error; err != nil {
		return result, err
	}
	err := query.Session(&gorm.Session{}).Select("api_key_id, MAX(request_api_key_name) AS name, " + metrics).
		Group("api_key_id").Order("calls DESC, api_key_id ASC").Limit(10).Scan(&result.TopKeys).Error
	return result, err
}
