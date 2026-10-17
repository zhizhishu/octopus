package fingerprint

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

// 这些测试用参考实现（lm-detector，MIT）现场跑出来的黄金数据逐层校验移植。
//
// 为什么值得这么麻烦：这些公式全是浮点连乘连加，**"看起来对"和"对"是两件事**。
// 差一个权重、少一步标准化，最终排名依然会给出一个很像样的答案，只是答案是错的。
// 分层比对的价值在于——一旦不一致，能立刻指出是解析、特征、还是合成出的问题。

// closeEnough 是浮点比较的容差。
//
// 参考实现是 JS（float64），数据源是 float32；Go 侧的运算顺序与 JS 逐条对齐，
// 所以差异应当只在 1e-9 量级。用 1e-7 留一点余量，但仍然紧到能抓住真实的公式错误
// （少一项加权就会差到 1e-2 以上）。
const closeEnough = 1e-7

func loadJSON(t *testing.T, name string, target any) {
	t.Helper()
	blob, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", name, err)
	}
	if err := json.Unmarshal(blob, target); err != nil {
		t.Fatalf("解析 %s 失败: %v", name, err)
	}
}

func requireClose(t *testing.T, label string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > closeEnough {
		t.Fatalf("%s 偏差过大: got %.12f want %.12f (差 %.3e)", label, got, want, math.Abs(got-want))
	}
}

// TestParseMatchesReference 校验整数解析规则。
func TestParseMatchesReference(t *testing.T) {
	var cases []struct {
		In     string `json:"in"`
		Got    []int  `json:"got"`
		Counts []int  `json:"counts"`
	}
	loadJSON(t, "g_parse.json", &cases)
	if len(cases) == 0 {
		t.Fatal("黄金解析用例为空")
	}
	for _, c := range cases {
		got := ParseNumbers(c.In)
		if len(got) != len(c.Got) {
			t.Fatalf("输入 %q 解析长度不符: got %v want %v", c.In, got, c.Got)
		}
		for i := range got {
			if got[i] != c.Got[i] {
				t.Fatalf("输入 %q 第 %d 位不符: got %d want %d", c.In, i, got[i], c.Got[i])
			}
		}
		counts := CountNumbers(got)
		for i := range counts {
			if counts[i] != c.Counts[i] {
				t.Fatalf("输入 %q 计数第 %d 维不符: got %d want %d", c.In, i, counts[i], c.Counts[i])
			}
		}
	}
}

// TestFeaturesMatchReference 校验两条特征块与稳健打分。
func TestFeaturesMatchReference(t *testing.T) {
	var golden struct {
		Seq        []int     `json:"seq"`
		Hellinger  []float64 `json:"hellinger"`
		Ordered    []float64 `json:"ordered"`
		Robust     []float64 `json:"robust"`
		Positional []float64 `json:"positional"`
	}
	loadJSON(t, "g_feat.json", &golden)

	hellinger := HellingerFeature(CountNumbers(golden.Seq))
	if len(hellinger) != len(golden.Hellinger) {
		t.Fatalf("Hellinger 维度不符: got %d want %d", len(hellinger), len(golden.Hellinger))
	}
	for i := range hellinger {
		requireClose(t, "hellinger", hellinger[i], golden.Hellinger[i])
	}

	ordered := OrderedBlockFeature(golden.Seq)
	if len(ordered) != len(golden.Ordered) {
		t.Fatalf("ordered 维度不符: got %d want %d", len(ordered), len(golden.Ordered))
	}
	for i := range ordered {
		requireClose(t, "ordered", ordered[i], golden.Ordered[i])
	}

	det, err := LoadDetector()
	if err != nil {
		t.Fatalf("加载检测器失败: %v", err)
	}

	positional := positionalDecision(golden.Seq, det.Ranker.Positional)
	if len(positional) != len(golden.Positional) {
		t.Fatalf("positional 维度不符: got %d want %d", len(positional), len(golden.Positional))
	}
	for i := range positional {
		requireClose(t, "positional", positional[i], golden.Positional[i])
	}
}

