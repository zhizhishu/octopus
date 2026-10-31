# modelverify — 来源与许可 / Provenance & License

本目录包含三个独立来源的移植，外加本项目自写的桥接与采集层：

| 路径 | 来源 | 许可 |
|---|---|---|
| `stats/` `probe/` `suite/` `suites/v1.example.yaml` | Token-Verifier | MIT |
| `behavior/`（含 `signature.go` `signature_probe.go`） | AI-Infra-Guard | Apache-2.0 |
| `tokenbank/`（含 `data/tokenizer_bank.json`）、`behavior/tokenizer_probe.go` | lm-detector | MIT |
| `fingerprint/`（含 `data/detector.bin` `suite.json` `models.json`）、`behavior/fingerprint_probe.go` | lm-detector | MIT |
| `types.go` `observe.go` `jsonanswer.go` `collect.go`；`fingerprint/generator.go` `neighbors.go` `data/neighbors.json` | 本项目自写 | 同本仓库 |

---

## 来源一：Token-Verifier（`stats/` `probe/` `suite/`）

本目录下的 `stats/`、`probe/`、`suite/` 三个包、它们的单元测试，以及
`suites/v1.example.yaml`，移植自 **Token-Verifier** 项目。

The `stats/`, `probe/` and `suite/` packages in this directory, their tests, and
`suites/v1.example.yaml` are ported from the **Token-Verifier** project.

- Source: <https://github.com/chaterm/Token-Verifier>
- Revision ported: `main` @ `a6e8a3d`
- Copyright: (c) 2026 chaterm
- License: MIT (full text below)

## 未移植的部分 / What was deliberately NOT ported

- `adapter/` and `transport/` — these render requests with a plain `net/http`
  client. Vendoring them would discard this project's forged outbound client
  identity and break its channel-fingerprint contract.
- `plan/`, `rawdata/`, `gate/`, `reconcile/` — a cross-tool comparison mechanism
  (plan digest gate plus a baseline file format). This project stores its own state.
- `report/`, `config/`, `cmd/tv/` — reporting backends, YAML config system and a
  standalone CLI entry point; this project has its own API and UI.

移植边界与理由的完整记录在容器层 `_references/notes/token-verifier-port.md`
（不在本仓库内，因为 `_references/` 不随仓库发布）。

## 变更说明 / Modifications

移植时仅做了以下机械性改动，未改动算法与判定语义：

- import 前缀改写为 `github.com/zhizhishu/octopus/internal/modelverify/`
- YAML 依赖由 `gopkg.in/yaml.v3` 改为 `go.yaml.in/yaml/v3`（同项目新家，API 兼容）
- 示例题库随包移动，测试中的相对路径相应调整

## 来源二：AI-Infra-Guard（`behavior/`）

`behavior/` 包移植自 **AI-Infra-Guard** 的模型审计模块（`relay_audit` 行为探针）。

- Source: <https://github.com/Tencent/AI-Infra-Guard>
- Revision ported: `main` @ `f09b267`
- Copyright: Tencent Zhuque Lab / Tencent
- License: Apache-2.0（全文见上游仓库根 `LICENSE`；本包是 Go 重写，非代码搬运）

上游是 Python。移植的是机制与数据，不是代码。

### 原样摘录的数据

- 15 个 glitch token 词表，与 8 个家族的签名编号表
- 四句 identity 提问、10 条 pip 命令池、9 个可疑改写词
- glitch 探针的题面措辞与编号解析规则
- 各风险项的权重与 40 / 70 风险分档
- Claude 签名验真的两段题面（harvest 问句、replay 复述指令）与 8 个 AWS 系痕迹词

### 移植的判定语义

- 截断降级：内容类探针被 max_tokens 砍断时降为低分"未能定论"，不当作渠道问题
- glitch 早停豁免：编号连续、前 n-1 条全对、第 n 条错时仍视为可分析
- glitch 一致性要求：失败编号含签名外编号时不得匹配家族
- identity 弱信号门槛：双方家族都识别出来且无交集才记录
- Claude 签名验真：harvest 取回 thinking.signature → 本地解出 protobuf 结构（绑定模型名 /
  nonce 段数 / 密文熵）→ 原样塞回 assistant 轮回放 → 从模型复述里找 AWS 系痕迹词

### 刻意偏离

- **只保留一套量纲**。上游同时存在"风险分 40/70"与"安全分 30/70"两套不同源的分档，
  换算边界方向相反（风险分 70 = 安全分 30，一个判高危一个判中危）。本包只输出风险分。
- **探针故障不产生风险项**。上游的流式探针把 HTTP 错误也记成"流完整性异常"，
  于是网络抖动会被读成中转站作弊。本包把「探针跑不动」与「探针发现问题」分成两条通道。
- **家族别名表增补中文品牌名**（通义/千问、文心、豆包、智谱、月之暗面、海螺、深度求索）。
  上游只收英文与拼音别名，而国内渠道的模型自称常是纯中文。
- **不移植 `models` 探针**：它需要单独的 `GET /models` 出站路径，而渠道有哪些模型
  Octopus 本来就知道，收益低。
- **不移植 `stream_integrity` 探针**：它依赖读原始 SSE 分片，而本项目的出站链路已把流
  归一化；硬做只能得到与上游语义不符的弱化版本。流是否完整由链路自身的错误通道体现。
- **不采用上游"让被测模型生成 summary"的做法**：那会让被测方参与自己的报告，且多花一次
  上游请求。本包用本地模板生成摘要。
