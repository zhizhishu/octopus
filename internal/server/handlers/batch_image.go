package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/batchimage"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/resp"
	"github.com/zhizhishu/octopus/internal/server/router"
	"gorm.io/gorm"
)

// 图片批处理的 HTTP 层。
//
// 【这一层只做三件事】解析入参、调 batchimage.Service、把结果写成 HTTP。
// 所有判断（谁能看、能不能提交、钱怎么算）都在下面那一层，这里不重复。
//
// 【为什么错误映射单独一个函数】10 条路由的错误语义必须一致：同一个 ErrNotFound
// 在 10 个地方各写一次 if，迟早有一条写成 403，而 403 会告诉探测者"这个批号是
// 真的，只是不是你的"。

// BatchImageService 是 handler 依赖的编排层能力（抽成接口便于测试替换）。
//
// 【为什么没有 Settings 方法】batchimage.Service 的 Settings 是个**字段**（配置读取
// 函数），不是方法。写成接口方法会与方法集冲突，而且 handler 需要的是"读一次快照"，
// 那件事由 batchImageSettings() 直接做，不必绕道 service。
type BatchImageService interface {
	Submit(ctx context.Context, input batchimage.SubmitInput) (*model.BatchImageJob, error)
	Settle(ctx context.Context, job *model.BatchImageJob) (bool, error)
}

// batchImageService 是包级单例（与其它 handler 的做法一致）。
var batchImageService BatchImageService = newDefaultBatchImageService()

// newDefaultBatchImageService 组装生产用的编排层。
func newDefaultBatchImageService() BatchImageService {
	return &batchimage.Service{
		Settings: batchImageSettingsFromStore,
	}
}

// batchImageSettingsFromStore 交给 batchimage 包里的共享实现。
//
// 【为什么不留一份自己的】提交路径与执行路径读配置的口径必须逐字相同。
// 两处各写一份的必然结果是"提交时说开着、执行时说关着"，而且只在生产上、
// 只在特定时序下复现。
func batchImageSettingsFromStore(ctx context.Context) batchimage.Settings {
	return batchimage.SettingsFromStore(ctx)
}

// batchImageSettings 是 handler 自己用的配置快照（已补齐零值）。
func batchImageSettings(ctx context.Context) batchimage.Settings {
	return batchimage.SettingsFromStore(ctx).Normalize()
}

// 批处理路由的注册顺序是**契约的一部分**。
//
// 【为什么顺序重要】gin 的路由树里同层级的字面量段与参数段会抢同一个位置：
//   - `/batches/models` 必须排在 `/batches/:id` 之前，否则 "models" 会被当成批号，
//     models 列表会变成"查一个叫 models 的批次"→ 永远 404；
//   - `/batches/:id/items/:custom_id/content`、`/batches/:id/download`、
//     `/batches/:id/cancel`、`/batches/:id/outputs` 都必须排在裸 `/batches/:id` 之前。
//
// 这个顺序由 batch_image_routes_test.go 用一个真 gin 引擎盯着——顺序错了那条测试
// 会以"models 被当成批号"的形式失败，而不是等线上 404。
func init() {
	router.NewGroupRouter("/v1").
		Use(middleware.APIKeyAuth()).
		AddRoute(
			router.NewRoute("/images/batches", http.MethodPost).
				Handle(HandleBatchImageSubmit),
		).
		AddRoute(
			router.NewRoute("/images/batches", http.MethodGet).
				Handle(HandleBatchImageList),
		).
		// models 是字面量，必须在 `:id` 之前。
		AddRoute(
			router.NewRoute("/images/batches/models", http.MethodGet).
				Handle(HandleBatchImageModels),
		).
		// 子路径（字面量尾段）全部先于裸 `:id`。
		AddRoute(
			router.NewRoute("/images/batches/:id/items/:custom_id/content", http.MethodGet).
				Handle(HandleBatchImageItemContent),
		).
		AddRoute(
			router.NewRoute("/images/batches/:id/items", http.MethodGet).
				Handle(HandleBatchImageItems),
		).
		AddRoute(
			router.NewRoute("/images/batches/:id/download", http.MethodGet).
				Handle(HandleBatchImageDownload),
		).
		AddRoute(
			router.NewRoute("/images/batches/:id/cancel", http.MethodPost).
				Handle(HandleBatchImageCancel),
		).
		AddRoute(
			router.NewRoute("/images/batches/:id/outputs", http.MethodDelete).
				Handle(HandleBatchImageDeleteOutputs),
		).
		// 裸 `:id` 放最后。
		AddRoute(
			router.NewRoute("/images/batches/:id", http.MethodGet).
				Handle(HandleBatchImageGet),
		).
		AddRoute(
			router.NewRoute("/images/batches/:id", http.MethodDelete).
				Handle(HandleBatchImageDelete),
		)
}

