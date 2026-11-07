package numberdist

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLoadBuiltins(t *testing.T) {
	bs, err := LoadBuiltins()
	if err != nil {
		t.Fatalf("LoadBuiltins: %v", err)
	}
	if len(bs) != 7 {
		t.Fatalf("基准数量 = %d, want 7", len(bs))
	}
	seen := map[string]bool{}
	for _, b := range bs {
		if seen[b.ID] {
			t.Fatalf("ID 重复: %s", b.ID)
		}
		seen[b.ID] = true
		if b.SchemaVersion != SchemaVersion {
			t.Errorf("%s SchemaVersion = %d", b.ID, b.SchemaVersion)
		}
		if b.PayloadSHA256 == "" || len(b.PayloadSHA256) != 64 {
			t.Errorf("%s PayloadSHA256 非法: %q", b.ID, b.PayloadSHA256)
		}
		if b.Source.Kind != SourceBuiltin {
			t.Errorf("%s Source.Kind = %q", b.ID, b.Source.Kind)
		}
		if b.Source.Commit != "7bddead107da09ddec459522df4b74eb0c7f8ac6" {
			t.Errorf("%s Commit = %q", b.ID, b.Source.Commit)
		}
		if len(b.RawNumbers) == 0 {
			t.Errorf("%s RawNumbers 为空", b.ID)
		}
		var total int
		for _, c := range b.Counts {
			total += c
		}
		if total != len(b.RawNumbers) {
			t.Errorf("%s Counts 总和 %d != rawData %d", b.ID, total, len(b.RawNumbers))
		}
		if b.CollectedAt != nil && b.CollectedAt.Location() != time.UTC {
			t.Errorf("%s CollectedAt 非 UTC: %v", b.ID, b.CollectedAt.Location())
		}
		// 旧数据条件全部 unknown，绝不臆断 temperature。
		if b.Conditions.TemperatureState != StateUnknown || b.Conditions.Temperature != nil {
			t.Errorf("%s Conditions.Temperature 被臆断: %+v", b.ID, b.Conditions)
		}
		if b.Conditions.Completeness != "unknown" {
			t.Errorf("%s Completeness = %q", b.ID, b.Conditions.Completeness)
		}
	}
}

// LoadBuiltins 每次返回独立深拷贝，改动不污染缓存。
func TestLoadBuiltins_DeepCopy(t *testing.T) {
	first, err := LoadBuiltins()
	if err != nil {
		t.Fatalf("LoadBuiltins: %v", err)
	}
	if len(first[0].RawNumbers) == 0 {
		t.Fatal("RawNumbers 为空")
	}
	first[0].RawNumbers[0] = 999999
	first[0].Conditions.UnknownFields[0] = "hacked"

	second, err := LoadBuiltins()
	if err != nil {
		t.Fatalf("LoadBuiltins again: %v", err)
	}
	if second[0].RawNumbers[0] == 999999 {
		t.Fatal("RawNumbers 共享了底层数组")
	}
	if second[0].Conditions.UnknownFields[0] == "hacked" {
		t.Fatal("UnknownFields 共享了底层数组")
	}
}

func TestImport_ValidObject(t *testing.T) {
	// 一份最小合法基准：3 个样本，distribution = counts/3。
	raw := []byte(`{
	  "name":"toy","model":"m","apiType":"openai",
	  "timestamp":"2026-01-02T03:04:05.000Z",
	  "iterations":3,
	  "rawData":[217,217,9],
	  "distribution":[` + distJSON(3, map[int]int{217: 2, 9: 1}) + `]
	}`)
	fixed := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	b, err := Import(raw, ImportOptions{ImportedAt: fixed})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if b.ID != "hlwy-import:toy@"+hashHex(raw)[:12] {
		t.Fatalf("ID = %q", b.ID)
	}
	if b.Stats.Mode != 217 || b.Stats.ModeCount != 2 {
		t.Fatalf("Stats = %+v", b.Stats)
	}
	if !b.ImportedAt.Equal(fixed) {
		t.Fatalf("ImportedAt = %v", b.ImportedAt)
	}
	if b.CollectedAt == nil || !b.CollectedAt.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("CollectedAt = %v", b.CollectedAt)
	}
	if b.Source.Kind != SourceImported {
		t.Fatalf("Source.Kind = %q", b.Source.Kind)
	}
}

