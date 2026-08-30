// Package intervention holds requests whose upstream attempts have all failed, so an
// operator can pick a working channel from the log page instead of the error reaching
// the client. A CLI (codex/cursor/claude code) that receives an upstream error aborts
// its turn and has to be told to continue; holding the connection open — the relay
// already sends ignorable SSE heartbeats for slow upstreams — keeps the CLI in its
// "working" state until a human resolves the request or the wait times out.
//
// The registry is process-local: a pending request is a live goroutine blocked on its
// own channel, so there is nothing to persist and nothing to share across replicas.
package intervention

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/bestruirui/octopus/internal/model"
)

// Action is the operator's decision for a held request.
type Action string

const (
	// ActionRetryChannel retries the request against one operator-chosen channel.
	ActionRetryChannel Action = "retry_channel"
	// ActionAbort gives up and lets the original upstream error reach the client.
	ActionAbort Action = "abort"
)

var (
	// ErrNotFound is returned when an intervention ID is unknown or already resolved.
	ErrNotFound = errors.New("intervention not found")
	// ErrTimeout reports that nobody resolved the request within the configured window.
	ErrTimeout = errors.New("intervention timed out")
	// ErrCanceled reports that the client hung up while the request was held.
	ErrCanceled = errors.New("intervention canceled")
	// ErrCapacity reports that the process-local registry is already full.
	ErrCapacity = errors.New("intervention capacity reached")
	// ErrDuplicateID reports an explicit ID collision.
	ErrDuplicateID = errors.New("intervention id already exists")
)

// Resolution is what the operator chose on the log page.
type Resolution struct {
	Action    Action `json:"action"`
	ChannelID int    `json:"channel_id"`
	KeyID     int    `json:"key_id"`
	ModelName string `json:"model_name"`
}

// Pending is one held request awaiting an operator decision.
type Pending struct {
	ID           string
	LogID        int64
	RequestModel string
	Endpoint     string
	Attempts     []model.ChannelAttempt
	LastError    string
	CreatedAt    time.Time

	resolve chan Resolution
}

// Snapshot is the read-only view handed to the API layer. It deliberately excludes the
// resolve channel so callers cannot resolve a request by mutating a copy.
type Snapshot struct {
	ID           string                 `json:"id"`
	LogID        int64                  `json:"log_id"`
	RequestModel string                 `json:"request_model"`
	Endpoint     string                 `json:"endpoint"`
	Attempts     []model.ChannelAttempt `json:"attempts"`
	LastError    string                 `json:"last_error"`
	CreatedAt    time.Time              `json:"created_at"`
	WaitingFor   string                 `json:"waiting_for"`
}

var registry = struct {
	sync.RWMutex
	pending map[string]*Pending
}{pending: make(map[string]*Pending)}

func newID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand only fails on a broken platform; a timestamp still gives a unique
		// enough key for a process-local map and keeps the relay path from erroring out.
		return "iv" + time.Now().Format("20060102150405.000000000")
	}
	return "iv" + hex.EncodeToString(buf)
}

// Register records a held request and returns its ID. Capacity is checked while holding
// the same lock used for insertion, so concurrent callers cannot exceed maxPending.
func Register(p *Pending) (string, error) {
	if p.ID == "" {
		p.ID = newID()
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now()
	}
	// Buffered so Resolve never blocks on an operator decision that arrives just as the
	// waiter times out and stops reading.
	p.resolve = make(chan Resolution, 1)
	p.Attempts = append([]model.ChannelAttempt(nil), p.Attempts...)

	registry.Lock()
	defer registry.Unlock()
	if len(registry.pending) >= maxPending {
		return "", ErrCapacity
	}
	if _, exists := registry.pending[p.ID]; exists {
		return "", ErrDuplicateID
	}
	registry.pending[p.ID] = p
	return p.ID, nil
}

// Wait blocks until an operator resolves the request, the client disconnects, or the
// timeout elapses. The entry is removed in every case.
func Wait(ctx context.Context, id string, timeout time.Duration) (Resolution, error) {
	registry.RLock()
	p, ok := registry.pending[id]
	registry.RUnlock()
	if !ok {
		return Resolution{}, ErrNotFound
	}
	defer remove(id)

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case resolution := <-p.resolve:
		return resolution, nil
	case <-ctx.Done():
		return Resolution{}, ErrCanceled
	case <-timer.C:
		return Resolution{}, ErrTimeout
	}
}

// Resolve delivers an operator decision to the waiting relay goroutine.
func Resolve(id string, r Resolution) error {
	registry.RLock()
	p, ok := registry.pending[id]
	registry.RUnlock()
	if !ok {
		return ErrNotFound
	}
	select {
	case p.resolve <- r:
		return nil
	default:
		// Buffer already holds a decision: someone resolved this request first.
		return ErrNotFound
	}
}

// Cancel drops a held request without resolving it.
func Cancel(id string) { remove(id) }

func remove(id string) {
	registry.Lock()
	delete(registry.pending, id)
	registry.Unlock()
}

// List returns every currently held request, newest last.
func List() []Snapshot {
	registry.RLock()
	defer registry.RUnlock()

	out := make([]Snapshot, 0, len(registry.pending))
	for _, p := range registry.pending {
		out = append(out, p.snapshot())
	}
	sort.Slice(out, func(leftIndex, rightIndex int) bool {
		if out[leftIndex].CreatedAt.Equal(out[rightIndex].CreatedAt) {
			return out[leftIndex].ID < out[rightIndex].ID
		}
		return out[leftIndex].CreatedAt.Before(out[rightIndex].CreatedAt)
	})
	return out
}

// Get returns one held request.
func Get(id string) (Snapshot, bool) {
	registry.RLock()
	defer registry.RUnlock()

	p, ok := registry.pending[id]
	if !ok {
		return Snapshot{}, false
	}
	return p.snapshot(), true
}

func (p *Pending) snapshot() Snapshot {
	return Snapshot{
		ID:           p.ID,
		LogID:        p.LogID,
		RequestModel: p.RequestModel,
		Endpoint:     p.Endpoint,
		Attempts:     append([]model.ChannelAttempt(nil), p.Attempts...),
		LastError:    p.LastError,
		CreatedAt:    p.CreatedAt,
		WaitingFor:   time.Since(p.CreatedAt).Truncate(time.Second).String(),
	}
}
