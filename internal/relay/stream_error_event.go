package relay

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/utils/log"
	"github.com/gin-gonic/gin"
)

type responsesFailedError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type responsesFailedBody struct {
	ID     string               `json:"id"`
	Object string               `json:"object"`
	Model  string               `json:"model,omitempty"`
	Status string               `json:"status"`
	Output []any                `json:"output"`
	Error  responsesFailedError `json:"error"`
}

type responsesFailedEvent struct {
	Type     string              `json:"type"`
	Response responsesFailedBody `json:"response"`
}

type anthropicStreamErrorBody struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeResponsesFailedSSE(c *gin.Context, requestModel string, code string, message string) bool {
	if c == nil || c.Writer == nil {
		return false
	}
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return false
	}
	code = strings.TrimSpace(code)
	if code == "" {
		code = "upstream_error"
	}
	message = strings.TrimSpace(message)
	if message == "" {
		message = "upstream stream failed"
	}

	payload, err := json.Marshal(responsesFailedEvent{
		Type: "response.failed",
		Response: responsesFailedBody{
			ID:     fmt.Sprintf("resp_%d", time.Now().UnixNano()),
			Object: "response",
			Model:  strings.TrimSpace(requestModel),
			Status: "failed",
			Output: []any{},
			Error: responsesFailedError{
				Code:    code,
				Message: message,
			},
		},
	})
	if err != nil {
		_ = c.Error(err)
		return true
	}

	if _, err := fmt.Fprintf(c.Writer, "event: response.failed\ndata: %s\n\ndata: [DONE]\n\n", payload); err != nil {
		_ = c.Error(err)
		return true
	}
	flusher.Flush()
	return true
}

func writeAnthropicErrorSSE(c *gin.Context, errorType string, message string) bool {
	if c == nil || c.Writer == nil {
		return false
	}
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return false
	}
	errorType = strings.TrimSpace(errorType)
	if errorType == "" {
		errorType = "api_error"
	}
	message = strings.TrimSpace(message)
	if message == "" {
		message = "upstream stream failed before terminal event"
	}

	body := anthropicStreamErrorBody{Type: "error"}
	body.Error.Type = errorType
	body.Error.Message = message
	payload, err := json.Marshal(body)
	if err != nil {
		_ = c.Error(err)
		return true
	}

	if _, err := fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", payload); err != nil {
		_ = c.Error(err)
		return true
	}
	flusher.Flush()
	return true
}

type chatStreamErrorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code,omitempty"`
	} `json:"error"`
}

// writeChatErrorSSE emits an OpenAI chat-completions-style error onto an already
// committed SSE stream (used when comment heartbeats committed HTTP 200 during
// failover and every channel then failed). It closes with the [DONE] sentinel.
func writeChatErrorSSE(c *gin.Context, code string, message string) bool {
	if c == nil || c.Writer == nil {
		return false
	}
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return false
	}
	message = strings.TrimSpace(message)
	if message == "" {
		message = "upstream stream failed"
	}
	body := chatStreamErrorBody{}
	body.Error.Message = message
	body.Error.Type = "upstream_error"
	body.Error.Code = strings.TrimSpace(code)
	payload, err := json.Marshal(body)
	if err != nil {
		_ = c.Error(err)
		return true
	}
	if _, err := fmt.Fprintf(c.Writer, "data: %s\n\ndata: [DONE]\n\n", payload); err != nil {
		_ = c.Error(err)
		return true
	}
	flusher.Flush()
	return true
}

func isResponsesInboundPath(path string) bool {
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	if path == "" {
		return false
	}
	return strings.HasSuffix(path, "/responses") || strings.Contains(path, "/responses/")
}

