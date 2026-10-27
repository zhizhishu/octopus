package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"strings"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/modelverify/behavior"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/price"
	transformerModel "github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"github.com/zhizhishu/octopus/internal/utils/tokenizer"
	"github.com/zhizhishu/octopus/internal/utils/xredact"
)

// redactLogBody 只用于写进日志的正文。出站请求体不经过这里。
func redactLogBody(body string) string {
	return xredact.Secrets(body)
}

const metricsPersistTimeout = 30 * time.Second

// RelayMetrics 负责最终的日志收集与持久化
type RelayMetrics struct {
	APIKeyID         int
	UserID           int
	RequestIP        string
	RequestUserAgent string
	RequestModel     string
	RequestEndpoint  string
	RequestPath      string
	StartTime        time.Time

	// 首 Token 时间
	FirstTokenTime time.Time

	// 诊断(仅 diagnostic_mode 消费): 发出上游请求 / 收到上游响应头 的时刻(仅最终尝试)。
	// ResponseHeaderTime-RequestSentTime = "等响应头"(首包)耗时——现有 metrics 缺这段,
	// 首包卡死(上游连上却不发头)时只有它能把"卡在哪一层"照出来。
	RequestSentTime    time.Time
	ResponseHeaderTime time.Time

	// 请求和响应内容
	InternalRequest  *transformerModel.InternalLLMRequest
	InternalResponse *transformerModel.InternalLLMResponse
	// upsteamRequestBody 是真实出站请求体快照（TransformRequest + param_override
	// 之后的字节，已脱敏）。空 = 未采集（开关关闭或采集失败）。
	upstreamRequestBody string

	// 统计指标
	ActualModel   string
	Stats         model.StatsMetrics
	SessionKey    string
	SessionSource string

	// UpstreamDeclaredModel 是上游响应里自报的模型名。必须在 relay 把
	// internalResponse.Model 改写成客户端可见名之前捕获(model_mapping 生效时会被覆盖),
	// 否则拿到的永远是请求模型名而不是上游真名。归属规则: 只记「产出最终
	// 响应的那次尝试」的观测, 失败尝试的声明不落请求级(见
	// CommitUpstreamDeclaredModel)。
	UpstreamDeclaredModel string

	// usageEstimated is true when Stats' token/cost figures were counted locally
	// because the upstream omitted usage (see SetInternalResponse). saveLog surfaces
	// this as usage_source=local_estimate so the numbers read as an estimate.
	usageEstimated bool

	// usageDelivered 标记"本次请求已经向下游交付了实质内容", 即使最终终态是失败。
	// 见 MarkUsageDelivered 与 Save 里的计费闸门。
	usageDelivered bool

	// ChannelKeyRemark 记录最终(成功/已提交)尝试所用渠道 Key 的备注, 仅供日志展示。
	// 重试会切换 Key, 因此由 relay 在成功/已写出下游的那一刻回填最终 Key 的备注。
	ChannelKeyRemark string

	// 参数覆盖
	ParamOverride   string
	AccessPlan      *model.AccessPlan
	AccessRouteRule *model.AccessRouteRule
	AccessRouteUsed bool
	BillingSnapshot model.AccessPlanBillingSnapshot
	PromptSnapshot  promptOverrideSnapshot
}

func NewRelayMetrics(apiKeyID int, userID int, requestIP string, requestModel string, req *transformerModel.InternalLLMRequest) *RelayMetrics {
	return &RelayMetrics{
		APIKeyID:        apiKeyID,
		UserID:          userID,
		RequestIP:       requestIP,
		RequestModel:    requestModel,
		StartTime:       time.Now(),
		InternalRequest: req,
	}
}

func (m *RelayMetrics) SetFirstTokenTime(t time.Time) {
	m.FirstTokenTime = t
}

// truncateUserAgentForLog caps the stored client User-Agent at 512 runes (the
// column width). Rune-safe so a multi-byte UA never splits mid-character, and
// no suffix marker is appended — the frontend parses the raw UA for client
// identification, so the stored value stays clean.
func truncateUserAgentForLog(ua string) string {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return ""
	}
	if r := []rune(ua); len(r) > 512 {
		return string(r[:512])
	}
	return ua
}

// truncateRunes 按 rune 截断到 max 个字符，不追加任何标记。
//
// 与 truncateString 的区别：那个按字节切并加 "...(truncated)" 后缀，
// 用在这里会污染观测值——system_fingerprint 是要跨行做等值比较的，
// 加了后缀就把"同一个值"变成"看起来不同的值"。超长本身就是异常，
// 截断只为保护列宽，不该改变语义。
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > max {
		return string(r[:max])
	}
	return s
}

func (m *RelayMetrics) SetAccessPlan(plan *model.AccessPlan, rule *model.AccessRouteRule, routeUsed bool) {
	m.AccessPlan = plan
	m.AccessRouteRule = rule
	m.AccessRouteUsed = routeUsed
}

func (m *RelayMetrics) SetRequestEndpoint(endpoint string, path string) {
	m.RequestEndpoint = cleanRelayEndpointName(endpoint)
	m.RequestPath = strings.TrimSpace(path)
}

// CommitUpstreamDeclaredModel 把「产出本次最终响应的尝试」的自报型号写进请求级观测。
//
// 调用时机是硬要求: 必须在 relay 的"客户端可见名还原"之前完成捕获(见
// relayAttempt.captureUpstreamDeclaredModel), 开启 model_mapping 时 relay 会把
// internalResponse.Model 覆盖成 ra.requestModel(防止上游身份外泄给客户端),
// 覆盖之后上游真名就没了。
//
// 无条件覆盖: "首个非空"的判定在尝试层(流式分片重复回显同一型号), 请求级只
// 反映赢家——失败尝试的声明留在尝试级不落日志, 赢家不自报就是未知(空), 不
// 沿用先前失败尝试的声明(F10)。纯观测——不参与、不改变任何转发字节。
func (m *RelayMetrics) CommitUpstreamDeclaredModel(name string) {
	if m == nil {
		return
	}
	m.UpstreamDeclaredModel = strings.TrimSpace(name)
}

// upstreamModelMismatch 三态比对"上游自报的模型名"与"我们发出去的模型名"。
//
// 返回 nil 表示无法判定: 上游没声明模型名(很多中转不回这个字段), 或我们不知道发出去的
// 是哪个名字。两种"没数据"都不该被读成"有问题"。
//
// 返回 false/true 才是有观测的结论。刻意偏离 sub2api 一处: 它在 sentModel 为空时判 true,
// 而 octopus 的失败/本地校验路径经常没有出站模型名, 照搬会把"没请求过"记成"不一致"。

