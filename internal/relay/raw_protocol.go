package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/helper"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/price"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	"github.com/bestruirui/octopus/internal/relay/bodycache"
	"github.com/bestruirui/octopus/internal/server/resp"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/bestruirui/octopus/internal/utils/log"
	"github.com/bestruirui/octopus/internal/utils/xurl"
	"github.com/gin-gonic/gin"
)

type RawProtocolOptions struct {
	Endpoint        string
	Name            string
	DefaultModel    string
	BinaryResponse  bool
	NonBilling      bool
	ResponseLogNote string
}

func RawProtocolHandler(options RawProtocolOptions, c *gin.Context) {
	options = normalizeRawProtocolOptions(options)
	ctx := c.Request.Context()

	apiKeyID := c.GetInt("api_key_id")
	userID := c.GetInt("user_id")

	bc, err := bodycache.New(c.Request.Body)
	if err != nil {
		var tooLarge *bodycache.BodyTooLargeError
		if errors.As(err, &tooLarge) {
			resp.Error(c, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() {
		if cerr := bc.Close(); cerr != nil {
			log.Warnf("failed to close raw protocol body cache: %v", cerr)
		}
	}()

	contentType := c.GetHeader("Content-Type")
	isMultipart := strings.Contains(strings.ToLower(contentType), "multipart/form-data")

	var (
		requestModel string
		stream       bool
		boundary     string
		jsonPayload  map[string]any
	)
	if isMultipart {
		_, params, perr := mime.ParseMediaType(contentType)
		if perr != nil {
			resp.Error(c, http.StatusBadRequest, "invalid multipart content-type")
			return
		}
		boundary = strings.TrimSpace(params["boundary"])
		if boundary == "" {
			resp.Error(c, http.StatusBadRequest, "invalid multipart boundary")
			return
		}
		m, s, perr := parseMultipartModelAndStreamWithDefault(bc, boundary, options.DefaultModel)
		if perr != nil {
			resp.Error(c, http.StatusBadRequest, perr.Error())
			return
		}
		requestModel = m
		stream = s
	} else {
		payload, m, s, perr := parseJSONModelAndStreamWithDefault(bc, options.DefaultModel)
		if perr != nil {
			resp.Error(c, http.StatusBadRequest, perr.Error())
			return
		}
		jsonPayload = payload
		requestModel = m
		stream = s
	}

	supportedModels := strings.TrimSpace(c.GetString("supported_models"))
	if !op.IsModelSupported(supportedModels, requestModel) {
		resp.Error(c, http.StatusBadRequest, "model not supported")
		return
	}

	routeResult, status, message, err := selectRouteGroup(c, apiKeyID, requestModel)
	if err != nil || status != 0 {
		if message == "" && err != nil {
			message = err.Error()
		}
		resp.Error(c, status, message)
		return
	}
	group := enrichGroupForSmartRouting(ctx, routeResult.Group, stream)
	clientSession := deriveClientSessionInfo(c.Request.Header, nil)
	if clientSession.Key == "" && jsonPayload != nil {
		clientSession = deriveRawProtocolClientSessionInfo(jsonPayload)
	}
	clientSessionKey := clientSession.Key

	iter := balancer.NewIteratorWithSessionKey(group, apiKeyID, requestModel, clientSessionKey)
	compactPreviousResponseID := ""
	if isResponsesCompactRawProtocol(options) {
		compactPreviousResponseID = rawProtocolPreviousResponseID(jsonPayload)
		prioritizeRawProtocolResponsesOwner(ctx, iter, compactPreviousResponseID)
	}
	if iter.Len() == 0 {
		resp.Error(c, http.StatusServiceUnavailable, "no available channel")
		return
	}

	metrics := newRawProtocolRelayMetrics(apiKeyID, userID, c.GetString("request_ip"), requestModel, options.Name, c.Request.URL.Path)
	metrics.SetAccessPlan(routeResult.AccessPlan, routeResult.AccessRouteRule, routeResult.AccessRouteUsed)
	metrics.SetClientSession(clientSession)
	metrics.RequestContent = buildRawProtocolRequestContent(isMultipart, bc, jsonPayload, options.Name)

	var (
		lastErr          error
		allAttempts      []model.ChannelAttempt
		triedReturnGroup bool
	)

runIterator:
	for iter.Next() {
		select {
		case <-ctx.Done():
			log.Infof("request context canceled, stopping raw protocol retry")
			metrics.Save(ctx, false, context.Canceled, append(allAttempts, iter.Attempts()...))
			return
		default:
		}

		item := iter.Item()

		channel, err := op.ChannelGet(item.ChannelID, ctx)
		if err != nil {
			log.Warnf("failed to get channel %d: %v", item.ChannelID, err)
			iter.Skip(item.ChannelID, 0, fmt.Sprintf("channel_%d", item.ChannelID), fmt.Sprintf("channel not found: %v", err))
			lastErr = err
			continue
		}
		if !channel.Enabled {
			iter.Skip(channel.ID, 0, channel.Name, "channel disabled")
			continue
		}
		if !isOpenAIWireChannelType(channel.Type) {
			iter.Skip(channel.ID, 0, channel.Name, fmt.Sprintf("unsupported channel type for raw protocol: %d", channel.Type))
			continue
		}

		availableKeys := channel.GetAvailableChannelKeys()
		if len(availableKeys) == 0 {
			iter.Skip(channel.ID, 0, channel.Name, "no available key")
			continue
		}
		preferredKeyID := 0
		if ownerKeyID := responsesOwnerKeyForChannel(ctx, compactPreviousResponseID, channel.ID); ownerKeyID > 0 {
			preferredKeyID = ownerKeyID
		} else if stickyKeyID := iter.StickyKeyIDForCurrentChannel(channel.ID); stickyKeyID > 0 {
			preferredKeyID = stickyKeyID
		}
		availableKeys = balancer.PrioritizeChannelKeysByHealth(availableKeys, channel.ID, item.ModelName, preferredKeyID)

		for keyIndex, usedKey := range availableKeys {
			if iter.SkipCircuitBreak(channel.ID, usedKey.ID, channel.Name) {
				continue
			}

			log.Infof("raw protocol %s request model %s, mode: %d, forwarding to channel: %s model: %s (attempt %d/%d, key %d/%d, sticky=%t, stream=%t)",
				options.Name, requestModel, group.Mode, channel.Name, item.ModelName,
				iter.Index()+1, iter.Len(), keyIndex+1, len(availableKeys), iter.IsSticky(), stream)

			span := iter.StartAttempt(channel.ID, usedKey.ID, channel.Name)
			forwardCompactCursor := true
			if isResponsesCompactRawProtocol(options) && strings.TrimSpace(compactPreviousResponseID) != "" {
				forwardCompactCursor = shouldForwardRawProtocolResponsesCursor(ctx, iter, compactPreviousResponseID, channel.ID, usedKey.ID, clientSession.Source)
			}
			statusCode, written, usage, upstreamCT, upstreamResponseID, fwdErr := func() (int, bool, *imagesUsage, string, string, error) {
				finishRuntimeAttempt := balancer.BeginRuntimeAttempt(channel.ID, usedKey.ID, item.ModelName)
				defer finishRuntimeAttempt()
				return rawProtocolAttempt(ctx, options, c, bc, isMultipart, boundary, jsonPayload, stream, channel, usedKey.ChannelKey, group.FirstTokenTimeOut, metrics, item.ModelName, clientSessionKey, forwardCompactCursor)
			}()
			usedAt := time.Now().Unix()

			if fwdErr == nil {
				metrics.ActualModel = item.ModelName
				if usage != nil && !options.NonBilling {
					metrics.SetUsage(item.ModelName, *usage)
				}
				metrics.ResponseContent = buildRawProtocolResponseContent(options, stream, upstreamCT, usage)
				if isResponsesCompactRawProtocol(options) && strings.TrimSpace(upstreamResponseID) != "" {
					recordResponsesSessionWithContext(ctx, upstreamResponseID, channel.ID, usedKey.ID)
				}

				costDelta := metrics.Stats.InputCost + metrics.Stats.OutputCost
				op.ChannelKeyRecordUse(usedKey, statusCode, usedAt, costDelta)

				span.End(model.AttemptSuccess, statusCode, "")
				balancer.RecordRuntimeSuccess(channel.ID, usedKey.ID, item.ModelName, balancer.AttemptRuntimeMetrics{
					Duration:     span.Duration(),
					FirstToken:   firstTokenDurationSince(metrics.FirstToken, span.StartedAt()),
					OutputTokens: metrics.Stats.OutputToken,
					Stream:       stream,
				})
				op.StatsChannelUpdate(channel.ID, model.StatsMetrics{
					WaitTime:       span.Duration().Milliseconds(),
					RequestSuccess: 1,
				})
				balancer.RecordSuccess(channel.ID, usedKey.ID, item.ModelName)
				balancer.SetStickyWithSessionKey(apiKeyID, requestModel, clientSessionKey, channel.ID, usedKey.ID)

				metrics.Save(ctx, true, nil, append(allAttempts, iter.Attempts()...))
				return
			}

			recordStatusCode := attemptStatusCode(statusCode, fwdErr)
			op.ChannelKeyRecordUse(usedKey, recordStatusCode, usedAt, 0)
			span.End(model.AttemptFailed, recordStatusCode, auditErrorMessage(fwdErr))
			breakerCounted := shouldRecordBreakerFailure(recordStatusCode, fwdErr)
			if breakerCounted {
				retryAfter, _ := retryAfterFromError(fwdErr)
				balancer.RecordRuntimeFailure(channel.ID, usedKey.ID, item.ModelName, recordStatusCode, span.Duration(), retryAfter)
			}
			channelStats := model.StatsMetrics{WaitTime: span.Duration().Milliseconds()}
			if breakerCounted {
				channelStats.RequestFailed = 1
			}
			op.StatsChannelUpdate(channel.ID, channelStats)
			if breakerCounted {
				balancer.RecordFailureWithStatus(channel.ID, usedKey.ID, item.ModelName, recordStatusCode)
			}

			if written || c.Writer.Written() {
				if isResponsesInboundPath(c.Request.URL.Path) {
					if message := responsesStreamFailureMessage(fwdErr); message != "" {
						writeResponsesFailedSSE(c, requestModel, "upstream_error", message)
					}
				}
				metrics.Save(ctx, false, fwdErr, append(allAttempts, iter.Attempts()...))
				return
			}

			lastErr = fmt.Errorf("channel %s failed: %w", channel.Name, fwdErr)
			if !shouldTryNextChannelKey(recordStatusCode) {
				break
			}
		}
	}

	allAttempts = append(allAttempts, iter.Attempts()...)
	if shouldReturnToOriginalGroup(routeResult, triedReturnGroup) {
		triedReturnGroup = true
		fallbackGroup, err := op.GroupGetEnabledMap(requestModel, ctx)
		if err != nil {
			lastErr = err
		} else {
			fallbackGroup = enrichGroupForSmartRouting(ctx, fallbackGroup, stream)
			fallbackIter := balancer.NewIteratorWithSessionKey(fallbackGroup, apiKeyID, requestModel, clientSessionKey)
			if isResponsesCompactRawProtocol(options) {
				prioritizeRawProtocolResponsesOwner(ctx, fallbackIter, compactPreviousResponseID)
			}
			if fallbackIter.Len() > 0 {
				group = fallbackGroup
				iter = fallbackIter
				goto runIterator
			}
		}
	}

	finalErr := lastErr
	if finalErr == nil {
		finalErr = routeSelectionErrorFromAttempts(allAttempts)
	}
	metrics.Save(ctx, false, finalErr, allAttempts)
	status, code, message := relayErrorResponse(finalErr)
	resp.ErrorWithCode(c, status, code, message)
}

func normalizeRawProtocolOptions(options RawProtocolOptions) RawProtocolOptions {
	options.Endpoint = "/" + strings.Trim(strings.TrimSpace(options.Endpoint), "/")
	if options.Name == "" {
		options.Name = strings.Trim(options.Endpoint, "/")
	}
	if options.ResponseLogNote == "" {
		options.ResponseLogNote = "response body omitted for storage"
	}
	return options
}

func isResponsesCompactRawProtocol(options RawProtocolOptions) bool {
	return strings.EqualFold(strings.TrimSpace(options.Name), "responses_compact") ||
		strings.EqualFold(strings.Trim(options.Endpoint, "/"), "responses/compact")
}

func deriveRawProtocolClientSessionInfo(payload map[string]any) clientSessionInfo {
	if len(payload) == 0 {
		return clientSessionInfo{}
	}
	if raw := rawProtocolJSONValue(payload["client_metadata"]); len(raw) > 0 {
		if info := deriveClientSessionInfoFromCodexClientMetadata(raw); info.Key != "" {
			return clientSessionInfo{
				Key:    info.Key,
				Source: "body:client_metadata" + strings.TrimPrefix(info.Source, "client_metadata"),
			}
		}
	}
	for _, key := range []string{"client_metadata", "metadata"} {
		metadata := rawProtocolStringMap(payload[key])
		if info := deriveClientSessionInfoFromMetadata(metadata); info.Key != "" {
			return clientSessionInfo{
				Key:    info.Key,
				Source: "body:" + key + strings.TrimPrefix(info.Source, "metadata"),
			}
		}
	}
	if value := normalizeRouteSessionValue(rawProtocolStringValue(payload["prompt_cache_key"])); value != "" {
		return clientSessionInfo{
			Key:    hashRouteSessionKey("prompt-cache", value),
			Source: "body:prompt_cache_key",
		}
	}
	return clientSessionInfo{}
}

func rawProtocolPreviousResponseID(payload map[string]any) string {
	if len(payload) == 0 {
		return ""
	}
	return strings.TrimSpace(rawProtocolStringValue(payload["previous_response_id"]))
}

func prioritizeRawProtocolResponsesOwner(ctx context.Context, iter *balancer.Iterator, previousResponseID string) {
	if iter == nil || strings.TrimSpace(previousResponseID) == "" {
		return
	}
	owner, ok := responsesSessionOwnerWithContext(ctx, previousResponseID)
	if !ok {
		return
	}
	iter.PrioritizeChannels(map[int]bool{owner.channelID: true})
}

func shouldForwardRawProtocolResponsesCursor(ctx context.Context, iter *balancer.Iterator, previousResponseID string, channelID int, channelKeyID int, clientSessionSource string) bool {
	if strings.TrimSpace(previousResponseID) == "" {
		return true
	}
	owner, ok := responsesSessionOwnerWithContext(ctx, previousResponseID)
	if !ok {
		return canTrustSessionSourceForUnknownResponsesCursor(clientSessionSource) &&
			iter != nil &&
			iter.IsStickyChannelKey(channelID, channelKeyID)
	}
	return owner.channelID == channelID && owner.channelKeyID == channelKeyID
}

func rawProtocolStringMap(value any) map[string]string {
	switch typed := value.(type) {
	case map[string]string:
		return typed
	case map[string]any:
		out := make(map[string]string, len(typed))
		for key, raw := range typed {
			if str := rawProtocolStringValue(raw); str != "" {
				out[key] = str
			}
		}
		return out
	default:
		return nil
	}
}

func rawProtocolStringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	default:
		return ""
	}
}

func rawProtocolCodexIdentity(payload map[string]any, clientSessionKey string) *transformerModel.InternalLLMRequest {
	req := &transformerModel.InternalLLMRequest{}
	sessionID := strings.TrimSpace(rawProtocolStringValue(payload["prompt_cache_key"]))
	if sessionID == "" {
		sessionID = strings.TrimSpace(clientSessionKey)
	}
	if sessionID != "" {
		req.PromptCacheKey = &sessionID
	}
	if raw := rawProtocolJSONValue(payload["client_metadata"]); len(raw) > 0 {
		req.ClientMetadata = raw
	}
	return req
}

func rawProtocolJSONValue(value any) json.RawMessage {
	if value == nil {
		return nil
	}
	switch typed := value.(type) {
	case json.RawMessage:
		return append(json.RawMessage(nil), typed...)
	default:
		raw, err := json.Marshal(typed)
		if err != nil || len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil
		}
		return raw
	}
}

