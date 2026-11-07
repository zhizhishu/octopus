package numberdist

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

// binTolerance 是「文件内 distribution 与按 rawData 重算的分布」允许的最大偏差。
// 上游以 count/total 生成本字段，JSON 往返为二进制最近邻，故可压到极紧。
const binTolerance = 1e-9

// sumTolerance 允许 distribution 总和与 1 的浮点误差。
const sumTolerance = 1e-6

var zeroTime = time.Time{}

// nowUTC 是可替换的时钟（测试可注入固定时间）。
var nowUTC = func() time.Time { return time.Now().UTC() }

// legacyStats 对应旧文件 stats 字段（camelCase）。
type legacyStats struct {
	Mean      *float64 `json:"mean"`
	StdDev    *float64 `json:"stdDev"`
	Median    *float64 `json:"median"`
	Min       *float64 `json:"min"`
	Max       *float64 `json:"max"`
	Unique    *float64 `json:"unique"`
	Mode      *float64 `json:"mode"`
	ModeCount *float64 `json:"modeCount"`
}

// legacyConditionsMeta 是旧文件 conditions 里与本协议相关的方法元数据（只读这些字段）。
type legacyConditionsMeta struct {
	ProtocolID    string `json:"protocol_id"`
	ParserVersion string `json:"parser_version"`
	Method        string `json:"method"`
}

// legacyBaseline 对应旧 hlwy 基准对象：distribution 是**小数归一化分布**（非整数样本），
// rawData 是有效的整数样本（1..355）。
//
// 方法元数据（protocol_id/parser_version/method）可能出现在顶层或 conditions 内；
// 旧文件通常缺省，缺失允许，但一旦显式声明就必须与本实现完全一致，否则拒绝。
type legacyBaseline struct {
	Name          string                `json:"name"`
	Model         string                `json:"model"`
	APIType       string                `json:"apiType"`
	ProtocolID    string                `json:"protocol_id"`
	ParserVersion string                `json:"parser_version"`
	Method        string                `json:"method"`
	Conditions    *legacyConditionsMeta `json:"conditions"`
	Timestamp     string                `json:"timestamp"`
	Iterations    *float64              `json:"iterations"`
	Distribution  []float64             `json:"distribution"`
	RawData       []json.Number         `json:"rawData"`
	Stats         *legacyStats          `json:"stats"`
}

// decodeJSON 是 json.Unmarshal 的薄封装，集中错误来源。
func decodeJSON(raw []byte, v any) error {
	return json.Unmarshal(raw, v)
}

// Import 解析一份旧 hlwy 基准（**单个对象**，per shared-contract-v1；列表包裹形态在
// 导入边界拒绝，仅内部内置文件加载器接受数组），做严格校验后返回不可变的 Baseline。
//
// 校验项：
//   - 文件大小 <= MaxBytes（默认 1 MiB）；
//   - name 非空、≤MaxNameBytes 字节、无控制字符、合法 UTF-8；model/apiType 同样约束；
//   - 显式声明的 protocol_id/parser_version/method 必须与本实现一致（缺失允许）；
//   - rawData 数量 1..MaxNumbers（默认 10000），每个都是 1..355 整数（否则报错，不过滤）；
//   - distribution 恰好 355 项、有限、非负、总和≈1，且与按 rawData 重算的分布一致。
//
// stats 一律按 rawData 重算，不信任声明值；timestamp 缺失/无效则 CollectedAt=nil。
func Import(raw []byte, opts ImportOptions) (Baseline, error) {
	opts = opts.normalize()
	if len(raw) > opts.MaxBytes {
		return Baseline{}, fmt.Errorf("基准文件 %d 字节超过上限 %d", len(raw), opts.MaxBytes)
	}
	b, err := buildBaseline(raw, opts.Source, opts.MaxNumbers, false)
	if err != nil {
		return Baseline{}, err
	}
	b.ImportedAt = opts.ImportedAt
	return b, nil
}

