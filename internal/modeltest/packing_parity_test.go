package modeltest

// next-1129 第 9 件：两套上游整装的对照测试（用户 2026-11-29 02:56 拍板方案三）。
//
// 背景：网关有两条把"一次探测"变成"一个发往上游的 HTTP 请求"的整装路径：
//   - 正式整装（relay 主链）：RunChannel → tryChannelViaRelay → RelaySender.Send →
//     relay.HandlerForChannelTest → parseRequest / applyTransformOptions /
//     applyChannelOutboundShaping / 出站适配器 TransformRequest / ApplyParamOverride。
//     这条链就是生产转发本身（只换成"钉死渠道、不鉴权、不选路"）。
//   - 直测链路（runner.go）：Run（带 ChannelID、不带 directChannel）→ tryChannel →
//     testChannelKey → 自拼 InternalLLMRequest + 探针形状 helper（prepareCodex*/forceClaude*）
//     → 同一个出站适配器 TransformRequest。模型校验探针、断路器探针、按路由跑的检测都走它。
//
// 本文件把「同一条探测请求」分别喂给两条路，用 httptest 假上游截获各自真正发出去的
// HTTP body，逐字节比对：白名单外的任何差异都算红（用户 02:56 的口径）。
//
// 这是"拆底层"（照 axonhub 共用整条 pipeline）之前的对照基线：底座没并之前，两条路的
// 出站字节只能靠这份测试钉住；将来拆底座时它必须仍然通过，或差异经批准进白名单。
//
// 白名单（用户 02:56 原文）与本文的对应关系：
//   1. max_tokens 64000（探测固定值）—— 直测链路给探针固定预算（Anthropic 64000、
//      其余 256），正式整装原样透传调用方的值；对应 $.max_tokens /
//      $.max_completion_tokens / $.generationConfig.maxOutputTokens。
//      同时在这里把 Anthropic 的 64000 钉死（只钉探针一侧的契约，不放过取值）。
//   2. 不映射模型（直测用用户原始模型名）—— 两个入口（Run / RunChannel）都会先在
//      runOne 里做渠道模型映射，所以本对照反而要求两侧都把 vanity 名映射成上游名；
//      真正"不映射"只发生在 ProbeSender（调用方传已映射名），不在这两条路的重叠范围。
//      这里按"映射必须一致"来钉（比允许不同更严）。
//   3. 指定 key（直测可指定单 key）—— 直测链路逐 key 尝试，正式整装按生产口径选 key。
//      key 只进出站 Authorization（不在 body 里），所以本对照给渠道配两把 key，要求
//      两条路无论选中哪把，body 都必须一致；另外钉住两把 key 都不许出现在 body 里。
//   4. codex 探测保留 instructions + 工具 —— 直测链路保留探针自己的 instructions 与
//      4 个精简工具，正式整装按 CLI 形状补完整工具表（runner-unify-refs-1129.md 4.2
//      的 ①②③：instructions、tools、text.verbosity）。
//
// 白名单条目的写法：每条都带理由，逐条列在对应 case 的 allow 里（超出用户原话字面的
// 只有 responses 的 $.text —— 依据是 runner-unify-refs-1129.md 4.2 ③ 把
// text.verbosity 明确列为已知差异；要不要长期允许请主管拍板，删一行就变红）。
// 反过来，正式整装替非 CLI 调用方补的 Claude CLI 顶层成员（thinking / output_config /
// context_management / safeguards，见 relay/claude_shape_body.go 的
// ensureClaudeCLIShapeTopLevelKeys）**故意不列白名单**：它们是本基线要暴露的真实差异，
// 不是用户批过的"故意差异"。
//
// 逐请求随机标识（会话 id / 轮次 id / 时间戳）：两条路都是"每次请求现场生成"，
// 真实 CLI 本身也每次都不同，值没法比；这里只把 uuid 与轮次时间戳归一化掉，
// 路径、键序、键数以及其余取值仍然逐字节比（同一个 builder 出的形状必须一致）。
//
// 全部走 httptest 假上游——本文件不产生任何真实上游请求。

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// packingParityPrompt 是对照用的固定题面。
//
// 不能用 defaultPromptSentinel（"Reply with exactly OK."）：两条路见到这个哨兵值都会
// 各自现场随机出一道算式题（nextDefaultModelTestPrompt），题面就不是同一句话，
// body 永远对不上。这里用一句哨兵之外的固定文字。
const packingParityPrompt = "packing parity probe: answer with exactly OK."

