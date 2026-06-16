package relay

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/gin-gonic/gin"
)

type responseLimitOutbound struct{}

func (responseLimitOutbound) TransformRequest(context.Context, *model.InternalLLMRequest, string, string) (*http.Request, error) {
	return nil, nil
}

func (responseLimitOutbound) TransformResponse(_ context.Context, response *http.Response) (*model.InternalLLMResponse, error) {
	_, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	return &model.InternalLLMResponse{}, nil
}

func (responseLimitOutbound) TransformStream(context.Context, []byte) (*model.InternalLLMResponse, error) {
	return nil, nil
}

type responseLimitInbound struct{}

func (responseLimitInbound) TransformRequest(context.Context, []byte) (*model.InternalLLMRequest, error) {
	return nil, nil
}

func (responseLimitInbound) TransformResponse(context.Context, *model.InternalLLMResponse) ([]byte, error) {
	return []byte(`{}`), nil
}

func (responseLimitInbound) TransformStream(context.Context, *model.InternalLLMResponse) ([]byte, error) {
	return nil, nil
}

func (responseLimitInbound) GetInternalResponse(context.Context) (*model.InternalLLMResponse, error) {
	return nil, nil
}

func TestLimitUpstreamResponseBodyAllowsExactLimit(t *testing.T) {
	t.Setenv("OCTOPUS_RELAY_UPSTREAM_RESPONSE_READ_MAX_BYTES", "3")

	response := &http.Response{Body: io.NopCloser(strings.NewReader("abc"))}
	limitUpstreamResponseBody(response)

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read exact-limit response: %v", err)
	}
	if string(body) != "abc" {
		t.Fatalf("body = %q, want abc", body)
	}
}

func TestLimitUpstreamResponseBodyRejectsOverLimit(t *testing.T) {
	t.Setenv("OCTOPUS_RELAY_UPSTREAM_RESPONSE_READ_MAX_BYTES", "3")

	response := &http.Response{Body: io.NopCloser(strings.NewReader("abcd"))}
	limitUpstreamResponseBody(response)

	_, err := io.ReadAll(response.Body)
	if !errors.Is(err, errUpstreamResponseBodyTooLarge) {
		t.Fatalf("error = %v, want errUpstreamResponseBodyTooLarge", err)
	}
}

func TestHandleResponseAppliesUpstreamBodyLimit(t *testing.T) {
	t.Setenv("OCTOPUS_RELAY_UPSTREAM_RESPONSE_READ_MAX_BYTES", "3")

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			c:         c,
			inAdapter: responseLimitInbound{},
		},
	}
	response := &http.Response{Body: io.NopCloser(strings.NewReader("abcd"))}

	err := ra.handleResponse(context.Background(), response, responseLimitOutbound{})
	if !errors.Is(err, errUpstreamResponseBodyTooLarge) {
		t.Fatalf("error = %v, want errUpstreamResponseBodyTooLarge", err)
	}
}
