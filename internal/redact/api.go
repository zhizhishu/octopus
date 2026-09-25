package redact

import (
	"fmt"
	"sync"

	"github.com/dop251/goja"
)

// Session is the per-request redaction handle. It borrows one VM from the pool for
// the whole request lifetime so JS-side state (the RedactionContext mapping and any
// SseRestorer) survives across the outbound redaction and the inbound restore, then
// returns the VM on Close. The mapping lives only inside the session — nothing is
// persisted, matching Cosy's request-scoped design.
type Session struct {
	engine   *Engine
	vm       *goja.Runtime
	ctx      goja.Value // JS RedactionContext
	flagsRaw string     // canonical flag letters, "" = all detectors
	protocol string
	notice   bool
	count    int // redacted value count, exported after the fact for audit logs

	// mu guards vm access across goroutines: the SSE reader goroutine calls
	// Ingest/Finish while the main request goroutine may call Close (client
	// disconnect unwinds the request before the reader's last in-flight JS call
	// finishes). Close blocks until in-flight calls release the lock, so a VM is
	// never returned to the pool while still in use.
	mu sync.Mutex
}

// NewSession borrows a VM, builds a fresh RedactionContext (new random salt), and
// pre-parses the detector flags. protocol is one of "openai_chat", "openai_responses",
// "anthropic_messages", "generic" (see Cosy detectProtocol); pass "" to let the core
// detect it from the first body.
func (e *Engine) NewSession(flagsRaw, protocol string, notice bool) (*Session, error) {
	vm, err := e.acquire()
	if err != nil {
		return nil, err
	}
	s := &Session{engine: e, vm: vm, flagsRaw: flagsRaw, protocol: protocol, notice: notice}
	ctxVal, err := vm.RunString("new (globalThis.__cosy.RedactionContext)({})")
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("redact: new RedactionContext: %w", err)
	}
	s.ctx = ctxVal
	vm.Set("__redactCtx", ctxVal)
	return s, nil
}

// Count reports how many distinct values this session redacted (audit logging).
func (s *Session) Count() int { return s.count }

// RedactText scans one plain text value and replaces secrets with placeholders.
// Exposed for the admin redaction test endpoint; the relay path uses
// RedactJSONBody. Updates the session count.
func (s *Session) RedactText(text string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vm == nil {
		return "", fmt.Errorf("redact: session closed")
	}
	flags, err := s.flagsValue()
	if err != nil {
		return "", err
	}
	cb, ok := goja.AssertFunction(s.ctx.ToObject(s.vm).Get("redactText"))
	if !ok {
		return "", fmt.Errorf("redact: ctx.redactText not callable")
	}
	out, err := cb(s.ctx, s.vm.ToValue(text), flags)
	if err != nil {
		return "", fmt.Errorf("redact: redactText: %w", err)
	}
	sizeVal := s.ctx.ToObject(s.vm).Get("rawToToken").ToObject(s.vm).Get("size")
	if n, ok := sizeVal.Export().(int64); ok {
		s.count = int(n)
	}
	return out.String(), nil
}

// RestoreText restores placeholders in one plain text value.
func (s *Session) RestoreText(text string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vm == nil {
		return "", fmt.Errorf("redact: session closed")
	}
	cb, ok := goja.AssertFunction(s.ctx.ToObject(s.vm).Get("restoreText"))
	if !ok {
		return "", fmt.Errorf("redact: ctx.restoreText not callable")
	}
	out, err := cb(s.ctx, s.vm.ToValue(text))
	if err != nil {
		return "", fmt.Errorf("redact: restoreText: %w", err)
	}
	return out.String(), nil
}

// Close returns the VM to the pool. Safe to call multiple times.
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vm != nil {
		s.engine.release(s.vm)
		s.vm = nil
	}
}

// fn returns an exported cosy function.
func (s *Session) fn(name string) (goja.Callable, error) {
	v := cosy(s.vm).ToObject(s.vm).Get(name)
	cb, ok := goja.AssertFunction(v)
	if !ok {
		return nil, fmt.Errorf("redact: cosy export %s is not callable", name)
	}
	return cb, nil
}

