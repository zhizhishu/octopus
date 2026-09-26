package relay

// Credential redaction wiring: the CLIENT-protocol request is scanned once per
// attempt with that attempt channel's rules, and the payload fields oct is about
// to send (Messages text, ResponsesInstructions, ResponsesInputRaw) are redacted
// IN PLACE on copies, so octopus's existing outbound transform/shape/send path
// carries the placeholders upstream without any field being re-parsed or
// wholesale replaced — the history-bridge rebuild, suppressCodexHoistedContext
// and synthesizeCodexResponsesInputRaw decisions therefore stand byte-for-byte
// apart from the placeholder text itself. Response text is restored on the
// CLIENT-format side — after the inbound transformer produced the downstream
// JSON/SSE — before the client sees it (non-stream JSON bodies and stream SSE
// events, including tool-argument deltas split across events). Detection and
// restoration logic is vendored from CosyRedactGateway (internal/redact).
// Fingerprint safety: this layer only rewrites application body text and
// downstream response text — the outbound TLS/header fingerprint path is never
// touched, so claude/codex CLI shapes and normal-model UAs are preserved
// byte-for-byte on every channel type.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/redact"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/model"
)

var (
	redactEngineMu sync.Mutex
	redactEngine   *redact.Engine

	// redactEngineTestErr lets tests force engine construction to fail so the
	// fail-closed path can be exercised without a shell or a broken real engine.
	// Production always leaves it nil.
	redactEngineTestErr error
)

