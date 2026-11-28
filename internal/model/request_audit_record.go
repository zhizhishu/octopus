package model

import "time"

// RequestAuditRecord 每笔请求风控审查落一条（next-1129 第 8 件）。
// 字段照 gpt-load internal/jev/observation.go:5-24 的 Observation 适配裁剪：
// reason/provider/group/credential 这套维度在 octopus 里分别对应
// Verdict/固定 decisions 通道/无分组/渠道内 key 轮换（不逐 key 记账），故省略；
// tokens 与费用维度保留，成本口径见 CostState。
//
// 费用拍板（用户 02:56）：审查是平台风控开销，平台自担、绝不扣下游 key——
// 审查链路本身不走下游计费（hook 里无任何下游扣费调用），本表只记账供「花费」块查询。
type RequestAuditRecord struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	CreatedAt time.Time `gorm:"index" json:"created_at"`

	APIKeyID   int    `gorm:"index" json:"api_key_id"`  // 被审查的下游 key
	ChannelID  int    `json:"channel_id"`             // 审查用的决策渠道
	AuditModel string `json:"audit_model"`            // upstream_model：decisions 模型
	DurationMs int64  `json:"duration_ms"`
	Called     bool   `json:"called"` // false=指纹缓存命中，未真打上游

	InputTokens int64   `json:"input_tokens"` // 估算 len(sample)/4，与 relay 估算同口径
	Verdict     string  `gorm:"index" json:"verdict"` // pass / block / unaudited（fail-open）
	RulesHit    string  `json:"rules_hit,omitempty"`  // 命中规则 id 逗号串
	MaxScore    float64 `json:"max_score,omitempty"`
	CostState   string  `json:"cost_state"` // 固定 "platform"：平台自担
	Reason      string  `json:"reason,omitempty"`

	// SampleText 只在拦截时保存取样原文原样（拦截记录块要查）；pass/unaudited 不存，
	// 免得全量复制用户请求体。原文原样、不打码（用户 11-28 拍板）。
	SampleText string `gorm:"type:text" json:"sample_text,omitempty"`
}

func (RequestAuditRecord) TableName() string {
	return "request_audit_records"
}
