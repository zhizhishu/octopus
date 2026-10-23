package model

import "testing"

// 契约第四节第 1 条：状态机只允许 processing/completed/failed 三个值。
//
// 这条用例守的是"不许发明第四个状态"：客户端按字符串判断状态，任何新值都是
// 一个客户端必须处理而多数人不会处理的分支。
func TestImageTaskStatusMachineHasExactlyThreeValues(t *testing.T) {
	allowed := []string{ImageTaskStatusProcessing, ImageTaskStatusCompleted, ImageTaskStatusFailed}
	if len(allowed) != 3 {
		t.Fatalf("image task status set must have exactly three values, got %d", len(allowed))
	}
	want := map[string]bool{"processing": true, "completed": true, "failed": true}
	for _, status := range allowed {
		if !want[status] {
			t.Fatalf("unexpected status %q in the contract set", status)
		}
		if !IsImageTaskStatusAllowed(status) {
			t.Fatalf("IsImageTaskStatusAllowed(%q) = false, want true", status)
		}
	}
	for _, forbidden := range []string{"pending", "queued", "cancelled", "canceled", "running", "", "PROCESSING"} {
		if IsImageTaskStatusAllowed(forbidden) {
			t.Fatalf("IsImageTaskStatusAllowed(%q) = true; only the three contract values are allowed", forbidden)
		}
	}
}

// 契约第四节第 1 条的后半句：非法值写进 create 必须被归一（不能留在库里当第四态），
// 而终态判定只认 completed/failed。
func TestNormalizeImageTaskStatusFallsBackToProcessing(t *testing.T) {
	for _, status := range []string{ImageTaskStatusProcessing, ImageTaskStatusCompleted, ImageTaskStatusFailed} {
		if got := NormalizeImageTaskStatus(status); got != status {
			t.Fatalf("NormalizeImageTaskStatus(%q) = %q, want unchanged", status, got)
		}
	}
	for _, bad := range []string{"", "pending", "DONE", "cancelled"} {
		if got := NormalizeImageTaskStatus(bad); got != ImageTaskStatusProcessing {
			t.Fatalf("NormalizeImageTaskStatus(%q) = %q, want %q", bad, got, ImageTaskStatusProcessing)
		}
	}

	if IsImageTaskTerminal(ImageTaskStatusProcessing) {
		t.Fatal("processing must not be terminal")
	}
	for _, status := range []string{ImageTaskStatusCompleted, ImageTaskStatusFailed} {
		if !IsImageTaskTerminal(status) {
			t.Fatalf("%q must be terminal", status)
		}
	}
}

// 表名固定：迁移与运行时都按这个名字找表，GORM 的复数化规则一变就错位。
func TestImageTaskTableNameIsPinned(t *testing.T) {
	if got := (ImageTask{}).TableName(); got != "image_tasks" {
		t.Fatalf("TableName() = %q, want image_tasks", got)
	}
}