// flagsValue resolves the session flags into the parsed flags object.
func (s *Session) flagsValue() (goja.Value, error) {
	parse, err := s.fn("parseFlags")
	if err != nil {
		return nil, err
	}
	v, err := parse(goja.Undefined(), s.vm.ToValue(s.flagsRaw))
	if err != nil {
		return nil, fmt.Errorf("redact: parseFlags(%q): %w", s.flagsRaw, err)
	}
	return v, nil
}

// resolveProtocol fills an empty protocol from the body shape via detectProtocol.
func (s *Session) resolveProtocol(parsed goja.Value) (string, error) {
	if s.protocol != "" {
		return s.protocol, nil
	}
	detect, err := s.fn("detectProtocol")
	if err != nil {
		return "", err
	}
	// detectProtocol(body, upstream, headers): pass a synthetic upstream path and no
	// headers so detection falls back to the body shape (messages/input arrays).
	v, err := detect(goja.Undefined(), parsed, s.vm.ToValue(map[string]any{"pathname": "/"}), goja.Null())
	if err != nil {
		return "generic", nil // detection is best-effort; generic still redacts text
	}
	proto, _ := v.Export().(string)
	if proto == "" {
		proto = "generic"
	}
	return proto, nil
}

// parseJSON parses body text via JSON.parse (never expression evaluation, so a
// non-JSON body can never execute as script).
func (s *Session) parseJSON(body []byte) (goja.Value, error) {
	parse, ok := goja.AssertFunction(s.vm.Get("JSON").ToObject(s.vm).Get("parse"))
	if !ok {
		return nil, fmt.Errorf("redact: JSON.parse not callable")
	}
	return parse(goja.Undefined(), s.vm.ToValue(string(body)))
}

// stringifyJSON serializes a JS value back to JSON bytes.
func (s *Session) stringifyJSON(v goja.Value) ([]byte, error) {
	str, ok := goja.AssertFunction(s.vm.Get("JSON").ToObject(s.vm).Get("stringify"))
	if !ok {
		return nil, fmt.Errorf("redact: JSON.stringify not callable")
	}
	out, err := str(goja.Undefined(), v)
	if err != nil {
		return nil, fmt.Errorf("redact: stringify: %w", err)
	}
	return []byte(out.String()), nil
}

// RedactJSONBody scans the outbound request body, replaces sensitive values with
// reversible placeholders, and (when the session has notice enabled) injects the
// redaction notice into the first user message. The returned bytes are what oct must
// send upstream — oct's own TLS/header fingerprint stack sends them unchanged.
// When nothing is redacted the input bytes are returned as-is (byte-identical, no
// re-serialization), so untouched traffic pays only one detection pass.
func (s *Session) RedactJSONBody(body []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vm == nil {
		return nil, fmt.Errorf("redact: session closed")
	}
	parsed, err := s.parseJSON(body)
	if err != nil {
		// Malformed JSON upstream of us is not ours to fix; pass through untouched.
		return body, nil
	}
	proto, err := s.resolveProtocol(parsed)
	if err != nil {
		return nil, err
	}
	flags, err := s.flagsValue()
	if err != nil {
		return nil, err
	}
	redact, err := s.fn("redactJson")
	if err != nil {
		return nil, err
	}
	out, err := redact(goja.Undefined(), parsed, s.ctx, flags, s.vm.ToValue(proto))
	if err != nil {
		return nil, fmt.Errorf("redact: redactJson: %w", err)
	}
	if s.notice {
		inject, err := s.fn("injectRedactNotice")
		if err != nil {
			return nil, err
		}
		if _, err := inject(goja.Undefined(), out, s.vm.ToValue(proto)); err != nil {
			return nil, fmt.Errorf("redact: injectRedactNotice: %w", err)
		}
	}
	sizeVal := s.ctx.ToObject(s.vm).Get("rawToToken").ToObject(s.vm).Get("size")
	if n, ok := sizeVal.Export().(int64); ok {
		s.count = int(n)
	}
	// Fast path: nothing redacted and no notice -> return the original bytes.
	if s.count == 0 && !s.notice {
		return body, nil
	}
	stringify, err := s.fn("__stringify")
	if err != nil {
		// Fall back to JSON.stringify on the VM.
		v, serr := s.vm.RunString("JSON.stringify")
		if serr != nil {
			return nil, serr
		}
		cb, ok := goja.AssertFunction(v)
		if !ok {
			return nil, fmt.Errorf("redact: JSON.stringify not callable")
		}
		out2, serr := cb(goja.Undefined(), out)
		if serr != nil {
			return nil, serr
		}
		return []byte(out2.String()), nil
	}
	v, err := stringify(goja.Undefined(), out)
	if err != nil {
		return nil, fmt.Errorf("redact: stringify: %w", err)
	}
	return []byte(v.String()), nil
}

