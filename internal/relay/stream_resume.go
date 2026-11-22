package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/relay/balancer"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	"github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
	"github.com/zhizhishu/octopus/internal/utils/tokenizer"
)

const (
	streamResumeWait       = 15 * time.Second
	streamResumeMaxPrefix  = 64 << 10
	streamResumeMaxRequest = 1 << 20
)

type streamPrefill uint8
type streamPrefillLookup func(*url.URL, string) streamPrefill

const (
	noStreamPrefill streamPrefill = iota
	chatStreamPrefix
	anthropicStreamPrefill
)

// Match the actual wire endpoint and model, not the route label or model alias.
func streamPrefillCapability(u *url.URL, name string) streamPrefill {
	if u == nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Port() != "" && u.Port() != "443") {
		return noStreamPrefill
	}
	switch strings.ToLower(u.Hostname()) {
	case "api.deepseek.com":
		if u.EscapedPath() == "/beta/chat/completions" && name == "deepseek-chat" {
			return chatStreamPrefix
		}
	case "api.anthropic.com":
		if u.EscapedPath() == "/v1/messages" {
			switch name {
			case "claude-opus-4-5-20251101", "claude-sonnet-4-5-20250929", "claude-haiku-4-5-20251001":
				return anthropicStreamPrefill
			}
		}
	}
	return noStreamPrefill
}

func streamResumeEnabled(t inbound.InboundType, channel outbound.OutboundType) bool {
	if !((t == inbound.InboundTypeOpenAIChat && (channel == outbound.OutboundTypeOpenAIChat || channel == outbound.OutboundTypeCustomOpenAIChat)) ||
		(t == inbound.InboundTypeAnthropic && channel == outbound.OutboundTypeAnthropic)) {
		return false
	}
	enabled, _ := op.SettingGetBool(dbmodel.SettingKeyRelayStreamResumeEnabled)
	return enabled
}

type streamResumeCapture struct {
	request      *http.Request
	body         map[string]json.RawMessage
	messages     []json.RawMessage
	budgetKey    string
	budget       int64
	outputModel  string
	runtimeModel string
	mode         streamPrefill
	text         strings.Builder
	id           string
	created      int64
	usage        *model.Usage
	invalid      bool
	wire         streamResumeWire
	includeUsage bool
}

type streamResumeRun struct {
	first         *streamResumeCapture
	request       *http.Request
	wire          streamResumeWire
	timer         *time.Timer
	echoOffset    int
	echoDecided   bool
	textWritten   strings.Builder
	accepted      bool
	upstreamUsage *model.Usage
	firstToken    time.Time
}

var (
	errStreamResumeUnsafe = errors.New("upstream did not return a safe text continuation")
	errStreamResumeWait   = errors.New("timed out waiting for stream continuation text")
	errStreamResumeEOF    = errors.New(upstreamStreamTruncatedMessage)
)

func streamResumeJSONKeys(fields map[string]json.RawMessage, allowed string) bool {
	for k := range fields {
		if strings.ContainsRune(k, '|') || !strings.Contains(allowed, "|"+k+"|") {
			return false
		}
	}
	return true
}

