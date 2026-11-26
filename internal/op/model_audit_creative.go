package op

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
	"gorm.io/gorm"
)

// 鹈鹕测试（创作题并排看）的数据层。
//
// 每个模型只发 1 次请求，走渠道检测的同一条出站链路（执行体在 handlers 的
// creative worker 里），并且**先扣每日检测额度**（ModelAuditBudgetReserve）再入队：
// 它和定时体检花的是同一份钱。结果只给人看，不打分、不进任何结论。

// 题目种类。
const (
	CreativeKindPelican = "pelican"
	CreativeKindHTML    = "html"
	CreativeKindCustom  = "custom"
)

// 固定题目原文。鹈鹕题就是社区里那道原题，一个字不改，图才能跟别处的对上。
const (
	CreativePelicanPrompt = "Generate an SVG of a pelican riding a bicycle"
	CreativeHTMLPrompt    = "Create a single self-contained HTML page (inline CSS and JavaScript, no external files or network requests) that shows an animated pelican riding a bicycle. Reply with the complete HTML document in one code block."
)

const (
	// creativeRetention 未收藏的图保留 7 天。
	creativeRetention = 7 * 24 * time.Hour
	// CreativeMaxTargets 一轮最多几个模型：每个都是一笔真实请求。
	CreativeMaxTargets = 12
	// CreativeMaxPromptRunes 自己写的题最长多少字。
	CreativeMaxPromptRunes = 4000
)

// ErrCreativeBudget 今天的检测额度不够这一轮。
var ErrCreativeBudget = errors.New("今天的检测额度不够了")

// CreativeTarget 一轮里的一个模型。
type CreativeTarget struct {
	ChannelID int    `json:"channel_id"`
	Model     string `json:"model"`
}

// CreativePromptFor 按种类给出要发的题目。custom 用用户写的。
func CreativePromptFor(kind, custom string) (string, error) {
	switch kind {
	case CreativeKindPelican:
		return CreativePelicanPrompt, nil
	case CreativeKindHTML:
		return CreativeHTMLPrompt, nil
	case CreativeKindCustom:
		p := strings.TrimSpace(custom)
		if p == "" {
			return "", errors.New("自己写的题不能是空的")
		}
		if utf8.RuneCountInString(p) > CreativeMaxPromptRunes {
			return "", fmt.Errorf("题目太长了（最多 %d 个字）", CreativeMaxPromptRunes)
		}
		return p, nil
	default:
		return "", fmt.Errorf("不认识的题目种类 %q", kind)
	}
}

func newCreativeRoundID(now time.Time) string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("r%s-%s", now.Format("20060102150405"), hex.EncodeToString(b[:]))
}

