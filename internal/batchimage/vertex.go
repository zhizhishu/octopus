package batchimage

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/log"
)

// Vertex AI 的离线批处理生图。
//
// 【与 Gemini 开发者 API 的关键差别，逐条列清楚】
//  1. **凭证不同**：Gemini 一把 API key；Vertex 要一个服务账号 JSON（私钥签 JWT
//     换 OAuth2 token）。所以渠道密钥栏里存的是**整个 JSON 原文**，不是 key。
//  2. **输入输出只能走 GCS**：Gemini 能内联、能走 File API；Vertex 的批处理
//     只认 gs:// 上的 JSONL，结果也写在 gs:// 上。这意味着必须配一个桶
//     （ChannelBatchImage.GCSBucket），没有桶这一路根本发不出去。
//  3. **接口是 aiplatform 的 batchPredictionJobs**，不是 generativelanguage 的
//     batches。地址里带 project/location，模型是 publishers/google/models/{model}。
//  4. 状态串是 JOB_STATE_* 但字段层级不同（顶层 state + error）。
//
// 【为什么不实现 GCS 的完整 SDK】我们只需要三个动作：上传一个对象、列一个前缀下的
// 对象、下载一个对象、删一个前缀。这四个都能用一行 HTTP 完成（JSON API + 上传）。
// 引一整个 cloud.google.com/go/storage 会带来几十个间接依赖，而它与本仓现有的
// 轻依赖风格不符。代价是签名 v4 要自己写——已经在下面写完了，并且有测试盯着。

const (
	// vertexDefaultBaseURL 是 aiplatform 的默认地址。
	//
	// 注意它不带 location：Vertex 的地址是 {location}-aiplatform.googleapis.com，
	// 而 location 是渠道配置的一部分（global 时前缀是 aiplatform.googleapis.com）。
	vertexDefaultBaseURL = "https://aiplatform.googleapis.com"
	// vertexDefaultLocation 是上游默认区域。
	vertexDefaultLocation = "global"
	// vertexDefaultGCSPrefix 是我们在桶里的默认前缀。
	vertexDefaultGCSPrefix = "octopus-batch-image"
	// vertexScope 是换 token 时申请的权限范围。
	//
	// 用 cloud-platform 而不是更窄的：批处理要读写 GCS + 调 aiplatform，
	// 两个服务的窄 scope 各不相同，配错的表现是"提交时 403 但看不出缺哪个"。
	vertexScope = "https://www.googleapis.com/auth/cloud-platform"
	// vertexJWTTokenURL 是服务账号换 token 的固定入口。
	vertexJWTTokenURL = "https://oauth2.googleapis.com/token"
)

// vertexServiceAccount 是服务账号 JSON 里我们需要的字段。
//
// 只解这几个：解全量会把私钥之外的东西（比如整个 JSON）留在内存里更久，
// 而且上游改字段时我们不该跟着崩。
type vertexServiceAccount struct {
	Type        string `json:"type"`
	ProjectID   string `json:"project_id"`
	PrivateKey  string `json:"private_key"`
	ClientEmail string `json:"client_email"`
	TokenURI    string `json:"token_uri"`
}

// vertexProvider 实现 Provider。
type vertexProvider struct {
	client *http.Client

	// tokenCache 缓存 OAuth2 token（按 client_email + scope）。
	//
	// 没有它的话每一轮轮询都要签一次 JWT、换一次 token：一批任务可能被轮询几百次，
	// 每次一个额外的往返，还会撞上 Google 的 token 端点限流。
	tokenMu    sync.Mutex
	tokenCache map[string]cachedVertexToken
}

// cachedVertexToken 是一条缓存下来的 token。
type cachedVertexToken struct {
	token     string
	expiresAt time.Time
}

func newVertexProvider() Provider {
	return &vertexProvider{tokenCache: make(map[string]cachedVertexToken)}
}

// Name 见 model.BatchImageProviderVertex。
func (p *vertexProvider) Name() string { return model.BatchImageProviderVertex }

