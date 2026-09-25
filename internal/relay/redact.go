package relay

// Credential redaction wiring: outbound request bodies get secrets replaced with
// reversible {{Redact:...}} placeholders before octopus sends them, and response
// text (non-stream JSON bodies and stream SSE events, including tool-argument
// deltas split across events) restores placeholders back before the client sees
// them. Detection/restoration logic is vendored from CosyRedactGateway
// (internal/redact). Fingerprint safety: this layer only rewrites application
// body text and downstream response text — the outbound TLS/header fingerprint
// path is never touched, so claude/codex CLI shapes and normal-model UAs are
// preserved byte-for-byte on every channel type.

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/redact"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/bestruirui/octopus/internal/utils/log"
)

var (
	redactEngineMu sync.Mutex
	redactEngine   *redact.Engine
)

// sharedRedactEngine builds the JS engine pool once per process. A failed build
// is NOT cached (审计修复): a transient first-use failure would otherwise
// silently disable redaction process-wide until restart. The next request
// retries the build.
func sharedRedactEngine() (*redact.Engine, error) {
	redactEngineMu.Lock()
	defer redactEngineMu.Unlock()
	if redactEngine == nil {
		e, err := redact.NewEngine()
		if err != nil {
			return nil, err
		}
		redactEngine = e
	}
	return redactEngine, nil
}

func redactGlobalEnabled() bool {
	if v, err := op.SettingGetBool(dbmodel.SettingKeyRedactEnabled); err == nil {
		return v
	}
	return false
}

func redactNoticeEnabled() bool {
	if v, err := op.SettingGetBool(dbmodel.SettingKeyRedactNoticeEnabled); err == nil {
		return v
	}
	return true
}

func redactDefaultFlags() string {
	if v, err := op.SettingGetString(dbmodel.SettingKeyRedactDefaultFlags); err == nil && strings.TrimSpace(v) != "" {
		return dbmodel.NormalizeRedactFlags(v)
	}
	return ""
}

// redactProtocolFor maps the channel's outbound type to the Cosy protocol family
// so the vendored core applies protocol-aware skipping (reasoning model state,
// control keys, notice placement) with the right shape instead of guessing from
// the body. Empty string lets the core detect from the body (generic fallback).
func redactProtocolFor(t outbound.OutboundType) string {
	switch t {
	case outbound.OutboundTypeOpenAIChat, outbound.OutboundTypeCustomOpenAIChat:
		return "openai_chat"
	case outbound.OutboundTypeOpenAIResponse:
		return "openai_responses"
	case outbound.OutboundTypeAnthropic:
		return "anthropic_messages"
	default:
		return "" // gemini/volcengine/embedding: generic walk
	}
}

// newRedactSessionForChannel creates a standalone redaction session for send
// paths that have no relayRequest (videos handler). Same gates as the relay
// path: global master switch + channel opt-in. Returns nil when redaction is
// inactive; the caller must Close a non-nil session when done.
func newRedactSessionForChannel(ch *dbmodel.Channel, protocol string) *redact.Session {
	if !redactGlobalEnabled() || ch == nil || !ch.RedactEnabled {
		return nil
	}
	engine, err := sharedRedactEngine()
	if err != nil {
		log.Errorf("redact: engine unavailable, redaction disabled: %v", err)
		return nil
	}
	flags := strings.TrimSpace(ch.RedactFlags)
	if flags == "" {
		flags = redactDefaultFlags()
	}
	s, err := engine.NewSession(flags, protocol, redactNoticeEnabled())
	if err != nil {
		log.Errorf("redact: session create failed, redaction disabled: %v", err)
		return nil
	}
	return s
}

// redactSessionFor returns this request's redaction session, creating it lazily
// on the first attempt whose channel has redaction enabled. The session (and its
// placeholder mapping) lives on relayRequest, so channel retries reuse the same
// placeholders — the model sees a consistent mapping across failover, and the
// final response restores against the same map. Returns nil when redaction is
// globally off, the channel opted out, or the engine failed to start (redaction
// silently disabled with an error log — business traffic is never blocked by a
// broken engine at session-creation time; per-request failures fail closed).
func (rr *relayRequest) redactSessionFor(ch *dbmodel.Channel, protocol string) *redact.Session {
	if !redactGlobalEnabled() || ch == nil || !ch.RedactEnabled {
		return nil
	}
	if rr.redactSession != nil {
		return rr.redactSession
	}
	engine, err := sharedRedactEngine()
	if err != nil {
		log.Errorf("redact: engine unavailable, redaction disabled: %v", err)
		return nil
	}
	flags := strings.TrimSpace(ch.RedactFlags)
	if flags == "" {
		flags = redactDefaultFlags()
	}
	s, err := engine.NewSession(flags, protocol, redactNoticeEnabled())
	if err != nil {
		log.Errorf("redact: session create failed, redaction disabled: %v", err)
		return nil
	}
	rr.redactSession = s
	return s
}

