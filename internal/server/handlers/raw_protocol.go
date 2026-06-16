package handlers

import (
	"net/http"

	"github.com/bestruirui/octopus/internal/relay"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/server/router"
	"github.com/gin-gonic/gin"
)

func init() {
	router.NewGroupRouter("/v1").
		Use(middleware.APIKeyAuth()).
		AddRoute(
			router.NewRoute("/completions", http.MethodPost).
				Handle(completions),
		).
		AddRoute(
			router.NewRoute("/edits", http.MethodPost).
				Handle(legacyEdits),
		).
		AddRoute(
			router.NewRoute("/responses/compact", http.MethodPost).
				Handle(responsesCompact),
		).
		AddRoute(
			router.NewRoute("/audio/speech", http.MethodPost).
				Handle(audioSpeech),
		).
		AddRoute(
			router.NewRoute("/audio/transcriptions", http.MethodPost).
				Handle(audioTranscriptions),
		).
		AddRoute(
			router.NewRoute("/audio/translations", http.MethodPost).
				Handle(audioTranslations),
		).
		AddRoute(
			router.NewRoute("/moderations", http.MethodPost).
				Handle(moderations),
		).
		AddRoute(
			router.NewRoute("/rerank", http.MethodPost).
				Handle(rerank),
		)

	router.NewGroupRouter("").
		Use(middleware.APIKeyAuth()).
		AddRoute(
			router.NewRoute("/responses/compact", http.MethodPost).
				Handle(responsesCompact),
		)

	router.NewGroupRouter("/backend-api/codex").
		Use(middleware.APIKeyAuth()).
		AddRoute(
			router.NewRoute("/responses/compact", http.MethodPost).
				Handle(responsesCompact),
		)
}

func completions(c *gin.Context) {
	relay.RawProtocolHandler(relay.RawProtocolOptions{
		Endpoint: "/completions",
		Name:     "completions",
	}, c)
}

func legacyEdits(c *gin.Context) {
	relay.RawProtocolHandler(relay.RawProtocolOptions{
		Endpoint: "/edits",
		Name:     "edits",
	}, c)
}

func responsesCompact(c *gin.Context) {
	relay.RawProtocolHandler(relay.RawProtocolOptions{
		Endpoint:   "/responses/compact",
		Name:       "responses_compact",
		NonBilling: true,
	}, c)
}

func audioSpeech(c *gin.Context) {
	relay.RawProtocolHandler(relay.RawProtocolOptions{
		Endpoint:        "/audio/speech",
		Name:            "audio_speech",
		BinaryResponse:  true,
		ResponseLogNote: "audio response omitted for storage",
	}, c)
}

func audioTranscriptions(c *gin.Context) {
	relay.RawProtocolHandler(relay.RawProtocolOptions{
		Endpoint: "/audio/transcriptions",
		Name:     "audio_transcriptions",
	}, c)
}

func audioTranslations(c *gin.Context) {
	relay.RawProtocolHandler(relay.RawProtocolOptions{
		Endpoint: "/audio/translations",
		Name:     "audio_translations",
	}, c)
}

func moderations(c *gin.Context) {
	relay.RawProtocolHandler(relay.RawProtocolOptions{
		Endpoint:     "/moderations",
		Name:         "moderations",
		DefaultModel: "omni-moderation-latest",
	}, c)
}

func rerank(c *gin.Context) {
	relay.RawProtocolHandler(relay.RawProtocolOptions{
		Endpoint: "/rerank",
		Name:     "rerank",
	}, c)
}
