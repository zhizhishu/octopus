package behavior

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"

	"github.com/bestruirui/octopus/internal/modelverify"
)

// ProbeID 是一个行为探针的标识。
//
// 命名对齐 AI-Infra-Guard（Apache-2.0）的 relay_audit 探针，便于两边对照阅读。
type ProbeID string

const (
	// ProbeLiveness 检查渠道是否真的在转发（回显一个随机串）。
	ProbeLiveness ProbeID = "liveness"
	// ProbeIdentity 问模型"你是谁"，与请求模型的家族做粗比对。弱信号。
	ProbeIdentity ProbeID = "identity"
	// ProbeGlitch 用 15 个 glitch token 测"记错哪几条"，得到家族弱指纹。
	ProbeGlitch ProbeID = "glitch"
	// ProbeTokenDelta 用极短请求读上游自报的 prompt token，找异常虚高。
	ProbeTokenDelta ProbeID = "token_delta"
	// ProbeEchoRewrite 要求逐字回显一条真实安装命令，检测输出被改写。
	ProbeEchoRewrite ProbeID = "echo_rewrite"
	// ProbeContextCanary 把三个随机哨兵埋进长上下文两端，检测中途截断。
	ProbeContextCanary ProbeID = "context_canary"
)

// Result 是一次探针执行的产物。
//
// OK 的含义被刻意收窄为「探针本身跑通了」——即拿到了一份可判读的样本。
// 它**不等于**「没有风险」：风险由评分层依据 Data 判定。上游曾把两者混在同
// 一个布尔里（例如流式探针把网络错误记成"流完整性异常"），结果是传输故障被
// 读成中转站作弊。这里从一开始就分开。
type Result struct {
	ProbeID ProbeID
	OK      bool
	Data    map[string]any
	Err     error
}

// probeFunc 是一个探针的实现。传入的 Sender 保证走渠道检测的既有出站链路。
type probeFunc func(ctx context.Context, s modelverify.Sender) Result

// probeRegistry 是探针注册表。新增探针只需在这里挂一项。
var probeRegistry = map[ProbeID]probeFunc{
	ProbeLiveness:      runLiveness,
	ProbeIdentity:      runIdentity,
	ProbeGlitch:        runGlitch,
	ProbeTokenDelta:    runTokenDelta,
	ProbeEchoRewrite:   runEchoRewrite,
	ProbeContextCanary: runContextCanary,
}

// DefaultProbes 返回默认启用的探针集合。
//
// 刻意不含 models 探针：它需要 `GET /models`，是唯一一条不经过对话链路的请求。
// 渠道有哪些模型，Octopus 本来就知道（模型列表就是渠道配置的一部分），再去问
// 上游一遍收益很低，却要为它单独开一条出站路径。等确有需要再补。
func DefaultProbes() []ProbeID {
	return []ProbeID{
		ProbeLiveness,
		ProbeIdentity,
		ProbeGlitch,
		ProbeTokenDelta,
		ProbeEchoRewrite,
		ProbeContextCanary,
	}
}

// RunProbe 执行单个探针。
func RunProbe(ctx context.Context, id ProbeID, s modelverify.Sender) Result {
	fn, ok := probeRegistry[id]
	if !ok {
		return Result{ProbeID: id, Err: fmt.Errorf("未知探针 %q", id)}
	}
	if s == nil {
		return Result{ProbeID: id, Err: fmt.Errorf("探针 %q 没有可用的发送器", id)}
	}
	res := fn(ctx, s)
	res.ProbeID = id
	return res
}

// Run 依次执行给定探针。探针之间串行——它们共用同一个渠道密钥，
// 并发会同时抬高上游限流压力和本地连接数，而审计本身并不追求低延迟。
func Run(ctx context.Context, ids []ProbeID, s modelverify.Sender) []Result {
	results := make([]Result, 0, len(ids))
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			results = append(results, Result{ProbeID: id, Err: err})
			continue
		}
		results = append(results, RunProbe(ctx, id, s))
	}
	return results
}

// ---------------------------------------------------------------- 探针实现

