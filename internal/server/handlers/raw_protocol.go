package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/relay"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/router"
)

func init() {
	router.NewGroupRouter("/v1").
		Use(middleware.APIKeyAuth()).
		AddRoute(
			router.NewRoute("/completions", http.MethodPost).
				Handle(completions),
		).
		AddRoute(
			router.NewRoute("/edits", http.MethodPost).
				Handle(legacyEdits),
		).
		AddRoute(
			router.NewRoute("/responses/compact", http.MethodPost).
				Handle(responsesCompact),
		).
		AddRoute(
			router.NewRoute("/audio/speech", http.MethodPost).
				Handle(audioSpeech),
		).
		AddRoute(
			router.NewRoute("/audio/transcriptions", http.MethodPost).
				Handle(audioTranscriptions),
		).
		AddRoute(
			router.NewRoute("/audio/translations", http.MethodPost).
				Handle(audioTranslations),
		).
		// xAI（Grok）语音 REST：文字转语音 / 语音转文字。照 sub2api
		// routes/gateway.go 的 /v1/tts、/v1/stt 与 CLIProxyAPI server_routes.go
		// 的 /v1/tts。上游路径与客户端路径同名（<base>/v1/tts、<base>/v1/stt），
		// 渠道选择、重试、熔断、日志全部复用 RawProtocolHandler。
		AddRoute(
			router.NewRoute("/tts", http.MethodPost).
				Handle(grokTTS),
		).
		AddRoute(
			router.NewRoute("/stt", http.MethodPost).
				Handle(grokSTT),
		).
		AddRoute(
			router.NewRoute("/moderations", http.MethodPost).
				Handle(moderations),
		).
		AddRoute(
			router.NewRoute("/rerank", http.MethodPost).
				Handle(rerank),
		).
		// 决策类上游（systemone 协议）的透传端点。
		//
		// 这类模型不接受 chat/messages 那套 messages 数组，它要的是
		// {model, state, questions} 并把答案按 id 放进 answers。oct 现有的所有
		// 入站协议都会把正文改写成对话结构再发，因此只能走 raw 透传：请求体原样
		// 交给上游（只重写 model 字段以支持 model_mapping），响应体原样回给客户端。
		//
		// 认证、渠道路由、重试、熔断、计费（上游 usage 里带 input_tokens /
		// output_tokens，usageScanner 能直接认）全部复用 RawProtocolHandler，
		// 不新建第二套链路。
		AddRoute(
			router.NewRoute("/systemone", http.MethodPost).
				Handle(systemone),
		)

	router.NewGroupRouter("").
		Use(middleware.APIKeyAuth()).
		AddRoute(
			router.NewRoute("/responses/compact", http.MethodPost).
				Handle(responsesCompact),
		)

	router.NewGroupRouter("/backend-api/codex").
		Use(middleware.APIKeyAuth()).
		AddRoute(
			router.NewRoute("/responses/compact", http.MethodPost).
				Handle(responsesCompact),
		)
}

func completions(c *gin.Context) {
	relay.RawProtocolHandler(relay.RawProtocolOptions{
		Endpoint: "/completions",
		Name:     "completions",
	}, c)
}

func legacyEdits(c *gin.Context) {
	relay.RawProtocolHandler(relay.RawProtocolOptions{
		Endpoint: "/edits",
		Name:     "edits",
	}, c)
}

func responsesCompact(c *gin.Context) {
	relay.RawProtocolHandler(relay.RawProtocolOptions{
		Endpoint:   "/responses/compact",
		Name:       "responses_compact",
		NonBilling: true,
	}, c)
}

func audioSpeech(c *gin.Context) {
	relay.RawProtocolHandler(relay.RawProtocolOptions{
		Endpoint:        "/audio/speech",
		Name:            "audio_speech",
		BinaryResponse:  true,
		ResponseLogNote: "audio response omitted for storage",
	}, c)
}

func audioTranscriptions(c *gin.Context) {
	relay.RawProtocolHandler(relay.RawProtocolOptions{
		Endpoint: "/audio/transcriptions",
		Name:     "audio_transcriptions",
	}, c)
}

func audioTranslations(c *gin.Context) {
	relay.RawProtocolHandler(relay.RawProtocolOptions{
		Endpoint: "/audio/translations",
		Name:     "audio_translations",
	}, c)
}

// grokTTSDefaultModel 是 /v1/tts 正文不带 model 时用来**选渠道与计费**的模型名。
// xAI 的 TTS 正文协议里没有 model 字段；名字照 CLIProxyAPI
// openai_speech_handlers.go 的 defaultXAISpeechModel。管理员要把它（或映射）配到
// 承接 Grok 语音的渠道上，否则「model not supported / no available channel」。
const grokTTSDefaultModel = "grok-tts"

// grokSTTDefaultModel 同理：xAI STT REST 的多段表单里没有 model，默认模型是
// grok-voice-transcribe-2.0（xAI 文档 STT 默认值）。
const grokSTTDefaultModel = "grok-voice-transcribe-2.0"

// grokTTS 转发 xAI POST /v1/tts（JSON → 音频二进制；with_timestamps=true 时上游
// 回 JSON，同样原样透传）。
//
//   - OmitAbsentModel：客户端没写 model 就不往上游正文里补（xAI 协议里没有这个字段）。
//   - BinaryResponse：音频不进日志，Content-Type 原样透传。
//   - PerCallBilling：音频响应没有 token 用量，按价格行 per_call 计次（与视频同口径）。
func grokTTS(c *gin.Context) {
	relay.RawProtocolHandler(relay.RawProtocolOptions{
		Endpoint:        "/tts",
		Name:            "audio_tts",
		DefaultModel:    grokTTSDefaultModel,
		BinaryResponse:  true,
		ResponseLogNote: "audio response omitted for storage",
		OmitAbsentModel: true,
		PerCallBilling:  true,
	}, c)
}

// grokSTT 转发 xAI POST /v1/stt（multipart，file 必须是最后一段 → raw 的多段复制
// 保持原顺序、只替换已有的 model 段、从不追加，满足这个约束）。
func grokSTT(c *gin.Context) {
	relay.RawProtocolHandler(relay.RawProtocolOptions{
		Endpoint:        "/stt",
		Name:            "audio_stt",
		DefaultModel:    grokSTTDefaultModel,
		OmitAbsentModel: true,
		PerCallBilling:  true,
	}, c)
}

func moderations(c *gin.Context) {
	relay.RawProtocolHandler(relay.RawProtocolOptions{
		Endpoint:     "/moderations",
		Name:         "moderations",
		DefaultModel: "omni-moderation-latest",
	}, c)
}

func rerank(c *gin.Context) {
	relay.RawProtocolHandler(relay.RawProtocolOptions{
		Endpoint: "/rerank",
		Name:     "rerank",
	}, c)
}

// systemone 透传决策类上游（{model, state, questions} -> {answers}）。
//
// 为什么用 raw 而不是走 chat 管道：这类模型的请求体没有 messages，响应体没有
// choices，任何一处协议转换都会把它改坏。raw 只重写 model 字段、其余字节原样
// 转发，正好是它需要的语义。
func systemone(c *gin.Context) {
	relay.RawProtocolHandler(relay.RawProtocolOptions{
		Endpoint: "/systemone",
		Name:     "systemone",
	}, c)
}
