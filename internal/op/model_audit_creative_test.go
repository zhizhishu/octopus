package op

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

func seedCreative(t *testing.T, status string, pinned bool, created time.Time) int64 {
	t.Helper()
	row := model.ModelAuditCreative{
		RoundID: "r-test", ChannelID: 1, Model: "m", Kind: CreativeKindPelican,
		Prompt: CreativePelicanPrompt, Status: status, Pinned: pinned, CreatedAt: created,
	}
	if err := db.GetDB().Create(&row).Error; err != nil {
		t.Fatalf("seed creative: %v", err)
	}
	return row.ID
}

func creativeExists(t *testing.T, id int64) bool {
	t.Helper()
	var n int64
	db.GetDB().Model(&model.ModelAuditCreative{}).Where("id = ?", id).Count(&n)
	return n == 1
}

// 7 天清理：只删 7 天前、没收藏、已经出完的；收藏的一直留着。
func TestCreativePurgeKeepsPinnedAndRecent(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	now := time.Now()
	old := now.Add(-8 * 24 * time.Hour)
	oldDone := seedCreative(t, model.CreativeDone, false, old)
	oldInvalid := seedCreative(t, model.CreativeInvalid, false, old)
	oldPinned := seedCreative(t, model.CreativeDone, true, old)
	oldPending := seedCreative(t, model.CreativePending, false, old)
	recent := seedCreative(t, model.CreativeDone, false, now.Add(-6*24*time.Hour))

	n, err := ModelAuditCreativePurge(ctx, now)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 2 {
		t.Fatalf("purged %d, want 2", n)
	}
	if creativeExists(t, oldDone) || creativeExists(t, oldInvalid) {
		t.Fatalf("old unpinned items should be gone")
	}
	for name, id := range map[string]int64{"pinned": oldPinned, "pending": oldPending, "recent": recent} {
		if !creativeExists(t, id) {
			t.Fatalf("%s item must survive the purge", name)
		}
	}
}

func TestCreativeClearUnpinnedAndDelete(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	now := time.Now()
	a := seedCreative(t, model.CreativeDone, false, now)
	pinned := seedCreative(t, model.CreativeDone, true, now)
	running := seedCreative(t, model.CreativeRunning, false, now)

	if err := ModelAuditCreativeDelete(ctx, running); !errors.Is(err, ErrCreativeRunning) {
		t.Fatalf("running item must not be deleted, got %v", err)
	}
	n, err := ModelAuditCreativeClearUnpinned(ctx)
	if err != nil || n != 1 {
		t.Fatalf("clear unpinned = %d, %v; want 1", n, err)
	}
	if creativeExists(t, a) || !creativeExists(t, pinned) || !creativeExists(t, running) {
		t.Fatalf("clear touched the wrong rows")
	}
	if err := ModelAuditCreativePin(ctx, pinned, false); err != nil {
		t.Fatalf("unpin: %v", err)
	}
	if err := ModelAuditCreativeDelete(ctx, pinned); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if creativeExists(t, pinned) {
		t.Fatalf("item should be deleted")
	}
}

