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

const (
	// StatusAutoRetrying indicates the request is undergoing machine-first automatic rescue rounds.
	StatusAutoRetrying = "auto_retrying"
	// StatusAwaitingOperator indicates machine rescue rounds are exhausted and the request is awaiting human operator resolution.
	StatusAwaitingOperator = "awaiting_operator"
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

// Pending is one held request awaiting an operator decision or automatic retry.
type Pending struct {
	mu           sync.Mutex
	ID           string
	LogID        int64
	RequestModel string
	Endpoint     string
	Attempts     []model.ChannelAttempt
	LastError    string
	CreatedAt    time.Time
	Status       string
	RescueRound  int
	NextRetryAt  *time.Time

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
	Status       string                 `json:"status"`
	RescueRound  int                    `json:"rescue_round"`
	NextRetryAt  *time.Time             `json:"next_retry_at,omitempty"`
}

var registry = struct {
	sync.RWMutex
	pending map[string]*Pending
}{pending: make(map[string]*Pending)}

func cloneAttempts(attempts []model.ChannelAttempt) []model.ChannelAttempt {
	if attempts == nil {
		return nil
	}
	cp := make([]model.ChannelAttempt, len(attempts))
	copy(cp, attempts)
	return cp
}

// BackoffDuration calculates the deterministic capped exponential backoff duration (1s, 2s, 4s, 8s, max 15s).
func BackoffDuration(round int) time.Duration {
	if round <= 1 {
		return 1 * time.Second
	}
	shift := round - 1
	if shift > 4 {
		return 15 * time.Second
	}
	secs := 1 << shift
	if secs > 15 {
		secs = 15
	}
	return time.Duration(secs) * time.Second
}

// UpdateStatus updates the status, rescue round, and next retry timestamp in a concurrency-safe manner.
func (p *Pending) UpdateStatus(status string, rescueRound int, nextRetryAt *time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Status = status
	p.RescueRound = rescueRound
	if nextRetryAt != nil {
		t := *nextRetryAt
		p.NextRetryAt = &t
	} else {
		p.NextRetryAt = nil
	}
}

// UpdateAttempts updates the recorded attempts and last error in a concurrency-safe manner.
func (p *Pending) UpdateAttempts(attempts []model.ChannelAttempt, lastErr string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Attempts = cloneAttempts(attempts)
	if lastErr != "" {
		p.LastError = lastErr
	}
}

// UpdateStatus updates the status of a pending intervention by ID.
func UpdateStatus(id string, status string, rescueRound int, nextRetryAt *time.Time) error {
	registry.RLock()
	p, ok := registry.pending[id]
	registry.RUnlock()
	if !ok {
		return ErrNotFound
	}
	p.UpdateStatus(status, rescueRound, nextRetryAt)
	return nil
}

// UpdateAttempts updates the recorded attempts of a pending intervention by ID.
func UpdateAttempts(id string, attempts []model.ChannelAttempt, lastErr string) error {
	registry.RLock()
	p, ok := registry.pending[id]
	registry.RUnlock()
	if !ok {
		return ErrNotFound
	}
	p.UpdateAttempts(attempts, lastErr)
	return nil
}

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
	if p.Status == "" {
		p.Status = StatusAutoRetrying
	}
	// Buffered so Resolve never blocks on an operator decision that arrives just as the
	// waiter times out and stops reading.
	p.resolve = make(chan Resolution, 1)
	p.Attempts = cloneAttempts(p.Attempts)

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

// Wait blocks until an operator resolves the request, total timeout context is done, or the client disconnects.
// The entry is removed when Wait returns.
func Wait(ctx context.Context, id string) (Resolution, error) {
	defer remove(id)
	return WaitOperator(ctx, id)
}

// WaitRound waits for either an operator resolution (preemption), client cancellation/timeout,
// or the round backoff duration expiring.
func WaitRound(ctx context.Context, id string, backoff time.Duration) (Resolution, bool, error) {
	registry.RLock()
	p, ok := registry.pending[id]
	registry.RUnlock()
	if !ok {
		return Resolution{}, false, ErrNotFound
	}

	if backoff <= 0 {
		select {
		case resolution := <-p.resolve:
			return resolution, true, nil
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return Resolution{}, false, ErrTimeout
			}
			return Resolution{}, false, ErrCanceled
		default:
			return Resolution{}, false, nil
		}
	}

	timer := time.NewTimer(backoff)
	defer timer.Stop()

	select {
	case resolution := <-p.resolve:
		return resolution, true, nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Resolution{}, false, ErrTimeout
		}
		return Resolution{}, false, ErrCanceled
	case <-timer.C:
		return Resolution{}, false, nil
	}
}

// WaitOperator blocks until an operator resolves the request, total timeout context is done, or client disconnects.
func WaitOperator(ctx context.Context, id string) (Resolution, error) {
	registry.RLock()
	p, ok := registry.pending[id]
	registry.RUnlock()
	if !ok {
		return Resolution{}, ErrNotFound
	}

	select {
	case resolution := <-p.resolve:
		return resolution, nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Resolution{}, ErrTimeout
		}
		return Resolution{}, ErrCanceled
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
	p.mu.Lock()
	defer p.mu.Unlock()

	var nextRetry *time.Time
	if p.NextRetryAt != nil {
		t := *p.NextRetryAt
		nextRetry = &t
	}

	return Snapshot{
		ID:           p.ID,
		LogID:        p.LogID,
		RequestModel: p.RequestModel,
		Endpoint:     p.Endpoint,
		Attempts:     cloneAttempts(p.Attempts),
		LastError:    p.LastError,
		CreatedAt:    p.CreatedAt,
		WaitingFor:   time.Since(p.CreatedAt).Truncate(time.Second).String(),
		Status:       p.Status,
		RescueRound:  p.RescueRound,
		NextRetryAt:  nextRetry,
	}
}