// UpstreamMismatchCodeAliasVariant 标记「同族别名疑似」: 名字剥掉日期后缀后同族前缀一致,
// 只有后缀/日期档位不同。仍是 mismatch(线索照记), 但前端文案按「别名疑似」而非「名字完全
// 不符」呈现。空串 = 完全不符(旧语义)。
const UpstreamMismatchCodeAliasVariant = "alias_variant"

// UpstreamMismatchCodeCrossFamily 标记「跨家族疑似」: 我们发出的名字与上游自报的名字, 都能被
// behavior 家族表识别、且两者家族无交集——比 alias_variant 更强的线索(名字不是同族档位差异,
// 而像换了另一个家族)。仍只是线索(回显本身可撒谎), 不宣称模型真实性。
const UpstreamMismatchCodeCrossFamily = "cross_family"

// UpstreamModelMismatchCode 把 mismatch 的细分 code 映射出来: mismatch 为 nil/false 时恒空。
// 没有额外数据可比对时也返回空, 与 mismatch 三态语义对齐。
// 优先级: cross_family > alias_variant——跨族是更强的线索, 只有非跨族时才退到同族别名疑似。
func UpstreamModelMismatchCode(mismatch *bool, sentModel, responseModel string) string {
	if mismatch == nil || !*mismatch {
		return ""
	}
	if upstreamCrossFamily(sentModel, responseModel) {
		return UpstreamMismatchCodeCrossFamily
	}
	if upstreamAliasVariantMatch(sentModel, responseModel) {
		return UpstreamMismatchCodeAliasVariant
	}
	if upstreamSameFamilyAliasEcho(sentModel, responseModel) {
		return UpstreamMismatchCodeAliasVariant
	}
	return ""
}

// upstreamCanonicalEchoFamilies: 上游只会回自家规范 id、与请求名没有公共字符串前缀的家族。
//
// 当前仅 moonshot(kimi): 请求 kimi-3.1-pro, 上游常回 moonshotai/Kimi-K3 / kimi-k2.5 /
// kimi-k3——都是同一个家族的不同叫法, 却被 upstreamAliasVariantMatch 的前缀判据漏掉
// (kimi-3.1-pro 与 kimi-k3 在 "kimi-k" 后分叉, 与 moonshotai/Kimi-K3 更是毫无公共前缀)。
//
// 新增家族必须有真实回显样本支撑, 不许按"感觉"放行——这里放行的是"同族不同名"这一档,
// 仍然只降级到 alias_variant(「名称变体」线索), **不**否认 mismatch 事实:
// 请求 kimi-3.1-pro 真的被换成 kimi-k3, 布尔位仍为 true, 前端仍显示变体提示。
var upstreamCanonicalEchoFamilies = map[string]struct{}{"moonshot": {}}

// upstreamSameFamilyAliasEcho 判定「请求名与回显名同属一个已知规范回显家族」。
// 只对白名单家族生效, 跨家族(请求 kimi 回 gpt/claude/deepseek)不在此放行,
// 同族不同款(opus vs sonnet)也不在白名单内, 二者仍走各自的判据。
func upstreamSameFamilyAliasEcho(sentModel, echoModel string) bool {
	sentFamilies := behavior.InferFamilies(sentModel)
	echoFamilies := behavior.InferFamilies(echoModel)
	for _, s := range sentFamilies {
		if _, ok := upstreamCanonicalEchoFamilies[s]; !ok {
			continue
		}
		for _, e := range echoFamilies {
			if s == e {
				return true
			}
		}
	}
	return false
}

// upstreamCrossFamily 判定「发出去的名字」与「上游自报的名字」是否跨家族: 两者都能被
// behavior 家族表识别, 且家族集合无交集。任一识别不出(含空) → false——缺一边不是信号,
// 与 evalModelFamilyVerdict 里 request/echo 家族比对的既有口径一致。纯名称/家族线索,
// 不宣称模型真实性。
func upstreamCrossFamily(sentModel, responseModel string) bool {
	sentFamilies := behavior.InferFamilies(sentModel)
	echoFamilies := behavior.InferFamilies(responseModel)
	if len(sentFamilies) == 0 || len(echoFamilies) == 0 {
		return false
	}
	return !behavior.FamiliesIntersect(sentFamilies, echoFamilies)
}

// 模型家族判定三档(落 model.RelayLog.ModelFamilyVerdict 列)。
const (
	// ModelFamilyVerdictTrusted: 无怀疑(回显与实际一致, 且请求端与自报端家族不冲突)。
	ModelFamilyVerdictTrusted = "trusted"
	// ModelFamilyVerdictSuspect: 有怀疑(回显不符, 或回显一致但请求端与自报端跨家族)。
	ModelFamilyVerdictSuspect = "suspect"
	// ModelFamilyVerdictUnknown: 无法判定(上游没自报, 或我们不知道发出去的模型名)。
	ModelFamilyVerdictUnknown = "unknown"
)

// evalModelFamilyVerdict 把「回显三态 + 家族别名表」收敛成一条可落库的三档判定。
//
// 输入:
//   - requestModel: 客户端请求的模型名(判定同族时用它, 而不是被 model_mapping 改写后的那份);
//   - actualModel:  oct 实际发出去的模型名(与 UpstreamModelMismatch 列同源, 保证两列一致);
//   - upstreamEcho: 上游响应自报的模型名;
//   - mismatchCode: UpstreamModelMismatchCode 的产物(空串 / alias_variant / cross_family)。
//     suspect 的细分类型已单独落在 UpstreamMismatchCode 列(前端据此区分「跨族」「别名疑似」
//     与「不符」), 这里对三档的唯一贡献是: 有该 code 就确认 mismatch 为真; 故不再单独分支。
//
// 判定:
//   - unknown: 上游没自报(echo 为空), 或上游没声明/我们没出站名(mismatch 为 nil);
//   - trusted: 回显与实际一致(mismatch==false), 且客户端请求家族与自报家族不冲突
//     (任一边识别不出家族, 或两边都识别出且有交集);
//   - suspect: 其余有观测的情形——回显与实际不符, 或回显一致但两边家族都能识别且无交集
//     (客户端要 A 家族、我们(经映射)发 B 家族、上游也回 B: 名字一致掩盖不了跨家族替换)。
//
// vendorHint 与判定独立: 只从上游自报名里按最后一个 '/' 或 ':' 剥前缀, 无前缀留空。
func evalModelFamilyVerdict(requestModel, actualModel, upstreamEcho string, mismatchCode string) (verdict string, vendorHint string) {
	echo := strings.TrimSpace(upstreamEcho)
	vendorHint = modelVendorHint(echo)
	if echo == "" {
		return ModelFamilyVerdictUnknown, vendorHint
	}
	mismatch := upstreamModelMismatch(actualModel, echo)
	if mismatch == nil {
		// 上游自报了名字, 但我们不知道发出去的是哪个(actualModel 空): 仍无法判定。
		return ModelFamilyVerdictUnknown, vendorHint
	}
	if *mismatch || mismatchCode != "" {
		// mismatchCode 非空必然意味着 mismatch 为真(见 UpstreamModelMismatchCode), 双保险。
		return ModelFamilyVerdictSuspect, vendorHint
	}
	// 回显与实际一致: 只要不是「请求家族与自报家族都能识别且完全无交集」, 就无怀疑。
	requestedFamilies := behavior.InferFamilies(requestModel)
	echoFamilies := behavior.InferFamilies(echo)
	if len(requestedFamilies) == 0 || len(echoFamilies) == 0 || behavior.FamiliesIntersect(requestedFamilies, echoFamilies) {
		return ModelFamilyVerdictTrusted, vendorHint
	}
	return ModelFamilyVerdictSuspect, vendorHint
}

