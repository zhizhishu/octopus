package op

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/modelverify/numberdist"
	"gorm.io/gorm"
)

// hlwy 基准目录：内置 7 份只读（来自嵌入数据）+ 本地表里的导入/自采件。
//
// 三条边界：
//
//  1. 内置件只读、不入库。它们没有"被谁改过"的可能，也就不存在「运行中把参照物
//     换掉」的窗口。
//  2. 本地件写入即不可变：没有更新/删除入口，同内容重复写入落到同一行。
//     想在旧基准上改一个数字，只能产生一份新基准，旧报告引用的仍是旧快照。
//  3. 读到的每一行都要重新自洽校验（哈希/计数/统计）。对不上就不能拿来对照，
//     也不"修一下继续用"——一份被截断的分布会悄悄把相似度抬到很好看的位置。
//     目录里把这种行标成 unavailable（可见但不可用），而不是整表报错或静默跳过。

// ErrHlwyBaselineNotFound 表示按 id 找不到基准（内置与本地都没有）。
var ErrHlwyBaselineNotFound = errors.New("hlwy 基准不存在")

// HlwyEntryReasonStoredInvalid 是目录项不可用时的原因码：库行本身坏了。
//
// 用一个结构化的原因码而不是库里原文错误：后者会把存储细节泄露到接口上，
// 而调用方真正需要知道的只有「这一行现在不能用」。
const HlwyEntryReasonStoredInvalid = "stored_baseline_invalid"

// hlwyBaselineMaxContentBytes 是本地行 ContentJSON 的体积上限。
// 归一化后的基准通常几十 KB；留到 4 MiB 是为了容纳上游允许的 10000 个样本，
// 同时挡住"把别的东西写进这一列"的意外。
const hlwyBaselineMaxContentBytes = 4 << 20

// HlwyBaselineEntry 是基准目录项（内置与本地同形）。
//
// 刻意**不含**任何「与某个目标有多像」的结论：没有采样目标时算不出相似度，
// 在目录里预先标一个"可比/不可比"只会变成误导。可对照程度在本轮采样完成后
// 才由 HlwyComparability 判定。
//
// Unavailable=true 表示这一行存在但读不出来（被改动/结构坏了）。它仍然列在目录里
// ——「库里确实有这一行」是事实，隐藏它只会让人以为数据丢了；但用它对照必失败。
type HlwyBaselineEntry struct {
	ID             string                `json:"id"`
	Label          string                `json:"label"`
	SourceKind     string                `json:"source_kind"`
	DeclaredModel  string                `json:"declared_model"`
	APIType        string                `json:"api_type,omitempty"`
	CollectedAt    *time.Time            `json:"collected_at"`
	ImportedAt     *time.Time            `json:"imported_at,omitempty"`
	ValidCount     int                   `json:"valid_count"`
	PayloadSHA256  string                `json:"payload_sha256"`
	Builtin        bool                  `json:"builtin"`
	Unavailable    bool                  `json:"unavailable,omitempty"`
	ReasonCode     string                `json:"reason_code,omitempty"`
	SourceResultID int64                 `json:"source_result_id,omitempty"`
	Conditions     numberdist.Conditions `json:"conditions"`
}

// HlwyBaselineResolved 是按 id 解析出来的基准，外加只有在库行上才知道的来源结果 id
// （用于"同一批样本自比"标记）。
type HlwyBaselineResolved struct {
	Baseline       numberdist.Baseline
	SourceResultID int64
	Builtin        bool
}

// Entry 把解析结果转成目录项（与目录里同一套形状，供计划接口回传）。
func (r HlwyBaselineResolved) Entry() HlwyBaselineEntry {
	return hlwyBaselineEntryOf(r.Baseline, r.Builtin, r.SourceResultID)
}

