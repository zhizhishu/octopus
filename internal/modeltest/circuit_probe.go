package modeltest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"time"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/relay"
	transformermodel "github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"github.com/zhizhishu/octopus/internal/utils/xredact"
)

// ── 断路器探测（breaker probe）──────────────────────────────────────────────
//
// 这里只做两件事：向「一个渠道 + 一个密钥 + 一个上游模型」发一道**可判对错**的最小
// 题目，然后按 relay 的既有口径把这次出站归因。什么时候探、探几次、探完怎么改熔断
// 状态，都不在这里——那是调度方的事。
//
// 铁律（都是外部可见行为，改动前先想清楚）：
//
//   - 只认调用方给的那一个渠道/密钥/上游模型：渠道不换、模型不再映射
//     （channel.ModelMapping 不参与）、密钥不轮转，也没有任何回退。
//   - 出站直接复用渠道检测的 testChannelKey，因此 shape / transport / fingerprint
//     与真实转发、渠道检测完全同一身身份，探针不加任何新的出站特征。
//   - 归因复用 relay 的既有判定（见 internal/relay/circuit_probe_seam.go），
//     不在这里另造一套"什么算渠道故障"。
//   - 判读在**脱敏之前**对上游原文做（脱敏会改写正文，见 circuitProbeHistoryAnswer）。
//   - 不写用户统计、不记账：探测不是用户请求。管理端只留一条专用标记的 Relay Log
//     （request_endpoint = CircuitProbeMarker，error_strategy 带 billable=false;
//     stats_counted=false;breaker_counted=false），不产生任何费用/统计/审计跟查。

const (
	// CircuitProbeMarker 是断路器探测的固定标记。它同时是：
	//   - 本包 stdout 日志行的前缀；
	//   - 管理端 Relay Logs 里这条探测行的 request_endpoint；
	//   - error_strategy 的第一段。
	// 后端 / UI 用这一条字符串（配合 CircuitProbeDisplayLabel 里的中文标签）把探测
	// 与用户请求分开。它是稳定的对外字符串，不要随手改。
	CircuitProbeMarker = "circuit_probe"
	// CircuitProbeDisplayLabel 是展示用的中文标签。
	CircuitProbeDisplayLabel = "探测"

	// circuitProbeRelayLogStrategy 是探测日志行的 error_strategy。
	//
	// 三个开关都有既有消费方，缺一个就会漏：
	//   - billable=false：没有计费；
	//   - stats_counted=false：与模型测试同一口径，统计/主页聚合直接跳过这一行
	//     （见 op/stats_breakdown.go 的 statsNotCountedMarker）；
	//   - breaker_counted=false：op.relayLogExcludedFromModelTelemetry 认这一段，
	//     于是模型健康遥测与"可疑日志自动跟查"(op/scheduled_audit.go)都不会把探测行
	//     当成可疑流量去再打一次上游。
	circuitProbeRelayLogStrategy = CircuitProbeMarker + ";billable=false;stats_counted=false;breaker_counted=false"

	// circuitProbeAnswerLimit 是历史答案的字符上限（按 rune 截断，不会切半个字）。
	circuitProbeAnswerLimit = 200
	// circuitProbeReasonLimit 同上，用于一句给人看的归因说明。
	circuitProbeReasonLimit = 160
	// circuitProbeKeyRedactPlaceholder 是精确剔除密钥字节后的占位符，与 xredact 的
	// URL 凭据占位符同款（不可还原，探测答案本来也不需要还原）。
	circuitProbeKeyRedactPlaceholder = "***"
	// circuitProbeRedactMinKeyRunes 是最短做精确替换的密钥长度：更短的串会把正常文字
	// 吃掉，而且这种 key 没有任何保密价值。
	circuitProbeRedactMinKeyRunes = 4
)

// 探测结论。这些字符串是给调度方/UI 分支用的稳定取值。
const (
	// CircuitProbeOutcomeOK 上游回答了，且回答里出现了正确答案（见 circuitProbeJudgeAnswer）：
	// 末位是它、或它在中间、或后面还跟着别的整数，都算对。
	CircuitProbeOutcomeOK = "ok"
	// CircuitProbeOutcomeEmbedding 嵌入探测拿到了非空且有效的向量。
	CircuitProbeOutcomeEmbedding = "embedding_ok"
	// CircuitProbeOutcomeWrongAnswer 回答里有整数，但没有一个等于正确答案。
	CircuitProbeOutcomeWrongAnswer = "wrong_answer"
	// CircuitProbeOutcomeAmbiguous 保留值：当前判读口径下已经不会产出（含正确答案的
	// 回答一律判对、不含的一律判错）。留着是为了对外取值稳定；万一将来某条路径再产出
	// 它，circuitProbeFinish 会强制 Attributable=false（歧义是我们判不出来，不是渠道的错）。
	CircuitProbeOutcomeAmbiguous = "ambiguous"
	// CircuitProbeOutcomeGibberish 回答里根本没有整数。
	CircuitProbeOutcomeGibberish = "gibberish"
	// CircuitProbeOutcomeNoAnswer 上游 2xx，但没有可用正文/向量。
	CircuitProbeOutcomeNoAnswer = "no_answer"
	// CircuitProbeOutcomeUpstreamError 上游回了非 2xx。
	CircuitProbeOutcomeUpstreamError = "upstream_error"
	// CircuitProbeOutcomeTransport 请求发出去了，但没拿到 HTTP 响应（dial/TLS/重置）。
	CircuitProbeOutcomeTransport = "transport_error"
	// CircuitProbeOutcomeTimeout 探针**自己的**死线到点：渠道没在窗口内应答。
	CircuitProbeOutcomeTimeout = "timeout"
	// CircuitProbeOutcomeCanceled 调用方取消或调用方预算到点：探测被撤掉，不是渠道故障。
	CircuitProbeOutcomeCanceled = "canceled"
	// CircuitProbeOutcomeSkipped 压根没发（渠道/密钥/模型不满足前置条件）。
	CircuitProbeOutcomeSkipped = "skipped"
)

