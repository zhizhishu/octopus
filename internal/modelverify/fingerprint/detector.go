// Package fingerprint 是"随机数指纹"检测：让模型凭第一反应挑一串整数，
// 用这串数字的分布去比对已知模型的参考分布，从而判断这条渠道背后到底是哪个模型。
//
// 移植自 lm-detector（https://github.com/Ikaleio/lm-detector，MIT License，Copyright (c) 2026 xqy2006）。
// 数据文件（detector.bin / suite.json / models.json）同样来自该项目，见 data/README.md。
//
// 为什么值得再做一个探针：现有的 identity 探针问"你是谁"，拿到的是**口供**——
// 中转站改一个字就能骗过。指纹不是口供：模型在"凭第一反应挑数字"这种没有正确答案的
// 任务上会暴露出稳定的取值偏好，这个偏好来自训练数据与采样实现，不是它能自报的。
//
// 但它也不是"原厂证明"：结论是**参考库内的闭集排名**，不在库里的模型也会得到一个
// "最像"的候选。它的正确用法是**证伪**——声明是 A 却怎么看都不像 A，那就是问题。
package fingerprint

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sync"
)

// Detector 是冻结的排名器与校准参数。
//
// 字段与上游 shared-detector-v2 一一对应；命名保持英文是为了让两边可以逐字段对照，
// 排查移植偏差时不用再做一次心算翻译。
type Detector struct {
	Schema                string       `json:"schema"`
	BaseSHA256            string       `json:"base_sha256"`
	SourceReferenceSHA256 string       `json:"source_reference_sha256"`
	BankBuiltAt           string       `json:"bank_built_at"`
	ModelIDs              []string     `json:"model_ids"`
	ResponseCounts        []int        `json:"response_counts"`
	Calibration           *Calibration `json:"calibration"`
	Ranker                Ranker       `json:"ranker"`
}

// Calibration 是参考库内的温度 softmax 参数。
//
// Tau 缺失或 binding 与检测器不匹配时**不出置信度**，只出排名——这是上游的铁律，
// 我们照搬：宁可说"排第一的是 X"，也不能拿一个对不上的校准去编造概率。
type Calibration struct {
	Schema         string  `json:"schema"`
	Method         string  `json:"method"`
	Tau            float64 `json:"tau"`
	CalibrationRun string  `json:"calibration_run"`
	Binding        Binding `json:"binding"`
}

// Binding 把校准参数钉死在某一份数据上：数据一换，校准自动失效。
type Binding struct {
	BaseSHA256      string   `json:"base_sha256"`
	ReferenceSHA256 string   `json:"reference_sha256"`
	ModelIDs        []string `json:"model_ids"`
}

// Ranker 是两路打分的全部参数。
type Ranker struct {
	HeadParams []Preprocessing `json:"head_params"`
	FullParams []Preprocessing `json:"full_params"`
	LDAWeights [][]float64     `json:"lda_weights"`
	LDABias    []float64       `json:"lda_bias"`
	References [][][]float64   `json:"references"`
	Bank       Bank            `json:"bank"`
	Blend      Blend           `json:"blend"`
	Positional PositionalHead  `json:"positional"`
}

// Preprocessing 是逐维标准化参数。
type Preprocessing struct {
	Mean  []float64 `json:"mean"`
	Scale []float64 `json:"scale"`
}

// Blend 是频率排名与位置/顺序判别的合成权重。
type Blend struct {
	Frequency  float64 `json:"frequency"`
	Positional float64 `json:"positional"`
}

// Bank 是"去干扰"所需的模型中心与干扰基。
type Bank struct {
	Hellinger     FeatureBank   `json:"hellinger"`
	OrderedBlocks OrderedBlocks `json:"ordered_blocks"`
}

// FeatureBank 是一个特征块的中心/尺度/干扰基/模型中心。
type FeatureBank struct {
	FeatureMean   []float64   `json:"feature_mean"`
	FeatureScale  []float64   `json:"feature_scale"`
	NuisanceBasis [][]float64 `json:"nuisance_basis"`
	Centroids     [][]float64 `json:"centroids"`
}

// OrderedBlocks 比 FeatureBank 多一层"每个提示环境各自的模型模板"。
//
// 提示环境的措辞会让同一模型的分布整体偏移；建库时对 12 种环境各存一份模板，
// 打分时取"最像的那个环境"，从而把这层偏移吸收掉，而不是误判成换了模型。
type OrderedBlocks struct {
	FeatureMean          []float64     `json:"feature_mean"`
	FeatureScale         []float64     `json:"feature_scale"`
	NuisanceBasis        [][]float64   `json:"nuisance_basis"`
	Centroids            [][]float64   `json:"centroids"`
	EnvironmentCentroids [][][]float64 `json:"environment_centroids"`
}

// PositionalHead 是位置/顺序判别的线性头（751 维输入）。
type PositionalHead struct {
	Params  []Preprocessing `json:"params"`
	Weights [][]float64     `json:"weights"`
	Bias    []float64       `json:"bias"`
}

// detectorBlob 是 detector.bin 的原始字节，由 embed 或测试注入。
//
// 解码只做一次：35 万次级的浮点搬运不该在每次检测里重复。
var (
	detectorOnce   sync.Once
	detectorCached *Detector
	detectorErr    error
)

