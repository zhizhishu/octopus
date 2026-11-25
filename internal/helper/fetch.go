package helper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/dlclark/regexp2"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
	"github.com/zhizhishu/octopus/internal/utils/xurl"
)

func FetchModels(ctx context.Context, request model.Channel) ([]string, error) {
	client, err := ChannelHttpClient(&request)
	if err != nil {
		return nil, err
	}
	fetchModel := make([]string, 0)
	switch request.Type {
	case outbound.OutboundTypeAnthropic:
		fetchModel, err = fetchAnthropicModels(client, ctx, request)
	case outbound.OutboundTypeGemini:
		fetchModel, err = fetchGeminiModels(client, ctx, request)
	default:
		fetchModel, err = fetchOpenAIModels(client, ctx, request)
	}
	if err != nil {
		return nil, err
	}
	if request.MatchRegex != nil && *request.MatchRegex != "" {
		matchModel := make([]string, 0)
		re, err := regexp2.Compile(*request.MatchRegex, regexp2.ECMAScript)
		if err != nil {
			return nil, err
		}
		for _, model := range fetchModel {
			matched, err := re.MatchString(model)
			if err != nil {
				return nil, err
			}
			if matched {
				matchModel = append(matchModel, model)
			}
		}
		return matchModel, nil
	}
	return fetchModel, nil
}

// refer: https://platform.openai.com/docs/api-reference/models/list
// ensureFetchModelsOK returns a clear error when the upstream model-list request
// failed with a non-2xx status, instead of letting json.Decode fail cryptically
// on an HTML/error body (the classic "looks like empty model list / invalid
// character" symptom). On 2xx it leaves the body untouched for decoding.
func ensureFetchModelsOK(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	snippet := strings.TrimSpace(string(body))
	if len(snippet) > 200 {
		snippet = snippet[:200]
	}
	if snippet == "" {
		return fmt.Errorf("upstream returned HTTP %d when fetching models", resp.StatusCode)
	}
	return fmt.Errorf("upstream returned HTTP %d when fetching models: %s", resp.StatusCode, snippet)
}