func isOpenAIWireChannelType(channelType outbound.OutboundType) bool {
	switch channelType {
	case outbound.OutboundTypeOpenAIChat, outbound.OutboundTypeOpenAIResponse, outbound.OutboundTypeOpenAIEmbedding:
		return true
	default:
		return false
	}
}

func parseJSONModelAndStreamWithDefault(bc *bodycache.BodyCache, defaultModel string) (payload map[string]any, modelName string, stream bool, err error) {
	r, err := bc.NewReader()
	if err != nil {
		return nil, "", false, err
	}
	defer r.Close()

	body, err := io.ReadAll(r)
	if err != nil {
		return nil, "", false, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, "", false, errors.New("empty body")
	}

	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, "", false, errors.New("invalid json")
	}

	modelStr := strings.TrimSpace(defaultModel)
	if rawModel, ok := m["model"]; ok {
		value, ok := rawModel.(string)
		if !ok || strings.TrimSpace(value) == "" {
			return nil, "", false, errors.New("model is required")
		}
		modelStr = strings.TrimSpace(value)
	}
	if modelStr == "" {
		return nil, "", false, errors.New("model is required")
	}

	if v, ok := m["stream"]; ok {
		switch vv := v.(type) {
		case bool:
			stream = vv
		case string:
			stream = strings.EqualFold(strings.TrimSpace(vv), "true")
		case float64:
			stream = vv != 0
		}
	}

	return m, modelStr, stream, nil
}

