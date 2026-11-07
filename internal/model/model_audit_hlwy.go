package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/zhizhishu/octopus/internal/modelverify/numberdist"
)

// hlwy「1–355 单数字」基准对照（kind=hlwy_compare）的持久化契约。
//
// 三条不可动的边界：
//
//  1. 版本常量一律取自 numberdist（协议 / 结构 / 算法）。这里再写一遍字面量，
//     迟早会出现「校验放行 A 版本、报告写 B 版本」的分裂——那种分裂查起来极贵，
//     因为两侧各自都自洽。
//  2. 选项里的候选基准是**入队那一刻固化的完整快照**（含 355 桶计数、统计与
//     条件），运行期不回查可变的基准表。管理员在跑到一半时导入/改名一份基准，
//     不能悄悄换掉本轮的比较对象；历史报告也必须能复现当时的候选集合。
//  3. 所有长度都有上限。这些值最终会进数据库、进报告 JSON、进详情接口；
//     无界输入等于给线上埋一颗慢速炸弹。

const (
	// HlwyDefaultSamples / HlwyMaxSamples 是首版唯一允许的采样次数。
	//
	// 只放开 100/200 而不是「1..200 任意值」：样本量直接决定上游费用与
	// 「有效≥40 才给参考分数」这条门槛的意义，允许随手填 37 只是制造
	// 看起来像结论、其实不够的对照。
	HlwyDefaultSamples = 100
	HlwyMaxSamples     = 200

	// HlwyMaxCandidates 是一轮固化的候选基准上限：7 份内置 + 1 份本地自采。
	HlwyMaxCandidates = 8

	// HlwyMaxOptionsBytes 冻结选项（含候选快照）序列化后的上限。
	HlwyMaxOptionsBytes = 2 << 20

	// 展示字段的长度上限。
	HlwyMaxBaselineIDBytes = 128
	HlwyMaxLabelBytes      = 255
	HlwyMaxNoteBytes       = 512

	// HlwyMinCaptureSamples 把一次自采结果存成基准的最低有效样本数。
	// 门槛与「有效≥40 才给分数」是两件事：存基准要的是能当参照的那一档。
	HlwyMinCaptureSamples = 100
)

// hlwyMaxOptionsBytes 是实际生效的上限（测试可临时调小以验证拦截路径）。
var hlwyMaxOptionsBytes = HlwyMaxOptionsBytes

// HlwyOptions 是 kind=hlwy_compare 任务 OptionsJSON 的类型。
//
// 请求 DTO 只允许提交 Samples / BaselineID / ProtocolID 三个字段；Candidates
// 由服务端在入队时按当轮目录固化后写入，客户端不能注入候选——
// 否则一份自造分布就能把相似度刷到自己想要的位置。
type HlwyOptions struct {
	Samples    int    `json:"samples"`
	BaselineID string `json:"baseline_id"`
	ProtocolID string `json:"protocol_id"`
	// Candidates 是入队时固化的候选基准完整快照；其中 ID==BaselineID 的那份
	// 是主对照，其余只参与候选表展示。
	Candidates []numberdist.Baseline `json:"candidates"`
}

// Validate 校验选项的首版硬约束。任何不合规一律报错，不做就近取整或静默补齐。
func (o HlwyOptions) Validate() error {
	if o.Samples != HlwyDefaultSamples && o.Samples != HlwyMaxSamples {
		return fmt.Errorf("采样次数只能 %d 或 %d，收到 %d",
			HlwyDefaultSamples, HlwyMaxSamples, o.Samples)
	}
	if o.ProtocolID != numberdist.ProtocolID {
		return fmt.Errorf("不支持的协议 %q（只支持 %q）", o.ProtocolID, numberdist.ProtocolID)
	}
	if err := validateHlwyLen("baseline_id", o.BaselineID, HlwyMaxBaselineIDBytes); err != nil {
		return err
	}
	if o.BaselineID == "" {
		return errors.New("缺少主对照基准 id")
	}
	if len(o.Candidates) == 0 {
		return errors.New("候选基准不能为空")
	}
	if len(o.Candidates) > HlwyMaxCandidates {
		return fmt.Errorf("候选基准 %d 份超过上限 %d", len(o.Candidates), HlwyMaxCandidates)
	}
	seen := make(map[string]struct{}, len(o.Candidates))
	chosen := false
	for i, b := range o.Candidates {
		if err := ValidateHlwyBaseline(b); err != nil {
			return fmt.Errorf("候选基准 #%d 非法: %w", i, err)
		}
		if _, dup := seen[b.ID]; dup {
			return fmt.Errorf("候选基准 id 重复: %s", b.ID)
		}
		seen[b.ID] = struct{}{}
		if b.ID == o.BaselineID {
			chosen = true
		}
	}
	if !chosen {
		return fmt.Errorf("主对照基准 %q 不在候选集合里", o.BaselineID)
	}
	raw, err := json.Marshal(o)
	if err != nil {
		return fmt.Errorf("选项序列化失败: %w", err)
	}
	if len(raw) > hlwyMaxOptionsBytes {
		return fmt.Errorf("选项 %d 字节超过上限 %d", len(raw), hlwyMaxOptionsBytes)
	}
	return nil
}

