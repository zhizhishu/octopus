package op

import (
	"context"
	"errors"

	"github.com/zhizhishu/octopus/internal/db"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
)

// ErrPromptRuleStoreUnavailable 表示 DB 尚未就绪时读规则表的失败：由 relay 侧
// loadPromptRulesOrNone 记日志后当作「本请求没有规则」继续转发，不给客户端报错。
var ErrPromptRuleStoreUnavailable = errors.New("prompt rule store unavailable")

// PromptRuleList 返回提示词规则表全量（内部小表，直达 DB，不走缓存）。
// relay 注入与提示词页预览共用这份读取；排序交给 prompt.Evaluate。
// DB 错误（含表缺失、未迁移）原样上抛给调用方；relay 侧按已定行为记日志后
// 当作「没有规则」继续转发，不升级成客户端错误，也绝不退回旧四层字段。
func PromptRuleList(ctx context.Context) ([]dbmodel.PromptRule, error) {
	d := db.GetDB()
	if d == nil {
		return nil, ErrPromptRuleStoreUnavailable
	}
	rules := make([]dbmodel.PromptRule, 0)
	if err := d.WithContext(ctx).Find(&rules).Error; err != nil {
		return nil, err
	}
	return rules, nil
}