func fetchOpenAIModels(client *http.Client, ctx context.Context, request model.Channel) ([]string, error) {
	// Custom (type 6) channels store a full request URL instead of a base URL, so
	// their model list has to be derived. They get their own bounded candidate
	// ladder (see fetchCustomChannelModels) instead of the single join below.
	if request.Type == outbound.OutboundTypeCustomOpenAIChat {
		return fetchCustomChannelModels(client, ctx, request)
	}

	modelURL, err := joinOpenAIModelsURL(request)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		modelURL,
		nil,
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+request.GetChannelKey().ChannelKey)
	for _, header := range request.CustomHeader {
		if header.HeaderKey != "" {
			req.Header.Set(header.HeaderKey, header.HeaderValue)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := ensureFetchModelsOK(resp); err != nil {
		return nil, err
	}

	var result model.OpenAIModelList

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	models := make([]string, 0, len(result.Data))
	for _, m := range result.Data {
		models = append(models, m.ID)
	}
	return models, nil
}

// errModelSyncDisabled is returned when a channel has no separate models URL.
// Type 7 only speaks /v1/systemone. Type 6 (custom) does not come through here:
// it derives a same-origin /models URL from its own full request URL, see
// fetchCustomChannelModels.
var errModelSyncDisabled = fmt.Errorf("this channel does not auto-sync a model list; enter model names by hand")

func joinOpenAIModelsURL(request model.Channel) (string, error) {
	if request.Type == outbound.OutboundTypeSystemOne {
		return "", errModelSyncDisabled
	}
	if strings.TrimSpace(request.OpenAIModelsPath) != "" {
		return xurl.JoinCustomOpenAIModelsOverride(request.GetBaseUrl(), request.OpenAIModelsPath)
	}
	return xurl.JoinOpenAIPath(request.GetBaseUrl(), "/v1/models")
}

// customModelsCandidate is one bounded attempt at a custom channel's model list.
// label names the candidate kind in operator-facing errors, so a failure report
// never has to print the URL (which may carry a query credential).
type customModelsCandidate struct {
	url   string
	label string
}

// customChannelModelsCandidates lists, in order, the model-list URLs a custom
// (type-6) channel may serve. Bounded to at most two attempts:
//
//  1. an explicit absolute models-path override wins outright — the operator has
//     already said where the list lives, so nothing else is guessed;
//  2. otherwise the sibling /models derivation (which keeps any version prefix,
//     e.g. /api/paas/v4/models) is tried first and the same-origin /v1/models
//     fallback second;
//  3. duplicates collapse, so a channel whose sibling derivation already is
//     <origin>/v1/models is fetched once.
func customChannelModelsCandidates(request model.Channel) ([]customModelsCandidate, error) {
	if abs, ok, err := xurl.ParseEndpointOverride(request.OpenAIModelsPath); err != nil {
		// url.Parse's error text echoes the value; this field is operator supplied
		// and may carry credentials, so the cause is reported without it.
		return nil, errors.New("the custom models path is not a usable absolute URL")
	} else if ok {
		return []customModelsCandidate{{url: abs, label: "explicit models URL"}}, nil
	}

	// The derivation source is the URL this channel actually posts chat requests
	// to: an absolute chat-path override wins over the stored base URL, exactly as
	// it does for real traffic (model.Channel.VerbatimRequestBaseURL).
	chatURL := strings.TrimSpace(request.VerbatimRequestBaseURL(false))
	if chatURL == "" {
		return nil, errors.New("this custom channel has no full request URL yet; fill in the channel URL before refreshing models")
	}

	sibling, err := xurl.CustomChannelModelsURL(chatURL)
	if err != nil {
		return nil, err
	}
	fallback, err := xurl.CustomChannelModelsFallbackURL(chatURL)
	if err != nil {
		return nil, err
	}

	candidates := make([]customModelsCandidate, 0, 2)
	candidates = append(candidates, customModelsCandidate{url: sibling, label: "derived /models endpoint"})
	// Exact string comparison: URL paths are case sensitive, so two candidates
	// that differ only by case are genuinely different requests.
	if fallback != sibling {
		candidates = append(candidates, customModelsCandidate{url: fallback, label: "origin /v1/models fallback"})
	}
	return candidates, nil
}

// fetchCustomChannelModels reads the model list of a custom (type-6) channel.
//
// It tries the bounded candidate ladder from customChannelModelsCandidates and
// fails only when every candidate failed: a non-2xx status, a body that is not a
// JSON model list, or an empty list. The last case matters — an empty list used to
// be indistinguishable from "this channel has no models", which silently wiped
// the operator's model selection on sync.
//
// Every failure is reported with its status/cause and the candidate kind. Nothing
// from the upstream body, no URL, and no key goes into the text: the caller may
// surface it in the UI or a log, and a gateway error body can echo either.
func fetchCustomChannelModels(client *http.Client, ctx context.Context, request model.Channel) ([]string, error) {
	candidates, err := customChannelModelsCandidates(request)
	if err != nil {
		return nil, fmt.Errorf("cannot read this custom channel's model list: %v", err)
	}

	causes := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		models, cause := fetchCustomChannelModelsOnce(client, ctx, request, candidate.url)
		if cause == nil {
			return models, nil
		}
		causes = append(causes, candidate.label+": "+cause.Error())
	}
	return nil, fmt.Errorf("cannot read this custom channel's model list (%s)", strings.Join(causes, "; "))
}

// fetchCustomChannelModelsOnce performs exactly one model-list request with the
// channel's own credentials (Bearer key plus every configured custom header), the
// same way the OpenAI-compatible fetch path does.
func fetchCustomChannelModelsOnce(client *http.Client, ctx context.Context, request model.Channel, modelURL string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelURL, nil)
	if err != nil {
		return nil, errors.New("the request could not be built from the channel URL")
	}
	req.Header.Set("Authorization", "Bearer "+request.GetChannelKey().ChannelKey)
	for _, header := range request.CustomHeader {
		if header.HeaderKey != "" {
			req.Header.Set(header.HeaderKey, header.HeaderValue)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %s", customModelsTransportCause(err))
	}
	defer resp.Body.Close()

	// Status only. A failed model-list body is arbitrary upstream/gateway text that
	// can echo a credential or the URL the gateway built, and this message reaches
	// the UI and the logs. Non-custom channels keep their existing body snippet.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
	}

	var result model.OpenAIModelList
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, errors.New("the response is not a JSON model list")
	}
	models := make([]string, 0, len(result.Data))
	for _, item := range result.Data {
		if id := strings.TrimSpace(item.ID); id != "" {
			models = append(models, id)
		}
	}
	if len(models) == 0 {
		return nil, errors.New("the response contained an empty model list")
	}
	return models, nil
}

