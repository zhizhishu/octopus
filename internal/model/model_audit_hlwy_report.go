package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/zhizhishu/octopus/internal/modelverify/numberdist"
)

// hlwy 对照报告。
//
// 为什么在 model 包里独立定义、而不复用 modelverify/behavior 的 ViewReport：
//
//   - 旧报告是「风险计分」的载体：score/verdict/findings 都是一套判定规则的输出。
//     hlwy 对照不做真伪判定，把它的结果塞进 verdict，就必然出现「verdict=unknown
//     ⇒ 界面显示检测失败」这种错话。
//   - 但顶层的 score/verdict/results/findings/errors/plain 必须**保持同形**：
//     历史列表、详情页、导出都按这些键取值。所以这里显式复刻一份向后兼容的外壳，
//     对照结果放在独立的 baseline_comparison 里，两套语义不互相污染。
//   - 报告要能被离线重算（换基准比较），所以它必须是可校验的结构化类型，
//     而不是一串随手拼的 map。

// 执行状态。
const (
	HlwyExecutionSampling    = "sampling"
	HlwyExecutionComplete    = "complete"
	HlwyExecutionCanceled    = "canceled"
	HlwyExecutionTimedOut    = "timed_out"
	HlwyExecutionFailed      = "failed"
	HlwyExecutionInterrupted = "interrupted"
)

// 样本充分性。只表示「够不够展示参考分数」，不表示结论可靠。
const (
	HlwySufficiencyInsufficient = "insufficient"
	HlwySufficiencyScorable     = "scorable"
)

// 可对照程度。unknown 不是错误：社区基准的参数多半没观测到，仍然可选、可比较。
const (
	HlwyComparabilityUnknown      = "unknown"
	HlwyComparabilityDifferent    = "different"
	HlwyComparabilityMatched      = "matched"
	HlwyComparabilityIncompatible = "incompatible"
)

// HlwyVerdictUnknown 是 hlwy 报告顶层 verdict 的固定值：本模式不参与风险判定。
const HlwyVerdictUnknown = "unknown"

// HlwyReasonIncompatibleProtocol 是「不可比所以不给分」的原因码。
//
// 用单独的原因码而不是复用 insufficient_samples：两者的处置完全不同——
// 前者再采样也没用，后者再多跑几轮就可以。
const HlwyReasonIncompatibleProtocol = "incompatible_protocol"

// 报告校验错误。分开定义是为了让调用方能区分「没有快照」和「快照坏了」：
// 前者是正常的旧结果，后者必须停下来查，不能当成同一种情况处理。
var (
	ErrHlwyReportEmpty = errors.New("没有可离线重算的 hlwy 报告快照")
	ErrHlwyReportKind  = errors.New("报告不是 hlwy 对照报告")
)

// HlwyAuditReport 是 kind=hlwy_compare 的完整报告，落库进 ModelAuditResult.Report。
type HlwyAuditReport struct {
	Kind           string `json:"kind"`
	RequestedModel string `json:"requested_model"`
	// UpstreamModel 是实际发往上游的型号（有映射时与 RequestedModel 不同）。
	UpstreamModel string `json:"upstream_model"`
	// Score/Verdict 恒为 0/unknown：本模式不产出风险分。
	Score   int    `json:"score"`
	Verdict string `json:"verdict"`
	// 三个数组沿用旧报告的键名与语义（旧组件会读它们）。
	Results  []json.RawMessage `json:"results"`
	Findings []json.RawMessage `json:"findings"`
	Errors   []json.RawMessage `json:"errors"`
	// Plain 是本模式自己的措辞，不套 PlainVerdictFor(unknown) 的「没查清」标题。
	Plain              HlwyPlain              `json:"plain"`
	BaselineComparison HlwyBaselineComparison `json:"baseline_comparison"`
}

// HlwyPlain 是对照模式的人话结论外壳（沿用旧报告的 plain 键名）。
type HlwyPlain struct {
	Headline string `json:"headline"`
	Summary  string `json:"summary"`
}

// HlwyCounts 是采样账目。恒等式：started_slots = valid+invalid+failed+unresolved。
//
// started_slots 是**保守**槽位账：进程可能在写账之后、真正发包之前崩溃，
// 所以它是「可能已经花掉的钱」的上界，不是精确账单。
type HlwyCounts struct {
	PlannedCalls int `json:"planned_calls"`
	StartedSlots int `json:"started_slots"`
	Valid        int `json:"valid"`
	Invalid      int `json:"invalid"`
	Failed       int `json:"failed"`
	Unresolved   int `json:"unresolved"`
}

