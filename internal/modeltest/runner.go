package modeltest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bestruirui/octopus/internal/helper"
	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	transformermodel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/bestruirui/octopus/internal/utils/log"
	"github.com/bestruirui/octopus/internal/utils/xredact"
	"github.com/google/uuid"
	"github.com/tmaxmax/go-sse"
)

const (
	defaultPromptSentinel                  = "Reply with exactly OK."
	defaultTimeoutSeconds                  = 30
	defaultAnthropicMessagesTimeoutSeconds = 180
	maxTimeoutSeconds                      = 300
	maxConcurrency                         = 20
	maxModels                              = 100
	upstreamBodyLimit                      = 32 * 1024
	defaultClaudeUserAgent                 = dbmodel.DefaultClaudeHeaderUserAgent
	defaultClaudePackageVersion            = "0.94.0"
	defaultClaudeRuntimeVersion            = "v24.3.0"
	defaultClaudeOS                        = "Windows"
	defaultClaudeArch                      = "x64"
	defaultClaudeTimeout                   = "600"
	defaultClaudeOneMillionBeta            = transformermodel.AnthropicOneMillionBeta
	defaultCodexUserAgent                  = dbmodel.DefaultCodexHeaderUserAgent
	defaultCodexBetaFeatures               = "terminal_resize_reflow"
	defaultCodexOriginator                 = "codex_exec"
	defaultCodexInstructions               = "You are Codex, a coding agent based on GPT-5. You and the user share one workspace. Answer directly and do not call tools unless the user asks for workspace inspection or file changes."
)

var defaultPromptCounter uint64

type testEndpoint struct {
	name      string
	apiFormat transformermodel.APIFormat
	path      func(string) string
}

type routeSelection struct {
	group     dbmodel.Group
	plan      *dbmodel.AccessPlan
	rule      *dbmodel.AccessRouteRule
	routeUsed bool
}

type requestIdentity struct {
	userID int
	apiKey *dbmodel.APIKey
}

type modelRunner struct {
	request dbmodel.ModelTestRequest
	result  dbmodel.ModelTestResult
	attempt int
}

func Run(ctx context.Context, req dbmodel.ModelTestRequest) (dbmodel.ModelTestResponse, error) {
	return run(ctx, req, nil)
}

func RunChannel(ctx context.Context, channel dbmodel.Channel, req dbmodel.ModelTestRequest) (dbmodel.ModelTestResponse, error) {
	req.ChannelID = 0
	req.AccessPlanSlug = ""
	req.APIKeyID = 0
	req.UserID = 0
	req.AuditLog = false
	return run(ctx, req, &channel)
}

func run(ctx context.Context, req dbmodel.ModelTestRequest, directChannel *dbmodel.Channel) (dbmodel.ModelTestResponse, error) {
	identity, err := resolveRequestIdentity(ctx, &req)
	if err != nil {
		return dbmodel.ModelTestResponse{}, err
	}

	models := normalizeModels(req)
	if len(models) == 0 {
		return dbmodel.ModelTestResponse{}, fmt.Errorf("at least one model is required")
	}
	if len(models) > maxModels {
		return dbmodel.ModelTestResponse{}, fmt.Errorf("too many models: maximum is %d", maxModels)
	}

	concurrency := req.Concurrency
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > maxConcurrency {
		concurrency = maxConcurrency
	}
	if concurrency > len(models) {
		concurrency = len(models)
	}

	started := time.Now()
	results := make([]dbmodel.ModelTestResult, len(models))
	jobs := make(chan int)
	var wg sync.WaitGroup

	for worker := 0; worker < concurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				results[index] = runOne(ctx, req, models[index], identity, directChannel)
			}
		}()
	}

	for index := range models {
		jobs <- index
	}
	close(jobs)
	wg.Wait()

	summary := dbmodel.ModelTestSummary{
		Total:      len(results),
		DurationMs: int(time.Since(started).Milliseconds()),
	}
	for _, result := range results {
		if result.Success {
			summary.Success++
		} else {
			summary.Failed++
		}
		if req.AuditLog {
			if err := addAuditLog(ctx, req, result, identity); err != nil {
				log.Warnf("failed to save model test audit log: %v", err)
			}
		}
	}

	return dbmodel.ModelTestResponse{
		Summary: summary,
		Results: results,
	}, nil
}

func resolveRequestIdentity(ctx context.Context, req *dbmodel.ModelTestRequest) (requestIdentity, error) {
	var identity requestIdentity
	if req == nil {
		return identity, nil
	}

	if req.UserID > 0 {
		if _, err := op.UserGet(req.UserID); err != nil {
			return identity, fmt.Errorf("user not found")
		}
		identity.userID = req.UserID
	}

	if req.APIKeyID > 0 {
		apiKey, err := op.APIKeyGet(req.APIKeyID, ctx)
		if err != nil {
			return identity, err
		}
		if req.UserID > 0 && apiKey.UserID != req.UserID {
			return identity, fmt.Errorf("API key does not belong to selected user")
		}
		req.UserID = apiKey.UserID
		identity.userID = apiKey.UserID
		identity.apiKey = &apiKey
	}

	return identity, nil
}

func addAuditLog(ctx context.Context, req dbmodel.ModelTestRequest, result dbmodel.ModelTestResult, identity requestIdentity) error {
	userID := identity.userID
	if userID == 0 {
		userID = req.UserID
	}
	apiKeyID := req.APIKeyID
	apiKeyName := ""
	if identity.apiKey != nil {
		apiKeyID = identity.apiKey.ID
		apiKeyName = identity.apiKey.Name
	}

	requestContent, _ := json.Marshal(map[string]any{
		"kind":             "model_test",
		"model":            result.RequestModel,
		"endpoint":         result.RequestEndpoint,
		"request_path":     result.RequestPath,
		"access_plan_slug": req.AccessPlanSlug,
		"user_id":          userID,
		"api_key_id":       apiKeyID,
		"prompt":           req.Prompt,
	})
	responseContent, _ := json.Marshal(map[string]any{
		"status":           modelTestStatus(result.Success),
		"status_code":      result.StatusCode,
		"upstream_model":   result.UpstreamModel,
		"upstream_path":    result.UpstreamPath,
		"latency_ms":       result.DurationMs,
		"attempts":         len(result.Attempts),
		"input_tokens":     result.InputTokens,
		"output_tokens":    result.OutputTokens,
		"response_preview": result.ResponsePreview,
		"error":            result.Error,
		"error_code":       result.ErrorCode,
	})

	relayLog := dbmodel.RelayLog{
		UserID:            userID,
		APIKeyID:          apiKeyID,
		Time:              time.Now().Unix(),
		RequestEndpoint:   modelTestAuditEndpoint(result.RequestEndpoint),
		RequestPath:       result.RequestPath,
		RequestModelName:  result.RequestModel,
		RequestAPIKeyName: apiKeyName,
		ChannelId:         result.ChannelID,
		ChannelName:       result.ChannelName,
		ActualModelName:   result.UpstreamModel,
		InputTokens:       int(result.InputTokens),
		OutputTokens:      int(result.OutputTokens),
		UseTime:           result.DurationMs,
		Cost:              0,
		RequestContent:    string(requestContent),
		ResponseContent:   string(responseContent),
		Error:             result.Error,
		ErrorCode:         result.ErrorCode,
		ErrorStatus:       modelTestErrorStatus(result),
		ErrorStrategy:     "model_test;billable=false;stats_counted=false",
		Attempts:          append([]dbmodel.ChannelAttempt(nil), result.Attempts...),
		TotalAttempts:     len(result.Attempts),
		AccessPlanID:      result.AccessPlanID,
		AccessPlanSlug:    result.AccessPlanSlug,
		AccessPlanName:    result.AccessPlanName,
		BillingModel:      result.RequestModel,
	}
	return op.RelayLogAdd(ctx, relayLog)
}