// batchImageOwner 取调用者身份。
//
// 两个归属一起取：只按 user_id 过滤会让同一用户的两个 key 互相看到对方的批次，
// 而"我这个 key 提交了什么"是用户真实会问的问题。
func batchImageOwner(c *gin.Context) (userID int, tokenID int, ok bool) {
	userIDValue, exists := c.Get("user_id")
	if !exists {
		resp.OpenAIError(c, http.StatusUnauthorized, "authentication_error", "missing_credentials",
			"an api key is required for batch image endpoints")
		return 0, 0, false
	}
	userID, _ = userIDValue.(int)
	if userID <= 0 {
		resp.OpenAIError(c, http.StatusUnauthorized, "authentication_error", "invalid_credentials",
			"the api key in this request does not map to a user")
		return 0, 0, false
	}
	if keyID, exists := c.Get("api_key_id"); exists {
		tokenID, _ = keyID.(int)
	}
	return userID, tokenID, true
}

// writeBatchImageError 统一错误映射。**所有**批处理错误都从这里出去。
func writeBatchImageError(c *gin.Context, err error) {
	if err == nil {
		err = errors.New("unknown batch image error")
	}
	// 归属缺失这类"查不到"必须是 404 而不是 403：403 等于确认这个批号存在。
	switch {
	case errors.Is(err, batchimage.ErrNotFound):
		resp.OpenAIError(c, http.StatusNotFound, "invalid_request_error", "BATCH_IMAGE_NOT_FOUND",
			"batch image job not found")
		return
	case errors.Is(err, batchimage.ErrDisabled):
		// 关掉时回 404（与 async_image_enabled 同规矩）：回 403 会让人以为是自己
		// 权限不够，跑去要权限，而实际上是运维没开。
		resp.OpenAIError(c, http.StatusNotFound, "invalid_request_error", "BATCH_IMAGE_DISABLED",
			"batch image endpoints are not enabled on this deployment")
		return
	case errors.Is(err, batchimage.ErrTooManyItems),
		errors.Is(err, batchimage.ErrTooManyActiveJobs),
		errors.Is(err, batchimage.ErrIdempotencyConflict),
		errors.Is(err, batchimage.ErrModelNotEnabled),
		errors.Is(err, batchimage.ErrNoChannelAvailable),
		errors.Is(err, batchimage.ErrProviderNotConfigured):
		resp.OpenAIError(c, http.StatusBadRequest, "invalid_request_error",
			firstNonEmpty(batchImageErrorCode(err), "BATCH_IMAGE_BAD_REQUEST"),
			batchImageErrorMessage(err))
		return
	case batchimage.IsUpstreamError(err):
		// 上游失败用 502：这是"我们没做错，上游没给"的语义，
		// 用 500 会让客户端以为是我们内部的 bug。
		resp.OpenAIError(c, http.StatusBadGateway, "upstream_error", "BATCH_IMAGE_UPSTREAM_ERROR",
			batchImageErrorMessage(err))
		return
	}
	code, message := batchimage.PublicError(err)
	resp.OpenAIError(c, http.StatusInternalServerError, "server_error", code, message)
}

// batchImageErrorCode 取错误码。
func batchImageErrorCode(err error) string {
	code, _ := batchimage.PublicError(err)
	return code
}

// batchImageErrorMessage 取脱敏后的错误消息。
func batchImageErrorMessage(err error) string {
	_, message := batchimage.PublicError(err)
	return message
}

