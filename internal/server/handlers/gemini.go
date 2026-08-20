package handlers

import (
	"net/http"
	"strings"

	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/server/router"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	geminiInbound "github.com/bestruirui/octopus/internal/transformer/inbound/gemini"
	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
)

func init() {
	router.NewGroupRouter("/v1beta").
		Use(middleware.APIKeyAuth()).
		Use(middleware.RequireJSON()).
		AddRoute(
			router.NewRoute("/models/*modelAction", http.MethodPost).
				Handle(geminiNative),
		).
		AddRoute(
			// GET /v1beta/models -> list models. Registered as a static route so
			// the common no-trailing-slash request hits it directly (no 301 that
			// could drop the x-goog-api-key header).
			router.NewRoute("/models", http.MethodGet).
				Handle(geminiListModels),
		).
		AddRoute(
			// GET /v1beta/models/{model} -> single model. Gemini-native IDE
			// plugins call these to discover available models.
			router.NewRoute("/models/*modelAction", http.MethodGet).
				Handle(geminiGetModel),
		)
}

func geminiNative(c *gin.Context) {
	modelName, stream, ok := parseGeminiModelAction(c.Param("modelAction"))
	if !ok {
		resp.Error(c, http.StatusBadRequest, "invalid gemini model action")
		return
	}
	q := c.Request.URL.Query()
	q.Del("alt")
	c.Request.URL.RawQuery = q.Encode()

	ctx := geminiInbound.WithRequestOptions(c.Request.Context(), modelName, stream)
	c.Request = c.Request.WithContext(ctx)
	relay.Handler(inbound.InboundTypeGemini, c)
}

// geminiModel is the Gemini-native model descriptor returned by
// GET /v1beta/models. Defined locally so we do not modify the shared model
// package (internal/model) which is out of scope here.
type geminiModel struct {
	Name                       string   `json:"name"`
	DisplayName                string   `json:"displayName"`
	SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
}

type geminiModelListResponse struct {
	Models []geminiModel `json:"models"`
}

// geminiListModels implements GET /v1beta/models. Model source is identical to
// GET /v1/models: the request key's allowed models (group plan ->
// supported_models filter -> gemini endpoint family filter).
func geminiListModels(c *gin.Context) {
	models, ok := geminiAllowedModels(c)
	if !ok {
		return
	}

	list := geminiModelListResponse{Models: make([]geminiModel, 0, len(models))}
	for _, m := range models {
		list.Models = append(list.Models, newGeminiModel(m))
	}
	c.JSON(http.StatusOK, list)
}

// geminiGetModel implements GET /v1beta/models/{model}. It returns the single
// matching model from the key's allowed set, or 404 if not allowed/unknown.
func geminiGetModel(c *gin.Context) {
	requested := parseGeminiModelGet(c.Param("modelAction"))
	if requested == "" {
		resp.Error(c, http.StatusBadRequest, "invalid gemini model name")
		return
	}

	models, ok := geminiAllowedModels(c)
	if !ok {
		return
	}

	for _, m := range models {
		if strings.EqualFold(m, requested) {
			c.JSON(http.StatusOK, newGeminiModel(m))
			return
		}
	}
	resp.Error(c, http.StatusNotFound, "model not found")
}

func newGeminiModel(id string) geminiModel {
	// Output official Google-style "models/" prefix for strict SDK compatibility.
	// parseGeminiModelAction and parseGeminiModelGet defensively strip both single
	// and doubled "models/" prefixes so bare-name and prefixed clients both route.
	modelName := id
	if !strings.HasPrefix(modelName, "models/") {
		modelName = "models/" + modelName
	}
	return geminiModel{
		Name:        modelName,
		DisplayName: id,
		SupportedGenerationMethods: []string{
			"generateContent",
			"streamGenerateContent",
		},
	}
}

// geminiAllowedModels resolves the model list for the current API key, mirroring
// getModelList in model.go. It writes an error response and returns ok=false on
// failure.
func geminiAllowedModels(c *gin.Context) (models []string, ok bool) {
	apiKeyId := c.GetInt("api_key_id")
	models, err := op.GroupListModelForAPIKeyPlan(apiKeyId, modelListAccessPlanHeader(c), c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusForbidden, err.Error())
		return nil, false
	}
	apiKey, err := op.APIKeyGet(apiKeyId, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return nil, false
	}
	if apiKey.SupportedModels != "" {
		supportedModels := op.SupportedModelsList(apiKey.SupportedModels)
		models = lo.Filter(models, func(m string, _ int) bool {
			return lo.Contains(supportedModels, m)
		})
	}
	// Deliberately NOT filtered by channel type / endpoint family, matching
	// the OpenAI /v1/models and Anthropic inbound list behavior (see
	// model.go getModelList). Octopus routes cross-protocol, so a Gemini
	// inbound key must see chat/anthropic/custom channels' models too.
	return models, true
}

// parseGeminiModelGet extracts a single model name from the GET path segment.
// Empty (i.e. "/v1beta/models") means "list all". A bare model name
// (e.g. "/v1beta/models/gemini-2.5-flash") means "fetch that model". Any
// action suffix (":generateContent") is ignored for GET.
func parseGeminiModelGet(raw string) string {
	raw = strings.TrimPrefix(strings.TrimSpace(raw), "/")
	if raw == "" {
		return ""
	}
	if idx := strings.LastIndex(raw, ":"); idx >= 0 {
		raw = strings.TrimSpace(raw[:idx])
	}
	raw = strings.TrimPrefix(raw, "models/")
	return raw
}

func parseGeminiModelAction(raw string) (modelName string, stream bool, ok bool) {
	raw = strings.TrimPrefix(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", false, false
	}

	idx := strings.LastIndex(raw, ":")
	if idx < 1 || idx == len(raw)-1 {
		return "", false, false
	}
	modelName = strings.TrimSpace(raw[:idx])
	// A Gemini-native client that picked a model from GET /v1beta/models sends it back
	// WITH the "models/" prefix (that list is emitted Google-style as "models/<id>"),
	// producing /v1beta/models/models/<id>:action here so modelName keeps a redundant
	// "models/" prefix. Strip it so routing matches the bare model name Octopus stores
	// internally (mirrors internal/helper/fetch.go, which strips "models/" when it
	// ingests an upstream Gemini model list). Bare names pass through unchanged.
	modelName = strings.TrimPrefix(modelName, "models/")
	action := strings.TrimSpace(raw[idx+1:])

	switch action {
	case "generateContent":
		return modelName, false, modelName != ""
	case "streamGenerateContent":
		return modelName, true, modelName != ""
	default:
		return "", false, false
	}
}
