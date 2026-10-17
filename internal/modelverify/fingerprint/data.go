package fingerprint

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
)

// 数据文件来自 lm-detector（MIT License, Copyright (c) 2026 xqy2006），见 data/README.md。
//
//go:embed data/detector.bin
var detectorBlob []byte

//go:embed data/suite.json
var suiteBlob []byte

//go:embed data/models.json
var modelsBlob []byte

func init() {
	// 让 LoadDetector 用打包进来的数据。
	setDetectorBlobReader(func() ([]byte, error) {
		if len(detectorBlob) == 0 {
			return nil, fmt.Errorf("检测器数据为空")
		}
		return detectorBlob, nil
	})
}

// Model 是候选模型的人话信息。
type Model struct {
	ID            string `json:"id"`
	DisplayName   string `json:"display_name"`
	Family        string `json:"family"`
	FamilyName    string `json:"family_name"`
	ResponseCount int    `json:"response_count"`
}

// Models 是裁剪后的模型清单，含库与检测器的配套校验信息。
type Models struct {
	Schema              string  `json:"schema"`
	BuiltAt             string  `json:"built_at"`
	ReferenceSHA256     string  `json:"reference_sha256"`
	MinimumValidNumbers int     `json:"minimum_valid_numbers"`
	RecommendedQueries  int     `json:"recommended_queries"`
	Models              []Model `json:"models"`
}

// Challenge 是一道固定挑战题。
//
// 题目必须**原题重放**：参考库的分布是在这 36 道题的具体措辞下采集的，
// 换一句话都会让分布偏移，指纹也就不可比了。
type Challenge struct {
	Condition     string `json:"condition"`
	System        string `json:"system"`
	UserPrefix    string `json:"user_prefix"`
	CHALLENGEID   string `json:"challenge_id"`
	ExpectedCount int    `json:"expected_count"`
	Prompt        string `json:"prompt"`
}

// PromptWithPrefix 返回实际发送的提示词。
func (c Challenge) PromptWithPrefix() string {
	if c.UserPrefix == "" {
		return c.Prompt
	}
	return c.UserPrefix + "\n\nFinal task:\n" + c.Prompt
}

var (
	modelsOnce   sync.Once
	modelsCached *Models
	modelsErr    error

	suiteOnce   sync.Once
	suiteCached []Challenge
	suiteErr    error
)

// LoadModels 解析模型清单。
func LoadModels() (*Models, error) {
	modelsOnce.Do(func() {
		var m Models
		if err := json.Unmarshal(modelsBlob, &m); err != nil {
			modelsErr = fmt.Errorf("模型清单解析失败: %w", err)
			return
		}
		if len(m.Models) == 0 {
			modelsErr = fmt.Errorf("模型清单为空")
			return
		}
		modelsCached = &m
	})
	return modelsCached, modelsErr
}

// LoadSuite 解析 36 道挑战题。
func LoadSuite() ([]Challenge, error) {
	suiteOnce.Do(func() {
		var list []Challenge
		if err := json.Unmarshal(suiteBlob, &list); err != nil {
			suiteErr = fmt.Errorf("题库解析失败: %w", err)
			return
		}
		if len(list) == 0 {
			suiteErr = fmt.Errorf("题库为空")
			return
		}
		suiteCached = list
	})
	return suiteCached, suiteErr
}

// PickChallenges 从题库里均匀取 count 道。
//
// 取法与上游 createManifest 完全一致（index * len / count），这样选中哪几道
// 是可以复现的——否则"上次测出来是 A、这次是 B"就无法归因是模型变了还是题目变了。
//
// 为什么默认 3 道：置信度校准是在"三条有效回答"的前提下拟合的。少于 3 条时
// 上游只给"部分样本排名"，不给概率。我们保留同样的克制。
func PickChallenges(count int) ([]Challenge, error) {
	suite, err := LoadSuite()
	if err != nil {
		return nil, err
	}
	if count < 1 {
		count = 1
	}
	if count > len(suite) {
		count = len(suite)
	}
	out := make([]Challenge, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, suite[i*len(suite)/count])
	}
	return out, nil
}