// packingParityKeys 是渠道上配的两把 key（**假 key**，不是任何真凭据）：直测链路逐
// key 尝试、正式整装按生产口径选 key，本对照要求"选中哪把都不改 body"。
// 日志里只回显后 4 位（见下面的 key 泄漏断言），真 key 不会进这份测试。
var packingParityKeys = []string{"sk-parity-not-a-real-key-0001", "sk-parity-not-a-real-key-0002"}

// packingParityAllowance 是白名单里的一条：允许正式整装与直测链路在一个 JSON 路径
// （或它整棵子树）上不同，也允许该路径只在一侧出现。
type packingParityAllowance struct {
	path   string
	reason string
}

// packingParityCase 是一个渠道类型下的对照单元。
type packingParityCase struct {
	name string
	// channelType 是被测渠道类型（出站适配器由它选）。
	channelType outbound.OutboundType
	// endpoint 是检测端点名（入站协议），两条路用同一个值。
	endpoint string
	// requestModel 是调用方（检测页/检测请求）写的模型名，故意与 upstreamModel
	// 不同，用来钉"渠道模型映射两侧都生效"。
	requestModel string
	// upstreamModel 是映射之后真正写进 body 的模型名。
	upstreamModel string
	// customFullPath 非空表示这是自定义渠道（type 6）：渠道里存的是完整 URL，
	// 端点必须显式选 "custom" 才原样使用它。
	customFullPath string
	// reply 是假上游对这一次请求的应答（两条路各自都要成功跑完）。
	reply func(w http.ResponseWriter)
	// allow 是本类型在用户白名单下允许的差异。
	allow []packingParityAllowance
	// clientKeyOrder：这个协议的顶层键序由入站客户端决定（Responses / Anthropic
	// 的 *KeyOrder 会把客户端原始键序原样回放）。探测请求是由 RelaySender 现拼的
	// body，探针也没有客户端键序可继承——两边的键序差异来自“输入”而不是整装，
	// 所以只比键集合与取值，不比顺序（其余协议照常比键序）。
	clientKeyOrder bool
}

// packingParityCaptured 是一次被假上游截获的出站请求。
type packingParityCaptured struct {
	method string
	path   string
	query  string
	body   []byte
}

// packingParityUpstream 是一个只记录、不判别（同一个请求只回一份固定应答）的假上游。
type packingParityUpstream struct {
	server *httptest.Server

	mu       sync.Mutex
	captured []packingParityCaptured
}

func newPackingParityUpstream(t *testing.T, reply func(w http.ResponseWriter)) *packingParityUpstream {
	t.Helper()
	upstream := &packingParityUpstream{}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		upstream.mu.Lock()
		upstream.captured = append(upstream.captured, packingParityCaptured{
			method: r.Method,
			path:   r.URL.Path,
			query:  r.URL.RawQuery,
			body:   body,
		})
		upstream.mu.Unlock()
		reply(w)
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

func (u *packingParityUpstream) url() string { return u.server.URL }

func (u *packingParityUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.captured)
}

func (u *packingParityUpstream) since(index int) []packingParityCaptured {
	u.mu.Lock()
	defer u.mu.Unlock()
	if index < 0 || index > len(u.captured) {
		return nil
	}
	return append([]packingParityCaptured(nil), u.captured[index:]...)
}