// HlwyBaselineSave 是一次保存的结果。Existed=true 表示同内容已经在目录里，
// 本次没有写入任何东西（幂等，不是失败）。
type HlwyBaselineSave struct {
	Baseline HlwyBaselineEntry `json:"baseline"`
	Existed  bool              `json:"existed"`
}

// HlwyBaselineList 返回内置 + 本地基准。零模型请求。
//
// 顺序：内置按内嵌清单顺序在前（与上游发布顺序一致），本地按创建时间倒序在后。
//
// 单行损坏不会整表消失：那一行标 unavailable + reason_code 照旧列出，其余照常可用。
// 否则一个字节写错就能让整份目录不可读，而且没人能从接口上看出“少的是哪一份”。
func HlwyBaselineList(ctx context.Context) ([]HlwyBaselineEntry, error) {
	builtins, err := numberdist.LoadBuiltins()
	if err != nil {
		return nil, fmt.Errorf("加载内置基准失败: %w", err)
	}
	out := make([]HlwyBaselineEntry, 0, len(builtins)+8)
	for _, b := range builtins {
		out = append(out, hlwyBaselineEntryOf(b, true, 0))
	}
	var rows []model.ModelAuditHlwyBaseline
	if err := db.GetDB().WithContext(ctx).
		Order("created_at DESC, id DESC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		b, err := hlwyBaselineVerifyRow(row)
		if err != nil {
			out = append(out, hlwyUnavailableEntryOf(row))
			continue
		}
		out = append(out, hlwyBaselineEntryOf(b, false, row.SourceResultID))
	}
	return out, nil
}

// HlwyBaselineResolve 按 id 解析基准：先内置（只读），再本地表。
func HlwyBaselineResolve(ctx context.Context, id string) (HlwyBaselineResolved, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return HlwyBaselineResolved{}, errors.New("缺少基准 id")
	}
	if len(id) > model.HlwyMaxBaselineIDBytes {
		return HlwyBaselineResolved{}, fmt.Errorf("基准 id 长度 %d 字节超过上限 %d",
			len(id), model.HlwyMaxBaselineIDBytes)
	}
	builtins, err := numberdist.LoadBuiltins()
	if err != nil {
		return HlwyBaselineResolved{}, fmt.Errorf("加载内置基准失败: %w", err)
	}
	for _, b := range builtins {
		if b.ID == id {
			return HlwyBaselineResolved{Baseline: b, Builtin: true}, nil
		}
	}

	var row model.ModelAuditHlwyBaseline
	err = db.GetDB().WithContext(ctx).Where("id = ?", id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return HlwyBaselineResolved{}, fmt.Errorf("%w: %s", ErrHlwyBaselineNotFound, id)
	}
	if err != nil {
		return HlwyBaselineResolved{}, err
	}
	b, err := hlwyBaselineVerifyRow(row)
	if err != nil {
		return HlwyBaselineResolved{}, fmt.Errorf("本地基准 %s 损坏: %w", row.ID, err)
	}
	return HlwyBaselineResolved{Baseline: b, SourceResultID: row.SourceResultID}, nil
}

// HlwyBaselineLoad 是 HlwyBaselineResolve 的薄包装，只要基准本体。
func HlwyBaselineLoad(ctx context.Context, id string) (numberdist.Baseline, error) {
	resolved, err := HlwyBaselineResolve(ctx, id)
	if err != nil {
		return numberdist.Baseline{}, err
	}
	return resolved.Baseline, nil
}

