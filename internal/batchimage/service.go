package batchimage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/utils/log"
)

// 编排层：提交、推进、结算、取图。
//
// 【这一层为什么存在】provider 只知道"怎么跟一家上游说话"，不知道钱、不知道
// 谁的任务、不知道重试。把这三件事集中在 service 里，provider 才能用一个假服务器
// 完整测完，而钱的部分只有一条路径可以走。
//
// 【钱的铁律】提交时预扣、终态时按**实际成功条数**结算、失败路径幂等退款。
// 三个写者（推进器、客户端取消、过期清扫）都可能碰到同一条任务，所以每次动钱
// 之前都必须先抢结算权（op.BatchImageClaimSettle），抢不到就不动。

// 一批任务的处理常量。
const (
	// defaultMaxItems 是单批最多条数。1000 而不是无限：一条 4K 图的成本不低，
	// 一个手滑的脚本提交一万条，用户醒来会看到一张吓人的账单。
	defaultMaxItems = 1000
	// defaultMaxConcurrentJobsPerOwner 是同一用户同时在跑的批次数上限。
	defaultMaxConcurrentJobsPerOwner = 4
	// defaultRetentionHours 是任务记录与上游产物的保留期。
	defaultRetentionHours = 48
	// maxPromptChars 是单条 prompt 的长度上限（按字符，不是字节）。
	maxPromptChars = 8000
	// maxPublicErrorChars 是对外错误消息的截断长度。
	maxPublicErrorChars = 500
)

// Settings 是编排层需要的配置快照。
//
// 传值而不是每次去读 setting：一轮处理里如果开关在跑到一半被关掉，
// 前半轮按开、后半轮按关会让同一条任务的处理依据不一致。
type Settings struct {
	// Enabled 是总开关。关掉时提交一律 404（不是 403：见 async_image_enabled 的先例，
	// 403 会让人以为是自己权限不够而去要权限）。
	Enabled bool
	// MaxItems 是单批条数上限。
	MaxItems int
	// MaxConcurrentJobsPerOwner 是每人并发批次数上限。
	MaxConcurrentJobsPerOwner int
	// RetentionHours 是保留期。
	RetentionHours int
	// UnitPrice 是每条成功图片的价格。0 表示不计费。
	UnitPrice float64
}

// Normalize 补齐零值，保证下游拿到的是一份可用配置。
func (s Settings) Normalize() Settings {
	if s.MaxItems <= 0 {
		s.MaxItems = defaultMaxItems
	}
	if s.MaxConcurrentJobsPerOwner <= 0 {
		s.MaxConcurrentJobsPerOwner = defaultMaxConcurrentJobsPerOwner
	}
	if s.RetentionHours <= 0 {
		s.RetentionHours = defaultRetentionHours
	}
	return s
}

// Service 是编排入口。
type Service struct {
	// Settings 返回当前配置。做成函数是为了每轮取最新值（设置页改完立刻生效），
	// 同时又能在一轮之内保持一致。
	Settings func(ctx context.Context) Settings

	// Resolver 按 id 取渠道与密钥。抽成接口是为了测试能塞一个不碰数据库的实现。
	Resolver ChannelResolver
}

// ChannelResolver 解析出"这条任务该用哪个渠道、哪个 key"。
type ChannelResolver interface {
	// Resolve 返回渠道与该渠道下可用的密钥。currency 里带着已解出的密钥原文。
	Resolve(ctx context.Context, channelID, channelKeyID int) (*model.Channel, *model.ChannelKey, error)
}

// ErrDisabled 表示批处理功能被关掉了。
var ErrDisabled = errors.New("batch image is disabled")

// ErrNotFound 表示任务不存在 / 不属于你 / 已过期（三者对调用方一律 404）。
var ErrNotFound = errors.New("batch image job not found")

// ErrTooManyItems 表示超过单批条数上限。
var ErrTooManyItems = errors.New("batch image job has too many items")

// ErrTooManyActiveJobs 表示并发批次数超限。
var ErrTooManyActiveJobs = errors.New("too many active batch image jobs")

// ErrIdempotencyConflict 表示同一个幂等键配了不同的请求体。
var ErrIdempotencyConflict = errors.New("idempotency key reused with a different request")