func modelTestStatus(success bool) string {
	if success {
		return "success"
	}
	return "failed"
}

func modelTestErrorStatus(result dbmodel.ModelTestResult) int {
	if result.Success {
		return 0
	}
	if result.StatusCode > 0 {
		return result.StatusCode
	}
	return http.StatusServiceUnavailable
}

func modelTestAuditEndpoint(endpoint string) string {
	switch strings.TrimSpace(endpoint) {
	case "openai_chat":
		return "model_test_chat"
	case "openai_responses":
		return "model_test_responses"
	case "anthropic_messages":
		return "model_test_anthropic_messages"
	case "gemini_generate_content":
		return "model_test_gemini"
	case "":
		return "model_test"
	default:
		return "model_test_" + strings.NewReplacer("/", "_", "-", "_", " ", "_").Replace(strings.ToLower(endpoint))
	}
}

func runOne(parent context.Context, req dbmodel.ModelTestRequest, modelName string, identity requestIdentity, directChannel *dbmodel.Channel) dbmodel.ModelTestResult {
	endpoint, err := normalizeEndpoint(req.Endpoint)
	if err != nil {
		return dbmodel.ModelTestResult{
			Model:           modelName,
			RequestModel:    modelName,
			RequestEndpoint: strings.TrimSpace(req.Endpoint),
			Success:         false,
			Error:           err.Error(),
		}
	}
	if identity.apiKey != nil && !op.IsModelSupported(identity.apiKey.SupportedModels, modelName) {
		return dbmodel.ModelTestResult{
			Model:           modelName,
			RequestModel:    modelName,
			RequestEndpoint: endpoint.name,
			RequestPath:     endpoint.path(modelName),
			StatusCode:      http.StatusBadRequest,
			Success:         false,
			Error:           "model not supported by selected API key",
			ErrorCode:       "model_not_supported",
		}
	}

	timeout := resolveTimeoutSeconds(req, endpoint)
	ctx, cancel := context.WithTimeout(parent, time.Duration(timeout)*time.Second)
	defer cancel()

	runner := &modelRunner{
		request: req,
		result: dbmodel.ModelTestResult{
			Model:           modelName,
			RequestModel:    modelName,
			RequestEndpoint: endpoint.name,
			RequestPath:     endpoint.path(modelName),
		},
	}

	started := time.Now()
	if directChannel != nil {
		if runner.tryChannel(ctx, directChannel, modelName, false) {
			runner.result.DurationMs = int(time.Since(started).Milliseconds())
			return runner.result
		}
		if !runner.result.Success && runner.result.Error == "" {
			runner.fail(errors.New("no available channel key succeeded"))
		}
		runner.result.DurationMs = int(time.Since(started).Milliseconds())
		return runner.result
	}

	if req.ChannelID > 0 {
		channel, err := op.ChannelGet(req.ChannelID, ctx)
		if err != nil {
			runner.fail(fmt.Errorf("channel %d not found: %w", req.ChannelID, err))
			runner.result.DurationMs = int(time.Since(started).Milliseconds())
			return runner.result
		}
		if runner.tryChannel(ctx, channel, modelName, false) {
			runner.result.DurationMs = int(time.Since(started).Milliseconds())
			return runner.result
		}
		if !runner.result.Success && runner.result.Error == "" {
			runner.fail(errors.New("no available channel key succeeded"))
		}
		runner.result.DurationMs = int(time.Since(started).Milliseconds())
		return runner.result
	}

	selection, err := selectRoute(ctx, req.APIKeyID, req.AccessPlanSlug, modelName)
	if err != nil {
		runner.fail(err)
		runner.result.DurationMs = int(time.Since(started).Milliseconds())
		return runner.result
	}
	runner.applyRouteMetadata(selection)

	if runner.trySelection(ctx, selection) {
		runner.result.DurationMs = int(time.Since(started).Milliseconds())
		return runner.result
	}

	if shouldFallbackToGroup(selection) {
		fallbackGroup, err := op.GroupGetEnabledMap(modelName, ctx)
		if err == nil {
			fallback := selection
			fallback.group = fallbackGroup
			fallback.routeUsed = false
			runner.result.RouteFallbackUsed = true
			_ = runner.trySelection(ctx, fallback)
		}
	}

	if !runner.result.Success && runner.result.Error == "" {
		runner.fail(errors.New("no available channel succeeded"))
	}
	runner.result.DurationMs = int(time.Since(started).Milliseconds())
	return runner.result
}

func resolveTimeoutSeconds(req dbmodel.ModelTestRequest, endpoint testEndpoint) int {
	timeout := req.TimeoutSeconds
	if timeout <= 0 {
		timeout = defaultTimeoutSeconds
		if endpoint.name == "anthropic_messages" {
			timeout = defaultAnthropicMessagesTimeoutSeconds
		}
	}
	if timeout > maxTimeoutSeconds {
		timeout = maxTimeoutSeconds
	}
	return timeout
}

func normalizeModels(req dbmodel.ModelTestRequest) []string {
	seen := make(map[string]struct{})
	models := make([]string, 0, len(req.Models)+1)
	appendModel := func(value string) {
		name := strings.TrimSpace(value)
		if name == "" {
			return
		}
		key := strings.ToLower(name)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		models = append(models, name)
	}

	appendModel(req.Model)
	for _, modelName := range req.Models {
		appendModel(modelName)
	}
	return models
}

