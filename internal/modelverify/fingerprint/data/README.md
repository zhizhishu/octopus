# 数据来源与许可

本目录下的数据文件来自 **lm-detector**（https://github.com/Ikaleio/lm-detector，MIT），
用于"随机数指纹"模型识别。

## 许可

```
MIT License

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
```

## 各文件说明

| 文件 | 来源 | 作用 |
| --- | --- | --- |
| `detector.bin` | `data/shared_detector.bin` | 排名器与校准参数。运行时唯一的大件。 |
| `suite.json` | `data/enrollment-suite.json` | 36 道固定挑战题。必须**原题重放**：参考库的分布是在这些具体措辞下采集的。 |
| `neighbors.json` | 本项目离线计算（不是上游文件） | 每个模型最难分开的 5 个近邻（相对距离），由 `TestRegenerateNeighbors` 从 `detector.bin` 的参考回答算出，绑定 `bank_built_at`。 |
| `models.json` | 由 `data/unified_bank.json` 裁剪 | 只保留 70 个模型的名称/家族/样本数，以及"库与检测器是否配套"的校验字段。原始统计库里的逐模型频次表在推理阶段用不到，故不打包。 |

参考库版本：`2026-11-12T18:48:30.723628+00:00`（见 `models.json` 的 `built_at`），取自上游 `main` @ `2c61f8f836de93d3ce60a9f76f8b8ff9e15011ac`（2026-11-13）。

## 能力边界（照抄上游文档的表述，务必保留）

- 输出的概率是**参考库内的闭集相对值**，只在库内 70 个候选之间分配，合计 100%。
- 它**不包含库外模型**，因此**不代表"确实是该模型"的概率**。库外模型同样会得到
  一个"最像"的候选。
- 相邻小版本之间几乎无法区分（同代 opus 4.6 与 4.7 的分布余弦相似度约 0.997）。
- 因此本检测的正确用法是**证伪**：声明是 A 却怎么看都不像 A，那才是问题。
  指纹吻合只能说明"没有矛盾"，**不能作为正品证明**。

## 如何更新

换参考库时必须三件一起换（`detector.bin`、`suite.json`、`models.json`），并重算 `neighbors.json`
（`OCTOPUS_REGEN_FP_NEIGHBORS=1 go test ./internal/modelverify/fingerprint -run TestRegenerateNeighbors`）与
`../testdata/` 黄金数据（用上游 TS 源码重新导出），
且 `models.json` 的 `built_at` 必须等于 `detector.bin` 里的 `bank_built_at`，
否则 `Analyze` 会直接报"参考库与检测器不配套"并拒绝出结论——错配的库会让排名
错得很隐蔽，照样给出一个很像样的答案。

### 本次刷新与复现

本次保留原有四组回答输入，增加 GPT-5.5、Gemini 3.1 Pro 以及 Gemini 4
Argon / Carbon / Hydrogen 三组公开参考回答。所有期望值来自上述固定提交的
TypeScript 实现，未用 Go 端输出反向生成期望值。浮点比对容差保持 `1e-7`。

在 Linux 验证环境运行：

```sh
node internal/modelverify/fingerprint/testdata/generate-reference.mjs /path/to/lm-detector /path/to/evidence /path/to/baseline-octopus
OCTOPUS_REGEN_FP_NEIGHBORS=1 go test ./internal/modelverify/fingerprint -run TestRegenerateNeighbors
go test ./internal/modelverify/fingerprint ./internal/modelverify/behavior
```

生成器会校验 detector、统计库和原始参考回答的 SHA-256 与配套关系，再导出
`g_*.json`。参考回答必须使用 Git blob 的原始字节；Windows 的 CRLF
工作副本或归档转换会触发校验失败，不能跳过校验。
`../testdata/reference_manifest.json` 记录来源提交、文件指纹、校准绑定和回答来源。
模型表、近邻表和黄金数据必须作为同一批次更新，不能单独替换其中一项。
