// Package redact embeds the CosyRedactGateway pure-logic core (detection, redaction,
// and placeholder restoration) and executes it on the in-process goja JS runtime.
//
// oct sends the request itself with its own TLS/header fingerprint stack; this package
// only rewrites request body text (secrets -> reversible placeholders) and restores
// placeholders in responses (JSON and SSE, including cross-chunk tool deltas). The
// upstream fingerprint is never touched.
package redact

import (
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/dop251/goja"
)

//go:embed cosy/worker-core.js
var cosyCoreJS string

// Engine is a pool of goja VMs pre-loaded with the Cosy core. VMs are stateless per
// call: per-request mapping state lives in Go (see Mapping) and is passed into each
// JS call, so a pool sized to concurrency is enough.
type Engine struct {
	pool sync.Pool
}

// NewEngine creates the VM pool. The first VM is warmed synchronously so a broken
// script fails fast at startup instead of on the first request.
func NewEngine() (*Engine, error) {
	e := &Engine{}
	if _, err := e.newVM(); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *Engine) newVM() (*goja.Runtime, error) {
	vm := goja.New()
	// Synchronous SHA-256 (replaces crypto.subtle.digest in the vendored core).
	shaFn := func(call goja.FunctionCall) goja.Value {
		s, ok := call.Argument(0).Export().(string)
		if !ok {
			panic(vm.NewTypeError("__octSha256Hex expects a string"))
		}
		sum := sha256.Sum256([]byte(s))
		return vm.ToValue(hex.EncodeToString(sum[:]))
	}
	// Cryptographic random hex (replaces crypto.getRandomValues for the runtime salt).
	rndFn := func(call goja.FunctionCall) goja.Value {
		n := 32
		if v, ok := call.Argument(0).Export().(int); ok && v > 0 {
			n = v
		}
		b := make([]byte, n)
		if _, err := rand.Read(b); err != nil {
			panic(vm.NewGoError(fmt.Errorf("__octRandomHex: %w", err)))
		}
		return vm.ToValue(hex.EncodeToString(b))
	}
	vm.Set("__octSha256Hex", shaFn)
	vm.Set("__octRandomHex", rndFn)
	if _, err := vm.RunString(cosyCoreJS); err != nil {
		return nil, fmt.Errorf("redact: load cosy core: %w", err)
	}
	if vm.Get("__cosy") == nil {
		return nil, fmt.Errorf("redact: cosy core did not export __cosy")
	}
	// Warm up: compile every regex literal and populate the entropy tables once per
	// VM so per-request calls skip first-use compilation costs.
	if _, err := vm.RunString(`new (globalThis.__cosy.RedactionContext)({}); globalThis.__cosy.findSensitiveSpans("warmup sk-abcdefghij a@b.com 13800138000", globalThis.__cosy.parseFlags("HPSIBEG"))`); err != nil {
		return nil, fmt.Errorf("redact: warmup: %w", err)
	}
	return vm, nil
}

func (e *Engine) acquire() (*goja.Runtime, error) {
	if v, ok := e.pool.Get().(*goja.Runtime); ok && v != nil {
		return v, nil
	}
	return e.newVM()
}

func (e *Engine) release(vm *goja.Runtime) {
	// Bound the pool: keep at most one idle VM per engine; concurrency spikes create
	// extra VMs that are dropped on release instead of accumulating.
	e.pool.Put(vm)
}

// cosy returns the __cosy export object from a warmed VM.
func cosy(vm *goja.Runtime) goja.Value {
	return vm.Get("__cosy")
}

// Mapping is the per-request redaction state (salt + both direction maps). It is JSON
// round-trippable so state survives across VM borrows and stream chunks.
type Mapping struct {
	Salt       string            `json:"salt"`
	RawToToken map[string]string `json:"raw_to_token"`
	TokenToRaw map[string]string `json:"token_to_raw"`
}

// NewMapping allocates a fresh per-request mapping with a random salt.
func NewMapping() *Mapping {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Errorf("redact: salt: %w", err))
	}
	return &Mapping{Salt: hex.EncodeToString(b), RawToToken: map[string]string{}, TokenToRaw: map[string]string{}}
}

// Count reports how many distinct values were redacted under this mapping.
func (m *Mapping) Count() int { return len(m.RawToToken) }