// Submit 上传 JSONL 到 GCS，再建 batchPredictionJob。
func (p *vertexProvider) Submit(ctx context.Context, request SubmitRequest) (*SubmitResult, error) {
	settings, err := p.resolveSettings(request.Channel)
	if err != nil {
		return nil, err
	}
	if len(request.JSONL) == 0 {
		return nil, fmt.Errorf("batch image: empty jsonl")
	}
	client := p.httpClient(request.Channel)

	token, err := p.accessToken(ctx, client, settings)
	if err != nil {
		return nil, err
	}

	// 输入对象名带批号：同一个批次重放（进程重启后重试）会写到同一个对象上，
	// 天然幂等，不会在桶里堆出一堆同名不同内容的孤儿。
	inputObject := settings.objectName(request.BatchID, "input/batch.jsonl")
	inputURI := settings.gcsURI(inputObject)
	if err := p.uploadGCSObject(ctx, client, settings, token, inputObject, request.JSONL); err != nil {
		return nil, err
	}

	outputPrefix := settings.gcsURI(settings.objectName(request.BatchID, "output/"))
	jobName, rawState, err := p.createBatchPredictionJob(ctx, client, settings, token, request, inputURI, outputPrefix)
	if err != nil {
		// 建作业失败就把刚上传的输入删掉，别在桶里留垃圾（桶是用户自己的，会占他钱）。
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		p.invalidateToken(settings)
		if cleanupErr := p.deleteGCSObject(cleanupCtx, client, settings, token, inputObject); cleanupErr != nil {
			log.Warnf("batch image: cleanup vertex input %s after submit failure: %v", inputObject, cleanupErr)
		}
		return nil, err
	}

	return &SubmitResult{
		ProviderJobName:   jobName,
		ProviderInputRef:  inputURI,
		ProviderOutputRef: outputPrefix,
		RawState:          rawState,
		InternalState:     InternalStateQueued,
	}, nil
}

// vertexSettings 是解析并校验过的 Vertex 配置。
type vertexSettings struct {
	account   vertexServiceAccount
	projectID string
	location  string
	bucket    string
	prefix    string
	baseURL   string
}

// objectName 拼出桶内的对象名。
//
// 前缀 + 批号 + 用途。批号在最外层是为了"删一个批次的所有东西"能用一次
// 按前缀删除完成（见 Cleanup）。
func (s vertexSettings) objectName(batchID, suffix string) string {
	parts := make([]string, 0, 3)
	if s.prefix != "" {
		parts = append(parts, strings.Trim(s.prefix, "/"))
	}
	parts = append(parts, batchID)
	parts = append(parts, strings.Trim(suffix, "/"))
	return strings.Join(parts, "/")
}

// gcsURI 把对象名变成 gs:// 地址。
func (s vertexSettings) gcsURI(object string) string {
	return "gs://" + s.bucket + "/" + strings.Trim(object, "/")
}