// SubmitInput 是一次提交的全部输入（已由 handler 解析完）。
type SubmitInput struct {
	OwnerUserID  int
	OwnerTokenID int
	// Model 是**客户端请求的**模型名（用于校验支持与计量），
	// 真正发给上游的是渠道映射后的名字。
	Model            string
	Items            []SubmitItem
	ResponseMimeType string
	AspectRatio      string
	ImageSize        string
	IdempotencyKey   string
	Metadata         map[string]string
	// Provider 只用于校验（必须是已知值）；选哪个渠道由 ResolveChannel 决定。
	Provider string
}

// Submit 落一条待执行的任务。
//
// 【为什么提交不等上游】上游的一次提交可能要几十秒（上传 JSONL）。
// 放在 HTTP 请求里等，客户端超时重试就会造出两条任务。所以提交只做本地的事：
// 校验、预扣、落库，然后立刻返回，由推进器去交给上游。
func (s *Service) Submit(ctx context.Context, input SubmitInput) (*model.BatchImageJob, error) {
	settings := s.settings(ctx)
	if !settings.Enabled {
		return nil, ErrDisabled
	}

	if input.OwnerUserID <= 0 {
		return nil, fmt.Errorf("batch image: submit without an owner")
	}
	if len(input.Items) == 0 {
		return nil, fmt.Errorf("batch image: submit without items")
	}
	if len(input.Items) > settings.MaxItems {
		return nil, fmt.Errorf("%w: %d items, at most %d allowed", ErrTooManyItems, len(input.Items), settings.MaxItems)
	}
	for index := range input.Items {
		if promptLen := len([]rune(input.Items[index].Prompt)); promptLen > maxPromptChars {
			return nil, fmt.Errorf("batch image: item %q prompt is %d characters, at most %d allowed",
				input.Items[index].CustomID, promptLen, maxPromptChars)
		}
	}

	// 幂等：同一个 key 再来一次，先看有没有旧任务。
	if key := strings.TrimSpace(input.IdempotencyKey); key != "" {
		existing, err := op.BatchImageGetByIdempotencyKey(ctx, input.OwnerUserID, input.OwnerTokenID, key)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			if existing.RequestHash != s.requestHash(input) {
				// 同一个 key 配不同请求体是客户端 bug。默默返回旧任务会让它
				// 以为新请求生效了。
				return nil, ErrIdempotencyConflict
			}
			return existing, nil
		}
	}

	// 选渠：**提交那一刻**选一次，把渠道与 key 钉在任务行上。
	//
	// 为什么不等到 worker 再选：批处理一旦交给某家上游，中途换渠道意味着
	// 批次句柄作废、钱没法算。所以这里选完就固定，渠道后来挂了就如实失败退款。
	selection, err := SelectChannel(ctx, input.Model)
	if err != nil {
		return nil, err
	}
	if provider := strings.TrimSpace(input.Provider); provider != "" && provider != selection.Provider {
		// 客户端声明了 provider 但选中的渠道不是那一家：以渠道为准，但要说清楚。
		// 允许这种情况（客户端只是偏好），不因此拒绝——渠道配置才是权威。
		log.Debugf("batch image: submit asked for provider %q, channel %d uses %q",
			provider, selection.Channel.ID, selection.Provider)
	}

	// 并发上限：按用户算，不按 key 算。一个人开四个 key 也不该能开八条并发批次。
	active, err := op.BatchImageCountActiveForOwner(ctx, input.OwnerUserID)
	if err != nil {
		return nil, err
	}
	if int(active) >= settings.MaxConcurrentJobsPerOwner {
		return nil, fmt.Errorf("%w: %d active, at most %d allowed",
			ErrTooManyActiveJobs, active, settings.MaxConcurrentJobsPerOwner)
	}

	// 预扣：按"全部条目都成功"占一笔额度。终态时按实际成功数结算，多退。
	//
	// 按上限扣而不是按预估扣，是因为预估不出来（一条 prompt 出几张图只有上游知道）。
	// 按上限扣的代价是提交时手上得有这笔钱；换来的是永远不会超支。
	holdAmount := settings.UnitPrice * float64(countOutputs(input.Items))
	if holdAmount > 0 {
		reserved, err := op.UserCanRelay(input.OwnerUserID, ctx)
		if err != nil {
			return nil, err
		}
		if reserved < holdAmount {
			return nil, fmt.Errorf("batch image: insufficient quota: need %.4f, reserved %.4f", holdAmount, reserved)
		}
		// 预扣走真正的扣费动作（提交即扣），终态再按实际成功数退差额。
		// 这样"用户提交后余额被别处花光"不会导致我们白干。
		if err := op.UserRecordUsage(input.OwnerUserID, holdAmount, ctx); err != nil {
			return nil, fmt.Errorf("batch image: reserve quota: %w", err)
		}
	}

	if err := validateBuildable(input); err != nil {
		// 构建校验失败要把刚扣的钱退回去，否则用户白丢一笔。
		if holdAmount > 0 {
			if refundErr := op.UserRefundUsage(input.OwnerUserID, holdAmount, ctx); refundErr != nil {
				log.Errorf("batch image: refund hold %.4f for user %d after validation failure: %v",
					holdAmount, input.OwnerUserID, refundErr)
			}
		}
		return nil, err
	}

	now := time.Now()
	requestJSON, err := json.Marshal(map[string]any{
		"model":              input.Model,
		"response_mime_type": input.ResponseMimeType,
		"aspect_ratio":       input.AspectRatio,
		"image_size":         input.ImageSize,
		"provider":           selection.Provider,
		"items":              redactItemsForStorage(input.Items),
	})
	if err != nil {
		if holdAmount > 0 {
			_ = op.UserRefundUsage(input.OwnerUserID, holdAmount, ctx)
		}
		return nil, fmt.Errorf("batch image: encode request: %w", err)
	}

	job := &model.BatchImageJob{
		BatchID:          op.BatchImageNewID(),
		Status:           model.BatchImageJobStatusQueued,
		Model:            selection.UpstreamModel,
		RequestModel:     input.Model,
		ChannelID:        selection.Channel.ID,
		ChannelKeyID:     selection.ChannelKey.ID,
		Provider:         selection.Provider,
		OwnerUserID:      input.OwnerUserID,
		OwnerTokenID:     input.OwnerTokenID,
		IdempotencyKey:   strings.TrimSpace(input.IdempotencyKey),
		RequestHash:      s.requestHash(input),
		ItemCount:        len(input.Items),
		ResponseMimeType: input.ResponseMimeType,
		AspectRatio:      input.AspectRatio,
		ImageSize:        input.ImageSize,
		RequestJSON:      string(requestJSON),
		HoldAmount:       holdAmount,
		ExpiresAt:        now.Add(time.Duration(settings.RetentionHours) * time.Hour).Unix(),
		Metadata:         input.Metadata,
	}
	if err := op.BatchImageCreate(ctx, job); err != nil {
		if holdAmount > 0 {
			if refundErr := op.UserRefundUsage(input.OwnerUserID, holdAmount, ctx); refundErr != nil {
				log.Errorf("batch image: refund hold after create failure: %v", refundErr)
			}
		}
		return nil, err
	}

	// 子项行：只存预览与偏移，不存完整 prompt（见 model.BatchImageItem 的注释）。
	items := make([]model.BatchImageItem, 0, len(input.Items))
	for index, item := range input.Items {
		items = append(items, model.BatchImageItem{
			BatchID:           job.BatchID,
			CustomID:          strings.TrimSpace(item.CustomID),
			Status:            model.BatchImageItemStatusPending,
			PromptPreview:     previewOf(item.Prompt),
			RequestHash:       op.BatchImageRequestHash([]byte(item.Prompt)),
			SourceLineNumber:  index + 1,
			ProviderSourceObj: "",
		})
	}
	if err := op.BatchImageItemCreateBatch(ctx, items); err != nil {
		// 子项落不进去这条任务就是废的：标失败并退款，别让它挂在 queued 上被推进器反复捡起来。
		s.failAndRefund(ctx, job, "batch_image_item_create_failed", err)
		return nil, err
	}

	return job, nil
}