func streamResumeText(raw json.RawMessage, blocks bool) bool {
	var str *string
	if json.Unmarshal(raw, &str) == nil {
		return str != nil
	}
	if !blocks {
		return false
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil || len(parts) == 0 {
		return false
	}
	for _, part := range parts {
		if !streamResumeJSONKeys(part, "|type|text|cache_control|") || string(part["type"]) != `"text"` ||
			!streamResumeText(part["text"], false) {
			return false
		}
	}
	return true
}

func parseStreamResumeBody(raw []byte, mode streamPrefill) (map[string]json.RawMessage, []json.RawMessage, bool) {
	var body map[string]json.RawMessage
	if len(raw) > streamResumeMaxRequest || json.Unmarshal(raw, &body) != nil || string(body["stream"]) != "true" {
		return nil, nil, false
	}
	common := "|model|stream|messages|temperature|top_p|max_tokens|"
	allowed := common + "stop|frequency_penalty|presence_penalty|stream_options|n|max_completion_tokens|"
	if mode == anthropicStreamPrefill {
		allowed = common + "top_k|stop_sequences|system|metadata|service_tier|"
		if sys, ok := body["system"]; ok && !streamResumeText(sys, true) {
			return nil, nil, false
		}
	} else if n, ok := body["n"]; ok && string(n) != "1" {
		return nil, nil, false
	}
	if !streamResumeJSONKeys(body, allowed) {
		return nil, nil, false
	}
	var messages []json.RawMessage
	if json.Unmarshal(body["messages"], &messages) != nil || len(messages) == 0 {
		return nil, nil, false
	}
	for i, raw := range messages {
		var msg map[string]json.RawMessage
		if json.Unmarshal(raw, &msg) != nil || !streamResumeJSONKeys(msg, "|role|content|") ||
			!streamResumeText(msg["content"], mode == anthropicStreamPrefill) {
			return nil, nil, false
		}
		role := string(msg["role"])
		if (i == len(messages)-1 && role != `"user"`) ||
			(role != `"user"` && role != `"assistant"` &&
				(mode == anthropicStreamPrefill || role != `"system"` && role != `"developer"`)) {
			return nil, nil, false
		}
	}
	return body, messages, true
}

func streamResumeRequestHasSideEffects(req *http.Request) bool {
	return req.Header.Get("Idempotency-Key") != "" || req.Header.Get("X-Idempotency-Key") != "" ||
		req.Header.Get("Content-Encoding") != ""
}

func (ra *relayAttempt) captureStreamResume(req *http.Request, willStream bool) {
	if ra.streamPrefillLookup == nil {
		return
	}
	ra.streamResumeCapture = nil
	if !willStream || ra.redactSession != nil || req.Method != http.MethodPost || req.URL == nil || req.GetBody == nil ||
		req.ContentLength <= 0 || req.ContentLength > streamResumeMaxRequest ||
		(req.Host != "" && req.Host != req.URL.Host) ||
		streamResumeRequestHasSideEffects(req) || streamResumeRequestHasSideEffects(ra.c.Request) {
		return
	}
	bodyReader, err := req.GetBody()
	if err != nil {
		return
	}
	defer bodyReader.Close()
	raw, err := io.ReadAll(io.LimitReader(bodyReader, streamResumeMaxRequest+1))
	if err != nil {
		return
	}
	var header struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(raw, &header) != nil {
		return
	}
	mode := ra.streamPrefillLookup(req.URL, header.Model)
	if !((mode == chatStreamPrefix && ra.inboundType == inbound.InboundTypeOpenAIChat) ||
		(mode == anthropicStreamPrefill && ra.inboundType == inbound.InboundTypeAnthropic)) {
		return
	}
	if _, _, ok := parseStreamResumeBody(ra.internalRequest.RawRequest, mode); !ok {
		return
	}
	body, messages, ok := parseStreamResumeBody(raw, mode)
	if !ok {
		return
	}
	budgetKey := "max_tokens"
	if _, ok := body["max_completion_tokens"]; ok {
		if _, duplicate := body["max_tokens"]; duplicate || mode != chatStreamPrefix {
			return
		}
		budgetKey = "max_completion_tokens"
	}
	var budget int64
	if json.Unmarshal(body[budgetKey], &budget) != nil || budget <= 0 {
		return
	}
	var options struct {
		IncludeUsage bool `json:"include_usage"`
	}
	var original struct {
		Options json.RawMessage `json:"stream_options"`
	}
	_ = json.Unmarshal(ra.internalRequest.RawRequest, &original)
	_ = json.Unmarshal(original.Options, &options)
	ra.streamResumeCapture = &streamResumeCapture{
		request: req, body: body, messages: messages, budgetKey: budgetKey, budget: budget,
		outputModel: header.Model, mode: mode, includeUsage: options.IncludeUsage,
	}
}

func (s *streamResumeCapture) continuation() *http.Request {
	text := s.text.String()
	if s.mode == anthropicStreamPrefill && strings.TrimRightFunc(text, unicode.IsSpace) != text {
		return nil
	}
	used := int64(tokenizer.CountTokens(text, s.outputModel))
	if s.usage != nil {
		used = max(used, s.usage.CompletionTokens)
	}
	if used <= 0 || used >= s.budget {
		return nil
	}
	last := map[string]any{"role": "assistant", "content": text}
	if s.mode == chatStreamPrefix {
		last["prefix"] = true
	}
	b, err := marshalJSONNoHTMLEscape(last)
	if err != nil {
		return nil
	}
	newMessages, err := marshalJSONNoHTMLEscape(append(s.messages, json.RawMessage(b)))
	if err != nil {
		return nil
	}
	s.body["messages"] = newMessages
	s.body[s.budgetKey], _ = json.Marshal(s.budget - used)
	raw, err := marshalJSONNoHTMLEscape(s.body)
	if err != nil {
		return nil
	}
	r := s.request.Clone(s.request.Context())
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(raw)), nil }
	r.Body, _ = r.GetBody()
	r.ContentLength = int64(len(raw))
	r.Header.Del("Content-Length")
	return r
}

