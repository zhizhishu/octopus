package model

// MidjourneyTask 记录一条 Midjourney（midjourney-proxy 协议）任务归属于哪个渠道、
// 哪把 key、哪个调用方。
//
// 为什么要落库：MJ 是「提交 → 拿 task id → 之后再 fetch / 放大 / 变换」的异步协议，
// 后续每一步都**必须**回到创建它的那个上游账号（任务只存在于那里），而且后续动作
// 可能在几小时后、进程重启之后才来。只存在内存里，一次重启就让所有在途任务的
// U1/V1 变成「task_not_found」。
//
// 与 new-api 的 midjourneys 表不同，这里**只存归属、不存任务状态**：octopus 的
// fetch 是把请求原样转给归属渠道（见 relay.MidjourneyTaskFetchHandler），状态以上游
// 为准，不需要后台轮询器去同步一份本地副本。
//
// 【铁律】与 VideoTask 相同：本表绝不存渠道密钥，只存 ChannelKeyID 引用。
type MidjourneyTask struct {
	ID        int64 `json:"id" gorm:"primaryKey;autoIncrement"`
	CreatedAt int64 `json:"created_at" gorm:"index"`

	// UpstreamTaskID 是上游 submit 响应里的 result（mj-proxy 的任务 id）。
	UpstreamTaskID string `json:"upstream_task_id" gorm:"size:128;index;default:''"`

	ChannelID    int `json:"channel_id" gorm:"default:0"`
	ChannelKeyID int `json:"channel_key_id" gorm:"default:0"`

	// Model 是计费/路由用的动作模型名（mj_imagine、mj_upscale、swap_face……），
	// 经过渠道 ModelMapping 之后的实际值。
	Model string `json:"model" gorm:"size:255;default:''"`
	// Action 是 mj-proxy 的动作名（IMAGINE / UPSCALE / ...），仅供排查。
	Action string `json:"action" gorm:"size:32;default:''"`

	// OwnerTokenID / OwnerUserID：只有创建者能 fetch / 继续操作这条任务。
	OwnerTokenID int `json:"owner_token_id" gorm:"index;default:0"`
	OwnerUserID  int `json:"owner_user_id" gorm:"index;default:0"`
}

// TableName 固定表名，避免 GORM 复数化规则将来变动导致迁移与运行时错位。
func (MidjourneyTask) TableName() string { return "midjourney_tasks" }
