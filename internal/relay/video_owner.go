package relay

import (
	"strings"
	"sync"
	"time"
)

// Video generation on the Agnes-style upstream is asynchronous: a create call
// returns a task/video id, and the client polls a GET endpoint until the video
// is ready. The poll MUST return to the exact channel+key that created the task
// (the task lives on that upstream account), so we remember which channel+key
// owns each id. This mirrors the responses-session owner map but is in-memory
// only: video tasks are short-lived (minutes) and a restart just forces the
// client to re-create the task, which is acceptable.
const (
	defaultVideoTaskTTL    = 2 * time.Hour
	videoTaskPruneInterval = time.Minute
)

type videoTaskEntry struct {
	channelID    int
	channelKeyID int
	model        string
	ownerTokenID int
	ownerUserID  int
	expiresAt    time.Time
}

var videoTaskStore = struct {
	sync.Mutex
	items       map[string]videoTaskEntry
	lastPruneAt time.Time
}{
	items: make(map[string]videoTaskEntry),
}

// recordVideoTaskOwner binds every id (task_id / video_id / id) returned by a
// successful create to the channel+key that served it, scoped to the requesting
// token/user so another tenant cannot poll a task they did not create.
func recordVideoTaskOwner(ids []string, channelID, channelKeyID int, model string, ownerTokenID, ownerUserID int) {
	if channelID == 0 || channelKeyID == 0 || len(ids) == 0 {
		return
	}
	now := time.Now()
	entry := videoTaskEntry{
		channelID:    channelID,
		channelKeyID: channelKeyID,
		model:        model,
		ownerTokenID: ownerTokenID,
		ownerUserID:  ownerUserID,
		expiresAt:    now.Add(defaultVideoTaskTTL),
	}
	videoTaskStore.Lock()
	defer videoTaskStore.Unlock()
	maybePruneVideoTasksLocked(now)
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		videoTaskStore.items[id] = entry
	}
}

func videoTaskOwner(id string) (videoTaskEntry, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return videoTaskEntry{}, false
	}
	now := time.Now()
	videoTaskStore.Lock()
	defer videoTaskStore.Unlock()
	maybePruneVideoTasksLocked(now)
	entry, ok := videoTaskStore.items[id]
	if !ok {
		return videoTaskEntry{}, false
	}
	if now.After(entry.expiresAt) {
		delete(videoTaskStore.items, id)
		return videoTaskEntry{}, false
	}
	return entry, true
}

func maybePruneVideoTasksLocked(now time.Time) {
	if !videoTaskStore.lastPruneAt.IsZero() && now.Sub(videoTaskStore.lastPruneAt) < videoTaskPruneInterval {
		return
	}
	videoTaskStore.lastPruneAt = now
	for id, entry := range videoTaskStore.items {
		if now.After(entry.expiresAt) {
			delete(videoTaskStore.items, id)
		}
	}
}

// videoTaskOwnerMatches applies the same fail-closed tenant check as the
// responses-session owner map: a task with a recorded identity may only be
// polled by the same token (else the same user); a task with no recorded
// identity (0/0) is treated as unrestricted for backward compatibility.
func videoTaskOwnerMatches(entry videoTaskEntry, reqTokenID, reqUserID int) bool {
	if entry.ownerTokenID == 0 && entry.ownerUserID == 0 {
		return true
	}
	if entry.ownerTokenID > 0 {
		return entry.ownerTokenID == reqTokenID
	}
	return entry.ownerUserID == reqUserID
}