// probeUpstreamFailure 是一次「收到了 HTTP 响应但不是 2xx」的原始观测。
//
// 只由 modeltest 自己在 captureProbeFailure 置真时抄下（见 modelRunner 的字段注释），
// 用来把上游原文交给 relay 的既有 typed 判定。它不落库、不出包、不还原成给人看的
// 错误串：body 绝不出现在返回结果、也绝不写入 Relay Log。
type probeUpstreamFailure struct {
	StatusCode       int
	Body             []byte
	RetryAfterHeader string
}

// CircuitProbeResult 是一次断路器探测的全部可交付观测。调度方按
// Recovered / Attributable / Outcome 三个字段做决定即可。
type CircuitProbeResult struct {
	// Recovered 表示上游这次交出了可用的正确答案（嵌入端点是有效向量）。
	Recovered bool
	// Attributable 表示这次结果**可以记在渠道账上**（= 渠道故障）。
	//
	// 只有渠道侧的失败才为真：答错、答非所问、上游非 2xx 且 relay 判定要计数、
	// 探针自己的死线到点。调用方取消/调用方预算到点、以及压根没发出去的前置失败
	// （渠道停用、密钥不对、模型为空、我们自己的构请求失败）一律为假。
	//
	// DisableCircuitBreaker 的渠道恒为假：生产口径下这类渠道永不累积熔断/健康度
	// 状态，探测结论同样不记在它账上。歧义（Ambiguous）同样恒为假。
	Attributable bool
	// StatusCode 是上游 HTTP 状态码（没拿到响应时为 0）。
	StatusCode int
	// RetryAfter 是上游给的 Retry-After 冷却（上游没给时为 0）。它来自 typed
	// upstreamError 的既有通道，口径与生产转发完全一致。
	RetryAfter time.Duration
	// Prompt 是这次实际发的题（有界、无凭据：题面本来就是我们自己生成的）。
	Prompt string
	// Answer 是**有界且安全**的历史答案：默认原样保留上游正文（含换行），只剔除了
	// 本次选中密钥的字节 + 走了既有通用凭据脱敏，再按 rune 截断。绝不包含上游错误体。
	Answer string
	// Duration 是这一次出站的耗时。
	Duration time.Duration
	// Outcome 见上面的 CircuitProbeOutcome* 常量。
	Outcome string
	// Reason 是一句有界、安全、只含我们自己的词汇的归因说明（不含上游原文）。
	Reason string
	// UpstreamPath / IsStream 是这次出站的事实（管理端日志用），不参与判读。
	UpstreamPath string
	IsStream     bool
	// 这一次探测针对的确切对象：渠道 ID、密钥 ID、上游模型、端点名、能力串。
	ChannelID     int
	ChannelKeyID  int
	UpstreamModel string
	Endpoint      string
	Capability    string
}

// RunCircuitProbe 向**指定的**渠道 + 密钥 + 上游模型发一道可判对错的题，并返回这次
// 出站的归因结果。签名是给调度方（熔断探测调度）用的稳定接口。
//
//   - channel/keyID/upstreamModel 原样使用：不换渠道、不轮转密钥、不再做模型映射。
//   - endpoint 与渠道检测同语义（openai_chat / openai_responses / anthropic_messages /
//     gemini_generate_content / openai_embeddings），空值按 openai_chat 处理。
//   - capability 目前只用于嵌入端点的**显式签核**：嵌入探测必须传 "embeddings"，
//     且渠道要真的声明了 embeddings 能力位，否则不发（返回 skipped）。其余端点上
//     该字段只做记录，不参与准入。
//
// 超时：调用方的 ctx 若自带死线就听它的（那样到点算调用方预算，不计渠道故障）；
// 没有死线时探针自带一个与渠道检测默认值同源的兜底死线（见 circuitProbeBudget）。
func RunCircuitProbe(ctx context.Context, channel dbmodel.Channel, keyID int, upstreamModel, endpoint, capability string) CircuitProbeResult {
	return runCircuitProbe(ctx, channel, keyID, upstreamModel, endpoint, capability, 0)
}

// runCircuitProbe 是 RunCircuitProbe 的实现，额外接受一个显式预算（0 = 用兜底值）。
// 拆出来只为让测试能在毫秒级验证"探针自己的时钟到点"这条归因，不改动对外签名。
// 真正出站的那一段在 circuitProbeAttempt 里；这里负责在回头补一条管理端日志。
func runCircuitProbe(ctx context.Context, channel dbmodel.Channel, keyID int, upstreamModel, endpoint, capability string, ownBudget time.Duration) CircuitProbeResult {
	if ctx == nil {
		ctx = context.Background()
	}
	result := circuitProbeAttempt(ctx, channel, keyID, upstreamModel, endpoint, capability, ownBudget)
	circuitProbeRecordLog(ctx, channel, result)
	return result
}

