package relay

import (
	"context"
	"errors"
	"net/http"
	"strings"
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
		{
			name:              "400 with transient capacity evidence is eligible -> true",
			enabled:           true,
			streamPrefers:     &streamTrue,
			wroteBusinessData: false,
			finalErr:          newUpstreamError(http.StatusBadRequest, []byte(`{"error":{"message":"upstream capacity temporarily exceeded, retry later"}}`)),
			wantHold:          true,
		},
		{
			name:              "400 deterministic availability advisory is not eligible -> false",
			enabled:           true,
			streamPrefers:     &streamTrue,
			wroteBusinessData: false,
			finalErr:          newUpstreamError(http.StatusBadRequest, []byte(`{"error":{"message":"model gpt-6-astra availability cannot be guaranteed, please use another model"}}`)),
			wantHold:          false,
		},
		{
			name:              "400 retry routing advice without transient evidence is not eligible -> false",
			enabled:           true,
			streamPrefers:     &streamTrue,
			wroteBusinessData: false,
			finalErr:          newUpstreamError(http.StatusBadRequest, []byte(`{"error":{"message":"please retry with a different model"}}`)),
			wantHold:          false,
		},
		{
			name:              "422 invalid_argument is not eligible -> false",
			enabled:           true,
			streamPrefers:     &streamTrue,
			wroteBusinessData: false,
			finalErr:          newUpstreamError(http.StatusUnprocessableEntity, []byte(`{"error":{"message":"invalid_argument"}}`)),
			wantHold:          false,
		},
		{
			name:              "400 unsupported parameter value is not eligible -> false",
			enabled:           true,
			streamPrefers:     &streamTrue,
			wroteBusinessData: false,
			finalErr:          newUpstreamError(http.StatusBadRequest, []byte(`{"error":{"message":"Unsupported value: 'none' is not supported with the 'gpt-6-astra' model. Supported values are: 'low', 'medium', 'high', 'xhigh', and 'max'."}}`)),
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

// TestMatchTransientCapacityText locks the generic transient-capacity evidence table:
// broad enough for universal capacity/rate-limit/overload phrasing, but with NO bare
// "retry" marker (deterministic advisories say "please retry with a different model")
// and no vendor/model-specific strings.
func TestMatchTransientCapacityText(t *testing.T) {
	positives := []string{
		"upstream capacity temporarily exceeded, retry later",
		"rate limit exceeded, slow down",
		"rate_limit_error",
		"429 too many requests",
		"server overloaded, try again soon",
		"model is overloaded",
		"channel is busy, please wait",
		"service temporarily unavailable",
		"request throttled, back off",
		"temporary failure, try again",
	}
	for _, s := range positives {
		if !matchTransientCapacityText(s) {
			t.Errorf("expected transient-capacity match for %q", s)
		}
	}

	negatives := []string{
		"",
		"please retry with a different model",
		"model gpt-6-astra availability cannot be guaranteed, please use another model",
		"invalid_request_error: failed to deserialize the JSON body",
		"prompt is too long: 201015 tokens > 200000 maximum",
		"Unsupported value: 'none' is not supported with the 'gpt-6-astra' model",
		"resource not found",
		"unauthorized: bad api key",
	}
	for _, s := range negatives {
		if matchTransientCapacityText(strings.ToLower(s)) {
			t.Errorf("did not expect transient-capacity match for %q", s)
		}
	}
}

// TestIsTransientCapacityUpstreamError locks the status gate and deterministic-wins
// tie-breaking of the end-to-end predicate: only 400/422 bodies with generic transient
// evidence qualify; request-invalid / context-window bodies never do; other statuses
// keep their own (already transient or already deterministic) handling.
func TestIsTransientCapacityUpstreamError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "400 capacity evidence -> true",
			err:  newUpstreamError(http.StatusBadRequest, []byte(`{"error":{"message":"upstream capacity temporarily exceeded, retry later"}}`)),
			want: true,
		},
		{
			name: "422 overloaded evidence -> true",
			err:  newUpstreamError(http.StatusUnprocessableEntity, []byte(`{"error":{"message":"model is overloaded, try again soon"}}`)),
			want: true,
		},
		{
			name: "400 without transient evidence -> false",
			err:  newUpstreamError(http.StatusBadRequest, []byte(`{"error":{"message":"please retry with a different model"}}`)),
			want: false,
		},
		{
			name: "400 request-invalid wins over capacity wording -> false",
			err:  newUpstreamError(http.StatusBadRequest, []byte(`{"error":{"message":"invalid_request_error: too many requests in body"}}`)),
			want: false,
		},
		{
			name: "400 context-window wins over capacity wording -> false",
			err:  newUpstreamError(http.StatusBadRequest, []byte(`{"error":{"message":"prompt is too long: 201015 tokens > 200000 maximum"}}`)),
			want: false,
		},
		{
			name: "429 stays outside this predicate -> false",
			err:  newUpstreamError(http.StatusTooManyRequests, []byte(`{"error":{"message":"rate limit exceeded"}}`)),
			want: false,
		},
		{
			name: "503 stays outside this predicate -> false",
			err:  newUpstreamError(http.StatusServiceUnavailable, []byte(`{"error":{"message":"overloaded"}}`)),
			want: false,
		},
		{
			name: "non-upstream error -> false",
			err:  errors.New("capacity temporarily exceeded"),
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientCapacityUpstreamError(tc.err); got != tc.want {
				t.Errorf("isTransientCapacityUpstreamError() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRescueDoesNotContinueOnDeterministic400 guards the re-entry fix in relay.Handler:
// a request that already started machine rescue must NOT keep rescuing when a later
// retry attempt fails with a deterministic 4xx (here a 400 invalid-request payload).
// The production eligibility predicate isRescueableHeldRequest (which gates the
// interventionRegistered&&... re-entry branch) returns false for such an error, so the
// rescue loop cannot spin forever on an error no channel will ever accept.
func TestRescueDoesNotContinueOnDeterministic400(t *testing.T) {
	setupRelayErrorDB(t)
	setInterventionEnabledForTest(t, true)

	streamTrue := true
	req := &relayRequest{
		internalRequest: &transformerModel.InternalLLMRequest{
			Stream: &streamTrue,
		},
		wroteBusinessData: false,
	}

	deterministic400 := newUpstreamError(http.StatusBadRequest, []byte(`{
		"error": {
			"message": "Failed to deserialize the JSON body into ChatCompletionRequest",
			"type": "invalid_request_error"
		}
	}`))

	// The retry produced a deterministic 400: rescue continuation must be refused.
	if isRescueableHeldRequest(req, nil, deterministic400) {
		t.Fatalf("later deterministic 400 must NOT be eligible for continued rescue")
	}
}

// TestRescueContinuationStillAllowedForTransientError proves the re-entry guard does
// not over-correct: a transient (eligible) error after rescue started still passes the
// eligibility predicate, so machine rescue keeps retrying while the budget is alive.
func TestRescueContinuationStillAllowedForTransientError(t *testing.T) {
	setupRelayErrorDB(t)
	setInterventionEnabledForTest(t, true)

	streamTrue := true
	req := &relayRequest{
		internalRequest: &transformerModel.InternalLLMRequest{
			Stream: &streamTrue,
		},
		wroteBusinessData: false,
	}

	transient503 := newUpstreamError(http.StatusServiceUnavailable, []byte(`{"error":{"message":"Overloaded"}}`))
	if !isRescueableHeldRequest(req, nil, transient503) {
		t.Fatalf("later transient 503 must remain eligible for continued rescue")
	}
}

func TestRelayStopAndRescueGateOnClientAbort(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	current, currentCancel := context.WithCancel(context.Background())
	defer currentCancel()
	clientGone, stopErr := relayStop(parent, current, nil, nil)
	if !clientGone || stopErr == nil {
		t.Fatalf("client abort should report clientGone, got gone=%v err=%v", clientGone, stopErr)
	}
}

func TestRelayStopRescueContextDoesNotLookLikeClientAbort(t *testing.T) {
	parent := context.Background()
	rescueCtx, cancel := context.WithCancel(parent)
	cancel()
	clientGone, stopErr := relayStop(parent, rescueCtx, rescueCtx, nil)
	if clientGone {
		t.Fatal("rescue abort must not be reported as clientGone")
	}
	if stopErr == nil {
		t.Fatal("rescue abort should return stopErr")
	}
}
