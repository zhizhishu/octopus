package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/helper"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	"github.com/bestruirui/octopus/internal/relay/bodycache"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/utils/log"
	"github.com/bestruirui/octopus/internal/utils/xurl"
	"github.com/gin-gonic/gin"
)

// videoResponseBodyLimit caps the buffered create/poll response. These are tiny
// JSON status objects (task id + url), so this is only a safety ceiling.
const videoResponseBodyLimit = 8 << 20 // 8MB

// VideosHandler relays an asynchronous video-generation create call
// (POST /v1/videos). It routes by model exactly like the other relays, forwards
// the request to the chosen channel/key, and on success records which
// channel+key owns each returned task/video id so a later poll can return to
// the same upstream account (the task only exists there).
func VideosHandler(c *gin.Context) {
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
			log.Warnf("failed to close video body cache: %v", cerr)
		}
	}()

	payload, requestModel, _, perr := parseJSONModelAndStreamWithDefault(bc, "")
	if perr != nil {
		resp.Error(c, http.StatusBadRequest, perr.Error())
		return
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
	group := enrichGroupForSmartRouting(ctx, routeResult.Group, false)
	iter := balancer.NewIteratorWithSession(group, apiKeyID, requestModel, "", false)
	if iter.Len() == 0 {
		resp.Error(c, http.StatusServiceUnavailable, "no available channel")
		return
	}

	metrics := newRawProtocolRelayMetrics(apiKeyID, userID, c.GetString("request_ip"), requestModel, "videos", c.Request.URL.Path)
	metrics.SetAccessPlan(routeResult.AccessPlan, routeResult.AccessRouteRule, routeResult.AccessRouteUsed)
	metrics.RequestContent = buildRawProtocolRequestContent(false, bc, payload, "videos")

	var (
		lastErr     error
		allAttempts []model.ChannelAttempt
	)

	for iter.Next() {
		select {
		case <-ctx.Done():
			log.Infof("request context canceled, stopping video create retry")
			metrics.Save(ctx, false, context.Canceled, append(allAttempts, iter.Attempts()...))
			return
		default:
		}

		item := iter.Item()
		channel, err := op.ChannelGet(item.ChannelID, ctx)
		if err != nil {
			iter.Skip(item.ChannelID, 0, fmt.Sprintf("channel_%d", item.ChannelID), fmt.Sprintf("channel not found: %v", err))
			lastErr = err
			continue
		}
		if !channel.Enabled {
			iter.Skip(channel.ID, 0, channel.Name, "channel disabled")
			continue
		}
		if !isOpenAIWireChannelType(channel.Type) {
			iter.Skip(channel.ID, 0, channel.Name, fmt.Sprintf("unsupported channel type for video: %d", channel.Type))
			continue
		}
		if mapped, ok := channel.ModelMapping[item.ModelName]; ok && mapped != "" {
			item.ModelName = mapped
			iter.RemapCurrentModel(mapped)
		}
		availableKeys := channel.GetAvailableChannelKeys()
		if len(availableKeys) == 0 {
			iter.Skip(channel.ID, 0, channel.Name, "no available key")
			continue
		}
		availableKeys = balancer.PrioritizeChannelKeysByHealth(availableKeys, channel.ID, item.ModelName, 0)

		for keyIndex, usedKey := range availableKeys {
			if iter.SkipCircuitBreak(channel.ID, usedKey.ID, channel.Name) {
				continue
			}
			log.Infof("video create model %s, forwarding to channel: %s model: %s (attempt %d/%d, key %d/%d)",
				requestModel, channel.Name, item.ModelName, iter.Index()+1, iter.Len(), keyIndex+1, len(availableKeys))

			span := iter.StartAttempt(channel.ID, usedKey.ID, channel.Name)
			recordAttemptProxy(span, channel)
			statusCode, body, upstreamCT, fwdErr := func() (int, []byte, string, error) {
				finish := balancer.BeginRuntimeAttempt(channel.ID, usedKey.ID, item.ModelName)
				defer finish()
				return videoForward(ctx, c, http.MethodPost, "/v1/videos", payload, item.ModelName, channel, usedKey.ChannelKey)
			}()
			usedAt := time.Now().Unix()

			if fwdErr == nil {
				ids := extractVideoTaskIDs(body)
				recordVideoTaskOwner(ids, channel.ID, usedKey.ID, item.ModelName, apiKeyID, userID)

				metrics.ActualModel = item.ModelName
				metrics.ResponseContent = buildVideoResponseNote(ids)
				op.ChannelKeyRecordUse(usedKey, statusCode, usedAt, 0)
				span.End(model.AttemptSuccess, statusCode, "")
				balancer.RecordRuntimeSuccess(channel.ID, usedKey.ID, item.ModelName, balancer.AttemptRuntimeMetrics{
					Duration:   span.Duration(),
					FirstToken: span.Duration(),
					Stream:     false,
				})
				op.StatsChannelUpdate(channel.ID, model.StatsMetrics{
					WaitTime:       span.Duration().Milliseconds(),
					RequestSuccess: 1,
				})
				balancer.RecordSuccess(channel.ID, usedKey.ID, item.ModelName)

				writeVideoResponse(c, statusCode, upstreamCT, body)
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

			lastErr = fmt.Errorf("channel %s failed: %w", channel.Name, fwdErr)
			if !shouldTryNextChannelKey(recordStatusCode) {
				break
			}
		}
	}

	allAttempts = append(allAttempts, iter.Attempts()...)
	finalErr := lastErr
	if finalErr == nil {
		finalErr = routeSelectionErrorFromAttempts(allAttempts)
	}
	metrics.Save(ctx, false, finalErr, allAttempts)
	status, code, message := relayErrorResponse(finalErr)
	resp.ErrorWithCode(c, status, code, message)
}

// VideoPollHandler relays a poll (GET /v1/videos/:id) back to the exact
// channel+key that created the task, so it always hits the upstream account
// that owns it. Returns 404 when the id is unknown/expired or belongs to a
// different tenant (fail-closed).
func VideoPollHandler(c *gin.Context) {
	ctx := c.Request.Context()
	apiKeyID := c.GetInt("api_key_id")
	userID := c.GetInt("user_id")

	taskID := strings.TrimSpace(c.Param("id"))
	if taskID == "" {
		resp.Error(c, http.StatusBadRequest, "video task id required")
		return
	}

	entry, ok := videoTaskOwner(taskID)
	if !ok || !videoTaskOwnerMatches(entry, apiKeyID, userID) {
		resp.Error(c, http.StatusNotFound, "unknown or expired video task; create the task again")
		return
	}

	channel, err := op.ChannelGet(entry.channelID, ctx)
	if err != nil {
		resp.Error(c, http.StatusBadGateway, "video task channel unavailable")
		return
	}
	channelKey, ok := videoChannelKeyByID(channel, entry.channelKeyID)
	if !ok {
		resp.Error(c, http.StatusBadGateway, "video task channel key unavailable")
		return
	}

	metrics := newRawProtocolRelayMetrics(apiKeyID, userID, c.GetString("request_ip"), entry.model, "videos_poll", c.Request.URL.Path)
	metrics.ActualModel = entry.model

	statusCode, body, upstreamCT, fwdErr := videoForward(ctx, c, http.MethodGet, "/v1/videos/"+url.PathEscape(taskID), nil, entry.model, channel, channelKey)
	if fwdErr != nil {
		metrics.Save(ctx, false, fwdErr, nil)
		status, code, message := relayErrorResponse(fwdErr)
		resp.ErrorWithCode(c, status, code, message)
		return
	}
	metrics.ResponseContent = "video task status"
	writeVideoResponse(c, statusCode, upstreamCT, body)
	metrics.Save(ctx, true, nil, nil)
}

// videoForward builds and sends a single upstream request (POST create or GET
// poll), buffering the small JSON response so the caller can inspect it (to
// record task ownership) before writing it downstream. It reuses the shared
// image/raw helpers for auth headers and the channel HTTP client.
func videoForward(ctx context.Context, c *gin.Context, method, upstreamPath string, payload map[string]any, actualModel string, channel *model.Channel, channelKey string) (int, []byte, string, error) {
	baseURL := channel.GetBaseUrl()
	targetURL, err := xurl.JoinOpenAIPath(baseURL, upstreamPath)
	if err != nil {
		return 0, nil, "", fmt.Errorf("failed to build video url: %w", err)
	}
	parsedURL, err := url.Parse(targetURL)
	if err != nil {
		return 0, nil, "", fmt.Errorf("failed to parse video url: %w", err)
	}
	mergeRequestQuery(parsedURL, c.Request.URL.Query())

	var bodyReader io.Reader
	contentType := ""
	if method == http.MethodPost && payload != nil {
		attemptPayload := make(map[string]any, len(payload)+1)
		for k, v := range payload {
			attemptPayload[k] = v
		}
		attemptPayload["model"] = actualModel
		b, merr := json.Marshal(attemptPayload)
		if merr != nil {
			return 0, nil, "", fmt.Errorf("failed to marshal video payload: %w", merr)
		}
		bodyReader = bytes.NewReader(b)
		contentType = "application/json"
	}

	req, err := http.NewRequestWithContext(ctx, method, parsedURL.String(), bodyReader)
	if err != nil {
		return 0, nil, "", fmt.Errorf("failed to create video request: %w", err)
	}
	copyHeadersToUpstream(req, c, channel, channelKey, contentType, false)

	httpClient, err := helper.ChannelHttpClient(channel)
	if err != nil {
		return 0, nil, "", err
	}
	respUp, err := helper.DoPreserveMethodRedirect(httpClient, req)
	if err != nil {
		return 0, nil, "", fmt.Errorf("failed to send video request: %w", err)
	}
	defer respUp.Body.Close()

	upstreamCT := respUp.Header.Get("Content-Type")
	if respUp.StatusCode < 200 || respUp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(respUp.Body, upstreamErrorBodyLimit))
		return respUp.StatusCode, nil, upstreamCT, newUpstreamError(respUp.StatusCode, b)
	}
	body, err := io.ReadAll(io.LimitReader(respUp.Body, videoResponseBodyLimit))
	if err != nil {
		return respUp.StatusCode, nil, upstreamCT, fmt.Errorf("failed to read video response: %w", err)
	}
	return respUp.StatusCode, body, upstreamCT, nil
}

func writeVideoResponse(c *gin.Context, statusCode int, contentType string, body []byte) {
	if strings.TrimSpace(contentType) == "" {
		contentType = "application/json"
	}
	c.Data(statusCode, contentType, body)
}

// extractVideoTaskIDs pulls every id the upstream returned (task_id / video_id /
// id) so the poll can look up ownership regardless of which id the client keeps.
func extractVideoTaskIDs(body []byte) []string {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil
	}
	var ids []string
	seen := make(map[string]bool)
	for _, key := range []string{"task_id", "video_id", "id"} {
		v, ok := m[key].(string)
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		ids = append(ids, v)
	}
	return ids
}

func videoChannelKeyByID(channel *model.Channel, keyID int) (string, bool) {
	for _, k := range channel.GetAvailableChannelKeys() {
		if k.ID == keyID {
			return k.ChannelKey, true
		}
	}
	return "", false
}

func buildVideoResponseNote(ids []string) string {
	if len(ids) == 0 {
		return "video task created"
	}
	return "video task created: " + strings.Join(ids, ",")
}