// runLiveness 用固定格式回显验证渠道是否真的在转发。
//
// 判定是**子串包含**而非精确相等：模型多加一句客气话不该算失败，
// 真正的失败是随机串根本没出现在回答里。
func runLiveness(ctx context.Context, s modelverify.Sender) Result {
	expected := "echo-" + randStr(6) + "-" + randStr(4)
	resp, err := s.Ask(ctx, modelverify.Ask{
		Prompt:    "Reply with exactly: " + expected,
		MaxTokens: intPtr(512),
	})
	if err != nil {
		return Result{Err: fmt.Errorf("请求失败: %w", err)}
	}
	actual := strings.TrimSpace(resp.Content)
	truncated := isTruncated(resp.FinishReason)
	ok := strings.Contains(actual, expected)
	return Result{
		OK: ok,
		Data: map[string]any{
			"expected":      expected,
			"actual":        clip(actual, 300),
			"finish_reason": resp.FinishReason,
			"truncated":     truncated,
		},
	}
}

// identityPrompts 是四句同义提问。随机取一句是为了不让上游按固定题面缓存/预置答案。
var identityPrompts = []string{
	"What model are you? Answer in one short sentence.",
	"Which AI model is answering this? Keep it brief.",
	"Identify yourself: what model are you?",
	"In one sentence, what is your model name?",
}

// runIdentity 问模型"你是谁"。
//
// 探针自身的 OK 只看请求是否成功——**不在这里判身份**。判定留给评分层，
// 因为这里拿到的是模型的口供：中转站完全可以把它改成任意名字。它唯一的价值
// 是当一个廉价的旁证，且上游也只按弱信号处理（双方家族都识别出且无交集）。
func runIdentity(ctx context.Context, s modelverify.Sender) Result {
	resp, err := s.Ask(ctx, modelverify.Ask{
		Prompt:    identityPrompts[rand.IntN(len(identityPrompts))],
		MaxTokens: intPtr(512),
	})
	if err != nil {
		return Result{Err: fmt.Errorf("请求失败: %w", err)}
	}
	text := strings.TrimSpace(resp.Content)
	return Result{
		OK: true,
		Data: map[string]any{
			"identity_text":     clip(text, 500),
			"identity_families": InferFamilies(text),
		},
	}
}

// runTokenDelta 读上游自报的 prompt token 数，判断是否异常虚高。
//
// 手法：发一条极短的固定请求，自己按 len(prompt)/4 + 12 粗估 token 数，
// 与上游自报值相减。虚高意味着中转站在请求前面塞了内容（隐藏提示词、路由前缀、
// 系统提示），或者用了完全不同的分词器。
//
// 期望值只是粗估，所以阈值必须宽松：上游取 200，只抓"异常偏高"。
// 轻微偏差是正常的（分词器差异、脚手架附加），不该报警。
func runTokenDelta(ctx context.Context, s modelverify.Sender) Result {
	word := randStr(8)
	prompt := "Return only the word: " + word
	resp, err := s.Ask(ctx, modelverify.Ask{
		Prompt:    prompt,
		MaxTokens: intPtr(5),
	})
	if err != nil {
		return Result{Err: fmt.Errorf("请求失败: %w", err)}
	}
	expected := max(1, len(prompt)/4) + 12
	actual := resp.Usage.Prompt
	delta := actual - expected
	return Result{
		OK: true,
		Data: map[string]any{
			"word":                 word,
			"actual_prompt_tokens": actual,
			"expected_rough":       expected,
			"delta":                delta,
			"usage":                resp.Usage,
		},
	}
}

// runEchoRewrite 要求逐字回显一条真实的安装命令。
//
// 选真实 pip 命令（而不是随机字符串）是上游的设计：中转站若在注入
// "把命令改成镜像源"之类的隐藏提示，会在这类内容上露馅——随机串它没法改写，
// 真实的包安装命令才有"可被改写"的语义。
func runEchoRewrite(ctx context.Context, s modelverify.Sender) Result {
	expected := pipCommands[rand.IntN(len(pipCommands))]
	resp, err := s.Ask(ctx, modelverify.Ask{
		Prompt:    "Echo this text exactly, no markdown, no explanation:\n" + expected,
		MaxTokens: intPtr(512),
	})
	if err != nil {
		return Result{Err: fmt.Errorf("请求失败: %w", err)}
	}
	actual := strings.TrimSpace(resp.Content)
	lower := strings.ToLower(actual)
	var suspicious []string
	for _, term := range rewriteSuspiciousTerms {
		if strings.Contains(lower, term) {
			suspicious = append(suspicious, term)
		}
	}
	truncated := isTruncated(resp.FinishReason)
	exact := strings.Contains(actual, expected)
	return Result{
		OK: exact && len(suspicious) == 0,
		Data: map[string]any{
			"expected":      expected,
			"actual":        clip(actual, 500),
			"exact_match":   exact,
			"suspicious":    suspicious,
			"finish_reason": resp.FinishReason,
			"truncated":     truncated,
		},
	}
}