// HlwyQuality 是解析质量计数（宽松解析与截断都是证据，不是错误）。
type HlwyQuality struct {
	StrictInteger int `json:"strict_integer"`
	Permissive    int `json:"permissive"`
	Truncated     int `json:"truncated"`
}

// HlwyBaselineComparison 是 hlwy 对照的核心结果。
//
//   - RawNumbers 是本轮的有效数字（1..355），离线重算的唯一输入。
//   - Conditions 是本轮**实际发出的请求**条件（未观测的字段保持 unknown）。
//   - BaselineSnapshots 是参与者基准的完整不可变快照，直接复用
//     numberdist.Baseline：来源、355 桶计数、raw_numbers、统计都在里面。
//     「有效数」= len(raw_numbers)，不再另存一份派生副本——两份派生值一旦
//     漂移，就没有第三方能判断哪一份是真的。
type HlwyBaselineComparison struct {
	SchemaVersion     int                            `json:"schema_version"`
	AlgorithmVersion  string                         `json:"algorithm_version"`
	Execution         string                         `json:"execution"`
	Sufficiency       string                         `json:"sufficiency"`
	Comparability     string                         `json:"comparability"`
	ReasonCodes       []string                       `json:"reason_codes"`
	Counts            HlwyCounts                     `json:"counts"`
	RawNumbers        []int                          `json:"raw_numbers"`
	Conditions        numberdist.Conditions          `json:"conditions"`
	BaselineSnapshots []numberdist.Baseline          `json:"baseline_snapshots"`
	Scorable          bool                           `json:"scorable"`
	Reason            string                         `json:"reason"`
	ValidCount        int                            `json:"valid_count"`
	Distribution      [numberdist.DistLength]float64 `json:"distribution"`
	Stats             numberdist.Stats               `json:"stats"`
	ChosenBaselineID  string                         `json:"chosen_baseline_id"`
	Matches           []numberdist.ScoreResult       `json:"matches"`
	Quality           HlwyQuality                    `json:"quality"`
	StopReason        string                         `json:"stop_reason"`
	SelfCompare       bool                           `json:"self_compare"`
}

// Normalize 把可空数组补成空数组（前端按数组遍历，null 会直接崩）。
func (r *HlwyAuditReport) Normalize() {
	if r.Results == nil {
		r.Results = []json.RawMessage{}
	}
	if r.Findings == nil {
		r.Findings = []json.RawMessage{}
	}
	if r.Errors == nil {
		r.Errors = []json.RawMessage{}
	}
	bc := &r.BaselineComparison
	if bc.ReasonCodes == nil {
		bc.ReasonCodes = []string{}
	}
	if bc.RawNumbers == nil {
		bc.RawNumbers = []int{}
	}
	if bc.BaselineSnapshots == nil {
		bc.BaselineSnapshots = []numberdist.Baseline{}
	}
	if bc.Matches == nil {
		bc.Matches = []numberdist.ScoreResult{}
	}
}

// Filled 返回补齐空数组后的副本。
func (r HlwyAuditReport) Filled() HlwyAuditReport {
	r.Normalize()
	return r
}

// Encode 序列化成落库字符串（数组一律非 null）。
func (r HlwyAuditReport) Encode() (string, error) {
	out := r.Filled()
	raw, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("hlwy 报告序列化失败: %w", err)
	}
	return string(raw), nil
}

// ParseHlwyReport 解析并校验一份落库的 hlwy 报告。
//
// 校验的每一条都是「不这样做就会有人拿坏数据当结论」的地方：
// 种类对不对、计数恒等式、有效数字范围与条数、分布/统计与数字是否一致、
// 分数是否有限且落在 0..1。任何一条不成立都返回错误，绝不修正后继续。
func ParseHlwyReport(raw string) (HlwyAuditReport, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return HlwyAuditReport{}, ErrHlwyReportEmpty
	}
	var r HlwyAuditReport
	if err := json.Unmarshal([]byte(trimmed), &r); err != nil {
		return HlwyAuditReport{}, fmt.Errorf("解析 hlwy 报告失败: %w", err)
	}
	if err := r.Validate(); err != nil {
		return HlwyAuditReport{}, err
	}
	return r.Filled(), nil
}