// redactClose releases the session's engine VM when the request finishes.
func (rr *relayRequest) redactClose() {
	if rr.redactSession != nil {
		rr.redactSession.Close()
		rr.redactSession = nil
	}
}

// applyOutboundRedaction rewrites the outbound request body in place: secrets
// become placeholders. Fail-closed: when redaction is active for this channel
// but the scan fails, the request is rejected rather than leaking the secret.
// The body rewrite happens after header copying and before sendRequest, so the
// fingerprint stack sends the redacted bytes exactly as it would any body —
// TLS, header order, and UA are untouched.
func (ra *relayAttempt) applyOutboundRedaction(req *http.Request, protocol string) error {
	s := ra.redactSessionFor(ra.channel, protocol)
	if s == nil || ra.redactFailed {
		return nil
	}
	if req == nil || req.Body == nil {
		return nil
	}
	body, err := io.ReadAll(req.Body)
	req.Body.Close()
	if err != nil {
		return fmt.Errorf("redact: read outbound body: %w", err)
	}
	redacted, err := s.RedactJSONBody(body)
	if err != nil {
		ra.redactFailed = true
		return fmt.Errorf("redact: outbound scan failed (request rejected to avoid leaking secrets): %w", err)
	}
	if len(redacted) != len(body) || !bytes.Equal(redacted, body) {
		ra.redactApplied = true
	}
	req.Body = io.NopCloser(bytes.NewReader(redacted))
	req.ContentLength = int64(len(redacted))
	req.Header.Set("Content-Length", strconv.Itoa(len(redacted)))
	// GetBody must serve the REDACTED bytes too: Go's transport uses it to re-send
	// the body on 307/308 redirects — a stale GetBody (e.g. set by ApplyParamOverride)
	// would leak the original unredacted secret to the redirect target.
	redactedCopy := redacted
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(redactedCopy)), nil
	}
	return nil
}

// redactRestoreResponseBody restores placeholders in a complete (non-streaming)
// upstream response body before the outbound transformer parses it. Restore
// failures degrade to passing the body through (the client may see placeholders,
// which is safe — the secret is not leaked — and the error is logged). Bodies
// larger than the restore cap are passed through un-restored the same way:
// the bytes already consumed are chained with the unread remainder so the
// stream is never truncated or corrupted.
func (ra *relayAttempt) redactRestoreResponseBody(response *http.Response) {
	s := ra.redactSession
	if s == nil || !ra.redactApplied || response == nil || response.Body == nil {
		return
	}
	original := response.Body
	body, err := io.ReadAll(io.LimitReader(original, redactMaxBodyBytes+1))
	if err != nil {
		log.Warnf("redact: read response body failed, passing through: %v", err)
		response.Body = chainedBody(bytes.NewReader(body), original)
		return
	}
	if int64(len(body)) > redactMaxBodyBytes {
		log.Warnf("redact: response body exceeds restore cap (%d bytes), passing through un-restored", redactMaxBodyBytes)
		response.Body = chainedBody(bytes.NewReader(body), original)
		return
	}
	restored, err := s.RestoreJSONBody(body)
	if err != nil {
		log.Warnf("redact: response restore failed, placeholders may leak to client: %v", err)
		restored = body
	}
	original.Close()
	response.Body = io.NopCloser(bytes.NewReader(restored))
	response.ContentLength = int64(len(restored))
	response.Header.Set("Content-Length", strconv.Itoa(len(restored)))
}

// chainedBody serves the already-buffered head followed by the unread rest of
// the original stream; closing closes the original.
func chainedBody(head io.Reader, rest io.ReadCloser) io.ReadCloser {
	return struct {
		io.Reader
		io.Closer
	}{io.MultiReader(head, rest), rest}
}

// redactRestoreSseEvent feeds one raw upstream SSE event (the data payload text,
// without the trailing blank line) through the stream restorer and returns zero
// or more restored event payloads joined by "\n\n". Empty string means the event
// is buffered inside the restorer (a placeholder is split across events); the
// caller skips transform for it and will receive it once the closing half
// arrives. Terminal/done classification must be done on the ORIGINAL event
// before calling this — buffered events surface later.
func (ra *relayAttempt) redactRestoreSseEvent(raw string) string {
	s := ra.redactSession
	if s == nil || !ra.redactApplied || ra.redactStreamBroken {
		return raw
	}
	if ra.redactSse == nil {
		r, err := s.NewSseRestorer()
		if err != nil {
			log.Warnf("redact: stream restorer unavailable, passing events through: %v", err)
			ra.redactStreamBroken = true
			return raw
		}
		ra.redactSse = r
	}
	out, err := ra.redactSse.Ingest(raw)
	if err != nil {
		log.Warnf("redact: stream restore failed, passing event through: %v", err)
		ra.redactStreamBroken = true
		return raw
	}
	return out
}

