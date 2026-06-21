package handlers

import (
	"net/http"
	"strings"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/modeltest"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/price"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/server/router"
	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
)

func init() {
	router.NewGroupRouter("/api/v1/model").
		Use(middleware.Auth()).
		Use(middleware.AdminOnly()).
		Use(middleware.RequireJSON()).
		AddRoute(
			router.NewRoute("/list", http.MethodGet).
				Handle(listLLM),
		).
		AddRoute(
			router.NewRoute("/create", http.MethodPost).
				Handle(createLLM),
		).
		AddRoute(
			router.NewRoute("/channel", http.MethodGet).
				Handle(listLLMByChannel),
		).
		AddRoute(
			router.NewRoute("/update", http.MethodPost).
				Handle(updateLLM),
		).
		AddRoute(
			router.NewRoute("/delete", http.MethodPost).
				Handle(deleteLLM),
		).
		AddRoute(
			router.NewRoute("/update-price", http.MethodPost).
				Handle(updateLLMPrice),
		).
		AddRoute(
			router.NewRoute("/last-update-time", http.MethodGet).
				Handle(getLastUpdateTime),
		).
		AddRoute(
			router.NewRoute("/test", http.MethodPost).
				Handle(testLLM),
		)
	router.NewGroupRouter("/v1").
		Use(middleware.APIKeyAuth()).
		AddRoute(
			router.NewRoute("/models", http.MethodGet).
				Handle(getModelList),
		).
		AddRoute(
			// GET /v1/models/{id} -> single model detail. Some OpenAI-compatible
			// tools probe a specific model id to validate availability. The model
			// source is identical to GET /v1/models.
			router.NewRoute("/models/:id", http.MethodGet).
				Handle(getModel),
		)
}

// apiKeyAllowedModels resolves the model list for the current API key: the
// group plan models, narrowed by the key's supported_models allow-list and then
// by the endpoint family. This is the single source of truth shared by
// GET /v1/models (list) and GET /v1/models/{id} (single). It writes an error
// response and returns ok=false on failure.
func apiKeyAllowedModels(c *gin.Context) (models []string, ok bool) {
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
	if family := modelListFilterFamily(c); family != "" {
		models = op.FilterModelNamesForEndpointFamily(c.Request.Context(), models, family)
	}
	return models, true
}

// modelListFilterFamily picks the endpoint family used to narrow the model list
// to what the caller can actually invoke. It must follow the CLIENT's protocol,
// not the static path classification: GET /v1/models is path-classified as
// openai-compatible, but an Anthropic client (x-api-key -> request_type
// "anthropic") that lists models will then call /v1/messages and must see
// anthropic-channel models such as Claude. Filtering by the path family instead
// wrongly hid every model served only by Anthropic/Gemini channels from those
// clients. Fall back to the path-derived family only when the request type is
// unknown (e.g. internal probes).
func modelListFilterFamily(c *gin.Context) model.APIKeyEndpointFamily {
	switch c.GetString("request_type") {
	case "anthropic":
		return model.APIKeyEndpointFamilyAnthropic
	case "gemini":
		return model.APIKeyEndpointFamilyGemini
	case "openai":
		return model.APIKeyEndpointFamilyOpenAICompatible
	}
	if endpointFamily := c.GetString("endpoint_family"); endpointFamily != "" {
		return model.APIKeyEndpointFamily(endpointFamily)
	}
	return ""
}

func getModelList(c *gin.Context) {
	models, ok := apiKeyAllowedModels(c)
	if !ok {
		return
	}

	if c.GetString("request_type") == "anthropic" {
		var anthropicModels []model.AnthropicModel
		for _, m := range models {
			anthropicModels = append(anthropicModels, model.AnthropicModel{
				ID:          m,
				CreatedAt:   "2024-01-01T00:00:00Z",
				DisplayName: m,
				Type:        "model",
			})
		}
		response := gin.H{
			"data":     anthropicModels,
			"has_more": false,
		}
		if len(anthropicModels) > 0 {
			response["first_id"] = anthropicModels[0].ID
			response["last_id"] = anthropicModels[len(anthropicModels)-1].ID
		}
		c.JSON(200, response)
	} else {
		var openAIModels []model.OpenAIModel
		for _, m := range models {
			openAIModels = append(openAIModels, model.OpenAIModel{
				ID:      m,
				Object:  "model",
				Created: 1763395200,
				OwnedBy: "octopus",
			})
		}
		c.JSON(200, gin.H{
			"success": true,
			"data":    openAIModels,
			"object":  "list",
		})
	}
}

// getModel implements GET /v1/models/{id}. It returns a single OpenAI-format
// model object only if the requested id is in the key's allowed model set (same
// source as GET /v1/models); otherwise it responds 404. Tools that validate a
// single model call this endpoint.
func getModel(c *gin.Context) {
	requested := strings.TrimSpace(c.Param("id"))
	if requested == "" {
		resp.Error(c, http.StatusNotFound, "model not found")
		return
	}

	models, ok := apiKeyAllowedModels(c)
	if !ok {
		return
	}

	for _, m := range models {
		if m == requested {
			c.JSON(http.StatusOK, model.OpenAIModel{
				ID:      m,
				Object:  "model",
				Created: 1763395200,
				OwnedBy: "octopus",
			})
			return
		}
	}
	resp.Error(c, http.StatusNotFound, "model not found")
}

func modelListAccessPlanHeader(c *gin.Context) string {
	if value := strings.TrimSpace(c.GetHeader("X-Octopus-Plan")); value != "" {
		return value
	}
	return strings.TrimSpace(c.GetHeader("X-Octopus-Group"))
}

func listLLM(c *gin.Context) {
	models, err := op.LLMList(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, models)
}

func listLLMByChannel(c *gin.Context) {
	channels, err := op.ChannelLLMList(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, channels)
}

func createLLM(c *gin.Context) {
	var model model.LLMInfo
	if err := c.ShouldBindJSON(&model); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := op.LLMCreate(model, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, model)
}

func updateLLM(c *gin.Context) {
	var model model.LLMInfo
	if err := c.ShouldBindJSON(&model); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := op.LLMUpdate(model, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, model)
}

func deleteLLM(c *gin.Context) {
	var req struct {
		Name string `json:"name" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := op.LLMDelete(req.Name, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, nil)
}

func updateLLMPrice(c *gin.Context) {
	err := price.UpdateLLMPrice(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, nil)
}

func getLastUpdateTime(c *gin.Context) {
	time := price.GetLastUpdateTime()
	resp.Success(c, time)
}

func testLLM(c *gin.Context) {
	var request model.ModelTestRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}

	result, err := modeltest.Run(c.Request.Context(), request)
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, result)
}