- **不采用上游的 `extra_body` 关闭推理参数**（`reasoning.enabled=false`）：本项目的出站
  body 形状受渠道契约约束，不能为探针随意加字段。
- **签名验真的密文熵阈值改为随样本量缩放**。上游用固定阈值 7.5，但香农熵是有限样本
  统计量：256 字节的完美随机数据期望熵只有约 7.28，永远到不了 7.5，上游的阈值在
  n < 512 时会把真签名判成伪造。本包改用「该样本量下的理论期望上限 − 容差」，
  并在样本少于 128 字节时直接跳过这一项。
- **签名验真只移植可执行的检测项**。上游 11 项里，「响应头指纹」（找 `x-amz*`）需要读
  原始 HTTP 头，而本项目的出站链路把响应归一化之后才交给探针，拿不到头；「随机数指纹」
  要额外 30 次采样，成本远超其余各项。两项都不移植，其余判据按原相对权重配对到风险分。
- **签名验真在非 Anthropic 出站上自报"不适用"**。其它协议没有 thinking.signature 这回事，
  探针直接返回并跳过，既不产生风险项也不产生错误项。

移植边界与理由的完整记录在容器层 `_references/notes/ai-infra-guard-api-checker.md`
（不在本仓库内，因为 `_references/` 不随仓库发布）。

## 来源三：lm-detector（`tokenbank/`）

`tokenbank/` 是 **lm-detector** 分词器差分探测的 Go 移植；`data/tokenizer_bank.json` 逐字节复制。

- Source: <https://github.com/Ikaleio/lm-detector>
- Revision ported: `main` @ `175355e8939c2ee7aaeeac2b22186fa5940412c3`（2026-10-30）
- Copyright: (c) 2026 xqy2006
- License: MIT

移植对应关系（上游 TS → 本目录 Go）：

| 上游 | 本目录 |
|---|---|
| `shared/tokenizer-bank.ts`（类型与校验） | `tokenbank/bank.go` |
| `shared/tokenizer-posterior.ts`（`TOKENIZER_MODEL` 常量、后验、`nextProbes`、`tokenizerVerdict`） | `tokenbank/posterior.go` `plan.go` `verdict.go` |
| `shared/tokenizer-probe.ts`（`probeTokenizer` 驱动） | `tokenbank/run.go` |
| `shared/tokenizer-usage.ts`（输入 token 口径） | `modeltest/probe.go` `probeBilledInputTokens` |

一致性：`tokenbank/testdata/reference_golden.json` 由上游 TS 源码打包后生成（93 个场景：62 个
精确类、12 个噪声、6 个近亲、6 个库外、6 个声明核对、1 个基线漂移），Go 实现逐请求、逐后验
（容差 1e-9）对齐。

本项目新增（上游没有）：

- `tokenbank/local.go`：可信渠道录的本地基线，作为第一方类合入库（闭源型号用）；
- `behavior/tokenizer_probe.go`：接入检测链，`tokenizer_baseline` 录制探针；
- 计分：照抄上游，分词器结果只作参考——"与声明不符"以 0 分、low 的参考项展示（带边界说明），
  不计入风险分、不进 verdict 的"跑通"分母，也不取代 `token_delta` 的计分项（两条都保留）。
- 超时：两项分词器探针按计划请求数（74 / 22 次）放大整单超时，每次 10 秒，封顶 30 分钟
  （`op.AuditTimeoutFor`）。

## 来源三（续）：lm-detector 随机数指纹（`fingerprint/`）

`fingerprint/` 是 lm-detector「随机数指纹」排名器与校准的 Go 移植；数据文件逐字节复制（`models.json` 由
`data/unified_bank.json` 裁剪）。详见 `fingerprint/data/README.md`。

- Source: <https://github.com/Ikaleio/lm-detector>
- Revision: `main` @ `175355e8939c2ee7aaeeac2b22186fa5940412c3`（2026-10-30），参考库 64 个模型，`built_at` 2026-10-27
- Copyright: (c) 2026 xqy2006
- License: MIT（全文见本文件末尾）

一致性：`fingerprint/testdata/` 的黄金数据由上游 TS 源码（`shared/shared-detector.ts` 等）对同一份
`shared_detector.bin` 现场导出，Go 实现逐项对齐（容差 1e-7）。

移植的判定语义：解析规则、`max(80, ceil(0.55N))` 采信门槛、两路排名、温度校准、
「序列形态」把关（相邻自相关 > −0.23 且小步占比 > 5% 才判不像模型手写，界限取自上游公布的
参考库统计）、「分布异常样本」（最小值 ≥ 200）提示。

本项目自写（上游没有或只学概念）：

- `fingerprint/generator.go`：随机出题器。概念来自上游网页检测的随机题，措辞、语言集、数量区间都是自己写的；
  固定 36 题保留为对照模式（设置 `model_audit_fingerprint_random_prompts=false`）。
- `fingerprint/neighbors.go` + `data/neighbors.json`：离线近邻表（相对距离口径与上游 `nearestModels` 相同，
  实现为 Go 自写，结果与上游逐个模型前三名一致）。
- 计分：「不像模型手写」与「分布异常」一律按证据不足处理（0 分，不定罪）。

刻意不学：上游的出站方式（浏览器直连 / 代理 / 自建 Worker、`reasoning_effort` 注入、读够数量就掐断流）。
本项目的探针只换题目文字，出站一律走渠道检测的同一条 shape 链路。

## MIT License

```
MIT License

Copyright (c) 2026 chaterm

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

---

### lm-detector — MIT License

Copyright (c) 2026 xqy2006

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