// circuitProbeAttempt 是纯发送 + 归因，不写任何日志/统计。
func circuitProbeAttempt(ctx context.Context, channel dbmodel.Channel, keyID int, upstreamModel, endpoint, capability string, ownBudget time.Duration) CircuitProbeResult {
	if ctx == nil {
		ctx = context.Background()
	}
	result := CircuitProbeResult{
		ChannelID:     channel.ID,
		ChannelKeyID:  keyID,
		UpstreamModel: strings.TrimSpace(upstreamModel),
		Capability:    strings.TrimSpace(capability),
	}

	streamingGemini := endpoint == "gemini_stream_generate_content"
	testEndpoint := endpoint
	if streamingGemini {
		testEndpoint = "gemini_generate_content"
	}
	ep, err := normalizeEndpoint(testEndpoint)
	if err != nil {
		return circuitProbeSkip(channel, result, "unsupported endpoint")
	}
	result.Endpoint = ep.name
	var stream *bool
	if ep.name == "gemini_generate_content" {
		stream = &streamingGemini
		if streamingGemini {
			result.Endpoint = endpoint
		}
	}

	// ── 前置条件：不复用 tryChannel 的路由/能力判定，只做"确切的这一个能不能发" ──
	if result.UpstreamModel == "" {
		return circuitProbeSkip(channel, result, "upstream model is required")
	}
	if channel.ID <= 0 {
		return circuitProbeSkip(channel, result, "channel has no id")
	}
	if !channel.Enabled {
		return circuitProbeSkip(channel, result, "channel disabled")
	}
	if channel.Type == outbound.OutboundTypeSystemOne {
		// 决策渠道没有聊天形状，探不出一道算术题，也就判不出对错。
		return circuitProbeSkip(channel, result, "decision-model channel has no arithmetic probe")
	}
	isEmbedding := ep.name == embeddingTestEndpointName
	if isEmbedding {
		// 嵌入探测只在调度方显式签核时跑：能力串必须是 embeddings，且渠道确实声明了
		// 这个能力位（沿用 SupportsCapability 的既有判据，空 Capabilities 按渠道类型判）。
		if result.Capability != dbmodel.CapabilityEmbeddings {
			return circuitProbeSkip(channel, result, "embedding probe requires an explicit embeddings sign-off")
		}
		if !channel.SupportsCapability(dbmodel.CapabilityEmbeddings) {
			return circuitProbeSkip(channel, result, "channel does not declare the embeddings capability")
		}
	} else if !outbound.IsChatChannelType(channel.Type) {
		return circuitProbeSkip(channel, result, fmt.Sprintf("channel type %d does not support a chat probe", channel.Type))
	}
	if strings.TrimSpace(channel.GetBaseUrl()) == "" {
		return circuitProbeSkip(channel, result, "channel has no base url")
	}
	adapter := modelTestOutboundAdapter(channel.Type, ep.name)
	if adapter == nil {
		return circuitProbeSkip(channel, result, fmt.Sprintf("channel type %d has no outbound adapter", channel.Type))
	}
	key, ok := circuitProbeKey(channel, keyID)
	if !ok {
		return circuitProbeSkip(channel, result, "exact enabled channel key not found")
	}

	// ── 题面：探针自带题库（三种题型 + 多套措辞/符号）；嵌入端点是它自己的输入形态 ──
	var prompt string
	var expected int
	if isEmbedding {
		prompt = circuitProbeEmbeddingInput()
	} else {
		prompt, expected = nextCircuitProbePrompt()
	}
	result.Prompt = prompt

	// ── 出站：完全走渠道检测那条链路（testChannelKey），不新增任何形状处理 ──
	if ownBudget <= 0 {
		ownBudget = circuitProbeBudget(ep)
	}
	callerCtx := ctx
	probeCtx, cancel := context.WithTimeout(callerCtx, ownBudget)
	defer cancel()

	runner := &modelRunner{
		request: dbmodel.ModelTestRequest{
			Endpoint: ep.name,
			Prompt:   prompt,
			Stream:   stream,
		},
		captureProbeFailure: true,
	}
	started := time.Now()
	status, parsed, sendErr := runner.testChannelKey(probeCtx, adapter, &channel, key, result.UpstreamModel)
	result.Duration = time.Since(started)
	// 出站事实（管理端日志用）。跳过/未发出时保持零值。
	result.UpstreamPath = runner.result.UpstreamPath
	result.IsStream = runner.result.IsStream

	if sendErr != nil {
		return circuitProbeFinish(channel, result, classifyCircuitProbeFailure(callerCtx, probeCtx, runner, status))
	}

	result.StatusCode = status
	if isEmbedding {
		return circuitProbeFinish(channel, result, circuitProbeEmbeddingVerdict(parsed))
	}

	// ── 判读：先判原文，再做历史答案的脱敏/截断 ──
	// 顺序不能反：脱敏会改写正文（尤其"上游把密钥回显在回答里"时），先脱敏再判读会把
	// 一个正确的回答判成答非所问。判读口径见 circuitProbeJudgeAnswer。
	text := ProbeResponseFrom(parsed).Content
	verdict := circuitProbeJudgeAnswer(text, expected)
	verdict.answer = circuitProbeHistoryAnswer(text, key.ChannelKey)
	return circuitProbeFinish(channel, result, verdict)
}

// circuitProbeBudget 是调用方没给死线时的兜底预算：与渠道检测的默认超时同源
// （resolveTimeoutSeconds 的零值语义），因此探针不会比一次渠道检测等得更久。
func circuitProbeBudget(ep testEndpoint) time.Duration {
	return time.Duration(resolveTimeoutSeconds(dbmodel.ModelTestRequest{}, ep)) * time.Second
}

