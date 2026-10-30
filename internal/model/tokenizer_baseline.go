package model

import "time"

// TokenizerBaseline 是管理员在**可信渠道**上录制的分词器差分计数基线。
//
// 用途：分词器差分探针（modelverify/tokenbank）的内置库来自 290 个开放分词器，
// 厂商没公开分词器的型号（Claude / Grok 等）在库里是 class=null，只能判"库外=相容"。
// 在自己确认可信的渠道上跑一次「录制分词器基线」检测，把那次检测得到的 72 条差分
// 计数存成这一行，之后声明该型号的渠道就能按精确吻合来核对。
//
// 计数只能来自一次真实完成的检测结果（SourceResultID），不接受手填数字。
type TokenizerBaseline struct {
	ID int64 `json:"id" gorm:"primaryKey;autoIncrement"`
	// Model 归一后的型号名（去厂商前缀、:tag、转小写），唯一。
	Model string `json:"model" gorm:"size:255;uniqueIndex"`
	// Label 展示名。
	Label string `json:"label" gorm:"size:255;default:''"`
	// Counts 探测文本 id → 差分计数的 JSON。
	Counts string `json:"counts" gorm:"type:text"`
	// BankGeneratedAt 录制时所用分词器库的 generated_at；库更新后该基线自动失效（需重录）。
	BankGeneratedAt string `json:"bank_generated_at" gorm:"size:64;default:''"`
	// 来源：哪个渠道、哪条检测结果。渠道改名/删除后仍可追溯。
	SourceChannelID   int       `json:"source_channel_id" gorm:"default:0"`
	SourceChannelName string    `json:"source_channel_name" gorm:"size:255;default:''"`
	SourceResultID    int64     `json:"source_result_id" gorm:"index;default:0"`
	CreatedAt         time.Time `json:"created_at"`
}

// TableName 固定表名。
func (TokenizerBaseline) TableName() string { return "tokenizer_baselines" }
