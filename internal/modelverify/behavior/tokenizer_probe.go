package behavior

import (
	"context"
	"fmt"
	"sync"

	"github.com/zhizhishu/octopus/internal/modelverify"
	"github.com/zhizhishu/octopus/internal/modelverify/tokenbank"
)

// 分词器差分计数探针（移植 lm-detector 的 tokenizer probe，见 tokenbank 包）。
//
// 做法：每条请求只有一条 user 消息 = 固定前缀 + 短探测文本 + 固定后缀；另发一条只有
// 前后缀的基线。x_j − x_0 = count(P t S) − count(P S)：脚手架模板、渠道注入的系统
// 提示、特殊 token 全部相减抵消，剩下的只取决于上游分词器怎么切这段文本。再拿这串
// 差分去和 290 个开放分词器离线算好的 62 类计数比，做开放集贝叶斯判定，并核对声明的
// 型号该用哪一类分词器。
//
// 与已有 token_delta 的关系：token_delta 是同一思路的单点粗版（一串重复词、两个手写
// 区间）；本探针是参考项目的完整版（72 条覆盖多文字系统的文本、离线库、后验）。两者
// 同时跑时，以本探针为准（见 score.go 的去重），避免同一语义两处计分。
//
// 默认**不启用**，与参考实现一致（CLI 需显式 --tokenizer）：一次大约多发 12 条、
// 至多 22 条极短请求。只能证伪不能认证：一个分词器常被多个模型共用，
// 中转站用 tiktoken 本地估算用量也会测成 o200k/cl100k。
const (
	// ProbeTokenizerID 分词器识别（自适应，约 12 条请求）。
	ProbeTokenizerID ProbeID = "tokenizer_id"
	// ProbeTokenizerBaseline 在可信渠道上全量录制分词器基线（74 条请求，不计分）。
	ProbeTokenizerBaseline ProbeID = "tokenizer_baseline"
)

var (
	tokenizerBaselineMu  sync.RWMutex
	tokenizerBaselineSrc func(ctx context.Context) ([]tokenbank.LocalBaseline, error)
)

// SetTokenizerBaselineSource 注册本地可信基线的读取函数（由持久层在启动时注入）。
// 不注册时探针只用内置库，与参考实现行为一致。
func SetTokenizerBaselineSource(fn func(ctx context.Context) ([]tokenbank.LocalBaseline, error)) {
	tokenizerBaselineMu.Lock()
	tokenizerBaselineSrc = fn
	tokenizerBaselineMu.Unlock()
}

func loadTokenizerBaselines(ctx context.Context) ([]tokenbank.LocalBaseline, error) {
	tokenizerBaselineMu.RLock()
	fn := tokenizerBaselineSrc
	tokenizerBaselineMu.RUnlock()
	if fn == nil {
		return nil, nil
	}
	return fn(ctx)
}

// tokenizerAsk 把 Sender 包成 tokenbank.AskFunc：取上游计费的全部输入 token。
// Messages 协议必须带输出上限（参考实现取 16）；其它协议与参考实现一样不设上限，
// 交给出站链路按渠道契约决定。
func tokenizerAsk(s modelverify.Sender) tokenbank.AskFunc {
	anthropic := false
	if p, ok := s.(ProtocolNamer); ok && p.Protocol() == "anthropic-messages" {
		anthropic = true
	}
	return func(ctx context.Context, prompt string) (int, string, error) {
		ask := modelverify.Ask{Prompt: prompt}
		if anthropic {
			ask.MaxTokens = intPtr(tokenbank.ProbeOutputTokens)
		}
		resp, err := s.Ask(ctx, ask)
		if err != nil {
			return 0, "", err
		}
		tokens := resp.Usage.Input
		if tokens <= 0 {
			// 老的 Sender 实现可能只填 Prompt：退回它，仍为 0 则由 tokenbank 报"没有用量"。
			tokens = resp.Usage.Prompt
		}
		return tokens, "", nil
	}
}

// tokenizerBank 内置库 + 本地可信基线。单条基线失效（库已更新、数据不全）时逐条剔除
// 并记警告，不让一条坏基线拖垮整个探针。
func tokenizerBank(ctx context.Context) (*tokenbank.Bank, []string, error) {
	bank, err := tokenbank.Builtin()
	if err != nil {
		return nil, nil, fmt.Errorf("加载分词器库失败: %w", err)
	}
	locals, err := loadTokenizerBaselines(ctx)
	if err != nil {
		return bank, []string{fmt.Sprintf("读取本地可信基线失败，只用内置库: %v", err)}, nil
	}
	var warnings []string
	var valid []tokenbank.LocalBaseline
	for _, l := range locals {
		if _, err := bank.WithLocal(append(append([]tokenbank.LocalBaseline(nil), valid...), l)); err != nil {
			warnings = append(warnings, fmt.Sprintf("跳过本地基线 %s: %v", l.Model, err))
			continue
		}
		valid = append(valid, l)
	}
	merged, err := bank.WithLocal(valid)
	if err != nil {
		return bank, append(warnings, fmt.Sprintf("合入本地基线失败，只用内置库: %v", err)), nil
	}
	return merged, warnings, nil
}