// RestoreJSONBody restores placeholders in a complete (non-streaming) response body.
// When the session redacted nothing the input bytes are returned untouched.
func (s *Session) RestoreJSONBody(body []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vm == nil {
		return nil, fmt.Errorf("redact: session closed")
	}
	if s.count == 0 {
		return body, nil
	}
	parsed, err := s.parseJSON(body)
	if err != nil {
		return body, nil // not JSON — nothing we can restore
	}
	restore, err := s.fn("restoreJson")
	if err != nil {
		return nil, err
	}
	out, err := restore(goja.Undefined(), parsed, s.ctx)
	if err != nil {
		return nil, fmt.Errorf("redact: restoreJson: %w", err)
	}
	return s.stringifyJSON(out)
}

// SseRestorer restores placeholders across SSE events (including tool-argument
// deltas split across chunks). It borrows the session VM; finish must be called
// before the session closes.
type SseRestorer struct {
	session *Session
	obj     goja.Value // nil = pass-through (nothing was redacted)
}

// NewSseRestorer builds the stream restorer on the session's VM.
func (s *Session) NewSseRestorer() (*SseRestorer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vm == nil {
		return nil, fmt.Errorf("redact: session closed")
	}
	if s.count == 0 {
		return &SseRestorer{session: s}, nil // pass-through: nothing was redacted
	}
	obj, err := s.vm.RunString("new (globalThis.__cosy.SseRestorer)(globalThis.__redactCtx)")
	if err != nil {
		return nil, fmt.Errorf("redact: new SseRestorer: %w", err)
	}
	return &SseRestorer{session: s, obj: obj}, nil
}

// Ingest feeds one raw SSE event (the text between blank-line separators, without
// the trailing blank line) and returns the restored event text to emit downstream
// ("" means hold back — the restorer is buffering an incomplete placeholder).
func (r *SseRestorer) Ingest(rawEvent string) (string, error) {
	r.session.mu.Lock()
	defer r.session.mu.Unlock()
	if r.obj == nil {
		return rawEvent, nil // pass-through mode
	}
	if r.session.vm == nil {
		return "", fmt.Errorf("redact: session closed")
	}
	m := r.obj.ToObject(r.session.vm).Get("ingest")
	cb, ok := goja.AssertFunction(m)
	if !ok {
		return "", fmt.Errorf("redact: SseRestorer.ingest not callable")
	}
	out, err := cb(r.obj, r.session.vm.ToValue(rawEvent))
	if err != nil {
		return "", fmt.Errorf("redact: ingest: %w", err)
	}
	return out.String(), nil
}

// Finish flushes any buffered channel state at end-of-stream and returns the final
// pending output ("" when nothing was held).
func (r *SseRestorer) Finish() (string, error) {
	r.session.mu.Lock()
	defer r.session.mu.Unlock()
	if r.obj == nil {
		return "", nil
	}
	if r.session.vm == nil {
		return "", fmt.Errorf("redact: session closed")
	}
	m := r.obj.ToObject(r.session.vm).Get("finish")
	cb, ok := goja.AssertFunction(m)
	if !ok {
		return "", fmt.Errorf("redact: SseRestorer.finish not callable")
	}
	out, err := cb(r.obj)
	if err != nil {
		return "", fmt.Errorf("redact: finish: %w", err)
	}
	return out.String(), nil
}
