package op

import (
	"context"
	"errors"
	"fmt"

	"github.com/zhizhishu/octopus/internal/model"
)

// 系统 Key（model.APIKey.System）是迁移 032 自动建的内置 Key，只给进程内部的
// 渠道测试 / 模型检测走正常 relay 链路用。它不是用户资产：
//   - 外部鉴权对它恒拒绝（middleware.ValidateAPIKey），所以明文泄露面必须最小——
//     列表/详情接口一律不返回它的明文；
//   - 删不得、停不得、改不得：下面的 errSystemKey* 在单个与批量两条路径上统一拦
//     （批量接口逐条委派给单个实现，这里守住单个实现就守住了批量）。
var (
	errSystemKeyDelete  = errors.New("系统 Key 不能删除：它是进程内部转发专用")
	errSystemKeyDisable = errors.New("系统 Key 不能停用：它是进程内部转发专用")
	errSystemKeyUpdate  = errors.New("系统 Key 不能编辑：它是进程内部转发专用")
)

// systemKeyRedactedSecret 是系统 Key 在对外响应里代替明文的固定占位。
// 不是可用的密钥、不含任何真实字符；前端本来就把 Key 遮住显示，复制按钮复制到它
// 也只是一段无效占位，不会泄出真身。
const systemKeyRedactedSecret = "sk-internal-redacted"

// redactSystemKeySecret 就地清空系统 Key 的明文（用于对外响应）。普通 Key 不动——
// 它们的明文本来就是对用户可见的（复制 Key 是既有功能）。
func redactSystemKeySecret(key *model.APIKey) {
	if key != nil && key.System {
		key.APIKey = systemKeyRedactedSecret
	}
}

// SystemAPIKeyGet 返回内置系统 Key（含明文）。仅供进程内部使用——
// 渠道测试 / 模型检测的 relay 发送器靠它把转发记到系统 Key 名下。
//
// 只读缓存、不落日志、不出 API。找不到时返回明确错误：迁移 032 在启动时建行，
// 正常运行的实例里它必然存在；真没有（极老库直拉新二进制+迁移失败之类）就让
// 调用方失败闭嘴，绝不退而使用任何用户 Key。
func SystemAPIKeyGet(ctx context.Context) (model.APIKey, error) {
	for _, key := range apiKeyCache.GetAll() {
		if key.System {
			return key, nil
		}
	}
	return model.APIKey{}, fmt.Errorf("system API key not found (migration 032 should have created it at startup)")
}
