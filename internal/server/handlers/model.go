package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/modeltest"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/price"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/resp"
	"github.com/zhizhishu/octopus/internal/server/router"
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
			router.NewRoute("/price-sync/preview", http.MethodPost).
				Handle(previewPriceSync),
		).
		AddRoute(
			router.NewRoute("/price-sync/apply", http.MethodPost).
				Handle(applyPriceSync),
		).
		AddRoute(
			router.NewRoute("/price-sync/status", http.MethodGet).
				Handle(getPriceSyncStatus),
		).
		AddRoute(
			router.NewRoute("/restore-price", http.MethodPost).
				Handle(restoreLLMPrice),
		).
		AddRoute(
			router.NewRoute("/test", http.MethodPost).
				Handle(testLLM),
		).
		AddRoute(
			router.NewRoute("/test-identity", http.MethodGet).
				Handle(testIdentityLLM),
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
	// Deliberately NOT filtered by channel type / endpoint family. octopus
	// transforms between the Anthropic / OpenAI / Gemini protocols, so a model
	// served by ANY enabled channel is callable by a client on ANY inbound
	// protocol — e.g. an Anthropic (x-api-key) client can use a model that lives
	// on an OpenAI-compatible channel because oct converts the request on the way
	// out (verified in production: a Claude request was served by an OpenAI
	// channel). The previous path-based family filter classified GET /v1/models as
	// openai-compatible and dropped every Anthropic/Gemini-channel model (e.g.
	// Claude), hiding usable models from clients. The key's supported_models
	// allow-list above is the only intended narrowing of the list.
	return models, true
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

// llmListItem is a price row plus the library price the catalog has for it, so the
// price page can tell "没价" (no price anywhere, billed at 0) from a placeholder row
// that bills at the library price.
type llmListItem struct {
	model.LLMInfo
	Library *price.SyncPrice `json:"library,omitempty"`
}

func listLLM(c *gin.Context) {
	models, err := op.LLMList(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]llmListItem, 0, len(models))
	for _, m := range models {
		item := llmListItem{LLMInfo: m}
		if lib, ok := price.CatalogPrice(m.Name); ok {
			sp := price.SyncPrice{Input: lib.Input, Output: lib.Output, CacheRead: lib.CacheRead, CacheWrite: lib.CacheWrite}
			item.Library = &sp
		}
		out = append(out, item)
	}
	resp.Success(c, out)
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
	// A price typed into the create dialog is the admin's own; an empty row is a
	// placeholder that follows the library.
	model.Source = sourceForEditedPrice(model.LLMPrice)
	if err := op.LLMCreate(model, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, model)
}

// updateLLM merges the request onto the stored price row: only the price fields
// the client actually sent change. Binding into a zero LLMInfo and saving the
// whole row would silently reset any field an older client does not know about
// (per_call was wiped this way, turning off per-call billing for video models).
//
// Changing a token rate marks the row manual (手动) so the library sync stops
// rewriting it; "恢复上游价" (/model/restore-price) is the way back. Editing only
// per_call keeps the source: the library has no per-call prices.
func updateLLM(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	var ident struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &ident); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := op.LLMUpdateWith(ident.Name, c.Request.Context(), func(stored model.LLMInfo) (model.LLMInfo, error) {
		next := model.LLMInfo{Name: stored.Name, LLMPrice: stored.LLMPrice}
		if err := json.Unmarshal(body, &next); err != nil {
			return model.LLMInfo{}, errBadUpdateBody{err}
		}
		next.Source = stored.Source
		if !next.LLMPrice.SameTokenPrice(stored.LLMPrice) {
			next.Source = sourceForEditedPrice(next.LLMPrice)
		}
		return next, nil
	})
	if err != nil {
		var bad errBadUpdateBody
		if errors.As(err, &bad) {
			resp.Error(c, http.StatusBadRequest, bad.err.Error())
			return
		}
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, updated)
}

type errBadUpdateBody struct{ err error }

func (e errBadUpdateBody) Error() string { return e.err.Error() }

// sourceForEditedPrice: a typed token price is manual. All-zero token rates are a
// placeholder: billing falls through to the library for them anyway (see
// price.lookupLLMPrice), so calling them manual would claim a lock that does not exist.
func sourceForEditedPrice(p model.LLMPrice) string {
	if p.TokenPriceIsZero() {
		return model.LLMSourceLibrary
	}
	return model.LLMSourceManual
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

// previewPriceSync fetches models.dev and returns the diff for the models in your
// channels. It writes no price row.
func previewPriceSync(c *gin.Context) {
	preview, err := price.Preview(c.Request.Context())
	if err != nil {
		// models.dev unreachable or abnormally small: an upstream problem, nothing written.
		resp.Error(c, http.StatusBadGateway, err.Error())
		return
	}
	resp.Success(c, preview)
}

// applyPriceSync writes the ticked preview rows. 409 when any row (or its library
// price) changed since the preview, like new-api's ErrModelPricingConflict.
func applyPriceSync(c *gin.Context) {
	var req struct {
		Changes []price.SyncApplyChange `json:"changes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.Changes) == 0 {
		resp.Error(c, http.StatusBadRequest, "no changes selected")
		return
	}
	res, err := price.Apply(c.Request.Context(), req.Changes)
	if err != nil {
		if errors.Is(err, op.ErrLLMPriceConflict) {
			resp.Error(c, http.StatusConflict, err.Error())
			return
		}
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, res)
}

func getPriceSyncStatus(c *gin.Context) {
	st, err := price.Status(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, st)
}

// restoreLLMPrice ("恢复上游价") puts a row back on the library price and makes it
// follow the library again. per_call is kept.
func restoreLLMPrice(c *gin.Context) {
	var req struct {
		Name string `json:"name" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	info, err := price.RestoreLibraryPrice(c.Request.Context(), req.Name)
	if err != nil {
		if errors.Is(err, price.ErrNoLibraryPrice) {
			resp.Error(c, http.StatusNotFound, err.Error())
			return
		}
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, info)
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

// testIdentityLLM returns the per-endpoint upstream identity (shape + UA) a
// channel test will present, resolved from the channel's fingerprint profile.
// Read-only: powers the test dialog's identity readout so the operator sees which
// CLI shape each endpoint simulates. It never overrides the channel's profile —
// the test always uses the channel's real identity (test==real-traffic invariant).
func testIdentityLLM(c *gin.Context) {
	channelID, err := strconv.Atoi(strings.TrimSpace(c.Query("channel_id")))
	if err != nil || channelID <= 0 {
		resp.Error(c, http.StatusBadRequest, "invalid channel_id")
		return
	}
	channel, err := op.ChannelGet(channelID, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusNotFound, "channel not found")
		return
	}
	resp.Success(c, modeltest.ResolveChannelTestIdentity(channel))
}
