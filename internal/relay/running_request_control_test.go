package relay

import (
	"context"
	"testing"
	"time"
)

func TestRunningRequestCancelFlow(t *testing.T) {
	resetRequestStateForTest()
	t.Cleanup(resetRequestStateForTest)

	state := newRequestState("test-model", "test-endpoint", 1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	state.BindCancel(func() {
		// Re-entering the registry proves the callback is invoked outside its lock.
		_ = GetRequestStateSnapshotForUser(1, false)
		cancel()
	})

	// 验证挂起 ctx 能被 CancelRunningRequest 终止
	err := CancelRunningRequest(state.ID, state.StartedAt)
	if err != nil {
		t.Fatalf("unexpected CancelRunningRequest error: %v", err)
	}

	select {
	case <-ctx.Done():
		// ok, context successfully canceled
	default:
		t.Fatalf("expected context to be canceled")
	}

	// 重复取消幂等成功，不 panic
	err = CancelRunningRequest(state.ID, state.StartedAt)
	if err != nil {
		t.Fatalf("repeat CancelRunningRequest should return nil, got: %v", err)
	}

	// 锁外调用快照防死锁验证
	snapshot := GetRequestStateSnapshotForUser(1, false)
	if len(snapshot) == 0 {
		t.Fatalf("expected snapshot to return running request")
	}
}

func TestRunningRequestCancelStaleAndFinished(t *testing.T) {
	resetRequestStateForTest()
	t.Cleanup(resetRequestStateForTest)

	state := newRequestState("test-model", "test-endpoint", 1, 1)
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	state.BindCancel(cancel)

	// 不存在的 ID
	err := CancelRunningRequest(99999, state.StartedAt)
	if err != ErrRunningRequestNotFound {
		t.Fatalf("expected ErrRunningRequestNotFound, got: %v", err)
	}

	// 时间戳不匹配 (stale time)
	err = CancelRunningRequest(state.ID, state.StartedAt.Add(time.Second))
	if err != ErrRunningRequestConflict {
		t.Fatalf("expected ErrRunningRequestConflict for stale time, got: %v", err)
	}

	// 正常成功完成请求，不是取消，终态应为 success，cancel 应被清除
	state.markSuccess()
	if state.Status != "success" {
		t.Fatalf("expected status success, got %s", state.Status)
	}
	if state.cancel != nil {
		t.Fatalf("expected cancel func to be cleared upon terminal state")
	}

	// 已终态请求再调用 CancelRunningRequest 应返回 conflict
	err = CancelRunningRequest(state.ID, state.StartedAt)
	if err != ErrRunningRequestConflict {
		t.Fatalf("expected ErrRunningRequestConflict for finished request, got: %v", err)
	}
}

func TestRunningRequestUnboundCancel(t *testing.T) {
	resetRequestStateForTest()
	t.Cleanup(resetRequestStateForTest)
	state := newRequestState("test-model", "messages", 1, 1)
	if err := CancelRunningRequest(state.ID, state.StartedAt); err != ErrRunningRequestConflict {
		t.Fatalf("unbound cancellation must fail, got %v", err)
	}
	if state.cancelCanceled {
		t.Fatal("rejected cancellation must not be recorded as accepted")
	}
}

func TestRequestStateInterventionIDBinding(t *testing.T) {
	resetRequestStateForTest()
	t.Cleanup(resetRequestStateForTest)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := SubscribeRequestState(ctx)
	defer UnsubscribeRequestState(updates)

	state := newRequestState("test-model", "test-endpoint", 1, 1)
	<-updates // initial running

	state.BindInterventionID("rescue-12345")
	update := <-updates
	if update.InterventionID != "rescue-12345" {
		t.Fatalf("expected intervention_id 'rescue-12345', got %s", update.InterventionID)
	}
}