// firstNonEmpty 取第一个非空串。
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// loadBatchImageJob 取一条属于调用者的任务，取不到就写 404 并返回 false。
func loadBatchImageJob(c *gin.Context, userID, tokenID int) (*model.BatchImageJob, bool) {
	job, err := op.BatchImageGetForOwner(c.Request.Context(), c.Param("id"), userID, tokenID)
	if err != nil {
		if isRecordNotFound(err) {
			writeBatchImageError(c, batchimage.ErrNotFound)
			return nil, false
		}
		writeBatchImageError(c, err)
		return nil, false
	}
	// 🔴 必须判 nil。持久层对"不存在 / 不是你的 / 已过期"一律返回 (nil, nil)
	// ——那是故意的，好让这三种情况对客户端不可区分（防探测）。
	// 但只判 err 就会把 nil 当成加载成功：后续 job.BatchID 直接空指针 panic，
	// 被 recovery 兜成 500，既不是约定的 404，也把"查不到"说成了"服务器坏了"。
	if job == nil {
		writeBatchImageError(c, batchimage.ErrNotFound)
		return nil, false
	}
	return job, true
}

// isRecordNotFound 判断宽松一点：不同驱动返回的错误串不同。
func isRecordNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "record not found")
}

// 提交请求体。
//
// 形状刻意贴近 OpenAI 的 batch 接口（input_file / requests），但用内联 items：
// 我们的用户是拿程序生成一批 prompt，让他们先拼一个 JSONL 文件再上传是多余的一步。
type batchImageSubmitRequest struct {
	Model            string                `json:"model"`
	Provider         string                `json:"provider"`
	Items            []batchImageItemInput `json:"items"`
	ResponseMimeType string                `json:"response_mime_type"`
	AspectRatio      string                `json:"aspect_ratio"`
	ImageSize        string                `json:"image_size"`
	Metadata         map[string]string     `json:"metadata"`
}

// batchImageItemInput 是一条子请求。
type batchImageItemInput struct {
	CustomID        string                     `json:"custom_id"`
	Prompt          string                     `json:"prompt"`
	OutputCount     int                        `json:"output_count"`
	ReferenceImages []batchImageReferenceInput `json:"reference_images"`
}

// batchImageReferenceInput 是一张参考图。
//
// base64 与 file_uri 二选一。这里用 `json:"-"` 之外的方式解析是因为
// 我们想给出"两个都给 / 都不给"这种明确的错误，而不是让 JSON 解析器猜。
type batchImageReferenceInput struct {
	MimeType string `json:"mime_type"`
	// DataBase64 是内联图片（base64 编码）。
	DataBase64 string `json:"data_base64"`
	// FileURI 是上游已有的文件引用。
	FileURI string `json:"file_uri"`
}