type streamResumeWire struct {
	opened       bool
	textStarted  bool
	textStopped  bool
	terminal     bool
	chatUsageEnd bool
}

func (s *streamResumeWire) textOnly(raw string, mode streamPrefill) bool {
	if raw == "[DONE]" {
		s.terminal = true
		return mode == chatStreamPrefix
	}
	var event map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &event) != nil || event == nil {
		return false
	}
	if mode == chatStreamPrefix {
		if string(event["object"]) != `"chat.completion.chunk"` ||
			!streamResumeJSONKeys(event, "|id|object|created|model|choices|usage|system_fingerprint|service_tier|") {
			return false
		}
		var choices []map[string]json.RawMessage
		if json.Unmarshal(event["choices"], &choices) != nil || len(choices) > 1 {
			return false
		}
		if len(choices) == 0 {
			var usage map[string]json.RawMessage
			if json.Unmarshal(event["usage"], &usage) == nil && usage != nil {
				s.terminal = true
				s.chatUsageEnd = true
			}
			return true
		}
		c := choices[0]
		if string(c["index"]) != "0" || !streamResumeJSONKeys(c, "|index|delta|finish_reason|logprobs|") ||
			(len(c["logprobs"]) > 0 && string(c["logprobs"]) != "null") {
			return false
		}
		if finish := string(c["finish_reason"]); finish != "" && finish != "null" {
			s.terminal = true
			if finish != `"stop"` && finish != `"length"` {
				return false
			}
		}
		var delta map[string]json.RawMessage
		if json.Unmarshal(c["delta"], &delta) != nil {
			return len(c["delta"]) == 0 && s.terminal
		}
		for key, field := range delta {
			switch key {
			case "role":
				if string(field) != `"assistant"` && string(field) != "null" {
					return false
				}
			case "content":
				if string(field) != "null" && !streamResumeText(field, false) {
					return false
				}
			case "reasoning_content", "tool_calls", "function_call":
				if string(field) != "null" {
					return false
				}
			default:
				return false
			}
		}
		return true
	}
	switch string(event["type"]) {
	case `"ping"`:
		return true
	case `"message_start"`:
		var msg struct {
			Content    []json.RawMessage `json:"content"`
			StopReason *string           `json:"stop_reason"`
		}
		if s.opened || json.Unmarshal(event["message"], &msg) != nil || len(msg.Content) != 0 || msg.StopReason != nil {
			return false
		}
		s.opened = true
		return true
	case `"content_block_start"`:
		var block map[string]json.RawMessage
		if !s.opened || s.textStarted || string(event["index"]) != "0" ||
			json.Unmarshal(event["content_block"], &block) != nil ||
			!streamResumeJSONKeys(block, "|type|text|") || string(block["type"]) != `"text"` ||
			string(block["text"]) != `""` {
			return false
		}
		s.textStarted = true
		return true
	case `"content_block_delta"`:
		var delta map[string]json.RawMessage
		return s.textStarted && !s.textStopped && string(event["index"]) == "0" &&
			json.Unmarshal(event["delta"], &delta) == nil && streamResumeJSONKeys(delta, "|type|text|") &&
			string(delta["type"]) == `"text_delta"` && streamResumeText(delta["text"], false)
	case `"content_block_stop"`:
		if !s.textStarted || s.textStopped || string(event["index"]) != "0" {
			return false
		}
		s.textStopped = true
		return true
	case `"message_delta"`:
		var d struct {
			StopReason string `json:"stop_reason"`
		}
		if !s.textStopped || s.terminal || json.Unmarshal(event["delta"], &d) != nil {
			return false
		}
		s.terminal = d.StopReason == "end_turn" || d.StopReason == "max_tokens" || d.StopReason == "stop_sequence"
		return s.terminal
	case `"message_stop"`:
		s.terminal = s.textStarted
		return s.terminal
	}
	return false
}

