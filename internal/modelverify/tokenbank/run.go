package tokenbank

import (
	"context"
	"errors"
	"fmt"
)

// 探测流程（lm-detector shared/tokenizer-probe.ts probeTokenizer，并发 = 1 的形态）。
//
// 参考实现在 CLI 非并行模式下一次只发一条（concurrency=1），我们的检测任务本来就
// 串行发题（同一渠道密钥，避免抬高上游限流），所以只移植这一种节奏：
//
//  1. 先发一条基线（只有前后缀）；
//  2. 每轮按当前后验规划 1 条探测文本，直到 Done 或规划不出有增益的题；
//  3. 收尾再发一条基线：两次基线计数不同 = 上游附加的隐藏输入长度在变，结果打警告。
//
// 任一请求失败即停，返回已答复部分并带上错误（与参考实现一致）。

// ProbeOutputTokens Messages 协议必须给输出上限，参考实现取 16。
const ProbeOutputTokens = 16

// AskFunc 发一条题面，返回上游计费的输入 token 数（含缓存读写）与回显的型号。
type AskFunc func(ctx context.Context, prompt string) (inputTokens int, responseModel string, err error)

// Step 一次请求的记录。Probe 为空串表示基线。
type Step struct {
	Probe    string `json:"probe"`
	Baseline bool   `json:"baseline"`
	State    string `json:"state"` // done | failed
	Tokens   int    `json:"tokens,omitempty"`
	Error    string `json:"error,omitempty"`
}

// RunResult 一次探测的结果。
type RunResult struct {
	Steps        []Step        `json:"steps"`
	Observations []Observation `json:"observations"`
	Posterior    *Posterior    `json:"-"`
	Verdict      *Verdict      `json:"verdict,omitempty"`
	// BaselineDrift 两次基线计数不同：上游附加了长度不定的隐藏输入。
	BaselineDrift bool  `json:"baseline_drift"`
	Err           error `json:"-"`
}

// ErrNoUsage 上游没有在 usage 里报输入 token 数，本探针无法工作。
var ErrNoUsage = errors.New("上游响应的 usage 里没有输入 token 数，分词器探针需要这个计数")

// Probe 对一个上游跑自适应探测。maximum ≤ 0 用默认上限 20。
func (b *Bank) Probe(ctx context.Context, ask AskFunc, model string, maximum int) RunResult {
	if maximum <= 0 {
		maximum = Model.MaximumProbes
	}
	run := RunResult{}
	asked := map[string]bool{}

	send := func(probe string, baseline bool) bool {
		text := ""
		if !baseline {
			t, ok := b.ProbeText(probe)
			if !ok {
				run.Err = fmt.Errorf("探测文本 %s 不在分词器库中", probe)
				return false
			}
			text = t
			asked[probe] = true
		}
		step := Step{Probe: probe, Baseline: baseline}
		if err := ctx.Err(); err != nil {
			step.State, step.Error = "failed", err.Error()
			run.Steps = append(run.Steps, step)
			run.Err = err
			return false
		}
		tokens, respModel, err := ask(ctx, b.Wrap(text))
		if err == nil && tokens <= 0 {
			err = ErrNoUsage
		}
		if err != nil {
			step.State, step.Error = "failed", err.Error()
			run.Steps = append(run.Steps, step)
			run.Err = err
			return false
		}
		step.State, step.Tokens = "done", tokens
		run.Steps = append(run.Steps, step)
		run.Observations = append(run.Observations, Observation{Probe: probe, Baseline: baseline, Tokens: tokens, ResponseModel: respModel})
		post, perr := b.ComputePosterior(run.Observations)
		if perr != nil {
			run.Err = perr
			return false
		}
		run.Posterior = post
		return true
	}

	if send("", true) {
		for run.Posterior != nil && !Done(run.Posterior, maximum) {
			left := maximum - run.Posterior.Answered
			n := 1
			if left < n {
				n = left
			}
			next := b.NextProbes(run.Posterior, asked, n)
			if len(next) == 0 || !send(next[0], false) {
				break
			}
		}
		if run.Err == nil && send("", true) {
			seen := map[int]bool{}
			for _, o := range run.Observations {
				if o.Baseline {
					seen[o.Tokens] = true
				}
			}
			run.BaselineDrift = len(seen) > 1
		}
	}
	if run.Posterior != nil && run.Posterior.Answered > 0 {
		v := b.Judge(run.Posterior, model)
		run.Verdict = &v
	}
	return run
}

// Sweep 依次发全部探测文本（首尾各一条基线），供"从可信渠道录制基线"使用。
// 它不做判定：调用方用 DifferentialCounts 把结果转成一个分词器类的计数。
func (b *Bank) Sweep(ctx context.Context, ask AskFunc) RunResult {
	run := RunResult{}
	one := func(probe string, baseline bool) bool {
		text := ""
		if !baseline {
			text, _ = b.ProbeText(probe)
		}
		step := Step{Probe: probe, Baseline: baseline}
		if err := ctx.Err(); err != nil {
			step.State, step.Error = "failed", err.Error()
			run.Steps = append(run.Steps, step)
			run.Err = err
			return false
		}
		tokens, respModel, err := ask(ctx, b.Wrap(text))
		if err == nil && tokens <= 0 {
			err = ErrNoUsage
		}
		if err != nil {
			step.State, step.Error = "failed", err.Error()
			run.Steps = append(run.Steps, step)
			run.Err = err
			return false
		}
		step.State, step.Tokens = "done", tokens
		run.Steps = append(run.Steps, step)
		run.Observations = append(run.Observations, Observation{Probe: probe, Baseline: baseline, Tokens: tokens, ResponseModel: respModel})
		return true
	}
	if !one("", true) {
		return run
	}
	for _, p := range b.Probes {
		if !one(p.ID, false) {
			return run
		}
	}
	if one("", true) {
		seen := map[int]bool{}
		for _, o := range run.Observations {
			if o.Baseline {
				seen[o.Tokens] = true
			}
		}
		run.BaselineDrift = len(seen) > 1
	}
	return run
}