// requestHash 算幂等指纹。
//
// 只把**会影响结果**的字段算进去：metadata 这类不影响生图结果的字段不参与，
// 否则客户端换个 trace id 就会被判成请求体变了。
func (s *Service) requestHash(input SubmitInput) string {
	payload := map[string]any{
		"model":              input.Model,
		"response_mime_type": input.ResponseMimeType,
		"aspect_ratio":       input.AspectRatio,
		"image_size":         input.ImageSize,
		"provider":           input.Provider,
		"items":              fingerprintItems(input.Items),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return op.BatchImageRequestHash(raw)
}

// fingerprintItems 把输入条目变成稳定的指纹串。
func fingerprintItems(items []SubmitItem) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		refs := make([]string, 0, len(item.ReferenceImages))
		for _, image := range item.ReferenceImages {
			// 参考图按内容的哈希参与指纹：同一张图换个文件名不算新请求。
			refs = append(refs, image.MimeType+"|"+op.BatchImageRequestHash(append([]byte(nil), image.Data...))+"|"+image.FileURI)
		}
		out = append(out, map[string]any{
			"custom_id": item.CustomID,
			"prompt":    item.Prompt,
			"count":     item.OutputCount,
			"refs":      refs,
		})
	}
	return out
}

// redactItemsForStorage 把条目转成可入库的形状（**不含参考图字节**）。
//
// 参考图可能是几 MB 的 base64。把整批塞进一行 JSON 会让这条记录大到拖慢每次查询，
// 而且我们本来就只在提交那一刻需要它。
func redactItemsForStorage(items []SubmitItem) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, map[string]any{
			"custom_id":     item.CustomID,
			"prompt":        item.Prompt,
			"output_count":  item.OutputCount,
			"ref_image_cnt": len(item.ReferenceImages),
		})
	}
	return out
}