// circuitProbeKey 找"指定的那一个"密钥：必须是这个渠道的、启用且带凭据的。
//
// 有意不看冷却/隔离状态：探测的目的正是"这个被熔断/冷却的 key 现在还能不能打"，
// 用可用性过滤会把要探的对象过滤掉。也绝不回退到别的 key——回退就不是探测了。
func circuitProbeKey(channel dbmodel.Channel, keyID int) (dbmodel.ChannelKey, bool) {
	if keyID <= 0 {
		return dbmodel.ChannelKey{}, false
	}
	for _, key := range channel.Keys {
		if key.ID != keyID {
			continue
		}
		if !key.Enabled || strings.TrimSpace(key.ChannelKey) == "" {
			return dbmodel.ChannelKey{}, false
		}
		return key, true
	}
	return dbmodel.ChannelKey{}, false
}

// circuitProbeEmbeddingInput 是嵌入探测的输入文本。
//
// 嵌入端点没有题面可判，只验"回了非空且有效的向量"，所以这里只要一句固定、无凭据、
// 不随渠道变化的短文本（与渠道检测里嵌入探针用题面字符串同源）。
func circuitProbeEmbeddingInput() string {
	return "octopus channel probe"
}

// ── 探测题面库（只给断路器探测用）────────────────────────────────────────────
//
// 为什么探测不用渠道检测那道固定题（nextDefaultModelTestPrompt）：探针是周期性、按渠道
// 反复发的，一道固定前缀 + 固定格式的题本身就是可识别的探针特征（上游或中转只要记住
// 那句话就能专门应答）。所以探针自带一套题库：三种题型、每种至少三套措辞与算式符号，
// 题型与措辞都随机，且不共享固定前缀，也不用 ok/ping/hi 这类探活惯用语。
//
// 边界：题库只给断路器探测。渠道检测/模型测试仍走 nextDefaultModelTestPrompt
// （863b0811 的原样行为，逐字未改）。

const (
	circuitProbeKindAddition       = "addition"
	circuitProbeKindSubtraction    = "subtraction"
	circuitProbeKindMultiplication = "multiplication"
)

// 每种题型的措辞与算式符号都写在模板里（%d 两个占位符 = 左右操作数），因此同一题
// 型内部也会换符号（+ / 加 / plus，- / − / 减 / minus，× / * / x / 乘 / times）。
var (
	circuitProbeAdditionPrompts = []string{
		"请只回答结果，不要解释：%d + %d = ?",
		"%d 加 %d 等于多少？只回答数字。",
		"求 %d + %d 的值，只回数字。",
		"%d 与 %d 的和是多少？只给数字，不要解释。",
		"Calculate %d plus %d. Answer with the number only.",
		"%d plus %d = ? Reply with digits only.",
	}
	circuitProbeSubtractionPrompts = []string{
		"计算 %d - %d，只回答差。",
		"从 %d 减去 %d 得多少？只回答数字。",
		"%d − %d = ? 只回结果，不要解释。",
		"%d 减 %d 的结果是多少？只给数字。",
		"What is %d minus %d? Respond with the number only.",
		"%d minus %d = ? Reply with the number only.",
	}
	circuitProbeMultiplicationPrompts = []string{
		"%d 乘 %d 是多少？只回答数字。",
		"计算 %d × %d，只回乘积。",
		"%d * %d = ? 只回数字，不要解释。",
		"%d x %d 等于多少？只给数字。",
		"求 %d * %d 的结果，只回数字。",
		"What is %d times %d? Answer with the number only.",
	}
)

// circuitProbeQuestion 是一道题的完整事实：题型、两个操作数、正确答案、以及真正发出去的题面。
// 暴露操作数只为让测试能逐题校验取值范围与算式正确性。
type circuitProbeQuestion struct {
	kind     string
	left     int
	right    int
	expected int
	prompt   string
}

// nextCircuitProbePrompt 生成一道随机探测题，返回题面与正确答案。
//
// 题型均匀随机；加法/减法用两个四位数（1000..9999），乘法用两个两位数（12..98）。
// 减法保证结果为正，并排除“差恰好等于减数”的组合（见 circuitProbeSubtractionOperands）。
func nextCircuitProbePrompt() (string, int) {
	question := circuitProbeQuestionWith(rand.IntN)
	return question.prompt, question.expected
}

// circuitProbeQuestionWith 用给定的 intn 生成一题（intn 语义同 math/rand.IntN）。
// 生产传全局 rand.IntN（并发安全）；测试传固定种子 rand.Rand 的 IntN，于是抽样完全可复现。
func circuitProbeQuestionWith(intn func(int) int) circuitProbeQuestion {
	switch intn(3) {
	case 0:
		left := 1000 + intn(9000)
		right := 1000 + intn(9000)
		return circuitProbeQuestion{
			kind:     circuitProbeKindAddition,
			left:     left,
			right:    right,
			expected: left + right,
			prompt:   fmt.Sprintf(circuitProbeAdditionPrompts[intn(len(circuitProbeAdditionPrompts))], left, right),
		}
	case 1:
		left, right := circuitProbeSubtractionOperands(intn)
		return circuitProbeQuestion{
			kind:     circuitProbeKindSubtraction,
			left:     left,
			right:    right,
			expected: left - right,
			prompt:   fmt.Sprintf(circuitProbeSubtractionPrompts[intn(len(circuitProbeSubtractionPrompts))], left, right),
		}
	default:
		left := 12 + intn(87)
		right := 12 + intn(87)
		return circuitProbeQuestion{
			kind:     circuitProbeKindMultiplication,
			left:     left,
			right:    right,
			expected: left * right,
			prompt:   fmt.Sprintf(circuitProbeMultiplicationPrompts[intn(len(circuitProbeMultiplicationPrompts))], left, right),
		}
	}
}