// buildBaseline 是共享的严格解析。maxNumbers<=0 表示用默认上限。
// allowArray 为 true 时接受只含一个对象的数组（仅内部内置数据加载器使用）；
// 对外 Import 传 false，拒绝列表包裹形态。
func buildBaseline(raw []byte, src Source, maxNumbers int, allowArray bool) (Baseline, error) {
	if maxNumbers <= 0 {
		maxNumbers = DefaultMaxNumbers
	}
	obj, err := decodeLegacy(raw, allowArray)
	if err != nil {
		return Baseline{}, err
	}

	if err := validateMethodMeta(obj); err != nil {
		return Baseline{}, err
	}

	name := strings.TrimSpace(obj.Name)
	if name == "" {
		return Baseline{}, fmt.Errorf("基准缺少 name")
	}
	if err := validateText("name", name, MaxNameBytes); err != nil {
		return Baseline{}, err
	}
	model := strings.TrimSpace(obj.Model)
	if err := validateText("model", model, MaxModelBytes); err != nil {
		return Baseline{}, err
	}
	apiType := strings.TrimSpace(obj.APIType)
	if err := validateText("apiType", apiType, MaxAPITypeBytes); err != nil {
		return Baseline{}, err
	}

	if len(obj.RawData) == 0 {
		return Baseline{}, fmt.Errorf("基准 %s 缺少 rawData", name)
	}
	if len(obj.RawData) > maxNumbers {
		return Baseline{}, fmt.Errorf("基准 %s 的 rawData %d 项超过上限 %d", name, len(obj.RawData), maxNumbers)
	}

	numbers := make([]int, 0, len(obj.RawData))
	var counts [DistLength]int
	for i, num := range obj.RawData {
		n, err := num.Int64()
		if err != nil {
			return Baseline{}, fmt.Errorf("基准 %s 的 rawData[%d]=%s 不是整数", name, i, num)
		}
		if n < 1 || n > DistLength {
			return Baseline{}, fmt.Errorf("基准 %s 的 rawData[%d]=%d 超出 1..%d", name, i, n, DistLength)
		}
		numbers = append(numbers, int(n))
		counts[n-1]++
	}

	// iterations 若声明，必须与样本数一致（上游以 results.length 记 iterations）。
	if obj.Iterations != nil {
		it := *obj.Iterations
		if math.Trunc(it) != it || int(it) != len(numbers) {
			return Baseline{}, fmt.Errorf("基准 %s 的 iterations=%v 与 rawData 数量 %d 不符", name, it, len(numbers))
		}
	}

	// 校验 distribution：355 项、有限、非负、总和≈1，且与重算一致。
	if len(obj.Distribution) != DistLength {
		return Baseline{}, fmt.Errorf("基准 %s 的 distribution 应为 %d 项，实测 %d", name, DistLength, len(obj.Distribution))
	}
	var sum float64
	total := float64(len(numbers))
	for i, v := range obj.Distribution {
		if !isFinite(v) || v < 0 {
			return Baseline{}, fmt.Errorf("基准 %s 的 distribution[%d]=%v 非法", name, i, v)
		}
		want := float64(counts[i]) / total
		if math.Abs(v-want) > binTolerance {
			return Baseline{}, fmt.Errorf("基准 %s 的 distribution[%d]=%v 与重算 %v 不符", name, i, v, want)
		}
		sum += v
	}
	if math.Abs(sum-1) > sumTolerance {
		return Baseline{}, fmt.Errorf("基准 %s 的 distribution 总和 %v 偏离 1", name, sum)
	}

	var collectedAt *time.Time
	if ts := strings.TrimSpace(obj.Timestamp); ts != "" {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			utc := t.UTC()
			collectedAt = &utc
		}
	}

	payload := hashHex(raw)
	b := Baseline{
		SchemaVersion: SchemaVersion,
		ID:            deriveID(src, name, payload),
		Label:         name,
		PayloadSHA256: payload,
		DeclaredModel: model,
		APIType:       apiType,
		CollectedAt:   collectedAt,
		ImportedAt:    zeroTime,
		Source:        src,
		Conditions:    unknownConditions(),
		RawNumbers:    numbers,
		Counts:        counts,
		Stats:         StatsOf(numbers),
	}
	return b, nil
}

