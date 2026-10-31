package balancer

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// SessionEntry 会话保持条目
type SessionEntry struct {
	ChannelID    int
	ChannelKeyID int
	Timestamp    time.Time
}

// 全局会话存储
var globalSession sync.Map // key: string -> value: *SessionEntry

// maxSessionEntries bounds the in-memory sticky store. Every client session now gets an
// entry (see DefaultSessionAffinityTTL), so a busy instance could otherwise grow the map
// until the 24h maintenance sweep. Past the cap a write triggers a trim: entries older
// than DefaultSessionAffinityTTL go first, then the oldest-looking remainder until the
// store is back under the low-water mark. Dropping an entry is harmless — that
// conversation just re-picks a channel on its next turn. Var so tests can shrink it.
var maxSessionEntries int64 = 200_000

var (
	sessionEntryCount atomic.Int64 // approximate live entry count (Store adds, Delete subtracts)
	sessionTrimming   atomic.Bool  // single-flight guard for trimSessionStore
)

// sessionKey 生成会话键：apiKeyID:requestModel[:clientSessionKey]
func sessionKey(apiKeyID int, requestModel, clientSessionKey string) string {
	if clientSessionKey != "" {
		return fmt.Sprintf("%d:%s:%s", apiKeyID, requestModel, clientSessionKey)
	}
	return fmt.Sprintf("%d:%s", apiKeyID, requestModel)
}

// GetSticky 获取粘性通道（ttl 内有效）
// ttl 由 Group.SessionKeepTime 决定，返回 nil 表示无有效会话
func GetSticky(apiKeyID int, requestModel string, ttl time.Duration) *SessionEntry {
	return GetStickyWithSessionKey(apiKeyID, requestModel, "", ttl)
}

// GetStickyWithSessionKey 获取客户端会话级粘性通道。clientSessionKey 为空时保持旧行为。
func GetStickyWithSessionKey(apiKeyID int, requestModel, clientSessionKey string, ttl time.Duration) *SessionEntry {
	key := sessionKey(apiKeyID, requestModel, clientSessionKey)
	v, ok := globalSession.Load(key)
	if !ok {
		return nil
	}
	entry := v.(*SessionEntry)

	if time.Since(entry.Timestamp) > ttl {
		// 过期，惰性清除
		deleteSessionEntry(key)
		return nil
	}

	return entry
}

// SetSticky 写入/更新粘性记录
func SetSticky(apiKeyID int, requestModel string, channelID, keyID int) {
	SetStickyWithSessionKey(apiKeyID, requestModel, "", channelID, keyID)
}

// SetStickyWithSessionKey 写入/更新客户端会话级粘性记录。clientSessionKey 为空时保持旧行为。
func SetStickyWithSessionKey(apiKeyID int, requestModel, clientSessionKey string, channelID, keyID int) {
	key := sessionKey(apiKeyID, requestModel, clientSessionKey)
	_, loaded := globalSession.Swap(key, &SessionEntry{
		ChannelID:    channelID,
		ChannelKeyID: keyID,
		Timestamp:    time.Now(),
	})
	if !loaded && sessionEntryCount.Add(1) > maxSessionEntries {
		trimSessionStore()
	}
}

func ClearStickyWithSessionKey(apiKeyID int, requestModel, clientSessionKey string) {
	key := sessionKey(apiKeyID, requestModel, clientSessionKey)
	deleteSessionEntry(key)
}

func deleteSessionEntry(key any) {
	if _, loaded := globalSession.LoadAndDelete(key); loaded {
		sessionEntryCount.Add(-1)
	}
}

// trimSessionStore brings the sticky store back under ~90% of maxSessionEntries.
// Single-flight: concurrent writers past the cap do not all range the map.
func trimSessionStore() {
	if !sessionTrimming.CompareAndSwap(false, true) {
		return
	}
	defer sessionTrimming.Store(false)
	SweepExpired(DefaultSessionAffinityTTL)
	lowWater := maxSessionEntries * 9 / 10
	if sessionEntryCount.Load() <= lowWater {
		return
	}
	// Still over: every entry is fresh. Drop the oldest ones. A full sort would be
	// O(n log n) on a hot write; instead evict everything older than the median-ish
	// cutoff found by halving the age window until we are under the low-water mark.
	window := DefaultSessionAffinityTTL / 2
	for window >= time.Second && sessionEntryCount.Load() > lowWater {
		SweepExpired(window)
		window /= 2
	}
	if sessionEntryCount.Load() > lowWater {
		// Pathological burst inside one second: drop arbitrary entries.
		globalSession.Range(func(k, _ any) bool {
			deleteSessionEntry(k)
			return sessionEntryCount.Load() > lowWater
		})
	}
}

// SweepExpired removes sticky entries not refreshed within olderThan and returns how many
// were dropped. GetSticky enforces the real per-call ttl on READ, but its lazy Delete only
// fires for a key that is looked up again — a conversation that simply ends is never looked
// up, so its entry lives in globalSession for the whole process lifetime. This periodic
// sweep reclaims those. olderThan must be a GENEROUS upper bound on any configured
// session-keep time; an entry wrongly swept just means the next request for that
// conversation re-picks a channel (harmless), so the bound is deliberately loose.
func SweepExpired(olderThan time.Duration) int {
	if olderThan <= 0 {
		return 0
	}
	cutoff := time.Now().Add(-olderThan)
	removed := 0
	globalSession.Range(func(k, v any) bool {
		entry, ok := v.(*SessionEntry)
		if !ok || entry.Timestamp.Before(cutoff) {
			if _, loaded := globalSession.LoadAndDelete(k); loaded {
				sessionEntryCount.Add(-1)
				removed++
			}
		}
		return true
	})
	return removed
}