// circuitProbeSubtractionOperands 取一对 (a, b)：两个四位数、a > b、且 a != 2b。
//
// 为什么要排除 a == 2b：那种情况下 a - b 恰好等于减数 b，模型只要把算式复述一遍
// （根本没算结果）也会让判读看到“正确答案”，那是对它宽松判分口径的误用。
//
// 重试次数有上界：intn 若是测试桩（恒定返回 0），不能在这里转不出去；实在取不到就
// 用一对固定的合法值兜底（7000 - 2000 = 5000，7000 != 2*2000），保证探测永远有题可发。
func circuitProbeSubtractionOperands(intn func(int) int) (int, int) {
	for attempt := 0; attempt < 64; attempt++ {
		left := 1000 + intn(9000)
		right := 1000 + intn(9000)
		if left > right && left != 2*right {
			return left, right
		}
	}
	return 7000, 2000
}

// circuitProbeVerdict 是内部归因中间态，由 circuitProbeFinish 落到结果上。
type circuitProbeVerdict struct {
	recovered    bool
	attributable bool
	statusCode   int
	retryAfter   time.Duration
	outcome      string
	reason       string
	answer       string
}

// circuitProbeFinish 落定结论、按渠道开关收敛归因，并打一条带标记的 stdout 日志。
func circuitProbeFinish(channel dbmodel.Channel, result CircuitProbeResult, verdict circuitProbeVerdict) CircuitProbeResult {
	result.Outcome = verdict.outcome
	result.Recovered = verdict.recovered
	result.Attributable = verdict.attributable
	if result.Outcome == CircuitProbeOutcomeAmbiguous {
		// 歧义 = 我们判不出来，不是渠道的错。当前口径已不会产出它，这里再钉一次：
		// 任何将来的歧义分支都不许记在渠道账上。
		result.Attributable = false
	}
	if channel.DisableCircuitBreaker && result.Attributable {
		// 生产口径（relay.go 的 recordChannelHealth）：关掉熔断的渠道永不累积
		// 熔断/健康度状态，探测结论同样不能记在它账上。
		result.Attributable = false
	}
	if verdict.statusCode != 0 {
		result.StatusCode = verdict.statusCode
	}
	result.RetryAfter = verdict.retryAfter
	if verdict.answer != "" {
		result.Answer = verdict.answer
	}
	result.Reason = circuitProbeBounded(verdict.reason, circuitProbeReasonLimit)
	circuitProbeLog(result)
	return result
}

// circuitProbeSkip 是"压根没发"的收敛点：不是渠道故障，也不需要上游状态。
func circuitProbeSkip(channel dbmodel.Channel, result CircuitProbeResult, reason string) CircuitProbeResult {
	return circuitProbeFinish(channel, result, circuitProbeVerdict{
		outcome: CircuitProbeOutcomeSkipped,
		reason:  reason,
	})
}

// classifyCircuitProbeFailure 把一次失败按"谁的锅"归因。
//
// 顺序要紧：先分"谁的钟"，再谈渠道。
//  1. 调用方取消 / 调用方预算到点 ⇒ 探测被撤掉（不是渠道故障，outcome=canceled）。
//  2. 探针自己的死线到点 ⇒ 渠道没能在窗口内应答（渠道侧，outcome=timeout）。
//     这一条与生产不同，是刻意的：生产里超时的是**调用端**，所以不算渠道账；
//     探测里超时的是我们自己给的时间窗口，被探的渠道没交卷。
//  3. 拿到非 2xx ⇒ 交给 relay 的既有 typed 判定（400/422 请求体拒收、
//     context-window、429/5xx、上游 Retry-After）。
//  4. 请求压根没发出去（构建请求/参数覆盖/客户端构造失败）⇒ 我们自己的问题，不算渠道账。
//  5. 其余传输失败（dial/TLS/重置）⇒ 交给 relay 的既有传输判定。
//  6. 2xx 但内容不可用（解析失败/空正文）⇒ 渠道侧。
func classifyCircuitProbeFailure(callerCtx, probeCtx context.Context, runner *modelRunner, status int) circuitProbeVerdict {
	if callerCtx != nil && callerCtx.Err() != nil {
		reason := "caller canceled"
		if errors.Is(callerCtx.Err(), context.DeadlineExceeded) {
			reason = "caller deadline exceeded"
		}
		return circuitProbeVerdict{outcome: CircuitProbeOutcomeCanceled, reason: reason}
	}
	if probeCtx != nil && errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
		return circuitProbeVerdict{outcome: CircuitProbeOutcomeTimeout, attributable: true, reason: "probe deadline exceeded"}
	}
	switch {
	case runner != nil && runner.lastProbeFailure != nil:
		failure := runner.lastProbeFailure
		counted, retryAfter := relay.ClassifyCircuitProbeFailure(failure.StatusCode, failure.Body, failure.RetryAfterHeader)
		return circuitProbeVerdict{
			outcome:      CircuitProbeOutcomeUpstreamError,
			attributable: counted,
			statusCode:   failure.StatusCode,
			retryAfter:   retryAfter,
			reason:       fmt.Sprintf("upstream status %d", failure.StatusCode),
		}
	case runner != nil && !runner.probeRequestSent:
		return circuitProbeVerdict{outcome: CircuitProbeOutcomeSkipped, reason: "probe never left the gateway"}
	case status == 0:
		return circuitProbeVerdict{
			outcome:      CircuitProbeOutcomeTransport,
			attributable: relay.ClassifyCircuitProbeTransportFailure(probeSentError(runner)),
			reason:       "transport failure",
		}
	default:
		// 2xx 但没有可用内容（解析失败 / 空正文）。
		return circuitProbeVerdict{
			outcome:      CircuitProbeOutcomeNoAnswer,
			attributable: true,
			statusCode:   status,
			reason:       "upstream answered without a usable payload",
		}
	}
}