func parseMultipartModelAndStreamWithDefault(bc *bodycache.BodyCache, boundary string, defaultModel string) (modelName string, stream bool, err error) {
	r, err := bc.NewReader()
	if err != nil {
		return "", false, err
	}
	defer r.Close()

	mr := multipart.NewReader(r, boundary)
	modelName = strings.TrimSpace(defaultModel)

	for {
		part, err := mr.NextPart()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", false, err
		}

		name := part.FormName()
		if name == "" {
			_, _ = io.Copy(io.Discard, part)
			_ = part.Close()
			continue
		}

		switch name {
		case "model":
			b, _ := io.ReadAll(io.LimitReader(part, 1024))
			modelName = strings.TrimSpace(string(b))
		case "stream":
			b, _ := io.ReadAll(io.LimitReader(part, 16))
			stream = strings.EqualFold(strings.TrimSpace(string(b)), "true")
		default:
			_, _ = io.Copy(io.Discard, part)
		}
		_ = part.Close()
	}

	if strings.TrimSpace(modelName) == "" {
		return "", false, errors.New("model is required")
	}
	return modelName, stream, nil
}

func rawProtocolAttempt(
	ctx context.Context,
	options RawProtocolOptions,
	c *gin.Context,
	bc *bodycache.BodyCache,
	isMultipart bool,
	boundary string,
	jsonPayload map[string]any,
	stream bool,
	channel *model.Channel,
	channelKey string,
	firstTokenTimeOutSec int,
	metrics *rawProtocolRelayMetrics,
	actualModel string,
	clientSessionKey string,
	forwardCompactCursor bool,
) (statusCode int, written bool, usage *imagesUsage, upstreamCT string, upstreamResponseID string, err error) {
	baseURL := channel.GetBaseUrl()
	targetURL, err := xurl.JoinOpenAIPath(baseURL, "/v1"+options.Endpoint)
	if err != nil {
		return 0, false, nil, "", "", fmt.Errorf("failed to build raw protocol url: %w", err)
	}
	parsedURL, err := url.Parse(targetURL)
	if err != nil {
		return 0, false, nil, "", "", fmt.Errorf("failed to parse raw protocol url: %w", err)
	}
	mergeRequestQuery(parsedURL, c.Request.URL.Query())

	var bodyReader io.Reader
	var contentType string

	if isMultipart {
		pr, pw := io.Pipe()
		mw := multipart.NewWriter(pw)
		contentType = mw.FormDataContentType()
		bodyReader = pr

		go func() {
			src, err := bc.NewReader()
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			defer src.Close()

			if err := copyMultipartReplaceModel(src, boundary, mw, actualModel); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			if err := mw.Close(); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			_ = pw.Close()
		}()
	} else {
		if jsonPayload == nil {
			return 0, false, nil, "", "", errors.New("nil json payload")
		}
		attemptPayload := make(map[string]any, len(jsonPayload)+1)
		for k, v := range jsonPayload {
			attemptPayload[k] = v
		}
		attemptPayload["model"] = actualModel
		if isResponsesCompactRawProtocol(options) && !forwardCompactCursor {
			delete(attemptPayload, "previous_response_id")
		}
		b, err := json.Marshal(attemptPayload)
		if err != nil {
			return 0, false, nil, "", "", fmt.Errorf("failed to marshal json: %w", err)
		}
		bodyReader = bytes.NewReader(b)
		contentType = "application/json"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, parsedURL.String(), bodyReader)
	if err != nil {
		return 0, false, nil, "", "", fmt.Errorf("failed to create request: %w", err)
	}
	copyHeadersToUpstream(req, c, channel, channelKey, contentType, stream)
	if isResponsesCompactRawProtocol(options) {
		applyCodexHeaderDefaultsWithFingerprint(req, rawProtocolCodexIdentity(jsonPayload, clientSessionKey), resolveFingerprintForChannel(channel))
	}

	httpClient, err := helper.ChannelHttpClient(channel)
	if err != nil {
		return 0, false, nil, "", "", err
	}

	respUp, err := helper.DoPreserveMethodRedirect(httpClient, req)
	if err != nil {
		return 0, false, nil, "", "", fmt.Errorf("failed to send request: %w", err)
	}
	defer respUp.Body.Close()

	upstreamCT = respUp.Header.Get("Content-Type")
	if respUp.StatusCode < 200 || respUp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(respUp.Body, upstreamErrorBodyLimit))
		upErr := newUpstreamError(respUp.StatusCode, b)
		log.Warnf("raw protocol upstream returned non-2xx: protocol=%s status=%d code=%s strategy=%s", options.Name, upErr.StatusCode(), upErr.ErrorCode(), upErr.Strategy())
		return respUp.StatusCode, false, nil, upstreamCT, "", upErr
	}

	if stream {
		u, w, responseID, err := proxySSEWithOptions(ctx, c, respUp, firstTokenTimeOutSec, metrics, proxySSEOptions{
			ScanAllData:             true,
			CaptureResponsesID:      isResponsesCompactRawProtocol(options),
			StopOnResponsesTerminal: isResponsesCompactRawProtocol(options),
		})
		return respUp.StatusCode, w, u, upstreamCT, responseID, err
	}
	if options.BinaryResponse {
		if metrics != nil {
			metrics.OpaqueResponse = true
		}
		u, w, err := proxyOpaqueResponse(c, respUp)
		return respUp.StatusCode, w, u, upstreamCT, "", err
	}
	if isResponsesCompactRawProtocol(options) {
		u, w, responseID, err := proxyNonStreamWithResponseID(c, respUp)
		return respUp.StatusCode, w, u, upstreamCT, responseID, err
	}
	u, w, err := proxyNonStream(c, respUp)
	return respUp.StatusCode, w, u, upstreamCT, "", err
}

