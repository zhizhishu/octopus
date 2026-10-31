package op

import (
	"context"
	"errors"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
)

// 统一任务接口（/v1/tasks）的只读查询。三张任务表各自已有「按 id 取」的函数，
// 这里只补「按归属列出」：视频、异步图片、Midjourney。
//
// 归属规则与各自的查询端点**逐字一致**，不另起一套：
//   - 视频 / Midjourney：relay.videoTaskOwnerMatches —— 记录了 token 的任务只认同一把 key，
//     没记录 token（存量行）才按用户认；两者都没记录的行（0/0）谁的 Key 都看不到——
//     by-id 查询只放行管理员控制台会话（relay.taskOwnerMatchesRequest），列表里一律不出现。
//   - 异步图片：op.ImageTaskGetForOwner —— token 与用户两个条件同时满足；已过期的不可见。
//
// 【红线】与 VideoTaskListByOwner 一样，归属缺失直接报错，绝不退化成全表。

// ErrTaskOwnerScopeRequired 表示调用方没有给出任何身份。
var ErrTaskOwnerScopeRequired = errors.New("task owner scope is required")

// TaskOwnerScope 是调用者身份：发起请求用的 key 与它的用户。
type TaskOwnerScope struct {
	TokenID int
	UserID  int
}

func (s TaskOwnerScope) valid() bool { return s.TokenID > 0 || s.UserID > 0 }

// tokenOrLegacyUserScope = videoTaskOwnerMatches 的 SQL 版本（不含 0/0 放行）。
func tokenOrLegacyUserScope(q *gorm.DB, s TaskOwnerScope) *gorm.DB {
	switch {
	case s.TokenID > 0 && s.UserID > 0:
		return q.Where("(owner_token_id = ?) OR (owner_token_id = 0 AND owner_user_id = ?)", s.TokenID, s.UserID)
	case s.TokenID > 0:
		return q.Where("owner_token_id = ?", s.TokenID)
	default:
		return q.Where("owner_token_id = 0 AND owner_user_id = ?", s.UserID)
	}
}

// TaskListWindow 是一次列表要从每张表取的前 N 行（按创建时间倒序）。统一列表在内存里
// 归并三张表，所以每张表都要取到 offset+page_size 为止；上限由调用方控制。
type TaskListWindow struct {
	Limit int
}

// VideoTaskListOwned 按归属列出视频任务（created_at DESC, id DESC），返回前 Limit 行与总数。
// statuses 为空表示不限。
func VideoTaskListOwned(ctx context.Context, s TaskOwnerScope, statuses []model.VideoTaskStatus, w TaskListWindow) ([]model.VideoTask, int64, error) {
	if !s.valid() {
		return nil, 0, ErrTaskOwnerScopeRequired
	}
	scoped := func() *gorm.DB {
		q := tokenOrLegacyUserScope(db.GetDB().WithContext(ctx).Model(&model.VideoTask{}), s)
		if len(statuses) > 0 {
			q = q.Where("status IN ?", statuses)
		}
		return q
	}
	var total int64
	if err := scoped().Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []model.VideoTask
	if w.Limit > 0 {
		if err := scoped().Order("created_at DESC").Order("id DESC").Limit(w.Limit).Find(&rows).Error; err != nil {
			return nil, 0, err
		}
	}
	return rows, total, nil
}

// ImageTaskListOwned 按归属列出未过期的异步图片任务。
func ImageTaskListOwned(ctx context.Context, s TaskOwnerScope, statuses []string, now int64, w TaskListWindow) ([]model.ImageTask, int64, error) {
	if !s.valid() {
		return nil, 0, ErrTaskOwnerScopeRequired
	}
	scoped := func() *gorm.DB {
		q := db.GetDB().WithContext(ctx).Model(&model.ImageTask{})
		if s.TokenID > 0 {
			q = q.Where("owner_token_id = ?", s.TokenID)
		}
		if s.UserID > 0 {
			q = q.Where("owner_user_id = ?", s.UserID)
		}
		q = q.Where("(expires_at = 0 OR expires_at > ?)", now)
		if len(statuses) > 0 {
			q = q.Where("status IN ?", statuses)
		}
		return q
	}
	var total int64
	if err := scoped().Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []model.ImageTask
	if w.Limit > 0 {
		if err := scoped().Order("created_at DESC").Order("id DESC").Limit(w.Limit).Find(&rows).Error; err != nil {
			return nil, 0, err
		}
	}
	return rows, total, nil
}

// MidjourneyTaskListOwned 按归属列出 Midjourney 任务归属行（状态在上游，见 relay 层）。
func MidjourneyTaskListOwned(ctx context.Context, s TaskOwnerScope, w TaskListWindow) ([]model.MidjourneyTask, int64, error) {
	if !s.valid() {
		return nil, 0, ErrTaskOwnerScopeRequired
	}
	scoped := func() *gorm.DB {
		return tokenOrLegacyUserScope(db.GetDB().WithContext(ctx).Model(&model.MidjourneyTask{}), s)
	}
	var total int64
	if err := scoped().Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []model.MidjourneyTask
	if w.Limit > 0 {
		if err := scoped().Order("created_at DESC").Order("id DESC").Limit(w.Limit).Find(&rows).Error; err != nil {
			return nil, 0, err
		}
	}
	return rows, total, nil
}
