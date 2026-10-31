package balancer

import (
	"fmt"
	"sync"
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
		globalSession.Delete(key)
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
	globalSession.Store(key, &SessionEntry{
		ChannelID:    channelID,
		ChannelKeyID: keyID,
		Timestamp:    time.Now(),
	})
}

func ClearStickyWithSessionKey(apiKeyID int, requestModel, clientSessionKey string) {
	key := sessionKey(apiKeyID, requestModel, clientSessionKey)
	globalSession.Delete(key)
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
			globalSession.Delete(k)
			removed++
		}
		return true
	})
	return removed
}