// Validate 校验报告自洽性。
func (r HlwyAuditReport) Validate() error {
	if r.Kind != AuditKindHlwy {
		return fmt.Errorf("%w: kind=%q", ErrHlwyReportKind, r.Kind)
	}
	bc := r.BaselineComparison
	// 风险分与判定必须保持“本模式不参与风险计分”这个事实。
	// 允许它们被随便写成 55/high，等于给界面递了一个假结论。
	if r.Score != 0 {
		return fmt.Errorf("hlwy 报告不应带风险分（score=%d）", r.Score)
	}
	if r.Verdict != HlwyVerdictUnknown {
		return fmt.Errorf("hlwy 报告的 verdict 必须是 %q，收到 %q", HlwyVerdictUnknown, r.Verdict)
	}
	if bc.SchemaVersion != numberdist.SchemaVersion {
		return fmt.Errorf("报告结构版本 %d 不受支持（当前 %d）",
			bc.SchemaVersion, numberdist.SchemaVersion)
	}
	if strings.TrimSpace(bc.AlgorithmVersion) == "" {
		return errors.New("报告缺少算法版本")
	}
	if !isHlwyExecution(bc.Execution) {
		return fmt.Errorf("未知执行状态 %q", bc.Execution)
	}
	if bc.Sufficiency != HlwySufficiencyInsufficient && bc.Sufficiency != HlwySufficiencyScorable {
		return fmt.Errorf("未知充分性状态 %q", bc.Sufficiency)
	}
	switch bc.Comparability {
	case HlwyComparabilityUnknown, HlwyComparabilityDifferent,
		HlwyComparabilityMatched, HlwyComparabilityIncompatible:
	default:
		return fmt.Errorf("未知可对照状态 %q", bc.Comparability)
	}

	c := bc.Counts
	if c.PlannedCalls != HlwyDefaultSamples && c.PlannedCalls != HlwyMaxSamples {
		return fmt.Errorf("计划调用数 %d 不合法（只允许 %d/%d）",
			c.PlannedCalls, HlwyDefaultSamples, HlwyMaxSamples)
	}
	if c.StartedSlots < 0 || c.Valid < 0 || c.Invalid < 0 || c.Failed < 0 || c.Unresolved < 0 {
		return errors.New("采样计数出现负数")
	}
	// 逐项先卡上界、再做求和：先加再判会让「几个大数相加溢出成负数」绕过恒等式检查。
	for _, kv := range []struct {
		name string
		v    int
	}{
		{"started_slots", c.StartedSlots}, {"valid", c.Valid}, {"invalid", c.Invalid},
		{"failed", c.Failed}, {"unresolved", c.Unresolved},
	} {
		if kv.v > c.PlannedCalls {
			return fmt.Errorf("%s=%d 超过计划调用 %d", kv.name, kv.v, c.PlannedCalls)
		}
	}
	if c.StartedSlots != c.Valid+c.Invalid+c.Failed+c.Unresolved {
		return fmt.Errorf("采样计数不自洽: started=%d ≠ valid+invalid+failed+unresolved=%d",
			c.StartedSlots, c.Valid+c.Invalid+c.Failed+c.Unresolved)
	}
	if c.StartedSlots > c.PlannedCalls {
		return fmt.Errorf("已开始槽位 %d 超过计划 %d", c.StartedSlots, c.PlannedCalls)
	}
	// complete = 每个槽位都落定。只看 execution 字段等于让一个写字的人决定结论，
	// 必须与账目互相对得上。
	if bc.Execution == HlwyExecutionComplete {
		if c.StartedSlots != c.PlannedCalls {
			return fmt.Errorf("执行状态 complete 但只开始 %d/%d 次采样", c.StartedSlots, c.PlannedCalls)
		}
		if c.Unresolved != 0 {
			return fmt.Errorf("执行状态 complete 却有 %d 个未落定槽位", c.Unresolved)
		}
	}
	if len(bc.RawNumbers) != c.Valid {
		return fmt.Errorf("raw_numbers %d 个与 counts.valid=%d 不符", len(bc.RawNumbers), c.Valid)
	}
	if bc.ValidCount != c.Valid {
		return fmt.Errorf("valid_count=%d 与 counts.valid=%d 不符", bc.ValidCount, c.Valid)
	}
	if bc.Quality.StrictInteger < 0 || bc.Quality.Permissive < 0 || bc.Quality.Truncated < 0 {
		return errors.New("质量计数出现负数")
	}
	if bc.Quality.StrictInteger+bc.Quality.Permissive > c.Valid {
		return fmt.Errorf("宽松/严格解析计数之和超过有效样本 %d", c.Valid)
	}
	if bc.Quality.Truncated > c.StartedSlots {
		return fmt.Errorf("截断计数 %d 超过已开始槽位 %d", bc.Quality.Truncated, c.StartedSlots)
	}

	// 分布/统计必须能从 raw_numbers 复算出来：对不上就是数据被改过或截断了。
	if c.Valid > 0 {
		counts, err := HlwyBinCounts(bc.RawNumbers)
		if err != nil {
			return err
		}
		for i := 0; i < numberdist.DistLength; i++ {
			want := float64(counts[i]) / float64(c.Valid)
			if !hlwyCloseEnough(bc.Distribution[i], want) {
				return fmt.Errorf("分布第 %d 桶 %v 与 raw_numbers 重算 %v 不符", i, bc.Distribution[i], want)
			}
		}
		if want := numberdist.StatsOf(bc.RawNumbers); !hlwyStatsEqual(bc.Stats, want) {
			return errors.New("统计与 raw_numbers 不符")
		}
	} else {
		for i := 0; i < numberdist.DistLength; i++ {
			if bc.Distribution[i] != 0 {
				return fmt.Errorf("有效样本为 0 时分布第 %d 桶应为 0，收到 %v", i, bc.Distribution[i])
			}
		}
	}

	// 充分性必须与有效数一致，避免「39 个样本却标着可评分」。
	if want := HlwySufficiency(c.Valid); bc.Sufficiency != want {
		return fmt.Errorf("充分性 %q 与有效样本 %d 不符（应为 %q）", bc.Sufficiency, c.Valid, want)
	}
	// 可评分 = 够样本 **且** 可对照：协议/解析/题目不同的基准，样本再多也不给分。
	if want := HlwyScorable(c.Valid, bc.Comparability); bc.Scorable != want {
		return fmt.Errorf("scorable=%v 与有效样本 %d、可对照程度 %q 不符",
			bc.Scorable, c.Valid, bc.Comparability)
	}

	if len(bc.BaselineSnapshots) > HlwyMaxCandidates {
		return fmt.Errorf("基准快照 %d 份超过上限 %d", len(bc.BaselineSnapshots), HlwyMaxCandidates)
	}
	snapIDs := make(map[string]struct{}, len(bc.BaselineSnapshots))
	for i, snap := range bc.BaselineSnapshots {
		if err := ValidateHlwyBaseline(snap); err != nil {
			return fmt.Errorf("基准快照 #%d: %w", i, err)
		}
		if _, dup := snapIDs[snap.ID]; dup {
			return fmt.Errorf("基准快照 id 重复: %s", snap.ID)
		}
		snapIDs[snap.ID] = struct{}{}
	}
	if len(bc.BaselineSnapshots) > 0 {
		if _, ok := snapIDs[bc.ChosenBaselineID]; !ok {
			return fmt.Errorf("主对照 %q 不在基准快照里", bc.ChosenBaselineID)
		}
	}
	if len(bc.Matches) > HlwyMaxCandidates {
		return fmt.Errorf("候选分数 %d 条超过上限 %d", len(bc.Matches), HlwyMaxCandidates)
	}
	if !bc.Scorable {
		// 不可评分就绝不下发分数：一份「不兼容但排名第一」的表格
		// 一定会被当成结论看。
		if len(bc.Matches) != 0 {
			return fmt.Errorf("不可评分却带 %d 条候选分数", len(bc.Matches))
		}
		if strings.TrimSpace(bc.Reason) == "" {
			return errors.New("不可评分却没有给出原因")
		}
		return nil
	}
	if len(bc.Matches) != len(snapIDs) {
		return fmt.Errorf("可评分时每份快照应恰好一条分数：%d 份快照 vs %d 条分数",
			len(snapIDs), len(bc.Matches))
	}
	matchIDs := make(map[string]struct{}, len(bc.Matches))
	for i, m := range bc.Matches {
		if err := validateHlwyScore(m); err != nil {
			return fmt.Errorf("候选分数 #%d: %w", i, err)
		}
		if _, ok := snapIDs[m.BaselineID]; !ok {
			return fmt.Errorf("候选分数 #%d 指向未知基准 %q", i, m.BaselineID)
		}
		if _, dup := matchIDs[m.BaselineID]; dup {
			return fmt.Errorf("候选分数里基准 %q 出现两次", m.BaselineID)
		}
		matchIDs[m.BaselineID] = struct{}{}
		if m.Rank != i+1 {
			return fmt.Errorf("候选分数 #%d 排名为 %d（应为 %d）", i, m.Rank, i+1)
		}
	}
	if _, ok := matchIDs[bc.ChosenBaselineID]; !ok {
		return fmt.Errorf("主对照 %q 没有对应的分数", bc.ChosenBaselineID)
	}
	return nil
}