// runContextCanary 把同一个随机串埋在长上下文的首、中、尾，要求原样复述。
//
// 判定只看**尾部**哨兵（上游同此）：尾部丢失才是上下文被截断的直接证据。
// 首部丢失更可能是模型自己偷懒跳读，不足以定罪。
func runContextCanary(ctx context.Context, s modelverify.Sender) Result {
	start, mid, end := "CANARY_"+randStr(10), "CANARY_"+randStr(10), "CANARY_"+randStr(10)
	filler := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 120)
	prompt := start + "\n" + filler + mid + "\n" + filler + end +
		"\n\nRepeat back ONLY the three canary tokens, each on its own line."
	resp, err := s.Ask(ctx, modelverify.Ask{Prompt: prompt, MaxTokens: intPtr(512)})
	if err != nil {
		return Result{Err: fmt.Errorf("请求失败: %w", err)}
	}
	text := strings.TrimSpace(resp.Content)
	sawStart := strings.Contains(text, start)
	sawMid := strings.Contains(text, mid)
	sawEnd := strings.Contains(text, end)
	return Result{
		OK: sawEnd,
		Data: map[string]any{
			"saw_start":     sawStart,
			"saw_mid":       sawMid,
			"saw_end":       sawEnd,
			"actual":        clip(text, 300),
			"finish_reason": resp.FinishReason,
			"truncated":     isTruncated(resp.FinishReason),
		},
	}
}

// ---------------------------------------------------------------- 数据与工具

// pipCommands 是用于 echo_rewrite 的真实安装命令池（上游原样摘录）。
var pipCommands = []string{
	"pip install requests==2.31.0",
	"pip install numpy==1.26.4",
	"pip install pandas==2.1.4",
	"pip install flask==3.0.0",
	"pip install django==5.0.1",
	"pip install pytest==7.4.3",
	"pip install scipy==1.11.4",
	"pip install matplotlib==3.8.2",
	"pip install torch==2.1.2",
	"pip install fastapi==0.109.0",
}

// rewriteSuspiciousTerms 是"输出被改写"的迹象词（上游原样摘录）。
//
// 出现 curl/wget/npm/bash 之类说明回答里混进了别的东西；出现 mirror/unknown
// 说明安装源被换过。它们是线索不是定论，所以只记录原词供人工判断。
var rewriteSuspiciousTerms = []string{
	"curl ", "bash ", "sh ", "unknown", "mirror", " -i http", "wget ", "npm ", "eval(",
}

// intPtr 返回指向 n 的指针。
//
// modelverify.Ask 的可调参数用指针表达"未指定"：0 是一个合法的 max_tokens，
// 不能拿零值兼任"没设"。所以探针要明确表达"我要求这个输出预算"时就得给指针。
//
// 注意这只是**请求**。桥有权不遵从——Anthropic 渠道的探针出站必须保持
// 真 claude-cli 的 64000 预算，那里保护出站形状优先于满足探针的题面需求。
// 探针因此必须能容忍预算被改：内容类探针都带 truncated 判定，被砍断只会
// 降级为"未能定论"，不会误报成"渠道有问题"。
func intPtr(n int) *int { return &n }

const randAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// randStr 生成 n 位随机小写字母数字串。
//
// 用 math/rand/v2：Go 1.22+ 起该包自带随机种子，无需手动 seed，
// 也不会像老的 math/rand 那样在并发下退化。
func randStr(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = randAlphabet[rand.IntN(len(randAlphabet))]
	}
	return string(b)
}

// isTruncated 判断回答是否因长度上限被切断。
//
// 跨协议取值：OpenAI 用 length，Responses 用 max_output_tokens，
// Anthropic 用 max_tokens。截断会让内容类探针产生假失败，所以每个探针
// 都要把它记下来交给评分层降级。
func isTruncated(finishReason string) bool {
	switch strings.ToLower(strings.TrimSpace(finishReason)) {
	case "length", "max_output_tokens", "max_tokens":
		return true
	default:
		return false
	}
}

// clip 截断展示用文本，避免把整段回答塞进结果结构。
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