// probeSentError 取回"请求已经发出去之后"的失败本身。
//
// testChannelKey 的错误串不携带上游原文（那是安全要求），所以归因只用到错误对象本身
// 的类别（dial/TLS/重置 等）；这里从 runner 上取回它，取不到就退回一个中性错误，
// 让 relay 的既有判定按"没有状态码的传输失败"处理（= 计数）。
func probeSentError(runner *modelRunner) error {
	if runner == nil || runner.lastProbeError == nil {
		return errProbeTransportFailure
	}
	return runner.lastProbeError
}

// errProbeTransportFailure 是取不到原始错误时的中性占位：语义就是"发出去了但没拿到响应"。
var errProbeTransportFailure = errors.New("probe transport failure without an upstream response")

// circuitProbeJudgeAnswer 判读一次算术回答，**必须在脱敏/截断之前**对上游原文调用。
//
// 三种题型（加法/减法/乘法）共用一套口径：
//   - 回答里任意位置出现等于正确答案的独立整数就算对：正好是最后一个整数算对，
//     不是最后一个但确实出现过也算对（典型：“答案是 6912。”、“**6912**”、
//     “1234 + 5678 = 6912”、“先算得 5000，再确认一次是 5001”）。
//   - 算式复述同样算过：乘法回答成 “12 × 12 = 144” / “12*12=144” / “12x12=144”
//     时，乘积 144 仍是独立整数，被判为正确；加法/减法的复述同理。
//     减法只认正结果（出题侧保证 a > b），负值复述（“3000 - 8000 = -5000”）不会被
//     洗成正数去误判成功。
//   - 有整数、但没有一个等于正确答案 ⇒ WrongAnswer（“答了，但答错了”）；
//   - 一个整数都没有 ⇒ Gibberish；空回答 ⇒ NoAnswer（这两条旧口径保留）。
//
// 旧口径要求“整段回答恰好一个整数”才判对，于是模型自带算式演算一遍的正确回答
// （“1234 + 5678 = 6912”）被自己的两个加数判成了歧义——那是误伤，已改掉。
func circuitProbeJudgeAnswer(text string, expected int) circuitProbeVerdict {
	if strings.TrimSpace(text) == "" {
		return circuitProbeVerdict{outcome: CircuitProbeOutcomeNoAnswer, attributable: true, reason: "empty answer"}
	}
	values := circuitProbeIntegers(text)
	if len(values) == 0 {
		return circuitProbeVerdict{outcome: CircuitProbeOutcomeGibberish, attributable: true, reason: "answer contains no integer"}
	}
	for _, value := range values {
		if value == expected {
			return circuitProbeVerdict{outcome: CircuitProbeOutcomeOK, recovered: true, reason: "answer contains the expected result"}
		}
	}
	return circuitProbeVerdict{outcome: CircuitProbeOutcomeWrongAnswer, attributable: true, reason: "answer omits the expected result"}
}

// circuitProbeIntegers 抽出文本里所有"独立"的整数候选（带符号）。
//
// 规则（都对着真实回答形态定过）：
//   - 只认 ASCII 数字串；左右紧贴标识符字符（字母/数字/下划线）的数字串属于型号或
//     编号，不是答案，整串丢掉（"gpt-4o" 里的 4、"id_1234" 里的 1234）；
//   - 负号只有紧贴数字、且负号前面不是标识符字符时才生效：'-1234' / '−1234' 是负数，
//     "x-1234" 里的 1234 仍是负数吗？不是——负号被判成标识符的一部分，只剩正数。
//     负数永远不可能等于正确答案（两个四位数之和为正），所以保留符号就是"别把负数
//     洗成正数去误判成功"；
//   - 千位分隔符只在三个条件同时满足时才连成一个数：逗号前是 1~3 位且不以 0 开头
//     （数学上合法的第一段）、逗号后**恰好三位**数字、且一个数只合并一次。
//     因此 "1,234" → 1234、"12,345" → 12345，而 "1234,5678" 是两个数 1234 与 5678
//     （第一段四位，数学上不是千位分组）、"0,123" 是两个数、"1,2345" 也是两个数；
//   - 超过 9 位的数字串直接丢弃（不可能是答案，也不必为它做溢出处理）。
func circuitProbeIntegers(text string) []int {
	runes := []rune(text)
	values := make([]int, 0, 4)
	for i := 0; i < len(runes); {
		if !circuitProbeASCIIDigit(runes[i]) {
			i++
			continue
		}
		firstDigit := i
		tokenStart := firstDigit
		negative := false
		// 负号：紧贴数字，且再往前一位不是标识符字符。
		if i > 0 && (runes[i-1] == '-' || runes[i-1] == '−') {
			if i-1 == 0 || !circuitProbeIdentRune(runes[i-2]) {
				negative = true
				tokenStart = i - 1
			}
		}
		value := 0
		digits := 0
		joined := false
		for i < len(runes) {
			r := runes[i]
			if circuitProbeASCIIDigit(r) {
				// 只累加前 10 位：更长的数字串反正会被丢掉，没必要为它做溢出。
				if digits < 10 {
					value = value*10 + int(r-'0')
				}
				digits++
				i++
				continue
			}
			if !joined && r == ',' && digits >= 1 && digits <= 3 && runes[firstDigit] != '0' &&
				i+3 < len(runes) &&
				circuitProbeASCIIDigit(runes[i+1]) &&
				circuitProbeASCIIDigit(runes[i+2]) &&
				circuitProbeASCIIDigit(runes[i+3]) &&
				(i+4 >= len(runes) || !circuitProbeASCIIDigit(runes[i+4])) {
				// 千位分隔符：吃掉逗号，接着扫后面的三位。
				joined = true
				i++
				continue
			}
			break
		}
		if digits == 0 || digits > 9 {
			continue
		}
		// 左右紧贴标识符字符 ⇒ 这是标识符/型号的一部分，不是答案。
		if tokenStart > 0 && circuitProbeIdentRune(runes[tokenStart-1]) {
			continue
		}
		if i < len(runes) && circuitProbeIdentRune(runes[i]) {
			continue
		}
		if negative {
			value = -value
		}
		values = append(values, value)
	}
	return values
}