// modelVendorHint 从模型名里剥出供应商前缀(最后一个 '/' 或 ':' 之前的部分)。
// 对齐 internal/price/price.go:GetLLMPrice 的 vendor-prefixed 回退: 无分隔符, 或分隔符在
// 首/尾(剥完为空)时留空——事实提取, 绝不猜造。
// 例: "vercel-deepseek/deepseek-v4" -> "vercel-deepseek"; "vendor:gpt-x" -> "vendor"。
func modelVendorHint(modelName string) string {
	name := strings.TrimSpace(modelName)
	if name == "" {
		return ""
	}
	i := strings.LastIndexAny(name, "/:")
	if i <= 0 || i+1 >= len(name) {
		return ""
	}
	return name[:i]
}

func upstreamModelMismatch(sentModel, responseModel string) *bool {
	responseModel = strings.TrimSpace(responseModel)
	sentModel = strings.TrimSpace(sentModel)
	if responseModel == "" || sentModel == "" {
		return nil
	}
	mismatch := !upstreamModelsMatchForAudit(sentModel, responseModel)
	return &mismatch
}

// upstreamModelsMatchForAudit 大小写不敏感比对(与 sub2api 同口径)。
//
// 刻意不做日期后缀归一进匹配本身: 上游回带日期后缀的同族变体(如 -20251101)仍记不一致,
// 但由 UpstreamModelMismatchCode 标成 alias_variant(「别名疑似」), 与「名字完全不符」区分。
//
// 受控命名空间归一: 先按原样 EqualFold; 不中时, 各自剥掉**一层**已知命名空间前缀
// (cn: / global:, 大小写不敏感)后再完整 EqualFold。
// 因此 "cn:gpt-5"、"global:kimi-k3.1-pro" 与裸名视为同一模型(合法命名空间差异不算回显不符),
// 而 "cn:gpt-5" 与 "gpt-6" 仍不符, "gpt-5:free" 与 "gpt-5" 也不会被误合并(只剥已知前缀一层,
// 不做通用 LastIndexAny——那会吞掉 gpt:free 这类带冒号的合法版本/档位后缀或多层模型名)。
func upstreamModelsMatchForAudit(sentModel, responseModel string) bool {
	sent := strings.TrimSpace(sentModel)
	response := strings.TrimSpace(responseModel)
	if strings.EqualFold(sent, response) {
		return true
	}
	return strings.EqualFold(modelNamespaceStripped(sent), modelNamespaceStripped(response))
}

// modelNamespacePrefixes 是路由侧给模型名加的命名空间, 不是模型身份。
// cn: 国内线路, global: 国际线路; 上游回显通常不带这层前缀。
var modelNamespacePrefixes = []string{"cn:", "global:"}

// modelNamespaceStripped 去掉一层已知命名空间前缀(trim 后大小写不敏感判断), 无该前缀则
// 原样返回(已 trim)。刻意不做通用 "最后一个 '/' 或 ':' 之后" 的剥离: 那会把 "gpt-5:free" 剥成
// "free"、把多层模型名 "a/b/c" 的中间段吞掉, 从而把不同模型/版本误判为相同。此处只做已知
// 命名空间的一层归一——仅用于比较, 落库字段仍保留原始名称(见 saveLog)。
func modelNamespaceStripped(modelName string) string {
	name := strings.TrimSpace(modelName)
	lower := strings.ToLower(name)
	for _, prefix := range modelNamespacePrefixes {
		if strings.HasPrefix(lower, prefix) {
			return strings.TrimSpace(name[len(prefix):])
		}
	}
	return name
}

// upstreamAliasVariantMatch 判定「同族不同后缀」: 两名小写化后, 剥掉尾部 \d{8} 日期后缀
// 与连字符, 一侧是另一侧的前缀(且双方非空)。只做受控归一, 不猜家族表——避免把完全不同
// 的模型名误判成同族。
func upstreamAliasVariantMatch(sentModel, responseModel string) bool {
	a, b := upstreamAliasNormalize(sentModel), upstreamAliasNormalize(responseModel)
	if a == "" || b == "" {
		return false
	}
	short, long := a, b
	if len(short) > len(long) {
		short, long = long, short
	}
	if !strings.HasPrefix(long, short) {
		return false
	}
	// 前缀边界必须是分隔符(-/_/.), 防止 "mod" 被当成 "modular-..." 的同族前缀。
	return len(long) == len(short) || long[len(short)] == '-' || long[len(short)] == '_' || long[len(short)] == '.'
}

// upstreamAliasNormalize 剥尾部 \d{8}$ 日期后缀与其前面的连字符, 小写化, 去首尾空白。
func upstreamAliasNormalize(name string) string {
	origTrimmed := strings.TrimSpace(name)
	name = strings.ToLower(origTrimmed)
	if loc := upstreamModelDateSuffix.FindStringSubmatchIndex(name); loc != nil {
		// loc[2]..loc[3] 是 \d{8} 组, 其前一个字符若为连字符一并剥掉。
		end := loc[2]
		if end > 0 && (name[end-1] == '-' || name[end-1] == '_' || name[end-1] == '.') {
			end--
		}
		name = name[:end]
	}
	// 整名就是 8 位数字时剥完为空：返回原名, 让上游判空归 false(不同族),
	// 而不是把纯数字名错剥成空串。
	if name == "" {
		name = strings.ToLower(strings.TrimSpace(origTrimmed))
	}
	return strings.TrimSpace(name)
}

var upstreamModelDateSuffix = regexp.MustCompile(`(\d{8})$`)

func (m *RelayMetrics) SetClientSession(info clientSessionInfo) {
	m.SessionKey = info.Key
	m.SessionSource = info.Source
}

func (m *RelayMetrics) SetPromptOverrideSnapshot(snapshot promptOverrideSnapshot) {
	m.PromptSnapshot = snapshot
}