func mergeRequestQuery(parsedURL *url.URL, requestQuery url.Values) {
	if parsedURL == nil || len(requestQuery) == 0 {
		return
	}
	q := parsedURL.Query()
	for k, values := range requestQuery {
		for _, v := range values {
			q.Add(k, v)
		}
	}
	parsedURL.RawQuery = q.Encode()
}

func proxyOpaqueResponse(c *gin.Context, respUp *http.Response) (*imagesUsage, bool, error) {
	ct := respUp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}
	c.Header("Content-Type", ct)
	c.Status(respUp.StatusCode)
	_, err := io.Copy(c.Writer, respUp.Body)
	return nil, c.Writer.Written(), err
}

type rawProtocolRelayMetrics struct {
	APIKeyID     int
	UserID       int
	RequestIP    string
	RequestModel string
	Protocol     string
	RequestPath  string
	ActualModel  string
	StartTime    time.Time
	FirstToken   time.Time

	Stats model.StatsMetrics

	RequestContent  string
	ResponseContent string
	AccessPlan      *model.AccessPlan
	AccessRouteRule *model.AccessRouteRule
	AccessRouteUsed bool
	BillingSnapshot model.AccessPlanBillingSnapshot
	SessionKey      string
	SessionSource   string
	UsageSeen       bool
	OpaqueResponse  bool
}