// validateHlwyScore 校验单条分数：必须有限、落在 0..1、排名从 1 起。
//
// 计算出的分数不会被这里「修正」——非有限值说明上游数据坏了，
// 展示 0% 或 100% 都比报错更误导。
func validateHlwyScore(m numberdist.ScoreResult) error {
	if strings.TrimSpace(m.BaselineID) == "" {
		return errors.New("缺少 baseline_id")
	}
	for name, v := range map[string]float64{
		"cosine": m.Cosine, "jsd": m.JSD, "mode_score": m.ModeScore, "overall": m.Overall,
	} {
		if !isFiniteFloat(v) {
			return fmt.Errorf("%s 非有限值", name)
		}
		if v < -1e-9 || v > 1+1e-9 {
			return fmt.Errorf("%s=%v 越出 0..1", name, v)
		}
	}
	if m.Rank < 1 {
		return fmt.Errorf("rank=%d 非法", m.Rank)
	}
	if m.GapToPrevious != nil {
		if !isFiniteFloat(*m.GapToPrevious) || *m.GapToPrevious < -1e-9 {
			return fmt.Errorf("gap_to_previous=%v 非法", *m.GapToPrevious)
		}
	}
	return nil
}

// HlwySufficiency 由有效样本数给出充分性档位。
func HlwySufficiency(valid int) string {
	if valid >= numberdist.MinScorable {
		return HlwySufficiencyScorable
	}
	return HlwySufficiencyInsufficient
}