func normalizeEndpoint(value string) (testEndpoint, error) {
	name := strings.ToLower(strings.TrimSpace(value))
	name = strings.NewReplacer("-", "_", "/", "_").Replace(name)
	if name == "" || name == "chat" || name == "openai" || name == "openai_chat_completions" {
		name = "openai_chat"
	}

	switch name {
	case "openai_chat":
		return testEndpoint{
			name:      "openai_chat",
			apiFormat: transformermodel.APIFormatOpenAIChatCompletion,
			path:      func(string) string { return "/v1/chat/completions" },
		}, nil
	case "openai_responses", "responses":
		return testEndpoint{
			name:      "openai_responses",
			apiFormat: transformermodel.APIFormatOpenAIResponse,
			path:      func(string) string { return "/v1/responses" },
		}, nil
	case "anthropic_messages", "messages", "claude":
		return testEndpoint{
			name:      "anthropic_messages",
			apiFormat: transformermodel.APIFormatAnthropicMessage,
			path:      func(string) string { return "/v1/messages" },
		}, nil
	case "gemini_generate_content", "gemini":
		return testEndpoint{
			name:      "gemini_generate_content",
			apiFormat: transformermodel.APIFormatGeminiContents,
			path: func(modelName string) string {
				return fmt.Sprintf("/v1beta/models/%s:generateContent", modelName)
			},
		}, nil
	default:
		return testEndpoint{}, fmt.Errorf("unsupported endpoint %q", value)
	}
}

func selectRoute(ctx context.Context, apiKeyID int, accessPlanSlug string, requestModel string) (routeSelection, error) {
	plan, err := op.AccessPlanSelect(apiKeyID, accessPlanSlug, ctx)
	if err != nil {
		return routeSelection{}, err
	}

	var originalRule *dbmodel.AccessRouteRule
	for _, candidate := range modelTestRouteCandidates(requestModel) {
		if plan != nil {
			group, rule, ok, err := op.AccessPlanGroupForModel(plan, candidate, ctx)
			if err != nil {
				return routeSelection{}, err
			}
			if rule != nil && strings.EqualFold(strings.TrimSpace(candidate), strings.TrimSpace(requestModel)) {
				originalRule = rule
			}
			if ok {
				return routeSelection{
					group:     group,
					plan:      plan,
					rule:      rule,
					routeUsed: true,
				}, nil
			}
			if rule != nil && strings.EqualFold(strings.TrimSpace(candidate), strings.TrimSpace(requestModel)) && rule.FallbackMode == dbmodel.AccessRouteFallbackNone {
				return routeSelection{plan: plan, rule: rule}, fmt.Errorf("route for %q has no enabled targets", requestModel)
			}
		}

		group, err := op.GroupGetEnabledMap(candidate, ctx)
		if err == nil {
			return routeSelection{
				group: group,
				plan:  plan,
				rule:  originalRule,
			}, nil
		}
	}
	return routeSelection{plan: plan, rule: originalRule}, fmt.Errorf("model %q not found in route or group", requestModel)
}

func modelTestRouteCandidates(requestModel string) []string {
	candidates := []string{requestModel}
	candidates = append(candidates, transformermodel.AnthropicModelAliasCandidates(requestModel)...)
	seen := map[string]bool{}
	result := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		key := strings.ToLower(candidate)
		if seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, candidate)
	}
	return result
}

func shouldFallbackToGroup(selection routeSelection) bool {
	return selection.routeUsed &&
		selection.rule != nil &&
		selection.rule.FallbackMode == dbmodel.AccessRouteFallbackReturnGroup
}

func (r *modelRunner) applyRouteMetadata(selection routeSelection) {
	r.result.RouteUsed = selection.routeUsed
	r.result.GroupName = selection.group.Name
	if selection.plan != nil {
		r.result.AccessPlanID = selection.plan.ID
		r.result.AccessPlanSlug = selection.plan.Slug
		r.result.AccessPlanName = selection.plan.DisplayName
	}
}

func (r *modelRunner) trySelection(ctx context.Context, selection routeSelection) bool {
	group := enrichTestGroupForSmartRouting(ctx, selection.group)
	candidates := balancer.GetBalancer(group.Mode).Candidates(group.Items)
	if len(candidates) == 0 {
		r.recordAttempt(0, 0, "", "", "", dbmodel.AttemptSkipped, 0, 0, "no route candidates")
		return false
	}

	for _, item := range candidates {
		channel, err := op.ChannelGet(item.ChannelID, ctx)
		if err != nil {
			r.recordAttempt(item.ChannelID, 0, fmt.Sprintf("channel_%d", item.ChannelID), item.ModelName, "", dbmodel.AttemptSkipped, 0, 0, err.Error())
			continue
		}
		if r.tryChannel(ctx, channel, item.ModelName, true) {
			return true
		}
	}
	return false
}

func enrichTestGroupForSmartRouting(ctx context.Context, group dbmodel.Group) dbmodel.Group {
	if group.Mode != dbmodel.GroupModeSmart || len(group.Items) == 0 {
		return group
	}
	items := make([]dbmodel.GroupItem, len(group.Items))
	copy(items, group.Items)
	for i := range items {
		channel, err := op.ChannelGet(items[i].ChannelID, ctx)
		if err == nil && channel != nil {
			items[i].ChannelPriority = channel.Priority
		}
		items[i].ChannelStats = op.StatsChannelGet(items[i].ChannelID)
	}
	group.Items = items
	return group
}

func (r *modelRunner) tryChannel(ctx context.Context, channel *dbmodel.Channel, upstreamModel string, respectEnabled bool) bool {
	channelName := channelDisplayName(channel)
	if channel == nil {
		r.recordAttempt(0, 0, channelName, upstreamModel, "", dbmodel.AttemptSkipped, 0, 0, "channel is nil")
		return false
	}
	if respectEnabled && !channel.Enabled {
		r.recordAttempt(channel.ID, 0, channelName, upstreamModel, "", dbmodel.AttemptSkipped, 0, 0, "channel disabled")
		return false
	}
	if !outbound.IsChatChannelType(channel.Type) {
		r.recordAttempt(channel.ID, 0, channelName, upstreamModel, "", dbmodel.AttemptSkipped, 0, 0, fmt.Sprintf("channel type %d does not support model connectivity tests", channel.Type))
		return false
	}
	if strings.TrimSpace(channel.GetBaseUrl()) == "" {
		r.recordAttempt(channel.ID, 0, channelName, upstreamModel, "", dbmodel.AttemptSkipped, 0, 0, "channel has no base url")
		return false
	}

	keys := channel.GetAvailableChannelKeys()
	if len(keys) == 0 {
		r.recordAttempt(channel.ID, 0, channelName, upstreamModel, "", dbmodel.AttemptSkipped, 0, 0, "no available key")
		return false
	}

	adapter := outbound.Get(channel.Type)
	if adapter == nil {
		r.recordAttempt(channel.ID, 0, channelName, upstreamModel, "", dbmodel.AttemptSkipped, 0, 0, fmt.Sprintf("unsupported channel type: %d", channel.Type))
		return false
	}
	if !r.ensureProxyConnectivity(ctx, channel, upstreamModel) {
		return false
	}

	for _, key := range keys {
		started := time.Now()
		status, parsed, err := r.testChannelKey(ctx, adapter, channel, key, upstreamModel)
		duration := int(time.Since(started).Milliseconds())
		if err == nil {
			r.succeed(channel, key, upstreamModel, status, duration, parsed)
			return true
		}
		r.recordAttempt(channel.ID, key.ID, channelName, upstreamModel, r.result.UpstreamPath, dbmodel.AttemptFailed, status, duration, err.Error())
		r.result.Error = xredact.Secrets(err.Error())
		r.result.StatusCode = status
		if status != http.StatusTooManyRequests {
			break
		}
	}
	return false
}

