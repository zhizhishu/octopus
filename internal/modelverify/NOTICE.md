# modelverify — 来源与许可 / Provenance & License

本目录包含两个独立来源的移植，外加本项目自写的桥接与采集层：

| 路径 | 来源 | 许可 |
|---|---|---|
| `stats/` `probe/` `suite/` `suites/v1.example.yaml` | Token-Verifier | MIT |
| `behavior/` | AI-Infra-Guard | Apache-2.0 |
| `types.go` `observe.go` `jsonanswer.go` `collect.go` | 本项目自写 | 同本仓库 |

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

- import 前缀改写为 `github.com/bestruirui/octopus/internal/modelverify/`
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

### 移植的判定语义

- 截断降级：内容类探针被 max_tokens 砍断时降为低分"未能定论"，不当作渠道问题
- glitch 早停豁免：编号连续、前 n-1 条全对、第 n 条错时仍视为可分析
- glitch 一致性要求：失败编号含签名外编号时不得匹配家族
- identity 弱信号门槛：双方家族都识别出来且无交集才记录

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

移植边界与理由的完整记录在容器层 `_references/notes/ai-infra-guard-api-checker.md`
（不在本仓库内，因为 `_references/` 不随仓库发布）。

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