// HlwyScorable 判定本轮能否给出相似度分数：样本够 **且** 可对照。
//
// 只卡 valid>=MinScorable 是错的：一份题目/解析/协议不同的基准，就算有 200 个
// 有效样本，算出的相似度也只是两个随机分布的相似度，展示出来必然被当成结论。
func HlwyScorable(valid int, comparability string) bool {
	return valid >= numberdist.MinScorable && comparability != HlwyComparabilityIncompatible
}

// HlwyComparability 判定本轮采样条件与某份基准条件的可对照程度。
//
// 口径（顺序即优先级）：
//
//		incompatible > different > unknown > matched
//
//	  - 协议/解析/题目不同 = 不可比：两批数字根本不是同一个实验，算出来的相似度
//	    只是两个随机分布的相似度，写成结论会误导。
//	  - 已知条件不同 = different：仍然可以算（这是描述性对照），但必须说明差异。
//	  - 任一侧没观测到 = unknown。**unknown + unknown 绝不算 matched**：
//	    「两边都不知道」不构成「两边一致」。
//	  - 只有双方都完整观测且逐项一致才是 matched。即便 matched，也只说明两批请求
//	    的参数看起来一致，不能证明实际落到同一个后端。
//	  - 字段值写成字符串 "unknown" 与空值等价：都是「没观测到」，绝不算一致。
func HlwyComparability(run, base numberdist.Conditions) (string, []string) {
	codes := make([]string, 0, 8)
	incompatible := false
	different := false
	unknown := false

	addIncompat := func(code string) { incompatible = true; codes = append(codes, code) }
	addDiff := func(code string) { different = true; codes = append(codes, code) }
	addUnknown := func(code string) { unknown = true; codes = append(codes, code) }

	// API 协议族先用一层：openai-chat vs anthropic-messages 属于「已知条件不同」
	// （仍然可对照，但必须留痕），不是「不可比」——同一个题目可以跑在不同协议上。
	switch {
	case !hlwyKnown(base.Protocol) || !hlwyKnown(run.Protocol):
		addUnknown("api_protocol_unknown")
	case !strings.EqualFold(run.Protocol, base.Protocol):
		addDiff("api_protocol_differs")
	}

	// 题目/解析协议：两边都空 = 谁也不知道，不算一致。
	switch {
	case !hlwyKnown(run.ProtocolID) || !hlwyKnown(base.ProtocolID):
		addUnknown("protocol_unknown")
	case run.ProtocolID != base.ProtocolID:
		addIncompat("protocol_mismatch")
	}
	switch {
	case !hlwyKnown(run.ParserVersion) || !hlwyKnown(base.ParserVersion):
		addUnknown("parser_unknown")
	case run.ParserVersion != base.ParserVersion:
		addIncompat("parser_mismatch")
	}
	switch {
	case !hlwyKnown(run.PromptSHA256) || !hlwyKnown(base.PromptSHA256):
		addUnknown("prompt_unknown")
	case run.PromptSHA256 != base.PromptSHA256:
		addIncompat("prompt_mismatch")
	}

	// 已观测的采样参数：三态里的 value 才参与比较。
	cmpFloat := func(name, runState string, runVal *float64, baseState string, baseVal *float64) {
		switch {
		case runState == numberdist.StateValue && baseState == numberdist.StateValue &&
			runVal != nil && baseVal != nil:
			if math.Abs(*runVal-*baseVal) > 1e-9 {
				addDiff(name + "_differs")
			}
		case runState == numberdist.StateAbsent && baseState == numberdist.StateAbsent:
			// 两边都观测到「最终请求体省略了该字段」——这才是可比的「都没设」。
		case runState == numberdist.StateAbsent && baseState == numberdist.StateValue,
			runState == numberdist.StateValue && baseState == numberdist.StateAbsent:
			addDiff(name + "_differs")
		default:
			addUnknown(name + "_unknown")
		}
	}
	cmpFloat("temperature", run.TemperatureState, run.Temperature, base.TemperatureState, base.Temperature)
	cmpFloat("top_p", run.TopPState, run.TopP, base.TopPState, base.TopP)

	cmpBool := func(name string, runVal, baseVal *bool) {
		if runVal == nil || baseVal == nil {
			addUnknown(name + "_unknown")
			return
		}
		if *runVal != *baseVal {
			addDiff(name + "_differs")
		}
	}
	cmpBool("stream", run.Stream, base.Stream)

	if run.MaxTokens == nil || base.MaxTokens == nil ||
		run.MaxTokenField == "" || base.MaxTokenField == "" {
		addUnknown("max_tokens_unknown")
	} else if *run.MaxTokens != *base.MaxTokens || run.MaxTokenField != base.MaxTokenField {
		addDiff("max_tokens_differs")
	}

	cmpString := func(name, a, b string) {
		if !hlwyKnown(a) || !hlwyKnown(b) {
			addUnknown(name + "_unknown")
			return
		}
		if !strings.EqualFold(a, b) {
			addDiff(name + "_differs")
		}
	}
	cmpString("thinking", run.Thinking, base.Thinking)
	cmpString("reasoning_effort", run.ReasoningEffort, base.ReasoningEffort)
	cmpBool("system", run.SystemInjected, base.SystemInjected)
	cmpBool("tools", run.ToolsInjected, base.ToolsInjected)

	switch {
	case !hlwyKnown(run.TransportProfile) || !hlwyKnown(base.TransportProfile):
		addUnknown("transport_unknown")
	case run.TransportProfile != base.TransportProfile:
		// 传输形态不同（裸客户端 vs 本项目中转链）不阻止对照，但必须留痕。
		addDiff("transport_differs")
	}

	if run.Completeness != "complete" || base.Completeness != "complete" {
		addUnknown("conditions_incomplete")
	}

	switch {
	case incompatible:
		return HlwyComparabilityIncompatible, codes
	case different:
		return HlwyComparabilityDifferent, codes
	case unknown:
		return HlwyComparabilityUnknown, codes
	default:
		return HlwyComparabilityMatched, codes
	}
}