// LoadDetector 解码内置的检测器数据。并发安全，只解一次。
func LoadDetector() (*Detector, error) {
	detectorOnce.Do(func() {
		blob, err := readDetectorBlob()
		if err != nil {
			detectorErr = err
			return
		}
		detectorCached, detectorErr = DecodeDetector(blob)
	})
	return detectorCached, detectorErr
}

// DecodeDetector 解析 shared-detector-v2 二进制。
//
// 布局（与上游 export.py 一致）：u32 小端头长度 → UTF-8 JSON 头 → 补零到 4 字节对齐
// → float32 数值区。**数值区按字节位分组存放**（先所有第 0 字节，再所有第 1 字节…），
// 这样 zstd 压缩率远高于交错的浮点。解码必须严格逆着这个转置，否则读出来全是噪声。
func DecodeDetector(blob []byte) (*Detector, error) {
	if len(blob) < 8 {
		return nil, fmt.Errorf("检测器数据过短: %d 字节", len(blob))
	}
	headerLen := int(binary.LittleEndian.Uint32(blob[0:4]))
	start := ((4 + headerLen) + 3) / 4 * 4
	if headerLen <= 0 || 4+headerLen > len(blob) || start > len(blob) {
		return nil, fmt.Errorf("检测器头长度非法: %d（总长 %d）", headerLen, len(blob))
	}
	count := (len(blob) - start) / 4
	if count <= 0 {
		return nil, fmt.Errorf("检测器没有数值区")
	}

	raw := make([]byte, count*4)
	for i := 0; i < count; i++ {
		for b := 0; b < 4; b++ {
			raw[i*4+b] = blob[start+b*count+i]
		}
	}
	floats := make([]float64, count)
	for i := 0; i < count; i++ {
		// 上游存的是 float32；这里同样先按 float32 取值再提升，
		// 保证与参考实现的精度完全一致（多做一步 float64 运算就会引入偏差）。
		floats[i] = float64(math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4 : i*4+4])))
	}

	var root any
	if err := json.Unmarshal(blob[4:4+headerLen], &root); err != nil {
		return nil, fmt.Errorf("检测器头不是合法 JSON: %w", err)
	}
	resolved, err := resolveFloatRefs(root, floats)
	if err != nil {
		return nil, err
	}
	// 解析后的结构再走一次 JSON 往返，复用上面的强类型定义，
	// 避免为 20 多种嵌套形状手写 20 个取值函数。
	encoded, err := json.Marshal(resolved)
	if err != nil {
		return nil, fmt.Errorf("检测器头重编码失败: %w", err)
	}
	var det Detector
	if err := json.Unmarshal(encoded, &det); err != nil {
		return nil, fmt.Errorf("检测器头结构不符: %w", err)
	}
	if det.Schema != "shared-detector-v2" {
		return nil, fmt.Errorf("检测器版本不支持: %q（期望 shared-detector-v2）", det.Schema)
	}
	if len(det.ModelIDs) == 0 {
		return nil, fmt.Errorf("检测器没有任何模型")
	}
	if len(det.ModelIDs) != len(det.ResponseCounts) {
		return nil, fmt.Errorf("检测器模型数与样本数不一致: %d vs %d", len(det.ModelIDs), len(det.ResponseCounts))
	}
	return &det, nil
}

// resolveFloatRefs 把 {"$f32":offset,"n":len} 就地换成真实数值数组。
func resolveFloatRefs(node any, floats []float64) (any, error) {
	switch v := node.(type) {
	case map[string]any:
		if off, ok := v["$f32"]; ok {
			n, ok2 := v["n"]
			if !ok2 {
				return nil, fmt.Errorf("$f32 引用缺少 n")
			}
			offset, ok3 := toInt(off)
			length, ok4 := toInt(n)
			if !ok3 || !ok4 {
				return nil, fmt.Errorf("$f32 引用参数不是整数")
			}
			if offset < 0 || length < 0 || offset+length > len(floats) {
				return nil, fmt.Errorf("$f32 引用越界: offset=%d n=%d 总数=%d", offset, length, len(floats))
			}
			out := make([]any, length)
			for i := 0; i < length; i++ {
				out[i] = floats[offset+i]
			}
			return out, nil
		}
		for k, child := range v {
			next, err := resolveFloatRefs(child, floats)
			if err != nil {
				return nil, err
			}
			v[k] = next
		}
		return v, nil
	case []any:
		for i, child := range v {
			next, err := resolveFloatRefs(child, floats)
			if err != nil {
				return nil, err
			}
			v[i] = next
		}
		return v, nil
	default:
		return node, nil
	}
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	default:
		return 0, false
	}
}

// readDetectorBlob 由 data_detector.go 的 go:embed 提供。
var readDetectorBlob = func() ([]byte, error) {
	return nil, fmt.Errorf("检测器数据未打包")
}

// setDetectorBlobReader 供测试注入。
func setDetectorBlobReader(fn func() ([]byte, error)) {
	readDetectorBlob = fn
	detectorOnce = sync.Once{}
	detectorCached = nil
	detectorErr = nil
}

// readFileIfExists 是给 data 加载用的小工具。
func readFileIfExists(path string) ([]byte, error) {
	return os.ReadFile(path)
}