// TestCalibrationMatchesReference 校验温度 softmax 与归一化。
func TestCalibrationMatchesReference(t *testing.T) {
	var golden struct {
		Scores []float64 `json:"scores"`
		Probs  []float64 `json:"probs"`
	}
	loadJSON(t, "g_calib.json", &golden)
	det, err := LoadDetector()
	if err != nil {
		t.Fatalf("加载检测器失败: %v", err)
	}
	got := Calibrate(golden.Scores, det)
	if got == nil {
		t.Fatal("校准返回 nil，但黄金数据是有校准的")
	}
	if len(got) != len(golden.Probs) {
		t.Fatalf("校准维度不符: got %d want %d", len(got), len(golden.Probs))
	}
	sum := 0.0
	for i := range got {
		requireClose(t, "calibration", got[i], golden.Probs[i])
		sum += got[i]
	}
	requireClose(t, "calibration 总和", sum, 1)
}

// TestFullPipelineMatchesReference 端到端校验：真实回答 → 排名 → 校准。
//
// 这一条是真正的把关项。前面三层都对、这一层不对，说明合成权重或顺序有问题。
func TestFullPipelineMatchesReference(t *testing.T) {
	var golden []struct {
		Source        string   `json:"source"`
		Texts         []string `json:"texts"`
		ExpectedCount []int    `json:"expected_count"`
		Prediction    string   `json:"prediction"`
		Family        string   `json:"family"`
		Probability   *float64 `json:"probability"`
		Score         float64  `json:"score"`
		Top5          []struct {
			Model string  `json:"model"`
			Score float64 `json:"score"`
		} `json:"top5"`
	}
	loadJSON(t, "g_full.json", &golden)
	if len(golden) == 0 {
		t.Fatal("黄金端到端数据为空")
	}

	for _, c := range golden {
		outputs := make([]Output, len(c.Texts))
		for i, text := range c.Texts {
			outputs[i] = Output{Text: text, ExpectedCount: c.ExpectedCount[i]}
		}
		analysis, err := Analyze(outputs)
		if err != nil {
			t.Fatalf("%s: 分析失败: %v", c.Source, err)
		}
		if analysis.Prediction != c.Prediction {
			t.Fatalf("%s: 预测不符 got %q want %q（前五: %v）",
				c.Source, analysis.Prediction, c.Prediction, topModels(analysis))
		}
		if analysis.Family != c.Family {
			t.Fatalf("%s: 家族不符 got %q want %q", c.Source, analysis.Family, c.Family)
		}
		requireClose(t, c.Source+" 排名分", analysis.RankingScore, c.Score)

		for i, want := range c.Top5 {
			if i >= len(analysis.Results) {
				t.Fatalf("%s: 结果数不足", c.Source)
			}
			if analysis.Results[i].Model != want.Model {
				t.Fatalf("%s: 第 %d 名不符 got %q want %q（前五: %v）",
					c.Source, i+1, analysis.Results[i].Model, want.Model, topModels(analysis))
			}
			requireClose(t, c.Source+" 候选分", analysis.Results[i].Score, want.Score)
		}

		if c.Probability != nil {
			if analysis.Probability == nil {
				t.Fatalf("%s: 期望有置信度但拿到 nil", c.Source)
			}
			requireClose(t, c.Source+" 置信度", *analysis.Probability, *c.Probability)
		}
	}
}

func topModels(a *Analysis) []string {
	limit := 5
	if len(a.Results) < limit {
		limit = len(a.Results)
	}
	out := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		out = append(out, a.Results[i].Model)
	}
	return out
}