func (r *modelRunner) testChannelKey(ctx context.Context, adapter transformermodel.Outbound, channel *dbmodel.Channel, key dbmodel.ChannelKey, upstreamModel string) (int, *transformermodel.InternalLLMResponse, error) {
	internalRequest := r.internalRequest(upstreamModel)
	if channel.Type == outbound.OutboundTypeAnthropic {
		if channel.AnthropicContext1M {
			internalRequest.TransformOptions.AnthropicOneMillionBeta = true
		}
		if enabled, err := op.SettingGetBool(dbmodel.SettingKeyAnthropicAutoCacheControl); err == nil {
			internalRequest.TransformOptions.AnthropicAutoCacheControl = enabled
		}
		prepareClaudeOneMillionModelTestShape(internalRequest, r.request)
	}
	if channel.Type == outbound.OutboundTypeOpenAIResponse {
		endpoint, _ := normalizeEndpoint(r.request.Endpoint)
		if endpoint.name == "openai_responses" {
			stream := true
			internalRequest.Stream = &stream
		}
	}
	if modelTestUsesCodexFingerprint(channel, r.request.Endpoint) {
		prepareCodexModelTestRequest(internalRequest, channel.Type, r.request)
	}

	outboundRequest, err := adapter.TransformRequest(ctx, internalRequest, modelTestOutboundBaseURL(channel), key.ChannelKey)
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	if outboundRequest != nil && outboundRequest.URL != nil {
		r.result.UpstreamPath = outboundRequest.URL.EscapedPath()
		if r.result.UpstreamPath == "" {
			r.result.UpstreamPath = outboundRequest.URL.Path
		}
	}
	applyHeaderDefaults(outboundRequest, channel, r.request.Endpoint, internalRequest)
	applyCustomHeaders(outboundRequest, channel.CustomHeader)
	if err := applyParamOverride(outboundRequest, channel.ParamOverride); err != nil {
		return 0, nil, err
	}

	client, err := helper.ChannelHttpClient(channel)
	if err != nil {
		return 0, nil, err
	}

	response, err := helper.DoPreserveMethodRedirect(client, outboundRequest)
	if err != nil {
		return 0, nil, fmt.Errorf("send request: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, upstreamBodyLimit))
		code, message := upstreamErrorSummary(body)
		if message == "" {
			message = xredact.Secrets(string(body))
		}
		if message == "" {
			message = http.StatusText(response.StatusCode)
		}
		if code != "" {
			r.result.ErrorCode = code
		}
		return response.StatusCode, nil, fmt.Errorf("upstream status %d: %s", response.StatusCode, message)
	}

	var parsed *transformermodel.InternalLLMResponse
	if internalRequest.Stream != nil && *internalRequest.Stream {
		parsed, err = transformModelTestStream(ctx, adapter, response)
	} else {
		parsed, err = adapter.TransformResponse(ctx, response)
	}
	if err != nil {
		return response.StatusCode, nil, fmt.Errorf("parse response: %w", err)
	}
	if parsed == nil || !parsed.IsChatResponse() {
		return response.StatusCode, parsed, fmt.Errorf("upstream returned no chat choices")
	}
	if responsePreview(parsed) == "" {
		return response.StatusCode, parsed, fmt.Errorf("upstream returned empty response text")
	}
	return response.StatusCode, parsed, nil
}

func modelTestOutboundBaseURL(channel *dbmodel.Channel) string {
	if channel == nil {
		return ""
	}
	if channel.Type == outbound.OutboundTypeCustomOpenAIChat {
		return channel.GetOpenAIChatBaseUrl()
	}
	return channel.GetBaseUrl()
}

func (r *modelRunner) internalRequest(upstreamModel string) *transformermodel.InternalLLMRequest {
	prompt := strings.TrimSpace(r.request.Prompt)
	if prompt == "" || prompt == defaultPromptSentinel {
		prompt = nextDefaultModelTestPrompt()
	}
	maxTokens := int64(8)
	stream := false
	if r.request.Stream != nil {
		stream = *r.request.Stream
	}
	temperature := 0.0

	endpoint, _ := normalizeEndpoint(r.request.Endpoint)
	if endpoint.name == "anthropic_messages" {
		stream = true
	}
	internalRequest := &transformermodel.InternalLLMRequest{
		Model: upstreamModel,
		Messages: []transformermodel.Message{{
			Role: "user",
			Content: transformermodel.MessageContent{
				Content: &prompt,
			},
		}},
		MaxTokens:    &maxTokens,
		Stream:       &stream,
		Temperature:  &temperature,
		RawAPIFormat: endpoint.apiFormat,
	}
	if endpoint.name == "anthropic_messages" && (transformermodel.AnthropicModelWantsOneMillionBeta(upstreamModel) || transformermodel.AnthropicModelWantsOneMillionBeta(r.result.RequestModel)) {
		internalRequest.TransformOptions.AnthropicOneMillionBeta = true
	}
	return internalRequest
}

func nextDefaultModelTestPrompt() string {
	n := atomic.AddUint64(&defaultPromptCounter, 1)
	seed := uint64(time.Now().UnixNano()) ^ (n * 0x9e3779b97f4a7c15)
	left := 1000 + int(seed%9000)
	right := 1000 + int((seed/9000)%9000)
	return fmt.Sprintf("请只回答算式结果，不要解释：%d + %d = ?", left, right)
}

func (r *modelRunner) succeed(channel *dbmodel.Channel, key dbmodel.ChannelKey, upstreamModel string, status int, duration int, response *transformermodel.InternalLLMResponse) {
	channelName := channelDisplayName(channel)
	r.recordAttempt(channel.ID, key.ID, channelName, upstreamModel, r.result.UpstreamPath, dbmodel.AttemptSuccess, status, duration, r.proxyAttemptMessage())
	r.result.Success = true
	r.result.Error = ""
	r.result.ErrorCode = ""
	r.result.StatusCode = status
	r.result.ChannelID = channel.ID
	r.result.ChannelName = channelName
	r.result.ChannelKeyID = key.ID
	r.result.UpstreamModel = upstreamModel
	r.result.ResponsePreview = responsePreview(response)
	if response != nil && response.Usage != nil {
		r.result.InputTokens = response.Usage.PromptTokens
		r.result.OutputTokens = response.Usage.CompletionTokens
	}
}

func channelDisplayName(channel *dbmodel.Channel) string {
	if channel == nil {
		return "channel"
	}
	if strings.TrimSpace(channel.Name) != "" {
		return channel.Name
	}
	if channel.ID > 0 {
		return fmt.Sprintf("channel_%d", channel.ID)
	}
	return "current channel"
}

func (r *modelRunner) fail(err error) {
	r.result.Success = false
	r.result.Error = xredact.Secrets(err.Error())
}

func (r *modelRunner) ensureProxyConnectivity(ctx context.Context, channel *dbmodel.Channel, upstreamModel string) bool {
	if channel == nil || !channel.Proxy {
		return true
	}
	started := time.Now()
	info, status, err := helper.CheckChannelProxyConnectivity(ctx, channel)
	duration := int(time.Since(started).Milliseconds())
	r.applyProxyInfo(info, status)
	if err == nil {
		return true
	}
	msg := err.Error()
	if strings.TrimSpace(info.Description) != "" {
		msg = fmt.Sprintf("%s: %s", info.Description, msg)
	}
	r.recordAttempt(channel.ID, 0, channelDisplayName(channel), upstreamModel, "", dbmodel.AttemptFailed, status, duration, fmt.Sprintf("proxy connectivity failed: %s", msg))
	r.result.Error = xredact.Secrets(fmt.Sprintf("proxy connectivity failed: %s", msg))
	r.result.ErrorCode = "proxy_connectivity_failed"
	r.result.StatusCode = status
	return false
}

func (r *modelRunner) applyProxyInfo(info helper.ChannelProxyInfo, status int) {
	if !info.Used {
		return
	}
	r.result.ProxyUsed = true
	r.result.ProxySource = info.Source
	r.result.ProxyScheme = info.Scheme
	r.result.ProxyTarget = info.Description
	r.result.ProxyStatus = status
}

func (r *modelRunner) proxyAttemptMessage() string {
	if !r.result.ProxyUsed {
		return ""
	}
	msg := "proxy connectivity ok"
	if strings.TrimSpace(r.result.ProxyTarget) != "" {
		msg += ": " + r.result.ProxyTarget
	}
	if r.result.ProxyStatus != 0 {
		msg += fmt.Sprintf(" (status %d)", r.result.ProxyStatus)
	}
	return msg
}

func (r *modelRunner) recordAttempt(channelID, keyID int, channelName, modelName, upstreamPath string, status dbmodel.AttemptStatus, statusCode int, duration int, msg string) {
	r.attempt++
	r.result.Attempts = append(r.result.Attempts, dbmodel.ChannelAttempt{
		ChannelID:    channelID,
		ChannelKeyID: keyID,
		ChannelName:  channelName,
		ModelName:    modelName,
		UpstreamPath: upstreamPath,
		AttemptNum:   r.attempt,
		Status:       status,
		Duration:     duration,
		Msg:          xredact.Secrets(msg),
		ProxyUsed:    r.result.ProxyUsed,
		ProxySource:  r.result.ProxySource,
		ProxyScheme:  r.result.ProxyScheme,
		ProxyTarget:  r.result.ProxyTarget,
		ProxyStatus:  r.result.ProxyStatus,
	})
	if statusCode != 0 && r.result.StatusCode == 0 {
		r.result.StatusCode = statusCode
	}
}

func applyCustomHeaders(req *http.Request, headers []dbmodel.CustomHeader) {
	for _, header := range headers {
		key := strings.TrimSpace(header.HeaderKey)
		if key == "" {
			continue
		}
		req.Header.Set(key, header.HeaderValue)
	}
}

func applyHeaderDefaults(req *http.Request, channel *dbmodel.Channel, endpointName string, internalRequest *transformermodel.InternalLLMRequest) {
	if req == nil || channel == nil {
		return
	}
	if !shouldApplyChannelCloak(channel.Cloak) {
		return
	}
	switch channel.Type {
	case outbound.OutboundTypeAnthropic:
		applyClaudeHeaderDefaults(req, internalRequest)
	case outbound.OutboundTypeOpenAIResponse:
		applyCodexHeaderDefaults(req, internalRequest)
	case outbound.OutboundTypeOpenAIChat, outbound.OutboundTypeCustomOpenAIChat:
		endpoint, err := normalizeEndpoint(endpointName)
		if err == nil && endpoint.name == "openai_responses" {
			applyCodexHeaderDefaults(req, internalRequest)
		}
	}
}

func shouldApplyChannelCloak(cloak dbmodel.ChannelCloak) bool {
	switch strings.ToLower(strings.TrimSpace(cloak.Mode)) {
	case "", "auto", "always":
		return true
	case "never", "off", "false", "disabled":
		return false
	default:
		return true
	}
}

func applyClaudeHeaderDefaults(req *http.Request, internalRequest *transformermodel.InternalLLMRequest) {
	ensureClaudeBetaQuery(req)
	setHeaderIfMissing(req.Header, "Anthropic-Dangerous-Direct-Browser-Access", "true")
	setHeaderIfMissing(req.Header, "Anthropic-Version", "2023-06-01")
	setHeaderIfMissing(req.Header, "User-Agent", settingString(dbmodel.SettingKeyClaudeHeaderUserAgent, defaultClaudeUserAgent))
	setHeaderIfMissing(req.Header, "X-App", "cli")
	setHeaderIfMissing(req.Header, "X-Client-Request-Id", uuid.NewString())
	setHeaderIfMissing(req.Header, "X-Claude-Code-Session-Id", modelTestClaudeSessionID(internalRequest))
	setHeaderIfMissing(req.Header, "X-Stainless-Lang", "js")
	setHeaderIfMissing(req.Header, "X-Stainless-Retry-Count", "0")
	setHeaderIfMissing(req.Header, "X-Stainless-Runtime", "node")
	setHeaderIfMissing(req.Header, "X-Stainless-Runtime-Version", settingString(dbmodel.SettingKeyClaudeHeaderRuntime, defaultClaudeRuntimeVersion))
	setHeaderIfMissing(req.Header, "X-Stainless-Package-Version", settingString(dbmodel.SettingKeyClaudeHeaderPackage, defaultClaudePackageVersion))
	setHeaderIfMissing(req.Header, "X-Stainless-Timeout", settingString(dbmodel.SettingKeyClaudeHeaderTimeout, defaultClaudeTimeout))
	if settingBool(dbmodel.SettingKeyClaudeHeaderStabilize, true) {
		setHeaderIfMissing(req.Header, "X-Stainless-OS", settingString(dbmodel.SettingKeyClaudeHeaderOS, defaultClaudeOS))
		setHeaderIfMissing(req.Header, "X-Stainless-Arch", settingString(dbmodel.SettingKeyClaudeHeaderArch, defaultClaudeArch))
	}
	if internalRequest != nil {
		for _, beta := range internalRequest.TransformOptions.AnthropicBetas {
			addAnthropicBetaHeader(req.Header, beta)
		}
	}
	for _, beta := range transformermodel.AnthropicClaudeCodeBetas(shouldEnableClaudeOneMillionBeta(internalRequest)) {
		addAnthropicBetaHeader(req.Header, beta)
	}
}

func ensureClaudeBetaQuery(req *http.Request) {
	if req == nil || req.URL == nil {
		return
	}
	q := req.URL.Query()
	if strings.TrimSpace(q.Get("beta")) == "" {
		q.Set("beta", "true")
		req.URL.RawQuery = q.Encode()
	}
}

func modelTestClaudeSessionID(internalRequest *transformermodel.InternalLLMRequest) string {
	if internalRequest != nil && internalRequest.PromptCacheKey != nil {
		if value := strings.TrimSpace(*internalRequest.PromptCacheKey); value != "" {
			sum := sha256.Sum256([]byte("model-test-claude-session:" + value))
			return hex.EncodeToString(sum[:16])
		}
	}
	return uuid.NewString()
}

func shouldEnableClaudeOneMillionBeta(internalRequest *transformermodel.InternalLLMRequest) bool {
	return transformermodel.AnthropicRequestWantsOneMillionBeta(internalRequest)
}

func addAnthropicBetaHeader(headers http.Header, beta string) {
	beta = strings.TrimSpace(beta)
	if beta == "" {
		return
	}
	existing := strings.Split(headers.Get("Anthropic-Beta"), ",")
	seen := map[string]bool{}
	values := make([]string, 0, len(existing)+1)
	for _, item := range existing {
		item = strings.TrimSpace(item)
		if item == "" || seen[strings.ToLower(item)] {
			continue
		}
		seen[strings.ToLower(item)] = true
		values = append(values, item)
	}
	if !seen[strings.ToLower(beta)] {
		values = append(values, beta)
	}
	headers.Set("Anthropic-Beta", strings.Join(values, ","))
}

func applyCodexHeaderDefaults(req *http.Request, internalRequest *transformermodel.InternalLLMRequest) {
	setHeaderIfMissing(req.Header, "Connection", "Keep-Alive")
	setHeaderIfMissing(req.Header, "Content-Type", "application/json")
	setHeaderIfMissing(req.Header, "Originator", defaultCodexOriginator)
	setHeaderIfMissing(req.Header, "User-Agent", settingString(dbmodel.SettingKeyCodexHeaderUserAgent, defaultCodexUserAgent))
	setHeaderIfMissing(req.Header, "X-Codex-Beta-Features", settingString(dbmodel.SettingKeyCodexHeaderBetaFeatures, defaultCodexBetaFeatures))
	applyCodexSessionHeaderDefaults(req.Header, internalRequest)
}

func modelTestUsesCodexFingerprint(channel *dbmodel.Channel, endpointName string) bool {
	if channel == nil || !shouldApplyChannelCloak(channel.Cloak) {
		return false
	}
	endpoint, err := normalizeEndpoint(endpointName)
	if err != nil || endpoint.name != "openai_responses" {
		return false
	}
	switch channel.Type {
	case outbound.OutboundTypeOpenAIResponse, outbound.OutboundTypeOpenAIChat, outbound.OutboundTypeCustomOpenAIChat:
		return true
	default:
		return false
	}
}

func prepareCodexModelTestRequest(req *transformermodel.InternalLLMRequest, channelType outbound.OutboundType, request dbmodel.ModelTestRequest) {
	if req == nil {
		return
	}
	sessionID := ""
	if req.PromptCacheKey != nil {
		sessionID = strings.TrimSpace(*req.PromptCacheKey)
	}
	if sessionID == "" {
		sessionID = uuid.NewString()
		req.PromptCacheKey = &sessionID
	}
	if len(req.ClientMetadata) == 0 {
		metadata := map[string]any{
			"x-codex-installation-id": codexModelTestInstallationID(request),
			"x-codex-window-id":       sessionID + ":0",
			"x-codex-turn-metadata":   codexModelTestTurnMetadata(sessionID),
		}
		req.ClientMetadata, _ = json.Marshal(metadata)
	}
	if channelType != outbound.OutboundTypeOpenAIResponse {
		return
	}
	prepareCodexModelTestShape(req)
}

func prepareClaudeOneMillionModelTestShape(req *transformermodel.InternalLLMRequest, request dbmodel.ModelTestRequest) {
	if req == nil || !transformermodel.AnthropicRequestWantsOneMillionBeta(req) {
		return
	}
	maxTokens := int64(64000)
	req.MaxTokens = &maxTokens
	if effort := claudeCLIReasoningEffort(); effort != "" && req.ReasoningEffort == "" && !req.AdaptiveThinking {
		req.ReasoningEffort = effort
		req.AdaptiveThinking = true
	}
	if settingBool(dbmodel.SettingKeyClaudeCLIAutoCompact, false) && len(req.AnthropicContextManagement) == 0 {
		req.AnthropicContextManagement = json.RawMessage(`{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}`)
	}
	sessionID := ""
	if req.PromptCacheKey != nil {
		sessionID = strings.TrimSpace(*req.PromptCacheKey)
	}
	if sessionID == "" {
		sessionID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("octopus:claude-1m:model-test:"+fmt.Sprintf("user=%d|api_key=%d|model=%s", request.UserID, request.APIKeyID, req.Model))).String()
		req.PromptCacheKey = &sessionID
	}
	if !modelTestMessagesContainInstruction(req.Messages) {
		content := "You are a Claude agent, built on Anthropic's Claude Agent SDK."
		req.Messages = append([]transformermodel.Message{{
			Role: "system",
			Content: transformermodel.MessageContent{
				Content: &content,
			},
		}}, req.Messages...)
	}
	if req.Metadata == nil {
		req.Metadata = map[string]string{}
	}
	if strings.TrimSpace(req.Metadata["user_id"]) == "" {
		userMeta := map[string]string{
			"device_id":    modelTestStableID("device", sessionID),
			"account_uuid": "",
			"session_id":   sessionID,
		}
		if encoded, err := json.Marshal(userMeta); err == nil {
			req.Metadata["user_id"] = string(encoded)
		}
	}
}