// HandleBatchImageSubmit 处理 POST /v1/images/batches。
func HandleBatchImageSubmit(c *gin.Context) {
	userID, tokenID, ok := batchImageOwner(c)
	if !ok {
		return
	}

	// 请求体先设硬上限再绑定。
	//
	// 【为什么必须在上限之内绑定】ShouldBindJSON 会把整个 body 解成字符串，
	// 之后我们再逐张 base64 解码参考图。没有上限时，一个有效 key 交一份
	// 几百 MB 的 items，JSON 字符串 + 解码字节 + 构建出的 JSONL 会同时在内存里，
	// 光靠 items 条数上限挡不住（条数少但单条巨大同样能打爆）。
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, batchImageMaxRequestBytes)
	var request batchImageSubmitRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		if isBodyTooLarge(err) {
			resp.OpenAIError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large",
				fmt.Sprintf("the request body is larger than %d bytes", batchImageMaxRequestBytes))
			return
		}
		resp.OpenAIError(c, http.StatusBadRequest, "invalid_request_error", "invalid_json",
			"the request body is not valid json")
		return
	}
	// 模型白名单：与既有的图片入口（relay/images.go）同口径。
	// 鉴权中间件只把该 key 的 allowed models 注入成字符串，检查得自己做。
	if !op.IsModelSupported(c.GetString("supported_models"), strings.TrimSpace(request.Model)) {
		resp.OpenAIError(c, http.StatusBadRequest, "invalid_request_error", "model_not_supported",
			"this api key is not allowed to use the requested model")
		return
	}
	if strings.TrimSpace(request.Model) == "" {
		resp.OpenAIError(c, http.StatusBadRequest, "invalid_request_error", "missing_model",
			"model is required")
		return
	}
	if len(request.Items) == 0 {
		resp.OpenAIError(c, http.StatusBadRequest, "invalid_request_error", "missing_items",
			"items must contain at least one entry")
		return
	}

	items := make([]batchimage.SubmitItem, 0, len(request.Items))
	for index, item := range request.Items {
		customID := strings.TrimSpace(item.CustomID)
		if customID == "" {
			// 没给就补一个稳定的：客户端总得有个东西来对结果。
			customID = fmt.Sprintf("item-%d", index+1)
		}
		refs := make([]batchimage.ReferenceImage, 0, len(item.ReferenceImages))
		for refIndex, ref := range item.ReferenceImages {
			parsed, err := decodeReferenceImage(ref)
			if err != nil {
				resp.OpenAIError(c, http.StatusBadRequest, "invalid_request_error", "invalid_reference_image",
					fmt.Sprintf("item %q reference image #%d: %s", customID, refIndex+1, err.Error()))
				return
			}
			refs = append(refs, parsed)
		}
		items = append(items, batchimage.SubmitItem{
			CustomID:        customID,
			Prompt:          item.Prompt,
			OutputCount:     item.OutputCount,
			ReferenceImages: refs,
		})
	}

	job, err := batchImageService.Submit(c.Request.Context(), batchimage.SubmitInput{
		OwnerUserID:      userID,
		OwnerTokenID:     tokenID,
		Model:            strings.TrimSpace(request.Model),
		Provider:         strings.TrimSpace(request.Provider),
		Items:            items,
		ResponseMimeType: strings.TrimSpace(request.ResponseMimeType),
		AspectRatio:      strings.TrimSpace(request.AspectRatio),
		ImageSize:        strings.TrimSpace(request.ImageSize),
		IdempotencyKey:   strings.TrimSpace(c.GetHeader("Idempotency-Key")),
		Metadata:         request.Metadata,
	})
	if err != nil {
		writeBatchImageError(c, err)
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"id":         job.BatchID,
		"object":     "image.batch",
		"status":     job.Status,
		"model":      job.RequestModel,
		"item_count": job.ItemCount,
		"created_at": job.CreatedAt,
		"expires_at": job.ExpiresAt,
	})
}

// decodeReferenceImage 把请求里的参考图变成内部形状，并强制"二选一"。
func decodeReferenceImage(ref batchImageReferenceInput) (batchimage.ReferenceImage, error) {
	mime := strings.TrimSpace(ref.MimeType)
	data := strings.TrimSpace(ref.DataBase64)
	fileURI := strings.TrimSpace(ref.FileURI)

	if data == "" && fileURI == "" {
		return batchimage.ReferenceImage{}, fmt.Errorf("needs either data_base64 or file_uri")
	}
	if data != "" && fileURI != "" {
		return batchimage.ReferenceImage{}, fmt.Errorf("gives both data_base64 and file_uri; pick one")
	}
	if mime == "" {
		return batchimage.ReferenceImage{}, fmt.Errorf("mime_type is required")
	}
	if fileURI != "" {
		return batchimage.ReferenceImage{MimeType: mime, FileURI: fileURI}, nil
	}
	decoded, err := decodeBase64Image(data)
	if err != nil {
		return batchimage.ReferenceImage{}, err
	}
	return batchimage.ReferenceImage{MimeType: mime, Data: decoded}, nil
}

// HandleBatchImageList 处理 GET /v1/images/batches。
func HandleBatchImageList(c *gin.Context) {
	userID, tokenID, ok := batchImageOwner(c)
	if !ok {
		return
	}
	limit := 20
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	cursor := op.BatchImageCursorFromString(c.Query("after"))

	jobs, nextCursor, err := op.BatchImageListForOwner(c.Request.Context(), userID, tokenID,
		strings.TrimSpace(c.Query("status")), limit, cursor)
	if err != nil {
		writeBatchImageError(c, err)
		return
	}

	data := make([]gin.H, 0, len(jobs))
	for index := range jobs {
		data = append(data, batchImageJobJSON(&jobs[index]))
	}
	c.JSON(http.StatusOK, gin.H{
		"object":   "list",
		"data":     data,
		"has_more": nextCursor != 0,
		"after":    nextCursor,
	})
}