// 带包前缀的命名是为了不和同包里后来者加的小工具重名（这个包还在多路并行改动中）。
func circuitProbeASCIIDigit(r rune) bool { return r >= '0' && r <= '9' }

func circuitProbeASCIILetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func circuitProbeIdentRune(r rune) bool {
	return circuitProbeASCIILetter(r) || circuitProbeASCIIDigit(r) || r == '_'
}

// circuitProbeEmbeddingVerdict 判读一次嵌入回答：要真的有数据，且至少一条非空有效向量。
func circuitProbeEmbeddingVerdict(parsed *transformermodel.InternalLLMResponse) circuitProbeVerdict {
	if parsed == nil || !parsed.IsEmbeddingResponse() {
		return circuitProbeVerdict{outcome: CircuitProbeOutcomeNoAnswer, attributable: true, reason: "upstream returned no embedding data"}
	}
	for _, item := range parsed.EmbeddingData {
		if circuitProbeVectorOK(item.Embedding) {
			return circuitProbeVerdict{outcome: CircuitProbeOutcomeEmbedding, recovered: true, reason: "embedding vector accepted"}
		}
	}
	return circuitProbeVerdict{outcome: CircuitProbeOutcomeNoAnswer, attributable: true, reason: "embedding vector empty or invalid"}
}

// circuitProbeVectorOK 判断一条向量是否"非空且有效"：浮点数组必须非空且不含
// NaN/Inf；base64 形态只要求非空（解码由适配器负责，探针不再重复解一遍）。
func circuitProbeVectorOK(vector transformermodel.Embedding) bool {
	if len(vector.FloatArray) > 0 {
		for _, value := range vector.FloatArray {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return false
			}
		}
		return true
	}
	if vector.Base64String != nil && strings.TrimSpace(*vector.Base64String) != "" {
		return true
	}
	return false
}

// circuitProbeHistoryAnswer 生成管理端可见的"历史答案"：默认原样保留上游正文
// （包括换行与标点，只要判读有用就尽量别丢信息），只做两件安全处理——
// 先精确剔除**本次选中密钥**的字节，再跑既有通用凭据脱敏，最后按 rune 有界截断。
//
// 调用点必须在判读之后（见 circuitProbeAttempt）：脱敏会改写正文，先脱敏再判读
// 会把"回答里恰好含密钥"的正确回答误判成答非所问。
func circuitProbeHistoryAnswer(text, channelKey string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	text = circuitProbeRedactExactKey(text, channelKey)
	text = xredact.Secrets(text)
	return circuitProbeTruncateRunes(text, circuitProbeAnswerLimit)
}

// circuitProbeRedactExactKey 把**本次实际选中的那个渠道密钥**的字节从文本里剔除。
//
// 为什么要单独做一次：xredact 的模式表只认常见云厂商格式（sk-… / JWT / 32+ 位 hex），
// 中转自定义格式的 key 它未必认得；而这个 key 正是本次出站真正用到的凭据，
// "上游把密钥回显在正文里"这种情况必须连字节一起去掉。
func circuitProbeRedactExactKey(text, channelKey string) string {
	if text == "" {
		return text
	}
	trimmed := strings.TrimSpace(channelKey)
	for _, candidate := range []string{channelKey, trimmed} {
		if candidate == "" || !strings.Contains(text, candidate) {
			continue
		}
		if len([]rune(candidate)) < circuitProbeRedactMinKeyRunes {
			return ""
		}
		text = strings.ReplaceAll(text, candidate, circuitProbeKeyRedactPlaceholder)
	}
	return text
}

// circuitProbeBounded 折叠空白并截断，用于一句给人看的归因说明。
func circuitProbeBounded(value string, limit int) string {
	return circuitProbeTruncateRunes(strings.Join(strings.Fields(value), " "), limit)
}

// circuitProbeTruncateRunes 按 rune 截断到 limit（不切半个字），不改动原始空白/换行。
func circuitProbeTruncateRunes(value string, limit int) string {
	if limit <= 0 {
		return value
	}
	if runes := []rune(value); len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return value
}

// circuitProbeErrorCode 是探测行 error_code 的稳定取值（"circuit_probe_<outcome>"）。
func circuitProbeErrorCode(outcome string) string {
	outcome = strings.TrimSpace(outcome)
	if outcome == "" {
		return CircuitProbeMarker
	}
	return CircuitProbeMarker + "_" + outcome
}

