package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
)

const upstreamErrorBodyLimit = 16 * 1024
const statusClientClosedRequest = 499

type upstreamError struct {
	statusCode    int
	code          string
	strategy      string
	message       string
	body          string
	retryAfter    time.Duration
	hasRetryAfter bool
}

func newUpstreamError(statusCode int, body []byte) *upstreamError {
	providerCode := normalizeErrorCode(extractUpstreamErrorCode(body))
	code := upstreamErrorCode(statusCode)
	strategy := "preserve_status_sanitize_body"
	if providerCode != "" {
		strategy = "preserve_status_sanitize_body_provider_code_observed"
	}
	message := fmt.Sprintf("Upstream request failed (status %d, code %s).", statusCode, code)
	return &upstreamError{
		statusCode: statusCode,
		code:       code,
		strategy:   strategy,
		message:    message,
		body:       string(body),
	}
}

func (e *upstreamError) Error() string {
	return e.message
}

func (e *upstreamError) StatusCode() int {
	return e.statusCode
}

func (e *upstreamError) ErrorCode() string {
	return e.code
}

func (e *upstreamError) Strategy() string {
	return e.strategy
}

func (e *upstreamError) Body() string {
	return e.body
}

func upstreamErrorDetails(err error) (status int, code string, strategy string, ok bool) {
	var upErr *upstreamError
	if !errors.As(err, &upErr) {
		return 0, "", "", false
	}
	return upErr.StatusCode(), upErr.ErrorCode(), configuredUpstreamErrorStrategy(upErr.Strategy()), true
}

type localRelayError struct {
	status   int
	code     string
	strategy string
	message  string
}

func (e *localRelayError) Error() string {
	if e == nil {
		return ""
	}
	return e.message
}

func localRelayErrorDetails(err error) (status int, code string, strategy string, ok bool) {
	var localErr *localRelayError
	if !errors.As(err, &localErr) || localErr == nil {
		return 0, "", "", false
	}
	return localErr.status, localErr.code, localErr.strategy, true
}

func clientAbortErrorDetails(err error) (status int, code string, strategy string, ok bool) {
	if err == nil {
		return 0, "", "", false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, "octopus_client_timeout", "client_timeout;upstream_forwarded=true;breaker_counted=false", true
	}
	if errors.Is(err, context.Canceled) {
		return statusClientClosedRequest, "octopus_client_canceled", "client_canceled;upstream_forwarded=true;breaker_counted=false", true
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "client disconnected") || strings.Contains(msg, "context canceled") {
		return statusClientClosedRequest, "octopus_client_canceled", "client_canceled;upstream_forwarded=true;breaker_counted=false", true
	}
	if strings.Contains(msg, "context deadline exceeded") {
		return http.StatusGatewayTimeout, "octopus_client_timeout", "client_timeout;upstream_forwarded=true;breaker_counted=false", true
	}
	return 0, "", "", false
}

func relayErrorDetails(err error) (status int, code string, strategy string, ok bool) {
	if status, code, strategy, ok := upstreamErrorDetails(err); ok {
		return status, code, strategy, true
	}
	if status, code, strategy, ok := localRelayErrorDetails(err); ok {
		return status, code, strategy, true
	}
	if status, code, strategy, ok := clientAbortErrorDetails(err); ok {
		return status, code, strategy, true
	}
	return 0, "", "", false
}

func attemptStatusCode(statusCode int, err error) int {
	if statusCode != 0 {
		return statusCode
	}
	if status, _, _, ok := relayErrorDetails(err); ok {
		return status
	}
	return statusCode
}

func relayErrorResponse(err error) (status int, code string, message string) {
	if status, code, _, ok := upstreamErrorDetails(err); ok && status >= 400 && status < 600 {
		if !upstreamErrorStatusPassthrough() {
			status = http.StatusBadGateway
		}
		return status, upstreamErrorPublicCode(code), upstreamErrorUserMessage(err)
	}
	if status, code, _, ok := localRelayErrorDetails(err); ok && status >= 400 && status < 600 {
		return status, code, errSafeMessage(err)
	}
	if status, code, _, ok := clientAbortErrorDetails(err); ok && status >= 400 && status < 600 {
		if code == "octopus_client_timeout" {
			return status, code, "request timed out before upstream response"
		}
		return status, code, "request canceled by client"
	}
	return http.StatusBadGateway, "octopus_all_channels_failed", "all channels failed"
}

func routeSelectionErrorFromAttempts(attempts []dbmodel.ChannelAttempt) error {
	if len(attempts) == 0 {
		return &localRelayError{
			status:   http.StatusServiceUnavailable,
			code:     "octopus_no_available_channel",
			strategy: "local_route_selection;reason=no_candidates;upstream_forwarded=false",
			message:  "no available channel",
		}
	}
	last := attempts[len(attempts)-1]
	for i := len(attempts) - 1; i >= 0; i-- {
		if attempts[i].ChannelID != 0 || attempts[i].ChannelName != "" {
			last = attempts[i]
			break
		}
	}
	reason := "no_available_channel"
	code := "octopus_no_available_channel"
	if last.Status == dbmodel.AttemptCircuitBreak {
		reason = "circuit_break"
		code = "octopus_channel_circuit_open"
	} else if last.Status == dbmodel.AttemptSkipped {
		reason = "skipped"
	}
	detail := strings.TrimSpace(last.Msg)
	if detail == "" {
		detail = reason
	}
	channel := strings.TrimSpace(last.ChannelName)
	if channel == "" {
		channel = fmt.Sprintf("#%d", last.ChannelID)
	}
	return &localRelayError{
		status:   http.StatusServiceUnavailable,
		code:     code,
		strategy: fmt.Sprintf("local_route_selection;reason=%s;upstream_forwarded=false", reason),
		message:  fmt.Sprintf("no available channel: %s (%s)", channel, detail),
	}
}