// packingParityCases 覆盖主链与 runner 都支持的渠道类型：openai chat / responses /
// claude / gemini，外加自定义 chat（type 6，存完整 URL 的那条）。
func packingParityCases() []packingParityCase {
	const (
		chatModel      = "gpt-5.2"
		responsesModel = "gpt-5.6-sol"
		claudeModel    = "claude-opus-4-6"
		geminiModel    = "gemini-2.5-pro"
		customModel    = "gpt-6-astra"
	)

	// 白名单第 1 条：探测固定输出预算。两条路的存在性与取值都可以不同。
	budgetAllow := []packingParityAllowance{
		{path: "$.max_tokens", reason: "白名单第 1 条：探测固定预算（Anthropic 64000 / 其余 256）"},
		{path: "$.max_completion_tokens", reason: "白名单第 1 条：同义预算字段（gpt-5 / o 系 chat 报文）"},
		{path: "$.generationConfig.maxOutputTokens", reason: "白名单第 1 条：gemini 报文里的同一份探测预算"},
	}

	return []packingParityCase{
		{
			name:          "openai-chat",
			channelType:   outbound.OutboundTypeOpenAIChat,
			endpoint:      "openai_chat",
			requestModel:  "parity/chat-route",
			upstreamModel: chatModel,
			reply: func(w http.ResponseWriter) {
				writePackingParityBody(w, "application/json", customDerivedChatFixture(chatModel))
			},
			allow: budgetAllow,
		},
		{
			name:          "openai-responses",
			channelType:   outbound.OutboundTypeOpenAIResponse,
			endpoint:      "openai_responses",
			requestModel:  "parity/responses-route",
			upstreamModel: responsesModel,
			reply: func(w http.ResponseWriter) {
				writePackingParityBody(w, "text/event-stream", customDerivedResponsesSSE(responsesModel))
			},
			clientKeyOrder: true,
			allow: []packingParityAllowance{
				{path: "$.instructions", reason: "白名单第 4 条：codex 探测保留自己的 instructions"},
				{path: "$.tools", reason: "白名单第 4 条：codex 探测保留自己的 4 个工具，正式整装补完整工具表"},
				{path: "$.text", reason: "证据 runner-unify-refs-1129.md 4.2 ③：正式整装补 text.verbosity=low，探针不带"},
			},
		},
		{
			name:          "anthropic-claude",
			channelType:   outbound.OutboundTypeAnthropic,
			endpoint:      "anthropic_messages",
			requestModel:  "parity/claude-route",
			upstreamModel: claudeModel,
			reply: func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, packingParityAnthropicSSE(claudeModel))
			},
			clientKeyOrder: true,
			allow: []packingParityAllowance{
				{path: "$.max_tokens", reason: "白名单第 1 条：探测固定 budget 64000（正式整装透传调用方的 32000）"},
			},
		},
		{
			name:          "gemini",
			channelType:   outbound.OutboundTypeGemini,
			endpoint:      "gemini_generate_content",
			requestModel:  "parity/gemini-route",
			upstreamModel: geminiModel,
			reply: func(w http.ResponseWriter) {
				writePackingParityBody(w, "application/json", customDerivedGeminiFixture(geminiModel))
			},
			allow: budgetAllow,
		},
		{
			name:           "custom-chat",
			channelType:    outbound.OutboundTypeCustomOpenAIChat,
			endpoint:       customTestEndpointName,
			requestModel:   "parity/custom-route",
			upstreamModel:  customModel,
			customFullPath: "/gateway/proxy/chat/completions",
			reply: func(w http.ResponseWriter) {
				writePackingParityBody(w, "application/json", customDerivedChatFixture(customModel))
			},
			allow: budgetAllow,
		},
	}
}