// HlwyBaselineImport 导入一份旧 hlwy 基准 JSON（对象；也接受只含一个对象的数组）。
//
// 零模型请求。同内容重复导入返回已有行（Existed=true），不写入。
// 与内置件内容相同的一律返回内置条目、不落库：否则库里会出现一条同 id 的行，
// 把只读的内置件遮住，还带着 imported 来源标签。
func HlwyBaselineImport(ctx context.Context, raw []byte, now time.Time) (HlwyBaselineSave, error) {
	if len(raw) == 0 {
		return HlwyBaselineSave{}, errors.New("基准内容为空")
	}
	if len(raw) > numberdist.DefaultMaxImportBytes {
		return HlwyBaselineSave{}, fmt.Errorf("基准文件 %d 字节超过上限 %d",
			len(raw), numberdist.DefaultMaxImportBytes)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	b, err := numberdist.Import(raw, numberdist.ImportOptions{
		Source:     numberdist.Source{Kind: numberdist.SourceImported, Repo: "hlwy"},
		ImportedAt: now.UTC(),
	})
	if err != nil {
		return HlwyBaselineSave{}, err
	}
	if err := model.ValidateHlwyBaseline(b); err != nil {
		return HlwyBaselineSave{}, err
	}

	builtins, err := numberdist.LoadBuiltins()
	if err != nil {
		return HlwyBaselineSave{}, fmt.Errorf("加载内置基准失败: %w", err)
	}
	for _, bi := range builtins {
		if bi.PayloadSHA256 == b.PayloadSHA256 {
			return HlwyBaselineSave{Baseline: hlwyBaselineEntryOf(bi, true, 0), Existed: true}, nil
		}
	}
	return hlwyBaselineStore(ctx, b, 0, now)
}

// HlwyBaselineFromResult 把一条**已完成**的 hlwy 对照结果存成自采基准。
//
// 门槛只认服务器侧证据：结果终态 done、所属任务 kind=hlwy_compare、报告执行状态
// complete、没有未确认槽位、计划槽位全部消费、有效样本 ≥ HlwyMinCaptureSamples，
// 且本轮确实跑的就是本协议/本解析/本题目。取消/失败/中断一律拒绕——哪怕已经
// 采到 100 个有效数字：一次被打断的采样不是"这一轮的全部证据"。
//
// 条件快照只要求"是自己的协议标记"，不要求观测齐：真实本地采样本来就可能
// 不知道 temperature/thinking（未发字段或看不到），那是要原样保留的证据。
// 把 incomplete 当作不可存，只会逼着人手工填值，反而造假。
//
// 计数、统计、分布一律从报告里的原始数字重算，不采信报告里现成的汇总值；
// 报告自带的分布/统计与原始数字不一致时，ParseHlwyReport 就已经拒绝。
func HlwyBaselineFromResult(ctx context.Context, resultID int64, label string) (HlwyBaselineSave, error) {
	if resultID <= 0 {
		return HlwyBaselineSave{}, errors.New("缺少检测结果 id")
	}
	res, _, err := hlwyResultAndJob(ctx, resultID)
	if err != nil {
		return HlwyBaselineSave{}, err
	}
	if res.Status != model.AuditResultDone {
		return HlwyBaselineSave{}, fmt.Errorf(
			"结果状态是 %q，只有正常结束的对照才能存为基准", res.Status)
	}

	report, err := model.ParseHlwyReport(res.Report)
	if err != nil {
		return HlwyBaselineSave{}, err
	}
	bc := report.BaselineComparison
	if bc.Execution != model.HlwyExecutionComplete {
		return HlwyBaselineSave{}, fmt.Errorf(
			"报告执行状态是 %q，只有 complete 才能存为基准", bc.Execution)
	}
	if bc.Counts.Unresolved != 0 {
		return HlwyBaselineSave{}, fmt.Errorf(
			"还有 %d 个未确认槽位，不能存为基准", bc.Counts.Unresolved)
	}
	if bc.Counts.StartedSlots != bc.Counts.PlannedCalls {
		return HlwyBaselineSave{}, fmt.Errorf(
			"计划 %d 次采样只消费了 %d 个槽位，不能存为基准",
			bc.Counts.PlannedCalls, bc.Counts.StartedSlots)
	}
	if bc.Counts.Valid < model.HlwyMinCaptureSamples {
		return HlwyBaselineSave{}, fmt.Errorf(
			"有效样本 %d 个，少于存基准要求的 %d 个", bc.Counts.Valid, model.HlwyMinCaptureSamples)
	}
	if bc.Conditions.ProtocolID != numberdist.ProtocolID {
		return HlwyBaselineSave{}, fmt.Errorf(
			"本轮协议 %q 与基准协议 %q 不一致，不能存为基准",
			bc.Conditions.ProtocolID, numberdist.ProtocolID)
	}
	// 解析版本与题目哈希也要核：不然一份用其他解析规则（如“取末尾数字”）
	// 或另一道题跑出来的数字，会被当作同协议数据存进基准目录。
	if bc.Conditions.ParserVersion != numberdist.ParserVersion {
		return HlwyBaselineSave{}, fmt.Errorf(
			"本轮解析版本 %q 与基准解析版本 %q 不一致，不能存为基准",
			bc.Conditions.ParserVersion, numberdist.ParserVersion)
	}
	if want := model.HlwyPromptSHA256(); bc.Conditions.PromptSHA256 != want {
		return HlwyBaselineSave{}, fmt.Errorf(
			"本轮题目哈希 %q 与基准题目 %q 不一致，不能存为基准",
			bc.Conditions.PromptSHA256, want)
	}

	label = strings.TrimSpace(label)
	if label == "" {
		label = fmt.Sprintf("%s 自采 #%d", hlwyResultModelName(res), resultID)
	}
	if len(label) > model.HlwyMaxLabelBytes {
		return HlwyBaselineSave{}, fmt.Errorf("label 长度 %d 字节超过上限 %d",
			len(label), model.HlwyMaxLabelBytes)
	}

	// 采集时间取这条结果真正结束的时刻；缺失时留空（未知），不补今天。
	// 把“不知道”写成“刚刚”会让一份旧基准看起来像新的。
	collected := res.FinishedAt
	now := time.Now().UTC()
	b, err := model.BuildHlwyCapture(model.HlwyCaptureInput{
		SourceResultID: resultID,
		Label:          label,
		DeclaredModel:  hlwyResultModelName(res),
		CollectedAt:    collected,
		Conditions:     bc.Conditions,
		RawNumbers:     bc.RawNumbers,
	}, now)
	if err != nil {
		return HlwyBaselineSave{}, err
	}
	if err := model.ValidateHlwyBaseline(b); err != nil {
		return HlwyBaselineSave{}, err
	}
	return hlwyBaselineStore(ctx, b, resultID, now)
}

// hlwyBaselineStore 按内容哈希幂等写入一行。
func hlwyBaselineStore(ctx context.Context, b numberdist.Baseline,
	sourceResultID int64, now time.Time) (HlwyBaselineSave, error) {

	gdb := db.GetDB().WithContext(ctx)
	var existing model.ModelAuditHlwyBaseline
	err := gdb.Where("payload_sha256 = ?", b.PayloadSHA256).First(&existing).Error
	if err == nil {
		parsed, verr := hlwyBaselineVerifyRow(existing)
		if verr != nil {
			return HlwyBaselineSave{}, fmt.Errorf("已有基准 %s 校验失败: %w", existing.ID, verr)
		}
		return HlwyBaselineSave{
			Baseline: hlwyBaselineEntryOf(parsed, false, existing.SourceResultID),
			Existed:  true,
		}, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return HlwyBaselineSave{}, err
	}

	content, err := json.Marshal(b)
	if err != nil {
		return HlwyBaselineSave{}, fmt.Errorf("基准序列化失败: %w", err)
	}
	// 落库前先卡体积：读路径上的上限只保护读的人，写的时候就应该拦住。
	if len(content) > hlwyBaselineMaxContentBytes {
		return HlwyBaselineSave{}, fmt.Errorf("基准内容 %d 字节超过上限 %d",
			len(content), hlwyBaselineMaxContentBytes)
	}
	row := model.ModelAuditHlwyBaseline{
		ID:             b.ID,
		PayloadSHA256:  b.PayloadSHA256,
		Label:          b.Label,
		SourceResultID: sourceResultID,
		ContentJSON:    string(content),
		CreatedAt:      now.UTC(),
	}
	if err := gdb.Create(&row).Error; err != nil {
		// 并发导入同一份内容：唯一索引会挡住第二个写入者。
		// 此时按哈希再查一次，能查到就说明「别人已经写进去了」，属于幂等命中。
		var raced model.ModelAuditHlwyBaseline
		if qerr := gdb.Where("payload_sha256 = ?", b.PayloadSHA256).First(&raced).Error; qerr == nil {
			parsed, verr := hlwyBaselineVerifyRow(raced)
			if verr != nil {
				return HlwyBaselineSave{}, fmt.Errorf("已有基准 %s 校验失败: %w", raced.ID, verr)
			}
			return HlwyBaselineSave{
				Baseline: hlwyBaselineEntryOf(parsed, false, raced.SourceResultID),
				Existed:  true,
			}, nil
		}
		return HlwyBaselineSave{}, err
	}
	return HlwyBaselineSave{Baseline: hlwyBaselineEntryOf(b, false, sourceResultID)}, nil
}

// hlwyBaselineVerifyRow 解析并自洽校验一行本地基准。
//
// 自采件额外复算规范化哈希：数字、条件、来源结果 id 任一被改过都对不上，
// 于是坏数据在读取时就被挡住，而不是先算出一个分数再让人去猜它可信不可信。
func hlwyBaselineVerifyRow(row model.ModelAuditHlwyBaseline) (numberdist.Baseline, error) {
	content := strings.TrimSpace(row.ContentJSON)
	if content == "" {
		return numberdist.Baseline{}, errors.New("基准内容为空")
	}
	if len(content) > hlwyBaselineMaxContentBytes {
		return numberdist.Baseline{}, fmt.Errorf("基准内容 %d 字节超过上限 %d",
			len(content), hlwyBaselineMaxContentBytes)
	}
	var b numberdist.Baseline
	if err := json.Unmarshal([]byte(content), &b); err != nil {
		return numberdist.Baseline{}, fmt.Errorf("解析基准内容失败: %w", err)
	}
	if err := model.ValidateHlwyBaseline(b); err != nil {
		return numberdist.Baseline{}, err
	}
	if b.ID != row.ID {
		return numberdist.Baseline{}, fmt.Errorf("内容里的 id %q 与行 id %q 不一致", b.ID, row.ID)
	}
	if b.PayloadSHA256 != row.PayloadSHA256 {
		return numberdist.Baseline{}, errors.New("内容里的 payload_sha256 与行不一致")
	}
	if b.Source.Kind == numberdist.SourceLocal {
		sha, err := model.HlwyCaptureHash(b.RawNumbers, b.Conditions, row.SourceResultID)
		if err != nil {
			return numberdist.Baseline{}, err
		}
		if sha != row.PayloadSHA256 {
			return numberdist.Baseline{}, errors.New("自采哈希与内容不符（数字或条件被改动过）")
		}
	}
	return b, nil
}

// hlwyUnavailableEntryOf 把读不出来的库行做成一条“可见但不可用”的目录项。
//
// 只带行里自带的元数据，不尝试解释哪里坏了：描述损坏细节会诱使人去“修一下”，
// 而正确的做法是让这份基准不可用。
func hlwyUnavailableEntryOf(row model.ModelAuditHlwyBaseline) HlwyBaselineEntry {
	entry := HlwyBaselineEntry{
		ID:             row.ID,
		Label:          row.Label,
		PayloadSHA256:  row.PayloadSHA256,
		SourceResultID: row.SourceResultID,
		Unavailable:    true,
		ReasonCode:     HlwyEntryReasonStoredInvalid,
		Conditions: numberdist.Conditions{
			TemperatureState: numberdist.StateUnknown,
			TopPState:        numberdist.StateUnknown,
			Completeness:     "unknown",
		},
	}
	if !row.CreatedAt.IsZero() {
		t := row.CreatedAt.UTC()
		entry.ImportedAt = &t
	}
	return entry
}

// hlwyBaselineEntryOf 把基准转成目录项。
func hlwyBaselineEntryOf(b numberdist.Baseline, builtin bool, sourceResultID int64) HlwyBaselineEntry {
	entry := HlwyBaselineEntry{
		ID:             b.ID,
		Label:          b.Label,
		SourceKind:     string(b.Source.Kind),
		DeclaredModel:  b.DeclaredModel,
		APIType:        b.APIType,
		ValidCount:     len(b.RawNumbers),
		PayloadSHA256:  b.PayloadSHA256,
		Builtin:        builtin,
		SourceResultID: sourceResultID,
		Conditions:     b.Conditions,
	}
	if b.CollectedAt != nil {
		t := b.CollectedAt.UTC()
		entry.CollectedAt = &t
	}
	if !b.ImportedAt.IsZero() {
		t := b.ImportedAt.UTC()
		entry.ImportedAt = &t
	}
	return entry
}

// hlwyLocalSourceResultID 只读取一行的来源结果 id（不解析内容、不校验）。
//
// 专用于离线重算时判断「这份基准是不是本结果自采的」：此时基准本体一律用冻结
// 快照，不能因为库行坏了就把一次本可完成的读重算拦下来。查不到就返回 0。
func hlwyLocalSourceResultID(ctx context.Context, id string) int64 {
	if strings.TrimSpace(id) == "" {
		return 0
	}
	var row model.ModelAuditHlwyBaseline
	if err := db.GetDB().WithContext(ctx).
		Select("id", "source_result_id").
		Where("id = ?", id).First(&row).Error; err != nil {
		return 0
	}
	return row.SourceResultID
}

// hlwyResultAndJob 取检测结果及其任务，并校验任务种类确实是 hlwy 对照。
//
// 校验任务种类而不是相信结果本身：结果行没有独立的种类列（也刻意不加），
// 「这条结果是不是 hlwy」的唯一事实来源就是它的任务。
func hlwyResultAndJob(ctx context.Context, resultID int64) (model.ModelAuditResult, model.ModelAuditJob, error) {
	gdb := db.GetDB().WithContext(ctx)
	var res model.ModelAuditResult
	if err := gdb.First(&res, resultID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return model.ModelAuditResult{}, model.ModelAuditJob{},
				fmt.Errorf("检测结果 #%d 不存在", resultID)
		}
		return model.ModelAuditResult{}, model.ModelAuditJob{}, err
	}
	var job model.ModelAuditJob
	if err := gdb.First(&job, res.JobID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return model.ModelAuditResult{}, model.ModelAuditJob{},
				fmt.Errorf("检测结果 #%d 所属任务 #%d 不存在", resultID, res.JobID)
		}
		return model.ModelAuditResult{}, model.ModelAuditJob{}, err
	}
	if hlwyJobKind(job) != model.AuditKindHlwy {
		return model.ModelAuditResult{}, model.ModelAuditJob{}, fmt.Errorf(
			"任务 #%d 的种类是 %q，不是 hlwy 对照", job.ID, hlwyJobKind(job))
	}
	return res, job, nil
}

// hlwyJobKind 归一任务种类：空列按 audit（存量行与旧调用方都不带 kind）。
func hlwyJobKind(job model.ModelAuditJob) string {
	if strings.TrimSpace(job.Kind) == "" {
		return model.AuditKindProbe
	}
	return job.Kind
}

// hlwyResultModelName 是结果的显示型号：优先请求名，空则退回映射后的型号。
func hlwyResultModelName(res model.ModelAuditResult) string {
	if m := strings.TrimSpace(res.Model); m != "" {
		return m
	}
	return strings.TrimSpace(res.UpstreamModel)
}