// upstreamRequestBodyLogLimit 是落库快照的字节上限。
//
// 请求体实测最大 818KB（智能体会话：880 条 messages + 43KB tools），整份存进
// SQLite 会让日志表膨胀数倍。128KB 覆盖绝大多数请求的完整顶层结构（model /
// thinking / reasoning_effort / tools 声明都在 messages 之前或之中），超出部分
// 截断并标记，绝不静默丢。
// 注意：**只有存储这份截断，真正发出去的 body 永远是完整的**。
const upstreamRequestBodyLogLimit = 128 * 1024

// SetUpstreamRequestBody 记录真实出站请求体（调用方已确认开关打开）。
// 只存一份快照，供 saveLog 落库；不参与路由、计费、指纹。
func (m *RelayMetrics) SetUpstreamRequestBody(body []byte) {
	if m == nil || len(body) == 0 {
		return
	}
	if len(body) > upstreamRequestBodyLogLimit {
		head := redactLogBody(string(body[:upstreamRequestBodyLogLimit]))
		m.upstreamRequestBody = head + fmt.Sprintf("\n...[truncated: %d bytes total]", len(body))
		return
	}
	m.upstreamRequestBody = redactLogBody(string(body))
}

func (m *RelayMetrics) SetInternalResponse(resp *transformerModel.InternalLLMResponse, actualModel string) {
	m.InternalResponse = resp
	m.ActualModel = actualModel

	if resp == nil {
		return
	}

	usage := resp.Usage
	// Local usage estimate. When the upstream omits usage entirely, or reports zero
	// completion tokens on a response that actually delivered content, count the
	// tokens locally via the tokenizer so a successful response is not logged/billed
	// as 0 tokens — mirroring new-api's patchGeminiZeroCompletionUsage (local
	// ResponseText2Usage). The estimate is applied only to the local Stats/billing
	// used for logging; resp.Usage is left untouched so usageAuditFromInternalResponse
	// still flags usage_missing_reason (the count is estimated, not upstream-authoritative).
	if usage == nil || usage.CompletionTokens == 0 {
		if est := estimateCompletionTokens(resp, actualModel); est > 0 {
			estimated := &transformerModel.Usage{}
			if usage != nil {
				*estimated = *usage
			}
			estimated.CompletionTokens = int64(est)
			if estimated.PromptTokens == 0 && m.InternalRequest != nil {
				estimated.PromptTokens = int64(estimatePromptTokens(m.InternalRequest, actualModel))
			}
			usage = estimated
			m.usageEstimated = true
		}
	}

	if usage == nil {
		return
	}

	m.Stats.InputToken = usage.PromptTokens
	m.Stats.OutputToken = usage.CompletionTokens
	m.Stats.CacheHitToken, m.Stats.CacheWriteToken, m.Stats.CacheInputToken = usageCacheStats(usage)

	if usage.PromptTokensDetails == nil {
		usage.PromptTokensDetails = &transformerModel.PromptTokensDetails{
			CachedTokens: 0,
		}
	}
	m.BillingSnapshot = m.buildBillingSnapshot(actualModel, usage)
	m.Stats.InputCost = m.BillingSnapshot.FinalInputCost + m.BillingSnapshot.FinalCacheReadCost + m.BillingSnapshot.FinalCacheWriteCost
	m.Stats.OutputCost = m.BillingSnapshot.FinalOutputCost
}

// estimateCompletionTokens counts the tokens of a response's assistant output
// (text + reasoning + tool-call arguments) via the local tokenizer, used only as a
// fallback when the upstream did not report completion tokens.
func estimateCompletionTokens(resp *transformerModel.InternalLLMResponse, model string) int {
	if resp == nil {
		return 0
	}
	var b strings.Builder
	for _, ch := range resp.Choices {
		if ch.Message != nil {
			writeMessageTextForEstimate(&b, ch.Message)
		}
	}
	if b.Len() == 0 {
		return 0
	}
	return tokenizer.CountTokens(b.String(), model)
}

// estimatePromptTokens counts the tokens of a request's messages via the local
// tokenizer, used only when the upstream reported no prompt tokens alongside a
// locally-estimated completion.
func estimatePromptTokens(req *transformerModel.InternalLLMRequest, model string) int {
	if req == nil {
		return 0
	}
	var b strings.Builder
	for i := range req.Messages {
		writeMessageTextForEstimate(&b, &req.Messages[i])
	}
	if b.Len() == 0 {
		return 0
	}
	return tokenizer.CountTokens(b.String(), model)
}

func writeMessageTextForEstimate(b *strings.Builder, msg *transformerModel.Message) {
	if msg == nil {
		return
	}
	if msg.Content.Content != nil {
		b.WriteString(*msg.Content.Content)
		b.WriteByte('\n')
	}
	for _, p := range msg.Content.MultipleContent {
		if p.Text != nil {
			b.WriteString(*p.Text)
			b.WriteByte('\n')
		}
	}
	if rc := msg.GetReasoningContent(); rc != "" {
		b.WriteString(rc)
		b.WriteByte('\n')
	}
	for _, tc := range msg.ToolCalls {
		b.WriteString(tc.Function.Name)
		b.WriteString(tc.Function.Arguments)
		b.WriteByte('\n')
	}
}

func (m *RelayMetrics) currentBillingSnapshot(actualModel string) model.AccessPlanBillingSnapshot {
	if m.BillingSnapshot.BillingModelName != "" || m.BillingSnapshot.AccessPlanID != 0 {
		return m.BillingSnapshot
	}
	return m.buildBillingSnapshot(actualModel, nil)
}