func newRawProtocolRelayMetrics(apiKeyID int, userID int, requestIP string, requestModel string, protocol string, requestPath string) *rawProtocolRelayMetrics {
	return &rawProtocolRelayMetrics{
		APIKeyID:     apiKeyID,
		UserID:       userID,
		RequestIP:    requestIP,
		RequestModel: requestModel,
		Protocol:     protocol,
		RequestPath:  strings.TrimSpace(requestPath),
		StartTime:    time.Now(),
	}
}

func (m *rawProtocolRelayMetrics) SetFirstTokenTime(t time.Time) {
	if m.FirstToken.IsZero() {
		m.FirstToken = t
	}
}

func (m *rawProtocolRelayMetrics) SetAccessPlan(plan *model.AccessPlan, rule *model.AccessRouteRule, routeUsed bool) {
	m.AccessPlan = plan
	m.AccessRouteRule = rule
	m.AccessRouteUsed = routeUsed
}

func (m *rawProtocolRelayMetrics) SetClientSession(info clientSessionInfo) {
	m.SessionKey = info.Key
	m.SessionSource = info.Source
}

func (m *rawProtocolRelayMetrics) SetUsage(actualModel string, u imagesUsage) {
	m.ActualModel = actualModel
	m.UsageSeen = true
	m.Stats.InputToken = int64(u.InputTokenCount())
	m.Stats.OutputToken = int64(u.OutputTokenCount())
	m.Stats.CacheHitToken = int64(u.CacheReadTokenCount())
	m.Stats.CacheWriteToken = int64(u.CacheWriteTokenCount())
	m.Stats.CacheInputToken = int64(u.CacheInputTokenCount())

	m.BillingSnapshot = m.buildBillingSnapshot(actualModel, &u)
	m.Stats.InputCost = m.BillingSnapshot.FinalInputCost + m.BillingSnapshot.FinalCacheReadCost + m.BillingSnapshot.FinalCacheWriteCost
	m.Stats.OutputCost = m.BillingSnapshot.FinalOutputCost
}