// redactFinishSse flushes the restorer at end-of-stream and returns any final
// buffered event payloads (same join format as redactRestoreSseEvent).
func (ra *relayAttempt) redactFinishSse() string {
	if ra.redactSse == nil {
		return ""
	}
	out, err := ra.redactSse.Finish()
	if err != nil {
		log.Warnf("redact: stream finish failed: %v", err)
	}
	return out
}

// redactForwardSseEvents feeds one raw upstream SSE event through the stream
// restorer and returns the event data payloads to forward downstream (0, 1, or N).
// Zero events means the restorer is buffering a placeholder split across
// events. Terminal/done/completed events force a restorer flush first so the
// stream's terminal semantics are never swallowed by the buffer; the natural
// end-of-stream flush is the reader loop's tail call to redactFinishSse.
// Pass-through (no redaction this request) returns the original event as-is.
//
// Wire format note: the relay's SSE reader hands us the bare data payload (no
// "data:" line), while the vendored SseRestorer parses full event text — it
// splits on data:/event: lines itself. We wrap the payload into event text on
// the way in and unwrap the restored events back to bare data payloads on the
// way out, keeping the reader contract unchanged.
func (ra *relayAttempt) redactForwardSseEvents(sseEventType, raw string) []restoredSseEvent {
	s := ra.redactSession
	if s == nil || !ra.redactApplied || ra.redactStreamBroken {
		return []restoredSseEvent{{eventType: sseEventType, data: raw}}
	}
	var eventText strings.Builder
	if sseEventType != "" {
		eventText.WriteString("event: " + sseEventType + "\n")
	}
	// 多行 data 按 SSE 规范拆成多条 data: 行 (Cosy 解析器逐行收、还原端 join 回
	// "\n") — 单行写法会把续行变成无前缀裸行, 解析时被丢弃 = 内容丢失。
	for _, line := range strings.Split(raw, "\n") {
		eventText.WriteString("data: " + line + "\n")
	}
	restored := ra.redactRestoreSseEvent(eventText.String())
	ec := classifyStreamEvent(raw)
	if ec.isDone || ec.isTerminal(sseEventType) || ec.isCompleted(sseEventType) {
		restored += ra.redactFinishSse()
	}
	if restored == "" {
		return nil
	}
	return splitRestoredEventData(restored, sseEventType)
}

// restoredSseEvent is one restored SSE event split back into the relay reader's
// wire format: the event name (from the event's own event: line — a buffered
// event keeps the type it was ingested with, NOT the type of the event that
// flushed it) and the bare data payload.
type restoredSseEvent struct {
	eventType string
	data      string
}

// splitRestoredEventData splits the restorer's joined event-text output back
// into the relay reader's wire format. Each event's own event: line wins; the
// fallback (events without one, e.g. OpenAI chat chunks that never carry an
// event name) is the triggering event's type. Non-data lines are dropped; an
// event's multiple data lines join with "\n" per SSE semantics.
func splitRestoredEventData(joined string, fallbackType string) []restoredSseEvent {
	parts := splitRestoredEvents(joined)
	out := make([]restoredSseEvent, 0, len(parts))
	for _, p := range parts {
		evType := ""
		var dataLines []string
		for _, line := range strings.Split(p, "\n") {
			if strings.HasPrefix(line, "event:") {
				evType = strings.TrimSpace(line[len("event:"):])
			} else if strings.HasPrefix(line, "data:") {
				payload := line[len("data:"):]
				payload = strings.TrimPrefix(payload, " ")
				dataLines = append(dataLines, payload)
			}
		}
		if len(dataLines) > 0 {
			if evType == "" {
				evType = fallbackType
			}
			out = append(out, restoredSseEvent{eventType: evType, data: strings.Join(dataLines, "\n")})
		}
	}
	return out
}

// splitRestoredEvents splits joined restored payloads back into individual
// event texts. Empty segments are dropped.
func splitRestoredEvents(joined string) []string {
	if joined == "" {
		return nil
	}
	parts := strings.Split(joined, "\n\n")
	out := parts[:0]
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

const redactMaxBodyBytes = 32 * 1024 * 1024