func (m *RelayMetrics) buildBillingSnapshot(actualModel string, usage *transformerModel.Usage) model.AccessPlanBillingSnapshot {
	upstreamModel := strings.TrimSpace(actualModel)
	if upstreamModel == "" {
		upstreamModel = m.RequestModel
	}

	snapshot := model.AccessPlanBillingSnapshot{
		DefaultMultiplier: 1,
		ModelMultiplier:   1,
		FinalMultiplier:   1,
	}

	billingSource := model.AccessBillingModelSourceRequest
	if m.AccessPlan != nil {
		snapshot.AccessPlanID = m.AccessPlan.ID
		snapshot.AccessPlanSlug = m.AccessPlan.Slug
		snapshot.AccessPlanName = m.AccessPlan.DisplayName
		if m.AccessPlan.RouteProfile != nil {
			snapshot.RouteProfileID = m.AccessPlan.RouteProfile.ID
			snapshot.RouteProfileName = m.AccessPlan.RouteProfile.Name
		}
		if m.AccessPlan.BillingProfile != nil {
			snapshot.BillingProfileID = m.AccessPlan.BillingProfile.ID
			snapshot.BillingProfileName = m.AccessPlan.BillingProfile.Name
			snapshot.DefaultMultiplier = positiveMultiplier(m.AccessPlan.BillingProfile.DefaultMultiplier)
		}
	}
	if m.AccessRouteRule != nil {
		if m.AccessRouteRule.BillingModelSource != "" {
			billingSource = m.AccessRouteRule.BillingModelSource
		}
	}

	switch billingSource {
	case model.AccessBillingModelSourceRequest:
		snapshot.BillingModelName = m.RequestModel
	case model.AccessBillingModelSourceOverride:
		if m.AccessRouteRule != nil && strings.TrimSpace(m.AccessRouteRule.BillingModelOverride) != "" {
			snapshot.BillingModelName = strings.TrimSpace(m.AccessRouteRule.BillingModelOverride)
		} else {
			snapshot.BillingModelName = upstreamModel
		}
	default:
		snapshot.BillingModelName = upstreamModel
	}
	if snapshot.BillingModelName == "" {
		snapshot.BillingModelName = m.RequestModel
	}
	snapshot.BillingModelSource = billingSource

	if m.AccessPlan != nil && m.AccessPlan.BillingProfile != nil {
		snapshot.ModelMultiplier = billingModelMultiplier(m.AccessPlan.BillingProfile.ModelRules, snapshot.BillingModelName)
	}
	snapshot.FinalMultiplier = snapshot.DefaultMultiplier * snapshot.ModelMultiplier

	modelPrice := price.GetLLMPrice(snapshot.BillingModelName)
	if modelPrice == nil {
		return snapshot
	}
	snapshot.BaseInputPrice = modelPrice.Input
	snapshot.BaseOutputPrice = modelPrice.Output
	snapshot.BaseCacheReadPrice = modelPrice.CacheRead
	snapshot.BaseCacheWritePrice = modelPrice.CacheWrite

	if usage == nil {
		return snapshot
	}
	if usage.PromptTokensDetails == nil {
		usage.PromptTokensDetails = &transformerModel.PromptTokensDetails{}
	}

	inputTokens := usage.PromptTokens
	cacheReadTokens := usage.PromptTokensDetails.CachedTokens
	cacheWriteTokens := usage.CacheCreationInputTokens
	if !usage.AnthropicUsage && !usage.SeparateCacheInputTokens {
		inputTokens -= cacheReadTokens
		if inputTokens < 0 {
			inputTokens = 0
		}
	}

	snapshot.FinalInputCost = float64(inputTokens) * snapshot.BaseInputPrice * snapshot.FinalMultiplier * 1e-6
	snapshot.FinalOutputCost = float64(usage.CompletionTokens) * snapshot.BaseOutputPrice * snapshot.FinalMultiplier * 1e-6
	snapshot.FinalCacheReadCost = float64(cacheReadTokens) * snapshot.BaseCacheReadPrice * snapshot.FinalMultiplier * 1e-6
	snapshot.FinalCacheWriteCost = float64(cacheWriteTokens) * snapshot.BaseCacheWritePrice * snapshot.FinalMultiplier * 1e-6
	return snapshot
}

func billingModelMultiplier(rules []model.AccessBillingModelRule, billingModel string) float64 {
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(rule.ModelName), strings.TrimSpace(billingModel)) {
			return positiveMultiplier(rule.Multiplier)
		}
	}
	return 1
}

func positiveMultiplier(value float64) float64 {
	if value <= 0 {
		return 1
	}
	return value
}

func applyBillingSnapshotToRelayLog(relayLog *model.RelayLog, snapshot model.AccessPlanBillingSnapshot) {
	if relayLog == nil {
		return
	}
	relayLog.AccessPlanID = snapshot.AccessPlanID
	relayLog.AccessPlanSlug = snapshot.AccessPlanSlug
	relayLog.AccessPlanName = snapshot.AccessPlanName
	relayLog.RouteProfileID = snapshot.RouteProfileID
	relayLog.RouteProfileName = snapshot.RouteProfileName
	relayLog.BillingProfileID = snapshot.BillingProfileID
	relayLog.BillingProfileName = snapshot.BillingProfileName
	relayLog.BillingModel = snapshot.BillingModelName
	relayLog.BaseInputPrice = snapshot.BaseInputPrice
	relayLog.BaseOutputPrice = snapshot.BaseOutputPrice
	relayLog.BaseCacheReadPrice = snapshot.BaseCacheReadPrice
	relayLog.BaseCacheWritePrice = snapshot.BaseCacheWritePrice
	relayLog.DefaultMultiplier = snapshot.DefaultMultiplier
	relayLog.ModelMultiplier = snapshot.ModelMultiplier
	relayLog.FinalMultiplier = snapshot.FinalMultiplier
	relayLog.FinalInputCost = snapshot.FinalInputCost
	relayLog.FinalOutputCost = snapshot.FinalOutputCost
	relayLog.FinalCacheReadCost = snapshot.FinalCacheReadCost
	relayLog.FinalCacheWriteCost = snapshot.FinalCacheWriteCost
}

// shouldBillUser 是用户计费闸门的唯一判据。
//
// 抽成独立函数不是为了好看，是为了能被两侧同时钉住：只测"该扣的扣了"会漏掉
// "不该扣的扣了"——后者同样严重（无效请求能烧光用户余额）。
//
//	success=true                     → 扣（正常的成功路径，行为不变）
//	success=false, delivered, cost>0 → 扣（已交付内容后中断：上游成本已产生）
//	success=false, 未交付            → 不扣（选路失败/上游 4xx/首字节前取消）
//	success=false, delivered, cost=0 → 不扣（没有可计费的金额）
func shouldBillUser(success, delivered bool, totalCost float64) bool {
	if success {
		return true
	}
	return delivered && totalCost > 0
}

// MarkUsageDelivered 标记「本次请求已经向下游交付了实质内容」, 即使最终终态是失败。
//
// 必须与 attemptResult.Written == true 严格成对调用: 只有那两个出口, 客户端拿到的
// 字节才真的回不去(不能再故障转移), 也就才存在"已经消耗了上游成本"的事实。
func (m *RelayMetrics) MarkUsageDelivered() {
	m.usageDelivered = true
}