// Encode 校验后序列化成落库字符串。
func (o HlwyOptions) Encode() (string, error) {
	if err := o.Validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(o)
	if err != nil {
		return "", fmt.Errorf("选项序列化失败: %w", err)
	}
	return string(raw), nil
}

// ParseHlwyOptions 解析并校验落库的选项。未知字段一律拒绝：
// 让「新版本写了旧代码不认识的字段」当场报错，而不是被静默忽略后按缺省值跑。
func ParseHlwyOptions(raw string) (HlwyOptions, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return HlwyOptions{}, errors.New("hlwy 选项为空")
	}
	// 先卡字节上限再解码：2 MiB 的选项不该先把内存吃掉再告诉调用方太大。
	if len(trimmed) > hlwyMaxOptionsBytes {
		return HlwyOptions{}, fmt.Errorf("hlwy 选项 %d 字节超过上限 %d",
			len(trimmed), hlwyMaxOptionsBytes)
	}
	var o HlwyOptions
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil {
		return HlwyOptions{}, fmt.Errorf("解析 hlwy 选项失败: %w", err)
	}
	// 尾随内容一律拒绝：{"samples":100} {"samples":200} 只能有一种解读，
	// 解码器默认只读第一个值，剩下的会被静默丢掉。
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return HlwyOptions{}, errors.New("hlwy 选项后面还有多余内容")
	}
	if err := o.Validate(); err != nil {
		return HlwyOptions{}, err
	}
	return o, nil
}

// ChosenBaseline 从冻结的候选里取出主对照基准。
func (o HlwyOptions) ChosenBaseline() (numberdist.Baseline, bool) {
	for _, b := range o.Candidates {
		if b.ID == o.BaselineID {
			return b, true
		}
	}
	return numberdist.Baseline{}, false
}

// ValidateHlwyConditions 校验条件快照的三态自洽性。
//
// 只查「有没有写错」，不查「是不是齐了」：真实本地取到的采样条件本来就可能
// 缺参数（默认温度未观测），那是要原样保留的证据，不是要拒收的错。
func ValidateHlwyConditions(c numberdist.Conditions) error {
	for _, st := range []struct {
		name  string
		state string
	}{
		{"temperature_state", c.TemperatureState},
		{"top_p_state", c.TopPState},
	} {
		switch st.state {
		case numberdist.StateUnknown, numberdist.StateAbsent, numberdist.StateValue:
		default:
			return fmt.Errorf("%s=%q 不是 unknown|absent|value", st.name, st.state)
		}
	}
	if (c.TemperatureState == numberdist.StateValue) != (c.Temperature != nil) {
		return errors.New("temperature_state 与 temperature 取值不一致")
	}
	if c.Temperature != nil && !isFiniteFloat(*c.Temperature) {
		return errors.New("temperature 不是有限数值")
	}
	if (c.TopPState == numberdist.StateValue) != (c.TopP != nil) {
		return errors.New("top_p_state 与 top_p 取值不一致")
	}
	if c.TopP != nil && !isFiniteFloat(*c.TopP) {
		return errors.New("top_p 不是有限数值")
	}
	switch c.Completeness {
	case "unknown", "partial", "complete":
	default:
		return fmt.Errorf("completeness=%q 不是 unknown|partial|complete", c.Completeness)
	}
	if c.Completeness == "complete" && len(c.UnknownFields) > 0 {
		return errors.New("标为 complete 的条件快照不应再有 unknown_fields")
	}
	for i, f := range c.UnknownFields {
		if strings.TrimSpace(f) == "" {
			return fmt.Errorf("unknown_fields[%d] 为空", i)
		}
	}
	return nil
}