// HandleBatchImageModels 处理 GET /v1/images/batches/models。
//
// 返回的是**当前渠道声明承接的**模型，不是"所有已知模型"。
// 列一堆实际跑不了的模型，用户选了才发现没有渠道，是自己给自己造坑。
func HandleBatchImageModels(c *gin.Context) {
	if _, _, ok := batchImageOwner(c); !ok {
		return
	}
	settings := batchImageSettings(c.Request.Context())
	if !settings.Enabled {
		writeBatchImageError(c, batchimage.ErrDisabled)
		return
	}

	channels, err := op.ChannelList(c.Request.Context())
	if err != nil {
		writeBatchImageError(c, err)
		return
	}
	seen := make(map[string]batchImageModelEntry)
	for index := range channels {
		channel := channels[index]
		if !channel.Enabled || channel.BatchImage == nil {
			continue
		}
		provider := channel.BatchImageProvider()
		if !model.IsSupportedBatchImageProvider(provider) {
			continue
		}
		for _, modelName := range channel.BatchImage.Models {
			name := strings.TrimSpace(modelName)
			if name == "" {
				continue
			}
			entry := seen[name]
			if entry.ID == "" {
				entry.ID = name
				entry.Object = "image.batch.model"
			}
			if !containsString(entry.Providers, provider) {
				entry.Providers = append(entry.Providers, provider)
			}
			seen[name] = entry
		}
	}

	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sortStrings(names)
	data := make([]batchImageModelEntry, 0, len(names))
	for _, name := range names {
		data = append(data, seen[name])
	}
	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   data,
		"limits": gin.H{
			"max_items":       settings.MaxItems,
			"max_active_jobs": settings.MaxConcurrentJobsPerOwner,
			"retention_hours": settings.RetentionHours,
		},
	})
}

// batchImageModelEntry 是 models 列表里的一项。
type batchImageModelEntry struct {
	ID        string   `json:"id"`
	Object    string   `json:"object"`
	Providers []string `json:"providers"`
}

// HandleBatchImageGet 处理 GET /v1/images/batches/:id。
func HandleBatchImageGet(c *gin.Context) {
	userID, tokenID, ok := batchImageOwner(c)
	if !ok {
		return
	}
	job, ok := loadBatchImageJob(c, userID, tokenID)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, batchImageJobJSON(job))
}

// HandleBatchImageItems 处理 GET /v1/images/batches/:id/items。
func HandleBatchImageItems(c *gin.Context) {
	userID, tokenID, ok := batchImageOwner(c)
	if !ok {
		return
	}
	job, ok := loadBatchImageJob(c, userID, tokenID)
	if !ok {
		return
	}
	items, err := op.BatchImageItemsByBatchID(c.Request.Context(), job.BatchID)
	if err != nil {
		writeBatchImageError(c, err)
		return
	}
	data := make([]gin.H, 0, len(items))
	for index := range items {
		data = append(data, batchImageItemJSON(&items[index], job.BatchID))
	}
	c.JSON(http.StatusOK, gin.H{
		"object":   "list",
		"batch_id": job.BatchID,
		"data":     data,
	})
}

// HandleBatchImageItemContent 处理 GET /v1/images/batches/:id/items/:custom_id/content。
//
// 【这条路由必须流式写】图片是 MB 级的字节。整份读进内存再写会让一批 200 张的同时
// 下载把内存打爆；而套一个限制 body 大小的中间件会把大图截断成坏文件。
// 所以这里只做 io.Copy，且不经过任何 JSON 中间件。
func HandleBatchImageItemContent(c *gin.Context) {
	userID, tokenID, ok := batchImageOwner(c)
	if !ok {
		return
	}
	job, ok := loadBatchImageJob(c, userID, tokenID)
	if !ok {
		return
	}
	item, err := op.BatchImageItemGet(c.Request.Context(), job.BatchID, c.Param("custom_id"))
	if err != nil {
		if isRecordNotFound(err) {
			writeBatchImageError(c, batchimage.ErrNotFound)
			return
		}
		writeBatchImageError(c, err)
		return
	}
	if item.Status != model.BatchImageItemStatusSuccess {
		resp.OpenAIError(c, http.StatusNotFound, "invalid_request_error", "BATCH_IMAGE_ITEM_NOT_READY",
			"this item has no image: status is "+item.Status)
		return
	}
	if job.Status == model.BatchImageJobStatusOutputDeleted {
		// 产物过了保留期被上游删了。如实说，别给一个空文件让客户端以为我们坏了。
		resp.OpenAIError(c, http.StatusGone, "invalid_request_error", "BATCH_IMAGE_OUTPUT_DELETED",
			"the results for this batch have passed their retention window and are no longer available")
		return
	}

	streamed, err := streamBatchImageItem(c, job, item)
	if err != nil {
		writeBatchImageError(c, err)
		return
	}
	if !streamed {
		resp.OpenAIError(c, http.StatusNotFound, "invalid_request_error", "BATCH_IMAGE_ITEM_NOT_READY",
			"the image for this item is not available")
	}
}