func modelTestStableID(prefix string, value string) string {
	sum := sha256.Sum256([]byte("model-test:" + prefix + ":" + strings.TrimSpace(value)))
	return hex.EncodeToString(sum[:16])
}

func codexModelTestInstallationID(request dbmodel.ModelTestRequest) string {
	seed := fmt.Sprintf("user=%d|api_key=%d", request.UserID, request.APIKeyID)
	if request.UserID == 0 && request.APIKeyID == 0 {
		seed = "admin-model-test"
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("octopus:codex:model-test-installation:"+seed)).String()
}

func prepareCodexModelTestShape(req *transformermodel.InternalLLMRequest) {
	addCodexModelTestInclude(req)
	if req.Store == nil {
		store := false
		req.Store = &store
	}
	if settingBool(dbmodel.SettingKeyCodexFastMode, false) && len(req.ResponsesTextRaw) == 0 {
		req.ResponsesTextRaw = json.RawMessage(`{"verbosity":"low"}`)
	}
	if settingBool(dbmodel.SettingKeyCodexFastMode, false) && strings.TrimSpace(req.ReasoningEffort) == "" {
		req.ReasoningEffort = "low"
	}
	if req.ResponsesInstructions == nil && !modelTestMessagesContainInstruction(req.Messages) {
		content := defaultCodexInstructions
		req.Messages = append([]transformermodel.Message{{
			Role: "system",
			Content: transformermodel.MessageContent{
				Content: &content,
			},
		}}, req.Messages...)
	}
	if len(req.Tools) == 0 && len(req.ResponsesToolsRaw) == 0 {
		req.Tools = defaultCodexModelTestTools()
	}
	if req.ToolChoice == nil && len(req.ResponsesToolChoiceRaw) == 0 && len(req.Tools) > 0 {
		choice := "auto"
		req.ToolChoice = &transformermodel.ToolChoice{ToolChoice: &choice}
	}
	if len(req.ResponsesInputRaw) == 0 {
		req.ResponsesInputRaw = synthesizeCodexModelTestInput(req.Messages)
	}
}

