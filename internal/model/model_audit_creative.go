package model

import "time"

// ModelAuditCreative 是「鹈鹕测试」的一张图：同一道创作题发给几个模型，各自交一份
// SVG/HTML，并排看谁画得像样。
//
// 它不是检测：不打分、不进风险结论，只给人看。所以单独一张表，不挤进
// model_audit_results（那张表的每一行都会被当成一次结论来统计）。
//
// 保留：默认 7 天后自动删（ModelAuditCreativePurge），收藏（Pinned）的不删。
//
// 跨库注意（参考 fix/db-mysql-pg）：要建索引的字符串列一律带 size，MySQL 不能给
// TEXT 建索引；大段内容用 size:262144——MySQL 上是 mediumtext（text 只有 64KB，
// 一张带动画的 SVG 很容易超），Postgres 上是 varchar(262144)，SQLite 上是 text。
// 这些列不带默认值：MySQL 不接受 TEXT 列上的字面量默认值。题目原文（上限 8KB，代码里截）
// 和出错原因用 type:text，免得几列 varchar 一起撑爆 MySQL 单行 65535 字节的上限。
type ModelAuditCreative struct {
	ID int64 `json:"id" gorm:"primaryKey;autoIncrement"`

	// RoundID 一轮的编号（同一次「一起出图」的几张图共用）。
	RoundID string `json:"round_id" gorm:"size:40;index;not null"`

	ChannelID   int    `json:"channel_id" gorm:"index;default:0"`
	ChannelName string `json:"channel_name" gorm:"size:255;default:''"`
	Model       string `json:"model" gorm:"size:255;default:''"`

	// Kind 题目种类：pelican / html / custom。
	Kind string `json:"kind" gorm:"size:16;default:'pelican'"`
	// Prompt 实际发出去的题目原文。
	Prompt string `json:"prompt" gorm:"type:text"`

	// Format 从回复里认出来的格式：svg / html；没认出来为空。
	Format string `json:"format" gorm:"size:8;default:''"`
	// Content 提取出来、要放进隔离框显示的 SVG/HTML（上限 CreativeMaxContentBytes）。
	Content string `json:"content" gorm:"size:262144"`
	// Raw 回复原文（截断到同一上限），没认出图或出错时给人看原文。
	Raw string `json:"raw" gorm:"size:262144"`

	// Status 见 Creative* 常量。
	Status string `json:"status" gorm:"size:16;index;default:'pending'"`
	// Error 没出成图的原因（超时、上游报错、没返回合法 SVG）。
	Error string `json:"error" gorm:"type:text"`

	DurationMs   int64 `json:"duration_ms" gorm:"default:0"`
	InputTokens  int   `json:"input_tokens" gorm:"default:0"`
	OutputTokens int   `json:"output_tokens" gorm:"default:0"`

	// Pinned 收藏：收藏的不会被 7 天自动清理、也不会被「清空未收藏」删掉。
	Pinned bool `json:"pinned" gorm:"index;default:false"`

	CreatedAt  time.Time  `json:"created_at" gorm:"index"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// 鹈鹕测试单张图的状态。
const (
	CreativePending = "pending"
	CreativeRunning = "running"
	CreativeDone    = "done"
	// CreativeInvalid 回复了，但里面找不到 SVG/HTML。
	CreativeInvalid = "invalid"
	CreativeFailed  = "failed"
	CreativeTimeout = "timeout"
)

// CreativeMaxContentBytes 单张图内容（以及原文）的上限：256KB。
const CreativeMaxContentBytes = 256 * 1024

func (ModelAuditCreative) TableName() string { return "model_audit_creatives" }