// relayEstimateClasses 参考实现的提示：判成这两类而声明对不上时，计数可能来自
// 中转站用 tiktoken 本地估算。
var relayEstimateClasses = map[string]bool{"o200k": true, "cl100k": true}

func runTokenizerID(ctx context.Context, s modelverify.Sender, targetModel string) Result {
	res := Result{ProbeID: ProbeTokenizerID, Data: map[string]any{}}
	bank, warnings, err := tokenizerBank(ctx)
	if err != nil {
		res.Err = err
		return res
	}
	res.Data["bank_generated_at"] = bank.GeneratedAt
	res.Data["bank_classes"] = len(bank.Classes)
	if len(warnings) > 0 {
		res.Data["warnings"] = warnings
	}

	run := bank.Probe(ctx, tokenizerAsk(s), targetModel, 0)
	res.Data["requests"] = len(run.Steps)
	res.Data["baseline_drift"] = run.BaselineDrift
	obs := make([]map[string]any, 0, len(run.Observations))
	for _, o := range run.Observations {
		obs = append(obs, map[string]any{"probe": o.Probe, "baseline": o.Baseline, "tokens": o.Tokens})
	}
	res.Data["observations"] = obs

	if run.Verdict == nil {
		// 一条探测都没答复：探针没跑成，不是证据。
		if run.Err != nil {
			res.Err = fmt.Errorf("分词器探测未完成: %w", run.Err)
		} else {
			res.Err = fmt.Errorf("分词器探测没有拿到任何计数")
		}
		return res
	}
	res.OK = true
	complete := run.Err == nil
	res.Data["complete"] = complete
	if run.Err != nil {
		res.Data["error"] = run.Err.Error()
	}
	// usable：完整跑完且两次基线一致。否则只展示、不计分、不进"查过"的分母。
	res.Data["usable"] = complete && !run.BaselineDrift

	v := run.Verdict
	p := run.Posterior
	res.Data["answered"] = p.Answered
	res.Data["kind"] = v.Kind
	res.Data["top_class"] = v.Top.ID
	if cls, ok := bank.Class(v.Top.ID); ok {
		res.Data["top_series"] = cls.Series
		res.Data["top_lab"] = cls.LabName
		res.Data["top_local"] = cls.Local
	}
	res.Data["confidence"] = v.Confidence
	res.Data["unknown"] = p.Unknown
	candidates := make([]map[string]any, 0, 5)
	for i, c := range p.Classes {
		if i >= 5 {
			break
		}
		item := map[string]any{"id": c.ID, "exact": c.Exact, "related": c.Related, "matched": c.Matched, "compared": c.Compared}
		if cls, ok := bank.Class(c.ID); ok {
			item["series"] = cls.Series
			item["lab"] = cls.LabName
		}
		candidates = append(candidates, item)
	}
	res.Data["candidates"] = candidates
	if v.Claim != nil {
		res.Data["claim_status"] = v.Claim.Status
		res.Data["claim_probability"] = v.Claim.Probability
		res.Data["claim_expected"] = v.Claim.Expected
		res.Data["claim_vendor"] = v.Claim.Vendor
		res.Data["claim_first_party"] = v.Claim.FirstParty
		if v.Claim.Status != tokenbank.ClaimConsistent && relayEstimateClasses[v.Top.ID] {
			res.Data["relay_estimate_possible"] = true
		}
	}
	return res
}

func runTokenizerBaseline(ctx context.Context, s modelverify.Sender, targetModel string) Result {
	res := Result{ProbeID: ProbeTokenizerBaseline, Data: map[string]any{
		// 录制不是检测：不计分、不算"查过"。
		"usable": false,
	}}
	bank, err := tokenbank.Builtin()
	if err != nil {
		res.Err = fmt.Errorf("加载分词器库失败: %w", err)
		return res
	}
	res.Data["bank_generated_at"] = bank.GeneratedAt
	res.Data["target_model"] = targetModel
	run := bank.Sweep(ctx, tokenizerAsk(s))
	res.Data["requests"] = len(run.Steps)
	res.Data["baseline_drift"] = run.BaselineDrift
	counts, err := bank.DifferentialCounts(run)
	if err != nil {
		if run.Err != nil && len(run.Observations) == 0 {
			res.Err = fmt.Errorf("基线录制未完成: %w", run.Err)
			return res
		}
		res.OK = true
		res.Data["recordable"] = false
		res.Data["reason"] = err.Error()
		return res
	}
	res.OK = true
	res.Data["recordable"] = true
	res.Data["counts"] = counts
	return res
}