// validateMethodMeta 检查显式声明的方法元数据是否与本实现一致。
//
// 规则（per plan6.3）：缺失/空允许（旧格式可 legacy 导入）；一旦非空，protocol_id 必须
// == ProtocolID、parser_version == ParserVersion、method == MethodSingleNumber，否则拒绝。
// 顶层与 conditions 内的声明都检查（两处都声明时都需一致）。
//
// 刻意不把这些声明写回 Baseline.Conditions——结构校验不能证明真实采集条件，
// 旧来源仍记为 unknown。
func validateMethodMeta(obj legacyBaseline) error {
	check := func(where, label, got, want string) error {
		v := strings.TrimSpace(got)
		if v == "" {
			return nil
		}
		if v != want {
			return fmt.Errorf("基准 %s%s 声明为 %q，与本实现 %q 不符", where, label, v, want)
		}
		return nil
	}
	if err := check("顶层", "protocol_id", obj.ProtocolID, ProtocolID); err != nil {
		return err
	}
	if err := check("顶层", "parser_version", obj.ParserVersion, ParserVersion); err != nil {
		return err
	}
	if err := check("顶层", "method", obj.Method, MethodSingleNumber); err != nil {
		return err
	}
	if c := obj.Conditions; c != nil {
		if err := check("conditions.", "protocol_id", c.ProtocolID, ProtocolID); err != nil {
			return err
		}
		if err := check("conditions.", "parser_version", c.ParserVersion, ParserVersion); err != nil {
			return err
		}
		if err := check("conditions.", "method", c.Method, MethodSingleNumber); err != nil {
			return err
		}
	}
	return nil
}

// validateText 校验文本字段：合法 UTF-8、≤maxBytes 字节、无控制字符（C0 与 DEL）。
func validateText(field, value string, maxBytes int) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("基准 %s 不是合法 UTF-8", field)
	}
	if len(value) > maxBytes {
		return fmt.Errorf("基准 %s 长度 %d 字节超过上限 %d", field, len(value), maxBytes)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("基准 %s 含控制字符", field)
		}
	}
	return nil
}

// maxIDSlugBytes 限制 ID 中 slug 段的字节数，避免把超长 name 塞进 ID。
const maxIDSlugBytes = 48

// deriveID 生成有界、内容相关的 opaque ID。
//
// builtin 用文件名 stem（不依赖可能重复的内部 name）；import/local 用有界 slug。
// 后缀是 payload sha256 前 12 位，保证同内容同来源 idempotent、不同内容不撞。
func deriveID(src Source, name, payload string) string {
	short := payload
	if len(short) > 12 {
		short = short[:12]
	}
	switch src.Kind {
	case SourceBuiltin:
		stem := strings.TrimSuffix(path.Base(src.Path), ".json")
		return "hlwy-builtin:" + stem + "@" + short
	case SourceLocal:
		return "hlwy-local:" + boundedSlug(name) + "@" + short
	default:
		return "hlwy-import:" + boundedSlug(name) + "@" + short
	}
}

// boundedSlug 截断 name 到 ≤maxIDSlugBytes 字节，并在 rune 边界收尾。
func boundedSlug(name string) string {
	name = strings.TrimSpace(name)
	if len(name) <= maxIDSlugBytes {
		return name
	}
	b := []byte(name)[:maxIDSlugBytes]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}

// decodeLegacy 返回 legacy 对象。allowArray 为 true 时接受只含一个对象的数组
// （内置文件即此形态）；为 false（对外 Import）时拒绝列表包裹，只接受单个对象。
func decodeLegacy(raw []byte, allowArray bool) (legacyBaseline, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return legacyBaseline{}, fmt.Errorf("基准内容为空")
	}
	var obj legacyBaseline
	if trimmed[0] == '[' {
		if !allowArray {
			return legacyBaseline{}, fmt.Errorf("基准顶层必须是单个对象，不接受数组包裹")
		}
		var list []json.RawMessage
		if err := decodeJSON(trimmed, &list); err != nil {
			return legacyBaseline{}, fmt.Errorf("解析基准数组失败: %w", err)
		}
		if len(list) != 1 {
			return legacyBaseline{}, fmt.Errorf("基准数组应恰好含 1 个对象，实测 %d", len(list))
		}
		if err := decodeJSON(list[0], &obj); err != nil {
			return legacyBaseline{}, fmt.Errorf("解析基准对象失败: %w", err)
		}
		return obj, nil
	}
	if err := decodeJSON(trimmed, &obj); err != nil {
		return legacyBaseline{}, fmt.Errorf("解析基准对象失败: %w", err)
	}
	return obj, nil
}