// HandleBatchImageDownload 处理 GET /v1/images/batches/:id/download（打包成 zip）。
func HandleBatchImageDownload(c *gin.Context) {
	userID, tokenID, ok := batchImageOwner(c)
	if !ok {
		return
	}
	job, ok := loadBatchImageJob(c, userID, tokenID)
	if !ok {
		return
	}
	if job.Status == model.BatchImageJobStatusOutputDeleted {
		resp.OpenAIError(c, http.StatusGone, "invalid_request_error", "BATCH_IMAGE_OUTPUT_DELETED",
			"the results for this batch have passed their retention window and are no longer available")
		return
	}

	items, err := op.BatchImageItemsByBatchID(c.Request.Context(), job.BatchID)
	if err != nil {
		writeBatchImageError(c, err)
		return
	}

	// Cache-Control: no-store：这份 zip 是按请求现拼的，缓存它没有意义，
	// 而且中间代理缓存别人的图是隐私事故。
	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition",
		fmt.Sprintf(`attachment; filename="batch-%s.zip"`, sanitizeFilename(job.BatchID)))
	c.Header("Cache-Control", "private, no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Status(http.StatusOK)

	if err := streamBatchImageZip(c, job, items); err != nil {
		// 响应头已经发出去了，没法再改成 JSON 错误。
		// 记日志并中断连接——让客户端看到的是一个不完整的 zip，而不是一个假装成功的空文件。
		_ = c.Error(err)
		return
	}
}

// HandleBatchImageCancel 处理 POST /v1/images/batches/:id/cancel。
func HandleBatchImageCancel(c *gin.Context) {
	userID, tokenID, ok := batchImageOwner(c)
	if !ok {
		return
	}
	job, ok := loadBatchImageJob(c, userID, tokenID)
	if !ok {
		return
	}

	job, err := cancelBatchImageJob(c.Request.Context(), job)
	if err != nil {
		writeBatchImageError(c, err)
		return
	}
	c.JSON(http.StatusOK, batchImageJobJSON(job))
}