func (m *rawProtocolRelayMetrics) currentBillingSnapshot(actualModel string) model.AccessPlanBillingSnapshot {
	if m.BillingSnapshot.BillingModelName != "" || m.BillingSnapshot.AccessPlanID != 0 {
		return m.BillingSnapshot
	}
	return m.buildBillingSnapshot(actualModel, nil)
}

func (m *rawProtocolRelayMetrics) buildBillingSnapshot(actualModel string, usage *imagesUsage) model.AccessPlanBillingSnapshot {
	upstreamModel := strings.TrimSpace(actualModel)
	if upstreamModel == "" {
		upstreamModel = m.RequestModel
	}

	snapshot := model.AccessPlanBillingSnapshot{
		DefaultMultiplier: 1,
		ModelMultiplier:   1,
		FinalMultiplier:   1,
	}

	billingSource := model.AccessBillingModelSourceRequest
	if m.AccessPlan != nil {
		snapshot.AccessPlanID = m.AccessPlan.ID
		snapshot.AccessPlanSlug = m.AccessPlan.Slug
		snapshot.AccessPlanName = m.AccessPlan.DisplayName
		if m.AccessPlan.RouteProfile != nil {
			snapshot.RouteProfileID = m.AccessPlan.RouteProfile.ID
			snapshot.RouteProfileName = m.AccessPlan.RouteProfile.Name
		}
		if m.AccessPlan.BillingProfile != nil {
			snapshot.BillingProfileID = m.AccessPlan.BillingProfile.ID
			snapshot.BillingProfileName = m.AccessPlan.BillingProfile.Name
			snapshot.DefaultMultiplier = positiveMultiplier(m.AccessPlan.BillingProfile.DefaultMultiplier)
		}
	}
	if m.AccessRouteRule != nil && m.AccessRouteRule.BillingModelSource != "" {
		billingSource = m.AccessRouteRule.BillingModelSource
	}

	switch billingSource {
	case model.AccessBillingModelSourceRequest:
		snapshot.BillingModelName = m.RequestModel
	case model.AccessBillingModelSourceOverride:
		if m.AccessRouteRule != nil && strings.TrimSpace(m.AccessRouteRule.BillingModelOverride) != "" {
			snapshot.BillingModelName = strings.TrimSpace(m.AccessRouteRule.BillingModelOverride)
		} else {
			snapshot.BillingModelName = upstreamModel
		}
	default:
		snapshot.BillingModelName = upstreamModel
	}
	if snapshot.BillingModelName == "" {
		snapshot.BillingModelName = m.RequestModel
	}
	snapshot.BillingModelSource = billingSource

	if m.AccessPlan != nil && m.AccessPlan.BillingProfile != nil {
		snapshot.ModelMultiplier = billingModelMultiplier(m.AccessPlan.BillingProfile.ModelRules, snapshot.BillingModelName)
	}
	snapshot.FinalMultiplier = snapshot.DefaultMultiplier * snapshot.ModelMultiplier

	modelPrice := price.GetLLMPrice(snapshot.BillingModelName)
	if modelPrice == nil {
		return snapshot
	}
	snapshot.BaseInputPrice = modelPrice.Input
	snapshot.BaseOutputPrice = modelPrice.Output
	snapshot.BaseCacheReadPrice = modelPrice.CacheRead
	snapshot.BaseCacheWritePrice = modelPrice.CacheWrite
	if usage == nil {
		return snapshot
	}

	inputTokens := usage.InputTokenCount()
	cacheReadTokens := usage.CacheReadTokenCount()
	cacheWriteTokens := usage.CacheWriteTokenCount()
	if !usage.HasSeparateCacheInputTokens() {
		if inputTokens < cacheReadTokens+cacheWriteTokens {
			inputTokens = cacheReadTokens + cacheWriteTokens
		}
		inputTokens -= cacheReadTokens
		if inputTokens < 0 {
			inputTokens = 0
		}
	}

	snapshot.FinalInputCost = float64(inputTokens) * snapshot.BaseInputPrice * snapshot.FinalMultiplier * 1e-6
	snapshot.FinalOutputCost = float64(usage.OutputTokenCount()) * snapshot.BaseOutputPrice * snapshot.FinalMultiplier * 1e-6
	snapshot.FinalCacheReadCost = float64(cacheReadTokens) * snapshot.BaseCacheReadPrice * snapshot.FinalMultiplier * 1e-6
	snapshot.FinalCacheWriteCost = float64(cacheWriteTokens) * snapshot.BaseCacheWritePrice * snapshot.FinalMultiplier * 1e-6
	return snapshot
}