// TestDetectorDataIsConsistent 校验内置数据自洽（库与检测器配套、模型数一致）。
func TestDetectorDataIsConsistent(t *testing.T) {
	det, err := LoadDetector()
	if err != nil {
		t.Fatalf("加载检测器失败: %v", err)
	}
	models, err := LoadModels()
	if err != nil {
		t.Fatalf("加载模型清单失败: %v", err)
	}
	if det.BankBuiltAt != models.BuiltAt {
		t.Fatalf("库与检测器不配套: %q vs %q", models.BuiltAt, det.BankBuiltAt)
	}
	if det.SourceReferenceSHA256 != models.ReferenceSHA256 {
		t.Fatalf("参考数据指纹不一致: %q vs %q", det.SourceReferenceSHA256, models.ReferenceSHA256)
	}
	if len(det.ModelIDs) != len(models.Models) {
		t.Fatalf("模型数不一致: %d vs %d", len(det.ModelIDs), len(models.Models))
	}
	// 家族覆盖：御四家与国产常见品牌都应在库里，否则这个探针对那些渠道等于白跑。
	want := map[string]bool{
		"GPT": false, "Claude": false, "Gemini": false, "Grok": false,
		"DeepSeek": false, "GLM": false, "Kimi": false, "Qwen": false,
	}
	for _, m := range models.Models {
		if _, ok := want[m.FamilyName]; ok {
			want[m.FamilyName] = true
		}
	}
	for family, found := range want {
		if !found {
			t.Fatalf("参考库缺少家族 %s", family)
		}
	}
}

// TestSuiteIsReplayable 校验题库可原题重放。
func TestSuiteIsReplayable(t *testing.T) {
	suite, err := LoadSuite()
	if err != nil {
		t.Fatalf("加载题库失败: %v", err)
	}
	if len(suite) != 36 {
		t.Fatalf("题库应有 36 道，实际 %d", len(suite))
	}
	picked, err := PickChallenges(3)
	if err != nil {
		t.Fatalf("选题失败: %v", err)
	}
	if len(picked) != 3 {
		t.Fatalf("应选 3 道，实际 %d", len(picked))
	}
	for _, c := range picked {
		if c.Prompt == "" || c.ExpectedCount < 1 {
			t.Fatalf("题目不完整: %+v", c)
		}
		if c.PromptWithPrefix() == "" {
			t.Fatalf("题目提示词为空: %s", c.CHALLENGEID)
		}
	}
	// 同样的入参必须选出同样的题：否则"上次测出 A 这次测出 B"无法归因。
	again, _ := PickChallenges(3)
	for i := range picked {
		if picked[i].CHALLENGEID != again[i].CHALLENGEID {
			t.Fatalf("选题不可复现: %d 次不同", i)
		}
	}
}

// TestInsufficientEvidenceIsNotAGuess 校验证据不足时不出结论。
//
// 这是这个模块最重要的行为约束：宁可说"测不出来"，也不能瞎猜。
func TestInsufficientEvidenceIsNotAGuess(t *testing.T) {
	// 全篇没有合法整数。
	analysis, err := Analyze([]Output{{Text: "I cannot help with that.", ExpectedCount: 240}})
	if err != nil {
		t.Fatalf("分析失败: %v", err)
	}
	if analysis.Decision != DecisionUnscorable {
		t.Fatalf("无有效样本应判 unscorable，实际 %q", analysis.Decision)
	}
	if analysis.Prediction != "" {
		t.Fatalf("证据不足时不应给出预测，实际 %q", analysis.Prediction)
	}

	// 只有一条有效回答：给排名，但**不给**置信度。
	suite, _ := LoadSuite()
	one := make([]Output, 1)
	one[0] = Output{Text: numberSequence(240), ExpectedCount: suite[0].ExpectedCount}
	single, err := Analyze(one)
	if err != nil {
		t.Fatalf("分析失败: %v", err)
	}
	if single.Decision != DecisionPartial {
		t.Fatalf("单条样本应判 partial，实际 %q", single.Decision)
	}
	if single.Probability != nil {
		t.Fatal("单条样本不应给出置信度")
	}
}

func numberSequence(n int) string {
	out := make([]byte, 0, n*4)
	value := 1
	for i := 0; i < n; i++ {
		if i > 0 {
			out = append(out, ',')
		}
		value = value*37 + 11
		v := value%355 + 1
		for _, digit := range []byte(itoa(v)) {
			out = append(out, digit)
		}
	}
	return string(out)
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