// HandleBatchImageDelete 处理 DELETE /v1/images/batches/:id。
//
// 【只删索引，不删上游】上游的结果对象由上游按保留期自己清；
// 我们删的是"我们认识这个批次"这件事。
func HandleBatchImageDelete(c *gin.Context) {
	userID, tokenID, ok := batchImageOwner(c)
	if !ok {
		return
	}
	job, ok := loadBatchImageJob(c, userID, tokenID)
	if !ok {
		return
	}
	// 还在跑的批次不许直接删：删了记录就再也没人能推进它，上游那份活会一直烧钱
	// 而我们连它叫什么都不知道。先取消。
	if !model.IsTerminalBatchImageJobStatus(job.Status) {
		resp.OpenAIError(c, http.StatusConflict, "invalid_request_error", "BATCH_IMAGE_JOB_ACTIVE",
			"cancel the batch before deleting it")
		return
	}
	deleted, err := op.BatchImageDeleteRecord(c.Request.Context(), job.ID, job.BatchID)
	if err != nil {
		writeBatchImageError(c, err)
		return
	}
	if !deleted {
		writeBatchImageError(c, batchimage.ErrNotFound)
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": job.BatchID, "object": "image.batch.deleted", "deleted": true})
}

// HandleBatchImageDeleteOutputs 处理 DELETE /v1/images/batches/:id/outputs。
//
// 标记产物已删。**钱不动**（早就结清了），也不动上游——上游的保留期是它自己的事。
func HandleBatchImageDeleteOutputs(c *gin.Context) {
	userID, tokenID, ok := batchImageOwner(c)
	if !ok {
		return
	}
	job, ok := loadBatchImageJob(c, userID, tokenID)
	if !ok {
		return
	}
	if job.Status != model.BatchImageJobStatusCompleted &&
		job.Status != model.BatchImageJobStatusOutputDeleted {
		resp.OpenAIError(c, http.StatusConflict, "invalid_request_error", "BATCH_IMAGE_NO_OUTPUT",
			"this batch has no outputs to delete: status is "+job.Status)
		return
	}
	if _, err := op.BatchImageMarkOutputDeleted(c.Request.Context(), job.ID); err != nil {
		writeBatchImageError(c, err)
		return
	}
	job.Status = model.BatchImageJobStatusOutputDeleted
	c.JSON(http.StatusOK, batchImageJobJSON(job))
}

// batchImageJobJSON 是任务对外的形状。
//
// 【不吐渠道 id】客户端不需要知道我们把活派给了哪个渠道；吐出去等于泄漏内部拓扑，
// 而且换渠道时客户端会以为出了问题。
func batchImageJobJSON(job *model.BatchImageJob) gin.H {
	if job == nil {
		return gin.H{}
	}
	out := gin.H{
		"id":              job.BatchID,
		"object":          "image.batch",
		"status":          job.Status,
		"model":           job.RequestModel,
		"item_count":      job.ItemCount,
		"success_count":   job.SuccessCount,
		"fail_count":      job.FailCount,
		"cancelled_count": job.CancelledCount,
		"created_at":      job.CreatedAt,
		"expires_at":      job.ExpiresAt,
	}
	if job.SubmittedAt > 0 {
		out["submitted_at"] = job.SubmittedAt
	}
	if job.CompletedAt > 0 {
		out["completed_at"] = job.CompletedAt
	}
	if job.OutputExpiresAt > 0 {
		out["output_expires_at"] = job.OutputExpiresAt
	}
	if job.ErrorCode != "" {
		out["error"] = gin.H{"code": job.ErrorCode, "message": job.ErrorMessage}
	}
	return out
}

// batchImageItemJSON 是子项对外的形状。
//
// content_url 只在成功时给：给一个必然 404 的地址比不给更糟。
func batchImageItemJSON(item *model.BatchImageItem, batchID string) gin.H {
	if item == nil {
		return gin.H{}
	}
	out := gin.H{
		"custom_id": item.CustomID,
		"status":    item.Status,
	}
	if item.PromptPreview != "" {
		out["prompt_preview"] = item.PromptPreview
	}
	if item.Status == model.BatchImageItemStatusSuccess {
		out["content_url"] = fmt.Sprintf("/v1/images/batches/%s/items/%s/content",
			url.PathEscape(batchID), url.PathEscape(item.CustomID))
		if item.MimeType != "" {
			out["mime_type"] = item.MimeType
		}
		if item.ImageCount > 0 {
			out["image_count"] = item.ImageCount
		}
	}
	if item.ErrorCode != "" {
		out["error"] = gin.H{"code": item.ErrorCode, "message": item.ErrorMessage}
	}
	return out
}

// sanitizeFilename 把批号压成安全的文件名。
//
// 批号是我们自己生成的（imgbatch_ + 十六进制），但**不能假设**：这条路径也会收到
// 从库里读出来的历史值。带引号或换行的文件名会把 Content-Disposition 头拆坏。
func sanitizeFilename(name string) string {
	var builder strings.Builder
	for _, char := range name {
		switch {
		case char >= 'a' && char <= 'z',
			char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9',
			char == '-', char == '_':
			builder.WriteRune(char)
		default:
			builder.WriteRune('_')
		}
	}
	out := builder.String()
	if out == "" {
		return "batch"
	}
	return out
}

// containsString 判断切片里有没有这个值。
func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// sortStrings 简单插入排序（避免为一个字符串列表引入 sort 的闭包）。
func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// batchImageMaxRequestBytes 是提交流程的请求体硬上限。
//
// 取值理由：单条 prompt 上限 8000 字符、单批上限 1000 条，纯文本远小于此；
// 参考图按 base64 算，128MB 够放十几张 4K 图。给一个**确定存在**的上限，
// 比"没有上限、只限制条数"安全：后者挡不住"条数很少但每条巨大"的请求。
const batchImageMaxRequestBytes int64 = 128 << 20

// isBodyTooLarge 识别 http.MaxBytesReader 的超限错误。
func isBodyTooLarge(err error) bool {
	if err == nil {
		return false
	}
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		return true
	}
	return strings.Contains(err.Error(), "http: request body too large")
}