func addCodexModelTestInclude(req *transformermodel.InternalLLMRequest) {
	for _, item := range req.Include {
		if strings.EqualFold(strings.TrimSpace(item), "reasoning.encrypted_content") {
			return
		}
	}
	req.Include = append(req.Include, "reasoning.encrypted_content")
}

func modelTestMessagesContainInstruction(messages []transformermodel.Message) bool {
	for _, msg := range messages {
		switch strings.TrimSpace(msg.Role) {
		case "system", "developer":
			return true
		}
	}
	return false
}

func defaultCodexModelTestTools() []transformermodel.Tool {
	return []transformermodel.Tool{
		codexModelTestTool("shell_command", "Runs a Powershell command (Windows) and returns its output.", `{"type":"object","additionalProperties":false,"properties":{"command":{"type":"string"}},"required":["command"]}`),
		codexModelTestTool("update_plan", "Updates the task plan.", `{"type":"object","additionalProperties":false,"properties":{"plan":{"type":"array","items":{"type":"object"}}},"required":["plan"]}`),
		codexModelTestTool("request_user_input", "Request user input and wait for the response.", `{"type":"object","additionalProperties":false,"properties":{"questions":{"type":"array","items":{"type":"object"}}},"required":["questions"]}`),
		codexModelTestTool("view_image", "View a local image from the filesystem.", `{"type":"object","additionalProperties":false,"properties":{"path":{"type":"string"}},"required":["path"]}`),
	}
}