// countOutputs 数这一批最多会出多少张图（用于预扣）。
func countOutputs(items []SubmitItem) int {
	total := 0
	for _, item := range items {
		if item.OutputCount > 0 {
			total += item.OutputCount
			continue
		}
		total++
	}
	return total
}

// previewOf 取 prompt 的前若干字符做预览。
func previewOf(prompt string) string {
	const previewRunes = 80
	runes := []rune(strings.TrimSpace(prompt))
	if len(runes) <= previewRunes {
		return string(runes)
	}
	return string(runes[:previewRunes]) + "…"
}

// validateBuildable 用真正的构建器试跑一次。
//
// 【为什么在这里试构建】客户端能在提交时看到"第 3 条缺 prompt"，而不是等几分钟后
// 看到一条莫名失败。构建器的校验规则只有一份（BuildJSONL），这里复用它而不是
// 另写一套——两套校验迟早会不一致。
func validateBuildable(input SubmitInput) error {
	_, err := BuildJSONL(BuildRequest{
		Items:            input.Items,
		ResponseMimeType: input.ResponseMimeType,
		AspectRatio:      input.AspectRatio,
		ImageSize:        input.ImageSize,
	})
	return err
}

// settings 取配置快照。
func (s *Service) settings(ctx context.Context) Settings {
	if s.Settings == nil {
		return Settings{}.Normalize()
	}
	return s.Settings(ctx).Normalize()
}

// settlementMutex 串行化**本进程内**的结算。
//
// 【为什么除了 CAS 还要一把锁】结算要做三件事：算成功条数、写金额、退差额。
// 它们不是一个原子操作。跨进程靠 settled_at 的 CAS 兜底，同进程内靠这把锁
// 保证不会出现"两个 goroutine 都抢到了权"这种本可避免的竞争。
var settlementMutex sync.Mutex

// Settle 按实际成功条数结算一条已到终态的任务。返回是否真的结算了。
//
// 【绝不在非终态调用】调用方必须先确认上游已经给出终态。提前结算等于按"零成功"
// 退款，而任务其实还在跑——用户拿了退款又拿了图。
func (s *Service) Settle(ctx context.Context, job *model.BatchImageJob) (bool, error) {
	if job == nil {
		return false, fmt.Errorf("batch image: settle nil job")
	}
	if !model.IsTerminalBatchImageJobStatus(job.Status) {
		return false, fmt.Errorf("batch image: settle called on non-terminal status %q", job.Status)
	}

	settlementMutex.Lock()
	defer settlementMutex.Unlock()

	claimed, err := op.BatchImageClaimSettle(ctx, job.ID)
	if err != nil {
		return false, err
	}
	if !claimed {
		// 已经有人结算过了。这不是错误：三个写者撞上是设计内的。
		return false, nil
	}

	success, failed, cancelled, err := op.BatchImageCountItems(ctx, job.BatchID)
	if err != nil {
		// 数不出来就别动钱，把结算权还回去让下一轮重试。
		// 不还的话 settled_at 已经非零，这条任务永远结算不了。
		if restoreErr := op.BatchImageRestoreSettleClaim(ctx, job.ID); restoreErr != nil {
			log.Errorf("batch image: restore settle claim for %s after count failure: %v (original: %v)",
				job.BatchID, restoreErr, err)
		}
		return false, err
	}

	settings := s.settings(ctx)
	// 退单不计费：失败与取消的条目都不该收钱。
	charged := settings.UnitPrice * float64(success)
	// 差额退给用户。holdAmount 是提交时实扣的金额。
	refund := job.HoldAmount - charged
	if refund < 0 {
		// 成功数超过预扣（理论上不该发生，因为预扣按上限算）。
		// 出现时按预扣封顶，不多扣用户的钱——宁可我们少收，不可多收。
		refund = 0
		charged = job.HoldAmount
	}

	if refund > 0 && job.OwnerUserID > 0 {
		if err := op.UserRefundUsage(job.OwnerUserID, refund, ctx); err != nil {
			// 写入与退款的一致性：钱没退成就把结算权还回去，让下一轮重试。
			// 与 ImageTaskRefund 的做法一致（先抢权、再动钱、失败就还权）。
			if restoreErr := op.BatchImageRestoreSettleClaim(ctx, job.ID); restoreErr != nil {
				log.Errorf("batch image: restore settle claim for %s after refund failure: %v (original: %v)",
					job.BatchID, restoreErr, err)
			}
			return false, err
		}
	}

	if err := op.BatchImageWriteSettlement(ctx, job.ID, charged, job.HoldAmount); err != nil {
		return false, err
	}

	if _, err := op.BatchImageStatusCAS(ctx, job.ID, job.Status, job.Status, map[string]any{
		"success_count":   success,
		"fail_count":      failed,
		"cancelled_count": cancelled,
	}); err != nil {
		// 计数写不进去不影响钱的正确性（金额已经落库），记一笔就够了。
		log.Warnf("batch image: write counts for %s failed: %v", job.BatchID, err)
	}
	return true, nil
}

