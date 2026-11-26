package op

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

func seedCreative(t *testing.T, status string, pinned bool, created time.Time) int64 {
	t.Helper()
	return seedCreativeInRound(t, "r-test", status, pinned, created)
}

func seedCreativeInRound(t *testing.T, roundID, status string, pinned bool, created time.Time) int64 {
	t.Helper()
	row := model.ModelAuditCreative{
		RoundID: roundID, ChannelID: 1, Model: "m", Kind: CreativeKindPelican,
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

// 多块时取最后一个完整闭合的代码块：模型经常先给草稿再给定稿。
func TestExtractCreativeTakesLastCompleteBlock(t *testing.T) {
	draft := `<svg viewBox="0 0 10 10"><circle r="1"/></svg>`
	final := `<svg viewBox="0 0 20 20"><rect width="20" height="20"/></svg>`

	twoBlocks := "草稿：\n```svg\n" + draft + "\n```\n定稿：\n```svg\n" + final + "\n```\n"
	if _, content, ok, reason := ExtractCreative(twoBlocks); !ok || content != final {
		t.Fatalf("two blocks: got %q ok=%v reason=%q, want the last block", content, ok, reason)
	}

	// 最后一个块没闭合：跳过它，用前面那个完整闭合的块。
	truncated := "```svg\n" + draft + "\n```\n```svg\n<svg viewBox=\"0 0 20 20\"><rect"
	if _, content, ok, reason := ExtractCreative(truncated); !ok || content != draft {
		t.Fatalf("trailing unclosed block: got %q ok=%v reason=%q, want the earlier complete block", content, ok, reason)
	}

	// 后面那块是纯文字（认不出图）时，也不能把前面那张图弄丢。
	withProse := "```svg\n" + draft + "\n```\n```text\n以上是最终结果。\n```\n"
	if _, content, ok, _ := ExtractCreative(withProse); !ok || content != draft {
		t.Fatalf("prose block last: got %q ok=%v, want the svg block", content, ok)
	}
}

// 嵌套 <svg> 取最外层完整闭合的那一个；两个并列的 svg 不能被并成一个。
func TestExtractCreativeOutermostSVG(t *testing.T) {
	nested := `<svg viewBox="0 0 100 100"><g><svg viewBox="0 0 10 10"><circle r="1"/></svg></g><rect width="100" height="100"/></svg>`
	if _, content, ok, reason := ExtractCreative("```svg\n" + nested + "\n```"); !ok || content != nested {
		t.Fatalf("nested svg: got %q ok=%v reason=%q, want the outer element", content, ok, reason)
	}

	first := `<svg viewBox="0 0 5 5"><circle r="2"/></svg>`
	second := `<svg viewBox="0 0 9 9"><rect width="9" height="9"/></svg>`
	if _, content, ok, _ := ExtractCreative("画了两个：" + first + " 还有 " + second); !ok || content != first {
		t.Fatalf("two separate svgs: got %q ok=%v, must not be merged", content, ok)
	}

	// 外框没闭合时不能把内层之外的东西都当一张图：退回内层那张完整闭合的。
	inner := `<svg viewBox="0 0 3 3"><circle r="1"/></svg>`
	if _, content, ok, _ := ExtractCreative("<svg viewBox=\"0 0 9 9\">" + inner); !ok || content != inner {
		t.Fatalf("unclosed outer svg: got %q ok=%v, want the inner complete one", content, ok)
	}
}

// html 代码块要有真正的 HTML 结构：裸 `<` 不算。
func TestExtractCreativeHTMLNeedsStructure(t *testing.T) {
	bare := "```html\n5 < 10 而且 x > 3，我画不出来\n```"
	if _, content, ok, reason := ExtractCreative(bare); ok || content != "" || reason == "" {
		t.Fatalf("a bare `<` must not pass as HTML, got %q ok=%v reason=%q", content, ok, reason)
	}

	for _, reply := range []string{
		"```html\n<!doctype html><html><body><p>hi</p></body></html>\n```",
		"```html\n<div class=\"scene\"><p>鹈鹕</p></div>\n```",
	} {
		if format, content, ok, reason := ExtractCreative(reply); !ok || format != "html" || content == "" {
			t.Fatalf("%q: got format=%q ok=%v reason=%q", reply, format, ok, reason)
		}
	}

	// 标成 html 但只交了 SVG：按 SVG 收，不丢掉这张图。
	if format, _, ok, _ := ExtractCreative("```html\n<svg viewBox=\"0 0 1 1\"><circle r=\"1\"/></svg>\n```"); !ok || format != "svg" {
		t.Fatalf("an svg inside an html fence must still be usable, got format=%q ok=%v", format, ok)
	}
}

// 提取是原样拷贝：viewBox 一个数字都不能被裁掉或改写。
func TestExtractCreativeKeepsViewBox(t *testing.T) {
	const viewBox = `viewBox="0 0 500 450"`
	svg := `<svg xmlns="http://www.w3.org/2000/svg" ` + viewBox + ` width="500" height="450"><rect width="500" height="450"/></svg>`
	if _, content, ok, _ := ExtractCreative("```svg\n" + svg + "\n```"); !ok || content != svg {
		t.Fatalf("svg content = %q, want it copied verbatim", content)
	}
	if !strings.Contains(svg, viewBox) {
		t.Fatalf("test fixture lost the viewBox")
	}
}

// 历史轮次：最近 limit 轮之外，收藏的老轮次要一并列出来，否则收藏的图找不回来。
func TestCreativeRoundsIncludePinnedOldRounds(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	now := time.Now()
	oldPlain := "r-old-plain"
	oldPinned := "r-old-pinned"
	// 先插老的：id 递增，最后插入的轮次 last_id 最大。
	seedCreativeInRound(t, oldPlain, model.CreativeDone, false, now.Add(-40*24*time.Hour))
	seedCreativeInRound(t, oldPinned, model.CreativeDone, true, now.Add(-30*24*time.Hour))
	for i := 0; i < 15; i++ {
		seedCreativeInRound(t, fmt.Sprintf("r-new-%02d", i), model.CreativeDone, false, now.Add(time.Duration(i)*time.Minute))
		// 新一轮里也有一张收藏：这一轮不能因为两段都命中就重复出现。
		if i == 13 {
			seedCreativeInRound(t, "r-new-13", model.CreativeDone, true, now.Add(13*time.Minute))
		}
	}

	rounds, err := ModelAuditCreativeRounds(ctx, 10)
	if err != nil {
		t.Fatalf("rounds: %v", err)
	}
	seen := make(map[string]int, len(rounds))
	for _, r := range rounds {
		seen[r.RoundID]++
	}
	if len(rounds) != 11 {
		t.Fatalf("rounds = %d (%v), want the latest 10 plus the pinned old round", len(rounds), seen)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("round %q listed %d times, want 1", id, n)
		}
	}
	if rounds[0].RoundID != "r-new-14" {
		t.Fatalf("newest round must be first, got %q", rounds[0].RoundID)
	}
	last := rounds[len(rounds)-1]
	if last.RoundID != oldPinned || last.Pinned != 1 {
		t.Fatalf("the pinned old round must be listed last, got %+v", last)
	}
	if _, listed := seen[oldPlain]; listed {
		t.Fatalf("an unpinned old round must stay out of the list")
	}
	if _, listed := seen["r-new-13"]; !listed {
		t.Fatalf("the recent round with a pinned item must still be listed")
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