func (ra *relayAttempt) streamResumeChatWire() *streamResumeWire {
	if r := ra.streamResumeRun; r != nil && r.first.mode == chatStreamPrefix && r.textWritten.Len() > 0 {
		return &r.wire
	}
	if s := ra.streamResumeCapture; s != nil && s.mode == chatStreamPrefix && !s.invalid && strings.TrimSpace(s.text.String()) != "" &&
		ra.wroteMeaningfulDownstream {
		return &s.wire
	}
	return nil
}

func streamResumeTemporaryEvent(raw string, mode streamPrefill) bool {
	var event struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(raw), &event) != nil {
		return false
	}
	if mode == anthropicStreamPrefill {
		return event.Type == "error" && (event.Error.Type == "overloaded_error" || event.Error.Type == "api_error")
	}
	return event.Error.Type == "server_error" &&
		(event.Error.Code == "" || event.Error.Code == "server_error" || event.Error.Code == "internal_error")
}

func streamResumeShapeError(raw string) error {
	var event struct {
		Error map[string]json.RawMessage `json:"error"`
	}
	if json.Unmarshal([]byte(raw), &event) != nil || len(event.Error) == 0 {
		return nil
	}
	err := newUpstreamError(http.StatusBadRequest, []byte(raw))
	if isRequestInvalidUpstreamError(err) {
		return err
	}
	return nil
}

func streamResumeReadInterrupted(err error) bool {
	var nerr net.Error
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, io.ErrUnexpectedEOF) ||
		(errors.As(err, &nerr) && nerr.Timeout())
}

func (ra *relayAttempt) deferStreamResumeTail(tail func()) bool {
	if ra.streamResumeRun != nil {
		ra.streamResumeTail = tail
		return true
	}
	s := ra.streamResumeCapture
	if s == nil || s.invalid || s.wire.terminal || !ra.wroteMeaningfulDownstream || strings.TrimSpace(s.text.String()) == "" ||
		s.id == "" || ra.c.Request.Context().Err() != nil {
		return false
	}
	ra.streamResumeTail = tail
	return true
}

func (ra *relayAttempt) observeStreamResumeChunk(chunk *model.InternalLLMResponse) {
	s := ra.streamResumeCapture
	if s == nil || s.invalid || chunk == nil {
		return
	}
	if s.id == "" && chunk.ID != "" {
		s.id, s.created = chunk.ID, chunk.Created
	}
	if chunk.Usage != nil {
		s.usage = chunk.Usage
	}
}

func (ra *relayAttempt) streamResumeTextWritten(chunk *model.InternalLLMResponse) {
	if ra.streamResumeRun != nil {
		ra.streamResumeRun.textWritten.WriteString(streamResumeChunkText(chunk))
		return
	}
	s := ra.streamResumeCapture
	if s == nil || s.invalid {
		return
	}
	if text := streamResumeChunkText(chunk); len(text) > streamResumeMaxPrefix-s.text.Len() {
		s.invalid = true
		s.text = strings.Builder{}
	} else {
		s.text.WriteString(text)
	}
}

func streamResumeChunkText(chunk *model.InternalLLMResponse) string {
	if chunk != nil && len(chunk.Choices) == 1 && chunk.Choices[0].Delta != nil &&
		chunk.Choices[0].Delta.Content.Content != nil {
		return *chunk.Choices[0].Delta.Content.Content
	}
	return ""
}

