package op

import (
	"sync"
	"time"

	"github.com/bestruirui/octopus/internal/model"
)

const (
	// Homepage polls model-health every 10s and model-rank every 30s. Each miss used
	// to scan relay_logs (rank: the whole table; health: all of today). Concurrent
	// polls plus a slow scan piled up on SQLite and froze the log page with it.
	// ponytail: process-wide TTL + singleflight; upgrade path is an incremental
	// stats table updated from RelayLogAdd instead of a full scan.
	modelHealthCacheTTL = 10 * time.Second
	modelRankCacheTTL   = 15 * time.Second
)

type ttlValue[T any] struct {
	mu        sync.Mutex
	wait      chan struct{}
	computing bool
	ready     bool
	storedAt  time.Time
	key       string
	value     T
	err       error
}

func (cache *ttlValue[T]) getOrCompute(key string, ttl time.Duration, compute func() (T, error)) (T, error) {
	cache.mu.Lock()
	if cache.ready && cache.err == nil && cache.key == key && time.Since(cache.storedAt) < ttl {
		value := cache.value
		cache.mu.Unlock()
		return value, nil
	}
	if cache.computing {
		wait := cache.wait
		cache.mu.Unlock()
		<-wait
		cache.mu.Lock()
		value, err := cache.value, cache.err
		cache.mu.Unlock()
		return value, err
	}
	wait := make(chan struct{})
	cache.wait = wait
	cache.computing = true
	cache.mu.Unlock()

	value, err := compute()

	cache.mu.Lock()
	cache.value = value
	cache.err = err
	cache.key = key
	cache.storedAt = time.Now()
	cache.ready = err == nil
	cache.computing = false
	close(wait)
	cache.mu.Unlock()
	return value, err
}

func (cache *ttlValue[T]) invalidate() {
	cache.mu.Lock()
	cache.ready = false
	cache.mu.Unlock()
}

var modelHealthCache ttlValue[model.ModelHealthResponse]
var modelRankCache ttlValue[[]model.ModelRankItem]

func invalidateModelTelemetryCache() {
	modelHealthCache.invalidate()
	modelRankCache.invalidate()
}
