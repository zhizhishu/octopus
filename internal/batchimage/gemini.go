package batchimage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"github.com/zhizhishu/octopus/internal/utils/xurl"
)

// Gemini 开发者 API（AI Studio）的离线批处理生图。
//
// 【协议形状】三步，全部只要一个 API key：
//  1. 把 JSONL 上传到 File API 换一个文件引用（POST /upload/v1beta/files）；
//  2. 用文件引用建批次（POST /v1beta/models/{model}:batchGenerateContent）；
//  3. 轮询（GET /v1beta/batches/{id}），完成后下载结果的 JSONL。
//
// 【保留期】上游对上传的输入文件和结果都有保留期（文档口径 48 小时），
// 过期后文件消失。这就是我们必须在 job 行上记 OutputExpiresAt 的原因：
// 过期之后本地索引还在、图却取不到了，只能如实告诉客户端产物已过期，
// 不能给一个空文件让他以为我们弄坏了。
//
// 【不模仿任何 CLI 指纹】按 AGENTS.md 的 Shape 铁律：Gemini 系不属于需要模拟
// codex/claude CLI 的那类，只要求一个**稳定一致**的 UA，不泄漏下游客户端身份。
// 这里统一用 octopus 自己的 UA。

const (
	// geminiDefaultBaseURL 是 AI Studio 的默认地址。
	geminiDefaultBaseURL = "https://generativelanguage.googleapis.com"
	// geminiAPIVersion 固定用 v1beta：batchGenerateContent 只在 beta 面提供。
	geminiAPIVersion = "v1beta"
	// geminiUploadPath 是 File API 的上传入口。
	geminiUploadPath = "/upload/v1beta/files"
)

// geminiProvider 实现 Provider。
type geminiProvider struct {
	// client 可注入，用于测试替换成 httptest 的 client。
	client *http.Client
}

func newGeminiProvider() Provider {
	return &geminiProvider{}
}

// Name 见 model.BatchImageProviderGeminiAPI。
func (p *geminiProvider) Name() string { return model.BatchImageProviderGeminiAPI }

// Submit 上传 JSONL 并建批次。
func (p *geminiProvider) Submit(ctx context.Context, request SubmitRequest) (*SubmitResult, error) {
	if strings.TrimSpace(request.Channel.APIKey) == "" {
		return nil, fmt.Errorf("%w: gemini_api needs an api key", ErrProviderNotConfigured)
	}
	if len(request.JSONL) == 0 {
		return nil, fmt.Errorf("batch image: empty jsonl")
	}
	client := p.httpClient(request.Channel)

	// 第 1 步：上传 JSONL。
	//
	// displayName 用我们的批号：上游控制台里看到的就是这个，出问题时能双向对照。
	fileRef, err := p.uploadJSONL(ctx, client, request.Channel, request.BatchID, request.JSONL)
	if err != nil {
		return nil, err
	}

	// 第 2 步：建批次。
	batchName, rawState, err := p.createBatch(ctx, client, request.Channel, request.Model, request.BatchID, fileRef)
	if err != nil {
		// 建批次失败要把已经上传的输入文件删掉，否则上游留一份孤儿文件占配额。
		// 用**脱离父 ctx 的超时上下文**：父 ctx 可能正是因为超时才失败的，
		// 拿它去清理会连清理都取消掉。
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if cleanupErr := p.deleteFile(cleanupCtx, client, request.Channel, fileRef); cleanupErr != nil {
			log.Warnf("batch image: cleanup uploaded input %s after submit failure: %v", fileRef, cleanupErr)
		}
		return nil, err
	}

	return &SubmitResult{
		ProviderJobName:  batchName,
		ProviderInputRef: fileRef,
		RawState:         rawState,
		InternalState:    InternalStateQueued,
	}, nil
}