// sharedRedactEngine builds the JS engine pool once per process. A failed build
// is NOT cached (审计修复): a transient first-use failure would otherwise
// silently disable redaction process-wide until restart. The next request
// retries the build.
func sharedRedactEngine() (*redact.Engine, error) {
	redactEngineMu.Lock()
	defer redactEngineMu.Unlock()
	if redactEngineTestErr != nil {
		return nil, redactEngineTestErr
	}
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

// inboundRedactProtocol maps the CLIENT (inbound) type to the Cosy protocol family
// so the vendored core applies protocol-aware skipping with the right shape. The
// scan that reaches upstream runs on the client-format body, so this is keyed by
// ra.inboundType, not by the outbound channel type.
func inboundRedactProtocol(t inbound.InboundType) string {
	switch t {
	case inbound.InboundTypeOpenAIChat:
		return "openai_chat"
	case inbound.InboundTypeOpenAIResponse:
		return "openai_responses"
	case inbound.InboundTypeAnthropic:
		return "anthropic_messages"
	default:
		return "" // not reached: applyInboundRedaction short-circuits uncovered types
	}
}

// inboundRedactActive reports whether this attempt should redact the inbound
// client text: global master switch ON, the request client-format is one of the
// covered protocols, the attempt channel opted in (or a prior attempt already
// demanded protection — redactRequired), and the request is not an embedding or a
// media-generation bridge (image/video paths stay untouched this round).
func (ra *relayAttempt) inboundRedactActive() bool {
	if !redactGlobalEnabled() || ra.channel == nil {
		return false
	}
	if ra.channel.RedactEnabled || ra.redactRequired {
		return ra.inboundRedactCovered() && !ra.inboundRedactExcludedRequest()
	}
	return false
}

func (ra *relayAttempt) inboundRedactCovered() bool {
	if ra == nil {
		return false
	}
	switch ra.inboundType {
	case inbound.InboundTypeOpenAIChat, inbound.InboundTypeOpenAIResponse, inbound.InboundTypeAnthropic:
		return true
	default:
		return false
	}
}

func (ra *relayAttempt) inboundRedactExcludedRequest() bool {
	if ra == nil || ra.internalRequest == nil {
		return true
	}
	// Media-generation paths and embedding requests are out of the supported scope;
	// never redact them this round even when a channel opted in.
	return ra.internalRequest.IsImageGenerationRequest() ||
		ra.internalRequest.EmbeddingInput != nil ||
		isVideoRequest(ra.relayRequest)
}

// applyInboundRedaction redacts THIS attempt's client-protocol request payload
// with the current channel's flags. The client bytes (a read-only scan of
// internalRequest.RawRequest) seed the session's token map and count signal; the
// fields oct is actually about to send are then redacted IN PLACE on copies —
// Messages (per-field text pass), ResponsesInstructions and ResponsesInputRaw —
// and written back. No field is ever re-parsed or wholesale replaced:
// applyTransformOptions runs BEFORE this point (history bridge,
// suppressCodexHoistedContext, synthesizeCodexResponsesInputRaw), and the
// per-field pass redacts the CURRENT field values, so oct's shape decisions stand
// byte-for-byte apart from the placeholder text. It runs inside the
// retryWithAdapter block, after the originalXxx captures so the originals stay
// available for restore-log/next-attempt, and before prepareResponsesSessionCursor.
// Fail-closed: any engine/session/scan/field failure rejects the attempt rather
// than sending the original text unprotected. Returns nil when redaction is
// inactive for this attempt (pure no-op).
func (ra *relayAttempt) applyInboundRedaction() error {
	if !ra.inboundRedactActive() || ra.redactFailed {
		return nil
	}
	if ra.internalRequest == nil || len(ra.internalRequest.RawRequest) == 0 {
		return nil
	}
	s := ra.redactSession
	if s == nil {
		engine, err := sharedRedactEngine()
		if err != nil {
			ra.redactFailed = true
			return fmt.Errorf("redact: engine unavailable, request rejected to avoid leaking secrets: %w", err)
		}
		flags := strings.TrimSpace(ra.channel.RedactFlags)
		if flags == "" {
			flags = redactDefaultFlags()
		}
		s, err = engine.NewSession(flags, inboundRedactProtocol(ra.inboundType), redactNoticeEnabled())
		if err != nil {
			ra.redactFailed = true
			return fmt.Errorf("redact: session create failed, request rejected to avoid leaking secrets: %w", err)
		}
		ra.redactSession = s
	}

	// Scan the client protocol bytes once: this mints (and caches in the session) a
	// reversible token for every sensitive value in the CURRENT turn and feeds the
	// count signal the gates below use. The returned bytes are discarded — oct's own
	// shape decisions own the wire (no whole-field graft). RedactJSONBody never
	// mutates its input, so no copy is needed here.
	if _, err := s.RedactJSONBody(ra.internalRequest.RawRequest); err != nil {
		ra.redactFailed = true
		return fmt.Errorf("redact: inbound scan failed, request rejected to avoid leaking secrets: %w", err)
	}
	// Fast path: nothing sensitive in the client bytes AND no notice to inject ->
	// keep the shaped fields untouched. A bridged continuation is exempt: the
	// rebuilt prior-turn history lives only in Messages (the bytes scanned above are
	// the CLIENT's current turn), so this count cannot see bridged-history
	// credentials — the text pass below must run before this attempt is a no-op.
	if s.Count() == 0 && !redactNoticeEnabled() && !ra.chatHistoryRebuilt {
		return nil
	}

	// Redact the fields oct is about to send, on copies. The originals captured at
	// the top of retryWithAdapter feed the restore log, the recorded transcript and
	// the outbound-request rollback, so the input is never mutated.
	messages, err := redactMessagesText(s, ra.internalRequest.Messages)
	if err != nil {
		ra.redactFailed = true
		return fmt.Errorf("redact: message redaction failed, request rejected to avoid leaking secrets: %w", err)
	}

	// ResponsesInstructions: only a non-empty current value is scanned. Nil stays
	// nil, and suppressCodexHoistedContext's suppression sentinel (a non-nil "")
	// is left untouched — RedactText("") would be a no-op anyway, but not touching
	// the sentinel keeps the wire decision pointer-identical.
	var instructions *string
	if current := ra.internalRequest.ResponsesInstructions; current != nil && *current != "" {
		redacted, ierr := s.RedactText(*current)
		if ierr != nil {
			ra.redactFailed = true
			return fmt.Errorf("redact: instructions redaction failed, request rejected to avoid leaking secrets: %w", ierr)
		}
		instructions = &redacted
	}

	// ResponsesInputRaw: the current (possibly shape-synthesized) input array. It
	// is WRAPPED as {"input": arr} before the scan so the core sees the
	// openai_responses path shape: BOTH the reasoning/compaction model-state skip
	// (isRequestModelState matches path ["input","<idx>"]) and the notice injection
	// (injectRedactNotice targets body.input arrays) are keyed off an "input" root —
	// on the bare array both silently miss, the high-entropy detector would then
	// chew through a reasoning item's encrypted_content, the upstream rejects it
	// with invalid_encrypted_content and oct retries via dropResponsesEncryptedContent:
	// exactly the codex-shape cost regression this layer must not cause.
	//
	// Notice — TWO paths, exactly ONE injection per request:
	//   - responses inbound: the session protocol IS openai_responses, so the
	//     RedactJSONBody call below injects into body.input itself (locked by
	//     TestRelayRedactResponsesReasoningStateUntouched).
	//   - other inbounds (chat/anthropic routed to a codex-shaped responses channel,
	//     whose input oct synthesized): the session protocol would never match
	//     body.input, so the notice is prepended EXPLICITLY first via
	//     InjectNoticeInputRaw (which forces the responses protocol). The
	//     RedactJSONBody call that follows still carries the SESSION protocol, and
	//     injectRedactNotice for openai_chat/anthropic_messages only ever matches
	//     body.messages — absent from this wrapper — so no double injection.
	//     isRequestModelState for those protocols requires path[0]=="messages", so on
	//     this wrapper the reasoning/compaction skip never matches either; harmless,
	//     because a synthesized input is built from chat Messages and carries no
	//     reasoning item / encrypted_content (synthesizeCodexResponsesInputRaw skips
	//     system/developer roles).
	var inputRaw []byte
	if current := ra.internalRequest.ResponsesInputRaw; len(current) > 0 {
		injected := current
		if ra.inboundType != inbound.InboundTypeOpenAIResponse {
			noticeRaw, nerr := s.InjectNoticeInputRaw(current)
			if nerr != nil {
				ra.redactFailed = true
				return fmt.Errorf("redact: responses input notice failed, request rejected to avoid leaking secrets: %w", nerr)
			}
			injected = noticeRaw
		}
		wrapped := []byte("{\"input\":" + string(injected) + "}")
		redactedWrapped, rerr := s.RedactJSONBody(wrapped)
		if rerr != nil {
			ra.redactFailed = true
			return fmt.Errorf("redact: responses input redaction failed, request rejected to avoid leaking secrets: %w", rerr)
		}
		var unwrapped struct {
			Input json.RawMessage `json:"input"`
		}
		if uerr := json.Unmarshal(redactedWrapped, &unwrapped); uerr != nil {
			ra.redactFailed = true
			return fmt.Errorf("redact: responses input unwrap failed, request rejected to avoid leaking secrets: %w", uerr)
		}
		if len(unwrapped.Input) == 0 {
			ra.redactFailed = true
			return fmt.Errorf("redact: responses input unwrap yielded an empty input, request rejected to avoid leaking secrets")
		}
		inputRaw = unwrapped.Input
	}

	// Strict no-op gate: nothing sensitive and no notice -> leave every field exactly
	// as oct shaped it (a bridged-but-clean history lands here).
	if s.Count() == 0 && !redactNoticeEnabled() {
		return nil
	}

	ra.internalRequest.Messages = messages
	if instructions != nil {
		ra.internalRequest.ResponsesInstructions = instructions
	}
	if inputRaw != nil {
		ra.internalRequest.ResponsesInputRaw = inputRaw
	}
	ra.redactApplied = true
	// A protection-demanding attempt pins the request redaction sticky so a later
	// attempt on an opted-out channel still redacts (never silently swap to an
	// unprotected channel).
	ra.redactRequired = true
	return nil
}

// redactMessagesText returns a DEEP COPY of msgs with every text-bearing field
// scanned through s.RedactText — message string content, content-part text, and
// tool-call arguments (both internal ToolCalls[] and the deprecated FunctionCall) —
// and, when the session was created with notice enabled, the core redaction notice
// prepended to the FIRST user message. It is the per-field counterpart of the
// core's body scan (RedactJSONBody), applied directly to the in-memory Messages
// oct is about to send: it covers prior-turn text the history bridges rebuilt from
// the transcript (which stores ORIGINAL, unredacted text — invisible to the
// client-bytes scan) while never whole-field-replacing Messages, so the bridge
// rebuild stays intact and mis-shaped reparse artifacts are impossible. Each new
// value mints a fresh token in THIS session, restorable against the same session.
// The input slice and its sub-values are NEVER mutated: forward()/
// prepareRacerAttempt capture the originals at the top of retryWithAdapter for the
// restore log, the recorded transcript, and the outbound-request rollback, so they
// must stay original.
func redactMessagesText(s *redact.Session, msgs []model.Message) ([]model.Message, error) {
	notice, err := s.NoticeText()
	if err != nil {
		return nil, err
	}
	out := make([]model.Message, 0, len(msgs))
	noticeInjected := false
	for _, src := range msgs {
		msg := src
		// Content: a single string value...
		if src.Content.Content != nil {
			redacted, perr := s.RedactText(*src.Content.Content)
			if perr != nil {
				return nil, perr
			}
			msg.Content.Content = &redacted
		}
		// ...or a part array; only text-bearing parts are scanned (ImageURL/audio/
		// file/identity payloads are left untouched).
		if len(src.Content.MultipleContent) > 0 {
			newParts := make([]model.MessageContentPart, 0, len(src.Content.MultipleContent))
			for _, part := range src.Content.MultipleContent {
				cp := part
				if part.Text != nil {
					redacted, perr := s.RedactText(*part.Text)
					if perr != nil {
						return nil, perr
					}
					cp.Text = &redacted
				}
				newParts = append(newParts, cp)
			}
			msg.Content.MultipleContent = newParts
		}
		// Tool-call arguments (the model's function-call payload carries real text).
		if len(src.ToolCalls) > 0 {
			newCalls := make([]model.ToolCall, 0, len(src.ToolCalls))
			for _, tc := range src.ToolCalls {
				call := tc
				redacted, perr := s.RedactText(tc.Function.Arguments)
				if perr != nil {
					return nil, perr
				}
				call.Function.Arguments = redacted
				newCalls = append(newCalls, call)
			}
			msg.ToolCalls = newCalls
		}
		if src.FunctionCall != nil {
			fc := *src.FunctionCall
			redacted, perr := s.RedactText(fc.Arguments)
			if perr != nil {
				return nil, perr
			}
			fc.Arguments = redacted
			msg.FunctionCall = &fc
		}
		// Notice: fixed first_user, matching the core's injectRedactNotice default.
		// REDACT_NOTICE_POSITION=last_user is an environment knob honored only by the
		// core's own RedactJSONBody path (oct never sets it), so a bridged continuation
		// always pins first_user. No user message -> silently skip (as the core does).
		if !noticeInjected && src.Role == "user" {
			msg.Content = prependRedactNotice(msg.Content, notice)
			noticeInjected = true
		}
		out = append(out, msg)
	}
	return out, nil
}

// prependRedactNotice places the core notice text before the given message content:
// string content becomes "notice\n\nstring", part-array content gets a leading text
// part, and empty content becomes the bare notice string. A blank notice (session
// without notice) is a no-op.
func prependRedactNotice(content model.MessageContent, notice string) model.MessageContent {
	if notice == "" {
		return content
	}
	prefix := notice + "\n\n"
	if content.Content != nil {
		merged := prefix + *content.Content
		content.Content = &merged
		return content
	}
	if len(content.MultipleContent) > 0 {
		noticeText := notice
		newParts := make([]model.MessageContentPart, 0, 1+len(content.MultipleContent))
		newParts = append(newParts, model.MessageContentPart{Type: "text", Text: &noticeText})
		newParts = append(newParts, content.MultipleContent...)
		content.MultipleContent = newParts
		return content
	}
	only := notice
	content.Content = &only
	return content
}

// redactClose releases the attempt's session engine VM when the attempt finishes.
func (ra *relayAttempt) redactClose() {
	if ra.redactSession != nil {
		ra.redactSession.Close()
		ra.redactSession = nil
	}
}

// redactRestoreSseEvent feeds one CLIENT-format SSE event's full text through the
// stream restorer and returns the restored event text (0, 1, or N events joined
// by "\n\n"). An empty result means the restorer is buffering a placeholder split
// across events; the caller drops this event's data and the closing half will
// surface it. Fail-closed: a restorer failure is returned as an error so the caller
// can surface it (in-band after commit) instead of passing raw placeholders on.
func (ra *relayAttempt) redactRestoreSseEvent(eventText string) (string, error) {
	if ra.redactSession == nil || !ra.redactApplied {
		return eventText, nil
	}
	if ra.redactStreamBroken {
		return "", fmt.Errorf("redact: stream restorer already failed")
	}
	if ra.redactSse == nil {
		r, err := ra.redactSession.NewSseRestorer()
		if err != nil {
			ra.redactStreamBroken = true
			return "", fmt.Errorf("redact: stream restorer unavailable: %w", err)
		}
		ra.redactSse = r
	}
	out, err := ra.redactSse.Ingest(eventText)
	if err != nil {
		ra.redactStreamBroken = true
		return "", fmt.Errorf("redact: stream restore failed: %w", err)
	}
	return out, nil
}

// redactFinishSse flushes the stream restorer at end-of-stream (or before a forced
// terminal write) and returns any final restored event text.
func (ra *relayAttempt) redactFinishSse() (string, error) {
	if ra.redactSession == nil || !ra.redactApplied || ra.redactStreamBroken || ra.redactSse == nil {
		return "", nil
	}
	out, err := ra.redactSse.Finish()
	if err != nil {
		ra.redactStreamBroken = true
		return "", fmt.Errorf("redact: stream finish failed: %w", err)
	}
	return out, nil
}

// restoreClientStreamSse restores placeholders inside one client-format SSE byte
// chunk (the downstream bytes produced by inAdapter.TransformStream) before they
// are written to the client. A placeholder split across events is buffered and only
// surfaces once its closing half arrives, so the returned bytes may be empty while
// an event is pending. No-op when this attempt redacted nothing.
func (ra *relayAttempt) restoreClientStreamSse(data []byte) ([]byte, error) {
	if ra.redactSession == nil || !ra.redactApplied || len(data) == 0 {
		return data, nil
	}
	var out bytes.Buffer
	for _, blob := range splitClientSseEvents(string(data)) {
		eventType, payload := parseClientSseEvent(blob)
		if payload == "" {
			// Comment/heartbeat/blank line: nothing to restore, forward as-is.
			out.WriteString(blob)
			continue
		}
		var eventText strings.Builder
		if eventType != "" {
			eventText.WriteString("event: " + eventType + "\n")
		}
		// 多行 data 按 SSE 规范拆成多条 data: 行 (Cosy 解析器逐行收、还原端 join 回
		// "\n") — 单行写法会把续行变成无前缀裸行, 解析时被丢弃 = 内容丢失。
		for _, line := range strings.Split(payload, "\n") {
			eventText.WriteString("data: " + line + "\n")
		}
		restored, err := ra.redactRestoreSseEvent(eventText.String())
		if err != nil {
			return nil, err
		}
		if restored == "" {
			continue // buffered: a placeholder half is pending
		}
		for _, ev := range splitRestoredEventData(restored, eventType) {
			out.WriteString(restoredSseEventText(ev))
		}
	}
	return out.Bytes(), nil
}

// redactFlushClientSse flushes the stream restorer and returns the trailing
// restored client-format SSE bytes (used at end-of-stream and before a forced
// terminal write so buffered text is never lost).
func (ra *relayAttempt) redactFlushClientSse() ([]byte, error) {
	tail, err := ra.redactFinishSse()
	if err != nil {
		return nil, err
	}
	if tail == "" {
		return nil, nil
	}
	var out bytes.Buffer
	for _, ev := range splitRestoredEventData(tail, "") {
		out.WriteString(restoredSseEventText(ev))
	}
	return out.Bytes(), nil
}

// splitClientSseEvents splits a client-format SSE byte chunk into individual event
// blobs (separated by a blank line). Empty segments are dropped.
func splitClientSseEvents(data string) []string {
	if data == "" {
		return nil
	}
	parts := strings.Split(data, "\n\n")
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseClientSseEvent extracts the event name and the joined data payload from a
// client-format SSE event blob.
func parseClientSseEvent(blob string) (eventType, payload string) {
	var dataLines []string
	for _, line := range strings.Split(blob, "\n") {
		switch {
		case strings.HasPrefix(line, "event:"):
			eventType = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimPrefix(line[len("data:"):], " "))
		}
	}
	if len(dataLines) == 0 {
		return eventType, ""
	}
	return eventType, strings.Join(dataLines, "\n")
}

// restoredSseEventText renders one restored event back into client-format SSE bytes.
func restoredSseEventText(ev restoredSseEvent) string {
	var b strings.Builder
	if ev.eventType != "" {
		b.WriteString("event: " + ev.eventType + "\n")
	}
	for _, line := range strings.Split(ev.data, "\n") {
		b.WriteString("data: " + line + "\n")
	}
	b.WriteString("\n")
	return b.String()
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
