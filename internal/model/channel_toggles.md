# 渠道表单的三个能力开关：需求决定（2026-10-31）

渠道编辑页「模型能力」里的三个开关。这里记录为什么这样做，改之前先读。

## 哪种渠道显示哪个

| 渠道类型 | Claude 1M 上下文 | 思考内容放进正文 | 也接嵌入请求 |
|---|---|---|---|
| OpenAI Chat (0) / Responses (1) / Gemini (3) / 自定义 Chat (6) | 隐藏 | 显示 | 显示 |
| Anthropic (2) | 显示 | 显示 | 显示 + 提示「只有上游中转也提供嵌入时才开」 |
| OpenAI Embedding (5) | 隐藏 | 隐藏 | 隐藏（本来就接嵌入），只单独显示「嵌入接口路径」 |
| 决策渠道 SystemOne (7) | 隐藏 | 隐藏 | 隐藏（手机端整个分组不出现） |

规则只有一处：`web/src/lib/channel-capabilities.ts` 的 `channelToggleVisibility`；
手机端「已开启 N 项」用同文件的 `channelCapabilityOnCount`，只数看得见的开关。

## 1. Claude 1M 上下文（`anthropic_context_1m`，只对 Anthropic 渠道）

- 开着时请求带 `anthropic-beta: context-1m-2025-08-07`；模型名照常填，不用写 `[1m]`。
- **Haiku 永远不加**：真 Claude Code 从不给 haiku 发 context-1m。唯一判据是
  `AnthropicModelSupportsOneMillion`，三条路都用它：CLI 形状下的 beta 合成/保留
  （`BuildClaudeCodeBetaHeader`）、关形状（cloak=never）路径、出站 transformer。
- **统一的 `[1m]` 规则**（`StripOneMillionSuffix` 等，在 `internal/transformer/model/anthropic_alias.go`）：
  只认结尾的 `[1m]`，大小写不敏感，可重复（`x[1M][1m]`）；认 `claude-*` 全名和短名
  `opus / sonnet / fable`。`opus[1m]`→`claude-opus-4-8`，`fable[1m]`→`claude-fable-5`，
  `sonnet[1m]`→`sonnet`（本项目没有固定的当前 sonnet id，交给模型映射/接入方案）。
  渠道模型名清洗（`CleanOneMillionCapabilityModelName`）和请求侧共用这套规则。
- 真 Claude Code 的 beta 顺序不变：context-1m 在第 2 位（紧跟 `claude-code-20250219`），不重复；
  客户端自己带的 beta 原样保留。
- 未改：关形状时只留 context-1m、丢掉客户端其它 beta 的既有行为。

## 2. 思考内容放进正文（`thinking_to_content`，默认关）

- 跟 new-api 一样：开了就**总是**把思考用 `<think>\n…\n</think>\n` 放到正文前面，
  **流式和非流式一致**，两边都去掉 `reasoning_content` / `reasoning`。
  （以前非流式只在正文为空时才折，并保留 reasoning_content；流式却总是折——不一致。）
- **工具调用轮不折**：非流式消息带 `tool_calls`（或 `function_call`）就原样返回；
  流式里某个 choice 一旦出现 `tool_calls`，这个 chunk 及之后的思考都照常走 `reasoning_content`。
  流式在看到 tool_calls 之前已经发出去的思考撤不回来；已经打开的 `<think>` 照常在 finish / `[DONE]` 闭合。
- 只对 `/v1/chat/completions` 的客户端生效；不改发给上游的请求；存下来用于计费/日志的响应不改。

## 3. 也接嵌入请求（`capabilities: ["embeddings"]`）

- 后端判定改为「**渠道类型自带 OR 勾选**」（`Channel.SupportsCapability`）。以前非空能力位列表会
  整体替换类型默认值——嵌入渠道将来多勾一个别的能力位就会悄悄丢掉本职的嵌入。
- 嵌入请求一律走通用嵌入出站，不套任何 CLI 身份（header / TLS），这条不变。
- Anthropic 官方没有嵌入接口，所以在 Anthropic 渠道上加提示，只有上游中转也提供嵌入时才开。