func TestCreativeEnqueueChargesDailyBudget(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	ch := ensureAuditChannel(t, "出图渠道")
	setModelAuditBudgetLimit(t, 3)
	now := time.Now()

	round, ids, err := ModelAuditCreativeEnqueue(ctx, CreativeKindPelican, "", []CreativeTarget{
		{ChannelID: ch.ID, Model: "m1"}, {ChannelID: ch.ID, Model: "m2"}, {ChannelID: ch.ID, Model: "m1"},
	}, now)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if round == "" || len(ids) != 2 {
		t.Fatalf("round=%q ids=%v, want 2 deduplicated rows", round, ids)
	}
	if used, _ := ModelAuditBudgetStatus(now); used != 2 {
		t.Fatalf("budget used = %d, want 2 (one per model)", used)
	}
	items, err := ModelAuditCreativeList(ctx, round)
	if err != nil || len(items) != 2 || items[0].Prompt != CreativePelicanPrompt || items[0].Status != model.CreativePending {
		t.Fatalf("items = %+v, %v", items, err)
	}
	rounds, err := ModelAuditCreativeRounds(ctx, 10)
	if err != nil || len(rounds) != 1 || rounds[0].RoundID != round || rounds[0].Total != 2 || rounds[0].Active != 2 || rounds[0].Kind != CreativeKindPelican {
		t.Fatalf("rounds = %+v, %v", rounds, err)
	}
	// 只剩 1 次额度，这一轮要 2 次：一行都不写。
	_, _, err = ModelAuditCreativeEnqueue(ctx, CreativeKindCustom, "画一只猫", []CreativeTarget{
		{ChannelID: ch.ID, Model: "m3"}, {ChannelID: ch.ID, Model: "m4"},
	}, now)
	if !errors.Is(err, ErrCreativeBudget) {
		t.Fatalf("over budget must be refused, got %v", err)
	}
	var total int64
	db.GetDB().Model(&model.ModelAuditCreative{}).Count(&total)
	if total != 2 {
		t.Fatalf("rows after refused round = %d, want 2", total)
	}
	if _, _, err := ModelAuditCreativeEnqueue(ctx, CreativeKindCustom, "   ", []CreativeTarget{{ChannelID: ch.ID, Model: "m"}}, now); err == nil {
		t.Fatalf("empty custom prompt must be refused")
	}
}

func TestExtractCreative(t *testing.T) {
	svg := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><circle r="4"/></svg>`
	cases := []struct {
		name, reply, format string
		ok                  bool
	}{
		{"svg fence", "Here you go:\n```svg\n" + svg + "\n```\nEnjoy", "svg", true},
		{"xml fence", "```xml\n<?xml version=\"1.0\"?>\n" + svg + "\n```", "svg", true},
		{"html fence", "```html\n<!DOCTYPE html><html><body><svg></svg></body></html>\n```", "html", true},
		{"bare svg", "Sure! " + svg + " that's it", "svg", true},
		{"bare html page", "<!doctype html>\n<html><head></head><body>hi</body></html>", "html", true},
		{"upper case", "<SVG viewBox='0 0 1 1'></SVG>", "svg", true},
		{"no svg", "I cannot draw that.", "", false},
		{"unclosed svg", "<svg><circle/>", "", false},
		{"empty", "   ", "", false},
	}
	for _, c := range cases {
		format, content, ok, reason := ExtractCreative(c.reply)
		if ok != c.ok || format != c.format {
			t.Fatalf("%s: got format=%q ok=%v reason=%q", c.name, format, ok, reason)
		}
		if ok && content == "" {
			t.Fatalf("%s: empty content", c.name)
		}
		if !ok && reason == "" {
			t.Fatalf("%s: a failure needs a reason", c.name)
		}
	}
	if _, content, _, _ := ExtractCreative("x\n```svg\n" + svg + "\n```"); content != svg {
		t.Fatalf("svg fence content = %q", content)
	}
	huge := "<svg>" + strings.Repeat("a", model.CreativeMaxContentBytes) + "</svg>"
	if _, _, ok, reason := ExtractCreative(huge); ok || !strings.Contains(reason, "256") {
		t.Fatalf("oversized svg must be refused with the limit in the reason, got ok=%v %q", ok, reason)
	}
}

func TestCreativeTruncateKeepsUTF8(t *testing.T) {
	s := strings.Repeat("鹈", model.CreativeMaxContentBytes) // 3 bytes each
	out := creativeTruncate(s)
	if len(out) > model.CreativeMaxContentBytes || !strings.HasPrefix(s, out) || !utf8ValidString(out) {
		t.Fatalf("truncate broke the string: len=%d", len(out))
	}
}

func utf8ValidString(s string) bool { return strings.ToValidUTF8(s, "\uFFFD") == s }
