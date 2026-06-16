package handlers

import (
	"net/http"
	"strings"

	"github.com/bestruirui/octopus/internal/relay"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/server/router"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	geminiInbound "github.com/bestruirui/octopus/internal/transformer/inbound/gemini"
	"github.com/gin-gonic/gin"
)

func init() {
	router.NewGroupRouter("/v1beta").
		Use(middleware.APIKeyAuth()).
		Use(middleware.RequireJSON()).
		AddRoute(
			router.NewRoute("/models/*modelAction", http.MethodPost).
				Handle(geminiNative),
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