func codexModelTestTool(name, description, parameters string) transformermodel.Tool {
	strict := false
	return transformermodel.Tool{
		Type: "function",
		Function: transformermodel.Function{
			Name:        name,
			Description: description,
			Parameters:  json.RawMessage(parameters),
			Strict:      &strict,
		},
	}
}

func synthesizeCodexModelTestInput(messages []transformermodel.Message) json.RawMessage {
	items := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		role := strings.TrimSpace(msg.Role)
		if role == "system" || role == "developer" {
			continue
		}
		text := strings.TrimSpace(messageText(&msg))
		if text == "" {
			continue
		}
		if role == "" || role == "tool" || role == "function" {
			role = "user"
		}
		contentType := "input_text"
		if role == "assistant" {
			contentType = "output_text"
		}
		items = append(items, map[string]any{
			"type": "message",
			"role": role,
			"content": []map[string]any{{
				"type": contentType,
				"text": text,
			}},
		})
	}
	if len(items) == 0 {
		return nil
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return nil
	}
	return raw
}

func applyCodexSessionHeaderDefaults(headers http.Header, req *transformermodel.InternalLLMRequest) {
	if headers == nil || req == nil || req.PromptCacheKey == nil {
		return
	}
	sessionID := strings.TrimSpace(*req.PromptCacheKey)
	if sessionID == "" {
		return
	}
	setHeaderIfMissing(headers, "Session_id", sessionID)
	setHeaderIfMissing(headers, "Session-Id", sessionID)
	setHeaderIfMissing(headers, "X-Session-Id", sessionID)
	setHeaderIfMissing(headers, "Thread-Id", sessionID)
	setHeaderIfMissing(headers, "X-Client-Request-Id", sessionID)
	setHeaderIfMissing(headers, "X-Codex-Window-Id", sessionID+":0")
	setHeaderIfMissing(headers, "X-Codex-Turn-Metadata", codexModelTestTurnMetadata(sessionID))
}

func codexModelTestTurnMetadata(sessionID string) string {
	metadata := map[string]any{
		"session_id":              sessionID,
		"thread_id":               sessionID,
		"thread_source":           "user",
		"turn_id":                 uuid.NewString(),
		"workspaces":              map[string]any{},
		"sandbox":                 "none",
		"turn_started_at_unix_ms": time.Now().UnixMilli(),
	}
	out, _ := json.Marshal(metadata)
	return string(out)
}

func setHeaderIfMissing(headers http.Header, key, value string) {
	value = strings.TrimSpace(value)
	if value == "" || headers.Get(key) != "" {
		return
	}
	headers.Set(key, value)
}

func claudeCLIReasoningEffort() string {
	effort := strings.ToLower(strings.TrimSpace(settingString(dbmodel.SettingKeyClaudeCLIReasoningEffort, "auto")))
	switch effort {
	case "", "auto", "off", "false", "disabled":
		return ""
	case "low", "medium", "high":
		return effort
	default:
		return ""
	}
}

func settingString(key dbmodel.SettingKey, fallback string) string {
	value, err := op.SettingGetString(key)
	if err != nil {
		return fallback
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	if key == dbmodel.SettingKeyClaudeHeaderUserAgent && value == "claude-cli/2.1.126 (external, claude-vscode, agent-sdk/0.2.126)" {
		return fallback
	}
	if key == dbmodel.SettingKeyClaudeHeaderPackage && value == "0.81.0" {
		return fallback
	}
	return value
}

func settingBool(key dbmodel.SettingKey, fallback bool) bool {
	value, err := op.SettingGetBool(key)
	if err != nil {
		return fallback
	}
	return value
}

func applyParamOverride(req *http.Request, override *string) error {
	if override == nil || strings.TrimSpace(*override) == "" || req.Body == nil {
		return nil
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		return fmt.Errorf("read request body for param_override: %w", err)
	}
	restore := func(data []byte) {
		req.Body = io.NopCloser(bytes.NewReader(data))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(data)), nil
		}
		req.ContentLength = int64(len(data))
	}

	var bodyMap map[string]any
	if err := json.Unmarshal(body, &bodyMap); err != nil {
		restore(body)
		return nil
	}
	var overrideMap map[string]any
	if err := json.Unmarshal([]byte(*override), &overrideMap); err != nil {
		restore(body)
		return nil
	}
	for key, value := range overrideMap {
		bodyMap[key] = value
	}
	updated, err := json.Marshal(bodyMap)
	if err != nil {
		restore(body)
		return nil
	}
	restore(updated)
	return nil
}

