package batchimage

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
)

// Provider 是"一个能跑离线批处理生图的上游"的全部能力。
//
// 【界面为什么这么窄】只有五件事：交出去、问进度、撤单、拿结果、清残留。
// 别的一律不做——不让 provider 碰数据库、不让它算钱、不让它决定路由。
// 这样每个 provider 都能用一个假服务器完整测出来，也不用为了测它去连真上游。
type Provider interface {
	// Name 返回 model.BatchImageProvider* 里的一个值。
	Name() string

	// Submit 把一批请求交给上游，返回"以后怎么找它"。
	// 上层保证：调用时 status 是 queued，且这一单已经预扣过钱了。
	Submit(ctx context.Context, request SubmitRequest) (*SubmitResult, error)

	// Poll 问一次进度。上层保证：调用时 status 是 submitted。
	Poll(ctx context.Context, request PollRequest) (*PollResult, error)

	// Cancel 尽力撤单。**不保证成功**：上游可能已经开始跑了。
	// 返回 nil 只代表"请求发出去了"，不代表上游已经停下。
	Cancel(ctx context.Context, request CancelRequest) error

	// OpenResult 打开结果对象，调用方从中逐行读。
	// 上层保证：调用时上游已经报完成。
	OpenResult(ctx context.Context, request ResultRequest) (io.ReadCloser, error)

	// Cleanup 清理我们在上游留下的东西（输入文件等）。尽力而为。
	Cleanup(ctx context.Context, request CleanupRequest) error
}

// ChannelContext 是 provider 干活需要的渠道信息（**凭证已经解出来了**）。
//
// 【为什么把密钥放在这里而不是让 provider 自己去查库】provider 不该碰数据库：
// 一旦它能自己取密钥，就没法用一个纯内存的假渠道测它，也没法保证"取密钥"这件事
// 只有一条路径（那条路径上有健康检查、key 轮换、熔断）。
type ChannelContext struct {
	ChannelID    int
	ChannelKeyID int
	// BaseURL 是渠道配置的上游地址（空 = 用该 provider 的默认地址）。
	BaseURL string
	// APIKey 是渠道密钥。对 gemini_api 是 API key；对 Vertex 是服务账号 JSON 原文。
	APIKey string
	// ProjectID / Location / GCSBucket / GCSPrefix 只有 Vertex 用得上。
	ProjectID string
	Location  string
	GCSBucket string
	GCSPrefix string
	// Headers 是渠道自定义请求头（照抄渠道配置，供需要伪装的场景用）。
	Headers map[string]string
	// HTTPTimeout 是整个上游调用的超时（含上传/下载）。
	HTTPTimeout time.Duration
}

// SubmitRequest 是一次提交。
type SubmitRequest struct {
	Channel ChannelContext
	// BatchID 是我们的批号，用来生成幂等的上游对象名（重启重放不会造出两份输入）。
	BatchID string
	// Model 是**上游**模型名（已过 model_mapping）。
	Model string
	// DisplayName 是给上游看的批次名（用我们的批号，便于双向对照）。
	DisplayName string
	// JSONL 是已经构建好的输入内容。
	JSONL []byte
	// Items 与 JSONL 同序，provider 需要它来知道条目数与 key。
	Items []SubmitItem
}

// SubmitResult 是提交的结果，全部字段都会被写回 job 行。
type SubmitResult struct {
	// ProviderJobName 是上游批次句柄（后面 Poll/Cancel 都靠它）。
	ProviderJobName string
	// ProviderInputRef 是我们在上游留下的输入对象引用（用于清理/排查）。
	ProviderInputRef string
	// ProviderOutputRef 是结果对象引用（部分上游提交时就给，部分要等完成）。
	ProviderOutputRef string
	// RawState 是上游此刻的状态串，仅用于展示。
	RawState string
	// InternalState 是归一化后的状态，决定 status 怎么推进。
	InternalState InternalState
}

// PollRequest 是一次进度查询。
type PollRequest struct {
	Channel ChannelContext
	BatchID string
	Model   string
	// JobName / OutputRef 是提交时存下来的上游引用。
	JobName   string
	OutputRef string
}

// PollResult 是一次进度查询的结果。
type PollResult struct {
	RawState      string
	InternalState InternalState
	OutputRef     string
	// ErrorCode / ErrorMessage 只在 InternalState 是失败/过期时给。
	ErrorCode    string
	ErrorMessage string
}

// CancelRequest 是一次撤单。
type CancelRequest struct {
	Channel ChannelContext
	BatchID string
	Model   string
	JobName string
}

// ResultRequest 是打开结果对象。
type ResultRequest struct {
	Channel   ChannelContext
	BatchID   string
	Model     string
	OutputRef string
}

// CleanupRequest 是清理上游残留。
type CleanupRequest struct {
	Channel  ChannelContext
	BatchID  string
	InputRef string
	// DeleteInput / DeleteOutput 说明要删什么。两个都 false 时 provider 应直接返回。
	DeleteInput  bool
	DeleteOutput bool
}

// InternalState 是**归一化后**的上游状态。
//
// 【为什么要有这一层】两家上游的状态串完全不同（Gemini 的 JOB_STATE_* 与 Vertex 的
// 作业状态），而"接下来该谁动"只有四种答案。上层只认这四种，新增一个上游时
// 只需把它的状态串映射过来，推进逻辑一行都不用改。
type InternalState string

const (
	// InternalStateQueued 上游收了但还没开始跑。
	InternalStateQueued InternalState = "queued"
	// InternalStateRunning 上游正在跑。
	InternalStateRunning InternalState = "running"
	// InternalStateSucceeded 跑完了，结果可取。
	InternalStateSucceeded InternalState = "succeeded"
	// InternalStateFailed 上游侧失败了。
	InternalStateFailed InternalState = "failed"
	// InternalStateCancelled 上游侧已撤单。
	InternalStateCancelled InternalState = "cancelled"
	// InternalStateExpired 上游侧过期了（保留期到了）。
	InternalStateExpired InternalState = "expired"
)

// IsTerminal 报告这个归一化状态是否已经结束。
func (s InternalState) IsTerminal() bool {
	switch s {
	case InternalStateSucceeded, InternalStateFailed, InternalStateCancelled, InternalStateExpired:
		return true
	}
	return false
}

// ErrProviderNotConfigured 表示渠道缺少该上游必需的配置。
//
// 【为什么要单独一个错误】"渠道没配好"和"上游挂了"对运维是两件完全不同的事：
// 前者要人去改配置，后者要人去看上游。混成一个错误会让人一直去查上游。
var ErrProviderNotConfigured = errors.New("batch image provider is not configured for this channel")

// ErrProviderUnsupportedModel 表示渠道没有声明承接这个模型的批处理。
var ErrProviderUnsupportedModel = errors.New("batch image model is not enabled on this channel")

// ProviderFor 按渠道配置造出对应的 provider。
//
// 返回错误而不是 nil provider：nil 会被一路传下去，最后在某个远端调用处
// 变成 panic 或者一句毫无线索的空指针错误。
func ProviderFor(channel *model.Channel) (Provider, error) {
	if channel == nil {
		return nil, ErrProviderNotConfigured
	}
	provider := channel.BatchImageProvider()
	switch provider {
	case model.BatchImageProviderGeminiAPI:
		return newGeminiProvider(), nil
	case model.BatchImageProviderVertex:
		return newVertexProvider(), nil
	case "":
		return nil, ErrProviderNotConfigured
	default:
		return nil, ErrProviderNotConfigured
	}
}
