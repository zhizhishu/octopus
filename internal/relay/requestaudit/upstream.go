package requestaudit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// AuditTimeout 审查调用超时（照 gpt-load internal/jev 默认 2s，工单口径）。
const AuditTimeout = 2 * time.Second

// decisionsRequest / decisionsResponse 是 OpenAI Decisions API 的最小形状
// （照 decisions-api-1128.md §1：POST /v1/decisions，字段 input+questions+model，
// predicate 题型返回答案概率）。只取需要的字段，其余原样忽略。
type decisionsRequest struct {
	Model     string              `json:"model"`
	Input     string              `json:"input"`
	Questions []decisionsQuestion `json:"questions"`
}

type decisionsQuestion struct {
	ID   string `json:"id"`
	Type string `json:"type"` // "predicate"
	Text string `json:"text"`
}

type decisionsResponse struct {
	Answers []struct {
		ID          string  `json:"id"`
		Probability float64 `json:"probability"`
	} `json:"answers"`
}

// ChannelResolver 由 relay 层注入：按配置渠道 id 取 base_url+key 并做闸门/RPM 记账。
// 放在接口后面，包本身不 import model/relay，方便单测 mock。
type ChannelResolver func(ctx context.Context, channelID int, estInputTokens int64, fn func(baseURL, key string) (int, error)) error

// NewChannelDecisionsCaller 用 resolver 构造生产 caller。
// resolver 内部必须：AdmitChannelGate 预占 RPM/TPM → 执行 fn（真正发 HTTP）→
// 成败都按「上游尝试」记账（feat/rpm-tpm 语义），2 秒超时由本函数控制。
func NewChannelDecisionsCaller(resolve ChannelResolver) DecisionsCaller {
	return func(sc SampleContext, questions []Question) (map[string]float64, error) {
		payload := decisionsRequest{
			Model:     sc.Model,
			Input:     string(sc.Sample),
			Questions: make([]decisionsQuestion, len(questions)),
		}
		for i, q := range questions {
			payload.Questions[i] = decisionsQuestion{ID: q.RuleID, Type: "predicate", Text: q.Query}
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		if payload.Model == "" {
			payload.Model = "gpt-6-luna"
		}
		var out map[string]float64
		call := func(baseURL, key string) (int, error) {
			ctx, cancel := context.WithTimeout(context.Background(), AuditTimeout)
			defer cancel()
			url := joinDecisionsURL(baseURL)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
			if err != nil {
				return 0, err
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+key)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return 0, err
			}
			defer resp.Body.Close()
			data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			if err != nil {
				return resp.StatusCode, err
			}
			if resp.StatusCode != http.StatusOK {
				return resp.StatusCode, fmt.Errorf("decisions upstream status %d", resp.StatusCode)
			}
			var parsed decisionsResponse
			if err := json.Unmarshal(data, &parsed); err != nil {
				return resp.StatusCode, fmt.Errorf("decisions response parse: %w", err)
			}
			out = make(map[string]float64, len(parsed.Answers))
			for _, a := range parsed.Answers {
				out[a.ID] = a.Probability
			}
			return resp.StatusCode, nil
		}
		estTokens := estInputTokens(len(raw))
		if err := resolve(context.Background(), sc.ChannelID, estTokens, call); err != nil {
			return nil, err
		}
		if out == nil {
			return nil, fmt.Errorf("decisions call produced no verdicts")
		}
		return out, nil
	}
}

func estInputTokens(bytesLen int) int64 {
	// 与 relay 的 len/4 估算同口径（feat/rpm-tpm 报告）。
	n := int64(bytesLen) / 4
	if n < 1 {
		n = 1
	}
	return n
}

func joinDecisionsURL(baseURL string) string {
	// 简化拼法：base_url 尾斜杠裁掉后接 /v1/decisions；渠道 base_url 已含 /v1 的
	// 情况由 relay 层 resolver 负责规整（与 raw_protocol 的 JoinOpenAIPath 同语义，
	// 那里对 systemone/decisions 的拼接规则见 raw_protocol.go:836-862）。
	base := baseURL
	for len(base) > 0 && base[len(base)-1] == '/' {
		base = base[:len(base)-1]
	}
	if len(base) >= 3 && base[len(base)-3:] == "/v1" {
		return base + "/decisions"
	}
	return base + "/v1/decisions"
}