func (m *RelayMetrics) Save(ctx context.Context, success bool, err error, attempts []model.ChannelAttempt) {
	duration := time.Since(m.StartTime)
	persistCtx, cancel := metricsPersistContext()
	defer cancel()

	globalStats := model.StatsMetrics{
		WaitTime:        duration.Milliseconds(),
		InputToken:      m.Stats.InputToken,
		OutputToken:     m.Stats.OutputToken,
		InputCost:       m.Stats.InputCost,
		OutputCost:      m.Stats.OutputCost,
		CacheHitToken:   m.Stats.CacheHitToken,
		CacheWriteToken: m.Stats.CacheWriteToken,
		CacheInputToken: m.Stats.CacheInputToken,
	}
	if success {
		globalStats.RequestSuccess = 1
	} else if !isClientAbortError(err) {
		globalStats.RequestFailed = 1
	}

	channelID, channelName := finalChannel(attempts)
	op.StatsTotalUpdate(globalStats)
	op.StatsHourlyUpdate(globalStats)
	op.StatsDailyUpdate(persistCtx, globalStats)
	op.StatsAPIKeyUpdate(m.APIKeyID, globalStats)
	op.StatsChannelUpdate(channelID, globalStats)
	// 落库分工: UserRecordRelayIP 是纯审计字段, 只写内存 + 周期任务批量补 DB;
	// UserRecordUsage 是计费(月卡/余额), 必须留在同步路径上扣减——额度判断零滞后,
	// 不能等批刷(用户定)。saveLog 里的中继日志同样已改成后台批量落库。
	if err := op.UserRecordRelayIP(m.UserID, m.RequestIP, m.StartTime.Unix(), persistCtx); err != nil {
		log.Warnf("failed to record user relay ip: %v", err)
	}
	// 计费闸门: 按「实际产生的可计费用量」结算, 而不是终态成功布尔值。
	//
	// 流式请求可能在已经交付了大半内容之后才失败(客户端提前 cancel ->
	// handleStreamResponse 返回 "client disconnected during stream";
	// 上游半流读取错误 -> return error)。这类出口在 relay.go:1092-1100
	// 返回 Success=false / Written=true: 上游成本已经产生、客户端也已经拿到
	// 实质内容, 却一分不扣 —— 收到首 token 就 cancel 可稳定白嫖。
	//
	// 两个条件必须同时成立:
	//   ① usageDelivered: relay 在 Written 出口标记过, 确有实质内容交付;
	//   ② totalCost > 0 : 这次请求确实产生了可计费用量。
	// 只有①没有②(没算出成本)不该扣; 只有②没有①(完全没交付: 选路失败/
	// 上游 4xx/5xx/首字节前取消)也必须不扣, 否则无效请求就能烧掉用户余额。
	totalCost := globalStats.InputCost + globalStats.OutputCost
	if shouldBillUser(success, m.usageDelivered, totalCost) {
		if err := op.UserRecordUsage(m.UserID, totalCost, persistCtx); err != nil {
			log.Warnf("failed to record user usage: %v", err)
		}
	}

	errorStatus, errorCode, errorStrategy, _ := relayErrorDetails(err)
	log.Infof("relay complete: model=%s, channel=%d(%s), success=%t, duration=%dms, input_token=%d, output_token=%d, cache_hit_token=%d, cache_rate=%.2f%%, input_cost=%f, output_cost=%f, total_cost=%f, attempts=%d, error_status=%d, error_code=%s, error_strategy=%s",
		m.RequestModel, channelID, channelName, success, duration.Milliseconds(),
		m.Stats.InputToken, m.Stats.OutputToken,
		m.Stats.CacheHitToken, cacheHitRate(m.Stats.CacheHitToken, m.Stats.CacheInputToken)*100,
		m.Stats.InputCost, m.Stats.OutputCost, m.Stats.InputCost+m.Stats.OutputCost,
		len(attempts), errorStatus, errorCode, errorStrategy)

	m.logDiagnostics(success, duration, attempts, channelID, channelName)

	m.saveLog(persistCtx, err, duration, attempts, channelID, channelName)
}

// logDiagnostics 在 diagnostic_mode 开启时额外打印一行各阶段耗时 + 逐次尝试原因,
// 便于排障"卡在哪一层"(等响应头/首字/生成)。关闭时的每请求成本仅一次(带缓存的)设置读取,
// 与 debug_load_balancer 同源。所有字段皆为内部计时/下游日志, 不触上游出站字节(shape 安全)。
// header_wait / first_token / gen_start 为 -1 表示该阶段未测到(如请求在拿到响应头前就失败)。
func (m *RelayMetrics) logDiagnostics(success bool, duration time.Duration, attempts []model.ChannelAttempt, channelID int, channelName string) {
	enabled, err := op.SettingGetBool(model.SettingKeyDiagnosticMode)
	if err != nil || !enabled {
		return
	}

	headerWaitMs := int64(-1)
	if !m.RequestSentTime.IsZero() && !m.ResponseHeaderTime.IsZero() {
		headerWaitMs = m.ResponseHeaderTime.Sub(m.RequestSentTime).Milliseconds()
	}
	firstTokenMs := int64(-1)
	if !m.FirstTokenTime.IsZero() {
		firstTokenMs = m.FirstTokenTime.Sub(m.StartTime).Milliseconds()
	}
	genStartMs := int64(-1)
	if !m.FirstTokenTime.IsZero() && !m.ResponseHeaderTime.IsZero() {
		genStartMs = m.FirstTokenTime.Sub(m.ResponseHeaderTime).Milliseconds()
	}

	var b strings.Builder
	for i, a := range attempts {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "#%d:ch%d(%s)/%s/%dms", a.AttemptNum, a.ChannelID, a.ChannelName, a.Status, a.Duration)
		if msg := strings.TrimSpace(a.Msg); msg != "" {
			fmt.Fprintf(&b, "[%s]", msg)
		}
	}

	log.Infof("[diag] model=%s channel=%d(%s) success=%t total=%dms header_wait=%dms first_token=%dms gen_start=%dms attempts=%d | %s",
		m.RequestModel, channelID, channelName, success, duration.Milliseconds(),
		headerWaitMs, firstTokenMs, genStartMs, len(attempts), b.String())
}

func metricsPersistContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), metricsPersistTimeout)
}

func finalChannel(attempts []model.ChannelAttempt) (int, string) {
	var lastID int
	var lastName string
	for i := len(attempts) - 1; i >= 0; i-- {
		a := attempts[i]
		if a.Status == model.AttemptSuccess {
			return a.ChannelID, a.ChannelName
		}
		if lastID == 0 && a.ChannelID != 0 {
			lastID = a.ChannelID
			lastName = a.ChannelName
		}
	}
	return lastID, lastName
}

// retryAttemptCount 数的是「真实的渠道尝试次数」, 也就是 len(attempts) 去掉竞速输家。
//
// 竞速输家(model.AttemptMsgRacedOut)是同一次请求里并发发出的 contender, 被兄弟
// contender 先赢而取消 —— 它既没失败也没被"重试", 却在 attempts 里留了一条 failed。
// 直接用 len(attempts) 当 total_attempts 会让竞速模式下每一个成功请求都变成 warn /
// 「重试过」, 于是 RetriedOnly 筛选等于全量, 真正的故障转移被淹没。
func retryAttemptCount(attempts []model.ChannelAttempt) int {
	count := 0
	for _, attempt := range attempts {
		if attempt.Status == model.AttemptFailed && attempt.Msg == model.AttemptMsgRacedOut {
			continue
		}
		count++
	}
	return count
}

func routeStickyHit(attempts []model.ChannelAttempt) bool {
	for _, attempt := range attempts {
		if attempt.Sticky && attempt.Status == model.AttemptSuccess {
			return true
		}
	}
	return false
}