func (m *rawProtocolRelayMetrics) Save(ctx context.Context, success bool, err error, attempts []model.ChannelAttempt) {
	duration := time.Since(m.StartTime)
	persistCtx, cancel := metricsPersistContext()
	defer cancel()

	globalStats := model.StatsMetrics{
		WaitTime:        duration.Milliseconds(),
		InputToken:      m.Stats.InputToken,
		OutputToken:     m.Stats.OutputToken,
		InputCost:       m.Stats.InputCost,
		OutputCost:      m.Stats.OutputCost,
		CacheHitToken:   m.Stats.CacheHitToken,
		CacheWriteToken: m.Stats.CacheWriteToken,
		CacheInputToken: m.Stats.CacheInputToken,
	}
	if success {
		globalStats.RequestSuccess = 1
	} else if !isClientAbortError(err) {
		globalStats.RequestFailed = 1
	}

	channelID, channelName := finalChannel(attempts)
	op.StatsTotalUpdate(globalStats)
	op.StatsHourlyUpdate(globalStats)
	op.StatsDailyUpdate(persistCtx, globalStats)
	op.StatsAPIKeyUpdate(m.APIKeyID, globalStats)
	op.StatsChannelUpdate(channelID, globalStats)
	if err := op.UserRecordRelayIP(m.UserID, m.RequestIP, m.StartTime.Unix(), persistCtx); err != nil {
		log.Warnf("failed to record user relay ip: %v", err)
	}
	if success {
		if err := op.UserRecordUsage(m.UserID, globalStats.InputCost+globalStats.OutputCost, persistCtx); err != nil {
			log.Warnf("failed to record user usage: %v", err)
		}
	}

	errorStatus, errorCode, errorStrategy, _ := relayErrorDetails(err)
	log.Infof("raw protocol relay complete: protocol=%s model=%s, channel=%d(%s), success=%t, duration=%dms, input_token=%d, output_token=%d, cache_hit_token=%d, cache_rate=%.2f%%, input_cost=%f, output_cost=%f, total_cost=%f, attempts=%d, error_status=%d, error_code=%s, error_strategy=%s",
		m.Protocol, m.RequestModel, channelID, channelName, success, duration.Milliseconds(),
		m.Stats.InputToken, m.Stats.OutputToken, m.Stats.CacheHitToken, cacheHitRate(m.Stats.CacheHitToken, m.Stats.CacheInputToken)*100,
		m.Stats.InputCost, m.Stats.OutputCost, m.Stats.InputCost+m.Stats.OutputCost,
		len(attempts), errorStatus, errorCode, errorStrategy)

	m.saveLog(persistCtx, err, duration, attempts, channelID, channelName)
}