// A complete, byte-for-byte echoed prefix can straddle SSE chunks. A partial
// match followed by a mismatch is ambiguous, so never flush those held bytes.
func (r *streamResumeRun) splice(text string) (string, error) {
	if r.echoDecided || text == "" {
		return text, nil
	}
	prefix := r.first.text.String()[r.echoOffset:]
	n := min(len(text), len(prefix))
	if text[:n] != prefix[:n] {
		// Match complete characters, not a shared leading UTF-8 byte.
		left, _ := utf8.DecodeRuneInString(text)
		right, _ := utf8.DecodeRuneInString(prefix)
		if left == right || r.echoOffset > 0 {
			return "", errStreamResumeUnsafe
		}
		r.echoDecided = true
		return text, nil
	}
	r.echoOffset += n
	if n == len(prefix) {
		r.echoDecided = true
	}
	return text[n:], nil
}

func (ra *relayAttempt) projectStreamContinuation(chunk *model.InternalLLMResponse) (*model.InternalLLMResponse, error) {
	r := ra.streamResumeRun
	if r == nil {
		return chunk, nil
	}
	if chunk.Usage != nil {
		r.upstreamUsage = chunk.Usage
	}
	if chunk.Object == "[DONE]" {
		if r.textWritten.Len() == 0 {
			return nil, errStreamResumeUnsafe
		}
		return chunk, nil
	}
	projected := *chunk
	projected.ID, projected.Created = r.first.id, r.first.created
	projected.Usage = nil
	if len(chunk.Choices) == 1 {
		ch := chunk.Choices[0]
		if ch.Delta != nil {
			d := *ch.Delta
			d.Role = ""
			text, err := r.splice(streamResumeChunkText(chunk))
			if err != nil {
				return nil, err
			}
			if text != "" {
				d.Content.Content = &text
				ch.Delta = &d
			} else {
				ch.Delta = nil
			}
		}
		if ch.Delta != nil || ch.FinishReason != nil {
			if r.wire.terminal && r.textWritten.Len() == 0 && ch.Delta == nil {
				return nil, errStreamResumeUnsafe
			}
			if ch.Delta == nil && r.first.mode == chatStreamPrefix {
				ch.Delta = &model.Message{}
			}
			projected.Choices = []model.Choice{ch}
			return &projected, nil
		}
	}
	if r.wire.terminal && r.textWritten.Len() == 0 {
		return nil, errStreamResumeUnsafe
	}
	if chunk.Usage != nil && r.wire.terminal && (r.first.mode == anthropicStreamPrefill || r.first.includeUsage) {
		projected.Usage = ra.streamResumeLogicalUsage()
		return &projected, nil
	}
	return nil, nil
}

func (ra *relayAttempt) streamResumeLogicalUsage() *model.Usage {
	s := ra.streamResumeRun.first
	var usage model.Usage
	if s.usage != nil {
		usage = *s.usage
	} else {
		usage.PromptTokens = int64(estimatePromptTokens(ra.internalRequest, s.outputModel))
	}
	usage.CompletionTokens = int64(tokenizer.CountTokens(s.text.String()+ra.streamResumeRun.textWritten.String(), s.outputModel))
	usage.CompletionTokensDetails = nil
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	return &usage
}

func (ra *relayAttempt) streamResumeAttemptUsage() *model.Usage {
	r := ra.streamResumeRun
	var u model.Usage
	if r.upstreamUsage != nil {
		u = *r.upstreamUsage
	}
	if !r.accepted {
		return &u
	}
	if r.upstreamUsage == nil {
		u.PromptTokens = int64(estimatePromptTokens(ra.internalRequest, r.first.outputModel) +
			tokenizer.CountTokens(r.first.text.String(), r.first.outputModel))
	}
	if u.CompletionTokens == 0 && (r.textWritten.Len() > 0 || r.echoOffset > 0) {
		u.CompletionTokens = int64(tokenizer.CountTokens(r.first.text.String()[:r.echoOffset]+r.textWritten.String(), r.first.outputModel))
	}
	return &u
}

func (ra *relayAttempt) recordStreamResumeProviderUsage(usage *model.Usage) float64 {
	cost := ra.metrics.buildBillingSnapshot(ra.internalRequest.Model, usage)
	cacheHit, cacheWrite, cacheInput := usageCacheStats(usage)
	inputCost := cost.FinalInputCost + cost.FinalCacheReadCost + cost.FinalCacheWriteCost
	if ra.metrics.streamResumeProviderStats == nil {
		ra.metrics.streamResumeProviderStats = &dbmodel.StatsMetrics{}
	}
	ra.metrics.streamResumeProviderStats.Add(dbmodel.StatsMetrics{
		InputToken: usage.PromptTokens, OutputToken: usage.CompletionTokens,
		InputCost: inputCost, OutputCost: cost.FinalOutputCost,
		CacheHitToken: cacheHit, CacheWriteToken: cacheWrite, CacheInputToken: cacheInput,
	})
	return inputCost + cost.FinalOutputCost
}