// TestPackingParityRelayVsRunner 是两套上游整装的对照基线。
func TestPackingParityRelayVsRunner(t *testing.T) {
	for _, tc := range packingParityCases() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx := setupModelTestDB(t)
			upstream := newPackingParityUpstream(t, tc.reply)

			baseURL := upstream.url()
			if tc.customFullPath != "" {
				baseURL += tc.customFullPath
			}
			channel := dbmodel.Channel{
				Name:     "packing-parity-" + tc.name,
				Type:     tc.channelType,
				Enabled:  true,
				Model:    tc.upstreamModel,
				BaseUrls: []dbmodel.BaseUrl{{URL: baseURL}},
				Keys: []dbmodel.ChannelKey{
					{Enabled: true, ChannelKey: packingParityKeys[0]},
					{Enabled: true, ChannelKey: packingParityKeys[1]},
				},
				// 白名单第 2 条的严版：两个入口都会在 runOne 里先做映射，所以两条路
				// 都必须把 vanity 名映射成上游名，谁漏了映射这份测试就红。
				ModelMapping: map[string]string{tc.requestModel: tc.upstreamModel},
			}
			if err := op.ChannelCreate(&channel, ctx); err != nil {
				t.Fatalf("创建渠道失败: %v", err)
			}
			stored, err := op.ChannelGet(channel.ID, ctx)
			if err != nil {
				t.Fatalf("回读渠道失败: %v", err)
			}

			// Stream 必须显式钉成 false：直测链路的默认是"优先流式"，不钉的话 chat /
			// gemini 两条路的 stream 字段天然不同（那是开关差异，不是整装差异）。
			// Anthropic / Responses 渠道两条路都会按形状强制流式，这个值对它们无效。
			noStream := false
			request := dbmodel.ModelTestRequest{
				Model:          tc.requestModel,
				ChannelID:      channel.ID,
				Endpoint:       tc.endpoint,
				Prompt:         packingParityPrompt,
				Stream:         &noStream,
				TimeoutSeconds: 60,
			}
			if tc.endpoint == customTestEndpointName {
				// "custom" 端点只在显式渠道测试里成立（runOne 的准入就是这么判的）。
				request.TestUpstreamEndpoint = true
			}

			// 直测链路（runner.go：testChannelKey 自拼出站）。
			direct := runPackingParityLeg(t, upstream, "直测链路", func() (dbmodel.ModelTestResponse, error) {
				return Run(ctx, request)
			})
			// 正式整装（relay 主链：RunChannel → RelaySender → HandlerForChannelTest）。
			official := runPackingParityLeg(t, upstream, "正式整装", func() (dbmodel.ModelTestResponse, error) {
				return RunChannel(ctx, *stored, request)
			})

			if direct.method != official.method || direct.path != official.path {
				t.Errorf("%s：两条路打的不是同一个上游地址：正式整装 %s %s（query=%q），直测链路 %s %s（query=%q）",
					tc.name, official.method, official.path, official.query, direct.method, direct.path, direct.query)
			}
			// key 只许出现在 Authorization，不许进 body（两把都不许）。
			for _, key := range packingParityKeys {
				if bytes.Contains(official.body, []byte(key)) || bytes.Contains(direct.body, []byte(key)) {
					t.Fatalf("%s：渠道 key 出现在出站 body 里（只写后 4 位：%s），这是凭据泄漏而不是形状差异", tc.name, key[len(key)-4:])
				}
			}

			checkPackingParityBodies(t, tc, official.body, direct.body)
		})
	}
}

// runPackingParityLeg 跑一条路，并返回它这一次真正发给上游的请求。
//
// 要求每次都只有一次上游请求：换 key / 换渠道的重试会让"第一次到底发了什么"被
// 后一次覆盖，对照就成了自欺。
func runPackingParityLeg(t *testing.T, upstream *packingParityUpstream, leg string, run func() (dbmodel.ModelTestResponse, error)) packingParityCaptured {
	t.Helper()
	before := upstream.count()
	result, err := run()
	if err != nil {
		t.Fatalf("%s 跑不起来: %v", leg, err)
	}
	if len(result.Results) != 1 {
		t.Fatalf("%s 应只产出 1 条结果，实际 %d 条: %#v", leg, len(result.Results), result.Results)
	}
	if !result.Results[0].Success {
		t.Fatalf("%s 没跑通（状态码 %d）：%s", leg, result.Results[0].StatusCode, result.Results[0].Error)
	}
	got := upstream.since(before)
	if len(got) != 1 {
		t.Fatalf("%s 发了 %d 次上游请求，对照要求恰好 1 次（重试会掩盖第一次的字节）", leg, len(got))
	}
	return got[0]
}