// HlwySnapshotOf 生成一份深拷贝的基准快照。
//
// 深拷贝不是洁癖：快照会落库、会作为下一次离线重算的输入。只要还和内存里的
// slice/指针共享底层数组，任何一处误改就会同时污染「历史证据」和「当前目录」。
func HlwySnapshotOf(b numberdist.Baseline) numberdist.Baseline {
	out := b
	out.RawNumbers = append([]int(nil), b.RawNumbers...)
	if b.CollectedAt != nil {
		t := b.CollectedAt.UTC()
		out.CollectedAt = &t
	}
	out.Conditions = hlwyCloneConditions(b.Conditions)
	return out
}

// hlwyCloneConditions 深拷贝条件快照：可空数值/布尔与 unknown_fields 都不能共享。
func hlwyCloneConditions(c numberdist.Conditions) numberdist.Conditions {
	out := c
	if c.Temperature != nil {
		v := *c.Temperature
		out.Temperature = &v
	}
	if c.TopP != nil {
		v := *c.TopP
		out.TopP = &v
	}
	if c.MaxTokens != nil {
		v := *c.MaxTokens
		out.MaxTokens = &v
	}
	if c.Stream != nil {
		v := *c.Stream
		out.Stream = &v
	}
	if c.SystemInjected != nil {
		v := *c.SystemInjected
		out.SystemInjected = &v
	}
	if c.ToolsInjected != nil {
		v := *c.ToolsInjected
		out.ToolsInjected = &v
	}
	out.UnknownFields = append([]string(nil), c.UnknownFields...)
	return out
}