// ModelAuditCreativeEnqueue 发起一轮：校验 → 扣额度 → 每个模型一行 pending。
//
// 额度先扣后写：扣不到就一行都不写；扣到了但写库失败，这一点额度不退
// （账本只有「扣」没有「退」，宁可少用一点也不让账对不上）。
func ModelAuditCreativeEnqueue(ctx context.Context, kind, customPrompt string, targets []CreativeTarget, now time.Time) (string, []int64, error) {
	prompt, err := CreativePromptFor(strings.TrimSpace(kind), customPrompt)
	if err != nil {
		return "", nil, err
	}
	kind = strings.TrimSpace(kind)

	type resolved struct {
		target CreativeTarget
		name   string
	}
	clean := make([]resolved, 0, len(targets))
	seen := map[string]struct{}{}
	for _, t := range targets {
		t.Model = strings.TrimSpace(t.Model)
		if t.ChannelID <= 0 || t.Model == "" {
			continue
		}
		key := fmt.Sprintf("%d\x00%s", t.ChannelID, t.Model)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		ch, err := ChannelGet(t.ChannelID, ctx)
		if err != nil || ch == nil {
			return "", nil, fmt.Errorf("渠道 #%d 不存在", t.ChannelID)
		}
		// 只有聊天类渠道能回一段文字：图片/嵌入渠道发过去只会白扣一次额度。
		if !outbound.IsChatChannelType(ch.Type) {
			return "", nil, fmt.Errorf("渠道「%s」不是聊天类渠道，出不了 SVG/HTML", ch.Name)
		}
		clean = append(clean, resolved{target: t, name: ch.Name})
	}
	if len(clean) == 0 {
		return "", nil, errors.New("至少选一个模型")
	}
	if len(clean) > CreativeMaxTargets {
		return "", nil, fmt.Errorf("一轮最多 %d 个模型", CreativeMaxTargets)
	}

	ok, used, limit := ModelAuditBudgetReserve(len(clean), now)
	if !ok {
		return "", nil, fmt.Errorf("%w：已用 %d / %d 次，这一轮要 %d 次", ErrCreativeBudget, used, limit, len(clean))
	}

	roundID := newCreativeRoundID(now)
	ids := make([]int64, 0, len(clean))
	err = db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, r := range clean {
			row := model.ModelAuditCreative{
				RoundID:     roundID,
				ChannelID:   r.target.ChannelID,
				ChannelName: r.name,
				Model:       r.target.Model,
				Kind:        kind,
				Prompt:      prompt,
				Status:      model.CreativePending,
				CreatedAt:   now,
			}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
			ids = append(ids, row.ID)
		}
		return nil
	})
	if err != nil {
		return "", nil, fmt.Errorf("鹈鹕测试入队失败: %w", err)
	}
	return roundID, ids, nil
}

// CreativeRound 历史轮次列表的一行。
type CreativeRound struct {
	RoundID   string    `json:"round_id"`
	Kind      string    `json:"kind"`
	Prompt    string    `json:"prompt"`
	CreatedAt time.Time `json:"created_at"`
	Total     int       `json:"total"`
	Active    int       `json:"active"`
	Pinned    int       `json:"pinned"`
}

// creativePinnedRoundCap 额外带出来的收藏轮次上限：收藏的轮次不会被 7 天清理删掉，
// 数量会一直长，所以给个上限，免得列表越拉越长。
const creativePinnedRoundCap = 100

// ModelAuditCreativeRounds 最近的轮次（新的在前）。
//
// 取「最近 limit 轮」并上「任何有收藏的轮次」：收藏的老图不该因为轮次太老就从
// 列表里消失、找不到。两段按 last_id 排序去重后一起返回。
func ModelAuditCreativeRounds(ctx context.Context, limit int) ([]CreativeRound, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	type row struct {
		RoundID string
		LastID  int64
		Total   int
		Active  int
		Pinned  int
	}
	const aggSelect = "round_id, MAX(id) AS last_id, COUNT(*) AS total, " +
		"SUM(CASE WHEN status IN ('pending','running') THEN 1 ELSE 0 END) AS active, " +
		"SUM(CASE WHEN pinned THEN 1 ELSE 0 END) AS pinned"

	var recent []row
	if err := db.GetDB().WithContext(ctx).Model(&model.ModelAuditCreative{}).
		Select(aggSelect).Group("round_id").Order("last_id DESC").Limit(limit).
		Scan(&recent).Error; err != nil {
		return nil, err
	}
	var pinned []row
	if err := db.GetDB().WithContext(ctx).Model(&model.ModelAuditCreative{}).
		Select(aggSelect).Group("round_id").
		Having("SUM(CASE WHEN pinned THEN 1 ELSE 0 END) > 0").
		Order("last_id DESC").Limit(creativePinnedRoundCap).
		Scan(&pinned).Error; err != nil {
		return nil, err
	}

	merged := make(map[string]row, len(recent)+len(pinned))
	order := make([]string, 0, len(recent)+len(pinned))
	for _, list := range [][]row{recent, pinned} {
		for _, r := range list {
			if _, dup := merged[r.RoundID]; dup {
				continue
			}
			merged[r.RoundID] = r
			order = append(order, r.RoundID)
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return merged[order[i]].LastID > merged[order[j]].LastID })

	out := make([]CreativeRound, 0, len(order))
	for _, roundID := range order {
		r := merged[roundID]
		var first model.ModelAuditCreative
		if err := db.GetDB().WithContext(ctx).Select("kind, prompt, created_at").
			Where("round_id = ?", r.RoundID).Order("id ASC").First(&first).Error; err != nil {
			continue
		}
		out = append(out, CreativeRound{
			RoundID: r.RoundID, Kind: first.Kind, Prompt: first.Prompt, CreatedAt: first.CreatedAt,
			Total: r.Total, Active: r.Active, Pinned: r.Pinned,
		})
	}
	return out, nil
}