func responsesStreamFailureMessage(err error) string {
	if err == nil {
		return "upstream stream failed"
	}
	if isClientAbortError(err) {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	if msg == "" {
		return "upstream stream failed"
	}
	if strings.Contains(strings.ToLower(msg), "client disconnected") {
		return ""
	}
	return "upstream stream failed"
}

func anthropicStreamFailureMessage(err error) string {
	if err == nil {
		return "upstream stream failed before terminal event"
	}
	if isClientAbortError(err) {
		return ""
	}
	return "upstream stream failed before terminal event"
}

// chatStreamFailureMessage mirrors anthropicStreamFailureMessage for the OpenAI
// chat inbound: a client abort (or a nil error) yields "" so no frame is written
// for a client that already hung up; every other failure yields the display text.
func chatStreamFailureMessage(err error) string {
	if err == nil {
		return "upstream stream failed"
	}
	if isClientAbortError(err) {
		return ""
	}
	return "upstream stream failed"
}

// Fixed, secret-free identifiers for the terminal frame written when the
// credential-redaction restorer could not be flushed safely at a committed stream
// failure. The underlying restorer error is deliberately NOT surfaced (it can echo
// restored/unrestored token data); the message also does not claim the content is
// complete.
const (
	streamRestoreErrorCode    = "stream_restore_error"
	streamRestoreErrorMessage = "response stream could not be restored after redaction; the stream is truncated"
)

// writeCommittedStreamFailure surfaces an upstream stream failure on an
// ALREADY-committed stream (meaningful content was flushed, so the channel can no
// longer be swapped): it writes the in-band error envelope in the client's inbound
// protocol exactly once. Shared by the two committed-failure sites (relayAttempt
// .attempt and runChannelRace's winner), which used to duplicate this switch — and
// both of which silently dropped the failure for OpenAI chat inbound, leaving the
// client a truncated stream with no error frame and no [DONE].
//
// Before the terminal error frame it flushes any *pending* restored client-format
// SSE tail the credential-redaction restorer was still buffering (real model output
// coalesced with a placeholder): without this, a committed failure silently dropped
// that buffered content. Only the not-yet-emitted tail is written, so no already-sent
// content or [DONE] is duplicated. A client abort emits NOTHING new (the client is
// already gone). When the restorer cannot produce a safe tail (broken, or a
// Finish/flush failure) its buffer may hold raw/unrestored token data, so it is
// dropped and the terminal frame carries the fixed streamRestoreError* identifiers
// instead of the upstream message.
//
// Only the three covered inbound protocols get a frame; Gemini / embedding inbound
// (and anything else) stay untouched, matching the previous Responses/Anthropic-only
// behaviour.
func writeCommittedStreamFailure(ra *relayAttempt, fwdErr error) {
	if ra == nil || ra.c == nil {
		return
	}
	// Client abort: the client hung up, so adding no bytes at all is correct — no
	// tail flush and no error frame.
	if isClientAbortError(fwdErr) {
		return
	}
	restoreFailed := flushCommittedRestoreTail(ra)
	switch ra.inboundType {
	case inbound.InboundTypeOpenAIResponse:
		code, message := "upstream_error", responsesStreamFailureMessage(fwdErr)
		if restoreFailed {
			code, message = streamRestoreErrorCode, streamRestoreErrorMessage
		}
		if message != "" {
			writeResponsesFailedSSE(ra.c, ra.requestModel, code, message)
		}
	case inbound.InboundTypeAnthropic:
		if restoreFailed {
			writeAnthropicErrorSSE(ra.c, "api_error", streamRestoreErrorMessage)
			return
		}
		if message := anthropicStreamFailureMessage(fwdErr); message != "" {
			writeAnthropicErrorSSE(ra.c, "api_error", message)
		}
	case inbound.InboundTypeOpenAIChat:
		code, message := "upstream_error", chatStreamFailureMessage(fwdErr)
		if restoreFailed {
			code, message = streamRestoreErrorCode, streamRestoreErrorMessage
		}
		if message != "" {
			writeChatErrorSSE(ra.c, code, message)
		}
	}
}

// flushCommittedRestoreTail writes the redaction restorer's still-pending tail to
// the client once, ahead of the terminal error frame, and reports whether the
// restorer could NOT produce a safe tail. It returns true (no bytes written) when
// the restorer already broke, or when Finish/flush fails: its buffer may hold raw
// placeholders / unrestored tokens, so nothing may be emitted and the caller must
// fail with the fixed restore-error frame. It returns false for the normal cases
// (nothing redacted, empty tail, or a safely flushed tail). redactFlushClientSse
// returns only the bytes the restorer has NOT emitted yet (and marks the restorer
// broken on failure), so this can never re-emit already-written content or a
// duplicate [DONE].
func flushCommittedRestoreTail(ra *relayAttempt) bool {
	if ra == nil || ra.c == nil || ra.redactSession == nil || !ra.redactApplied {
		return false
	}
	if ra.redactStreamBroken {
		return true
	}
	tail, err := ra.redactFlushClientSse()
	if err != nil {
		log.Errorf("redact: flush restored stream tail before terminal error failed: %v", err)
		return true
	}
	if len(tail) == 0 {
		return false
	}
	if _, werr := ra.c.Writer.Write(tail); werr != nil {
		log.Warnf("redact: write restored stream tail before terminal error: %v", werr)
		return false
	}
	ra.c.Writer.Flush()
	return false
}

// writeRelayErrorPreStream writes a pre-stream error in the inbound-aware envelope.
//
// On the /v1/responses path, octopus's internal ResponseStruct {code,error_code,message}
// is NOT what cursor's responses parser expects — it sees an unknown shape and surfaces
// "OpenAI Responses API failed: unknown error" to the user. Mirror new-api's types.NewAPIError
// contract: ALWAYS emit the OpenAI shape {"error":{"message":..,"type":..,"code":..}} to
// OpenAI-protocol inbound. Chat / Anthropic inbound keep their existing octopus-internal
// / Anthropic error shapes; only the responses inbound changes here.
//
// This is the pre-stream (no SSE prelude committed) branch counterpart of the post-stream
// switch in relay.go (writeResponsesFailedSSE / writeChatErrorSSE / writeAnthropicErrorSSE):
// here c.Writer.Written() is false and we deliver the error as a normal JSON HTTP response.
// Once any meaningful bytes are flushed (heartbeats or prelude), the post-stream switch
// takes over and writes the error in-band on the SSE stream.
func writeRelayErrorPreStream(c *gin.Context, inboundType inbound.InboundType, httpStatus int, errType string, errCode string, message string) {
	switch inboundType {
	case inbound.InboundTypeOpenAIResponse:
		resp.OpenAIError(c, httpStatus, errType, errCode, message)
	default:
		if errCode == "" {
			resp.Error(c, httpStatus, message)
		} else {
			resp.ErrorWithCode(c, httpStatus, errCode, message)
		}
	}
}

// writeUpstreamRetryAfterHint forwards the provider's Retry-After hint onto the
// downstream error response when the status we are about to send is itself a
// "come back later" status (429 / 503).
//
// A client routed through the relay must pace itself exactly as it would against the
// provider directly. Dropping the hint leaves claude-code / codex with nothing to wait
// on, so they retry immediately and a brief upstream rate-limit degenerates into a
// self-inflicted hammering loop — the relay looking far worse than the raw provider.
//
// The value is a pure timing hint: it carries no provider identity, so it travels safely
// alongside the redacted body and honours the admin passthrough/body policies unchanged.
// It is only written before the response head is committed; once anything has been
// flushed the failure travels in-band and headers are no longer settable.
func writeUpstreamRetryAfterHint(c *gin.Context, httpStatus int, err error) {
	if c == nil || err == nil || c.Writer.Written() {
		return
	}
	if httpStatus != http.StatusTooManyRequests && httpStatus != http.StatusServiceUnavailable {
		return
	}
	hint := upstreamRetryAfter(err)
	if hint <= 0 {
		return
	}
	// Retry-After is defined in whole seconds; round up so we never advertise a shorter
	// wait than the provider asked for.
	seconds := int64(hint / time.Second)
	if hint%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		seconds = 1
	}
	c.Header("Retry-After", strconv.FormatInt(seconds, 10))
}