func usageAuditFromInternalResponse(resp *transformerModel.InternalLLMResponse, err error) (string, string) {
	if resp != nil && resp.Usage != nil {
		hitTokens, writeTokens, _ := usageCacheStats(resp.Usage)
		if resp.Usage.PromptTokens == 0 && resp.Usage.CompletionTokens == 0 && hitTokens == 0 && writeTokens == 0 {
			return model.RelayLogUsageSourceUpstream, model.RelayLogUsageMissingReasonZeroUsageReported
		}
		return model.RelayLogUsageSourceUpstream, ""
	}
	if isClientAbortError(err) {
		return model.RelayLogUsageSourceNoUsage, model.RelayLogUsageMissingReasonClientAborted
	}
	if resp == nil {
		return model.RelayLogUsageSourceNoUsage, model.RelayLogUsageMissingReasonNoInternalResponse
	}
	return model.RelayLogUsageSourceNoUsage, model.RelayLogUsageMissingReasonUpstreamUsageMissing
}

func usageAuditFromStats(stats model.StatsMetrics, usageSeen bool, err error, opaque bool) (string, string) {
	if usageSeen {
		if stats.InputToken == 0 && stats.OutputToken == 0 && stats.CacheHitToken == 0 && stats.CacheWriteToken == 0 {
			return model.RelayLogUsageSourceUpstream, model.RelayLogUsageMissingReasonZeroUsageReported
		}
		return model.RelayLogUsageSourceUpstream, ""
	}
	if isClientAbortError(err) {
		return model.RelayLogUsageSourceNoUsage, model.RelayLogUsageMissingReasonClientAborted
	}
	if opaque {
		return model.RelayLogUsageSourceNoUsage, model.RelayLogUsageMissingReasonOpaqueResponse
	}
	return model.RelayLogUsageSourceNoUsage, model.RelayLogUsageMissingReasonUpstreamUsageMissing
}

func cleanRelayEndpointName(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	endpoint = strings.TrimPrefix(endpoint, "/")
	endpoint = strings.TrimPrefix(endpoint, "v1/")
	endpoint = strings.TrimPrefix(endpoint, "v1beta/")
	endpoint = strings.Trim(endpoint, "/")
	endpoint = strings.ReplaceAll(endpoint, ":", "_")
	endpoint = strings.ReplaceAll(endpoint, "/", "_")
	if endpoint == "" {
		return "unknown"
	}
	return endpoint
}

func (m *RelayMetrics) saveLog(ctx context.Context, err error, duration time.Duration, attempts []model.ChannelAttempt, channelID int, channelName string) {
	actualModel := m.ActualModel
	if actualModel == "" {
		actualModel = m.RequestModel
	}
	billingSnapshot := m.currentBillingSnapshot(actualModel)

	// 回显三态只算一次, 保证 UpstreamModelMismatch / UpstreamMismatchCode / ModelFamilyVerdict
	// 三列互相一致(mismatch 与 code 沿用早先口径: 只拿 m.ActualModel 比, 不做 RequestModel 回退)。
	mismatch := upstreamModelMismatch(m.ActualModel, m.UpstreamDeclaredModel)
	mismatchCode := UpstreamModelMismatchCode(mismatch, m.ActualModel, m.UpstreamDeclaredModel)
	familyVerdict, vendorHint := evalModelFamilyVerdict(m.RequestModel, m.ActualModel, m.UpstreamDeclaredModel, mismatchCode)

	relayLog := model.RelayLog{
		UserID:                m.UserID,
		APIKeyID:              m.APIKeyID,
		RequestIP:             m.RequestIP,
		RequestUserAgent:      truncateUserAgentForLog(m.RequestUserAgent),
		Time:                  m.StartTime.Unix(),
		RequestEndpoint:       m.RequestEndpoint,
		RequestPath:           m.RequestPath,
		RequestModelName:      m.RequestModel,
		ChannelName:           channelName,
		ChannelId:             channelID,
		ChannelKeyRemark:      m.ChannelKeyRemark,
		ActualModelName:       actualModel,
		UpstreamResponseModel: m.UpstreamDeclaredModel,
		UpstreamModelMismatch: mismatch,
		UpstreamMismatchCode:  mismatchCode,
		ModelFamilyVerdict:    familyVerdict,
		ModelVendorHint:       vendorHint,
		UseTime:               int(duration.Milliseconds()),
		Attempts:              attempts,
		TotalAttempts:         retryAttemptCount(attempts),
		SessionKey:            m.SessionKey,
		SessionSource:         m.SessionSource,
		RouteStickyHit:        routeStickyHit(attempts),
		IsStream:              m.InternalRequest != nil && m.InternalRequest.Stream != nil && *m.InternalRequest.Stream,
		PromptOverrideMode:    string(m.PromptSnapshot.Mode),
		PromptOverrideSources: append([]string(nil), m.PromptSnapshot.Sources...),
	}
	applyBillingSnapshotToRelayLog(&relayLog, billingSnapshot)

	// 推理强度: InternalRequest 是与转发链共享的指针, 此处读到的是经 gpt-5.6 自动抬升等
	// 归一化后的有效值(见 transformer/model/model.go:ReasoningEffort)。reasoning_effort 是
	// 客户端自由透传字段(Validate 不校验), 必须按列宽 size:32 截断——否则一个超长值会让
	// PG/MySQL 批量 INSERT 失败, 而 flush 失败的坏行不排空, 会卡死整个 relay 日志落库批次。
	// 纯审计快照, 截断无害; 已知 effort(high/low/max/…) 远短于 32。
	if m.InternalRequest != nil {
		effort := m.InternalRequest.ReasoningEffort
		if r := []rune(effort); len(r) > 32 {
			effort = string(r[:32])
		}
		relayLog.ReasoningEffort = effort
	}

	if apiKey, getErr := op.APIKeyGet(m.APIKeyID, ctx); getErr == nil {
		relayLog.RequestAPIKeyName = apiKey.Name
	}
	// 用户名: 与 APIKeyGet 同样按 id 反查, 命中失败则留空(不阻断日志写入)。
	if user, getErr := op.UserGet(m.UserID); getErr == nil {
		relayLog.UserName = user.Username
	}

	// 首字时间
	if !m.FirstTokenTime.IsZero() {
		relayLog.Ftut = int(m.FirstTokenTime.Sub(m.StartTime).Milliseconds())
	}

	// Usage. Token/cost fields come from m.Stats, which holds either the real
	// upstream usage or the local estimate (see SetInternalResponse). This keeps the
	// logged tokens consistent with the cost/billing that m.Stats also feeds — a
	// locally-estimated usage would otherwise log 0 tokens while the cost reflected
	// the estimate. usage_missing_reason (from resp.Usage) still flags that the count
	// is estimated, not upstream-authoritative. The detailed cache-creation breakdown
	// is only available from the raw upstream usage.
	relayLog.UsageSource, relayLog.UsageMissingReason = usageAuditFromInternalResponse(m.InternalResponse, err)
	if m.usageEstimated {
		// Tokens/cost were counted locally because the upstream omitted usage; mark the
		// source as an estimate (keep the missing reason to say why it was estimated).
		relayLog.UsageSource = model.RelayLogUsageSourceLocalEstimate
	}
	relayLog.InputTokens = int(m.Stats.InputToken)
	relayLog.OutputTokens = int(m.Stats.OutputToken)
	relayLog.CacheHitTokens = int(m.Stats.CacheHitToken)
	relayLog.CacheWriteTokens = int(m.Stats.CacheWriteToken)
	relayLog.CacheInputTokens = int(m.Stats.CacheInputToken)
	relayLog.CacheHitRate = cacheHitRate(m.Stats.CacheHitToken, m.Stats.CacheInputToken)
	relayLog.Cost = m.Stats.InputCost + m.Stats.OutputCost
	if m.InternalResponse != nil && m.InternalResponse.Usage != nil {
		relayLog.CacheWrite5mTokens = int(m.InternalResponse.Usage.CacheCreation5mInputTokens)
		relayLog.CacheWrite1hTokens = int(m.InternalResponse.Usage.CacheCreation1hInputTokens)
	}

	// 请求内容
	if m.InternalRequest != nil {
		reqJSON, jsonErr := json.Marshal(m.InternalRequest)
		if jsonErr != nil {
			relayLog.RequestContent = string(reqJSON)
		} else if m.ParamOverride == "" {
			relayLog.RequestContent = string(reqJSON)
		} else {
			var reqMap map[string]any
			if err := json.Unmarshal(reqJSON, &reqMap); err != nil {
				relayLog.RequestContent = string(reqJSON)
			} else {
				var override map[string]any
				if err := json.Unmarshal([]byte(m.ParamOverride), &override); err != nil {
					relayLog.RequestContent = string(reqJSON)
				} else {
					maps.Copy(reqMap, override)
					if finalJSON, err := json.Marshal(reqMap); err != nil {
						relayLog.RequestContent = string(reqJSON)
					} else {
						relayLog.RequestContent = string(finalJSON)
					}
				}
			}
		}
		// 入库前打码：只改写进日志的这份，发给上游的请求体一个字节不动。
		relayLog.RequestContent = redactLogBody(relayLog.RequestContent)
	}

	// 真实出站请求体（开关打开且采集成功时才有；否则留空，不假装有值）。
	if m.upstreamRequestBody != "" {
		relayLog.UpstreamRequestBody = m.upstreamRequestBody
	}

	// 响应内容
	if m.InternalResponse != nil {
		// 上游自报的后端标识与服务档位：纯观测，原样存，不做任何推断或改写。
		// 截断是防御性的——上游理论上可以回一个超长串把列撑爆。
		relayLog.UpstreamSystemFingerprint = truncateRunes(m.InternalResponse.SystemFingerprint, 128)
		relayLog.UpstreamServiceTier = truncateRunes(m.InternalResponse.ServiceTier, 32)

		respForLog := m.filterResponseForLog(m.InternalResponse)
		if respJSON, jsonErr := json.Marshal(respForLog); jsonErr == nil {
			if m.InternalResponse.Usage != nil && m.InternalResponse.Usage.AnthropicUsage {
				respStr := string(respJSON)
				old := `"usage":{`
				insert := fmt.Sprintf(`"usage":{"cache_creation_input_tokens":%d,`, m.InternalResponse.Usage.CacheCreationInputTokens)
				respJSON = []byte(strings.Replace(respStr, old, insert, 1))
			}
			relayLog.ResponseContent = redactLogBody(string(respJSON))
		}
	}

	// 错误信息
	if err != nil {
		relayLog.Error = errSafeMessage(err)
		if status, code, strategy, ok := relayErrorDetails(err); ok {
			relayLog.ErrorStatus = status
			relayLog.ErrorCode = code
			relayLog.ErrorStrategy = strategy
		}
	}

	if logErr := op.RelayLogAdd(ctx, relayLog); logErr != nil {
		log.Warnf("failed to save relay log: %v", logErr)
	}
}