// customModelsTransportCause renders a transport failure as one of a few fixed
// causes. The underlying error is deliberately not printed: it can carry the
// request URL (including userinfo) and the resolved host, and this text reaches
// the UI and the logs.
func customModelsTransportCause(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "the request timed out"
	case errors.Is(err, context.Canceled):
		return "the request was cancelled"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "the request timed out"
	}
	return "the request could not be completed"
}

// refer: https://ai.google.dev/api/models
func fetchGeminiModels(client *http.Client, ctx context.Context, request model.Channel) ([]string, error) {
	var allModels []string
	pageToken := ""

	for {
		modelURL, err := xurl.JoinGeminiPath(request.GetBaseUrl(), "/v1beta/models")
		if err != nil {
			return nil, err
		}

		req, err := http.NewRequestWithContext(
			ctx,
			http.MethodGet,
			modelURL,
			nil,
		)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-Goog-Api-Key", request.GetChannelKey().ChannelKey)
		for _, header := range request.CustomHeader {
			if header.HeaderKey != "" {
				req.Header.Set(header.HeaderKey, header.HeaderValue)
			}
		}
		if pageToken != "" {
			q := req.URL.Query()
			q.Add("pageToken", pageToken)
			req.URL.RawQuery = q.Encode()
		}

		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		if err := ensureFetchModelsOK(resp); err != nil {
			return nil, err
		}

		var result model.GeminiModelList

		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return nil, err
		}

		for _, m := range result.Models {
			name := strings.TrimPrefix(m.Name, "models/")
			allModels = append(allModels, name)
		}

		if result.NextPageToken == "" {
			break
		}
		pageToken = result.NextPageToken
	}
	if len(allModels) == 0 {
		return fetchOpenAIModels(client, ctx, request)
	}
	return allModels, nil
}

// refer: https://platform.claude.com/docs
func fetchAnthropicModels(client *http.Client, ctx context.Context, request model.Channel) ([]string, error) {

	var allModels []string
	var afterID string
	for {
		modelURL, err := xurl.JoinAnthropicPath(request.GetBaseUrl(), "/v1/models")
		if err != nil {
			return nil, err
		}

		req, err := http.NewRequestWithContext(
			ctx,
			http.MethodGet,
			modelURL,
			nil,
		)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-Api-Key", request.GetChannelKey().ChannelKey)
		req.Header.Set("Anthropic-Version", "2023-06-01")
		for _, header := range request.CustomHeader {
			if header.HeaderKey != "" {
				req.Header.Set(header.HeaderKey, header.HeaderValue)
			}
		}
		// 设置多页参数
		q := req.URL.Query()

		if afterID != "" {
			q.Set("after_id", afterID)
		}
		req.URL.RawQuery = q.Encode()

		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		if err := ensureFetchModelsOK(resp); err != nil {
			if len(allModels) == 0 {
				// Some Anthropic-compatible upstreams (new-api-style proxies) reject
				// the native Anthropic model-list auth (X-Api-Key) and only serve an
				// OpenAI-style /v1/models behind Bearer auth, returning 401
				// "no token provided". Fall back to the OpenAI-style listing (same
				// fallback as the empty-result case below) instead of surfacing the
				// auth error. Real Anthropic upstreams accept X-Api-Key and never
				// reach this branch.
				return fetchOpenAIModels(client, ctx, request)
			}
			break
		}

		var result model.AnthropicModelList

		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return nil, err
		}

		for _, m := range result.Data {
			allModels = append(allModels, m.ID)
		}

		if !result.HasMore {
			break
		}

		afterID = result.LastID
	}
	if len(allModels) == 0 {
		return fetchOpenAIModels(client, ctx, request)
	}
	return allModels, nil
}
