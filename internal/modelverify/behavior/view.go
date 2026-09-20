package behavior

import (
	"encoding/json"

	"github.com/bestruirui/octopus/internal/utils/xredact"
)

// ViewReport 是给管理面板用的只读报告。
// 不落库；Result.Err 换成字符串，避免 JSON 丢错误。
type ViewReport struct {
	RequestedModel    string        `json:"requested_model"`
	RequestedFamilies []string      `json:"requested_families,omitempty"`
	Score             int           `json:"score"`
	Verdict           Verdict       `json:"verdict"`
	Findings          []ViewFinding `json:"findings"`
	Results           []ViewResult  `json:"results"`
	Errors            []ProbeError  `json:"errors"`
	Disclaimer        string        `json:"disclaimer"`
}

// ViewFinding 是一条风险项的 JSON 形态。
type ViewFinding struct {
	Probe          ProbeID        `json:"probe"`
	Severity       Severity       `json:"severity"`
	Score          int            `json:"score"`
	Title          string         `json:"title"`
	Evidence       map[string]any `json:"evidence,omitempty"`
	Recommendation string         `json:"recommendation,omitempty"`
}

// ViewResult 是单探针原始结果的 JSON 形态。
type ViewResult struct {
	ProbeID ProbeID        `json:"probe_id"`
	OK      bool           `json:"ok"`
	Data    map[string]any `json:"data,omitempty"`
	Error   string         `json:"error,omitempty"`
}

const viewDisclaimer = "检查结果提供参考，不等同模型真实性证明；身份、词元、签名仅为线索。"

// ToView 把内存报告转成可下发的 JSON 视图，错误串走密钥脱敏。
func ToView(report Report) ViewReport {
	out := ViewReport{
		RequestedModel:    report.RequestedModel,
		RequestedFamilies: report.RequestedFamilies,
		Score:             report.Score,
		Verdict:           report.Verdict,
		Disclaimer:        viewDisclaimer,
		Findings:          make([]ViewFinding, 0, len(report.Findings)),
		Results:           make([]ViewResult, 0, len(report.Results)),
		Errors:            make([]ProbeError, 0, len(report.Errors)),
	}
	for _, f := range report.Findings {
		out.Findings = append(out.Findings, ViewFinding{
			Probe:          f.Probe,
			Severity:       f.Severity,
			Score:          f.Score,
			Title:          f.Title,
			Evidence:       jsonSafeMap(f.Evidence),
			Recommendation: f.Recommendation,
		})
	}
	for _, res := range report.Results {
		item := ViewResult{
			ProbeID: res.ProbeID,
			OK:      res.OK,
			Data:    jsonSafeMap(res.Data),
		}
		if res.Err != nil {
			item.Error = xredact.Secrets(res.Err.Error())
		}
		out.Results = append(out.Results, item)
	}
	for _, e := range report.Errors {
		out.Errors = append(out.Errors, ProbeError{
			Probe: e.Probe,
			Error: xredact.Secrets(e.Error),
		})
	}
	return out
}

func jsonSafeMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return map[string]any{"_omitted": "unserializable"}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}