// resolveSettings 从渠道配置里解出 Vertex 需要的全部参数并校验。
//
// 【全部必填项在这里一次报清】Vertex 的失败模式很糟：少一个 location 会在建作业时
// 报一个看不出缺什么的 400。这里逐项检查并明确说是哪一项缺了。
func (p *vertexProvider) resolveSettings(channel ChannelContext) (vertexSettings, error) {
	rawKey := strings.TrimSpace(channel.APIKey)
	if rawKey == "" {
		return vertexSettings{}, fmt.Errorf("%w: vertex needs a service account json", ErrProviderNotConfigured)
	}
	var account vertexServiceAccount
	if err := json.Unmarshal([]byte(rawKey), &account); err != nil {
		return vertexSettings{}, fmt.Errorf("%w: service account json is not valid json", ErrProviderNotConfigured)
	}
	if strings.TrimSpace(account.ClientEmail) == "" || strings.TrimSpace(account.PrivateKey) == "" {
		return vertexSettings{}, fmt.Errorf("%w: service account json is missing client_email or private_key", ErrProviderNotConfigured)
	}
	if strings.TrimSpace(account.ProjectID) == "" {
		return vertexSettings{}, fmt.Errorf("%w: service account json is missing project_id", ErrProviderNotConfigured)
	}

	projectID := strings.TrimSpace(channel.ProjectID)
	if projectID == "" {
		projectID = strings.TrimSpace(account.ProjectID)
	}
	location := strings.TrimSpace(channel.Location)
	if location == "" {
		location = vertexDefaultLocation
	}
	bucket := strings.TrimSpace(channel.GCSBucket)
	if bucket == "" {
		return vertexSettings{}, fmt.Errorf("%w: vertex needs a gcs bucket (batch prediction reads and writes gs:// only)", ErrProviderNotConfigured)
	}
	prefix := strings.Trim(strings.TrimSpace(channel.GCSPrefix), "/")
	if prefix == "" {
		prefix = vertexDefaultGCSPrefix
	}

	return vertexSettings{
		account:   account,
		projectID: projectID,
		location:  location,
		bucket:    bucket,
		prefix:    prefix,
		baseURL:   strings.TrimSpace(channel.BaseURL),
	}, nil
}

// accessToken 拿一个可用的 OAuth2 token（有缓存）。
func (p *vertexProvider) accessToken(ctx context.Context, client *http.Client, settings vertexSettings) (string, error) {
	cacheKey := settings.account.ClientEmail + "|" + vertexScope

	p.tokenMu.Lock()
	cached, ok := p.tokenCache[cacheKey]
	p.tokenMu.Unlock()
	// 提前一分钟当过期：留出余量，避免"刚好在请求途中过期"。
	if ok && time.Now().Before(cached.expiresAt.Add(-time.Minute)) {
		return cached.token, nil
	}

	token, expiresIn, err := p.fetchAccessToken(ctx, client, settings.account)
	if err != nil {
		return "", err
	}
	p.tokenMu.Lock()
	p.tokenCache[cacheKey] = cachedVertexToken{
		token:     token,
		expiresAt: time.Now().Add(time.Duration(expiresIn) * time.Second),
	}
	p.tokenMu.Unlock()
	return token, nil
}

// invalidateToken 丢掉缓存（401 时用）。
func (p *vertexProvider) invalidateToken(settings vertexSettings) {
	p.tokenMu.Lock()
	delete(p.tokenCache, settings.account.ClientEmail+"|"+vertexScope)
	p.tokenMu.Unlock()
}

// fetchAccessToken 用服务账号私钥签 JWT，换 access token。
//
// 这是 Google 的标准服务账号流程（两段式 JWT bearer）：
//   - assertion 的 iss = client_email、sub 可省、aud = token_uri、scope = 目标权限；
//   - 用 RS256 签；
//   - POST 到 token_uri 换 access_token。
func (p *vertexProvider) fetchAccessToken(ctx context.Context, client *http.Client, account vertexServiceAccount) (string, int, error) {
	tokenURL := strings.TrimSpace(account.TokenURI)
	if tokenURL == "" {
		tokenURL = vertexJWTTokenURL
	}

	now := time.Now()
	claims := map[string]any{
		"iss":   account.ClientEmail,
		"scope": vertexScope,
		"aud":   tokenURL,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
	assertion, err := signRS256JWT(claims, account.PrivateKey)
	if err != nil {
		return "", 0, fmt.Errorf("%w: cannot sign service account jwt: %v", ErrProviderNotConfigured, err)
	}

	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	form.Set("assertion", assertion)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("batch image: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", batchImageUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("batch image: request access token: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", 0, newUpstreamError("request access token", resp.StatusCode, body)
	}

	var parsed struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", 0, fmt.Errorf("batch image: parse token response: %w", err)
	}
	if strings.TrimSpace(parsed.AccessToken) == "" {
		return "", 0, fmt.Errorf("batch image: token response has no access_token")
	}
	if parsed.ExpiresIn <= 0 {
		// 上游没给有效期时按一小时算（Google 的默认值），别缓存成永不过期。
		parsed.ExpiresIn = 3600
	}
	return parsed.AccessToken, parsed.ExpiresIn, nil
}

// signRS256JWT 用 PEM 私钥签一个 RS256 JWT。
func signRS256JWT(claims map[string]any, privateKeyPEM string) (string, error) {
	key, err := parseRSAPrivateKey(privateKeyPEM)
	if err != nil {
		return "", err
	}

	header := map[string]any{"alg": "RS256", "typ": "JWT"}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("encode jwt header: %w", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("encode jwt claims: %w", err)
	}

	unsigned := base64.RawURLEncoding.EncodeToString(headerJSON) + "." +
		base64.RawURLEncoding.EncodeToString(claimsJSON)

	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign jwt: %w", err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// parseRSAPrivateKey 解析 PEM 私钥（PKCS8 是服务账号的格式，PKCS1 一并认）。
func parseRSAPrivateKey(privateKeyPEM string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(privateKeyPEM)))
	if block == nil {
		return nil, fmt.Errorf("private key is not valid PEM")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("private key is not an RSA key")
		}
		return rsaKey, nil
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	return nil, fmt.Errorf("private key is neither PKCS8 nor PKCS1 RSA")
}

