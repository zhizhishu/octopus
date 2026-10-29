package modeltest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zhizhishu/octopus/internal/helper"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/xredact"
	"github.com/zhizhishu/octopus/internal/utils/xurl"
)

func isSystemOneTestEndpoint(endpoint string) bool {
	normalized, err := normalizeEndpoint(endpoint)
	if err != nil {
		return false
	}
	return normalized.name == "systemone"
}

// trySystemOneChannel sends one minimal decision request:
// {model, state:"ping", questions:{q:{type:"noul", instructions:"ping"}}}.
// The chat runner cannot build that body, so type 7 does not go through it.
// Headers stay generic: no codex/claude identity and no TLS face.
func (r *modelRunner) trySystemOneChannel(ctx context.Context, channel *dbmodel.Channel, upstreamModel string) bool {
	channelName := channelDisplayName(channel)
	if strings.TrimSpace(channel.GetBaseUrl()) == "" {
		r.recordAttempt(channel.ID, 0, channelName, upstreamModel, "", dbmodel.AttemptSkipped, 0, 0, "channel has no base url")
		return false
	}
	if strings.TrimSpace(upstreamModel) == "" {
		r.recordAttempt(channel.ID, 0, channelName, upstreamModel, "", dbmodel.AttemptSkipped, 0, 0, "no model to probe")
		return false
	}
	var keys []dbmodel.ChannelKey
	if channel.DisableCircuitBreaker {
		keys = channel.GetAllEnabledChannelKeys()
	} else {
		keys = channel.GetAvailableChannelKeys()
	}
	if len(keys) == 0 {
		r.recordAttempt(channel.ID, 0, channelName, upstreamModel, "", dbmodel.AttemptSkipped, 0, 0, "no available key")
		return false
	}
	if !r.ensureProxyConnectivity(ctx, channel, upstreamModel) {
		return false
	}

	targetURL, err := xurl.JoinOpenAIPath(channel.GetBaseUrl(), "/v1/systemone")
	if err != nil {
		r.recordAttempt(channel.ID, 0, channelName, upstreamModel, "", dbmodel.AttemptFailed, 0, 0, err.Error())
		r.fail(err)
		return false
	}

	for _, key := range keys {
		started := time.Now()
		status, preview, err := postSystemOneProbe(ctx, channel, key.ChannelKey, upstreamModel, targetURL)
		duration := int(time.Since(started).Milliseconds())
		r.result.UpstreamPath = urlPathOf(targetURL)
		if err == nil {
			r.recordAttempt(channel.ID, key.ID, channelName, upstreamModel, r.result.UpstreamPath, dbmodel.AttemptSuccess, status, duration, r.proxyAttemptMessage())
			r.result.Success = true
			r.result.Error = ""
			r.result.ErrorCode = ""
			r.result.StatusCode = status
			r.result.ChannelID = channel.ID
			r.result.ChannelName = channelName
			r.result.ChannelKeyID = key.ID
			r.result.UpstreamModel = upstreamModel
			r.result.ResponsePreview = preview
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

func postSystemOneProbe(ctx context.Context, channel *dbmodel.Channel, key, modelName, targetURL string) (int, string, error) {
	body, err := json.Marshal(map[string]any{
		"model": modelName,
		"state": "ping",
		"questions": map[string]any{
			"q": map[string]any{
				"type":         "noul",
				"instructions": "ping",
			},
		},
	})
	if err != nil {
		return 0, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("User-Agent", dbmodel.DefaultGenericUA)
	for _, header := range channel.CustomHeader {
		if strings.TrimSpace(header.HeaderKey) != "" {
			req.Header.Set(header.HeaderKey, header.HeaderValue)
		}
	}
	httpClient, err := helper.ChannelHttpClient(channel)
	if err != nil {
		return 0, "", err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	preview := strings.TrimSpace(string(raw))
	if len(preview) > 240 {
		preview = preview[:240]
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if preview == "" {
			preview = http.StatusText(resp.StatusCode)
		}
		return resp.StatusCode, "", fmt.Errorf("upstream status %d: %s", resp.StatusCode, xredact.Secrets(preview))
	}
	return resp.StatusCode, preview, nil
}

func urlPathOf(rawURL string) string {
	if i := strings.Index(rawURL, "://"); i >= 0 {
		rest := rawURL[i+3:]
		if slash := strings.Index(rest, "/"); slash >= 0 {
			path := rest[slash:]
			if q := strings.Index(path, "?"); q >= 0 {
				return path[:q]
			}
			return path
		}
	}
	return rawURL
}