func usageCacheStats(usage *transformerModel.Usage) (hitTokens int64, writeTokens int64, inputTokens int64) {
	if usage == nil {
		return 0, 0, 0
	}
	if usage.PromptTokensDetails != nil {
		hitTokens = usage.PromptTokensDetails.CachedTokens
	}
	writeTokens = usage.CacheCreationInputTokens
	inputTokens = usage.PromptTokens
	if usage.AnthropicUsage || usage.SeparateCacheInputTokens {
		inputTokens += hitTokens + writeTokens
	}
	if inputTokens < hitTokens+writeTokens {
		inputTokens = hitTokens + writeTokens
	}
	return hitTokens, writeTokens, inputTokens
}

func cacheHitRate(hitTokens, inputTokens int64) float64 {
	if hitTokens <= 0 || inputTokens <= 0 {
		return 0
	}
	return float64(hitTokens) / float64(inputTokens)
}

// filterResponseForLog 创建响应的浅拷贝，过滤掉 images、MultipleContent 中的图片数据和 Audio.Data 以减少存储压力
func (m *RelayMetrics) filterResponseForLog(resp *transformerModel.InternalLLMResponse) *transformerModel.InternalLLMResponse {
	if resp == nil {
		return nil
	}

	filterMsg := func(msg *transformerModel.Message) *transformerModel.Message {
		if msg == nil {
			return nil
		}
		c := *msg
		c.Images = nil
		if len(c.Content.MultipleContent) > 0 {
			parts := make([]transformerModel.MessageContentPart, 0, len(c.Content.MultipleContent))
			for _, p := range c.Content.MultipleContent {
				if p.Type == "image_url" && p.ImageURL != nil {
					parts = append(parts, transformerModel.MessageContentPart{
						Type:     "image_url",
						ImageURL: &transformerModel.ImageURL{URL: "[image data omitted for storage]"},
					})
				} else {
					parts = append(parts, p)
				}
			}
			c.Content = transformerModel.MessageContent{Content: c.Content.Content, MultipleContent: parts}
		}
		if c.Audio != nil && c.Audio.Data != "" {
			a := *c.Audio
			a.Data = "[audio data omitted for storage]"
			c.Audio = &a
		}
		return &c
	}

	filtered := *resp
	filtered.Choices = make([]transformerModel.Choice, len(resp.Choices))
	for i, choice := range resp.Choices {
		filtered.Choices[i] = choice
		filtered.Choices[i].Message = filterMsg(choice.Message)
		filtered.Choices[i].Delta = filterMsg(choice.Delta)
	}
	return &filtered
}