// failAndRefund 把一条任务标失败并全额退款。
//
// 提交路径内部用的兜底：落库失败、子项落不进去这类"任务根本没发出去"的情况。
func (s *Service) failAndRefund(ctx context.Context, job *model.BatchImageJob, code string, cause error) {
	if job == nil {
		return
	}
	message := "batch image job failed"
	if cause != nil {
		message = truncateText(cause.Error(), maxPublicErrorChars)
	}
	if _, err := op.BatchImageStatusCAS(ctx, job.ID, model.BatchImageJobStatusQueued,
		model.BatchImageJobStatusFailed, map[string]any{
			"error_code":    code,
			"error_message": message,
			"completed_at":  time.Now().Unix(),
		}); err != nil {
		log.Errorf("batch image: mark job %s failed: %v", job.BatchID, err)
	}
	job.Status = model.BatchImageJobStatusFailed
	if _, err := s.Settle(ctx, job); err != nil {
		log.Errorf("batch image: settle failed job %s: %v", job.BatchID, err)
	}
}

// PublicError 把内部错误压成可以给客户端看的形状。
//
// 上游消息已经在 provider 里过了一遍脱敏；这里再截断一次，防止一个超长
// HTML 错误页被原样吐给客户端。
func PublicError(err error) (code string, message string) {
	if err == nil {
		return "", ""
	}
	switch {
	case errors.Is(err, ErrDisabled):
		return "BATCH_IMAGE_DISABLED", "batch image is disabled on this deployment"
	case errors.Is(err, ErrNotFound):
		return "BATCH_IMAGE_NOT_FOUND", "batch image job not found"
	case errors.Is(err, ErrTooManyItems):
		return "BATCH_IMAGE_TOO_MANY_ITEMS", truncateText(err.Error(), maxPublicErrorChars)
	case errors.Is(err, ErrTooManyActiveJobs):
		return "BATCH_IMAGE_TOO_MANY_ACTIVE_JOBS", truncateText(err.Error(), maxPublicErrorChars)
	case errors.Is(err, ErrIdempotencyConflict):
		return "BATCH_IMAGE_IDEMPOTENCY_CONFLICT", "the idempotency key was reused with a different request body"
	case errors.Is(err, ErrProviderNotConfigured):
		return "BATCH_IMAGE_PROVIDER_NOT_CONFIGURED", "no channel is configured to serve batch image requests right now"
	}
	var upstream *upstreamError
	if errors.As(err, &upstream) {
		return "BATCH_IMAGE_UPSTREAM_ERROR", truncateText(upstream.Error(), maxPublicErrorChars)
	}
	return "BATCH_IMAGE_ERROR", truncateText(err.Error(), maxPublicErrorChars)
}

// IsUpstreamError 报告 err 是不是一次上游失败（而不是我们自己的问题）。
//
// 【为什么需要这个判断】上游失败与内部 bug 在 HTTP 层必须是不同的状态码：
// 502 让客户端知道"重试可能有用、别来报 bug"；500 则意味着我们该修代码。
// 把两者混成 500 会让人白查很久。
func IsUpstreamError(err error) bool {
	var upstream *upstreamError
	return errors.As(err, &upstream)
}
