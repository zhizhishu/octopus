# Adaptive thinking 的 `display`：需求决定

出站 Anthropic 请求里 `thinking` 对象要不要带 `display`、带什么值。改之前先读。
相关代码：`claudeAdaptiveThinking`（`outbound/authropic/messages.go`）、
`claudeCLIShapeThinking`（`claude_shape_body.go`）、签名探针 harvest（`modeltest/runner.go`）。
真机捕获：`/workspace/cli_capture`（Claude Code 2.1.281 / 2.1.316）。

## 结论（一句话）

- **伪装成 Claude Code**（cloak 开）用 **-p 打印模式画像**：
  `{"type":"adaptive","display":"omitted"}`。与 2.1.281 黄金捕获、2.1.316 `-p` text/json
  字节一致；本画像的 beta 集**不含** `thinking-display-updates-2026-08-18` /
  `redact-thinking-2026-02-12`。
- **cloak=never**：只发 `{"type":"adaptive"}`（关思考时 sonnet-5-5 可用 API 的
  `between_tools`），不戴 CLI 的 `display`。
- **真 CLI 入站**：原样透传，一个字节都不改。
- **铁律**：从不单独发某个 `display` 值而不带它对应的 beta。

## 真 Claude Code 发什么（2.1.316 线捕）

| 场景 | `thinking.display` | 相关 beta |
|---|---|---|
| `-p` text / json（打印模式） | `"omitted"` | 无 display-updates / redact |
| 交互，设置 unset/false；或 `stream-json --verbose`（SDK） | `"updates"` | 尾部追加 `thinking-display-updates-2026-08-18`（在 effort-2025-11-24 之后） |
| 交互 + `showThinkingSummaries: true` / `--thinking-display summarized` | `"summarized"` | 无 display-updates |
| `--thinking-display omitted` 在交互 / stream-json | 仍被抬成 `"updates"` | 带 display-updates |
| `CLAUDE_CODE_THINKING_DISPLAY_UPDATES=0` | **无** `display` 键 | 带 `redact-thinking-2026-02-12` |
| stream-json + showThinkingSummaries true | **无** `display` 键 | （见捕获） |
| 2.1.281 交互默认 | **无** `display` 键 | — |

「关思考」：opus-4-7/4-8/sonnet-4-6 → `{"type":"disabled"}`；opus-5-5 / sonnet-5-5 →
仍是 `adaptive`+`omitted`（effort 默认 medium；`--effort low` 可得 low）。**从不发
`between_tools`。**

多轮回传：上一轮思考块原样为
`{"type":"thinking","thinking":"","signature":"..."}`（键序 type → thinking → signature，
空字符串保留）。

## 为什么伪装用 omitted（-p 画像）

- 对齐本仓库黄金捕获与 `-p` 线捕，不是「API 默认省略键」。
- 合成 beta 集与 `-p` 一致：没有 display-updates / redact。若发 `updates` 而不带
  `thinking-display-updates-2026-08-18`，违反「display 必须配 beta」。
- cloak=never 表示别冒充 Claude；带 `display:"omitted"` 是 CLI 体特征。

## 签名探针

Harvest 用 `display:"omitted"`（-p 画像）。`omitted` 只有签名、正文为空——合法；
回放靠签名解封。出站必须把空正文 + signature 按键序 type/thinking/signature 发出。

## sonnet-5-5「关」

- 伪装 / 探针：`adaptive`+`omitted`+`effort:low`（CLI 可产生）。
- cloak=never：可保留 API 的 `between_tools`+`low`。
- adaptive / between_tools 都仍会丢掉 temperature/top_p（sonnet-5-5 拒非默认采样）。

## 残留

交互且 summaries 关时，真 CLI 发 `updates`+beta，或（关 updates 环境变量时）省略
`display` 键。伪装侧固定 -p 画像的显式 `"omitted"`，不假装交互路径。