// ModelAuditCreativeList 一轮里的全部图（按发起顺序）。
func ModelAuditCreativeList(ctx context.Context, roundID string) ([]model.ModelAuditCreative, error) {
	roundID = strings.TrimSpace(roundID)
	if roundID == "" {
		return nil, errors.New("缺少轮次")
	}
	var items []model.ModelAuditCreative
	err := db.GetDB().WithContext(ctx).Where("round_id = ?", roundID).Order("id ASC").Find(&items).Error
	return items, err
}

// ModelAuditCreativePin 收藏 / 取消收藏。
func ModelAuditCreativePin(ctx context.Context, id int64, pinned bool) error {
	res := db.GetDB().WithContext(ctx).Model(&model.ModelAuditCreative{}).Where("id = ?", id).Update("pinned", pinned)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		var n int64
		db.GetDB().WithContext(ctx).Model(&model.ModelAuditCreative{}).Where("id = ?", id).Count(&n)
		if n == 0 {
			return gorm.ErrRecordNotFound
		}
	}
	return nil
}

// ErrCreativeRunning 这张图正在出，删不了。
var ErrCreativeRunning = errors.New("这张图正在出，等它出完再删")

// ModelAuditCreativeDelete 删一张。还在排队的可以删（等于不出了）；正在出的不删。
func ModelAuditCreativeDelete(ctx context.Context, id int64) error {
	var row model.ModelAuditCreative
	if err := db.GetDB().WithContext(ctx).Select("id, status").Where("id = ?", id).First(&row).Error; err != nil {
		return err
	}
	if row.Status == model.CreativeRunning {
		return ErrCreativeRunning
	}
	res := db.GetDB().WithContext(ctx).
		Where("id = ? AND status <> ?", id, model.CreativeRunning).Delete(&model.ModelAuditCreative{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrCreativeRunning
	}
	return nil
}

// ModelAuditCreativeClearUnpinned 删掉所有没收藏、已经出完的图。还在排队/在出的不动。
func ModelAuditCreativeClearUnpinned(ctx context.Context) (int64, error) {
	res := db.GetDB().WithContext(ctx).
		Where("pinned = ? AND status NOT IN ?", false, []string{model.CreativePending, model.CreativeRunning}).
		Delete(&model.ModelAuditCreative{})
	return res.RowsAffected, res.Error
}

// ModelAuditCreativePurge 自动清理：7 天前、没收藏、已经出完的图。
func ModelAuditCreativePurge(ctx context.Context, now time.Time) (int64, error) {
	cutoff := now.Add(-creativeRetention)
	res := db.GetDB().WithContext(ctx).
		Where("pinned = ? AND created_at < ? AND status NOT IN ?", false, cutoff,
			[]string{model.CreativePending, model.CreativeRunning}).
		Delete(&model.ModelAuditCreative{})
	return res.RowsAffected, res.Error
}

// ModelAuditCreativeClaimNext 取一张待出的图并占成 running。没有就返回 (nil, nil)。
func ModelAuditCreativeClaimNext(ctx context.Context) (*model.ModelAuditCreative, error) {
	var row model.ModelAuditCreative
	err := db.GetDB().WithContext(ctx).Where("status = ?", model.CreativePending).Order("id ASC").First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	claim := db.GetDB().WithContext(ctx).Model(&model.ModelAuditCreative{}).
		Where("id = ? AND status = ?", row.ID, model.CreativePending).
		Update("status", model.CreativeRunning)
	if claim.Error != nil {
		return nil, claim.Error
	}
	if claim.RowsAffected == 0 {
		return nil, nil // 被删了或被别人拿走
	}
	row.Status = model.CreativeRunning
	return &row, nil
}

// CreativeOutcome 一张图出完后要写回的东西。
type CreativeOutcome struct {
	Status       string
	Format       string
	Content      string
	Raw          string
	Error        string
	DurationMs   int64
	InputTokens  int
	OutputTokens int
}

// ModelAuditCreativeFinish 把结果写回。只写还在 running 的行：中途被删了就算了。
func ModelAuditCreativeFinish(ctx context.Context, id int64, out CreativeOutcome, now time.Time) error {
	return db.GetDB().WithContext(ctx).Model(&model.ModelAuditCreative{}).
		Where("id = ? AND status = ?", id, model.CreativeRunning).
		Updates(map[string]any{
			"status":        out.Status,
			"format":        out.Format,
			"content":       creativeTruncate(out.Content),
			"raw":           creativeTruncate(out.Raw),
			"error":         out.Error,
			"duration_ms":   out.DurationMs,
			"input_tokens":  out.InputTokens,
			"output_tokens": out.OutputTokens,
			"finished_at":   now,
		}).Error
}

// ModelAuditCreativeReclaim 进程起来时把上次没出完的图收尾成失败（不重发：可能已经花过钱）。
func ModelAuditCreativeReclaim(ctx context.Context, now time.Time) (int64, error) {
	res := db.GetDB().WithContext(ctx).Model(&model.ModelAuditCreative{}).
		Where("status = ?", model.CreativeRunning).
		Updates(map[string]any{
			"status":      model.CreativeFailed,
			"error":       "执行中断（服务重启），可能已经发过请求；为避免重复花钱不自动重发",
			"finished_at": now,
		})
	return res.RowsAffected, res.Error
}

// creativeTruncate 按字节截到上限，且不切坏 UTF-8。
func creativeTruncate(s string) string {
	if len(s) <= model.CreativeMaxContentBytes {
		return s
	}
	cut := model.CreativeMaxContentBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

var (
	creativeFenceRe = regexp.MustCompile("(?s)```[ \\t]*([A-Za-z0-9_+-]*)[^\\n]*\\n(.*?)```")
	creativeHTMLRe  = regexp.MustCompile(`(?is)(<!doctype\s+html.*?</html\s*>|<html[\s>].*?</html\s*>)`)
	// 配对 <svg> 用：开标签、闭标签分开找，再按嵌套层数配对。一条 `.*?</svg>`
	// 非贪婪只会配到内层，贪婪又会把两个独立的 svg 并成一个，都不行。
	creativeSVGOpenRe  = regexp.MustCompile(`(?i)<svg[\s/>]`)
	creativeSVGCloseRe = regexp.MustCompile(`(?i)</svg\s*>`)
)

// creativeBlockTags 成对闭合的块级标签：只有 <div>…</div> 这种才算「有 HTML 结构」，
// 裸一个 `<`（比如模型在解释 5 < 10）不算。
var creativeBlockTags = []string{"html", "head", "body", "div", "section", "main", "article", "figure", "canvas", "table", "ul", "ol", "p"}

var creativeBlockTagRe = func() [][2]*regexp.Regexp {
	out := make([][2]*regexp.Regexp, 0, len(creativeBlockTags))
	for _, tag := range creativeBlockTags {
		out = append(out, [2]*regexp.Regexp{
			regexp.MustCompile(`(?is)<` + tag + `[\s>]`),
			regexp.MustCompile(`(?is)</` + tag + `\s*>`),
		})
	}
	return out
}()

// creativeHasHTMLStructure 这一段里有没有成对闭合的块级标签。
func creativeHasHTMLStructure(s string) bool {
	for _, pair := range creativeBlockTagRe {
		if pair[0].MatchString(s) && pair[1].MatchString(s) {
			return true
		}
	}
	return false
}

type creativeSVGToken struct {
	start, end int
	close      bool
}

// creativeOutermostSVG 取最外层完整闭合的 <svg>…</svg>。
//
// 嵌套的 <svg>（外框里再画小图标）按层数配对到最外层的闭合标签；两个并列的
// <svg> 各自配对，不会被并成一个；前一个 <svg> 没闭合就跳过它接着找下一个。
func creativeOutermostSVG(s string) string {
	tokens := make([]creativeSVGToken, 0, 8)
	for _, m := range creativeSVGOpenRe.FindAllStringIndex(s, -1) {
		tokens = append(tokens, creativeSVGToken{start: m[0], end: m[1]})
	}
	for _, m := range creativeSVGCloseRe.FindAllStringIndex(s, -1) {
		tokens = append(tokens, creativeSVGToken{start: m[0], end: m[1], close: true})
	}
	if len(tokens) == 0 {
		return ""
	}
	sort.Slice(tokens, func(i, j int) bool { return tokens[i].start < tokens[j].start })
	for i := range tokens {
		if tokens[i].close {
			continue
		}
		depth := 0
		for j := i; j < len(tokens); j++ {
			if tokens[j].close {
				depth--
			} else {
				depth++
			}
			if depth == 0 {
				return s[tokens[i].start:tokens[j].end]
			}
		}
	}
	return ""
}

// ExtractCreative 从回复里认出要显示的 SVG 或 HTML。
//
// 认的顺序：先看代码块（```svg / ```html / ```xml，或者块里有 <svg）——模型多半
// 这么交；多块时从最后一块往前找，模型常常先写草稿再给最终版，没闭合的块本来就
// 配不上、直接跳过。再在正文里找最外层完整闭合的 <svg>…</svg>；最后找整页 HTML。
// 都找不到返回 ok=false（页面上显示「没返回合法 SVG」并给原文）。
// 超过 256KB 也算不合法：存不下，也不该塞进页面。
func ExtractCreative(reply string) (format, content string, ok bool, reason string) {
	text := strings.TrimSpace(reply)
	if text == "" {
		return "", "", false, "回复是空的"
	}
	pick := func(f, c string) (string, string, bool, string) {
		c = strings.TrimSpace(c)
		if len(c) > model.CreativeMaxContentBytes {
			return "", "", false, fmt.Sprintf("图太大了（%d KB，上限 256 KB）", len(c)/1024)
		}
		return f, c, true, ""
	}
	// fromBody 从一个代码块的正文里认出图；认不出来返回 ok=false 且 reason 为空。
	fromBody := func(lang, body string) (string, string, bool, string) {
		switch lang {
		case "svg":
			if svg := creativeOutermostSVG(body); svg != "" {
				return pick("svg", svg)
			}
		case "html", "htm":
			// 光含一个 `<` 不算 HTML：要整页形态或成对闭合的块级标签。
			if creativeHTMLRe.MatchString(body) || creativeHasHTMLStructure(body) {
				return pick("html", body)
			}
			if svg := creativeOutermostSVG(body); svg != "" {
				return pick("svg", svg)
			}
		default:
			if svg := creativeOutermostSVG(body); svg != "" {
				return pick("svg", svg)
			}
			if page := creativeHTMLRe.FindString(body); page != "" {
				return pick("html", page)
			}
			if creativeHasHTMLStructure(body) {
				return pick("html", body)
			}
		}
		return "", "", false, ""
	}
	blocks := creativeFenceRe.FindAllStringSubmatch(text, -1)
	for i := len(blocks) - 1; i >= 0; i-- {
		lang := strings.ToLower(strings.TrimSpace(blocks[i][1]))
		if f, c, ok, reason := fromBody(lang, blocks[i][2]); ok || reason != "" {
			return f, c, ok, reason
		}
	}
	if page := creativeHTMLRe.FindString(text); page != "" {
		// 整页 HTML 里通常也有 <svg>：整页优先，脚本和样式才不会丢。
		return pick("html", page)
	}
	if svg := creativeOutermostSVG(text); svg != "" {
		return pick("svg", svg)
	}
	return "", "", false, "没返回合法的 SVG/HTML"
}