// checkPackingParityBodies 逐字节比两条路的出站 body：白名单外的差异一律报红。
func checkPackingParityBodies(t *testing.T, tc packingParityCase, official, direct []byte) {
	t.Helper()
	officialLines := jsonSkeleton(t, official)
	directLines := jsonSkeleton(t, direct)

	// 白名单第 1 条里点名的那条契约：Anthropic 渠道的探针预算必须还是 64000。
	// 只钉探针一侧——它会把"整条 max_tokens 被白名单放过"变成一个值也不许漂的断言。
	if tc.channelType == outbound.OutboundTypeAnthropic {
		if line, ok := packingParityLine(directLines, "$.max_tokens"); !ok || line != "$.max_tokens=64000" {
			t.Errorf("%s：直测链路在 Anthropic 渠道上必须带 max_tokens=64000（探针固定预算），实际 %q", tc.name, line)
		}
	}

	if bytes.Equal(official, direct) {
		t.Logf("%s：两套整装出站 body 逐字节一致（%d B，%d 条键路径）", tc.name, len(official), len(officialLines))
		return
	}

	officialKept, officialDropped := packingParityDropWhitelisted(officialLines, tc.allow)
	directKept, directDropped := packingParityDropWhitelisted(directLines, tc.allow)
	if len(officialKept) == 0 || len(directKept) == 0 {
		t.Fatalf("%s：白名单把整份 body 都盖掉了（正式整装剩 %d 条、直测链路剩 %d 条），对照失去意义",
			tc.name, len(officialKept), len(directKept))
	}
	for _, line := range officialDropped {
		t.Logf("%s：白名单放过（正式整装）%s", tc.name, line)
	}
	for _, line := range directDropped {
		t.Logf("%s：白名单放过（直测链路）%s", tc.name, line)
	}

	officialKept = packingParityNormalize(officialKept)
	directKept = packingParityNormalize(directKept)
	diffs := packingParityDiffs(officialKept, directKept, !tc.clientKeyOrder)
	if tc.clientKeyOrder {
		t.Logf("%s：键序不参与对照——这个协议的顶层键序由入站客户端决定（*KeyOrder 原样回放），探针没有客户端键序可继承", tc.name)
	}
	// 一处结构性插入（比如正式整装多补了一个顶层成员）会带出很多条路径差异，
	// 逐条最多列 24 条：够定位，又不会把日志冲爆。总数仍然报出来。
	const maxReported = 24
	for index, diff := range diffs {
		if index == maxReported {
			t.Errorf("%s：另有 %d 处白名单外差异未逐条列出（共 %d 处）", tc.name, len(diffs)-maxReported, len(diffs))
			break
		}
		t.Errorf("%s：白名单外差异 %s", tc.name, diff)
	}
}

// packingParityLine 在骨架里找一条精确路径对应的行。
func packingParityLine(lines []string, path string) (string, bool) {
	for _, line := range lines {
		if candidate, _, ok := strings.Cut(line, "="); ok && candidate == path {
			return line, true
		}
	}
	return "", false
}

// packingParityDropWhitelisted 把命中白名单的行从骨架里剔除（两侧都剔，所以白名单
// 字段允许只在一侧出现），并把剔掉的行连同理由返回，供日志把基线说清楚。
func packingParityDropWhitelisted(lines []string, allow []packingParityAllowance) (kept []string, dropped []string) {
	for _, line := range lines {
		matched := ""
		for _, entry := range allow {
			if packingParityPathAllowed(line, entry.path) {
				matched = entry.path + "（" + entry.reason + "）"
				break
			}
		}
		if matched == "" {
			kept = append(kept, line)
			continue
		}
		dropped = append(dropped, line+"  ← 白名单 "+matched)
	}
	return kept, dropped
}

// packingParityPathAllowed：白名单条目按"精确路径或整棵子树"匹配
// （"$.tools" 覆盖 "$.tools[0].function.name" 这类展开后的路径）。
func packingParityPathAllowed(line, prefix string) bool {
	path, _, ok := strings.Cut(line, "=")
	if !ok {
		return false
	}
	if path == prefix {
		return true
	}
	return strings.HasPrefix(path, prefix+".") || strings.HasPrefix(path, prefix+"[")
}

var (
	// packingParityUUID：会话 id / 轮次 id / window id 的形状。
	packingParityUUID = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	// packingParityTurnClock：codex turn metadata 里的轮次起始时间（毫秒）。
	// 兼容 client_metadata 里被二次转义（\"）的那种写法。
	packingParityTurnClock = regexp.MustCompile(`turn_started_at_unix_ms\\?":\s*\d+`)
)