func upstreamErrorUserMessage(err error) string {
	switch upstreamErrorBodyMode() {
	case "custom_message":
		if message := upstreamErrorCustomMessage(); message != "" {
			return message
		}
		return "upstream request failed"
	case "octopus_standard":
		return "upstream request failed"
	default:
		if !upstreamErrorStatusPassthrough() {
			return "upstream request failed"
		}
		return errSafeMessage(err)
	}
}

func configuredUpstreamErrorStrategy(base string) string {
	return fmt.Sprintf("%s;status_passthrough=%t;body_mode=%s;public_code=%s", base, upstreamErrorStatusPassthrough(), upstreamErrorBodyMode(), upstreamErrorPublicCode(""))
}

func upstreamErrorStatusPassthrough() bool {
	value, err := op.SettingGetString(dbmodel.SettingKeyUpstreamErrorStatusPass)
	if err != nil {
		return false
	}
	enabled, err := strconv.ParseBool(value)
	if err != nil {
		return false
	}
	return enabled
}

func upstreamErrorBodyMode() string {
	value, err := op.SettingGetString(dbmodel.SettingKeyUpstreamErrorBodyMode)
	if err != nil {
		return "redacted_upstream"
	}
	switch strings.TrimSpace(value) {
	case "custom_message", "octopus_standard":
		return strings.TrimSpace(value)
	default:
		return "redacted_upstream"
	}
}

func upstreamErrorCustomMessage() string {
	value, err := op.SettingGetString(dbmodel.SettingKeyUpstreamErrorCustom)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func upstreamErrorPublicCode(fallback string) string {
	value, err := op.SettingGetString(dbmodel.SettingKeyUpstreamErrorPublicCode)
	if err != nil {
		return fallback
	}
	code := normalizeErrorCode(value)
	if code == "" {
		return fallback
	}
	return code
}

func errSafeMessage(err error) string {
	var upErr *upstreamError
	if errors.As(err, &upErr) {
		return upErr.Error()
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

func isClientAbortError(err error) bool {
	_, _, _, ok := clientAbortErrorDetails(err)
	return ok
}

func extractUpstreamErrorCode(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return ""
	}
	if m, ok := root.(map[string]any); ok {
		if code := stringValue(m["code"]); code != "" {
			return code
		}
		if errObj, ok := m["error"].(map[string]any); ok {
			if code := stringValue(errObj["code"]); code != "" {
				return code
			}
			if code := stringValue(errObj["type"]); code != "" {
				return code
			}
		}
	}
	return ""
}

func stringValue(value any) string {
	if s, ok := value.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func normalizeErrorCode(code string) string {
	code = strings.TrimSpace(code)
	if code == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range code {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.' {
			b.WriteRune(unicode.ToLower(r))
		}
		if b.Len() >= 80 {
			break
		}
	}
	return strings.Trim(b.String(), "._-")
}

func upstreamErrorCode(statusCode int) string {
	switch {
	case statusCode == http.StatusTooManyRequests:
		return "octopus_upstream_rate_limited"
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		return "octopus_upstream_auth_failed"
	case statusCode == http.StatusBadRequest:
		return "octopus_upstream_bad_request"
	case statusCode >= 500:
		return "octopus_upstream_unavailable"
	default:
		return "octopus_upstream_non_2xx"
	}
}

// parseRetryAfter parses an HTTP Retry-After header value, which is either
// delta-seconds (e.g. "30") or an HTTP-date. A negative/zero/invalid value
// reports ok=false so the caller falls back to the default cooldown.
func parseRetryAfter(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(value); err == nil {
		if secs <= 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(value); err == nil {
		if d := time.Until(t); d > 0 {
			return d, true
		}
		return 0, false
	}
	return 0, false
}

// retryAfterFromHeader extracts an upstream-provided cooldown from a response's
// Retry-After header, used to back off a rate-limited channel/key precisely
// instead of guessing a fixed duration.
func retryAfterFromHeader(h http.Header) (time.Duration, bool) {
	if h == nil {
		return 0, false
	}
	return parseRetryAfter(h.Get("Retry-After"))
}

// retryAfterFromError returns the upstream Retry-After cooldown carried by an
// upstreamError, if the upstream provided one.
func retryAfterFromError(err error) (time.Duration, bool) {
	var upErr *upstreamError
	if errors.As(err, &upErr) && upErr != nil && upErr.hasRetryAfter {
		return upErr.retryAfter, true
	}
	return 0, false
}
