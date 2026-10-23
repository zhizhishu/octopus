package model

import "github.com/zhizhishu/octopus/internal/transformer/outbound"

type LLMPrice struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`

	// PerCall 是**每次调用**的单价（按次计费，不按 token）。
	//
	// 为什么需要它：视频生成（/v1/videos）上游按"生成一条片子"收费，响应体里
	// 根本没有任何 token 用量（只有 task_id），所以按 Input/Output 单价计费的结果
	// 恒为 0 —— 这是本项目最贵的端点，却是唯一完全免费的：余额不动、月卡不动、
	// 单 key 的 MaxCost 也不动，持任意 key 可无限刷。
	//
	// 语义与默认值：
	//   - 0 = **不启用按次计费**（默认）。存量模型的价格行不会因此改变账单。
	//   - > 0 = 每成功调用一次收这么多（单位与 Input/Output 一致：每百万 token 的
	//     计价基准，这里是"每次"的绝对金额，由管理员按上游成本自行填写）。
	//
	// 只对真正"一次交付一个成品"的端点生效（目前是视频创建成功），
	// 不参与 LLM 聊天的 token 计费。
	PerCall float64 `json:"per_call"`
}

// IsZero reports whether the price carries no billable rate at all. A zero LLMInfo row is a
// placeholder (created when a model is added before it had a catalog price), not a deliberate
// free price, so callers use this to fall through to the models.dev catalog instead of
// billing the model at 0 forever.
func (p LLMPrice) IsZero() bool {
	return p.Input == 0 && p.Output == 0 && p.CacheRead == 0 && p.CacheWrite == 0 && p.PerCall == 0
}

type LLMInfo struct {
	Name string `json:"name" gorm:"primaryKey;not null"`
	LLMPrice
}

type LLMChannel struct {
	Name        string                `json:"name"`
	Enabled     bool                  `json:"enabled"`
	ChannelID   int                   `json:"channel_id"`
	ChannelName string                `json:"channel_name"`
	ChannelType outbound.OutboundType `json:"channel_type"`
}

type GeminiModel struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
}

type GeminiModelList struct {
	Models        []GeminiModel `json:"models"`
	NextPageToken string        `json:"nextPageToken"`
}

type OpenAIModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int    `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type OpenAIModelList struct {
	Object string        `json:"object"`
	Data   []OpenAIModel `json:"data"`
}
type AnthropicModel struct {
	ID          string `json:"id"`
	CreatedAt   string `json:"created_at"`
	DisplayName string `json:"display_name"`
	Type        string `json:"type"`
}

type AnthropicModelList struct {
	Data    []AnthropicModel `json:"data"`
	FirstID string           `json:"first_id"`
	HasMore bool             `json:"has_more"`
	LastID  string           `json:"last_id"`
}