func (ra *relayAttempt) streamResumeFirstUsage() *model.Usage {
	s := ra.streamResumeCapture
	var usage model.Usage
	if s.usage != nil {
		usage = *s.usage
	} else {
		usage.PromptTokens = int64(estimatePromptTokens(ra.internalRequest, s.outputModel))
	}
	usage.CompletionTokens = max(usage.CompletionTokens, int64(tokenizer.CountTokens(s.text.String(), s.outputModel)))
	return &usage
}

func (ra *relayAttempt) forwardStreamContinuation(ctx context.Context, span *balancer.AttemptSpan) (int, error) {
	r := ra.streamResumeRun
	parent := ctx
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	r.timer = time.AfterFunc(streamResumeWait, func() { cancel(errStreamResumeWait) })
	defer r.timer.Stop()
	if ctx.Err() != nil {
		return 0, context.Cause(ctx)
	}
	req := r.request.WithContext(ctx)
	span.SetUpstreamPath(req.URL.EscapedPath())
	resp, err := ra.sendRequestObserved(req)
	if err == nil {
		defer resp.Body.Close()
		ra.upstreamResponded = true
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, readErr := readErrorBodyFromResponse(resp)
			if readErr != nil {
				err = readErr
			} else {
				ra.debugErrorBody(body)
				upErr := newUpstreamError(resp.StatusCode, body)
				if delay, ok := retryAfterFromHeader(resp.Header); ok {
					upErr.retryAfter, upErr.hasRetryAfter = delay, true
				}
				err = upErr
			}
		} else {
			r.accepted = true
			ra.upstreamEnvelope.HeaderFamilies = dbmodel.ClassifyUpstreamHeaderFamilies(resp.Header)
			err = ra.handleStreamResponse(ctx, resp, ra.outAdapter)
		}
	}
	if parent.Err() == nil && (errors.Is(context.Cause(ctx), errStreamResumeWait) || errors.Is(err, errStreamResumeWait)) {
		return http.StatusGatewayTimeout, &localRelayError{
			status: http.StatusGatewayTimeout, code: "octopus_upstream_stream_timeout",
			strategy: "stream_resume_no_text_timeout;upstream_forwarded=true", message: errStreamResumeWait.Error(),
		}
	}
	if resp == nil {
		return 0, err
	}
	return resp.StatusCode, err
}

func (ra *relayAttempt) finishStreamResume(result attemptResult) attemptResult {
	if ra.streamResumeTail == nil || ra.streamResumeCapture == nil {
		return result
	}
	s := ra.streamResumeCapture
	next := s.continuation()
	adapter := outbound.Get(ra.channel.Type)
	if next != nil && adapter != nil && ra.c.Request.Context().Err() == nil &&
		(ra.totalDeadline.IsZero() || time.Now().Before(ra.totalDeadline)) &&
		(ra.channel.DisableCircuitBreaker || balancer.CircuitClosedScoped(ra.channel.ID, ra.usedKey.ID,
			s.runtimeModel, ra.metrics.RequestEndpoint, ra.routingCapabilityKey())) {
		resumed := &relayAttempt{
			relayRequest: ra.relayRequest, outAdapter: adapter, channel: ra.channel, usedKey: ra.usedKey,
			wroteMeaningfulDownstream: true, streamResumeRun: &streamResumeRun{first: s, request: next},
		}
		retry := resumed.attempt()
		if resumed.streamResumeTail == nil && retry.Success {
			return retry
		}
		if ra.c.Request.Context().Err() == nil {
			if resumed.streamResumeTail != nil {
				resumed.streamResumeTail()
			} else {
				ra.streamResumeTail()
			}
		}
		return retry
	}
	if ra.c.Request.Context().Err() == nil {
		ra.streamResumeTail()
	}
	ra.collectResponse()
	return result
}
