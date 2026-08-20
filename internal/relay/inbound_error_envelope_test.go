package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/gin-gonic/gin"
)

// TestWriteRelayErrorPreStreamEmitsOpenAIEnvelopeForResponsesInbound locks the
// cursor "OpenAI Responses API failed: unknown error" fix: on the /v1/responses
// inbound path, octopus must ALWAYS emit the OpenAI error shape
// {"error":{"message":..,"type":..,"code":..}} for pre-stream errors (model not
// supported, route selection, parseRequest validation, all channels failed pre-commit).
// Other inbound types keep their existing octopus-internal ResponseStruct envelope.
func TestWriteRelayErrorPreStreamEmitsOpenAIEnvelopeForResponsesInbound(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("responses_inbound_gets_openai_error_shape", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)

		writeRelayErrorPreStream(c, inbound.InboundTypeOpenAIResponse, http.StatusBadRequest, "invalid_request_error", "model_not_supported", "model not supported")

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected status 400, got %d", w.Code)
		}

		var payload map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatalf("unmarshal error body: %v, raw=%s", err, w.Body.String())
		}

		errObj, ok := payload["error"].(map[string]any)
		if !ok {
			t.Fatalf("expected top-level 'error' object in OpenAI envelope, got %v", payload)
		}
		if errObj["message"] != "model not supported" {
			t.Errorf("expected error.message='model not supported', got %v", errObj["message"])
		}
		if errObj["type"] != "invalid_request_error" {
			t.Errorf("expected error.type='invalid_request_error', got %v", errObj["type"])
		}
		if errObj["code"] != "model_not_supported" {
			t.Errorf("expected error.code='model_not_supported', got %v", errObj["code"])
		}

		// Confirm octopus-internal top-level fields are absent
		if _, hasCode := payload["code"]; hasCode {
			t.Errorf("OpenAI envelope must not carry top-level 'code', got %v", payload)
		}
		if _, hasErrorCode := payload["error_code"]; hasErrorCode {
			t.Errorf("OpenAI envelope must not carry top-level 'error_code', got %v", payload)
		}
	})

	t.Run("chat_inbound_keeps_internal_response_struct", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)

		// Chat inbound (inboundType=0 or other) uses default branch -> resp.ErrorWithCode
		writeRelayErrorPreStream(c, inbound.InboundType(999), http.StatusBadRequest, "invalid_request_error", "client_validation_error", "bad body")

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected status 400, got %d", w.Code)
		}

		var payload map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatalf("unmarshal error body: %v, raw=%s", err, w.Body.String())
		}

		// Default branch emits ResponseStruct {code, error_code, message}
		if _, hasErrorObj := payload["error"]; hasErrorObj {
			t.Errorf("non-responses inbound default branch must keep internal envelope, got %v", payload)
		}
		if payload["message"] != "bad body" {
			t.Errorf("expected message='bad body', got %v", payload["message"])
		}
		if payload["error_code"] != "client_validation_error" {
			t.Errorf("expected error_code='client_validation_error', got %v", payload["error_code"])
		}
	})
}