func upstreamErrorSummary(body []byte) (string, string) {
	var payload struct {
		Error any `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", summarizeNonJSONErrorBody(body)
	}

	switch value := payload.Error.(type) {
	case map[string]any:
		code := stringify(value["code"])
		message := xredact.Secrets(stringify(value["message"]))
		if message == "" {
			message = xredact.Secrets(stringify(value["type"]))
		}
		return code, message
	case string:
		return "", xredact.Secrets(value)
	default:
		return "", ""
	}
}

func transformModelTestStream(ctx context.Context, adapter transformermodel.Outbound, response *http.Response) (*transformermodel.InternalLLMResponse, error) {
	if response == nil || response.Body == nil {
		return nil, fmt.Errorf("response is nil")
	}
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	if !strings.Contains(contentType, "text/event-stream") {
		return adapter.TransformResponse(ctx, response)
	}

	var text strings.Builder
	var usage *transformermodel.Usage
	readCfg := &sse.ReadConfig{MaxEventSize: 1024 * 1024}
	for ev, err := range sse.Read(response.Body, readCfg) {
		if err != nil {
			return nil, err
		}
		streamResp, err := adapter.TransformStream(ctx, []byte(ev.Data))
		if err != nil {
			return nil, err
		}
		if streamResp == nil {
			continue
		}
		if streamResp.Usage != nil {
			usage = streamResp.Usage
		}
		for _, choice := range streamResp.Choices {
			if choice.Message != nil {
				if content := messageText(choice.Message); content != "" {
					text.WriteString(content)
				}
			}
			if choice.Delta != nil {
				if content := messageText(choice.Delta); content != "" {
					text.WriteString(content)
				}
			}
		}
		if modelTestStreamTerminalEvent(ev.Type, ev.Data, streamResp) {
			break
		}
	}

	content := strings.TrimSpace(text.String())
	if content == "" {
		return nil, fmt.Errorf("upstream stream returned empty response text")
	}
	message := &transformermodel.Message{
		Role: "assistant",
		Content: transformermodel.MessageContent{
			Content: &content,
		},
	}
	finishReason := "stop"
	return &transformermodel.InternalLLMResponse{
		Choices: []transformermodel.Choice{{
			Index:        0,
			Message:      message,
			FinishReason: &finishReason,
		}},
		Usage: usage,
	}, nil
}

func modelTestStreamTerminalEvent(eventType string, data string, resp *transformermodel.InternalLLMResponse) bool {
	if resp != nil && resp.Object == "[DONE]" {
		return true
	}
	if strings.HasPrefix(strings.TrimSpace(data), "[DONE]") {
		return true
	}
	switch strings.TrimSpace(eventType) {
	case "message_stop", "response.completed":
		return true
	}
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &envelope); err != nil {
		return false
	}
	switch strings.TrimSpace(envelope.Type) {
	case "message_stop", "response.completed":
		return true
	default:
		return false
	}
}

func summarizeNonJSONErrorBody(body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return ""
	}
	lower := strings.ToLower(text)
	if strings.Contains(lower, "<html") || strings.Contains(lower, "<body") || strings.Contains(lower, "<h1") {
		title := firstBetween(text, "<title>", "</title>")
		if title == "" {
			title = firstBetween(text, "<h1>", "</h1>")
		}
		server := lastBetween(text, "<center>", "</center>")
		if title != "" {
			if server != "" && !strings.EqualFold(server, title) {
				return xredact.Secrets(fmt.Sprintf("upstream returned HTML error page: %s (%s)", cleanHTMLText(title), cleanHTMLText(server)))
			}
			return xredact.Secrets(fmt.Sprintf("upstream returned HTML error page: %s", cleanHTMLText(title)))
		}
		return "upstream returned HTML error page"
	}
	return xredact.Secrets(trimPreview(text))
}

func firstBetween(value, start, end string) string {
	lower := strings.ToLower(value)
	startIdx := strings.Index(lower, strings.ToLower(start))
	if startIdx < 0 {
		return ""
	}
	startIdx += len(start)
	endIdx := strings.Index(lower[startIdx:], strings.ToLower(end))
	if endIdx < 0 {
		return ""
	}
	return strings.TrimSpace(value[startIdx : startIdx+endIdx])
}

func lastBetween(value, start, end string) string {
	lower := strings.ToLower(value)
	startLower := strings.ToLower(start)
	endLower := strings.ToLower(end)
	searchFrom := 0
	last := ""
	for {
		startIdx := strings.Index(lower[searchFrom:], startLower)
		if startIdx < 0 {
			return last
		}
		startIdx += searchFrom + len(start)
		endIdx := strings.Index(lower[startIdx:], endLower)
		if endIdx < 0 {
			return last
		}
		last = strings.TrimSpace(value[startIdx : startIdx+endIdx])
		searchFrom = startIdx + endIdx + len(end)
	}
}

func cleanHTMLText(value string) string {
	value = stripHTMLTags(value)
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "\r", " ")
	return strings.Join(strings.Fields(value), " ")
}

func stripHTMLTags(value string) string {
	var out strings.Builder
	inTag := false
	for _, r := range value {
		switch r {
		case '<':
			inTag = true
		case '>':
			inTag = false
		default:
			if !inTag {
				out.WriteRune(r)
			}
		}
	}
	return out.String()
}

func stringify(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case float64:
		return fmt.Sprintf("%.0f", v)
	case json.Number:
		return v.String()
	default:
		return ""
	}
}

func responsePreview(response *transformermodel.InternalLLMResponse) string {
	if response == nil {
		return ""
	}
	for _, choice := range response.Choices {
		if choice.Message != nil {
			if text := messageText(choice.Message); text != "" {
				return trimPreview(text)
			}
		}
		if choice.Delta != nil {
			if text := messageText(choice.Delta); text != "" {
				return trimPreview(text)
			}
		}
	}
	return ""
}

func messageText(message *transformermodel.Message) string {
	if message == nil {
		return ""
	}
	if message.Content.Content != nil {
		return strings.TrimSpace(*message.Content.Content)
	}
	parts := make([]string, 0, len(message.Content.MultipleContent))
	for _, part := range message.Content.MultipleContent {
		if part.Type == "text" && part.Text != nil {
			parts = append(parts, *part.Text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, ""))
}

func trimPreview(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 240 {
		return value
	}
	return value[:240] + "..."
}
