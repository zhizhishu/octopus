package relay

import (
	"testing"
	"time"
)

func TestExtractVideoTaskIDs(t *testing.T) {
	// task_id appears twice (also as id), video_id once -> two unique ids.
	ids := extractVideoTaskIDs([]byte(`{"id":"task_1","task_id":"task_1","video_id":"video_9","status":"queued"}`))
	if len(ids) != 2 {
		t.Fatalf("expected 2 unique ids, got %d: %v", len(ids), ids)
	}
	want := map[string]bool{"task_1": true, "video_9": true}
	for _, id := range ids {
		if !want[id] {
			t.Fatalf("unexpected id %q in %v", id, ids)
		}
	}

	if got := extractVideoTaskIDs([]byte(`not json`)); got != nil {
		t.Fatalf("expected nil for invalid json, got %v", got)
	}
	if got := extractVideoTaskIDs([]byte(`{"status":"queued"}`)); got != nil {
		t.Fatalf("expected nil when no ids present, got %v", got)
	}
}

func TestVideoTaskOwnerRoundTrip(t *testing.T) {
	videoTaskStore.Lock()
	videoTaskStore.items = make(map[string]videoTaskEntry)
	videoTaskStore.lastPruneAt = time.Time{}
	videoTaskStore.Unlock()

	recordVideoTaskOwner([]string{"video_abc", "task_abc"}, 7, 3, "agnes-video-v2.0", 42, 100)

	entry, ok := videoTaskOwner("video_abc")
	if !ok {
		t.Fatal("expected owner for video_abc")
	}
	if entry.channelID != 7 || entry.channelKeyID != 3 || entry.model != "agnes-video-v2.0" {
		t.Fatalf("unexpected entry: %+v", entry)
	}
	// alias id resolves to the same owner (create returned multiple ids).
	if e2, ok := videoTaskOwner("task_abc"); !ok || e2.channelID != 7 {
		t.Fatalf("alias id lookup failed: %+v ok=%v", e2, ok)
	}
	if _, ok := videoTaskOwner("nope"); ok {
		t.Fatal("expected miss for unknown id")
	}
}

func TestVideoTaskOwnerExpires(t *testing.T) {
	videoTaskStore.Lock()
	videoTaskStore.items = map[string]videoTaskEntry{
		"stale": {channelID: 1, channelKeyID: 1, expiresAt: time.Now().Add(-time.Minute)},
	}
	videoTaskStore.lastPruneAt = time.Time{}
	videoTaskStore.Unlock()

	if _, ok := videoTaskOwner("stale"); ok {
		t.Fatal("expected expired entry to be a miss")
	}
}

func TestVideoTaskOwnerMatches(t *testing.T) {
	if !videoTaskOwnerMatches(videoTaskEntry{}, 1, 2) {
		t.Fatal("0/0 owner should be unrestricted")
	}
	tokenScoped := videoTaskEntry{ownerTokenID: 5}
	if !videoTaskOwnerMatches(tokenScoped, 5, 0) {
		t.Fatal("same token should match")
	}
	if videoTaskOwnerMatches(tokenScoped, 6, 0) {
		t.Fatal("different token must not match")
	}
	userScoped := videoTaskEntry{ownerUserID: 9}
	if !videoTaskOwnerMatches(userScoped, 0, 9) {
		t.Fatal("same user should match")
	}
	if videoTaskOwnerMatches(userScoped, 0, 10) {
		t.Fatal("different user must not match")
	}
}