// uploadJSONL 走 File API 的 multipart 上传，返回文件引用（files/xxx）。
func (p *geminiProvider) uploadJSONL(ctx context.Context, client *http.Client, channel ChannelContext, batchID string, jsonl []byte) (string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	// 第一个 part 是元数据 JSON。顺序不能反：File API 要求 metadata 在文件之前，
	// 反过来会被当成一个没有 displayName 的文件（不报错，但控制台里认不出来）。
	metadataHeader := make(textproto.MIMEHeader)
	metadataHeader.Set("Content-Disposition", `form-data; name="metadata"`)
	metadataHeader.Set("Content-Type", "application/json; charset=UTF-8")
	metadataPart, err := writer.CreatePart(metadataHeader)
	if err != nil {
		return "", fmt.Errorf("batch image: build upload metadata part: %w", err)
	}
	metadata := map[string]any{
		"file": map[string]any{
			"displayName": "batch-image-" + batchID,
			// mimeType 必须是 application/jsonl：写成 application/json 时上游
			// 会按"单条请求"解析整份文件。
			"mimeType": "application/jsonl",
		},
	}
	if err := json.NewEncoder(metadataPart).Encode(metadata); err != nil {
		return "", fmt.Errorf("batch image: encode upload metadata: %w", err)
	}

	fileHeader := make(textproto.MIMEHeader)
	fileHeader.Set("Content-Disposition", `form-data; name="file"; filename="batch.jsonl"`)
	fileHeader.Set("Content-Type", "application/jsonl")
	filePart, err := writer.CreatePart(fileHeader)
	if err != nil {
		return "", fmt.Errorf("batch image: build upload file part: %w", err)
	}
	if _, err := filePart.Write(jsonl); err != nil {
		return "", fmt.Errorf("batch image: write upload file part: %w", err)
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("batch image: close upload body: %w", err)
	}

	endpoint, err := joinGeminiURL(channel.BaseURL, geminiUploadPath)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return "", fmt.Errorf("batch image: build upload request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	p.applyHeaders(req, channel)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("batch image: upload jsonl: %w", err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("batch image: read upload response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", newUpstreamError("upload jsonl", resp.StatusCode, payload)
	}

	var parsed struct {
		File struct {
			Name string `json:"name"`
		} `json:"file"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return "", fmt.Errorf("batch image: parse upload response: %w", err)
	}
	ref := strings.TrimSpace(parsed.File.Name)
	if ref == "" {
		return "", fmt.Errorf("batch image: upload response did not include a file name")
	}
	return ref, nil
}

// createBatch 建批次，返回批次句柄与上游状态串。
func (p *geminiProvider) createBatch(ctx context.Context, client *http.Client, channel ChannelContext, modelName, batchID, fileRef string) (string, string, error) {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return "", "", fmt.Errorf("batch image: empty model")
	}

	// 模型名要做 path 转义：上游允许 `models/gemini-x` 和 `gemini-x` 两种写法，
	// 直接拼进 URL 时斜杠会被当成路径分隔符，把 :batchGenerateContent 挂到错的段上。
	escaped := url.PathEscape(strings.TrimPrefix(modelName, "models/"))
	path := fmt.Sprintf("/%s/models/%s:batchGenerateContent", geminiAPIVersion, escaped)
	endpoint, err := joinGeminiURL(channel.BaseURL, path)
	if err != nil {
		return "", "", err
	}

	payload := map[string]any{
		"batch": map[string]any{
			"displayName": "batch-image-" + batchID,
			"inputConfig": map[string]any{
				"fileName": fileRef,
			},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", "", fmt.Errorf("batch image: encode create batch body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return "", "", fmt.Errorf("batch image: build create batch request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	p.applyHeaders(req, channel)

	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("batch image: create batch: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", "", fmt.Errorf("batch image: read create batch response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", newUpstreamError("create batch", resp.StatusCode, body)
	}

	var parsed struct {
		Name  string `json:"name"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", "", fmt.Errorf("batch image: parse create batch response: %w", err)
	}
	name := strings.TrimSpace(parsed.Name)
	if name == "" {
		return "", "", fmt.Errorf("batch image: create batch response did not include a name")
	}
	return name, strings.TrimSpace(parsed.State), nil
}

