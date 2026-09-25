package handlers

import (
	"net/http"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/redact"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/server/router"
	"github.com/gin-gonic/gin"
)

// Credential-redaction admin endpoints. The test endpoint lets the operator
// verify the redact->restore round trip with a synthetic sample from the
// redaction management page before enabling it on live channels.

type redactTestRequest struct {
	Text  string `json:"text" binding:"required"`
	Flags string `json:"flags"`
}

type redactTestResponse struct {
	Redacted string `json:"redacted"`
	Restored string `json:"restored"`
	Count    int    `json:"count"`
}

func init() {
	router.NewGroupRouter("/api/v1/redact").
		Use(middleware.Auth()).
		Use(middleware.AdminOnly()).
		AddRoute(
			router.NewRoute("/test", http.MethodPost).
				Use(middleware.RequireJSON()).
				Handle(postRedactTest),
		)
}

// postRedactTest runs a synthetic sample through redaction and restoration:
// the request text is redacted with the given flags (empty = all detectors),
// then the redacted text is restored as if the model had echoed every
// placeholder back. The response shows both halves so the operator can see
// exactly what the upstream would receive and what the client would get back.
func postRedactTest(c *gin.Context) {
	var req redactTestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if model.HasInvalidRedactFlag(req.Flags) {
		resp.Error(c, http.StatusBadRequest, "flags may only contain the detector letters H P S I B E G (empty = all detectors)")
		return
	}
	// Cap the sample: the JS interpreter runs ~0.13MB/s, so an unbounded paste
	// would pin a CPU for minutes. 64KB is plenty for a verification sample.
	if len(req.Text) > 64*1024 {
		resp.Error(c, http.StatusBadRequest, "text too large for the test endpoint (max 64KB)")
		return
	}
	flags := model.NormalizeRedactFlags(req.Flags)

	engine, err := redact.NewEngine()
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	s, err := engine.NewSession(flags, "generic", false)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	defer s.Close()

	redacted, err := s.RedactText(req.Text)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	restored, err := s.RestoreText(redacted)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, redactTestResponse{
		Redacted: redacted,
		Restored: restored,
		Count:    s.Count(),
	})
}