// circuitProbeRecordLog 写一条管理端 Relay Log（探测专用标记）。
//
// 口径与渠道检测的 audit 行同一套"非计费"先例，但**更保守**：
//   - user_id / api_key_id 都是 0：探测没有调用方，不能挂到任何用户名下；
//   - cost = 0、输入/输出 token = 0、usage_source = no_usage：不产生费用，也不进统计
//     （error_strategy 里的 stats_counted=false 让 op/stats_breakdown.go 直接跳过这一行）；
//   - error 字段只放我们自己词汇的归因，绝不放上游错误体；request/response_content
//     只放题目、判定、以及已脱敏的有界答案——没有上游原文，也没有凭据；
//   - error_strategy 带 breaker_counted=false：op.relayLogExcludedFromModelTelemetry
//     认这一段，于是模型健康遥测与"可疑日志自动跟查"都不会拿探测行再去打一次上游。
//
// skipped 的探测压根没发出请求，不写日志（没有可看的尝试，也不该污染日志页）。
func circuitProbeRecordLog(ctx context.Context, channel dbmodel.Channel, result CircuitProbeResult) {
	if result.Outcome == "" || result.Outcome == CircuitProbeOutcomeSkipped {
		return
	}
	requestContent, err := json.Marshal(map[string]any{
		"kind":           CircuitProbeMarker,
		"model":          result.UpstreamModel,
		"endpoint":       result.Endpoint,
		"capability":     result.Capability,
		"channel_id":     channel.ID,
		"channel_key_id": result.ChannelKeyID,
		"prompt":         result.Prompt,
	})
	if err != nil {
		requestContent = nil
	}
	responseContent, err := json.Marshal(map[string]any{
		"outcome":        result.Outcome,
		"recovered":      result.Recovered,
		"attributable":   result.Attributable,
		"status_code":    result.StatusCode,
		"retry_after_ms": result.RetryAfter.Milliseconds(),
		"answer":         result.Answer,
	})
	if err != nil {
		responseContent = nil
	}

	attemptStatus := dbmodel.AttemptFailed
	errorText := ""
	errorCode := ""
	errorStatus := 0
	if result.Recovered {
		attemptStatus = dbmodel.AttemptSuccess
	} else {
		errorText = result.Reason
		errorCode = circuitProbeErrorCode(result.Outcome)
		if result.StatusCode >= 400 {
			errorStatus = result.StatusCode
		}
	}

	relayLog := dbmodel.RelayLog{
		Time:             time.Now().Unix(),
		UserID:           0,
		APIKeyID:         0,
		RequestEndpoint:  CircuitProbeMarker,
		RequestPath:      result.UpstreamPath,
		RequestModelName: result.UpstreamModel,
		ActualModelName:  result.UpstreamModel,
		ChannelId:        channel.ID,
		ChannelName:      channelDisplayName(&channel),
		UseTime:          int(result.Duration.Milliseconds()),
		Cost:             0,
		IsStream:         result.IsStream,
		RequestContent:   string(requestContent),
		ResponseContent:  string(responseContent),
		Error:            errorText,
		ErrorCode:        errorCode,
		ErrorStatus:      errorStatus,
		ErrorStrategy:    circuitProbeRelayLogStrategy,
		UsageSource:      dbmodel.RelayLogUsageSourceNoUsage,
		Attempts: []dbmodel.ChannelAttempt{{
			ChannelID:    channel.ID,
			ChannelKeyID: result.ChannelKeyID,
			ChannelName:  channelDisplayName(&channel),
			ModelName:    result.UpstreamModel,
			UpstreamPath: result.UpstreamPath,
			Endpoint:     result.Endpoint,
			Capability:   result.Capability,
			AttemptNum:   1,
			Status:       attemptStatus,
			Duration:     int(result.Duration.Milliseconds()),
			Msg:          circuitProbeBounded(result.Reason, circuitProbeReasonLimit),
		}},
		TotalAttempts: 1,
	}
	if err := op.RelayLogAdd(ctx, relayLog); err != nil {
		log.Warnf("failed to save %s relay log: %v", CircuitProbeMarker, err)
	}
}

// circuitProbeLog 打一条带固定标记的 stdout 日志。
//
// 只打判定字段：题目与答案不写日志（题目我们自己生成，答案是模型文本，没必要入
// stdout），上游原文更是从不出现。没有统计、没有计费。
func circuitProbeLog(result CircuitProbeResult) {
	if result.Outcome == CircuitProbeOutcomeSkipped {
		// 跳过是配置/调用方问题，不是探测结论：降到 debug，避免调度频繁重试时刷屏。
		log.Debugf("%s[%s] channel=%d key=%d model=%s endpoint=%s capability=%s outcome=%s recovered=%t attributable=%t status=%d retry_after_ms=%d duration_ms=%d reason=%s",
			CircuitProbeMarker, CircuitProbeDisplayLabel, result.ChannelID, result.ChannelKeyID,
			result.UpstreamModel, result.Endpoint, result.Capability, result.Outcome,
			result.Recovered, result.Attributable, result.StatusCode,
			result.RetryAfter.Milliseconds(), result.Duration.Milliseconds(), result.Reason)
		return
	}
	log.Infof("%s[%s] channel=%d key=%d model=%s endpoint=%s capability=%s outcome=%s recovered=%t attributable=%t status=%d retry_after_ms=%d duration_ms=%d reason=%s",
		CircuitProbeMarker, CircuitProbeDisplayLabel, result.ChannelID, result.ChannelKeyID,
		result.UpstreamModel, result.Endpoint, result.Capability, result.Outcome,
		result.Recovered, result.Attributable, result.StatusCode,
		result.RetryAfter.Milliseconds(), result.Duration.Milliseconds(), result.Reason)
}