// Poll 查一次进度。
func (p *geminiProvider) Poll(ctx context.Context, request PollRequest) (*PollResult, error) {
	if strings.TrimSpace(request.JobName) == "" {
		return nil, fmt.Errorf("batch image: poll without a batch name")
	}
	client := p.httpClient(request.Channel)

	// 批次句柄可能已经带 batches/ 前缀，也可能只是 id。
	name := strings.TrimSpace(request.JobName)
	if !strings.HasPrefix(name, "batches/") {
		name = "batches/" + name
	}
	endpoint, err := joinGeminiURL(request.Channel.BaseURL, "/"+geminiAPIVersion+"/"+name)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("batch image: build poll request: %w", err)
	}
	p.applyHeaders(req, request.Channel)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("batch image: poll batch: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("batch image: read poll response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, newUpstreamError("poll batch", resp.StatusCode, body)
	}

	var parsed struct {
		Metadata struct {
			State string `json:"state"`
		} `json:"metadata"`
		State  string `json:"state"`
		Done   bool   `json:"done"`
		Output struct {
			ResponsesFile string `json:"responsesFile"`
		} `json:"output"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("batch image: parse poll response: %w", err)
	}

	// 上游把状态放在 metadata.state 里；顶层 state 是兼容老形状的兜底。
	rawState := strings.TrimSpace(parsed.Metadata.State)
	if rawState == "" {
		rawState = strings.TrimSpace(parsed.State)
	}

	result := &PollResult{
		RawState:      rawState,
		InternalState: mapGeminiBatchState(rawState, parsed.Done),
		OutputRef:     strings.TrimSpace(parsed.Output.ResponsesFile),
	}
	if parsed.Error != nil {
		result.InternalState = InternalStateFailed
		result.ErrorCode = strings.TrimSpace(parsed.Error.Status)
		if result.ErrorCode == "" {
			result.ErrorCode = fmt.Sprintf("upstream_%d", parsed.Error.Code)
		}
		result.ErrorMessage = truncateText(strings.TrimSpace(parsed.Error.Message), 500)
	}
	return result, nil
}

// mapGeminiBatchState 把上游状态串映射成归一化状态。
//
// 【未知状态按"还在跑"处理，不按失败】把没见过的状态当成失败会让一次上游改名
// 就把所有在途批次判死；当成还在跑只是多轮询几次，代价小得多。
func mapGeminiBatchState(rawState string, done bool) InternalState {
	switch strings.ToUpper(strings.TrimSpace(rawState)) {
	case "JOB_STATE_PENDING", "JOB_STATE_QUEUED", "PENDING", "QUEUED":
		return InternalStateQueued
	case "JOB_STATE_RUNNING", "RUNNING":
		return InternalStateRunning
	case "JOB_STATE_SUCCEEDED", "SUCCEEDED", "JOB_STATE_PARTIALLY_SUCCEEDED":
		// 部分成功也按成功收：逐条结果里会带自己的错误，那才是权威。
		return InternalStateSucceeded
	case "JOB_STATE_FAILED", "FAILED":
		return InternalStateFailed
	case "JOB_STATE_CANCELLED", "CANCELLED", "CANCELED":
		return InternalStateCancelled
	case "JOB_STATE_EXPIRED", "EXPIRED":
		return InternalStateExpired
	}
	if done {
		return InternalStateSucceeded
	}
	return InternalStateRunning
}

// Cancel 尽力撤单。
func (p *geminiProvider) Cancel(ctx context.Context, request CancelRequest) error {
	if strings.TrimSpace(request.JobName) == "" {
		return nil
	}
	client := p.httpClient(request.Channel)

	name := strings.TrimSpace(request.JobName)
	if !strings.HasPrefix(name, "batches/") {
		name = "batches/" + name
	}
	endpoint, err := joinGeminiURL(request.Channel.BaseURL, "/"+geminiAPIVersion+"/"+name+":cancel")
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader("{}"))
	if err != nil {
		return fmt.Errorf("batch image: build cancel request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	p.applyHeaders(req, request.Channel)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("batch image: cancel batch: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return newUpstreamError("cancel batch", resp.StatusCode, body)
	}
	return nil
}

// OpenResult 打开结果 JSONL。
func (p *geminiProvider) OpenResult(ctx context.Context, request ResultRequest) (io.ReadCloser, error) {
	ref := strings.TrimSpace(request.OutputRef)
	if ref == "" {
		return nil, fmt.Errorf("batch image: no result file reference")
	}
	client := p.httpClient(request.Channel)

	// 结果引用有两种形状：文件名（files/xxx）或已经是一个下载 URI。
	// 后者在上游返回 files/xxx;download 时出现，必须原样用，不能再拼一次。
	downloadURL := ref
	if !strings.HasPrefix(ref, "http://") && !strings.HasPrefix(ref, "https://") {
		endpoint, err := joinGeminiURL(request.Channel.BaseURL, "/"+geminiAPIVersion+"/"+ref)
		if err != nil {
			return nil, err
		}
		// 先查元数据拿 downloadUri，再用它下载。多一次请求是值得的：
		// 直接拼 /files/xxx:download 在上游改路径时会静默 404。
		metaReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("batch image: build file metadata request: %w", err)
		}
		p.applyHeaders(metaReq, request.Channel)
		metaResp, err := client.Do(metaReq)
		if err != nil {
			return nil, fmt.Errorf("batch image: get result file metadata: %w", err)
		}
		metaBody, readErr := io.ReadAll(io.LimitReader(metaResp.Body, 1<<20))
		metaResp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("batch image: read result file metadata: %w", readErr)
		}
		if metaResp.StatusCode < 200 || metaResp.StatusCode >= 300 {
			return nil, newUpstreamError("get result file metadata", metaResp.StatusCode, metaBody)
		}
		var meta struct {
			DownloadURI string `json:"downloadUri"`
			URI         string `json:"uri"`
		}
		if err := json.Unmarshal(metaBody, &meta); err != nil {
			return nil, fmt.Errorf("batch image: parse result file metadata: %w", err)
		}
		downloadURL = strings.TrimSpace(meta.DownloadURI)
		if downloadURL == "" {
			downloadURL = strings.TrimSpace(meta.URI)
		}
		if downloadURL == "" {
			return nil, fmt.Errorf("batch image: result file metadata has no download uri")
		}
	}

	// 下载地址可能指到别的主机（上游的文件 CDN）。**必须校验**：这是我们带着密钥
	// 要去访问的地址，指到任意主机就是把密钥交出去。
	if err := validateDownloadHost(downloadURL, request.Channel.BaseURL); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("batch image: build result download request: %w", err)
	}
	// 下载走 header 认证，不走 query：query 里的 key 会进上游访问日志。
	p.applyHeaders(req, request.Channel)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("batch image: download result: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, newUpstreamError("download result", resp.StatusCode, body)
	}
	return resp.Body, nil
}

// Cleanup 删掉我们上传的输入文件。
//
// 结果文件不删：上游按保留期自己清，我们主动删会让"过期后还能再下"变成"立刻就没了"。
func (p *geminiProvider) Cleanup(ctx context.Context, request CleanupRequest) error {
	if !request.DeleteInput || strings.TrimSpace(request.InputRef) == "" {
		return nil
	}
	return p.deleteFile(ctx, p.httpClient(request.Channel), request.Channel, request.InputRef)
}

// deleteFile 删一个 File API 文件。
func (p *geminiProvider) deleteFile(ctx context.Context, client *http.Client, channel ChannelContext, ref string) error {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil
	}
	endpoint, err := joinGeminiURL(channel.BaseURL, "/"+geminiAPIVersion+"/"+ref)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return fmt.Errorf("batch image: build delete file request: %w", err)
	}
	p.applyHeaders(req, channel)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("batch image: delete file: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound {
		// 已经不在了 = 目的达到。
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return newUpstreamError("delete file", resp.StatusCode, body)
	}
	return nil
}

// httpClient 返回带超时的 client。
func (p *geminiProvider) httpClient(channel ChannelContext) *http.Client {
	if p.client != nil {
		return p.client
	}
	timeout := channel.HTTPTimeout
	if timeout <= 0 {
		// 批处理的上传与下载都可能很慢（几十 MB 的结果文件），
		// 超时给足；轮询本身是短请求，由外层 ctx 控制。
		timeout = 5 * time.Minute
	}
	return &http.Client{Timeout: timeout}
}

// applyHeaders 统一加认证与自定义头。
func (p *geminiProvider) applyHeaders(req *http.Request, channel ChannelContext) {
	// 密钥走 header 不走 query 里的 ?key=：query 会进上游访问日志、
	// 也会出现在我们自己的错误串里。
	req.Header.Set("x-goog-api-key", channel.APIKey)
	req.Header.Set("User-Agent", batchImageUserAgent)
	for key, value := range channel.Headers {
		req.Header.Set(key, value)
	}
}

// batchImageUserAgent 是出站 UA。
//
// 固定值，不带版本号变量：带变量会让同一份代码在不同构建下指纹抖动，
// 而稳定的 UA 对排查"到底是不是我们发的"更有用（见 AGENTS.md Shape 铁律：
// gemini 系只要求稳定一致，不模拟 CLI）。
const batchImageUserAgent = "octopus-batch-image/1.0"

// joinGeminiURL 拼上游地址，复用本仓的 Gemini 路径归一化。
//
// 直接复用 xurl.JoinGeminiPath 而不是自己拼：渠道 BaseURL 允许用户填
// "…/v1beta" 或 "…/v1beta/models" 之类的尾巴，JoinGeminiPath 会把这些尾巴
// 统一剥掉再拼，自己拼就会拼出 /v1beta/models/v1beta/models/... 这种地址。
func joinGeminiURL(baseURL, path string) (string, error) {
	base := strings.TrimSpace(baseURL)
	if base == "" {
		base = geminiDefaultBaseURL
	}
	joined, err := xurl.JoinGeminiPath(base, path)
	if err != nil {
		return "", fmt.Errorf("batch image: join upstream url: %w", err)
	}
	return joined, nil
}

// validateDownloadHost 校验下载地址与我们信任的上游同源。
//
// 【这是安全边界，不是洁癖】下载请求会带上渠道密钥。上游若能让我们去下
// 任意主机，等于把密钥送出去。放行的只有两种：同一个 host，或 googleapis.com
// 后缀（Google 的文件下载确实会指到内容 CDN）。
func validateDownloadHost(rawURL, baseURL string) error {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return fmt.Errorf("batch image: result url is not parseable: %w", err)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return fmt.Errorf("batch image: result url has unsupported scheme %q", parsed.Scheme)
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return fmt.Errorf("batch image: result url has no host")
	}
	if strings.HasSuffix(host, ".googleapis.com") || host == "googleapis.com" {
		return nil
	}
	base := strings.TrimSpace(baseURL)
	if base == "" {
		base = geminiDefaultBaseURL
	}
	baseParsed, err := url.Parse(base)
	if err != nil {
		// 渠道地址解析不了时，只放行 googleapis.com（上面已经判过了），
		// 到这儿说明不是，那就拒绝。
		return fmt.Errorf("batch image: refusing to download results from %q", host)
	}
	if strings.EqualFold(baseParsed.Hostname(), host) {
		return nil
	}
	return fmt.Errorf("batch image: refusing to download results from %q", host)
}

// upstreamError 是一次上游失败，带脱敏后的消息。
type upstreamError struct {
	Op         string
	StatusCode int
	// Code 是上游给的错误码串（如 INVALID_ARGUMENT）。
	Code string
	// Message 是**已脱敏**的消息，可以进日志与客户端。
	Message string
}

func (e *upstreamError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("batch image: %s failed: upstream %d %s: %s", e.Op, e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("batch image: %s failed: upstream %d: %s", e.Op, e.StatusCode, e.Message)
}

// Temporary 报告这个错误是否值得重试。
//
// 只有 5xx 与 429 值得重试：4xx 里除了限流都是"请求本身不对"，
// 重试多少次都一样，还会把上游打得更狠。
func (e *upstreamError) Temporary() bool {
	return e.StatusCode >= 500 || e.StatusCode == http.StatusTooManyRequests
}

// newUpstreamError 从一次失败的响应造错误，并把上游消息压成一句话。
func newUpstreamError(op string, statusCode int, body []byte) *upstreamError {
	code, message := parseUpstreamErrorBody(body)
	if message == "" {
		message = "upstream returned status " + strconv.Itoa(statusCode)
	}
	return &upstreamError{Op: op, StatusCode: statusCode, Code: code, Message: message}
}

// parseUpstreamErrorBody 从上游错误体里抠出 code 与 message。
//
// 上游的错误体有两层：Google 标准的 {"error":{"code":..,"message":..,"status":..}}
// 和网关层的 {"message":..}。两种都认，认不出来就返回空串（由调用方兜底）。
func parseUpstreamErrorBody(body []byte) (string, string) {
	if len(body) == 0 {
		return "", ""
	}
	var envelope struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil {
		if envelope.Error != nil {
			code := strings.TrimSpace(envelope.Error.Status)
			if code == "" && envelope.Error.Code != 0 {
				code = strconv.Itoa(envelope.Error.Code)
			}
			return code, sanitizeUpstreamMessage(envelope.Error.Message)
		}
		if strings.TrimSpace(envelope.Message) != "" {
			return "", sanitizeUpstreamMessage(envelope.Message)
		}
	}
	// 不是 JSON：把前 200 字节压成一行当消息，比"未知错误"有用得多。
	return "", sanitizeUpstreamMessage(string(body))
}

// sanitizeUpstreamMessage 把上游消息压成一句话并截断。
//
// 【为什么必须做】上游消息里可能回显我们的请求地址（含查询串）、也可能是一大段
// HTML。直接透给客户端既难看又可能泄漏渠道信息。
func sanitizeUpstreamMessage(message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return ""
	}
	// 压成单行：多行消息进日志会把后面的行污染成假日志。
	message = strings.Join(strings.Fields(message), " ")
	return truncateText(message, 500)
}