func TestImport_RejectsBad(t *testing.T) {
	goodDist := distJSON(3, map[int]int{217: 2, 9: 1})
	longName := strings.Repeat("a", MaxNameBytes+1)
	// 用 JSON 转义 \u0001，使 json.Unmarshal 解出一个真正的控制字符，才能真正触发
	// validateText 的控制字符检查（而不是被 JSON 解析器先拒绝）。
	ctrlName := `bad\u0001name`
	longModel := strings.Repeat("m", MaxModelBytes+1)
	longAPIType := strings.Repeat("o", MaxAPITypeBytes+1)
	cases := map[string]string{
		"missing_name":        `{"rawData":[217],"distribution":[` + distJSON(1, map[int]int{217: 1}) + `]}`,
		"no_raw":              `{"name":"x","distribution":[` + goodDist + `]}`,
		"out_of_range":        `{"name":"x","rawData":[217,400],"distribution":[` + goodDist + `]}`,
		"non_integer":         `{"name":"x","rawData":[217,1.5],"distribution":[` + goodDist + `]}`,
		"bad_iter":            `{"name":"x","iterations":9,"rawData":[217,217,9],"distribution":[` + goodDist + `]}`,
		"bins_mismatch":       `{"name":"x","rawData":[217,217,9],"distribution":[` + distJSON(3, map[int]int{217: 3}) + `]}`,
		"short_bins":          `{"name":"x","rawData":[217],"distribution":[0.0,1.0]}`,
		"empty":               `   `,
		"array_wrapped":       `[{"name":"x","rawData":[217],"distribution":[` + distJSON(1, map[int]int{217: 1}) + `]}]`,
		"long_name":           `{"name":"` + longName + `","rawData":[217],"distribution":[` + distJSON(1, map[int]int{217: 1}) + `]}`,
		"ctrl_name":           `{"name":"` + ctrlName + `","rawData":[217],"distribution":[` + distJSON(1, map[int]int{217: 1}) + `]}`,
		"long_model":          `{"name":"x","model":"` + longModel + `","rawData":[217],"distribution":[` + distJSON(1, map[int]int{217: 1}) + `]}`,
		"long_apitype":        `{"name":"x","apiType":"` + longAPIType + `","rawData":[217],"distribution":[` + distJSON(1, map[int]int{217: 1}) + `]}`,
		"foreign_proto":       `{"name":"x","protocol_id":"hlwy-sequence-v1","rawData":[217],"distribution":[` + distJSON(1, map[int]int{217: 1}) + `]}`,
		"foreign_method":      `{"name":"x","method":"sequence-fingerprint","rawData":[217],"distribution":[` + distJSON(1, map[int]int{217: 1}) + `]}`,
		"foreign_cond_method": `{"name":"x","conditions":{"method":"sequence-fingerprint"},"rawData":[217],"distribution":[` + distJSON(1, map[int]int{217: 1}) + `]}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Import([]byte(payload), ImportOptions{}); err == nil {
				t.Fatalf("期望报错，实际通过: %s", payload)
			}
		})
	}
}

// 显式声明与本实现一致的方法元数据允许导入，且不会被写回 Conditions（仍为 unknown）。
func TestImport_MethodMeta(t *testing.T) {
	dist := `[` + distJSON(2, map[int]int{217: 2}) + `]`
	// 顶层 + conditions 两处都声明且一致 → 允许。
	okRaw := []byte(`{
		"name":"x","protocol_id":"` + ProtocolID + `","parser_version":"` + ParserVersion + `",
		"method":"` + MethodSingleNumber + `",
		"conditions":{"protocol_id":"` + ProtocolID + `","parser_version":"` + ParserVersion + `","method":"` + MethodSingleNumber + `"},
		"rawData":[217,217],"distribution":` + dist + `
	}`)
	b, err := Import(okRaw, ImportOptions{})
	if err != nil {
		t.Fatalf("一致元数据应允许: %v", err)
	}
	if b.Conditions.TemperatureState != StateUnknown || b.Conditions.Completeness != "unknown" {
		t.Fatalf("声明不应写回 Conditions: %+v", b.Conditions)
	}

	// 导入不带元数据的旧格式也允许（默认路径）。
	plain := []byte(`{"name":"x","rawData":[217,217],"distribution":` + dist + `}`)
	if _, err := Import(plain, ImportOptions{}); err != nil {
		t.Fatalf("缺失元数据应允许: %v", err)
	}
}

func TestImport_Bounds(t *testing.T) {
	raw := []byte(`{"name":"x","rawData":[217],"distribution":[` + distJSON(1, map[int]int{217: 1}) + `]}`)
	if _, err := Import(raw, ImportOptions{MaxBytes: 10}); err == nil {
		t.Fatal("超过 MaxBytes 应报错")
	}
	if _, err := Import(raw, ImportOptions{MaxNumbers: 0}); err != nil {
		// MaxNumbers<=0 用默认，不应报错
		t.Fatalf("默认上限下不应报错: %v", err)
	}
}

func TestImport_RejectsArrayForm(t *testing.T) {
	// 内置文件物理形态是单元素数组，但对外 Import 按 shared-contract-v1 只接受
	// 单个 JSON 对象；数组包裹必须在导入边界拒绝，不能“顺手也接受”。
	raw, err := os.ReadFile("data/hlwy/gpt-6-astra.json")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, err := Import(raw, ImportOptions{}); err == nil {
		t.Fatal("导入数组包裹形态应报错")
	}
	// 内部加载器仍接受该形态（否则内置无法加载）。
	if _, err := buildBaseline(raw, Source{Kind: SourceBuiltin, Path: "data/hlwy/gpt-6-astra.json"}, DefaultMaxNumbers, true); err != nil {
		t.Fatalf("内部加载器应接受数组形态: %v", err)
	}
}

func TestCompare_Insufficient(t *testing.T) {
	bs, err := LoadBuiltins()
	if err != nil {
		t.Fatalf("LoadBuiltins: %v", err)
	}
	nums := make([]int, MinScorable-1)
	for i := range nums {
		nums[i] = 217
	}
	cmp, err := Compare(nums, bs, bs[0].ID)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if cmp.Scorable {
		t.Fatal("样本不足不应可评分")
	}
	if cmp.Reason != "insufficient_samples" {
		t.Fatalf("Reason = %q", cmp.Reason)
	}
	if len(cmp.Matches) != 0 {
		t.Fatal("不可评分时不应有排名")
	}
	if cmp.ValidCount != MinScorable-1 {
		t.Fatalf("ValidCount = %d", cmp.ValidCount)
	}
}

func TestCompare_Ranking(t *testing.T) {
	bs, err := LoadBuiltins()
	if err != nil {
		t.Fatalf("LoadBuiltins: %v", err)
	}
	// 用 claude-opus-5 自身 100 个样本重放：应与自身得分最高。
	var target Baseline
	for _, b := range bs {
		if b.Label == "claude-opus-5" {
			target = b
		}
	}
	nums := append([]int(nil), target.RawNumbers...)
	cmp, err := Compare(nums, bs, target.ID)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !cmp.Scorable {
		t.Fatalf("应可评分: %q", cmp.Reason)
	}
	if len(cmp.Matches) != len(bs) {
		t.Fatalf("匹配数 = %d, want %d", len(cmp.Matches), len(bs))
	}
	if cmp.Matches[0].BaselineID != target.ID {
		t.Fatalf("第一名 = %q, want 自身 %q", cmp.Matches[0].BaselineID, target.ID)
	}
	if cmp.Matches[0].Rank != 1 || cmp.Matches[0].GapToPrevious != nil {
		t.Fatalf("第一名 rank/gap 非法: %+v", cmp.Matches[0])
	}
	if cmp.Matches[0].Overall < 0.999 {
		t.Fatalf("自身匹配 overall = %v, want ~1", cmp.Matches[0].Overall)
	}
	for i := 1; i < len(cmp.Matches); i++ {
		if cmp.Matches[i].Rank != i+1 {
			t.Fatalf("rank 不连续: %+v", cmp.Matches[i])
		}
		if cmp.Matches[i].GapToPrevious == nil {
			t.Fatalf("非第一名缺 gap: %+v", cmp.Matches[i])
		}
		if cmp.Matches[i-1].Overall < cmp.Matches[i].Overall {
			t.Fatalf("未按 overall 降序排列")
		}
		if cmp.Matches[i].GapToPrevious == nil ||
			*cmp.Matches[i].GapToPrevious < 0 {
			t.Fatalf("gap 为负或缺失")
		}
	}
}

func TestCompare_Empty(t *testing.T) {
	bs, err := LoadBuiltins()
	if err != nil {
		t.Fatalf("LoadBuiltins: %v", err)
	}
	cmp, err := Compare(nil, bs, bs[0].ID)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if cmp.Scorable || cmp.Reason != "empty_distribution" {
		t.Fatalf("空输入结果 = %+v", cmp)
	}
	// 空样本仍应保留 chosenID，且 matches 是非 nil 的空数组（非 null）。
	if cmp.ChosenBaselineID != bs[0].ID {
		t.Fatalf("空样本 loses chosenID: %q", cmp.ChosenBaselineID)
	}
	if cmp.Matches == nil {
		t.Fatal("Matches 应为非 nil 空数组")
	}
	if len(cmp.Matches) != 0 {
		t.Fatalf("空样本 Matches = %d", len(cmp.Matches))
	}
}

func TestCompare_RejectsOutOfRange(t *testing.T) {
	bs, err := LoadBuiltins()
	if err != nil {
		t.Fatalf("LoadBuiltins: %v", err)
	}
	if _, err := Compare([]int{217, 400}, bs, ""); err == nil {
		t.Fatal("越界数字应报错")
	}
}

func TestCompare_RejectsBadBaselineSet(t *testing.T) {
	bs, err := LoadBuiltins()
	if err != nil {
		t.Fatalf("LoadBuiltins: %v", err)
	}
	nums := make([]int, MinScorable)
	for i := range nums {
		nums[i] = 217
	}

	t.Run("no_baselines", func(t *testing.T) {
		if _, err := Compare(nums, nil, ""); err == nil {
			t.Fatal("空基准集合应报错")
		}
	})
	t.Run("empty_id", func(t *testing.T) {
		bad := []Baseline{bs[0]}
		bad[0].ID = ""
		if _, err := Compare(nums, bad, ""); err == nil {
			t.Fatal("空 ID 应报错")
		}
	})
	t.Run("duplicate_id", func(t *testing.T) {
		bad := []Baseline{bs[0], bs[1]}
		bad[1].ID = bad[0].ID
		if _, err := Compare(nums, bad, ""); err == nil {
			t.Fatal("重复 ID 应报错")
		}
	})
	t.Run("unknown_chosen", func(t *testing.T) {
		if _, err := Compare(nums, bs, "hlwy-builtin:does-not-exist@000000000000"); err == nil {
			t.Fatal("不存在的 chosenID 应报错")
		}
	})
	t.Run("zero_stats", func(t *testing.T) {
		bad := []Baseline{bs[0]}
		bad[0].Stats = Stats{}
		if _, err := Compare(nums, bad, ""); err == nil {
			t.Fatal("零统计应报错")
		}
	})
}

// clone 必须深拷贝所有可空 Conditions 指针，防止复用内嵌缓存。
func TestBaseline_CloneDeepCopiesPointers(t *testing.T) {
	temp, topp := 0.7, 0.9
	stream, sys, tools := true, false, true
	maxtok := 1024
	when := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	b := Baseline{
		ID:          "x",
		RawNumbers:  []int{1, 2},
		CollectedAt: &when,
		Conditions: Conditions{
			Temperature:    &temp,
			TopP:           &topp,
			Stream:         &stream,
			MaxTokens:      &maxtok,
			SystemInjected: &sys,
			ToolsInjected:  &tools,
			UnknownFields:  []string{"temperature"},
		},
	}
	c := b.clone()
	if c.Conditions.Temperature == b.Conditions.Temperature || c.Conditions.TopP == b.Conditions.TopP {
		t.Fatal("temperature/top_p 指针被共享")
	}
	if c.Conditions.Stream == b.Conditions.Stream || c.Conditions.MaxTokens == b.Conditions.MaxTokens {
		t.Fatal("stream/max_tokens 指针被共享")
	}
	if c.Conditions.SystemInjected == b.Conditions.SystemInjected || c.Conditions.ToolsInjected == b.Conditions.ToolsInjected {
		t.Fatal("system/tools 指针被共享")
	}
	if c.CollectedAt == b.CollectedAt {
		t.Fatal("CollectedAt 指针被共享")
	}
	*c.Conditions.Temperature = 0.1
	*c.Conditions.Stream = false
	*c.CollectedAt = time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	if *b.Conditions.Temperature != 0.7 || *b.Conditions.Stream != true {
		t.Fatal("改动副本污染了原指针值")
	}
	if !b.CollectedAt.Equal(when) {
		t.Fatal("改动副本污染了 CollectedAt")
	}
}

// distJSON 返回长度为 355 的归一化分布的数组体（逗号分隔的值，不含外层方括号），
// 供调用方放进 [...] 内拼进 JSON。切勿在此再加一层 []，否则会拼出 [[...]] 而无法解码。
func distJSON(total int, counts map[int]int) string {
	vals := make([]float64, DistLength)
	for num, c := range counts {
		vals[num-1] = float64(c) / float64(total)
	}
	var b strings.Builder
	for i, v := range vals {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(jsonFloat(v))
	}
	return b.String()
}

// jsonFloat 用最短可往返表示写出 float64，保证 JSON 往返后与原值逐位一致。
func jsonFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}