func (m *rawProtocolRelayMetrics) saveLog(ctx context.Context, err error, duration time.Duration, attempts []model.ChannelAttempt, channelID int, channelName string) {
	actualModel := m.ActualModel
	if actualModel == "" {
		actualModel = m.RequestModel
	}
	billingSnapshot := m.currentBillingSnapshot(actualModel)

	relayLog := model.RelayLog{
		UserID:           m.UserID,
		APIKeyID:         m.APIKeyID,
		RequestIP:        m.RequestIP,
		Time:             m.StartTime.Unix(),
		RequestEndpoint:  cleanRelayEndpointName(m.Protocol),
		RequestPath:      m.RequestPath,
		RequestModelName: m.RequestModel,
		ChannelName:      channelName,
		ChannelId:        channelID,
		ActualModelName:  actualModel,
		UseTime:          int(duration.Milliseconds()),
		Attempts:         attempts,
		TotalAttempts:    len(attempts),
		SessionKey:       m.SessionKey,
		SessionSource:    m.SessionSource,
		RouteStickyHit:   routeStickyHit(attempts),
		RequestContent:   m.RequestContent,
		ResponseContent:  m.ResponseContent,
	}
	applyBillingSnapshotToRelayLog(&relayLog, billingSnapshot)

	if apiKey, getErr := op.APIKeyGet(m.APIKeyID, ctx); getErr == nil {
		relayLog.RequestAPIKeyName = apiKey.Name
	}
	if !m.FirstToken.IsZero() {
		relayLog.Ftut = int(m.FirstToken.Sub(m.StartTime).Milliseconds())
	}
	relayLog.UsageSource, relayLog.UsageMissingReason = usageAuditFromStats(m.Stats, m.UsageSeen, err, m.OpaqueResponse)
	if m.Stats.InputToken > 0 || m.Stats.OutputToken > 0 {
		relayLog.InputTokens = int(m.Stats.InputToken)
		relayLog.OutputTokens = int(m.Stats.OutputToken)
		relayLog.CacheHitTokens = int(m.Stats.CacheHitToken)
		relayLog.CacheWriteTokens = int(m.Stats.CacheWriteToken)
		relayLog.CacheInputTokens = int(m.Stats.CacheInputToken)
		relayLog.CacheHitRate = cacheHitRate(m.Stats.CacheHitToken, m.Stats.CacheInputToken)
		relayLog.Cost = m.Stats.InputCost + m.Stats.OutputCost
	}
	if err != nil {
		relayLog.Error = errSafeMessage(err)
		if status, code, strategy, ok := relayErrorDetails(err); ok {
			relayLog.ErrorStatus = status
			relayLog.ErrorCode = code
			relayLog.ErrorStrategy = strategy
		}
	}

	if logErr := op.RelayLogAdd(ctx, relayLog); logErr != nil {
		log.Warnf("failed to save relay log: %v", logErr)
	}
}

func buildRawProtocolRequestContent(isMultipart bool, bc *bodycache.BodyCache, jsonPayload map[string]any, protocol string) string {
	if isMultipart {
		return fmt.Sprintf(`{"protocol":%q,"content_type":"multipart/form-data","size_bytes":%d,"note":"multipart request content omitted for storage"}`, protocol, bc.Size())
	}
	if jsonPayload == nil {
		return ""
	}
	b, err := json.Marshal(jsonPayload)
	if err != nil {
		return ""
	}
	return truncateString(string(b), 8*1024)
}

func buildRawProtocolResponseContent(options RawProtocolOptions, stream bool, upstreamCT string, usage *imagesUsage) string {
	type respForLog struct {
		Protocol    string       `json:"protocol"`
		Stream      bool         `json:"stream"`
		ContentType string       `json:"content_type,omitempty"`
		Usage       *imagesUsage `json:"usage,omitempty"`
		Note        string       `json:"note,omitempty"`
	}
	obj := respForLog{
		Protocol:    options.Name,
		Stream:      stream,
		ContentType: upstreamCT,
		Usage:       usage,
	}
	if options.BinaryResponse || usage == nil {
		obj.Note = options.ResponseLogNote
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return ""
	}
	return string(b)
}
