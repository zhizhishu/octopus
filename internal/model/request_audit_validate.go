package model

import (
	"github.com/zhizhishu/octopus/internal/relay/requestaudit"
)

// ValidateRequestAuditConfig 把 setting 校验委托给 requestaudit.ParseConfig。
// 分层说明：requestaudit 包零依赖（纯逻辑），model 反向 import 它不构成循环；
// 配置形状是 relay 层语义，不该在 model 里复制第二份校验。
func ValidateRequestAuditConfig(raw string) error {
	_, err := requestaudit.ParseConfig(raw)
	return err
}