// packingParityVolatilePaths 是"两条路都逐请求现场生成"的标识字段：值没法比
// （真实 CLI 每次也都是新的），但路径、键序与其余字段必须一致。
var packingParityVolatilePaths = map[string]bool{
	"$.prompt_cache_key": true, // codex 会话 id
	"$.metadata.user_id": true, // claude metadata.user_id 里的 session_id
	"$.client_metadata":  true, // codex client_metadata：window id + turn metadata（含 turn id 与时间戳）
}

// packingParityNormalize 把逐请求随机标识归一化，其余行原样返回。
func packingParityNormalize(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		path, value, ok := strings.Cut(line, "=")
		// 对象根按前缀命中：jsonSkeleton 把嵌套对象拍平成全路径，写进表里的是
		// "$.client_metadata" 这类根，实际行是它的子路径（next-1129 集成修正）。
		volatile := false
		if ok {
			for root := range packingParityVolatilePaths {
				if path == root || strings.HasPrefix(path, root+".") {
					volatile = true
					break
				}
			}
		}
		if !ok || !volatile {
			out = append(out, line)
			continue
		}
		value = packingParityUUID.ReplaceAllString(value, "<uuid>")
		value = packingParityTurnClock.ReplaceAllString(value, `turn_started_at_unix_ms:<ms>`)
		out = append(out, path+"="+value)
	}
	return out
}

// packingParityDiffs 先按路径集合比"只在一边出现的键 / 取值不同的键"，再（可选）查键序。
//
// 不按位置逐行比：一处插入会让位置比法把它后面所有键都报成差异，清单没法读。
// 键序本身也是上游可见特征（JSON 成员顺序），所以键集合与取值都一致、只有顺序不同时
// 仍然要报出来——除非调用方声明这个协议本来就从客户端继承键序（clientKeyOrder）。
func packingParityDiffs(official, direct []string, compareOrder bool) []string {
	officialOrder, officialValues := packingParityIndex(official)
	directOrder, directValues := packingParityIndex(direct)

	var out []string
	for _, path := range officialOrder {
		directValue, ok := directValues[path]
		if !ok {
			out = append(out, fmt.Sprintf("只在正式整装出现：%s=%s", path, packingParityClip(officialValues[path])))
			continue
		}
		if directValue != officialValues[path] {
			out = append(out, fmt.Sprintf("取值不同：%s\t正式整装=%s\t直测链路=%s",
				path, packingParityClip(officialValues[path]), packingParityClip(directValue)))
		}
	}
	for _, path := range directOrder {
		if _, ok := officialValues[path]; !ok {
			out = append(out, fmt.Sprintf("只在直测链路出现：%s=%s", path, packingParityClip(directValues[path])))
		}
	}
	if compareOrder && len(out) == 0 && !packingParitySameOrder(officialOrder, directOrder) {
		for index := 0; index < len(officialOrder) && index < len(directOrder); index++ {
			if officialOrder[index] != directOrder[index] {
				out = append(out, fmt.Sprintf("键序不同（第 %d 个键）：正式整装=%s 直测链路=%s",
					index+1, officialOrder[index], directOrder[index]))
				break
			}
		}
	}
	return out
}

// packingParityIndex 把骨架压成"按出现顺序的路径列表 + 路径→取值"。
func packingParityIndex(lines []string) ([]string, map[string]string) {
	order := make([]string, 0, len(lines))
	values := make(map[string]string, len(lines))
	for _, line := range lines {
		path, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if _, seen := values[path]; !seen {
			order = append(order, path)
		}
		values[path] = value
	}
	return order, values
}

func packingParitySameOrder(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// packingParityClip 只截断日志里的大字段（工具表可达数 KB），不影响比较。
func packingParityClip(line string) string {
	const limit = 240
	if len(line) <= limit {
		return line
	}
	return line[:limit] + "…"
}

// writePackingParityBody 写一份假上游应答。
func writePackingParityBody(w http.ResponseWriter, contentType, body string) {
	w.Header().Set("Content-Type", contentType)
	_, _ = io.WriteString(w, body)
}

// packingParityAnthropicSSE 是 Anthropic 流式应答（cloak 生效时两条路都会被强制流式，
// 非流式 JSON 会让它们各自降级重试）。
func packingParityAnthropicSSE(model string) string {
	return "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_parity","type":"message","role":"assistant","model":` + fmt.Sprintf("%q", model) + `,"content":[],"usage":{"input_tokens":3,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
}
