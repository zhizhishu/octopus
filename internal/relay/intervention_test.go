package relay

import (
	"context"
	"errors"
	"net/http"
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
)

func setInterventionEnabledForTest(t *testing.T, enabled bool) {
	t.Helper()
	val := "false"
	if enabled {
		val = "true"
	}
	if err := op.SettingSetString(dbmodel.SettingKeyRelayInterventionEnabled, val); err != nil {
		t.Fatalf("failed to set intervention enabled: %v", err)
	}
}

func TestShouldHoldForOperator(t *testing.T) {
	setupRelayErrorDB(t)

	streamTrue := true
	streamFalse := false

	tests := []struct {
		name              string
		enabled           bool
		streamPrefers     *bool
		wroteBusinessData bool
		contextWindowErr  error
		finalErr          error
		wantHold          bool
	}{
		{
			name:              "intervention disabled -> false",
			enabled:           false,
			streamPrefers:     &streamTrue,
			wroteBusinessData: false,
			finalErr:          errors.New("502 bad gateway"),
			wantHold:          false,
		},
		{
			name:              "eligible stream request with transient upstream failure -> true",
			enabled:           true,
			streamPrefers:     &streamTrue,
			wroteBusinessData: false,
			finalErr:          errors.New("upstream stream ended without internal response"),
			wantHold:          true,
		},
		{
			name:              "non-stream request cannot hold -> false",
			enabled:           true,
			streamPrefers:     &streamFalse,
			wroteBusinessData: false,
			finalErr:          errors.New("502 bad gateway"),
			wantHold:          false,
		},
		{
			name:              "already wrote business data to client -> false",
			enabled:           true,
			streamPrefers:     &streamTrue,
			wroteBusinessData: true,
			finalErr:          errors.New("502 bad gateway"),
			wantHold:          false,
		},
		{
			name:              "client canceled context -> false",
			enabled:           true,
			streamPrefers:     &streamTrue,
			wroteBusinessData: false,
			finalErr:          context.Canceled,
			wantHold:          false,
		},
		{
			name:              "context window error in contextWindowErr -> false",
			enabled:           true,
			streamPrefers:     &streamTrue,
			wroteBusinessData: false,
			contextWindowErr:  errors.New("context length exceeded"),
			finalErr:          errors.New("context length exceeded"),
			wantHold:          false,
		},
		{
			name:              "context window error in finalErr -> false",
			enabled:           true,
			streamPrefers:     &streamTrue,
			wroteBusinessData: false,
			finalErr: newUpstreamError(http.StatusBadRequest, []byte(`{
				"error": {
					"message": "prompt is too long: 201015 tokens > 200000 maximum",
					"type": "invalid_request_error"
				}
			}`)),
			wantHold: false,
		},
		{
			name:              "request invalid / parsing error -> false",
			enabled:           true,
			streamPrefers:     &streamTrue,
			wroteBusinessData: false,
			finalErr: newUpstreamError(http.StatusBadRequest, []byte(`{
				"error": {
					"message": "Failed to deserialize the JSON body into ChatCompletionRequest",
					"type": "invalid_request_error"
				}
			}`)),
			wantHold: false,
		},
		{
			name:              "unsupported responses endpoint -> false",
			enabled:           true,
			streamPrefers:     &streamTrue,
			wroteBusinessData: false,
			finalErr: newUpstreamError(http.StatusBadRequest, []byte(`{
				"error": {
					"message": "responses endpoint is not supported by this compatible upstream",
					"code": "invalid_request_error"
				}
			}`)),
			wantHold: false,
		},
		{
			name:              "transient 429 rate limit is eligible -> true",
			enabled:           true,
			streamPrefers:     &streamTrue,
			wroteBusinessData: false,
			finalErr:          newUpstreamError(http.StatusTooManyRequests, []byte(`{"error":{"message":"Rate limit reached"}}`)),
			wantHold:          true,
		},
		{
			name:              "transient 503 service unavailable is eligible -> true",
			enabled:           true,
			streamPrefers:     &streamTrue,
			wroteBusinessData: false,
			finalErr:          newUpstreamError(http.StatusServiceUnavailable, []byte(`{"error":{"message":"Overloaded"}}`)),
			wantHold:          true,
		},
		{
			name:              "deterministic 404 is not eligible -> false",
			enabled:           true,
			streamPrefers:     &streamTrue,
			wroteBusinessData: false,
			finalErr:          newUpstreamError(http.StatusNotFound, []byte(`{"error":{"message":"Resource not found"}}`)),
			wantHold:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setInterventionEnabledForTest(t, tt.enabled)

			req := &relayRequest{
				internalRequest: &transformerModel.InternalLLMRequest{
					Stream: tt.streamPrefers,
				},
				wroteBusinessData: tt.wroteBusinessData,
			}

			got := shouldHoldForOperator(req, tt.contextWindowErr, tt.finalErr)
			if got != tt.wantHold {
				t.Errorf("shouldHoldForOperator() = %v, want %v", got, tt.wantHold)
			}
		})
	}
}