// ValidateHlwyBaseline 校验一份基准的自洽性。
//
// 「失败/不兼容的数据不能偷偷修好」这条在这里落地：计数与统计必须与
// RawNumbers 重算的结果一致，不一致就报错，绝不换成重算值继续用——
// 那会把一份被截断/改坏的基准变成「看起来正常」的参照物。
func ValidateHlwyBaseline(b numberdist.Baseline) error {
	if err := validateHlwyLen("baseline id", b.ID, HlwyMaxBaselineIDBytes); err != nil {
		return err
	}
	if b.ID == "" {
		return errors.New("基准缺少 id")
	}
	if err := validateHlwyLen("baseline label", b.Label, HlwyMaxLabelBytes); err != nil {
		return err
	}
	if strings.TrimSpace(b.Label) == "" {
		return fmt.Errorf("基准 %s 缺少 label", b.ID)
	}
	if !isLowerHex64(b.PayloadSHA256) {
		return fmt.Errorf("基准 %s 的 payload_sha256 不是 64 位小写十六进制", b.ID)
	}
	if b.SchemaVersion != numberdist.SchemaVersion {
		return fmt.Errorf("基准 %s 的结构版本 %d 不受支持（当前 %d）",
			b.ID, b.SchemaVersion, numberdist.SchemaVersion)
	}
	switch b.Source.Kind {
	case numberdist.SourceBuiltin, numberdist.SourceImported, numberdist.SourceLocal:
	default:
		return fmt.Errorf("基准 %s 的来源类别 %q 不受支持", b.ID, string(b.Source.Kind))
	}
	if err := validateHlwyLen("source note", b.Source.Note, HlwyMaxNoteBytes); err != nil {
		return err
	}
	if err := ValidateHlwyConditions(b.Conditions); err != nil {
		return fmt.Errorf("基准 %s 的条件快照非法: %w", b.ID, err)
	}

	counts, err := HlwyBinCounts(b.RawNumbers)
	if err != nil {
		return fmt.Errorf("基准 %s: %w", b.ID, err)
	}
	if counts != b.Counts {
		return fmt.Errorf("基准 %s 的 355 桶计数与 raw_numbers 不符", b.ID)
	}
	if want := numberdist.StatsOf(b.RawNumbers); !hlwyStatsEqual(b.Stats, want) {
		return fmt.Errorf("基准 %s 的统计与 raw_numbers 不符", b.ID)
	}
	return nil
}

// HlwyPromptSHA256 返回固定题目的规范化哈希（sha256(UTF-8 Prompt)）。
//
// 采样的条件快照与自采基准的校验都用它：题目改一个字哈希就变，
// 「两批数字是同一套实验」这个前提当场失效。
func HlwyPromptSHA256() string {
	sum := sha256.Sum256([]byte(numberdist.Prompt))
	return hex.EncodeToString(sum[:])
}

// HlwyBinCounts 按原始数字重建 355 桶计数（桶 i 对应整数 i+1）。
//
// 越界/空一律报错：坏数据既不修也不丢，直接拒绝——把 356 塞进最后一桶，
// 或者悄悄丢掉一个坏样本，都会让「有效数」这个账目失去意义。
func HlwyBinCounts(numbers []int) ([numberdist.DistLength]int, error) {
	var counts [numberdist.DistLength]int
	if len(numbers) == 0 {
		return counts, errors.New("有效数字为空")
	}
	if len(numbers) > numberdist.DefaultMaxNumbers {
		return counts, fmt.Errorf("数字 %d 个超过上限 %d", len(numbers), numberdist.DefaultMaxNumbers)
	}
	for i, n := range numbers {
		if n < 1 || n > numberdist.DistLength {
			return counts, fmt.Errorf("raw_numbers[%d]=%d 超出 1..%d", i, n, numberdist.DistLength)
		}
		counts[n-1]++
	}
	return counts, nil
}

// hlwyStatsEqual 比较两份统计：整数类字段必须逐位相等，
// 均值/标准差允许浮点往返误差（不是允许不同）。
func hlwyStatsEqual(a, b numberdist.Stats) bool {
	const eps = 1e-9
	if a.Median != b.Median || a.Min != b.Min || a.Max != b.Max ||
		a.Unique != b.Unique || a.Mode != b.Mode || a.ModeCount != b.ModeCount {
		return false
	}
	if d := a.Mean - b.Mean; d > eps || d < -eps {
		return false
	}
	if d := a.StdDev - b.StdDev; d > eps || d < -eps {
		return false
	}
	return true
}

// validateHlwyLen 检查字节长度上限。
func validateHlwyLen(field, value string, max int) error {
	if len(value) > max {
		return fmt.Errorf("%s 长度 %d 字节超过上限 %d", field, len(value), max)
	}
	return nil
}

// isLowerHex64 判断是否为 64 位小写十六进制（sha256 的规范形态）。
func isLowerHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