// hlwyKnown 判断一个可空字符串字段是否真的观测到过值。
// 字符串 "unknown" 与空值同义：它是「没观测到」的记录，不是观测到的取值。
func hlwyKnown(v string) bool {
	t := strings.TrimSpace(v)
	return t != "" && !strings.EqualFold(t, numberdist.StateUnknown)
}

// HlwyPlainOf 生成对照模式的 headline/summary。
//
// 关键差别：这里**不**套 PlainVerdictFor(unknown) 的「没查清」标题。
// 一次正常的对照，verdict 注定是 unknown（本模式不计风险），
// 用旧的 unknown 措辞会把「对照做完了」说成「检测失败」。
func HlwyPlainOf(bc HlwyBaselineComparison, selfCompare bool) HlwyPlain {
	headline := "对照完成"
	switch bc.Execution {
	case HlwyExecutionSampling:
		headline = "正在采样"
	case HlwyExecutionCanceled:
		headline = "已取消（保留已有结果）"
	case HlwyExecutionTimedOut:
		headline = "已超时结束（保留已有结果）"
	case HlwyExecutionFailed:
		headline = "对照失败"
	case HlwyExecutionInterrupted:
		headline = "执行中断（保留已有结果）"
	}

	parts := []string{
		fmt.Sprintf("本轮有效样本 %d 个（计划 %d 次采样）。",
			bc.Counts.Valid, bc.Counts.PlannedCalls),
	}
	if bc.ValidCount < numberdist.MinScorable {
		parts = append(parts, fmt.Sprintf("有效样本不足 %d 个，不给出相似度分数。",
			numberdist.MinScorable))
	} else if bc.Comparability == HlwyComparabilityIncompatible {
		// 样本够但题目/解析/协议不同：不能给分，也不能说成「样本不够」。
		parts = append(parts, "所选基准与本轮不是同一套采样规则，不给出相似度分数。")
	} else {
		parts = append(parts, fmt.Sprintf("对照基准 %s，相似度见下方分项。", bc.ChosenBaselineID))
	}
	if selfCompare {
		parts = append(parts, "这是同一批样本的自比，不构成独立验证。")
	}
	parts = append(parts, "相似度是分布描述，不判定真伪。")
	return HlwyPlain{Headline: headline, Summary: strings.Join(parts, "")}
}

func isHlwyExecution(v string) bool {
	switch v {
	case HlwyExecutionSampling, HlwyExecutionComplete, HlwyExecutionCanceled,
		HlwyExecutionTimedOut, HlwyExecutionFailed, HlwyExecutionInterrupted:
		return true
	}
	return false
}

func isFiniteFloat(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// hlwyCloseEnough 是分布桶的比对容差：上游文件与重算值走的是
// count/total 的同一算式，JSON 往返只差二进制最近邻。
func hlwyCloseEnough(a, b float64) bool {
	if math.IsNaN(a) || math.IsInf(a, 0) {
		return false
	}
	d := a - b
	return d <= 1e-9 && d >= -1e-9
}