// gcsDefaultBaseURL 是 GCS 的 JSON API 入口。
const gcsDefaultBaseURL = "https://storage.googleapis.com"

// gcsBaseURL 返回 GCS 的入口地址。
//
// 【为什么跟着 BaseURL 走】Vertex 的企业部署常常把整个流量导到一个自建网关
// （这也是渠道允许填 BaseURL 的原因）。如果存储调用写死到 googleapis.com，
// 那种部署里 aiplatform 走网关、GCS 却直连外网——一半请求绕过网关，
// 在只有网关能出网的机房里表现成"作业建起来了但输入永远传不上去"。
func gcsBaseURL(baseURL string) string {
	if base := strings.TrimRight(strings.TrimSpace(baseURL), "/"); base != "" {
		return base
	}
	return gcsDefaultBaseURL
}

// uploadGCSObject 上传一个对象到 GCS（简单上传，media 直传）。
func (p *vertexProvider) uploadGCSObject(ctx context.Context, client *http.Client, settings vertexSettings, token, object string, payload []byte) error {
	endpoint := gcsBaseURL(settings.baseURL) + "/upload/storage/v1/b/" + url.PathEscape(settings.bucket) +
		"/o?uploadType=media&name=" + url.QueryEscape(object)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("batch image: build gcs upload request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	// 上游的批处理读的是 application/jsonl；写成 octet-stream 时 Vertex 会
	// 按 instancesFormat=jsonl 解析失败并报一个看不出原因的错。
	req.Header.Set("Content-Type", "application/jsonl")
	req.Header.Set("User-Agent", batchImageUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("batch image: upload gcs object: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return newUpstreamError("upload gcs object", resp.StatusCode, body)
	}
	return nil
}

// deleteGCSObject 删一个对象。不存在算成功。
func (p *vertexProvider) deleteGCSObject(ctx context.Context, client *http.Client, settings vertexSettings, token, object string) error {
	endpoint := gcsBaseURL(settings.baseURL) + "/storage/v1/b/" + url.PathEscape(settings.bucket) +
		"/o/" + url.PathEscape(object)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return fmt.Errorf("batch image: build gcs delete request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", batchImageUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("batch image: delete gcs object: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return newUpstreamError("delete gcs object", resp.StatusCode, body)
	}
	return nil
}

// vertexAiplatformHost 拼出 aiplatform 的区域地址。
//
// global 区域是个特例：地址不带区域前缀。写错的表现是 DNS 解析失败或 404，
// 而不是一个能看懂的报错，所以这里显式分支。
func vertexAiplatformHost(baseURL, location string) string {
	if strings.TrimSpace(baseURL) != "" {
		return strings.TrimRight(strings.TrimSpace(baseURL), "/")
	}
	location = strings.TrimSpace(location)
	if location == "" || strings.EqualFold(location, "global") {
		return vertexDefaultBaseURL
	}
	return "https://" + location + "-aiplatform.googleapis.com"
}

// createBatchPredictionJob 建一个批处理作业。
func (p *vertexProvider) createBatchPredictionJob(
	ctx context.Context,
	client *http.Client,
	settings vertexSettings,
	token string,
	request SubmitRequest,
	inputURI string,
	outputPrefix string,
) (string, string, error) {
	modelName := strings.TrimSpace(request.Model)
	if modelName == "" {
		return "", "", fmt.Errorf("batch image: empty model")
	}
	modelPath := vertexModelResource(settings.projectID, settings.location, modelName)

	endpoint := vertexAiplatformHost(settings.baseURL, settings.location) +
		"/v1/projects/" + url.PathEscape(settings.projectID) +
		"/locations/" + url.PathEscape(settings.location) +
		"/batchPredictionJobs"

	payload := map[string]any{
		"displayName": "batch-image-" + request.BatchID,
		"model":       modelPath,
		"inputConfig": map[string]any{
			"instancesFormat": "jsonl",
			"gcsSource": map[string]any{
				"uris": []string{inputURI},
			},
		},
		"outputConfig": map[string]any{
			"predictionsFormat": "jsonl",
			"gcsDestination": map[string]any{
				"outputUriPrefix": outputPrefix,
			},
		},
		// keyField 告诉上游"用哪一列当结果回填的键"。我们 JSONL 里的列叫 key，
		// 与 Gemini 侧一致；不指定时上游会按行号对，而那条路会因乱序而错位。
		"instanceConfig": map[string]any{
			"keyField": "key",
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", "", fmt.Errorf("batch image: encode vertex job body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return "", "", fmt.Errorf("batch image: build vertex job request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", batchImageUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("batch image: create vertex job: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", newUpstreamError("create vertex job", resp.StatusCode, body)
	}

	var parsed struct {
		Name  string `json:"name"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", "", fmt.Errorf("batch image: parse vertex job response: %w", err)
	}
	name := strings.TrimSpace(parsed.Name)
	if name == "" {
		return "", "", fmt.Errorf("batch image: vertex job response has no name")
	}
	return name, strings.TrimSpace(parsed.State), nil
}

// vertexModelResource 把模型名拼成 Vertex 的资源路径。
//
// 用户可能填 gemini-2.5-flash、也可能已经填了完整路径。已经带 projects/ 的
// 一律原样用（那是用户显式指定，覆盖我们的默认拼接）。
func vertexModelResource(projectID, location, modelName string) string {
	if strings.HasPrefix(modelName, "projects/") {
		return modelName
	}
	modelName = strings.TrimPrefix(modelName, "publishers/google/models/")
	return "projects/" + projectID + "/locations/" + location +
		"/publishers/google/models/" + modelName
}

// Poll 查一次作业进度。
func (p *vertexProvider) Poll(ctx context.Context, request PollRequest) (*PollResult, error) {
	settings, err := p.resolveSettings(request.Channel)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.JobName) == "" {
		return nil, fmt.Errorf("batch image: poll without a job name")
	}
	client := p.httpClient(request.Channel)
	token, err := p.accessToken(ctx, client, settings)
	if err != nil {
		return nil, err
	}

	jobName := strings.TrimPrefix(strings.TrimSpace(request.JobName), "/")
	endpoint := vertexAiplatformHost(settings.baseURL, settings.location) + "/v1/" + jobName

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("batch image: build vertex poll request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", batchImageUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("batch image: poll vertex job: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		// token 可能被上游提前作废：丢掉缓存，下一轮重新换。
		p.invalidateToken(settings)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, newUpstreamError("poll vertex job", resp.StatusCode, body)
	}

	var parsed struct {
		State      string `json:"state"`
		OutputInfo struct {
			GcsOutputDirectory string `json:"gcsOutputDirectory"`
		} `json:"outputInfo"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("batch image: parse vertex poll response: %w", err)
	}

	rawState := strings.TrimSpace(parsed.State)
	result := &PollResult{
		RawState:      rawState,
		InternalState: mapVertexJobState(rawState),
		OutputRef:     strings.TrimSpace(parsed.OutputInfo.GcsOutputDirectory),
	}
	if result.OutputRef == "" {
		result.OutputRef = strings.TrimSpace(request.OutputRef)
	}
	if parsed.Error != nil {
		result.InternalState = InternalStateFailed
		result.ErrorCode = "upstream_error"
		if parsed.Error.Code != 0 {
			result.ErrorCode = fmt.Sprintf("upstream_%d", parsed.Error.Code)
		}
		result.ErrorMessage = truncateText(sanitizeUpstreamMessage(parsed.Error.Message), 500)
	}
	return result, nil
}

// mapVertexJobState 把 Vertex 的作业状态映射成归一化状态。
//
// Vertex 的 JOB_STATE_PARTIALLY_SUCCEEDED 按成功收：逐条结果里带自己的错误，
// 那才是权威；整批判失败会让成功的那部分图也拿不到。
func mapVertexJobState(rawState string) InternalState {
	switch strings.ToUpper(strings.TrimSpace(rawState)) {
	case "JOB_STATE_PENDING", "JOB_STATE_QUEUED":
		return InternalStateQueued
	case "JOB_STATE_RUNNING", "JOB_STATE_UPDATING", "JOB_STATE_CANCELLING":
		// CANCELLING 按还在跑处理：上游还没停，这时候报"已取消"会让客户端
		// 以为钱退了，而实际上它还在花我们的额度。
		return InternalStateRunning
	case "JOB_STATE_SUCCEEDED", "JOB_STATE_PARTIALLY_SUCCEEDED":
		return InternalStateSucceeded
	case "JOB_STATE_FAILED":
		return InternalStateFailed
	case "JOB_STATE_CANCELLED":
		return InternalStateCancelled
	case "JOB_STATE_EXPIRED":
		return InternalStateExpired
	}
	// 未知状态按还在跑处理，理由同 Gemini 侧：上游改名不该把所有在途批次判死。
	return InternalStateRunning
}

// Cancel 尽力撤单。
func (p *vertexProvider) Cancel(ctx context.Context, request CancelRequest) error {
	settings, err := p.resolveSettings(request.Channel)
	if err != nil {
		return err
	}
	if strings.TrimSpace(request.JobName) == "" {
		return nil
	}
	client := p.httpClient(request.Channel)
	token, err := p.accessToken(ctx, client, settings)
	if err != nil {
		return err
	}

	jobName := strings.TrimPrefix(strings.TrimSpace(request.JobName), "/")
	endpoint := vertexAiplatformHost(settings.baseURL, settings.location) + "/v1/" + jobName + ":cancel"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader("{}"))
	if err != nil {
		return fmt.Errorf("batch image: build vertex cancel request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", batchImageUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("batch image: cancel vertex job: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return newUpstreamError("cancel vertex job", resp.StatusCode, body)
	}
	return nil
}

// OpenResult 打开 Vertex 的结果。
//
// 【为什么不是"打开一个文件"】Vertex 把结果写进
// gs://bucket/{outputUriPrefix}/predictions.jsonl（可能还带前缀子目录与多个分片）。
// 所以要：列前缀 → 排序 → 把多个分片**首尾相接**成一个流。
// 不排序的话同一个批次两次读到的行序会不同，索引就对不上了。
func (p *vertexProvider) OpenResult(ctx context.Context, request ResultRequest) (io.ReadCloser, error) {
	settings, err := p.resolveSettings(request.Channel)
	if err != nil {
		return nil, err
	}
	outputRef := strings.TrimSpace(request.OutputRef)
	if outputRef == "" {
		return nil, fmt.Errorf("batch image: no vertex output reference")
	}
	client := p.httpClient(request.Channel)
	token, err := p.accessToken(ctx, client, settings)
	if err != nil {
		return nil, err
	}

	prefix, err := gcsPrefixFromURI(outputRef, settings.bucket)
	if err != nil {
		return nil, err
	}
	objects, err := p.listGCSObjects(ctx, client, settings, token, prefix)
	if err != nil {
		return nil, err
	}
	if len(objects) == 0 {
		return nil, fmt.Errorf("batch image: vertex output prefix %q has no objects", prefix)
	}
	sort.Strings(objects)
	return &vertexCombinedReader{
		ctx:      ctx,
		provider: p,
		client:   client,
		settings: settings,
		token:    token,
		objects:  objects,
	}, nil
}

// gcsPrefixFromURI 从 gs:// 地址里取出对象前缀，并校验桶就是我们配的那个桶。
//
// 【为什么要校验桶】结果地址来自上游，如果它给了一个别的桶，我们就会带着 token
// 去读用户没授权的数据；更糟的是这说明有东西在诱导我们越界。
func gcsPrefixFromURI(uri, expectedBucket string) (string, error) {
	uri = strings.TrimSpace(uri)
	if !strings.HasPrefix(uri, "gs://") {
		return "", fmt.Errorf("batch image: %q is not a gs:// uri", uri)
	}
	rest := strings.TrimPrefix(uri, "gs://")
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return "", fmt.Errorf("batch image: %q has no object prefix", uri)
	}
	bucket := rest[:slash]
	prefix := strings.TrimPrefix(rest[slash+1:], "/")
	if !strings.EqualFold(bucket, expectedBucket) {
		return "", fmt.Errorf("batch image: refusing to read gs://%s (configured bucket is %q)", bucket, expectedBucket)
	}
	return prefix, nil
}

// listGCSObjects 列出一个前缀下的全部对象名（自动翻页）。
func (p *vertexProvider) listGCSObjects(ctx context.Context, client *http.Client, settings vertexSettings, token, prefix string) ([]string, error) {
	const pageSize = 1000
	var objects []string
	pageToken := ""
	// 上限保护：正常情况下一个批次的输出只有几个分片。真列出一万个对象说明
	// 前缀配错（比如指到了桶根），这时候继续翻页只会把内存吃光。
	const maxObjects = 5000

	for {
		endpoint := gcsBaseURL(settings.baseURL) + "/storage/v1/b/" + url.PathEscape(settings.bucket) + "/o?prefix=" +
			url.QueryEscape(strings.Trim(prefix, "/")+"/") + "&maxResults=" + fmt.Sprint(pageSize)
		if pageToken != "" {
			endpoint += "&pageToken=" + url.QueryEscape(pageToken)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("batch image: build gcs list request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("User-Agent", batchImageUserAgent)

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("batch image: list gcs objects: %w", err)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("batch image: read gcs list response: %w", readErr)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, newUpstreamError("list gcs objects", resp.StatusCode, body)
		}

		var parsed struct {
			Items []struct {
				Name string `json:"name"`
			} `json:"items"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("batch image: parse gcs list response: %w", err)
		}
		for _, item := range parsed.Items {
			name := strings.TrimSpace(item.Name)
			if name == "" {
				continue
			}
			objects = append(objects, name)
			if len(objects) >= maxObjects {
				return objects, nil
			}
		}
		if strings.TrimSpace(parsed.NextPageToken) == "" {
			return objects, nil
		}
		pageToken = parsed.NextPageToken
	}
}

// vertexCombinedReader 把多个 GCS 对象首尾相接读成一个流。
//
// 为什么需要它：上层只有"打开结果、逐行解析"这一个动作。让上层去处理多分片
// 会把 provider 的细节漏上去。
type vertexCombinedReader struct {
	ctx      context.Context
	provider *vertexProvider
	client   *http.Client
	settings vertexSettings
	token    string
	objects  []string

	index   int
	current io.ReadCloser
}

// Read 实现 io.Reader，跨对象边界连续读。
func (r *vertexCombinedReader) Read(p []byte) (int, error) {
	for {
		if r.current == nil {
			if r.index >= len(r.objects) {
				return 0, io.EOF
			}
			next, err := r.openObject(r.objects[r.index])
			if err != nil {
				return 0, err
			}
			r.current = next
			r.index++
		}
		n, err := r.current.Read(p)
		if err == io.EOF {
			// 当前对象读完，关掉它再继续下一个。
			r.current.Close()
			r.current = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

// Close 实现 io.Closer。
func (r *vertexCombinedReader) Close() error {
	if r.current != nil {
		err := r.current.Close()
		r.current = nil
		return err
	}
	return nil
}

// openObject 打开一个 GCS 对象（media 下载）。
func (r *vertexCombinedReader) openObject(object string) (io.ReadCloser, error) {
	endpoint := gcsBaseURL(r.settings.baseURL) + "/storage/v1/b/" + url.PathEscape(r.settings.bucket) +
		"/o/" + url.PathEscape(object) + "?alt=media"
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("batch image: build gcs download request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("User-Agent", batchImageUserAgent)

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("batch image: download gcs object: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, newUpstreamError("download gcs object", resp.StatusCode, body)
	}
	return resp.Body, nil
}

// Cleanup 删掉我们在这个批次下留在桶里的东西。
//
// Vertex 的输入和输出都在用户的桶里，**删之前必须确认这些对象是我们造的**：
// 对象名一定以我们配的前缀 + 批号开头（见 objectName），这个判断在下面显式做。
// 前缀配错（比如配成 "/"）时宁可什么都不删，也不能把用户桶里别的东西扫掉。
func (p *vertexProvider) Cleanup(ctx context.Context, request CleanupRequest) error {
	if !request.DeleteInput && !request.DeleteOutput {
		return nil
	}
	settings, err := p.resolveSettings(request.Channel)
	if err != nil {
		return err
	}
	batchPrefix := settings.objectName(request.BatchID, "/")
	// 安全闸：批号必须出现在前缀里，且前缀不能是空/根。
	if strings.TrimSpace(request.BatchID) == "" || !strings.Contains(batchPrefix, request.BatchID) {
		return fmt.Errorf("batch image: refusing to clean vertex objects without a batch scoped prefix")
	}

	client := p.httpClient(request.Channel)
	token, err := p.accessToken(ctx, client, settings)
	if err != nil {
		return err
	}

	targets := make([]string, 0, 2)
	if request.DeleteInput {
		targets = append(targets, settings.objectName(request.BatchID, "input/"))
	}
	if request.DeleteOutput {
		targets = append(targets, settings.objectName(request.BatchID, "output/"))
	}

	var firstErr error
	for _, prefix := range targets {
		objects, listErr := p.listGCSObjects(ctx, client, settings, token, prefix)
		if listErr != nil {
			if firstErr == nil {
				firstErr = listErr
			}
			continue
		}
		for _, object := range objects {
			// 再确认一次：只删真正落在本批次前缀下的对象。
			if !strings.HasPrefix(object, strings.Trim(prefix, "/")+"/") {
				log.Warnf("batch image: skipping vertex object %q outside batch prefix %q", object, prefix)
				continue
			}
			if err := p.deleteGCSObject(ctx, client, settings, token, object); err != nil {
				if firstErr == nil {
					firstErr = err
				}
			}
		}
	}
	return firstErr
}

// httpClient 见 gemini 侧同名方法（超时口径一致）。
func (p *vertexProvider) httpClient(channel ChannelContext) *http.Client {
	if p.client != nil {
		return p.client
	}
	timeout := channel.HTTPTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	return &http.Client{Timeout: timeout}
}
